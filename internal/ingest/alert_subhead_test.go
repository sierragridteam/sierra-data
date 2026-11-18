package ingest

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/nws"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// The two Winter Storm Warnings NWS Sacramento issued as one product on
// 2026-11-17 (urn:oid:…3dfb3581…001.2 and …003.1), as api.weather.gov served
// them: the alerts this field was added for. Both read "Winter Storm Warning".
const (
	wswAboveDesc = `* WHAT...As widespread snowfall has dissipated this evening, the
Winter Weather Advisory will be allowed to expire. Some lingering
light snow showers may continue for the next hour or two. For the
Winter Storm Warning, heavy snow expected. Total snow accumulations
of 1 to 3 feet above 5000 feet, with up to 4 feet at the highest
peaks. Total snow accumulations up to a foot between 4000 and 5000
feet. Light flurries to a dusting is possible as low as 3500 feet.
Winds gusting as high as 55 mph.

* WHERE...For the Winter Storm Warning, elevations above 4000 feet in
the Sierra Nevada and southern Cascades.

* WHEN...From 10 PM Tuesday to 4 AM PST Friday.`
	wswAboveArea = "Lassen Volcanic National Park; Sierra Nevada of Sierra-Nevada-Placer Counties Above 5000 ft Including Interstate 80; Sierra Nevada of El Dorado-Amador-Northern Alpine Counties Above 5000 ft Including Highway 50; Sierra Nevada of Calaveras-Tuolumne-Southern Alpine Counties Above 5000 ft"

	wswBetweenDesc = `* WHAT...Heavy snow expected. Total snow accumulations up to a foot
between 4000 and 5000 feet. Light flurries to a dusting is possible
as low as 3500 feet. Winds could gust as high as 40 mph.

* WHERE...Northern Sierra above 4000 feet, including Highway 50,
Interstate 80, State Route 4, State Route 108, Southern Cascades
above 4000 feet.

* WHEN...From 10 PM Tuesday to 4 AM PST Friday.`
	wswBetweenArea = "Sierra Nevada of Tehama-Butte-Plumas Counties Above 3000 ft; Sierra Nevada of Yuba-Sierra-Nevada-Placer Counties Between 3000-5000 ft Including Interstate 80; Sierra Nevada of El Dorado and Amador Counties Between 3000-5000 ft Including Highway 50; Sierra Nevada of Calaveras and Tuolumne Counties Between 3000-5000 ft"
)

var (
	wswAboveZones   = []string{"CAZ128", "CAZ133", "CAZ136", "CAZ139"}
	wswBetweenZones = []string{"CAZ130", "CAZ132", "CAZ135", "CAZ138"}
	ourZones        = []string{"CAZ137", "CAZ138", "CAZ139"} // prefab.yaml weather.nws.zones
)

func wswPair(now time.Time) []nws.Alert {
	return []nws.Alert{
		{
			ID: "urn:oid:2.49.0.1.840.0.3dfb.001.2", Event: "Winter Storm Warning", Severity: "Severe",
			Description: wswAboveDesc, AreaDesc: wswAboveArea, Zones: wswAboveZones,
			Effective: now.Add(-time.Hour), Expires: now.Add(48 * time.Hour),
		},
		{
			ID: "urn:oid:2.49.0.1.840.0.3dfb.003.1", Event: "Winter Storm Warning", Severity: "Severe",
			Description: wswBetweenDesc, AreaDesc: wswBetweenArea, Zones: wswBetweenZones,
			Effective: now.Add(-time.Hour), Expires: now.Add(48 * time.Hour),
		},
	}
}

func alertEvent(id, product string) *gridv1.Event {
	ev := schedEvent(id, "nws", gridv1.Layer_WEATHER_ALERT)
	ev.Headline, ev.Category = product, product
	ev.Detail = &gridv1.Event_WeatherAlert{WeatherAlert: &gridv1.WeatherAlertDetail{}}
	return ev
}

// Siblings are the other alerts of the same product in the same poll, and
// nothing else: not the alert itself, not another product, not another layer.
func TestAlertSiblings(t *testing.T) {
	a := alertEvent("wx:a", "Winter Storm Warning")
	b := alertEvent("wx:b", "Winter Storm Warning")
	c := alertEvent("wx:c", "winter storm warning ") // the product name, not its spelling
	wind := alertEvent("wx:wind", "Wind Advisory")
	quake := schedEvent("usgs:q", "usgs", gridv1.Layer_EARTHQUAKE)
	quake.Category = "Winter Storm Warning"
	unnamed := alertEvent("wx:u1", "")
	unnamed2 := alertEvent("wx:u2", "")
	poll := []*gridv1.Event{a, quake, wind, b, c, unnamed, unnamed2}

	assert.Equal(t, []*gridv1.Event{b, c}, alertSiblings(poll, a))
	assert.Equal(t, []*gridv1.Event{a, c}, alertSiblings(poll, b))
	assert.Empty(t, alertSiblings(poll, wind), "a lone product has no sibling")
	assert.Empty(t, alertSiblings(poll, quake), "only weather alerts have siblings")
	assert.Empty(t, alertSiblings(poll, unnamed), "alerts with no product name are not one product")
	assert.Empty(t, alertSiblings([]*gridv1.Event{a}, a))
}

func TestZoneNamesSubhead(t *testing.T) {
	// The production pair: each alert covers one of our zones, and its name
	// carries the elevation band that tells the two apart.
	assert.Equal(t, "Sierra Nevada of Calaveras-Tuolumne-Southern Alpine Counties Above 5000 ft",
		zoneNamesSubhead(wswAboveArea, wswAboveZones, ourZones))
	assert.Equal(t, "Sierra Nevada of Calaveras and Tuolumne Counties Between 3000-5000 ft",
		zoneNamesSubhead(wswBetweenArea, wswBetweenZones, ourZones))

	// Several of our zones: all their names, in the alert's order.
	assert.Equal(t, "Mid Slope; High Slope",
		zoneNamesSubhead("Valley; Mid Slope; High Slope", []string{"CAZ017", "caz138", " CAZ139 "}, ourZones))

	// Names and codes that do not pair up: which names are ours cannot be
	// determined, and an out-of-area name would be worse than none.
	assert.Empty(t, zoneNamesSubhead("Valley; Mid Slope", []string{"CAZ017", "CAZ138", "CAZ139"}, ourZones))
	assert.Empty(t, zoneNamesSubhead("", []string{"CAZ138"}, ourZones))
	assert.Empty(t, zoneNamesSubhead("Somewhere", nil, ourZones), "a zoneless alert")
	assert.Empty(t, zoneNamesSubhead("Valley", []string{"CAZ017"}, ourZones), "none of its zones is ours")
	assert.Empty(t, zoneNamesSubhead("Mid Slope", []string{"CAZ138"}, nil), "no configured zones")

	// More than fits: whole names, and a count of the rest.
	long := strings.Repeat("Sierra Nevada of Somewhere Counties Between 3000-5000 ft; ", 3)
	got := zoneNamesSubhead(strings.TrimSuffix(long, "; ")+"; Another Zone Above 5000 ft; Third Zone",
		[]string{"CAZ137", "CAZ017", "CAZ018", "CAZ138", "CAZ139"}, ourZones)
	assert.Equal(t, "Sierra Nevada of Somewhere Counties Between 3000-5000 ft; Another Zone Above 5000 ft; Third Zone", got,
		"a repeated name is said once")

	names := []string{strings.Repeat("Alpha ", 12) + "A", strings.Repeat("Bravo ", 12) + "B", strings.Repeat("Charlie ", 9) + "C"}
	got = zoneNamesSubhead(strings.Join(names, "; "), ourZones, ourZones)
	assert.Equal(t, names[0]+"; "+names[1]+"; +1 more", got)
	assert.LessOrEqual(t, utf8.RuneCountInString(got), maxZoneSubheadRunes)

	// One name longer than the cap is cut at a word.
	got = zoneNamesSubhead(strings.Repeat("Sierra ", 40), []string{"CAZ138"}, ourZones)
	assert.LessOrEqual(t, utf8.RuneCountInString(got), maxZoneSubheadRunes)
	assert.True(t, strings.HasSuffix(got, "Sierra…"), got)
}

func TestAcceptDistinction(t *testing.T) {
	above := alertEvent("wx:above", "Winter Storm Warning")
	above.Description = wswAboveDesc
	between := alertEvent("wx:between", "Winter Storm Warning")
	between.Description = wswBetweenDesc

	d, ok := acceptDistinction("  1 to 3 ft above 5,000 ft \n", above)
	assert.True(t, ok)
	assert.Equal(t, "1 to 3 ft above 5,000 ft", d, "trimmed; 5,000 is the alert's 5000")
	d, ok = acceptDistinction("Up to a foot between 4000 and 5000 ft", between)
	assert.True(t, ok)
	assert.Equal(t, "Up to a foot between 4000 and 5000 ft", d)

	sixty := strings.Repeat("a", maxDistinctionRunes)
	_, ok = acceptDistinction(sixty, above)
	assert.True(t, ok, "exactly the cap")
	_, ok = acceptDistinction(strings.Repeat("é", maxDistinctionRunes), above)
	assert.True(t, ok, "the cap counts characters, not bytes")

	for name, bad := range map[string]string{
		"empty":                "   ",
		"over the cap":         sixty + "a",
		"two lines":            "1 to 3 ft\nabove 5000 ft",
		"carriage return":      "1 to 3 ft\rabove 5000 ft",
		"the product name":     "Winter storm warning above 5000 ft",
		"a zone code":          "CAZ139, above 5000 ft",
		"a day-relative word":  "Heaviest snow tonight above 5000 ft",
		"a number not in text": "2 to 4 ft above 5000 ft",
	} {
		got, ok := acceptDistinction(bad, above)
		assert.False(t, ok, name)
		assert.Empty(t, got, name)
	}
	// The failure the number check is for: the sibling's amounts on the
	// lower-elevation alert, whose own text never says "1 to 3".
	_, ok = acceptDistinction("1 to 3 ft above 5000 ft", between)
	assert.False(t, ok)
}

// The normalizer sets the zone-name subhead on alerts that have a sibling in
// the poll and on no other, and it mints nothing: the hash does not see it.
func TestWeatherAlertPoll_SubheadFromZoneNames(t *testing.T) {
	now := time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.Weather.NWS.Zones = ourZones
	alerts := append(wswPair(now), nws.Alert{
		ID: "urn:oid:2.49.0.1.840.0.wind", Event: "Wind Advisory", Severity: "Moderate",
		AreaDesc: "Southern Motherlode Between 1000-3000 ft", Zones: []string{"CAZ137"},
		Effective: now.Add(-time.Hour), Expires: now.Add(8 * time.Hour),
	})
	api := &fakeWeatherAPI{alerts: alerts, fetchedAt: now}
	n := NewWeatherAlertNormalizer(cfg, api)
	n.now = func() time.Time { return now }

	res, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, res.Events, 3)
	above, between, wind := res.Events[0], res.Events[1], res.Events[2]
	assert.Equal(t, above.GetHeadline(), between.GetHeadline(), "the headline cannot tell them apart")
	assert.Equal(t, "Sierra Nevada of Calaveras-Tuolumne-Southern Alpine Counties Above 5000 ft", above.GetSubhead())
	assert.Equal(t, "Sierra Nevada of Calaveras and Tuolumne Counties Between 3000-5000 ft", between.GetSubhead())
	assert.Empty(t, wind.GetSubhead(), "a lone alert needs no distinction")
	assert.Nil(t, above.GetEnhancement(), "the zone-name subhead is not AI text")
	assert.Empty(t, res.ForceWrite, "nothing stored to bring up to date")

	// One of the pair alone (its sibling was cancelled): no subhead, and the
	// same content hash as with one, so the change is not a revision.
	api.alerts = alerts[:1]
	alone, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, alone.Events, 1)
	assert.Empty(t, alone.Events[0].GetSubhead())
	assert.Equal(t, store.ContentHash(above), store.ContentHash(alone.Events[0]))

	// A copy an Update in the list replaced is not emitted, so it is not a
	// sibling of its replacement either.
	update := wswPair(now)[0]
	update.ID, update.MessageType = "urn:oid:2.49.0.1.840.0.3dfb.new", "Update"
	update.References = []string{alerts[0].ID}
	api.alerts = []nws.Alert{alerts[0], update}
	replaced, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	require.Len(t, replaced.Events, 1)
	assert.Empty(t, replaced.Events[0].GetSubhead())
}

// ForceWrite is how a hash-excluded field reaches a stored event. It lists an
// alert only when the stored subhead should change: never one a model wrote,
// and nothing on a tick where the two already agree.
func TestWeatherAlertPoll_SubheadForceWrite(t *testing.T) {
	now := time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.Weather.NWS.Zones = ourZones
	api := &fakeWeatherAPI{alerts: wswPair(now), fetchedAt: now}
	n := NewWeatherAlertNormalizer(cfg, api)
	n.now = func() time.Time { return now }

	first, err := n.Poll(testCtx(), nil)
	require.NoError(t, err)
	aboveID, betweenID := first.Events[0].GetId(), first.Events[1].GetId()
	held := func(evs ...*gridv1.Event) Prior {
		p := &priorEvents{byID: map[string]*gridv1.Event{}, bySource: map[string][]*gridv1.Event{}}
		for _, ev := range evs {
			p.byID[ev.GetId()] = ev
			p.bySource["nws"] = append(p.bySource["nws"], ev)
		}
		return p
	}

	// Stored before the field existed (as production's alerts are on the
	// first tick after deploy): both are listed.
	oldAbove, oldBetween := alertEvent(aboveID, "Winter Storm Warning"), alertEvent(betweenID, "Winter Storm Warning")
	oldAbove.Summary, oldAbove.Enhancement = "AI summary.", &gridv1.Enhancement{Fields: []string{"summary"}}
	res, err := n.Poll(testCtx(), held(oldAbove, oldBetween))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{aboveID, betweenID}, res.ForceWrite)

	// Stored and current: nothing to write, so the tick skips them.
	res, err = n.Poll(testCtx(), held(first.Events[0], first.Events[1]))
	require.NoError(t, err)
	assert.Empty(t, res.ForceWrite)

	// A model wrote the stored one: it is not ours to replace.
	ai := alertEvent(aboveID, "Winter Storm Warning")
	ai.Subhead = "1 to 3 ft above 5,000 ft"
	ai.Enhancement = &gridv1.Enhancement{Fields: []string{"summary", "subhead"}}
	res, err = n.Poll(testCtx(), held(ai, first.Events[1]))
	require.NoError(t, err)
	assert.Empty(t, res.ForceWrite)

	// The sibling leaves: the survivor's zone-name subhead is cleared.
	api.alerts = wswPair(now)[:1]
	res, err = n.Poll(testCtx(), held(first.Events[0], first.Events[1]))
	require.NoError(t, err)
	assert.Equal(t, []string{aboveID}, res.ForceWrite)
	assert.Empty(t, res.Events[0].GetSubhead())
}
