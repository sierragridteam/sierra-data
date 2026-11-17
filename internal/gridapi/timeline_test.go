package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

func at(min int) *timestamppb.Timestamp {
	return timestamppb.New(time.Date(2026, 11, 10, 19, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute))
}

func rev(n uint32, ingested int, ev *gridv1.Event) *gridv1.EventRevision {
	c := proto.Clone(ev).(*gridv1.Event)
	c.Revision = n
	return &gridv1.EventRevision{Revision: n, IngestedAt: at(ingested), ObservedAt: at(ingested), Event: c}
}

func labels(t *gridv1.EventTimeline) []string {
	var out []string
	for _, e := range t.GetEntries() {
		out = append(out, e.GetLabel())
	}
	return out
}

// A CHP incident: the report time and each log line are the source's clock;
// first stored and resolved are the Grid's. Same instant: source first.
func TestTimelineCHPIncident(t *testing.T) {
	ev := &gridv1.Event{
		Id: "chp:1", Layer: gridv1.Layer_ROAD_INCIDENT, Status: gridv1.EventStatus_ACTIVE, Severity: gridv1.Severity_MINOR,
		Effective:  at(21),
		Provenance: &gridv1.Provenance{SourceName: "CHP / Caltrans"},
		Detail: &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{Log: []*gridv1.IncidentLogEntry{
			{Number: 1, At: at(21), Text: "1183-Trfc Collision-Unkn Inj"},
			{Number: 2, At: at(28), Text: "TOW ENRT"},
		}}},
	}
	resolved := proto.Clone(ev).(*gridv1.Event)
	resolved.Status = gridv1.EventStatus_RESOLVED
	resolved.Revision = 3
	tl := buildTimeline(resolved, []*gridv1.EventRevision{rev(3, 70, resolved), rev(1, 28, ev), rev(2, 41, ev)}, nil, false)

	assert.Equal(t, []string{"Reported", "1183-Trfc Collision-Unkn Inj", "TOW ENRT", "First recorded by the Grid", "Resolved"}, labels(tl))
	e := tl.GetEntries()
	assert.Equal(t, gridv1.TimelineClock_TIMELINE_CLOCK_SOURCE, e[0].GetClock())
	assert.Equal(t, "CHP / Caltrans", e[0].GetDetail())
	assert.Equal(t, "Log line 2", e[2].GetDetail())
	assert.Equal(t, gridv1.TimelineClock_TIMELINE_CLOCK_SOURCE, e[2].GetClock(), "at 19:28 the log line precedes the Grid's first record")
	assert.Equal(t, gridv1.TimelineClock_TIMELINE_CLOCK_GRID, e[3].GetClock())
	assert.Equal(t, "2 changes since", e[3].GetDetail(), "revision 1 is the first record itself")
	last := e[4]
	assert.Equal(t, gridv1.TimelineKind_TIMELINE_STATUS, last.GetKind())
	assert.Equal(t, uint32(3), last.GetRevision())
	assert.Equal(t, "Active", last.GetBefore())
	assert.Equal(t, "Resolved", last.GetAfter())
}

func TestTimelineLaneClosureAndFire(t *testing.T) {
	closure := &gridv1.Event{
		Id: "caltrans:d10-1", Layer: gridv1.Layer_ROAD_INCIDENT, Effective: at(0),
		Detail: &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{Closure: &gridv1.LaneClosureDetail{
			PlannedStart: at(0), SetUpAt: at(12), PlannedEnd: at(600),
		}}},
	}
	assert.Equal(t, []string{"Planned start", "Set up on the road", "Planned end"}, labels(buildTimeline(closure, nil, nil, false)))

	fire := &gridv1.Event{Id: "calfire:1", Layer: gridv1.Layer_WILDFIRE, Effective: at(0), Severity: gridv1.Severity_SEVERE,
		Detail: &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{Acres: 120, Containment: 0}}}
	grown := proto.Clone(fire).(*gridv1.Event)
	grown.GetWildfire().Acres, grown.GetWildfire().Containment = 450.25, 15
	tl := buildTimeline(grown, []*gridv1.EventRevision{rev(1, 5, fire), rev(2, 90, grown)}, nil, false)
	assert.Equal(t, []string{"Started", "First recorded by the Grid", "Size 450.3 acres", "Containment 15%"}, labels(tl))
	assert.Equal(t, "120 acres", tl.GetEntries()[2].GetBefore())
}

// Truncated history: the page is the newest changes as a block, so nothing is
// diffed against revision 1 across the cut, and FIRST_RECORDED still comes
// from revision 1 when the caller supplies it.
func TestTimelineTruncated(t *testing.T) {
	ev := &gridv1.Event{Id: "x", Layer: gridv1.Layer_EARTHQUAKE, Effective: at(0), Revision: 900}
	other := proto.Clone(ev).(*gridv1.Event)
	other.Severity = gridv1.Severity_SEVERE
	first := proto.Clone(ev).(*gridv1.Event)
	first.Severity = gridv1.Severity_MINOR
	tl := buildTimeline(ev, []*gridv1.EventRevision{rev(900, 50, other), rev(899, 40, other)}, rev(1, 1, first), true)
	assert.True(t, tl.GetTruncated())
	assert.Equal(t, []string{"Occurred", "First recorded by the Grid"}, labels(tl))
}

// History is read as changes only, so change numbers skip wherever readings
// sit between them. The change after a run of readings is still diffed
// against the change before it, and the count leaves the readings out.
func TestTimelineChangeAfterReadings(t *testing.T) {
	burn := func(p gridv1.BurnPermission, d gridv1.BurnDay) *gridv1.Event {
		return &gridv1.Event{Id: "burn:calaveras-county", Layer: gridv1.Layer_BURN_STATUS, Revision: 7, Readings: 5,
			Detail: &gridv1.Event_BurnStatus{BurnStatus: &gridv1.BurnStatusDetail{Permission: p, BurnDay: d,
				CalfireStatus: gridv1.CalfireBurnStatus_CALFIRE_PERMIT_REQUIRED}}}
	}
	yes := burn(gridv1.BurnPermission_BURN_PERMISSION_ALLOWED, gridv1.BurnDay_BURN_DAY_YES)
	no := burn(gridv1.BurnPermission_BURN_PERMISSION_PROHIBITED, gridv1.BurnDay_BURN_DAY_NO)
	tl := buildTimeline(no, []*gridv1.EventRevision{rev(1, 5, yes), rev(7, 90, no)}, nil, false)
	assert.Equal(t, []string{"First recorded by the Grid", "Burning prohibited", "County line: no-burn day"}, labels(tl))
	assert.Equal(t, "1 change since", tl.GetEntries()[0].GetDetail())
	assert.Equal(t, "allowed", tl.GetEntries()[1].GetBefore())
	assert.Equal(t, uint32(7), tl.GetEntries()[1].GetRevision())
}

// A READING revision is one timeline entry at the time of the call, naming
// the line that was called.
func TestTimelineReadings(t *testing.T) {
	snap := func(revision uint32, aMin, bMin int) *gridv1.EventRevision {
		line := func(id string, min int) *gridv1.BurnLineReading {
			return &gridv1.BurnLineReading{Id: id, Name: id + " line", BurnDay: gridv1.BurnDay_BURN_DAY_YES,
				Observation: &gridv1.BurnObservation{Message: "burn day per " + id, ObservedAt: at(min)}}
		}
		return &gridv1.EventRevision{Revision: revision, Kind: gridv1.RevisionKind_READING, Event: &gridv1.Event{
			Id: "burn:x", Layer: gridv1.Layer_BURN_STATUS,
			Detail: &gridv1.Event_BurnStatus{BurnStatus: &gridv1.BurnStatusDetail{BurnLines: []*gridv1.BurnLineReading{line("a", aMin), line("b", bMin)}}}}}
	}
	tl := &gridv1.EventTimeline{}
	// Newest first: revision 4 moved line b's call, revision 3 line a's.
	addReadings(tl, []*gridv1.EventRevision{snap(4, 100, 130), snap(3, 100, 10), snap(2, 20, 10)})
	require.Len(t, tl.GetEntries(), 3)
	last := tl.GetEntries()[2]
	assert.Equal(t, gridv1.TimelineKind_TIMELINE_READING, last.GetKind())
	assert.Equal(t, at(130).AsTime(), last.GetAt().AsTime())
	assert.Equal(t, "burn day per b", last.GetDetail())
	assert.Equal(t, "Checked: burn day", last.GetLabel())
	assert.Equal(t, "burn day per a", tl.GetEntries()[1].GetDetail())
	assert.Equal(t, uint32(3), tl.GetEntries()[1].GetRevision())
}

func TestGetEventTimelineRPC(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	g := NewGridServer(s)
	tl, err := g.GetEventTimeline(rpcBG(), &gridv1.GetEventTimelineRequest{Id: "caltrans:d10-1"})
	require.NoError(t, err)
	require.NotEmpty(t, tl.GetEntries())
	assert.Equal(t, "caltrans:d10-1", tl.GetEventId())
	_, err = g.GetEventTimeline(rpcBG(), &gridv1.GetEventTimelineRequest{Id: "nope:1"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// A mesh node's timeline lists the moves of its read path from the telemetry
// archive, naming the gateways, on the monitor's clock.
func TestTimelineMeshReadPathChanges(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{{ID: "meshcore", Name: "MeshCore", PollInterval: time.Minute}}))
	node := func(key, name string) {
		upsert(t, s.Store, &gridv1.Event{
			Id: "meshcore:" + key, Layer: gridv1.Layer_MESH, Category: "node", Headline: name,
			Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE,
			ObservedAt: timestamppb.New(base), Provenance: &gridv1.Provenance{SourceId: "meshcore", SourceName: "MeshCore"},
			Detail: &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: key, Name: name}},
		})
	}
	node("aa01aa01", "Eagle Gateway")
	node("bb01bb01", "Eagle One")
	read := func(n int, via string) store.MeshTelemetrySample {
		at := base.Add(time.Duration(n) * 30 * time.Minute)
		return store.MeshTelemetrySample{PubKey: "bb01bb01", Reporter: "relay", ReportedAt: at, ReceivedAt: at,
			LastSuccessAt: at, UptimeS: int64(100 + n), Via: via}
	}
	// The second gateway has no node on the Grid: it shows by key.
	require.NoError(t, s.Store.InsertMeshTelemetry(ctx, []store.MeshTelemetrySample{
		read(0, "aa01aa01"), read(1, "aa01aa01"), read(2, "cc02cc02cc02cc02"),
	}))

	tl, err := NewGridServer(s).GetEventTimeline(rpcBG(), &gridv1.GetEventTimelineRequest{Id: "meshcore:bb01bb01"})
	require.NoError(t, err)
	var moves []*gridv1.TimelineEntry
	for _, e := range tl.GetEntries() {
		if e.GetKind() == gridv1.TimelineKind_TIMELINE_READ_PATH {
			moves = append(moves, e)
		}
	}
	require.Len(t, moves, 2)
	assert.Equal(t, "Read through Eagle Gateway", moves[0].GetLabel())
	assert.Equal(t, "relay", moves[0].GetDetail())
	assert.Empty(t, moves[0].GetBefore())
	assert.Equal(t, "Read through cc02cc02cc02", moves[1].GetLabel())
	assert.Equal(t, "was Eagle Gateway", moves[1].GetDetail())
	assert.Equal(t, "readVia", moves[1].GetField())
	assert.Equal(t, base.Add(time.Hour).Unix(), moves[1].GetAt().AsTime().Unix())
	assert.Equal(t, gridv1.TimelineClock_TIMELINE_CLOCK_SOURCE, moves[1].GetClock())

	arch, err := NewGridServer(s).GetMeshTelemetry(rpcBG(), &gridv1.GetMeshTelemetryRequest{Node: "bb01bb01",
		From: timestamppb.New(base.Add(-time.Hour)), To: timestamppb.New(base.Add(3 * time.Hour))})
	require.NoError(t, err)
	require.Len(t, arch.GetSamples(), 3)
	assert.Equal(t, "cc02cc02cc02cc02", arch.GetSamples()[2].GetReading().GetVia())
}
