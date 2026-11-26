package gridapi

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// LayerNature says what an event of a layer is: something that happens, or
// the standing state of something the Grid tracks. It is the one place that
// knows. The summary's hazard rollups, every client's "how many events are
// active" and the API's Event.nature all follow from it, so a new layer is
// classified here once and nowhere else.
//
// STATE layers: one long-lived event per thing (a mesh node, a county's burn
// status, an upstream feed, a monitored road, a weather location), ACTIVE for
// as long as the thing exists, revised when its state changes. Everything
// else is an OCCURRENCE: it begins and it ends.
func LayerNature(l gridv1.Layer) gridv1.EventNature {
	switch l {
	case gridv1.Layer_LAYER_UNSPECIFIED:
		return gridv1.EventNature_EVENT_NATURE_UNSPECIFIED
	case gridv1.Layer_MESH, gridv1.Layer_BURN_STATUS, gridv1.Layer_SOURCE_HEALTH,
		gridv1.Layer_ROAD_SEGMENT, gridv1.Layer_WEATHER:
		return gridv1.EventNature_STATE
	}
	return gridv1.EventNature_OCCURRENCE
}

// NatureInterceptor sets Event.nature on every event in every GridService
// response, wherever it sits (a list, a revision's snapshot, a search hit, a
// related event). It is done here, over the finished response, and not where
// each RPC builds its events, so no present or future response path can
// return an event without it. The field is derived, never stored.
func NatureInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			if m, ok := resp.(proto.Message); ok {
				StampNature(m)
			}
		}
		return resp, err
	}
}

// StampNature sets Event.nature on every grid.v1.Event reachable from m.
func StampNature(m proto.Message) {
	if m == nil {
		return
	}
	stampNature(m.ProtoReflect())
}

var eventName = (&gridv1.Event{}).ProtoReflect().Descriptor().FullName()

func stampNature(m protoreflect.Message) {
	if !m.IsValid() {
		return
	}
	if m.Descriptor().FullName() == eventName {
		if ev, ok := m.Interface().(*gridv1.Event); ok {
			ev.Nature = LayerNature(ev.GetLayer())
			completeBeyond(ev)
		}
		return // an event holds no other event
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Message() == nil || !strings.HasPrefix(string(fd.Message().FullName()), "grid.v1.") {
			return true // scalars, timestamps, wrappers, structs
		}
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					stampNature(mv.Message())
					return true
				})
			}
		case fd.IsList():
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				stampNature(list.Get(i).Message())
			}
		default:
			stampNature(v.Message())
		}
		return true
	})
}

// completeBeyond fills `seasonal` and `severity` on a road event's closures
// past its end (roadSegment.beyond[]) where the stored snapshot has neither:
// every revision recorded before 2026-11-26, when the two fields were added.
// Unfilled, such an entry would read `seasonal: false`, which nobody ever
// recorded and which is wrong: an entry is in `beyond` only because it
// matched a configured named seasonal closure, then as now, and its severity
// has been INFO since the list was introduced (hazards' roadBeyond says the
// same of the map layer). An empty severity is the mark of an entry written
// before the fields; one written since always has it, and is left alone.
//
// Done here with nature, over every response, so a revision in a history
// page, a search hit and the current event all read alike. Nothing is
// written back: the stored snapshots and their hashes do not change.
func completeBeyond(ev *gridv1.Event) {
	for _, b := range ev.GetRoadSegment().GetBeyond() {
		if b.GetSeverity() == "" {
			b.Seasonal, b.Severity = true, "INFO"
		}
	}
}
