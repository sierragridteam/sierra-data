package ingest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/search"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// A writer term brings the search index level with the store and marks it
// complete: until then a text query is refused, afterwards it answers. Rows
// the index is missing (an older release's writes) are built in the walk.
func TestSchedulerBuildsTheSearchIndex(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "s1")
	for _, id := range []string{"s1:a", "s1:b", "s1:c"} {
		ev := schedEvent(id, "s1", gridv1.Layer_ROAD_INCIDENT)
		ev.Headline = "Rockslide " + id
		_, err := st.UpsertEvent(ctx, ev)
		require.NoError(t, err)
	}
	// As the previous release's writer would have left them: no search rows.
	_, err := st.Pool().Exec(ctx, `DELETE FROM event_search`)
	require.NoError(t, err)
	_, err = st.Pool().Exec(ctx, `DELETE FROM event_revision_search`)
	require.NoError(t, err)

	q, err := search.ParseQuery("rockslide", false)
	require.NoError(t, err)
	_, err = st.SearchEvents(ctx, store.EventQuery{Search: &q})
	require.ErrorIs(t, err, store.ErrSearchNotReady)

	defer func(a, p time.Duration) { searchBackfillAfter, searchBackfillPause = a, p }(searchBackfillAfter, searchBackfillPause)
	searchBackfillAfter, searchBackfillPause = time.Millisecond, time.Millisecond

	sched := NewScheduler(st, SchedulerConfig{})
	sched.Start(ctx)
	defer func() { _ = sched.Stop(time.Second, 5*time.Second) }()

	require.Eventually(t, func() bool {
		ev, rev, err := st.SearchReady(ctx)
		return err == nil && ev && rev
	}, 10*time.Second, 10*time.Millisecond)
	p, err := st.SearchEvents(ctx, store.EventQuery{Search: &q})
	require.NoError(t, err)
	assert.Equal(t, 3, p.Total)
	h, err := st.SearchHistory(ctx, store.HistoryQuery{Search: &q})
	require.NoError(t, err)
	assert.Equal(t, 3, h.Total)
}
