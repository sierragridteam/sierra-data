package mcp

import "encoding/json"

// A single MCP resource: a compact reference the agent can read to understand
// the data model and — critically — the fail-loud honesty contract. The full
// contract is https://data.sierragridteam.org/api/openapi.json.

const docsResourceURI = "grid://reference"

const docsText = `# The Grid — MCP reference

Public, read-only, keyless hazard/road/weather data for the central Sierra
(Calaveras & Tuolumne counties). Reference only — verify with official sources.

## Tools
- grid_situation(location) — area mode + rollup for a place/address/lat,lng. Start here.
- grid_events(location?, layer?, severity_min?, status?, since?, q?, count?, limit?) — list or text-search events (hazards + mesh-node presence, layer=mesh).
- grid_event(id) — full detail on one event.
- grid_conditions(location?) — roads + weather conditions.
- grid_resolve(address | lat,lng) — location → containing places.
- grid_places(kind?, q?) — the place directory (discover valid slugs).
- grid_sources() — upstream feed health.
- grid_history(location?, layer?, from?, to?, q?, every_revision?, limit?) — revision timeline; with q, when an event's text first and last said something.

## Severity scale (rank 0–4)
INFO < MINOR < MODERATE < SEVERE < EXTREME. Editorial response-urgency, not
magnitude — an Evacuation Order (EXTREME) outranks an M5 earthquake (SEVERE).

## Query patterns (how to answer common questions)
Text search (q) answers most "about a thing" questions in one call. Words are
all required; "quoted words" are a phrase; -word excludes; OR separates
alternatives. Stems match (flooded finds flooding) and a numbered road matches
in any spelling ("hwy 4", "SR-4", "state route 4").
- "What happened on <a specific road>?" — grid_events with q "hwy 4" (add
  since for a window, layer=road_incident for incidents only). With q, ended
  events are included unless you pass status; each row says its status and
  carries a short match excerpt. For the verbatim CHP report, grid_event(id).
- "How many <X> / how many collisions on 108 this month?" — grid_events with
  q "sr 108 collision" and since: the result's total is the count across every
  page and counts breaks it down by layer and category, so do not page to
  tally. count=true gives the same totals without q. grid_situation gives
  per-domain active counts for one place.
- "Tell me about the <name> fire" / "is node <name> up?" — q with the name
  finds the event id; then grid_event(id).
- "When was <an order/warning/restriction> first issued, and when was it
  lifted?" — grid_history with q. It searches what each stored revision said,
  so it finds text the event no longer carries, and answers one row per event:
  the first matching revision, the last, and how many.
- No match means no stored report's text matched, NOT that nothing happened.
  Every search result carries search.since (when collection began) and a note;
  pass that caveat on. Try another wording before concluding (wires down OR
  power lines), and check grid_sources for gaps.
- Scope with the broadest place that covers the question (county/area), not a
  street: an address resolves to its containing places, so it scopes to the
  county/area, not to that one street.
- "Is <node> up / when did <node> last advertise?" (MeshCore mesh nodes) —
  grid_events with layer=mesh (legacy alias "network") and q set to the node's
  name or the start of its public key; unscoped is fine, the mesh spans
  places. Each row is one node: name in headline/areaLabel, pubkey + radio
  telemetry in detail. Freshness
  is mesh.telemetry.lastRelayedAt (when a packet the node RELAYED last reached the
  Grid; for a repeater this is near-constant) and mesh.telemetry.lastHeardAt
  (when its own advert last did); take the later. Both are the Grid's clock and
  null means none on record, not "down". NOT the row's observedAt, which only
  moves when the node's identity, position or reachability changes, so it can
  be days old for a node that is talking now. mesh.telemetry.lastAdvertAt is
  the node's own advert stamp and is diagnostic only (node clocks skew). A
  node missing from the list is not proof it's down.

## Map layers (URLs for a maps client — not data to reason over)
These tools deliberately omit geometry (they return a centroid + bbox instead);
raw coordinates are not useful to a model, and the analyzable rows already come
back from grid_events / grid_situation. If you are building or driving a map UI
(MapLibre/Leaflet), hand it these layer URLs directly — do not fetch them to
reason over:
- GET /api/v1/places/{place}/map/{layer}.geojson — one RFC 7946 FeatureCollection
  per layer, coordinates [lng,lat]. {layer} is a slug: wildfire, evacuation,
  weather_alert, earthquake, road_incident, power, mesh_node, road_segment,
  chain_control, message_sign, fire_weather. (mesh_node here is the map slug for
  the same layer that grid_events calls "mesh". message_sign is what Caltrans's
  roadside message signs are showing — mostly safety-campaign boilerplate,
  always INFO, never a hazard.)
- Mesh relay topology: GET /api/v1/places/{place}/map/mesh_link.geojson
  (place-scoped) or GET /api/v1/mesh/links (whole mesh, JSON edge list).
- Caltrans traffic cameras: GET /api/v1/places/{place}/map/camera.geojson, or
  the JSON list GET /api/v1/cameras?place=. Each carries a live snapshot
  imageUrl and (usually) an HLS streamUrl — links to Caltrans for a person to
  look at. Cameras are reference views, not hazards; an empty list means no
  camera nearby, never an all-clear.
Each feature carries the same camelCase properties envelope as the event rows
(id, layer, severity, headline, source, …), so the map and the tool answers stay
consistent.

## The honesty contract (must respect when relaying)
- Every source reports OK | STALE | UNAVAILABLE. UNAVAILABLE means the status is
  UNKNOWN, not clear. Never present absence of data as an all-clear. This cuts
  both ways for mesh: a healthy MeshCore feed (grid_sources) means the Grid
  reached the bridge, not that any given node is up — node liveness is per-node
  in grid_events layer=mesh, never the feed's poll time.
- Evacuation: activeEvacuations is an explicit null (source errored → UNKNOWN,
  render "check Genasys"), 0 (Cal OES healthy, no active zones — a caveated
  confirmed-empty, not a guarantee), or N (active). null and 0 are different;
  never collapse them.
- Evacuation and life-safety text is reference-only; link the authoritative
  source and never paraphrase directive orders.`

func resourceList() []map[string]interface{} {
	return []map[string]interface{}{{
		"uri":         docsResourceURI,
		"name":        "The Grid API reference",
		"description": "Data model, severity scale, and the fail-loud honesty contract.",
		"mimeType":    "text/markdown",
	}}
}

func readResource(params json.RawMessage) rpcResponse {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcResponse{Error: &rpcError{errBadParams, "invalid params: " + err.Error()}}
	}
	if p.URI != docsResourceURI {
		return rpcResponse{Error: &rpcError{errBadParams, "resource not found: " + p.URI}}
	}
	return rpcResponse{Result: map[string]interface{}{
		"contents": []map[string]interface{}{{
			"uri":      docsResourceURI,
			"mimeType": "text/markdown",
			"text":     docsText,
		}},
	}}
}
