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
)

// RoadSegmentSourceID is the registry row of the road-segment events: the
// roads service's own combined read (Google Routes travel times, Caltrans and
// CHP for status), not any one upstream.
const RoadSegmentSourceID = "roads"

// RoadLister is the roads service's current set. ListRoads serves it from the
// service's cache; this poller never causes an upstream fetch of its own
// beyond what any reader of the roads service does.
type RoadLister interface {
	ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error)
}

// RoadSegmentNormalizer records each monitored road's STATE as an event
// (layer ROAD_SEGMENT, id "road:<road id>", the id of its road_segment map
// feature): status, congestion level, and the seasonal closures in effect
// past its ends. The event is revised when one of those changes, so its
// history is when the road closed and reopened.
//
// This is the mesh-node pattern. The numbers that move at every refresh
// (travel time, delay) are not on the event: they are rows of road_samples,
// written by the scheduler's road sampler and served by
// GET /api/v1/events/{id}/samples. Hashing them would mint a revision per
// refresh per road.
//
// Everything on the event is deterministic. The roads service's
// status_explanation is model-written and reworded between refreshes, so it
// is left out: hashed, it would mint a revision each time (the lane-closure
// headline rule).
//
// The events have no geometry. A segment's shape is the road_segment layer's
// (and the corridor place's); a straight line between its ends would attach
// to places the road never touches. Places are preset: the segment's own
// corridor, and the places containing either end.
type RoadSegmentNormalizer struct {
	cfg      *config.Config
	roads    RoadLister
	placesAt func(ctx context.Context, lat, lng float64) ([]string, error)
	now      func() time.Time
}

// NewRoadSegmentNormalizer wires the poller. placesAt may be nil.
func NewRoadSegmentNormalizer(cfg *config.Config, roads RoadLister, placesAt func(ctx context.Context, lat, lng float64) ([]string, error)) *RoadSegmentNormalizer {
	return &RoadSegmentNormalizer{cfg: cfg, roads: roads, placesAt: placesAt, now: time.Now}
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
	byID := make(map[string]*api.Road, len(resp.GetRoads()))
	for _, rd := range resp.GetRoads() {
		byID[rd.GetId()] = rd
	}
	now := n.now()
	res := &PollResult{}
	var missing []string
	for _, mr := range monitored {
		rd := byID[mr.ID]
		if rd == nil || rd.GetStatus() == api.RoadStatus_ROAD_STATUS_UNSPECIFIED {
			// The service did not answer for this road. Its event keeps its
			// last state, and the sweep must not read the gap as "no longer
			// monitored".
			missing = append(missing, mr.ID)
			continue
		}
		ev, err := n.event(ctx, mr, rd, prior, now)
		if err != nil {
			return nil, err
		}
		res.Events = append(res.Events, ev)
	}
	if len(missing) > 0 {
		res.PerSource = map[string]error{RoadSegmentSourceID: fmt.Errorf("no status for monitored road(s): %s", strings.Join(missing, ", "))}
	}
	return res, nil
}

func (n *RoadSegmentNormalizer) event(ctx context.Context, mr config.MonitoredRoad, rd *api.Road, prior Prior, now time.Time) (*gridv1.Event, error) {
	id := "road:" + mr.ID
	status := strings.TrimPrefix(rd.GetStatus().String(), "ROAD_STATUS_")
	congestion := ""
	sevRoad := rd
	if rd.GetCongestionLevel() != api.CongestionLevel_CONGESTION_LEVEL_UNSPECIFIED {
		congestion = rd.GetCongestionLevel().String()
	} else if p := prior.ByID(id); p != nil {
		// No travel time this refresh (Google Routes did not answer): that
		// is a gap in the samples, not a change of state. Keep the last
		// known level, for the severity too.
		congestion = p.GetRoadSegment().GetCongestion()
		sevRoad = &api.Road{Status: rd.GetStatus(), CongestionLevel: api.CongestionLevel(api.CongestionLevel_value[congestion])}
	}

	d := &gridv1.RoadSegmentDetail{RoadId: mr.ID, Name: mr.Name, Section: mr.Section, Status: status, Congestion: congestion}
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

	headline := strings.TrimSpace(mr.Name+" "+mr.Section) + ": " + roadStatusWord(status)
	for _, b := range d.Beyond {
		if b.GetName() != "" {
			headline += "; " + b.GetName() + " in effect"
		}
	}
	ev := NewEvent(id, gridv1.Layer_ROAD_SEGMENT, SeverityFromLabel(hazards.SeverityFromRoad(sevRoad)), gridv1.EventStatus_ACTIVE, headline)
	ev.Category = strings.ToLower(status)
	ev.AreaLabel = strings.TrimSpace(mr.Name + " " + mr.Section)
	ev.ObservedAt = tsProto(now)
	ev.Provenance = NewProvenance(RoadSegmentSourceID, "Road conditions", "Google · quickmap.dot.ca.gov", "")
	ev.Provenance.FetchedAt = tsProto(now)
	ev.Detail = &gridv1.Event_RoadSegment{RoadSegment: d}

	places := map[string]bool{"corridor:" + mr.ID: true}
	if n.placesAt != nil {
		for _, c := range []config.Coordinates{mr.Origin, mr.Destination} {
			ids, err := n.placesAt(ctx, c.Latitude, c.Longitude)
			if err != nil {
				return nil, fmt.Errorf("road segment %s: places: %w", mr.ID, err)
			}
			for _, pid := range ids {
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
