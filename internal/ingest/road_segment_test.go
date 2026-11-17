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
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeRoadLister is the roads service's current set. at is the refresh it
// came from (zero: now).
type fakeRoadLister struct {
	roads []*api.Road
	at    time.Time
	err   error
}

func (f *fakeRoadLister) ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	at := f.at
	if at.IsZero() {
		at = time.Now()
	}
	return &api.ListRoadsResponse{Roads: f.roads, LastUpdated: timestamppb.New(at)}, nil
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
	refreshed := time.Now().Add(-4 * time.Minute).Truncate(time.Second).UTC()
	lister := &fakeRoadLister{at: refreshed, roads: []*api.Road{
		road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 38),
		road("hwy49-angels-sonora", api.RoadStatus_CLOSED, api.CongestionLevel_CLEAR, 25),
	}}
	lister.roads[0].Beyond = []*api.RoadBeyond{{Status: api.RoadStatus_CLOSED, Side: "END", Name: "Ebbetts Pass winter closure", Description: "text that varies"}}
	n := NewRoadSegmentNormalizer(roadSegmentConfig(), lister, func(_ context.Context, lat, _ float64) ([]string, error) {
		// Each end is in a county, in a town, and within another road's
		// corridor buffer (roads meet in towns).
		if lat > 38.4 {
			return []string{"county:alpine-county", "corridor:hwy4-arnold-bearvalley"}, nil
		}
		return []string{"county:calaveras-county", "town:arnold", "corridor:hwy4-murphys-arnold", "corridor:hwy4-arnold-bearvalley"}, nil
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
	assert.Equal(t, refreshed, hwy4.GetObservedAt().AsTime(), "observed at the refresh, not at this poll")
	assert.Equal(t, []string{"corridor:hwy4-arnold-bearvalley", "county:alpine-county", "county:calaveras-county", "town:arnold"}, hwy4.GetPlaceIds(),
		"its own corridor, never a neighbour's")
	d := hwy4.GetRoadSegment()
	assert.Equal(t, "OPEN", d.GetStatus())
	require.Len(t, d.GetBeyond(), 1)
	assert.Equal(t, "Ebbetts Pass winter closure", d.GetBeyond()[0].GetName())
	assert.Equal(t, "CLOSED", d.GetBeyond()[0].GetStatus())

	closed := res.Events[1]
	assert.Equal(t, gridv1.Severity_SEVERE, closed.GetSeverity())
	assert.Equal(t, "Hwy 49 Angels Camp to Sonora: closed", closed.GetHeadline())

	// Travel time, congestion and a reworded explanation are samples and
	// prose, not state: traffic going from clear to heavy writes nothing.
	lister.roads[0].DurationMinutes, lister.roads[0].DelayMinutes = 55, 17
	lister.roads[0].CongestionLevel = api.CongestionLevel_HEAVY
	lister.roads[0].StatusExplanation = "reworded"
	again, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Equal(t, store.ContentHash(hwy4), store.ContentHash(again.Events[0]))
	assert.Equal(t, gridv1.Severity_INFO, again.Events[0].GetSeverity())

	// A closure is, at once.
	lister.roads[0].Status = api.RoadStatus_CLOSED
	changed, err := n.Poll(testCtx(), &scriptedPrior{events: res.Events})
	require.NoError(t, err)
	assert.Equal(t, "CLOSED", changed.Events[0].GetRoadSegment().GetStatus())
}

// A change that eases a road waits for a second refresh. The roads service
// reads a failed upstream fetch as "nothing reported", which is OPEN with no
// seasonal closure: one refresh of good news after bad is what that looks
// like, and written down it is a reopening that never happened.
func TestRoadSegmentNormalizer_EasingWaitsForASecondRefresh(t *testing.T) {
	cfg := roadSegmentConfig()
	cfg.Roads.MonitoredRoads = cfg.Roads.MonitoredRoads[:1]
	t0 := time.Now().Add(-40 * time.Minute).Truncate(time.Second)
	winter := []*api.RoadBeyond{{Status: api.RoadStatus_CLOSED, Side: "END", Name: "Ebbetts Pass winter closure"}}
	closedRoad := func() *api.Road {
		r := road("hwy4-arnold-bearvalley", api.RoadStatus_CLOSED, api.CongestionLevel_CLEAR, 0)
		r.Beyond = winter
		return r
	}
	lister := &fakeRoadLister{at: t0, roads: []*api.Road{closedRoad()}}
	n := NewRoadSegmentNormalizer(cfg, lister, nil)
	first, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	prior := &scriptedPrior{events: first.Events}
	state := func(res *PollResult) (string, int) {
		d := res.Events[0].GetRoadSegment()
		return d.GetStatus(), len(d.GetBeyond())
	}

	// One refresh says open, no winter closure (a failed conditions fetch).
	lister.at, lister.roads = t0.Add(15*time.Minute), []*api.Road{road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 30)}
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	status, beyond := state(res)
	assert.Equal(t, "CLOSED", status, "held")
	assert.Equal(t, 1, beyond)
	assert.Equal(t, store.ContentHash(first.Events[0]), store.ContentHash(res.Events[0]))
	// The same refresh offered again (the poller ticks faster than the roads
	// refresh) is not a second opinion.
	res, err = n.Poll(testCtx(), prior)
	require.NoError(t, err)
	status, _ = state(res)
	assert.Equal(t, "CLOSED", status)

	// The next refresh is back to closed: nothing was ever written.
	lister.at, lister.roads = t0.Add(30*time.Minute), []*api.Road{closedRoad()}
	res, err = n.Poll(testCtx(), prior)
	require.NoError(t, err)
	assert.Equal(t, store.ContentHash(first.Events[0]), store.ContentHash(res.Events[0]))

	// A real reopening: two refreshes agree, and the second records it.
	open := road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 30)
	open.Beyond = winter
	lister.at, lister.roads = t0.Add(45*time.Minute), []*api.Road{open}
	res, err = n.Poll(testCtx(), prior)
	require.NoError(t, err)
	status, _ = state(res)
	assert.Equal(t, "CLOSED", status, "first sight")
	lister.at = t0.Add(60 * time.Minute)
	res, err = n.Poll(testCtx(), prior)
	require.NoError(t, err)
	status, beyond = state(res)
	assert.Equal(t, "OPEN", status, "confirmed")
	assert.Equal(t, 1, beyond)

	// Restricting is never held, even when something else eases with it.
	prior = &scriptedPrior{events: res.Events}
	lister.at, lister.roads = t0.Add(75*time.Minute), []*api.Road{road("hwy4-arnold-bearvalley", api.RoadStatus_CLOSED, api.CongestionLevel_CLEAR, 0)}
	res, err = n.Poll(testCtx(), prior)
	require.NoError(t, err)
	status, beyond = state(res)
	assert.Equal(t, "CLOSED", status, "closed at once")
	assert.Equal(t, 0, beyond)
}

// The roads service goes on serving its last set while its refresh fails. A
// set older than twice the refresh interval is not a reading of the roads
// now: the events keep their state and the source says why.
func TestRoadSegmentNormalizer_StaleSetIsNotAReading(t *testing.T) {
	cfg := roadSegmentConfig()
	cfg.Roads.MonitoredRoads = cfg.Roads.MonitoredRoads[:1]
	cfg.Roads.RefreshInterval = 15 * time.Minute
	lister := &fakeRoadLister{roads: []*api.Road{road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 38)}}
	n := NewRoadSegmentNormalizer(cfg, lister, nil)
	first, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Empty(t, first.PerSource)

	lister.at = time.Now().Add(-80 * time.Minute)
	lister.roads[0].Status = api.RoadStatus_CLOSED
	res, err := n.Poll(testCtx(), &scriptedPrior{events: first.Events})
	require.NoError(t, err)
	require.Error(t, res.PerSource[RoadSegmentSourceID])
	assert.Contains(t, res.PerSource[RoadSegmentSourceID].Error(), "last refreshed")
	require.Len(t, res.Events, 1, "still listed, so nothing is swept either way")
	assert.Equal(t, "OPEN", res.Events[0].GetRoadSegment().GetStatus(), "an old set changes nothing")
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

	t0 := time.Now().Add(-40 * time.Minute)
	lister.at, lister.roads[0].Status = t0, api.RoadStatus_CLOSED
	sched.tick(ctx, spec, ps)
	// Reopening takes two refreshes that agree.
	lister.at, lister.roads[0].Status = t0.Add(15*time.Minute), api.RoadStatus_OPEN
	sched.tick(ctx, spec, ps)
	assert.Equal(t, uint32(2), eventRevision(t, st, id), "one refresh saying open is not yet a reopening")
	lister.at = t0.Add(30 * time.Minute)
	sched.tick(ctx, spec, ps)

	revs, _, err := st.EventHistory(ctx, id, 10, "")
	require.NoError(t, err)
	require.Len(t, revs, 3)
	assert.Equal(t, "OPEN", revs[0].GetEvent().GetRoadSegment().GetStatus())
	assert.Equal(t, "CLOSED", revs[1].GetEvent().GetRoadSegment().GetStatus())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, revs[1].GetEvent().GetStatus(), "the event is the road; closed is its state")
}
