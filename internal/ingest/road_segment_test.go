package ingest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/hazards"
	"github.com/sierragridteam/sierra-data/internal/services"
	"github.com/sierragridteam/sierra-data/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeRoadLister is the roads service's current set. at is the refresh it
// came from (zero: now).
type fakeRoadLister struct {
	roads []*api.Road
	at    time.Time
	err   error
	// failed is the service's RoadInputHealth: status inputs it could not
	// read at its last refresh.
	failed []services.RoadInputFailure
}

func (f *fakeRoadLister) RoadInputHealth() ([]services.RoadInputFailure, time.Time) {
	return f.failed, f.at
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

// A status input the roads service could not read is reported on the roads
// source, with what failed and since when. The events are still written:
// they come from the service's last good read of that input.
func TestRoadSegmentNormalizer_ReportsFailedStatusInputs(t *testing.T) {
	cfg := roadSegmentConfig()
	cfg.Roads.RefreshInterval = 15 * time.Minute
	now := time.Date(2026, 11, 18, 17, 21, 0, 0, time.UTC)
	closed := road("hwy4-arnold-bearvalley", api.RoadStatus_CLOSED, api.CongestionLevel_CLEAR, 38)
	closed.StatusCauses = []*api.RoadStatusCause{conditionCause(api.RoadStatus_CLOSED, "Is closed from Camp Connell to Lake Alpine - Due to snow")}
	closed.Beyond = []*api.RoadBeyond{{Status: api.RoadStatus_CLOSED, Side: "END", Name: "Ebbetts Pass winter closure"}}
	lister := &fakeRoadLister{at: now.Add(-time.Minute), roads: []*api.Road{
		closed,
		road("hwy49-angels-sonora", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 25),
	}}
	n := NewRoadSegmentNormalizer(cfg, lister, nil)
	n.now = func() time.Time { return now }

	first, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Empty(t, first.PerSource)
	require.Len(t, first.Events, 2)

	// The next refresh could not read Hwy 4's conditions or CHP, and went on
	// with the last good read of each: the same roads, a later refresh.
	now = now.Add(15 * time.Minute)
	lister.at = now.Add(-time.Minute)
	lister.failed = []services.RoadInputFailure{
		{Input: "CHP incidents", Since: lister.at, LastGoodUsed: true, Err: errors.New("HTTP error 500 downloading KML")},
		{Input: "road conditions for Hwy 4", Since: lister.at, LastGoodUsed: true, Err: errors.New("HTTP error 503 fetching road conditions for highway 4")},
	}
	res, err := n.Poll(testCtx(), &scriptedPrior{events: first.Events})
	require.NoError(t, err, "a failed input degrades the source; it is not a failed poll")
	require.Error(t, res.PerSource[RoadSegmentSourceID])
	assert.Equal(t,
		"road segments: CHP incidents unavailable since 17:35Z; last good read used (HTTP error 500 downloading KML); "+
			"road conditions for Hwy 4 unavailable since 17:35Z; last good read used (HTTP error 503 fetching road conditions for highway 4)",
		res.PerSource[RoadSegmentSourceID].Error())

	require.Len(t, res.Events, 2, "every road is still written")
	hwy4 := res.Events[0].GetRoadSegment()
	assert.Equal(t, "CLOSED", hwy4.GetStatus(), "from the last good read")
	require.Len(t, hwy4.GetBeyond(), 1)
	assert.Equal(t, store.ContentHash(first.Events[0]), store.ContentHash(res.Events[0]), "the same state: no revision")
	assert.Equal(t, lister.at.Unix(), res.Events[0].GetObservedAt().AsTime().Unix())

	// Past the stale window the service's refresh fails and it serves the
	// set of an earlier one. Both facts are on the source.
	now = now.Add(45 * time.Minute)
	lister.failed[1].LastGoodUsed = false
	res, err = n.Poll(testCtx(), &scriptedPrior{events: first.Events})
	require.NoError(t, err)
	msg := res.PerSource[RoadSegmentSourceID].Error()
	assert.Contains(t, msg, "road status last refreshed 46m0s ago")
	assert.Contains(t, msg, "road conditions for Hwy 4 unavailable since 17:35Z; no last good read to use, road status not refreshed")
	require.Len(t, res.Events, 2)
	assert.Equal(t, "CLOSED", res.Events[0].GetRoadSegment().GetStatus(), "the events keep their state")

	// The inputs are read again: the source is clean.
	lister.failed, lister.at = nil, now.Add(-time.Minute)
	res, err = n.Poll(testCtx(), &scriptedPrior{events: first.Events})
	require.NoError(t, err)
	assert.Empty(t, res.PerSource)
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

// modelText is what the roads service says of an incident or a lane
// closure: a model's words, different at every refresh.
func modelText() string { return "Roadway fully closed, as a model put it at " + time.Now().String() }

func incidentCause(status api.RoadStatus, logNumber string) *api.RoadStatusCause {
	return &api.RoadStatusCause{Status: status, Source: "incident", SourceId: logNumber,
		Title: "CHP Incident " + logNumber, Description: modelText(),
		Begin: &api.Coordinates{Latitude: 38.19 + float64(time.Now().Nanosecond())*1e-12, Longitude: -120.37}}
}

func conditionCause(status api.RoadStatus, text string) *api.RoadStatusCause {
	return &api.RoadStatusCause{Status: status, Source: "road_condition", Description: text}
}

func oneRoadNormalizer(lister *fakeRoadLister) *RoadSegmentNormalizer {
	cfg := roadSegmentConfig()
	cfg.Roads.MonitoredRoads = cfg.Roads.MonitoredRoads[:1]
	return NewRoadSegmentNormalizer(cfg, lister, nil)
}

// A road closed by a CHP incident says which incident, by the id the map
// layer gives it, and carries none of the roads service's words for it: they
// are a model's and change between refreshes.
func TestRoadSegmentNormalizer_IncidentCause(t *testing.T) {
	closed := road("hwy4-arnold-bearvalley", api.RoadStatus_CLOSED, api.CongestionLevel_CLEAR, 0)
	closed.StatusCauses = []*api.RoadStatusCause{incidentCause(api.RoadStatus_CLOSED, "261118ST0331")}
	lister := &fakeRoadLister{roads: []*api.Road{closed}}
	n := oneRoadNormalizer(lister)
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	ev := res.Events[0]

	causes := ev.GetRoadSegment().GetCauses()
	require.Len(t, causes, 1)
	assert.Equal(t, "CLOSED", causes[0].GetStatus())
	assert.Equal(t, "incident", causes[0].GetSource())
	assert.Equal(t, "chp:261118ST0331", causes[0].GetEventId())
	assert.Equal(t, hazards.RoadCauseEventID(closed.StatusCauses[0]), causes[0].GetEventId(), "the map layer's id")
	assert.Empty(t, causes[0].GetText(), "an incident's wording is a model's")
	assert.Empty(t, ev.GetDescription())
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: closed", ev.GetHeadline(), "no reason in the headline")
	raw, err := protojson.Marshal(ev)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "model", "no model text anywhere on the event")
	assert.NotContains(t, string(raw), "CHP Incident")

	// The next refresh rewords the incident and moves its point: no change.
	closed.StatusCauses = []*api.RoadStatusCause{incidentCause(api.RoadStatus_CLOSED, "261118ST0331")}
	again, err := n.Poll(testCtx(), &scriptedPrior{events: res.Events})
	require.NoError(t, err)
	assert.Equal(t, store.ContentHash(ev), store.ContentHash(again.Events[0]))

	// A lane closure names no event (the map layer gives it none) and keeps
	// no text either.
	closed.StatusCauses = []*api.RoadStatusCause{{Status: api.RoadStatus_RESTRICTED, Source: "lane_closure", SourceId: "C4TA", Description: modelText()}}
	closed.Status = api.RoadStatus_RESTRICTED
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	lc := res.Events[0].GetRoadSegment().GetCauses()[0]
	assert.Equal(t, "lane_closure", lc.GetSource())
	assert.Empty(t, lc.GetEventId())
	assert.Empty(t, lc.GetText())

	// An open road has no causes, whatever the service lists (the map
	// layer's rule).
	closed.Status = api.RoadStatus_OPEN
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Empty(t, res.Events[0].GetRoadSegment().GetCauses())
}

// A road restricted by Caltrans's highway conditions carries the condition
// text, as the cause's text and as the event's description.
func TestRoadSegmentNormalizer_RoadConditionCause(t *testing.T) {
	const oneWay = "1-WAY CONTROLLED TRAFFIC AT VARIOUS LOCATIONS FROM ARNOLD TO DORRINGTON /CALAVERAS CO/ - DUE TO TREE WORK"
	const snow = "IS CLOSED FROM CAMP CONNELL TO BEAR VALLEY /CALAVERAS CO/ - DUE TO SNOW"
	r := road("hwy4-arnold-bearvalley", api.RoadStatus_RESTRICTED, api.CongestionLevel_CLEAR, 40)
	r.StatusCauses = []*api.RoadStatusCause{conditionCause(api.RoadStatus_RESTRICTED, "1-WAY CONTROLLED TRAFFIC  AT VARIOUS LOCATIONS FROM ARNOLD TO DORRINGTON /CALAVERAS CO/ - DUE TO TREE WORK\n")}
	lister := &fakeRoadLister{roads: []*api.Road{r}}
	n := oneRoadNormalizer(lister)
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	ev := res.Events[0]
	causes := ev.GetRoadSegment().GetCauses()
	require.Len(t, causes, 1)
	assert.Equal(t, "RESTRICTED", causes[0].GetStatus())
	assert.Equal(t, "road_condition", causes[0].GetSource())
	assert.Empty(t, causes[0].GetEventId())
	assert.Equal(t, oneWay, causes[0].GetText(), "Caltrans's text, whitespace collapsed")
	assert.Equal(t, oneWay, ev.GetDescription())
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: restricted", ev.GetHeadline(), "the headline is unchanged")

	// With an incident beside two conditions, the description is the
	// conditions only, closures first.
	r.Status = api.RoadStatus_CLOSED
	r.StatusCauses = []*api.RoadStatusCause{
		conditionCause(api.RoadStatus_RESTRICTED, oneWay),
		incidentCause(api.RoadStatus_CLOSED, "261118ST0331"),
		conditionCause(api.RoadStatus_CLOSED, snow),
	}
	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Equal(t, snow+"\n\n"+oneWay, res.Events[0].GetDescription())
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: closed", res.Events[0].GetHeadline())
}

// The roads service promises no order of causes and can list one twice. The
// same causes in another order, through the scheduler, write no revision.
func TestRoadSegmentCauses_OrderAndDuplicatesAreNotAChange(t *testing.T) {
	ctx := testCtx()
	cfg := roadSegmentConfig()
	cfg.Roads.MonitoredRoads = cfg.Roads.MonitoredRoads[:1]
	st := newSchedStore(t)
	seedSchedSources(t, st, RoadSegmentSourceID)
	causes := func() []*api.RoadStatusCause {
		return []*api.RoadStatusCause{
			conditionCause(api.RoadStatus_RESTRICTED, "CHAINS OR SNOW TIRES ARE REQUIRED"),
			incidentCause(api.RoadStatus_CLOSED, "261118ST0331"),
			incidentCause(api.RoadStatus_RESTRICTED, "261118ST0007"),
			conditionCause(api.RoadStatus_CLOSED, "IS CLOSED DUE TO SNOW"),
		}
	}
	r := road("hwy4-arnold-bearvalley", api.RoadStatus_CLOSED, api.CongestionLevel_CLEAR, 0)
	r.StatusCauses = causes()
	t0 := time.Now().Add(-40 * time.Minute)
	lister := &fakeRoadLister{at: t0, roads: []*api.Road{r}}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{RoadSegmentSourceID: {Disappearance: store.DisappearanceResolve}},
	})
	ps := &pollerState{}
	spec := PollerSpec{Normalizer: NewRoadSegmentNormalizer(cfg, lister, nil), Interval: time.Minute}
	const id = "road:hwy4-arnold-bearvalley"
	sched.tick(ctx, spec, ps)
	require.Equal(t, uint32(1), eventRevision(t, st, id))

	for i, at := range []time.Duration{15 * time.Minute, 30 * time.Minute} {
		c := causes()
		slices.Reverse(c)
		if i == 1 {
			c = append(c[2:], append(causes(), c[:2]...)...) // rotated, and every cause twice
		}
		lister.at, r.StatusCauses = t0.Add(at), c
		sched.tick(ctx, spec, ps)
		assert.Equal(t, uint32(1), eventRevision(t, st, id), "the same causes, reordered and reworded")
	}

	got, err := st.GetEvent(ctx, id)
	require.NoError(t, err)
	var order []string
	for _, c := range got.GetRoadSegment().GetCauses() {
		order = append(order, c.GetStatus()+" "+c.GetSource()+" "+c.GetEventId()+c.GetText())
	}
	assert.Equal(t, []string{
		"CLOSED incident chp:261118ST0331",
		"CLOSED road_condition IS CLOSED DUE TO SNOW",
		"RESTRICTED incident chp:261118ST0007",
		"RESTRICTED road_condition CHAINS OR SNOW TIRES ARE REQUIRED",
	}, order, "closures first, then source, event, text")
}

// A cause that is no longer listed while the status holds is what a failed
// CHP or conditions fetch looks like: it waits for a second refresh, like a
// reopening. A cause appearing, or anything more restrictive, is recorded at
// once.
func TestRoadSegmentNormalizer_CauseEasing(t *testing.T) {
	t0 := time.Now().Add(-100 * time.Minute).Truncate(time.Second)
	fire := func() *api.RoadStatusCause { return incidentCause(api.RoadStatus_CLOSED, "261118ST0331") }
	snow := func() *api.RoadStatusCause { return conditionCause(api.RoadStatus_CLOSED, "IS CLOSED DUE TO SNOW") }
	r := road("hwy4-arnold-bearvalley", api.RoadStatus_CLOSED, api.CongestionLevel_CLEAR, 0)
	r.StatusCauses = []*api.RoadStatusCause{fire(), snow()}
	lister := &fakeRoadLister{at: t0, roads: []*api.Road{r}}
	n := oneRoadNormalizer(lister)
	first, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	prior := &scriptedPrior{events: first.Events}
	poll := func(at time.Duration, status api.RoadStatus, causes ...*api.RoadStatusCause) []string {
		t.Helper()
		lister.at, r.Status, r.StatusCauses = t0.Add(at), status, causes
		res, err := n.Poll(testCtx(), prior)
		require.NoError(t, err)
		d := res.Events[0].GetRoadSegment()
		out := []string{d.GetStatus()}
		for _, c := range d.GetCauses() {
			out = append(out, c.GetStatus()+" "+c.GetSource())
		}
		prior = &scriptedPrior{events: res.Events} // what the store now holds
		return out
	}
	both := []string{"CLOSED", "CLOSED incident", "CLOSED road_condition"}

	// The incident is gone for one refresh (a failed CHP fetch); the road is
	// still closed for snow. Held, then back: nothing was written.
	assert.Equal(t, both, poll(15*time.Minute, api.RoadStatus_CLOSED, snow()), "held")
	assert.Equal(t, both, poll(15*time.Minute, api.RoadStatus_CLOSED, snow()), "the same refresh again is not a second opinion")
	assert.Equal(t, both, poll(30*time.Minute, api.RoadStatus_CLOSED, snow(), fire()))

	// Gone for two refreshes: recorded at the second.
	assert.Equal(t, both, poll(45*time.Minute, api.RoadStatus_CLOSED, snow()), "first sight")
	assert.Equal(t, []string{"CLOSED", "CLOSED road_condition"}, poll(60*time.Minute, api.RoadStatus_CLOSED, snow()), "confirmed")

	// A cause appearing is recorded at once.
	assert.Equal(t, both, poll(75*time.Minute, api.RoadStatus_CLOSED, snow(), fire()), "at once")

	// A cause that sets less than it did, the status unchanged, eases: held.
	assert.Equal(t, both, poll(90*time.Minute, api.RoadStatus_CLOSED, snow(), incidentCause(api.RoadStatus_RESTRICTED, "261118ST0331")), "held")
	// One cause gone and another come: recorded at once, whatever it eases.
	assert.Equal(t, both, poll(105*time.Minute, api.RoadStatus_CLOSED, fire(), conditionCause(api.RoadStatus_CLOSED, "IS CLOSED DUE TO A ROCK SLIDE")), "a new cause")
	assert.Equal(t, "IS CLOSED DUE TO A ROCK SLIDE", prior.events[0].GetDescription(), "snow went and the slide came: recorded at once")

	// More restrictive, with a cause gone at the same refresh: at once.
	n = oneRoadNormalizer(lister)
	lister.at, r.Status, r.StatusCauses = t0, api.RoadStatus_RESTRICTED, []*api.RoadStatusCause{conditionCause(api.RoadStatus_RESTRICTED, "CHAINS REQUIRED"), incidentCause(api.RoadStatus_RESTRICTED, "261118ST0007")}
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	prior = &scriptedPrior{events: res.Events}
	assert.Equal(t, []string{"CLOSED", "CLOSED incident"}, poll(15*time.Minute, api.RoadStatus_CLOSED, fire()), "closed at once, though two causes went")

	// Reopening drops every cause with the status, and waits like any easing.
	assert.Equal(t, []string{"CLOSED", "CLOSED incident"}, poll(30*time.Minute, api.RoadStatus_OPEN), "held")
	assert.Equal(t, []string{"OPEN"}, poll(45*time.Minute, api.RoadStatus_OPEN), "confirmed")
}
