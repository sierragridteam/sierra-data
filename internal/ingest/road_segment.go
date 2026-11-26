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
	"github.com/sierragridteam/sierra-data/internal/services"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// RoadSegmentSourceID is the registry row of the road-segment events: the
// roads service's own combined read (Google Routes travel times, Caltrans and
// CHP for status), not any one upstream.
const RoadSegmentSourceID = "roads"

// RoadLister is the roads service's current set. ListRoads serves it from
// the service's cache when it has one; with a cold or evicted cache it runs
// the service's own refresh, as any other reader of the roads service does.
//
// RoadInputHealth is what ListRoads cannot say: which of the service's
// status inputs (CHP incidents, road conditions per highway) failed at its
// last refresh. The service then serves the last good read of that input, or
// past its stale window the road set of an earlier refresh, and either looks
// like a clean answer from here.
type RoadLister interface {
	ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error)
	RoadInputHealth() (failed []services.RoadInputFailure, at time.Time)
}

// RoadChainReader is which chain requirements lie on a monitored road:
// hazards.Service.RoadChainsOn, the rule the road_segment map layer writes
// road.chains with. known false means the read cannot say.
type RoadChainReader interface {
	RoadChainsOn(ctx context.Context) func(mr config.MonitoredRoad, rd *api.Road) (chains []hazards.RoadChainProps, known bool)
}

// RoadSegmentNormalizer records each monitored road's STATE as an event
// (layer ROAD_SEGMENT, id "road:<road id>", the id of its road_segment map
// feature): its status, what set that status (causes), the seasonal
// closures in effect past its ends and the chain requirements on it. The
// event is revised when one of those changes, so its history is when the
// road closed, went under chains and reopened, and for what.
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
// Chains are beside the status, never in it (decided with the user on
// 2026-11-19): a road under a chain requirement is OPEN, and a restricted or
// closed road can be under chains too, so a fourth status value would have
// to choose. The detail lists the stretches on the road (level and stretch
// id, hazards.Service.RoadChainsOn); the headline says "open, chains
// required (R2)" and the category is "chains" while the road is open under
// one, so a list reads closed / chains / open. Severity stays the status's:
// the chain stretch is its own, rated event. Only the level and the stretch
// id are on the event, so a stretch growing or shrinking is not a revision
// here. A stretch id is a slot per route and level, and a renumbered stretch
// (see "Chain requirements as events" in AGENTS.md) is a revision with the
// level unchanged. When the chain read cannot say, the event keeps the
// chains it has: unknown is not "none", and it is not a change.
//
// Four rules keep the history honest about the roads service's own inputs:
//
//   - An input the service could not read is reported, not recorded. The
//     service serves the last good read of a failed CHP or road-conditions
//     fetch while it is servable, then fails its refresh and keeps its last
//     road set (RoadInputHealth). The events go on being written from that
//     data; the roads source says which input failed and since when. Until
//     2026-11-19 a failed fetch read as "nothing reported", which is OPEN
//     and no seasonal closure: on 2026-11-18 Hwy 4 Arnold to Bear Valley
//     went CLOSED, OPEN, CLOSED for five hours (revisions 4 to 9).
//   - A change that EASES a road (to open, a seasonal closure gone, a
//     cause no longer reported, or chains lifted or lowered) is held until a
//     second refresh says the same.
//     It was the only guard against a failed fetch, and covered one refresh
//     of it. It stays for what the service cannot report: a read that
//     succeeds and is wrong for one refresh, and lcs2way.kml, whose failures
//     are still swallowed when CWWP2 lane closures are not configured. A
//     change that restricts is recorded at once.
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
	chains   RoadChainReader // nil: chains are never known, and stay as stored
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

// UseRoadChains wires the chain requirements on each road. Without it the
// events keep whatever chains they have stored.
func (n *RoadSegmentNormalizer) UseRoadChains(r RoadChainReader) *RoadSegmentNormalizer {
	n.chains = r
	return n
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
	// One read of the chain-control registry for the poll, made only when a
	// road has a reading to place it on.
	chainsOn := func(config.MonitoredRoad, *api.Road) ([]hazards.RoadChainProps, bool) { return nil, false }
	if n.chains != nil {
		chainsOn = n.chains.RoadChainsOn(ctx)
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
		// Not known is not "none": the event keeps the chains it has. The
		// chain-control source (caltrans-cc) reports why its read cannot say.
		chains := held.GetRoadSegment().GetChains()
		if on, known := chainsOn(mr, rd); known {
			chains = roadChains(on)
		}
		ev, err := n.event(ctx, mr, rd, chains, refreshed, now)
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
	// Inputs the service could not read at its last refresh. The events above
	// stand: they come from the last good read of each (or, with the refresh
	// failing, from the set of an earlier one, which the age check covers).
	// Only the source is marked, so the failure is on record without logs.
	failed, _ := n.roads.RoadInputHealth()
	for _, f := range failed {
		problems = append(problems, f.Describe(now))
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

// chainRank orders a chain level by how much it asks of a driver.
func chainRank(level string) int {
	switch level {
	case "R1":
		return 1
	case "R2":
		return 2
	case "R3":
		return 3
	}
	return 0
}

// topChainLevel is the highest level among a road's chain requirements, or
// "" with none: what the headline and the easing rule go by.
func topChainLevel(chains []*gridv1.RoadSegmentChain) string {
	top := ""
	for _, c := range chains {
		if chainRank(c.GetLevel()) > chainRank(top) {
			top = c.GetLevel()
		}
	}
	return top
}

// roadChains is a road's chain requirements as the event keeps them: the
// level and the stretch's id, in the read's order (highest level first, then
// id), a stretch listed once. Nothing else of the stretch goes on the road's
// event, because it is hashed: where a stretch begins and ends moves through
// a storm, and that history is the stretch's own event's.
func roadChains(on []hazards.RoadChainProps) []*gridv1.RoadSegmentChain {
	var out []*gridv1.RoadSegmentChain
	seen := map[string]bool{}
	for _, c := range on {
		k := c.Level + "\x00" + c.EventID
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, &gridv1.RoadSegmentChain{Level: c.Level, EventId: c.EventID})
	}
	return out
}

// eases reports whether now is less restrictive than was and in no way more.
// It eases when the status is lower, a seasonal closure is no longer listed,
// a cause is no longer listed, a cause still listed sets a lower status
// than it did, or the highest chain level is lower (chains lifted is the
// lowest). It restricts when the status is higher, a seasonal closure
// was added, a cause was added, a cause still listed sets a higher
// status, or the highest chain level is higher. A chain change that leaves
// the highest level alone (a second, lower stretch ending; a stretch
// renumbered) is neither, and is recorded at once. A change that eases something and restricts nothing waits for a
// second refresh (see RoadSegmentNormalizer); a change that restricts
// anything is recorded at once, whatever else it eases. A cause is the same
// cause while its source, event and text are the same, so reworded condition
// text is one cause gone and another added: recorded at once.
//
// A cause gone is held like a reopening for the same reason: while another
// cause holds the status up, the lost one is all that one bad read of its
// feed would change.
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
	chainsBefore, chainsAfter := chainRank(topChainLevel(was.GetChains())), chainRank(topChainLevel(now.GetChains()))
	if added || causeAdded || chainsAfter > chainsBefore || roadRank(now.GetStatus()) > roadRank(was.GetStatus()) {
		return false
	}
	return lost || causeLost || chainsAfter < chainsBefore || roadRank(now.GetStatus()) < roadRank(was.GetStatus())
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

func (n *RoadSegmentNormalizer) event(ctx context.Context, mr config.MonitoredRoad, rd *api.Road, chains []*gridv1.RoadSegmentChain, refreshed, now time.Time) (*gridv1.Event, error) {
	id := "road:" + mr.ID
	status := strings.TrimPrefix(rd.GetStatus().String(), "ROAD_STATUS_")

	d := &gridv1.RoadSegmentDetail{RoadId: mr.ID, Name: mr.Name, Section: mr.Section, Status: status}
	for _, b := range rd.GetBeyond() {
		d.Beyond = append(d.Beyond, &gridv1.RoadSegmentBeyond{
			Name: b.GetName(), Side: b.GetSide(), Status: strings.TrimPrefix(b.GetStatus().String(), "ROAD_STATUS_"),
			// What the map layer says of the same closure (hazards'
			// roadBeyond): a named seasonal closure is the road's yearly
			// state, INFO whatever its status. Written here so that a
			// client reading the event does not have to decide it.
			Seasonal: true, Severity: hazards.SevInfo,
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
	d.Chains = chains

	// The headline says the status and no reason: an incident's wording is a
	// model's, and the causes are on the detail. Chains are said beside the
	// status, by their highest level: "open, chains required (R2)".
	headline := strings.TrimSpace(mr.Name+" "+mr.Section) + ": " + roadStatusWord(status)
	chainLevel := topChainLevel(chains)
	if chainLevel != "" {
		headline += ", chains required (" + chainLevel + ")"
	}
	for _, b := range d.Beyond {
		if b.GetName() != "" {
			headline += "; " + b.GetName() + " in effect"
		}
	}
	// Severity from the status alone, as the map layer grades a road with
	// clear traffic: congestion is not on the event, and a chain requirement
	// is rated on its own stretch's event (rating it here too would count one
	// requirement twice in every rollup).
	sev := hazards.SeverityFromRoad(&api.Road{Status: rd.GetStatus()})
	ev := NewEvent(id, gridv1.Layer_ROAD_SEGMENT, SeverityFromLabel(sev), gridv1.EventStatus_ACTIVE, headline)
	// The one-word state for lists and filters. An open road under chains is
	// "chains"; a restricted or closed one keeps its status, which is the
	// stronger statement, and has the chains on its detail.
	ev.Category = strings.ToLower(status)
	if status == "OPEN" && chainLevel != "" {
		ev.Category = "chains"
	}
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
