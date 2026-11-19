package services

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/cache"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/lib/alerts"
	"github.com/sierragridteam/sierra-data/internal/lib/geo"
	"github.com/sierragridteam/sierra-data/internal/lib/routing"
)

func TestCHPIncidentOnHighway(t *testing.T) {
	tests := []struct {
		name     string
		location string
		highway  string
		meters   float64
		want     bool
	}{
		// 2026-11-18, CHP 261118MR0292 at (38.008802, -120.38848): a county
		// road 82 m from the Hwy 49 line. It closed Hwy 49 for 75 minutes.
		{"the real case: a county road 82 m off", "21151 Old Sonora Columbia Rd", "49", 82, false},
		{"named by its route, 60 m off", "Sr49 / Reynolds Ferry Rd", "49", 60, true},
		{"not named, 5 m off", "21151 Old Sonora Columbia Rd", "49", 5, true},
		{"not named, at the threshold", "Parrotts Ferry Rd / Sawmill Flat Rd", "49", 25, true},
		{"not named, just past it", "Parrotts Ferry Rd / Sawmill Flat Rd", "49", 25.5, false},
		{"no location text, on the line", "", "49", 3, true},
		{"no location text, off the line", "", "49", 40, false},
		{"another highway named, off the line", "Sr4 / Avery Sheep Ranch Rd", "49", 60, false},
		{"another highway named, on the line", "Sr4 / Avery Sheep Ranch Rd", "49", 7, true},
		{"the road's highway number is not known", "Sr49 / Reynolds Ferry Rd", "", 60, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, chpIncidentOnHighway(tt.location, tt.highway, tt.meters))
		})
	}
}

func TestNamesHighway(t *testing.T) {
	names := []struct{ location, highway string }{
		// The forms in a chp-only.kml capture of 2026-11-19.
		{"Sr49 / Reynolds Ferry Rd", "49"},
		{"Sr4 / Avery Sheep Ranch Rd", "4"},
		{"SR29 / Bottle Rock Rd", "29"},
		{"Sr49 N / Jackson St", "49"},
		{"Santa Fe Ave / Sr132", "132"},
		{"Hawkins Rd / Sr113", "113"},
		{"Us50 / N Echo Summit Rd", "50"},
		{"Us101 N / W Sunset Blvd Ofr", "101"},
		{"I5 N / Empire Ave", "5"},
		{"I580 W / Sr238 N I580 W Con", "238"},
		{"I580 W / Sr238 N I580 W Con", "580"},
		// Other ways of writing a route.
		{"SR 49 / Rawhide Rd", "49"},
		{"SR-49 at Tuttletown", "49"},
		{"State Route 49 / Parrotts Ferry Rd", "49"},
		{"Hwy 49 / Shaws Flat Rd", "49"},
		{"12345 Highway 49", "49"},
		{"CA-49 / Springfield Rd", "49"},
		{"Ca 4 / Pool Station Rd", "4"},
		{"I-5 / Sr4", "5"},
		{"I 80 / Donner Pass Rd", "80"},
		{"US 50 / Pioneer Trl", "50"},
		{"US-395 / Sr108", "395"},
		{"Sr049 / Rawhide Rd", "49"},
	}
	for _, tt := range names {
		assert.Truef(t, namesHighway(tt.location, tt.highway), "%q names %s", tt.location, tt.highway)
	}

	doesNot := []struct{ location, highway string }{
		// A number is a whole token.
		{"Sr49 / Reynolds Ferry Rd", "4"},
		{"Sr4 / Avery Sheep Ranch Rd", "49"},
		{"Sr149 / Sr70", "49"},
		{"Us101 N / Abbott", "1"},
		{"Us101 N / Abbott", "10"},
		{"I405 N / Skirball Center Dr", "5"},
		{"I405 N / Skirball Center Dr", "40"},
		// Numbers that are not routes.
		{"21151 Old Sonora Columbia Rd", "49"},
		{"4949 Main St", "49"},
		{"49 Pine St", "49"},
		{"Obyrnes Ferry Rd / Old O Byrnes Ferry Rd", "4"},
		{"9300 Mm40 E Sbd R93.00", "40"},
		{"J59 / La Grange Rd", "59"},
		{"Tuolumne County Chains", "49"},
		{"", "49"},
		{"Sr49 / Reynolds Ferry Rd", ""},
	}
	for _, tt := range doesNot {
		assert.Falsef(t, namesHighway(tt.location, tt.highway), "%q does not name %q", tt.location, tt.highway)
	}
}

// oldSonoraColumbiaRd is where CHP put incident 261118MR0292.
var oldSonoraColumbiaRd = geo.Point{Latitude: 38.008802, Longitude: -120.38848}

// metersEastAt is degrees of longitude for a distance east at a latitude.
func metersEastAt(lat, meters float64) float64 {
	return meters / (111320 * math.Cos(lat*math.Pi/180))
}

// hwy49Beside is a Hwy 49 section running due north-south, passing the given
// distance west of oldSonoraColumbiaRd. With no Google key the service uses
// the straight line between a road's ends, so this is the route's line.
func hwy49Beside(meters float64) config.MonitoredRoad {
	lng := oldSonoraColumbiaRd.Longitude - metersEastAt(oldSonoraColumbiaRd.Latitude, meters)
	return config.MonitoredRoad{
		ID: "hwy49-angels-sonora", Name: "Hwy 49", Section: "Angels Camp to Sonora",
		Origin:      config.Coordinates{Latitude: 38.03, Longitude: lng},
		Destination: config.Coordinates{Latitude: 37.99, Longitude: lng},
	}
}

// closedEnhancer stands in for the model: every incident it is shown has
// closed the road, as it read the power pole on Old Sonora Columbia Rd.
type closedEnhancer struct{}

func (closedEnhancer) EnhanceAlert(_ context.Context, raw alerts.RawAlert) (alerts.EnhancedAlert, error) {
	return alerts.EnhancedAlert{StructuredDescription: alerts.StructuredDescription{
		Details: raw.Description, Impact: "severe", RoadStatus: "closed", RestrictionDetails: "Roadway closed",
	}}, nil
}

func (closedEnhancer) HealthCheck(context.Context) error { return nil }

// chpKML is a chp-only.kml with one incident, in the feed's 2026 markup.
func chpKML(logNumber, location string, at geo.Point) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<kml xmlns="http://www.opengis.net/kml/2.2"><Document>
  <Placemark>
    <name> </name>
    <description><![CDATA[<div class="infowindow-content"><div class="iw-header"><div class="iw-header-left">CHP Incident %s</div></div>
<div class="iw-body"><h2 class="iw-title">1125-Traffic Hazard</h2>
<p class="iw-text">Nov 18 2026  2:41PM <br> %s</p>
<p class="iw-text">Nov 18 2026  2:44PM [2] ROAD CLOSED <br />Nov 18 2026  2:41PM [1] POWER POLE DOWN <br /></p></div></div>]]></description>
    <styleUrl>#chp</styleUrl>
    <Point><coordinates>%f,%f,0</coordinates></Point>
  </Placemark>
</Document></kml>`, logNumber, location, at.Longitude, at.Latitude)
}

func onHighwayService(t *testing.T, road config.MonitoredRoad, chp string) *RoadsService {
	t.Helper()
	doer := &statusInputDoer{}
	doer.setCHP(servePage(chp))
	parser := caltrans.NewFeedParser()
	parser.HTTPClient = doer
	cfg := &config.Config{Roads: config.RoadsConfig{
		RefreshInterval: 15 * time.Minute,
		MonitoredRoads:  []config.MonitoredRoad{road},
	}}
	return NewRoadsService(nil, parser, cache.NewCache(), cfg, closedEnhancer{})
}

// The 2026-11-18 defect, end to end. The incident's id, location text and
// point are CHP's; its type and log lines here are stand-ins. It is 82 m
// from the Hwy 49 line and on another road: Hwy 49 stays open, and the
// incident is still listed on it, as nearby.
func TestRefreshRoadData_IncidentOnAnotherRoadDoesNotCloseTheHighway(t *testing.T) {
	road := hwy49Beside(82)
	s := onHighwayService(t, road, chpKML("261118MR0292", "21151 Old Sonora Columbia Rd", oldSonoraColumbiaRd))

	// The fixture is what it says: 82 m from the line, inside the matcher's 100 m.
	line := geo.Polyline{Points: []geo.Point{
		{Latitude: road.Origin.Latitude, Longitude: road.Origin.Longitude},
		{Latitude: road.Destination.Latitude, Longitude: road.Destination.Longitude},
	}}
	d, err := geo.NewGeoUtils().PointToPolyline(oldSonoraColumbiaRd, line)
	require.NoError(t, err)
	require.InDelta(t, 82, d, 1)

	roads, err := s.refreshRoadData(testCtx())
	require.NoError(t, err)
	require.Len(t, roads, 1)
	hwy49 := roads[0]
	assert.Equal(t, api.RoadStatus_OPEN, hwy49.GetStatus(), "a closed county road is not a closed Hwy 49")
	assert.Empty(t, hwy49.GetStatusCauses())
	assert.Empty(t, hwy49.GetStatusExplanation())
	require.Len(t, hwy49.GetAlerts(), 1, "still an alert on the road")
	assert.Equal(t, "261118MR0292", hwy49.GetAlerts()[0].GetId())
	assert.Equal(t, api.AlertClassification_NEARBY, hwy49.GetAlerts()[0].GetClassification())
	assert.InDelta(t, 82, hwy49.GetAlerts()[0].GetDistanceToRouteMeters(), 1)
}

// An incident on the highway still sets its status: one CHP names by its
// route though its point is 60 m off the line, and one CHP does not name
// that sits 5 m from it.
func TestRefreshRoadData_IncidentOnTheHighwaySetsItsStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		meters   float64
		location string
	}{
		"named by its route, 60 m off": {60, "Sr49 / Reynolds Ferry Rd"},
		"not named, 5 m off":           {5, "21151 Old Sonora Columbia Rd"},
	} {
		t.Run(name, func(t *testing.T) {
			s := onHighwayService(t, hwy49Beside(tc.meters), chpKML("261118MR0301", tc.location, oldSonoraColumbiaRd))
			roads, err := s.refreshRoadData(testCtx())
			require.NoError(t, err)
			require.Len(t, roads, 1)
			hwy49 := roads[0]
			assert.Equal(t, api.RoadStatus_CLOSED, hwy49.GetStatus())
			require.Len(t, hwy49.GetStatusCauses(), 1)
			assert.Equal(t, "incident", hwy49.GetStatusCauses()[0].GetSource())
			assert.Equal(t, "261118MR0301", hwy49.GetStatusCauses()[0].GetSourceId())
			require.Len(t, hwy49.GetAlerts(), 1)
			assert.Equal(t, api.AlertClassification_ON_ROUTE, hwy49.GetAlerts()[0].GetClassification())
		})
	}
}

// The rule narrows the matcher's ON_ROUTE and nothing else: a route named
// from farther than 100 m stays NEARBY (Hwy 49 outside the monitored
// section names the route too), and a lane closure, which carries its
// route, is not put through it.
func TestProcessGlobalAlerts_OnHighwayRuleIsForCHPWithinTheMatchersThreshold(t *testing.T) {
	road := hwy49Beside(82)
	s := onHighwayService(t, road, "")
	routes := []routing.Route{s.buildRouteFromMonitoredRoad(testCtx(), road, "")}
	at := &api.Coordinates{Latitude: oldSonoraColumbiaRd.Latitude, Longitude: oldSonoraColumbiaRd.Longitude}
	chpHTML := func(location string) string {
		return `<h2 class="iw-title">1125-Traffic Hazard</h2><p class="iw-text">Nov 18 2026  2:41PM <br> ` + location + `</p>`
	}
	classify := func(in caltrans.CaltransIncident) routing.AlertClassification {
		t.Helper()
		byRoute, err := s.processGlobalAlerts(testCtx(), []caltrans.CaltransIncident{in}, routes)
		require.NoError(t, err)
		require.Len(t, byRoute[road.ID], 1)
		return byRoute[road.ID][0].Classification
	}

	assert.Equal(t, routing.Nearby, classify(caltrans.CaltransIncident{
		FeedType: caltrans.CHP_INCIDENT, Name: "CHP Incident 261118MR0292", Coordinates: at,
		DescriptionHtml: chpHTML("21151 Old Sonora Columbia Rd"),
	}), "CHP, 82 m off, not named")
	assert.Equal(t, routing.OnRoute, classify(caltrans.CaltransIncident{
		FeedType: caltrans.CHP_INCIDENT, Name: "CHP Incident 261118MR0301", Coordinates: at,
		DescriptionHtml: chpHTML("Sr49 / Shaws Flat Rd"),
	}), "CHP, 82 m off, named by its route")
	assert.Equal(t, routing.OnRoute, classify(caltrans.CaltransIncident{
		FeedType: caltrans.LANE_CLOSURE, Name: "Route 49 Lane Closure", Coordinates: at,
		DescriptionHtml: `<h2 class="iw-title">Route 49 Lane Closure</h2><p class="iw-text">From Shaws Flat Rd to Parrotts Ferry Rd</p>`,
	}), "a lane closure 82 m off is on the road, as before")

	far := &api.Coordinates{Latitude: at.Latitude, Longitude: at.Longitude + metersEastAt(at.Latitude, 400)}
	assert.Equal(t, routing.Nearby, classify(caltrans.CaltransIncident{
		FeedType: caltrans.CHP_INCIDENT, Name: "CHP Incident 261118MR0310", Coordinates: far,
		DescriptionHtml: chpHTML("Sr49 / Shaws Flat Rd"),
	}), "naming the route does not put an incident 480 m away on the road")
}
