package hazards

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dpup/prefab/logging"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/cache"
	"github.com/sierragridteam/sierra-data/internal/clients/caloes"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/clients/nws"
	"github.com/sierragridteam/sierra-data/internal/config"
)

// RoadsAPI / WeatherAPI are the narrow slices of the roads/weather services the
// surviving condition builders need — road_segment reads ListRoads, fire_weather
// reads ListWeather. Interfaces keep the package testable and let the /v1 grid
// API construct the service with fakes via NewServiceWithAPIs.
type RoadsAPI interface {
	ListRoads(context.Context, *api.ListRoadsRequest) (*api.ListRoadsResponse, error)
}
type WeatherAPI interface {
	ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error)
	// LocationForecasts returns per-location fire-weather forecasts (fail-soft;
	// nil/partial, never an error).
	LocationForecasts(context.Context) map[string]*nws.Forecast
}

// Service projects the roads/weather feeds into the unified GeoJSON hazard
// model. Since the /api/v1 hazards surface was removed, its only consumer is the
// /v1 grid API (internal/gridapi), which calls BuildLayer for the four
// condition layers (road_segment, chain_control, fire_weather, message_sign);
// the event layers are projected directly from the store by gridapi.
type Service struct {
	cfg      *config.Config
	roads    RoadsAPI
	weather  WeatherAPI
	caltrans *caltrans.FeedParser
	cache    *cache.Cache

	// signs and signDistricts back message_sign; set by UseMessageSigns.
	signs         MessageSignAPI
	signDistricts []int

	// history backs road_segment's statusSince/typicalMinutes; set by
	// UseRoadHistory once the store is open.
	history atomic.Pointer[roadHistoryHolder]

	// roadChains backs road_segment's road.chains: the last read of the
	// chain-control registry (roadChainRegistry).
	roadChains roadChainMemo

	// layerBuilders is derived once from layerRegistry() so the dispatch map has
	// one source of truth.
	layerBuilders map[string]builder
}

// NewServiceWithAPIs constructs the service with the roads/weather dependencies
// as their narrow interfaces, so consumers outside this package (the /v1 grid
// API) can construct it with fakes.
func NewServiceWithAPIs(cfg *config.Config, roads RoadsAPI, weather WeatherAPI, ct *caltrans.FeedParser, c *cache.Cache) *Service {
	s := &Service{
		cfg:      cfg,
		roads:    roads,
		weather:  weather,
		caltrans: ct,
		cache:    c,
	}
	reg := s.layerRegistry()
	s.layerBuilders = make(map[string]builder, len(reg))
	for _, e := range reg {
		s.layerBuilders[e.name] = e.build
	}
	return s
}

// builder produces a layer's features for an area. Returning an error makes the
// layer fail-loud (UNAVAILABLE metadata, empty features) rather than fabricating
// a clear state. A builder may return partialData(err) to keep its (incomplete)
// features while signalling STALE.
type builder func(ctx context.Context, area config.HazardArea) ([]Feature, error)

// layerEntry binds a layer name to its builder. layerRegistry is the single
// canonical list the dispatch map derives from.
type layerEntry struct {
	name  string
	build builder
}

func (s *Service) layerRegistry() []layerEntry {
	return []layerEntry{
		{LayerChainControl, s.chainControls},
		{LayerFireWeather, s.fireWeather},
		{LayerMessageSign, s.messageSigns},
		{LayerRoadSegment, s.roadSegments},
	}
}

// layerMetadata carries per-layer collection metadata.
type layerMetadata struct {
	attribution string
	sourceURL   string
}

func layerMeta(layer string) layerMetadata {
	switch layer {
	case LayerEvacuation:
		// Always carry the authoritative Genasys link + "reference only" framing,
		// in every state (OK/STALE/UNAVAILABLE) — a confirmed-empty is "no active
		// zones per Cal OES", never a guarantee.
		return layerMetadata{
			attribution: "Cal OES / California County Governments — reference only",
			sourceURL:   caloes.SourceURL,
		}
	default:
		return layerMetadata{}
	}
}

// layerResult is the outcome of building one layer: its features plus the
// fail-loud-adjusted source status and metadata.
type layerResult struct {
	features         []Feature
	status           string
	meta             layerMetadata
	lastSourceUpdate time.Time // when the underlying data was last fetched OK (STALE only)
}

// partialDataError signals a builder produced usable but incomplete data (e.g.
// one of several sources failed). buildLayer surfaces it as STALE and KEEPS the
// returned features, rather than UNAVAILABLE with empty features.
type partialDataError struct{ err error }

func (e *partialDataError) Error() string { return e.err.Error() }
func (e *partialDataError) Unwrap() error { return e.err }
func partialData(err error) error         { return &partialDataError{err} }

// layerTTL is the cache lifetime for a layer's upstream data, or 0 for layers
// that are already cached by the underlying roads/weather services (no
// double-caching). The new keyless upstreams + the live Caltrans chain-control
// fetch (CWWP2 + cc.kml) are cached here so a burst of map clients doesn't fan out to
// every source on every request, and so a transient upstream blip can fall back
// to the last good fetch (STALE) instead of going UNAVAILABLE.
func layerTTL(layer string) time.Duration {
	switch layer {
	case LayerEarthquake, LayerWildfire, LayerChainControl, LayerFireWeather, LayerMessageSign:
		return 5 * time.Minute
	case LayerEvacuation:
		return 2 * time.Minute // life-safety: short, so STALE fallback stays recent
	default:
		return 0
	}
}

// cacheEmptyResults reports whether a layer's CLEAN EMPTY successes may be
// cached. Non-empty successes are always cached; this governs only the "source
// is healthy and currently reports nothing" case.
//
// Caching an empty is a straight latency win for a layer that is empty most of
// the time — chain_control outside snow season re-parsed the Caltrans KML on
// every request for a 212-byte answer that never changed (36-49 ms each,
// measured). It is safe for the fail-loud contract because an upstream ERROR
// still refuses to serve a cached empty (see the len(stale) > 0 guard above).
//
// EVACUATION is deliberately excluded, and this is not an oversight to tidy up.
// The cost of caching an empty is that the FIRST evacuation order takes up to
// the TTL to appear, and that is the one transition on the whole service where
// delay is least acceptable. The layer accepts up to 2 minutes of staleness once
// zones exist, but keeps "nothing -> something" instant. It is also not a
// performance problem — Cal OES answers in ~1 ms here, so there is nothing to
// buy and something real to lose.
func cacheEmptyResults(layer string) bool { return layer != LayerEvacuation }

// buildLayer runs one layer's builder and applies the fail-loud rules uniformly.
//
// Status resolution:
//   - fresh cache hit            -> OK (served from cache, no upstream call)
//   - builder OK                 -> OK (and the non-empty result is cached)
//   - builder partialData(err)   -> STALE, features kept (one source degraded);
//     a non-empty partial is cached for the TTL, tagged so a hit stays STALE
//   - fresh cached partial       -> STALE + its fetch time (no upstream call)
//   - builder hard error + cache -> STALE, last good features served
//   - builder hard error, none   -> UNAVAILABLE, empty
//
// A clean empty success is OK with zero features — NOT UNAVAILABLE. The
// life-safety property is "an error never becomes a 0": UNAVAILABLE means the
// source genuinely failed (so a consumer shows "unknown / check the official
// source"), while OK + 0 means the source is healthy and currently reports
// nothing. The two are deliberately distinguishable.
func (s *Service) buildLayer(ctx context.Context, area config.HazardArea, layer string, build builder) layerResult {
	meta := layerMeta(layer)
	ttl := layerTTL(layer)
	key := "hazard:" + area.ID + ":" + layer

	cleanSource, partialSource := "hazard:"+layer, "hazard:"+layer+":partial"

	if ttl > 0 && s.cache != nil {
		var cached []Feature
		if entry, ok, derr := s.cache.GetWithMetadata(key, &cached); ok && derr == nil && time.Now().Before(entry.ExpiresAt) {
			// A cached partial result is still partial: it stays STALE for its
			// whole TTL, dated to when it was fetched — never promoted to OK.
			if entry.Source == partialSource {
				return finalize(meta, cached, "STALE", entry.CreatedAt)
			}
			return finalize(meta, cached, "OK", time.Time{})
		}
	}

	features, err := build(ctx, area)
	if err != nil {
		var pd *partialDataError
		if errors.As(err, &pd) {
			// Usable but incomplete — keep the features, flag STALE.
			logging.Warnw(ctx, "Hazard layer degraded (partial data)", "layer", layer, "area", area.ID, "error", err)
			// Cache it for the TTL, tagged partial, so a degraded upstream is
			// refetched at most once per TTL instead of on every request
			// (message_sign is 125KB+ per fetch). The tag keeps a cache hit
			// STALE; the next clean fetch overwrites it with an untagged entry.
			// Only cache when there is data: a partial with nothing in it has
			// nothing worth serving for a TTL.
			now := time.Now()
			if ttl > 0 && s.cache != nil && len(features) > 0 {
				_ = s.cache.Set(key, features, ttl, partialSource)
			}
			return finalize(meta, features, "STALE", now)
		}
		logging.Errorw(ctx, "Hazard layer build failed", "layer", layer, "area", area.ID, "error", err)
		// Stale-on-error: serve the last good fetch if we have one.
		//
		// `len(stale) > 0` is LOAD-BEARING, not a tidy-up. It is the single place
		// the "an error never becomes a 0" invariant is enforced now that clean
		// empties ARE cached (see the success path below). Drop it and an upstream
		// failure would replay a cached empty as though the source had reported
		// nothing — turning "status unknown" into a fabricated all-clear on a
		// life-safety layer. An error must fall through to UNAVAILABLE.
		if ttl > 0 && s.cache != nil {
			var stale []Feature
			if entry, ok, derr := s.cache.GetWithMetadata(key, &stale); ok && derr == nil && len(stale) > 0 {
				logging.Warnw(ctx, "Serving stale cached hazard layer after upstream failure",
					"layer", layer, "area", area.ID, "age", time.Since(entry.CreatedAt).String())
				return finalize(meta, stale, "STALE", entry.CreatedAt)
			}
		}
		return finalize(meta, nil, "UNAVAILABLE", time.Time{})
	}

	// Success. Cache it, INCLUDING a clean empty.
	//
	// Caching empties used to be forbidden here, to keep the safety property that
	// a later fetch error falls through to UNAVAILABLE rather than replaying a
	// stale "0". That property is real and still holds — but it is enforced on
	// the ERROR path above, which requires len(stale) > 0 before serving a cached
	// value, so an error can never be answered with a cached empty no matter what
	// is stored here. Refusing to WRITE the empty was belt-and-braces, and it cost
	// a full upstream fetch on every single request for any layer that is
	// legitimately empty most of the time.
	//
	// chain_control is exactly that layer: outside snow season it is empty by
	// definition, so it re-fetched the Caltrans KML on every /summary and every
	// chain_control.geojson — measured 36-49 ms per request, indefinitely, for a
	// 212-byte answer that never changes.
	//
	// The two states stay distinguishable, which is the whole contract: a fresh
	// cached empty is served as OK + 0 features ("source healthy, reports
	// nothing"), and it can only have been written by a SUCCESSFUL build. An
	// error still yields UNAVAILABLE + empty ("status unknown"). What is cached
	// is a success, not an absence of information.
	if ttl > 0 && s.cache != nil && (len(features) > 0 || cacheEmptyResults(layer)) {
		_ = s.cache.Set(key, features, ttl, cleanSource)
	}
	return finalize(meta, features, "OK", time.Time{})
}

// DegradeStoreStatus maps a store-derived source status onto the SERVED
// envelope status — the fail-loud table buildLayerFromStore documents. It is
// shared with the /v1 surface (internal/gridapi map layers and place summary)
// so the two endpoints can never disagree about identical store state:
//
//	OK                    -> OK, zero lastUpdate (freshness needs no caveat)
//	STALE                 -> STALE + lastUpdate
//	UNAVAILABLE + hasData -> STALE + lastUpdate: the store IS the last-good
//	                         cache, so a down source with stored data serves
//	                         stale data — never UNAVAILABLE hiding live data
//	UNAVAILABLE, no data  -> UNAVAILABLE, zero lastUpdate (the envelope never
//	                         carries last_source_update in this state)
//
// Any unrecognized status ranks as UNAVAILABLE — health unknown is not OK.
// The invariant preserved: UNAVAILABLE always means empty features, so a
// client that draws nothing on UNAVAILABLE never hides data the server sent.
func DegradeStoreStatus(status string, hasData bool, lastUpdate time.Time) (string, time.Time) {
	switch status {
	case "OK":
		return "OK", time.Time{}
	case "STALE":
		return "STALE", lastUpdate
	default:
		if hasData {
			return "STALE", lastUpdate
		}
		return "UNAVAILABLE", time.Time{}
	}
}

// finalize packages a built layer's result.
func finalize(meta layerMetadata, features []Feature, status string, lastUpdate time.Time) layerResult {
	return layerResult{features: features, status: status, meta: meta, lastSourceUpdate: lastUpdate}
}

// BuildLayer builds one layer for an area through the same fail-loud path the
// GeoJSON and /situation endpoints use (buildLayer + layerMeta), exported for
// the /v1 grid API's condition-backed map layers (road_segment, chain_control,
// fire_weather, message_sign). ok is false for an unknown layer; every other
// return mirrors the metadata block the shipped endpoints emit (status
// OK|STALE|UNAVAILABLE, lastSourceUpdate zero unless serving stale, per-layer
// attribution/sourceURL).
func (s *Service) BuildLayer(ctx context.Context, area config.HazardArea, layer string) (features []Feature, status string, lastSourceUpdate time.Time, attribution, sourceURL string, ok bool) {
	build, found := s.layerBuilders[layer]
	if !found {
		return nil, "", time.Time{}, "", "", false
	}
	res := s.buildLayer(ctx, area, layer, build)
	return res.features, res.status, res.lastSourceUpdate, res.meta.attribution, res.meta.sourceURL, true
}

// --- layer builders (re-project existing feeds) ---
// sortedChainControls is what Caltrans reported, sorted for the two readers
// of it (the map layer per area, and ChainRequirements for the stored
// events), so both group the same signs and number the same stretches.
type sortedChainControls struct {
	// active is every chain requirement Caltrans reported, anywhere: the
	// stretches are grouped and numbered over all of it, so an id means the
	// same stretch wherever it is read.
	active []caltrans.ChainControlData
	// points is everything else with a position: road closures, and cc.kml's
	// truck and hold levels (MAX, MIN, TS, HT, TTA). Not stretch members on
	// purpose. They are not car requirements, cc.kml gives no level we parse
	// and no end for them, and folding a "hold all traffic" into an R2
	// stretch would bury it.
	points []caltrans.ChainControlData
	// quiet is the checkpoints that end a direction's requirement: the ones
	// handed in plus every unrecognized one.
	quiet []caltrans.ChainControlData
	// unrecognized and uncorroborated name in-scope signs; unplaced names
	// every sign without a position (it cannot be ruled in or out of scope).
	unrecognized, unplaced, uncorroborated []string
}

// sortChainControls sorts the registry's controls. inScope says whether a
// sign is in the reader's scope (an area, or any area).
func sortChainControls(controls, quiet []caltrans.ChainControlData, inScope func(caltrans.ChainControlData) bool) sortedChainControls {
	out := sortedChainControls{quiet: quiet}
	for _, c := range controls {
		if c.Coordinates == nil {
			// Can't be ruled in or out of scope, so the reader can't claim
			// to be complete. (Only CWWP2 rows can lack a position.) A
			// requirement is still grouped: with a milepost it is listed
			// inside its stretch, which the signs around it place.
			out.unplaced = append(out.unplaced, strings.TrimSpace(c.Highway+" "+c.LocationName))
		}
		if c.Uncorroborated && inScope(c) {
			out.uncorroborated = append(out.uncorroborated, strings.TrimSpace(c.Highway+" "+c.Direction+" "+c.LocationName+" "+c.Level))
		}
		switch {
		case c.Unrecognized:
			// Neither a requirement nor an all-clear: keep it off the map, but
			// don't let the reader claim OK over a checkpoint we can't read.
			// It is still a sign: the requirement of the one before it in
			// its direction ends there.
			out.quiet = append(out.quiet, c)
			if inScope(c) {
				out.unrecognized = append(out.unrecognized, fmt.Sprintf("%s %s (%q)", c.Highway, c.LocationName, c.RawStatus))
			}
		case !c.Closed && isChainLevel(c.Level):
			out.active = append(out.active, c)
		case c.Coordinates != nil:
			out.points = append(out.points, c)
		}
	}
	return out
}

// ChainRead is one read of the chain-control registry for the stored events.
type ChainRead struct {
	// Stretches is every stretch in scope (see ChainRequirements), as the
	// map layer's feature for it.
	Stretches []Feature
	// SourceDown is set when one of the two sources failed and the read was
	// put together from the other (caltrans.PartialError). The stretches are
	// real, but their detail is the surviving source's: cc.kml alone names
	// signs differently, drops seconds and mileposts, and infers every
	// stretch. A reader that stores them must not take that as a change.
	SourceDown error
	// Incomplete is set when the read cannot vouch for completeness for
	// another reason (an in-scope sign with an unreadable status, or a sign
	// with no position). The stretches and their detail are good.
	Incomplete error
}

// Degraded is why a stretch missing from the read proves nothing, or nil
// when the read is complete.
func (r ChainRead) Degraded() error { return errors.Join(r.SourceDown, r.Incomplete) }

// ChainRequirements returns the chain-control stretches the stored events
// cover, whole, each as the map layer's feature for it (id, headline,
// severity and signs: chainStretchFeature). It groups over everything
// Caltrans reported, with the sorting and numbering the chain_control map
// layer uses, so one read gives a stretch the id the map gives it. It is its
// own read of Caltrans, not the map layer's cached one: the two agree per
// read, and the map can lag by its cache.
//
// A stretch is in scope when it has a sign inside one of areas, OR when
// held(id, signIDs, sourceDown) says the caller already holds an event for
// it (sourceDown: this read is from one source only, see ChainRead). The second
// half is what keeps a requirement from being recorded as lifted when it is
// not: a stretch's signs clear one at a time, and when the last sign inside
// an area clears while signs outside it still require chains, the stretch
// is still in force. Scope decides when a stretch STARTS being recorded;
// only Caltrans dropping it ends it. held may be nil.
//
// err is a failed read: nothing is known. A ChainRead with Degraded() nil
// and no stretches is Caltrans confirming there are none.
func (s *Service) ChainRequirements(ctx context.Context, areas []config.HazardArea, held func(id string, signIDs []string, sourceDown bool) bool) (ChainRead, error) {
	controls, quiet, err := s.caltrans.ParseChainControlRegistry(ctx)
	var partial *caltrans.PartialError
	if err != nil && !errors.As(err, &partial) {
		return ChainRead{}, err
	}
	inScope := func(c caltrans.ChainControlData) bool {
		if c.Coordinates == nil {
			return false
		}
		for _, a := range areas {
			if a.Bounds.Contains(c.Coordinates.Latitude, c.Coordinates.Longitude) {
				return true
			}
		}
		return false
	}
	sorted := sortChainControls(controls, quiet, inScope)
	grouped := groupChainStretches(sorted.active, sorted.quiet)
	var read ChainRead
	for i, st := range grouped {
		id := chainStretchID(grouped, i)
		if !anySign(st.Signs, inScope) {
			if held == nil {
				continue
			}
			signIDs := make([]string, len(st.Signs))
			for j, c := range st.Signs {
				signIDs[j] = chainControlID(c)
			}
			if !held(id, signIDs, partial != nil) {
				continue
			}
		}
		if f, ok := chainStretchFeature(st, id); ok {
			read.Stretches = append(read.Stretches, f)
		}
	}
	if partial != nil {
		read.SourceDown = partial
	}
	var problems []error
	if len(sorted.unrecognized) > 0 {
		problems = append(problems, fmt.Errorf("unrecognized chain-control status at %s", strings.Join(sorted.unrecognized, "; ")))
	}
	if len(sorted.unplaced) > 0 {
		problems = append(problems, fmt.Errorf("chain-control entries without a position: %s", strings.Join(sorted.unplaced, "; ")))
	}
	read.Incomplete = errors.Join(problems...)
	return read, nil
}

func (s *Service) chainControls(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	controls, quiet, err := s.caltrans.ParseChainControlRegistry(ctx)
	var partial *caltrans.PartialError
	if err != nil && !errors.As(err, &partial) {
		return nil, err
	}
	inArea := func(c caltrans.ChainControlData) bool {
		return c.Coordinates != nil && area.Bounds.Contains(c.Coordinates.Latitude, c.Coordinates.Longitude)
	}
	sorted := sortChainControls(controls, quiet, inArea)
	active, points, quiet := sorted.active, sorted.points, sorted.quiet
	unrecognized, unplaced, uncorroborated := sorted.unrecognized, sorted.unplaced, sorted.uncorroborated

	var out []Feature
	stretches := groupChainStretches(active, quiet)
	for i, st := range stretches {
		// A stretch is on the map of every area it has a sign in, whole: the
		// same id, extent and signs wherever it is read.
		if anySign(st.Signs, inArea) {
			if f, ok := chainStretchFeature(st, chainStretchID(stretches, i)); ok {
				out = append(out, f)
			}
		}
	}
	gates := seasonalGates(s.cfg)
	for _, c := range dedupeChainSigns(points) {
		if !inArea(c) {
			continue
		}
		attribution := c.Source
		if attribution == "" {
			attribution = caltrans.SourceQuickMap
		}
		category, headline := strings.ToLower(c.Level), strings.TrimSpace(c.Highway+" chain control "+c.Level)
		if c.Closed {
			category, headline = "closed", strings.TrimSpace(c.Highway+" road closed")
		}
		p := Properties{
			ID:           chainControlID(c),
			Layer:        strings.ToUpper(LayerChainControl),
			Kind:         "Chain control",
			Category:     category,
			Headline:     headline,
			Description:  c.Description,
			AreaLabel:    c.LocationName,
			Effective:    c.EffectiveTime,
			Source:       Source{ID: "caltrans", Name: "Caltrans", Attribution: attribution},
			ChainControl: &ChainControlProps{Level: c.Level, Highway: c.Highway, Direction: c.Direction},
		}
		p.setSeverity(fromChainLevelStr(c.Level))
		if c.Closed {
			// One notch above a clear checkpoint: a closed road is worth seeing
			// on the map. Kept below R-1 because the seasonal pass gates hold
			// this state all winter. Note /summary's roads domain counts
			// anything above INFO as active, so an in-area gate shows there too.
			p.setSeverity(SevMinor)
			// A gate of a named seasonal closure is the road's yearly state,
			// not today's news: it says so, carries the closure's name and
			// is INFO, as road.beyond[] is on the road_segment layer. So it
			// no longer counts as active in /summary's roads domain.
			if name := seasonalClosureOf(c, gates); name != "" {
				p.ChainControl.Seasonal, p.ChainControl.Closure = true, name
				p.Headline = strings.TrimSpace(c.Highway + ": " + name)
				p.setSeverity(SevInfo)
			}
		}
		out = append(out, Feature{Type: "Feature", Geometry: PointGeom(c.Coordinates.Latitude, c.Coordinates.Longitude), Properties: p})
	}
	var degraded []error
	if partial != nil {
		degraded = append(degraded, partial)
	}
	if len(unrecognized) > 0 {
		degraded = append(degraded, fmt.Errorf("unrecognized chain-control status at %s", strings.Join(unrecognized, "; ")))
	}
	if len(unplaced) > 0 {
		degraded = append(degraded, fmt.Errorf("chain-control entries without a position: %s", strings.Join(unplaced, "; ")))
	}
	if len(uncorroborated) > 0 {
		// Shown (the safe side), not degraded: the evidence for issue #12.
		logging.Warnw(ctx, "cc.kml reports chain controls CWWP2 does not", "area", area.ID, "controls", uncorroborated)
	}
	if len(degraded) > 0 {
		return out, partialData(errors.Join(degraded...))
	}
	return out, nil
}

// chainControlID is "cc:<district>-<message id>": Caltrans numbers cc.kml
// messages per district, so the id alone repeats across districts. A CWWP2
// checkpoint index already starts with its district ("10-17") and is kept as
// it is. A row missing either part is identified by its position.
func chainControlID(c caltrans.ChainControlData) string {
	district, id := strings.TrimSpace(c.District), strings.TrimSpace(c.MessageID)
	switch {
	case (district == "" || id == "") && c.Coordinates == nil:
		return "cc:" + district + "-" + id
	case district == "" || id == "":
		return fmt.Sprintf("cc:%.5f,%.5f", c.Coordinates.Latitude, c.Coordinates.Longitude)
	case c.Source == caltrans.SourceCWWP2 && strings.HasPrefix(id, district+"-"):
		return "cc:" + id
	}
	return "cc:" + district + "-" + id
}

func (s *Service) roadSegments(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	resp, err := s.roads.ListRoads(ctx, &api.ListRoadsRequest{})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*api.Road, len(resp.GetRoads()))
	for _, rd := range resp.GetRoads() {
		byID[rd.GetId()] = rd
	}

	// The chain-control registry, read once for the request and only if a
	// road in this area has a path to place signs on.
	chainsOn := s.RoadChainsOn(ctx)

	var out []Feature
	for _, mr := range s.cfg.Roads.MonitoredRoads {
		// Include the segment if either endpoint is in the area, and always on
		// the segment's own corridor (whose synthesized area id is the road id).
		// The corridor's bbox comes from its 5-decimal road path, which can sit
		// a few metres inside the configured endpoints, so the endpoint test
		// alone dropped every corridor's own segment from its map and summary.
		if mr.ID != area.ID &&
			!area.Bounds.Contains(mr.Origin.Latitude, mr.Origin.Longitude) &&
			!area.Bounds.Contains(mr.Destination.Latitude, mr.Destination.Longitude) {
			continue
		}
		rd := byID[mr.ID]

		// Draw the segment along the actual route (the decoded Google polyline
		// exposed on Road.polyline) so it follows the highway; fall back to a
		// straight origin→destination line when the polyline is unavailable.
		var geom *Geometry
		if rd != nil && len(rd.GetPolyline()) >= 2 {
			pts := make([]LatLng, len(rd.GetPolyline()))
			for i, c := range rd.GetPolyline() {
				pts[i] = LatLng{Lat: c.GetLatitude(), Lng: c.GetLongitude()}
			}
			geom = LineStringGeom(pts)
		}
		if geom == nil {
			geom = LineStringGeom([]LatLng{
				{Lat: mr.Origin.Latitude, Lng: mr.Origin.Longitude},
				{Lat: mr.Destination.Latitude, Lng: mr.Destination.Longitude},
			})
		}

		p := Properties{
			ID:        "road:" + mr.ID,
			Layer:     strings.ToUpper(LayerRoadSegment),
			Kind:      "Road segment",
			Headline:  strings.TrimSpace(mr.Name + " — " + mr.Section),
			AreaLabel: mr.Section,
			Source:    Source{ID: "google", Name: "Google Routes + Caltrans", Attribution: "Google · quickmap.dot.ca.gov"},
			Road:      &RoadProps{RoadID: mr.ID},
		}
		sev := SevInfo
		if rd != nil {
			p.Status = strings.TrimPrefix(rd.GetStatus().String(), "ROAD_STATUS_")
			p.Road.Congestion = strings.TrimPrefix(rd.GetCongestionLevel().String(), "CONGESTION_LEVEL_")
			p.Road.DelayMinutes = i32ptr(rd.GetDelayMinutes())
			p.Road.DurationMinutes = i32ptr(rd.GetDurationMinutes())
			p.Road.DistanceKm = i32ptr(rd.GetDistanceKm())
			sev = roadSeverity(rd)
			if e := rd.GetStatusExplanation(); e != "" {
				p.Description = e
			}
			s.addRoadHistory(ctx, p.Road, rd, time.Now())
			p.Road.StatusCauses = roadStatusCauses(rd, pathOf(rd, mr))
			p.Road.Beyond = roadBeyond(rd, mr)
			// Not known: no `chains` key, never an empty list.
			if chains, known := chainsOn(mr, rd); known {
				p.Road.Chains = &chains
			}
		}
		p.setSeverity(sev)
		out = append(out, Feature{Type: "Feature", Geometry: geom, Properties: p})
	}
	return out, nil
}
func (s *Service) fireWeather(ctx context.Context, area config.HazardArea) ([]Feature, error) {
	resp, err := s.weather.ListWeather(ctx, &api.ListWeatherRequest{})
	if err != nil {
		return nil, err
	}
	var out []Feature

	// Banner: the region's ISSUED state (per-zone), colored by severity. Only for
	// areas whose zones the product covers.
	if fw := resp.GetFireWeather(); fw != nil && zonesMatch(area.Zones, fw.GetZones()) {
		state := strings.ToLower(strings.TrimPrefix(fw.GetState().String(), "FIRE_WEATHER_STATE_"))
		state = strings.ReplaceAll(state, "_", "-")
		p := Properties{
			ID:        "fw:region",
			Layer:     strings.ToUpper(LayerFireWeather),
			Kind:      "Fire weather",
			Category:  state,
			Headline:  nonEmpty(fw.GetHeadline(), "Fire weather: "+state),
			Effective: tsToRFC3339(fw.GetEffective()),
			Expires:   tsToRFC3339(fw.GetExpires()),
			Source:    Source{ID: "nws", Name: nonEmpty(fw.GetSenderName(), "National Weather Service"), Attribution: "NOAA / National Weather Service"},
			FireWeather: &FireWeatherProps{
				State:       state,
				SourceEvent: fw.GetSourceEvent(),
				Zones:       fw.GetZones(),
			},
		}
		p.setSeverity(fromFireWeatherState(state))
		out = append(out, Feature{Type: "Feature", Geometry: nil, Properties: p}) // null geometry = banner
	}

	// Per-location forecast Points: geolocated wind/RH outlook, INFO severity —
	// informational, never an issued warning (see docs/design/fire-weather-forecast-design.md).
	// Fail-soft: LocationForecasts never errors, so a forecast outage just omits points.
	if forecasts := s.weather.LocationForecasts(ctx); len(forecasts) > 0 {
		for _, loc := range s.cfg.Weather.Locations {
			if !area.Bounds.Contains(loc.Coordinates.Latitude, loc.Coordinates.Longitude) {
				continue
			}
			if f := forecasts[loc.ID]; f != nil {
				out = append(out, forecastPointFeature(loc, f))
			}
		}
	}
	return out, nil
}

// forecastPointFeature builds the INFO-severity Point for a location's forecast
// summary. Severity is always INFO — a windy/dry forecast never colors like an
// issued Red Flag.
func forecastPointFeature(loc config.WeatherLocation, f *nws.Forecast) Feature {
	gust := int32(math.Round(f.PeakGustKmh))
	summary := &ForecastSummary{
		Source:          f.Source,
		IssuedAt:        rfc3339Time(f.IssuedAt),
		HorizonHours:    f.HorizonHours,
		PeakWindGustKmh: gust,
		PeakWindGustAt:  rfc3339Time(f.PeakGustAt),
	}
	if f.HasMinHumidity {
		summary.MinHumidityPercent = int32(math.Round(f.MinHumidityPct))
	}
	p := Properties{
		ID:        "fw:forecast:" + loc.ID,
		Layer:     strings.ToUpper(LayerFireWeather),
		Kind:      "Fire-weather forecast",
		AreaLabel: loc.Name,
		Headline:  fmt.Sprintf("%s forecast — peak gust %d km/h", loc.Name, gust),
		Source:    Source{ID: "nws", Name: "National Weather Service", Attribution: "NOAA / National Weather Service"},
		FireWeather: &FireWeatherProps{
			Forecast: summary,
		},
	}
	p.setSeverity(SevInfo)
	return Feature{Type: "Feature", Geometry: PointGeom(loc.Coordinates.Latitude, loc.Coordinates.Longitude), Properties: p}
}

// rfc3339Time renders a time as RFC 3339 UTC, "" for the zero value.
func rfc3339Time(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// normFireName normalizes an incident/perimeter name for joining CAL FIRE
// incidents and FIRIS perimeters (e.g. "Salt Springs Fire" and "Salt Springs" →
// "saltsprings").
func normFireName(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSuffix(strings.TrimSpace(s), " fire")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// roadSeverity derives a unified severity from a road's status + congestion.
func roadSeverity(rd *api.Road) string {
	switch rd.GetStatus() {
	case api.RoadStatus_CLOSED:
		return SevSevere
	case api.RoadStatus_RESTRICTED, api.RoadStatus_MAINTENANCE:
		return SevModerate
	}
	switch rd.GetCongestionLevel() {
	case api.CongestionLevel_SEVERE, api.CongestionLevel_HEAVY:
		return SevModerate
	case api.CongestionLevel_MODERATE:
		return SevMinor
	default:
		return SevInfo
	}
}

func tsToRFC3339(ts interface{ GetSeconds() int64 }) string {
	// Accept any *timestamppb.Timestamp via its getter; nil-safe.
	if ts == nil {
		return ""
	}
	secs := ts.GetSeconds()
	if secs == 0 {
		return ""
	}
	return time.Unix(secs, 0).UTC().Format(time.RFC3339)
}

// i32ptr returns a pointer to v (for optional JSON numerics).
func i32ptr(v int32) *int32 { return &v }

// zonesMatch reports whether an alert belongs to an area, by NWS forecast zone:
//   - area has no configured zones    -> matches (unscoped single-area deployment)
//   - alert has no zones (e.g. OWM)   -> matches (can't be scoped; keep it)
//   - otherwise                       -> the zone sets intersect
func zonesMatch(areaZones, alertZones []string) bool {
	if len(areaZones) == 0 || len(alertZones) == 0 {
		return true
	}
	set := make(map[string]bool, len(areaZones))
	for _, z := range areaZones {
		set[z] = true
	}
	for _, z := range alertZones {
		if set[z] {
			return true
		}
	}
	return false
}
func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// pathOf is the segment's drawn path in internal order: the route polyline,
// or the straight origin-destination fallback.
func pathOf(rd *api.Road, mr config.MonitoredRoad) []LatLng {
	if len(rd.GetPolyline()) >= 2 {
		pts := make([]LatLng, len(rd.GetPolyline()))
		for i, c := range rd.GetPolyline() {
			pts[i] = LatLng{Lat: c.GetLatitude(), Lng: c.GetLongitude()}
		}
		return pts
	}
	return []LatLng{{Lat: mr.Origin.Latitude, Lng: mr.Origin.Longitude}, {Lat: mr.Destination.Latitude, Lng: mr.Destination.Longitude}}
}
