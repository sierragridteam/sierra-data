package services

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/cache"
	"github.com/sierragridteam/sierra-data/internal/clients/caltrans"
	"github.com/sierragridteam/sierra-data/internal/config"
)

// conditionsPage is a roads.dot.ca.gov page for one highway: the sentence
// every real page carries, then the highway's section when it has
// conditions. With none it is a real page that lists nothing.
func conditionsPage(highway string, paragraphs ...string) string {
	var b strings.Builder
	b.WriteString("<html><body><div><hr>\n<p><em>This highway information is the latest reported as of Wednesday, November 18th, 2026 at 09:05 AM.</em></p>\n")
	if len(paragraphs) > 0 {
		b.WriteString("<h3>SR " + highway + "</h3>\n")
		for _, p := range paragraphs {
			b.WriteString("<p>\n" + p + "\n</p>\n")
		}
	}
	b.WriteString("<hr />\n</div></body></html>")
	return b.String()
}

// A Hwy 4 page in the shape of the 2026-11-18 storm (not a capture): a
// closure on Arnold to Bear Valley, and the Ebbetts Pass winter closure past
// its end, in Caltrans's wording of that week.
const (
	hwy4SnowClosure    = "Is closed from 0.5 mi east of Camp Connell (Calaveras Co) to Lake Alpine (Alpine Co) - Due to snow"
	hwy4EbbettsClosure = "Is closed from the Mt. Reba turnoff to the Jct of SR 89 /Ebbetts Pass/ (Alpine Co) - Due to snow - Motorists are advised to use an alternate route"
)

// statusInputDoer serves the two status inputs, each replaceable by a
// failure. lcs2way.kml and cc.kml fail, which refreshRoadData tolerates.
type statusInputDoer struct {
	mu sync.Mutex
	// chp is chp-only.kml (nil: an empty feed).
	chp func() (*http.Response, error)
	// conditions is roads.dot.ca.gov by highway number (missing: a real page
	// that lists nothing for the highway).
	conditions map[string]func() (*http.Response, error)
	hits       map[string]int
}

func (d *statusInputDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.hits == nil {
		d.hits = map[string]int{}
	}
	u := req.URL.String()
	switch {
	case strings.Contains(u, "chp-only"):
		d.hits["chp"]++
		if d.chp != nil {
			return d.chp()
		}
		return kmlResponse(`<?xml version="1.0"?><kml xmlns="http://www.opengis.net/kml/2.2"><Document></Document></kml>`), nil
	case strings.Contains(u, "roadscell.php"):
		hw := req.URL.Query().Get("roadnumber")
		d.hits["conditions:"+hw]++
		if serve := d.conditions[hw]; serve != nil {
			return serve()
		}
		return kmlResponse(conditionsPage(hw)), nil
	}
	return nil, fmt.Errorf("simulated outage: %s", u)
}

func (d *statusInputDoer) setConditions(highway string, serve func() (*http.Response, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conditions == nil {
		d.conditions = map[string]func() (*http.Response, error){}
	}
	d.conditions[highway] = serve
}

func (d *statusInputDoer) setCHP(serve func() (*http.Response, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.chp = serve
}

func servePage(body string) func() (*http.Response, error) {
	return func() (*http.Response, error) { return kmlResponse(body), nil }
}

func serveStatus(code int) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		resp := kmlResponse("upstream error")
		resp.StatusCode = code
		return resp, nil
	}
}

func serveError(msg string) func() (*http.Response, error) {
	return func() (*http.Response, error) { return nil, errors.New(msg) }
}

// stormRoads are the two shipped roads the 2026-11-18 defects were seen on,
// with the keywords and seasonal closures production runs with.
func stormRoads(t *testing.T) []config.MonitoredRoad {
	t.Helper()
	ends := map[string][2]config.Coordinates{
		"hwy4-arnold-bearvalley": {{Latitude: 38.265006, Longitude: -120.333654}, {Latitude: 38.461045, Longitude: -120.042368}},
		"hwy49-angels-sonora":    {{Latitude: 38.067400, Longitude: -120.540200}, {Latitude: 37.984100, Longitude: -120.382700}},
	}
	var out []config.MonitoredRoad
	for _, r := range shippedRoads(t) {
		if e, ok := ends[r.ID]; ok {
			r.Origin, r.Destination = e[0], e[1]
			out = append(out, r)
		}
	}
	require.Len(t, out, 2)
	return out
}

// statusInputClock is the service's clock in these tests.
type statusInputClock struct{ at time.Time }

func (c *statusInputClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newStatusInputService(t *testing.T, doer *statusInputDoer) (*RoadsService, *cache.Cache, *statusInputClock) {
	t.Helper()
	parser := caltrans.NewFeedParser()
	parser.HTTPClient = doer
	c := cache.NewCache()
	cfg := &config.Config{Roads: config.RoadsConfig{
		RefreshInterval: 15 * time.Minute,
		MonitoredRoads:  stormRoads(t),
		IncidentAreas:   []config.IncidentArea{motherLode()},
	}}
	s := NewRoadsService(nil, parser, c, cfg, &statusEnhancer{})
	clock := &statusInputClock{at: time.Date(2026, 11, 18, 17, 5, 0, 0, time.UTC)}
	s.now = func() time.Time { return clock.at }
	return s, c, clock
}

func roadByID(t *testing.T, roads []*api.Road, id string) *api.Road {
	t.Helper()
	for _, r := range roads {
		if r.GetId() == id {
			return r
		}
	}
	t.Fatalf("no road %q", id)
	return nil
}

// requireStormState is Hwy 4 Arnold to Bear Valley as that page has it:
// closed, with the Ebbetts Pass winter closure past it.
func requireStormState(t *testing.T, roads []*api.Road, msg string) {
	t.Helper()
	hwy4 := roadByID(t, roads, "hwy4-arnold-bearvalley")
	require.Equal(t, api.RoadStatus_CLOSED, hwy4.GetStatus(), msg)
	require.Len(t, hwy4.GetBeyond(), 1, msg)
	assert.Equal(t, "Ebbetts Pass winter closure", hwy4.GetBeyond()[0].GetName(), msg)
}

// The 2026-11-18 defect. Whichever way the roads.dot.ca.gov read fails, the
// road keeps the state of the last good read while that read is
// servable-stale; past that the refresh fails, so the caller keeps its last
// good road set. It never reads as "no conditions", which is OPEN with no
// seasonal closure.
func TestRefreshRoadData_FailedConditionsReadIsNotOpen(t *testing.T) {
	failures := map[string]func() (*http.Response, error){
		"HTTP error":             serveStatus(http.StatusServiceUnavailable),
		"timeout":                serveError("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"),
		"200 with another page":  servePage("<html><body><h1>Service Temporarily Unavailable</h1></body></html>"),
		"200 with an empty body": servePage(""),
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			doer := &statusInputDoer{}
			doer.setConditions("4", servePage(conditionsPage("4", hwy4SnowClosure, hwy4EbbettsClosure)))
			s, c, clock := newStatusInputService(t, doer)

			roads, err := s.refreshRoadData(testCtx())
			require.NoError(t, err)
			requireStormState(t, roads, "the good read")
			failed, at := s.RoadInputHealth()
			assert.Empty(t, failed)
			assert.Equal(t, clock.at, at)

			// The read fails; the last good one is fresh.
			doer.setConditions("4", failure)
			clock.advance(15 * time.Minute)
			firstFailure := clock.at
			roads, err = s.refreshRoadData(testCtx())
			require.NoError(t, err)
			requireStormState(t, roads, "a failed read with a fresh last good read")
			assert.Equal(t, api.RoadStatus_OPEN, roadByID(t, roads, "hwy49-angels-sonora").GetStatus())
			failed, _ = s.RoadInputHealth()
			require.Len(t, failed, 1)
			assert.Equal(t, "road conditions for Hwy 4", failed[0].Input)
			assert.Equal(t, firstFailure, failed[0].Since)
			assert.True(t, failed[0].LastGoodUsed)
			require.Error(t, failed[0].Err)

			// Still failing; the last good read is stale and still servable.
			c.Backdate(roadConditionsKeyPrefix+"4", 20*time.Minute)
			clock.advance(15 * time.Minute)
			roads, err = s.refreshRoadData(testCtx())
			require.NoError(t, err)
			requireStormState(t, roads, "a failed read with a servable-stale last good read")
			failed, _ = s.RoadInputHealth()
			require.Len(t, failed, 1)
			assert.Equal(t, firstFailure, failed[0].Since, "one run of failures has one start")
			assert.True(t, failed[0].LastGoodUsed)

			// Past the stale window: the refresh fails rather than guess.
			c.Backdate(roadConditionsKeyPrefix+"4", 15*time.Minute) // 35m > 2x15m
			clock.advance(15 * time.Minute)
			_, err = s.refreshRoadData(testCtx())
			require.Error(t, err, "an unreadable Hwy 4 must not publish its roads as OPEN")
			assert.Contains(t, err.Error(), "road conditions for Hwy 4")
			failed, at = s.RoadInputHealth()
			require.Len(t, failed, 1)
			assert.Equal(t, firstFailure, failed[0].Since)
			assert.False(t, failed[0].LastGoodUsed)
			assert.Equal(t, clock.at, at, "a failed refresh is still the last refresh")

			// The read comes back.
			doer.setConditions("4", servePage(conditionsPage("4", hwy4SnowClosure, hwy4EbbettsClosure)))
			clock.advance(15 * time.Minute)
			roads, err = s.refreshRoadData(testCtx())
			require.NoError(t, err)
			requireStormState(t, roads, "the read is back")
			failed, _ = s.RoadInputHealth()
			assert.Empty(t, failed)
		})
	}
}

// With nothing cached (a cold start into an outage) a failed read fails the
// refresh.
func TestRefreshRoadData_FailedConditionsReadWithNoLastGood(t *testing.T) {
	doer := &statusInputDoer{}
	doer.setConditions("49", serveStatus(http.StatusBadGateway))
	s, _, _ := newStatusInputService(t, doer)

	_, err := s.refreshRoadData(testCtx())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "road conditions for Hwy 49")
	failed, _ := s.RoadInputHealth()
	require.Len(t, failed, 1)
	assert.Equal(t, "road conditions for Hwy 49", failed[0].Input)
	assert.False(t, failed[0].LastGoodUsed)
}

// A page that lists nothing for the highway is a good read (Hwy 49 normally
// has none), cached like any other: a failed read after it serves "no
// conditions" from the cache, and the failure is still reported.
func TestRefreshRoadData_NoConditionsIsAGoodReadAndIsCached(t *testing.T) {
	doer := &statusInputDoer{}
	s, c, clock := newStatusInputService(t, doer)

	roads, err := s.refreshRoadData(testCtx())
	require.NoError(t, err)
	assert.Equal(t, api.RoadStatus_OPEN, roadByID(t, roads, "hwy49-angels-sonora").GetStatus())
	failed, _ := s.RoadInputHealth()
	assert.Empty(t, failed)

	var cached []caltrans.RoadCondition
	_, found, err := c.GetWithMetadata(roadConditionsKeyPrefix+"49", &cached)
	require.NoError(t, err)
	assert.True(t, found, "no conditions is cached as a read")
	assert.Empty(t, cached)

	doer.setConditions("49", serveError("connection reset"))
	clock.advance(15 * time.Minute)
	roads, err = s.refreshRoadData(testCtx())
	require.NoError(t, err, "the last good read said no conditions, and is still servable")
	assert.Equal(t, api.RoadStatus_OPEN, roadByID(t, roads, "hwy49-angels-sonora").GetStatus())
	failed, _ = s.RoadInputHealth()
	require.Len(t, failed, 1)
	assert.Equal(t, "road conditions for Hwy 49", failed[0].Input)
	assert.True(t, failed[0].LastGoodUsed)

	// One fetch per highway per refresh, failing or not.
	assert.Equal(t, 2, doer.hits["conditions:49"])
	assert.Equal(t, 2, doer.hits["conditions:4"])
	assert.Equal(t, 2, doer.hits["chp"])
}

// chpOnHwy49 is a CHP incident at the Angels Camp end of Hwy 49 Angels Camp
// to Sonora, named by its route.
const chpOnHwy49 = `<?xml version="1.0" encoding="UTF-8"?>
<kml xmlns="http://www.opengis.net/kml/2.2"><Document>
  <Placemark>
    <name>CHP Incident 261118ST0331</name>
    <description><![CDATA[<h2 class="iw-title">1183-Trfc Collision-Unkn Inj</h2><p class="iw-text">Nov 18 2026  9:02AM <br> Sr49 / Sr4</p>]]></description>
    <styleUrl>#chp</styleUrl>
    <Point><coordinates>-120.540200,38.067400,0</coordinates></Point>
  </Placemark>
</Document></kml>`

// A failed chp-only.kml read gets the same treatment: the last good set
// while it is servable-stale, then a failed refresh.
func TestRefreshRoadData_FailedCHPReadIsNotOpen(t *testing.T) {
	doer := &statusInputDoer{}
	doer.setCHP(servePage(chpOnHwy49))
	s, c, clock := newStatusInputService(t, doer)

	restricted := func(roads []*api.Road, msg string) {
		t.Helper()
		hwy49 := roadByID(t, roads, "hwy49-angels-sonora")
		require.Equal(t, api.RoadStatus_RESTRICTED, hwy49.GetStatus(), msg)
		require.Len(t, hwy49.GetStatusCauses(), 1, msg)
		assert.Equal(t, "261118ST0331", hwy49.GetStatusCauses()[0].GetSourceId(), msg)
	}

	roads, err := s.refreshRoadData(testCtx())
	require.NoError(t, err)
	restricted(roads, "the good read")

	doer.setCHP(serveStatus(http.StatusInternalServerError))
	clock.advance(15 * time.Minute)
	firstFailure := clock.at
	roads, err = s.refreshRoadData(testCtx())
	require.NoError(t, err)
	restricted(roads, "a failed CHP read keeps the incident of the last good one")
	failed, _ := s.RoadInputHealth()
	require.Len(t, failed, 1)
	assert.Equal(t, "CHP incidents", failed[0].Input)
	assert.Equal(t, firstFailure, failed[0].Since)
	assert.True(t, failed[0].LastGoodUsed)

	c.Backdate(chpIncidentsKey, 35*time.Minute)
	clock.advance(15 * time.Minute)
	_, err = s.refreshRoadData(testCtx())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CHP incidents")
	failed, _ = s.RoadInputHealth()
	require.Len(t, failed, 1)
	assert.False(t, failed[0].LastGoodUsed)
	assert.Equal(t, firstFailure, failed[0].Since)
}

// The incidents feed reads chp-only.kml on its own, faster cadence. A good
// read there is the roads refresh's last good set too, at no extra fetch.
func TestRefreshRoadData_UsesTheIncidentFeedsLastGoodCHPRead(t *testing.T) {
	doer := &statusInputDoer{}
	doer.setCHP(servePage(chpOnHwy49))
	s, _, _ := newStatusInputService(t, doer)

	_, err := s.refreshIncidents(testCtx(), motherLode())
	require.NoError(t, err)
	require.Equal(t, 1, doer.hits["chp"])

	doer.setCHP(serveError("connection refused"))
	roads, err := s.refreshRoadData(testCtx())
	require.NoError(t, err)
	assert.Equal(t, api.RoadStatus_RESTRICTED, roadByID(t, roads, "hwy49-angels-sonora").GetStatus())
	assert.Equal(t, 2, doer.hits["chp"], "one fetch by each reader, none added")
	failed, _ := s.RoadInputHealth()
	require.Len(t, failed, 1)
	assert.Equal(t, "CHP incidents", failed[0].Input)
	assert.True(t, failed[0].LastGoodUsed)
}

// Both inputs failing are both reported, CHP first.
func TestRoadInputHealth_ReportsEveryFailedInput(t *testing.T) {
	doer := &statusInputDoer{}
	s, _, clock := newStatusInputService(t, doer)
	_, err := s.refreshRoadData(testCtx())
	require.NoError(t, err)

	doer.setCHP(serveError("no route to host"))
	doer.setConditions("4", serveStatus(http.StatusServiceUnavailable))
	clock.advance(15 * time.Minute)
	_, err = s.refreshRoadData(testCtx())
	require.NoError(t, err)

	failed, _ := s.RoadInputHealth()
	require.Len(t, failed, 2)
	assert.Equal(t, "CHP incidents", failed[0].Input)
	assert.Equal(t, "road conditions for Hwy 4", failed[1].Input)
}

func TestRoadInputFailure_Describe(t *testing.T) {
	since := time.Date(2026, 11, 18, 17, 5, 0, 0, time.UTC)
	f := RoadInputFailure{
		Input: "road conditions for Hwy 4", Since: since, LastGoodUsed: true,
		Err: errors.New("HTTP error 503 fetching road conditions for highway 4"),
	}
	assert.Equal(t,
		"road conditions for Hwy 4 unavailable since 17:05Z; last good read used (HTTP error 503 fetching road conditions for highway 4)",
		f.Describe(since.Add(20*time.Minute)))

	f.LastGoodUsed = false
	assert.Equal(t,
		"road conditions for Hwy 4 unavailable since 17:05Z; no last good read to use, road status not refreshed (HTTP error 503 fetching road conditions for highway 4)",
		f.Describe(since.Add(50*time.Minute)))

	chp := RoadInputFailure{Input: "CHP incidents", Since: since.In(time.FixedZone("PST", -8*3600)), LastGoodUsed: true}
	assert.Equal(t, "CHP incidents unavailable since 17:05Z; last good read used", chp.Describe(since.Add(time.Hour)),
		"the time is UTC whatever zone it was taken in")
	assert.Equal(t, "CHP incidents unavailable since 2026-11-18 17:05Z; last good read used", chp.Describe(since.Add(30*time.Hour)),
		"a day on, the time carries its date")
}
