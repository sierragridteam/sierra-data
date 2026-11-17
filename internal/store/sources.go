package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// Disappearance policies (implementation plan decision 8): what the
// lifecycle sweep does when an event vanishes from its source's feed.
const (
	DisappearanceResolve = "resolve" // authoritative active-only feed => RESOLVED
	DisappearanceExpire  = "expire"  // missing AND past expires/grace => EXPIRED
)

// SourceSeed carries the config-owned fields of a source registry row.
// Runtime fields (last_attempt, last_success, last_error, status) are owned
// by RecordAttempt and never touched by seeding.
type SourceSeed struct {
	ID            string
	Name          string
	Attribution   string
	HomepageURL   string            // upstream's human-facing page, surfaced as Source.homepage_url
	Kind          gridv1.SourceKind // FEED or REPORTER; unspecified seeds as FEED
	PollInterval  time.Duration
	StaleAfter    time.Duration // 0 => default 3x PollInterval
	ExpireAfter   time.Duration // 0 => never auto-expire
	Disappearance string        // DisappearanceResolve (default) | DisappearanceExpire
}

// SeedSources inserts or updates the config-owned fields of each source,
// preserving runtime health fields across restarts and config changes.
func (s *Store) SeedSources(ctx context.Context, seeds []SourceSeed) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, seed := range seeds {
			if seed.ID == "" {
				return fmt.Errorf("store: source seed with empty id")
			}
			disappearance := seed.Disappearance
			if disappearance == "" {
				disappearance = DisappearanceResolve
			}
			kind := seed.Kind
			if kind == gridv1.SourceKind_SOURCE_KIND_UNSPECIFIED {
				kind = gridv1.SourceKind_FEED
			}
			var staleAfter, expireAfter any
			if seed.StaleAfter > 0 {
				staleAfter = int64(seed.StaleAfter.Seconds())
			}
			if seed.ExpireAfter > 0 {
				expireAfter = int64(seed.ExpireAfter.Seconds())
			}
			if _, err := tx.Exec(`
				INSERT INTO sources (id, name, attribution, homepage_url, kind, poll_interval_seconds,
				                     stale_after_seconds, expire_after_seconds, disappearance)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				ON CONFLICT (id) DO UPDATE SET
				  name = excluded.name, attribution = excluded.attribution,
				  homepage_url = excluded.homepage_url, kind = excluded.kind,
				  poll_interval_seconds = excluded.poll_interval_seconds,
				  stale_after_seconds = excluded.stale_after_seconds,
				  expire_after_seconds = excluded.expire_after_seconds,
				  disappearance = excluded.disappearance, retired_at = NULL`,
				seed.ID, seed.Name, seed.Attribution, seed.HomepageURL, int32(kind),
				int64(seed.PollInterval.Seconds()),
				staleAfter, expireAfter, disappearance,
			); err != nil {
				return fmt.Errorf("store: seed source %s: %w", seed.ID, err)
			}
		}
		return nil
	})
}

// DeleteSource removes a source registry row (health + config). Used to retire a
// source no poller declares anymore, so /api/v1/sources doesn't list a defunct
// entry forever (SeedSources is upsert-only and never removes). Idempotent —
// deleting an absent id is a no-op. Events keep their own source_id in the proto
// blob and are unaffected (retire them separately via TransitionEvents).
func (s *Store) DeleteSource(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM sources WHERE id = $1`, id); err != nil {
			return fmt.Errorf("store: delete source %s: %w", id, err)
		}
		return nil
	})
}

// RetireSource takes a source the running configuration no longer uses out
// of service: its ACTIVE and SCHEDULED events become EXPIRED (a revision each;
// nobody is watching them any more, which is not the same as "ended"), and
// its registry row is marked retired, so /api/v1/sources stops listing a row
// that would otherwise keep its last status for ever. The row stays, because
// its events name it. Idempotent, and a no-op for an id that was never
// seeded. Seeding the source again (SeedSources) brings the row back, and the
// next poll that lists an event makes it ACTIVE again.
//
// This is what turning a layer's flag off means. Without it the layer's
// events stayed ACTIVE with no poller to end them.
func (s *Store) RetireSource(ctx context.Context, id string) (expired int, err error) {
	now := time.Now()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE sources SET retired_at = $1 WHERE id = $2 AND retired_at IS NULL`, now.Unix(), id); err != nil {
			return fmt.Errorf("store: retire source %s: %w", id, err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM events WHERE source_id = $1 AND status = ANY($2) ORDER BY id`,
			id, enumInts([]gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED}))
		if err != nil {
			return fmt.Errorf("store: retire source %s: %w", id, err)
		}
		var ids []string
		for rows.Next() {
			var eid string
			if err := rows.Scan(&eid); err != nil {
				rows.Close()
				return fmt.Errorf("store: retire source %s: %w", id, err)
			}
			ids = append(ids, eid)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: retire source %s: %w", id, err)
		}
		expired = len(ids)
		for start := 0; start < len(ids); start += transitionChunk {
			if err := transitionChunkTx(ctx, tx, ids[start:min(start+transitionChunk, len(ids))], gridv1.EventStatus_EXPIRED, now, nil); err != nil {
				return err
			}
		}
		return nil
	})
	return expired, err
}

// RecordAttempt records the outcome of one poll of a source. last_attempt is
// always stamped. On success: last_success=now, last_error cleared,
// status=OK. On failure: last_error set, and status degrades to STALE while
// the last success is within stale_after (default 3x poll interval), then
// UNAVAILABLE — never-succeeded sources go straight to UNAVAILABLE.
//
// With WithSourceHealth, a feed whose recorded health differs from its
// SOURCE_HEALTH event gets a revision (source_health.go). The row always
// commits: the event is written under a savepoint, and a failure there is
// logged and left for the next attempt, which compares again. Current health
// is what every fail-loud reader depends on; its history must never be able
// to stop it being recorded.
func (s *Store) RecordAttempt(ctx context.Context, id string, attemptErr error) error {
	now := time.Now()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var pollInterval int64
		var staleAfter, lastSuccess sql.NullInt64
		var name string
		var kind int32
		err := tx.QueryRow(
			`SELECT poll_interval_seconds, stale_after_seconds, last_success_at, name, kind FROM sources WHERE id = $1`, id,
		).Scan(&pollInterval, &staleAfter, &lastSuccess, &name, &kind)
		if err == sql.ErrNoRows {
			return fmt.Errorf("store: record attempt for unseeded source %q", id)
		}
		if err != nil {
			return fmt.Errorf("store: record attempt %s: %w", id, err)
		}

		if attemptErr == nil {
			_, err := tx.Exec(`
				UPDATE sources SET last_attempt_at = $1, last_success_at = $1, last_error = '', status = $2
				WHERE id = $3`,
				now.Unix(), int32(gridv1.SourceStatus_OK), id)
			if err != nil {
				return fmt.Errorf("store: record success %s: %w", id, err)
			}
			s.recordSourceHealth(ctx, tx, sourceHealthState{
				id: id, name: name, kind: gridv1.SourceKind(kind), status: gridv1.SourceStatus_OK,
				lastSuccess: now, at: now, pollInterval: pollInterval,
			})
			return nil
		}

		staleWindow := staleAfter.Int64
		if !staleAfter.Valid {
			staleWindow = 3 * pollInterval
		}
		status := gridv1.SourceStatus_UNAVAILABLE
		if lastSuccess.Valid && now.Unix()-lastSuccess.Int64 <= staleWindow {
			status = gridv1.SourceStatus_STALE
		}
		if _, err := tx.Exec(`
			UPDATE sources SET last_attempt_at = $1, last_error = $2, status = $3 WHERE id = $4`,
			now.Unix(), attemptErr.Error(), int32(status), id); err != nil {
			return fmt.Errorf("store: record failure %s: %w", id, err)
		}
		st := sourceHealthState{id: id, name: name, kind: gridv1.SourceKind(kind), status: status, err: attemptErr.Error(), at: now, pollInterval: pollInterval}
		if lastSuccess.Valid {
			st.lastSuccess = time.Unix(lastSuccess.Int64, 0)
		}
		s.recordSourceHealth(ctx, tx, st)
		return nil
	})
}

// ListSources returns the registry with health, ordered by id.
// stale_after_seconds is surfaced as the effective value (3x poll interval
// when unset); expire_after_seconds stays 0 for "never auto-expire".
func (s *Store) ListSources(ctx context.Context) ([]*gridv1.Source, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, attribution, homepage_url, kind, poll_interval_seconds, stale_after_seconds,
		       expire_after_seconds, last_success_at, last_attempt_at, last_error, status
		FROM sources WHERE retired_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list sources: %w", err)
	}
	defer rows.Close()

	var out []*gridv1.Source
	for rows.Next() {
		var src gridv1.Source
		var pollInterval int64
		var staleAfter, expireAfter, lastSuccess, lastAttempt sql.NullInt64
		var status, kind int32
		if err := rows.Scan(&src.Id, &src.Name, &src.Attribution, &src.HomepageUrl, &kind, &pollInterval,
			&staleAfter, &expireAfter, &lastSuccess, &lastAttempt, &src.LastError, &status); err != nil {
			return nil, fmt.Errorf("store: scan source: %w", err)
		}
		src.Kind = gridv1.SourceKind(kind)
		src.PollIntervalSeconds = uint32(pollInterval)
		if staleAfter.Valid {
			src.StaleAfterSeconds = uint32(staleAfter.Int64)
		} else {
			src.StaleAfterSeconds = uint32(3 * pollInterval)
		}
		if expireAfter.Valid {
			src.ExpireAfterSeconds = uint32(expireAfter.Int64)
		}
		src.LastSuccessAt = tsFromNull(lastSuccess)
		src.LastAttemptAt = tsFromNull(lastAttempt)
		src.Status = gridv1.SourceStatus(status)
		out = append(out, &src)
	}
	return out, rows.Err()
}
