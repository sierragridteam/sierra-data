package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

func seedRoadSamples(t *testing.T, s *Service, road string) {
	t.Helper()
	d := int32(42)
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), []store.RoadSample{
		{RoadID: road, At: base.Add(-2 * time.Hour), Status: int32(api.RoadStatus_OPEN), Congestion: int32(api.CongestionLevel_CLEAR), DurationMinutes: &d, DelayMinutes: new(int32)},
		{RoadID: road, At: base.Add(-time.Hour), Status: int32(api.RoadStatus_CLOSED)},
	}))
}

func TestGetRoadHistory(t *testing.T) {
	s := newTestService(t)
	road := s.Cfg.Roads.MonitoredRoads[0].ID
	seedRoadSamples(t, s, road)
	g := NewGridServer(s)

	h, err := g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: "road:" + road})
	require.NoError(t, err)
	require.Len(t, h.GetSamples(), 2)
	assert.Equal(t, "OPEN", h.GetSamples()[0].GetStatus())
	assert.Equal(t, "CLEAR", h.GetSamples()[0].GetCongestion())
	assert.Equal(t, int32(42), h.GetSamples()[0].GetDurationMinutes().GetValue())
	assert.Equal(t, "CLOSED", h.GetSamples()[1].GetStatus())
	assert.Nil(t, h.GetSamples()[1].GetDurationMinutes(), "no travel time is null")
	assert.Equal(t, "", h.GetSamples()[1].GetCongestion())

	_, err = g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: "nowhere"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	// A summary headline id for a closure past the segment is not a road:
	// it is refused, not read as the segment.
	for _, id := range []string{road + ":beyond-0", "road:" + road + ":beyond-0"} {
		_, err = g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: id})
		assert.Equal(t, codes.NotFound, status.Code(err), id)
	}
	_, err = g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: road, From: timestamppb.New(base), To: timestamppb.New(base)})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// The samples of a ROAD_SEGMENT event are the road's archive, reached through
// the event: the same rows GetRoadHistory returns. Other layers keep none.
func TestGetEventSamples(t *testing.T) {
	s := newTestService(t)
	road := s.Cfg.Roads.MonitoredRoads[0].ID
	seedRoadSamples(t, s, road)
	require.NoError(t, s.Store.SeedSources(context.Background(), []store.SourceSeed{
		{ID: "roads", Name: "Road conditions", PollInterval: 5 * time.Minute},
	}))
	upsert(t, s.Store, &gridv1.Event{
		Id: "road:" + road, Layer: gridv1.Layer_ROAD_SEGMENT, Category: "open",
		Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE, Headline: "Hwy 4: open",
		Provenance: &gridv1.Provenance{SourceId: "roads", SourceName: "Road conditions", FetchedAt: timestamppb.New(base)},
		Detail:     &gridv1.Event_RoadSegment{RoadSegment: &gridv1.RoadSegmentDetail{RoadId: road, Name: "Hwy 4", Status: "OPEN"}},
	})
	g := NewGridServer(s)

	got, err := g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: "road:" + road})
	require.NoError(t, err)
	assert.Equal(t, gridv1.Layer_ROAD_SEGMENT, got.GetLayer())
	assert.Equal(t, "road:"+road, got.GetEventId())
	h, err := g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: road})
	require.NoError(t, err)
	require.Len(t, got.GetRoad(), len(h.GetSamples()))
	assert.Equal(t, h.GetSamples()[0].GetAt().AsTime(), got.GetRoad()[0].GetAt().AsTime())
	assert.Equal(t, int32(42), got.GetRoad()[0].GetDurationMinutes().GetValue())

	_, err = g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: "road:nowhere"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: "usgs:q1"})
	assert.Equal(t, codes.NotFound, status.Code(err), "an event on a layer without samples")
	_, err = g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: "road:" + road, From: timestamppb.New(base), To: timestamppb.New(base)})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
