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

// A revision is a status change and nothing else: repeated successes and
// repeated failures write none.
func TestSourceHealthRevisionPerStatusChange(t *testing.T) {
	ctx := context.Background()
	s := healthStore(t)
	ok, stale, down := gridv1.SourceStatus_OK, gridv1.SourceStatus_STALE, gridv1.SourceStatus_UNAVAILABLE

	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	assert.Equal(t, []gridv1.SourceStatus{ok}, healthStatuses(t, s, "usgs"))

	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 503")))
	require.NoError(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 502")))
	assert.Equal(t, []gridv1.SourceStatus{ok, stale}, healthStatuses(t, s, "usgs"))

	// Past the stale window, the next failed attempt is the change.
	_, err := s.db.Exec(`UPDATE sources SET last_success_at = $1 WHERE id = 'usgs'`, time.Now().Add(-10*time.Minute).Unix())
	require.NoError(t, err)
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

// The status and its history commit together: when the event cannot be
// written, the attempt is not recorded either.
func TestSourceHealthIsAtomicWithTheAttempt(t *testing.T) {
	ctx := context.Background()
	s := healthStore(t)
	require.NoError(t, s.RecordAttempt(ctx, "usgs", nil))
	// Without its owner row the event cannot be inserted (foreign key).
	_, err := s.db.Exec(`DELETE FROM event_search WHERE event_id = 'source:usgs'`)
	require.NoError(t, err)
	_, err = s.db.Exec(`DELETE FROM event_revision_search; DELETE FROM event_lite; DELETE FROM event_revisions; DELETE FROM events`)
	require.NoError(t, err)
	require.NoError(t, s.DeleteSource(ctx, GridSourceID))

	require.Error(t, s.RecordAttempt(ctx, "usgs", errors.New("HTTP 503")))
	assert.Equal(t, gridv1.SourceStatus_OK, getSource(t, s, "usgs").GetStatus(), "the failed write rolled the status back")
}

// The term-start seed: a feed already attempted gets its first revision, a
// disagreement gets a revision, agreement writes nothing, a never-attempted
// feed waits for its first attempt, and a feed gone from the registry is
// resolved.
func TestSeedSourceHealth(t *testing.T) {
	ctx := context.Background()
	// A store without source health records attempts and no events: the
	// state a first deploy finds.
	url := storetest.URL(t)
	plain, err := Open(url)
	require.NoError(t, err)
	require.NoError(t, plain.SeedSources(ctx, []SourceSeed{
		{ID: "usgs", Name: "USGS", PollInterval: time.Minute},
		{ID: "nws", Name: "NWS", PollInterval: time.Minute},
		{ID: "pge", Name: "PG&E", PollInterval: time.Minute},
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

	assert.Equal(t, []gridv1.SourceStatus{gridv1.SourceStatus_OK}, healthStatuses(t, s, "usgs"))
	assert.Equal(t, []gridv1.SourceStatus{gridv1.SourceStatus_UNAVAILABLE}, healthStatuses(t, s, "nws"))
	_, err = s.GetEvent(ctx, "source:pge")
	assert.ErrorIs(t, err, ErrNotFound, "never attempted: no revision until its first attempt")

	// Agreement: a second seed writes nothing.
	require.NoError(t, s.SeedSourceHealth(ctx))
	assert.Len(t, healthStatuses(t, s, "usgs"), 1)

	// Disagreement (a build without the layer moved the row): a revision.
	_, err = s.db.Exec(`UPDATE sources SET status = $1, last_error = 'HTTP 503' WHERE id = 'usgs'`, int32(gridv1.SourceStatus_STALE))
	require.NoError(t, err)
	require.NoError(t, s.SeedSourceHealth(ctx))
	assert.Equal(t, []gridv1.SourceStatus{gridv1.SourceStatus_OK, gridv1.SourceStatus_STALE}, healthStatuses(t, s, "usgs"))

	// A feed that leaves the registry: its event is resolved, history kept.
	require.NoError(t, s.DeleteSource(ctx, "nws"))
	require.NoError(t, s.SeedSourceHealth(ctx))
	ev, err := s.GetEvent(ctx, "source:nws")
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, ev.GetStatus())
	require.NoError(t, s.SeedSourceHealth(ctx))
	ev, err = s.GetEvent(ctx, "source:nws")
	require.NoError(t, err)
	assert.Equal(t, uint32(2), ev.GetRevision(), "resolving is idempotent")
}
