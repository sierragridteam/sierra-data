package hazards

import (
	"regexp"
	"strings"

	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/config"
)

// seasonalGate is one configured seasonal closure whose gates can be
// recognized among cc.kml's closure points.
type seasonalGate struct {
	name  string // the closure's name ("Ebbetts Pass winter closure")
	gates config.SeasonalClosureGates
}

// seasonalGates lists the configured seasonal closures that name their gates.
func seasonalGates(cfg *config.Config) []seasonalGate {
	if cfg == nil {
		return nil
	}
	var out []seasonalGate
	for _, mr := range cfg.Roads.MonitoredRoads {
		for _, sc := range mr.SeasonalClosures {
			if sc.Gates == nil || strings.TrimSpace(sc.Name) == "" || strings.TrimSpace(sc.Gates.Highway) == "" {
				continue
			}
			out = append(out, seasonalGate{name: strings.TrimSpace(sc.Name), gates: *sc.Gates})
		}
	}
	return out
}

var (
	routeNumber = regexp.MustCompile(`[0-9]+`)
	notWord     = regexp.MustCompile(`[^a-z0-9]+`)
)

// words lower-cases text and reduces it to words with one space between and
// around them, so that a name is found as whole words whatever the
// punctuation: "MT. REBA RD - EBBETT'S PASS" is " mt reba rd ebbetts pass ".
func words(text string) string {
	text = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(text))
	return " " + strings.TrimSpace(notWord.ReplaceAllString(text, " ")) + " "
}

// seasonalClosureOf names the seasonal closure a cc.kml closure point is a
// gate of, or "" when it is none. Only a closure point (Closed) on the
// closure's highway can be one. It is one when its place names the closure
// (any of the configured names, as whole words) or when it lies inside the
// closure's box; either is enough. Caltrans renames and moves the gate
// through the season, so neither test alone would hold: the name catches a
// gate at a new spot, the box a gate under a new name.
//
// The highway is compared as a route number: "Highway 4", "SR 4" and "SR-4"
// are 4, "Highway 49" is not.
func seasonalClosureOf(c caltrans.ChainControlData, closures []seasonalGate) string {
	if !c.Closed {
		return ""
	}
	route := routeNumber.FindString(c.Highway)
	if route == "" {
		return ""
	}
	place := words(c.LocationName)
	for _, sc := range closures {
		if routeNumber.FindString(sc.gates.Highway) != route {
			continue
		}
		for _, n := range sc.gates.Names {
			if n = words(n); n != "  " && strings.Contains(place, n) {
				return sc.name
			}
		}
		b := sc.gates.Bounds
		if (b.South != 0 || b.North != 0 || b.West != 0 || b.East != 0) && c.Coordinates != nil &&
			c.Coordinates.Latitude >= b.South && c.Coordinates.Latitude <= b.North &&
			c.Coordinates.Longitude >= b.West && c.Coordinates.Longitude <= b.East {
			return sc.name
		}
	}
	return ""
}
