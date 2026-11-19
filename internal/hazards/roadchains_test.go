package hazards

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/clients/cwwp2"
	"github.com/sierragridteam/sierra-data/internal/config"
)

// --- the membership rule, on paths small enough to check by hand ---

// eastWest is a 10 km road due east along latitude 38: about 0.1139 degrees
// of longitude. A hundredth of a degree of latitude is 1.11 km.
var eastWest = []LatLng{{Lat: 38, Lng: -120.1139}, {Lat: 38, Lng: -120.0570}, {Lat: 38, Lng: -120}}

func placedSign(highway string, lat, lng float64) caltrans.ChainControlData {
	return caltrans.ChainControlData{Highway: highway, Level: "R2", Coordinates: &api.Coordinates{Latitude: lat, Longitude: lng}}
}

func stretchOf(highway string, signs ...caltrans.ChainControlData) chainStretch {
	return chainStretch{Highway: highway, Level: "R2", Signs: signs}
}

func TestChainStretchOnRoad(t *testing.T) {
	const (
		onRoad      = -120.05   // a longitude halfway along
		pastOrigin  = -120.20   // 7.5 km west of the origin
		pastDest    = -119.90   // 8.8 km east of the destination
		within250m  = 38.0020   // 222 m north of the path
		beyond250m  = 38.0025   // 278 m north of the path
		farPastDest = -119.8000 // 17.5 km east of the destination
	)
	for _, tc := range []struct {
		name    string
		highway string
		st      chainStretch
		want    bool
	}{
		{"a sign on the path", "Hwy 4", stretchOf("Highway 4", placedSign("Highway 4", 38, onRoad)), true},
		{"a sign 222 m off the path", "Hwy 4", stretchOf("Highway 4", placedSign("Highway 4", within250m, onRoad)), true},
		{"a sign 278 m off the path", "Hwy 4", stretchOf("Highway 4", placedSign("Highway 4", beyond250m, onRoad)), false},
		{"one sign on the road is enough", "Hwy 4", stretchOf("Highway 4",
			placedSign("Highway 4", 38, onRoad), placedSign("Highway 4", 38, farPastDest)), true},
		{"reaches only the last 200 m of the road", "Hwy 4", stretchOf("Highway 4",
			placedSign("Highway 4", 38, -120.0023), placedSign("Highway 4", 38, pastDest)), false},
		{"reaches only the first 200 m of the road", "Hwy 4", stretchOf("Highway 4",
			placedSign("Highway 4", 38, pastOrigin), placedSign("Highway 4", 38, eastWest[0].Lng+0.0023)), false},
		{"two signs at one end and nothing past it", "Hwy 4", stretchOf("Highway 4",
			placedSign("Highway 4", 38, -120.0023), placedSign("Highway 4", 38, -120.0040)), true},

		{"Highway 49 is not Hwy 4", "Hwy 4", stretchOf("Highway 49", placedSign("Highway 49", 38, onRoad)), false},
		{"Hwy 4 is not Highway 49", "Hwy 49", stretchOf("Highway 4", placedSign("Highway 4", 38, onRoad)), false},
		{"Highway 4 written two ways", "Highway 4", stretchOf("Hwy 4", placedSign("Hwy 4", 38, onRoad)), true},
		{"US 4 is not Hwy 4", "Hwy 4", stretchOf("US 4", placedSign("US 4", 38, onRoad)), false},
		{"a stretch with no highway", "Hwy 4", stretchOf("", placedSign("", 38, onRoad)), false},
		{"a road with no highway number", "Main Street", stretchOf("Main Street", placedSign("Main Street", 38, onRoad)), false},

		{"spans the road: signs past both ends, none on it", "Hwy 4", stretchOf("Highway 4",
			placedSign("Highway 4", 38, pastOrigin), placedSign("Highway 4", 38, pastDest)), true},
		{"spans the road, listed high end first", "Hwy 4", stretchOf("Highway 4",
			placedSign("Highway 4", 38, pastDest), placedSign("Highway 4", 38, pastOrigin)), true},
		{"past the destination only", "Hwy 4", stretchOf("Highway 4",
			placedSign("Highway 4", 38, pastDest), placedSign("Highway 4", 38, farPastDest)), false},
		{"past the origin only", "Hwy 4", stretchOf("Highway 4", placedSign("Highway 4", 38, pastOrigin)), false},
		{"another highway spanning the road", "Hwy 4", stretchOf("Highway 49",
			placedSign("Highway 49", 38, pastOrigin), placedSign("Highway 49", 38, pastDest)), false},
		{"a sign with no position places nothing", "Hwy 4", stretchOf("Highway 4",
			caltrans.ChainControlData{Highway: "Highway 4", Level: "R2"}, placedSign("Highway 4", 38, pastDest)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chainStretchOnRoad(tc.highway, eastWest, tc.st))
		})
	}

	t.Run("no path, no answer", func(t *testing.T) {
		st := stretchOf("Highway 4", placedSign("Highway 4", 38, onRoad))
		assert.False(t, chainStretchOnRoad("Hwy 4", nil, st))
		assert.False(t, chainStretchOnRoad("Hwy 4", eastWest[:1], st))
	})

	// The known edge: two roads that meet. A sign 200 m short of the
	// junction is on the road it stands on and on the one that starts there.
	t.Run("a sign by a junction is on both roads", func(t *testing.T) {
		next := []LatLng{eastWest[2], {Lat: 38, Lng: -119.9}}
		byJunction := stretchOf("Highway 4", placedSign("Highway 4", 38, -120.0023))
		assert.True(t, chainStretchOnRoad("Hwy 4", eastWest, byJunction))
		assert.True(t, chainStretchOnRoad("Hwy 4", next, byJunction))
		clear := stretchOf("Highway 4", placedSign("Highway 4", 38, -120.0040)) // 350 m short
		assert.True(t, chainStretchOnRoad("Hwy 4", eastWest, clear))
		assert.False(t, chainStretchOnRoad("Hwy 4", next, clear))
	})
}

// --- the 2026-11-13 storm on the configured roads ---

// shippedRoads reads roads.monitoredRoads from the shipped prefab.yaml.
func shippedRoads(t *testing.T) []config.MonitoredRoad {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "prefab.yaml"))
	require.NoError(t, err)
	var doc struct {
		Roads struct {
			MonitoredRoads []struct {
				Name, Section, ID   string
				Origin, Destination struct{ Latitude, Longitude float64 }
			} `yaml:"monitoredRoads"`
		} `yaml:"roads"`
	}
	require.NoError(t, yaml.Unmarshal(b, &doc))
	var out []config.MonitoredRoad
	for _, r := range doc.Roads.MonitoredRoads {
		out = append(out, config.MonitoredRoad{Name: r.Name, Section: r.Section, ID: r.ID,
			Origin:      config.Coordinates{Latitude: r.Origin.Latitude, Longitude: r.Origin.Longitude},
			Destination: config.Coordinates{Latitude: r.Destination.Latitude, Longitude: r.Destination.Longitude}})
	}
	require.Len(t, out, 4, "three Hwy 4 sections and Hwy 49")
	return out
}

// roadCheckpoints are the Caltrans checkpoints that stand on each configured
// road, in the road's order of travel. The repository holds no route
// polyline for these roads (Google's are not ours to commit), so the test
// paths are hand-built: the configured origin, these checkpoints' positions
// from the capture, the configured destination. Straight lines between
// checkpoints are not the road, but every sign that matters here is a vertex
// or kilometres away. Checked on 2026-11-19 against the routes production
// serves: each of these sits within 30 m of its road's path, Murphys
// eastbound is 670 m past the end of Angels Camp to Murphys, and Blue Lake
// Springs westbound is 170 m short of the start of Arnold to Bear Valley.
var roadCheckpoints = map[string][]string{
	"hwy4-angels-murphys": {"10-CAL-4-27.17-E-01E"},
	"hwy4-murphys-arnold": {
		"10-CAL-4-30.05-E-02E", "10-CAL-4-32.43-E-03E", "10-CAL-4-33.69-E-04E", "10-CAL-4-35.48-W-04W",
		"10-CAL-4-36.6-E-05E", "10-CAL-4-39.25-E-06E", "10-CAL-4-41.08-W-06W", "10-CAL-4-41.64-E-07E",
		"10-CAL-4-42.53-W-07W",
	},
	"hwy4-arnold-bearvalley": {
		"10-CAL-4-42.68-E-08E", "10-CAL-4-44-E-08EA", "10-CAL-4-47.08-W-08W", "10-CAL-4-49.28-E-09E",
		"10-CAL-4-49.9-E-10E", "10-CAL-4-53-W-10W", "10-CAL-4-56.3-E-12E", "10-CAL-4-61.7-E-13E",
		"10-CAL-4-63.9-W-13W", "10-CAL-4-63.99-E-14E", "10-ALP-4-0.65-W-14W",
	},
	// Signed northbound, driven southbound: mileposts fall along this road.
	"hwy49-angels-sonora": {"10-TUO-49-22.3-S-02S", "10-TUO-49-19.1-N-01N"},
}

func roadPaths(t *testing.T, roads []config.MonitoredRoad, cps []cwwp2.ChainControl) map[string][]LatLng {
	t.Helper()
	at := map[string]LatLng{}
	for _, cp := range cps {
		if cp.Location.HasPosition {
			at[cp.ID] = LatLng{Lat: cp.Location.Latitude, Lng: cp.Location.Longitude}
		}
	}
	paths := map[string][]LatLng{}
	for _, mr := range roads {
		ids, ok := roadCheckpoints[mr.ID]
		require.True(t, ok, "no hand-built path for %s", mr.ID)
		path := []LatLng{{Lat: mr.Origin.Latitude, Lng: mr.Origin.Longitude}}
		for _, id := range ids {
			p, ok := at[id]
			require.True(t, ok, "checkpoint %s is not in the capture", id)
			path = append(path, p)
		}
		paths[mr.ID] = append(path, LatLng{Lat: mr.Destination.Latitude, Lng: mr.Destination.Longitude})
	}
	return paths
}

// countingSource counts the CWWP2 reads: one per registry read per district.
type countingSource struct {
	src   caltrans.ChainControlSource
	reads *atomic.Int32
}

func (c countingSource) ChainControls(ctx context.Context, district int) ([]cwwp2.ChainControl, error) {
	c.reads.Add(1)
	return c.src.ChainControls(ctx, district)
}

// roadService serves the configured roads, OPEN, each along its hand-built
// path, over the given chain-control sources.
func roadService(t *testing.T, kml kmlDoer, src caltrans.ChainControlSource, cps []cwwp2.ChainControl) *Service {
	t.Helper()
	roads := shippedRoads(t)
	paths := roadPaths(t, roads, cps)
	var served []*api.Road
	for _, mr := range roads {
		rd := &api.Road{Id: mr.ID, Status: api.RoadStatus_OPEN, CongestionLevel: api.CongestionLevel_CLEAR}
		for _, p := range paths[mr.ID] {
			rd.Polyline = append(rd.Polyline, &api.Coordinates{Latitude: p.Lat, Longitude: p.Lng})
		}
		served = append(served, rd)
	}
	p := &caltrans.FeedParser{HTTPClient: kml}
	p.UseCWWP2ChainControls(src, []int{10})
	cfg := &config.Config{Roads: config.RoadsConfig{MonitoredRoads: roads}}
	return NewServiceWithAPIs(cfg, fakeRoads{roads: served}, nil, p, nil)
}

// roadChainsByID is each road's `road` block as JSON, and its chains (nil
// when the feature carries none).
func roadChainsByID(t *testing.T, s *Service) (chains map[string]*[]RoadChainProps, raw map[string]map[string]json.RawMessage) {
	t.Helper()
	feats, err := s.roadSegments(testCtx(), ebbettsArea)
	require.NoError(t, err)
	require.Len(t, feats, 4)
	chains, raw = map[string]*[]RoadChainProps{}, map[string]map[string]json.RawMessage{}
	for _, f := range feats {
		id := f.Properties.Road.RoadID
		chains[id] = f.Properties.Road.Chains
		b, err := json.Marshal(f.Properties.Road)
		require.NoError(t, err)
		var block map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(b, &block))
		raw[id] = block
	}
	return chains, raw
}

func setLevel(t *testing.T, cps []cwwp2.ChainControl, id string, level cwwp2.Level, raw string) []cwwp2.ChainControl {
	t.Helper()
	out := append([]cwwp2.ChainControl(nil), cps...)
	for i := range out {
		if out[i].ID == id {
			out[i].Level, out[i].RawStatus = level, raw
			return out
		}
	}
	t.Fatalf("checkpoint %s is not in the capture", id)
	return nil
}

// The storm on the four configured roads: Hwy 4 was under R2 from Poison
// Springs to Mount Reba Road, which is on Arnold to Bear Valley and on
// nothing else. Hwy 88, 108 and 207 were under R2 too, on no monitored road.
func TestRoadChains_Storm(t *testing.T) {
	cps := loadCheckpoints(t, stormCWWP2)
	s := roadService(t, kmlDoer{200, loadKML(t, stormKML)}, fixtureSource{cps}, cps)
	chains, raw := roadChainsByID(t, s)

	require.NotNil(t, chains["hwy4-arnold-bearvalley"])
	assert.Equal(t, []RoadChainProps{{
		Level: "R2", EventID: "cc:hwy4:r2",
		Headline: "Hwy 4 chains required, Poison Springs to Mount Reba Road (R2)",
		From:     "Poison Springs", To: "Mount Reba Road",
		Since: "2026-11-13T13:45:34-08:00", Inferred: false,
	}}, *chains["hwy4-arnold-bearvalley"])
	assert.JSONEq(t, `[{"level": "R2", "eventId": "cc:hwy4:r2",
		"headline": "Hwy 4 chains required, Poison Springs to Mount Reba Road (R2)",
		"from": "Poison Springs", "to": "Mount Reba Road",
		"since": "2026-11-13T13:45:34-08:00", "inferred": false}]`, string(raw["hwy4-arnold-bearvalley"]["chains"]))

	// Known, and none: an empty list, which is not the same as no key.
	for _, id := range []string{"hwy4-murphys-arnold", "hwy4-angels-murphys", "hwy49-angels-sonora"} {
		require.NotNil(t, chains[id], id)
		assert.Empty(t, *chains[id], id)
		assert.JSONEq(t, `[]`, string(raw[id]["chains"]), id)
	}

	// The entry is the chain_control layer's stretch, field for field, and
	// the whole stretch: its high end is 2.9 km past the road's.
	layer, status, _, _, _, _ := chainService(kmlDoer{200, loadKML(t, stormKML)}, cps).BuildLayer(testCtx(), ebbettsArea, LayerChainControl)
	require.Equal(t, "OK", status)
	stretch := layer[0].Properties
	entry := (*chains["hwy4-arnold-bearvalley"])[0]
	assert.Equal(t, stretch.ID, entry.EventID)
	assert.Equal(t, stretch.Headline, entry.Headline)
	assert.Equal(t, stretch.Effective, entry.Since)
	assert.Equal(t, stretch.ChainControl.From.Place, entry.From)
	assert.Equal(t, stretch.ChainControl.To.Place, entry.To)
	assert.Equal(t, stretch.ChainControl.Level, entry.Level)
	assert.Equal(t, stretch.ChainControl.Inferred, entry.Inferred)

	// Every other stretch in the state that evening is on no monitored
	// road: 88, 89, 108 and 207 in District 10, and the cc.kml districts.
	read := s.roadChainRegistry(testCtx())
	require.NoError(t, read.err)
	paths := roadPaths(t, shippedRoads(t), cps)
	highways := map[string]bool{}
	for _, st := range read.stretches {
		highways[highwayKey(st.stretch.Highway)] = true
		for _, mr := range shippedRoads(t) {
			on := chainStretchOnRoad(mr.Name, paths[mr.ID], st.stretch)
			assert.Equal(t, st.entry.EventID == "cc:hwy4:r2" && mr.ID == "hwy4-arnold-bearvalley", on, "%s on %s", st.entry.EventID, mr.ID)
		}
	}
	for _, key := range []string{"hwy4", "hwy88", "hwy89", "hwy108", "hwy207", "i80", "us50"} {
		assert.True(t, highways[key], "the capture has a stretch on %s", key)
	}
}

// Chains do not change the road: with and without a chain-control source the
// feature is the same but for `chains`.
func TestRoadChains_LeaveTheRoadAlone(t *testing.T) {
	cps := loadCheckpoints(t, stormCWWP2)
	with := roadService(t, kmlDoer{200, loadKML(t, stormKML)}, fixtureSource{cps}, cps)
	without := roadService(t, kmlDoer{200, loadKML(t, stormKML)}, fixtureSource{cps}, cps)
	without.caltrans = nil

	a, err := with.roadSegments(testCtx(), ebbettsArea)
	require.NoError(t, err)
	b, err := without.roadSegments(testCtx(), ebbettsArea)
	require.NoError(t, err)
	require.Len(t, a, len(b))
	for i := range a {
		assert.Nil(t, b[i].Properties.Road.Chains, "no source: not known")
		require.NotNil(t, a[i].Properties.Road.Chains)
		assert.Equal(t, "OPEN", a[i].Properties.Status)
		assert.Equal(t, SevInfo, a[i].Properties.Severity)
		a[i].Properties.Road.Chains = nil
		assert.Equal(t, b[i], a[i])
	}
}

// What production showed on 2026-11-19: the requirement reaching down to
// Mill Creek. Still one stretch, still on Arnold to Bear Valley alone: Mill
// Creek is 9.9 km past the end of Murphys to Arnold.
func TestRoadChains_MillCreekToMountRebaRoad(t *testing.T) {
	cps := loadCheckpoints(t, stormCWWP2)
	cps = setLevel(t, cps, "10-CAL-4-49.9-E-10E", cwwp2.LevelR2, "R-2")
	cps = setLevel(t, cps, "10-CAL-4-53.01-E-11E", cwwp2.LevelR2, "R-2")
	chains, _ := roadChainsByID(t, roadService(t, kmlDoer{200, loadKML(t, stormKML)}, fixtureSource{cps}, cps))

	require.NotNil(t, chains["hwy4-arnold-bearvalley"])
	require.Len(t, *chains["hwy4-arnold-bearvalley"], 1)
	got := (*chains["hwy4-arnold-bearvalley"])[0]
	assert.Equal(t, "cc:hwy4:r2", got.EventID)
	assert.Equal(t, "Hwy 4 chains required, Mill Creek to Mount Reba Road (R2)", got.Headline)
	assert.Equal(t, "Mill Creek", got.From)
	assert.Equal(t, "Mount Reba Road", got.To)
	for _, id := range []string{"hwy4-murphys-arnold", "hwy4-angels-murphys", "hwy49-angels-sonora"} {
		require.NotNil(t, chains[id], id)
		assert.Empty(t, *chains[id], id)
	}
}

// The storm of 2026-11-18: R2 from Blue Lake Springs up. Blue Lake Springs
// westbound stands on Murphys to Arnold, 170 m short of its end. The stretch
// is Arnold to Bear Valley's; it only reaches the end of the road below
// (roadChainMinReachKm), which stays clear.
func TestRoadChains_StretchReachingTheRoadBelowIsNotOnIt(t *testing.T) {
	cps := loadCheckpoints(t, stormCWWP2)
	var from float64
	for _, c := range cps {
		if c.ID == "10-CAL-4-42.53-W-07W" {
			from = c.Milepost
		}
	}
	require.NotZero(t, from, "Blue Lake Springs westbound is in the capture")
	var set int
	for _, c := range cps {
		// Every Hwy 4 checkpoint in Calaveras from Blue Lake Springs up.
		if strings.HasPrefix(c.ID, "10-CAL-4-") && c.HasMilepost && c.Milepost >= from {
			cps = setLevel(t, cps, c.ID, cwwp2.LevelR2, "R-2")
			set++
		}
	}
	require.Greater(t, set, 2)
	chains, _ := roadChainsByID(t, roadService(t, kmlDoer{200, loadKML(t, stormKML)}, fixtureSource{cps}, cps))

	require.NotNil(t, chains["hwy4-arnold-bearvalley"])
	require.Len(t, *chains["hwy4-arnold-bearvalley"], 1)
	got := (*chains["hwy4-arnold-bearvalley"])[0]
	assert.Equal(t, "cc:hwy4:r2", got.EventID)
	assert.Equal(t, "Blue Lake Springs", got.From)
	for _, id := range []string{"hwy4-murphys-arnold", "hwy4-angels-murphys", "hwy49-angels-sonora"} {
		require.NotNil(t, chains[id], id)
		assert.Empty(t, *chains[id], id)
	}
}

// The known edge, on the real junction. Blue Lake Springs westbound stands
// on Murphys to Arnold, 170 m short of where Arnold to Bear Valley begins.
// At R1 it is a second stretch (the first on the highway, so it takes the
// bare id of its level), on both roads; the R2 above stays on one. The road
// with both lists the higher level first.
func TestRoadChains_JunctionSignIsOnBothRoads(t *testing.T) {
	cps := setLevel(t, loadCheckpoints(t, stormCWWP2), "10-CAL-4-42.53-W-07W", cwwp2.LevelR1, "R-1")
	chains, _ := roadChainsByID(t, roadService(t, kmlDoer{200, loadKML(t, stormKML)}, fixtureSource{cps}, cps))

	ids := func(road string) []string {
		require.NotNil(t, chains[road], road)
		var out []string
		for _, c := range *chains[road] {
			out = append(out, c.Level+" "+c.EventID)
		}
		return out
	}
	assert.Equal(t, []string{"R1 cc:hwy4:r1"}, ids("hwy4-murphys-arnold"))
	assert.Equal(t, []string{"R2 cc:hwy4:r2", "R1 cc:hwy4:r1"}, ids("hwy4-arnold-bearvalley"))
	assert.Empty(t, ids("hwy4-angels-murphys"))
	assert.Empty(t, ids("hwy49-angels-sonora"))
}

// Two stretches of one level on one road come in id order.
func TestRoadChainRead_OrdersByLevelThenID(t *testing.T) {
	read := roadChainRead{}
	for _, e := range []RoadChainProps{
		{Level: "R1", EventID: "cc:hwy4:r1"}, {Level: "R2", EventID: "cc:hwy4:r2:2"},
		{Level: "R3", EventID: "cc:hwy4:r3"}, {Level: "R2", EventID: "cc:hwy4:r2"},
	} {
		read.stretches = append(read.stretches, roadChainStretch{
			stretch: stretchOf("Highway 4", placedSign("Highway 4", 38, -120.05)), entry: e})
	}
	chains, known := read.on("Hwy 4", eastWest)
	require.True(t, known)
	var got []string
	for _, c := range chains {
		got = append(got, c.EventID)
	}
	assert.Equal(t, []string{"cc:hwy4:r3", "cc:hwy4:r2", "cc:hwy4:r2:2", "cc:hwy4:r1"}, got)
}

// --- known versus none: an error never becomes a 0 ---

// hasChains reports whether the road's JSON carries a `chains` key at all.
func hasChains(raw map[string]map[string]json.RawMessage, road string) bool {
	_, ok := raw[road]["chains"]
	return ok
}

var allRoads = []string{"hwy4-angels-murphys", "hwy4-murphys-arnold", "hwy4-arnold-bearvalley", "hwy49-angels-sonora"}

func TestRoadChains_AbsentWhenNotKnown(t *testing.T) {
	storm := loadCheckpoints(t, stormCWWP2)
	kml := kmlDoer{200, loadKML(t, stormKML)}

	t.Run("both sources failed", func(t *testing.T) {
		chains, raw := roadChainsByID(t, roadService(t, kmlDoer{500, ""}, failingSource{}, storm))
		for _, id := range allRoads {
			assert.Nil(t, chains[id], id)
			assert.False(t, hasChains(raw, id), "%s must carry no chains key, not []", id)
		}
	})

	// cc.kml alone still lists the storm's signs, and the chain_control
	// layer serves them as STALE. A road cannot be told "none" from it.
	t.Run("CWWP2 down, cc.kml answering", func(t *testing.T) {
		s := roadService(t, kml, failingSource{}, storm)
		read := s.roadChainRegistry(testCtx())
		var partial *caltrans.PartialError
		require.ErrorAs(t, read.err, &partial)
		_, raw := roadChainsByID(t, s)
		for _, id := range allRoads {
			assert.False(t, hasChains(raw, id), id)
		}
	})

	t.Run("cc.kml down, CWWP2 answering", func(t *testing.T) {
		_, raw := roadChainsByID(t, roadService(t, kmlDoer{503, ""}, fixtureSource{storm}, storm))
		for _, id := range allRoads {
			assert.False(t, hasChains(raw, id), id)
		}
	})

	// One Hwy 4 checkpoint we cannot read: every Hwy 4 road is unknown,
	// including the one whose R2 stretch we can see. Hwy 49 is not affected.
	t.Run("an unreadable sign on the road's highway", func(t *testing.T) {
		cps := setLevel(t, storm, "10-CAL-4-27.17-E-01E", cwwp2.LevelUnknown, "-118.1307759")
		chains, raw := roadChainsByID(t, roadService(t, kml, fixtureSource{cps}, cps))
		for _, id := range []string{"hwy4-angels-murphys", "hwy4-murphys-arnold", "hwy4-arnold-bearvalley"} {
			assert.False(t, hasChains(raw, id), id)
		}
		require.NotNil(t, chains["hwy49-angels-sonora"])
		assert.Empty(t, *chains["hwy49-angels-sonora"])
	})

	// A requirement with no position cannot be put on a road or off it.
	t.Run("a sign with no position on the road's highway", func(t *testing.T) {
		cps := setLevel(t, storm, "10-TUO-49-19.1-N-01N", cwwp2.LevelR2, "R-2")
		for i := range cps {
			if cps[i].ID == "10-TUO-49-19.1-N-01N" {
				cps[i].Location.HasPosition = false
			}
		}
		s := roadService(t, kml, fixtureSource{cps}, storm) // paths from the untouched capture
		chains, raw := roadChainsByID(t, s)
		assert.False(t, hasChains(raw, "hwy49-angels-sonora"))
		require.NotNil(t, chains["hwy4-arnold-bearvalley"])
		assert.Len(t, *chains["hwy4-arnold-bearvalley"], 1)
		require.NotNil(t, chains["hwy4-murphys-arnold"])
		assert.Empty(t, *chains["hwy4-murphys-arnold"])
	})

	// The two-point fallback line is not the road: nothing can be placed on
	// it, so nothing is claimed, and the registry is not even read.
	t.Run("a road with no measured path", func(t *testing.T) {
		reads := &atomic.Int32{}
		s := roadService(t, kml, countingSource{fixtureSource{storm}, reads}, storm)
		fake := s.roads.(fakeRoads)
		for _, rd := range fake.roads {
			rd.Polyline = nil
		}
		_, raw := roadChainsByID(t, s)
		for _, id := range allRoads {
			assert.False(t, hasChains(raw, id), id)
		}
		assert.Zero(t, reads.Load())
	})

	t.Run("a road the service has not measured yet", func(t *testing.T) {
		s := roadService(t, kml, fixtureSource{storm}, storm)
		s.roads = fakeRoads{}
		_, raw := roadChainsByID(t, s)
		for _, id := range allRoads {
			assert.False(t, hasChains(raw, id), id)
		}
	})
}

// A requirement whose highway we could not read matches no road by name.
// Standing on a road's path it makes that road unknown; anywhere else it
// changes nothing.
func TestRoadChainRead_UnnamedRequirementOnThePath(t *testing.T) {
	onPath := roadChainRead{stretches: []roadChainStretch{{stretch: stretchOf("", placedSign("", 38, -120.05))}}}
	chains, known := onPath.on("Hwy 4", eastWest)
	assert.False(t, known)
	assert.Nil(t, chains)

	elsewhere := roadChainRead{stretches: []roadChainStretch{{stretch: stretchOf("", placedSign("", 39, -121))}}}
	chains, known = elsewhere.on("Hwy 4", eastWest)
	assert.True(t, known)
	assert.NotNil(t, chains, "known and none is an empty list, never nil")
	assert.Empty(t, chains)

	// A blind sign with no highway could be on any road.
	blind := roadChainRead{blind: []caltrans.ChainControlData{{Unrecognized: true}}}
	_, known = blind.on("Hwy 4", eastWest)
	assert.False(t, known)

	// And a road whose name gives no highway number cannot be matched at all.
	_, known = roadChainRead{}.on("Parrotts Ferry Road", eastWest)
	assert.False(t, known)
}

// --- the cost of a request ---

// road_segment has no layer cache, so the registry read is kept: any number
// of requests inside the chain_control TTL are one read of Caltrans.
func TestRoadChains_OneRegistryReadPerTTL(t *testing.T) {
	storm := loadCheckpoints(t, stormCWWP2)
	reads := &atomic.Int32{}
	s := roadService(t, kmlDoer{200, loadKML(t, stormKML)}, countingSource{fixtureSource{storm}, reads}, storm)

	for i := 0; i < 5; i++ {
		chains, _ := roadChainsByID(t, s)
		require.NotNil(t, chains["hwy4-arnold-bearvalley"])
		assert.Len(t, *chains["hwy4-arnold-bearvalley"], 1)
	}
	// Corridor areas ask too (/summary reads road_segment for every place).
	_, err := s.roadSegments(testCtx(), config.HazardArea{ID: "hwy4-arnold-bearvalley"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, reads.Load(), "one district, one read")
	assert.WithinDuration(t, time.Now().Add(layerTTL(LayerChainControl)), s.roadChains.until, 5*time.Second)

	s.roadChains.until = time.Now().Add(-time.Second) // the TTL passes
	roadChainsByID(t, s)
	assert.EqualValues(t, 2, reads.Load())
}

// A failed read is remembered for a minute, so an outage is not a failing
// fetch on every request. It is never answered from the last good read, and
// the next read that succeeds is served.
func TestRoadChains_FailedReadIsRememberedBriefly(t *testing.T) {
	storm := loadCheckpoints(t, stormCWWP2)
	reads := &atomic.Int32{}
	var src caltrans.ChainControlSource = fixtureSource{storm}
	source := sourceFunc(func(ctx context.Context, d int) ([]cwwp2.ChainControl, error) {
		reads.Add(1)
		return src.ChainControls(ctx, d)
	})
	s := roadService(t, kmlDoer{200, loadKML(t, stormKML)}, source, storm)

	chains, _ := roadChainsByID(t, s)
	require.NotNil(t, chains["hwy4-arnold-bearvalley"])

	// Caltrans goes down as the TTL passes.
	src = failingSource{}
	s.roadChains.until = time.Now().Add(-time.Second)
	for i := 0; i < 3; i++ {
		_, raw := roadChainsByID(t, s)
		for _, id := range allRoads {
			assert.False(t, hasChains(raw, id), "%s: a failed read is not answered from the last good one", id)
		}
	}
	assert.EqualValues(t, 2, reads.Load(), "the failure is not refetched on every request")
	assert.WithinDuration(t, time.Now().Add(roadChainRetry), s.roadChains.until, 5*time.Second)

	// It answers again.
	src = fixtureSource{storm}
	s.roadChains.until = time.Now().Add(-time.Second)
	chains, _ = roadChainsByID(t, s)
	require.NotNil(t, chains["hwy4-arnold-bearvalley"])
	assert.Len(t, *chains["hwy4-arnold-bearvalley"], 1)
	assert.EqualValues(t, 3, reads.Load())
}

type sourceFunc func(context.Context, int) ([]cwwp2.ChainControl, error)

func (f sourceFunc) ChainControls(ctx context.Context, d int) ([]cwwp2.ChainControl, error) {
	return f(ctx, d)
}

// A caller that goes away mid-read has learned nothing about Caltrans: the
// failure is not kept, and the next request reads.
func TestRoadChains_CancelledCallerIsNotRemembered(t *testing.T) {
	storm := loadCheckpoints(t, stormCWWP2)
	ctx, cancel := context.WithCancel(testCtx())
	source := sourceFunc(func(c context.Context, _ int) ([]cwwp2.ChainControl, error) {
		if c.Err() != nil {
			return nil, c.Err()
		}
		return storm, nil
	})
	s := roadService(t, kmlDoer{200, loadKML(t, stormKML)}, source, storm)

	cancel()
	read := s.roadChainRegistry(ctx)
	require.Error(t, read.err)
	assert.True(t, s.roadChains.until.IsZero(), "nothing kept")

	chains, _ := roadChainsByID(t, s)
	require.NotNil(t, chains["hwy4-arnold-bearvalley"])
	assert.Len(t, *chains["hwy4-arnold-bearvalley"], 1)
}
