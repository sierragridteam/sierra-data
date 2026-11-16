package gridapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

const defaultRoadHistoryWindow = 7 * 24 * time.Hour

// GetRoadHistory returns one monitored segment's archived samples.
func (g *GridServer) GetRoadHistory(ctx context.Context, req *gridv1.GetRoadHistoryRequest) (*gridv1.RoadHistory, error) {
	road := strings.TrimPrefix(req.GetRoad(), "road:")
	known := false
	if g.svc.Cfg != nil {
		for _, r := range g.svc.Cfg.Roads.MonitoredRoads {
			known = known || r.ID == road
		}
	}
	if !known {
		return nil, notFoundErr("unknown road segment: %q", req.GetRoad())
	}
	samples, from, to, err := g.roadSamples(ctx, road, req.GetFrom(), req.GetTo())
	if err != nil {
		return nil, err
	}
	return &gridv1.RoadHistory{Road: road, From: timestamppb.New(from), To: timestamppb.New(to), Samples: samples}, nil
}

// GetEventSamples returns the series behind an event whose layer keeps one.
// Today that is ROAD_SEGMENT, from the same table as GetRoadHistory.
func (g *GridServer) GetEventSamples(ctx context.Context, req *gridv1.GetEventSamplesRequest) (*gridv1.EventSamples, error) {
	ev, err := g.svc.Store.GetEvent(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	if ev.GetLayer() != gridv1.Layer_ROAD_SEGMENT || ev.GetRoadSegment().GetRoadId() == "" {
		return nil, notFoundErr("events on the %s layer keep no samples (ROAD_SEGMENT events do)", ev.GetLayer())
	}
	samples, from, to, err := g.roadSamples(ctx, ev.GetRoadSegment().GetRoadId(), req.GetFrom(), req.GetTo())
	if err != nil {
		return nil, err
	}
	return &gridv1.EventSamples{EventId: ev.GetId(), Layer: ev.GetLayer(), From: timestamppb.New(from), To: timestamppb.New(to), Road: samples}, nil
}

// roadSamples reads one road's samples over the requested window (default the
// 7 days before to, default now; clamped to the archive's 400 days).
func (g *GridServer) roadSamples(ctx context.Context, road string, reqFrom, reqTo *timestamppb.Timestamp) ([]*gridv1.RoadSample, time.Time, time.Time, error) {
	to := g.svc.Now()
	if reqTo != nil {
		to = reqTo.AsTime()
	}
	from := to.Add(-defaultRoadHistoryWindow)
	if reqFrom != nil {
		from = reqFrom.AsTime()
	}
	if !from.Before(to) {
		return nil, from, to, invalidErr(errors.New("from must be before to"))
	}
	if to.Sub(from) > maxTelemetryWindow {
		from = to.Add(-maxTelemetryWindow)
	}
	samples, err := g.svc.Store.RoadSamples(ctx, road, from, to)
	if err != nil {
		return nil, from, to, internalErr(ctx, err)
	}
	var out []*gridv1.RoadSample
	for _, s := range samples {
		rs := &gridv1.RoadSample{
			At:         timestamppb.New(s.At),
			Status:     enumName(api.RoadStatus(s.Status).String()),
			Congestion: enumName(api.CongestionLevel(s.Congestion).String()),
		}
		if s.DurationMinutes != nil {
			rs.DurationMinutes = wrapperspb.Int32(*s.DurationMinutes)
		}
		if s.DelayMinutes != nil {
			rs.DelayMinutes = wrapperspb.Int32(*s.DelayMinutes)
		}
		out = append(out, rs)
	}
	return out, from, to, nil
}

// enumName is an enum value's name, "" for UNSPECIFIED.
func enumName(s string) string {
	if strings.HasSuffix(s, "_UNSPECIFIED") {
		return ""
	}
	return s
}
