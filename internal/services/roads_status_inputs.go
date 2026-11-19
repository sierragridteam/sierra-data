package services

import (
	"context"
	"fmt"
	"time"

	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
)

// Status inputs: the two reads behind a road's status that used to fail
// silently. A failed roads.dot.ca.gov or chp-only.kml fetch read as "nothing
// reported", which is OPEN. On 2026-11-18 Hwy 4 Arnold to Bear Valley
// alternated CLOSED and OPEN for five hours, and each OPEN, one to four
// refreshes long, had lost the Ebbetts Pass winter closure too, which
// Caltrans listed throughout: the conditions read had returned nothing.
//
// Both now follow segmentLaneClosures: a failed read serves the last good
// one while it is servable-stale (< 2x roads.refreshInterval); past that it
// is an error that fails refreshRoadData, so the periodic refresher keeps
// the last good roads:all instead of publishing a guessed OPEN. Either way
// the failure is remembered (RoadInputHealth) and the road-segment poller
// reports it on the roads source.

const (
	// roadConditionsKeyPrefix + a highway number ("road_conditions:4") caches
	// the last good roads.dot.ca.gov read of that highway. A read with no
	// conditions is a good read and is cached as one.
	roadConditionsKeyPrefix = "road_conditions:"
	// chpIncidentsKey caches the last good chp-only.kml read, the raw
	// statewide set. Both CHP readers write it after a good read (this
	// refresh every roads.refreshInterval, refreshIncidents on its own
	// faster cadence), so the fallback is as fresh as the newest of the two.
	// Only refreshRoadData reads it: ListIncidents has its own per-area
	// stale fallback and IncidentFeedHealth.
	chpIncidentsKey = "chp_incidents:last_good"
)

// statusInputTTL is the TTL of a cached status input: the roads refresh
// interval, so an entry is servable-stale until twice that.
func (s *RoadsService) statusInputTTL() time.Duration {
	if ttl := s.config.Roads.RefreshInterval; ttl > 0 {
		return ttl
	}
	return 15 * time.Minute
}

// RoadInputFailure is one status input that failed at the roads service's
// last refresh.
type RoadInputFailure struct {
	// Input names what could not be read: "road conditions for Hwy 4",
	// "CHP incidents".
	Input string
	// Since is the first refresh of this run of failures.
	Since time.Time
	// Err is the latest failure.
	Err error
	// LastGoodUsed is true when the last good read was still servable and
	// the refresh went on with it. False means the refresh failed, and the
	// service is serving the road set of an earlier refresh.
	LastGoodUsed bool
}

// Describe is the failure as source health shows it:
//
//	road conditions for Hwy 4 unavailable since 17:05Z; last good read used (HTTP error 503 ...)
//	CHP incidents unavailable since 17:05Z; no last good read to use, road status not refreshed (...)
//
// The time is UTC; it carries its date once the failure is a day old.
func (f RoadInputFailure) Describe(now time.Time) string {
	layout := "15:04Z"
	if now.Sub(f.Since) >= 24*time.Hour {
		layout = "2006-01-02 15:04Z"
	}
	outcome := "no last good read to use, road status not refreshed"
	if f.LastGoodUsed {
		outcome = "last good read used"
	}
	msg := fmt.Sprintf("%s unavailable since %s; %s", f.Input, f.Since.UTC().Format(layout), outcome)
	if f.Err != nil {
		msg += fmt.Sprintf(" (%v)", f.Err)
	}
	return msg
}

// roadInputOutcome is how one status input's read went in one refresh.
type roadInputOutcome struct {
	key, input   string
	err          error // nil: read from upstream
	lastGoodUsed bool
}

// readStatusInput reads one status input. A good read is cached under key
// and returned. A failed one returns the last good read while it is
// servable-stale (outcome.lastGoodUsed), and otherwise nothing: the caller
// must then fail the refresh rather than carry on as if nothing were
// reported.
func readStatusInput[T any](ctx context.Context, s *RoadsService, key, input string, fetch func() (T, error)) (T, roadInputOutcome) {
	out := roadInputOutcome{key: key, input: input}
	v, err := fetch()
	if err == nil {
		if cerr := s.cache.Set(key, v, s.statusInputTTL(), "road_status_input"); cerr != nil {
			logging.Errorw(ctx, "Failed to cache road status input", "input", input, "error", cerr)
		}
		return v, out
	}
	out.err = err
	var cached T
	_, found, cerr := s.cache.GetWithMetadata(key, &cached)
	if !found || cerr != nil || s.cache.IsVeryStale(key) {
		logging.Errorw(ctx, "Road status input failed and there is no servable last good read",
			"input", input, "error", err)
		var none T
		return none, out
	}
	out.lastGoodUsed = true
	logging.Warnw(ctx, "Road status input failed; using the last good read", "input", input, "error", err)
	return cached, out
}

// statusCHPIncidents is the CHP incident set per-road status is read from.
func (s *RoadsService) statusCHPIncidents(ctx context.Context) ([]caltrans.CaltransIncident, roadInputOutcome) {
	return readStatusInput(ctx, s, chpIncidentsKey, "CHP incidents", func() ([]caltrans.CaltransIncident, error) {
		return s.caltransClient.ParseCHPIncidents(ctx)
	})
}

// cacheCHPIncidents records a good chp-only.kml read made outside the roads
// refresh as the last good set (see chpIncidentsKey).
func (s *RoadsService) cacheCHPIncidents(ctx context.Context, incidents []caltrans.CaltransIncident) {
	if err := s.cache.Set(chpIncidentsKey, incidents, s.statusInputTTL(), "road_status_input"); err != nil {
		logging.Errorw(ctx, "Failed to cache CHP incidents", "error", err)
	}
}

// fetchRoadConditions reads roads.dot.ca.gov once per highway of the
// monitored roads. The map holds the highways that have conditions; the
// outcomes hold every highway read.
func (s *RoadsService) fetchRoadConditions(ctx context.Context) (map[string][]caltrans.RoadCondition, []roadInputOutcome) {
	result := make(map[string][]caltrans.RoadCondition)
	var outcomes []roadInputOutcome

	seen := make(map[string]bool)
	for _, road := range s.config.Roads.MonitoredRoads {
		hwNum := extractHighwayNumber(road.Name)
		if hwNum == "" || seen[hwNum] {
			continue
		}
		seen[hwNum] = true

		conditions, outcome := readStatusInput(ctx, s, roadConditionsKeyPrefix+hwNum, "road conditions for Hwy "+hwNum,
			func() ([]caltrans.RoadCondition, error) {
				return s.caltransClient.ParseRoadConditions(ctx, hwNum)
			})
		outcomes = append(outcomes, outcome)

		if len(conditions) > 0 {
			result[hwNum] = conditions
			logging.Infow(ctx, "Road conditions",
				"highway", hwNum,
				"conditions", len(conditions),
				"last_good_used", outcome.lastGoodUsed)
		}
	}

	return result, outcomes
}

// recordRoadInputHealth stores how the status inputs read in the refresh
// that just ran. An input failing again keeps the time it first failed.
func (s *RoadsService) recordRoadInputHealth(outcomes []roadInputOutcome) {
	now := s.clock()
	s.roadInputMu.Lock()
	defer s.roadInputMu.Unlock()

	since := make(map[string]time.Time)
	var failed []RoadInputFailure
	for _, o := range outcomes {
		if o.err == nil {
			continue
		}
		at, failing := s.roadInputSince[o.key]
		if !failing {
			at = now
		}
		since[o.key] = at
		failed = append(failed, RoadInputFailure{Input: o.input, Since: at, Err: o.err, LastGoodUsed: o.lastGoodUsed})
	}
	s.roadInputSince = since
	s.roadInputFailed = failed
	s.roadInputAt = now
}

// RoadInputHealth reports the status inputs (CHP incidents, and road
// conditions per highway) that failed at the most recent road refresh, and
// when that refresh ran. Empty means every input was read; a zero at means
// no refresh has run yet.
//
// ListRoads cannot show this: with the last good read still servable the
// refresh succeeds on it, and past that the service goes on serving the road
// set of an earlier refresh. Like IncidentFeedHealth, this accessor is how a
// consumer (the grid road-segment poller) tells either from a clean read.
func (s *RoadsService) RoadInputHealth() (failed []RoadInputFailure, at time.Time) {
	s.roadInputMu.Lock()
	defer s.roadInputMu.Unlock()
	return append([]RoadInputFailure(nil), s.roadInputFailed...), s.roadInputAt
}
