package gridapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dpup/prefab/plugins/etag"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

const (
	// defaultSamplePoints is maxPoints when the request has none: enough for
	// a chart a screen wide, and under a week of one road (about 670 samples),
	// so the default window is answered in buckets.
	defaultSamplePoints = 500
	// maxSamplePoints is the ceiling maxPoints is clamped to.
	maxSamplePoints = 2000
	// sampleGapFactor: consecutive samples further apart than this many
	// sampling intervals are a gap. One missed refresh is not (the sampler and
	// the roads refresh are two 15-minute clocks that can slip a tick).
	sampleGapFactor = 3
)

// sampleBucketLadder is the bucket widths a series is answered in, in seconds,
// narrowest first. Past a day the width is a whole number of days.
var sampleBucketLadder = []int64{300, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400}

// GetEventSamples returns the series behind an event whose layer keeps one,
// as at most maxPoints points. Today that is ROAD_SEGMENT, from the table
// GetRoadHistory reads (which answers every row, unbucketed).
//
// The window's rows are read whole and bucketed here: the window is clamped to
// the archive's 400 days, about 38,000 rows of five integers for one road.
func (g *GridServer) GetEventSamples(ctx context.Context, req *gridv1.GetEventSamplesRequest) (*gridv1.EventSamples, error) {
	maxPoints := defaultSamplePoints
	if req.MaxPoints != nil {
		if req.GetMaxPoints() < 1 {
			return nil, invalidErr(fmt.Errorf("maxPoints must be at least 1, got %d", req.GetMaxPoints()))
		}
		maxPoints = int(min(req.GetMaxPoints(), maxSamplePoints))
	}
	ev, err := g.svc.Store.GetEvent(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	road := ev.GetRoadSegment().GetRoadId()
	if ev.GetLayer() != gridv1.Layer_ROAD_SEGMENT || road == "" {
		return nil, notFoundErr("events on the %s layer keep no samples (ROAD_SEGMENT events do)", ev.GetLayer())
	}
	from, to, err := g.roadWindow(req.GetFrom(), req.GetTo())
	if err != nil {
		return nil, err
	}
	now := g.svc.Now()
	cadence := g.roadSampleCadence()

	// The validator, from one index-only read, before the rows are loaded.
	first, last, n, err := g.svc.Store.RoadSampleCoverage(ctx, road, from, to)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	prefix, err := g.validatorPrefix(ctx)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	tag := fmt.Sprintf("%s:samples:%s:%d:%s:%d.%d.%d", prefix, ev.GetId(), maxPoints,
		sampleWindowKey(req, from, to, now, cadence), n, first.Unix(), last.Unix())
	if err := etag.Guard(ctx, etag.Weak(tag)); err != nil {
		return nil, err
	}

	samples, err := g.svc.Store.RoadSamples(ctx, road, from, to)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	times := make([]time.Time, len(samples))
	for i, s := range samples {
		times[i] = s.At
	}
	width := sampleBucketWidth(times, maxPoints)
	out := &gridv1.EventSamples{
		EventId: ev.GetId(), Layer: ev.GetLayer(),
		From: timestamppb.New(from), To: timestamppb.New(to),
		Coverage:      &gridv1.SampleCoverage{Samples: int32(len(samples))},
		BucketSeconds: int32(width),
		Gaps:          sampleGaps(times, from, minTime(to, now), sampleGapFactor*cadence),
		Road:          roadSamplePoints(samples, width),
	}
	if len(samples) > 0 {
		out.Coverage.First = timestamppb.New(times[0])
		out.Coverage.Last = timestamppb.New(times[len(times)-1])
	}
	return out, nil
}

// roadSampleCadence is how far apart road samples normally are: the sampler's
// interval, or the roads refresh when that is slower (a sample is stamped with
// its set's refresh time, so one refresh is at most one sample).
func (g *GridServer) roadSampleCadence() time.Duration {
	if g.svc.Cfg != nil && g.svc.Cfg.Roads.RefreshInterval > store.RoadSampleInterval {
		return g.svc.Cfg.Roads.RefreshInterval
	}
	return store.RoadSampleInterval
}

// sampleWindowKey is the window's part of the validator. A window that ends in
// the past is its two bounds. One that ends now (no `to`, or a `to` in the
// future) moves with every request, and a tag carrying its bounds would never
// match; it is keyed by the current sampling interval instead, so the tag
// holds between samples and still turns over while none arrive (the trailing
// gap grows, and old samples leave the back of a default window). The sample
// count and bounds beside it catch every sample that lands or leaves sooner.
func sampleWindowKey(req *gridv1.GetEventSamplesRequest, from, to, now time.Time, cadence time.Duration) string {
	if req.GetTo() != nil && !to.After(now) {
		return fmt.Sprintf("%d-%d", from.UnixNano(), to.UnixNano())
	}
	start := "d"
	if req.GetFrom() != nil {
		start = fmt.Sprint(from.UnixNano())
	}
	end := "now"
	if req.GetTo() != nil {
		end = fmt.Sprint(to.UnixNano())
	}
	return fmt.Sprintf("%s-%s@%d", start, end, now.Unix()/int64(cadence/time.Second))
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// sampleGaps is the stretches of [from, end) without samples: between
// consecutive samples (times, oldest first) further apart than threshold,
// before the first and after the last. Without samples the window is one gap.
func sampleGaps(times []time.Time, from, end time.Time, threshold time.Duration) []*gridv1.SampleGap {
	var gaps []*gridv1.SampleGap
	add := func(a, b time.Time) {
		if b.Sub(a) > threshold {
			gaps = append(gaps, &gridv1.SampleGap{From: timestamppb.New(a), To: timestamppb.New(b)})
		}
	}
	prev := from
	for _, t := range times {
		add(prev, t)
		prev = t
	}
	add(prev, end)
	return gaps
}

// sampleBucketWidth is the bucket width, in seconds, that answers the samples
// (times, oldest first) in at most maxPoints points: 0 (each sample its own
// point) when they already fit, else the narrowest ladder width whose
// non-empty buckets number at most maxPoints, else the fewest whole days.
func sampleBucketWidth(times []time.Time, maxPoints int) int64 {
	if len(times) <= maxPoints {
		return 0
	}
	fits := func(width int64) bool {
		n, prev := 0, int64(math.MinInt64)
		for _, t := range times {
			if b := floorDiv(t.Unix(), width); b != prev {
				if n, prev = n+1, b; n > maxPoints {
					return false
				}
			}
		}
		return true
	}
	for _, w := range sampleBucketLadder {
		if fits(w) {
			return w
		}
	}
	const day = 86400
	// Ends: maxPoints is at least 1, and a wide enough bucket holds them all.
	for days := int64(2); ; days++ {
		if fits(days * day) {
			return days * day
		}
	}
}

// floorDiv rounds toward negative infinity, so bucket edges are multiples of
// the width on both sides of the epoch.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

// roadStatusRank orders statuses by how much they restrict the road.
func roadStatusRank(s int32) int {
	switch api.RoadStatus(s) {
	case api.RoadStatus_CLOSED:
		return 4
	case api.RoadStatus_RESTRICTED:
		return 3
	case api.RoadStatus_MAINTENANCE:
		return 2
	case api.RoadStatus_OPEN:
		return 1
	}
	return 0
}

// roadBucket accumulates the samples of one point.
type roadBucket struct {
	at                 time.Time
	n                  int32
	status, congestion int32
	durSum, durN       int64
	durMin, durMax     int32
	delaySum, delayN   int64
	delayMax           int32
}

func (b *roadBucket) add(s store.RoadSample) {
	b.n++
	if b.n == 1 || roadStatusRank(s.Status) > roadStatusRank(b.status) {
		b.status = s.Status
	}
	b.congestion = max(b.congestion, s.Congestion) // the enum ascends CLEAR..SEVERE
	// A closed road has no travel time, whatever the router answered for it.
	if api.RoadStatus(s.Status) == api.RoadStatus_CLOSED {
		return
	}
	if d := s.DurationMinutes; d != nil {
		if b.durN == 0 {
			b.durMin, b.durMax = *d, *d
		}
		b.durMin, b.durMax = min(b.durMin, *d), max(b.durMax, *d)
		b.durSum += int64(*d)
		b.durN++
	}
	if d := s.DelayMinutes; d != nil {
		if b.delayN == 0 {
			b.delayMax = *d
		}
		b.delayMax = max(b.delayMax, *d)
		b.delaySum += int64(*d)
		b.delayN++
	}
}

func (b *roadBucket) point() *gridv1.RoadSamplePoint {
	p := &gridv1.RoadSamplePoint{
		At:         timestamppb.New(b.at),
		Samples:    b.n,
		Status:     enumName(api.RoadStatus(b.status).String()),
		Congestion: enumName(api.CongestionLevel(b.congestion).String()),
	}
	if b.durN > 0 {
		p.DurationMinutes = wrapperspb.Double(meanTenths(b.durSum, b.durN))
		p.DurationMinutesMin = wrapperspb.Int32(b.durMin)
		p.DurationMinutesMax = wrapperspb.Int32(b.durMax)
	}
	if b.delayN > 0 {
		p.DelayMinutes = wrapperspb.Double(meanTenths(b.delaySum, b.delayN))
		p.DelayMinutesMax = wrapperspb.Int32(b.delayMax)
	}
	return p
}

// meanTenths is sum/n rounded to one decimal.
func meanTenths(sum, n int64) float64 {
	return math.Round(float64(sum)*10/float64(n)) / 10
}

// roadSamplePoints turns samples (oldest first) into points: one each when
// width is 0, else one per non-empty bucket of width seconds, aligned to the
// epoch and stamped with the bucket's start.
func roadSamplePoints(samples []store.RoadSample, width int64) []*gridv1.RoadSamplePoint {
	var out []*gridv1.RoadSamplePoint
	var cur *roadBucket
	for _, s := range samples {
		at := s.At
		if width > 0 {
			at = time.Unix(floorDiv(s.At.Unix(), width)*width, 0).UTC()
		}
		if cur == nil || width == 0 || !cur.at.Equal(at) {
			if cur != nil {
				out = append(out, cur.point())
			}
			cur = &roadBucket{at: at}
		}
		cur.add(s)
	}
	if cur != nil {
		out = append(out, cur.point())
	}
	return out
}
