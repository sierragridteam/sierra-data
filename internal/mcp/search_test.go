package mcp

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// recordingGrid answers /api/v1/events and /api/v1/history with canned search
// bodies and keeps the query string each was called with.
type recordingGrid struct {
	events, history, search url.Values
}

func (g *recordingGrid) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/events":
		g.events = r.URL.Query()
		if r.URL.Query().Get("q") == "" && r.URL.Query().Get("counts") == "" {
			_, _ = w.Write([]byte(`{"events":[{"id":"e1","headline":"Big Fire","status":"ACTIVE"}],"nextPageToken":"","totalMatches":null,"counts":[],"matches":{},"search":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[{"id":"chp:1","headline":"Road flooded","status":"RESOLVED","description":"long","geometry":{"centroid":{"lat":38,"lng":-120}}}],
			"nextPageToken":"tok","totalMatches":23,
			"counts":[{"layer":"ROAD_INCIDENT","category":"incident","events":23}],
			"matches":{"chp:1":{"field":"text","parts":[{"text":"Road ","hit":false},{"text":"flooded","hit":true},{"text":" at ","hit":false},{"text":"SR 108","hit":true}]}},
			"search":{"query":"route108 flooded","since":"2026-07-07T00:19:16Z","note":"No match does not mean nothing happened."}}`))
	case "/api/v1/history":
		g.history = r.URL.Query()
		switch {
		case r.URL.Query().Get("collapse") == "event":
			_, _ = w.Write([]byte(`{"revisions":[],"nextPageToken":"","totalMatches":1,"search":{"query":"evacuation order","since":"2026-07-07T00:19:16Z","note":"n"},
				"collapsed":[{"eventId":"evac:1","lastRevision":3,"lastObservedAt":"2026-08-21T03:01:38Z","matchingRevisions":3,
				  "first":{"revision":1,"observedAt":"2026-08-20T22:10:00Z","event":{"id":"evac:1","headline":"Evacuation Order","description":"long","geometry":{"geojson":"HUGE"}},
				           "match":{"field":"title","parts":[{"text":"Evacuation","hit":true},{"text":" ","hit":false},{"text":"Order","hit":true}]}}}]}`))
		case r.URL.Query().Get("q") != "":
			_, _ = w.Write([]byte(`{"revisions":[{"revision":2,"event":{"id":"cc:88","headline":"R2 on 88","description":"long"},"match":{"field":"title","parts":[{"text":"R2","hit":true},{"text":" on 88","hit":false}]}}],"nextPageToken":"","totalMatches":2,"collapsed":[],"search":{"query":"r2","note":"n"}}`))
		default:
			_, _ = w.Write([]byte(`{"revisions":[{"revision":1,"event":{"id":"e1","headline":"Big Fire","description":"long"},"match":null}],"nextPageToken":"","totalMatches":null,"collapsed":[],"search":null}`))
		}
	case "/api/v1/search":
		g.search = r.URL.Query()
		_, _ = w.Write([]byte(`{"hits":[
			{"kind":"PLACE","id":"arnold","title":"Arnold","detail":"Town","match":null,"apiPath":"/api/v1/places/arnold/summary","url":"","layer":"LAYER_UNSPECIFIED","category":"","status":"EVENT_STATUS_UNSPECIFIED","severity":"SEVERITY_UNSPECIFIED","observedAt":null},
			{"kind":"ROAD","id":"hwy4-arnold-bearvalley","title":"Hwy 4","detail":"Arnold to Bear Valley","match":null,"apiPath":"/api/v1/roads/hwy4-arnold-bearvalley/history","url":"","layer":"LAYER_UNSPECIFIED","category":"","status":"EVENT_STATUS_UNSPECIFIED","severity":"SEVERITY_UNSPECIFIED","observedAt":null},
			{"kind":"EVENT","id":"calfire:x","title":"Arnold Fire","detail":"","match":{"field":"title","parts":[{"text":"Arnold","hit":true},{"text":" Fire","hit":false}]},"apiPath":"/api/v1/events/calfire:x","url":"https://fire.ca.gov/x","layer":"WILDFIRE","category":"wildfire","status":"RESOLVED","severity":"SEVERE","observedAt":"2026-08-01T00:00:00Z"},
			{"kind":"CAMERA","id":"c1","title":"Hwy 4 at Arnold","detail":"SR-4, Arnold","match":null,"apiPath":"/api/v1/cameras","url":"https://cwwp2.dot.ca.gov/c1.jpg","layer":"LAYER_UNSPECIFIED","category":"","status":"EVENT_STATUS_UNSPECIFIED","severity":"SEVERITY_UNSPECIFIED","observedAt":null}],
			"totals":[{"kind":"PLACE","matches":1},{"kind":"ROAD","matches":1},{"kind":"EVENT","matches":12},{"kind":"MESH_NODE","matches":0},{"kind":"SCANNER","matches":0}],
			"unavailable":[],"search":{"query":"arnold","since":"2026-07-07T00:19:16Z","note":"n"}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":5,"message":"not found"}`))
	}
}

func TestGridSearch(t *testing.T) {
	g := &recordingGrid{}
	s := NewHandler(g)
	out := structured(t, s, "grid_search", `{"q":"arnold","kind":"place,event","limit":3}`)
	if g.search.Get("q") != "arnold" || g.search.Get("kind") != "place,event" || g.search.Get("limit") != "3" {
		t.Errorf("search params = %v", g.search)
	}
	hits := out["hits"].([]interface{})
	if len(hits) != 4 || out["count"] != float64(4) {
		t.Fatalf("hits = %v", hits)
	}
	next := map[string]string{}
	for _, h := range hits {
		hit := h.(map[string]interface{})
		kind := hit["kind"].(string)
		if n, ok := hit["next"].(string); ok {
			next[kind] = n
		}
		switch kind {
		case "EVENT":
			// An event keeps its facts and gets a one-line excerpt.
			if hit["status"] != "RESOLVED" || hit["layer"] != "WILDFIRE" || hit["match"] != "**Arnold** Fire" {
				t.Errorf("event hit = %v", hit)
			}
			if _, ok := hit["detail"]; ok {
				t.Error("an empty detail should be dropped")
			}
		case "PLACE", "ROAD", "CAMERA":
			// Fields that only mean something for an event are not sent as
			// "unspecified" for a model to misread.
			for _, k := range []string{"layer", "status", "severity", "observedAt", "match"} {
				if _, ok := hit[k]; ok {
					t.Errorf("%s hit carries %s", kind, k)
				}
			}
		}
		if kind == "CAMERA" {
			if hit["url"] != "https://cwwp2.dot.ca.gov/c1.jpg" {
				t.Errorf("camera url = %v", hit["url"])
			}
			if _, ok := hit["apiPath"]; ok {
				t.Error("a camera's link is its url")
			}
		}
	}
	want := map[string]string{
		"PLACE": "grid_situation location=arnold",
		"ROAD":  `grid_events q="Hwy 4" layer=road_incident`,
		"EVENT": "grid_event id=calfire:x",
	}
	for k, v := range want {
		if next[k] != v {
			t.Errorf("next[%s] = %q, want %q", k, next[k], v)
		}
	}
	if len(out["totals"].([]interface{})) != 5 {
		t.Errorf("totals = %v", out["totals"])
	}
	if _, ok := out["unavailable"]; ok {
		t.Error("an empty unavailable list should be dropped")
	}
	if out["search"].(map[string]interface{})["note"] == nil {
		t.Error("search note missing")
	}

	// q is required.
	res := call(t, s, "tools/call", `{"name":"grid_search","arguments":{}}`)
	if res["isError"] != true {
		t.Error("grid_search without q should be a tool error")
	}
}

func TestGridEventsSearch(t *testing.T) {
	g := &recordingGrid{}
	s := NewHandler(g)
	out := structured(t, s, "grid_events", `{"q":"hwy 108 flooded","layer":"road_incident"}`)

	// What the API was asked: the text, every status (ended events are what
	// a search is usually for), no shapes.
	if got := g.events.Get("q"); got != "hwy 108 flooded" {
		t.Errorf("q = %q", got)
	}
	if got := g.events.Get("status"); got != allStatuses {
		t.Errorf("status = %q, want every status with q", got)
	}
	if got := g.events.Get("geometry_format"); got != "NONE" {
		t.Errorf("geometry_format = %q", got)
	}

	// What the model reads: the total over every page, the breakdown, the
	// coverage note, and a one-line excerpt on the row.
	if out["total"] != float64(23) {
		t.Errorf("total = %v", out["total"])
	}
	if len(out["counts"].([]interface{})) != 1 {
		t.Errorf("counts = %v", out["counts"])
	}
	if out["search"].(map[string]interface{})["since"] == nil {
		t.Error("search coverage missing")
	}
	if out["nextPageToken"] != "tok" {
		t.Errorf("nextPageToken = %v", out["nextPageToken"])
	}
	ev := out["events"].([]interface{})[0].(map[string]interface{})
	if ev["match"] != "Road **flooded** at **SR 108**" {
		t.Errorf("match = %q", ev["match"])
	}
	if _, ok := ev["description"]; ok {
		t.Error("list rows drop the long description")
	}
	if ev["status"] != "RESOLVED" {
		t.Error("each row must say its status when ended events are included")
	}
}

func TestGridEventsStatusRules(t *testing.T) {
	g := &recordingGrid{}
	s := NewHandler(g)
	// An explicit status wins over the search default.
	structured(t, s, "grid_events", `{"q":"flooded","status":"ACTIVE"}`)
	if got := g.events.Get("status"); got != "ACTIVE" {
		t.Errorf("status = %q", got)
	}
	// "any" works without q.
	structured(t, s, "grid_events", `{"status":"any"}`)
	if got := g.events.Get("status"); got != allStatuses {
		t.Errorf("status = %q", got)
	}
	// No q, no status: the API's own default, and no search fields invented.
	out := structured(t, s, "grid_events", `{}`)
	if g.events.Has("status") || g.events.Has("q") {
		t.Errorf("unexpected params: %v", g.events)
	}
	for _, k := range []string{"total", "counts", "search"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s present without q or count", k)
		}
	}
	// count=true asks for the tally alone.
	out = structured(t, s, "grid_events", `{"count":true,"layer":"road_incident"}`)
	if g.events.Get("counts") != "true" {
		t.Errorf("counts not requested: %v", g.events)
	}
	if out["total"] != float64(23) {
		t.Errorf("total = %v", out["total"])
	}
}

func TestGridHistorySearchCollapsesByDefault(t *testing.T) {
	g := &recordingGrid{}
	s := NewHandler(g)
	out := structured(t, s, "grid_history", `{"q":"evacuation order","limit":5}`)
	if g.history.Get("collapse") != "event" || g.history.Get("q") != "evacuation order" || g.history.Get("page_size") != "5" {
		t.Errorf("history params = %v", g.history)
	}
	rows := out["matches"].([]interface{})
	if len(rows) != 1 || out["total"] != float64(1) {
		t.Fatalf("matches = %v total = %v", rows, out["total"])
	}
	row := rows[0].(map[string]interface{})
	if row["matchingRevisions"] != float64(3) || row["lastRevision"] != float64(3) {
		t.Errorf("row = %v", row)
	}
	first := row["first"].(map[string]interface{})
	if first["match"] != "**Evacuation** **Order**" {
		t.Errorf("first.match = %q", first["match"])
	}
	ev := first["event"].(map[string]interface{})
	if _, ok := ev["geometry"]; ok {
		t.Error("geometry should be stripped")
	}
	if _, ok := out["revisions"]; ok {
		t.Error("collapsed answer should not also carry revisions")
	}

	// every_revision=true: one row per matching revision.
	out = structured(t, s, "grid_history", `{"q":"r2","every_revision":true}`)
	if g.history.Has("collapse") {
		t.Errorf("collapse sent with every_revision: %v", g.history)
	}
	revs := out["revisions"].([]interface{})
	if revs[0].(map[string]interface{})["match"] != "**R2** on 88" {
		t.Errorf("revision match = %v", revs[0])
	}

	// Without q: as before, and a null match is dropped, not rendered.
	out = structured(t, s, "grid_history", `{}`)
	if g.history.Has("q") || g.history.Has("collapse") {
		t.Errorf("history params = %v", g.history)
	}
	if _, ok := out["revisions"].([]interface{})[0].(map[string]interface{})["match"]; ok {
		t.Error("match should be absent without q")
	}
	if _, ok := out["total"]; ok {
		t.Error("total present without q")
	}
}

// The generic pair some clients require by name (ChatGPT deep research):
// search(query) -> {results:[{id,title,url}]}, fetch(id) -> {id,title,text,url,metadata}.
func TestGenericSearchAndFetch(t *testing.T) {
	g := &recordingGrid{}
	s := NewHandler(g)
	out := structured(t, s, "search", `{"query":"arnold"}`)
	if g.search.Get("q") != "arnold" || g.search.Get("kind") != "EVENT,MESH_NODE,PLACE" {
		t.Errorf("search params = %v", g.search)
	}
	results := out["results"].([]interface{})
	if len(results) != 4 {
		t.Fatalf("results = %v", results)
	}
	byID := map[string]map[string]interface{}{}
	for _, r := range results {
		m := r.(map[string]interface{})
		for _, k := range []string{"id", "title", "url"} {
			if v, _ := m[k].(string); v == "" {
				t.Errorf("result %v has no %s (a client only cites a result with a url)", m, k)
			}
		}
		byID[m["id"].(string)] = m
	}
	place := byID["place:arnold"]
	if place == nil || place["title"] != "Arnold (Town)" || place["url"] != "https://data.sierragridteam.org/api/v1/places/arnold/summary" {
		t.Errorf("place result = %v", place)
	}
	ev := byID["event:calfire:x"]
	if ev == nil || ev["title"] != "Arnold Fire [RESOLVED]" || ev["url"] != "https://fire.ca.gov/x" {
		t.Errorf("event result = %v", ev)
	}

	// fetch an event, with and without the kind prefix; and a place.
	f := NewHandler(fakeGrid{})
	for _, id := range []string{"event:e1", "e1"} {
		doc := structured(t, f, "fetch", `{"id":"`+id+`"}`)
		if doc["id"] != id || doc["title"] != "Big Fire — 5000 ac" || doc["url"] != "https://fire.ca.gov/x" {
			t.Errorf("fetch %s = %v", id, doc)
		}
		text := doc["text"].(string)
		for _, want := range []string{"Big Fire — 5000 ac", "Severity: EXTREME", "Source's description: verbatim long text"} {
			if !strings.Contains(text, want) {
				t.Errorf("fetch %s text lacks %q: %s", id, want, text)
			}
		}
		if strings.Contains(text, "HUGE_BASE64_BLOB") {
			t.Error("geometry leaked into the text")
		}
		if doc["metadata"].(map[string]interface{})["severity"] != "EXTREME" {
			t.Errorf("metadata = %v", doc["metadata"])
		}
	}
	doc := structured(t, f, "fetch", `{"id":"place:ebbetts-pass"}`)
	if !strings.Contains(doc["text"].(string), "UNAVAILABLE") {
		t.Errorf("a place fetch must carry the fail-loud evacuation status: %v", doc["text"])
	}
	if doc["url"] != "https://data.sierragridteam.org/api/v1/places/ebbetts-pass/summary" {
		t.Errorf("place url = %v", doc["url"])
	}

	// Unknown and missing ids are tool errors, not empty documents.
	for _, args := range []string{`{"id":"event:nope"}`, `{}`} {
		res := call(t, f, "tools/call", `{"name":"fetch","arguments":`+args+`}`)
		if res["isError"] != true {
			t.Errorf("fetch %s should be a tool error", args)
		}
	}
}
