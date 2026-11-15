package search

import (
	"strings"
	"testing"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRouteTokensReadEverySpelling(t *testing.T) {
	// The forms found in stored events, plus what people type.
	for in, want := range map[string]string{
		"Lime Kiln Rd at SR 108, near Sonora": "route108",
		"Lime Kiln Rd / Sr108":                "route108",
		"State Route 26 westbound":            "route26",
		"Hwy 26 full closure":                 "route26",
		"SR-26":                               "route26",
		"Route 49 Mokelumne Hill":             "route49",
		"US 50 one-way traffic control":       "route50",
		"Us395 at Bridgeport":                 "route395",
		"US-50":                               "route50",
		"I-80 at Donner":                      "route80",
		"I80 Wb":                              "route80",
		"Highway 4":                           "route4",
		"hwy4":                                "route4",
		"CA-4":                                "route4",
		"Rte 88":                              "route88",
		"Interstate 5":                        "route5",
		"State Hwy 12":                        "route12",
	} {
		assert.Equal(t, []string{want}, RouteTokens(in), in)
	}
}

func TestRouteTokensLeaveProseAlone(t *testing.T) {
	for _, in := range []string{
		"tell us 3 things",        // not US Route 3
		"i 5 minutes late",        // lowercase i is a typo, not an interstate
		"3622-4027",               // an address range
		"Nov 14 2026 9:07AM",      // a date
		"lane 1 (of 2 lanes)",     // a lane
		"Evacuation zone CCU-043", // a zone id
		"4",
	} {
		assert.Empty(t, RouteTokens(in), in)
	}
}

func TestRouteTokensOncePerRoadInOrder(t *testing.T) {
	assert.Equal(t, []string{"route108", "route49"},
		RouteTokens("SR 108 at Sr49, then Hwy 108 again"))
}

func TestParseQuery(t *testing.T) {
	word := func(s string) Term { return Term{Text: s} }
	cases := []struct {
		in   string
		want []Group
	}{
		{"lime kiln ", []Group{{[]Term{word("lime"), word("kiln")}}}},
		{"Lime KILN", []Group{{[]Term{word("lime"), {Text: "kiln", Prefix: true}}}}},
		{"floo", []Group{{[]Term{{Text: "floo", Prefix: true}}}}},
		// Roads in any spelling become one token, never a prefix.
		{"hwy 4", []Group{{[]Term{word("route4")}}}},
		{"crash state route 108", []Group{{[]Term{word("crash"), word("route108")}}}},
		{"us 50 closure ", []Group{{[]Term{word("route50"), word("closure")}}}},
		{"sr4", []Group{{[]Term{word("route4")}}}},
		// A bare number is not a road and not a prefix.
		{"108", []Group{{[]Term{word("108")}}}},
		// Phrases keep their words; a road inside one is left as typed.
		{`"road flooded" sonora `, []Group{{[]Term{{Text: "road flooded", Phrase: true}, word("sonora")}}}},
		{`"hwy 4"`, []Group{{[]Term{{Text: "hwy 4", Phrase: true}}}}},
		{`“smart quotes”`, []Group{{[]Term{{Text: "smart quotes", Phrase: true}}}}},
		{`"unclosed phrase`, []Group{{[]Term{{Text: "unclosed phrase", Phrase: true}}}}},
		// Exclusions.
		{"outage -planned", []Group{{[]Term{word("outage"), {Text: "planned", Exclude: true}}}}},
		{`fire -"red flag" `, []Group{{[]Term{word("fire"), {Text: "red flag", Phrase: true, Exclude: true}}}}},
		// Alternatives.
		{"wires down OR power lines ", []Group{
			{[]Term{word("wires"), word("down")}}, {[]Term{word("power"), word("lines")}}}},
		{"or", nil},
		{"fire or", []Group{{[]Term{word("fire")}}}},
		// Identifiers survive: a node name with a hyphen, a pubkey prefix.
		{"sierra-12", []Group{{[]Term{word("sierra-12")}}}},
		{"950ffe39", []Group{{[]Term{{Text: "950ffe39", Prefix: true}}}}},
	}
	for _, c := range cases {
		q, err := ParseQuery(c.in, true)
		if c.want == nil {
			assert.ErrorIs(t, err, ErrQueryEmpty, c.in)
			continue
		}
		require.NoError(t, err, c.in)
		assert.Equal(t, c.want, q.Any, c.in)
	}
}

func TestParseQueryRejects(t *testing.T) {
	for _, in := range []string{"", "   ", `""`, "-fire", "- - -", "?!", "-a OR -b"} {
		_, err := ParseQuery(in, true)
		assert.ErrorIs(t, err, ErrQueryEmpty, "%q", in)
	}
	_, err := ParseQuery(strings.Repeat("a", MaxQueryLen+1), false)
	assert.ErrorIs(t, err, ErrQueryTooLong)
	_, err = ParseQuery(strings.Repeat("a ", maxTerms+1), false)
	assert.ErrorIs(t, err, ErrQueryTooLong)
}

func TestParseQueryDropsAnAllExcludedAlternative(t *testing.T) {
	q, err := ParseQuery("fire OR -smoke", false)
	require.NoError(t, err)
	assert.Equal(t, "fire", q.String())
}

func TestQueryStringEchoesWhatWasUnderstood(t *testing.T) {
	q, err := ParseQuery(`Crash on HWY 4 -"big rig" or sr-108`, false)
	require.NoError(t, err)
	assert.Equal(t, `crash on route4 -"big rig" OR route108`, q.String())
	assert.Equal(t, []string{"crash", "on", "route4", "route108"}, q.Words())
}

func TestBuildWeightsAndRoutes(t *testing.T) {
	ev := &gridv1.Event{
		Id: "chp:261114MR0161", Layer: gridv1.Layer_ROAD_INCIDENT, Category: "incident",
		Headline: "Road flooded and impassable", AreaLabel: "Lime Kiln Rd at SR 108, near Sonora",
		Summary:     "Road flooded at the intersection of Lime Kiln Road and SR 108.",
		Description: "CHP Incident 261114MR0161 1125-Traffic Hazard Lime Kiln Rd / Sr108 [1] ROAD FLOODED",
		Provenance:  &gridv1.Provenance{SourceName: "CHP", Attribution: "California Highway Patrol"},
		Enhancement: &gridv1.Enhancement{Request: "SECRET PROMPT", Response: "SECRET RESPONSE"},
		Detail: &gridv1.Event_RoadIncident{RoadIncident: &gridv1.RoadIncidentDetail{
			LogNumber: "261114MR0161", Impact: "severe",
			Metadata: map[string]string{"lanes_blocked": "all lanes", "incident_type": "road flooding"},
			Log:      []*gridv1.IncidentLogEntry{{Text: "ROAD FLOODED"}},
		}},
	}
	d := Build(ev)
	a, b, c, dd := d.Indexed()
	assert.Equal(t, "Road flooded and impassable\nLime Kiln Rd at SR 108, near Sonora\n261114MR0161 route108", a)
	assert.Equal(t, "Road flooded at the intersection of Lime Kiln Road and SR 108.\nincident\nsevere\nroad flooding\nall lanes route108", b)
	assert.Contains(t, c, "ROAD FLOODED")
	assert.True(t, strings.HasSuffix(c, " route108"))
	assert.Equal(t, "CHP\nCalifornia Highway Patrol", dd)
	assert.Equal(t, "incident", d.Category)
	// What a snippet is cut from: the words as written, no identifiers,
	// typed facts or road tokens.
	assert.Equal(t, "Road flooded and impassable\nLime Kiln Rd at SR 108, near Sonora", d.Title)
	assert.Equal(t, "Road flooded at the intersection of Lime Kiln Road and SR 108.\n"+
		"CHP Incident 261114MR0161 1125-Traffic Hazard Lime Kiln Rd / Sr108 [1] ROAD FLOODED\nROAD FLOODED", d.Prose)
	assert.NotContains(t, d.Text(), "SECRET")
	// Deterministic: map order must not move the text (it is hashed).
	for i := 0; i < 20; i++ {
		assert.Equal(t, d, Build(ev))
	}
}

func TestBuildLeavesMeshTelemetryOut(t *testing.T) {
	ev := &gridv1.Event{
		Layer: gridv1.Layer_MESH, Category: "repeater", Headline: "Marelink West-1w",
		Detail: &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{
			PublicKey: "950ffe39d708", NodeType: "repeater", Name: "Marelink West-1w",
			Telemetry: &gridv1.MeshTelemetry{Gateways: []string{"0B0B037985E4"}},
		}},
	}
	before := Build(ev)
	assert.Contains(t, before.A, "950ffe39d708")
	assert.NotContains(t, before.Text(), "0B0B037985E4")
	ev.GetMesh().Telemetry = &gridv1.MeshTelemetry{Gateways: []string{"FFFF"}}
	assert.Equal(t, before, Build(ev), "a telemetry refresh must not change the search text")
}

func TestParseQueryMatchesWholeWordsUnlessTyping(t *testing.T) {
	q, err := ParseQuery("deer", false)
	require.NoError(t, err)
	assert.False(t, q.Any[0].Terms[0].Prefix)
	q, err = ParseQuery("deer", true)
	require.NoError(t, err)
	assert.True(t, q.Any[0].Terms[0].Prefix)
}

func TestMarkAndRestoreRoutes(t *testing.T) {
	in := "Crash on SR 4 at Hwy 49, then State Route 4 again; US 50 closed. Lane 4 open."
	marked, mentions := MarkRoutes(in)
	assert.Equal(t, "Crash on route4x0 at route49x1, then route4x2 again; route50x3 closed. Lane 4 open.", marked)
	assert.Equal(t, []string{"SR 4", "Hwy 49", "State Route 4", "US 50"}, mentions)
	assert.Equal(t, in, RestoreRoutes(marked, mentions))
	// A fragment holding only some placeholders still restores each rightly.
	assert.Equal(t, "then State Route 4 again", RestoreRoutes("then route4x2 again", mentions))
	assert.True(t, strings.HasPrefix("route4x2", RoutePlaceholder("route4")))
	assert.False(t, strings.HasPrefix("route49x1", RoutePlaceholder("route4")))
	plain, none := MarkRoutes("no roads here")
	assert.Equal(t, "no roads here", plain)
	assert.Empty(t, none)
}

func TestMatchSmallLists(t *testing.T) {
	q := func(s string, typing bool) Query {
		p, err := ParseQuery(s, typing)
		require.NoError(t, err, s)
		return p
	}
	type row struct{ name, where, rest string }
	rows := []row{
		{"Bear Valley", "SR-4 at Bear Valley", "Alpine County eastbound"},
		{"Arnold", "Hwy 4 : Arnold Byways", "Calaveras County"},
		{"Big Trees", "SR-4 at Big Trees Rd", "near Arnold"},
		{"Sonora Pass", "SR-108 summit", "Tuolumne County"},
		{"Ebbetts Pass Corridor", "", "ebbetts-pass AREA"},
	}
	find := func(query Query) []string {
		type hit struct {
			name  string
			score int
		}
		var hits []hit
		for _, r := range rows {
			if s, ok := query.Match(r.name, r.where, r.rest); ok {
				hits = append(hits, hit{r.name, s})
			}
		}
		// Stable, best first.
		for i := 1; i < len(hits); i++ {
			for j := i; j > 0 && hits[j].score > hits[j-1].score; j-- {
				hits[j], hits[j-1] = hits[j-1], hits[j]
			}
		}
		out := make([]string, len(hits))
		for i, h := range hits {
			out[i] = h.name
		}
		return out
	}
	// The name outranks a mention elsewhere.
	assert.Equal(t, []string{"Arnold", "Big Trees"}, find(q("arnold", false)))
	// A road in any spelling, against any spelling.
	assert.Equal(t, []string{"Bear Valley", "Arnold", "Big Trees"}, find(q("hwy 4", false)))
	assert.Equal(t, []string{"Sonora Pass"}, find(q("state route 108", false)))
	// Whole words, or a prefix while typing.
	assert.Empty(t, find(q("ebb", false)))
	assert.Equal(t, []string{"Ebbetts Pass Corridor"}, find(q("ebb", true)))
	// Every word, any field; phrases adjacent; exclusions; alternatives.
	assert.Equal(t, []string{"Bear Valley"}, find(q("valley alpine", false)))
	assert.Equal(t, []string{"Big Trees"}, find(q(`"big trees"`, false)))
	assert.Empty(t, find(q(`"trees big"`, false)))
	assert.Equal(t, []string{"Sonora Pass"}, find(q("pass -ebbetts", false)))
	assert.Equal(t, []string{"Arnold", "Sonora Pass", "Ebbetts Pass Corridor", "Big Trees"}, find(q("pass OR arnold", false)))
	// A slug with a hyphen is matched as its words.
	assert.Equal(t, []string{"Ebbetts Pass Corridor"}, find(q("ebbetts-pass", false)))
}
