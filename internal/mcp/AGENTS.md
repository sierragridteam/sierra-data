# internal/mcp: the MCP endpoint

`/mcp` serves The Grid's read-only data to LLM clients as Model Context
Protocol tools: JSON-RPC 2.0 over HTTP POST (Streamable HTTP, revision
`2025-06-18`), stateless, no session, no SSE stream, no authentication, no MCP
SDK. Design history: `docs/design/mcp-design.md`.

## The one rule: a tool is a view of `/api/v1`

Every tool answers by issuing in-process GETs against the gRPC-Gateway mux
(`Server.callAPI`, `callAPIJSON`) and reshaping the JSON. The package holds no
store, no service and no second query path, so the tools cannot say anything
the public HTTP API does not, and cannot expose anything it does not.

- A capability that needs a new query (one node's links, a path over the link
  graph) is built in `internal/gridapi` as a public route with tests and an
  OpenAPI entry, and the tool calls it. Do not compute it here.
- What does belong here is the reading a model would otherwise get wrong:
  names beside ids, ages beside times, dropped geometry, and the caveat in the
  data. That is presentation, and it is tested (`mesh_model.go`).
- `callAPIJSON` turns a non-2xx into a tool error. Use it wherever a failed
  call would otherwise read as an empty result. `callAPI` is for callers that
  need the status (a 404 that means "no such event").
- Public and read-only. Nothing from the mesh relay (its admin API, tokens,
  client lists, gateway credentials, frames) is reachable from this process;
  keep it that way. Do not add a tool that takes a credential.

## Tools

`registerTools` (tools.go) and `meshTools` (mesh.go) are the registry; a tool
is a name, a description, a JSON Schema and a handler. The description is the
only thing a model reads before choosing a tool: say when to use it, what the
arguments accept, and what the answer cannot show.

| Tool | Reads |
| --- | --- |
| `grid_situation` | `/places/{place}/summary` |
| `grid_events`, `grid_event` | `/events`, `/events/{id}` |
| `grid_search`; `search`, `fetch` | `/search`; the generic pair some clients require by exactly those names |
| `grid_conditions`, `grid_resolve`, `grid_places`, `grid_sources`, `grid_history` | `/conditions`, `/places:resolve`, `/places`, `/sources`, `/history` |
| `grid_mesh_status` | `/events?layer=mesh` (a place, or the whole feed), `/sources` |
| `grid_mesh_node` | `/events/{id}`, `/mesh/links?node=`, `/events/{id}/related`, `/events/{id}/timeline` |
| `grid_mesh_path` | `/mesh/path` |
| `grid_mesh_telemetry` | `/mesh/telemetry` |
| `grid_mesh_changes` | `/history?layer=mesh`, `/events/{id}/timeline` |

**When you add, rename or remove a tool**, update every list of them in the
same change: `cmd/server/landing/mcp.html` ("What the assistant gets"),
`cmd/server/landing/llms.txt`, the `/mcp` description in
`api/grid/v1/openapi.overlay.yaml` (then `make openapi`), the `grid://reference`
resource (`resources.go`), the table above, the count in `TestInitializeAndList`
and a `CHANGELOG.md` section.

## Results

- `toolResult` sends the same object twice: indented JSON as text, and
  `structuredContent`. `callTool` adds the reference-only `disclaimer` to every
  result, error or not.
- Geometry is dropped (`reshape.go`); a centroid and bbox stay as `location`.
- A location or node argument that resolves to nothing is a **result**
  (`resolved: false` with a message, and `candidates` when a node name matches
  several), not an error: a model can act on it. A missing required argument or
  a failed upstream call is a tool error (`isError`). An unknown tool is a
  JSON-RPC error.
- Location arguments take a place slug or id, a directory name, an address or
  `lat,lng` (`resolvePlace`). Arguments that may be a node or a place
  (`grid_mesh_path`) skip the address geocoder (`lookupPlace`) and say how each
  side was read (`read_as`).
- Keep results small. Lists are capped with a `more` note; a series is at most
  12 bucketed means, never the raw samples.

## The honesty rules

They are the product. Every result that could be read as an all-clear says in
its data that it is not one.

- A source that is `UNAVAILABLE` is unknown, not clear. Evacuation `null` is
  not `0`.
- **Mesh: there is no "down".** The Grid hears the mesh through community
  brokers and a few gateways, so the most it can say is that it has not heard
  a node. `mesh_model.go` keeps four things apart and never merges them into
  one light:
  - activity (`active` within 12 h, `quiet` to 48 h, `silent` beyond,
    `unknown` with none on record), from the later of `lastHeardAt`,
    `lastRelayedAt` and the monitor's `lastSuccessAt`, never `observedAt`;
  - the monitor's own verdict (`answering`, `no_answer`, `not_read_lately`,
    `never_read`): a failed login is "did not answer that monitor";
  - a reading's age: a gauge older than 24 h is given with its age and raises
    no issue, and a gauge the monitor did not read stays `null`;
  - the feed: when the `meshcore` source is not OK, `summary.health` is
    `unknown`, because a deaf Grid and a quiet mesh look the same.
  The 12 h / 48 h marks and the monitor windows are the Grid explorer's
  (sierra-grid `src/lib/mesh-fleet.ts`, `mesh-monitors.ts`). Change them
  together or the two will disagree about a node.
- A node is never named by its key alone: name plus `short_id` (the first 8
  hex), the full key only as `id` or in a `url`. A relay the catalogue does
  not hold is "uncatalogued node {short id}".
- Links are learned only from packet paths the Grid could resolve without
  guessing, so a link or path that is not listed was not observed. A path is an
  inference: `basis` and `answer` say so, found or not.
- The `honest` helper in `mesh_test.go` runs on every mesh result: no "is
  down", no `healthy`, no bare 64-character key. Call it in new mesh tests.

## Things that bit

- The finder (`/api/v1/search`, and `q` on `/events`) holds a public key as one
  word and matches the start of a word only for a query still being typed.
  A key prefix therefore needs `q_typing=true` (`looksLikeKeyPrefix`); without
  it "the start of its public key" finds nothing, which is how `grid_search`
  and `grid_events` behaved until 2026-11-20.
- A town is a point in the place directory and no mesh node attaches to it:
  `scopeNodes` takes the nodes within `radius_km` instead. Areas, counties and
  corridors use the store's attachment (`?place=`).
- Gateways sit at volunteers' houses, some outside every place and some
  sharing no position. The default scope adds every monitored node to the
  Ebbetts Pass area's for that reason.
- The landing page is served at the same URL to a browser (`mcpRoute` in
  `cmd/server/site.go`). The handler here still answers every non-POST with 405.

## Tests

`go test ./internal/mcp` needs no database: `fakeGrid` (mcp_test.go) and
`meshGrid` (mesh_test.go) stand in for the gateway mux, and `Server.clock` pins
now for the ages. The API routes the mesh tools call are tested where they
live (`internal/gridapi/mesh_path_test.go`).
