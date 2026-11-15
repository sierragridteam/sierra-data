package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/search"
	"google.golang.org/protobuf/proto"
)

// Full-text search (migration 0004: event_search, event_revision_search).
//
// Both tables are derived from the proto blobs, like event_lite. The rule is
// the same: every write of events.proto that can change the search text
// writes event_search in the same transaction, and every insert into
// event_revisions writes event_revision_search with it. The text comes from
// search.Build; nothing here is canonical.
//
// What can change the text: a new revision (UpsertEvent, TransitionEvents) and
// a fresh AI summary on the hash-equal path. Mesh telemetry, burn observations
// and place ids are not in the text, so those refreshes write nothing here.

// ErrSearchNotReady is returned for a text query while the index is still
// being built (a new database, or the first boot of a release that added
// search). An incomplete index would answer "no matches" for events that
// exist, so the query fails instead. The API maps it to 503.
var ErrSearchNotReady = errors.New("store: the search index is still being built")

// ErrSearchTimeout is returned when a search statement ran past
// searchStatementTimeout. The endpoints are public; a query must not be able
// to hold a connection for long.
var ErrSearchTimeout = errors.New("store: the search took too long")

// searchStatementTimeout bounds each statement of a search. Measured on the
// stored events (12k, 2026-11-15) a search with its count and snippets takes
// about 20 ms.
const searchStatementTimeout = "5s"

// searchTx is readTx with the statement timeout set, and a timeout reported
// as ErrSearchTimeout.
func (s *Store) searchTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+searchStatementTimeout+`'`); err != nil {
			return fmt.Errorf("store: set search timeout: %w", err)
		}
		return fn(tx)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" { // query_canceled
		return ErrSearchTimeout
	}
	return err
}

const (
	// versions keys. ready:* hold the search.DocVersion a complete backfill
	// walk last finished at (0 or absent: never). search counts backfill
	// writes, which change results without a revision, for the validators.
	versionSearchReady        = "search_ready:events"
	versionSearchHistoryReady = "search_ready:revisions"
	versionSearch             = "search"
)

// tsvExpr builds the weighted vector from four text parameters. English
// stemming everywhere ("flooded" finds "flooding"); an unstemmed copy of the
// title and summary so a half-typed word still matches by prefix ("collisio"
// is not a prefix of the stem "collis").
func tsvExpr(a, b, c, d string) string {
	return `(setweight(to_tsvector('english', ` + a + `), 'A') || setweight(to_tsvector('english', ` + b + `), 'B') || ` +
		`setweight(to_tsvector('english', ` + c + `), 'C') || setweight(to_tsvector('english', ` + d + `), 'D') || ` +
		`setweight(to_tsvector('simple', ` + a + `), 'A') || setweight(to_tsvector('simple', ` + b + `), 'B'))`
}

// tsqueryExpr renders q as a tsquery expression with its words bound as
// parameters. Every function is called with constants only, so PostgreSQL
// evaluates the expression once and can serve `tsv @@ <expr>` from GIN.
func tsqueryExpr(q search.Query, args *sqlArgs) string { return tsqueryExprFor(q, args, false) }

// tsqueryExprFor renders q for matching (forSnippet false) or for cutting a
// snippet from text prepared by search.MarkRoutes, where a road is found by
// its placeholder words instead of its token.
func tsqueryExprFor(q search.Query, args *sqlArgs, forSnippet bool) string {
	var groups []string
	for _, g := range q.Any {
		var terms []string
		for _, t := range g.Terms {
			var e string
			switch {
			case t.Phrase:
				e = `phraseto_tsquery('english', ` + args.add(indexText(t.Text)) + `)`
			case forSnippet && search.IsRouteToken(t.Text):
				e = `to_tsquery('simple', ` + args.add(search.RoutePlaceholder(t.Text)+":*") + `)`
			case t.Prefix:
				// search.ParseQuery only marks a word of letters and digits,
				// so "word:*" is always valid to_tsquery input. The unstemmed
				// alternative is for a half-typed word; a word English drops
				// ("the") must stay dropped, or it would match "then", "there"
				// and most of the table.
				p := args.add(t.Text + ":*")
				e = `(CASE WHEN numnode(to_tsquery('english', ` + p + `)) = 0 THEN to_tsquery('english', ` + p + `)` +
					` ELSE to_tsquery('english', ` + p + `) || to_tsquery('simple', ` + p + `) END)`
			default:
				e = `plainto_tsquery('english', ` + args.add(indexText(t.Text)) + `)`
			}
			if t.Exclude {
				e = `!!` + e
			}
			terms = append(terms, e)
		}
		groups = append(groups, `(`+strings.Join(terms, ` && `)+`)`)
	}
	return `(` + strings.Join(groups, ` || `) + `)`
}

func docHash(d search.Doc) string {
	sum := sha256.Sum256([]byte(d.Text()))
	return hex.EncodeToString(sum[:16])
}

// searchRow is one event's search document, ready to write.
type searchRow struct {
	id         string
	revision   int64
	observedAt int64
	doc        search.Doc
}

func newSearchRow(ev *gridv1.Event, revision uint32, observedAt int64) searchRow {
	return searchRow{id: ev.GetId(), revision: int64(revision), observedAt: observedAt, doc: search.Build(ev)}
}

// eventSearchUpsert is the ON CONFLICT clause every event_search write
// shares. A row whose text is unchanged keeps its stored vector (the same
// datum, so the update stays heap-only and the GIN index is not touched);
// only the bookkeeping moves.
const eventSearchUpsert = `
	ON CONFLICT (event_id) DO UPDATE SET
	  category = excluded.category, revision = excluded.revision, doc_version = excluded.doc_version,
	  text_hash = excluded.text_hash,
	  tsv = CASE WHEN event_search.text_hash = excluded.text_hash AND event_search.doc_version = excluded.doc_version
	             THEN event_search.tsv ELSE excluded.tsv END
	WHERE event_search.text_hash <> excluded.text_hash OR event_search.doc_version <> excluded.doc_version
	   OR event_search.revision <> excluded.revision OR event_search.category <> excluded.category`

// putSearch writes the search rows for events in one statement: the current
// text of each (event_search) and, when withRevisions, the snapshot row of the
// revision just stored (event_revision_search). One round trip whatever the
// count. Each id may appear once.
func putSearch(ctx context.Context, tx *sql.Tx, rows []searchRow, withRevisions bool) error {
	if len(rows) == 0 {
		return nil
	}
	n := len(rows)
	ids, cats, hashes := make([]string, n), make([]string, n), make([]string, n)
	revs, obs := make([]int64, n), make([]int64, n)
	a, b, c, d := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	for i, r := range rows {
		ids[i], cats[i], hashes[i] = r.id, r.doc.Category, docHash(r.doc)
		revs[i], obs[i] = r.revision, r.observedAt
		da, db, dc, dd := r.doc.Indexed()
		a[i], b[i], c[i], d[i] = indexText(da), indexText(db), indexText(dc), indexText(dd)
	}
	stmt := `
		WITH v AS (
		  SELECT id, category, text_hash, revision, observed_at, ` + tsvExpr("a", "b", "c", "d") + ` AS tsv
		  FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[], $5::bigint[],
		              $6::text[], $7::text[], $8::text[], $9::text[])
		       AS u(id, category, text_hash, revision, observed_at, a, b, c, d)
		)`
	if withRevisions {
		stmt += `, cur AS (
		  INSERT INTO event_search (event_id, category, tsv, text_hash, doc_version, revision)
		  SELECT id, category, tsv, text_hash, $10, revision FROM v` + eventSearchUpsert + `
		)
		INSERT INTO event_revision_search (event_id, revision, observed_at, tsv, doc_version)
		SELECT id, revision, observed_at, tsv, $10 FROM v
		ON CONFLICT (event_id, revision) DO UPDATE SET
		  observed_at = excluded.observed_at, tsv = excluded.tsv, doc_version = excluded.doc_version`
	} else {
		stmt += `
		INSERT INTO event_search (event_id, category, tsv, text_hash, doc_version, revision)
		SELECT id, category, tsv, text_hash, $10, revision FROM v` + eventSearchUpsert
	}
	if _, err := tx.ExecContext(ctx, stmt, ids, cats, hashes, revs, obs, a, b, c, d, search.DocVersion); err != nil {
		return fmt.Errorf("store: write search rows for %d events: %w", n, err)
	}
	return nil
}

// indexText prepares document text for to_tsvector, and tsqueryExpr applies
// the same rules to query words, so both sides tokenise alike.
//
//   - PostgreSQL's parser reads a hyphen before a digit as a minus sign: the
//     query "CCU-043" becomes 'ccu' & '-043', while the stored zone id
//     "US-CA-XCA-CCU-043" yields '043', and they never meet. A hyphen next to
//     a digit is therefore a space ("West-1w", "SR-26", "I-80").
//   - It reads words joined by a slash as a file path, one token: CHP's
//     "SLIDE-Mud/Dirt/Rock" and "Mud/rock slide" would not be found by "rock".
//     A slash is a space.
func indexText(s string) string {
	s = strings.NewReplacer("/", " ", "\\", " ").Replace(pgText(s))
	return hyphenDigit.ReplaceAllString(s, "$1$3 $2$4")
}

var hyphenDigit = regexp.MustCompile(`([\pL\pN])-(\pN)|(\pN)-([\pL\pN])`)

// pgText makes s storable as PostgreSQL text: no NUL byte, valid UTF-8.
// Upstream text is neither guaranteed.
func pgText(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", " ")
	}
	return strings.ToValidUTF8(s, " ")
}

// --- readiness ---

// SearchReady reports whether a complete index exists for current events and
// for revisions. False until the writer's first backfill walk has finished.
func (s *Store) SearchReady(ctx context.Context) (events, revisions bool, err error) {
	vals, err := s.versions.get(ctx, s.db)
	if err != nil {
		return false, false, fmt.Errorf("store: search readiness: %w", err)
	}
	return vals[versionSearchReady] > 0, vals[versionSearchHistoryReady] > 0, nil
}

// SearchVersion changes whenever a backfill rewrites search rows, which moves
// search results without a revision. Validators of a text query fold it in.
func (s *Store) SearchVersion(ctx context.Context) (int64, error) {
	vals, err := s.versions.get(ctx, s.db)
	if err != nil {
		return 0, fmt.Errorf("store: search version: %w", err)
	}
	return vals[versionSearch] + vals[versionSearchReady] + vals[versionSearchHistoryReady], nil
}

func setVersion(ctx context.Context, tx *sql.Tx, key string, value int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO versions (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value WHERE versions.value <> excluded.value`, key, value); err != nil {
		return fmt.Errorf("store: set version %s: %w", key, err)
	}
	return nil
}

// --- backfill ---

// SearchBackfill is one step of a backfill walk.
type SearchBackfill struct {
	// Done is true when the walk reached the end of the table.
	Done bool
	// Written counts rows built in this step, Skipped blobs that would not
	// unmarshal (left without a row, like a lite copy).
	Written, Skipped int
	// The cursor for the next step.
	NextID  string
	NextRev int64
}

// BackfillEventSearch builds event_search rows for up to batch events after
// the cursor that have none, or whose row was built from another revision or
// by other rules (an older release's writer does not maintain the table; a
// search.DocVersion bump rebuilds everything). The read and the write share
// one writer transaction, so no upsert can land between them. When the walk
// reaches the end it marks the index complete.
func (s *Store) BackfillEventSearch(ctx context.Context, after string, batch int) (SearchBackfill, error) {
	var res SearchBackfill
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res = SearchBackfill{}
		// The shape-free copy where there is one: search text needs no
		// geometry, and a perimeter is most of an event's bytes.
		rows, err := tx.QueryContext(ctx, `
			SELECT e.id, e.revision, e.observed_at, COALESCE(l.proto, e.proto)
			FROM events e LEFT JOIN event_search s ON s.event_id = e.id
			LEFT JOIN event_lite l ON l.event_id = e.id
			WHERE e.id > $1 AND (s.event_id IS NULL OR s.revision <> e.revision OR s.doc_version <> $2)
			ORDER BY e.id LIMIT $3`, after, search.DocVersion, batch)
		if err != nil {
			return fmt.Errorf("store: search backfill scan: %w", err)
		}
		var out []searchRow
		n := 0
		for rows.Next() {
			var id string
			var rev, obs int64
			var blob []byte
			if err := rows.Scan(&id, &rev, &obs, &blob); err != nil {
				rows.Close()
				return fmt.Errorf("store: search backfill scan: %w", err)
			}
			n++
			res.NextID = id
			ev := &gridv1.Event{}
			if err := proto.Unmarshal(blob, ev); err != nil {
				res.Skipped++
				continue
			}
			out = append(out, searchRow{id: id, revision: rev, observedAt: obs, doc: search.Build(ev)})
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: search backfill scan: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: search backfill scan: %w", err)
		}
		res.Written = len(out)
		if err := putSearch(ctx, tx, out, false); err != nil {
			return err
		}
		if len(out) > 0 {
			if err := bumpVersion(tx, versionSearch, int64(len(out))); err != nil {
				return err
			}
		}
		if n < batch {
			res.Done = true
			return setVersion(ctx, tx, versionSearchReady, search.DocVersion)
		}
		return nil
	})
	if err != nil {
		return SearchBackfill{}, err
	}
	return res, nil
}

// revisionBackfillBytes is the most revision blob bytes one backfill step
// reads (after its first row).
const revisionBackfillBytes = 8 << 20

// BackfillRevisionSearch is BackfillEventSearch for event_revision_search:
// revisions after (afterID, afterRev) in primary-key order that have no row
// or one built by other rules.
func (s *Store) BackfillRevisionSearch(ctx context.Context, afterID string, afterRev int64, batch int) (SearchBackfill, error) {
	var res SearchBackfill
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res = SearchBackfill{}
		// Revisions are full snapshots, shapes included, and a fire's
		// perimeter revisions run to megabytes each. The batch is cut where
		// its blobs pass revisionBackfillBytes (the first row always comes),
		// so one step never holds the writer lock, or this process's memory,
		// for a hundred perimeters. octet_length reads a stored size, not the
		// blob. candidates is how many rows the LIMIT found, cut or not.
		rows, err := tx.QueryContext(ctx, `
			SELECT event_id, revision, observed_at, proto, candidates FROM (
			  SELECT c.*, COUNT(*) OVER () AS candidates,
			         SUM(octet_length(c.proto)) OVER (ORDER BY c.event_id, c.revision) - octet_length(c.proto) AS before
			  FROM (
			    SELECT r.event_id, r.revision, r.observed_at, r.proto
			    FROM event_revisions r
			    LEFT JOIN event_revision_search s ON s.event_id = r.event_id AND s.revision = r.revision
			    WHERE (r.event_id, r.revision) > ($1, $2) AND (s.event_id IS NULL OR s.doc_version <> $3)
			    ORDER BY r.event_id, r.revision LIMIT $4
			  ) c
			) t WHERE before < $5 ORDER BY event_id, revision`, afterID, afterRev, search.DocVersion, batch, revisionBackfillBytes)
		if err != nil {
			return fmt.Errorf("store: revision search backfill scan: %w", err)
		}
		var ids []string
		var revs, obs []int64
		var a, b, c, d []string
		n, candidates := 0, 0
		for rows.Next() {
			var id string
			var rev, o int64
			var blob []byte
			if err := rows.Scan(&id, &rev, &o, &blob, &candidates); err != nil {
				rows.Close()
				return fmt.Errorf("store: revision search backfill scan: %w", err)
			}
			n++
			res.NextID, res.NextRev = id, rev
			ev := &gridv1.Event{}
			if err := proto.Unmarshal(blob, ev); err != nil {
				res.Skipped++
				continue
			}
			doc := search.Build(ev)
			ids, revs, obs = append(ids, id), append(revs, rev), append(obs, o)
			da, db, dc, dd := doc.Indexed()
			a, b, c, d = append(a, indexText(da)), append(b, indexText(db)), append(c, indexText(dc)), append(d, indexText(dd))
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: revision search backfill scan: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: revision search backfill scan: %w", err)
		}
		res.Written = len(ids)
		if len(ids) > 0 {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO event_revision_search (event_id, revision, observed_at, tsv, doc_version)
				SELECT id, revision, observed_at, `+tsvExpr("a", "b", "c", "d")+`, $8
				FROM unnest($1::text[], $2::bigint[], $3::bigint[], $4::text[], $5::text[], $6::text[], $7::text[])
				     AS u(id, revision, observed_at, a, b, c, d)
				ON CONFLICT (event_id, revision) DO UPDATE SET
				  observed_at = excluded.observed_at, tsv = excluded.tsv, doc_version = excluded.doc_version`,
				ids, revs, obs, a, b, c, d, search.DocVersion); err != nil {
				return fmt.Errorf("store: write %d revision search rows: %w", len(ids), err)
			}
			if err := bumpVersion(tx, versionSearch, int64(len(ids))); err != nil {
				return err
			}
		}
		// The walk is over when the LIMIT found fewer rows than it allows AND
		// the byte cut kept them all.
		if candidates < batch && n == candidates {
			res.Done = true
			return setVersion(ctx, tx, versionSearchHistoryReady, search.DocVersion)
		}
		return nil
	})
	if err != nil {
		return SearchBackfill{}, err
	}
	return res, nil
}

// --- reads ---

// EventCount is how many matching events share a layer and category.
type EventCount struct {
	Layer    gridv1.Layer
	Category string
	Events   int
}

// Snippet is where a query matched in one event's text: runs of text, the
// matched words flagged.
type Snippet struct {
	// Field is "text" (summary, description, incident log) or "title"
	// (headline, location, identifiers), whichever the snippet was cut from.
	Field string
	Parts []SnippetPart
}

// SnippetPart is a run of snippet text; Hit marks a matched word.
type SnippetPart struct {
	Text string
	Hit  bool
}

// EventSearchPage is one page of QueryEventsPage plus what only a search or
// a count request produces.
type EventSearchPage struct {
	Page[*gridv1.Event]
	// Total is how many events match the filters and the text, over every
	// page. -1 when the query asked for neither text nor counts.
	Total int
	// Counts breaks Total down by layer and category.
	Counts []EventCount
	// Snippets is keyed by event id, for text queries.
	Snippets map[string]Snippet
}

// eventsSearchPageSQL is eventsPageSQL for a text query. The differences: the
// match joins event_search, and the order is newest first (observed_at DESC,
// id ASC) with no severity lead, because someone searching for a thing wants
// its latest report, not every EXTREME row first. Same two-phase shape: keys,
// then blobs for the page.
func eventsSearchPageSQL(q EventQuery, pageSize int) (string, []any, error) {
	var sb strings.Builder
	var args sqlArgs
	blobCol := "e.proto"
	if q.Lite {
		blobCol = "COALESCE(l.proto, e.proto)"
	}
	sb.WriteString(`SELECT ` + blobCol + `, k.severity, k.observed_at, k.id FROM (` +
		`SELECT e.id, e.severity, e.observed_at FROM events e`)
	sb.WriteString(eventsFilterSQL(q, &args))
	if q.PageToken != "" {
		var cur eventCursor
		if err := decodeToken(q.PageToken, &cur); err != nil {
			return "", nil, err
		}
		ov, iv := args.add(cur.O), args.add(cur.ID)
		sb.WriteString(` AND (e.observed_at < ` + ov + ` OR (e.observed_at = ` + ov + ` AND e.id > ` + iv + `))`)
	}
	sb.WriteString(` ORDER BY e.observed_at DESC, e.id ASC LIMIT ` + args.add(pageSize+1) + `) k` +
		` JOIN events e ON e.id = k.id`)
	if q.Lite {
		sb.WriteString(` LEFT JOIN event_lite l ON l.event_id = k.id`)
	}
	sb.WriteString(` ORDER BY k.observed_at DESC, k.id ASC`)
	return sb.String(), args, nil
}

// eventsFilterSQL is the joins and WHERE clause every events statement
// shares (after `FROM events e`), text match included when the query has one.
func eventsFilterSQL(q EventQuery, args *sqlArgs) string {
	statuses := q.Statuses
	if len(statuses) == 0 {
		statuses = []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED}
	}
	var sb strings.Builder
	if q.PlaceID != "" {
		sb.WriteString(` JOIN event_places ep ON ep.event_id = e.id AND ep.place_id = ` + args.add(q.PlaceID))
	}
	if q.Search != nil {
		sb.WriteString(` JOIN event_search s ON s.event_id = e.id AND s.tsv @@ ` + tsqueryExpr(*q.Search, args))
	}
	sb.WriteString(` WHERE e.status = ANY(` + args.add(enumInts(statuses)) + `)`)
	if len(q.Layers) > 0 {
		sb.WriteString(` AND e.layer = ANY(` + args.add(enumInts(q.Layers)) + `)`)
	}
	if q.MinSeverity > gridv1.Severity_INFO {
		sb.WriteString(` AND e.severity >= ` + args.add(int32(q.MinSeverity)))
	}
	if !q.Since.IsZero() {
		sb.WriteString(` AND e.observed_at >= ` + args.add(q.Since.Unix()))
	}
	return sb.String()
}

// checkSearchable fails a text query that cannot be answered: the index is
// not complete, or the words are all ones the language drops ("the", "of"),
// which would match nothing and read as "no results".
func (s *Store) checkSearchable(ctx context.Context, tx *sql.Tx, q search.Query, revisions bool) error {
	ev, rev, err := s.SearchReady(ctx)
	if err != nil {
		return err
	}
	if (!revisions && !ev) || (revisions && !rev) {
		return ErrSearchNotReady
	}
	var args sqlArgs
	var nodes int
	if err := tx.QueryRowContext(ctx, `SELECT numnode(`+tsqueryExpr(q, &args)+`)`, args...).Scan(&nodes); err != nil {
		return fmt.Errorf("store: parse search query: %w", err)
	}
	if nodes == 0 {
		return search.ErrQueryEmpty
	}
	return nil
}

// SearchEvents is QueryEventsPage with a text query, a count request, or
// both. With q.Search the page is newest first and carries a snippet per
// event; with q.Search or q.Counts it carries the total and its breakdown.
// Everything is read in one snapshot, so the total and the page agree.
func (s *Store) SearchEvents(ctx context.Context, q EventQuery) (EventSearchPage, error) {
	out := EventSearchPage{Total: -1}
	if q.Search == nil && !q.Counts {
		p, err := s.QueryEventsPage(ctx, q)
		out.Page = p
		return out, err
	}
	pageSize := clampPageSize(q.PageSize)
	err := s.searchTx(ctx, func(tx *sql.Tx) error {
		if q.Search != nil {
			if err := s.checkSearchable(ctx, tx, *q.Search, false); err != nil {
				return err
			}
		}
		// Counts by category read event_search even without a text query,
		// so they need the index complete too.
		if q.Search == nil {
			if ev, _, err := s.SearchReady(ctx); err != nil {
				return err
			} else if !ev {
				return ErrSearchNotReady
			}
		}
		var args sqlArgs
		countSQL := `SELECT e.layer, cs.category, COUNT(*) FROM events e` + eventsFilterSQL(q, &args)
		// The category lives on the search row; a text query already joined it.
		if q.Search != nil {
			countSQL = strings.Replace(countSQL, `SELECT e.layer, cs.category`, `SELECT e.layer, s.category`, 1) +
				` GROUP BY e.layer, s.category ORDER BY e.layer, s.category`
		} else {
			countSQL = strings.Replace(countSQL, ` WHERE `, ` JOIN event_search cs ON cs.event_id = e.id WHERE `, 1) +
				` GROUP BY e.layer, cs.category ORDER BY e.layer, cs.category`
		}
		rows, err := tx.QueryContext(ctx, countSQL, args...)
		if err != nil {
			return fmt.Errorf("store: count events: %w", err)
		}
		out.Total = 0
		for rows.Next() {
			var c EventCount
			var layer int32
			if err := rows.Scan(&layer, &c.Category, &c.Events); err != nil {
				rows.Close()
				return fmt.Errorf("store: count events: %w", err)
			}
			c.Layer = gridv1.Layer(layer)
			out.Counts = append(out.Counts, c)
			out.Total += c.Events
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: count events: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: count events: %w", err)
		}

		var query string
		var pargs []any
		if q.Search != nil {
			query, pargs, err = eventsSearchPageSQL(q, pageSize)
		} else {
			query, pargs, err = eventsPageSQL(q, pageSize)
		}
		if err != nil {
			return err
		}
		prow, err := tx.QueryContext(ctx, query, pargs...)
		if err != nil {
			return fmt.Errorf("store: query events: %w", err)
		}
		page, err := scanEventsPage(prow, pageSize, q.Lite)
		if err != nil {
			return err
		}
		out.Page = page
		if q.Search != nil && len(page.Items) > 0 {
			docs := make([]search.Doc, len(page.Items))
			for i, ev := range page.Items {
				docs[i] = search.Build(ev)
			}
			snips, err := snippets(ctx, tx, *q.Search, docs)
			if err != nil {
				return err
			}
			out.Snippets = make(map[string]Snippet, len(snips))
			for i, sn := range snips {
				out.Snippets[page.Items[i].GetId()] = sn
			}
		}
		return nil
	})
	if err != nil {
		return EventSearchPage{}, err
	}
	return out, nil
}

// Snippet markers: private-use code points, which no source text carries
// (they are stripped from the input first), so a marker can never be forged
// by the text being highlighted.
const (
	hitStart = ""
	hitStop  = ""
)

var stripMarkers = strings.NewReplacer(hitStart, "", hitStop, "")

// snippets cuts a short excerpt around the match in each document, in one
// round trip. The prose (summary, description, log) is preferred; when the
// match is only in the headline or location, that is returned instead; when
// it is in neither (an identifier, the source's name) there is no snippet.
func snippets(ctx context.Context, tx *sql.Tx, q search.Query, docs []search.Doc) ([]Snippet, error) {
	bodies, titles := make([]string, len(docs)), make([]string, len(docs))
	bodyRoads, titleRoads := make([][]string, len(docs)), make([][]string, len(docs))
	for i, d := range docs {
		bodies[i], bodyRoads[i] = search.MarkRoutes(pgText(stripMarkers.Replace(d.Prose)))
		titles[i], titleRoads[i] = search.MarkRoutes(pgText(stripMarkers.Replace(d.Title)))
	}
	var args sqlArgs
	tsq := tsqueryExprFor(q, &args, true)
	opts := `'StartSel=` + hitStart + `, StopSel=` + hitStop + `, MaxFragments=2, MaxWords=22, MinWords=8, FragmentDelimiter=" … "'`
	// A title is short: return all of it, highlighted, never a fragment.
	titleOpts := `'StartSel=` + hitStart + `, StopSel=` + hitStop + `, HighlightAll=true'`
	rows, err := tx.QueryContext(ctx, `
		SELECT ts_headline('english', u.body, t.q, `+opts+`), ts_headline('english', u.title, t.q, `+titleOpts+`)
		FROM unnest(`+args.add(bodies)+`::text[], `+args.add(titles)+`::text[]) WITH ORDINALITY AS u(body, title, ord),
		     (SELECT `+tsq+` AS q) t
		ORDER BY u.ord`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search snippets: %w", err)
	}
	defer rows.Close()
	out := make([]Snippet, 0, len(docs))
	for i := 0; rows.Next(); i++ {
		var body, title string
		if err := rows.Scan(&body, &title); err != nil {
			return nil, fmt.Errorf("store: search snippets: %w", err)
		}
		if i >= len(docs) {
			return nil, fmt.Errorf("store: search snippets: more rows than documents")
		}
		switch {
		case strings.Contains(body, hitStart):
			out = append(out, Snippet{Field: "text", Parts: snippetParts(search.RestoreRoutes(body, bodyRoads[i]))})
		case strings.Contains(title, hitStart):
			out = append(out, Snippet{Field: "title", Parts: snippetParts(search.RestoreRoutes(title, titleRoads[i]))})
		default:
			out = append(out, Snippet{})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search snippets: %w", err)
	}
	if len(out) != len(docs) {
		return nil, fmt.Errorf("store: search snippets: %d rows for %d documents", len(out), len(docs))
	}
	return out, nil
}

var spaceRuns = regexp.MustCompile(`\s+`)

// snippetParts splits marked-up headline text into runs.
func snippetParts(s string) []SnippetPart {
	var out []SnippetPart
	// Runs are meant to be concatenated as they are, so spacing is kept
	// (line breaks become one space) and nothing is trimmed.
	add := func(text string, hit bool) {
		text = spaceRuns.ReplaceAllString(text, " ")
		if text == "" {
			return
		}
		if n := len(out); n > 0 && out[n-1].Hit == hit {
			out[n-1].Text += text
			return
		}
		out = append(out, SnippetPart{Text: text, Hit: hit})
	}
	for s != "" {
		i := strings.Index(s, hitStart)
		if i < 0 {
			add(s, false)
			break
		}
		add(s[:i], false)
		s = s[i+len(hitStart):]
		j := strings.Index(s, hitStop)
		if j < 0 {
			add(s, true)
			break
		}
		add(s[:j], true)
		s = s[j+len(hitStop):]
	}
	return out
}

// --- history ---

// RevisionSearchPage is one page of revisions matching a text query.
type RevisionSearchPage struct {
	Page[*gridv1.EventRevision]
	// Total is how many revisions match, over every page.
	Total int
	// Snippets is parallel to Items.
	Snippets []Snippet
}

// historyFilterSQL is the joins and WHERE every history search statement
// shares, after `FROM event_revision_search s JOIN events e ON e.id =
// s.event_id`. Layer and place read the event's CURRENT row, as QueryHistory
// does; the window is over the revision's observed_at.
func historyFilterSQL(q HistoryQuery, args *sqlArgs) string {
	var sb strings.Builder
	if q.PlaceID != "" {
		sb.WriteString(` JOIN event_places ep ON ep.event_id = s.event_id AND ep.place_id = ` + args.add(q.PlaceID))
	}
	sb.WriteString(` WHERE s.tsv @@ ` + tsqueryExpr(*q.Search, args))
	if len(q.Layers) > 0 {
		sb.WriteString(` AND e.layer = ANY(` + args.add(enumInts(q.Layers)) + `)`)
	}
	if !q.From.IsZero() {
		sb.WriteString(` AND s.observed_at >= ` + args.add(q.From.Unix()))
	}
	if !q.To.IsZero() {
		sb.WriteString(` AND s.observed_at < ` + args.add(q.To.Unix()))
	}
	return sb.String()
}

const historySearchFrom = ` FROM event_revision_search s JOIN events e ON e.id = s.event_id`

// SearchHistory is QueryHistoryPage restricted to revisions whose text
// matches q.Search: same order (observed_at DESC, event_id ASC, revision
// DESC), same cursor, plus the total and a snippet per revision.
func (s *Store) SearchHistory(ctx context.Context, q HistoryQuery) (RevisionSearchPage, error) {
	if q.Search == nil {
		return RevisionSearchPage{}, fmt.Errorf("store: SearchHistory needs a text query")
	}
	pageSize := clampPageSize(q.PageSize)
	var out RevisionSearchPage
	err := s.searchTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkSearchable(ctx, tx, *q.Search, true); err != nil {
			return err
		}
		var cargs sqlArgs
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)`+historySearchFrom+historyFilterSQL(q, &cargs), cargs...).
			Scan(&out.Total); err != nil {
			return fmt.Errorf("store: count history matches: %w", err)
		}

		var args sqlArgs
		var sb strings.Builder
		// Keys first, blobs for the page only (revisions carry full shapes).
		sb.WriteString(`SELECT r.revision, r.observed_at, r.ingested_at, r.proto, r.event_id FROM (` +
			`SELECT s.event_id, s.revision, s.observed_at` + historySearchFrom + historyFilterSQL(q, &args))
		if q.PageToken != "" {
			var cur historyCursor
			if err := decodeToken(q.PageToken, &cur); err != nil {
				return err
			}
			ov, iv, rv := args.add(cur.O), args.add(cur.ID), args.add(cur.R)
			sb.WriteString(` AND (s.observed_at < ` + ov + ` OR (s.observed_at = ` + ov + ` AND s.event_id > ` + iv + `)
				OR (s.observed_at = ` + ov + ` AND s.event_id = ` + iv + ` AND s.revision < ` + rv + `))`)
		}
		sb.WriteString(` ORDER BY s.observed_at DESC, s.event_id ASC, s.revision DESC LIMIT ` + args.add(pageSize+1) + `) k` +
			` JOIN event_revisions r ON r.event_id = k.event_id AND r.revision = k.revision` +
			` ORDER BY k.observed_at DESC, k.event_id ASC, k.revision DESC`)
		rows, err := tx.QueryContext(ctx, sb.String(), args...)
		if err != nil {
			return fmt.Errorf("store: search history: %w", err)
		}
		var docs []search.Doc
		for rows.Next() {
			if len(out.Items) == pageSize {
				out.More = true
				break
			}
			var eventID string
			rev, err := scanRevision(func(dest ...any) error { return rows.Scan(append(dest, &eventID)...) })
			if err != nil {
				rows.Close()
				return err
			}
			out.Items = append(out.Items, rev)
			// The cursor is the search row's observed_at, which is the
			// revision's index column, the value the order used.
			out.After = append(out.After, encodeToken(historyCursor{
				O: rev.GetObservedAt().AsTime().Unix(), ID: eventID, R: rev.GetRevision(),
			}))
			docs = append(docs, search.Build(rev.GetEvent()))
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: search history: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: search history: %w", err)
		}
		if len(docs) > 0 {
			if out.Snippets, err = snippets(ctx, tx, *q.Search, docs); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return RevisionSearchPage{}, err
	}
	return out, nil
}

// HistoryMatch is one event's matching revisions, collapsed: when its text
// first matched, when it last did, and how many stored revisions do.
type HistoryMatch struct {
	EventID string
	// First is the earliest matching revision, in full.
	First   *gridv1.EventRevision
	Snippet Snippet
	// LastRevision and LastObservedAt identify the latest matching revision
	// (the same as First when only one matches).
	LastRevision   uint32
	LastObservedAt int64
	// Matching is the number of matching revisions.
	Matching int
}

// HistoryMatchPage is one page of SearchHistoryByEvent.
type HistoryMatchPage struct {
	Page[HistoryMatch]
	// Total is how many events have a matching revision.
	Total int
}

// historyMatchCursor is the keyset for SearchHistoryByEvent's order (latest
// matching observed_at DESC, event_id ASC).
type historyMatchCursor struct {
	O  int64  `json:"lo"`
	ID string `json:"id"`
}

// SearchHistoryByEvent answers "when did this event's text say X": the
// revisions matching q.Search, grouped by event, ordered by each event's
// latest match (newest first, then event id). A fire whose forty updates all
// name it is one row, not forty. The window (From, To) limits which revisions
// count, so First is the earliest match inside the window.
func (s *Store) SearchHistoryByEvent(ctx context.Context, q HistoryQuery) (HistoryMatchPage, error) {
	if q.Search == nil {
		return HistoryMatchPage{}, fmt.Errorf("store: SearchHistoryByEvent needs a text query")
	}
	pageSize := clampPageSize(q.PageSize)
	var out HistoryMatchPage
	err := s.searchTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkSearchable(ctx, tx, *q.Search, true); err != nil {
			return err
		}
		var cargs sqlArgs
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT s.event_id)`+historySearchFrom+historyFilterSQL(q, &cargs), cargs...).
			Scan(&out.Total); err != nil {
			return fmt.Errorf("store: count history matches: %w", err)
		}

		var args sqlArgs
		var sb strings.Builder
		sb.WriteString(`SELECT r.revision, r.observed_at, r.ingested_at, r.proto, g.event_id, g.last_rev, g.last_at, g.n FROM (` +
			`SELECT s.event_id, MIN(s.revision) AS first_rev, MAX(s.revision) AS last_rev, MAX(s.observed_at) AS last_at, COUNT(*) AS n` +
			historySearchFrom + historyFilterSQL(q, &args) + ` GROUP BY s.event_id`)
		if q.PageToken != "" {
			var cur historyMatchCursor
			if err := decodeToken(q.PageToken, &cur); err != nil {
				return err
			}
			ov, iv := args.add(cur.O), args.add(cur.ID)
			sb.WriteString(` HAVING MAX(s.observed_at) < ` + ov + ` OR (MAX(s.observed_at) = ` + ov + ` AND s.event_id > ` + iv + `)`)
		}
		sb.WriteString(` ORDER BY last_at DESC, s.event_id ASC LIMIT ` + args.add(pageSize+1) + `) g` +
			` JOIN event_revisions r ON r.event_id = g.event_id AND r.revision = g.first_rev` +
			` ORDER BY g.last_at DESC, g.event_id ASC`)
		rows, err := tx.QueryContext(ctx, sb.String(), args...)
		if err != nil {
			return fmt.Errorf("store: search history by event: %w", err)
		}
		var docs []search.Doc
		for rows.Next() {
			if len(out.Items) == pageSize {
				out.More = true
				break
			}
			var m HistoryMatch
			rev, err := scanRevision(func(dest ...any) error {
				return rows.Scan(append(dest, &m.EventID, &m.LastRevision, &m.LastObservedAt, &m.Matching)...)
			})
			if err != nil {
				rows.Close()
				return err
			}
			m.First = rev
			out.Items = append(out.Items, m)
			out.After = append(out.After, encodeToken(historyMatchCursor{O: m.LastObservedAt, ID: m.EventID}))
			docs = append(docs, search.Build(rev.GetEvent()))
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: search history by event: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: search history by event: %w", err)
		}
		if len(docs) > 0 {
			snips, err := snippets(ctx, tx, *q.Search, docs)
			if err != nil {
				return err
			}
			for i := range out.Items {
				out.Items[i].Snippet = snips[i]
			}
		}
		return nil
	})
	if err != nil {
		return HistoryMatchPage{}, err
	}
	return out, nil
}

// --- ranked search (the finder) ---

// RankedEvent is one hit of SearchTopEvents.
type RankedEvent struct {
	Event   *gridv1.Event // the geometry-free copy
	Snippet Snippet
}

// rankWeights are ts_rank_cd's weights for D, C, B, A: a match in the
// headline, location or an identifier counts ten times one in the source's
// name and five times one in the verbatim description.
const rankWeights = `'{0.1, 0.2, 0.4, 1.0}'`

// SearchTopEvents returns the best few events for a query, best first, and
// how many match in all. It is for finding a thing, not for listing: the
// order is relevance (where the words matched and how close together), raised
// for events that are active or scheduled and for recent ones, and it does
// not page. placeID keeps only events attached to that place ("" for any);
// layers and exclude filter by layer (exclude wins). Every status is searched.
//
// The recency factor runs from 3 for something observed now towards 1 for
// something old (2 at one week, 1.5 at three): enough to put this week's
// crash on Highway 4 above August's, which a long incident log would
// otherwise outrank by repeating the road, and not enough to bury an exact
// name match under a recent passing mention (a headline match weighs five
// times a description match). Tried against the stored events on 2026-11-15;
// length normalisation was tried too and ranked worse.
func (s *Store) SearchTopEvents(ctx context.Context, q search.Query, placeID string, layers, exclude []gridv1.Layer, limit int) ([]RankedEvent, int, error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("store: SearchTopEvents needs a positive limit")
	}
	var out []RankedEvent
	var total int
	err := s.searchTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkSearchable(ctx, tx, q, false); err != nil {
			return err
		}
		filter := func(args *sqlArgs) string {
			f := ` FROM events e JOIN event_search s ON s.event_id = e.id AND s.tsv @@ ` + tsqueryExpr(q, args)
			if placeID != "" {
				f += ` JOIN event_places ep ON ep.event_id = e.id AND ep.place_id = ` + args.add(placeID)
			}
			f += ` WHERE true`
			if len(layers) > 0 {
				f += ` AND e.layer = ANY(` + args.add(enumInts(layers)) + `)`
			}
			if len(exclude) > 0 {
				f += ` AND e.layer <> ALL(` + args.add(enumInts(exclude)) + `)`
			}
			return f
		}
		var cargs sqlArgs
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)`+filter(&cargs), cargs...).Scan(&total); err != nil {
			return fmt.Errorf("store: count ranked matches: %w", err)
		}
		if total == 0 {
			return nil
		}
		var args sqlArgs
		tsq := tsqueryExpr(q, &args)
		now := args.add(time.Now().Unix())
		live := args.add(enumInts([]gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED}))
		stmt := `SELECT COALESCE(l.proto, e.proto) FROM (
			SELECT e.id, e.observed_at,
			       ts_rank_cd(` + rankWeights + `, s.tsv, ` + tsq + `)
			       * CASE WHEN e.status = ANY(` + live + `) THEN 1.5 ELSE 1 END
			       * (1 + 2 / (1 + GREATEST(` + now + ` - e.observed_at, 0) / 604800.0)) AS score` +
			filter(&args) + `
			ORDER BY score DESC, e.observed_at DESC, e.id ASC LIMIT ` + args.add(limit) + `) k
			JOIN events e ON e.id = k.id LEFT JOIN event_lite l ON l.event_id = k.id
			ORDER BY k.score DESC, k.observed_at DESC, k.id ASC`
		rows, err := tx.QueryContext(ctx, stmt, args...)
		if err != nil {
			return fmt.Errorf("store: ranked search: %w", err)
		}
		var docs []search.Doc
		for rows.Next() {
			var blob []byte
			if err := rows.Scan(&blob); err != nil {
				rows.Close()
				return fmt.Errorf("store: ranked search: %w", err)
			}
			ev := &gridv1.Event{}
			if err := proto.Unmarshal(blob, ev); err != nil {
				rows.Close()
				return fmt.Errorf("store: unmarshal ranked event: %w", err)
			}
			stripShape(ev)
			out = append(out, RankedEvent{Event: ev})
			docs = append(docs, search.Build(ev))
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: ranked search: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: ranked search: %w", err)
		}
		if len(docs) > 0 {
			snips, err := snippets(ctx, tx, q, docs)
			if err != nil {
				return err
			}
			for i := range out {
				out[i].Snippet = snips[i]
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}
