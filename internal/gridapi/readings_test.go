package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

// readingsService is a service on a store with READING revisions on, holding
// one county's burn event: a first call (NO), a second call that repeats it
// (a reading), and a third that changes the answer (a change).
func readingsService(t *testing.T) *GridServer {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(storetest.URL(t), store.WithReadings(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.SeedSources(ctx, []store.SourceSeed{{ID: "burnline", Name: "County burn line", PollInterval: time.Hour}}))

	call := func(day int, answer gridv1.BurnDay, msg string) {
		at := base.Add(time.Duration(day) * 24 * time.Hour)
		perm := gridv1.BurnPermission_BURN_PERMISSION_PROHIBITED
		if answer == gridv1.BurnDay_BURN_DAY_YES {
			perm = gridv1.BurnPermission_BURN_PERMISSION_ALLOWED
		}
		ev := &gridv1.Event{
			Id: "burn:calaveras-county", Layer: gridv1.Layer_BURN_STATUS, Severity: gridv1.Severity_INFO,
			Status: gridv1.EventStatus_ACTIVE, Headline: "Calaveras County burn status",
			Provenance: &gridv1.Provenance{SourceId: "burnline", SourceName: "County burn line", FetchedAt: timestamppb.New(at)},
			ObservedAt: timestamppb.New(at),
			Detail: &gridv1.Event_BurnStatus{BurnStatus: &gridv1.BurnStatusDetail{
				BurnDay: answer, Permission: perm, CalfireStatus: gridv1.CalfireBurnStatus_CALFIRE_PERMIT_REQUIRED,
				BurnLines: []*gridv1.BurnLineReading{{
					Id: "calaveras-apcd", Name: "Calaveras County burn line", BurnDay: answer,
					Observation: &gridv1.BurnObservation{Message: msg, Confidence: 95, ObservedAt: timestamppb.New(at)},
				}},
			}},
		}
		// As the burn poller does: a later call of the line is a reading.
		_, err := st.UpsertEventReading(ctx, ev, func(stored *gridv1.Event) bool {
			old := stored.GetBurnStatus().GetBurnLines()[0].GetObservation().GetObservedAt()
			return old == nil || at.After(old.AsTime())
		})
		require.NoError(t, err)
	}
	call(0, gridv1.BurnDay_BURN_DAY_NO, "Not a burn day")
	call(1, gridv1.BurnDay_BURN_DAY_NO, "Still not a burn day")
	call(2, gridv1.BurnDay_BURN_DAY_YES, "Today is a burn day")

	svc := NewService(st, &fakeWeather{resp: weatherResp()}, &fakeCensus{}, testConfig(), nil)
	svc.Now = func() time.Time { return base.Add(72 * time.Hour) }
	return NewGridServer(svc)
}

func TestHistoryKindParameter(t *testing.T) {
	g := readingsService(t)
	const id = "burn:calaveras-county"

	def, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: id})
	require.NoError(t, err)
	require.Len(t, def.GetRevisions(), 2, "default history is changes only")
	assert.Equal(t, uint32(3), def.GetRevisions()[0].GetRevision())
	assert.Equal(t, uint32(1), def.GetRevisions()[1].GetRevision(), "change numbers skip the reading")
	assert.Equal(t, gridv1.RevisionKind_CHANGE, def.GetRevisions()[0].GetKind())
	assert.Equal(t, uint32(1), def.GetRevisions()[0].GetEvent().GetReadings())

	readings, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: id, Kind: []string{"reading"}})
	require.NoError(t, err)
	require.Len(t, readings.GetRevisions(), 1)
	assert.Equal(t, gridv1.RevisionKind_READING, readings.GetRevisions()[0].GetKind())
	assert.Equal(t, "Still not a burn day",
		readings.GetRevisions()[0].GetEvent().GetBurnStatus().GetBurnLines()[0].GetObservation().GetMessage())

	both, err := g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: id, Kind: []string{"CHANGE,READING"}})
	require.NoError(t, err)
	assert.Len(t, both.GetRevisions(), 3)

	_, err = g.GetEventHistory(rpcBG(), &gridv1.GetEventHistoryRequest{Id: id, Kind: []string{"observation"}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	arch, err := g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{})
	require.NoError(t, err)
	assert.Len(t, arch.GetRevisions(), 2)
	arch, err = g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Kind: []string{"READING"}})
	require.NoError(t, err)
	assert.Len(t, arch.GetRevisions(), 1)

	_, err = g.ListHistory(rpcBG(), &gridv1.ListHistoryRequest{Q: "burn", Kind: []string{"READING"}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "text search is over changes only")
}

func TestStatsCountReadings(t *testing.T) {
	g := readingsService(t)
	st, err := g.GetStats(rpcBG(), &gridv1.GetStatsRequest{})
	require.NoError(t, err)
	assert.EqualValues(t, 3, st.GetRevisions())
	assert.EqualValues(t, 2, st.GetChanges())
	assert.EqualValues(t, 1, st.GetReadings())
	require.Len(t, st.GetLayers(), 1)
	assert.EqualValues(t, 1, st.GetLayers()[0].GetReadings())
	assert.EqualValues(t, 2, st.GetLayers()[0].GetChanges())
}

// The timeline shows the change that follows a reading, with the change
// before it as "before", and the reading as its own entry on the call's time.
func TestTimelineWithReadings(t *testing.T) {
	g := readingsService(t)
	tl, err := g.GetEventTimeline(rpcBG(), &gridv1.GetEventTimelineRequest{Id: "burn:calaveras-county"})
	require.NoError(t, err)
	var changed, reading *gridv1.TimelineEntry
	for _, e := range tl.GetEntries() {
		switch {
		case e.GetField() == "permission":
			changed = e
		case e.GetKind() == gridv1.TimelineKind_TIMELINE_READING:
			reading = e
		}
	}
	require.NotNil(t, changed, "the change after the reading is on the timeline")
	assert.Equal(t, "prohibited", changed.GetBefore())
	assert.Equal(t, "allowed", changed.GetAfter())
	assert.Equal(t, uint32(3), changed.GetRevision())
	require.NotNil(t, reading)
	assert.Equal(t, uint32(2), reading.GetRevision())
	assert.Equal(t, "Still not a burn day", reading.GetDetail())
	assert.Equal(t, base.Add(24*time.Hour).Unix(), reading.GetAt().AsTime().Unix())
}
