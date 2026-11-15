package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/search"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// Relevance on real text. testdata/search_corpus.json is 42 events as the
// public API served them on 2026-11-15 (shapes, AI request/response and mesh
// telemetry removed), chosen to cover what search has to get right: one road
// in the spellings the sources really use, incident types, fire and node
// names, identifiers, and near-misses that must NOT match. When a change to
// search.Build, the query parser or the tsvector expression moves a result,
// this is the test that says so. Add a case here before tuning anything.
func TestSearchRelevanceOnRealEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	raw, err := os.ReadFile("testdata/search_corpus.json")
	require.NoError(t, err)
	var corpus struct{ Events []json.RawMessage }
	require.NoError(t, json.Unmarshal(raw, &corpus))
	require.Len(t, corpus.Events, 42)
	seeded := map[string]bool{}
	for _, r := range corpus.Events {
		ev := &gridv1.Event{}
		require.NoError(t, protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(r, ev))
		if src := ev.GetProvenance().GetSourceId(); !seeded[src] {
			seeded[src] = true
			seedSource(t, s, src)
		}
		_, err := s.UpsertEvent(ctx, ev)
		require.NoError(t, err, ev.GetId())
	}
	finishSearchBackfill(t, s)

	const (
		sr108Abbrev   = "chp:260901MR0381"                           // "Sr108 / Brown Ct"
		hwy108Closure = "caltrans:d10-M108GA-0004-2026-11-10-083000" // "Hwy 108 ...", route "SR-108"
		limeKilnFlood = "chp:261114MR0161"                           // "Lime Kiln Rd at SR 108"
		limeKilnWater = "chp:261113MR0407"                           // "State Route 108 at Lime Kiln Road"
		soulsbyville  = "chp:261022MR0424"                           // "... at State Route 108, near Twain Harte"
		winterStorm   = "wx:urn:oid:2.49.0.1.840.0.02f6d8463f18ad66c73444c601b890a94b4bd8d6.001.1"
		sr4Stallion   = "chp:261113ST0403" // "State Route 4 at Stallion Way"
		sr4Appaloosa  = "chp:261113ST0187" // "SR 4 at Appaloosa Way"
		hwy4TreeWork  = "caltrans:d10-M4LA-0002-2026-11-17-080100"
		nodeHwy4      = "meshcore:f2c3a5a1a51ead0796a69c21edbc266373bc577bb2e9b471ab66bac14dfd4650" // named "HWY 4 V2"
		deerSR49      = "chp:261113ST0290"                                                          // "SR 49 at Middle Bar Road"
		deerUS50      = "chp:261113SA1174"
		deerfield     = "chp:261113MR0361" // "Gopher Dr / Deerfield Dr"
		us50Closure   = "caltrans:d3-C50BA-0001-2026-11-20-200100"
		priestFire    = "calfire:d2e2f55d-a944-489d-a8e0-70e134bc3381"
		oldPriest     = "chp:261112MR0043" // "Old Priest / State Route 120"
		domeFire      = "firis:dome"
		slinkFire     = "calfire:033439b0-d0ed-435a-a61b-d3dabb77d4b3" // "Highway 395 just south of Highway 89"
		nodeMarelink  = "meshcore:950ffe39d7087803864e0e1cdc77cc1463b4af54e8c0a904f7c79638a7910439"
		nodeArnold    = "meshcore:30059adfc99019769d6ee095ed2dbf81c6cf2b640d8fafb0a2aa6249558a9656" // "SIERRA Arnold Summit"
		nodeConnell   = "meshcore:ab074a85d45b976a6b7b35189d43781385a309e2dbb878cc6507687be2c3e95b" // "SIERRA Camp Connell"
		evacOrder     = "evac:US-CA-XCA-CCU-043"
		evacWarning   = "evac:US-CA-Toulumne118"
		pspsWarning   = "psps:23794:TP07_10142026"
		outage        = "pge:383765"
		poleWires     = "chp:260925MR0672" // "pole and power lines down"
		bigRigWires   = "chp:260821MR0290" // "Big rig struck power lines; wires down"
		teslaPole     = "chp:261106ST0458"
		i80Hazard     = "chp:260731SA0372" // "eastbound I-80 at Elm Avenue"
		chainR2       = "chp:261113SA0837"
		mudRockSlide  = "chp:261101SA0803"
		rockMudSlide  = "chp:261022MR0299"
		burnCalaveras = "burn:calaveras-county"
	)
	all108 := []string{sr108Abbrev, hwy108Closure, limeKilnFlood, limeKilnWater, soulsbyville}

	cases := []struct {
		q       string
		typing  bool
		must    []string
		mustNot []string
	}{
		// One road, every way the sources write it, by every way people type it.
		{q: "hwy 108", must: all108, mustNot: []string{sr4Stallion, deerSR49, us50Closure, winterStorm}},
		{q: "sr108", must: all108},
		{q: "SR-108", must: all108},
		{q: "state route 108", must: all108},
		{q: "highway 108", must: all108},
		{q: "ca-108", must: all108},
		{q: "route 108", must: all108},
		// Route 4 is not route 49, 140 or 50; a node named for the road counts.
		{q: "hwy 4", must: []string{sr4Stallion, sr4Appaloosa, hwy4TreeWork, nodeHwy4},
			mustNot: []string{deerSR49, rockMudSlide, us50Closure, limeKilnFlood}},
		{q: "us 50", must: []string{us50Closure, deerUS50}, mustNot: []string{sr4Stallion}},
		{q: "i-80", must: []string{i80Hazard}, mustNot: []string{sr4Stallion}},
		{q: "us 395", must: []string{slinkFire}},
		// A road plus what happened.
		{q: "sr 108 flooding", must: []string{limeKilnFlood, limeKilnWater, soulsbyville}, mustNot: []string{sr108Abbrev, hwy108Closure}},
		{q: "lime kiln flooded", must: []string{limeKilnFlood, limeKilnWater}, mustNot: []string{soulsbyville}},
		{q: "hwy 4 crash", must: []string{sr4Stallion, sr4Appaloosa}, mustNot: []string{hwy4TreeWork, nodeHwy4}},
		{q: "chain control r2", must: []string{chainR2}},
		// Whole words for a finished question; a prefix only while typing.
		{q: "deer", must: []string{deerSR49, deerUS50}, mustNot: []string{deerfield}},
		{q: "deer", typing: true, must: []string{deerSR49, deerUS50, deerfield}},
		{q: "floo", mustNot: []string{limeKilnFlood}},
		{q: "floo", typing: true, must: []string{limeKilnFlood, limeKilnWater, soulsbyville}},
		// Names.
		{q: "priest fire", must: []string{priestFire}, mustNot: []string{oldPriest}},
		{q: "priest", must: []string{priestFire, oldPriest}},
		{q: "dome", must: []string{domeFire}},
		{q: "marelink", must: []string{nodeMarelink}, mustNot: []string{nodeArnold}},
		{q: "sierra arnold", must: []string{nodeArnold}, mustNot: []string{nodeConnell}},
		{q: "sierra", must: []string{nodeArnold, nodeConnell}},
		// Identifiers: a pubkey prefix while typing, a zone id, an outage id.
		{q: "950ffe39", typing: true, must: []string{nodeMarelink}},
		{q: "CCU-043", must: []string{evacOrder}, mustNot: []string{evacWarning}},
		{q: "US-CA-XCA-CCU-043", must: []string{evacOrder}},
		{q: "383765", must: []string{outage}},
		// Types and alternatives.
		{q: "psps", must: []string{pspsWarning}, mustNot: []string{outage}},
		{q: "evacuation warning", must: []string{evacWarning}},
		{q: "wires down OR power lines", must: []string{poleWires, bigRigWires, teslaPole}},
		{q: `"power lines"`, must: []string{poleWires, bigRigWires}},
		{q: "rock slide", must: []string{mudRockSlide, rockMudSlide}},
		{q: "winter storm warning", must: []string{winterStorm}},
		{q: "burn day calaveras", must: []string{burnCalaveras}},
		// Exclusion.
		{q: "sr 108 -flooding", must: []string{sr108Abbrev, hwy108Closure}, mustNot: []string{limeKilnFlood, limeKilnWater, soulsbyville}},
	}
	for _, c := range cases {
		q, err := search.ParseQuery(c.q, c.typing)
		require.NoError(t, err, c.q)
		p, err := s.SearchEvents(ctx, EventQuery{Search: &q, Statuses: allStatuses, PageSize: 200})
		require.NoError(t, err, c.q)
		got := map[string]bool{}
		for _, ev := range p.Items {
			got[ev.GetId()] = true
		}
		name := c.q
		if c.typing {
			name += " (typing)"
		}
		for _, id := range c.must {
			assert.True(t, got[id], "%q should find %s", name, id)
		}
		for _, id := range c.mustNot {
			assert.False(t, got[id], "%q should not find %s", name, id)
		}
	}
}
