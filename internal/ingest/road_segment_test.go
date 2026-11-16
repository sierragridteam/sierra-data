package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/store"
)

type fakeRoadLister struct {
	roads []*api.Road
	err   error
}

func (f *fakeRoadLister) ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &api.ListRoadsResponse{Roads: f.roads}, nil
}

func roadSegmentConfig() *config.Config {
	cfg := testConfig()
	cfg.Roads.MonitoredRoads = []config.MonitoredRoad{
		{ID: "hwy4-arnold-bearvalley", Name: "Hwy 4", Section: "Arnold to Bear Valley",
			Origin: config.Coordinates{Latitude: 38.25, Longitude: -120.35}, Destination: config.Coordinates{Latitude: 38.46, Longitude: -120.04}},
		{ID: "hwy49-angels-sonora", Name: "Hwy 49", Section: "Angels Camp to Sonora",
			Origin: config.Coordinates{Latitude: 38.07, Longitude: -120.54}, Destination: config.Coordinates{Latitude: 37.98, Longitude: -120.38}},
	}
	return cfg
}

func road(id string, status api.RoadStatus, congestion api.CongestionLevel, minutes int32) *api.Road {
	return &api.Road{Id: id, Status: status, CongestionLevel: congestion, DurationMinutes: minutes,
		StatusExplanation: "A model's wording, different every refresh " + time.Now().String()}
}

func TestRoadSegmentNormalizer_Event(t *testing.T) {
	lister := &fakeRoadLister{roads: []*api.Road{
		road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 38),
		road("hwy49-angels-sonora", api.RoadStatus_CLOSED, api.CongestionLevel_CLEAR, 25),
	}}
	lister.roads[0].Beyond = []*api.RoadBeyond{{Status: api.RoadStatus_CLOSED, Side: "END", Name: "Ebbetts Pass winter closure", Description: "text that varies"}}
	n := NewRoadSegmentNormalizer(roadSegmentConfig(), lister, func(_ context.Context, lat, _ float64) ([]string, error) {
		if lat > 38.4 {
			return []string{"county:alpine-county"}, nil
		}
		return []string{"county:calaveras-county"}, nil
	})
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Len(t, res.Events, 2)
	assert.Empty(t, res.PerSource)

	hwy4 := res.Events[0]
	assert.Equal(t, "road:hwy4-arnold-bearvalley", hwy4.GetId(), "the road_segment map feature's id")
	assert.Equal(t, gridv1.Layer_ROAD_SEGMENT, hwy4.GetLayer())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, hwy4.GetStatus())
	assert.Equal(t, gridv1.Severity_INFO, hwy4.GetSeverity())
	assert.Equal(t, "open", hwy4.GetCategory())
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: open; Ebbetts Pass winter closure in effect", hwy4.GetHeadline())
	assert.Nil(t, hwy4.GetGeometry(), "no geometry: a chord between the ends is not the road")
	assert.Equal(t, []string{"corridor:hwy4-arnold-bearvalley", "county:alpine-county", "county:calaveras-county"}, hwy4.GetPlaceIds())
	d := hwy4.GetRoadSegment()
	assert.Equal(t, "OPEN", d.GetStatus())
	assert.Equal(t, "CLEAR", d.GetCongestion())
	require.Len(t, d.GetBeyond(), 1)
	assert.Equal(t, "Ebbetts Pass winter closure", d.GetBeyond()[0].GetName())
	assert.Equal(t, "CLOSED", d.GetBeyond()[0].GetStatus())

	closed := res.Events[1]
	assert.Equal(t, gridv1.Severity_SEVERE, closed.GetSeverity())
	assert.Equal(t, "Hwy 49 Angels Camp to Sonora: closed", closed.GetHeadline())

	// A new travel time and a reworded explanation are not a change of state.
	lister.roads[0].DurationMinutes, lister.roads[0].DelayMinutes = 55, 17
	lister.roads[0].StatusExplanation = "reworded"
	again, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Equal(t, store.ContentHash(hwy4), store.ContentHash(again.Events[0]))

	// A closure is.
	lister.roads[0].Status = api.RoadStatus_CLOSED
	changed, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.NotEqual(t, store.ContentHash(hwy4), store.ContentHash(changed.Events[0]))
}

// A refresh with no travel time (Google Routes down) leaves the congestion
// level unspecified. That is a gap in the samples: the event keeps its last
// known level instead of writing a change and another when it comes back.
func TestRoadSegmentNormalizer_KeepsCongestionAcrossAGap(t *testing.T) {
	cfg := roadSegmentConfig()
	cfg.Roads.MonitoredRoads = cfg.Roads.MonitoredRoads[:1]
	lister := &fakeRoadLister{roads: []*api.Road{road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_HEAVY, 70)}}
	n := NewRoadSegmentNormalizer(cfg, lister, nil)
	first, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Equal(t, gridv1.Severity_MODERATE, first.Events[0].GetSeverity())

	lister.roads[0].CongestionLevel, lister.roads[0].DurationMinutes = api.CongestionLevel_CONGESTION_LEVEL_UNSPECIFIED, 0
	gap, err := n.Poll(testCtx(), &scriptedPrior{events: first.Events})
	require.NoError(t, err)
	assert.Equal(t, "HEAVY", gap.Events[0].GetRoadSegment().GetCongestion())
	assert.Equal(t, store.ContentHash(first.Events[0]), store.ContentHash(gap.Events[0]))
}

func TestRoadSegmentNormalizer_FailLoud(t *testing.T) {
	// The roads service failing is a hard error.
	_, err := NewRoadSegmentNormalizer(roadSegmentConfig(), &fakeRoadLister{err: errors.New("roads down")}, nil).Poll(testCtx(), &fakePrior{})
	require.Error(t, err)

	// No monitored roads: never a successful empty poll (the sweep would end
	// every road's event).
	_, err = NewRoadSegmentNormalizer(testConfig(), &fakeRoadLister{}, nil).Poll(testCtx(), &fakePrior{})
	require.Error(t, err)

	// A monitored road the service has no status for: the others land, the
	// source is marked, nothing is swept.
	res, err := NewRoadSegmentNormalizer(roadSegmentConfig(), &fakeRoadLister{roads: []*api.Road{
		road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 38),
	}}, nil).Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Len(t, res.Events, 1)
	require.Error(t, res.PerSource[RoadSegmentSourceID])
	assert.Contains(t, res.PerSource[RoadSegmentSourceID].Error(), "hwy49-angels-sonora")
}

// Through the scheduler: closing and reopening are two revisions of one
// long-lived event, and refreshes in between write nothing.
func TestRoadSegmentLifecycle(t *testing.T) {
	ctx := testCtx()
	cfg := roadSegmentConfig()
	cfg.Roads.MonitoredRoads = cfg.Roads.MonitoredRoads[:1]
	st := newSchedStore(t)
	seedSchedSources(t, st, RoadSegmentSourceID)
	lister := &fakeRoadLister{roads: []*api.Road{road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 38)}}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{RoadSegmentSourceID: {Disappearance: store.DisappearanceResolve}},
	})
	ps := &pollerState{}
	spec := PollerSpec{Normalizer: NewRoadSegmentNormalizer(cfg, lister, nil), Interval: time.Minute}
	const id = "road:hwy4-arnold-bearvalley"

	sched.tick(ctx, spec, ps)
	lister.roads[0].DurationMinutes = 41
	sched.tick(ctx, spec, ps)
	assert.Equal(t, uint32(1), eventRevision(t, st, id))

	lister.roads[0].Status = api.RoadStatus_CLOSED
	sched.tick(ctx, spec, ps)
	lister.roads[0].Status = api.RoadStatus_OPEN
	sched.tick(ctx, spec, ps)

	revs, _, err := st.EventHistory(ctx, id, 10, "")
	require.NoError(t, err)
	require.Len(t, revs, 3)
	assert.Equal(t, "OPEN", revs[0].GetEvent().GetRoadSegment().GetStatus())
	assert.Equal(t, "CLOSED", revs[1].GetEvent().GetRoadSegment().GetStatus())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, revs[1].GetEvent().GetStatus(), "the event is the road; closed is its state")
}
