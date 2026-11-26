package hazards

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/cache"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/config"
)

// ebbettsGates is the Ebbetts Pass winter closure as prefab.yaml configures
// its gates (TestPrefabConfiguresTheEbbettsGates holds the two together).
func ebbettsGates() *config.SeasonalClosureGates {
	g := &config.SeasonalClosureGates{Highway: "4", Names: []string{"Ebbetts Pass", "Ebbets Pass", "Ebbett Pass"}}
	g.Bounds.South, g.Bounds.West, g.Bounds.North, g.Bounds.East = 38.44, -120.02, 38.68, -119.75
	return g
}

func ebbettsConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Roads.MonitoredRoads = []config.MonitoredRoad{{
		ID: "hwy4-arnold-bearvalley",
		SeasonalClosures: []config.SeasonalClosure{{Name: "Ebbetts Pass winter closure", Gates: ebbettsGates()}},
	}}
	return cfg
}

func TestSeasonalClosureOf(t *testing.T) {
	closures := seasonalGates(ebbettsConfig())
	require.Len(t, closures, 1)
	at := func(lat, lng float64) *api.Coordinates { return &api.Coordinates{Latitude: lat, Longitude: lng} }
	mtReba, lakeAlpine, raymond := at(38.48042, -120.01496), at(38.48135, -119.99421), at(38.58947, -119.78531)
	bearValley, arnold := at(38.461045, -120.042368), at(38.2550, -120.3510)

	for _, tc := range []struct {
		name string
		c    caltrans.ChainControlData
		want bool
	}{
		// Every gate Caltrans has listed for the pass (cc.kml, 2026-11-13 and 2026-11-26).
		{"west gate at Mount Reba Road", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "MOUNT REBA ROAD - EBBETTS PASS", Coordinates: mtReba}, true},
		{"west gate moved to Lake Alpine", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "EAST END OF LAKE ALPINE - EBBETTS PASS", Coordinates: lakeAlpine}, true},
		{"east gate at Raymond Meadows", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "RAYMOND MEADOWS - EBBETTS PASS", Coordinates: raymond}, true},
		// The name alone: a gate at a spot outside the box, or with no position.
		{"named, outside the box", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "BEAR VALLEY - EBBETTS PASS", Coordinates: bearValley}, true},
		{"named, no position", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "Mount Reba Road - Ebbetts Pass"}, true},
		// The position alone: a gate under a name that does not say the pass.
		{"in the box, new name", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "SILVER CREEK CAMPGROUND", Coordinates: at(38.58, -119.80)}, true},
		{"in the box, no name", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", Coordinates: lakeAlpine}, true},
		// Wording: case, punctuation, the apostrophe, the one-t spelling, the route's other names.
		{"apostrophe and periods", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "Mt. Reba Rd. - Ebbett's Pass"}, true},
		{"one t", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "MT REBA TURNOFF/EBBETS PASS"}, true},
		{"slashes", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "JCT OF SR 207 /MT REBA TURNOFF/ TO JCT OF SR 89 /EBBETTS PASS/"}, true},
		{"SR 4", caltrans.ChainControlData{Closed: true, Highway: "SR 4", LocationName: "MOUNT REBA ROAD - EBBETTS PASS"}, true},
		{"SR-4", caltrans.ChainControlData{Closed: true, Highway: "SR-4", LocationName: "MOUNT REBA ROAD - EBBETTS PASS"}, true},

		// Not a closure point: a chain sign at the gate is a chain requirement.
		{"a chain sign at the gate", caltrans.ChainControlData{Level: "R2", Highway: "Highway 4", LocationName: "MOUNT REBA ROAD - EBBETTS PASS", Coordinates: mtReba}, false},
		// Another highway, even inside the box or naming the pass.
		{"Highway 89 at Monitor Pass", caltrans.ChainControlData{Closed: true, Highway: "Highway 89", LocationName: "MONITOR PASS", Coordinates: at(38.66055, -119.72635)}, false},
		{"Highway 89 in the box", caltrans.ChainControlData{Closed: true, Highway: "Highway 89", LocationName: "JCT SR 4", Coordinates: at(38.60, -119.78)}, false},
		{"Highway 49 is not Highway 4", caltrans.ChainControlData{Closed: true, Highway: "Highway 49", LocationName: "EBBETTS PASS RD"}, false},
		{"no highway", caltrans.ChainControlData{Closed: true, LocationName: "MOUNT REBA ROAD - EBBETTS PASS", Coordinates: mtReba}, false},
		// On Highway 4 but on the monitored segment, and not naming the pass: a real closure.
		{"a closure in Arnold", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "ARNOLD", Coordinates: arnold}, false},
		{"a closure at Bear Valley", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "BEAR VALLEY", Coordinates: bearValley}, false},
		// A name is whole words: "Ebbetts Passage" does not name the pass.
		{"not the whole name", caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "EBBETTS PASSAGE LODGE", Coordinates: arnold}, false},
		// Sonora Pass, the other winter gate in the two counties, is not configured.
		{"Sonora Pass on Highway 108", caltrans.ChainControlData{Closed: true, Highway: "Highway 108", LocationName: "SNOW PARK - SONORA PASS", Coordinates: at(38.25237, -119.99796)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := seasonalClosureOf(tc.c, closures)
			if tc.want {
				assert.Equal(t, "Ebbetts Pass winter closure", got)
			} else {
				assert.Empty(t, got)
			}
		})
	}

	// Nothing configured: nothing is a gate.
	assert.Empty(t, seasonalClosureOf(caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "MOUNT REBA ROAD - EBBETTS PASS", Coordinates: mtReba}, nil))
	// No box configured: the names still work, the position does not.
	named := []seasonalGate{{name: "x", gates: config.SeasonalClosureGates{Highway: "4", Names: []string{"Ebbetts Pass"}}}}
	assert.Equal(t, "x", seasonalClosureOf(caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "A - EBBETTS PASS"}, named))
	assert.Empty(t, seasonalClosureOf(caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "A", Coordinates: at(0, 0)}, named))
	// A blank name matches nothing.
	blank := []seasonalGate{{name: "x", gates: config.SeasonalClosureGates{Highway: "4", Names: []string{" "}}}}
	assert.Empty(t, seasonalClosureOf(caltrans.ChainControlData{Closed: true, Highway: "Highway 4", LocationName: "ARNOLD"}, blank))
}

// On the layer, a recognized gate says it is seasonal, carries the closure's
// name and is INFO; with no gates configured it is the plain closure point.
func TestChainControlLayer_SeasonalGate(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "testdata", "caltrans", "chain_controls_20261113_133700.kml"))
	require.NoError(t, err)
	p := &caltrans.FeedParser{HTTPClient: kmlDoer{200, string(b)}}
	p.UseCWWP2ChainControls(fixtureSource{loadCheckpoints(t, "cc_d10_20260930.json")}, []int{10})
	s := NewServiceWithAPIs(ebbettsConfig(), nil, nil, p, cache.NewCache())
	features, _, _, _, _, _ := s.BuildLayer(testCtx(), ebbettsArea, LayerChainControl)
	var gate *Properties
	closed := 0
	for i := range features {
		if features[i].Properties.Category == "closed" {
			closed++
		}
		if features[i].Properties.ID == "cc:10-2515" {
			gate = &features[i].Properties
		}
	}
	require.NotNil(t, gate, "the eastbound Highway 4 gate")
	assert.Equal(t, "closed", gate.Category)
	assert.True(t, gate.ChainControl.Seasonal)
	assert.Equal(t, "Ebbetts Pass winter closure", gate.ChainControl.Closure)
	assert.Equal(t, "Highway 4: Ebbetts Pass winter closure", gate.Headline)
	assert.Equal(t, SevInfo, gate.Severity)
	assert.Equal(t, 0, gate.SeverityRank)
	assert.Equal(t, "EAST END OF LAKE ALPINE - EBBETTS PASS", gate.AreaLabel, "the gate's own place is kept")
	// Every other closure point in the area is left as it was.
	for i := range features {
		if q := features[i].Properties; q.Category == "closed" && q.ID != "cc:10-2515" {
			assert.False(t, q.ChainControl.Seasonal, q.ID)
			assert.Equal(t, SevMinor, q.Severity, q.ID)
		}
	}
	t.Logf("%d closure points in the area", closed)
}

// prefab.yaml configures the gates this file tests with.
func TestPrefabConfiguresTheEbbettsGates(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "prefab.yaml"))
	require.NoError(t, err)
	y := string(b)
	assert.Contains(t, y, `highway: "4"`)
	assert.Contains(t, y, `names: ["Ebbetts Pass", "Ebbets Pass", "Ebbett Pass"]`)
	assert.Contains(t, y, `bounds: { south: 38.44, west: -120.02, north: 38.68, east: -119.75 }`)
}
