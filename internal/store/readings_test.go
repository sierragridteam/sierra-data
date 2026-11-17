package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/search"
)

const burnID = "burn:calaveras-county"

// burnCall is one line's call for burnEvent: when it was made and what it
// answered.
type burnCall struct {
	at  time.Time
	day gridv1.BurnDay
	msg string
}

// burnEvent builds a county's burn event as the poller offers it at tick: the
// envelope and CAL FIRE stamps are the tick time, each line carries its own
// call. A zero call time is a line with no reading.
func burnEvent(tick time.Time, calls map[string]burnCall) *gridv1.Event {
	d := &gridv1.BurnStatusDetail{
		BurnDay:           gridv1.BurnDay_BURN_DAY_NO,
		Permission:        gridv1.BurnPermission_BURN_PERMISSION_PROHIBITED,
		CalfireObservedAt: timestamppb.New(tick),
	}
	for _, id := range []string{"calaveras-apcd", "second-line"} {
		c, ok := calls[id]
		if !ok {
			continue
		}
		line := &gridv1.BurnLineReading{Id: id, BurnDay: c.day}
		if !c.at.IsZero() {
			line.Observation = &gridv1.BurnObservation{Message: c.msg, Confidence: 90, ObservedAt: timestamppb.New(c.at)}
		}
		if c.day == gridv1.BurnDay_BURN_DAY_YES {
			d.BurnDay, d.Permission = gridv1.BurnDay_BURN_DAY_YES, gridv1.BurnPermission_BURN_PERMISSION_ALLOWED
		}
		d.BurnLines = append(d.BurnLines, line)
	}
	return &gridv1.Event{
		Id: burnID, Layer: gridv1.Layer_BURN_STATUS, Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE,
		Headline:   "Calaveras County burn status",
		Provenance: &gridv1.Provenance{SourceId: "burnline", FetchedAt: timestamppb.New(tick)},
		ObservedAt: timestamppb.New(tick),
		Detail:     &gridv1.Event_BurnStatus{BurnStatus: d},
	}
}

// upsertBurn upserts a burn event as its poller does: with the layer's
// reading test (a line's call later than the stored call of the same line).
// The store itself has no idea what a reading is.
func upsertBurn(ctx context.Context, s *Store, ev *gridv1.Event) (UpsertResult, error) {
	return s.UpsertEventReading(ctx, ev, func(stored *gridv1.Event) bool {
		have := map[string]*gridv1.BurnObservation{}
		for _, l := range stored.GetBurnStatus().GetBurnLines() {
			have[l.GetId()] = l.GetObservation()
		}
		for _, l := range ev.GetBurnStatus().GetBurnLines() {
			old, known := have[l.GetId()]
			if at := l.GetObservation().GetObservedAt(); at != nil && known &&
				(old.GetObservedAt() == nil || at.AsTime().After(old.GetObservedAt().AsTime())) {
				return true
			}
		}
		return false
	})
}

func revisionKinds(t *testing.T, s *Store, id string) []int {
	t.Helper()
	rows, err := s.db.Query(`SELECT kind FROM event_revisions WHERE event_id = $1 ORDER BY revision`, id)
	require.NoError(t, err)
	defer rows.Close()
	var out []int
	for rows.Next() {
		var k int
		require.NoError(t, rows.Scan(&k))
		out = append(out, k)
	}
	require.NoError(t, rows.Err())
	return out
}

// A call that repeats the answer is a READING revision: the content hash
// stays, readings and revision increment, and the snapshot holds that call. A
// re-offered call (the poller ticks hourly with the same reading) writes
// nothing, which is what keeps it to one revision per call.
func TestReadingRevisionPerCall(t *testing.T) {
	ctx := context.Background()
	rec := &statementRecorder{}
	s := newTestStore(t, WithReadings(true), WithQueryTracer(rec))
	seedSource(t, s, "burnline")
	no := gridv1.BurnDay_BURN_DAY_NO

	day1 := baseTime
	res, err := upsertBurn(ctx, s, burnEvent(day1, map[string]burnCall{"calaveras-apcd": {day1, no, "October 9th is not a burn day"}}))
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: true, Revision: 1}, res)
	var hash1 string
	require.NoError(t, s.db.QueryRow(`SELECT content_hash FROM events WHERE id = $1`, burnID).Scan(&hash1))

	day2 := day1.Add(24 * time.Hour)
	tick := day2.Add(20 * time.Minute)
	call2 := map[string]burnCall{"calaveras-apcd": {day2, no, "October 10th is not a burn day"}}
	res, err = upsertBurn(ctx, s, burnEvent(tick, call2))
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Reading: true, Revision: 2}, res)

	got, err := s.GetEvent(ctx, burnID)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), got.GetRevision())
	assert.Equal(t, uint32(1), got.GetReadings())
	assert.Equal(t, "October 10th is not a burn day", got.GetBurnStatus().GetBurnLines()[0].GetObservation().GetMessage())
	assert.True(t, got.GetObservedAt().AsTime().Equal(tick), "observed_at moves with the revision")

	var hash2 string
	var readings, observedCol int64
	require.NoError(t, s.db.QueryRow(`SELECT content_hash, readings, observed_at FROM events WHERE id = $1`, burnID).Scan(&hash2, &readings, &observedCol))
	assert.Equal(t, hash1, hash2, "a reading leaves the content hash alone")
	assert.EqualValues(t, 1, readings)
	assert.Equal(t, tick.Unix(), observedCol)
	assert.Equal(t, []int{1, 2}, revisionKinds(t, s, burnID))

	// The same call offered again an hour later: no revision, and with CAL
	// FIRE's stamp unchanged nothing to refresh either.
	rec.take()
	again := burnEvent(tick, call2)
	res, err = upsertBurn(ctx, s, again)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Revision: 2}, res)
	assert.Len(t, rec.take(), 4, "re-offered call: begin, fence, lookup, commit")

	// An hour on, only the tick-time stamps moved: refreshed in place, and
	// the event's observed_at stays with its latest revision.
	later := burnEvent(tick.Add(time.Hour), call2)
	res, err = upsertBurn(ctx, s, later)
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Revision: 2}, res)
	got, err = s.GetEvent(ctx, burnID)
	require.NoError(t, err)
	assert.True(t, got.GetObservedAt().AsTime().Equal(tick), "observed_at does not follow a refresh")
	assert.True(t, got.GetBurnStatus().GetCalfireObservedAt().AsTime().Equal(tick.Add(time.Hour)), "the CAL FIRE stamp is refreshed")
	assert.Equal(t, []int{1, 2}, revisionKinds(t, s, burnID))

	// The lite copy follows the reading.
	lite, _, err := s.QueryEvents(ctx, EventQuery{Layers: []gridv1.Layer{gridv1.Layer_BURN_STATUS}, Lite: true})
	require.NoError(t, err)
	require.Len(t, lite, 1)
	assert.Equal(t, uint32(2), lite[0].GetRevision())
	assert.Equal(t, uint32(1), lite[0].GetReadings())
}

// Two lines called at different times are two readings: the comparison is
// per line, never the latest call across lines.
func TestReadingPerLine(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, WithReadings(true))
	seedSource(t, s, "burnline")
	no := gridv1.BurnDay_BURN_DAY_NO

	d1 := baseTime
	_, err := upsertBurn(ctx, s, burnEvent(d1, map[string]burnCall{
		"calaveras-apcd": {d1, no, "a day 1"}, "second-line": {d1, no, "b day 1"}}))
	require.NoError(t, err)

	d2 := d1.Add(24 * time.Hour)
	// Line a is called at 09:00; line b's call is still yesterday's.
	res, err := upsertBurn(ctx, s, burnEvent(d2.Add(10*time.Minute), map[string]burnCall{
		"calaveras-apcd": {d2, no, "a day 2"}, "second-line": {d1, no, "b day 1"}}))
	require.NoError(t, err)
	assert.True(t, res.Reading)
	// Line b is called an hour later, EARLIER than nothing but later than its own last call.
	res, err = upsertBurn(ctx, s, burnEvent(d2.Add(70*time.Minute), map[string]burnCall{
		"calaveras-apcd": {d2, no, "a day 2"}, "second-line": {d2.Add(-30 * time.Minute), no, "b day 2"}}))
	require.NoError(t, err)
	assert.True(t, res.Reading, "line b's call is older than line a's, and still a new reading of line b")
	assert.Equal(t, []int{1, 2, 2}, revisionKinds(t, s, burnID))

	got, err := s.GetEvent(ctx, burnID)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), got.GetReadings())
}

// A call that changes the answer is ONE revision, a CHANGE that carries its
// reading, and the readings count is kept across it.
func TestReadingThenChange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, WithReadings(true))
	seedSource(t, s, "burnline")

	d1 := baseTime
	_, err := upsertBurn(ctx, s, burnEvent(d1, map[string]burnCall{"calaveras-apcd": {d1, gridv1.BurnDay_BURN_DAY_NO, "no"}}))
	require.NoError(t, err)
	d2 := d1.Add(24 * time.Hour)
	_, err = upsertBurn(ctx, s, burnEvent(d2, map[string]burnCall{"calaveras-apcd": {d2, gridv1.BurnDay_BURN_DAY_NO, "still no"}}))
	require.NoError(t, err)
	d3 := d2.Add(24 * time.Hour)
	res, err := upsertBurn(ctx, s, burnEvent(d3, map[string]burnCall{"calaveras-apcd": {d3, gridv1.BurnDay_BURN_DAY_YES, "burn day"}}))
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: true, Revision: 3}, res)
	assert.Equal(t, []int{1, 2, 1}, revisionKinds(t, s, burnID))

	got, err := s.GetEvent(ctx, burnID)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), got.GetReadings(), "a change keeps the count of readings")
	var col int64
	require.NoError(t, s.db.QueryRow(`SELECT readings FROM events WHERE id = $1`, burnID).Scan(&col))
	assert.EqualValues(t, 1, col)

	// A lifecycle transition keeps it too (it rewrites the stored blob).
	require.NoError(t, s.TransitionEvents(ctx, []string{burnID}, gridv1.EventStatus_RESOLVED, d3.Add(time.Hour)))
	got, err = s.GetEvent(ctx, burnID)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), got.GetReadings())
	assert.Equal(t, uint32(4), got.GetRevision())

	// Default history is changes only, so its revision numbers skip; kinds
	// ask for the rest.
	changes, _, err := s.EventHistory(ctx, burnID, 50, "")
	require.NoError(t, err)
	require.Len(t, changes, 3)
	assert.Equal(t, []uint32{4, 3, 1}, []uint32{changes[0].GetRevision(), changes[1].GetRevision(), changes[2].GetRevision()})
	for _, r := range changes {
		assert.Equal(t, gridv1.RevisionKind_CHANGE, r.GetKind())
	}
	readings, _, err := s.EventHistory(ctx, burnID, 50, "", gridv1.RevisionKind_READING)
	require.NoError(t, err)
	require.Len(t, readings, 1)
	assert.Equal(t, uint32(2), readings[0].GetRevision())
	assert.Equal(t, gridv1.RevisionKind_READING, readings[0].GetKind())
	assert.Equal(t, "still no", readings[0].GetEvent().GetBurnStatus().GetBurnLines()[0].GetObservation().GetMessage())
	both, _, err := s.EventHistory(ctx, burnID, 50, "", gridv1.RevisionKind_CHANGE, gridv1.RevisionKind_READING)
	require.NoError(t, err)
	assert.Len(t, both, 4)

	// The archive follows the same rule, and a page token walks changes.
	arch, _, err := s.QueryHistory(ctx, HistoryQuery{})
	require.NoError(t, err)
	assert.Len(t, arch, 3)
	arch, _, err = s.QueryHistory(ctx, HistoryQuery{Kinds: []gridv1.RevisionKind{gridv1.RevisionKind_READING}})
	require.NoError(t, err)
	require.Len(t, arch, 1)
	assert.Equal(t, gridv1.RevisionKind_READING, arch[0].GetKind())
	page, token, err := s.EventHistory(ctx, burnID, 1, "")
	require.NoError(t, err)
	require.Len(t, page, 1)
	page, _, err = s.EventHistory(ctx, burnID, 1, token)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, uint32(3), page[0].GetRevision())

	// Totals count readings apart.
	totals, err := s.Totals(ctx, "")
	require.NoError(t, err)
	assert.EqualValues(t, 4, totals.Revisions)
	assert.EqualValues(t, 1, totals.Readings)
	require.Len(t, totals.Layers, 1)
	assert.EqualValues(t, 1, totals.Layers[0].Readings)
}

// With the option off (the default) a repeated answer takes the refresh path
// it always has: TestUpsertPersistsBurnObservationOnHashEqual pins the
// details, this pins that no reading is written.
func TestReadingsOffIsTheRefreshPath(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "burnline")
	no := gridv1.BurnDay_BURN_DAY_NO
	d1 := baseTime
	_, err := upsertBurn(ctx, s, burnEvent(d1, map[string]burnCall{"calaveras-apcd": {d1, no, "day 1"}}))
	require.NoError(t, err)
	d2 := d1.Add(24 * time.Hour)
	res, err := upsertBurn(ctx, s, burnEvent(d2, map[string]burnCall{"calaveras-apcd": {d2, no, "day 2"}}))
	require.NoError(t, err)
	assert.Equal(t, UpsertResult{Changed: false, Revision: 1}, res)
	assert.Equal(t, []int{1}, revisionKinds(t, s, burnID))
	got, err := s.GetEvent(ctx, burnID)
	require.NoError(t, err)
	assert.Zero(t, got.GetReadings())
	assert.True(t, got.GetObservedAt().AsTime().Equal(d2), "off: observed_at follows the reading, as before")
}

// A reading has no search rows: its content is outside the search text. The
// revision backfill must not treat that as a missing row, or its walk would
// never end and history search would never become ready.
func TestReadingHasNoSearchRowAndBackfillEnds(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, WithReadings(true))
	seedSource(t, s, "burnline")
	no := gridv1.BurnDay_BURN_DAY_NO
	d1 := baseTime
	_, err := upsertBurn(ctx, s, burnEvent(d1, map[string]burnCall{"calaveras-apcd": {d1, no, "day 1"}}))
	require.NoError(t, err)
	for i := 1; i <= 3; i++ {
		at := d1.Add(time.Duration(i) * 24 * time.Hour)
		res, err := upsertBurn(ctx, s, burnEvent(at, map[string]burnCall{"calaveras-apcd": {at, no, "again"}}))
		require.NoError(t, err)
		require.True(t, res.Reading)
	}
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM event_revision_search WHERE event_id = $1`, burnID).Scan(&n))
	assert.Equal(t, 1, n, "only the change has a search row")

	// The current search row follows the event's revision, so the term-start
	// walk has nothing to rebuild (and no reason to move the search version).
	var stamped, current int64
	require.NoError(t, s.db.QueryRow(`SELECT s.revision, e.revision FROM event_search s JOIN events e ON e.id = s.event_id WHERE e.id = $1`, burnID).Scan(&stamped, &current))
	assert.Equal(t, current, stamped)
	walk, err := s.BackfillEventSearch(ctx, "", 100)
	require.NoError(t, err)
	assert.Zero(t, walk.Written, "a reading leaves no stale search row behind")

	res, err := s.BackfillRevisionSearch(ctx, "", 0, 2)
	require.NoError(t, err)
	assert.True(t, res.Done, "nothing is missing: readings are not candidates")
	assert.Zero(t, res.Written)

	// History search counts changes, as before readings existed.
	_, err = s.BackfillEventSearch(ctx, "", 100)
	require.NoError(t, err)
	q, err := search.ParseQuery("burn", false)
	require.NoError(t, err)
	hits, err := s.SearchHistory(ctx, HistoryQuery{Search: &q})
	require.NoError(t, err)
	assert.Equal(t, 1, hits.Total)

	// Collapsed per event, the last revision at which the text matched runs
	// on through the readings after the change: the text is the same at each.
	// Reported as the change alone, an event ending in readings always looked
	// as if its text had stopped matching.
	byEvent, err := s.SearchHistoryByEvent(ctx, HistoryQuery{Search: &q})
	require.NoError(t, err)
	require.Len(t, byEvent.Items, 1)
	assert.EqualValues(t, 4, byEvent.Items[0].LastRevision, "revision 1 matched and revisions 2-4 are readings of it")
	assert.Equal(t, 1, byEvent.Items[0].Matching)
}

// A revision row written without kind (the previous release's writer during a
// deploy) reads back as a CHANGE.
func TestRevisionWithoutKindIsAChange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	ev := testEvent("usgs:old", gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, "quake")
	_, err := s.UpsertEvent(ctx, ev)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO event_revisions (event_id, revision, observed_at, ingested_at, proto)
		SELECT id, 2, observed_at, ingested_at, proto FROM events WHERE id = 'usgs:old'`)
	require.NoError(t, err)
	revs, _, err := s.EventHistory(ctx, "usgs:old", 50, "")
	require.NoError(t, err)
	require.Len(t, revs, 2)
	assert.Equal(t, gridv1.RevisionKind_CHANGE, revs[0].GetKind())
}

// The readings count survives the previous release's writer. That writer
// rebuilds the blob from the poller's event on a change, so the blob loses
// the field while the column keeps it; a reading written afterwards must
// count from the column, or every earlier reading is lost from the count.
func TestReadingsCountSurvivesAnOlderWriter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, WithReadings(true))
	seedSource(t, s, "burnline")
	no := gridv1.BurnDay_BURN_DAY_NO
	d := baseTime
	_, err := upsertBurn(ctx, s, burnEvent(d, map[string]burnCall{"calaveras-apcd": {d, no, "day 0"}}))
	require.NoError(t, err)
	for i := 1; i <= 3; i++ {
		at := d.Add(time.Duration(i) * 24 * time.Hour)
		_, err := upsertBurn(ctx, s, burnEvent(at, map[string]burnCall{"calaveras-apcd": {at, no, "again"}}))
		require.NoError(t, err)
	}
	// The older writer's change: a blob without the field, the column untouched.
	got, err := s.GetEvent(ctx, burnID)
	require.NoError(t, err)
	got.Readings = 0
	blob, err := proto.Marshal(got)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE events SET proto = $1 WHERE id = $2`, blob, burnID)
	require.NoError(t, err)

	at := d.Add(5 * 24 * time.Hour)
	res, err := upsertBurn(ctx, s, burnEvent(at, map[string]burnCall{"calaveras-apcd": {at, no, "after the rollback"}}))
	require.NoError(t, err)
	require.True(t, res.Reading)
	got, err = s.GetEvent(ctx, burnID)
	require.NoError(t, err)
	assert.Equal(t, uint32(4), got.GetReadings())
	totals, err := s.Totals(ctx, "")
	require.NoError(t, err)
	assert.EqualValues(t, 4, totals.Readings)
}

// Without a reading test (every layer but the ones whose poller supplies
// one), a hash-equal upsert is the refresh path even with readings on.
func TestNoReadingWithoutTheLayersTest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, WithReadings(true))
	seedSource(t, s, "burnline")
	no := gridv1.BurnDay_BURN_DAY_NO
	d := baseTime
	_, err := s.UpsertEvent(ctx, burnEvent(d, map[string]burnCall{"calaveras-apcd": {d, no, "day 0"}}))
	require.NoError(t, err)
	at := d.Add(24 * time.Hour)
	res, err := s.UpsertEvent(ctx, burnEvent(at, map[string]burnCall{"calaveras-apcd": {at, no, "day 1"}}))
	require.NoError(t, err)
	assert.False(t, res.Reading)
	assert.Equal(t, []int{1}, revisionKinds(t, s, burnID))
}
