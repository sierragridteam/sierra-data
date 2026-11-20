package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The mesh tools. Like every tool here they are views of /api/v1: node
// records are layer=mesh events, liveness is the events' two activity clocks,
// links and paths are /api/v1/mesh/links?node= and /api/v1/mesh/path (the
// graph work is the API's, internal/gridapi/mesh_path.go), a battery curve is
// /api/v1/mesh/telemetry, and "what changed" is the events' history and
// timelines. What this file adds is the reading a model needs and would
// otherwise get wrong: names instead of keys, ages beside times, and a plain
// statement in the data of what the answer cannot show (mesh_model.go).
//
// Nothing here reaches the mesh relay. Its admin surface, tokens, client
// lists and frames are a different service this process does not import.

const (
	// defaultMeshPlace scopes a question that names no place: the Ebbetts
	// Pass area SIERRA serves. The feed itself reaches far beyond it (a
	// broker carries much of Northern California's mesh), and a health
	// answer about 700 strangers' repeaters is not the one being asked for.
	defaultMeshPlace = "ebbetts-pass"

	defaultMeshRadiusKm = 15.0
	maxMeshRadiusKm     = 60.0
	defaultMeshRows     = 40
	maxMeshRows         = 100
	// maxPathSide is the most nodes a place contributes to one side of a
	// path search (the API takes 16).
	maxPathSide = 12
	// maxChangeNodes bounds the timelines grid_mesh_changes reads.
	maxChangeNodes = 40
)

const (
	noteNotDown = "States describe what the Grid has heard, not what a node is doing. " +
		"\"silent\" and \"quiet\" mean no packet from or through the node reached the Grid's brokers and gateways in that time; " +
		"\"unknown\" means it has no activity on record. None of them means the node is off, and \"active\" does not mean it will carry a message now."
	noteLinks = "Links are learned only from packet paths the Grid could resolve without guessing (1-byte hops resolve conservatively), " +
		"so some real links are missing by design: a link not listed was not observed, which is not proof there is none."
	noteCoverage = "Radio coverage is not modelled. These are nodes located in or near the place; terrain decides what they can actually reach."
)

func (s *Server) meshTools() []tool {
	return []tool{
		{
			Name: "grid_mesh_status",
			Description: "Is the mesh healthy right now, and which repeaters or gateways need attention? The MeshCore radio mesh " +
				"around a place: how many nodes were heard recently, which are quiet, silent or have no activity on record and since when, " +
				"which did not answer the monitor that logs in to them, low batteries, and whether the Grid's own mesh feed and monitors are " +
				"reporting (if the feed is down, every node looks quiet and the summary says health is unknown). Also answers \"which nodes " +
				"are near/cover <place>\": pass the place and show=all. Defaults to the Ebbetts Pass area (SIERRA's own network); the feed " +
				"carries nodes far outside it. A town, address or lat,lng lists nodes within radius_km with distances; an area, county or " +
				"corridor lists the nodes located in it. There is no \"down\" state: the Grid only knows what it has heard. For one node's " +
				"detail and links use grid_mesh_node.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"place slug/id or name (e.g. \"ebbetts-pass\", \"Arnold\", \"calaveras-county\", \"hwy4-arnold-bearvalley\"), a street address, or \"lat,lng\". Default: ebbetts-pass"},"show":{"type":"string","description":"attention (default: only nodes with something to look at) | all (every node, nearest or most recently active first) | monitored (nodes an operator monitor logs in to) | gateways (the relay's gateway radios)"},"radius_km":{"type":"number","description":"for a town, address or lat,lng: how far out to look. Default 15, max 60"},"limit":{"type":"integer","description":"rows returned, default 40, max 100"}}}`),
			Handler:     s.handleMeshStatus,
		},
		{
			Name: "grid_mesh_node",
			Description: "Everything about one mesh node: who it is, when it was last heard and last relayed (with ages), what the monitor " +
				"that logs in to it last read (battery, temperature, noise floor, uptime, and how old that reading is), which gateway it is " +
				"read through, and what it hears: its links to neighbouring nodes with how much traffic each carried, the best SNR, a " +
				"strength band, and when each was last seen. Name the node by its name (\"SIERRA Forest Meadows\", or part of it), its short " +
				"id (the first 8 characters of its public key), a longer key prefix, the full key, or its event id (meshcore:…). If the name " +
				"matches several nodes the result lists them and reads none. Links are observed, not exhaustive: a neighbour that is not " +
				"listed was not observed. For a battery or temperature trend use grid_mesh_telemetry; for a route between two nodes or " +
				"places use grid_mesh_path.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"node":{"type":"string","description":"node name or part of it, short id (first 8 hex of its key), key prefix, full public key, or event id"},"window":{"type":"string","description":"how far back links count: 24h, 72h (default), 168h or 720h"},"limit":{"type":"integer","description":"links listed, most recently seen first. Default 15, max 50"}},"required":["node"]}`),
			Handler:     s.handleMeshNode,
		},
		{
			Name: "grid_mesh_path",
			Description: "Is there a path across the mesh from A to B, and through which nodes? A and B are each a node (name, short id, " +
				"key) or a place (\"Arnold\", \"Bear Valley\", a slug, \"lat,lng\"); a place stands for the nodes within radius_km of it. " +
				"Returns the route with the fewest hops over links the Grid has observed carrying traffic, each hop named, with each link's " +
				"traffic, best SNR, strength and last-seen time, the weakest link, and a second route that avoids the first one's relays " +
				"when one exists (none means a single point of failure in what was observed). This is an inference from observed links, " +
				"never a guarantee of delivery, and found=false means no path was observed, not that none exists: the result says which " +
				"end had no observed links. Each side reports how it was read (as a node or a place), so check that before relaying the answer.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"from":{"type":"string","description":"a node (name, short id, key, event id) or a place (name, slug, or \"lat,lng\")"},"to":{"type":"string","description":"a node or a place, as from"},"window":{"type":"string","description":"how far back links count: 24h, 72h (default), 168h or 720h. A longer window finds more paths over older evidence"},"radius_km":{"type":"number","description":"for a place: how far out its nodes are taken from. Default 15, max 60"}},"required":["from","to"]}`),
			Handler:     s.handleMeshPath,
		},
		{
			Name: "grid_mesh_telemetry",
			Description: "A node's battery, temperature and radio trend from the operator monitor that logs in to it: first, last, min, " +
				"max and change over the window, a short evenly spaced series, reboots, receive-error share, and the newest reading with " +
				"its age. Only nodes a monitor reads have telemetry (most do not): for the rest, and for a window with no readings, the " +
				"answer is \"unknown\" with what the archive does hold, never \"fine\". A reading is the node's state when it was read, " +
				"which can be hours or days ago. Name the node as in grid_mesh_node.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"node":{"type":"string","description":"node name, short id, key prefix, full key or event id"},"hours":{"type":"integer","description":"window ending now. Default 24, max 720 (30 days)"}},"required":["node"]}`),
			Handler:     s.handleMeshTelemetry,
		},
		{
			Name: "grid_mesh_changes",
			Description: "What changed on the mesh lately: nodes first recorded, nodes that expired (no advert or relay for about 14 h) or " +
				"came back, renames and moves, and a monitored node whose reads moved to a different gateway, each with its time, newest " +
				"first, plus which infrastructure nodes went quiet inside the window. Scoped to a place (default the Ebbetts Pass area) " +
				"and to the last 24 h unless hours says otherwise. It reports recorded changes only: signal readings and activity move " +
				"without a record, a node that was already silent before the window does not appear (use grid_mesh_status), and a gap in " +
				"the feed leaves a gap here.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"place slug/id or name, as grid_mesh_status. Default: ebbetts-pass"},"hours":{"type":"integer","description":"how far back. Default 24, max 168"},"radius_km":{"type":"number","description":"for a town, address or lat,lng. Default 15, max 60"}}}`),
			Handler:     s.handleMeshChanges,
		},
	}
}

/* ----------------------------------------------------------------- fetching */

// meshEvents reads every mesh node record in a place ("" = the whole feed),
// expired ones included: a repeater that stopped being heard is exactly what
// a health question is about.
func (s *Server) meshEvents(ctx context.Context, place string) ([]meshNode, error) {
	q := url.Values{"layer": {"mesh"}, "status": {"ACTIVE,EXPIRED"}, "geometry_format": {"NONE"}, "page_size": {"200"}}
	if place != "" {
		q.Set("place", place)
	}
	var out []meshNode
	for page := 0; page < 50; page++ {
		body, err := s.callAPIJSON(ctx, "/api/v1/events?"+q.Encode())
		if err != nil {
			return nil, err
		}
		for _, e := range asArray(body["events"]) {
			if ev, ok := e.(map[string]interface{}); ok {
				out = append(out, parseMeshNode(ev))
			}
		}
		next, _ := body["nextPageToken"].(string)
		if next == "" {
			return out, nil
		}
		q.Set("page_token", next)
	}
	return out, nil
}

// meshHealth reads the feed and monitor rows of /api/v1/sources: whether the
// Grid is hearing the mesh at all. Without it, a quiet mesh and a deaf Grid
// look the same.
func (s *Server) meshHealth(ctx context.Context, now time.Time, reporters map[string]bool) (map[string]interface{}, bool) {
	body, err := s.callAPIJSON(ctx, "/api/v1/sources")
	if err != nil {
		return map[string]interface{}{
			"mesh_feed": map[string]interface{}{"status": "UNKNOWN"},
			"note":      "Source health could not be read (" + err.Error() + "): whether the Grid is hearing the mesh is unknown, so node states below may be stale.",
		}, false
	}
	row := func(src map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{"id": src["id"], "name": src["name"], "status": src["status"]}
		if at := parseTime(src["lastSuccessAt"]); !at.IsZero() {
			out["last_success_at"] = stamp(at)
			out["last_success_ago"] = ageText(now.Sub(at))
		} else {
			out["last_success_at"] = nil
		}
		if e, _ := src["lastError"].(string); e != "" && src["status"] != "OK" {
			if len(e) > 160 {
				e = e[:160] + "…"
			}
			out["last_error"] = e
		}
		return out
	}
	out := map[string]interface{}{"mesh_feed": map[string]interface{}{"status": "UNKNOWN"}}
	feedOK := false
	monitors := []interface{}{}
	var notOK []string
	for _, v := range asArray(body["sources"]) {
		src, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := src["id"].(string)
		switch {
		case id == "meshcore":
			out["mesh_feed"] = row(src)
			feedOK = src["status"] == "OK"
		case src["kind"] == "REPORTER" && reporters[id]:
			monitors = append(monitors, row(src))
			if src["status"] != "OK" {
				notOK = append(notOK, id)
			}
		}
	}
	out["monitors"] = monitors
	var notes []string
	if !feedOK {
		notes = append(notes, "The Grid's mesh feed is not OK, so it may not be hearing the mesh: nodes can read quiet or silent because of the feed, not because of the nodes. Treat every state below as last known.")
	}
	if len(notOK) > 0 {
		notes = append(notes, "Not reporting: monitor "+strings.Join(notOK, ", ")+". Its readings below are the last it sent and say nothing about now.")
	}
	if len(notes) > 0 {
		out["note"] = strings.Join(notes, " ")
	}
	return out, feedOK
}

/* ---------------------------------------------------------------- resolving */

var (
	hexRun  = regexp.MustCompile(`^[0-9a-f]{4,64}$`)
	fullKey = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// keyRef returns ref as a bare lowercase key or key prefix, or "".
func keyRef(ref string) string {
	k := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ref)), "meshcore:")
	if hexRun.MatchString(k) {
		return k
	}
	return ""
}

// looksLikeKeyPrefix: a single run of hex that could be the start of a public
// key. The search index holds a key as one word, and matches the start of a
// word only for a query still being typed, so a key prefix is sent that way.
func looksLikeKeyPrefix(q string) bool { return keyRef(q) != "" && !fullKey.MatchString(keyRef(q)) }

// unresolvedNode is the standard result when a node argument names no node,
// or more than one. It is a result, not an error: "which of these did you
// mean" is an answer a model can act on.
func unresolvedNode(ref string, candidates []interface{}) map[string]interface{} {
	out := map[string]interface{}{"resolved": false}
	if len(candidates) > 0 {
		out["message"] = fmt.Sprintf("%q matches %d mesh nodes. Nothing was read. Call again with one node's short_id or full name.", ref, len(candidates))
		out["candidates"] = candidates
		return out
	}
	out["message"] = fmt.Sprintf("no mesh node on record matches %q. That is not evidence about any radio: the Grid lists only nodes whose adverts reached it. "+
		"Try part of the name, the first 8 characters of its public key, or grid_mesh_status show=all to list the nodes of a place.", ref)
	return out
}

// resolveNode turns what a person calls a node into its record. unresolved is
// non-nil when the reference matched nothing or several nodes.
func (s *Server) resolveNode(ctx context.Context, ref string) (node *meshNode, unresolved map[string]interface{}, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil, fmt.Errorf("node is required (a name, a short id, a key prefix or an event id)")
	}
	if k := keyRef(ref); fullKey.MatchString(k) {
		body, code, err := s.callAPI(ctx, "/api/v1/events/"+url.PathEscape("meshcore:"+k))
		if err != nil {
			return nil, nil, err
		}
		if code == 404 {
			return nil, unresolvedNode(ref, nil), nil
		}
		if code != 200 {
			return nil, nil, fmt.Errorf("node lookup failed (HTTP %d)", code)
		}
		n := parseMeshNode(body)
		return &n, nil, nil
	}
	// The finder searches every status, so a node that expired last week
	// still resolves. q_typing lets the last word be unfinished ("pinebr",
	// or the start of a key).
	q := url.Values{"q": {ref}, "kind": {"MESH_NODE"}, "limit": {"20"}, "q_typing": {"true"}}
	body, err := s.callAPIJSON(ctx, "/api/v1/search?"+q.Encode())
	if err != nil {
		return nil, nil, fmt.Errorf("node search unavailable: %w", err)
	}
	if un := asArray(body["unavailable"]); len(un) > 0 {
		return nil, nil, fmt.Errorf("node search is unavailable right now (the index is being rebuilt); pass the node's full public key, or try again shortly")
	}
	type hit struct {
		id, name, kind, status string
	}
	var hits []hit
	prefix := keyRef(ref)
	for _, h := range asArray(body["hits"]) {
		m, ok := h.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		title, _ := m["title"].(string)
		ev, _ := m["event"].(map[string]interface{})
		kind, _ := ev["category"].(string)
		status, _ := ev["status"].(string)
		// The finder titles a companion "Name (companion)".
		name := strings.TrimSuffix(title, " ("+kind+")")
		hits = append(hits, hit{id, name, kind, status})
	}
	pick := func(match func(hit) bool) []hit {
		var out []hit
		for _, h := range hits {
			if match(h) {
				out = append(out, h)
			}
		}
		return out
	}
	chosen := hits
	if prefix != "" {
		// A run of hex is a key first; only if no key starts with it is it a
		// name ("cafe").
		if byKey := pick(func(h hit) bool { return strings.HasPrefix(strings.TrimPrefix(h.id, "meshcore:"), prefix) }); len(byKey) > 0 {
			chosen = byKey
		}
	}
	if len(chosen) > 1 {
		if exact := pick(func(h hit) bool { return strings.EqualFold(h.name, ref) }); len(exact) == 1 {
			chosen = exact
		}
	}
	switch len(chosen) {
	case 0:
		return nil, unresolvedNode(ref, nil), nil
	case 1:
		return s.resolveNode(ctx, strings.TrimPrefix(chosen[0].id, "meshcore:"))
	}
	candidates := make([]interface{}, 0, len(chosen))
	for _, h := range chosen {
		candidates = append(candidates, map[string]interface{}{
			"name": h.name, "short_id": shortID(strings.TrimPrefix(h.id, "meshcore:")), "type": h.kind, "record": h.status,
		})
	}
	return nil, unresolvedNode(ref, candidates), nil
}

// meshScope is the set of nodes a place-shaped question is about.
type meshScope struct {
	// describe goes into the result, so the answer says what it covers.
	describe map[string]interface{}
	nodes    []meshNode
	// distance from the scope's point, by key; nil for a place with an area.
	distance map[string]float64
	// place is the slug for history queries, "" for a bare point; placeID is
	// its namespaced id, set for the default scope.
	place, placeID string
}

// scopeNodes resolves a location to the mesh nodes in or near it. An area,
// county or corridor is the nodes located in it. A town is a point in the
// place directory, and so is an address or a lat,lng: those take the nodes
// within radiusKm, nearest first. unresolved is the standard not-a-place
// result.
func (s *Server) scopeNodes(ctx context.Context, location string, radiusKm float64, geocode bool) (*meshScope, map[string]interface{}, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return s.defaultScope(ctx)
	}
	if radiusKm <= 0 {
		radiusKm = defaultMeshRadiusKm
	}
	if radiusKm > maxMeshRadiusKm {
		radiusKm = maxMeshRadiusKm
	}
	near := func(lat, lng float64, name, slug string) (*meshScope, map[string]interface{}, error) {
		all, err := s.meshEvents(ctx, "")
		if err != nil {
			return nil, nil, err
		}
		sc := &meshScope{distance: map[string]float64{}, place: slug}
		for _, n := range all {
			if !n.HasLocation {
				continue
			}
			if d := distanceKm(lat, lng, n.Lat, n.Lng); d <= radiusKm {
				sc.nodes = append(sc.nodes, n)
				sc.distance[n.Key] = d
			}
		}
		sort.SliceStable(sc.nodes, func(i, j int) bool { return sc.distance[sc.nodes[i].Key] < sc.distance[sc.nodes[j].Key] })
		sc.describe = map[string]interface{}{
			"kind": "near", "name": name, "radius_km": radiusKm,
			"covers": fmt.Sprintf("Mesh nodes that advertise a position within %g km of %s. Nodes that share no position are not included.", radiusKm, name),
		}
		if slug != "" {
			sc.describe["place"] = slug
		}
		return sc, nil, nil
	}
	if lat, lng, ok := parseLatLng(location); ok {
		la, _ := strconv.ParseFloat(lat, 64)
		ln, _ := strconv.ParseFloat(lng, 64)
		return near(la, ln, lat+","+lng, "")
	}
	var slug string
	if geocode {
		var err error
		if slug, _, err = s.resolvePlace(ctx, location); err != nil {
			return nil, nil, err
		}
	} else {
		slug = s.lookupPlace(ctx, location)
	}
	if slug == "" {
		return nil, unresolved(location), nil
	}
	place, err := s.callAPIJSON(ctx, "/api/v1/places/"+url.PathEscape(slug))
	if err != nil {
		return nil, nil, fmt.Errorf("place %q: %w", slug, err)
	}
	name, _ := place["name"].(string)
	if name == "" {
		name = slug
	}
	if lat, lng, ok := pointPlace(place); ok {
		return near(lat, lng, name, slug)
	}
	nodes, err := s.meshEvents(ctx, slug)
	if err != nil {
		return nil, nil, err
	}
	return &meshScope{
		nodes: nodes, place: slug,
		describe: map[string]interface{}{
			"kind": "in", "place": slug, "name": name,
			"covers": "Mesh nodes that advertise a position inside " + name + ". Nodes that share no position are not included.",
		},
	}, nil, nil
}

// defaultScope is what a question that names no place is about: SIERRA's own
// network. That is the nodes located in the Ebbetts Pass area, plus every
// node an operator monitor logs in to wherever it is: the relay's gateways
// sit at volunteers' houses, some outside the area and some sharing no
// position, and "is a gateway not answering" must not depend on where it is.
func (s *Server) defaultScope(ctx context.Context) (*meshScope, map[string]interface{}, error) {
	place, err := s.callAPIJSON(ctx, "/api/v1/places/"+defaultMeshPlace)
	if err != nil {
		return nil, nil, fmt.Errorf("place %q: %w", defaultMeshPlace, err)
	}
	id, _ := place["id"].(string)
	name, _ := place["name"].(string)
	all, err := s.meshEvents(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	sc := &meshScope{place: defaultMeshPlace, placeID: id, describe: map[string]interface{}{
		"kind": "default", "place": defaultMeshPlace, "name": name,
		"covers": "SIERRA's network: mesh nodes that advertise a position inside " + name +
			", plus every node an operator monitor logs in to, wherever it is. The feed carries hundreds of other nodes across Northern California; pass a location to look elsewhere.",
	}}
	for _, n := range all {
		in := false
		for _, p := range n.PlaceIDs {
			in = in || p == id
		}
		if in || n.Admin != nil || n.Reachability == "REACHABLE" || n.Reachability == "UNREACHABLE" {
			sc.nodes = append(sc.nodes, n)
		}
	}
	return sc, nil, nil
}

// lookupPlace is resolvePlace without the address geocoder: a slug or a
// directory name, or "". For arguments that may equally be a node's name,
// where a ten-second geocode of "SIERRA Arnold Summit" helps nobody.
func (s *Server) lookupPlace(ctx context.Context, location string) string {
	if !strings.ContainsAny(location, " ,") {
		if body, code, err := s.callAPI(ctx, "/api/v1/places/"+url.PathEscape(location)); err == nil && code == 200 {
			if slug := argStr(body, "slug"); slug != "" {
				return slug
			}
		}
	}
	for _, term := range nameTerms(location) {
		body, code, err := s.callAPI(ctx, "/api/v1/places?"+url.Values{"q": {term}}.Encode())
		if err != nil || code != 200 {
			continue
		}
		if slug := firstSlug(asArray(body["places"])); slug != "" {
			return slug
		}
	}
	return ""
}

// pointPlace reports whether a place is a single point (a town or a site),
// and where.
func pointPlace(place map[string]interface{}) (lat, lng float64, ok bool) {
	g, _ := place["geometry"].(map[string]interface{})
	if g == nil {
		g, _ = place["location"].(map[string]interface{})
	}
	b, _ := g["bbox"].(map[string]interface{})
	c, _ := g["centroid"].(map[string]interface{})
	if b == nil || c == nil {
		return 0, 0, false
	}
	if b["minLat"] != b["maxLat"] || b["minLng"] != b["maxLng"] {
		return 0, 0, false
	}
	lat, okLat := c["lat"].(float64)
	lng, okLng := c["lng"].(float64)
	return lat, lng, okLat && okLng
}

func argRadius(args map[string]interface{}) float64 {
	r, _ := argFloat(args, "radius_km")
	return r
}

func meshWindow(args map[string]interface{}) (string, error) {
	w := argStr(args, "window")
	if w == "" {
		return "72h", nil
	}
	if _, err := time.ParseDuration(w); err != nil {
		return "", fmt.Errorf("window %q is not a duration: use 24h, 72h, 168h or 720h", w)
	}
	return w, nil
}

/* ------------------------------------------------------------- grid_mesh_status */

func (s *Server) handleMeshStatus(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	show := strings.ToLower(argStr(args, "show"))
	switch show {
	case "":
		show = "attention"
	case "attention", "all", "monitored", "gateways":
	default:
		return nil, fmt.Errorf("show %q is not one of attention, all, monitored, gateways", show)
	}
	scope, unres, err := s.scopeNodes(ctx, argStr(args, "location"), argRadius(args), true)
	if err != nil {
		return nil, err
	}
	if unres != nil {
		return unres, nil
	}
	now := s.now()
	reporters := map[string]bool{}
	for _, n := range scope.nodes {
		if r, _ := n.Admin["reporterId"].(string); r != "" {
			reporters[r] = true
		}
	}
	health, feedOK := s.meshHealth(ctx, now, reporters)

	byState := map[string]int{stateActive: 0, stateQuiet: 0, stateSilent: 0, stateUnknown: 0}
	var infra, infraActive, monitored, noAnswer, gateways, handhelds, longSilent int
	names := map[string]string{}
	for _, n := range scope.nodes {
		names[n.Key] = n.label()
	}
	nameOf := func(key string) string {
		if v, ok := names[key]; ok {
			return v
		}
		return "node " + shortID(key)
	}
	type scored struct {
		node  meshNode
		alarm bool
		n     int
	}
	var attention []scored
	for _, n := range scope.nodes {
		state := n.activityState(now)
		byState[state]++
		if n.infrastructure() {
			infra++
			if state == stateActive {
				infraActive++
			}
		} else {
			handhelds++
		}
		if n.isGateway() {
			gateways++
		}
		if m := n.monitorState(now); m != "" {
			monitored++
			if m == monitorNoAnswer {
				noAnswer++
			}
		}
		if !n.infrastructure() {
			continue // a handheld that went home is not a fault
		}
		is := n.issues(now)
		if len(is) == 0 {
			continue
		}
		// A node nobody monitors that has been gone for weeks, or was never
		// heard, is old news: count it, list it only on request.
		at, _ := n.lastActive(now)
		if n.monitorState(now) == "" && (at.IsZero() || now.Sub(at) > meshLongSilentAfter) {
			longSilent++
			continue
		}
		sc := scored{node: n, n: len(is)}
		for _, x := range is {
			sc.alarm = sc.alarm || x.alarm
		}
		attention = append(attention, sc)
	}
	sort.SliceStable(attention, func(i, j int) bool {
		a, b := attention[i], attention[j]
		if a.alarm != b.alarm {
			return a.alarm
		}
		if am, bm := a.node.monitorState(now) != "", b.node.monitorState(now) != ""; am != bm {
			return am
		}
		if a.n != b.n {
			return a.n > b.n
		}
		return a.node.label() < b.node.label()
	})

	var listed []meshNode
	switch show {
	case "attention":
		for _, a := range attention {
			listed = append(listed, a.node)
		}
	case "monitored":
		for _, n := range scope.nodes {
			if n.monitorState(now) != "" {
				listed = append(listed, n)
			}
		}
	case "gateways":
		for _, n := range scope.nodes {
			if n.isGateway() {
				listed = append(listed, n)
			}
		}
	default:
		listed = append(listed, scope.nodes...)
		if scope.distance == nil {
			// No point to measure from: most recently active first, the
			// never-heard last.
			sort.SliceStable(listed, func(i, j int) bool {
				a, _ := listed[i].lastActive(now)
				b, _ := listed[j].lastActive(now)
				return a.After(b)
			})
		}
	}
	limit := argInt(args, "limit")
	if limit <= 0 {
		limit = defaultMeshRows
	}
	if limit > maxMeshRows {
		limit = maxMeshRows
	}
	rows := make([]interface{}, 0, len(listed))
	for i, n := range listed {
		if i == limit {
			break
		}
		row := n.row(now)
		if m := n.monitor(now, nameOf); m != nil {
			row["monitor"] = m
		}
		if d, ok := scope.distance[n.Key]; ok {
			row["distance_km"] = round(d, 1)
		}
		rows = append(rows, row)
	}

	summary := map[string]interface{}{
		"nodes":                 len(scope.nodes),
		"by_state":              byState,
		"infrastructure":        infra,
		"infrastructure_active": infraActive,
		"handhelds":             handhelds,
		"gateways":              gateways,
		"monitored":             monitored,
		"monitor_no_answer":     noAnswer,
		"need_attention":        len(attention),
		"long_silent_unlisted":  longSilent,
	}
	switch {
	case !feedOK:
		summary["health"] = "unknown"
		summary["text"] = "Health is UNKNOWN: the Grid's mesh feed is not reporting, so it cannot tell a quiet mesh from its own deafness. The node states are the last it knew."
	case len(scope.nodes) == 0:
		summary["health"] = "unknown"
		summary["text"] = "No mesh nodes are on record here. That is not a statement about radio coverage: only nodes whose adverts reach the Grid, and that share a position, are listed."
	case len(attention) > 0:
		summary["health"] = "problems_seen"
		summary["text"] = fmt.Sprintf("%d of %d repeaters, room servers and gateways were heard or relayed in the last 12 h. %d need a look (listed under show=attention, worst first).",
			infraActive, infra, len(attention))
	default:
		summary["health"] = "no_problems_seen"
		summary["text"] = fmt.Sprintf("%d of %d repeaters, room servers and gateways were heard or relayed in the last 12 h and nothing stands out in the data. "+
			"That is not an all-clear: it says what the Grid heard, not that a message sent now would get through.", infraActive, infra)
	}
	if longSilent > 0 {
		summary["long_silent_note"] = fmt.Sprintf("%d unmonitored nodes not heard for over 14 days, or never, are counted but not listed under attention; show=all lists them.", longSilent)
	}
	out := map[string]interface{}{
		"scope":       scope.describe,
		"as_of":       stamp(now),
		"data_health": health,
		"summary":     summary,
		"show":        show,
		"nodes":       rows,
		"count":       len(rows),
		"caveats":     []string{noteNotDown, noteCoverage},
	}
	if len(listed) > len(rows) {
		out["more"] = fmt.Sprintf("%d more not shown; raise limit (max %d).", len(listed)-len(rows), maxMeshRows)
	}
	return out, nil
}

/* --------------------------------------------------------------- grid_mesh_node */

func (s *Server) handleMeshNode(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	window, err := meshWindow(args)
	if err != nil {
		return nil, err
	}
	n, unres, err := s.resolveNode(ctx, argStr(args, "node"))
	if err != nil {
		return nil, err
	}
	if unres != nil {
		return unres, nil
	}
	now := s.now()
	out := map[string]interface{}{"resolved": true, "as_of": stamp(now)}
	node := n.row(now)
	node["id"] = n.ID
	if n.URL != "" {
		node["url"] = n.URL
	}
	if n.HasLocation {
		node["location"] = map[string]interface{}{"lat": n.Lat, "lng": n.Lng}
	}
	if len(n.PlaceIDs) > 0 {
		node["places"] = n.PlaceIDs
	}
	out["node"] = node

	// Links, with the names of the nodes at their other ends.
	names := map[string]string{n.Key: n.label()}
	nameOf := func(key string) string {
		if v, ok := names[key]; ok {
			return v
		}
		return "uncatalogued node " + shortID(key)
	}
	links := map[string]interface{}{"window": window, "note": noteLinks}
	if len(n.Key) == 64 {
		body, err := s.callAPIJSON(ctx, "/api/v1/mesh/links?"+url.Values{"node": {n.Key}, "window": {window}}.Encode())
		if err != nil {
			// The node's record stands without its links; say they are missing.
			links["unavailable"] = "links could not be read (" + err.Error() + "): unknown, not none"
		} else {
			refs, _ := body["nodes"].(map[string]interface{})
			for k, v := range refs {
				if ref, ok := v.(map[string]interface{}); ok {
					if name, _ := ref["name"].(string); name != "" {
						names[k] = name
					}
				}
			}
			all := asArray(body["links"])
			limit := argInt(args, "limit")
			if limit <= 0 {
				limit = 15
			}
			if limit > 50 {
				limit = 50
			}
			rows := []interface{}{}
			for i, l := range all {
				if i == limit {
					break
				}
				link, ok := l.(map[string]interface{})
				if !ok {
					continue
				}
				other, _ := link["a"].(string)
				if other == n.Key {
					other, _ = link["b"].(string)
				}
				row := linkRow(link, now)
				row["neighbour"] = nameOf(other)
				row["short_id"] = shortID(other)
				if ref, ok := refs[other].(map[string]interface{}); ok {
					row["type"] = ref["nodeType"]
					if ref["status"] != "ACTIVE" {
						row["record"] = ref["status"]
					}
					if km, ok := refDistanceKm(refs[n.Key], ref); ok {
						row["distance_km"] = km
					}
				} else {
					row["catalogued"] = false
				}
				rows = append(rows, row)
			}
			links["count"] = len(all)
			links["neighbours"] = rows
			if len(all) > len(rows) {
				links["more"] = fmt.Sprintf("%d more not shown; raise limit (max 50).", len(all)-len(rows))
			}
			if len(all) == 0 {
				links["none_observed"] = "No link to or from this node was observed in the window. " +
					"That is common for a node whose traffic the Grid cannot attribute, and is not evidence it hears nothing."
			}
		}
	} else {
		links["unavailable"] = "this node is known only by a key prefix (a monitor reports it; no advert has reached the Grid), so no links can be attributed to it"
	}
	out["links"] = links

	if m := n.monitor(now, nameOf); m != nil {
		node["monitor"] = m
	}
	if r := n.reading(now); r != nil {
		out["last_reading"] = r
	} else {
		out["last_reading"] = nil
		out["last_reading_note"] = "No operator monitor reads this node, so its battery, temperature and radio state are unknown (not fine, not bad). Most nodes are not monitored."
	}

	// Which gateways it is read through, and its monitor relations, named.
	if rel := s.meshRelated(ctx, n.ID); rel != nil {
		out["related"] = rel
	}
	if hist := s.meshTimeline(ctx, n.ID, time.Time{}, 6); hist != nil {
		out["recent_changes"] = hist
	}
	out["caveats"] = []string{noteNotDown}
	out["next"] = "grid_mesh_telemetry node=" + shortID(n.Key) + " for its battery and temperature trend; grid_mesh_path from/to for a route"
	return out, nil
}

// refDistanceKm is the distance between two nodes of a links or path
// response's nodes object, when both share a position. A hop of a hundred
// kilometres is worth seeing: it is either a mountaintop or a misattribution.
func refDistanceKm(a, b interface{}) (float64, bool) {
	at := func(v interface{}) (lat, lng float64, ok bool) {
		ref, _ := v.(map[string]interface{})
		loc, _ := ref["location"].(map[string]interface{})
		lat, okLat := loc["lat"].(float64)
		lng, okLng := loc["lng"].(float64)
		return lat, lng, okLat && okLng
	}
	lat1, lng1, ok1 := at(a)
	lat2, lng2, ok2 := at(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return round(distanceKm(lat1, lng1, lat2, lng2), 1), true
}

// linkRow is one link as a model reads it.
func linkRow(link map[string]interface{}, now time.Time) map[string]interface{} {
	row := map[string]interface{}{
		"observations": link["observations"],
		"days_active":  link["daysActive"],
	}
	if snr, ok := link["bestSnr"].(float64); ok {
		row["strength"] = linkStrength(snr)
		if snr != 0 {
			row["best_snr_db"] = snr
		}
	}
	if at := parseTime(link["lastSeen"]); !at.IsZero() {
		row["last_seen_at"] = stamp(at)
		row["last_seen_ago"] = ageText(now.Sub(at))
	}
	if at := parseTime(link["firstSeen"]); !at.IsZero() {
		row["first_seen_at"] = stamp(at)
	}
	return row
}

// meshRelated names the nodes the API relates this one to along monitor
// reads: its gateway, the repeaters read through the same gateway.
func (s *Server) meshRelated(ctx context.Context, id string) []interface{} {
	body, code, err := s.callAPI(ctx, "/api/v1/events/"+url.PathEscape(id)+"/related")
	if err != nil || code != 200 {
		return nil
	}
	var out []interface{}
	for _, r := range asArray(body["related"]) {
		m, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		ev, _ := m["event"].(map[string]interface{})
		if layer, _ := ev["layer"].(string); layer != "MESH" {
			continue
		}
		other := parseMeshNode(ev)
		out = append(out, map[string]interface{}{
			"name": other.label(), "short_id": shortID(other.Key), "type": other.Type,
			"relation": m["reason"],
		})
	}
	return out
}

// meshTimeline reads the recorded changes of one node: when the Grid first
// recorded it, when it expired and came back, renames, and read-path moves.
// since bounds them (zero = all); the newest max are returned, newest first.
// The activity entries are left out: the node's own clocks carry those.
func (s *Server) meshTimeline(ctx context.Context, id string, since time.Time, max int) []interface{} {
	body, code, err := s.callAPI(ctx, "/api/v1/events/"+url.PathEscape(id)+"/timeline")
	if err != nil || code != 200 {
		return nil
	}
	out := []interface{}{}
	entries := asArray(body["entries"])
	for i := len(entries) - 1; i >= 0 && len(out) < max; i-- {
		e, ok := entries[i].(map[string]interface{})
		if !ok {
			continue
		}
		kind, _ := e["kind"].(string)
		switch kind {
		case "TIMELINE_FIRST_RECORDED", "TIMELINE_STATUS", "TIMELINE_CHANGED", "TIMELINE_READ_PATH":
		default:
			continue
		}
		at := parseTime(e["at"])
		if at.IsZero() || (!since.IsZero() && at.Before(since)) {
			continue
		}
		row := map[string]interface{}{"at": stamp(at), "what": e["label"]}
		if kind == "TIMELINE_FIRST_RECORDED" {
			row["what"] = "First recorded by the Grid"
		} else if d, _ := e["detail"].(string); d != "" {
			row["detail"] = d
		}
		out = append(out, row)
	}
	return out
}

/* --------------------------------------------------------------- grid_mesh_path */

// pathSide is one end of a path question, as it was understood.
type pathSide struct {
	describe map[string]interface{}
	nodes    []meshNode
}

// resolveSide reads one end of a path: a key or short id is a node; a slug
// or directory name is a place (the nodes near or in it); anything else is
// tried as a node name. The result says which, because "Arnold" the town and
// "SIERRA Arnold Summit" the repeater are different questions.
func (s *Server) resolveSide(ctx context.Context, ref string, radiusKm float64, now time.Time) (*pathSide, map[string]interface{}, error) {
	ref = strings.TrimSpace(ref)
	asNode := func() (*pathSide, map[string]interface{}, error) {
		n, unres, err := s.resolveNode(ctx, ref)
		if err != nil || unres != nil {
			return nil, unres, err
		}
		return &pathSide{
			describe: map[string]interface{}{"input": ref, "read_as": "node", "name": n.label(), "short_id": shortID(n.Key)},
			nodes:    []meshNode{*n},
		}, nil, nil
	}
	if keyRef(ref) != "" {
		if side, unres, err := asNode(); err != nil || side != nil {
			return side, unres, err
		}
	}
	_, _, isPoint := parseLatLng(ref)
	if isPoint || s.lookupPlace(ctx, ref) != "" {
		scope, unres, err := s.scopeNodes(ctx, ref, radiusKm, false)
		if err != nil || unres != nil {
			return nil, unres, err
		}
		side := &pathSide{describe: map[string]interface{}{"input": ref, "read_as": "place"}}
		for k, v := range scope.describe {
			if k != "kind" {
				side.describe[k] = v
			}
		}
		// A place's nodes are the ones still part of the mesh: skip a
		// record that expired and has been silent past the long mark.
		used := []interface{}{}
		for _, n := range scope.nodes {
			if len(side.nodes) == maxPathSide {
				break
			}
			if len(n.Key) != 64 {
				continue
			}
			if at, _ := n.lastActive(now); at.IsZero() || now.Sub(at) > meshLongSilentAfter {
				continue
			}
			side.nodes = append(side.nodes, n)
			row := map[string]interface{}{"name": n.label(), "short_id": shortID(n.Key), "type": n.Type, "state": n.activityState(now)}
			if d, ok := scope.distance[n.Key]; ok {
				row["distance_km"] = round(d, 1)
			}
			used = append(used, row)
		}
		side.describe["nodes"] = used
		if len(side.nodes) == 0 {
			side.describe["problem"] = "No mesh node heard in the last 14 days is on record in or near this place, so there is nothing to route to or from. " + noteCoverage
		}
		return side, nil, nil
	}
	return asNode()
}

func (s *Server) handleMeshPath(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	window, err := meshWindow(args)
	if err != nil {
		return nil, err
	}
	fromRef, toRef := argStr(args, "from"), argStr(args, "to")
	if fromRef == "" || toRef == "" {
		return nil, fmt.Errorf("from and to are required (each a node or a place)")
	}
	now := s.now()
	sides := map[string]*pathSide{}
	for _, end := range [][2]string{{"from", fromRef}, {"to", toRef}} {
		name := end[0]
		side, unres, err := s.resolveSide(ctx, end[1], argRadius(args), now)
		if err != nil {
			return nil, err
		}
		if unres != nil {
			unres["argument"] = name
			unres["found"] = nil
			return unres, nil
		}
		sides[name] = side
	}
	out := map[string]interface{}{
		"as_of": stamp(now), "window": window,
		"from": sides["from"].describe, "to": sides["to"].describe,
	}
	if len(sides["from"].nodes) == 0 || len(sides["to"].nodes) == 0 {
		out["found"] = nil
		out["answer"] = "UNKNOWN: one side has no mesh node to route from or to (see its problem). No path was searched."
		return out, nil
	}
	keysOf := func(nodes []meshNode) string {
		ks := make([]string, len(nodes))
		for i, n := range nodes {
			ks[i] = n.Key
		}
		return strings.Join(ks, ",")
	}
	for _, name := range []string{"from", "to"} {
		for _, n := range sides[name].nodes {
			if len(n.Key) != 64 {
				out["found"] = nil
				out["answer"] = "UNKNOWN: " + n.label() + " is known only by a key prefix, so no links can be attributed to it."
				return out, nil
			}
		}
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/mesh/path?"+url.Values{
		"from": {keysOf(sides["from"].nodes)}, "to": {keysOf(sides["to"].nodes)}, "window": {window},
	}.Encode())
	if err != nil {
		return nil, fmt.Errorf("path search unavailable: %w", err)
	}
	refs, _ := body["nodes"].(map[string]interface{})
	nameOf := func(key string) string {
		if ref, ok := refs[key].(map[string]interface{}); ok {
			if name, _ := ref["name"].(string); name != "" {
				return name
			}
		}
		return "uncatalogued node " + shortID(key)
	}
	route := func(v interface{}) map[string]interface{} {
		p, ok := v.(map[string]interface{})
		if !ok {
			return nil
		}
		keys := asArray(p["nodes"])
		var names []string
		hops := []interface{}{}
		for _, k := range keys {
			key, _ := k.(string)
			names = append(names, nameOf(key))
			hop := map[string]interface{}{"name": nameOf(key), "short_id": shortID(key)}
			if ref, ok := refs[key].(map[string]interface{}); ok {
				n := meshNode{Key: key, Type: fmt.Sprint(ref["nodeType"]), Heard: parseTime(ref["lastHeardAt"]), Relayed: parseTime(ref["lastRelayedAt"])}
				hop["type"] = n.Type
				hop["state"] = n.activityState(now)
				if at, _ := n.lastActive(now); !at.IsZero() {
					hop["last_active_ago"] = ageText(now.Sub(at))
				}
				if ref["status"] != "ACTIVE" {
					hop["record"] = ref["status"]
				}
			} else {
				hop["catalogued"] = false
			}
			hops = append(hops, hop)
		}
		links := []interface{}{}
		var weakest map[string]interface{}
		var oldest time.Time
		for i, l := range asArray(p["links"]) {
			link, ok := l.(map[string]interface{})
			if !ok || i+1 >= len(keys) {
				continue
			}
			row := linkRow(link, now)
			a, _ := keys[i].(string)
			b, _ := keys[i+1].(string)
			row["between"] = nameOf(a) + " ↔ " + nameOf(b)
			if km, ok := refDistanceKm(refs[a], refs[b]); ok {
				row["distance_km"] = km
			}
			links = append(links, row)
			// The weakest link is the one seen longest ago: age is what
			// turns an observed link into a guess.
			if at := parseTime(link["lastSeen"]); weakest == nil || at.Before(oldest) {
				weakest, oldest = row, at
			}
		}
		r := map[string]interface{}{
			"hops": p["hops"], "route": strings.Join(names, " → "), "nodes": hops, "links": links,
		}
		if weakest != nil && len(links) > 1 {
			r["least_recent_link"] = weakest["between"]
		}
		return r
	}
	found, _ := body["found"].(bool)
	out["found"] = found
	out["basis"] = body["basis"]
	if p, _ := body["path"].(map[string]interface{}); found && p != nil {
		if hops, _ := p["hops"].(float64); hops == 0 {
			// One node stands on both sides: a node asked for a route to a
			// place it is in, or two places closer together than the radius.
			shared := ""
			if keys := asArray(p["nodes"]); len(keys) == 1 {
				key, _ := keys[0].(string)
				shared = nameOf(key)
			}
			out["found"] = nil
			out["answer"] = "NOTHING TO ROUTE: " + shared + " is on both sides (it is one of the nodes in or near each), so the two ends are the same radio as far as this search can tell. " +
				"That says nothing about coverage between them. Name two different nodes, or pass a smaller radius_km so the places do not overlap."
			return out, nil
		}
	}
	if found {
		out["path"] = route(body["path"])
		if alt := route(body["alternate"]); alt != nil {
			out["alternate"] = alt
		} else {
			out["alternate"] = nil
			out["alternate_note"] = "No second route avoiding the first one's relays was observed: on this evidence each relay on the path is a single point of failure. A missing link is not proof there is none."
		}
		out["answer"] = "A path was OBSERVED (" + fmt.Sprint(out["path"].(map[string]interface{})["route"]) +
			"). It is inferred from links that carried traffic within the window, not a guarantee a message sent now would be delivered: check each link's last_seen_ago and each node's state."
		return out, nil
	}
	// Not found: say which end had nothing to go on.
	counts, _ := body["linkCounts"].(map[string]interface{})
	isolated := func(nodes []meshNode) (int, []string) {
		linked := 0
		var without []string
		for _, n := range nodes {
			if c, _ := counts[n.Key].(float64); c > 0 {
				linked++
			} else {
				without = append(without, n.label())
			}
		}
		return linked, without
	}
	why := []string{}
	for _, name := range []string{"from", "to"} {
		linked, without := isolated(sides[name].nodes)
		sides[name].describe["nodes_with_observed_links"] = linked
		if linked == 0 {
			why = append(why, fmt.Sprintf("no link at all was observed for the %q side (%s) in the window", name, strings.Join(without, ", ")))
		}
	}
	if len(why) == 0 {
		why = append(why, "both sides have observed links, but no chain of them joins the two")
	}
	out["answer"] = "NO PATH OBSERVED, which is not the same as no path: " + strings.Join(why, "; ") +
		". The Grid learns links only from packets whose route it could resolve, so real links are missing. Try window=168h or 720h for older evidence, or grid_mesh_node on each end."
	return out, nil
}

/* ---------------------------------------------------------- grid_mesh_telemetry */

func (s *Server) handleMeshTelemetry(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	n, unres, err := s.resolveNode(ctx, argStr(args, "node"))
	if err != nil {
		return nil, err
	}
	if unres != nil {
		return unres, nil
	}
	hours := argInt(args, "hours")
	if hours <= 0 {
		hours = 24
	}
	if hours > 720 {
		hours = 720
	}
	now := s.now()
	from := now.Add(-time.Duration(hours) * time.Hour)
	body, err := s.callAPIJSON(ctx, "/api/v1/mesh/telemetry?"+url.Values{
		"node": {n.Key}, "from": {stamp(from)}, "to": {stamp(now)},
	}.Encode())
	if err != nil {
		return nil, fmt.Errorf("telemetry for %s unavailable: %w", n.label(), err)
	}
	out := map[string]interface{}{
		"resolved": true, "as_of": stamp(now),
		"node":   map[string]interface{}{"name": n.label(), "short_id": shortID(n.Key), "type": n.Type, "state": n.activityState(now)},
		"window": map[string]interface{}{"from": stamp(from), "to": stamp(now), "hours": hours},
	}
	coverage, _ := body["coverage"].(map[string]interface{})
	total, _ := coverage["samples"].(float64)
	archive := map[string]interface{}{"samples": int(total)}
	if at := parseTime(coverage["from"]); !at.IsZero() {
		archive["from"] = stamp(at)
	}
	if at := parseTime(coverage["to"]); !at.IsZero() {
		archive["to"] = stamp(at)
		archive["newest_ago"] = ageText(now.Sub(at))
	}
	out["archive"] = archive
	samples := asArray(body["samples"])
	out["samples"] = len(samples)
	if len(samples) == 0 {
		out["trend"] = nil
		switch {
		case total == 0 && n.monitorState(now) != "":
			out["answer"] = "UNKNOWN: a monitor is set to read this node but the archive holds no reading for it, so there is no trend. " +
				"grid_mesh_node shows the last reading the monitor reported, if any, and how old it is."
		case total == 0:
			out["answer"] = "UNKNOWN: no operator monitor reads this node, so the Grid holds no battery, temperature or radio telemetry for it. " +
				"That says nothing about its condition. Most mesh nodes are not monitored."
		default:
			out["answer"] = fmt.Sprintf("UNKNOWN for this window: no reading in the last %d h. The archive holds %d readings, the newest %s ago (%s). "+
				"The monitor has not read the node since, so its condition now is not known; widen hours to see the older trend.",
				hours, int(total), archive["newest_ago"], archive["to"])
		}
		return out, nil
	}
	series := map[string][]sample{}
	var errsFirst, errsLast, recvFirst, recvLast int64
	var haveCounters bool
	var newest map[string]interface{}
	var newestAt time.Time
	for _, v := range samples {
		m, _ := v.(map[string]interface{})
		r, _ := m["reading"].(map[string]interface{})
		at := parseTime(r["reportedAt"])
		if at.IsZero() {
			continue
		}
		for _, f := range []string{"batteryVolts", "temperatureC", "noiseFloorDbm"} {
			if x, ok := r[f].(float64); ok {
				series[f] = append(series[f], sample{at, x})
			}
		}
		if at.After(newestAt) {
			newest, newestAt = r, at
		}
		e, okE := int64Field(r["recvErrors"])
		p, okP := int64Field(r["packetsReceived"])
		if okE && okP {
			if !haveCounters {
				errsFirst, recvFirst, haveCounters = e, p, true
			}
			errsLast, recvLast = e, p
		}
	}
	trends := map[string]interface{}{}
	for _, f := range []struct {
		field, name string
		places      int
	}{{"batteryVolts", "battery_volts", 2}, {"temperatureC", "temperature_c", 1}, {"noiseFloorDbm", "noise_floor_dbm", 0}} {
		if t := trend(series[f.field], from, now, 12, f.places); t != nil {
			trends[f.name] = t
		} else {
			trends[f.name] = nil // the monitor did not read this gauge: unknown
		}
	}
	out["trend"] = trends
	reboots := asArray(body["reboots"])
	out["reboots"] = reboots
	if c, ok := int64Field(body["cadenceSeconds"]); ok && c > 0 {
		out["typical_gap_between_readings"] = ageText(time.Duration(c) * time.Second)
	}
	// Counters restart at a reboot, so a share across one would be nonsense.
	if haveCounters && len(reboots) == 0 && errsLast >= errsFirst && recvLast > recvFirst {
		e, p := float64(errsLast-errsFirst), float64(recvLast-recvFirst)
		out["receive_error_share_percent"] = round(100*e/(e+p), 1)
	}
	if truncated, _ := body["truncated"].(bool); truncated {
		out["truncated"] = "The window held more readings than one response carries; the trend covers the earliest part. Narrow hours."
	}
	latest := (meshNode{Admin: newest}).reading(now)
	if via, _ := newest["via"].(string); via != "" {
		latest["read_through_short_id"] = shortID(via)
	}
	if by, _ := newest["reporterId"].(string); by != "" {
		latest["by"] = by
	}
	out["latest"] = latest
	age := now.Sub(newestAt)
	if age > readingCurrentWithin/4 {
		out["answer"] = fmt.Sprintf("The newest reading is %s old (%s). The trend is real up to then; the node's condition since is unknown.", ageText(age), stamp(newestAt))
	} else {
		out["answer"] = fmt.Sprintf("%d readings in the last %d h, the newest %s ago. Values are what the monitor read at those times.", len(samples), hours, ageText(age))
	}
	return out, nil
}

/* ------------------------------------------------------------ grid_mesh_changes */

func (s *Server) handleMeshChanges(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	hours := argInt(args, "hours")
	if hours <= 0 {
		hours = 24
	}
	if hours > 168 {
		hours = 168
	}
	scope, unres, err := s.scopeNodes(ctx, argStr(args, "location"), argRadius(args), true)
	if err != nil {
		return nil, err
	}
	if unres != nil {
		return unres, nil
	}
	now := s.now()
	since := now.Add(-time.Duration(hours) * time.Hour)
	health, feedOK := s.meshHealth(ctx, now, nil)
	delete(health, "monitors")

	// A change is a revision, so the nodes that changed are the ones whose
	// record moved inside the window. Their timelines say what moved.
	inScope := map[string]meshNode{}
	for _, n := range scope.nodes {
		inScope[n.ID] = n
	}
	changed, complete, err := s.meshRevisedSince(ctx, scope, since, inScope)
	if err != nil {
		return nil, err
	}
	changes := []interface{}{}
	for _, id := range changed {
		n := inScope[id]
		for _, e := range s.meshTimeline(ctx, id, since, 20) {
			row := e.(map[string]interface{})
			row["node"] = n.label()
			row["short_id"] = shortID(n.Key)
			row["type"] = n.Type
			changes = append(changes, row)
		}
	}
	sort.SliceStable(changes, func(i, j int) bool {
		return fmt.Sprint(changes[i].(map[string]interface{})["at"]) > fmt.Sprint(changes[j].(map[string]interface{})["at"])
	})

	// Went quiet inside the window: last active after it began, and not for
	// 12 h since. No revision records that, the clocks do.
	wentQuiet := []interface{}{}
	for _, n := range scope.nodes {
		at, kind := n.lastActive(now)
		if !n.infrastructure() || at.IsZero() || at.Before(since) || now.Sub(at) <= meshQuietAfter {
			continue
		}
		wentQuiet = append(wentQuiet, map[string]interface{}{
			"node": n.label(), "short_id": shortID(n.Key), "type": n.Type,
			"last_active_at": stamp(at), "last_active_ago": ageText(now.Sub(at)), "last_active_was": kind,
		})
	}
	out := map[string]interface{}{
		"scope": scope.describe, "as_of": stamp(now),
		"window":      map[string]interface{}{"from": stamp(since), "to": stamp(now), "hours": hours},
		"data_health": health,
		"changes":     changes, "count": len(changes),
		"went_quiet": wentQuiet,
		"caveats": []string{
			"Only recorded changes are listed: a node first recorded, expired or active again, renamed or moved, or read through a different gateway. " +
				"Signal readings and activity clocks move without a record, and a node already silent before the window is not here (grid_mesh_status lists it).",
			noteNotDown,
		},
	}
	switch {
	case !feedOK:
		out["answer"] = "INCOMPLETE: the Grid's mesh feed is not reporting, so changes on the mesh since it stopped were not recorded. An empty or short list is not \"nothing changed\"."
	case len(changes) == 0 && len(wentQuiet) == 0:
		out["answer"] = fmt.Sprintf("No recorded change in the last %d h among the %d nodes in scope. That is the record, not an all-clear: see the caveats.", hours, len(scope.nodes))
	default:
		out["answer"] = fmt.Sprintf("%d recorded changes in the last %d h; %d infrastructure nodes went quiet inside the window.", len(changes), hours, len(wentQuiet))
	}
	if !complete {
		out["truncated"] = fmt.Sprintf("Only %d nodes' records were read; more nodes in scope may have changed. Narrow the place, the radius or the hours.", maxChangeNodes)
	}
	return out, nil
}

// meshRevisedSince lists the ids of in-scope nodes whose timeline is worth
// reading for changes since the given time, and whether the list is complete.
//
// A place with an area asks the store which of its events were revised in the
// window (/api/v1/history filters by place). Nodes the store cannot filter
// to, the ones near a point and the monitored nodes outside the default
// area, are few, so their timelines are simply read: paging the whole feed's
// revisions to find them would be thousands of rows for a handful of nodes.
func (s *Server) meshRevisedSince(ctx context.Context, scope *meshScope, since time.Time, inScope map[string]meshNode) ([]string, bool, error) {
	var ids []string
	seen := map[string]bool{}
	add := func(id string) bool {
		if _, ok := inScope[id]; !ok || seen[id] {
			return true
		}
		if len(ids) == maxChangeNodes {
			return false
		}
		seen[id] = true
		ids = append(ids, id)
		return true
	}
	if scope.describe["kind"] != "near" {
		q := url.Values{"layer": {"mesh"}, "place": {scope.place}, "from": {stamp(since)}, "geometry_format": {"NONE"}, "page_size": {"200"}}
		for page := 0; ; page++ {
			body, err := s.callAPIJSON(ctx, "/api/v1/history?"+q.Encode())
			if err != nil {
				return nil, false, err
			}
			for _, r := range asArray(body["revisions"]) {
				m, _ := r.(map[string]interface{})
				ev, _ := m["event"].(map[string]interface{})
				id, _ := ev["id"].(string)
				if !add(id) {
					return ids, false, nil
				}
			}
			next, _ := body["nextPageToken"].(string)
			if next == "" {
				break
			}
			if page == 9 {
				return ids, false, nil
			}
			q.Set("page_token", next)
		}
		if scope.describe["kind"] == "in" {
			return ids, true, nil
		}
	}
	// Near a point, or the default scope's nodes outside its area.
	placed := map[string]bool{}
	if scope.describe["kind"] == "default" {
		for _, n := range s.inPlace(scope) {
			placed[n.ID] = true
		}
	}
	for _, n := range scope.nodes {
		if placed[n.ID] {
			continue
		}
		if !add(n.ID) {
			return ids, false, nil
		}
	}
	return ids, true, nil
}

// inPlace is the nodes of a default scope that sit inside its area: the ones
// the store's place filter already covers.
func (s *Server) inPlace(scope *meshScope) []meshNode {
	var out []meshNode
	for _, n := range scope.nodes {
		for _, p := range n.PlaceIDs {
			if p == scope.placeID {
				out = append(out, n)
				break
			}
		}
	}
	return out
}
