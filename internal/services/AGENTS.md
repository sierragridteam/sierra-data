# gRPC Services

These implement the proto services in `api/v1`. They orchestrate clients,
caching, route classification, and AI enhancement.

| File              | Responsibility |
|-------------------|----------------|
| `roads.go`        | `RoadsService`: per-road traffic, alerts, status, chain control. |
| `incidents.go`    | `RoadsService.ListIncidents`: region-wide CHP/Caltrans incident feed. |
| `weather.go`      | `WeatherService`: current conditions + combined alerts list. |
| `weather_nws.go`  | NWS zone alerts + fire-weather classification for `WeatherService`. |
| `roads_lane_closures.go` | Lane closures behind per-road status (CWWP2 or `lcs2way.kml`). |
| `roads_status_inputs.go` | CHP incidents and road conditions behind per-road status: last good read, `RoadInputHealth`. |
| `roads_on_highway.go` | `chpIncidentOnHighway`: when a CHP incident near a road may set its status. |
| `periodic_refresh.go` | Background goroutine that warms the roads cache. |
| `cameras.go`      | `CameraService`: the Caltrans CCTV directory behind `GET /api/v1/cameras`. Own background refresh, not the TTL cache — see below. |

## Caching model (read this before adding an endpoint)

Every read endpoint follows the same shape:

1. `Get(key, &dst)` — serve **fresh** data only (`Get` returns found=false for
   stale entries).
2. On miss/stale, refresh from upstream, then `Set(key, data, ttl, kind)`.
3. On refresh failure, fall back to stale cache via `GetWithMetadata(key, &dst)`
   — the accessor that returns stale entries — gated by `!IsVeryStale(key)`
   (2× the refresh interval). **Don't use `Get` in a stale-fallback branch; it
   can never return stale data** (this exact bug made the weather fallbacks
   dead code until 2026-07).

Staleness tiers: fresh (< TTL) → servable-stale (< 2×TTL) → very stale
(evicted by the hourly cleanup goroutine started in `cmd/server/main.go`).

The cache is in-memory JSON (TTL-based), so any value must be JSON-serializable
(this is why `nws.Alert` uses exported fields). TTLs: API data ~5–15m,
AI-enhanced alerts 24h (keyed by content hash to dedupe OpenAI calls).

Roads AND current weather conditions are kept warm by `periodic_refresh.go`;
incidents still refresh lazily on request.

`weather:all` was the last major cache refreshing lazily, and its TTL equals the
refresher's interval — so every time it expired, the next USER REQUEST paid the
full 7-location OpenWeather fan-out plus the NWS fire-weather computation. That
is a multi-second cliff landing on whoever asked first, on a 15-minute cadence.
`refreshCacheData` now calls `ListWeather` (the public method, so cache key and
TTL cannot drift; a fresh hit is a no-op). It makes the documented worst case the
actual case: 7 locations x 96 ticks/day = 672 calls/day, every day. Adding
`weather.locations` raises that linearly.

**Forecasts are warmed first at process start, on their own goroutine**
(`warmForecastsAtStart`). The request path never fetches NWS, so a new process
has no forecast until the refresher warms it, and the refresher reaches the
forecasts only after the road refresh, which takes minutes. Until 2026-11-19
`/api/v1/conditions` answered `forecast: []` for minutes after every deploy
(Grid's Overview forecast chart test failed whenever it ran in that window).
The first tick then finds them fresh and skips them.

A failure in one upstream must not abandon the rest of the tick — roads and
weather are independent, so `refreshCacheData` logs and continues rather than
returning early.

Google Routes has a separate 20-minute cache (`google_routes_<id>`) to stay
within the monthly API budget — adding monitored roads increases that load.

**The camera directory is the one exception to the model above.**
`CameraService` refreshes every district in the background (`Run`, 6h; 5 min
after a failure) and keeps each district's last good list for as long as a
failure lasts, served as `STALE` with its fetch time. It never evicts at 2×TTL.
The camera list is near-static (edited weeks to months apart), so last week's
list is still right, and the images it points at are live regardless. Evicting
it would turn a portal blip into "no cameras". Out-of-service cameras and rows
with no position or https image are dropped there; the geography filter lives in
`internal/gridapi/cameras.go`, which has the place directory.

## Adding a new endpoint

1. Add the RPC + messages to the relevant `.proto`, then `make proto`
   (see root AGENTS.md for the toolchain — Go/protoc are not pre-installed).
2. Implement the method on the existing service struct (the gateway wiring in
   `cmd/server/main.go` is already registered per-service, so new RPCs on an
   existing service need no extra registration).
3. Request fields map automatically: fields named in the path template are path
   params (`/incidents/{area}` → `ListIncidentsRequest.area`), the rest become
   query params (`?zones=` → repeated `zones`). Convention: path params identify
   a resource (road/location/area id); query params filter a collection.
4. Add focused unit tests next to the file (construct inputs directly; don't hit
   the network).

## Region-wide incidents (`incidents.go`)

Surfaces the same Caltrans/CHP data as road alerts, but as a flat list scoped by
a configured bounding box (`roads.incidentAreas`) instead of per-route. Its only
caller is the grid's road-incident poller. Parsing of log number / type /
location / time is done structurally from the KML description. See
`internal/clients/AGENTS.md` for the 2026 feed-format caveat.

**With CWWP2 lane closures configured (`laneClosureDistricts`), `refreshIncidents`
does not read `lcs2way.kml` at all**: the grid's `LaneClosureNormalizer` owns
closures, and reading the KML here would only spend enhancement budget on
incidents nobody stores. CHP is then the only feed, so a CHP failure fails the
refresh, and `IncidentFeedHealth`'s `laneErr` stays nil.

## Per-road status closures (`roads_lane_closures.go`)

Per-road status (`refreshRoadData`, behind the `road_segment` layer) takes its
lane closures from `segmentLaneClosures`. With `laneClosureDistricts` empty that
is `lcs2way.kml`, fetch errors swallowed, as before. With them set it is the
CWWP2 windows that are **set up right now** (`PhaseAt == PhaseActive`) — the
same rows the `road_incident` layer shows as ACTIVE, so a segment's status and
the closure events agree. SCHEDULED windows never count; `Unrecognized` rows are
logged and skipped. `caltrans.IncidentFromCWWP2LaneClosure` reshapes each window
into the `CaltransIncident` lcs2way.kml produced, so route matching, the
Closure-ID alert id and the AI `road_status` call are unchanged. Its text is
built only from fields fixed for the window's life: it is the enhancement cache
key, so a time-varying field would cost an OpenAI call per closure per refresh.

**Fail-loud, unlike the KML path.** Any district failing fails the set (a missing
district reads as "no closures", so OPEN, across its footprint). The last good
ACTIVE set (`cwwp2_lane_closures:active`) is served while servable-stale
(< 2x `roads.refreshInterval`); past that `refreshRoadData` errors, and the
periodic refresher keeps the last good `roads:all` instead of publishing a guessed
OPEN. Budget: one fetch per district per roads refresh (15m), the cadence
lcs2way.kml had; it uses the shared `cwwp2.Client` from `cmd/server`
(`UseCWWP2LaneClosures`), the one the lane-closure poller also uses.

## A status input that fails (`roads_status_inputs.go`)

Per-road status has three inputs that say "closed": lane closures (above),
CHP incidents (`chp-only.kml`) and road conditions (`roads.dot.ca.gov`, one
page per highway). **None of them may read as "nothing reported" when its
fetch failed**, because nothing reported is OPEN. Until 2026-11-19 the last
two did: `refreshRoadData` dropped the CHP error and logged the conditions
one. On 2026-11-18 `road:hwy4-arnold-bearvalley` went CLOSED, OPEN, CLOSED
for five hours (revisions 4 to 9) at 15 to 60 minute spacing. Each OPEN had
lost the Ebbetts Pass `beyond` closure too, which Caltrans listed throughout:
the conditions read had returned nothing for one to four refreshes in a row.
There are no logs from that day, so whether the reads failed by HTTP error,
timeout or a 200 with some other page is not known; all three are handled.

`readStatusInput` gives both the lane-closure treatment:

- A good read is cached: `chp_incidents:last_good` (the raw statewide set,
  TTL `roads.refreshInterval`) and `road_conditions:<highway number>` (TTL
  half of `roadConditionsServableFor`, so servable for two hours).
  **A page with no conditions is a good read and is cached as one** (Hwy 49
  usually has none). A page that is not a conditions page is an error in the
  client (`caltrans.ErrNotConditionsPage`).
- A failed read serves the last good one while it is servable. Past that,
  or with nothing cached, `refreshRoadData` returns an error and the
  periodic refresher keeps the last good `roads:all`; the road-segment
  poller then holds the events once `roads:all` is itself older than 2x the
  interval.
- **The two windows differ on purpose.** CHP incidents turn over in minutes:
  2x `roads.refreshInterval`, the lane-closure window (with a 15m refresh,
  one failed refresh runs on the last good set and the next usually fails).
  Road conditions are paragraphs Caltrans edits a few times a day at most,
  and on 2026-11-18 the page was unreadable for up to an hour at a time:
  `roadConditionsServableFor`, two hours. With the short window an hour's
  outage of that one page would have stopped every road's travel times and
  then emptied the road layer, in a storm. The cost: a closure Caltrans
  lifts during an outage stays up to two hours (the error this code
  prefers), and the `roads` source says the read is old the whole time.
- `chp_incidents:last_good` is written by both CHP readers:
  `refreshRoadData` and `refreshIncidents` (the incidents feed, on its own 5m
  cadence), so the fallback is as fresh as the newer of the two. Neither
  reads the other's fetch in place of its own, and no fetch was added: one
  `chp-only.kml` and one conditions page per highway per roads refresh, as
  before. `ListIncidents` keeps its own per-area fallback and
  `IncidentFeedHealth`.

**`RoadInputHealth()` is how anyone finds out** (the shape of
`IncidentFeedHealth`): the inputs that failed at the last refresh, each with
the time its run of failures began, the latest error and whether the last
good read was used. `refreshRoadData` records it before it returns, failed or
not, and reads every input before failing on any. The road-segment poller
puts `RoadInputFailure.Describe` on the `roads` source:

    road conditions for Hwy 4 unavailable since 17:05Z; last good read used (HTTP error 503 ...)
    CHP incidents unavailable since 17:05Z; no last good read to use, road status not refreshed (...)

Lane closures are not in it: a CWWP2 failure already degrades the `caltrans`
source through the lane-closure poller, and the `lcs2way.kml` path (no
`laneClosureDistricts`) still swallows its errors. Chain controls
(`ParseChainControlsDetailed`) are logged and tolerated as before; they set
`chain_control`, never a road's status.

## A CHP incident sets a road's status only on the highway (`roads_on_highway.go`)

The route matcher (`internal/lib/routing`) calls any alert within 100 m of a
road's line ON_ROUTE, and an ON_ROUTE alert the model reads as "closed" or
"restricted" sets the road's status and is a cause. For CHP incidents 100 m
takes in the roads beside the highway. On 2026-11-18
`road:hwy49-angels-sonora` read RESTRICTED, then CLOSED, for 75 minutes
(revisions 6 and 7) on CHP incident 261118MR0292, "21151 Old Sonora Columbia
Rd": a power pole down on that county road, 82 m from the Hwy 49 line. The
five CHP incidents measured that afternoon that were on the monitored
highways ("Sr49 / Reynolds Ferry Rd", "Sr4 / Avery Sheep Ranch Rd", ...) were
1 to 7 m from the line.

`chpIncidentOnHighway(locationText, highwayNumber, distanceMeters)` is the
whole rule, applied in `processGlobalAlerts` to a CHP incident the matcher
put ON_ROUTE. It stays ON_ROUTE only when EITHER

- **its location text names the road's highway number as a route**
  (`namesHighway`): "Sr49", "SR 49", "SR-49", "State Route 49", "Hwy 49",
  "Highway 49", "CA-49", and "I5" / "I-5" / "Us50" / "US 50" for those. The
  number is a whole token (4 is not in "Sr49"). The text is **CHP's raw
  location line** from the KML description (`parseIncidentDetail(...).location`),
  never the model's `location.description`, which rewrites it; OR
- **its point is within 25 m of the road's line** (`chpOnHighwayMeters`).

Otherwise it is downgraded to NEARBY: still an alert on the road, no status,
no cause. Rules of use:

- It only narrows ON_ROUTE. An incident farther than 100 m is never promoted
  by its text: Hwy 49 outside the monitored section names the route too.
- The road's number comes from `extractHighwayNumber(road name)`. California
  numbers its routes once across SR, US and I, so the prefix is not compared.
- Lane closures are not put through it (CWWP2 and `lcs2way.kml` rows carry
  their route), nor are roads.dot.ca.gov conditions.
- **The trade-off, accepted:** a CHP incident on the highway whose location
  is a street address that does not name the route and whose point is more
  than 25 m off the line reads as NEARBY. And the other way: a road named
  after the highway ("Old Hwy 49") or a frontage road within 25 m still
  counts. Change the threshold or the pattern in this one function, with a
  measured case.
- Without a Google polyline the road's line is the straight line between its
  ends, and both distances are taken to that, as the matcher's always was.

## Incident enhancement, de-duplication and ids (`incidents.go`)

**Every incident is AI-enhanced** (`enhanceIncident`), via the same
content-hash 24h cache as road alerts (`enhanceRawAlert` in `roads.go` —
shared, so an incident that is also a road alert costs one OpenAI call).
`severity` is driven by the model's impact assessment (`severityFromImpact`,
mirroring the roads mapping); the keyword heuristic (`incidentSeverity`) is
only the placeholder until enhancement lands. Cache-miss calls are capped at
`maxIncidentEnhancementsPerRefresh` per refresh to bound latency/cost;
deferred incidents pick up enhancement on a later refresh. Enhancement is
strictly additive: failures keep the structural fields and heuristic severity.
The model is `openai.model` in prefab.yaml (gpt-5-mini); the enhancer handles
gpt-5-family param differences (no temperature, `max_completion_tokens`,
`reasoning_effort=low`) — see `internal/lib/alerts/enhancer.go`.

Each incident normalizes to the same primitives the other APIs use (shared
`AlertType`/`AlertSeverity` enums, `Coordinates`, `google.protobuf.Timestamp`,
`location_description`). `normalizeIncidents` then keeps the list clean:

- **Drops geometry-only placemarks.** The lane-closure feed emits a separate
  LineString "path" placemark per closure with no description — skipped by the
  empty-description check.
- **Dedupes by `id`.** A closure is published twice in `lcs2way` — once per
  endpoint — so the pair collapses to one incident. The survivor is chosen
  deterministically (southernmost/westernmost, `southWestOf`), NOT "first
  wins": the endpoints are ~2.5 km apart and the stored location is part of the
  grid event's content hash, so feed-order-dependent selection made geometry
  flip-flop and mint a revision every time Caltrans reordered its rows.

**The id is `incidentID`, and getting it wrong is expensive.** `Closure ID` is a
route-level PROJECT id — on a live feed 593 rows carried only 271 of them, and
`C99CB` alone covered 16 unrelated Route 99 ramps. Since the id is also the dedup
key above, 15 of those 16 were dropped every poll and the survivor varied with
feed order, so a single grid event's history walked 30 km. The key is
`(Closure ID, Log Number, location text)`; the text is the discriminator that
separates closures sharing the first two, and the endpoint pair shares all three
by design. CHP is unaffected — its log number is genuinely unique. Ids are
namespaced per feed (`chp:` / `caltrans:`) so they agree with
`provenance.sourceId`.

**Enhancement is GROUNDED — the model may not invent geography.** `RawAlert`
carries a `PlaceNames` list (`nearbyPlaceNames`, `nearby_places.go`), built from
config alone — no store, no network, because this service runs upstream of the
grid store and the place directory is unreachable here. It offers configured
towns, monitored corridors and the settlements along them **within 15 km**,
closest first, with coordinate-known places ranked above corridor keywords
(a keyword has no coordinates of its own). The system prompt then forbids naming
any locality outside that list.

**The distance cap is the load-bearing part, and an empty list is a finding.**
Without a cap, grounding just trades a wrong place for a less-wrong one: a
collision at Sonora Pass was labelled "(near Merced)" — 134 km away, from a CHP
dispatch-centre token — and the nearest configured town is still 52.8 km off.
An empty list is serialized explicitly as `[]` (never omitted) and means "we
looked and nothing is near", at which point the model must name nothing and the
feed's own route-and-cross-street text survives.

`PlaceNames` is deliberately NOT in the content hash — it is a deterministic
function of coordinates already inside the hashed `location`, so including it
would dump the cache on any config edit. The PROMPT version is in the hash, so
a prompt fix invalidates cached enhancements instead of serving pre-fix text for
the 24h TTL.

CHP incidents carry a `started` time; lane closures are scheduled operations with
no dispatch time, so their `started` is null (expected, not a bug).

## Weather alerts & fire weather

`ListWeatherAlerts` returns authoritative **NWS** zone alerts only (source
`NWS`). `?zones=CAZ064,...` filters to alerts in those zones. Per-location
`weather_data[].alerts` are the NWS alerts for that location's configured
`zone` (see `nwsAlertsForZone` in `weather_nws.go`). OpenWeatherMap One Call
alerts were removed 2026-07-04: for US locations they duplicate NWS, and the
One Call 3.0 endpoint's 1,000 calls/day cap was being exceeded — don't
reintroduce per-location One Call fetches. OpenWeather now serves current
conditions only (`/data/2.5/weather`, one call per location per refresh).

`fire_weather` is **region-wide** (NWS fire-weather products are issued by zone,
not point), so it lives on the response (`ListWeatherResponse` /
`GetLocationWeatherResponse`) computed once from the configured `weather.nws.zones`
— not duplicated on every location.

## Road conditions and seasonal closures

roads.dot.ca.gov conditions are text with no geometry. `applyRoadConditions`
attaches one to a monitored road when its text names one of the road's ends or
`locationKeywords` (`caltrans.MatchConditionToSegment`), and it then sets the
road's status or chain control. That is deliberately crude and errs towards
closed: "Lake Alpine", "Mt Reba" and "Ebbetts" are keywords of Arnold to Bear
Valley although they are past Bear Valley.

The one exception is a **named seasonal closure** (`seasonalClosures` on the
monitored road in prefab.yaml; a list). The Ebbetts Pass winter closure is at
the same place every year and starts where Arnold to Bear Valley ends, so it
is named rather than inferred. `caltrans.MatchSeasonalClosure` accepts a
condition only when the WHOLE text, case and spacing aside, reads

    [Is] closed from [N mi <dir> of] <start> to <end> [(... Co)] [24 hrs a day]
    [- Due to snow|ice|weather...] [- For the winter|season]
    [- Motorists are advised to use an alternate route][.]

- it is a closure (not a chain requirement, restriction or notice);
- the start is made of the configured `start` names and nothing else, bare or
  offset by at most `maxStartOffsetMiles` in `startOffsetDirection` (the
  direction away from the road: "0.5 mi east of ...");
- the end is made of the configured `end` names and nothing else, no offset;
- what follows the end is only the tail above (`seasonalClosureTail`, a
  whitelist with a fixed list of reasons). It is a whitelist because the
  places a trailing sentence might name are not all known: "Also closed at
  Black Springs", a chain requirement merged into the same paragraph after a
  `<br />`, "24 hrs a day and at Cottage Springs". Do not turn it into a
  blacklist of known places, and do not match on `ConditionSpan`, which drops
  the tail (it is for display: `beyond.from` / `beyond.to`).

Then the road keeps its own status, and the closure is reported as
`Road.beyond` (with the configured name, once per name) and as a NEARBY alert. Anything else,
including every near miss, is an ordinary condition on the road. Do not loosen
this into a general "past the end" rule: one was built and reviewed on
2026-11-13 and produced false all-clears (an unknown place, or an offset
pointing back onto the road, read as "past the end"). A false all-clear is the
one outcome this code must not produce; a false closure is the fallback.

`internal/hazards` gives each `beyond` entry a headline
("Hwy 4: Ebbetts Pass winter closure (past Bear Valley) — snow"),
`seasonal: true` and severity INFO: it is the road's yearly state, not
today's news. `internal/gridapi/summary.go` (`beyondItems`) still builds a
roads item for it, but an INFO condition is baseline, so the place summary
does not count or list it. Until 2026-11-19 it was SEVERE and led the roads
domain all winter, and each client had to rank it down.

"Chains or snow tires are required" (R1) is a chain requirement like the R2
and R3 wordings (`classifyCondition`, `requiresChains`); all three set
`ChainControlStatus_REQUIRED`. The verb must follow directly and "no" must not
precede: "not required", "no longer required", "may be required" and "no
chains ... required" are notices. Order matters: closure, then the wordings
main always read ("chains are required", "chains required", "chain control"),
then restriction, then the R1 wordings, so a restriction mentioning R1 stays a
restriction.
