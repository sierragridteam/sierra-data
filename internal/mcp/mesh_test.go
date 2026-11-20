package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// meshNow is the pinned clock of every mesh test.
var meshNow = time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)

func ago(d time.Duration) string { return meshNow.Add(-d).Format(time.RFC3339) }

// k pads a short label to a 64-character public key.
func k(label string) string { return label + strings.Repeat("0", 64-len(label)) }

// fixtureNode is one MESH event of the fake Grid, in the API's JSON.
type fixtureNode struct {
	key, name, kind, status string
	heard, relayed          string // RFC 3339 or ""
	reachability            string
	admin                   map[string]interface{}
	lat, lng                float64
	places                  []string
}

func (n fixtureNode) event() map[string]interface{} {
	tel := map[string]interface{}{"lastHeardAt": nil, "lastRelayedAt": nil, "admin": nil, "readVia": []interface{}{}}
	if n.heard != "" {
		tel["lastHeardAt"] = n.heard
	}
	if n.relayed != "" {
		tel["lastRelayedAt"] = n.relayed
	}
	if n.admin != nil {
		tel["admin"] = n.admin
	}
	reach := n.reachability
	if reach == "" {
		reach = "MESH_REACHABILITY_UNSPECIFIED"
	}
	status := n.status
	if status == "" {
		status = "ACTIVE"
	}
	ev := map[string]interface{}{
		"id": "meshcore:" + n.key, "layer": "MESH", "category": n.kind, "severity": "INFO", "status": status,
		"headline": n.name, "canonicalUrl": "https://corescope.example/#/nodes/" + n.key,
		// The record's own time is deliberately ancient: nothing may read it
		// as "last seen".
		"observedAt": "2026-08-01T00:00:00Z",
		"placeIds":   n.places,
		"mesh": map[string]interface{}{
			"publicKey": n.key, "nodeType": n.kind, "name": n.name, "telemetry": tel, "reachability": reach,
		},
	}
	if n.lat != 0 {
		ev["geometry"] = map[string]interface{}{
			"geojson":  "",
			"centroid": map[string]interface{}{"lat": n.lat, "lng": n.lng},
			"bbox":     map[string]interface{}{"minLat": n.lat, "maxLat": n.lat, "minLng": n.lng, "maxLng": n.lng},
		}
	}
	return ev
}

var area = []string{"area:ebbetts-pass", "county:calaveras-county"}

// meshFixture is a small mesh with one of each case the tools must tell
// apart. Keys are hex so they resolve as short ids.
func meshFixture() []fixtureNode {
	return []fixtureNode{
		// Relaying now, read by the relay half an hour ago: nothing to say.
		{key: k("a1"), name: "SIERRA Arnold Summit", kind: "repeater", relayed: ago(10 * time.Minute), heard: ago(20 * time.Hour),
			reachability: "REACHABLE", lat: 38.25, lng: -120.35, places: area,
			admin: map[string]interface{}{"reporterId": "relay", "reportedAt": ago(5 * time.Minute), "lastSuccessAt": ago(30 * time.Minute),
				"lastAttemptAt": ago(30 * time.Minute), "via": k("9a"), "batteryVolts": 4.05, "batteryPercent": nil,
				"temperatureC": 3.5, "noiseFloorDbm": -116.0, "uptimeSeconds": "432000"}},
		// Relaying now, but the monitor has not got in for six days.
		{key: k("b2"), name: "SIERRA Forest Meadows", kind: "repeater", relayed: ago(15 * time.Minute),
			reachability: "UNREACHABLE", lat: 38.16, lng: -120.41, places: area,
			admin: map[string]interface{}{"reporterId": "alanpi", "reportedAt": ago(time.Hour), "lastSuccessAt": ago(6 * 24 * time.Hour),
				"lastAttemptAt": ago(time.Hour), "via": "", "batteryVolts": 3.4}},
		// Not heard for three days, expired, nobody monitors it.
		{key: k("c3"), name: "SIERRA Lake Alpine", kind: "repeater", status: "EXPIRED", relayed: ago(72 * time.Hour),
			lat: 38.47, lng: -120.0, places: area},
		// Quiet for twenty hours.
		{key: k("d4"), name: "SIERRA Camp Connell", kind: "repeater", heard: ago(20 * time.Hour), lat: 38.32, lng: -120.26, places: area},
		// Never heard: no activity on record at all.
		{key: k("e5"), name: "SIERRA Doud Hill", kind: "repeater", status: "EXPIRED", lat: 38.2, lng: -120.3, places: area},
		// A gateway outside the area, sharing no position, read a minute ago.
		{key: k("9a"), name: "SIERRA Pinebrook Gtwy", kind: "companion", heard: ago(9 * time.Hour), reachability: "REACHABLE",
			admin: map[string]interface{}{"reporterId": "relay", "reportedAt": ago(time.Minute), "lastSuccessAt": ago(time.Minute),
				"lastAttemptAt": ago(time.Minute), "via": "", "batteryVolts": 4.17}},
		// Someone's handheld that went home days ago: not a fault.
		{key: k("f6"), name: "Hiker", kind: "companion", status: "EXPIRED", heard: ago(5 * 24 * time.Hour), lat: 38.26, lng: -120.33, places: area},
		// A stranger's repeater far away: in the feed, never in SIERRA's scope.
		{key: k("77"), name: "Bay Hilltop", kind: "repeater", relayed: ago(time.Minute), lat: 37.9, lng: -122.1},
		// Two nodes that share most of a name.
		{key: k("abcd01"), name: "SIERRA Pinebrook Tree", kind: "repeater", relayed: ago(2 * time.Hour), lat: 38.236, lng: -120.339, places: area},
		{key: k("abcd02"), name: "SIERRA Pinebrook Roof", kind: "repeater", relayed: ago(3 * time.Hour), lat: 38.236, lng: -120.338, places: area},
	}
}

// meshGrid is a fake /api/v1 for the mesh tools. It answers from nodes and
// records every request path, so a test can assert what was asked.
type meshGrid struct {
	nodes      []fixtureNode
	feedStatus string // of the meshcore source
	relayOK    bool
	links      map[string][]map[string]interface{} // by node key
	path       map[string]interface{}              // the /mesh/path body
	telemetry  map[string]map[string]interface{}   // by node key
	timelines  map[string][]map[string]interface{} // by event id
	revised    []string                            // event ids with a revision in the window
	searchDown bool
	requests   []string
}

func newMeshGrid() *meshGrid {
	return &meshGrid{nodes: meshFixture(), feedStatus: "OK", relayOK: true,
		links: map[string][]map[string]interface{}{}, telemetry: map[string]map[string]interface{}{},
		timelines: map[string][]map[string]interface{}{}}
}

func (g *meshGrid) node(key string) (fixtureNode, bool) {
	for _, n := range g.nodes {
		if n.key == key {
			return n, true
		}
	}
	return fixtureNode{}, false
}

// refs is the nodes object of a links or path response.
func (g *meshGrid) refs(keys ...string) map[string]interface{} {
	out := map[string]interface{}{}
	for _, key := range keys {
		n, ok := g.node(key)
		if !ok {
			continue // uncatalogued: absent, as the API leaves it
		}
		ev := n.event()
		tel := ev["mesh"].(map[string]interface{})["telemetry"].(map[string]interface{})
		ref := map[string]interface{}{"name": n.name, "nodeType": n.kind, "status": ev["status"],
			"lastHeardAt": tel["lastHeardAt"], "lastRelayedAt": tel["lastRelayedAt"], "location": nil}
		if n.lat != 0 {
			ref["location"] = map[string]interface{}{"lat": n.lat, "lng": n.lng}
		}
		out[key] = ref
	}
	return out
}

func (g *meshGrid) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.requests = append(g.requests, r.URL.RequestURI())
	w.Header().Set("Content-Type", "application/json")
	write := func(v interface{}) { _ = json.NewEncoder(w).Encode(v) }
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		write(map[string]interface{}{"code": 5, "message": "not found"})
	}
	q := r.URL.Query()
	path := r.URL.Path
	switch {
	case path == "/api/v1/events":
		events := []interface{}{}
		for _, n := range g.nodes {
			if place := q.Get("place"); place != "" && !strings.Contains(strings.Join(n.places, " "), ":"+place) {
				continue
			}
			events = append(events, n.event())
		}
		write(map[string]interface{}{"events": events, "nextPageToken": ""})
	case strings.HasPrefix(path, "/api/v1/events/"):
		rest := strings.TrimPrefix(path, "/api/v1/events/")
		id, sub, _ := strings.Cut(rest, "/")
		n, ok := g.node(strings.TrimPrefix(id, "meshcore:"))
		if !ok {
			notFound()
			return
		}
		switch sub {
		case "":
			write(n.event())
		case "related":
			related := []interface{}{}
			if via, _ := n.admin["via"].(string); via != "" {
				if gw, ok := g.node(via); ok {
					related = append(related, map[string]interface{}{"event": gw.event(), "rule": "mesh-monitor-gateway", "reason": "A gateway this node is read through"})
				}
			}
			write(map[string]interface{}{"eventId": id, "related": related})
		case "timeline":
			entries := []interface{}{}
			for _, e := range g.timelines[id] {
				entries = append(entries, e)
			}
			write(map[string]interface{}{"eventId": id, "entries": entries})
		default:
			notFound()
		}
	case path == "/api/v1/search":
		if g.searchDown {
			write(map[string]interface{}{"hits": []interface{}{}, "unavailable": []interface{}{"MESH_NODE"}})
			return
		}
		// The real finder: every word must match a word of the name or the
		// key; only a query still being typed (q_typing) matches by prefix.
		words := strings.Fields(strings.ToLower(q.Get("q")))
		typing := q.Get("q_typing") == "true"
		hits := []interface{}{}
		for _, n := range g.nodes {
			have := append(strings.Fields(strings.ToLower(n.name)), n.key)
			all := true
			for i, word := range words {
				found := false
				for _, h := range have {
					if h == word || (typing && i == len(words)-1 && strings.HasPrefix(h, word)) {
						found = true
					}
				}
				all = all && found
			}
			if !all {
				continue
			}
			title := n.name
			if n.kind == "companion" {
				title += " (companion)"
			}
			ev := n.event()
			hits = append(hits, map[string]interface{}{"kind": "MESH_NODE", "id": ev["id"], "title": title, "detail": "",
				"apiPath": "/api/v1/events/" + ev["id"].(string), "url": ev["canonicalUrl"],
				"event": map[string]interface{}{"layer": "MESH", "category": n.kind, "status": ev["status"]}})
		}
		write(map[string]interface{}{"hits": hits, "totals": []interface{}{}, "unavailable": []interface{}{}})
	case path == "/api/v1/sources":
		relay := "OK"
		if !g.relayOK {
			relay = "STALE"
		}
		write(map[string]interface{}{"sources": []interface{}{
			map[string]interface{}{"id": "meshcore", "name": "MeshCore Mesh", "kind": "FEED", "status": g.feedStatus, "lastSuccessAt": ago(time.Minute), "lastError": "broker not delivering"},
			map[string]interface{}{"id": "relay", "name": "SIERRA Mesh Relay", "kind": "REPORTER", "status": relay, "lastSuccessAt": ago(2 * time.Minute), "lastError": ""},
			map[string]interface{}{"id": "alanpi", "name": "SIERRA Dorrington MeshCore Gateway", "kind": "REPORTER", "status": "OK", "lastSuccessAt": ago(time.Minute), "lastError": ""},
			map[string]interface{}{"id": "burn-line", "name": "Burn line dialer", "kind": "REPORTER", "status": "OK", "lastSuccessAt": ago(time.Hour)},
			map[string]interface{}{"id": "nws", "name": "NWS", "kind": "FEED", "status": "OK"},
		}})
	case path == "/api/v1/places/ebbetts-pass":
		write(map[string]interface{}{"id": "area:ebbetts-pass", "slug": "ebbetts-pass", "kind": "AREA", "name": "Ebbetts Pass Corridor",
			"geometry": map[string]interface{}{"centroid": map[string]interface{}{"lat": 38.3, "lng": -120.2},
				"bbox": map[string]interface{}{"minLat": 38.0, "maxLat": 38.6, "minLng": -120.6, "maxLng": -119.8}}})
	case path == "/api/v1/places/arnold", path == "/api/v1/places/bearvalley":
		lat, lng, name := 38.265, -120.334, "Arnold, CA"
		if strings.HasSuffix(path, "bearvalley") {
			lat, lng, name = 38.461, -120.042, "Bear Valley, CA"
		}
		write(map[string]interface{}{"id": "town:" + strings.TrimPrefix(path, "/api/v1/places/"), "slug": strings.TrimPrefix(path, "/api/v1/places/"), "kind": "TOWN", "name": name,
			"geometry": map[string]interface{}{"centroid": map[string]interface{}{"lat": lat, "lng": lng},
				"bbox": map[string]interface{}{"minLat": lat, "maxLat": lat, "minLng": lng, "maxLng": lng}}})
	case path == "/api/v1/places":
		places := []interface{}{}
		switch strings.ToLower(q.Get("q")) {
		case "arnold":
			places = append(places, map[string]interface{}{"slug": "arnold", "kind": "TOWN", "name": "Arnold, CA"})
		case "bear valley":
			places = append(places, map[string]interface{}{"slug": "bearvalley", "kind": "TOWN", "name": "Bear Valley, CA"})
		}
		write(map[string]interface{}{"places": places})
	case path == "/api/v1/places:resolve":
		write(map[string]interface{}{"places": []interface{}{}})
	case path == "/api/v1/mesh/links":
		key := q.Get("node")
		links := []interface{}{}
		keys := []string{key}
		for _, l := range g.links[key] {
			links = append(links, l)
			keys = append(keys, l["a"].(string), l["b"].(string))
		}
		write(map[string]interface{}{"window": q.Get("window"), "generatedAt": meshNow.Format(time.RFC3339), "node": key, "links": links, "nodes": g.refs(keys...)})
	case path == "/api/v1/mesh/path":
		if g.path == nil {
			notFound()
			return
		}
		write(g.path)
	case path == "/api/v1/mesh/telemetry":
		if body, ok := g.telemetry[q.Get("node")]; ok {
			write(body)
			return
		}
		write(map[string]interface{}{"node": q.Get("node"), "coverage": map[string]interface{}{"from": nil, "to": nil, "samples": 0},
			"cadenceSeconds": nil, "reboots": []interface{}{}, "truncated": false, "samples": []interface{}{}})
	case path == "/api/v1/history":
		revs := []interface{}{}
		for _, id := range g.revised {
			revs = append(revs, map[string]interface{}{"revision": 2, "event": map[string]interface{}{"id": id}})
		}
		write(map[string]interface{}{"revisions": revs, "nextPageToken": ""})
	default:
		notFound()
	}
}

func (g *meshGrid) asked(substr string) bool {
	for _, r := range g.requests {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func meshServer(g *meshGrid) *Server {
	s := NewHandler(g)
	s.clock = func() time.Time { return meshNow }
	return s
}

func meshLink(a, b string, obs int, snr float64, lastSeen string) map[string]interface{} {
	return map[string]interface{}{"a": a, "b": b, "observations": obs, "daysActive": 3, "firstSeen": ago(70 * time.Hour), "lastSeen": lastSeen, "bestSnr": snr}
}

func rowsByName(t *testing.T, v interface{}) map[string]map[string]interface{} {
	t.Helper()
	out := map[string]map[string]interface{}{}
	for _, r := range v.([]interface{}) {
		row := r.(map[string]interface{})
		out[row["name"].(string)] = row
	}
	return out
}

var (
	rawKey   = regexp.MustCompile(`[0-9a-f]{64}`)
	downWord = regexp.MustCompile(`(?i)\b(is|are|was) (down|offline|dead)\b|"(state|health)": "(down|offline|healthy|ok|fine)"`)
)

// honest checks what every mesh result must hold to: no verdict of "down" or
// "healthy", and no bare 64-character key outside an id or a url.
func honest(t *testing.T, res map[string]interface{}) {
	t.Helper()
	text := prettyJSON(res)
	if m := downWord.FindString(text); m != "" {
		t.Errorf("result states %q: the Grid knows only what it heard\n%s", m, text)
	}
	for _, line := range strings.Split(text, "\n") {
		if rawKey.MatchString(line) && !strings.Contains(line, `"id"`) && !strings.Contains(line, `"url"`) {
			t.Errorf("a raw 64-character key outside id/url: %s", strings.TrimSpace(line))
		}
	}
}

/* ----------------------------------------------------------- grid_mesh_status */

func TestMeshStatus_DefaultScopeAndStates(t *testing.T) {
	g := newMeshGrid()
	s := meshServer(g)
	res := structured(t, s, "grid_mesh_status", `{"show":"all"}`)
	honest(t, res)

	scope := res["scope"].(map[string]interface{})
	if scope["kind"] != "default" || !strings.Contains(scope["covers"].(string), "SIERRA's network") {
		t.Errorf("scope = %v", scope)
	}
	rows := rowsByName(t, res["nodes"])
	if _, ok := rows["Bay Hilltop"]; ok {
		t.Error("a stranger's repeater outside the area is not SIERRA's network")
	}
	// The gateway shares no position and is in no place: it is in scope
	// because a monitor reads it.
	gw := rows["SIERRA Pinebrook Gtwy"]
	if gw == nil || gw["role"] != "gateway" {
		t.Fatalf("gateway row = %v", gw)
	}
	// Its own advert was 9 h ago, but it answered a monitor a minute ago.
	if gw["state"] != stateActive || gw["last_active_was"] != "answered a monitor" || gw["last_active_ago"] != "1 min" {
		t.Errorf("gateway activity = %v / %v / %v", gw["state"], gw["last_active_was"], gw["last_active_ago"])
	}
	if m := gw["monitor"].(map[string]interface{}); m["state"] != monitorAnswering {
		t.Errorf("gateway monitor = %v", m)
	}
	for name, want := range map[string]string{
		"SIERRA Arnold Summit": stateActive, "SIERRA Forest Meadows": stateActive,
		"SIERRA Lake Alpine": stateSilent, "SIERRA Camp Connell": stateQuiet, "SIERRA Doud Hill": stateUnknown,
	} {
		if got := rows[name]["state"]; got != want {
			t.Errorf("%s state = %v, want %s", name, got, want)
		}
	}
	// Relaying now and yet not answering its monitor: both are said, and the
	// first does not cancel the second.
	fm := rows["SIERRA Forest Meadows"]
	if m := fm["monitor"].(map[string]interface{}); m["state"] != monitorNoAnswer || m["last_read_ago"] != "6 days" || m["battery_is_current"] != false {
		t.Errorf("Forest Meadows monitor = %v", m)
	}
	issues := strings.Join(toStrings(fm["issues"]), " ")
	if !strings.Contains(issues, "Did not answer the alanpi monitor; last answered 6 days ago") {
		t.Errorf("issues = %q", issues)
	}
	if strings.Contains(issues, "Battery low") {
		t.Error("a six-day-old 3.4 V reading must not be reported as today's battery")
	}
	// No activity on record is unknown: a null time, never an age or a zero.
	doud := rows["SIERRA Doud Hill"]
	if v, ok := doud["last_active_at"]; !ok || v != nil {
		t.Errorf("Doud Hill last_active_at = %v, want an explicit null", v)
	}
	if !strings.Contains(strings.Join(toStrings(doud["issues"]), " "), "Unknown, not down") {
		t.Errorf("Doud Hill issues = %v", doud["issues"])
	}
	// The record's observedAt (August) is not a time anything was heard.
	if strings.Contains(prettyJSON(res), "2026-08-01") {
		t.Error("observedAt leaked into a mesh result as if it were activity")
	}
	// The read path is named, never a key.
	if m := rows["SIERRA Arnold Summit"]["monitor"].(map[string]interface{}); m["read_through"] != "SIERRA Pinebrook Gtwy" {
		t.Errorf("read_through = %v", m["read_through"])
	}

	sum := res["summary"].(map[string]interface{})
	if sum["health"] != "problems_seen" {
		t.Errorf("health = %v", sum["health"])
	}
	if sum["nodes"] != float64(9) || sum["handhelds"] != float64(1) || sum["gateways"] != float64(1) {
		t.Errorf("summary = %v", sum)
	}
	if len(res["caveats"].([]interface{})) == 0 {
		t.Error("caveats missing")
	}
	// Only the feed and the monitors these nodes name: not burn-line, not nws.
	health := prettyJSON(res["data_health"])
	if strings.Contains(health, "burn-line") || strings.Contains(health, "nws") || !strings.Contains(health, "alanpi") {
		t.Errorf("data_health = %s", health)
	}
}

func toStrings(v interface{}) []string {
	var out []string
	for _, x := range asArray(v) {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func TestMeshStatus_AttentionListsInfrastructureWorstFirst(t *testing.T) {
	s := meshServer(newMeshGrid())
	res := structured(t, s, "grid_mesh_status", `{}`)
	honest(t, res)
	var names []string
	for _, r := range res["nodes"].([]interface{}) {
		names = append(names, r.(map[string]interface{})["name"].(string))
	}
	got := strings.Join(names, ", ")
	// Lake Alpine (silent 3 days) is an alarm; Forest Meadows (monitored, no
	// answer) leads the rest; the handheld and the never-heard, unmonitored
	// Doud Hill are not listed.
	if got != "SIERRA Lake Alpine, SIERRA Forest Meadows, SIERRA Camp Connell" {
		t.Errorf("attention = %s", got)
	}
	sum := res["summary"].(map[string]interface{})
	if sum["long_silent_unlisted"] != float64(1) || !strings.Contains(sum["long_silent_note"].(string), "show=all") {
		t.Errorf("long silent = %v / %v", sum["long_silent_unlisted"], sum["long_silent_note"])
	}
}

// A feed that is not reporting makes every state the last the Grid knew. The
// summary must say unknown, not count the quiet nodes as problems or the
// active ones as fine.
func TestMeshStatus_FeedDownIsUnknown(t *testing.T) {
	g := newMeshGrid()
	g.feedStatus = "UNAVAILABLE"
	g.relayOK = false
	res := structured(t, meshServer(g), "grid_mesh_status", `{}`)
	honest(t, res)
	sum := res["summary"].(map[string]interface{})
	if sum["health"] != "unknown" || !strings.Contains(sum["text"].(string), "UNKNOWN") {
		t.Errorf("summary = %v", sum)
	}
	health := res["data_health"].(map[string]interface{})
	note := health["note"].(string)
	if !strings.Contains(note, "mesh feed is not OK") || !strings.Contains(note, "monitor relay") {
		t.Errorf("note = %q", note)
	}
	if feed := health["mesh_feed"].(map[string]interface{}); feed["status"] != "UNAVAILABLE" || feed["last_error"] == nil {
		t.Errorf("mesh_feed = %v", feed)
	}
}

// Nothing standing out is still not an all-clear, and the data says so.
func TestMeshStatus_NoProblemsIsNotAnAllClear(t *testing.T) {
	g := newMeshGrid()
	g.nodes = g.nodes[:1] // Arnold Summit alone: active, answering
	res := structured(t, meshServer(g), "grid_mesh_status", `{}`)
	honest(t, res)
	sum := res["summary"].(map[string]interface{})
	if sum["health"] != "no_problems_seen" || !strings.Contains(sum["text"].(string), "not an all-clear") {
		t.Errorf("summary = %v", sum)
	}
	if res["count"] != float64(0) {
		t.Errorf("attention rows = %v", res["nodes"])
	}

	// And no nodes at all is unknown, not healthy.
	g.nodes = nil
	sum = structured(t, meshServer(g), "grid_mesh_status", `{}`)["summary"].(map[string]interface{})
	if sum["health"] != "unknown" || !strings.Contains(sum["text"].(string), "not a statement about radio coverage") {
		t.Errorf("empty summary = %v", sum)
	}
}

// A town is a point: its nodes are the ones within reach, nearest first, with
// distances. This is "which nodes cover Arnold".
func TestMeshStatus_NearATown(t *testing.T) {
	g := newMeshGrid()
	res := structured(t, meshServer(g), "grid_mesh_status", `{"location":"Arnold","show":"all","radius_km":8}`)
	honest(t, res)
	scope := res["scope"].(map[string]interface{})
	if scope["kind"] != "near" || scope["place"] != "arnold" || scope["radius_km"] != float64(8) {
		t.Errorf("scope = %v", scope)
	}
	rows := res["nodes"].([]interface{})
	if len(rows) == 0 {
		t.Fatal("no nodes near Arnold")
	}
	last := -1.0
	for _, r := range rows {
		row := r.(map[string]interface{})
		d := row["distance_km"].(float64)
		if d < last || d > 8 {
			t.Errorf("%v at %v km: not nearest first within 8 km", row["name"], d)
		}
		last = d
		if row["name"] == "SIERRA Lake Alpine" || row["name"] == "SIERRA Pinebrook Gtwy" {
			t.Errorf("%v is not within 8 km of Arnold (or shares no position)", row["name"])
		}
	}
	if !strings.Contains(strings.Join(toStrings(res["caveats"]), " "), "Radio coverage is not modelled") {
		t.Error("coverage caveat missing")
	}
	// lat,lng works the same, without a place.
	res = structured(t, meshServer(g), "grid_mesh_status", `{"location":"38.265,-120.334","show":"all"}`)
	if res["scope"].(map[string]interface{})["kind"] != "near" || res["count"] == float64(0) {
		t.Errorf("lat,lng scope = %v, count %v", res["scope"], res["count"])
	}
}

func TestMeshStatus_UnresolvedPlaceAndBadShow(t *testing.T) {
	s := meshServer(newMeshGrid())
	res := structured(t, s, "grid_mesh_status", `{"location":"Timbuktu"}`)
	if res["resolved"] != false {
		t.Errorf("unresolved place = %v", res)
	}
	out := call(t, s, "tools/call", `{"name":"grid_mesh_status","arguments":{"show":"broken"}}`)
	if out["isError"] != true {
		t.Error("a bad show value must be a tool error")
	}
}

/* ------------------------------------------------------------- grid_mesh_node */

func TestMeshNode_ResolvesByNameShortIDPrefixAndKey(t *testing.T) {
	for _, ref := range []string{
		"SIERRA Forest Meadows", "forest meadows", "forest mead", // a name, part of it, a word still being typed
		"b2000000", "b200", "B2000000000000", // short id, shorter and longer key prefixes, any case
		k("b2"), "meshcore:" + k("b2"), // the key and the event id
	} {
		g := newMeshGrid()
		res := structured(t, meshServer(g), "grid_mesh_node", `{"node":"`+ref+`"}`)
		if res["resolved"] != true {
			t.Errorf("%q did not resolve: %v", ref, res["message"])
			continue
		}
		node := res["node"].(map[string]interface{})
		if node["name"] != "SIERRA Forest Meadows" || node["short_id"] != "b2000000" || node["id"] != "meshcore:"+k("b2") {
			t.Errorf("%q resolved to %v", ref, node)
		}
	}
}

// The finder matches the start of a key only for a query still being typed:
// without q_typing a short id finds nothing. This pins that the tools send it.
func TestMeshNode_KeyPrefixSearchIsSentAsTyping(t *testing.T) {
	g := newMeshGrid()
	structured(t, meshServer(g), "grid_mesh_node", `{"node":"b2000000"}`)
	if !g.asked("q_typing=true") {
		t.Errorf("node search did not ask for prefix matching: %v", g.requests)
	}
	// grid_search and grid_events promise "the start of its public key" too.
	for tool, args := range map[string]string{"grid_search": `{"q":"b2000000"}`, "grid_events": `{"q":"b2000000","layer":"mesh"}`, "search": `{"query":"b2000000"}`} {
		g = newMeshGrid()
		structured(t, meshServer(g), tool, args)
		if !g.asked("q_typing=true") {
			t.Errorf("%s did not send a key prefix as a typed query: %v", tool, g.requests)
		}
	}
	// A plain word is not a key prefix.
	g = newMeshGrid()
	structured(t, meshServer(g), "grid_search", `{"q":"pinebrook"}`)
	if g.asked("q_typing") {
		t.Error("a name search must not be sent as typing")
	}
}

func TestMeshNode_AmbiguousAndUnknown(t *testing.T) {
	g := newMeshGrid()
	s := meshServer(g)
	// Three nodes are called Pinebrook: read none, list them.
	res := structured(t, s, "grid_mesh_node", `{"node":"pinebrook"}`)
	honest(t, res)
	if res["resolved"] != false || res["node"] != nil {
		t.Fatalf("ambiguous name was resolved: %v", res)
	}
	cands := res["candidates"].([]interface{})
	if len(cands) != 3 {
		t.Fatalf("candidates = %v", cands)
	}
	for _, c := range cands {
		row := c.(map[string]interface{})
		if row["name"] == "" || row["short_id"] == "" || strings.Contains(row["name"].(string), "(companion)") {
			t.Errorf("candidate = %v", row)
		}
	}
	if g.asked("/api/v1/mesh/links") {
		t.Error("links were read for an unresolved node")
	}
	// A short id shared by two keys is ambiguous too.
	res = structured(t, s, "grid_mesh_node", `{"node":"abcd"}`)
	if res["resolved"] != false || len(res["candidates"].([]interface{})) != 2 {
		t.Errorf("shared key prefix = %v", res)
	}
	// An exact name wins over a longer one that contains it.
	g.nodes = append(g.nodes, fixtureNode{key: k("abcd03"), name: "SIERRA Pinebrook", kind: "repeater"})
	res = structured(t, meshServer(g), "grid_mesh_node", `{"node":"sierra pinebrook"}`)
	if res["resolved"] != true || res["node"].(map[string]interface{})["short_id"] != "abcd0300" {
		t.Errorf("exact name = %v", res["node"])
	}

	// Nothing matches: a result that says so, and says it is not evidence.
	for _, ref := range []string{"No Such Node", "ffffffff", k("ff")} {
		res = structured(t, meshServer(newMeshGrid()), "grid_mesh_node", `{"node":"`+ref+`"}`)
		if res["resolved"] != false || !strings.Contains(res["message"].(string), "not evidence about any radio") {
			t.Errorf("%q = %v", ref, res)
		}
	}
	// A missing argument is a tool error.
	if out := call(t, s, "tools/call", `{"name":"grid_mesh_node","arguments":{}}`); out["isError"] != true {
		t.Error("node is required")
	}
	// The finder being rebuilt is an error, never "no such node".
	g = newMeshGrid()
	g.searchDown = true
	if out := call(t, meshServer(g), "tools/call", `{"name":"grid_mesh_node","arguments":{"node":"forest"}}`); out["isError"] != true {
		t.Error("an unavailable finder must be an error, not an empty answer")
	}
}

func TestMeshNode_LinksAreNamedAndReadingsAged(t *testing.T) {
	g := newMeshGrid()
	stranger := k("5e")
	g.links[k("b2")] = []map[string]interface{}{
		meshLink(k("a1"), k("b2"), 208, 13, ago(49*time.Minute)),
		meshLink(k("b2"), k("c3"), 4, 1.5, ago(60*time.Hour)),
		meshLink(stranger, k("b2"), 1, 0, ago(30*time.Hour)),
	}
	res := structured(t, meshServer(g), "grid_mesh_node", `{"node":"forest meadows","window":"168h"}`)
	honest(t, res)
	if !g.asked("window=168h") || !g.asked("node="+k("b2")) {
		t.Errorf("links request = %v", g.requests)
	}
	links := res["links"].(map[string]interface{})
	if links["count"] != float64(3) || !strings.Contains(links["note"].(string), "not proof there is none") {
		t.Errorf("links = %v", links)
	}
	rows := links["neighbours"].([]interface{})
	first := rows[0].(map[string]interface{})
	if first["neighbour"] != "SIERRA Arnold Summit" || first["short_id"] != "a1000000" || first["strength"] != "strong" ||
		first["last_seen_ago"] != "49 min" || first["observations"] != float64(208) || first["distance_km"] == nil {
		t.Errorf("first link = %v", first)
	}
	second := rows[1].(map[string]interface{})
	if second["strength"] != "weak" || second["record"] != "EXPIRED" || second["last_seen_ago"] != "3 days" {
		t.Errorf("second link = %v", second)
	}
	// A relay the Grid keeps no record of: named as such, by short id, and no
	// SNR invented from the API's 0.
	third := rows[2].(map[string]interface{})
	if third["neighbour"] != "uncatalogued node 5e000000" || third["catalogued"] != false || third["strength"] != "unmeasured" {
		t.Errorf("third link = %v", third)
	}
	if _, ok := third["best_snr_db"]; ok {
		t.Error("an unmeasured SNR was reported as a number")
	}
	// The reading is six days old: it is given, with its age, and flagged.
	reading := res["last_reading"].(map[string]interface{})
	if reading["is_current"] != false || reading["read_ago"] != "6 days" || !strings.Contains(reading["note"].(string), "then, not now") {
		t.Errorf("reading = %v", reading)
	}
	// A gauge the monitor did not read is null, not 0.
	if v, ok := reading["temperature_c"]; !ok || v != nil {
		t.Errorf("temperature_c = %v, want an explicit null", v)
	}
}

func TestMeshNode_UnmonitoredAndUnlinked(t *testing.T) {
	g := newMeshGrid()
	g.timelines["meshcore:"+k("d4")] = []map[string]interface{}{
		{"at": "2026-09-01T00:00:00Z", "kind": "TIMELINE_FIRST_RECORDED", "label": "First recorded by the Grid", "detail": "12 changes since"},
		{"at": ago(30 * time.Hour), "kind": "TIMELINE_STATUS", "label": "Expired", "detail": "was Active"},
		{"at": ago(26 * time.Hour), "kind": "TIMELINE_STATUS", "label": "Active again", "detail": "was Expired"},
		{"at": ago(20 * time.Hour), "kind": "TIMELINE_ACTIVITY", "label": "Last heard"},
	}
	res := structured(t, meshServer(g), "grid_mesh_node", `{"node":"camp connell"}`)
	honest(t, res)
	if res["last_reading"] != nil || !strings.Contains(res["last_reading_note"].(string), "unknown (not fine, not bad)") {
		t.Errorf("unmonitored reading = %v / %v", res["last_reading"], res["last_reading_note"])
	}
	links := res["links"].(map[string]interface{})
	if links["count"] != float64(0) || !strings.Contains(links["none_observed"].(string), "not evidence it hears nothing") {
		t.Errorf("no links = %v", links)
	}
	if _, ok := res["node"].(map[string]interface{})["monitor"]; ok {
		t.Error("an unmonitored node has no monitor block")
	}
	// Recorded changes, newest first, without the activity entries.
	changes := res["recent_changes"].([]interface{})
	if len(changes) != 3 || changes[0].(map[string]interface{})["what"] != "Active again" {
		t.Errorf("recent_changes = %v", changes)
	}
}

/* ------------------------------------------------------------- grid_mesh_path */

func pathBody(g *meshGrid, found bool, path, alt []string, counts map[string]int) map[string]interface{} {
	route := func(keys []string) interface{} {
		if keys == nil {
			return nil
		}
		links := []interface{}{}
		for i := 1; i < len(keys); i++ {
			links = append(links, meshLink(keys[i-1], keys[i], 50*i, 9, ago(time.Duration(i)*5*time.Hour)))
		}
		nodes := []interface{}{}
		for _, key := range keys {
			nodes = append(nodes, key)
		}
		return map[string]interface{}{"hops": len(keys) - 1, "nodes": nodes, "links": links}
	}
	lc := map[string]interface{}{}
	for key, n := range counts {
		lc[key] = n
	}
	return map[string]interface{}{"window": "72h", "found": found, "path": route(path), "alternate": route(alt),
		"linkCounts": lc, "nodes": g.refs(append(append([]string{}, path...), alt...)...),
		"basis": "Inferred from links the Grid observed. Not a guarantee."}
}

func TestMeshPath_BetweenPlaces(t *testing.T) {
	g := newMeshGrid()
	stranger := k("5e")
	g.path = pathBody(g, true, []string{k("abcd01"), k("d4"), k("c3")}, []string{k("a1"), stranger, k("c3")}, nil)
	res := structured(t, meshServer(g), "grid_mesh_path", `{"from":"Arnold","to":"Bear Valley"}`)
	honest(t, res)
	from, to := res["from"].(map[string]interface{}), res["to"].(map[string]interface{})
	if from["read_as"] != "place" || from["place"] != "arnold" || to["read_as"] != "place" || to["place"] != "bearvalley" {
		t.Errorf("sides = %v / %v", from, to)
	}
	// The place's nodes go to the API as keys: nearest first, without the
	// never-heard Doud Hill and without the node that shares no position.
	var req string
	for _, r := range g.requests {
		if strings.HasPrefix(r, "/api/v1/mesh/path") {
			req = r
		}
	}
	if !strings.Contains(req, "from="+k("f6")) || strings.Contains(req, k("e5")) || strings.Contains(req, k("9a")) || !strings.Contains(req, "to="+k("c3")) {
		t.Errorf("path request = %s", req)
	}
	if res["found"] != true {
		t.Fatalf("found = %v", res["found"])
	}
	path := res["path"].(map[string]interface{})
	if path["route"] != "SIERRA Pinebrook Tree → SIERRA Camp Connell → SIERRA Lake Alpine" || path["hops"] != float64(2) {
		t.Errorf("path = %v", path)
	}
	hops := path["nodes"].([]interface{})
	if last := hops[2].(map[string]interface{}); last["state"] != stateSilent || last["record"] != "EXPIRED" {
		t.Errorf("the silent, expired end of the path must say so: %v", last)
	}
	link := path["links"].([]interface{})[1].(map[string]interface{})
	if link["between"] != "SIERRA Camp Connell ↔ SIERRA Lake Alpine" || link["last_seen_ago"] != "10 h" || link["distance_km"] == nil {
		t.Errorf("link = %v", link)
	}
	if path["least_recent_link"] != "SIERRA Camp Connell ↔ SIERRA Lake Alpine" {
		t.Errorf("least_recent_link = %v", path["least_recent_link"])
	}
	alt := res["alternate"].(map[string]interface{})
	if alt["route"] != "SIERRA Arnold Summit → uncatalogued node 5e000000 → SIERRA Lake Alpine" {
		t.Errorf("alternate = %v", alt["route"])
	}
	answer := res["answer"].(string)
	if !strings.Contains(answer, "OBSERVED") || !strings.Contains(answer, "not a guarantee") || res["basis"] == nil {
		t.Errorf("answer = %q basis = %v", answer, res["basis"])
	}
}

func TestMeshPath_NoPathObservedIsNotNoPath(t *testing.T) {
	g := newMeshGrid()
	g.path = pathBody(g, false, nil, nil, map[string]int{k("b2"): 22, k("c3"): 0})
	res := structured(t, meshServer(g), "grid_mesh_path", `{"from":"SIERRA Forest Meadows","to":"c3000000","window":"24h"}`)
	honest(t, res)
	if res["found"] != false || res["path"] != nil {
		t.Errorf("found = %v path = %v", res["found"], res["path"])
	}
	answer := res["answer"].(string)
	for _, want := range []string{"NO PATH OBSERVED, which is not the same as no path", `no link at all was observed for the "to" side (SIERRA Lake Alpine)`, "window=168h"} {
		if !strings.Contains(answer, want) {
			t.Errorf("answer %q lacks %q", answer, want)
		}
	}
	if from := res["from"].(map[string]interface{}); from["read_as"] != "node" || from["name"] != "SIERRA Forest Meadows" || from["nodes_with_observed_links"] != float64(1) {
		t.Errorf("from = %v", from)
	}
	if !g.asked("window=24h") {
		t.Error("window was not passed on")
	}
	// A found path with no second route says what that means.
	g.path = pathBody(g, true, []string{k("b2"), k("a1")}, nil, nil)
	res = structured(t, meshServer(g), "grid_mesh_path", `{"from":"forest meadows","to":"arnold summit"}`)
	if res["alternate"] != nil || !strings.Contains(res["alternate_note"].(string), "single point of failure") {
		t.Errorf("alternate = %v / %v", res["alternate"], res["alternate_note"])
	}
}

func TestMeshPath_UnresolvableSides(t *testing.T) {
	g := newMeshGrid()
	s := meshServer(g)
	// A node that does not exist: the standard unresolved result, naming the side.
	res := structured(t, s, "grid_mesh_path", `{"from":"forest meadows","to":"Nowhere Repeater"}`)
	if res["resolved"] != false || res["argument"] != "to" || res["found"] != nil {
		t.Errorf("unresolved side = %v", res)
	}
	// An ambiguous node name lists the candidates and searches nothing.
	res = structured(t, s, "grid_mesh_path", `{"from":"pinebrook","to":"forest meadows"}`)
	if res["resolved"] != false || res["argument"] != "from" || len(res["candidates"].([]interface{})) != 3 {
		t.Errorf("ambiguous side = %v", res)
	}
	if g.asked("/api/v1/mesh/path") {
		t.Error("a path was searched with an unresolved side")
	}
	// A place with no node heard lately: unknown, not "no path".
	g.nodes = []fixtureNode{g.nodes[0]}
	res = structured(t, meshServer(g), "grid_mesh_path", `{"from":"arnold summit","to":"Bear Valley"}`)
	if res["found"] != nil || !strings.HasPrefix(res["answer"].(string), "UNKNOWN") {
		t.Errorf("empty side = %v", res)
	}
	if to := res["to"].(map[string]interface{}); !strings.Contains(to["problem"].(string), "No mesh node heard in the last 14 days") {
		t.Errorf("to = %v", to)
	}
	// One node on both sides is not a path of zero hops, it is no question.
	g = newMeshGrid()
	g.path = pathBody(g, true, []string{k("abcd01")}, nil, nil)
	res = structured(t, meshServer(g), "grid_mesh_path", `{"from":"SIERRA Pinebrook Tree","to":"Arnold"}`)
	honest(t, res)
	if res["found"] != nil || !strings.HasPrefix(res["answer"].(string), "NOTHING TO ROUTE: SIERRA Pinebrook Tree is on both sides") || res["path"] != nil {
		t.Errorf("shared node = %v", res)
	}
	if out := call(t, s, "tools/call", `{"name":"grid_mesh_path","arguments":{"from":"arnold"}}`); out["isError"] != true {
		t.Error("to is required")
	}
}

/* -------------------------------------------------------- grid_mesh_telemetry */

func telemetryBody(g *meshGrid, key string, n int, newest time.Duration, total int) {
	samples := []interface{}{}
	for i := n - 1; i >= 0; i-- {
		at := newest + time.Duration(i)*30*time.Minute
		reading := map[string]interface{}{
			"reporterId": "relay", "reportedAt": ago(at), "lastSuccessAt": nil, "via": k("9a"),
			"batteryVolts": 4.10 - 0.01*float64(n-1-i), "temperatureC": nil, "noiseFloorDbm": -116.0,
			"uptimeSeconds": fmt.Sprint(100000 - int(at.Seconds())), "recvErrors": fmt.Sprint(100 + 2*(n-1-i)), "packetsReceived": fmt.Sprint(1000 + 98*(n-1-i)),
		}
		samples = append(samples, map[string]interface{}{"reading": reading, "receivedAt": ago(at)})
	}
	g.telemetry[key] = map[string]interface{}{"node": key,
		"coverage":       map[string]interface{}{"from": ago(30 * 24 * time.Hour), "to": ago(newest), "samples": total},
		"cadenceSeconds": "1800", "reboots": []interface{}{}, "truncated": false, "samples": samples}
}

func TestMeshTelemetry_Trend(t *testing.T) {
	g := newMeshGrid()
	telemetryBody(g, k("a1"), 20, 20*time.Minute, 900)
	res := structured(t, meshServer(g), "grid_mesh_telemetry", `{"node":"arnold summit","hours":12}`)
	honest(t, res)
	if !g.asked("from=" + strings.ReplaceAll(ago(12*time.Hour), ":", "%3A")) {
		t.Errorf("window not passed: %v", g.requests)
	}
	trends := res["trend"].(map[string]interface{})
	b := trends["battery_volts"].(map[string]interface{})
	if b["first"] != 4.10 || b["last"] != 3.91 || b["change"] != -0.19 || b["min"] != 3.91 || b["samples"] != float64(20) {
		t.Errorf("battery = %v", b)
	}
	if series := b["series"].([]interface{}); len(series) == 0 || len(series) > 12 {
		t.Errorf("series has %d points", len(series))
	}
	// The monitor never read a temperature: null, not a flat line at 0.
	if v, ok := trends["temperature_c"]; !ok || v != nil {
		t.Errorf("temperature trend = %v, want null", v)
	}
	if res["receive_error_share_percent"] != 2.0 {
		t.Errorf("error share = %v", res["receive_error_share_percent"])
	}
	latest := res["latest"].(map[string]interface{})
	if latest["read_ago"] != "20 min" || fmt.Sprintf("%.2f", latest["battery_volts"]) != "3.91" || latest["read_through_short_id"] != "9a000000" {
		t.Errorf("latest = %v", latest)
	}
	if res["typical_gap_between_readings"] != "30 min" {
		t.Errorf("cadence = %v", res["typical_gap_between_readings"])
	}
}

// Readings that stopped two days ago are a trend up to then and nothing
// about now. And a window with none is unknown, with what the archive holds.
func TestMeshTelemetry_StaleAndMissingAreUnknown(t *testing.T) {
	g := newMeshGrid()
	telemetryBody(g, k("b2"), 6, 50*time.Hour, 291)
	res := structured(t, meshServer(g), "grid_mesh_telemetry", `{"node":"forest meadows","hours":72}`)
	honest(t, res)
	if a := res["answer"].(string); !strings.Contains(a, "The newest reading is 2 days old") || !strings.Contains(a, "condition since is unknown") {
		t.Errorf("stale answer = %q", a)
	}
	if latest := res["latest"].(map[string]interface{}); latest["is_current"] != false {
		t.Errorf("latest = %v", latest)
	}

	// No sample in the window, but the archive has older ones.
	telemetryBody(g, k("b2"), 0, 50*time.Hour, 291)
	res = structured(t, meshServer(g), "grid_mesh_telemetry", `{"node":"forest meadows"}`)
	honest(t, res)
	if a := res["answer"].(string); !strings.HasPrefix(a, "UNKNOWN for this window") || !strings.Contains(a, "291 readings, the newest 2 days ago") {
		t.Errorf("empty-window answer = %q", a)
	}
	if res["trend"] != nil {
		t.Errorf("trend = %v, want null", res["trend"])
	}

	// A node no monitor reads: unknown, never "fine".
	res = structured(t, meshServer(newMeshGrid()), "grid_mesh_telemetry", `{"node":"camp connell"}`)
	honest(t, res)
	if a := res["answer"].(string); !strings.Contains(a, "UNKNOWN: no operator monitor reads this node") || !strings.Contains(a, "says nothing about its condition") {
		t.Errorf("unmonitored answer = %q", a)
	}
	// A monitored node whose archive is empty is a different unknown.
	res = structured(t, meshServer(newMeshGrid()), "grid_mesh_telemetry", `{"node":"arnold summit"}`)
	if a := res["answer"].(string); !strings.Contains(a, "a monitor is set to read this node but the archive holds no reading") {
		t.Errorf("monitored-empty answer = %q", a)
	}
	// And a node that does not resolve reads nothing.
	g = newMeshGrid()
	res = structured(t, meshServer(g), "grid_mesh_telemetry", `{"node":"No Such Node"}`)
	if res["resolved"] != false || g.asked("/api/v1/mesh/telemetry") {
		t.Errorf("unresolved telemetry = %v", res)
	}
}

/* ---------------------------------------------------------- grid_mesh_changes */

func TestMeshChanges(t *testing.T) {
	g := newMeshGrid()
	lake, arnold := "meshcore:"+k("c3"), "meshcore:"+k("a1")
	g.revised = []string{lake, arnold, "meshcore:" + k("77")} // the stranger's change is out of scope
	g.timelines[lake] = []map[string]interface{}{
		{"at": "2026-08-01T00:00:00Z", "kind": "TIMELINE_FIRST_RECORDED", "label": "First recorded by the Grid"},
		{"at": ago(40 * time.Hour), "kind": "TIMELINE_STATUS", "label": "Active again", "detail": "was Expired"}, // before the window
		{"at": ago(10 * time.Hour), "kind": "TIMELINE_STATUS", "label": "Expired", "detail": "was Active"},
	}
	g.timelines[arnold] = []map[string]interface{}{
		{"at": ago(3 * time.Hour), "kind": "TIMELINE_READ_PATH", "label": "Read through SIERRA Pinebrook Gtwy", "detail": "relay"},
		{"at": ago(10 * time.Minute), "kind": "TIMELINE_ACTIVITY", "label": "Last relayed"},
	}
	res := structured(t, meshServer(g), "grid_mesh_changes", `{}`)
	honest(t, res)
	changes := res["changes"].([]interface{})
	if len(changes) != 2 {
		t.Fatalf("changes = %v", changes)
	}
	first, second := changes[0].(map[string]interface{}), changes[1].(map[string]interface{})
	if first["node"] != "SIERRA Arnold Summit" || first["what"] != "Read through SIERRA Pinebrook Gtwy" || first["at"] != ago(3*time.Hour) {
		t.Errorf("first = %v", first)
	}
	if second["node"] != "SIERRA Lake Alpine" || second["what"] != "Expired" || second["short_id"] != "c3000000" {
		t.Errorf("second = %v", second)
	}
	// Camp Connell was last heard 20 h ago: inside the window, and quiet since.
	quiet := res["went_quiet"].([]interface{})
	if len(quiet) != 1 || quiet[0].(map[string]interface{})["node"] != "SIERRA Camp Connell" {
		t.Errorf("went_quiet = %v", quiet)
	}
	if w := res["window"].(map[string]interface{}); w["hours"] != float64(24) || w["from"] != ago(24*time.Hour) {
		t.Errorf("window = %v", w)
	}

	// Nothing recorded is the record, not an all-clear.
	g = newMeshGrid()
	g.nodes = g.nodes[:1]
	res = structured(t, meshServer(g), "grid_mesh_changes", `{"hours":6}`)
	if a := res["answer"].(string); !strings.Contains(a, "No recorded change") || !strings.Contains(a, "not an all-clear") {
		t.Errorf("quiet answer = %q", a)
	}
	// With the feed down, an empty list is a gap.
	g.feedStatus = "STALE"
	res = structured(t, meshServer(g), "grid_mesh_changes", `{}`)
	if a := res["answer"].(string); !strings.HasPrefix(a, "INCOMPLETE") {
		t.Errorf("feed-down answer = %q", a)
	}
}

/* ----------------------------------------------------------- generic tools */

func TestGenericToolsKnowMeshNodes(t *testing.T) {
	g := newMeshGrid()
	s := meshServer(g)
	hits := structured(t, s, "grid_search", `{"q":"forest meadows"}`)["hits"].([]interface{})
	if len(hits) != 1 || hits[0].(map[string]interface{})["next"] != "grid_mesh_node node=meshcore:"+k("b2") {
		t.Errorf("grid_search hit = %v", hits)
	}
	results := structured(t, s, "search", `{"query":"forest meadows"}`)["results"].([]interface{})
	if len(results) != 1 {
		t.Fatalf("search results = %v", results)
	}
	id := results[0].(map[string]interface{})["id"].(string)
	doc := structured(t, s, "fetch", `{"id":"`+id+`"}`)
	text := doc["text"].(string)
	for _, want := range []string{"SIERRA Forest Meadows (MeshCore repeater, short id b2000000)", "Last activity: relayed 15 min ago", "Monitor: no answer; last read 6 days ago", "silence is not proof a node is off"} {
		if !strings.Contains(text, want) {
			t.Errorf("fetch text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Observed:") || strings.Contains(text, "2026-08-01") {
		t.Errorf("fetch presents the record time as if it were activity:\n%s", text)
	}
	if doc["metadata"].(map[string]interface{})["kind"] != "mesh_node" {
		t.Errorf("metadata = %v", doc["metadata"])
	}
}

func TestMeshToolsAreListedAndDescribed(t *testing.T) {
	s := meshServer(newMeshGrid())
	listed := map[string]map[string]interface{}{}
	for _, v := range call(t, s, "tools/list", "")["tools"].([]interface{}) {
		tool := v.(map[string]interface{})
		listed[tool["name"].(string)] = tool
	}
	for _, name := range []string{"grid_mesh_status", "grid_mesh_node", "grid_mesh_path", "grid_mesh_telemetry", "grid_mesh_changes"} {
		tool, ok := listed[name]
		if !ok {
			t.Errorf("%s is not listed", name)
			continue
		}
		if d := tool["description"].(string); len(d) < 200 {
			t.Errorf("%s: the description must say when to use it (%d chars)", name, len(d))
		}
		schema := tool["inputSchema"].(map[string]interface{})
		if schema["type"] != "object" || schema["properties"] == nil {
			t.Errorf("%s: schema = %v", name, schema)
		}
	}
}

/* ------------------------------------------------------------------- the model */

func TestMeshModelStates(t *testing.T) {
	node := func(heard, relayed time.Duration) meshNode {
		n := meshNode{Type: "repeater"}
		if heard > 0 {
			n.Heard = meshNow.Add(-heard)
		}
		if relayed > 0 {
			n.Relayed = meshNow.Add(-relayed)
		}
		return n
	}
	for _, c := range []struct {
		n    meshNode
		want string
	}{
		{node(0, 0), stateUnknown},
		{node(time.Hour, 0), stateActive},
		{node(30*time.Hour, 11*time.Hour), stateActive}, // the later clock wins
		{node(13*time.Hour, 0), stateQuiet},
		{node(0, 49*time.Hour), stateSilent},
		// A stamp from the future is a fault, not activity.
		{meshNode{Heard: meshNow.Add(time.Hour)}, stateUnknown},
	} {
		if got := c.n.activityState(meshNow); got != c.want {
			t.Errorf("state(heard %v, relayed %v) = %s, want %s", c.n.Heard, c.n.Relayed, got, c.want)
		}
	}

	admin := func(reporter string, read, tried time.Duration) map[string]interface{} {
		a := map[string]interface{}{"reporterId": reporter}
		if read > 0 {
			a["lastSuccessAt"] = ago(read)
		}
		if tried > 0 {
			a["lastAttemptAt"] = ago(tried)
		}
		return a
	}
	for _, c := range []struct {
		name string
		n    meshNode
		want string
	}{
		{"unmonitored", meshNode{Type: "repeater"}, ""},
		{"marked unreachable, no reading", meshNode{Type: "repeater", Reachability: "UNREACHABLE"}, monitorNoAnswer},
		{"never read", meshNode{Type: "repeater", Admin: admin("relay", 0, 0)}, monitorNeverRead},
		{"read on schedule", meshNode{Type: "repeater", Admin: admin("relay", 40*time.Minute, 40*time.Minute)}, monitorAnswering},
		{"relay read overdue", meshNode{Type: "repeater", Admin: admin("relay", 2*time.Hour, 2*time.Hour)}, monitorNotRead},
		{"operator monitor keeps its own hours", meshNode{Type: "repeater", Admin: admin("alanpi", 2*time.Hour, 2*time.Hour)}, monitorAnswering},
		{"tried after the last good read", meshNode{Type: "repeater", Admin: admin("relay", 3*time.Hour, 20*time.Minute)}, monitorNoAnswer},
		{"tried, never answered", meshNode{Type: "repeater", Admin: admin("relay", 0, 20*time.Minute)}, monitorNoAnswer},
		// A gateway not read for 20 minutes is in use or paused, not failing.
		{"gateway not read lately", meshNode{Type: "companion", Admin: admin("relay", 20*time.Minute, 20*time.Minute)}, monitorNotRead},
		{"gateway fresh", meshNode{Type: "companion", Admin: admin("relay", 4*time.Minute, 4*time.Minute)}, monitorAnswering},
	} {
		if got := c.n.monitorState(meshNow); got != c.want {
			t.Errorf("%s: monitor state = %q, want %q", c.name, got, c.want)
		}
	}

	// A low battery is only an issue while the reading is current.
	low := meshNode{Type: "repeater", Relayed: meshNow.Add(-time.Minute),
		Admin: map[string]interface{}{"reporterId": "relay", "lastSuccessAt": ago(10 * time.Minute), "lastAttemptAt": ago(10 * time.Minute), "batteryVolts": 3.45}}
	if is := low.issues(meshNow); len(is) != 1 || !is[0].alarm || !strings.Contains(is[0].text, "Battery low: 3.45 V, read 10 min ago") {
		t.Errorf("low battery issues = %v", is)
	}
	low.Admin["lastSuccessAt"], low.Admin["lastAttemptAt"] = ago(30*time.Hour), ago(30*time.Hour)
	low.Relayed = meshNow.Add(-time.Minute)
	for _, x := range low.issues(meshNow) {
		if strings.Contains(x.text, "Battery") {
			t.Errorf("a 30-hour-old reading raised a battery issue: %v", x)
		}
	}
}

func TestMeshModelWording(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "1 min", 12 * time.Minute: "12 min", 90 * time.Minute: "2 h", 47 * time.Hour: "47 h", 72 * time.Hour: "3 days", -time.Hour: "1 min",
	} {
		if got := ageText(d); got != want {
			t.Errorf("ageText(%v) = %q, want %q", d, got, want)
		}
	}
	for snr, want := range map[float64]string{13: "strong", 8: "strong", 5: "fair", 2.9: "weak", -6: "weak", 0: "unmeasured"} {
		if got := linkStrength(snr); got != want {
			t.Errorf("linkStrength(%v) = %q, want %q", snr, got, want)
		}
	}
	for ref, want := range map[string]bool{"cb0c0579": true, "CB0C": true, "meshcore:cb0c0579": true, "pinebrook": false, "cb0": false, "cafe 1234": false, k("ab"): false} {
		if got := looksLikeKeyPrefix(ref); got != want {
			t.Errorf("looksLikeKeyPrefix(%q) = %v, want %v", ref, got, want)
		}
	}
	// Arnold to Bear Valley is about 34 km.
	if d := distanceKm(38.265, -120.334, 38.461, -120.042); d < 32 || d > 36 {
		t.Errorf("distanceKm = %v", d)
	}
	if trend(nil, meshNow.Add(-time.Hour), meshNow, 12, 2) != nil {
		t.Error("no readings is no trend")
	}
	// A gap in the readings is a gap in the series, not an interpolated point.
	pts := []sample{{meshNow.Add(-23 * time.Hour), 4.0}, {meshNow.Add(-time.Hour), 3.8}}
	tr := trend(pts, meshNow.Add(-24*time.Hour), meshNow, 12, 2)
	if len(tr["series"].([]interface{})) != 2 || tr["change"] != -0.2 {
		t.Errorf("trend = %v", tr)
	}
}
