package gridapi

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Every layer says what its events are. A layer added to the enum without a
// decision here fails: OCCURRENCE is the default in code, and this test is
// what makes someone choose.
func TestEveryLayerHasADecidedNature(t *testing.T) {
	state := map[gridv1.Layer]bool{
		gridv1.Layer_MESH: true, gridv1.Layer_BURN_STATUS: true, gridv1.Layer_SOURCE_HEALTH: true,
		gridv1.Layer_ROAD_SEGMENT: true, gridv1.Layer_WEATHER: true,
	}
	occurrence := map[gridv1.Layer]bool{
		gridv1.Layer_WILDFIRE: true, gridv1.Layer_EVACUATION: true, gridv1.Layer_WEATHER_ALERT: true,
		gridv1.Layer_FIRE_WEATHER: true, gridv1.Layer_EARTHQUAKE: true, gridv1.Layer_ROAD_INCIDENT: true,
		gridv1.Layer_POWER: true, gridv1.Layer_GAUGE: true, gridv1.Layer_AIR_QUALITY: true,
		gridv1.Layer_ANNOUNCEMENT: true, gridv1.Layer_CHAIN_CONTROL: true,
	}
	for n, name := range gridv1.Layer_name {
		l := gridv1.Layer(n)
		switch {
		case l == gridv1.Layer_LAYER_UNSPECIFIED:
			assert.Equal(t, gridv1.EventNature_EVENT_NATURE_UNSPECIFIED, LayerNature(l))
		case state[l]:
			assert.Equal(t, gridv1.EventNature_STATE, LayerNature(l), name)
		case occurrence[l]:
			assert.Equal(t, gridv1.EventNature_OCCURRENCE, LayerNature(l), name)
		default:
			t.Errorf("layer %s has no decided nature: add it to LayerNature (if it is standing state) and to this test", name)
		}
	}
}

// The interceptor reaches an event wherever a response holds one: a list, a
// revision's snapshot, a collapsed history match, a finder hit, a related
// event. No RPC stamps its own.
func TestNatureIsOnEveryEventInEveryResponse(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	indexSearch(t, s)
	g := NewGridServer(s)
	stamp := NatureInterceptor()
	call := func(resp any, err error) any {
		t.Helper()
		require.NoError(t, err)
		out, err := stamp(rpcBG(), nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { return resp, nil })
		require.NoError(t, err)
		return out
	}

	list := call(g.ListEvents(rpcBG(), &gridv1.ListEventsRequest{Status: []string{"ACTIVE", "SCHEDULED", "RESOLVED", "EXPIRED"}, PageSize: 200})).(*gridv1.EventList)
	require.NotEmpty(t, list.GetEvents())
	seen := map[gridv1.EventNature]bool{}
	for _, ev := range list.GetEvents() {
		assert.Equal(t, LayerNature(ev.GetLayer()), ev.GetNature(), ev.GetId())
		seen[ev.GetNature()] = true
	}
	assert.True(t, seen[gridv1.EventNature_STATE] && seen[gridv1.EventNature_OCCURRENCE], "the fixtures hold both kinds")

	one := call(g.GetEvent(rpcBG(), &gridv1.GetEventRequest{Id: "source:caloes"})).(*gridv1.Event)
	assert.Equal(t, gridv1.EventNature_STATE, one.GetNature())

	hist := call(g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: "cc:hwy4:r2"})).(*gridv1.EventRevisionList)
	require.NotEmpty(t, hist.GetRevisions())
	assert.Equal(t, gridv1.EventNature_OCCURRENCE, hist.GetRevisions()[0].GetEvent().GetNature(), "inside a revision")

	collapsed := call(g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Q: "earthquake", Collapse: "event"})).(*gridv1.EventRevisionList)
	require.NotEmpty(t, collapsed.GetCollapsed())
	assert.Equal(t, gridv1.EventNature_OCCURRENCE, collapsed.GetCollapsed()[0].GetFirst().GetEvent().GetNature(), "inside a collapsed match")

	// Derived, never content: an event that comes back into the store
	// carrying it (a poller re-offering what it read from the store) is the
	// same event.
	plain := &gridv1.Event{Id: "x", Layer: gridv1.Layer_MESH}
	stamped := &gridv1.Event{Id: "x", Layer: gridv1.Layer_MESH, Nature: gridv1.EventNature_STATE}
	assert.Equal(t, store.ContentHash(plain), store.ContentHash(stamped))
}

// A road event's closure past its end recorded before `seasonal` and
// `severity` existed is served with both, wherever the event sits in a
// response; one recorded since is left as it is, and nothing else is touched.
func TestBeyondOnOldRevisionsIsCompleted(t *testing.T) {
	old := func() *gridv1.Event {
		return &gridv1.Event{Id: "road:hwy4-arnold-bearvalley", Layer: gridv1.Layer_ROAD_SEGMENT, Detail: &gridv1.Event_RoadSegment{RoadSegment: &gridv1.RoadSegmentDetail{
			Status: "OPEN", Beyond: []*gridv1.RoadSegmentBeyond{{Name: "Ebbetts Pass winter closure", Side: "END", Status: "CLOSED"}},
		}}}
	}
	resp := &gridv1.EventRevisionList{Revisions: []*gridv1.EventRevision{{Revision: 18, Event: old()}}}
	StampNature(resp)
	b := resp.GetRevisions()[0].GetEvent().GetRoadSegment().GetBeyond()[0]
	assert.True(t, b.GetSeasonal())
	assert.Equal(t, "INFO", b.GetSeverity())
	assert.Equal(t, "Ebbetts Pass winter closure", b.GetName())
	assert.Equal(t, "CLOSED", b.GetStatus())

	// Recorded with the fields: whatever it says stands.
	rated := old()
	rated.GetRoadSegment().GetBeyond()[0].Severity = "MINOR"
	StampNature(rated)
	assert.False(t, rated.GetRoadSegment().GetBeyond()[0].GetSeasonal())
	assert.Equal(t, "MINOR", rated.GetRoadSegment().GetBeyond()[0].GetSeverity())

	// An event with no such closure, and one of another layer, are untouched.
	plain := &gridv1.Event{Layer: gridv1.Layer_ROAD_SEGMENT, Detail: &gridv1.Event_RoadSegment{RoadSegment: &gridv1.RoadSegmentDetail{Status: "OPEN"}}}
	StampNature(plain)
	assert.Empty(t, plain.GetRoadSegment().GetBeyond())
	StampNature(&gridv1.Event{Layer: gridv1.Layer_MESH})
}
