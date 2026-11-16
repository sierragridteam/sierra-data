package gridapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// seedRelatedFire adds a fire beside the presence fixtures' evacuation zone
// (evac:CAL-E-1 at 38.1,-120.4), named in its notes.
func seedRelatedFire(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{{ID: "calfire", Name: "CAL FIRE", PollInterval: time.Minute}}))
	upsert(t, s.Store, &gridv1.Event{
		Id: "calfire:presence", Layer: gridv1.Layer_WILDFIRE, Category: "wildfire",
		Severity: gridv1.Severity_SEVERE, Status: gridv1.EventStatus_ACTIVE,
		Headline:  "Presence Fire — 40 ac, 0% contained",
		Geometry:  pointGeom(38.1, -120.41),
		Effective: timestamppb.New(base.Add(-time.Hour)), ObservedAt: timestamppb.New(base.Add(-time.Hour)),
		Provenance: &gridv1.Provenance{SourceId: "calfire", SourceName: "CAL FIRE"},
		Detail:     &gridv1.Event_Wildfire{Wildfire: &gridv1.WildfireDetail{Acres: 40}},
	})
}

func TestListRelatedEvents(t *testing.T) {
	s := newTestService(t)
	seedPresenceFixtures(t, s)
	seedRelatedFire(t, s)
	g := NewGridServer(s)

	res, err := g.ListRelatedEvents(rpcBG(), &gridv1.ListRelatedEventsRequest{Id: "calfire:presence"})
	require.NoError(t, err)
	var evac *gridv1.RelatedEvent
	for _, r := range res.GetRelated() {
		if r.GetEvent().GetId() == "evac:CAL-E-1" {
			evac = r
		}
	}
	require.NotNil(t, evac, "the zone 0.9 km away, issued an hour after the fire started")
	assert.Equal(t, "fire-evacuation-near", evac.GetRule())
	assert.Equal(t, gridv1.RelationConfidence_RELATION_MEDIUM, evac.GetConfidence())
	assert.NotEmpty(t, evac.GetReason())
	assert.Empty(t, evac.GetEvent().GetGeometry().GetGeojson(), "related events carry no shapes")

	back, err := g.ListRelatedEvents(rpcBG(), &gridv1.ListRelatedEventsRequest{Id: "evac:CAL-E-1"})
	require.NoError(t, err)
	require.NotEmpty(t, back.GetRelated())
	assert.Equal(t, "calfire:presence", back.GetRelated()[0].GetEvent().GetId())

	_, err = g.ListRelatedEvents(rpcBG(), &gridv1.ListRelatedEventsRequest{Id: "nope:1"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// A mesh node's related events are the nodes a monitor's reads connect it to.
func TestListRelatedEventsMeshNodes(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	require.NoError(t, s.Store.SeedSources(ctx, []store.SourceSeed{
		{ID: "meshcore", Name: "MeshCore", PollInterval: time.Minute},
		{ID: "relay", Name: "SIERRA Mesh Relay", Kind: gridv1.SourceKind_REPORTER, PollInterval: time.Minute},
	}))
	node := func(key, name, nodeType, reporter string, via ...string) {
		d := &gridv1.MeshDetail{PublicKey: key, Name: name, NodeType: nodeType}
		if reporter != "" {
			d.Telemetry = &gridv1.MeshTelemetry{Admin: &gridv1.MeshAdminTelemetry{ReporterId: reporter}, ReadVia: via}
		}
		upsert(t, s.Store, &gridv1.Event{
			Id: "meshcore:" + key, Layer: gridv1.Layer_MESH, Category: "node", Headline: name,
			Severity: gridv1.Severity_INFO, Status: gridv1.EventStatus_ACTIVE,
			Geometry:  pointGeom(38.2, -120.3),
			Effective: timestamppb.New(base), ObservedAt: timestamppb.New(base),
			Provenance: &gridv1.Provenance{SourceId: "meshcore", SourceName: "MeshCore"},
			Detail:     &gridv1.Event_Mesh{Mesh: d},
		})
	}
	node("aa01", "Eagle Gateway", "companion", "relay")
	node("bb01", "Eagle HQ", "repeater", "relay", "aa01")
	node("aa02", "Pinebrook Gateway", "companion", "relay")
	node("bb02", "Pinebrook Rptr", "repeater", "relay", "aa02")
	node("dd01", "Somebody's node", "repeater", "")
	g := NewGridServer(s)

	res, err := g.ListRelatedEvents(rpcBG(), &gridv1.ListRelatedEventsRequest{Id: "meshcore:aa01"})
	require.NoError(t, err)
	require.Len(t, res.GetRelated(), 1)
	r := res.GetRelated()[0]
	assert.Equal(t, "meshcore:bb01", r.GetEvent().GetId())
	assert.Equal(t, "mesh-monitor-gateway", r.GetRule())
	assert.Equal(t, gridv1.RelationConfidence_RELATION_MEDIUM, r.GetConfidence())
	assert.Equal(t, "Read through this gateway", r.GetReason())
	assert.Empty(t, r.GetEvent().GetGeometry().GetGeojson(), "related events carry no shapes")
	assert.Equal(t, "relay", r.GetEvent().GetMesh().GetTelemetry().GetAdmin().GetReporterId(), "the related node keeps its reading")

	back, err := g.ListRelatedEvents(rpcBG(), &gridv1.ListRelatedEventsRequest{Id: "meshcore:bb01"})
	require.NoError(t, err)
	require.Len(t, back.GetRelated(), 1)
	assert.Equal(t, "meshcore:aa01", back.GetRelated()[0].GetEvent().GetId())
	assert.Equal(t, "A gateway this node is read through", back.GetRelated()[0].GetReason())

	none, err := g.ListRelatedEvents(rpcBG(), &gridv1.ListRelatedEventsRequest{Id: "meshcore:dd01"})
	require.NoError(t, err)
	assert.Empty(t, none.GetRelated(), "a node no monitor reads")
}
