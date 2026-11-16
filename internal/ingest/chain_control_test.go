package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/hazards"
	"github.com/sierragridteam/sierra-data/internal/places"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// fakeChainSource scripts ChainRequirements.
type fakeChainSource struct {
	stretches []hazards.Feature
	degraded  error
	err       error
}

func (f *fakeChainSource) ChainRequirements(context.Context, []config.HazardArea) ([]hazards.Feature, error, error) {
	return f.stretches, f.degraded, f.err
}

// chainFeature is a stretch as hazards.Service projects it: signs are
// (place, lat, lng), low end first.
func chainFeature(id, highway, level string, signs ...[3]any) hazards.Feature {
	cc := &hazards.ChainControlProps{Level: level, Highway: highway}
	for i, s := range signs {
		place, lat, lng := s[0].(string), s[1].(float64), s[2].(float64)
		cc.Signs = append(cc.Signs, hazards.ChainSign{
			ID: id + "-sign-" + string(rune('a'+i)), Place: place, Direction: "Eastbound", Level: level,
			Since: "2026-11-13T15:29:26-08:00", Coordinates: []float64{lng, lat}, Source: "cwwp2.dot.ca.gov",
			County: "Calaveras", Postmile: "56.30",
		})
	}
	first, last := cc.Signs[0], cc.Signs[len(cc.Signs)-1]
	cc.From = &hazards.ChainPlace{Place: first.Place, Coordinates: first.Coordinates}
	cc.To = &hazards.ChainPlace{Place: last.Place, Coordinates: last.Coordinates}
	return hazards.Feature{Type: "Feature", Properties: hazards.Properties{
		ID: id, Layer: "CHAIN_CONTROL", Kind: "Chain control", Category: "r2", Severity: hazards.SevModerate,
		Headline:    highway + " chains required, " + first.Place + " to " + last.Place + " (" + level + ")",
		Description: "Chains or traction devices are required.", AreaLabel: first.Place + " to " + last.Place,
		Effective:    "2026-11-13T13:45:34-08:00",
		Source:       hazards.Source{ID: "caltrans", Name: "Caltrans", Attribution: "cwwp2.dot.ca.gov"},
		ChainControl: cc,
	}}
}

// hwy4 has its low end in the calaveras area and its high end outside it,
// inside tuolumne's rectangle (testConfig's areas).
func hwy4() hazards.Feature {
	return chainFeature("cc:hwy4:r2", "Highway 4", "R2",
		[3]any{"Poison Springs", 38.39, -120.18}, [3]any{"Far End", 38.10, -119.60})
}

func TestChainControlNormalizer_Event(t *testing.T) {
	cfg := testConfig()
	src := &fakeChainSource{stretches: []hazards.Feature{hwy4()}}
	n := NewChainControlNormalizer(cfg, src, func(_ context.Context, lat, _ float64) ([]string, error) {
		if lat > 38.3 {
			return []string{"county:calaveras-county"}, nil
		}
		return []string{"county:tuolumne-county"}, nil
	})
	res, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	assert.Empty(t, res.PerSource)

	ev := res.Events[0]
	assert.Equal(t, "cc:hwy4:r2", ev.GetId())
	assert.Equal(t, gridv1.Layer_CHAIN_CONTROL, ev.GetLayer())
	assert.Equal(t, gridv1.Severity_MODERATE, ev.GetSeverity())
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus())
	assert.Equal(t, "r2", ev.GetCategory())
	assert.Equal(t, "Highway 4 chains required, Poison Springs to Far End (R2)", ev.GetHeadline())
	assert.Equal(t, ChainControlSourceID, ev.GetProvenance().GetSourceId())
	assert.Equal(t, time.Date(2026, 11, 13, 21, 45, 34, 0, time.UTC), ev.GetEffective().AsTime(), "a proper UTC timestamp")
	assert.InDelta(t, 38.39, ev.GetGeometry().GetCentroid().GetLat(), 1e-9, "the point is the low end")

	d := ev.GetChainControl()
	assert.Equal(t, "R2", d.GetLevel())
	assert.Equal(t, "Poison Springs", d.GetFrom().GetPlace())
	assert.Equal(t, "Far End", d.GetTo().GetPlace())
	require.Len(t, d.GetSigns(), 2)
	assert.InDelta(t, -119.60, d.GetSigns()[1].GetLocation().GetLng(), 1e-9)
	assert.NotNil(t, d.GetSigns()[0].GetSince())

	// Places come from EVERY sign, not the low-end point alone.
	assert.Equal(t, []string{"area:calaveras", "area:tuolumne", "county:calaveras-county", "county:tuolumne-county"}, ev.GetPlaceIds())

	// The same read again is the same content: no revision per poll.
	again, err := n.Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Equal(t, store.ContentHash(ev), store.ContentHash(again.Events[0]))
}

func TestChainControlNormalizer_FailLoud(t *testing.T) {
	cfg := testConfig()

	// A failed registry read is a hard error.
	_, err := NewChainControlNormalizer(cfg, &fakeChainSource{err: errors.New("cwwp2 down")}, nil).Poll(testCtx(), &fakePrior{})
	require.Error(t, err)

	// No areas: refusing to poll an empty scope, never a success with nothing.
	_, err = NewChainControlNormalizer(&config.Config{}, &fakeChainSource{}, nil).Poll(testCtx(), &fakePrior{})
	require.Error(t, err)

	// A read that cannot vouch for completeness lands what it has and marks
	// the source, so nothing is swept.
	res, err := NewChainControlNormalizer(cfg, &fakeChainSource{
		stretches: []hazards.Feature{hwy4()}, degraded: errors.New("unrecognized chain-control status at Highway 4 X"),
	}, nil).Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Len(t, res.Events, 1)
	assert.Error(t, res.PerSource[ChainControlSourceID])

	// A confirmed-empty read is a success with no events.
	res, err = NewChainControlNormalizer(cfg, &fakeChainSource{}, nil).Poll(testCtx(), &fakePrior{})
	require.NoError(t, err)
	assert.Empty(t, res.Events)
	assert.Empty(t, res.PerSource)
}

// Through the scheduler and the store: a requirement appears, grows (a
// revision), survives a degraded read, changes level (one event resolves,
// another opens) and ends (resolved by a confirmed-empty read). The event
// attaches to every area with a sign in it.
func TestChainControlLifecycle(t *testing.T) {
	ctx := testCtx()
	cfg := testConfig()
	st := newSchedStore(t)
	require.NoError(t, places.Seed(ctx, st, &config.Config{Hazards: cfg.Hazards}))
	seedSchedSources(t, st, ChainControlSourceID)
	src := &fakeChainSource{stretches: []hazards.Feature{hwy4()}}
	n := NewChainControlNormalizer(cfg, src, func(ctx context.Context, lat, lng float64) ([]string, error) {
		found, err := st.PlacesContaining(ctx, lat, lng)
		ids := make([]string, len(found))
		for i, p := range found {
			ids[i] = p.GetId()
		}
		return ids, err
	})
	sched := NewScheduler(st, SchedulerConfig{
		Tuning: map[string]config.SourceTuning{ChainControlSourceID: {Disappearance: store.DisappearanceResolve}},
	})
	ps := &pollerState{}
	spec := PollerSpec{Normalizer: n, Interval: time.Minute}
	const r2, r3 = "cc:hwy4:r2", "cc:hwy4:r3"

	sched.tick(ctx, spec, ps)
	ev, err := st.GetEvent(ctx, r2)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), ev.GetRevision())
	assert.Contains(t, ev.GetPlaceIds(), "area:calaveras")
	assert.Contains(t, ev.GetPlaceIds(), "area:tuolumne", "attached where its far sign is, not only at its point")
	for _, area := range []string{"area:calaveras", "area:tuolumne"} {
		got, _, err := st.QueryEvents(ctx, store.EventQuery{PlaceID: area, Layers: []gridv1.Layer{gridv1.Layer_CHAIN_CONTROL}})
		require.NoError(t, err)
		assert.Len(t, got, 1, area)
	}

	// The same read: nothing written.
	sched.tick(ctx, spec, ps)
	assert.Equal(t, uint32(1), eventRevision(t, st, r2))

	// The stretch grows by a checkpoint: a change.
	src.stretches = []hazards.Feature{chainFeature(r2, "Highway 4", "R2",
		[3]any{"Hathaway Pines", 38.20, -120.36}, [3]any{"Poison Springs", 38.39, -120.18}, [3]any{"Far End", 38.10, -119.60})}
	sched.tick(ctx, spec, ps)
	assert.Equal(t, uint32(2), eventRevision(t, st, r2))

	// A degraded read that lists nothing proves nothing: not swept.
	src.stretches, src.degraded = nil, errors.New("District 10 chain controls: HTTP 503")
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, r2))
	assert.NotEqual(t, gridv1.SourceStatus_OK, sourceByID(t, st, ChainControlSourceID).GetStatus())

	// The level rises: a different requirement, so a different event.
	src.degraded = nil
	src.stretches = []hazards.Feature{chainFeature(r3, "Highway 4", "R3", [3]any{"Poison Springs", 38.39, -120.18})}
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, r2))
	assert.Equal(t, gridv1.EventStatus_ACTIVE, eventStatus(t, st, r3))

	// Caltrans confirms no chain controls: the end of the storm.
	src.stretches = nil
	sched.tick(ctx, spec, ps)
	assert.Equal(t, gridv1.EventStatus_RESOLVED, eventStatus(t, st, r3))
	assert.Equal(t, gridv1.SourceStatus_OK, sourceByID(t, st, ChainControlSourceID).GetStatus())

	// And the same stretch coming back is the same event, active again.
	src.stretches = []hazards.Feature{hwy4()}
	sched.tick(ctx, spec, ps)
	ev, err = st.GetEvent(ctx, r2)
	require.NoError(t, err)
	assert.Equal(t, gridv1.EventStatus_ACTIVE, ev.GetStatus())
	assert.Equal(t, uint32(4), ev.GetRevision(), "appeared, grew, resolved, reappeared: each a revision")
}

func eventRevision(t *testing.T, st *store.Store, id string) uint32 {
	t.Helper()
	ev, err := st.GetEvent(context.Background(), id)
	require.NoError(t, err)
	return ev.GetRevision()
}
