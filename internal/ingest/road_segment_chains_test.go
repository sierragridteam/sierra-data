package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	api "github.com/sierragridteam/sierra-data/api/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/hazards"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// fakeRoadChains is hazards.Service.RoadChainsOn: per road id, the stretches
// on it. unknown is the read that cannot say.
type fakeRoadChains struct {
	on      map[string][]hazards.RoadChainProps
	unknown bool
}

func (f *fakeRoadChains) RoadChainsOn(context.Context) func(config.MonitoredRoad, *api.Road) ([]hazards.RoadChainProps, bool) {
	return func(mr config.MonitoredRoad, _ *api.Road) ([]hazards.RoadChainProps, bool) {
		if f.unknown {
			return nil, false
		}
		return append([]hazards.RoadChainProps{}, f.on[mr.ID]...), true
	}
}

func r2OnHwy4(from, to string) map[string][]hazards.RoadChainProps {
	return map[string][]hazards.RoadChainProps{"hwy4-arnold-bearvalley": {{
		Level: "R2", EventID: "cc:hwy4:r2", From: from, To: to,
		Headline: "Hwy 4 chains required, " + from + " to " + to + " (R2)",
	}}}
}

// chainsNormalizer is one open road (Hwy 4 Arnold to Bear Valley), a lister
// whose refresh time the test moves, and the chain read.
func chainsNormalizer(t *testing.T) (*RoadSegmentNormalizer, *fakeRoadLister, *fakeRoadChains) {
	t.Helper()
	lister := &fakeRoadLister{at: time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC),
		roads: []*api.Road{road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 38)}}
	chains := &fakeRoadChains{}
	n := oneRoadNormalizer(lister).UseRoadChains(chains)
	n.now = func() time.Time { return lister.at.Add(time.Minute) }
	return n, lister, chains
}

func pollOne(t *testing.T, n *RoadSegmentNormalizer, held *gridv1.Event) *gridv1.Event {
	t.Helper()
	var prior Prior = &fakePrior{}
	if held != nil {
		prior = &scriptedPrior{events: []*gridv1.Event{held}}
	}
	res, err := n.Poll(testCtx(), prior)
	require.NoError(t, err)
	require.Empty(t, res.PerSource)
	require.Len(t, res.Events, 1)
	return res.Events[0]
}

// A road under a chain requirement is open, and its event says so beside the
// status: the level and the stretch on the detail, "chains required" in the
// headline, "chains" as the category. Its severity is the open road's.
func TestRoadSegmentChains_OpenUnderChains(t *testing.T) {
	n, lister, chains := chainsNormalizer(t)
	open := pollOne(t, n, nil)
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: open", open.GetHeadline())
	assert.Equal(t, "open", open.GetCategory())
	assert.Empty(t, open.GetRoadSegment().GetChains())

	// R2 goes up on the road: recorded at once, by the same poll.
	chains.on = r2OnHwy4("Blue Lake Springs", "Mount Reba Road")
	under := pollOne(t, n, open)
	d := under.GetRoadSegment()
	assert.Equal(t, "OPEN", d.GetStatus(), "chains never change the status")
	require.Len(t, d.GetChains(), 1)
	assert.Equal(t, "R2", d.GetChains()[0].GetLevel())
	assert.Equal(t, "cc:hwy4:r2", d.GetChains()[0].GetEventId())
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: open, chains required (R2)", under.GetHeadline())
	assert.Equal(t, "chains", under.GetCategory())
	assert.Equal(t, open.GetSeverity(), under.GetSeverity(), "the stretch's own event is the rated one")
	assert.NotEqual(t, store.ContentHash(open), store.ContentHash(under), "a revision")

	// The stretch grows through the storm. That is the stretch's history,
	// not the road's: no revision here.
	lister.at = lister.at.Add(15 * time.Minute)
	chains.on = r2OnHwy4("Arnold", "Mount Reba Road")
	grown := pollOne(t, n, under)
	assert.Equal(t, store.ContentHash(under), store.ContentHash(grown))
}

// Wiring the chain read is not a change: a road with no chains hashes as it
// did before the event carried them, so the deploy mints no revision.
func TestRoadSegmentChains_NoChainsHashesAsBefore(t *testing.T) {
	lister := &fakeRoadLister{roads: []*api.Road{road("hwy4-arnold-bearvalley", api.RoadStatus_OPEN, api.CongestionLevel_CLEAR, 38)}}
	before := pollOne(t, oneRoadNormalizer(lister), nil)
	after := pollOne(t, oneRoadNormalizer(lister).UseRoadChains(&fakeRoadChains{}), nil)
	assert.Equal(t, store.ContentHash(before), store.ContentHash(after))
}

// Chains lifted is held like a reopening: recorded when a later refresh says
// the same. A requirement that comes back in between was never lifted.
func TestRoadSegmentChains_LiftingWaitsForASecondRefresh(t *testing.T) {
	n, lister, chains := chainsNormalizer(t)
	chains.on = r2OnHwy4("Blue Lake Springs", "Mount Reba Road")
	under := pollOne(t, n, nil)
	require.Equal(t, "chains", under.GetCategory())

	// One refresh reads no requirement. Held.
	lister.at = lister.at.Add(15 * time.Minute)
	chains.on = nil
	assert.Equal(t, store.ContentHash(under), store.ContentHash(pollOne(t, n, under)))
	assert.Equal(t, store.ContentHash(under), store.ContentHash(pollOne(t, n, under)), "the same refresh, offered again")

	// The next has it again: nothing was recorded, and the count starts over.
	lister.at = lister.at.Add(15 * time.Minute)
	chains.on = r2OnHwy4("Blue Lake Springs", "Mount Reba Road")
	assert.Equal(t, store.ContentHash(under), store.ContentHash(pollOne(t, n, under)))
	lister.at = lister.at.Add(15 * time.Minute)
	chains.on = nil
	assert.Equal(t, "chains", pollOne(t, n, under).GetCategory(), "first refresh without it")

	// A second refresh agrees: lifted.
	lister.at = lister.at.Add(15 * time.Minute)
	lifted := pollOne(t, n, under)
	assert.Empty(t, lifted.GetRoadSegment().GetChains())
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: open", lifted.GetHeadline())
	assert.Equal(t, "open", lifted.GetCategory())
}

// A higher level is recorded at once, a lower one when a second refresh
// agrees. A level is its own stretch (cc:hwy4:r1, cc:hwy4:r2).
func TestRoadSegmentChains_LevelChanges(t *testing.T) {
	n, lister, chains := chainsNormalizer(t)
	level := func(l string) map[string][]hazards.RoadChainProps {
		return map[string][]hazards.RoadChainProps{"hwy4-arnold-bearvalley": {{Level: l, EventID: "cc:hwy4:" + map[string]string{"R1": "r1", "R2": "r2"}[l]}}}
	}
	chains.on = level("R1")
	r1 := pollOne(t, n, nil)
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: open, chains required (R1)", r1.GetHeadline())

	lister.at = lister.at.Add(15 * time.Minute)
	chains.on = level("R2")
	r2 := pollOne(t, n, r1)
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: open, chains required (R2)", r2.GetHeadline(), "raised at once")

	lister.at = lister.at.Add(15 * time.Minute)
	chains.on = level("R1")
	assert.Equal(t, store.ContentHash(r2), store.ContentHash(pollOne(t, n, r2)), "lowered: held one refresh")
	lister.at = lister.at.Add(15 * time.Minute)
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: open, chains required (R1)", pollOne(t, n, r2).GetHeadline())

	// Two stretches on one road: the headline goes by the higher, the detail
	// lists both, a stretch once.
	lister.at = lister.at.Add(15 * time.Minute)
	chains.on = map[string][]hazards.RoadChainProps{"hwy4-arnold-bearvalley": {
		{Level: "R2", EventID: "cc:hwy4:r2"}, {Level: "R1", EventID: "cc:hwy4:r1"}, {Level: "R2", EventID: "cc:hwy4:r2"},
	}}
	both := pollOne(t, n, r1)
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: open, chains required (R2)", both.GetHeadline())
	require.Len(t, both.GetRoadSegment().GetChains(), 2)
	assert.Equal(t, "cc:hwy4:r2", both.GetRoadSegment().GetChains()[0].GetEventId())
	assert.Equal(t, "cc:hwy4:r1", both.GetRoadSegment().GetChains()[1].GetEventId())
}

// A chain read that cannot say (a Caltrans source down, an unreadable sign
// on the highway) is not "no chains": the event keeps what it has, with no
// revision, for as long as the read cannot say.
func TestRoadSegmentChains_UnknownKeepsTheLastState(t *testing.T) {
	n, lister, chains := chainsNormalizer(t)
	chains.on = r2OnHwy4("Blue Lake Springs", "Mount Reba Road")
	under := pollOne(t, n, nil)

	chains.unknown, chains.on = true, nil
	for i := 0; i < 3; i++ {
		lister.at = lister.at.Add(15 * time.Minute)
		kept := pollOne(t, n, under)
		assert.Equal(t, store.ContentHash(under), store.ContentHash(kept))
		assert.Equal(t, "chains", kept.GetCategory())
	}

	// A road first seen while the read cannot say has no chains to keep.
	fresh := pollOne(t, n, nil)
	assert.Empty(t, fresh.GetRoadSegment().GetChains())
	assert.Equal(t, "open", fresh.GetCategory())

	// The read answers again, with none: now it counts, from its first refresh.
	chains.unknown = false
	lister.at = lister.at.Add(15 * time.Minute)
	assert.Equal(t, "chains", pollOne(t, n, under).GetCategory())
	lister.at = lister.at.Add(15 * time.Minute)
	assert.Equal(t, "open", pollOne(t, n, under).GetCategory())
}

// A closed or restricted road can be under chains too. The status is the
// stronger statement and stays the category; the chains are on the detail
// and in the headline. Reopening under chains is still an easing.
func TestRoadSegmentChains_ClosedUnderChains(t *testing.T) {
	n, lister, chains := chainsNormalizer(t)
	chains.on = r2OnHwy4("Blue Lake Springs", "Mount Reba Road")
	lister.roads[0].Status = api.RoadStatus_CLOSED
	lister.roads[0].StatusCauses = []*api.RoadStatusCause{conditionCause(api.RoadStatus_CLOSED, "IS CLOSED FROM CAMP CONNELL TO BEAR VALLEY - DUE TO SNOW")}
	closed := pollOne(t, n, nil)
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: closed, chains required (R2)", closed.GetHeadline())
	assert.Equal(t, "closed", closed.GetCategory())
	require.Len(t, closed.GetRoadSegment().GetChains(), 1)

	lister.at = lister.at.Add(15 * time.Minute)
	lister.roads[0].Status, lister.roads[0].StatusCauses = api.RoadStatus_OPEN, nil
	assert.Equal(t, "closed", pollOne(t, n, closed).GetCategory(), "held one refresh")
	lister.at = lister.at.Add(15 * time.Minute)
	reopened := pollOne(t, n, closed)
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: open, chains required (R2)", reopened.GetHeadline())
	assert.Equal(t, "chains", reopened.GetCategory())

	// Closing is recorded at once even while the chains are being lifted.
	lister.at = lister.at.Add(15 * time.Minute)
	chains.on = nil
	lister.roads[0].Status = api.RoadStatus_CLOSED
	lister.roads[0].StatusCauses = []*api.RoadStatusCause{conditionCause(api.RoadStatus_CLOSED, "IS CLOSED FROM CAMP CONNELL TO BEAR VALLEY - DUE TO SNOW")}
	again := pollOne(t, n, reopened)
	assert.Equal(t, "Hwy 4 Arnold to Bear Valley: closed", again.GetHeadline())
}
