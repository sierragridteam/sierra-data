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

// roadCauseRelated lists, for a monitored road, the stored events its causes
// name, and for any other event, the monitored roads whose causes name it.
func (e *Engine) roadCauseRelated(ctx context.Context, subject *gridv1.Event) ([]Relation, error) {
	if subject.GetLayer() == gridv1.Layer_ROAD_SEGMENT {
		var out []Relation
		seen := map[string]bool{subject.GetId(): true}
		for _, c := range subject.GetRoadSegment().GetCauses() {
			id := c.GetEventId()
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ev, err := e.Store.GetEvent(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				continue // named by the roads service, never stored: nothing to list
			}
			if err != nil {
				return nil, err
			}
			// As every other relation: no shape, the stored box and centroid.
			if g := ev.GetGeometry(); g != nil {
				g.Geojson, g.Object = nil, nil
			}
			out = append(out, Relation{Event: ev, Rule: RuleRoadStatusCause, Confidence: High,
				Reason: "Set this road's status: " + strings.ToLower(c.GetStatus())})
		}
		return out, nil
	}
	// Only layers a cause can name: today CHP incidents.
	if subject.GetLayer() != gridv1.Layer_ROAD_INCIDENT {
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
		}
		q.PageToken = page.Next(len(page.Items))
		if q.PageToken == "" {
			break
		}
	}
	return out, nil
}
