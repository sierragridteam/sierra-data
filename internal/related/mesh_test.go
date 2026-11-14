package related

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// meshNode is a mesh event; reporter is the monitor that reads it ("" = none).
func meshNode(key, name, nodeType, reporter string) *gridv1.Event {
	e := ev("meshcore:"+key, gridv1.Layer_MESH, "node", name, 0, nil)
	e.Severity = gridv1.Severity_INFO
	d := &gridv1.MeshDetail{PublicKey: key, Name: name, NodeType: nodeType}
	if reporter != "" {
		d.Telemetry = &gridv1.MeshTelemetry{Admin: &gridv1.MeshAdminTelemetry{ReporterId: reporter}}
	}
	e.Detail = &gridv1.Event_Mesh{Mesh: d}
	return e
}

type rel struct {
	rule   string
	conf   Confidence
	reason string
}

func rels(rs []Relation) map[string]rel {
	out := map[string]rel{}
	for _, r := range rs {
		out[r.Event.GetId()] = rel{r.Rule, r.Confidence, r.Reason}
	}
	return out
}

// Nodes read by one monitor are related through it, from either side; a node
// another monitor reads, or none reads, is not.
func TestMeshNodesRelateThroughTheirMonitor(t *testing.T) {
	gwA := meshNode("aa01", "Eagle Gateway", "companion", "relay")
	gwB := meshNode("aa02", "Pinebrook Gateway", "companion", "relay")
	rptA := meshNode("bb01", "Eagle HQ", "repeater", "relay")
	rptB := meshNode("bb02", "Mission Outpost", "repeater", "relay")
	other := meshNode("cc01", "Arnold Summit", "repeater", "alanpi")
	lone := meshNode("cc02", "Camp Connell", "repeater", "alanpi")
	unread := meshNode("dd01", "Somebody's node", "repeater", "")
	e, st := newEngine(t)
	ctx := context.Background()
	require.NoError(t, seedSource(ctx, st, "meshcore"))
	for _, n := range []*gridv1.Event{gwA, gwB, rptA, rptB, other, lone, unread} {
		_, err := st.UpsertEvent(ctx, n)
		require.NoError(t, err)
	}
	e.ReporterName = func(id string) string { return map[string]string{"relay": "SIERRA Mesh Relay"}[id] }

	// A gateway: the nodes its monitor reads first, then its fellow gateway.
	rs, err := e.Related(ctx, gwA)
	require.NoError(t, err)
	assert.Equal(t, map[string]rel{
		"meshcore:bb01": {RuleMeshMonitorGateway, Medium, "Read by SIERRA Mesh Relay, which listens through this gateway"},
		"meshcore:bb02": {RuleMeshMonitorGateway, Medium, "Read by SIERRA Mesh Relay, which listens through this gateway"},
		"meshcore:aa02": {RuleMeshMonitorPeer, Context, "Another gateway of SIERRA Mesh Relay"},
	}, rels(rs))
	assert.Equal(t, []string{"meshcore:bb01", "meshcore:bb02", "meshcore:aa02"},
		[]string{rs[0].Event.GetId(), rs[1].Event.GetId(), rs[2].Event.GetId()}, "strongest first, then by name")

	// A repeater points back at the gateways, and at the other repeater.
	rs, err = e.Related(ctx, rptA)
	require.NoError(t, err)
	assert.Equal(t, map[string]rel{
		"meshcore:aa01": {RuleMeshMonitorGateway, Medium, "A gateway of SIERRA Mesh Relay, which reads this node"},
		"meshcore:aa02": {RuleMeshMonitorGateway, Medium, "A gateway of SIERRA Mesh Relay, which reads this node"},
		"meshcore:bb02": {RuleMeshMonitorPeer, Context, "Also read by SIERRA Mesh Relay"},
	}, rels(rs))

	// A monitor with no radio of its own on the mesh: its repeaters are peers.
	// Its name is unknown to the caller, so the id is quoted.
	rs, err = e.Related(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, map[string]rel{"meshcore:cc02": {RuleMeshMonitorPeer, Context, `Also read by the monitor "alanpi"`}}, rels(rs))

	// A node no monitor reads has no relations, and is never anyone's.
	rs, err = e.Related(ctx, unread)
	require.NoError(t, err)
	assert.Empty(t, rs)
}

// The mesh path does not leak into the situational rules, nor they into it.
func TestMeshRelationsStayInTheMeshLayer(t *testing.T) {
	gw := meshNode("aa01", "Eagle Gateway", "companion", "relay")
	gw.Geometry = point(38.12, -120.32)
	rpt := meshNode("bb01", "Eagle HQ", "repeater", "relay")
	rpt.Geometry = point(38.12, -120.32)
	fire := ev("calfire:loma", gridv1.Layer_WILDFIRE, "wildfire", "Loma Fire — 19 ac", 0, box(38.12, -120.32, 0.01))
	e, st := newEngine(t, fire)
	ctx := context.Background()
	require.NoError(t, seedSource(ctx, st, "meshcore"))
	for _, n := range []*gridv1.Event{gw, rpt} {
		_, err := st.UpsertEvent(ctx, n)
		require.NoError(t, err)
	}
	rs, err := e.Related(ctx, gw)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"meshcore:bb01": RuleMeshMonitorGateway}, ids(rs))
	rs, err = e.Related(ctx, fire)
	require.NoError(t, err)
	assert.Empty(t, rs, "a node standing in a fire's footprint is not related to the fire")
}
