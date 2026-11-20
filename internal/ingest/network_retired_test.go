package ingest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/meshcore"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/pushingest"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// otherKey is a second node, sharing no prefix with fullKey.
const otherKey = "ab0715314cfa9b5e" + "0011223344556677" + "8899aabbccddeeff" + "0123456789abcdef"

// retiredNormalizer is pushNormalizer with fullKey in grid.meshcore.retired.
func retiredNormalizer(t *testing.T, reg MeshRegistry, snap pushingest.MeshSnapshot) *NetworkNormalizer {
	t.Helper()
	cfg := testConfig()
	cfg.Grid.Meshcore.Retired = []config.MeshcoreRetiredNode{{Key: fullKey, Name: "SIERRA Eagle One", Since: "2026-11-20"}}
	n := NewNetworkNormalizer(cfg, reg, &fakePush{snap: snap})
	n.now = func() time.Time { return pollNow }
	return n
}

// A node its operator retired is not presented, even by its own advert: the
// advert most likely to arrive is an old one a gateway's radio kept. The
// stored event is handed to the scheduler once, to be RESOLVED.
func TestRetiredNodeIsNotPresentedByItsAdvert(t *testing.T) {
	heard := func(key, name string) meshcore.NodeState {
		return meshcore.NodeState{PubKey: key, Role: meshcore.RoleRepeater, Name: name,
			HasLocation: true, Lat: 38.2, Lng: -120.3, LastHeardAt: pollNow.Add(-time.Minute), ReceivedAt: pollNow.Add(-time.Minute)}
	}
	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{heard(fullKey, "SIERRA Eagle One"), heard(otherKey, "SIERRA Eagle HQ")}}
	n := retiredNormalizer(t, reg, pushingest.MeshSnapshot{})

	res, err := n.Poll(testCtx(), established())
	require.NoError(t, err)
	assert.Equal(t, []string{"meshcore:" + otherKey}, eventIDs(res.Events))
	assert.Equal(t, []string{"meshcore:" + fullKey}, res.Retired, "the store still shows it")
	for _, a := range res.MeshActivity {
		assert.NotEqual(t, fullKey, a.PubKey, "and its clocks stop moving")
	}

	// Handed over once: the expired catalog can show a resolved node for
	// several minutes more, and that must not repeat the transition.
	res, err = n.Poll(testCtx(), established())
	require.NoError(t, err)
	assert.Empty(t, res.Retired)

	// A write that failed is tried again later.
	n.now = func() time.Time { return pollNow.Add(2 * expiredTTL) }
	res, err = n.Poll(testCtx(), established())
	require.NoError(t, err)
	assert.Equal(t, []string{"meshcore:" + fullKey}, res.Retired)

	t.Run("nothing to resolve when the store does not hold it", func(t *testing.T) {
		res, err := retiredNormalizer(t, reg, pushingest.MeshSnapshot{}).Poll(testCtx(), &fakePrior{})
		require.NoError(t, err)
		assert.Empty(t, res.Retired)
		assert.Equal(t, []string{"meshcore:" + otherKey}, eventIDs(res.Events))
	})
}

// The way SIERRA Eagle One came back on 2026-11-18, eight days after it
// expired: one relay credit, on a hop that fit its stored key. A retired node
// is not a relay candidate, and a credit that names it anyway is dropped.
func TestRetiredNodeIsNotRevivedByARelay(t *testing.T) {
	stored := expiredNode(t, 38.2, -120.3)
	reg := &fakeMeshRegistry{connected: 1, relays: map[string]time.Time{fullKey: pollNow.Add(-time.Minute)}}
	n := retiredNormalizer(t, reg, pushingest.MeshSnapshot{}).WithCatalog(catalogOf(stored))

	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Empty(t, res.Events)
	assert.NotContains(t, reg.knownSeen, meshcore.KnownNode{PubKey: fullKey, Role: meshcore.RoleRepeater},
		"not offered for hop resolution: it is gone, so the hop is someone else's")
	assert.Empty(t, res.MeshActivity, "no relay is recorded against it")
	assert.Equal(t, []string{"meshcore:" + fullKey}, res.Retired, "an EXPIRED node is resolved too")

	res, err = n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Empty(t, res.Events, "and the credit is not kept for a later tick")
}

// A monitor that still lists a retired node, and even reaches it, does not
// hold it present. Its reports are dropped by full key or by prefix.
func TestRetiredNodeIsNotHeldByAMonitor(t *testing.T) {
	for _, id := range []string{keyPrefix, fullKey} {
		n := retiredNormalizer(t, nil, pushingest.MeshSnapshot{
			Live:    1,
			Reports: []pushingest.MeshNodeReport{report(id, "SIERRA Eagle One", pollNow)},
		})
		res, err := n.Poll(testCtx(), established())
		require.NoError(t, err)
		assert.Empty(t, res.Events, "report for %s", id)
		assert.Empty(t, res.MeshTelemetry, "report for %s", id)
	}
}

// A node that is not retired is untouched by the list.
func TestRetiredListLeavesOtherNodesAlone(t *testing.T) {
	cfg := testConfig()
	cfg.Grid.Meshcore.Retired = []config.MeshcoreRetiredNode{{Key: otherKey}}
	n := NewNetworkNormalizer(cfg, nil, &fakePush{snap: pushingest.MeshSnapshot{
		Live:    1,
		Reports: []pushingest.MeshNodeReport{report(keyPrefix, "SIERRA Arnold Summit", pollNow)},
	}})
	n.now = func() time.Time { return pollNow }
	res, err := n.Poll(testCtx(), established())
	require.NoError(t, err)
	assert.Equal(t, []string{"meshcore:" + fullKey}, eventIDs(res.Events))
	assert.Empty(t, res.Retired)
}

// PollResult.Retired resolves an event from ACTIVE or EXPIRED, keeps its
// history, and does not wait for a clean fetch: the list is the operator's
// word, not something read off the feed.
func TestTickRetiredResolvesFromAnyStatus(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "meshcore", "nws")
	now := time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC)

	active := schedEvent("meshcore:active", "meshcore", gridv1.Layer_MESH)
	expired := schedEvent("meshcore:expired", "meshcore", gridv1.Layer_MESH)
	kept := schedEvent("meshcore:kept", "meshcore", gridv1.Layer_MESH)
	fn := &fakeNormalizer{ids: []string{"meshcore"}, result: &PollResult{Events: []*gridv1.Event{active, expired, kept}}}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"meshcore": {Disappearance: store.DisappearanceExpire, ExpireAfter: 2 * time.Hour}},
	})
	ps := &pollerState{}
	sched.now = func() time.Time { return now }
	spec := PollerSpec{Normalizer: fn, Interval: time.Minute}
	sched.tick(ctx, spec, ps)
	require.NoError(t, st.TransitionEvents(ctx, []string{"meshcore:expired"}, gridv1.EventStatus_EXPIRED, now))

	// Another poller's event, which this one must not be able to retire.
	other := &fakeNormalizer{ids: []string{"nws"}, result: &PollResult{Events: []*gridv1.Event{schedEvent("nws:alert", "nws", gridv1.Layer_WEATHER_ALERT)}}}
	sched.tick(ctx, PollerSpec{Normalizer: other, Interval: time.Minute}, &pollerState{})

	// The source is failing this tick, so the sweep and supersession are off.
	now = now.Add(time.Minute)
	fn.result = &PollResult{
		Events:    []*gridv1.Event{kept},
		PerSource: map[string]error{"meshcore": assert.AnError},
		Retired:   []string{"meshcore:active", "meshcore:expired", "nws:alert"},
	}
	sched.tick(ctx, spec, ps)

	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, "meshcore:active"))
	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, "meshcore:expired"))
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "meshcore:kept"))
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, "nws:alert"), "not this poller's to retire")

	hist, _, err := st.EventHistory(ctx, "meshcore:active", 50, "")
	require.NoError(t, err)
	assert.Len(t, hist, 2, "the history is kept, with one revision for the retirement")

	// Naming it again changes nothing.
	now = now.Add(time.Minute)
	sched.tick(ctx, spec, ps)
	hist, _, err = st.EventHistory(ctx, "meshcore:active", 50, "")
	require.NoError(t, err)
	assert.Len(t, hist, 2)
}

// The whole path against a real store: an ACTIVE node is retired on the next
// tick, an advert and a relay credit arriving afterwards change nothing, and
// taking the entry out of the list lets the next advert bring the node back.
func TestRetiredNodeThroughTheScheduler(t *testing.T) {
	ctx := testCtx()
	st := newSchedStore(t)
	seedSchedSources(t, st, "meshcore")
	now := pollNow
	id := "meshcore:" + fullKey

	reg := &fakeMeshRegistry{connected: 1, nodes: []meshcore.NodeState{{
		PubKey: fullKey, Role: meshcore.RoleRepeater, Name: "SIERRA Eagle One",
		HasLocation: true, Lat: 38.2, Lng: -120.3, LastHeardAt: pollNow, ReceivedAt: pollNow,
	}}}
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{"meshcore": {Disappearance: store.DisappearanceExpire, ExpireAfter: 2 * time.Hour}},
	})
	sched.now = func() time.Time { return now }
	tick := func(n *NetworkNormalizer, ps *pollerState) {
		t.Helper()
		n.now = func() time.Time { return now }
		sched.tick(ctx, PollerSpec{Normalizer: n, Interval: time.Minute}, ps)
		now = now.Add(time.Minute)
	}
	revisions := func() int {
		t.Helper()
		hist, _, err := st.EventHistory(ctx, id, 50, "")
		require.NoError(t, err)
		return len(hist)
	}

	// In service: one ACTIVE event.
	inService := pushNormalizer(t, reg, pushingest.MeshSnapshot{})
	tick(inService, &pollerState{})
	require.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, id))
	require.Equal(t, 1, revisions())

	// Retired (a deploy with the entry: a new process, a new normalizer).
	retired, ps := retiredNormalizer(t, reg, pushingest.MeshSnapshot{}), &pollerState{}
	tick(retired, ps)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, id))
	assert.Equal(t, 2, revisions(), "one revision for the retirement; the history is kept")

	// Its advert is still in the registry and a relay is credited to it.
	for i := 0; i < 3; i++ {
		reg.relays = map[string]time.Time{fullKey: now}
		tick(retired, ps)
	}
	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, id))
	assert.Equal(t, 2, revisions(), "nothing brings it back, and nothing more is written")

	// The entry is removed: the next advert presents the node again.
	tick(pushNormalizer(t, reg, pushingest.MeshSnapshot{}), &pollerState{})
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, id))
	assert.Equal(t, 3, revisions())
}
