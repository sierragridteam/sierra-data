package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/search"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var allStatuses = []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED,
	gridv1.EventStatus_RESOLVED, gridv1.EventStatus_EXPIRED}

// searchStore is a store whose search index is marked complete, as it is once
// the writer's first backfill walk has run.
func searchStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	finishSearchBackfill(t, s)
	return s
}

func finishSearchBackfill(t *testing.T, s *Store) (events, revisions int) {
	t.Helper()
	ctx := context.Background()
	for after := ""; ; {
		b, err := s.BackfillEventSearch(ctx, after, 3)
		require.NoError(t, err)
		events += b.Written
		if b.Done {
			break
		}
		after = b.NextID
	}
	for id, rev := "", int64(0); ; {
		b, err := s.BackfillRevisionSearch(ctx, id, rev, 3)
		require.NoError(t, err)
		revisions += b.Written
		if b.Done {
			break
		}
		id, rev = b.NextID, b.NextRev
	}
	return events, revisions
}

func mustQuery(t *testing.T, q string) *search.Query {
	t.Helper()
	p, err := search.ParseQuery(q, true)
	require.NoError(t, err, q)
	return &p
}

// roadEvent is a CHP-style incident observed `age` after baseTime.
func roadEvent(id, headline, area, summary, description string, age time.Duration) *gridv1.Event {
	ev := testEvent(id, gridv1.Severity_MINOR, gridv1.EventStatus_ACTIVE, headline)
	ev.Layer = gridv1.Layer_ROAD_INCIDENT
	ev.Category = "incident"
	ev.AreaLabel, ev.Summary, ev.Description = area, summary, description
	ev.ObservedAt = timestamppb.New(baseTime.Add(age))
	return ev
}

func upsert(t *testing.T, s *Store, evs ...*gridv1.Event) {
	t.Helper()
	for _, ev := range evs {
		_, err := s.UpsertEvent(context.Background(), ev)
		require.NoError(t, err)
	}
}

func searchIDs(t *testing.T, s *Store, q string) []string {
	t.Helper()
	p, err := s.SearchEvents(context.Background(), EventQuery{Search: mustQuery(t, q), Statuses: allStatuses, PageSize: 200})
	require.NoError(t, err, q)
	ids := make([]string, len(p.Items))
	for i, ev := range p.Items {
		ids[i] = ev.GetId()
	}
	require.Equal(t, len(ids), p.Total, "one page holds everything here, so the total is its length")
	return ids
}

func seedRoads(t *testing.T, s *Store) {
	t.Helper()
	upsert(t, s,
		roadEvent("chp:1", "Road flooded and impassable", "Lime Kiln Rd at SR 108, near Sonora",
			"Road flooded at the intersection of Lime Kiln Road and SR 108.",
			"CHP Incident 1125-Traffic Hazard Lime Kiln Rd / Sr108 [1] ROAD FLOODED", 1*time.Hour),
		roadEvent("chp:2", "Single-vehicle crash, both lanes blocked", "State Route 26 westbound",
			"Traffic collision with no injuries reported. A vehicle is off the right shoulder.",
			"1182-Trfc Collision-No Inj Sr26 VEH OTD", 2*time.Hour),
		roadEvent("chp:3", "Tree down across the roadway", "Hwy 4 at Big Trees Rd",
			"A fallen tree is blocking the eastbound lane of Highway 4.", "TREE DOWN BLKG EB LN", 3*time.Hour),
		roadEvent("chp:4", "Collision with injuries", "Sr49 at Rawhide Rd",
			"Two vehicles collided; an ambulance is responding.", "1179-Trfc Collision-1141 Enrt Sr49", 4*time.Hour),
	)
}

func TestSearchFindsWordsStemsAndRoadsInAnySpelling(t *testing.T) {
	s := searchStore(t)
	seedRoads(t, s)
	for q, want := range map[string][]string{
		// Every word is required, in any order and case; newest first.
		"lime kiln ":        {"chp:1"},
		"KILN lime ":        {"chp:1"},
		"lime sonora crash": {},
		// Stemming: the text says "flooded", "collided", "collision".
		"flooding ":   {"chp:1"},
		"collisions ": {"chp:4", "chp:2"},
		// A road typed any way finds it written any way.
		"hwy 108":        {"chp:1"},
		"sr108":          {"chp:1"},
		"state route 26": {"chp:2"},
		"highway 26":     {"chp:2"},
		"sr-4":           {"chp:3"},
		"route 4":        {"chp:3"},
		"hwy 49":         {"chp:4"},
		"route 4 ":       {"chp:3"}, // never route 49
		// The last word typed matches as a prefix, even mid-stem.
		"floo":     {"chp:1"},
		"collisio": {"chp:4", "chp:2"},
		"ambul":    {"chp:4"},
		// A finished word (trailing space) does not.
		"floo ": {},
		// Phrases, exclusions, alternatives.
		`"eastbound lane"`:     {"chp:3"},
		`"lane eastbound"`:     {},
		"collision -injuries ": {},
		"collision -ambulance": {"chp:2"},
		"flooded OR tree ":     {"chp:3", "chp:1"},
		// The source's own abbreviated text is searchable too.
		"otd ": {"chp:2"},
		// And who reported it.
		"usgs ": {"chp:4", "chp:3", "chp:2", "chp:1"},
	} {
		got := searchIDs(t, s, q)
		if len(want) == 0 {
			assert.Empty(t, got, q)
			continue
		}
		assert.Equal(t, want, got, q)
	}
	// "collision -injuries": chp:2 says "no injuries", chp:4 "with injuries".
	assert.Empty(t, searchIDs(t, s, "collision -injuries "))
}

func TestSearchFindsIdentifiers(t *testing.T) {
	s := searchStore(t)
	node := testEvent("meshcore:950ffe39d7087803", gridv1.Severity_INFO, gridv1.EventStatus_ACTIVE, "Marelink West-1w")
	node.Layer, node.Category = gridv1.Layer_MESH, "repeater"
	node.Detail = &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: "950ffe39d7087803", Name: "Marelink West-1w", NodeType: "repeater"}}
	evac := testEvent("evac:US-CA-XCA-CCU-043", gridv1.Severity_EXTREME, gridv1.EventStatus_ACTIVE, "Evacuation Order")
	evac.Layer, evac.Category = gridv1.Layer_EVACUATION, "order"
	evac.Detail = &gridv1.Event_Evacuation{Evacuation: &gridv1.EvacuationDetail{ZoneId: "US-CA-XCA-CCU-043", Level: "ORDER"}}
	upsert(t, s, node, evac)

	assert.Equal(t, []string{node.GetId()}, searchIDs(t, s, "marelink"))
	assert.Equal(t, []string{node.GetId()}, searchIDs(t, s, "marel"))
	assert.Equal(t, []string{node.GetId()}, searchIDs(t, s, "west-1w "))
	assert.Equal(t, []string{node.GetId()}, searchIDs(t, s, "950ffe39"), "a pubkey prefix")
	assert.Equal(t, []string{evac.GetId()}, searchIDs(t, s, "CCU-043 "))
	assert.Equal(t, []string{evac.GetId()}, searchIDs(t, s, "US-CA-XCA-CCU-043 "))
}

func TestSearchHonoursFiltersAndCounts(t *testing.T) {
	s := searchStore(t)
	seedRoads(t, s)
	closure := roadEvent("caltrans:1", "Hwy 4 full closure", "Hwy 4 from Arnold to Dorrington", "", "Closed: all lanes.", 5*time.Hour)
	closure.Category, closure.Status = "closure", gridv1.EventStatus_SCHEDULED
	quake := testEvent("usgs:q", gridv1.Severity_MODERATE, gridv1.EventStatus_ACTIVE, "M 4.1 near Hwy 4")
	quake.ObservedAt = timestamppb.New(baseTime.Add(6 * time.Hour))
	upsert(t, s, closure, quake)
	ctx := context.Background()
	require.NoError(t, s.TransitionEvents(ctx, []string{"chp:3"}, gridv1.EventStatus_RESOLVED, baseTime.Add(7*time.Hour)))

	// Default statuses (ACTIVE, SCHEDULED): the resolved incident is out.
	p, err := s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, "hwy 4")})
	require.NoError(t, err)
	assert.Equal(t, 2, p.Total)
	assert.Equal(t, []EventCount{
		{gridv1.Layer_EARTHQUAKE, "", 1}, {gridv1.Layer_ROAD_INCIDENT, "closure", 1}}, p.Counts)

	p, err = s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, "hwy 4"), Statuses: allStatuses,
		Layers: []gridv1.Layer{gridv1.Layer_ROAD_INCIDENT}})
	require.NoError(t, err)
	assert.Equal(t, 2, p.Total)
	assert.Equal(t, []EventCount{
		{gridv1.Layer_ROAD_INCIDENT, "closure", 1}, {gridv1.Layer_ROAD_INCIDENT, "incident", 1}}, p.Counts)
	// Newest first: the transition moved chp:3's observed_at past the closure's.
	require.Len(t, p.Items, 2)
	assert.Equal(t, "chp:3", p.Items[0].GetId())

	p, err = s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, "hwy 4"), Statuses: allStatuses,
		MinSeverity: gridv1.Severity_MODERATE, Since: baseTime.Add(5 * time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, 1, p.Total)

	// Counts without a text query: the ordinary severity-ordered page, plus
	// the total and its breakdown.
	p, err = s.SearchEvents(ctx, EventQuery{Counts: true, Statuses: allStatuses})
	require.NoError(t, err)
	assert.Equal(t, 6, p.Total)
	assert.Equal(t, []EventCount{
		{gridv1.Layer_EARTHQUAKE, "", 1}, {gridv1.Layer_ROAD_INCIDENT, "closure", 1},
		{gridv1.Layer_ROAD_INCIDENT, "incident", 4}}, p.Counts)
	assert.Equal(t, "usgs:q", p.Items[0].GetId(), "severity leads when there is no text query")
	assert.Nil(t, p.Snippets)

	// Neither: exactly QueryEventsPage, and no total.
	p, err = s.SearchEvents(ctx, EventQuery{Statuses: allStatuses})
	require.NoError(t, err)
	assert.Equal(t, -1, p.Total)
	assert.Len(t, p.Items, 6)
}

func TestSearchPagesAreCompleteAndNewestFirst(t *testing.T) {
	s := searchStore(t)
	var want []string
	for i := 0; i < 23; i++ {
		// Pairs share an observed_at, so the id tie-break is exercised.
		id := fmt.Sprintf("chp:%02d", i)
		upsert(t, s, roadEvent(id, "Rockslide", "Hwy 4", "", "", time.Duration(i/2)*time.Hour))
		want = append(want, id)
	}
	upsert(t, s, roadEvent("chp:other", "Crash", "Hwy 4", "", "", time.Hour))
	// Newest first, ids ascending within one observed_at.
	var sorted []string
	for hour := 11; hour >= 0; hour-- {
		for _, id := range want {
			var n int
			fmt.Sscanf(id, "chp:%d", &n)
			if n/2 == hour {
				sorted = append(sorted, id)
			}
		}
	}
	var got []string
	q := EventQuery{Search: mustQuery(t, "rockslide "), PageSize: 5}
	for pages := 0; ; pages++ {
		p, err := s.SearchEvents(context.Background(), q)
		require.NoError(t, err)
		assert.Equal(t, 23, p.Total, "the total is over every page")
		for _, ev := range p.Items {
			got = append(got, ev.GetId())
		}
		if q.PageToken = p.Next(len(p.Items)); q.PageToken == "" {
			assert.Equal(t, 4, pages)
			break
		}
	}
	assert.Equal(t, sorted, got)
}

func TestSearchSnippets(t *testing.T) {
	s := searchStore(t)
	seedRoads(t, s)
	ctx := context.Background()
	text := func(sn Snippet) (all string, hits []string) {
		for _, p := range sn.Parts {
			all += p.Text
			if p.Hit {
				hits = append(hits, p.Text)
			}
		}
		return
	}

	p, err := s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, "flooding sonora ")})
	require.NoError(t, err)
	sn := p.Snippets["chp:1"]
	all, hits := text(sn)
	// "Sonora" is only in the location, "flooded" in both: the body wins when
	// it has a hit.
	assert.Equal(t, "text", sn.Field)
	assert.Contains(t, all, "Road flooded at the intersection")
	assert.Contains(t, hits, "flooded")

	p, err = s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, "sonora ")})
	require.NoError(t, err)
	sn = p.Snippets["chp:1"]
	all, hits = text(sn)
	assert.Equal(t, "title", sn.Field)
	assert.Equal(t, []string{"Sonora"}, hits)
	assert.Contains(t, all, "near Sonora")
	assert.NotContains(t, all, hitStart)

	// A road is highlighted as it is written, whatever spelling was typed.
	p, err = s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, "highway 108 flooded ")})
	require.NoError(t, err)
	all, hits = text(p.Snippets["chp:1"])
	assert.Contains(t, hits, "SR 108")
	assert.NotContains(t, all, "route108")
	p, err = s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, "sr 49 ")})
	require.NoError(t, err)
	all, hits = text(p.Snippets["chp:4"])
	assert.Equal(t, []string{"Sr49"}, hits)

	// Matched only on the source name: no snippet, not an error.
	p, err = s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, "usgs ")})
	require.NoError(t, err)
	assert.Empty(t, p.Snippets["chp:1"].Parts)
}

func TestSnippetPartsKeepSpacing(t *testing.T) {
	got := snippetParts("Road " + hitStart + "flooded" + hitStop + ", near\nthe " + hitStart + "bridge" + hitStop)
	assert.Equal(t, []SnippetPart{{"Road ", false}, {"flooded", true}, {", near the ", false}, {"bridge", true}}, got)
	var joined strings.Builder
	for _, p := range got {
		joined.WriteString(p.Text)
	}
	assert.Equal(t, "Road flooded, near the bridge", joined.String())
}

func TestSearchRefusesWhatItCannotAnswer(t *testing.T) {
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	seedRoads(t, s)
	ctx := context.Background()

	// Before the backfill has walked the table the index is not known to be
	// complete: fail, do not answer "no matches".
	_, err := s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, "flooded ")})
	assert.ErrorIs(t, err, ErrSearchNotReady)
	_, err = s.SearchEvents(ctx, EventQuery{Counts: true})
	assert.ErrorIs(t, err, ErrSearchNotReady)
	_, err = s.SearchHistory(ctx, HistoryQuery{Search: mustQuery(t, "flooded ")})
	assert.ErrorIs(t, err, ErrSearchNotReady)

	finishSearchBackfill(t, s)
	assert.Equal(t, []string{"chp:1"}, searchIDs(t, s, "flooded "))

	// Words the language drops are not a query, typed or finished.
	for _, q := range []string{"the of ", "the", "of the"} {
		_, err = s.SearchEvents(ctx, EventQuery{Search: mustQuery(t, q)})
		assert.ErrorIs(t, err, search.ErrQueryEmpty, q)
	}
	// Whole words unless the query says it is still being typed.
	whole, err := search.ParseQuery("floo", false)
	require.NoError(t, err)
	p, err := s.SearchEvents(ctx, EventQuery{Search: &whole})
	require.NoError(t, err)
	assert.Zero(t, p.Total)
}

func searchRowXmin(t *testing.T, s *Store, id string) (xmin string, revision int64) {
	t.Helper()
	require.NoError(t, s.db.QueryRow(`SELECT xmin::text, revision FROM event_search WHERE event_id = $1`, id).Scan(&xmin, &revision))
	return
}

// Every write that can change an event's text writes its search row in the
// same transaction; the writes that cannot (telemetry, liveness) leave it
// alone, which is the reason the index is not on events itself.
func TestSearchRowsFollowEveryWritePath(t *testing.T) {
	s := searchStore(t)
	ctx := context.Background()
	ev := roadEvent("chp:1", "Road flooded", "SR 108", "", "", time.Hour)
	upsert(t, s, ev)
	require.Equal(t, []string{"chp:1"}, searchIDs(t, s, "flooded "))

	// A content change: new revision, new text, old text gone from the
	// current index but kept in the revision's.
	ev2 := roadEvent("chp:1", "Road reopened", "SR 108", "", "", 2*time.Hour)
	upsert(t, s, ev2)
	assert.Empty(t, searchIDs(t, s, "flooded "))
	assert.Equal(t, []string{"chp:1"}, searchIDs(t, s, "reopened "))
	var revRows int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM event_revision_search WHERE event_id = 'chp:1'`).Scan(&revRows))
	assert.Equal(t, 2, revRows)

	// A late AI summary on the hash-equal path: no revision, but searchable.
	ev3 := roadEvent("chp:1", "Road reopened", "SR 108", "Crews cleared the culvert.", "", 2*time.Hour)
	ev3.Enhancement = &gridv1.Enhancement{Model: "test"}
	res, err := s.UpsertEvent(ctx, ev3)
	require.NoError(t, err)
	require.False(t, res.Changed)
	assert.Equal(t, []string{"chp:1"}, searchIDs(t, s, "culvert "))

	// A transition: the revision moves, the text does not.
	require.NoError(t, s.TransitionEvents(ctx, []string{"chp:1"}, gridv1.EventStatus_RESOLVED, baseTime.Add(3*time.Hour)))
	_, rev := searchRowXmin(t, s, "chp:1")
	assert.Equal(t, int64(3), rev)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM event_revision_search WHERE event_id = 'chp:1'`).Scan(&revRows))
	assert.Equal(t, 3, revRows)

	// Liveness and a mesh telemetry refresh do not write the search row.
	node := testEvent("meshcore:aa", gridv1.Severity_INFO, gridv1.EventStatus_ACTIVE, "Node A")
	node.Layer = gridv1.Layer_MESH
	node.Detail = &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: "aa", Name: "Node A", Telemetry: &gridv1.MeshTelemetry{Gateways: []string{"g1"}}}}
	upsert(t, s, node)
	before, _ := searchRowXmin(t, s, "meshcore:aa")
	node.GetMesh().Telemetry = &gridv1.MeshTelemetry{Gateways: []string{"g2"}}
	res, err = s.UpsertEvent(ctx, node)
	require.NoError(t, err)
	require.False(t, res.Changed)
	stored, err := s.GetEvent(ctx, "meshcore:aa")
	require.NoError(t, err)
	require.Equal(t, []string{"g2"}, stored.GetMesh().GetTelemetry().GetGateways(), "the refresh did rewrite the blob")
	_, err = s.TouchSeen(ctx, []string{"meshcore:aa"}, time.Now(), time.Now())
	require.NoError(t, err)
	after, _ := searchRowXmin(t, s, "meshcore:aa")
	assert.Equal(t, before, after, "the search row was not rewritten")
}

// The backfill builds rows for events the search-aware writer never wrote
// (stored before the migration, or written by the previous release during a
// deploy) and rebuilds rows that are behind their event or built by old rules.
func TestSearchBackfillCatchesUp(t *testing.T) {
	s := newTestStore(t)
	seedSource(t, s, "usgs")
	seedRoads(t, s)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := s.db.Exec(q, args...)
		require.NoError(t, err)
	}
	// As a release without search would have left it.
	exec(`DELETE FROM event_search`)
	exec(`DELETE FROM event_revision_search`)
	exec(`DELETE FROM versions WHERE key LIKE 'search%'`)
	s.versions.markDirty()
	sv0, err := s.SearchVersion(ctx)
	require.NoError(t, err)

	events, revisions := finishSearchBackfill(t, s)
	assert.Equal(t, 4, events)
	assert.Equal(t, 4, revisions)
	assert.Equal(t, []string{"chp:1"}, searchIDs(t, s, "flooded "))
	sv1, err := s.SearchVersion(ctx)
	require.NoError(t, err)
	assert.Greater(t, sv1, sv0, "a backfill moves results without a revision, so it moves the validator")

	// A second walk finds nothing to do.
	events, revisions = finishSearchBackfill(t, s)
	assert.Zero(t, events+revisions)
	sv2, err := s.SearchVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, sv1, sv2)

	// The previous release's writer revised an event without touching the
	// index: the row is behind its event.
	exec(`UPDATE event_search SET revision = 0, text_hash = 'old', tsv = to_tsvector('simple', 'stale') WHERE event_id = 'chp:2'`)
	// Built by older rules.
	exec(`UPDATE event_search SET doc_version = 0, tsv = to_tsvector('simple', 'stale') WHERE event_id = 'chp:3'`)
	exec(`UPDATE event_revision_search SET doc_version = 0, tsv = to_tsvector('simple', 'stale') WHERE event_id = 'chp:4'`)
	events, revisions = finishSearchBackfill(t, s)
	assert.Equal(t, 2, events)
	assert.Equal(t, 1, revisions)
	assert.Empty(t, searchIDs(t, s, "stale "))
	assert.Equal(t, []string{"chp:3"}, searchIDs(t, s, "tree "))
}

// --- history ---

func seedHistory(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	// A chain-control story on 88: R1, then R2, then lifted.
	for i, h := range []string{"Chain controls R1 on SR 88", "Chain controls R2 on SR 88", "Chain controls R2 on SR 88 at Peddler Hill", "No chain controls on SR 88"} {
		upsert(t, s, roadEvent("cc:88", h, "SR 88 Carson Spur", "", "", time.Duration(i+1)*time.Hour))
	}
	// An order later downgraded: the current text no longer says "order".
	upsert(t, s, roadEvent("evac:1", "Evacuation Order for Swiss Ranch", "CALAVERAS", "", "", 2*time.Hour))
	upsert(t, s, roadEvent("evac:1", "Evacuation Warning for Swiss Ranch", "CALAVERAS", "", "", 6*time.Hour))
	require.NoError(t, s.TransitionEvents(ctx, []string{"evac:1"}, gridv1.EventStatus_RESOLVED, baseTime.Add(9*time.Hour)))
	// Noise.
	upsert(t, s, roadEvent("chp:9", "Crash on SR 88", "SR 88 at Ham's Station", "", "", 5*time.Hour))
}

func TestSearchHistoryFindsWhatTheTextOnceSaid(t *testing.T) {
	s := searchStore(t)
	seedHistory(t, s)
	ctx := context.Background()

	// The current index no longer has the order; history does.
	assert.Empty(t, searchIDs(t, s, "order "))
	p, err := s.SearchHistory(ctx, HistoryQuery{Search: mustQuery(t, "evacuation order ")})
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	assert.Equal(t, 1, p.Total)
	assert.EqualValues(t, 1, p.Items[0].GetRevision())
	assert.Equal(t, "Evacuation Order for Swiss Ranch", p.Items[0].GetEvent().GetHeadline())
	require.Len(t, p.Snippets, 1)
	assert.Equal(t, "title", p.Snippets[0].Field)

	// Per revision, newest first, the archive's own order.
	p, err = s.SearchHistory(ctx, HistoryQuery{Search: mustQuery(t, "r2 ")})
	require.NoError(t, err)
	require.Len(t, p.Items, 2)
	assert.EqualValues(t, 3, p.Items[0].GetRevision())
	assert.EqualValues(t, 2, p.Items[1].GetRevision())

	// The window is over the revision's observed_at.
	p, err = s.SearchHistory(ctx, HistoryQuery{Search: mustQuery(t, "route 88 "), From: baseTime.Add(4 * time.Hour), To: baseTime.Add(6 * time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, 2, p.Total, "the lifted-controls revision and the crash")

	// Paging walks every match once.
	var seen []string
	hq := HistoryQuery{Search: mustQuery(t, "route 88 "), PageSize: 2}
	for {
		p, err := s.SearchHistory(ctx, hq)
		require.NoError(t, err)
		assert.Equal(t, 5, p.Total)
		for _, r := range p.Items {
			seen = append(seen, fmt.Sprintf("%s@%d", r.GetEvent().GetId(), r.GetRevision()))
		}
		if hq.PageToken = p.Next(len(p.Items)); hq.PageToken == "" {
			break
		}
	}
	assert.Equal(t, []string{"chp:9@1", "cc:88@4", "cc:88@3", "cc:88@2", "cc:88@1"}, seen)
}

func TestSearchHistoryByEventCollapses(t *testing.T) {
	s := searchStore(t)
	seedHistory(t, s)
	ctx := context.Background()

	// "When did R2 first appear on 88?" One row: first at revision 2, last at
	// 3, two matching updates.
	p, err := s.SearchHistoryByEvent(ctx, HistoryQuery{Search: mustQuery(t, "r2 route 88 ")})
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	assert.Equal(t, 1, p.Total)
	m := p.Items[0]
	assert.Equal(t, "cc:88", m.EventID)
	assert.EqualValues(t, 2, m.First.GetRevision())
	assert.Equal(t, baseTime.Add(2*time.Hour).Unix(), m.First.GetObservedAt().AsTime().Unix())
	assert.EqualValues(t, 3, m.LastRevision)
	assert.Equal(t, baseTime.Add(3*time.Hour).Unix(), m.LastObservedAt)
	assert.Equal(t, 2, m.Matching)
	assert.Equal(t, "title", m.Snippet.Field)

	// Several events, ordered by their latest match; pages are complete.
	var seen []string
	hq := HistoryQuery{Search: mustQuery(t, "route 88 OR evacuation "), PageSize: 1}
	for {
		p, err := s.SearchHistoryByEvent(ctx, hq)
		require.NoError(t, err)
		assert.Equal(t, 3, p.Total)
		for _, m := range p.Items {
			seen = append(seen, fmt.Sprintf("%s:%d", m.EventID, m.Matching))
		}
		if hq.PageToken = p.Next(len(p.Items)); hq.PageToken == "" {
			break
		}
	}
	// evac:1's resolve at +9h is its latest matching revision.
	assert.Equal(t, []string{"evac:1:3", "chp:9:1", "cc:88:4"}, seen)

	// A window narrows which revisions count: First is the earliest inside it.
	p, err = s.SearchHistoryByEvent(ctx, HistoryQuery{Search: mustQuery(t, "chain controls "), From: baseTime.Add(150 * time.Minute)})
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	assert.EqualValues(t, 3, p.Items[0].First.GetRevision())
	assert.Equal(t, 2, p.Items[0].Matching)
}

func TestIndexTextSplitsHyphensNextToDigits(t *testing.T) {
	for in, want := range map[string]string{
		"US-CA-XCA-CCU-043":    "US-CA-XCA-CCU 043",
		"West-1w":              "West 1w",
		"SR-26":                "SR 26",
		"1-w":                  "1 w",
		"3622-4027":            "3622 4027",
		"one-way traffic":      "one-way traffic",
		"Mud/Dirt/Rock":        "Mud Dirt Rock",
		"Lime Kiln Rd / Sr108": "Lime Kiln Rd   Sr108",
		"a\x00b":               "a b",
	} {
		assert.Equal(t, want, indexText(in), in)
	}
}

// The finder ranks: where the words matched first, then live and recent.
func TestSearchTopEventsRanks(t *testing.T) {
	s := searchStore(t)
	ctx := context.Background()
	now := time.Now()
	at := func(ev *gridv1.Event, age time.Duration, status gridv1.EventStatus) *gridv1.Event {
		ev.ObservedAt = timestamppb.New(now.Add(-age))
		ev.Status = status
		return ev
	}
	fire := roadEvent("calfire:priest", "Priest Fire — 10 ac, 100% contained", "Old Priest Grade and Highway 120", "", "", 0)
	fire.Layer, fire.Category = gridv1.Layer_WILDFIRE, "wildfire"
	node := testEvent("meshcore:aa", gridv1.Severity_INFO, gridv1.EventStatus_ACTIVE, "Priest Grade Repeater")
	node.Layer = gridv1.Layer_MESH
	node.Detail = &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: "aa", Name: "Priest Grade Repeater"}}
	upsert(t, s,
		// The named fire, months old and over.
		at(fire, 120*24*time.Hour, gridv1.EventStatus_RESOLVED),
		// Yesterday's incident that mentions both words only in passing.
		at(roadEvent("chp:passing", "Traffic hazard reported", "SR 120", "",
			"Units from CAL FIRE staged near Old Priest Grade.", 0), 24*time.Hour, gridv1.EventStatus_RESOLVED),
		// Two crashes on the same road, one live now and one from the summer.
		at(roadEvent("chp:new", "Crash on SR 120", "SR 120 at Old Priest", "", "", 0), time.Hour, gridv1.EventStatus_ACTIVE),
		at(roadEvent("chp:old", "Crash on SR 120", "SR 120 at Old Priest", "", "", 0), 100*24*time.Hour, gridv1.EventStatus_RESOLVED),
		node,
	)
	top := func(q string, layers, exclude []gridv1.Layer, limit int) ([]string, int) {
		t.Helper()
		hits, total, err := s.SearchTopEvents(ctx, *mustQuery(t, q+" "), "", layers, exclude, limit)
		require.NoError(t, err)
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.Event.GetId()
			assert.Empty(t, h.Event.GetGeometry().GetGeojson(), "ranked hits carry no shapes")
		}
		return ids, total
	}
	mesh := []gridv1.Layer{gridv1.Layer_MESH}

	// A name in the headline beats a recent mention in the description.
	ids, total := top("priest fire", nil, mesh, 5)
	assert.Equal(t, []string{"calfire:priest", "chp:passing"}, ids)
	assert.Equal(t, 2, total)

	// Equal text: the live, recent one first.
	ids, _ = top("crash", nil, mesh, 5)
	assert.Equal(t, []string{"chp:new", "chp:old"}, ids)

	// The limit cuts the list, not the total; layers split the kinds.
	ids, total = top("priest", nil, mesh, 2)
	assert.Len(t, ids, 2)
	assert.Equal(t, 4, total)
	ids, total = top("priest", mesh, nil, 5)
	assert.Equal(t, []string{"meshcore:aa"}, ids)
	assert.Equal(t, 1, total)

	// Nothing: zero, no error. A snippet rides along when there is one.
	ids, total = top("volcano", nil, nil, 5)
	assert.Empty(t, ids)
	assert.Zero(t, total)
	hits, _, err := s.SearchTopEvents(ctx, *mustQuery(t, "staged "), "", nil, nil, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "text", hits[0].Snippet.Field)
}
