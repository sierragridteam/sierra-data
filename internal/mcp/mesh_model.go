package mcp

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The mesh tools' reading of the data, kept apart from the fetching so the
// thresholds and the wording are tested rather than eyeballed. Everything
// here is a view of one /api/v1 response; nothing is queried or derived that
// the API does not already serve.
//
// The words matter more than the numbers. The Grid hears the mesh through
// community MQTT brokers and a few gateways, so "we have not heard it" is the
// strongest thing it can say about a node: never "down". A monitor that logs
// in to a repeater can say "it did not answer me", which is still not "off".
// And a reading that is hours old describes then, not now. Each of those is a
// separate field with its own time, and none of them is ever folded into a
// green light.

const (
	// A relay that has been neither heard nor credited with a relay for this
	// long is worth a look, and for this long is worth worrying about. The
	// same marks the Grid explorer's fleet view uses (sierra-grid
	// src/lib/mesh-fleet.ts), so the two never disagree about a node.
	meshQuietAfter  = 12 * time.Hour
	meshSilentAfter = 48 * time.Hour
	// Past this a silent node is history, not news: counted, not listed.
	meshLongSilentAfter = 14 * 24 * time.Hour

	// How recent a monitor's last successful read must be to call the node
	// "answering": the relay reads its gateways every 5 minutes and shared
	// repeaters every 30; an operator's own monitor keeps irregular hours
	// (sierra-grid src/lib/mesh-monitors.ts).
	gatewayFreshWithin  = 15 * time.Minute
	relayReadWithin     = 60 * time.Minute
	operatorReadWithin  = 8 * time.Hour
	monitorTriedAfterBy = time.Minute

	// A gauge older than this is reported with its age and judged by nobody.
	readingCurrentWithin = 24 * time.Hour
	batteryLowVolts      = 3.7
	batteryCriticalVolts = 3.5
)

// Activity states. There is no "down": see the package note above.
const (
	stateActive  = "active"  // heard or relayed within 12 h
	stateQuiet   = "quiet"   // 12 to 48 h
	stateSilent  = "silent"  // over 48 h
	stateUnknown = "unknown" // the Grid has no activity on record
)

// Monitor states: what an operator-run monitor's reads of the node say.
const (
	monitorAnswering = "answering"
	monitorNoAnswer  = "no_answer"
	monitorNotRead   = "not_read_lately"
	monitorNeverRead = "never_read"
)

// meshNode is one MESH event as the tools read it.
type meshNode struct {
	ID, Key, Name, Type, Status string
	URL                         string
	Heard, Relayed              time.Time
	Reachability                string
	Admin                       map[string]interface{} // nil when no monitor reads it
	ReadVia                     []string
	HasLocation                 bool
	Lat, Lng                    float64
	PlaceIDs                    []string
}

func parseMeshNode(ev map[string]interface{}) meshNode {
	n := meshNode{}
	n.ID, _ = ev["id"].(string)
	n.Status, _ = ev["status"].(string)
	n.URL, _ = ev["canonicalUrl"].(string)
	mesh, _ := ev["mesh"].(map[string]interface{})
	n.Key, _ = mesh["publicKey"].(string)
	n.Key = strings.ToLower(n.Key)
	n.Name, _ = mesh["name"].(string)
	n.Type, _ = mesh["nodeType"].(string)
	n.Reachability, _ = mesh["reachability"].(string)
	if n.Name == "" {
		n.Name, _ = ev["headline"].(string)
	}
	if n.Key == "" {
		n.Key = strings.TrimPrefix(n.ID, "meshcore:")
	}
	tel, _ := mesh["telemetry"].(map[string]interface{})
	n.Heard = parseTime(tel["lastHeardAt"])
	n.Relayed = parseTime(tel["lastRelayedAt"])
	if a, ok := tel["admin"].(map[string]interface{}); ok && a != nil {
		n.Admin = a
	}
	for _, v := range asArray(tel["readVia"]) {
		if k, _ := v.(string); k != "" {
			n.ReadVia = append(n.ReadVia, strings.ToLower(k))
		}
	}
	// Events arrive with their shape (geometry) or already leaned (location).
	for _, field := range []string{"geometry", "location"} {
		if g, ok := ev[field].(map[string]interface{}); ok {
			if c, ok := g["centroid"].(map[string]interface{}); ok {
				lat, okLat := c["lat"].(float64)
				lng, okLng := c["lng"].(float64)
				if okLat && okLng {
					n.Lat, n.Lng, n.HasLocation = lat, lng, true
				}
			}
		}
	}
	for _, v := range asArray(ev["placeIds"]) {
		if p, _ := v.(string); p != "" {
			n.PlaceIDs = append(n.PlaceIDs, p)
		}
	}
	return n
}

func parseTime(v interface{}) time.Time {
	s, _ := v.(string)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// shortID is the first eight characters of a key: what people read out, and
// enough to tell the nodes of one mesh apart. Always shown beside a name.
func shortID(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}

// label names a node for a sentence, falling back to its short id.
func (n meshNode) label() string {
	if n.Name != "" {
		return n.Name
	}
	return "unnamed node " + shortID(n.Key)
}

func relayCapable(nodeType string) bool { return nodeType == "repeater" || nodeType == "room_server" }

// isGateway: a companion radio an operator-run monitor reads is one of the
// relay's gateways (a radio at a volunteer's house, bridged to the internet).
func (n meshNode) isGateway() bool { return n.Type == "companion" && n.Admin != nil }

// infrastructure is what the mesh depends on, as opposed to someone's
// handheld, which comes and goes without it being news.
func (n meshNode) infrastructure() bool { return relayCapable(n.Type) || n.isGateway() }

// lastActive is the latest evidence the node was working and what it was:
// its own advert reached the Grid ("heard"), a packet it relayed did
// ("relayed"), or a monitor logged in to it ("answered a monitor"). The
// explorer's fleet view reads the first two; a login that succeeded twenty
// minutes ago is at least as good as either, and leaving it out called a
// gateway that had just answered "quiet". A stamp in the future is ignored:
// the first two are the Grid's clock and can only be ahead of now by a fault.
func (n meshNode) lastActive(now time.Time) (time.Time, string) {
	var at time.Time
	kind := ""
	for _, c := range []struct {
		at   time.Time
		kind string
	}{{n.Heard, "heard"}, {n.Relayed, "relayed"}, {parseTime(n.Admin["lastSuccessAt"]), "answered a monitor"}} {
		if !c.at.IsZero() && !c.at.After(now) && c.at.After(at) {
			at, kind = c.at, c.kind
		}
	}
	return at, kind
}

func (n meshNode) activityState(now time.Time) string {
	at, _ := n.lastActive(now)
	switch {
	case at.IsZero():
		return stateUnknown
	case now.Sub(at) > meshSilentAfter:
		return stateSilent
	case now.Sub(at) > meshQuietAfter:
		return stateQuiet
	}
	return stateActive
}

// monitorState reads the monitor's own clocks. A try that failed after the
// last good read is a real "no answer". A gateway that simply has not been
// read lately is not: the relay records no try while someone is using the
// radio or its host has paused it.
func (n meshNode) monitorState(now time.Time) string {
	if n.Admin == nil {
		if n.Reachability == "UNREACHABLE" {
			return monitorNoAnswer
		}
		return ""
	}
	read, tried := parseTime(n.Admin["lastSuccessAt"]), parseTime(n.Admin["lastAttemptAt"])
	if read.IsZero() && tried.IsZero() {
		if n.Reachability == "UNREACHABLE" {
			return monitorNoAnswer
		}
		return monitorNeverRead
	}
	if n.Reachability == "UNREACHABLE" || (!tried.IsZero() && (read.IsZero() || tried.After(read.Add(monitorTriedAfterBy)))) {
		return monitorNoAnswer
	}
	within := operatorReadWithin
	reporter, _ := n.Admin["reporterId"].(string)
	switch {
	case n.isGateway():
		within = gatewayFreshWithin
	case reporter == "relay":
		within = relayReadWithin
	}
	if now.Sub(read) <= within {
		return monitorAnswering
	}
	return monitorNotRead
}

// issue is one thing about a node worth a person's attention.
type issue struct {
	alarm bool
	text  string
}

// issues lists what stands out about a node. Activity applies to every node;
// the monitor and battery rules only where a monitor reads it.
func (n meshNode) issues(now time.Time) []issue {
	var out []issue
	at, kind := n.lastActive(now)
	switch n.activityState(now) {
	case stateUnknown:
		out = append(out, issue{false, "No activity on record: the Grid has never heard or been relayed by it since it began tracking. Unknown, not down."})
	case stateSilent:
		out = append(out, issue{true, fmt.Sprintf("Not heard for %s (last %s %s).", ageText(now.Sub(at)), kind, stamp(at))})
	case stateQuiet:
		out = append(out, issue{false, fmt.Sprintf("Quiet for %s (last %s %s).", ageText(now.Sub(at)), kind, stamp(at))})
	}
	if n.monitorState(now) == monitorNoAnswer {
		who := "a monitor"
		if r, _ := n.Admin["reporterId"].(string); r != "" {
			who = "the " + r + " monitor"
		}
		read := parseTime(n.Admin["lastSuccessAt"])
		if read.IsZero() {
			out = append(out, issue{false, fmt.Sprintf("Did not answer %s; no successful read on record.", who)})
		} else {
			out = append(out, issue{now.Sub(read) > 7*24*time.Hour,
				fmt.Sprintf("Did not answer %s; last answered %s ago (%s).", who, ageText(now.Sub(read)), stamp(read))})
		}
	}
	if v, at, ok := n.battery(); ok && now.Sub(at) <= readingCurrentWithin && v < batteryLowVolts {
		out = append(out, issue{v < batteryCriticalVolts, fmt.Sprintf("Battery low: %.2f V, read %s ago.", v, ageText(now.Sub(at)))})
	}
	return out
}

// battery is the last battery voltage a monitor read and when it read it.
func (n meshNode) battery() (volts float64, at time.Time, ok bool) {
	if n.Admin == nil {
		return 0, time.Time{}, false
	}
	v, ok := n.Admin["batteryVolts"].(float64)
	if !ok {
		return 0, time.Time{}, false
	}
	at = parseTime(n.Admin["lastSuccessAt"])
	if at.IsZero() {
		at = parseTime(n.Admin["reportedAt"])
	}
	return v, at, !at.IsZero()
}

// row is a node as one line of a list: who it is, when it was last active,
// what a monitor says, and what stands out.
func (n meshNode) row(now time.Time) map[string]interface{} {
	out := map[string]interface{}{
		"name":     n.label(),
		"short_id": shortID(n.Key),
		"type":     n.Type,
		"state":    n.activityState(now),
	}
	if n.isGateway() {
		out["role"] = "gateway"
	}
	if n.Status != "" && n.Status != "ACTIVE" {
		// EXPIRED: no advert or relay for the presence window (about 14 h).
		out["record"] = n.Status
	}
	if at, kind := n.lastActive(now); !at.IsZero() {
		out["last_active_at"] = stamp(at)
		out["last_active_ago"] = ageText(now.Sub(at))
		out["last_active_was"] = kind
	} else {
		out["last_active_at"] = nil
	}
	if m := n.monitor(now, nil); m != nil {
		out["monitor"] = m
	}
	if is := n.issues(now); len(is) > 0 {
		texts := make([]string, len(is))
		for i, x := range is {
			texts[i] = x.text
		}
		out["issues"] = texts
	}
	return out
}

// monitor is the compact monitor block of a row. names resolves the key of
// the gateway a reading was taken through, when the caller has it.
func (n meshNode) monitor(now time.Time, names func(key string) string) map[string]interface{} {
	state := n.monitorState(now)
	if state == "" {
		return nil
	}
	m := map[string]interface{}{"state": state}
	if n.Admin == nil {
		m["note"] = "Marked unreachable by a monitor; no reading is on record."
		return m
	}
	if r, _ := n.Admin["reporterId"].(string); r != "" {
		m["by"] = r
	}
	if read := parseTime(n.Admin["lastSuccessAt"]); !read.IsZero() {
		m["last_read_at"] = stamp(read)
		m["last_read_ago"] = ageText(now.Sub(read))
	} else {
		m["last_read_at"] = nil
	}
	if tried := parseTime(n.Admin["lastAttemptAt"]); !tried.IsZero() && state == monitorNoAnswer {
		m["last_tried_at"] = stamp(tried)
		m["last_tried_ago"] = ageText(now.Sub(tried))
	}
	if v, at, ok := n.battery(); ok {
		m["battery_volts"] = round(v, 2)
		if now.Sub(at) > readingCurrentWithin {
			m["battery_is_current"] = false
		}
	}
	if via, _ := n.Admin["via"].(string); via != "" && names != nil {
		m["read_through"] = names(strings.ToLower(via))
	}
	return m
}

// reading is the full last monitor reading, for one node's report. Every
// gauge keeps its null: a battery the monitor could not read is unknown.
func (n meshNode) reading(now time.Time) map[string]interface{} {
	if n.Admin == nil {
		return nil
	}
	out := map[string]interface{}{}
	at := parseTime(n.Admin["lastSuccessAt"])
	if at.IsZero() {
		at = parseTime(n.Admin["reportedAt"])
	}
	if !at.IsZero() {
		out["read_at"] = stamp(at)
		out["read_ago"] = ageText(now.Sub(at))
		out["is_current"] = now.Sub(at) <= readingCurrentWithin
		if now.Sub(at) > readingCurrentWithin {
			out["note"] = "This reading is " + ageText(now.Sub(at)) + " old: it describes the node then, not now."
		}
	}
	for _, f := range []struct{ from, to string }{
		{"batteryVolts", "battery_volts"}, {"batteryPercent", "battery_percent"},
		{"temperatureC", "temperature_c"}, {"noiseFloorDbm", "noise_floor_dbm"},
		{"lastSnrDb", "last_snr_db"}, {"lastRssiDbm", "last_rssi_dbm"},
	} {
		out[f.to] = n.Admin[f.from] // nil stays nil
	}
	if up, ok := int64Field(n.Admin["uptimeSeconds"]); ok && up > 0 {
		out["uptime"] = ageText(time.Duration(up) * time.Second)
	}
	return out
}

// int64Field reads a 64-bit proto field, which arrives as a JSON string.
func int64Field(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	case float64:
		return int64(x), true
	}
	return 0, false
}

/* ------------------------------------------------------------------ wording */

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ageText is a duration as a person says it: "12 min", "5 h", "3 days".
func ageText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Hour:
		m := int(math.Round(d.Minutes()))
		if m < 1 {
			m = 1
		}
		return fmt.Sprintf("%d min", m)
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(math.Round(d.Hours())))
	}
	return fmt.Sprintf("%d days", int(math.Round(d.Hours()/24)))
}

// linkStrength bands a link by the best SNR heard on it (MeshCore decodes
// down to about -7.5 dB at SF8). The API sends 0 when none was measured.
func linkStrength(snr float64) string {
	switch {
	case snr == 0:
		return "unmeasured"
	case snr >= 8:
		return "strong"
	case snr >= 3:
		return "fair"
	}
	return "weak"
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// distanceKm is the great-circle distance between two points.
func distanceKm(lat1, lng1, lat2, lng2 float64) float64 {
	const r = 6371.0
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLng := rad(lat2-lat1), rad(lng2-lng1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(rad(lat1))*math.Cos(rad(lat2))*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(a)))
}

/* ---------------------------------------------------------- telemetry trend */

type sample struct {
	at    time.Time
	value float64
}

// trend summarises one gauge over a window: where it started and ended, its
// range, and at most maxPoints evenly spaced means so a model sees the shape
// without the thousand samples behind it. nil when nothing was read.
func trend(points []sample, from, to time.Time, maxPoints int, places int) map[string]interface{} {
	if len(points) == 0 {
		return nil
	}
	sort.Slice(points, func(i, j int) bool { return points[i].at.Before(points[j].at) })
	first, last := points[0], points[len(points)-1]
	lo, hi := first.value, first.value
	for _, p := range points {
		lo, hi = math.Min(lo, p.value), math.Max(hi, p.value)
	}
	out := map[string]interface{}{
		"first": round(first.value, places), "first_at": stamp(first.at),
		"last": round(last.value, places), "last_at": stamp(last.at),
		"min": round(lo, places), "max": round(hi, places),
		"change":  round(last.value-first.value, places),
		"samples": len(points),
	}
	span := to.Sub(from)
	if maxPoints < 1 || span <= 0 {
		return out
	}
	step := span / time.Duration(maxPoints)
	sums, counts := make([]float64, maxPoints), make([]int, maxPoints)
	for _, p := range points {
		i := int(p.at.Sub(from) / step)
		if i < 0 {
			i = 0
		}
		if i >= maxPoints {
			i = maxPoints - 1
		}
		sums[i] += p.value
		counts[i]++
	}
	series := []interface{}{}
	for i := range sums {
		if counts[i] == 0 {
			continue // a gap stays a gap: no reading, no point
		}
		series = append(series, map[string]interface{}{
			"at": stamp(from.Add(time.Duration(i) * step)), "mean": round(sums[i]/float64(counts[i]), places),
		})
	}
	out["series"] = series
	out["series_step"] = ageText(step)
	return out
}

// meshNodeText is a node's record as the plain text the generic fetch tool
// returns: the same facts and the same caution as the grid_mesh_* tools.
func meshNodeText(n meshNode, now time.Time) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (MeshCore %s, short id %s)\n", n.label(), n.Type, shortID(n.Key))
	if at, kind := n.lastActive(now); !at.IsZero() {
		fmt.Fprintf(&sb, "Last activity: %s %s ago (%s). State: %s.\n", kind, ageText(now.Sub(at)), stamp(at), n.activityState(now))
	} else {
		sb.WriteString("Last activity: none on record. State: unknown (not down).\n")
	}
	if n.Status != "" && n.Status != "ACTIVE" {
		fmt.Fprintf(&sb, "Record: %s (no advert or relay reached the Grid for its presence window, about 14 h).\n", n.Status)
	}
	if m := n.monitorState(now); m != "" {
		fmt.Fprintf(&sb, "Monitor: %s", strings.ReplaceAll(m, "_", " "))
		if read := parseTime(n.Admin["lastSuccessAt"]); !read.IsZero() {
			fmt.Fprintf(&sb, "; last read %s ago (%s)", ageText(now.Sub(read)), stamp(read))
		}
		if v, at, ok := n.battery(); ok {
			fmt.Fprintf(&sb, "; battery %.2f V as of %s ago", v, ageText(now.Sub(at)))
		}
		sb.WriteString(".\n")
	} else {
		sb.WriteString("Monitor: none reads this node, so its battery and radio state are unknown.\n")
	}
	for _, x := range n.issues(now) {
		sb.WriteString("Note: " + x.text + "\n")
	}
	sb.WriteString("The Grid knows only what it has heard: silence is not proof a node is off. " +
		"Its links, a path to another node and its battery trend are the grid_mesh_node, grid_mesh_path and grid_mesh_telemetry tools.\n")
	return sb.String()
}
