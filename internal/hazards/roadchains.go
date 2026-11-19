package hazards

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dpup/prefab/logging"

	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
)

// A monitored road under a chain requirement is OPEN: chains do not change a
// road's status. Until 2026-11-19 nothing on the road's own feature said the
// requirement was there, so each client matched chain_control stretches to
// roads for itself (and the website, matching on a road id the stretches never
// carried, printed "Chains: None" through an R2 storm). road.chains states it
// once: which stretches lie on which road.

// roadChainMaxOffsetMeters is how far from a road's path a chain sign may
// stand and still be on that road. A judgment, not a measurement. Measured
// on 2026-11-19 against the routes Google returns for the four configured
// roads, every Caltrans checkpoint on them sits within 30 m of the path, so
// 250 m is slack for a sign set back from the carriageway or a route that
// cuts a corner; the nearest checkpoint that is NOT on a road is Murphys
// eastbound, 670 m past the end of Angels Camp to Murphys.
//
// The edge that is left: a stretch of a single sign standing within 250 m of
// where two monitored roads meet is on both (roadChainMinReachKm covers a
// longer stretch that only reaches a road's end).
const roadChainMaxOffsetMeters = 250

// roadChainMinReachKm is how much of a road a stretch must cover to be on it
// when the rest of the stretch lies past one of the road's ends. Without it
// a stretch belongs to the road below as well as to its own: on 2026-11-18
// Hwy 4 was under R2 "Blue Lake Springs to Mount Reba Road" for hours, and
// Blue Lake Springs westbound stands 170 m short of where Murphys to Arnold
// ends and Arnold to Bear Valley begins. "Chains R2" on Murphys to Arnold
// would have been true of its last 170 m and wrong for anyone driving it.
// A judgment: more than the spacing of a checkpoint's two signs across a
// junction, less than any distance a driver would call part of the road.
const roadChainMinReachKm = 1.0

// chainStretchOnRoad reports whether a chain-control stretch lies on a road,
// given the road's highway as its name writes it ("Hwy 4") and its path.
// Pure: no clock, no I/O. The rule:
//
//   - Same highway only, by highwayKey: "Hwy 4" is "Highway 4" and never
//     "Highway 49".
//   - A sign is on the road when it has a position within
//     roadChainMaxOffsetMeters of the path. A stretch with a sign on the
//     road is on the road, unless it only reaches the road's end: the rest
//     of it lies past one end, and from there it covers less than
//     roadChainMinReachKm of the road. A stretch with nothing past either
//     end (one checkpoint, or all of it on this road) is on the road however
//     short it is.
//   - A stretch with no sign on the road still lies on it when it spans it:
//     its signs run from the low end to the high end, and two that follow one
//     another stand one past the road's origin and the other past its
//     destination. "Past" an end means off the path and nearer that end than
//     the other. No sign is on the road at that point, so signs past both
//     ends are enough: somewhere in the run two neighbours straddle it.
//   - Nothing else attaches.
//
// What the rule cannot see. A sign speaks for its direction of travel as far
// as the next sign facing the same way (groupChainStretches, rule 1), so the
// requirement of the last sign in a direction runs on past it with no sign
// to mark where: a lone westbound sign above a road puts that road's
// westbound traffic under the requirement and is not on it by this rule. And
// "nearer that end" is a straight line, not a milepost: a highway that came
// back around the far side of a road could put a distant stretch's signs
// either side of it. Neither Hwy 4 nor Hwy 49 does that near the configured
// roads.
func chainStretchOnRoad(roadHighway string, path []LatLng, st chainStretch) bool {
	if len(path) < 2 || !numberedHighway(roadHighway) || !numberedHighway(st.Highway) || !sameHighway(roadHighway, st.Highway) {
		return false
	}
	origin, destination := path[0], path[len(path)-1]
	length, _ := alongPath(path, destination.Lat, destination.Lng)
	var pastOrigin, pastDestination, onRoad bool
	lo, hi := length, 0.0 // km along the path of the lowest and highest sign on the road
	for _, c := range st.Signs {
		if c.Coordinates == nil {
			continue
		}
		lat, lng := c.Coordinates.Latitude, c.Coordinates.Longitude
		if km, off := alongPath(path, lat, lng); off <= roadChainMaxOffsetMeters {
			onRoad = true
			lo, hi = min(lo, km), max(hi, km)
			continue
		}
		if metersFromPoint(origin, lat, lng) <= metersFromPoint(destination, lat, lng) {
			pastOrigin = true
		} else {
			pastDestination = true
		}
	}
	if !onRoad {
		return pastOrigin && pastDestination
	}
	if !pastOrigin && !pastDestination {
		return true
	}
	// The stretch runs on past an end: it covers the road from that end.
	if pastOrigin {
		lo = 0
	}
	if pastDestination {
		hi = length
	}
	return hi-lo >= roadChainMinReachKm
}

func signOnPath(path []LatLng, lat, lng float64) bool {
	_, off := alongPath(path, lat, lng)
	return off <= roadChainMaxOffsetMeters
}

func metersFromPoint(p LatLng, lat, lng float64) float64 {
	_, off := alongPath([]LatLng{p, p}, lat, lng)
	return off
}

// numberedHighway reports whether a label names a highway by number, which
// is what sameHighway can compare. A label without one ("", a street name)
// matches nothing, and nothing is known about it.
func numberedHighway(label string) bool {
	return highwayNumberRe.MatchString(strings.TrimSpace(label))
}

// roadChainRead is one read of the chain-control registry as road_segment
// uses it: every stretch Caltrans reported, anywhere, grouped and numbered
// the way the chain_control layer and ChainRequirements do it, so an entry's
// id is that stretch's id in every area.
type roadChainRead struct {
	stretches []roadChainStretch
	// blind are the signs that could hide a requirement: a status we could
	// not read, or no position. The chain_control layer degrades to STALE on
	// one inside its area; here one makes every road on its highway unknown.
	blind []caltrans.ChainControlData
	// err is set when nothing is known: the read failed, or came from one
	// source only (caltrans.PartialError). cc.kml alone lists active signs
	// and infers every stretch; CWWP2 alone lacks the controls only cc.kml
	// reports. Either way a requirement could be missing.
	err error
}

type roadChainStretch struct {
	stretch chainStretch
	entry   RoadChainProps
}

// newRoadChainRead groups a complete registry read (both sources answered).
func newRoadChainRead(controls, quiet []caltrans.ChainControlData) roadChainRead {
	var read roadChainRead
	for _, c := range controls {
		if c.Unrecognized || c.Coordinates == nil {
			read.blind = append(read.blind, c)
		}
	}
	sorted := sortChainControls(controls, quiet, func(caltrans.ChainControlData) bool { return false })
	grouped := groupChainStretches(sorted.active, sorted.quiet)
	for i, st := range grouped {
		// The entry is read off the stretch's own feature, so it cannot say
		// anything the chain_control layer does not.
		f, ok := chainStretchFeature(st, chainStretchID(grouped, i))
		if !ok {
			continue // no sign in it has a position: every one is in blind
		}
		p := f.Properties
		read.stretches = append(read.stretches, roadChainStretch{stretch: st, entry: RoadChainProps{
			Level:    st.Level,
			EventID:  p.ID,
			Headline: p.Headline,
			From:     p.ChainControl.From.Place,
			To:       p.ChainControl.To.Place,
			Since:    p.Effective,
			Inferred: p.ChainControl.Inferred,
		}})
	}
	return read
}

// on returns the stretches that lie on a road, highest level first and then
// by id. known is false when the read cannot say: chains is then nil and the
// feature must carry no `chains` at all. With known true, chains is never
// nil, and an empty list is Caltrans reporting no requirement on this road.
func (r roadChainRead) on(roadHighway string, path []LatLng) (chains []RoadChainProps, known bool) {
	if r.err != nil || len(path) < 2 || !numberedHighway(roadHighway) {
		return nil, false
	}
	for _, c := range r.blind {
		// A sign on another highway cannot be on this road. One with no
		// highway we can read could be anywhere.
		if !numberedHighway(c.Highway) || sameHighway(roadHighway, c.Highway) {
			return nil, false
		}
	}
	chains = []RoadChainProps{}
	for _, s := range r.stretches {
		if !numberedHighway(s.stretch.Highway) {
			// A requirement whose highway we could not read matches no road.
			// Standing on this road's path, it may well be this road's.
			if anySign(s.stretch.Signs, func(c caltrans.ChainControlData) bool {
				return c.Coordinates != nil && signOnPath(path, c.Coordinates.Latitude, c.Coordinates.Longitude)
			}) {
				return nil, false
			}
			continue
		}
		if chainStretchOnRoad(roadHighway, path, s.stretch) {
			chains = append(chains, s.entry)
		}
	}
	sort.SliceStable(chains, func(i, j int) bool {
		if chains[i].Level != chains[j].Level {
			return chains[i].Level > chains[j].Level // R3, R2, R1
		}
		return chains[i].EventID < chains[j].EventID
	})
	return chains, true
}

const (
	// roadChainRetry is how long a failed read is remembered. Without it a
	// Caltrans outage would put a failing fetch on every road_segment
	// request, and /summary reads road_segment on every call.
	roadChainRetry = time.Minute
	// roadChainReadTimeout bounds what the read may add to a road_segment
	// request. The feeds' own HTTP timeout is 30 s; the roads themselves are
	// already in hand by then, and `chains` absent is an honest answer.
	roadChainReadTimeout = 10 * time.Second
)

// roadChainMemo keeps the last registry read for road_segment.
type roadChainMemo struct {
	mu    sync.Mutex
	read  roadChainRead
	until time.Time
}

// roadChainRegistry is the chain-control registry for road_segment, read
// from Caltrans at most once per chain_control TTL (layerTTL, 5 minutes) per
// process, and at most once a minute while the read fails.
//
// Why its own memo. road_segment has no layer TTL (the roads service caches
// its data), so anything read here is read on every request unless it is
// kept. The chain_control layer's cache cannot be borrowed: it holds one
// area's features, and a road needs the whole registry (a stretch that spans
// a road has no sign inside it; a sign 250 m off a corridor's path is outside
// the corridor's box), plus which signs were unreadable, which the features
// do not carry. The client keeps nothing between calls. So this is a third
// reader of the registry beside the layer (per area, cached 5 minutes) and
// ChainRequirements (the events' poller): the three agree per read and can
// differ by up to a TTL.
//
// One caller reads at a time and the rest wait for it, so a burst after
// expiry is one fetch. A failed read is never answered from an older good
// one: `chains` goes absent until Caltrans answers again.
func (s *Service) roadChainRegistry(ctx context.Context) roadChainRead {
	if s.caltrans == nil {
		return roadChainRead{err: errors.New("no chain-control source configured")}
	}
	m := &s.roadChains
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.until) {
		return m.read
	}
	rctx, cancel := context.WithTimeout(ctx, roadChainReadTimeout)
	defer cancel()
	controls, quiet, err := s.caltrans.ParseChainControlRegistry(rctx)
	if err != nil {
		read := roadChainRead{err: err}
		if ctx.Err() != nil {
			// The caller went away mid-read. That says nothing about
			// Caltrans, so the next request reads again.
			return read
		}
		logging.Warnw(ctx, "Chain-control read for road_segment failed; road.chains omitted", "error", err)
		m.read, m.until = read, time.Now().Add(roadChainRetry)
		return read
	}
	m.read, m.until = newRoadChainRead(controls, quiet), time.Now().Add(layerTTL(LayerChainControl))
	return m.read
}
