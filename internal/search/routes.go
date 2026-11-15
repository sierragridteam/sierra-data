package search

import (
	"regexp"
	"strconv"
	"strings"
)

// Numbered roads. The sources spell one road many ways; across the stored
// events on 2026-11-15 the forms were, most common first: "Sr108", "Route 108",
// "State Route 108", "SR 108", "US 50", "SR-108", "Hwy 108", "Us50",
// "Highway 108", "I-80", "I80", "Rte 108", "CA-108". People add their own
// ("hwy4", "highway 4").
//
// Every form maps to one token, "route108", which Build appends to the indexed
// text and ParseQuery substitutes into the query. The number alone is not a
// road: "4" stays "4".
var (
	// Case-insensitive designators. "us" and "i" are handled apart because
	// they are also English words.
	routeWord = regexp.MustCompile(`(?i)\b(?:state\s+(?:route|rte|hwy|highway)|sr|ca|hwy|highway|route|rte|interstate)[\s-]?(\d{1,3})\b`)
	// US 50, US-50, Us395, us50: in running text the two-letter word needs a
	// capital, a hyphen or no gap, so "tell us 3 things" is not US Route 3.
	routeUSDoc = regexp.MustCompile(`\b(?:U[Ss][\s-]?|us-?)(\d{1,3})\b`)
	// I-80, I80, I 80: a capital I only, in running text.
	routeIDoc = regexp.MustCompile(`\bI[\s-]?(\d{1,3})\b`)
	// In a query nobody is writing prose: "us 50" and "i 80" are roads.
	routeUSIQuery = regexp.MustCompile(`(?i)\b(?:us|i)[\s-]?(\d{1,3})\b`)
)

func routeToken(number string) string {
	return "route" + strings.TrimLeft(number, "0")
}

// RouteTokens returns the canonical token of every numbered road text
// mentions, each once, in order of first mention.
func RouteTokens(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{routeWord, routeUSDoc, routeIDoc} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if t := routeToken(m[1]); t != "route" && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// rewriteRoutes replaces each road mention in a query with its token.
func rewriteRoutes(q string) string {
	sub := func(re *regexp.Regexp, s string) string {
		return re.ReplaceAllStringFunc(s, func(m string) string {
			t := routeToken(re.FindStringSubmatch(m)[1])
			if t == "route" {
				return m
			}
			return t
		})
	}
	return sub(routeUSIQuery, sub(routeWord, q))
}

// MarkRoutes prepares text for a match snippet. A query for a road holds the
// canonical token, which the text as written does not contain, so a snippet
// cut from the plain text could never highlight the road. MarkRoutes replaces
// each road mention with a placeholder word, "route108x0" for the first
// mention, and returns the mentions as written; RoutePlaceholder is the prefix
// a snippet query matches them by, and RestoreRoutes puts the original
// spelling back afterwards.
func MarkRoutes(text string) (marked string, mentions []string) {
	for _, re := range []*regexp.Regexp{routeWord, routeUSDoc, routeIDoc} {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			t := routeToken(re.FindStringSubmatch(m)[1])
			if t == "route" {
				return m
			}
			mentions = append(mentions, m)
			return t + "x" + strconv.Itoa(len(mentions)-1)
		})
	}
	return text, mentions
}

// RoutePlaceholder is the prefix of every placeholder MarkRoutes writes for a
// road token: "route4x" for "route4" (never matching route 49's "route49x").
func RoutePlaceholder(token string) string { return token + "x" }

var routePlaceholder = regexp.MustCompile(`route\d+x(\d+)`)

// RestoreRoutes replaces the placeholders in s with the mentions MarkRoutes
// took out.
func RestoreRoutes(s string, mentions []string) string {
	if len(mentions) == 0 {
		return s
	}
	return routePlaceholder.ReplaceAllStringFunc(s, func(m string) string {
		i, err := strconv.Atoi(routePlaceholder.FindStringSubmatch(m)[1])
		if err != nil || i >= len(mentions) {
			return m
		}
		return mentions[i]
	})
}

// IsRouteToken reports whether a query word is a canonical road token.
func IsRouteToken(w string) bool {
	if !strings.HasPrefix(w, "route") || len(w) == len("route") {
		return false
	}
	for _, r := range w[len("route"):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
