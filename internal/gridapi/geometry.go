package gridapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/dpup/prefab/logging"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// geometryFormat is how a response carries each Geometry's GeoJSON
// (`geometryFormat` on every RPC that returns events, revisions or places).
//
// The store holds GeoJSON as protobuf bytes, so the JSON mapping sends it as a
// base64 string that every client must atob + JSON.parse. That stays the
// default, byte for byte: existing clients such as ersn.net decode it. OBJECT is
// the opt-in clean path: the same geometry as a google.protobuf.Struct in
// Geometry.object, which protojson writes as a plain JSON object, and
// geojson cleared, so a page never carries the geometry twice (it is the
// bulk of a perimeter-heavy page, and pages are cut at a byte budget).
// Geometry.object is proto3 `optional`, so in the default form it is left
// out of the JSON entirely rather than sent as `null`.
type geometryFormat int

const (
	geometryBase64 geometryFormat = iota
	geometryObject
	// geometryNone drops the shape and keeps bbox and centroid: for lists
	// that do not draw. Only the list RPCs that say so accept it
	// (parseListGeometryFormat). ListEvents has its own NONE, which reads the
	// stored shape-free copy; revisions have no such copy and are stripped
	// here, which still saves the bytes on the wire.
	geometryNone
)

func (f geometryFormat) String() string {
	switch f {
	case geometryObject:
		return "OBJECT"
	case geometryNone:
		return "NONE"
	}
	return "BASE64"
}

// parseListGeometryFormat is parseGeometryFormat for the revision lists,
// which also take NONE.
func parseListGeometryFormat(v string) (geometryFormat, error) {
	if strings.EqualFold(strings.TrimSpace(v), "NONE") {
		return geometryNone, nil
	}
	f, err := parseGeometryFormat(v)
	if err != nil {
		return 0, fmt.Errorf("%w; this list also takes NONE", err)
	}
	return f, nil
}

// parseGeometryFormat reads the request parameter: "" is the default,
// matching is case-insensitive, and anything else is refused rather than
// silently served in the default form.
func parseGeometryFormat(v string) (geometryFormat, error) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "", "BASE64":
		return geometryBase64, nil
	case "OBJECT":
		return geometryObject, nil
	default:
		return 0, fmt.Errorf("unknown geometry_format: %q (want BASE64 or OBJECT)", v)
	}
}

// applyGeometryFormat rewrites each geometry in place into format f. BASE64
// is the stored form, so it is a no-op. Callers pass freshly unmarshaled
// protos (the store never hands out shared ones).
//
// A stored GeoJSON that does not parse keeps its base64 geojson and gets no
// object, and the failure is logged: one corrupt geometry must not fail a
// whole page, and the bytes are still there for the client to inspect.
func applyGeometryFormat(ctx context.Context, f geometryFormat, geoms ...*gridv1.Geometry) {
	if f == geometryNone {
		for _, g := range geoms {
			if g != nil {
				g.Geojson, g.Object = nil, nil
			}
		}
		return
	}
	if f != geometryObject {
		return
	}
	for _, g := range geoms {
		if g == nil || len(g.GetGeojson()) == 0 {
			continue
		}
		obj := &structpb.Struct{}
		if err := protojson.Unmarshal(g.GetGeojson(), obj); err != nil {
			logging.Warnw(logging.EnsureLogger(ctx), "gridapi: stored GeoJSON does not parse; serving it as base64", "error", err)
			continue
		}
		g.Object = obj
		g.Geojson = nil
	}
}

func eventGeometries(evs []*gridv1.Event) []*gridv1.Geometry {
	out := make([]*gridv1.Geometry, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.GetGeometry())
	}
	return out
}

func revisionGeometries(revs []*gridv1.EventRevision) []*gridv1.Geometry {
	out := make([]*gridv1.Geometry, 0, len(revs))
	for _, r := range revs {
		out = append(out, r.GetEvent().GetGeometry())
	}
	return out
}

func placeGeometries(places []*gridv1.Place) []*gridv1.Geometry {
	out := make([]*gridv1.Geometry, 0, len(places))
	for _, p := range places {
		out = append(out, p.GetGeometry())
	}
	return out
}
