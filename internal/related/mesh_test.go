package related

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// meshNode is a mesh event; reporter is the monitor that reads it ("" = none)
// and via the gateways (keys) it is read through.
func meshNode(key, name, nodeType, reporter string, via ...string) *gridv1.Event {
	e := ev("meshcore:"+key, gridv1.Layer_MESH, "node", name, 0, nil)
	e.Severity = gridv1.Severity_INFO
	d := &gridv1.MeshDetail{PublicKey: key, Name: name, NodeType: nodeType}
	if reporter != "" {
		d.Telemetry = &gridv1.MeshTelemetry{Admin: &gridv1.MeshAdminTelemetry{ReporterId: reporter}, ReadVia: via}
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

// Nodes are related along the reads a monitor states: a gateway and the
// repeaters read through it, repeaters of one gateway, gateways of one
// repeater. Two parts of the mesh the same monitor reads through different
// gateways share nothing.
func TestMeshNodesRelateThroughTheGatewaysThatReadThem(t *testing.T) {
	eagleGW := meshNode("aa01", "Eagle Gateway", "companion", "relay")
	eagleGW2 := meshNode("aa03", "Eagle Annex", "companion", "relay")
	pineGW := meshNode("aa02", "Pinebrook Gateway", "companion", "relay")
	idleGW := meshNode("aa04", "Idle Gateway", "companion", "relay")
	eagleHQ := meshNode("bb01", "Eagle HQ", "repeater", "relay", "aa01", "aa03")
	eagleOne := meshNode("bb02", "Eagle One", "repeater", "relay", "aa01")
	pineRpt := meshNode("bb03", "Pinebrook Rptr", "repeater", "relay", "aa02")
	// Read by the relay, but through a gateway it does not publish.
	hidden := meshNode("bb04", "Shadow", "repeater", "relay")
	other := meshNode("cc01", "Arnold Summit", "repeater", "alanpi")
	lone := meshNode("cc02", "Camp Connell", "repeater", "alanpi")
	unread := meshNode("dd01", "Somebody's node", "repeater", "")
	e, st := newEngine(t)
	ctx := context.Background()
	require.NoError(t, seedSource(ctx, st, "meshcore"))
	for _, n := range []*gridv1.Event{eagleGW, eagleGW2, pineGW, idleGW, eagleHQ, eagleOne, pineRpt, hidden, other, lone, unread} {
		_, err := st.UpsertEvent(ctx, n)
		require.NoError(t, err)
	}
	e.ReporterName = func(id string) string { return map[string]string{"relay": "SIERRA Mesh Relay"}[id] }

	// A gateway: the repeaters read through it, then the gateway that reads
	// one of them too. Nothing of Pinebrook's.
	rs, err := e.Related(ctx, eagleGW)
	require.NoError(t, err)
	assert.Equal(t, map[string]rel{
		"meshcore:bb01": {RuleMeshMonitorGateway, Medium, "Read through this gateway"},
		"meshcore:bb02": {RuleMeshMonitorGateway, Medium, "Read through this gateway"},
		"meshcore:aa03": {RuleMeshMonitorPeer, Context, "Also reads Eagle HQ"},
	}, rels(rs))
	assert.Equal(t, []string{"meshcore:bb01", "meshcore:bb02", "meshcore:aa03"},
		[]string{rs[0].Event.GetId(), rs[1].Event.GetId(), rs[2].Event.GetId()}, "strongest first, then by name")

	// A repeater: its gateways, then the repeaters read through them.
	rs, err = e.Related(ctx, eagleOne)
	require.NoError(t, err)
	assert.Equal(t, map[string]rel{
		"meshcore:aa01": {RuleMeshMonitorGateway, Medium, "A gateway this node is read through"},
		"meshcore:bb01": {RuleMeshMonitorPeer, Context, "Read through the same gateway, Eagle Gateway"},
	}, rels(rs))

	// Pinebrook is its own group.
	rs, err = e.Related(ctx, pineGW)
	require.NoError(t, err)
	assert.Equal(t, map[string]rel{"meshcore:bb03": {RuleMeshMonitorGateway, Medium, "Read through this gateway"}}, rels(rs))
	rs, err = e.Related(ctx, pineRpt)
	require.NoError(t, err)
	assert.Equal(t, map[string]rel{"meshcore:aa02": {RuleMeshMonitorGateway, Medium, "A gateway this node is read through"}}, rels(rs))

	// A gateway that has read nothing, and a repeater with no stated gateway,
	// of a monitor that has gateways: no relations, not the whole monitor.
	for _, n := range []*gridv1.Event{idleGW, hidden} {
		rs, err = e.Related(ctx, n)
		require.NoError(t, err)
		assert.Empty(t, rs, n.GetHeadline())
	}

	// A monitor with no radio of its own on the mesh reads everything from
	// one place: its repeaters are peers. Its name is unknown to the caller,
	// so the id is quoted.
	rs, err = e.Related(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, map[string]rel{"meshcore:cc02": {RuleMeshMonitorPeer, Context, `Also read by the monitor "alanpi"`}}, rels(rs))

	// A node no monitor reads has no relations, and is never anyone's.
	rs, err = e.Related(ctx, unread)
	require.NoError(t, err)
	assert.Empty(t, rs)
}

// A readVia entry may be a key prefix, and the node carrying the freshest
// reading from another monitor is still linked through its gateways.
func TestMeshReadViaResolvesPrefixesAcrossMonitors(t *testing.T) {
	gw := meshNode("aa01ff", "Eagle Gateway", "companion", "relay")
	rpt := meshNode("bb01", "Eagle HQ", "repeater", "alanpi", "AA01")
	e, st := newEngine(t)
	ctx := context.Background()
	require.NoError(t, seedSource(ctx, st, "meshcore"))
	for _, n := range []*gridv1.Event{gw, rpt} {
		_, err := st.UpsertEvent(ctx, n)
		require.NoError(t, err)
	}
	rs, err := e.Related(ctx, gw)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"meshcore:bb01": RuleMeshMonitorGateway}, ids(rs))
}

// The mesh path does not leak into the situational rules, nor they into it.
func TestMeshRelationsStayInTheMeshLayer(t *testing.T) {
	gw := meshNode("aa01", "Eagle Gateway", "companion", "relay")
	gw.Geometry = point(38.12, -120.32)
	rpt := meshNode("bb01", "Eagle HQ", "repeater", "relay", "aa01")
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
