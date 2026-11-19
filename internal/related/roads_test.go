package related

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// roadEvent is a monitored road's event as the ingest poller writes it: no
// shape, no effective time, and the causes of its status.
func roadEvent(id, status string, causes ...*gridv1.RoadSegmentCause) *gridv1.Event {
	return &gridv1.Event{Id: "road:" + id, Layer: gridv1.Layer_ROAD_SEGMENT, Status: gridv1.EventStatus_ACTIVE,
		Severity: gridv1.Severity_SEVERE, Headline: id + ": " + status, ObservedAt: ts(0),
		Provenance: &gridv1.Provenance{SourceId: "roads", SourceName: "roads"},
		Detail:     &gridv1.Event_RoadSegment{RoadSegment: &gridv1.RoadSegmentDetail{RoadId: id, Status: status, Causes: causes}}}
}

// A road closed by a CHP incident lists the incident, and the incident lists
// the road: the road's event names it. Nothing else about the two decides it
// (the road has no shape; the incident began a day before the road's
// current revision).
func TestRoadAndTheEventsThatSetItsStatus(t *testing.T) {
	fire := ev("chp:261118ST0331", gridv1.Layer_ROAD_INCIDENT, "incident", "Full roadway closure for structure fire", -30, point(38.19, -120.37))
	other := ev("chp:261118ST0400", gridv1.Layer_ROAD_INCIDENT, "incident", "Traffic hazard", -1, point(38.20, -120.36))
	hwy4 := roadEvent("hwy4-murphys-arnold", "CLOSED",
		&gridv1.RoadSegmentCause{Status: "CLOSED", Source: "incident", EventId: "chp:261118ST0331"},
		&gridv1.RoadSegmentCause{Status: "RESTRICTED", Source: "incident", EventId: "chp:never-stored"},
		&gridv1.RoadSegmentCause{Status: "CLOSED", Source: "road_condition", Text: "IS CLOSED DUE TO SNOW"},
		&gridv1.RoadSegmentCause{Status: "RESTRICTED", Source: "lane_closure"})
	hwy49 := roadEvent("hwy49-angels-sonora", "OPEN")
	e, st := newEngine(t)
	ctx := context.Background()
	require.NoError(t, st.SeedSources(ctx, []store.SourceSeed{{ID: "roads", Name: "roads", PollInterval: time.Minute}}))
	for _, x := range []*gridv1.Event{fire, other, hwy4, hwy49} {
		_, err := st.UpsertEvent(ctx, x)
		require.NoError(t, err)
	}

	// Road -> incident. The cause whose event was never stored, and the
	// causes that are no event, are not listed.
	rs, err := e.Related(ctx, hwy4)
	require.NoError(t, err)
	require.Len(t, rs, 1)
	assert.Equal(t, "chp:261118ST0331", rs[0].Event.GetId())
	assert.Equal(t, RuleRoadStatusCause, rs[0].Rule)
	assert.Equal(t, High, rs[0].Confidence)
	assert.Equal(t, "Set this road's status: closed", rs[0].Reason)
	assert.Empty(t, rs[0].Event.GetGeometry().GetGeojson(), "no shape, as every related event")
	assert.NotNil(t, rs[0].Event.GetGeometry().GetCentroid())

	// Incident -> road.
	rs, err = e.Related(ctx, fire)
	require.NoError(t, err)
	require.Len(t, rs, 1)
	assert.Equal(t, "road:hwy4-murphys-arnold", rs[0].Event.GetId())
	assert.Equal(t, RuleRoadStatusCause, rs[0].Rule)
	assert.Equal(t, High, rs[0].Confidence)
	assert.Equal(t, "This road's status (closed) was set by this report", rs[0].Reason)

	// An incident no road names, and a road with no causes, relate to neither.
	rs, err = e.Related(ctx, other)
	require.NoError(t, err)
	assert.Empty(t, rs)
	rs, err = e.Related(ctx, hwy49)
	require.NoError(t, err)
	assert.Empty(t, rs)

	// It ranks with the other High relations and above the rest: a storm
	// the incident sits inside is context, listed after the road.
	snow := ev("wx:snow", gridv1.Layer_WEATHER_ALERT, "Winter Storm Warning", "Winter Storm Warning", -40, box(38.19, -120.37, 0.2))
	snow.Expires = ts(48)
	_, err = st.UpsertEvent(ctx, snow)
	require.NoError(t, err)
	rs, err = e.Related(ctx, fire)
	require.NoError(t, err)
	require.Len(t, rs, 2)
	assert.Equal(t, "road:hwy4-murphys-arnold", rs[0].Event.GetId(), "strongest first")
	assert.Equal(t, "weather-road", rs[1].Rule)
}

// The link follows the road's current causes: when the road's event drops
// the cause, neither side lists the other.
func TestRoadCauseRelationEndsWithTheCause(t *testing.T) {
	fire := ev("chp:261118ST0331", gridv1.Layer_ROAD_INCIDENT, "incident", "Full roadway closure for structure fire", -30, point(38.19, -120.37))
	hwy4 := roadEvent("hwy4-murphys-arnold", "CLOSED", &gridv1.RoadSegmentCause{Status: "CLOSED", Source: "incident", EventId: "chp:261118ST0331"})
	e, st := newEngine(t)
	ctx := context.Background()
	require.NoError(t, st.SeedSources(ctx, []store.SourceSeed{{ID: "roads", Name: "roads", PollInterval: time.Minute}}))
	for _, x := range []*gridv1.Event{fire, hwy4, roadEvent("hwy4-murphys-arnold", "OPEN")} {
		_, err := st.UpsertEvent(ctx, x)
		require.NoError(t, err)
	}
	rs, err := e.Related(ctx, fire)
	require.NoError(t, err)
	assert.Empty(t, rs)
	open, err := st.GetEvent(ctx, "road:hwy4-murphys-arnold")
	require.NoError(t, err)
	rs, err = e.Related(ctx, open)
	require.NoError(t, err)
	assert.Empty(t, rs)
}

// A road under a chain requirement lists the stretch, and the stretch lists
// the road: the road's event names it. A stretch named and never stored (one
// with no sign inside a hazard area) is not listed, and the link ends when
// the road's event drops the chains.
func TestRoadAndTheChainRequirementOnIt(t *testing.T) {
	stretch := ev("cc:hwy4:r2", gridv1.Layer_CHAIN_CONTROL, "r2", "Hwy 4 chains required, Blue Lake Springs to Mount Reba Road (R2)", -6, point(38.27, -120.31))
	stretch.Provenance = &gridv1.Provenance{SourceId: "caltrans-cc", SourceName: "caltrans-cc"}
	under := func() *gridv1.Event {
		r := roadEvent("hwy4-arnold-bearvalley", "OPEN")
		r.GetRoadSegment().Chains = []*gridv1.RoadSegmentChain{{Level: "R2", EventId: "cc:hwy4:r2"}, {Level: "R1", EventId: "cc:hwy4:r1"}}
		return r
	}
	below := roadEvent("hwy4-murphys-arnold", "OPEN")
	e, st := newEngine(t)
	ctx := context.Background()
	require.NoError(t, st.SeedSources(ctx, []store.SourceSeed{
		{ID: "roads", Name: "roads", PollInterval: time.Minute}, {ID: "caltrans-cc", Name: "caltrans-cc", PollInterval: time.Minute}}))
	for _, x := range []*gridv1.Event{stretch, under(), below} {
		_, err := st.UpsertEvent(ctx, x)
		require.NoError(t, err)
	}

	rs, err := e.Related(ctx, under())
	require.NoError(t, err)
	require.Len(t, rs, 1, "cc:hwy4:r1 was never stored")
	assert.Equal(t, "cc:hwy4:r2", rs[0].Event.GetId())
	assert.Equal(t, RuleRoadChains, rs[0].Rule)
	assert.Equal(t, High, rs[0].Confidence)
	assert.Equal(t, "Chain requirement on this road: R2", rs[0].Reason)

	rs, err = e.Related(ctx, stretch)
	require.NoError(t, err)
	require.Len(t, rs, 1, "the road below is not under it")
	assert.Equal(t, "road:hwy4-arnold-bearvalley", rs[0].Event.GetId())
	assert.Equal(t, RuleRoadChains, rs[0].Rule)
	assert.Equal(t, "This road is under this chain requirement", rs[0].Reason)

	// Chains lifted: the road's event no longer names the stretch.
	_, err = st.UpsertEvent(ctx, roadEvent("hwy4-arnold-bearvalley", "OPEN"))
	require.NoError(t, err)
	rs, err = e.Related(ctx, stretch)
	require.NoError(t, err)
	assert.Empty(t, rs)
}
