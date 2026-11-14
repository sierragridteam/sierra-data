package related

import (
	"context"
	"fmt"
	"sort"
	"strings"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Mesh relations are structural, not situational: two mesh nodes are related
// when the same operator-run monitor reads both. A monitor (a REPORTER source:
// the SIERRA relay, an operator's own Pi) is not a node, so it has no event of
// its own; the nodes it reads are what can be linked, through the reporter id
// each one's admin telemetry carries.
//
// They do not go through Rule: nothing here depends on a time window or a
// distance (a shared repeater may have no published position at all), which is
// all a Rule can express.
const (
	// A gateway (the companion radio a monitor listens through) and a node
	// that monitor reads.
	RuleMeshMonitorGateway = "mesh-monitor-gateway"
	// Two nodes of the same kind read by one monitor: its repeaters with each
	// other, its gateways with each other.
	RuleMeshMonitorPeer = "mesh-monitor-peer"
)

// meshReporter is the monitor that reads the node, "" when none does.
func meshReporter(ev *gridv1.Event) string {
	return ev.GetMesh().GetTelemetry().GetAdmin().GetReporterId()
}

// meshGateway reports whether the node is a monitor's own radio: the relay and
// the operators' monitors listen through companions, and read everything else.
func meshGateway(ev *gridv1.Event) bool {
	return strings.EqualFold(ev.GetMesh().GetNodeType(), "companion")
}

// meshRelated lists the nodes read by the monitor that reads subject. What is
// claimed is only what the monitor's reports state: that it reads both. Which
// gateway a repeater was read through is not reported, so a gateway and a
// repeater are related through their monitor at Medium, never as "this gateway
// reads that repeater"; two repeaters (or two gateways) of one monitor are
// Context.
func (e *Engine) meshRelated(ctx context.Context, subject *gridv1.Event) ([]Relation, error) {
	reporter := meshReporter(subject)
	if reporter == "" {
		return nil, nil // no monitor reads it: most nodes
	}
	name := e.reporterName(reporter)
	var out []Relation
	q := store.EventQuery{Layers: []gridv1.Layer{gridv1.Layer_MESH}, PageSize: store.MaxPageSize, Lite: true}
	for {
		page, err := e.Store.QueryEventsPage(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, other := range page.Items {
			if other.GetId() == subject.GetId() || meshReporter(other) != reporter {
				continue
			}
			out = append(out, meshRelation(subject, other, name))
		}
		q.PageToken = page.Next(len(page.Items))
		if q.PageToken == "" {
			break
		}
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

func meshRelation(subject, other *gridv1.Event, monitor string) Relation {
	r := Relation{Event: other}
	switch sg, og := meshGateway(subject), meshGateway(other); {
	case sg && og:
		r.Rule, r.Confidence, r.Reason = RuleMeshMonitorPeer, Context, fmt.Sprintf("Another gateway of %s", monitor)
	case sg:
		r.Rule, r.Confidence, r.Reason = RuleMeshMonitorGateway, Medium, fmt.Sprintf("Read by %s, which listens through this gateway", monitor)
	case og:
		r.Rule, r.Confidence, r.Reason = RuleMeshMonitorGateway, Medium, fmt.Sprintf("A gateway of %s, which reads this node", monitor)
	default:
		r.Rule, r.Confidence, r.Reason = RuleMeshMonitorPeer, Context, fmt.Sprintf("Also read by %s", monitor)
	}
	return r
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
