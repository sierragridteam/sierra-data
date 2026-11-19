package ingest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/hazards"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// RoadSegmentSourceID is the registry row of the road-segment events: the
// roads service's own combined read (Google Routes travel times, Caltrans and
// CHP for status), not any one upstream.
const RoadSegmentSourceID = "roads"

// RoadLister is the roads service's current set. ListRoads serves it from
// the service's cache when it has one; with a cold or evicted cache it runs
// the service's own refresh, as any other reader of the roads service does.
type RoadLister interface {
	ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error)
}

// RoadSegmentNormalizer records each monitored road's STATE as an event
// (layer ROAD_SEGMENT, id "road:<road id>", the id of its road_segment map
// feature): its status, what set that status (causes) and the seasonal
// closures in effect past its ends. The event is revised when one of those
// changes, so its history is when the road closed and reopened, and for what.
//
// This is the mesh-node pattern. Everything that moves at each refresh
// (travel time, delay, congestion level) is not on the event: it is a row of
// road_samples, written by the scheduler's road sampler and served by
// GET /api/v1/events/{id}/samples. Congestion was on the event at first; in
// a week of production samples 11 of one road's 14 changes were traffic
// crossing the clear/light line and back, which buried its two closures.
//
// Everything on the event is deterministic. The roads service's
// status_explanation is model-written and reworded between refreshes, so it
// is left out: hashed, it would mint a revision each time (the lane-closure
// headline rule). The same holds for a status cause (roadCauses): of an
// incident or a lane closure the event keeps which one it is, never the
// service's words for it.
//
// Three rules keep the history honest, because the roads service does not
// say when its own inputs failed (a failed CHP or road-conditions fetch
// reads as "nothing reported", which is OPEN):
//
//   - A change that EASES a road (to open, a seasonal closure gone, or a
//     cause no longer reported) is held until a second refresh says the same. One refresh of good news
//     after bad is what a failed upstream fetch looks like; production
//     samples show a restricted road reading open for exactly one refresh.
//     A change that restricts is recorded at once.
//   - A set older than twice the refresh interval is the service serving its
//     last good answer: the events keep their state and the source says so.
//   - observed_at is the set's own refresh time, not the time of this poll.
//
// The events have no geometry. A segment's shape is the road_segment layer's
// (and the corridor place's); a straight line between its ends would attach
// to places the road never touches. Places are preset: the segment's own
// corridor, and the areas, counties and towns containing either end (never
// another road's corridor, which shares a town with it and not a metre of
// pavement).
type RoadSegmentNormalizer struct {
	cfg      *config.Config
	roads    RoadLister
	placesAt func(ctx context.Context, lat, lng float64) ([]string, error)
	now      func() time.Time
	// pending is, per event id, an easing change seen in one refresh and
	// waiting for a second. Poller-local and lost with the writer term,
	// which only delays an easing by one more refresh.
	pending map[string]pendingEase
}

// pendingEase is an easing change first seen in the refresh stamped at.
type pendingEase struct {
	hash string
	at   time.Time
}

// NewRoadSegmentNormalizer wires the poller. placesAt may be nil.
func NewRoadSegmentNormalizer(cfg *config.Config, roads RoadLister, placesAt func(ctx context.Context, lat, lng float64) ([]string, error)) *RoadSegmentNormalizer {
	return &RoadSegmentNormalizer{cfg: cfg, roads: roads, placesAt: placesAt, now: time.Now, pending: map[string]pendingEase{}}
}

// SourceIDs implements Normalizer.
func (n *RoadSegmentNormalizer) SourceIDs() []string { return []string{RoadSegmentSourceID} }

// Poll implements Normalizer.
func (n *RoadSegmentNormalizer) Poll(ctx context.Context, prior Prior) (*PollResult, error) {
	monitored := n.cfg.Roads.MonitoredRoads
	if len(monitored) == 0 {
		return nil, errEmptyScope("monitored roads")
	}
	resp, err := n.roads.ListRoads(ctx, &api.ListRoadsRequest{})
	if err != nil {
		return nil, err
	}
	now := n.now()
	refreshed := resp.GetLastUpdated().AsTime()
	if resp.GetLastUpdated() == nil {
		return nil, fmt.Errorf("road segments: the roads service gave no refresh time")
	}
	res := &PollResult{}
	var problems []string
	// The service keeps serving its last set while its refresh fails. That
	// set is not a reading of the roads now: every event keeps its state.
	stale := false
	if maxAge := 2 * n.cfg.Roads.RefreshInterval; maxAge > 0 && now.Sub(refreshed) > maxAge {
		stale = true
		problems = append(problems, fmt.Sprintf("road status last refreshed %s ago", now.Sub(refreshed).Round(time.Minute)))
	}
	byID := make(map[string]*api.Road, len(resp.GetRoads()))
	for _, rd := range resp.GetRoads() {
		byID[rd.GetId()] = rd
	}
	for _, mr := range monitored {
		id := "road:" + mr.ID
		held := prior.ByID(id)
		rd := byID[mr.ID]
		if stale || rd == nil || rd.GetStatus() == api.RoadStatus_ROAD_STATUS_UNSPECIFIED {
			// No reading of this road. Its event keeps its last state, and
			// the sweep must not read the gap as "no longer monitored".
			if !stale {
				problems = append(problems, "no status for "+mr.ID)
			}
			if held != nil {
				res.Events = append(res.Events, held)
			}
			continue
		}
		ev, err := n.event(ctx, mr, rd, refreshed, now)
		if err != nil {
			return nil, err
		}
		if held != nil && eases(held.GetRoadSegment(), ev.GetRoadSegment()) {
			hash := store.ContentHash(ev)
			if p, ok := n.pending[id]; !ok || p.hash != hash {
				n.pending[id] = pendingEase{hash: hash, at: refreshed}
				res.Events = append(res.Events, held)
				continue
			} else if !refreshed.After(p.at) {
				res.Events = append(res.Events, held) // the same refresh, offered again
				continue
			}
		}
		delete(n.pending, id)
		res.Events = append(res.Events, ev)
	}
	if len(problems) > 0 {
		res.PerSource = map[string]error{RoadSegmentSourceID: fmt.Errorf("road segments: %s", strings.Join(problems, "; "))}
	}
	return res, nil
}

// roadRank orders a road's status by how much it restricts travel.
func roadRank(status string) int {
	switch status {
	case "CLOSED":
		return 2
	case "RESTRICTED", "MAINTENANCE":
		return 1
	}
	return 0
}

// eases reports whether now is less restrictive than was and in no way more.
// It eases when the status is lower, a seasonal closure is no longer listed,
// a cause is no longer listed, or a cause still listed sets a lower status
// than it did. It restricts when the status is higher, a seasonal closure
// was added, a cause was added, or a cause still listed sets a higher
// status. A change that eases something and restricts nothing waits for a
// second refresh (see RoadSegmentNormalizer); a change that restricts
// anything is recorded at once, whatever else it eases. A cause is the same
// cause while its source, event and text are the same, so reworded condition
// text is one cause gone and another added: recorded at once.
//
// A cause gone is held like a reopening for the same reason: a failed CHP or
// road-conditions fetch reads as "nothing reported", and while another cause
// holds the status up, the lost one is all that fetch would change.
func eases(was, now *gridv1.RoadSegmentDetail) bool {
	if was == nil {
		return false
	}
	causeRanks := func(d *gridv1.RoadSegmentDetail) map[string]int {
		m := map[string]int{}
		for _, c := range d.GetCauses() {
			k := c.GetSource() + "\x00" + c.GetEventId() + "\x00" + c.GetText()
			m[k] = max(m[k], roadRank(c.GetStatus()))
		}
		return m
	}
	causesBefore, causesAfter := causeRanks(was), causeRanks(now)
	causeAdded, causeLost := false, false
	for k, r := range causesAfter {
		prev, had := causesBefore[k]
		causeAdded = causeAdded || !had || r > prev
		causeLost = causeLost || (had && r < prev)
	}
	for k := range causesBefore {
		if _, still := causesAfter[k]; !still {
			causeLost = true
		}
	}
	key := func(b *gridv1.RoadSegmentBeyond) string { return b.GetSide() + "|" + b.GetName() }
	before, after := map[string]bool{}, map[string]bool{}
	for _, b := range was.GetBeyond() {
		before[key(b)] = true
	}
	added := false
	for _, b := range now.GetBeyond() {
		after[key(b)] = true
		added = added || !before[key(b)]
	}
	lost := false
	for k := range before {
		lost = lost || !after[k]
	}
	if added || causeAdded || roadRank(now.GetStatus()) > roadRank(was.GetStatus()) {
		return false
	}
	return lost || causeLost || roadRank(now.GetStatus()) < roadRank(was.GetStatus())
}

// roadCauses is what set a road's status, as the event keeps it: for each
// cause the roads service lists, the status it set, its source, the Grid
// event it is (hazards.RoadCauseEventID, the map layer's rule) and the
// source's own text. Nothing else goes on the event, because it is hashed:
//
//   - The service's description of an incident or a lane closure is the
//     model's restriction_details (services.alertCause), reworded between
//     refreshes, and its fallback is the alert's description, which the
//     model also rewrites. So those causes carry no text: the reader has the
//     linked event. A road condition's description is Caltrans's paragraph
//     as parsed from roads.dot.ca.gov (services.conditionCause), with
//     whitespace collapsed here.
//   - Title, source id, begin and end are left out (the id is in event_id;
//     positions are the map layer's).
//
// The service promises no order and can list one condition twice (Caltrans
// repeats a paragraph per district), so the set is de-duplicated and sorted:
// closures first, then source, event id, text. An open road has none, as on
// the map layer.
func roadCauses(rd *api.Road) []*gridv1.RoadSegmentCause {
	if rd.GetStatus() == api.RoadStatus_OPEN {
		return nil
	}
	var out []*gridv1.RoadSegmentCause
	seen := map[string]bool{}
	for _, c := range rd.GetStatusCauses() {
		rc := &gridv1.RoadSegmentCause{
			Status:  strings.TrimPrefix(c.GetStatus().String(), "ROAD_STATUS_"),
			Source:  c.GetSource(),
			EventId: hazards.RoadCauseEventID(c),
		}
		if c.GetSource() == "road_condition" {
			rc.Text = strings.Join(strings.Fields(c.GetDescription()), " ")
		}
		k := rc.GetStatus() + "\x00" + rc.GetSource() + "\x00" + rc.GetEventId() + "\x00" + rc.GetText()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, rc)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := roadRank(a.GetStatus()), roadRank(b.GetStatus()); ra != rb {
			return ra > rb
		}
		if a.GetSource() != b.GetSource() {
			return a.GetSource() < b.GetSource()
		}
		if a.GetEventId() != b.GetEventId() {
			return a.GetEventId() < b.GetEventId()
		}
		return a.GetText() < b.GetText()
	})
	return out
}

func (n *RoadSegmentNormalizer) event(ctx context.Context, mr config.MonitoredRoad, rd *api.Road, refreshed, now time.Time) (*gridv1.Event, error) {
	id := "road:" + mr.ID
	status := strings.TrimPrefix(rd.GetStatus().String(), "ROAD_STATUS_")

	d := &gridv1.RoadSegmentDetail{RoadId: mr.ID, Name: mr.Name, Section: mr.Section, Status: status}
	for _, b := range rd.GetBeyond() {
		d.Beyond = append(d.Beyond, &gridv1.RoadSegmentBeyond{
			Name: b.GetName(), Side: b.GetSide(), Status: strings.TrimPrefix(b.GetStatus().String(), "ROAD_STATUS_"),
		})
	}
	// The service promises no order; an order flip must not be a revision.
	sort.Slice(d.Beyond, func(i, j int) bool {
		if d.Beyond[i].GetSide() != d.Beyond[j].GetSide() {
			return d.Beyond[i].GetSide() < d.Beyond[j].GetSide()
		}
		return d.Beyond[i].GetName() < d.Beyond[j].GetName()
	})

	d.Causes = roadCauses(rd)

	// The headline says the status and no reason: an incident's wording is a
	// model's, and the causes are on the detail.
	headline := strings.TrimSpace(mr.Name+" "+mr.Section) + ": " + roadStatusWord(status)
	for _, b := range d.Beyond {
		if b.GetName() != "" {
			headline += "; " + b.GetName() + " in effect"
		}
	}
	// Severity from the status alone, as the map layer grades a road with
	// clear traffic: congestion is not on the event.
	sev := hazards.SeverityFromRoad(&api.Road{Status: rd.GetStatus()})
	ev := NewEvent(id, gridv1.Layer_ROAD_SEGMENT, SeverityFromLabel(sev), gridv1.EventStatus_ACTIVE, headline)
	ev.Category = strings.ToLower(status)
	ev.AreaLabel = strings.TrimSpace(mr.Name + " " + mr.Section)
	ev.ObservedAt = tsProto(refreshed)
	ev.Provenance = NewProvenance(RoadSegmentSourceID, "Road conditions", "Google · quickmap.dot.ca.gov", "")
	ev.Provenance.FetchedAt = tsProto(now)
	ev.Detail = &gridv1.Event_RoadSegment{RoadSegment: d}
	// Caltrans's condition text, when that is a cause; an incident's words
	// are on its own event.
	var conditions []string
	for _, c := range d.Causes {
		if c.GetSource() == "road_condition" && c.GetText() != "" {
			conditions = append(conditions, c.GetText())
		}
	}
	ev.Description = strings.Join(conditions, "\n\n")

	own := "corridor:" + mr.ID
	places := map[string]bool{own: true}
	if n.placesAt != nil {
		for _, c := range []config.Coordinates{mr.Origin, mr.Destination} {
			ids, err := n.placesAt(ctx, c.Latitude, c.Longitude)
			if err != nil {
				return nil, fmt.Errorf("road segment %s: places: %w", mr.ID, err)
			}
			for _, pid := range ids {
				// Another road's corridor passes within its buffer of a
				// town both roads meet at. This road is not on it.
				if strings.HasPrefix(pid, "corridor:") && pid != own {
					continue
				}
				places[pid] = true
			}
		}
	}
	for pid := range places {
		ev.PlaceIds = append(ev.PlaceIds, pid)
	}
	sort.Strings(ev.PlaceIds)
	return ev, nil
}

func roadStatusWord(status string) string {
	switch status {
	case "OPEN":
		return "open"
	case "CLOSED":
		return "closed"
	case "RESTRICTED":
		return "restricted"
	case "MAINTENANCE":
		return "roadwork"
	}
	return strings.ToLower(status)
}
