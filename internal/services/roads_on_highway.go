package services

import (
	"regexp"
	"strings"
)

// chpOnHighwayMeters is how close to a road's line a CHP incident's point
// must be to count as on the highway when its location text does not name
// the route.
const chpOnHighwayMeters = 25.0

// routeMention finds a state, US or interstate route named in CHP location
// text and captures its number: "Sr49", "SR 49", "SR-49", "State Route 49",
// "Hwy 49", "Highway 49", "CA-49", "Us50", "US 50", "I5", "I-5". The number
// is a whole token: "Sr49" names 49 and not 4, and "Sr4" does not name 49.
// California numbers its routes once across the three systems, so the
// number alone says which road; the prefix only says the text means a route
// and not a house number or a county road ("J59").
var routeMention = regexp.MustCompile(`(?i)\b(?:sr|ca|hwy|highway|route|rte|us|i|interstate)[\s-]*(\d+)\b`)

// namesHighway reports whether CHP location text names the route with this
// number ("49").
func namesHighway(locationText, highwayNumber string) bool {
	highwayNumber = strings.TrimLeft(strings.TrimSpace(highwayNumber), "0")
	if highwayNumber == "" {
		return false
	}
	for _, m := range routeMention.FindAllStringSubmatch(locationText, -1) {
		if strings.TrimLeft(m[1], "0") == highwayNumber {
			return true
		}
	}
	return false
}

// chpIncidentOnHighway is the rule for when a CHP incident that the route
// matcher placed on a road (within its 100 m ON_ROUTE threshold) may set
// that road's status: only when the incident is on the highway itself.
// That is EITHER
//
//   - its own location text from the feed names the road's highway number
//     as a route (routeMention). locationText is CHP's raw location line
//     (parseIncidentDetail), never the model's rewrite of it; OR
//   - its point lies within chpOnHighwayMeters (25 m) of the road's line.
//
// Otherwise the incident stays on the road as a NEARBY alert and sets
// neither its status nor a cause.
//
// Why: on 2026-11-18 road:hwy49-angels-sonora read RESTRICTED and then
// CLOSED for 75 minutes (revisions 6 and 7) because of CHP incident
// 261118MR0292, "21151 Old Sonora Columbia Rd": a power pole down on that
// county road, which was closed. Its point (38.008802, -120.38848) was 82 m
// from the Hwy 49 line, inside the matcher's 100 m. Measured the same
// afternoon, the five CHP incidents that really were on the monitored
// highways ("Sr49 / Reynolds Ferry Rd", "Sr4 / Avery Sheep Ranch Rd", ...)
// were 1 to 7 m from the line.
//
// The trade-off: a CHP incident on the highway whose location is a street
// address that does not name the route, and whose point is geocoded more
// than 25 m off the line, reads as nearby and does not set the status. The
// reverse error is not closed either: an address on a road named after the
// highway ("Old Hwy 49"), or a point on a frontage road within 25 m, still
// counts as on it.
//
// This only narrows the matcher's ON_ROUTE; it never promotes an incident
// farther than 100 m, however its text reads (an incident on Hwy 49 beyond
// the monitored section names the route too). Lane closures do not come
// through here: CWWP2 and lcs2way.kml rows carry their route.
func chpIncidentOnHighway(locationText, highwayNumber string, distanceMeters float64) bool {
	return distanceMeters <= chpOnHighwayMeters || namesHighway(locationText, highwayNumber)
}
