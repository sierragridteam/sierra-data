package ingest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/config"
	"github.com/sierragridteam/sierra-data/internal/hazards"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ChainControlSourceID is the registry row of the chain-control events. Its
// own id, not `caltrans`: the disappearance sweep runs per source, and
// `caltrans` belongs to the lane-closure poller, whose sweep would resolve
// every chain requirement it does not list.
const ChainControlSourceID = "caltrans-cc"

// ChainRequirementSource is the read the poller needs: the chain-control
// stretches with a sign inside one of the areas (hazards.Service).
type ChainRequirementSource interface {
	ChainRequirements(ctx context.Context, areas []config.HazardArea) (stretches []hazards.Feature, degraded, err error)
}

// ChainControlNormalizer records chain requirements as events (layer
// CHAIN_CONTROL, id namespace "cc:"): one event per stretch of highway under
// one requirement level, the same stretch and id as the chain_control map
// layer shows, because both come from hazards.Service over one cached read.
//
// A requirement starts, holds and ends, and until this poller existed nothing
// stored that: the map layer is a live projection, so a storm's sequence was
// gone when it was over. Lifecycle is `disappearance: resolve`: a stretch
// missing from a successful, complete read is RESOLVED, and a confirmed-empty
// read resolves every one of them, which is the end of the storm. A change of
// level is a different id by design (see hazards.chainStretchID).
//
// Fail-loud, like the lane closures: a failed registry read is a hard error;
// a read that cannot vouch for completeness (a district file failed, or an
// in-scope sign is unrecognized or has no position) is a PerSource error, so
// the stretches it did report land and nothing is swept.
type ChainControlNormalizer struct {
	cfg    *config.Config
	source ChainRequirementSource
	// placesAt returns the ids of the places containing a point (the store's
	// PlacesContaining). nil in tests that do not care about places.
	placesAt func(ctx context.Context, lat, lng float64) ([]string, error)
	now      func() time.Time
}

// NewChainControlNormalizer wires the poller. placesAt may be nil.
func NewChainControlNormalizer(cfg *config.Config, source ChainRequirementSource, placesAt func(ctx context.Context, lat, lng float64) ([]string, error)) *ChainControlNormalizer {
	return &ChainControlNormalizer{cfg: cfg, source: source, placesAt: placesAt, now: time.Now}
}

// SourceIDs implements Normalizer.
func (n *ChainControlNormalizer) SourceIDs() []string { return []string{ChainControlSourceID} }

// Poll implements Normalizer.
func (n *ChainControlNormalizer) Poll(ctx context.Context, _ Prior) (*PollResult, error) {
	areas := n.cfg.Hazards.Areas
	if len(areas) == 0 {
		return nil, errEmptyScope("hazard areas")
	}
	stretches, degraded, err := n.source.ChainRequirements(ctx, areas)
	if err != nil {
		return nil, err
	}
	now := n.now()
	res := &PollResult{}
	for _, f := range stretches {
		ev, err := n.event(ctx, f, now)
		if err != nil {
			// A stretch we cannot build is a stretch missing from the set:
			// the sweep must not read that as lifted.
			degraded = joinErr(degraded, err)
			continue
		}
		res.Events = append(res.Events, ev)
	}
	if degraded != nil {
		res.PerSource = map[string]error{ChainControlSourceID: degraded}
	}
	return res, nil
}

func joinErr(a, b error) error {
	if a == nil {
		return b
	}
	return fmt.Errorf("%w; %w", a, b)
}

// event builds the stored event from the map layer's feature for a stretch.
func (n *ChainControlNormalizer) event(ctx context.Context, f hazards.Feature, now time.Time) (*gridv1.Event, error) {
	p := f.Properties
	cc := p.ChainControl
	if cc == nil || cc.From == nil || cc.To == nil {
		return nil, fmt.Errorf("chain control %s: no stretch detail", p.ID)
	}
	var at []float64
	for _, s := range cc.Signs {
		if len(s.Coordinates) == 2 {
			at = s.Coordinates
			break
		}
	}
	if at == nil {
		return nil, fmt.Errorf("chain control %s: no sign has a position", p.ID)
	}

	ev := NewEvent(p.ID, gridv1.Layer_CHAIN_CONTROL, SeverityFromLabel(p.Severity), gridv1.EventStatus_ACTIVE, p.Headline)
	ev.Category = p.Category
	ev.Description = p.Description
	ev.AreaLabel = p.AreaLabel
	ev.Geometry = GeometryFromPoint(at[1], at[0]) // the low end, as the feature draws it
	ev.Effective = rfc3339Proto(p.Effective)
	ev.ObservedAt = tsProto(now)
	ev.Provenance = NewProvenance(ChainControlSourceID, "Caltrans", p.Source.Attribution, "")
	ev.Provenance.FetchedAt = tsProto(now)

	d := &gridv1.ChainControlDetail{
		Level: cc.Level, Highway: cc.Highway, Direction: cc.Direction, Inferred: cc.Inferred,
		From: chainPlace(cc.From), To: chainPlace(cc.To),
	}
	// Place ids are preset from EVERY sign: the point alone would attach the
	// stretch only where its low end is, while the map shows it in every
	// area it has a sign in (and the store unions these with its own match).
	places := map[string]bool{}
	for _, s := range cc.Signs {
		sign := &gridv1.ChainControlSign{
			Id: s.ID, Place: s.Place, Direction: s.Direction, Level: s.Level,
			Since: rfc3339Proto(s.Since), Source: s.Source, County: s.County, Postmile: s.Postmile,
		}
		if len(s.Coordinates) == 2 {
			lat, lng := s.Coordinates[1], s.Coordinates[0]
			sign.Location = &gridv1.LatLng{Lat: lat, Lng: lng}
			for _, a := range n.cfg.Hazards.Areas {
				if a.Bounds.Contains(lat, lng) {
					places["area:"+a.ID] = true
				}
			}
			if n.placesAt != nil {
				ids, err := n.placesAt(ctx, lat, lng)
				if err != nil {
					return nil, fmt.Errorf("chain control %s: places at %s: %w", p.ID, s.Place, err)
				}
				for _, id := range ids {
					places[id] = true
				}
			}
		}
		d.Signs = append(d.Signs, sign)
	}
	for id := range places {
		ev.PlaceIds = append(ev.PlaceIds, id)
	}
	sort.Strings(ev.PlaceIds)
	ev.Detail = &gridv1.Event_ChainControl{ChainControl: d}
	return ev, nil
}

func chainPlace(p *hazards.ChainPlace) *gridv1.ChainControlPlace {
	out := &gridv1.ChainControlPlace{Place: p.Place}
	if len(p.Coordinates) == 2 {
		out.Location = &gridv1.LatLng{Lat: p.Coordinates[1], Lng: p.Coordinates[0]}
	}
	return out
}

// rfc3339Proto parses an RFC 3339 time, nil when empty or unparseable.
func rfc3339Proto(s string) *timestamppb.Timestamp {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return tsProto(t.UTC())
}
