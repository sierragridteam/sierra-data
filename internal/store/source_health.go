package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Source health as events (layer SOURCE_HEALTH).
//
// One long-lived event per FEED row of `sources`, id `source:<id>`,
// permanently ACTIVE, always INFO, in no place. A revision is a change of the
// source's status and nothing else, so nothing about the event is excluded
// from the content hash and it never takes the hash-equal refresh path. The
// `sources` row stays the registry and the current health (latest error,
// latest attempt); the event is its history.
//
// Reporter rows (push monitors) get no event: mesh reporter health is in
// process memory, so every deploy would record an outage that did not happen.

const (
	// GridSourceID is the registry row that owns the events the Grid derives
	// from its own state. events.source_id is a foreign key to sources, and
	// giving these events to the source they describe would hand them to that
	// source's poller (its sweep would resolve them).
	GridSourceID = "grid"
	// SourceHealthPrefix + a source id is that source's health event id.
	SourceHealthPrefix = "source:"
)

// GridSourceSeed is the registry row for GridSourceID.
func GridSourceSeed() SourceSeed {
	return SourceSeed{
		ID: GridSourceID, Name: "The Grid", Attribution: "S.I.E.R.R.A Grid",
		Kind: gridv1.SourceKind_INTERNAL,
		// Not polled. The row records a success at the start of each writer
		// term (SeedSourceHealth); a day is far longer than any deploy gap.
		PollInterval: 24 * time.Hour,
	}
}

// sourceHealthState is a source's health at one attempt.
type sourceHealthState struct {
	id, name     string
	kind         gridv1.SourceKind
	status       gridv1.SourceStatus
	err          string
	lastSuccess  time.Time // zero: never
	at           time.Time // the attempt that found this status
	pollInterval int64     // seconds; 0 when not known (the seed)
}

// recorded is the status the history takes for st, which is not always the
// row's. One failed poll makes a source STALE and the next good one makes it
// OK again; written as they happen that is two full revisions for a blip,
// and a feed polled every minute that fails every other poll would write
// more revisions in a day than the rest of the store. STALE is therefore
// recorded only once it has lasted: the last success is at least two poll
// intervals old, which a single missed poll never is. UNAVAILABLE (past the
// stale window, or never read) is recorded at once. The row itself is not
// debounced: /api/v1/sources shows every failed poll.
func (st sourceHealthState) recorded() gridv1.SourceStatus {
	if st.status == gridv1.SourceStatus_STALE && st.pollInterval > 0 && !st.lastSuccess.IsZero() &&
		st.at.Sub(st.lastSuccess) < 2*time.Duration(st.pollInterval)*time.Second {
		return gridv1.SourceStatus_OK
	}
	return st.status
}

// recordSourceHealth brings a feed's health event in line with st, inside
// the caller's transaction but under a savepoint, and never fails it: the
// caller is recording the attempt itself, which must commit whatever happens
// here. It compares against the stored event (not the row's previous status),
// so an event that is missing or was left behind by an earlier failure is
// put right at the next attempt. A no-op unless source health is on and the
// row is a feed.
func (s *Store) recordSourceHealth(ctx context.Context, tx *sql.Tx, st sourceHealthState) {
	if !s.sourceHealth || st.kind != gridv1.SourceKind_FEED {
		return
	}
	want := st.recorded()
	if want == gridv1.SourceStatus_SOURCE_STATUS_UNSPECIFIED {
		return
	}
	fail := func(err error) {
		if _, rerr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT source_health`); rerr != nil {
			log.Printf("store: source health %s: rollback to savepoint: %v", st.id, rerr)
		}
		log.Printf("store: source health %s: not recorded this attempt (the attempt itself is): %v", st.id, err)
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT source_health`); err != nil {
		log.Printf("store: source health %s: savepoint: %v", st.id, err)
		return
	}
	var blob []byte
	err := tx.QueryRowContext(ctx, `SELECT proto FROM events WHERE id = $1`, SourceHealthPrefix+st.id).Scan(&blob)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		fail(err)
		return
	}
	if blob != nil {
		ev := &gridv1.Event{}
		// A blob that will not parse is rewritten, not an obstacle.
		if proto.Unmarshal(blob, ev) == nil && ev.GetStatus() == gridv1.EventStatus_ACTIVE && ev.GetSourceHealth().GetStatus() == want {
			_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT source_health`)
			return
		}
	}
	rec := st
	rec.status = want
	if want == gridv1.SourceStatus_OK {
		rec.err = ""
	}
	if _, err := s.upsertEventTx(ctx, tx, sourceHealthEvent(rec)); err != nil {
		fail(err)
		return
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT source_health`); err != nil {
		log.Printf("store: source health %s: release savepoint: %v", st.id, err)
	}
}

func sourceHealthEvent(st sourceHealthState) *gridv1.Event {
	d := &gridv1.SourceHealthDetail{SourceId: st.id, Status: st.status, Since: timestamppb.New(st.at.Truncate(time.Second)), Error: st.err}
	if !st.lastSuccess.IsZero() {
		d.LastSuccessAt = timestamppb.New(st.lastSuccess.Truncate(time.Second))
	}
	name := st.name
	if name == "" {
		name = st.id
	}
	word := map[gridv1.SourceStatus]string{
		gridv1.SourceStatus_OK:          "OK",
		gridv1.SourceStatus_STALE:       "stale",
		gridv1.SourceStatus_UNAVAILABLE: "unavailable",
	}[st.status]
	if word == "" {
		word = "not read yet"
	}
	return &gridv1.Event{
		Id:          SourceHealthPrefix + st.id,
		Layer:       gridv1.Layer_SOURCE_HEALTH,
		Category:    strings.ToLower(strings.ReplaceAll(word, " ", "-")),
		Severity:    gridv1.Severity_INFO,
		Status:      gridv1.EventStatus_ACTIVE,
		Headline:    name + ": " + word,
		Description: st.err,
		AreaLabel:   name,
		Provenance:  &gridv1.Provenance{SourceId: GridSourceID, SourceName: "The Grid", FetchedAt: timestamppb.New(st.at.Truncate(time.Second))},
		Effective:   d.Since,
		ObservedAt:  d.Since,
		Detail:      &gridv1.Event_SourceHealth{SourceHealth: d},
	}
}

// SeedSourceHealth runs at the start of a writer term. Each attempt already
// reconciles its own source's event (recordSourceHealth), so what is left for
// the term start is what no attempt will reach: a health event whose source
// is no longer a feed in the registry (removed, retired, or now a reporter)
// is RESOLVED, and the `grid` row, which nothing polls, records a success.
// A no-op when source health is off.
//
// It deliberately does not create or revise events from the rows: a row's
// status at term start may be hours old (the process was down), and the next
// attempt of each feed, seconds away, writes the event from a real reading.
func (s *Store) SeedSourceHealth(ctx context.Context) error {
	if !s.sourceHealth {
		return nil
	}
	now := time.Now()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sources SET last_attempt_at = $1, last_success_at = $1, last_error = '', status = $2
			WHERE id = $3`, now.Unix(), int32(gridv1.SourceStatus_OK), GridSourceID)
		if err != nil {
			return fmt.Errorf("store: source health: grid row: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("store: source health is on but the `grid` source row is not seeded (GridSourceSeed)")
		}
		gone, err := tx.QueryContext(ctx, `
			SELECT e.id FROM events e
			WHERE e.layer = $1 AND e.status = $2 AND NOT EXISTS (
			  SELECT 1 FROM sources s WHERE $3 || s.id = e.id AND s.kind = $4 AND s.retired_at IS NULL)
			ORDER BY e.id`,
			int32(gridv1.Layer_SOURCE_HEALTH), int32(gridv1.EventStatus_ACTIVE), SourceHealthPrefix, int32(gridv1.SourceKind_FEED))
		if err != nil {
			return fmt.Errorf("store: source health seed: %w", err)
		}
		var resolve []string
		for gone.Next() {
			var id string
			if err := gone.Scan(&id); err != nil {
				gone.Close()
				return fmt.Errorf("store: source health seed: %w", err)
			}
			resolve = append(resolve, id)
		}
		if err := gone.Close(); err != nil {
			return fmt.Errorf("store: source health seed: %w", err)
		}
		if len(resolve) > 0 {
			return transitionChunkTx(ctx, tx, resolve, gridv1.EventStatus_RESOLVED, now, nil)
		}
		return nil
	})
}
