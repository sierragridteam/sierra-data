package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error)
}

func (s *Server) toolList() []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return out
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) rpcResponse {
	var p struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return rpcResponse{Error: &rpcError{errBadParams, "invalid params: " + err.Error()}}
	}
	var t *tool
	for i := range s.tools {
		if s.tools[i].Name == p.Name {
			t = &s.tools[i]
			break
		}
	}
	if t == nil {
		// An unrecognized tool is a protocol error, not a tool-execution error
		// (MCP splits the two channels) — return a JSON-RPC error, not isError.
		return rpcResponse{Error: &rpcError{errBadParams, "unknown tool: " + p.Name}}
	}
	res, err := t.Handler(ctx, p.Arguments)
	if err != nil {
		return toolResult(map[string]interface{}{"error": err.Error(), "disclaimer": disclaimer}, true)
	}
	if res == nil {
		res = map[string]interface{}{}
	}
	res["disclaimer"] = disclaimer
	return toolResult(res, false)
}

// toolResult packages a structured object as an MCP tool result — a text content
// block (indented JSON, for models that read text) plus structuredContent.
func toolResult(obj map[string]interface{}, isErr bool) rpcResponse {
	return rpcResponse{Result: map[string]interface{}{
		"content":           []map[string]interface{}{{"type": "text", "text": prettyJSON(obj)}},
		"structuredContent": obj,
		"isError":           isErr,
	}}
}

/* --------------------------------------------------------------- registry */

func (s *Server) registerTools() []tool {
	return []tool{
		{
			Name: "grid_situation",
			Description: "The headline call: what's happening at a place, address, or lat,lng in the " +
				"central Sierra (Calaveras/Tuolumne). Returns the area mode (QUIET/WATCH/ACTIVE), " +
				"per-domain status and counts (fire/evacuation/weather/roads/seismic/power), top headlines, " +
				"evacuation status (activeEvacuations is an explicit null when the source is UNAVAILABLE " +
				"= unknown, 0 when confirmed none, N when active — never treat null/absence as safe), and " +
				"per-source freshness. Reference only; verify with official sources.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"a place slug/id (e.g. \"ebbetts-pass\"), a street address, or \"lat,lng\""}},"required":["location"]}`),
			Handler:     s.handleSituation,
		},
		{
			Name: "grid_events",
			Description: "List active events (wildfire, evacuation, weather alert, earthquake, road " +
				"incident, power outage / Public Safety Power Shutoff, and mesh-node presence) — " +
				"optionally scoped to a location and filtered by " +
				"layer/severity/status/time. " +
				"Compact rows; call grid_event for full detail (incl. the verbatim report) on one. " +
				"Geometry is omitted (a location centroid is included). " +
				"Pass q to search the events' text instead of reading every row: words are all required, " +
				"\"quoted words\" are a phrase, -word excludes, OR separates alternatives; stems match (flooded finds " +
				"flooding) and a numbered road matches in any spelling (\"hwy 4\", \"SR-4\"). So \"what happened on " +
				"Highway 4 this week\" is q \"hwy 4\" with since, and a sub-type question is q \"hwy 4 collision\". " +
				"With q the rows are newest first, each carries a short match excerpt, and — unless you pass status — " +
				"ended events are included (each row says its status). With q or count=true the result has total " +
				"(every matching event, not just this page) and counts by layer and category, so a tally needs one " +
				"call and no paging. An empty result means no stored report's text matched, NOT that nothing " +
				"happened: see the result's search.since and note. " +
				"Caltrans planned lane closures are road_incident rows with category \"closure\", one per " +
				"work window: a window not yet set up is status SCHEDULED (effective = its planned start), " +
				"so pass status ACTIVE for only what is on the road now, or SCHEDULED for upcoming roadwork. " +
				"MeshCore mesh-node presence is layer \"mesh\" (legacy alias \"network\"): one INFO row per " +
				"node, the node's name in headline/areaLabel and its pubkey + radio telemetry in detail. To " +
				"find or check a specific node, pass its name or the start of its public key as q with " +
				"layer=mesh (unscoped is fine, the mesh spans places). Freshness is the later of " +
				"mesh.telemetry.lastRelayedAt (a packet the node relayed last reached the Grid) and " +
				"mesh.telemetry.lastHeardAt (its own advert did), both the Grid's clock; NOT observedAt, " +
				"which moves only when the node's identity, position or reachability changes and can be " +
				"days old for a node that is talking now. mesh.telemetry.lastAdvertAt is the node's " +
				"self-reported advert time and is diagnostic only (clocks skew). Absence of a " +
				"node here is not proof it's down. Raise limit (max 200) and follow " +
				"nextPageToken to get the full set. (grid_situation already gives per-domain active counts " +
				"for a single place.)",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"place slug/id, address, or lat,lng to scope to"},"layer":{"type":"string","description":"wildfire|evacuation|weather_alert|earthquake|road_incident|power|mesh|burn_status|source_health|chain_control|road_segment (source_health = one event per upstream feed whose history is that feed's outages; chain_control = one event per stretch of highway under a chain requirement; road_segment = one event per monitored road, revised when it closes, reopens, or chains are required or lifted on it (category open|chains|restricted|closed); power = PG&E outages AND Public Safety Power Shutoffs, separated by each row's category: unplanned|planned|psps. Most outages are tiny — the statewide median affects ONE customer — so pair layer=power with severity_min unless you want every service call. mesh = MeshCore node presence; \"network\" is a legacy alias)"},"severity_min":{"type":"string","description":"INFO|MINOR|MODERATE|SEVERE|EXTREME"},"status":{"type":"string","description":"ACTIVE|SCHEDULED|RESOLVED|EXPIRED, comma-separated, or \"any\". Default ACTIVE,SCHEDULED; with q the default is any"},"since":{"type":"string","description":"RFC3339; only events observed at/after"},"q":{"type":"string","description":"text search over headline, location, summary, description, incident log, typed facts, node names and keys; e.g. \"hwy 4 collision\", \"lime kiln flooded\", \"wires down OR power lines\""},"count":{"type":"boolean","description":"also return total and counts by layer/category without q"},"limit":{"type":"integer"},"page_token":{"type":"string"}}}`),
			Handler:     s.handleEvents,
		},
		{
			Name: "grid_search",
			Description: "Find a thing by name when you do not have its id: a fire, a road incident, a place, a monitored " +
				"road, a MeshCore mesh node (by name or the start of its public key), a traffic camera, a scanner feed. " +
				"Returns the best few matches of each kind with what to call next (next), and totals per kind so you know " +
				"when the few shown are not all. Same q syntax as grid_events (words all required, \"quoted phrase\", " +
				"-exclude, OR; a road in any spelling). Events and nodes are ranked by relevance, active and recent first, " +
				"and every status is searched; each hit says its status. Use this to resolve a name, then grid_event / " +
				"grid_situation for the facts. For every matching event in date order with a count, use grid_events with q " +
				"instead. A kind listed in unavailable could not be searched (unknown, not none), and no hit at all means " +
				"nothing stored matched, not that nothing happened.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","description":"what to find, e.g. \"priest fire\", \"SIERRA Arnold\", \"bear valley camera\", \"hwy 4\""},"kind":{"type":"string","description":"PLACE|ROAD|EVENT|MESH_NODE|CAMERA|SCANNER, comma-separated; default all"},"limit":{"type":"integer","description":"hits per kind, default 5, max 20"}},"required":["q"]}`),
			Handler:     s.handleSearch,
		},
		{
			// The two names below are fixed by clients, not by us: ChatGPT's deep
			// research and company knowledge only use a server's tools named
			// exactly "search" and "fetch", with these argument and result
			// shapes (developers.openai.com/docs/mcp, checked 2026-11-15). They
			// are thin forms of grid_search and grid_event; a client that can
			// call any tool should prefer the grid_* tools.
			Name: "search",
			Description: "Search The Grid (central Sierra hazard, road, weather-alert, power and mesh events, and places) " +
				"by text and return matching records as {id, title, url}. Pass each id to fetch for the full record. " +
				"Words are all required; a road matches in any spelling (\"hwy 4\"). No result means nothing stored " +
				"matched, not that nothing happened. Reference only; verify with official sources.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"what to search for"}},"required":["query"]}`),
			Handler:     s.handleGenericSearch,
		},
		{
			Name: "fetch",
			Description: "Fetch one record found by search, by its id: the full text of an event (headline, status, " +
				"severity, location, summary, the source's own description) or a place's current situation.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"an id returned by search"}},"required":["id"]}`),
			Handler:     s.handleGenericFetch,
		},
		{
			Name:        "grid_event",
			Description: "Full detail on one event by id (from grid_events): headline, AI summary, verbatim description, severity/status, typed detail (acres/containment, evac level, magnitude…), provenance and the canonical source link.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"event id, e.g. calfire:2026-priest-fire"}},"required":["id"]}`),
			Handler:     s.handleEvent,
		},
		{
			Name:        "grid_conditions",
			Description: "Current weather and fire-weather state per location — temperature, wind, visibility, and the fire-weather classification — optionally scoped to a location. Conditions, not events. (Road status/incidents are events: grid_events with layer=road_incident.)",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"place slug/id, address, or lat,lng to scope to"}}}`),
			Handler:     s.handleConditions,
		},
		{
			Name:        "grid_resolve",
			Description: "Resolve an address or lat,lng to the places that contain it (most-specific first: site, evac zone, town, corridor, county, area). Use the returned slug with the other tools.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"address":{"type":"string"},"lat":{"type":"number"},"lng":{"type":"number"}}}`),
			Handler:     s.handleResolve,
		},
		{
			Name:        "grid_places",
			Description: "The place directory for discovery — areas, counties, towns, evacuation zones, corridors — so you can find valid place slugs. Filter by kind or a name query.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","description":"AREA|COUNTY|TOWN|EVAC_ZONE|CORRIDOR|SITE"},"q":{"type":"string","description":"name substring"}}}`),
			Handler:     s.handlePlaces,
		},
		{
			Name:        "grid_sources",
			Description: "Feed health for every upstream source (OK/STALE/UNAVAILABLE, last success, last error) — use it to disclose data gaps. A source that is UNAVAILABLE means its hazard status is unknown, not clear. This is health NOW; for when a source was down in the past, read the history of its event `source:<id>` (grid_event, or grid_history with layer=source_health), where the Grid records it.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			Handler:     s.handleSources,
		},
		{
			Name: "grid_history",
			Description: "After-action / timeline: the revisions of events over a time range, newest first (how a fire grew, " +
				"when a warning escalated, when it was resolved). Optionally scope by location and layer. " +
				"Pass q (same syntax as grid_events) to search what reports said AT EACH REVISION, which finds text an " +
				"event no longer carries: \"when was the evacuation order for that zone first issued, and when was it " +
				"lifted\", \"when did R2 chain controls first appear on 88\". With q the answer is one row per event " +
				"(matches): its first matching revision in full with a match excerpt, the last matching revision's " +
				"number and time, how many revisions matched, and total. If last_revision is below the first row's " +
				"event's current revision, the text stopped matching after it. Pass every_revision=true for each " +
				"matching revision instead. Limits: only stored revisions are searched (see search.since; a source " +
				"that was down left a gap), a revision is stored when content changes rather than on every poll, and " +
				"location/layer are the event's current ones. No match is not proof it never said so. By default only " +
				"revisions where something changed are returned; kind=READING (or CHANGE,READING) adds readings that " +
				"confirmed an unchanged state, such as each daily call to a county burn line. kind=READING cannot be " +
				"combined with q.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"},"layer":{"type":"string"},"kind":{"type":"string","description":"CHANGE (default), READING, or both comma-separated"},"from":{"type":"string","description":"RFC3339 start"},"to":{"type":"string","description":"RFC3339 end"},"q":{"type":"string","description":"text search over each revision's text"},"every_revision":{"type":"boolean","description":"with q: one row per matching revision instead of one per event"},"limit":{"type":"integer"},"page_token":{"type":"string"}}}`),
			Handler:     s.handleHistory,
		},
	}
}

/* --------------------------------------------------------------- handlers */

func (s *Server) handleSituation(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	loc := argStr(args, "location")
	if loc == "" {
		return nil, fmt.Errorf("location is required (a place slug, address, or lat,lng)")
	}
	slug, resolved, err := s.resolvePlace(ctx, loc)
	if err != nil {
		return nil, err
	}
	if slug == "" {
		return unresolved(loc), nil
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/places/"+url.PathEscape(slug)+"/summary")
	if err != nil {
		return nil, fmt.Errorf("summary for %q unavailable: %w", slug, err)
	}
	body["resolved_place"] = slug
	if len(resolved) > 1 {
		body["also_contains"] = compactPlaceRefs(resolved)
	}
	return body, nil
}

func (s *Server) handleEvents(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if loc := argStr(args, "location"); loc != "" {
		slug, _, err := s.resolvePlace(ctx, loc)
		if err != nil {
			return nil, err
		}
		if slug == "" {
			return unresolved(loc), nil
		}
		q.Set("place", slug)
	}
	for _, k := range []string{"layer", "severity_min", "since", "page_token"} {
		if v := argStr(args, k); v != "" {
			q.Set(k, v)
		}
	}
	text := strings.TrimSpace(argStr(args, "q"))
	status := argStr(args, "status")
	// Someone searching for a thing wants it whether or not it is over, and
	// an agent will not think to ask: with q the default is every status.
	if strings.EqualFold(status, "any") || (status == "" && text != "") {
		status = allStatuses
	}
	if status != "" {
		q.Set("status", status)
	}
	if text != "" {
		q.Set("q", text)
	}
	if argBool(args, "count") {
		q.Set("counts", "true")
	}
	if n := argInt(args, "limit"); n > 0 {
		q.Set("page_size", strconv.Itoa(n))
	}
	// Lists never draw shapes: ask for none.
	q.Set("geometry_format", "NONE")
	body, err := s.callAPIJSON(ctx, "/api/v1/events?"+q.Encode())
	if err != nil {
		return nil, err
	}
	matches, _ := body["matches"].(map[string]interface{})
	events := leanEvents(asArray(body["events"]))
	for _, e := range events {
		if ev, ok := e.(map[string]interface{}); ok {
			if id, _ := ev["id"].(string); id != "" {
				if m := matchText(matches[id]); m != "" {
					ev["match"] = m
				}
			}
		}
	}
	out := map[string]interface{}{"events": events, "count": len(events)}
	if t, ok := body["nextPageToken"]; ok && t != "" {
		out["nextPageToken"] = t
	}
	if total, ok := body["totalMatches"]; ok && total != nil {
		out["total"] = total
		out["counts"] = body["counts"]
	}
	if info, ok := body["search"]; ok && info != nil {
		out["search"] = info
	}
	return out, nil
}

func (s *Server) handleSearch(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	text := strings.TrimSpace(argStr(args, "q"))
	if text == "" {
		return nil, fmt.Errorf("q is required (what to find)")
	}
	q := url.Values{"q": {text}}
	if k := argStr(args, "kind"); k != "" {
		q.Set("kind", k)
	}
	if n := argInt(args, "limit"); n > 0 {
		q.Set("limit", strconv.Itoa(n))
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/search?"+q.Encode())
	if err != nil {
		return nil, err
	}
	hits := asArray(body["hits"])
	for _, h := range hits {
		hit, ok := h.(map[string]interface{})
		if !ok {
			continue
		}
		if m := matchText(hit["match"]); m != "" {
			hit["match"] = m
		} else {
			delete(hit, "match")
		}
		kind, _ := hit["kind"].(string)
		id, _ := hit["id"].(string)
		title, _ := hit["title"].(string)
		switch kind {
		case "EVENT", "MESH_NODE":
			hit["next"] = "grid_event id=" + id
		case "PLACE":
			hit["next"] = "grid_situation location=" + id
		case "ROAD":
			hit["next"] = "grid_events q=\"" + title + "\" layer=road_incident"
		default:
			// A camera or a feed is a link, not a record: url is the thing.
			delete(hit, "apiPath")
		}
		// An event's facts arrive nested (null for other kinds): lift them
		// beside the title, where a model reads them with the hit.
		if ev, ok := hit["event"].(map[string]interface{}); ok {
			for _, k := range []string{"layer", "category", "status", "severity", "observedAt"} {
				if v, ok := ev[k]; ok && v != nil {
					hit[k] = v
				}
			}
		}
		delete(hit, "event")
		for _, k := range []string{"url", "detail", "category"} {
			if v, _ := hit[k].(string); v == "" {
				delete(hit, k)
			}
		}
	}
	out := map[string]interface{}{"hits": hits, "count": len(hits), "totals": body["totals"]}
	if un := asArray(body["unavailable"]); len(un) > 0 {
		out["unavailable"] = un
	}
	if info, ok := body["search"]; ok && info != nil {
		out["search"] = info
	}
	return out, nil
}

// publicBaseURL is where the API is served, for the links the generic
// search/fetch tools must return (a client only cites a result with a url).
const publicBaseURL = "https://data.sierragridteam.org"

// Ids of the generic tools carry their kind, so fetch knows what to read.
const (
	genericEvent = "event:"
	genericPlace = "place:"
)

func (s *Server) handleGenericSearch(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	text := strings.TrimSpace(argStr(args, "query"))
	if text == "" {
		return nil, fmt.Errorf("query is required")
	}
	q := url.Values{"q": {text}, "kind": {"EVENT,MESH_NODE,PLACE"}, "limit": {"10"}}
	body, err := s.callAPIJSON(ctx, "/api/v1/search?"+q.Encode())
	if err != nil {
		return nil, err
	}
	results := []interface{}{}
	for _, h := range asArray(body["hits"]) {
		hit, ok := h.(map[string]interface{})
		if !ok {
			continue
		}
		kind, _ := hit["kind"].(string)
		id, _ := hit["id"].(string)
		title, _ := hit["title"].(string)
		if detail, _ := hit["detail"].(string); detail != "" {
			title += " (" + detail + ")"
		}
		link, _ := hit["url"].(string)
		switch kind {
		case "PLACE":
			id = genericPlace + id
		default:
			if ev, ok := hit["event"].(map[string]interface{}); ok {
				if st, _ := ev["status"].(string); st != "" && st != "ACTIVE" {
					title += " [" + st + "]"
				}
			}
			id = genericEvent + id
		}
		if link == "" {
			if p, _ := hit["apiPath"].(string); p != "" {
				link = publicBaseURL + p
			}
		}
		results = append(results, map[string]interface{}{"id": id, "title": title, "url": link})
	}
	return map[string]interface{}{"results": results}, nil
}

func (s *Server) handleGenericFetch(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	id := strings.TrimSpace(argStr(args, "id"))
	switch {
	case strings.HasPrefix(id, genericPlace):
		slug := strings.TrimPrefix(id, genericPlace)
		path := "/api/v1/places/" + url.PathEscape(slug) + "/summary"
		body, err := s.callAPIJSON(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("place %q: %w", slug, err)
		}
		return map[string]interface{}{
			"id": id, "title": "Current situation: " + slug, "text": prettyJSON(body), "url": publicBaseURL + path,
			"metadata": map[string]interface{}{"kind": "place"},
		}, nil
	case id == "":
		return nil, fmt.Errorf("id is required (an id returned by search)")
	}
	// An event; the prefix is optional so a bare event id works too.
	eventID := strings.TrimPrefix(id, genericEvent)
	path := "/api/v1/events/" + url.PathEscape(eventID)
	body, code, err := s.callAPI(ctx, path)
	if err != nil {
		return nil, err
	}
	if code == 404 {
		return nil, fmt.Errorf("no record with id %q", id)
	}
	if code != 200 {
		return nil, fmt.Errorf("lookup failed (HTTP %d)", code)
	}
	str := func(k string) string { v, _ := body[k].(string); return v }
	var text strings.Builder
	for _, line := range []struct{ label, value string }{
		{"", str("headline")},
		{"Status", str("status")}, {"Severity", str("severity")}, {"Layer", str("layer")},
		{"Location", str("areaLabel")}, {"Observed", str("observedAt")},
		{"Summary", str("summary")}, {"Source's description", str("description")},
	} {
		if line.value == "" {
			continue
		}
		if line.label != "" {
			text.WriteString(line.label + ": ")
		}
		text.WriteString(line.value + "\n")
	}
	link := str("canonicalUrl")
	if link == "" {
		link = publicBaseURL + path
	}
	meta := map[string]interface{}{"kind": "event"}
	for _, k := range []string{"layer", "category", "status", "severity", "observedAt"} {
		if v := str(k); v != "" {
			meta[k] = v
		}
	}
	if prov, ok := body["provenance"].(map[string]interface{}); ok {
		if a, _ := prov["attribution"].(string); a != "" {
			meta["attribution"] = a
		}
	}
	return map[string]interface{}{"id": id, "title": str("headline"), "text": text.String(), "url": link, "metadata": meta}, nil
}

func (s *Server) handleEvent(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	id := argStr(args, "id")
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	body, code, err := s.callAPI(ctx, "/api/v1/events/"+url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	if code == 404 {
		return nil, fmt.Errorf("no event with id %q", id)
	}
	if code != 200 {
		return nil, fmt.Errorf("event lookup failed (HTTP %d)", code)
	}
	return leanEvent(body, true), nil
}

func (s *Server) handleConditions(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if loc := argStr(args, "location"); loc != "" {
		slug, _, err := s.resolvePlace(ctx, loc)
		if err != nil {
			return nil, err
		}
		if slug == "" {
			return unresolved(loc), nil
		}
		q.Set("place", slug)
	}
	path := "/api/v1/conditions"
	if e := q.Encode(); e != "" {
		path += "?" + e
	}
	return s.callAPIJSON(ctx, path)
}

func (s *Server) handleResolve(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if addr := argStr(args, "address"); addr != "" {
		q.Set("address", addr)
	} else if lat, latOK := argFloat(args, "lat"); latOK {
		lng, lngOK := argFloat(args, "lng")
		if !lngOK {
			return nil, fmt.Errorf("lat requires lng")
		}
		q.Set("lat", strconv.FormatFloat(lat, 'f', -1, 64))
		q.Set("lng", strconv.FormatFloat(lng, 'f', -1, 64))
	} else {
		return nil, fmt.Errorf("provide address, or lat and lng")
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/places:resolve?"+q.Encode())
	if err != nil {
		return nil, err // surface geocode/lookup failure as a tool error (isError)
	}
	return map[string]interface{}{"places": leanPlaces(asArray(body["places"]))}, nil
}

func (s *Server) handlePlaces(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if k := argStr(args, "kind"); k != "" {
		q.Set("kind", k)
	}
	if qq := argStr(args, "q"); qq != "" {
		q.Set("q", qq)
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/places?"+q.Encode())
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"places": leanPlaces(asArray(body["places"]))}, nil
}

func (s *Server) handleSources(ctx context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	// grid_sources exists to disclose feed health, so a failed /api/v1/sources call
	// must surface as an error, not a body that looks like a health rollup.
	return s.callAPIJSON(ctx, "/api/v1/sources")
}

func (s *Server) handleHistory(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	q := url.Values{}
	if loc := argStr(args, "location"); loc != "" {
		slug, _, err := s.resolvePlace(ctx, loc)
		if err != nil {
			return nil, err
		}
		if slug == "" {
			return unresolved(loc), nil
		}
		q.Set("place", slug)
	}
	for _, k := range []string{"layer", "kind", "from", "to", "page_token"} {
		if v := argStr(args, k); v != "" {
			q.Set(k, v)
		}
	}
	if n := argInt(args, "limit"); n > 0 {
		q.Set("page_size", strconv.Itoa(n))
	}
	text := strings.TrimSpace(argStr(args, "q"))
	collapsed := text != "" && !argBool(args, "every_revision")
	if text != "" {
		q.Set("q", text)
	}
	if collapsed {
		// One row per event: reading forty revisions to find the first that
		// says something is the cost this tool exists to remove.
		q.Set("collapse", "event")
	}
	body, err := s.callAPIJSON(ctx, "/api/v1/history?"+q.Encode())
	if err != nil {
		return nil, err
	}
	leanRevision := func(r interface{}) {
		m, ok := r.(map[string]interface{})
		if !ok {
			return
		}
		if ev, ok := m["event"].(map[string]interface{}); ok {
			leanEvent(ev, false)
		}
		if text := matchText(m["match"]); text != "" {
			m["match"] = text
		} else {
			delete(m, "match")
		}
	}
	out := map[string]interface{}{}
	if collapsed {
		rows := asArray(body["collapsed"])
		for _, r := range rows {
			if m, ok := r.(map[string]interface{}); ok {
				leanRevision(m["first"])
			}
		}
		out["matches"], out["count"] = rows, len(rows)
	} else {
		revs := asArray(body["revisions"])
		for _, r := range revs {
			leanRevision(r)
		}
		out["revisions"], out["count"] = revs, len(revs)
	}
	if t, ok := body["nextPageToken"]; ok && t != "" {
		out["nextPageToken"] = t
	}
	if total, ok := body["totalMatches"]; ok && total != nil {
		out["total"] = total
	}
	if info, ok := body["search"]; ok && info != nil {
		out["search"] = info
	}
	return out, nil
}

// allStatuses is every event status, for a search that should not hide what
// has ended.
const allStatuses = "ACTIVE,SCHEDULED,RESOLVED,EXPIRED"

// matchText flattens an API SearchMatch into one line for a model: the
// excerpt with each matched run wrapped in **. Empty when there is none.
func matchText(v interface{}) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, p := range asArray(m["parts"]) {
		part, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		text, _ := part["text"].(string)
		if hit, _ := part["hit"].(bool); hit {
			sb.WriteString("**" + text + "**")
		} else {
			sb.WriteString(text)
		}
	}
	return sb.String()
}

/* ---------------------------------------------------------- resolve helper */

// resolvePlace turns a free-form location (place slug/id, "lat,lng", or address)
// into a place slug plus the full containing-places list (most-specific first).
func (s *Server) resolvePlace(ctx context.Context, location string) (string, []interface{}, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", nil, nil
	}
	// 1. "lat,lng"
	if lat, lng, ok := parseLatLng(location); ok {
		body, _, err := s.callAPI(ctx, "/api/v1/places:resolve?"+url.Values{"lat": {lat}, "lng": {lng}}.Encode())
		if err != nil {
			return "", nil, err
		}
		places := asArray(body["places"])
		return firstSlug(places), places, nil
	}
	// 2. a place slug/id (single token, no spaces) — try a direct lookup. This is
	// one candidate strategy, so a transient failure falls through to the next.
	if !strings.ContainsAny(location, " ,") {
		if body, code, err := s.callAPI(ctx, "/api/v1/places/"+url.PathEscape(location)); err == nil && code == 200 {
			if slug := argStr(body, "slug"); slug != "" {
				return slug, []interface{}{leanPlace(body)}, nil
			}
		}
	}
	// 3. name search in the directory — resolves town/county/area names like
	// "Arnold" or "Calaveras County" that the street-address geocoder can't.
	for _, term := range nameTerms(location) {
		body, _, err := s.callAPI(ctx, "/api/v1/places?"+url.Values{"q": {term}}.Encode())
		if err != nil {
			return "", nil, err
		}
		if places := asArray(body["places"]); len(places) > 0 {
			return firstSlug(places), leanPlaces(places), nil
		}
	}
	// 4. street-address geocode (Census)
	body, _, err := s.callAPI(ctx, "/api/v1/places:resolve?"+url.Values{"address": {location}}.Encode())
	if err != nil {
		return "", nil, err
	}
	places := asArray(body["places"])
	return firstSlug(places), places, nil
}

// unresolved is the standard result when a location argument can't be mapped to
// a covered place — one shape across all location-scoped tools.
func unresolved(loc string) map[string]interface{} {
	return map[string]interface{}{
		"resolved": false,
		"message":  fmt.Sprintf("could not resolve %q to a covered place. This service only covers the central Sierra (Calaveras & Tuolumne). Try grid_places to see coverage.", loc),
	}
}

func firstSlug(places []interface{}) string {
	if len(places) == 0 {
		return ""
	}
	if m, ok := places[0].(map[string]interface{}); ok {
		if s := argStr(m, "slug"); s != "" {
			return s
		}
		return argStr(m, "id")
	}
	return ""
}

// compactPlaceRefs reduces resolved places to {slug,kind,name} refs.
func compactPlaceRefs(places []interface{}) []interface{} {
	out := make([]interface{}, 0, len(places))
	for _, p := range places {
		if m, ok := p.(map[string]interface{}); ok {
			out = append(out, map[string]interface{}{"slug": m["slug"], "kind": m["kind"], "name": m["name"]})
		}
	}
	return out
}

/* ---------------------------------------------------------------- arg utils */

func argStr(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func argInt(args map[string]interface{}, key string) int {
	if args == nil {
		return 0
	}
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func argFloat(args map[string]interface{}, key string) (float64, bool) {
	if args == nil {
		return 0, false
	}
	switch v := args[key].(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

// nameTerms yields directory-search terms for a location string: the whole
// string, and (if comma-separated, e.g. "Arnold, CA") the part before the first
// comma. Deduped, non-empty.
func nameTerms(location string) []string {
	terms := []string{location}
	if i := strings.Index(location, ","); i > 0 {
		if head := strings.TrimSpace(location[:i]); head != "" && head != location {
			terms = append(terms, head)
		}
	}
	return terms
}

// parseLatLng recognizes a "lat,lng" pair with both in valid ranges.
func parseLatLng(s string) (lat, lng string, ok bool) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return "", "", false
	}
	a, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	b, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || a < -90 || a > 90 || b < -180 || b > 180 {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}

// argBool reads a boolean argument; a model may send true, "true" or 1.
func argBool(args map[string]interface{}, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(v))
		return b
	case float64:
		return v != 0
	}
	return false
}
