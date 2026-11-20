package gridapi

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/meshcore"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Two questions the edge list alone answers badly, both asked of one node at
// a time: "what does this node hear" (GET /api/v1/mesh/links?node=) and "is
// there a way from here to there" (GET /api/v1/mesh/path). Both read the same
// derived links as the unfiltered list (Store.MeshLinks), so there is one
// topology and three views of it, and both name the nodes they mention: a
// 64-character key is not an answer a person can use.
//
// What neither can say: the links are learned only from paths the Grid could
// resolve without guessing (internal/clients/meshcore/relay.go), so a link
// that is absent was not observed, which is not the same as not existing, and
// a path is a chain of links that each carried traffic in the window, not a
// promise that a packet sent now would arrive.

const (
	// maxPathEndpoints bounds each side of a path request. A side is a set
	// because a caller often means a place, not one radio: "from Arnold" is
	// whichever node near Arnold has a way out.
	maxPathEndpoints = 16
	// pathFreshWithin and pathThinBelow mark a link the search would rather
	// not lean on: last seen more than a day ago, or seen only once or twice
	// in the whole window. Each costs half a hop, so a route over two such
	// links loses to one a hop longer over links that are carrying traffic.
	pathFreshWithin = 24 * time.Hour
	pathThinBelow   = 3
)

// meshNodeRefJSON is what the Grid knows about a node a link or path names.
type meshNodeRefJSON struct {
	Name     string `json:"name"`
	NodeType string `json:"nodeType"`
	Status   string `json:"status"`
	// The two activity clocks (MeshTelemetry.lastHeardAt / lastRelayedAt),
	// null when the Grid has none on record.
	LastHeardAt   *string         `json:"lastHeardAt"`
	LastRelayedAt *string         `json:"lastRelayedAt"`
	Location      *meshLatLngJSON `json:"location"`
}

type meshLatLngJSON struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// meshNodeIndex is every catalogued mesh node by lowercase public key, in
// any status: a node that expired yesterday still ends links seen this week.
func (s *Service) meshNodeIndex(ctx context.Context) (map[string]*gridv1.Event, error) {
	q := store.EventQuery{
		Layers: []gridv1.Layer{gridv1.Layer_MESH},
		Statuses: []gridv1.EventStatus{
			gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED,
			gridv1.EventStatus_RESOLVED, gridv1.EventStatus_EXPIRED,
		},
		PageSize: 200,
		Lite:     true,
	}
	out := map[string]*gridv1.Event{}
	var all []*gridv1.Event
	for {
		page, next, err := s.Store.QueryEvents(ctx, q)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		q.PageToken = next
	}
	s.overlayMeshActivity(ctx, all)
	for _, ev := range all {
		if pk := strings.ToLower(ev.GetMesh().GetPublicKey()); pk != "" {
			out[pk] = ev
		}
	}
	return out, nil
}

func meshNodeRef(ev *gridv1.Event) meshNodeRefJSON {
	ref := meshNodeRefJSON{
		Name:     ev.GetMesh().GetName(),
		NodeType: ev.GetMesh().GetNodeType(),
		Status:   ev.GetStatus().String(),
	}
	if t := ev.GetMesh().GetTelemetry().GetLastHeardAt(); t != nil {
		v := t.AsTime().UTC().Format(time.RFC3339)
		ref.LastHeardAt = &v
	}
	if t := ev.GetMesh().GetTelemetry().GetLastRelayedAt(); t != nil {
		v := t.AsTime().UTC().Format(time.RFC3339)
		ref.LastRelayedAt = &v
	}
	if c := ev.GetGeometry().GetCentroid(); c != nil {
		ref.Location = &meshLatLngJSON{Lat: c.GetLat(), Lng: c.GetLng()}
	}
	return ref
}

// meshNodeRefs describes the catalogued nodes among keys. A key the catalogue
// does not hold is left out: the links know relays the Grid keeps no record
// of (outside its area, or never heard advertising), and inventing a name for
// one would be worse than its absence.
func meshNodeRefs(index map[string]*gridv1.Event, keys []string) map[string]meshNodeRefJSON {
	out := make(map[string]meshNodeRefJSON, len(keys))
	for _, k := range keys {
		if ev, ok := index[k]; ok {
			out[k] = meshNodeRef(ev)
		}
	}
	return out
}

func toMeshLinkJSON(l store.MeshLink) meshLinkJSON {
	return meshLinkJSON{
		A: l.A, B: l.B, Observations: l.Observations, DaysActive: l.DaysActive,
		FirstSeen: l.FirstSeen.Format(time.RFC3339), LastSeen: l.LastSeen.Format(time.RFC3339),
		BestSnr: l.BestSNR,
	}
}

/* ------------------------------------------------------- one node's links */

type meshNodeLinksResponse struct {
	Window      string                     `json:"window"`
	GeneratedAt string                     `json:"generatedAt"`
	Node        string                     `json:"node"`
	Links       []meshLinkJSON             `json:"links"`
	Nodes       map[string]meshNodeRefJSON `json:"nodes"`
}

// serveMeshNodeLinks answers GET /api/v1/mesh/links?node=: the links that end
// at one node, most recently seen first, with the nodes they name. The
// unfiltered list keeps its own shape and validator (serveMeshLinks).
func (s *Service) serveMeshNodeLinks(w http.ResponseWriter, r *http.Request, window time.Duration, rawNode string) {
	ctx := r.Context()
	node, ok := meshcore.ParseNodeKey(rawNode)
	if !ok || len(node) != meshcore.MaxNodeIDHex {
		invalidArgument(w, fmt.Sprintf(
			"invalid node %q: want one node's full public key (%d hex characters), optionally prefixed %q; find it with /api/v1/search?kind=MESH_NODE",
			rawNode, meshcore.MaxNodeIDHex, meshcore.EventIDPrefix))
		return
	}
	now := s.Now()
	links, err := s.Store.MeshLinks(ctx, now.Add(-window))
	if err != nil {
		internal(ctx, w, err)
		return
	}
	index, err := s.meshNodeIndex(ctx)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	resp := meshNodeLinksResponse{
		Window: windowLabel(window), GeneratedAt: now.UTC().Format(time.RFC3339),
		Node: node, Links: []meshLinkJSON{},
	}
	keys := []string{node}
	var mine []store.MeshLink
	for _, l := range links {
		a, b := strings.ToLower(l.A), strings.ToLower(l.B)
		if a != node && b != node {
			continue
		}
		mine = append(mine, l)
		if a == node {
			keys = append(keys, b)
		} else {
			keys = append(keys, a)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].LastSeen.After(mine[j].LastSeen) })
	for _, l := range mine {
		resp.Links = append(resp.Links, toMeshLinkJSON(l))
	}
	resp.Nodes = meshNodeRefs(index, keys)
	writeMeshJSON(ctx, w, r, &resp, func() { resp.GeneratedAt = "" })
}

// writeMeshJSON writes v with a weak validator over everything but the
// per-request stamp: blank clears it, the tag is taken, and the body that goes
// out is the one marshaled before.
func writeMeshJSON(ctx context.Context, w http.ResponseWriter, r *http.Request, v any, blank func()) {
	body, err := json.Marshal(v)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	blank()
	data, err := json.Marshal(v)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	const contentType = "application/json"
	writeJSON(w, r, body, dataETag(contentType, data), contentType, maxAgeConditions)
}

/* ------------------------------------------------------------- path search */

// meshPathJSON is one route: the nodes in order and the link between each
// neighbouring pair (len(links) == len(nodes)-1).
type meshPathJSON struct {
	Hops  int            `json:"hops"`
	Nodes []string       `json:"nodes"`
	Links []meshLinkJSON `json:"links"`
}

type meshPathResponse struct {
	Window      string   `json:"window"`
	GeneratedAt string   `json:"generatedAt"`
	From        []string `json:"from"`
	To          []string `json:"to"`
	// Found is false when no chain of observed links joins the two sides.
	// That is "not observed", never "no path": see Basis.
	Found bool `json:"found"`
	// Path is the preferred route, null when none was found. Alternate is a
	// second route that shares none of Path's relays (or, when Path is a
	// single link, does not use that link), null when the links hold none.
	Path      *meshPathJSON `json:"path"`
	Alternate *meshPathJSON `json:"alternate"`
	// LinkCounts is how many links each requested endpoint has in the window.
	// An endpoint with none is why a search fails more often than distance.
	LinkCounts map[string]int             `json:"linkCounts"`
	Nodes      map[string]meshNodeRefJSON `json:"nodes"`
	Basis      string                     `json:"basis"`
}

// meshPathBasis rides on every answer, found or not: a client that shows the
// route without it is showing an inference as a fact.
const meshPathBasis = "Inferred from links the Grid observed carrying traffic in the window. " +
	"Links are learned only from packet paths it could resolve without guessing, so a missing " +
	"link or path was not observed, which does not mean it does not exist; and a path found " +
	"is not a guarantee that a message sent now would be delivered. The route prefers fewer " +
	"hops, counting a link last seen over 24h ago or seen fewer than 3 times as half a hop more."

// serveMeshPath answers GET /api/v1/mesh/path?from=&to=&window=.
func (s *Service) serveMeshPath(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	window, err := parseMeshWindow(r.URL.Query().Get("window"))
	if err != nil {
		invalidArgument(w, err.Error())
		return
	}
	from, err := parsePathEndpoints("from", r.URL.Query()["from"])
	if err != nil {
		invalidArgument(w, err.Error())
		return
	}
	to, err := parsePathEndpoints("to", r.URL.Query()["to"])
	if err != nil {
		invalidArgument(w, err.Error())
		return
	}
	now := s.Now()
	links, err := s.Store.MeshLinks(ctx, now.Add(-window))
	if err != nil {
		internal(ctx, w, err)
		return
	}
	index, err := s.meshNodeIndex(ctx)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	g := newMeshGraph(links, now, func(key string) bool {
		ev, ok := index[key]
		// A relay the catalogue does not hold was still named in a resolved
		// path, so it relays. A catalogued companion or sensor never does.
		return !ok || meshcore.RelayCapable(ev.GetMesh().GetNodeType())
	})
	resp := meshPathResponse{
		Window: windowLabel(window), GeneratedAt: now.UTC().Format(time.RFC3339),
		From: from, To: to, LinkCounts: map[string]int{}, Basis: meshPathBasis,
	}
	keys := append(append([]string{}, from...), to...)
	for _, k := range keys {
		resp.LinkCounts[k] = len(g.adj[k])
	}
	if p := g.shortest(from, to, nil, nil); p != nil {
		resp.Found = true
		resp.Path = g.pathJSON(p)
		keys = append(keys, p...)
		// A second way round: without the first route's relays, or, when the
		// two ends hear each other directly, without that one link.
		avoidNodes, avoidLink := map[string]bool{}, (*linkKey)(nil)
		if len(p) > 2 {
			for _, k := range p[1 : len(p)-1] {
				avoidNodes[k] = true
			}
		} else if len(p) == 2 {
			k := orderedLinkKey(p[0], p[1])
			avoidLink = &k
		}
		if len(p) > 1 {
			if alt := g.shortest(from, to, avoidNodes, avoidLink); alt != nil {
				resp.Alternate = g.pathJSON(alt)
				keys = append(keys, alt...)
			}
		}
	}
	resp.Nodes = meshNodeRefs(index, keys)
	writeMeshJSON(ctx, w, r, &resp, func() { resp.GeneratedAt = "" })
}

// parsePathEndpoints reads one side of a path request: full public keys,
// repeated or comma-separated, deduplicated in the order given.
func parsePathEndpoints(name string, values []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, v := range values {
		for _, raw := range strings.Split(v, ",") {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			key, ok := meshcore.ParseNodeKey(raw)
			if !ok || len(key) != meshcore.MaxNodeIDHex {
				return nil, fmt.Errorf(
					"invalid %s %q: want full node public keys (%d hex characters each, optionally prefixed %q), comma-separated",
					name, strings.TrimSpace(raw), meshcore.MaxNodeIDHex, meshcore.EventIDPrefix)
			}
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is required: one or more node public keys, comma-separated", name)
	}
	if len(out) > maxPathEndpoints {
		return nil, fmt.Errorf("%s lists %d nodes; at most %d", name, len(out), maxPathEndpoints)
	}
	return out, nil
}

func orderedLinkKey(a, b string) linkKey {
	if a > b {
		a, b = b, a
	}
	return linkKey{a, b}
}

type linkKey struct{ a, b string }

// meshGraph is the undirected link graph a path is searched in.
type meshGraph struct {
	adj   map[string][]meshEdge
	links map[linkKey]store.MeshLink
	// relays reports whether a node may sit in the middle of a path.
	relays func(key string) bool
}

type meshEdge struct {
	to   string
	cost float64
}

func newMeshGraph(links []store.MeshLink, now time.Time, relays func(string) bool) *meshGraph {
	g := &meshGraph{adj: map[string][]meshEdge{}, links: map[linkKey]store.MeshLink{}, relays: relays}
	for _, l := range links {
		a, b := strings.ToLower(l.A), strings.ToLower(l.B)
		if a == b || a == "" || b == "" {
			continue
		}
		cost := 1.0
		if now.Sub(l.LastSeen) > pathFreshWithin {
			cost += 0.5
		}
		if l.Observations < pathThinBelow {
			cost += 0.5
		}
		g.links[orderedLinkKey(a, b)] = l
		g.adj[a] = append(g.adj[a], meshEdge{b, cost})
		g.adj[b] = append(g.adj[b], meshEdge{a, cost})
	}
	// A fixed neighbour order makes equal-cost routes come out the same on
	// every request, whatever order the store returned the links in.
	for k := range g.adj {
		sort.Slice(g.adj[k], func(i, j int) bool { return g.adj[k][i].to < g.adj[k][j].to })
	}
	return g
}

// shortest returns the cheapest route from any node of from to any node of
// to, as the keys in order, or nil. Only a relay-capable node is passed
// through; avoidNodes are never entered and avoidLink is never crossed.
func (g *meshGraph) shortest(from, to []string, avoidNodes map[string]bool, avoidLink *linkKey) []string {
	target := make(map[string]bool, len(to))
	for _, k := range to {
		target[k] = true
	}
	dist := map[string]float64{}
	prev := map[string]string{}
	done := map[string]bool{}
	pq := &pathQueue{}
	for _, k := range from {
		if target[k] {
			return []string{k} // one node is on both sides
		}
		if _, seen := dist[k]; !seen {
			dist[k] = 0
			heap.Push(pq, pathItem{k, 0})
		}
	}
	source := make(map[string]bool, len(from))
	for _, k := range from {
		source[k] = true
	}
	for pq.Len() > 0 {
		cur := heap.Pop(pq).(pathItem)
		if done[cur.key] {
			continue
		}
		done[cur.key] = true
		if target[cur.key] {
			path := []string{cur.key}
			for k := cur.key; !source[k]; {
				k = prev[k]
				path = append(path, k)
			}
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			return path
		}
		// Leaving a node means it relayed: an endpoint may be any node, but
		// nothing in between may be a companion or a sensor.
		if !source[cur.key] && !g.relays(cur.key) {
			continue
		}
		for _, e := range g.adj[cur.key] {
			if done[e.to] || avoidNodes[e.to] || source[e.to] {
				continue
			}
			if avoidLink != nil && orderedLinkKey(cur.key, e.to) == *avoidLink {
				continue
			}
			d := cur.dist + e.cost
			if old, ok := dist[e.to]; !ok || d < old {
				dist[e.to] = d
				prev[e.to] = cur.key
				heap.Push(pq, pathItem{e.to, d})
			}
		}
	}
	return nil
}

func (g *meshGraph) pathJSON(keys []string) *meshPathJSON {
	p := &meshPathJSON{Hops: len(keys) - 1, Nodes: keys, Links: make([]meshLinkJSON, 0, len(keys))}
	for i := 1; i < len(keys); i++ {
		p.Links = append(p.Links, toMeshLinkJSON(g.links[orderedLinkKey(keys[i-1], keys[i])]))
	}
	return p
}

type pathItem struct {
	key  string
	dist float64
}

// pathQueue is a min-heap on distance, then key, so ties resolve the same way
// every time.
type pathQueue []pathItem

func (q pathQueue) Len() int { return len(q) }
func (q pathQueue) Less(i, j int) bool {
	if q[i].dist != q[j].dist {
		return q[i].dist < q[j].dist
	}
	return q[i].key < q[j].key
}
func (q pathQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *pathQueue) Push(x any)   { *q = append(*q, x.(pathItem)) }
func (q *pathQueue) Pop() any {
	old := *q
	it := old[len(old)-1]
	*q = old[:len(old)-1]
	return it
}
