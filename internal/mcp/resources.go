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
- grid_search(q, kind?, limit?) — find a thing by name across kinds (events, mesh nodes, places, roads, cameras, scanner feeds) and get the id or link.
- grid_event(id) — full detail on one event.
- grid_conditions(location?) — roads + weather conditions.
- grid_resolve(address | lat,lng) — location → containing places.
- grid_places(kind?, q?) — the place directory (discover valid slugs).
- grid_sources() — upstream feed health.
- grid_history(location?, layer?, from?, to?, q?, every_revision?, limit?) — revision timeline; with q, when an event's text first and last said something.
- grid_mesh_status(location?, show?, radius_km?, limit?) — the MeshCore radio mesh around a place: who was heard, who is quiet, silent or unknown and since when, which nodes did not answer their monitor, and whether the Grid is hearing the mesh at all.
- grid_mesh_node(node, window?, limit?) — one node by name, short id or key: its clocks, its monitor's last reading, and the links to its neighbours.
- grid_mesh_path(from, to, window?, radius_km?) — an observed route between two nodes or places, hop by hop, with a second route when there is one.
- grid_mesh_telemetry(node, hours?) — a monitored node's battery, temperature and noise-floor trend.
- grid_mesh_changes(location?, hours?) — nodes first recorded, expired, back, renamed, or read through a different gateway.
- search(query) / fetch(id) — the generic pair some clients require by name; thin forms of grid_search and grid_event. Prefer the grid_* tools when you can call them.

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
- "Tell me about the <name> fire" / "is node <name> up?" / "which camera shows
  <place>?" — grid_search with the name: the best few of each kind, ranked,
  each with what to call next. Then grid_event(id) for the facts. Use
  grid_events with q when you need every match or a count, not a name lookup.
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
- "Is the mesh healthy / which repeaters are down?" — grid_mesh_status. It
  never says "down": a node is active (heard, relayed or read by a monitor in
  the last 12 h), quiet (12 to 48 h), silent (longer) or unknown (no activity on
  record), and separately a monitor that logs in to it is answering or got no
  answer. Read summary.health first: "unknown" means the Grid's own feed is not
  reporting, and then every state is only the last it knew.
- "Is <node> up / what does it hear / how good are its links?" —
  grid_mesh_node with the node's name, its short id (first 8 hex of its key) or
  a key prefix. A name that matches several nodes returns the candidates and
  reads none. Freshness is last_active_at (the later of its advert reaching the
  Grid, a packet it relayed reaching the Grid, and a monitor's successful
  read), never an event's observedAt.
- "Is there a path from <A> to <B>?" — grid_mesh_path. The answer is an
  inference from links observed carrying traffic, not a guarantee, and
  found=false means none was observed, not that none exists: links are learned
  only from packet paths the Grid could resolve without guessing, so real ones
  are missing. Check from.read_as / to.read_as: "Arnold" is the town (the nodes
  near it), "SIERRA Arnold Summit" is one repeater.
- "How is <node>'s battery?" — grid_mesh_telemetry. Only monitored nodes have
  readings; for the rest the answer is unknown, and a reading is the node as it
  was when read (see its age).
- "What changed on the mesh today?" — grid_mesh_changes.
- "Which nodes cover <place>?" — grid_mesh_status with the place and show=all.
  It lists nodes located in or near the place; radio coverage is not modelled.
- grid_events with layer=mesh still lists the raw node records (one INFO row
  per node, liveness in mesh.telemetry.lastRelayedAt / lastHeardAt, both the
  Grid's clock; null means none on record, not "down"). Prefer the grid_mesh_*
  tools: they name the nodes, give ages, and state what the data cannot show.

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
  (grid_mesh_status, grid_mesh_node), never the feed's poll time. And an
  unhealthy feed means every node's state is only the last the Grid knew.
- Mesh: there is no "down". Silence is the Grid not hearing a node, a monitor's
  failed login is the node not answering that monitor, and an old reading is the
  node as it was. A link or path that is not listed was not observed, which is
  not proof it does not exist; a path that is listed is not a promise of delivery.
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
