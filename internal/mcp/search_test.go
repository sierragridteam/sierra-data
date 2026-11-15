package mcp

import (
	"net/http"
	"net/url"
	"testing"
)

// recordingGrid answers /api/v1/events and /api/v1/history with canned search
// bodies and keeps the query string each was called with.
type recordingGrid struct {
	events, history url.Values
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
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":5,"message":"not found"}`))
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
