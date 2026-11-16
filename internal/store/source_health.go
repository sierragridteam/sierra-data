package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	id, name    string
	kind        gridv1.SourceKind
	status      gridv1.SourceStatus
	err         string
	lastSuccess time.Time // zero: never
	at          time.Time // the attempt that found this status
}

// sourceStatusChanged writes the source's health event when its status moved
// away from old. A no-op unless source health is on and the row is a feed.
func (s *Store) sourceStatusChanged(ctx context.Context, tx *sql.Tx, st sourceHealthState, old gridv1.SourceStatus) error {
	if !s.sourceHealth || st.kind != gridv1.SourceKind_FEED || st.status == old {
		return nil
	}
	return s.putSourceHealth(ctx, tx, st)
}

// putSourceHealth upserts the source's health event at st.
func (s *Store) putSourceHealth(ctx context.Context, tx *sql.Tx, st sourceHealthState) error {
	_, err := s.upsertEventTx(ctx, tx, sourceHealthEvent(st))
	return err
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

// SeedSourceHealth brings the health events in line with the registry, at the
// start of a writer term. A feed that has been attempted and has no event
// gets one at its current status; one whose event disagrees with its row
// (statuses compared, never hashes) gets a revision, dated now, because when
// it changed is not known. That covers the first deploy and any stretch
// written by a build without source health. A health event whose source is
// no longer a feed in the registry is RESOLVED. It also records a success
// for the `grid` row, which nothing polls. A no-op when source health is off.
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

		rows, err := tx.QueryContext(ctx, `
			SELECT s.id, s.name, s.status, s.last_error, s.last_success_at, e.proto
			FROM sources s LEFT JOIN events e ON e.id = $1 || s.id
			WHERE s.kind = $2 ORDER BY s.id`, SourceHealthPrefix, int32(gridv1.SourceKind_FEED))
		if err != nil {
			return fmt.Errorf("store: source health seed: %w", err)
		}
		var todo []sourceHealthState
		feeds := map[string]bool{}
		for rows.Next() {
			st := sourceHealthState{kind: gridv1.SourceKind_FEED, at: now}
			var status int32
			var lastSuccess sql.NullInt64
			var blob []byte
			if err := rows.Scan(&st.id, &st.name, &status, &st.err, &lastSuccess, &blob); err != nil {
				rows.Close()
				return fmt.Errorf("store: source health seed: %w", err)
			}
			feeds[st.id] = true
			st.status = gridv1.SourceStatus(status)
			if lastSuccess.Valid {
				st.lastSuccess = time.Unix(lastSuccess.Int64, 0)
			}
			if st.status == gridv1.SourceStatus_SOURCE_STATUS_UNSPECIFIED {
				continue // never attempted: its first attempt writes revision 1
			}
			if blob != nil {
				ev := &gridv1.Event{}
				if err := proto.Unmarshal(blob, ev); err != nil {
					rows.Close()
					return fmt.Errorf("store: source health seed: unmarshal %s: %w", st.id, err)
				}
				if ev.GetSourceHealth().GetStatus() == st.status && ev.GetStatus() == gridv1.EventStatus_ACTIVE {
					continue
				}
			}
			todo = append(todo, st)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: source health seed: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: source health seed: %w", err)
		}
		for _, st := range todo {
			if err := s.putSourceHealth(ctx, tx, st); err != nil {
				return err
			}
		}

		// Health events of sources that left the registry (or stopped being
		// feeds): the history stays, the event ends.
		gone, err := tx.QueryContext(ctx, `SELECT id FROM events WHERE layer = $1 AND status = $2 ORDER BY id`,
			int32(gridv1.Layer_SOURCE_HEALTH), int32(gridv1.EventStatus_ACTIVE))
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
			if !feeds[strings.TrimPrefix(id, SourceHealthPrefix)] {
				resolve = append(resolve, id)
			}
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
