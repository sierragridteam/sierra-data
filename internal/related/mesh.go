package related

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Mesh relations are structural, not situational: two mesh nodes are related
// when a monitor's reads connect them. A monitor (a REPORTER source: the
// SIERRA relay, an operator's own Pi) is not a node, so it has no event of its
// own. The relay listens through many gateways (companion radios at hosts'
// houses) and says which of them it has read each repeater through
// (telemetry.readVia); those reads are the links. Being read by the same
// monitor is not: the relay's gateways sit in parts of the mesh that cannot
// hear each other, and one part's nodes are not related to another's.
//
// They do not go through Rule: nothing here depends on a time window or a
// distance (a shared repeater may have no published position at all), which is
// all a Rule can express.
const (
	// A gateway and a node read through it.
	RuleMeshMonitorGateway = "mesh-monitor-gateway"
	// Two nodes one read apart: repeaters read through the same gateway,
	// gateways that read the same repeater. Also the nodes of a monitor that
	// states no gateways (it reads everything from one place).
	RuleMeshMonitorPeer = "mesh-monitor-peer"
)

// meshReporter is the monitor whose reading the node carries, "" when none.
func meshReporter(ev *gridv1.Event) string {
	return ev.GetMesh().GetTelemetry().GetAdmin().GetReporterId()
}

// meshCompanion reports whether the node is a companion radio, the kind a
// monitor listens through.
func meshCompanion(ev *gridv1.Event) bool {
	return strings.EqualFold(ev.GetMesh().GetNodeType(), "companion")
}

func meshKey(ev *gridv1.Event) string { return strings.ToLower(ev.GetMesh().GetPublicKey()) }

// meshRelated lists the nodes one read away from subject, then the nodes two
// reads away. What is claimed is only what the reports state: this gateway
// read that node. Nothing is followed further, so two parts of the mesh with
// no gateway and no repeater in common share no relations.
func (e *Engine) meshRelated(ctx context.Context, subject *gridv1.Event) ([]Relation, error) {
	var nodes []*gridv1.Event
	q := store.EventQuery{Layers: []gridv1.Layer{gridv1.Layer_MESH}, PageSize: store.MaxPageSize, Lite: true}
	for {
		page, err := e.Store.QueryEventsPage(ctx, q)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, page.Items...)
		q.PageToken = page.Next(len(page.Items))
		if q.PageToken == "" {
			break
		}
	}
	byID := map[string]*gridv1.Event{subject.GetId(): subject}
	for _, n := range nodes {
		if n.GetId() != subject.GetId() {
			byID[n.GetId()] = n
		}
	}
	// gatewayID names the node a readVia entry means: the one node whose key
	// it is, or is a prefix of. An entry that fits no stored node (or several)
	// still links the nodes that share it, under its own text.
	gatewayID := func(via string) string {
		via = strings.ToLower(via)
		found := ""
		for id, n := range byID {
			if k := meshKey(n); k != "" && strings.HasPrefix(k, via) {
				if found != "" {
					return "?" + via
				}
				found = id
			}
		}
		if found == "" {
			return "?" + via
		}
		return found
	}
	gatewaysOf := map[string][]string{} // node id -> the gateways it is read through
	readBy := map[string][]string{}     // gateway -> the node ids read through it
	stated := map[string]bool{}         // monitors that have gateways
	for id, n := range byID {
		via := n.GetMesh().GetTelemetry().GetReadVia()
		if r := meshReporter(n); r != "" && (len(via) > 0 || meshCompanion(n)) {
			stated[r] = true
		}
		for _, v := range via {
			if gw := gatewayID(v); gw != id && !slices.Contains(gatewaysOf[id], gw) {
				gatewaysOf[id] = append(gatewaysOf[id], gw)
				readBy[gw] = append(readBy[gw], id)
			}
		}
	}

	best := map[string]Relation{}
	keep := func(id, rule string, c Confidence, reason string) {
		other := byID[id]
		if other == nil || id == subject.GetId() {
			return
		}
		cur, ok := best[id]
		if !ok || c > cur.Confidence || c == cur.Confidence && reason < cur.Reason {
			best[id] = Relation{Event: other, Rule: rule, Confidence: c, Reason: reason}
		}
	}
	sid := subject.GetId()
	for _, gw := range gatewaysOf[sid] {
		keep(gw, RuleMeshMonitorGateway, Medium, "A gateway this node is read through")
		for _, peer := range readBy[gw] {
			reason := "Read through the same gateway"
			if g := byID[gw]; g != nil && meshName(g) != "" {
				reason = fmt.Sprintf("Read through the same gateway, %s", meshName(g))
			}
			keep(peer, RuleMeshMonitorPeer, Context, reason)
		}
	}
	for _, node := range readBy[sid] {
		keep(node, RuleMeshMonitorGateway, Medium, "Read through this gateway")
		for _, gw := range gatewaysOf[node] {
			reason := "Reads a node this gateway also reads"
			if name := meshName(byID[node]); name != "" {
				reason = fmt.Sprintf("Also reads %s", name)
			}
			keep(gw, RuleMeshMonitorPeer, Context, reason)
		}
	}
	// A monitor that states no gateways reads all its nodes from one place
	// (an operator's Pi): they are each other's peers.
	if reporter := meshReporter(subject); reporter != "" && !stated[reporter] {
		reason := fmt.Sprintf("Also read by %s", e.reporterName(reporter))
		for id, n := range byID {
			if meshReporter(n) == reporter {
				keep(id, RuleMeshMonitorPeer, Context, reason)
			}
		}
	}

	out := make([]Relation, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		// Within a confidence: by name, so the card reads the same every time.
		if a, b := meshName(out[i].Event), meshName(out[j].Event); a != b {
			return a < b
		}
		return out[i].Event.GetId() < out[j].Event.GetId()
	})
	return out, nil
}

func meshName(ev *gridv1.Event) string {
	if n := ev.GetMesh().GetName(); n != "" {
		return n
	}
	return ev.GetHeadline()
}

// reporterName is the monitor's display name ("SIERRA Mesh Relay"), or its id
// in quotes when the caller supplied no names or does not know this one.
func (e *Engine) reporterName(id string) string {
	if e.ReporterName != nil {
		if n := strings.TrimSpace(e.ReporterName(id)); n != "" {
			return n
		}
	}
	return fmt.Sprintf("the monitor %q", id)
}
