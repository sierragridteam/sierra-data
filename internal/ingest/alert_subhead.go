package ingest

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Event.subhead for NWS alerts: what tells one alert from the others of the
// same product.
//
// NWS issues one product as several alerts, one per group of zones (in the
// Sierra, one per elevation band). Their headlines are the same line, because
// the headline is composed from the product name (nws.Alert.ShortHeadline),
// and it has to stay that way (see "Weather-alert headline" in AGENTS.md). In
// production on 2026-11-17 two events read "Winter Storm Warning": CAZ139,
// "1 to 3 feet above 5000 feet", and CAZ138, "up to a foot between 4000 and
// 5000 feet". In a list they were one card shown twice.
//
// The subhead has two writers and one rule about which wins:
//
//   - the enhancer's `distinction`, when the alert's content changed, budget
//     remained, the model returned one and acceptDistinction let it through.
//     enhancement.fields then lists "subhead".
//   - otherwise the alert's own zone names (zoneNamesSubhead), which need no
//     model and are set by the normalizer on every poll.
//
// Both only while the alert has a sibling in the poll. A lone alert has no
// subhead: there is nothing to tell it from.
//
// subhead is outside the content hash, like summary, so neither writer mints
// a revision. The store keeps a model-written subhead over a later
// deterministic one (refreshEventPlaces), and the normalizer lists an alert in
// PollResult.ForceWrite only when its stored subhead is not model-written and
// differs from this poll's, which is when a sibling arrived or left.

const (
	// maxDistinctionRunes is the cap on a model-written subhead. It sits on
	// one line beside the headline.
	maxDistinctionRunes = 60
	// maxZoneSubheadRunes is the cap on the zone-name subhead. Two of our
	// zones' names fit ("Sierra Nevada of Calaveras and Tuolumne Counties
	// Between 3000-5000 ft" is 69 characters); a third is counted.
	maxZoneSubheadRunes = 160
)

// alertSiblings returns the other weather alerts in events with ev's NWS
// event name (Event.category: "Winter Storm Warning"), in poll order. The
// poll's events are the alerts NWS lists now, less the ones an Update in the
// same list replaced, so a superseded copy is never a sibling.
func alertSiblings(events []*gridv1.Event, ev *gridv1.Event) []*gridv1.Event {
	product := strings.TrimSpace(ev.GetCategory())
	if product == "" || ev.GetLayer() != gridv1.Layer_WEATHER_ALERT {
		return nil
	}
	var out []*gridv1.Event
	for _, other := range events {
		if other == ev || other.GetId() == ev.GetId() || other.GetLayer() != gridv1.Layer_WEATHER_ALERT {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(other.GetCategory()), product) {
			out = append(out, other)
		}
	}
	return out
}

// zoneNamesSubhead is the deterministic subhead: the names, from the alert's
// own CAP areaDesc, of the zones it covers that are ours (weather.nws.zones).
//
// areaDesc is "name; name; ..." and geocode.UGC the zone codes, in the same
// order; nothing else in the alert ties a name to a code. So the two must be
// the same length, or which names are ours cannot be determined and the
// result is empty: an out-of-area zone's name ("Lassen Volcanic National
// Park") would be worse than none. Empty too when none of the alert's zones
// is ours (a zoneless alert).
//
// All of them when they fit in maxZoneSubheadRunes. Otherwise whole names
// while they fit and a count of the rest ("; +1 more"); a name longer than
// the room there is, is cut at a word.
func zoneNamesSubhead(areaDesc string, zones, ours []string) string {
	var names []string
	for _, n := range strings.Split(areaDesc, ";") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 || len(names) != len(zones) {
		return ""
	}
	want := make(map[string]bool, len(ours))
	for _, z := range ours {
		want[strings.ToUpper(strings.TrimSpace(z))] = true
	}
	var mine []string
	seen := map[string]bool{}
	for i, z := range zones {
		if want[strings.ToUpper(strings.TrimSpace(z))] && !seen[names[i]] {
			seen[names[i]] = true
			mine = append(mine, names[i])
		}
	}
	if len(mine) == 0 {
		return ""
	}
	if all := strings.Join(mine, "; "); utf8.RuneCountInString(all) <= maxZoneSubheadRunes {
		return all
	}
	if len(mine) == 1 {
		return cutAtWord(mine[0], maxZoneSubheadRunes)
	}
	// Not all fit: whole names while there is room for the count of the rest.
	room := maxZoneSubheadRunes - len("; +99 more")
	out := cutAtWord(mine[0], room)
	kept := 1
	for ; kept < len(mine); kept++ {
		next := out + "; " + mine[kept]
		if utf8.RuneCountInString(next) > room {
			break
		}
		out = next
	}
	return out + "; +" + strconv.Itoa(len(mine)-kept) + " more"
}

// cutAtWord shortens s to at most limit runes, ending on a whole word and an
// ellipsis.
func cutAtWord(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	cut := string(r[:limit-1])
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ;,") + "…"
}

var (
	// ugcCodeRE matches an NWS zone or county code (CAZ138, CAC009).
	ugcCodeRE = regexp.MustCompile(`(?i)\b[a-z]{2}[zc]\d{3}\b`)
	// dayRelativeRE matches the words NWSSystemPrompt rule 6 bans: a stored
	// subhead is read on later days, when they are wrong.
	dayRelativeRE = regexp.MustCompile(`(?i)\b(today|tonight|tomorrow|yesterday|this (morning|afternoon|evening|weekend))\b`)
	numberRE      = regexp.MustCompile(`\d+(?:\.\d+)?`)
	digitCommaRE  = regexp.MustCompile(`(\d),(\d)`)
)

// acceptDistinction returns the model's distinction trimmed, and whether it
// may be used as the alert's subhead. Anything refused falls back to
// zoneNamesSubhead; nothing is repaired, because a repaired line is one the
// model did not write and the alert did not say.
//
// The prompt asks for all of this (NWSSystemPrompt rules 11-14). What code can
// check, it checks:
//
//   - empty, longer than maxDistinctionRunes, or more than one line;
//   - the product name: it is the headline the subhead sits beside;
//   - a zone code, or a day-relative word;
//   - a number that is not in THIS alert's text. The model reads the siblings
//     to see what differs, and the failure that matters is taking a sibling's
//     amount ("1 to 3 feet") for this alert's ("up to a foot"). Thousands
//     separators are ignored, so "5,000 ft" matches the alert's "5000 feet".
func acceptDistinction(distinction string, ev *gridv1.Event) (string, bool) {
	d := strings.TrimSpace(distinction)
	if d == "" || strings.ContainsAny(d, "\n\r") || utf8.RuneCountInString(d) > maxDistinctionRunes {
		return "", false
	}
	if product := strings.TrimSpace(ev.GetCategory()); product != "" &&
		strings.Contains(strings.ToLower(d), strings.ToLower(product)) {
		return "", false
	}
	if ugcCodeRE.MatchString(d) || dayRelativeRE.MatchString(d) {
		return "", false
	}
	own := map[string]bool{}
	for _, n := range numbersIn(ev.GetHeadline() + " " + ev.GetDescription()) {
		own[n] = true
	}
	for _, n := range numbersIn(d) {
		if !own[n] {
			return "", false
		}
	}
	return d, true
}

// numbersIn lists the numbers written in s, thousands separators removed.
func numbersIn(s string) []string {
	for digitCommaRE.MatchString(s) {
		s = digitCommaRE.ReplaceAllString(s, "$1$2")
	}
	return numberRE.FindAllString(s, -1)
}

// applyAlertSubheads sets the deterministic subhead on every alert in the
// poll that has a sibling, and returns the ids whose STORED subhead it should
// replace although nothing hashed changed (PollResult.ForceWrite): stored
// alerts whose subhead no model wrote and which differs from this poll's. That
// is the tick a sibling arrives or leaves, and the first tick after this
// field shipped; on every other tick it lists nothing, so the hash-equal skip
// (shouldUpsert) still holds.
func applyAlertSubheads(events []*gridv1.Event, ours []string, prior Prior) []string {
	var force []string
	for _, ev := range events {
		ev.Subhead = ""
		if w := ev.GetWeatherAlert(); w != nil && len(alertSiblings(events, ev)) > 0 {
			ev.Subhead = zoneNamesSubhead(w.GetAreaDesc(), w.GetZones(), ours)
		}
		if prior == nil {
			continue
		}
		if held := prior.ByID(ev.GetId()); held != nil &&
			held.GetSubhead() != ev.GetSubhead() && !store.EnhancementWrote(held, "subhead") {
			force = append(force, ev.GetId())
		}
	}
	return force
}
