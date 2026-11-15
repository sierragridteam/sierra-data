package gridapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/cwwp2"
)

// indexSearch runs the writer's search backfill to the end, which is what
// marks the index complete; until then a text query is refused.
func indexSearch(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	for after := ""; ; {
		b, err := s.Store.BackfillEventSearch(ctx, after, 100)
		require.NoError(t, err)
		if b.Done {
			break
		}
		after = b.NextID
	}
	for id, rev := "", int64(0); ; {
		b, err := s.Store.BackfillRevisionSearch(ctx, id, rev, 100)
		require.NoError(t, err)
		if b.Done {
			break
		}
		id, rev = b.NextID, b.NextRev
	}
}

func searchServer(t *testing.T) (*GridServer, *Service) {
	t.Helper()
	s := newTestService(t)
	indexSearch(t, s)
	return NewGridServer(s), s
}

func matchText(m *gridv1.SearchMatch) (all string, hits []string) {
	for _, p := range m.GetParts() {
		all += p.GetText()
		if p.GetHit() {
			hits = append(hits, p.GetText())
		}
	}
	return
}

func TestListEventsTextSearch(t *testing.T) {
	g, s := searchServer(t)
	ctx := context.Background()
	for i, h := range []string{"Crash on SR 4 at Big Trees Rd", "Rockslide on Hwy 4 near Dorrington"} {
		_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
			Id: []string{"chp:s1", "chp:s2"}[i], Layer: gridv1.Layer_ROAD_INCIDENT, Category: "incident",
			Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_ACTIVE, Headline: h,
			Summary:    "Reported by a passing driver.",
			ObservedAt: timestamppb.New(base.Add(time.Duration(i) * time.Minute)),
			Provenance: &gridv1.Provenance{SourceId: "chp", SourceName: "CHP"},
		})
		require.NoError(t, err)
	}

	out, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Q: "highway 4"})
	require.NoError(t, err)
	// Newest first, not by severity.
	require.Len(t, out.GetEvents(), 2)
	assert.Equal(t, "chp:s2", out.GetEvents()[0].GetId())
	assert.EqualValues(t, 2, out.GetTotalMatches().GetValue())
	require.Len(t, out.GetCounts(), 1)
	assert.Equal(t, gridv1.Layer_ROAD_INCIDENT, out.GetCounts()[0].GetLayer())
	assert.Equal(t, "incident", out.GetCounts()[0].GetCategory())
	assert.EqualValues(t, 2, out.GetCounts()[0].GetEvents())

	// How the query was read, since when, and the standing caveat.
	assert.Equal(t, "route4", out.GetSearch().GetQuery())
	assert.NotNil(t, out.GetSearch().GetSince())
	assert.Equal(t, SearchNote, out.GetSearch().GetNote())

	// A snippet per event on the page, the road highlighted as written.
	require.Len(t, out.GetMatches(), 2)
	all, hits := matchText(out.GetMatches()["chp:s1"])
	assert.Equal(t, "title", out.GetMatches()["chp:s1"].GetField())
	assert.Equal(t, []string{"SR 4"}, hits)
	assert.Contains(t, all, "Crash on SR 4 at Big Trees Rd")

	// The other filters still apply, the status default included.
	out, err = g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Q: "roadway"})
	require.NoError(t, err)
	assert.Empty(t, out.GetEvents(), "chp:i1 is RESOLVED")
	assert.EqualValues(t, 0, out.GetTotalMatches().GetValue())
	assert.NotNil(t, out.GetTotalMatches(), "zero is a count, not an absence")
	out, err = g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Q: "roadway", Status: allStatuses})
	require.NoError(t, err)
	require.Len(t, out.GetEvents(), 1)
	assert.Equal(t, "chp:i1", out.GetEvents()[0].GetId())

	// Whole words unless the box says it is still being typed.
	out, err = g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Q: "rocksl"})
	require.NoError(t, err)
	assert.Empty(t, out.GetEvents())
	out, err = g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Q: "rocksl", QTyping: true})
	require.NoError(t, err)
	require.Len(t, out.GetEvents(), 1)
}

func TestListEventsWithoutSearchIsUnchanged(t *testing.T) {
	g, _ := searchServer(t)
	out, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, out.GetEvents())
	assert.Nil(t, out.GetTotalMatches())
	assert.Empty(t, out.GetCounts())
	assert.Empty(t, out.GetMatches())
	assert.Nil(t, out.GetSearch())
	// Blank q is no query.
	blank, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Q: "   "})
	require.NoError(t, err)
	assert.Nil(t, blank.GetSearch())
	assert.Equal(t, len(out.GetEvents()), len(blank.GetEvents()))
}

func TestListEventsCountsWithoutText(t *testing.T) {
	g, _ := searchServer(t)
	out, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Counts: true, Status: allStatuses, PageSize: 1})
	require.NoError(t, err)
	assert.Len(t, out.GetEvents(), 1)
	assert.EqualValues(t, 4, out.GetTotalMatches().GetValue(), "the total is over every page")
	var sum int32
	for _, c := range out.GetCounts() {
		sum += c.GetEvents()
	}
	assert.EqualValues(t, 4, sum)
	assert.Nil(t, out.GetSearch())
	assert.Empty(t, out.GetMatches())
	// Severity order is kept: this is the ordinary list with a total.
	assert.Equal(t, "calfire:f1", out.GetEvents()[0].GetId())
}

func TestSearchErrors(t *testing.T) {
	// An index that is not known complete refuses, loudly.
	s := newTestService(t)
	g := NewGridServer(s)
	_, err := g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Q: "arnold"})
	assert.Equal(t, codes.Unavailable, status.Code(err))
	_, err = g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Counts: true})
	assert.Equal(t, codes.Unavailable, status.Code(err))
	_, err = g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Q: "arnold"})
	assert.Equal(t, codes.Unavailable, status.Code(err))
	// Without q nothing changes.
	_, err = g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{})
	require.NoError(t, err)

	indexSearch(t, s)
	for _, q := range []string{"-fire", "the", `""`, "?!", string(make([]byte, 201))} {
		_, err = g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Q: q})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%q", q)
		_, err = g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Q: q})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%q", q)
	}
	_, err = g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Collapse: "event"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "collapse needs q")
	_, err = g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Q: "arnold", Collapse: "day"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestListHistoryTextSearch(t *testing.T) {
	g, _ := searchServer(t)
	// The fixture's quake was revised: "M4.2 earthquake near Arnold", then
	// "M4.4 earthquake near Arnold (revised)".
	out, err := g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Q: "earthquake arnold"})
	require.NoError(t, err)
	require.Len(t, out.GetRevisions(), 2)
	assert.EqualValues(t, 2, out.GetTotalMatches().GetValue())
	assert.Empty(t, out.GetCollapsed())
	assert.Equal(t, "earthquake arnold", out.GetSearch().GetQuery())
	for _, r := range out.GetRevisions() {
		_, hits := matchText(r.GetMatch())
		assert.Equal(t, []string{"earthquake", "Arnold"}, hits)
	}

	// What it said only at first.
	out, err = g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Q: `"M4.2"`})
	require.NoError(t, err)
	require.Len(t, out.GetRevisions(), 1)
	assert.EqualValues(t, 1, out.GetRevisions()[0].GetRevision())

	// Collapsed: one row for the event.
	out, err = g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Q: "earthquake arnold", Collapse: "EVENT"})
	require.NoError(t, err)
	assert.Empty(t, out.GetRevisions())
	require.Len(t, out.GetCollapsed(), 1)
	assert.EqualValues(t, 1, out.GetTotalMatches().GetValue())
	m := out.GetCollapsed()[0]
	assert.Equal(t, "usgs:q1", m.GetEventId())
	assert.EqualValues(t, 1, m.GetFirst().GetRevision())
	assert.Equal(t, "M4.2 earthquake near Arnold", m.GetFirst().GetEvent().GetHeadline())
	assert.NotEmpty(t, m.GetFirst().GetMatch().GetParts())
	assert.EqualValues(t, 2, m.GetLastRevision())
	assert.NotNil(t, m.GetLastObservedAt())
	assert.EqualValues(t, 2, m.GetMatchingRevisions())

	// Without q the archive is as before.
	out, err = g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{})
	require.NoError(t, err)
	assert.Nil(t, out.GetTotalMatches())
	assert.Nil(t, out.GetSearch())
	for _, r := range out.GetRevisions() {
		assert.Nil(t, r.GetMatch())
	}
}

// The validator moves when the index is rebuilt, and differs per query.
func TestSearchETagFollowsTheQueryAndTheIndex(t *testing.T) {
	g, s := searchServer(t)
	tagOf := func(req *gridv1.ListEventsRequest) string {
		t.Helper()
		return rpcTag(t, func(ctx context.Context) error { _, err := g.ListEvents(ctx, req); return err })
	}
	a := tagOf(&gridv1.ListEventsRequest{Q: "arnold"})
	assert.Equal(t, a, tagOf(&gridv1.ListEventsRequest{Q: "arnold"}))
	assert.NotEqual(t, a, tagOf(&gridv1.ListEventsRequest{Q: "salt"}))
	assert.NotEqual(t, a, tagOf(&gridv1.ListEventsRequest{Q: "arnold", QTyping: true}))
	assert.NotEqual(t, a, tagOf(&gridv1.ListEventsRequest{}))

	// A rebuild (as after a rule change) with no new revision.
	_, err := s.Store.Pool().Exec(context.Background(), `UPDATE event_search SET doc_version = 0`)
	require.NoError(t, err)
	indexSearch(t, s)
	assert.NotEqual(t, a, tagOf(&gridv1.ListEventsRequest{Q: "arnold"}))
}

// Over HTTP, the way a browser or the SDK calls it: the parameter names in
// both casings, the JSON keys, and the status codes of a refusal.
func TestSearchThroughTheGateway(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	baseURL := serveGateway(t, g, GatewayDialOptions()...)

	// Not indexed yet: 503, retryable, for q; the plain list is untouched.
	assert.Equal(t, http.StatusServiceUnavailable, getJSON(t, baseURL+"/api/v1/events?q=arnold", nil))
	assert.Equal(t, http.StatusOK, getJSON(t, baseURL+"/api/v1/events", nil))
	indexSearch(t, s)

	var list struct {
		Events []struct {
			ID string `json:"id"`
		} `json:"events"`
		TotalMatches *int `json:"totalMatches"`
		Counts       []struct {
			Layer, Category string
			Events          int
		} `json:"counts"`
		Matches map[string]struct {
			Field string `json:"field"`
			Parts []struct {
				Text string `json:"text"`
				Hit  bool   `json:"hit"`
			} `json:"parts"`
		} `json:"matches"`
		Search *struct {
			Query, Since, Note string
		} `json:"search"`
	}
	require.Equal(t, http.StatusOK, getJSON(t, baseURL+"/api/v1/events?q=earthquake+arnold", &list))
	require.Len(t, list.Events, 1)
	assert.Equal(t, "usgs:q1", list.Events[0].ID)
	require.NotNil(t, list.TotalMatches)
	assert.Equal(t, 1, *list.TotalMatches)
	require.Len(t, list.Counts, 1)
	assert.Equal(t, "EARTHQUAKE", list.Counts[0].Layer)
	require.Contains(t, list.Matches, "usgs:q1")
	assert.Equal(t, "title", list.Matches["usgs:q1"].Field)
	require.NotNil(t, list.Search)
	assert.Equal(t, "earthquake arnold", list.Search.Query)
	assert.NotEmpty(t, list.Search.Since)

	// qTyping in both casings; counts alone.
	for _, p := range []string{"q=earthq&qTyping=true", "q=earthq&q_typing=true"} {
		list.Events = nil
		require.Equal(t, http.StatusOK, getJSON(t, baseURL+"/api/v1/events?"+p, &list), p)
		assert.Len(t, list.Events, 1, p)
	}
	list.Events = nil
	require.Equal(t, http.StatusOK, getJSON(t, baseURL+"/api/v1/events?q=earthq", &list))
	assert.Empty(t, list.Events, "whole words without qTyping")
	list.TotalMatches, list.Search = nil, nil
	require.Equal(t, http.StatusOK, getJSON(t, baseURL+"/api/v1/events?counts=true&layer=earthquake", &list))
	require.NotNil(t, list.TotalMatches)
	assert.Nil(t, list.Search)

	// History, collapsed.
	var hist struct {
		Revisions []json.RawMessage `json:"revisions"`
		Collapsed []struct {
			EventID string `json:"eventId"`
			First   struct {
				Revision int `json:"revision"`
				Match    *struct {
					Field string `json:"field"`
				} `json:"match"`
			} `json:"first"`
			LastRevision      int    `json:"lastRevision"`
			LastObservedAt    string `json:"lastObservedAt"`
			MatchingRevisions int    `json:"matchingRevisions"`
		} `json:"collapsed"`
		TotalMatches *int `json:"totalMatches"`
	}
	require.Equal(t, http.StatusOK, getJSON(t, baseURL+"/api/v1/history?q=earthquake&collapse=event", &hist))
	assert.Empty(t, hist.Revisions)
	require.Len(t, hist.Collapsed, 1)
	assert.Equal(t, "usgs:q1", hist.Collapsed[0].EventID)
	assert.Equal(t, 1, hist.Collapsed[0].First.Revision)
	require.NotNil(t, hist.Collapsed[0].First.Match)
	assert.Equal(t, 2, hist.Collapsed[0].LastRevision)
	assert.NotEmpty(t, hist.Collapsed[0].LastObservedAt)
	assert.Equal(t, 2, hist.Collapsed[0].MatchingRevisions)

	// Refusals.
	assert.Equal(t, http.StatusBadRequest, getJSON(t, baseURL+"/api/v1/events?q=the", nil))
	assert.Equal(t, http.StatusBadRequest, getJSON(t, baseURL+"/api/v1/events?q=-fire", nil))
	assert.Equal(t, http.StatusBadRequest, getJSON(t, baseURL+"/api/v1/history?collapse=event", nil))
}

func hitIDs(res *gridv1.SearchResults, kind gridv1.SearchKind) []string {
	var out []string
	for _, h := range res.GetHits() {
		if h.GetKind() == kind {
			out = append(out, h.GetId())
		}
	}
	return out
}

func totalOf(res *gridv1.SearchResults, kind gridv1.SearchKind) int {
	for _, t := range res.GetTotals() {
		if t.GetKind() == kind {
			return int(t.GetMatches())
		}
	}
	return -1
}

func TestFinderSearchesEveryKind(t *testing.T) {
	g, s := searchServer(t)
	ctx := context.Background()
	node := &gridv1.Event{
		Id: "meshcore:ab12", Layer: gridv1.Layer_MESH, Category: "repeater",
		Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE, Headline: "SIERRA Arnold Summit",
		ObservedAt: timestamppb.New(base), Provenance: &gridv1.Provenance{SourceId: "usgs"},
		Detail: &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: "ab12", Name: "SIERRA Arnold Summit", NodeType: "repeater"}},
	}
	_, err := s.Store.UpsertEvent(ctx, node)
	require.NoError(t, err)

	res, err := g.Search(rpcBG(), &gridv1.SearchRequest{Q: "arnold"})
	require.NoError(t, err)

	// The town, the road segment that ends there, an event and a mesh node
	// named for it, each with the path that returns it. The town itself comes
	// before the corridor that only mentions it.
	places := hitIDs(res, gridv1.SearchKind_PLACE)
	require.NotEmpty(t, places)
	assert.Equal(t, "arnold", places[0])
	assert.Equal(t, []string{"hwy-4"}, hitIDs(res, gridv1.SearchKind_ROAD))
	assert.Equal(t, []string{"usgs:q1"}, hitIDs(res, gridv1.SearchKind_EVENT))
	assert.Equal(t, []string{"meshcore:ab12"}, hitIDs(res, gridv1.SearchKind_MESH_NODE))
	assert.Equal(t, 1, totalOf(res, gridv1.SearchKind_EVENT))
	assert.Equal(t, 1, totalOf(res, gridv1.SearchKind_MESH_NODE))
	var kinds []gridv1.SearchKind
	for _, h := range res.GetHits() {
		if len(kinds) == 0 || kinds[len(kinds)-1] != h.GetKind() {
			kinds = append(kinds, h.GetKind())
		}
		assert.NotEmpty(t, h.GetTitle())
		assert.NotEmpty(t, h.GetApiPath())
	}
	assert.Equal(t, []gridv1.SearchKind{gridv1.SearchKind_PLACE, gridv1.SearchKind_ROAD, gridv1.SearchKind_EVENT, gridv1.SearchKind_MESH_NODE}, kinds,
		"kinds come in the documented order")
	for _, h := range res.GetHits() {
		switch h.GetKind() {
		case gridv1.SearchKind_PLACE:
			if h.GetId() == "arnold" {
				assert.Equal(t, "/api/v1/places/arnold/summary", h.GetApiPath())
				assert.Equal(t, "Town", h.GetDetail())
			}
			assert.Nil(t, h.GetEvent(), "only an event hit carries event facts")
		case gridv1.SearchKind_EVENT:
			assert.Equal(t, "/api/v1/events/usgs:q1", h.GetApiPath())
			assert.Equal(t, gridv1.Layer_EARTHQUAKE, h.GetEvent().GetLayer())
			assert.Equal(t, gridv1.EventStatus_ACTIVE, h.GetEvent().GetStatus())
			assert.Equal(t, gridv1.Severity_MODERATE, h.GetEvent().GetSeverity())
			assert.NotNil(t, h.GetEvent().GetObservedAt())
			_, hits := matchText(h.GetMatch())
			assert.Equal(t, []string{"Arnold"}, hits)
		}
	}
	assert.Equal(t, "arnold", res.GetSearch().GetQuery())
	assert.Equal(t, SearchNote, res.GetSearch().GetNote())

	// Every status is searched: the resolved incident is found, and says so.
	res, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: "roadway"})
	require.NoError(t, err)
	require.Equal(t, []string{"chp:i1"}, hitIDs(res, gridv1.SearchKind_EVENT))
	assert.Equal(t, gridv1.EventStatus_RESOLVED, res.GetHits()[0].GetEvent().GetStatus())

	// kind narrows; limit caps a kind but not its total.
	res, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: "arnold", Kind: []string{"place,mesh_node"}})
	require.NoError(t, err)
	assert.Empty(t, hitIDs(res, gridv1.SearchKind_EVENT))
	assert.Empty(t, hitIDs(res, gridv1.SearchKind_ROAD))
	assert.Equal(t, -1, totalOf(res, gridv1.SearchKind_EVENT), "a kind not asked for has no total")
	assert.Equal(t, []string{"meshcore:ab12"}, hitIDs(res, gridv1.SearchKind_MESH_NODE))
	res, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: "arnold", Kind: []string{"PLACE"}, Limit: 1})
	require.NoError(t, err)
	assert.Len(t, res.GetHits(), 1)
	assert.Greater(t, totalOf(res, gridv1.SearchKind_PLACE), 1, "the limit caps the hits, not the total")

	// Typing: a prefix finds the place and the node.
	res, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: "arn", QTyping: true})
	require.NoError(t, err)
	assert.Contains(t, hitIDs(res, gridv1.SearchKind_PLACE), "arnold")
	assert.Equal(t, []string{"meshcore:ab12"}, hitIDs(res, gridv1.SearchKind_MESH_NODE))
	res, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: "arn"})
	require.NoError(t, err)
	assert.Empty(t, res.GetHits())
	assert.Equal(t, 0, totalOf(res, gridv1.SearchKind_PLACE), "zero is stated for every kind searched")
}

func TestFinderDirectories(t *testing.T) {
	g, s := searchServer(t)

	// Roads and scanner feeds come from configuration.
	road := s.Cfg.Roads.MonitoredRoads[0]
	res, err := g.Search(rpcBG(), &gridv1.SearchRequest{Q: road.ID, Kind: []string{"ROAD"}})
	require.NoError(t, err)
	require.Equal(t, []string{road.ID}, hitIDs(res, gridv1.SearchKind_ROAD))
	assert.Equal(t, "/api/v1/roads/"+road.ID+"/history", res.GetHits()[0].GetApiPath())

	feeds := s.allScannerFeeds()
	if len(feeds) > 0 {
		res, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: `"` + feeds[0].ChannelLabel + `"`, Kind: []string{"SCANNER"}})
		require.NoError(t, err)
		require.Contains(t, hitIDs(res, gridv1.SearchKind_SCANNER), feeds[0].FeedID)
		assert.Contains(t, res.GetHits()[0].GetUrl(), "broadcastify.com")
	}

	// Cameras: the list ListCameras serves, matched on name, route and place.
	s.Cameras = &fakeCameras{cams: []cwwp2.Camera{cam("c1", 38.2552, -120.3512), cam("c2", 38.26, -120.35)}, status: "OK"}
	res, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: "hwy 108", Kind: []string{"CAMERA"}})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"c1", "c2"}, hitIDs(res, gridv1.SearchKind_CAMERA), "the fixture cameras are on SR-108")
	assert.Equal(t, 2, totalOf(res, gridv1.SearchKind_CAMERA))
	assert.Empty(t, res.GetUnavailable())

	// A camera list that never loaded was not searched: unknown, not none.
	s.Cameras = &fakeCameras{status: "UNAVAILABLE"}
	res, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: "hwy 108"})
	require.NoError(t, err)
	assert.Equal(t, []gridv1.SearchKind{gridv1.SearchKind_CAMERA}, res.GetUnavailable())
	assert.Equal(t, -1, totalOf(res, gridv1.SearchKind_CAMERA), "no total for a kind that could not be searched")
	s.Cameras = nil
	res, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: "hwy 108", Kind: []string{"camera"}})
	require.NoError(t, err)
	assert.Equal(t, []gridv1.SearchKind{gridv1.SearchKind_CAMERA}, res.GetUnavailable())
}

func TestFinderErrors(t *testing.T) {
	s := newTestService(t)
	g := NewGridServer(s)
	_, err := g.Search(rpcBG(), &gridv1.SearchRequest{Q: "arnold"})
	assert.Equal(t, codes.Unavailable, status.Code(err), "an index not known complete is refused")
	// Kinds that need no index still answer.
	res, err := g.Search(rpcBG(), &gridv1.SearchRequest{Q: "arnold", Kind: []string{"PLACE"}})
	require.NoError(t, err)
	assert.Contains(t, hitIDs(res, gridv1.SearchKind_PLACE), "arnold")

	indexSearch(t, s)
	for _, req := range []*gridv1.SearchRequest{
		{}, {Q: "  "}, {Q: "the"}, {Q: "-fire"}, {Q: "arnold", Kind: []string{"planet"}}, {Q: "arnold", Limit: -1},
	} {
		_, err := g.Search(rpcBG(), req)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "%v", req)
	}
	// "the" is not a word English keeps, but a place could be named for it:
	// the refusal comes from the event index only when events are searched.
	_, err = g.Search(rpcBG(), &gridv1.SearchRequest{Q: "the", Kind: []string{"PLACE"}})
	require.NoError(t, err)
}

// One event per work window means a road's best hits can be the same closure
// five times. The finder shows each look-alike once; the total counts them all.
func TestFinderDropsLookAlikes(t *testing.T) {
	g, s := searchServer(t)
	ctx := context.Background()
	for i, id := range []string{"caltrans:w1", "caltrans:w2", "caltrans:w3", "caltrans:other"} {
		head, area := "Hwy 26 one-way traffic control (Guardrail Work)", "Hwy 26 from Route 49 to Main Street"
		if id == "caltrans:other" {
			area = "Hwy 26 from Ridge Road to Glencoe"
		}
		_, err := s.Store.UpsertEvent(ctx, &gridv1.Event{
			Id: id, Layer: gridv1.Layer_ROAD_INCIDENT, Category: "closure",
			Severity: gridv1.Severity_MINOR, Status: gridv1.EventStatus_SCHEDULED, Headline: head, AreaLabel: area,
			ObservedAt: timestamppb.New(base.Add(time.Duration(i) * time.Minute)),
			Provenance: &gridv1.Provenance{SourceId: "chp"},
		})
		require.NoError(t, err)
	}
	res, err := g.Search(rpcBG(), &gridv1.SearchRequest{Q: "hwy 26 guardrail", Kind: []string{"EVENT"}})
	require.NoError(t, err)
	assert.Len(t, res.GetHits(), 2, "the three identical windows show once, the other stretch once")
	assert.Equal(t, 4, totalOf(res, gridv1.SearchKind_EVENT))
	// A title snippet keeps the headline and the location apart.
	all, _ := matchText(res.GetHits()[0].GetMatch())
	assert.Contains(t, all, "(Guardrail Work) · Hwy 26 from")
}
