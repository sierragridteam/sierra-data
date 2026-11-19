package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// sampleT0 is a UTC midnight, so it is a bucket edge at every ladder width.
var sampleT0 = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

const (
	stOpen       = int32(api.RoadStatus_OPEN)
	stClosed     = int32(api.RoadStatus_CLOSED)
	stRestricted = int32(api.RoadStatus_RESTRICTED)
)

func p32(v int32) *int32 { return &v }

// roadSample is an OPEN sample i sampling intervals after sampleT0.
func roadSample(i int, duration int32) store.RoadSample {
	return store.RoadSample{
		RoadID: "r", At: sampleT0.Add(time.Duration(i) * store.RoadSampleInterval),
		Status: stOpen, Congestion: int32(api.CongestionLevel_CLEAR),
		DurationMinutes: p32(duration), DelayMinutes: p32(duration - 27),
	}
}

// series is n samples from sampleT0, travel time cycling 27..30 minutes.
func series(n int) []store.RoadSample {
	out := make([]store.RoadSample, n)
	for i := range out {
		out[i] = roadSample(i, 27+int32(i%4))
	}
	return out
}

func sampleTimes(samples []store.RoadSample) []time.Time {
	out := make([]time.Time, len(samples))
	for i, s := range samples {
		out[i] = s.At
	}
	return out
}

func bucketCount(times []time.Time, width int64) int {
	seen := map[int64]bool{}
	for _, t := range times {
		seen[t.Unix()/width] = true
	}
	return len(seen)
}

// Raw while the samples fit; then the narrowest ladder width that fits; then
// whole days.
func TestSampleBucketWidth(t *testing.T) {
	week := sampleTimes(series(7 * 96))
	for _, tc := range []struct {
		name      string
		times     []time.Time
		maxPoints int
		want      int64
	}{
		{"no samples", nil, 500, 0},
		{"exactly maxPoints is raw", week[:10], 10, 0},
		{"one over: 5 and 15 minutes do not reduce 15-minute samples", week[:10], 9, 1800},
		{"a week at the default", week, 500, 1800},
		{"a week in 300", week, 300, 3600},
		{"a week in 100", week, 100, 7200},
		{"a week in 60", week, 60, 10800},
		{"a week in 28", week, 28, 21600},
		{"a week in 14", week, 14, 43200},
		{"a week in 7", week, 7, 86400},
		{"one point", week, 1, 0}, // checked below: whole days
	} {
		got := sampleBucketWidth(tc.times, tc.maxPoints)
		if tc.maxPoints == 1 {
			assert.Zero(t, got%86400, "%s: whole days", tc.name)
			assert.Equal(t, 1, bucketCount(tc.times, got), tc.name)
			assert.Greater(t, bucketCount(tc.times, got-86400), 1, "%s: the fewest days that fit", tc.name)
			continue
		}
		assert.Equal(t, tc.want, got, tc.name)
		if got > 0 {
			assert.LessOrEqual(t, bucketCount(tc.times, got), tc.maxPoints, tc.name)
		}
	}

	// Past a day: the fewest whole days, not the next "round" width.
	got := sampleBucketWidth(week, 3)
	assert.Zero(t, got%86400)
	assert.Greater(t, got, int64(86400))
	assert.LessOrEqual(t, bucketCount(week, got), 3)
	assert.Greater(t, bucketCount(week, got-86400), 3)
}

// Buckets are aligned to the epoch: the same window buckets the same way
// twice, and a window moved by one bucket shares every overlapping bucket.
func TestSampleBucketsAreEpochAligned(t *testing.T) {
	all := series(8 * 96)
	window := func(from time.Time) []store.RoadSample {
		var out []store.RoadSample
		for _, s := range all {
			if !s.At.Before(from) && s.At.Before(from.Add(7*24*time.Hour)) {
				out = append(out, s)
			}
		}
		return out
	}
	const maxPoints = 100
	a := window(sampleT0)
	width := sampleBucketWidth(sampleTimes(a), maxPoints)
	require.Equal(t, int64(7200), width)
	pa, again := roadSamplePoints(a, width), roadSamplePoints(window(sampleT0), width)
	require.Len(t, again, len(pa))
	for i := range pa {
		assert.True(t, proto.Equal(pa[i], again[i]), "point %d", i)
		assert.Zero(t, pa[i].GetAt().AsTime().Unix()%width, "a bucket starts on a multiple of its width")
	}

	b := window(sampleT0.Add(2 * time.Hour))
	require.Equal(t, width, sampleBucketWidth(sampleTimes(b), maxPoints), "the moved window picks the same width")
	pb := roadSamplePoints(b, width)
	require.Len(t, pb, len(pa))
	for i := 1; i < len(pa); i++ {
		assert.True(t, proto.Equal(pa[i], pb[i-1]), "bucket %s", pa[i].GetAt().AsTime())
	}
	assert.Equal(t, pa[len(pa)-1].GetAt().AsTime().Add(2*time.Hour), pb[len(pb)-1].GetAt().AsTime())

	// A window that starts inside a bucket: the first point is stamped with
	// the bucket's start, before the window, and holds only what is inside.
	part := roadSamplePoints(a[3:], width)
	assert.Equal(t, sampleT0, part[0].GetAt().AsTime())
	assert.Equal(t, int32(5), part[0].GetSamples())
	assert.Equal(t, int32(8), part[1].GetSamples())
}

func TestRoadSamplePoints(t *testing.T) {
	closed := func(i int) store.RoadSample {
		s := roadSample(i, 29) // the router still answers for a closed road
		s.Status, s.Congestion = stClosed, 0
		return s
	}
	with := func(s store.RoadSample, status, congestion int32) store.RoadSample {
		s.Status, s.Congestion = status, congestion
		return s
	}
	noTime := roadSample(11, 0)
	noTime.DurationMinutes, noTime.DelayMinutes = nil, nil

	samples := []store.RoadSample{
		// hour 0: open, 27..30 minutes
		roadSample(0, 27), roadSample(1, 28), roadSample(2, 30), roadSample(3, 30),
		// hour 1: two open (27, 28), two closed
		roadSample(4, 27), with(roadSample(5, 28), stRestricted, int32(api.CongestionLevel_HEAVY)), closed(6), closed(7),
		// hour 2: closed throughout (one short of the hour)
		closed(8), closed(9), closed(10),
		// hour 2, last quarter: open, but the refresh had no travel time
		noTime,
		// hour 3: restricted beats open; the worst congestion wins
		with(roadSample(12, 31), stRestricted, int32(api.CongestionLevel_LIGHT)),
		with(roadSample(13, 40), stOpen, int32(api.CongestionLevel_SEVERE)),
		with(roadSample(14, 33), int32(api.RoadStatus_MAINTENANCE), int32(api.CongestionLevel_MODERATE)),
	}

	raw := roadSamplePoints(samples, 0)
	require.Len(t, raw, len(samples))
	open := raw[0]
	assert.Equal(t, int32(1), open.GetSamples())
	assert.Equal(t, sampleT0, open.GetAt().AsTime())
	assert.Equal(t, "OPEN", open.GetStatus())
	assert.Equal(t, 27.0, open.GetDurationMinutes().GetValue())
	assert.Equal(t, int32(27), open.GetDurationMinutesMin().GetValue())
	assert.Equal(t, int32(27), open.GetDurationMinutesMax().GetValue())
	assert.Equal(t, 0.0, open.GetDelayMinutes().GetValue())
	require.NotNil(t, open.GetDelayMinutes(), "a delay of 0 is a figure, not null")
	assert.Equal(t, int32(0), open.GetDelayMinutesMax().GetValue())
	require.NotNil(t, open.GetDelayMinutesMax())

	for _, p := range []*gridv1.RoadSamplePoint{raw[6], raw[11]} { // closed; no travel time
		assert.Nil(t, p.GetDurationMinutes())
		assert.Nil(t, p.GetDurationMinutesMin())
		assert.Nil(t, p.GetDurationMinutesMax())
		assert.Nil(t, p.GetDelayMinutes())
		assert.Nil(t, p.GetDelayMinutesMax())
	}
	assert.Equal(t, "CLOSED", raw[6].GetStatus())
	assert.Equal(t, "", raw[6].GetCongestion())
	assert.Equal(t, "OPEN", raw[11].GetStatus())

	hourly := roadSamplePoints(samples, 3600)
	require.Len(t, hourly, 4)
	h0, mixed, shut, h3 := hourly[0], hourly[1], hourly[2], hourly[3]

	assert.Equal(t, int32(4), h0.GetSamples())
	assert.Equal(t, 28.8, h0.GetDurationMinutes().GetValue(), "115/4 = 28.75, to one decimal")
	assert.Equal(t, int32(27), h0.GetDurationMinutesMin().GetValue())
	assert.Equal(t, int32(30), h0.GetDurationMinutesMax().GetValue())
	assert.Equal(t, 1.8, h0.GetDelayMinutes().GetValue())
	assert.Equal(t, int32(3), h0.GetDelayMinutesMax().GetValue())
	assert.Equal(t, "OPEN", h0.GetStatus())
	assert.Equal(t, "CLEAR", h0.GetCongestion())

	assert.Equal(t, sampleT0.Add(time.Hour), mixed.GetAt().AsTime())
	assert.Equal(t, int32(4), mixed.GetSamples(), "closed samples are counted")
	assert.Equal(t, "CLOSED", mixed.GetStatus(), "the most restrictive status")
	assert.Equal(t, "HEAVY", mixed.GetCongestion())
	assert.Equal(t, 27.5, mixed.GetDurationMinutes().GetValue(), "the mean of the two samples that were not closed")
	assert.Equal(t, int32(27), mixed.GetDurationMinutesMin().GetValue())
	assert.Equal(t, int32(28), mixed.GetDurationMinutesMax().GetValue())
	assert.Equal(t, 0.5, mixed.GetDelayMinutes().GetValue())
	assert.Equal(t, int32(1), mixed.GetDelayMinutesMax().GetValue())

	assert.Equal(t, int32(4), shut.GetSamples())
	assert.Equal(t, "CLOSED", shut.GetStatus())
	assert.Nil(t, shut.GetDurationMinutes(), "closed, or without a travel time: nothing counts")
	assert.Nil(t, shut.GetDurationMinutesMin())
	assert.Nil(t, shut.GetDurationMinutesMax())
	assert.Nil(t, shut.GetDelayMinutes())
	assert.Nil(t, shut.GetDelayMinutesMax())
	assert.Equal(t, "CLEAR", shut.GetCongestion())

	assert.Equal(t, "RESTRICTED", h3.GetStatus(), "RESTRICTED over MAINTENANCE over OPEN")
	assert.Equal(t, "SEVERE", h3.GetCongestion())
	assert.Equal(t, 34.7, h3.GetDurationMinutes().GetValue())
	assert.Equal(t, int32(40), h3.GetDurationMinutesMax().GetValue())

	// An all-closed bucket, with nothing else in it.
	only := roadSamplePoints([]store.RoadSample{closed(0), closed(1)}, 3600)
	require.Len(t, only, 1)
	assert.Equal(t, int32(2), only[0].GetSamples())
	assert.Nil(t, only[0].GetDurationMinutes())
	assert.Nil(t, only[0].GetDelayMinutes())
}

func TestSampleGaps(t *testing.T) {
	const threshold = 3 * store.RoadSampleInterval
	at := func(minutes int) time.Time { return sampleT0.Add(time.Duration(minutes) * time.Minute) }
	type gap [2]int
	for _, tc := range []struct {
		name     string
		times    []int
		from, to int
		want     []gap
	}{
		{"dense", []int{0, 15, 30, 45, 60}, 0, 75, nil},
		{"exactly three intervals is not a gap", []int{0, 45, 90}, -45, 135, nil},
		{"inner", []int{0, 15, 61, 75}, 0, 90, []gap{{15, 61}}},
		{"leading", []int{60, 75}, 0, 90, []gap{{0, 60}}},
		{"trailing", []int{0, 15}, 0, 90, []gap{{15, 90}}},
		{"all three", []int{100, 115, 300}, 0, 400, []gap{{0, 100}, {115, 300}, {300, 400}}},
		{"no samples", nil, 0, 400, []gap{{0, 400}}},
		{"no samples, a window too short to tell", nil, 0, 45, nil},
		{"the end is before the last sample", []int{0, 15}, 0, 10, nil},
	} {
		var times []time.Time
		for _, m := range tc.times {
			times = append(times, at(m))
		}
		var got []gap
		for _, g := range sampleGaps(times, at(tc.from), at(tc.to), threshold) {
			got = append(got, gap{int(g.GetFrom().AsTime().Sub(sampleT0).Minutes()), int(g.GetTo().AsTime().Sub(sampleT0).Minutes())})
		}
		assert.Equal(t, tc.want, got, tc.name)
	}
}

// roadEventService is a service holding one ROAD_SEGMENT event and no
// samples, with the clock at now.
func roadEventService(t *testing.T, now time.Time) (*Service, *GridServer, string) {
	t.Helper()
	s := newTestService(t)
	s.Now = func() time.Time { return now }
	require.NoError(t, s.Store.SeedSources(context.Background(), []store.SourceSeed{
		{ID: "roads", Name: "Road conditions", PollInterval: 5 * time.Minute},
	}))
	upsert(t, s.Store, &gridv1.Event{
		Id: "road:r", Layer: gridv1.Layer_ROAD_SEGMENT, Category: "open",
		Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE, Headline: "Hwy 4: open",
		Provenance: &gridv1.Provenance{SourceId: "roads", SourceName: "Road conditions", FetchedAt: timestamppb.New(base)},
		Detail:     &gridv1.Event_RoadSegment{RoadSegment: &gridv1.RoadSegmentDetail{RoadId: "r", Name: "Hwy 4", Status: "OPEN"}},
	})
	return s, NewGridServer(s), "road:r"
}

func samplesReq(id string, from, to time.Time, maxPoints ...int32) *gridv1.GetEventSamplesRequest {
	req := &gridv1.GetEventSamplesRequest{Id: id, From: timestamppb.New(from), To: timestamppb.New(to)}
	if len(maxPoints) > 0 {
		req.MaxPoints = &maxPoints[0]
	}
	return req
}

// The window is answered raw up to maxPoints samples and in buckets past it;
// coverage counts samples either way.
func TestGetEventSamplesRawOrBucketed(t *testing.T) {
	s, g, id := roadEventService(t, sampleT0.Add(30*24*time.Hour))
	const n = 2 * 96 // two days
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), series(n)))
	from, to := sampleT0, sampleT0.Add(2*24*time.Hour)

	raw, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to, n))
	require.NoError(t, err)
	assert.Equal(t, id, raw.GetEventId())
	assert.Equal(t, gridv1.Layer_ROAD_SEGMENT, raw.GetLayer())
	assert.Equal(t, from, raw.GetFrom().AsTime())
	assert.Equal(t, to, raw.GetTo().AsTime())
	assert.Zero(t, raw.GetBucketSeconds())
	require.Len(t, raw.GetRoad(), n)
	assert.Equal(t, int32(1), raw.GetRoad()[0].GetSamples())
	assert.Equal(t, int32(n), raw.GetCoverage().GetSamples())
	assert.Equal(t, sampleT0, raw.GetCoverage().GetFirst().AsTime())
	assert.Equal(t, to.Add(-store.RoadSampleInterval), raw.GetCoverage().GetLast().AsTime())
	assert.Empty(t, raw.GetGaps(), "a dense window has no gaps")

	bucketed, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to, n-1))
	require.NoError(t, err)
	assert.Equal(t, int32(1800), bucketed.GetBucketSeconds())
	require.Len(t, bucketed.GetRoad(), n/2)
	assert.Equal(t, int32(2), bucketed.GetRoad()[0].GetSamples())
	assert.Equal(t, 27.5, bucketed.GetRoad()[0].GetDurationMinutes().GetValue())
	assert.True(t, proto.Equal(raw.GetCoverage(), bucketed.GetCoverage()), "coverage is of the samples, not the points")
	assert.Empty(t, bucketed.GetGaps())

	// The default is 500 points: two days fit, a week does not.
	def, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to))
	require.NoError(t, err)
	assert.Zero(t, def.GetBucketSeconds())
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), series(7*96)))
	def, err = g.GetEventSamples(rpcBG(), samplesReq(id, from, from.Add(7*24*time.Hour)))
	require.NoError(t, err)
	assert.Equal(t, int32(1800), def.GetBucketSeconds())
	assert.Len(t, def.GetRoad(), 336)
	assert.Equal(t, int32(672), def.GetCoverage().GetSamples())
}

func TestGetEventSamplesMaxPoints(t *testing.T) {
	s, g, id := roadEventService(t, sampleT0.Add(60*24*time.Hour))
	const n = maxSamplePoints + 1
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), series(n)))
	from, to := sampleT0, sampleT0.Add(30*24*time.Hour)

	for _, bad := range []int32{0, -1} {
		_, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to, bad))
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "maxPoints=%d", bad)
	}
	one, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to, 1))
	require.NoError(t, err)
	require.Len(t, one.GetRoad(), 1)
	assert.Equal(t, int32(n), one.GetRoad()[0].GetSamples())
	assert.Zero(t, one.GetBucketSeconds()%86400)

	// Above the ceiling is read as the ceiling: 2001 samples are bucketed,
	// where an unclamped 5000 would have answered them raw.
	atCeiling, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to, maxSamplePoints))
	require.NoError(t, err)
	over, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to, 5000))
	require.NoError(t, err)
	assert.Equal(t, int32(1800), over.GetBucketSeconds())
	assert.LessOrEqual(t, len(over.GetRoad()), maxSamplePoints)
	assert.True(t, proto.Equal(atCeiling, over))
	assert.Equal(t, rpcTag(t, samplesCall(g, samplesReq(id, from, to, maxSamplePoints))),
		rpcTag(t, samplesCall(g, samplesReq(id, from, to, 5000))), "and shares its validator")
}

// Coverage and gaps on empty and holed windows; the trailing gap ends at now
// when the window runs past it.
func TestGetEventSamplesCoverageAndGaps(t *testing.T) {
	now := sampleT0.Add(6 * time.Hour)
	s, g, id := roadEventService(t, now)

	empty, err := g.GetEventSamples(rpcBG(), samplesReq(id, sampleT0, sampleT0.Add(3*time.Hour)))
	require.NoError(t, err)
	require.NotNil(t, empty.GetCoverage())
	assert.Zero(t, empty.GetCoverage().GetSamples())
	assert.Nil(t, empty.GetCoverage().GetFirst())
	assert.Nil(t, empty.GetCoverage().GetLast())
	assert.Empty(t, empty.GetRoad())
	assert.Zero(t, empty.GetBucketSeconds())
	require.Len(t, empty.GetGaps(), 1, "nothing in the window: one gap")
	assert.Equal(t, sampleT0, empty.GetGaps()[0].GetFrom().AsTime())
	assert.Equal(t, sampleT0.Add(3*time.Hour), empty.GetGaps()[0].GetTo().AsTime())

	// Samples at 1h00..2h00, then 4h00..5h00: a leading gap, a hole, and a
	// trailing gap that stops at now (6h), not at the window's end (12h).
	var batch []store.RoadSample
	for _, i := range []int{4, 5, 6, 7, 8, 16, 17, 18, 19, 20} {
		batch = append(batch, roadSample(i, 28))
	}
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), batch))
	got, err := g.GetEventSamples(rpcBG(), samplesReq(id, sampleT0, sampleT0.Add(12*time.Hour)))
	require.NoError(t, err)
	assert.Equal(t, int32(10), got.GetCoverage().GetSamples())
	assert.Equal(t, sampleT0.Add(time.Hour), got.GetCoverage().GetFirst().AsTime())
	assert.Equal(t, sampleT0.Add(5*time.Hour), got.GetCoverage().GetLast().AsTime())
	hours := func(g *gridv1.SampleGap) [2]float64 {
		return [2]float64{g.GetFrom().AsTime().Sub(sampleT0).Hours(), g.GetTo().AsTime().Sub(sampleT0).Hours()}
	}
	require.Len(t, got.GetGaps(), 3)
	assert.Equal(t, [2]float64{0, 1}, hours(got.GetGaps()[0]), "leading")
	assert.Equal(t, [2]float64{2, 4}, hours(got.GetGaps()[1]), "inner")
	assert.Equal(t, [2]float64{5, 6}, hours(got.GetGaps()[2]), "trailing, to now")

	// Gaps come from the samples, not the buckets: the hole is the same
	// when the answer is bucketed.
	bucketed, err := g.GetEventSamples(rpcBG(), samplesReq(id, sampleT0, sampleT0.Add(12*time.Hour), 4))
	require.NoError(t, err)
	assert.Equal(t, int32(3600), bucketed.GetBucketSeconds())
	require.Len(t, bucketed.GetGaps(), 3)
	assert.Equal(t, [2]float64{2, 4}, hours(bucketed.GetGaps()[1]))

	// The last sample is 45 minutes old: exactly three intervals, no gap yet.
	s.Now = func() time.Time { return sampleT0.Add(5*time.Hour + 45*time.Minute) }
	recent, err := g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: id, From: timestamppb.New(sampleT0.Add(4 * time.Hour))})
	require.NoError(t, err)
	assert.Empty(t, recent.GetGaps())
}

// The wire shape, as the gateway writes it: a sample, a closed sample and a
// bucket share one shape, and a closed road's figures are null, not 0.
func TestGetEventSamplesJSON(t *testing.T) {
	s, g, id := roadEventService(t, sampleT0.Add(24*time.Hour))
	shut := roadSample(1, 29)
	shut.Status, shut.Congestion = stClosed, 0
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), []store.RoadSample{roadSample(0, 28), shut, roadSample(2, 30)}))
	from, to := sampleT0, sampleT0.Add(45*time.Minute)

	raw, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to))
	require.NoError(t, err)
	body, err := gatewaySummaryJSON.Marshal(raw)
	require.NoError(t, err)
	t.Logf("raw: %s", body)
	assert.JSONEq(t, `{
		"eventId": "road:r", "layer": "ROAD_SEGMENT",
		"from": "2026-11-10T00:00:00Z", "to": "2026-11-10T00:45:00Z",
		"coverage": {"first": "2026-11-10T00:00:00Z", "last": "2026-11-10T00:30:00Z", "samples": 3},
		"bucketSeconds": 0,
		"gaps": [],
		"road": [
			{"at": "2026-11-10T00:00:00Z", "samples": 1, "status": "OPEN", "congestion": "CLEAR",
			 "durationMinutes": 28, "durationMinutesMin": 28, "durationMinutesMax": 28, "delayMinutes": 1, "delayMinutesMax": 1},
			{"at": "2026-11-10T00:15:00Z", "samples": 1, "status": "CLOSED", "congestion": "",
			 "durationMinutes": null, "durationMinutesMin": null, "durationMinutesMax": null, "delayMinutes": null, "delayMinutesMax": null},
			{"at": "2026-11-10T00:30:00Z", "samples": 1, "status": "OPEN", "congestion": "CLEAR",
			 "durationMinutes": 30, "durationMinutesMin": 30, "durationMinutesMax": 30, "delayMinutes": 3, "delayMinutesMax": 3}
		]}`, string(body))

	bucketed, err := g.GetEventSamples(rpcBG(), samplesReq(id, from, to, 2))
	require.NoError(t, err)
	body, err = gatewaySummaryJSON.Marshal(bucketed)
	require.NoError(t, err)
	t.Logf("bucketed: %s", body)
	assert.JSONEq(t, `{
		"eventId": "road:r", "layer": "ROAD_SEGMENT",
		"from": "2026-11-10T00:00:00Z", "to": "2026-11-10T00:45:00Z",
		"coverage": {"first": "2026-11-10T00:00:00Z", "last": "2026-11-10T00:30:00Z", "samples": 3},
		"bucketSeconds": 1800,
		"gaps": [],
		"road": [
			{"at": "2026-11-10T00:00:00Z", "samples": 2, "status": "CLOSED", "congestion": "CLEAR",
			 "durationMinutes": 28, "durationMinutesMin": 28, "durationMinutesMax": 28, "delayMinutes": 1, "delayMinutesMax": 1},
			{"at": "2026-11-10T00:30:00Z", "samples": 1, "status": "OPEN", "congestion": "CLEAR",
			 "durationMinutes": 30, "durationMinutesMin": 30, "durationMinutesMax": 30, "delayMinutes": 3, "delayMinutesMax": 3}
		]}`, string(body))
}

func samplesCall(g *GridServer, req *gridv1.GetEventSamplesRequest) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := g.GetEventSamples(ctx, req)
		return err
	}
}

// The validator holds until a sample lands in the window, then changes; a
// sample outside the window does not move it.
func TestGetEventSamplesConditional(t *testing.T) {
	now := sampleT0.Add(2 * time.Hour)
	s, g, id := roadEventService(t, now)
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), series(4)))
	fixed := samplesReq(id, sampleT0, sampleT0.Add(90*time.Minute))
	call := samplesCall(g, fixed)

	tag := rpcTag(t, call)
	assert.Equal(t, tag, rpcTag(t, call), "the same data, the same validator")
	assert.True(t, rpcNotModified(t, tag, call))

	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), []store.RoadSample{roadSample(8, 28)})) // 02:00, outside
	assert.True(t, rpcNotModified(t, tag, call), "a sample outside the window")
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), []store.RoadSample{roadSample(4, 28)})) // 01:00, inside
	assert.False(t, rpcNotModified(t, tag, call), "a sample inside the window")
	next := rpcTag(t, call)
	assert.NotEqual(t, tag, next)
	assert.True(t, rpcNotModified(t, next, call))

	// maxPoints and the window are part of the answer, so of the validator.
	assert.NotEqual(t, next, rpcTag(t, samplesCall(g, samplesReq(id, sampleT0, sampleT0.Add(90*time.Minute), 2))))
	assert.NotEqual(t, next, rpcTag(t, samplesCall(g, samplesReq(id, sampleT0, sampleT0.Add(91*time.Minute)))))

	// A window that ends now moves with the clock. Its validator holds
	// between samples, through requests seconds apart, and changes when a
	// sample lands or, with none, at the next sampling interval (the
	// trailing gap has grown).
	live := samplesCall(g, &gridv1.GetEventSamplesRequest{Id: id})
	s.Now = func() time.Time { return now.Add(time.Second) }
	tag = rpcTag(t, live)
	s.Now = func() time.Time { return now.Add(10 * time.Minute) }
	assert.True(t, rpcNotModified(t, tag, live), "later in the same sampling interval")
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), []store.RoadSample{roadSample(9, 28)})) // 02:15 is after now: not in the window
	assert.True(t, rpcNotModified(t, tag, live))
	require.NoError(t, s.Store.InsertRoadSamples(rpcBG(), []store.RoadSample{{RoadID: "r", At: now.Add(5 * time.Minute), Status: stOpen}}))
	assert.False(t, rpcNotModified(t, tag, live), "a new sample")
	tag = rpcTag(t, live)
	s.Now = func() time.Time { return now.Add(16 * time.Minute) }
	assert.False(t, rpcNotModified(t, tag, live), "the next sampling interval")
}

// Unknown events and layers without a series are 404; an empty window is 400.
func TestGetEventSamplesErrors(t *testing.T) {
	_, g, id := roadEventService(t, base)
	_, err := g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: "road:nowhere"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = g.GetEventSamples(rpcBG(), &gridv1.GetEventSamplesRequest{Id: "usgs:q1"})
	assert.Equal(t, codes.NotFound, status.Code(err), "an event on a layer without samples")
	_, err = g.GetEventSamples(rpcBG(), samplesReq(id, base, base))
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = g.GetEventSamples(rpcBG(), samplesReq(id, base, base.Add(-time.Hour)))
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}
