package related

import (
	"context"
	"errors"
	"strings"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// RuleRoadStatusCause relates a monitored road and an event its status was
// read from: the road's own event names it (roadSegment.causes[].eventId).
//
// It does not go through Rule. The road names the other event outright, so
// no time window or distance decides anything (a road event has no shape at
// all), and reading the named ids is cheaper than judging every road
// incident. It is High: a source names the other event. Here the source is
// the roads service, whose reading of the status the road event records.
//
// The link follows the road's CURRENT causes. When the incident ends and the
// road reopens, the road's event drops the cause and the link goes with it;
// the road's history still has it.
const RuleRoadStatusCause = "road-status-cause"

// RuleRoadChains relates a monitored road and a chain-control stretch that
// lies on it: the road's own event names the stretch
// (roadSegment.chains[].eventId), by the rule the road_segment map layer
// writes road.chains with. High, and no window or distance, for the same
// reason as a cause. It follows the road's CURRENT chains; a requirement
// that ended is in the road's history. A stretch's id is a slot per route
// and level, so the event it names can be RESOLVED for the refresh in which
// the road's event still waits to record the lifting.
const RuleRoadChains = "road-chains"

// roadNamedRelated lists, for a monitored road, the stored events its causes
// and chains name, and for any other event, the monitored roads that name it.
func (e *Engine) roadNamedRelated(ctx context.Context, subject *gridv1.Event) ([]Relation, error) {
	if subject.GetLayer() == gridv1.Layer_ROAD_SEGMENT {
		var out []Relation
		seen := map[string]bool{subject.GetId(): true}
		named := func(id, rule, reason string) error {
			if id == "" || seen[id] {
				return nil
			}
			seen[id] = true
			ev, err := e.Store.GetEvent(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return nil // named, never stored: nothing to list
			}
			if err != nil {
				return err
			}
			// As every other relation: no shape, the stored box and centroid.
			if g := ev.GetGeometry(); g != nil {
				g.Geojson, g.Object = nil, nil
			}
			out = append(out, Relation{Event: ev, Rule: rule, Confidence: High, Reason: reason})
			return nil
		}
		for _, c := range subject.GetRoadSegment().GetCauses() {
			if err := named(c.GetEventId(), RuleRoadStatusCause, "Set this road's status: "+strings.ToLower(c.GetStatus())); err != nil {
				return nil, err
			}
		}
		for _, c := range subject.GetRoadSegment().GetChains() {
			if err := named(c.GetEventId(), RuleRoadChains, "Chain requirement on this road: "+c.GetLevel()); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	// Only layers a road can name: CHP incidents (causes) and chain-control
	// stretches (chains).
	if subject.GetLayer() != gridv1.Layer_ROAD_INCIDENT && subject.GetLayer() != gridv1.Layer_CHAIN_CONTROL {
		return nil, nil
	}
	var out []Relation
	// Roads are permanently ACTIVE; there are as many as roads.monitoredRoads.
	q := store.EventQuery{Layers: []gridv1.Layer{gridv1.Layer_ROAD_SEGMENT}, PageSize: store.MaxPageSize, Lite: true,
		Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE}}
	for {
		page, err := e.Store.QueryEventsPage(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, road := range page.Items {
			for _, c := range road.GetRoadSegment().GetCauses() {
				if c.GetEventId() == subject.GetId() {
					out = append(out, Relation{Event: road, Rule: RuleRoadStatusCause, Confidence: High,
						Reason: "This road's status (" + strings.ToLower(c.GetStatus()) + ") was set by this report"})
					break
				}
			}
			for _, c := range road.GetRoadSegment().GetChains() {
				if c.GetEventId() == subject.GetId() {
					out = append(out, Relation{Event: road, Rule: RuleRoadChains, Confidence: High,
						Reason: "This road is under this chain requirement"})
					break
				}
			}
		}
		q.PageToken = page.Next(len(page.Items))
		if q.PageToken == "" {
			break
		}
	}
	return out, nil
}
