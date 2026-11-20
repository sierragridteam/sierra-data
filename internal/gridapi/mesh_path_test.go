package gridapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// key64 pads a short label to a full public key, so fixtures read as "aa",
// "bb" and still pass the 64-hex rule.
func key64(label string) string {
	return label + strings.Repeat("0", 64-len(label))
}

func link(a, b string, obs int, lastSeen time.Time) store.MeshLink {
	a, b = key64(a), key64(b)
	if a > b {
		a, b = b, a
	}
	return store.MeshLink{A: a, B: b, Observations: obs, DaysActive: 1, FirstSeen: lastSeen.Add(-time.Hour), LastSeen: lastSeen, BestSNR: 6}
}

func keys(labels ...string) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = key64(l)
	}
	return out
}

func anyRelays(string) bool { return true }

func TestMeshGraphPrefersFewerHops(t *testing.T) {
	now := base
	g := newMeshGraph([]store.MeshLink{
		link("aa", "bb", 10, now), link("bb", "cc", 10, now), link("cc", "dd", 10, now),
		link("aa", "ee", 10, now), link("ee", "dd", 10, now),
	}, now, anyRelays)
	assert.Equal(t, keys("aa", "ee", "dd"), g.shortest(keys("aa"), keys("dd"), nil, nil))
	assert.Nil(t, g.shortest(keys("aa"), keys("ff"), nil, nil), "a node with no links is not reached")
	assert.Equal(t, keys("aa"), g.shortest(keys("aa", "bb"), keys("aa"), nil, nil), "one node on both sides")
}

// A link last seen days ago or seen once is half a hop each: two such links
// lose to three that are carrying traffic.
func TestMeshGraphDiscountsStaleAndThinLinks(t *testing.T) {
	now := base
	old := now.Add(-48 * time.Hour)
	g := newMeshGraph([]store.MeshLink{
		link("aa", "ee", 1, old), link("ee", "dd", 1, old), // 2 hops, each stale and thin: cost 4
		link("aa", "bb", 10, now), link("bb", "cc", 10, now), link("cc", "dd", 10, now), // 3 clean hops: cost 3
	}, now, anyRelays)
	assert.Equal(t, keys("aa", "bb", "cc", "dd"), g.shortest(keys("aa"), keys("dd"), nil, nil))
}

// A companion ends a path but never carries one.
func TestMeshGraphPassesOnlyThroughRelays(t *testing.T) {
	now := base
	companion := key64("cc")
	g := newMeshGraph([]store.MeshLink{
		link("aa", "cc", 10, now), link("cc", "dd", 10, now),
		link("aa", "bb", 10, now), link("bb", "ee", 10, now), link("ee", "dd", 10, now),
	}, now, func(k string) bool { return k != companion })
	assert.Equal(t, keys("aa", "bb", "ee", "dd"), g.shortest(keys("aa"), keys("dd"), nil, nil))
	assert.Equal(t, keys("aa", "cc"), g.shortest(keys("aa"), keys("cc"), nil, nil), "it can be the destination")
	assert.Equal(t, keys("cc", "dd"), g.shortest(keys("cc"), keys("dd"), nil, nil), "or the origin")
}

func TestMeshGraphFromAndToAreSets(t *testing.T) {
	now := base
	g := newMeshGraph([]store.MeshLink{
		link("a1", "bb", 10, now), link("bb", "cc", 10, now), link("cc", "d1", 10, now),
		link("a2", "d2", 10, now),
	}, now, anyRelays)
	assert.Equal(t, keys("a2", "d2"), g.shortest(keys("a1", "a2"), keys("d1", "d2"), nil, nil))
}

func seedPathNode(t *testing.T, s *Service, label, name, role string, status gridv1.EventStatus) {
	t.Helper()
	_, err := s.Store.UpsertEvent(context.Background(), &gridv1.Event{
		Id: "meshcore:" + key64(label), Layer: gridv1.Layer_MESH, Severity: gridv1.Severity_INFO,
		Status: status, Headline: name, Geometry: pointGeom(38.1, -120.5),
		ObservedAt: timestamppb.New(base),
		Provenance: &gridv1.Provenance{SourceId: "meshcore"},
		Detail:     &gridv1.Event_Mesh{Mesh: &gridv1.MeshDetail{PublicKey: key64(label), NodeType: role, Name: name}},
	})
	require.NoError(t, err)
}

// seedPathMesh is A - B - D with a second way round through E, a companion C
// hanging off B, and X: a relay the catalogue has never recorded.
func seedPathMesh(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{
		{ID: "meshcore", Name: "MeshCore Mesh", PollInterval: time.Minute},
	}))
	seedPathNode(t, s, "aa", "Arnold Summit", "repeater", gridv1.EventStatus_ACTIVE)
	seedPathNode(t, s, "bb", "Forest Meadows", "repeater", gridv1.EventStatus_ACTIVE)
	seedPathNode(t, s, "cc", "Handheld", "companion", gridv1.EventStatus_ACTIVE)
	seedPathNode(t, s, "dd", "Bear Valley", "repeater", gridv1.EventStatus_EXPIRED)
	seedPathNode(t, s, "ee", "Lake Alpine", "repeater", gridv1.EventStatus_ACTIVE)
	seedPathNode(t, s, "ff", "Island", "repeater", gridv1.EventStatus_ACTIVE)
	heard := base.Add(-2 * time.Hour)
	var obs []store.MeshObservation
	// eachEdge chains origin -> path hops, so one reception with two hops
	// yields two links. Three receptions each, so no link is thin.
	for i := 0; i < 3; i++ {
		at := heard.Add(time.Duration(i) * time.Minute)
		obs = append(obs,
			store.MeshObservation{PubKey: key64("aa"), HeardAt: at, SNR: 9, PathNodes: []string{key64("bb"), key64("dd")}},
			store.MeshObservation{PubKey: key64("aa"), HeardAt: at, SNR: 2, PathNodes: []string{key64("99"), key64("ee"), key64("dd")}},
			store.MeshObservation{PubKey: key64("cc"), HeardAt: at, SNR: 4, PathNodes: []string{key64("bb")}},
		)
	}
	require.NoError(t, s.Store.InsertMeshObservations(ctx, obs))
	require.NoError(t, s.Store.RecordMeshTick(ctx, nil, []store.MeshActivity{
		{PubKey: key64("bb"), LastRelayedAt: heard},
	}))
}

func getMesh(t *testing.T, s *Service, target string, v any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if strings.HasPrefix(target, "/api/v1/mesh/path") {
		s.serveMeshPath(rec, req)
	} else {
		s.serveMeshLinks(rec, req)
	}
	if v != nil {
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		decode(t, rec, v)
	}
	return rec
}

func TestServeMeshPath(t *testing.T) {
	s := newTestService(t)
	seedPathMesh(t, s)
	unknown := key64("99")

	var resp meshPathResponse
	getMesh(t, s, "/api/v1/mesh/path?from=meshcore:"+key64("aa")+"&to="+strings.ToUpper(key64("dd")), &resp)
	require.True(t, resp.Found)
	assert.Equal(t, keys("aa", "bb", "dd"), resp.Path.Nodes)
	assert.Equal(t, 2, resp.Path.Hops)
	require.Len(t, resp.Path.Links, 2)
	assert.Equal(t, 3, resp.Path.Links[0].Observations)
	assert.InDelta(t, 9, resp.Path.Links[0].BestSnr, 1e-9)

	// The second route shares no relay with the first, and crosses a relay
	// the catalogue does not hold: it is in the route and absent from nodes.
	require.NotNil(t, resp.Alternate)
	assert.Equal(t, []string{key64("aa"), unknown, key64("ee"), key64("dd")}, resp.Alternate.Nodes)
	assert.NotContains(t, resp.Nodes, unknown)
	assert.Equal(t, "Forest Meadows", resp.Nodes[key64("bb")].Name)
	assert.Equal(t, "EXPIRED", resp.Nodes[key64("dd")].Status, "an expired node still ends a link seen in the window")
	require.NotNil(t, resp.Nodes[key64("bb")].LastRelayedAt)
	assert.Nil(t, resp.Nodes[key64("aa")].LastRelayedAt, "no activity on record is null, not a time")
	assert.Equal(t, 2, resp.LinkCounts[key64("aa")])
	assert.Contains(t, resp.Basis, "not a guarantee")

	// Nothing observed joins the island to anything: found is false, the
	// basis still rides, and the count says the island has no links at all.
	resp = meshPathResponse{}
	getMesh(t, s, "/api/v1/mesh/path?from="+key64("aa")+"&to="+key64("ff"), &resp)
	assert.False(t, resp.Found)
	assert.Nil(t, resp.Path)
	assert.Nil(t, resp.Alternate)
	assert.Equal(t, 0, resp.LinkCounts[key64("ff")])
	assert.Contains(t, resp.Basis, "was not observed")

	// A companion is a valid end; the route to it goes through its repeater.
	resp = meshPathResponse{}
	getMesh(t, s, "/api/v1/mesh/path?from="+key64("cc")+"&to="+key64("dd")+","+key64("ff"), &resp)
	require.True(t, resp.Found)
	assert.Equal(t, keys("cc", "bb", "dd"), resp.Path.Nodes)
	assert.Nil(t, resp.Alternate, "the companion's only link is through bb")
}

func TestServeMeshPathRejectsBadEndpoints(t *testing.T) {
	s := newTestService(t)
	many := ""
	for i := 0; i < maxPathEndpoints+1; i++ {
		many += key64(string(rune('a'+i))+"1") + ","
	}
	for _, target := range []string{
		"/api/v1/mesh/path",
		"/api/v1/mesh/path?from=" + key64("aa"),
		"/api/v1/mesh/path?from=aabbccdd&to=" + key64("dd"), // a prefix is not one node
		"/api/v1/mesh/path?from=Arnold&to=" + key64("dd"),
		"/api/v1/mesh/path?from=" + many + "&to=" + key64("dd"),
		"/api/v1/mesh/path?window=soon&from=" + key64("aa") + "&to=" + key64("dd"),
	} {
		rec := getMesh(t, s, target, nil)
		requireStatus(t, rec, http.StatusBadRequest, int(codes.InvalidArgument))
	}
}

func TestServeMeshNodeLinks(t *testing.T) {
	s := newTestService(t)
	seedPathMesh(t, s)

	var resp meshNodeLinksResponse
	getMesh(t, s, "/api/v1/mesh/links?node="+key64("bb"), &resp)
	assert.Equal(t, key64("bb"), resp.Node)
	assert.Equal(t, "72h", resp.Window)
	require.Len(t, resp.Links, 3, "aa, cc and dd; not the ee side of the mesh")
	for _, l := range resp.Links {
		assert.True(t, l.A == key64("bb") || l.B == key64("bb"))
	}
	assert.Equal(t, "Forest Meadows", resp.Nodes[key64("bb")].Name)
	assert.Equal(t, "companion", resp.Nodes[key64("cc")].NodeType)
	assert.Len(t, resp.Nodes, 4)

	// A node with no links in the window is an empty list, with its own name.
	resp = meshNodeLinksResponse{}
	getMesh(t, s, "/api/v1/mesh/links?node="+key64("ff"), &resp)
	assert.Empty(t, resp.Links)
	assert.NotNil(t, resp.Links)
	assert.Equal(t, "Island", resp.Nodes[key64("ff")].Name)

	// A key the catalogue does not hold is still a valid question.
	resp = meshNodeLinksResponse{}
	getMesh(t, s, "/api/v1/mesh/links?node="+key64("99"), &resp)
	assert.Len(t, resp.Links, 2)
	assert.NotContains(t, resp.Nodes, key64("99"))

	for _, bad := range []string{"", "bb", "aabbccdd", "SIERRA"} {
		rec := getMesh(t, s, "/api/v1/mesh/links?node="+bad, nil)
		requireStatus(t, rec, http.StatusBadRequest, int(codes.InvalidArgument))
	}

	// The validator ignores generatedAt, as the unfiltered list's does.
	first := getMesh(t, s, "/api/v1/mesh/links?node="+key64("bb"), nil)
	s.Now = func() time.Time { return base.Add(time.Second) }
	second := getMesh(t, s, "/api/v1/mesh/links?node="+key64("bb"), nil)
	assert.Equal(t, first.Header().Get("ETag"), second.Header().Get("ETag"))
	assert.NotEqual(t, first.Body.String(), second.Body.String())
}

// Without ?node the list is the whole mesh in its original shape: no node, no
// nodes.
func TestServeMeshLinksUnfilteredShapeUnchanged(t *testing.T) {
	s := newTestService(t)
	seedPathMesh(t, s)
	rec := getMesh(t, s, "/api/v1/mesh/links", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"nodes"`)
	assert.NotContains(t, rec.Body.String(), `"node"`)
}
