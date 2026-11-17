package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/storetest"
)

func healthStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t, WithSourceHealth(true))
	require.NoError(t, s.SeedSources(context.Background(), []SourceSeed{
		GridSourceSeed(),
		{ID: "usgs", Name: "USGS", PollInterval: time.Minute}, // stale window = 180s
		{ID: "alanpi", Name: "Alan's monitor", Kind: gridv1.SourceKind_REPORTER, PollInterval: time.Minute},
	}))
	return s
}

func healthStatuses(t *testing.T, s *Store, id string) []gridv1.SourceStatus {
	t.Helper()
	revs, _, err := s.EventHistory(context.Background(), SourceHealthPrefix+id, 200, "")
	require.NoError(t, err)
	out := make([]gridv1.SourceStatus, len(revs))
	for i, r := range revs { // newest first; return oldest first
		out[len(revs)-1-i] = r.GetEvent().GetSourceHealth().GetStatus()
	}
	return out
}

// lastSuccessAgo moves a source's last success into the past, as if that
// long had gone by without a good poll.
func lastSuccessAgo(t *testing.T, s *Store, id string, d time.Duration) {
	t.Helper()
	_, err := s.db.Exec(`UPDATE sources SET last_success_at = $1 WHERE id = $2`, time.Now().Add(-d).Unix(), id)
	require.NoError(t, err)
}

// The history is the source's health as it LASTED. One failed poll and its
// recovery write nothing: the row shows the blip (/api/v1/sources), the
// history does not. A failure that outlives a second poll is STALE; past the
// stale window it is UNAVAILABLE; recovery is one revision.
func TestSourceHealthRevisionPerLastingChange(t *testing.T) {
	ctx := context.Background()
	s := healthStore(t)
	ok, stale, down := gridv1.SourceStatus_OK, gridv1.SourceStatus_STALE, gridv1.SourceStatus_UNAVAILABLE

	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	assert.Equal(t, []gridv1.SourceStatus{ok}, healthStatuses(t, s, "usgs"))

	// A blip: one failed poll, then a good one.
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 503")))
	assert.Equal(t, stale, getSource(t, s, "usgs").GetStatus(), "the row is not debounced")
	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	assert.Equal(t, []gridv1.SourceStatus{ok}, healthStatuses(t, s, "usgs"), "a blip is not history")

	// Still failing two poll intervals after the last success: STALE, once.
	lastSuccessAgo(t, s, "usgs", 2*time.Minute+time.Second)
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 503")))
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 502")))
	assert.Equal(t, []gridv1.SourceStatus{ok, stale}, healthStatuses(t, s, "usgs"))

	// Past the stale window, the next failed attempt is the change.
	lastSuccessAgo(t, s, "usgs", 10*time.Minute)
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("connect timeout")))
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("connect timeout again")))
	assert.Equal(t, []gridv1.SourceStatus{ok, stale, down}, healthStatuses(t, s, "usgs"))

	ev, err := s.GetEvent(ctx, "source:usgs")
	require.NoError(t, err)
	assert.Equal(t, gridv1.Layer_SOURCE_HEALTH, ev.GetLayer())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus())
	assert.Equal(t, gridv1.Severity_INFO, ev.GetSeverity())
	assert.Equal(t, GridSourceID, ev.GetProvenance().GetSourceId())
	assert.Equal(t, "USGS: unavailable", ev.GetHeadline())
	assert.Empty(t, ev.GetPlaceIds())
	d := ev.GetSourceHealth()
	assert.Equal(t, "usgs", d.GetSourceId())
	assert.Equal(t, "connect timeout", d.GetError(), "the error of the attempt that changed the status, not the latest")
	assert.NotNil(t, d.GetSince())
	assert.NotNil(t, d.GetLastSuccessAt())

	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	assert.Equal(t, []gridv1.SourceStatus{ok, stale, down, ok}, healthStatuses(t, s, "usgs"))
	ev, err = s.GetEvent(ctx, "source:usgs")
	require.NoError(t, err)
	assert.Empty(t, ev.GetSourceHealth().GetError())
	assert.Equal(t, uint32(4), ev.GetRevision())
}

// A source that has never been read goes straight to UNAVAILABLE, and that
// first failure is revision 1.
func TestSourceHealthFirstAttemptFails(t *testing.T) {
	s := healthStore(t)
	require.NoError(t, s.RecordAttempt(context.Background(), "usgs", errors.New("HTTP 500")))
	assert.Equal(t, []gridv1.SourceStatus{gridv1.SourceStatus_UNAVAILABLE}, healthStatuses(t, s, "usgs"))
}

// Reporter rows get no event (their health does not survive a restart), and
// neither does the grid row itself.
func TestSourceHealthFeedsOnly(t *testing.T) {
	ctx := context.Background()
	s := healthStore(t)
	require.NoError(t, s.RecordAttempt(ctx, "alanpi", errors.New("no report received yet")))
	require.NoError(t, s.RecordAttempt(ctx, "alanpi", nil))
	require.NoError(t, s.SeedSourceHealth(ctx))
	_, err := s.GetEvent(ctx, "source:alanpi")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = s.GetEvent(ctx, "source:grid")
	assert.ErrorIs(t, err, ErrNotFound)

	grid := getSource(t, s, GridSourceID)
	assert.Equal(t, gridv1.SourceKind_INTERNAL, grid.GetKind())
	assert.Equal(t, gridv1.SourceStatus_OK, grid.GetStatus(), "the seed records the grid row's success")
}

// The history must never cost the health. When the event cannot be written
// the attempt is still recorded (status, error, attempt time), and the next
// attempt that can write the event puts the history right.
func TestSourceHealthFailureNeverBlocksTheAttempt(t *testing.T) {
	ctx := context.Background()
	s := healthStore(t)
	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	before := getSource(t, s, "usgs")

	// Make every revision write fail, as a broken index or a bad row would.
	_, err := s.db.Exec(`CREATE FUNCTION refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused'; END $$;
		CREATE TRIGGER refuse_revisions BEFORE INSERT ON event_revisions FOR EACH ROW EXECUTE FUNCTION refuse()`)
	require.NoError(t, err)

	lastSuccessAgo(t, s, "usgs", 10*time.Minute)
	time.Sleep(1100 * time.Millisecond) // attempt times are whole seconds
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 503")), "the attempt commits whatever the history does")
	after := getSource(t, s, "usgs")
	assert.Equal(t, gridv1.SourceStatus_UNAVAILABLE, after.GetStatus())
	assert.Equal(t, "HTTP 503", after.GetLastError())
	assert.True(t, after.GetLastAttemptAt().AsTime().After(before.GetLastAttemptAt().AsTime()))
	assert.Equal(t, []gridv1.SourceStatus{gridv1.SourceStatus_OK}, healthStatuses(t, s, "usgs"), "the revision was refused")

	// The obstacle goes away: the next attempt records what was missed.
	_, err = s.db.Exec(`DROP TRIGGER refuse_revisions ON event_revisions`)
	require.NoError(t, err)
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 503 again")))
	assert.Equal(t, []gridv1.SourceStatus{gridv1.SourceStatus_OK, gridv1.SourceStatus_UNAVAILABLE}, healthStatuses(t, s, "usgs"))
}

// Turning the layer on over a database that never had it: nothing is written
// from the rows (their status may be hours old); each feed's next attempt
// writes its first revision from a real reading.
func TestSourceHealthStartsAtTheNextAttempt(t *testing.T) {
	ctx := context.Background()
	url := storetest.URL(t)
	plain, err := Open(url)
	require.NoError(t, err)
	require.NoError(t, plain.SeedSources(ctx, []SourceSeed{
		{ID: "usgs", Name: "USGS", PollInterval: time.Minute},
		{ID: "nws", Name: "NWS", PollInterval: time.Minute},
	}))
	require.NoError(t, plain.RecordAttempt(ctx, "usgs", nil))
	require.NoError(t, plain.RecordAttempt(ctx, "nws", errors.New("HTTP 500")))
	_, err = plain.GetEvent(ctx, "source:usgs")
	require.ErrorIs(t, err, ErrNotFound, "off: no events")
	require.NoError(t, plain.Close())

	s, err := Open(url, WithSourceHealth(true))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	require.Error(t, s.SeedSourceHealth(ctx), "the grid row must be seeded first")
	require.NoError(t, s.SeedSources(ctx, []SourceSeed{GridSourceSeed()}))
	require.NoError(t, s.SeedSourceHealth(ctx))
	_, err = s.GetEvent(ctx, "source:usgs")
	assert.ErrorIs(t, err, ErrNotFound, "nothing is written from a row")

	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	require.NoError(t, s.RecordAttempt(ctx, "nws", errors.New("HTTP 500")))
	assert.Equal(t, []gridv1.SourceStatus{gridv1.SourceStatus_OK}, healthStatuses(t, s, "usgs"))
	assert.Equal(t, []gridv1.SourceStatus{gridv1.SourceStatus_UNAVAILABLE}, healthStatuses(t, s, "nws"))
}

// A feed that leaves the registry, is retired, or becomes a reporter: its
// health event is resolved at the next term, the history kept, and resolving
// is idempotent. A source that comes back is ACTIVE again at its next attempt.
func TestSourceHealthEventsOfDepartedSourcesAreResolved(t *testing.T) {
	ctx := context.Background()
	s := healthStore(t)
	require.NoError(t, s.SeedSources(ctx, []SourceSeed{{ID: "nws", Name: "NWS", PollInterval: time.Minute}}))
	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	require.NoError(t, s.RecordAttempt(ctx, "nws", nil))

	_, err := s.RetireSource(ctx, "nws")
	require.NoError(t, err)
	require.NoError(t, s.SeedSources(ctx, []SourceSeed{{ID: "usgs", Name: "USGS", Kind: gridv1.SourceKind_REPORTER, PollInterval: time.Minute}}))
	require.NoError(t, s.SeedSourceHealth(ctx))
	for _, id := range []string{"source:nws", "source:usgs"} {
		ev, err := s.GetEvent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, gridv1.EventStatus_RESOLVED, ev.GetStatus(), id)
		assert.Equal(t, uint32(2), ev.GetRevision(), id)
	}
	require.NoError(t, s.SeedSourceHealth(ctx))
	ev, err := s.GetEvent(ctx, "source:nws")
	require.NoError(t, err)
	assert.Equal(t, uint32(2), ev.GetRevision(), "resolving is idempotent")

	// The feed is configured again.
	require.NoError(t, s.SeedSources(ctx, []SourceSeed{{ID: "nws", Name: "NWS", PollInterval: time.Minute}}))
	require.NoError(t, s.RecordAttempt(ctx, "nws", nil))
	ev, err = s.GetEvent(ctx, "source:nws")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus())
}

// RetireSource is what turning a layer off does: the source's live events are
// EXPIRED (nobody is watching them; that is not "ended"), its row leaves the
// registry listing, and both come back when the source is seeded and polled
// again. Without it a layer that was switched off left its events ACTIVE.
func TestRetireSource(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	seedSource(t, s, "caltrans-cc")
	mk := func(id string, status gridv1.EventStatus) *gridv1.Event {
		ev := testEvent(id, gridv1.Severity_MODERATE, status, "Hwy 4 chains required")
		ev.Layer = gridv1.Layer_CHAIN_CONTROL
		ev.Provenance.SourceId = "caltrans-cc"
		return ev
	}
	for _, ev := range []*gridv1.Event{mk("cc:hwy4:r2", gridv1.EventStatus_ACTIVE), mk("cc:hwy88:r2", gridv1.EventStatus_ACTIVE), mk("cc:hwy108:r1", gridv1.EventStatus_RESOLVED)} {
		_, err := s.UpsertEvent(ctx, ev)
		require.NoError(t, err)
	}
	other := testEvent("usgs:q", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "quake")
	_, err := s.UpsertEvent(ctx, other)
	require.NoError(t, err)

	n, err := s.RetireSource(ctx, "caltrans-cc")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	for id, want := range map[string]gridv1.EventStatus{
		"cc:hwy4:r2": gridv1.EventStatus_EXPIRED, "cc:hwy88:r2": gridv1.EventStatus_EXPIRED,
		"cc:hwy108:r1": gridv1.EventStatus_RESOLVED, "usgs:q": gridv1.EventStatus_ACTIVE,
	} {
		ev, err := s.GetEvent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, want, ev.GetStatus(), id)
	}
	listed := map[string]bool{}
	sources, err := s.ListSources(ctx)
	require.NoError(t, err)
	for _, src := range sources {
		listed[src.GetId()] = true
	}
	assert.False(t, listed["caltrans-cc"], "a retired row is not listed")
	assert.True(t, listed["usgs"])

	// Again, and for an id that was never seeded: nothing to do, no error.
	n, err = s.RetireSource(ctx, "caltrans-cc")
	require.NoError(t, err)
	assert.Zero(t, n)
	_, err = s.RetireSource(ctx, "never-seeded")
	require.NoError(t, err)

	// On again: the row is listed, and the next poll's event is ACTIVE.
	seedSource(t, s, "caltrans-cc")
	sources, err = s.ListSources(ctx)
	require.NoError(t, err)
	listed = map[string]bool{}
	for _, src := range sources {
		listed[src.GetId()] = true
	}
	assert.True(t, listed["caltrans-cc"])
	res, err := s.UpsertEvent(ctx, mk("cc:hwy4:r2", gridv1.EventStatus_ACTIVE))
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Equal(t, uint32(3), res.Revision)
}
