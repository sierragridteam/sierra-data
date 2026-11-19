package gridapi

import (
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

// GET /api/v1/roads/{road}/history has readers outside this repository: its
// body is one row per stored sample, exactly as stored, whatever the event
// samples endpoint does with the same table. A closed road's travel time is
// still the router's figure here, and nothing is bucketed, however many rows.
func TestRoadHistoryShapeIsFixed(t *testing.T) {
	s := newTestService(t)
	road := s.Cfg.Roads.MonitoredRoads[0].ID
	seedRoadSamples(t, s, road)
	d, delay := int32(29), int32(2)
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), []store.RoadSample{
		{RoadID: road, At: base.Add(-30 * time.Minute), Status: int32(api.RoadStatus_CLOSED), Congestion: int32(api.CongestionLevel_HEAVY), DurationMinutes: &d, DelayMinutes: &delay},
	}))
	g := NewGridServer(s)

	h, err := g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: road})
	require.NoError(t, err)
	body, err := gatewaySummaryJSON.Marshal(h)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"road": "hwy-4",
		"from": "2026-06-24T12:00:00Z", "to": "2026-07-01T12:00:00Z",
		"samples": [
			{"at": "2026-07-01T10:00:00Z", "status": "OPEN", "congestion": "CLEAR", "durationMinutes": 42, "delayMinutes": 0},
			{"at": "2026-07-01T11:00:00Z", "status": "CLOSED", "congestion": "", "durationMinutes": null, "delayMinutes": null},
			{"at": "2026-07-01T11:30:00Z", "status": "CLOSED", "congestion": "HEAVY", "durationMinutes": 29, "delayMinutes": 2}
		]}`, string(body))

	// No ceiling and no buckets: more rows than the samples endpoint's
	// largest answer come back one each.
	var many []store.RoadSample
	for i := range maxSamplePoints + 100 {
		many = append(many, store.RoadSample{RoadID: road, At: base.Add(-100*24*time.Hour + time.Duration(i)*15*time.Minute), Status: int32(api.RoadStatus_OPEN)})
	}
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), many))
	h, err = g.GetRoadHistory(rpcBG(), &gridv1.GetRoadHistoryRequest{Road: road, From: timestamppb.New(base.Add(-100 * 24 * time.Hour)), To: timestamppb.New(base.Add(-50 * 24 * time.Hour))})
	require.NoError(t, err)
	assert.Len(t, h.GetSamples(), maxSamplePoints+100)

	// It carries no validator: a conditional request is answered in full.
	ctx, fs := rpcCtx()
	_, err = g.GetRoadHistory(ctx, &gridv1.GetRoadHistoryRequest{Road: road})
	require.NoError(t, err)
	assert.Empty(t, fs.hdr.Get("grpc-metadata-etag"))
}
