package gridapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/clients/cwwp2"
	"github.com/sierragridteam/sierra-data/internal/search"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Text search over events and revisions (the q parameter). The matching is
// the store's (internal/store/search.go, internal/search); this file is the
// request parsing, the error mapping and the response shaping.

// SearchNote is the standing caveat on every text-search result. A search
// answers "which stored reports say this", and an agent or a hurried reader
// will take an empty answer for "nothing happened", so the response says what
// it did and did not look at.
const SearchNote = "Matches are stored event reports whose text contains the query. " +
	"No match does not mean nothing happened: only events collected since `since` are stored, " +
	"a source that was down left a gap, and weather readings and road conditions are not events. " +
	"Reference only; verify with official sources."

// parseSearch reads the q and qTyping parameters. A blank q is no query (nil).
func parseSearch(q string, typing bool) (*search.Query, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	parsed, err := search.ParseQuery(q, typing)
	if err != nil {
		return nil, searchInvalid(err)
	}
	return &parsed, nil
}

func searchInvalid(err error) error {
	switch {
	case errors.Is(err, search.ErrQueryTooLong):
		return status.Errorf(codes.InvalidArgument, "q is too long: at most %d bytes and 16 terms", search.MaxQueryLen)
	default:
		return status.Error(codes.InvalidArgument,
			`q has nothing to search for: it needs at least one word that is not excluded and is not a common word like "the"`)
	}
}

// searchErr maps a store search failure. An index still being built is 503
// (retry), never an empty result; a query of only dropped words is the
// caller's (400); a query that ran out of time is 504.
func searchErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, store.ErrSearchNotReady):
		return status.Error(codes.Unavailable, "the search index is still being built; retry shortly (requests without q or counts are unaffected)")
	case errors.Is(err, search.ErrQueryEmpty), errors.Is(err, search.ErrQueryTooLong):
		return searchInvalid(err)
	case errors.Is(err, store.ErrSearchTimeout):
		return status.Error(codes.DeadlineExceeded, "the search took too long; narrow it with more words, a layer, a place or a time window")
	}
	return tokenOrInternal(ctx, err)
}

// searchInfo describes a query for the response: how it was read, and since
// when events have been stored (from the cached totals; left null if those
// cannot be read, which must not fail a search that succeeded).
func (g *GridServer) searchInfo(ctx context.Context, q *search.Query) *gridv1.SearchInfo {
	info := &gridv1.SearchInfo{Query: q.String(), Note: SearchNote}
	if st, err := g.GetStats(ctx, &gridv1.GetStatsRequest{}); err == nil {
		info.Since = st.GetSince()
	}
	return info
}

func searchMatch(s store.Snippet) *gridv1.SearchMatch {
	m := &gridv1.SearchMatch{Field: s.Field}
	for _, p := range s.Parts {
		m.Parts = append(m.Parts, &gridv1.SearchMatchPart{Text: p.Text, Hit: p.Hit})
	}
	return m
}

func eventCounts(counts []store.EventCount) []*gridv1.EventCount {
	out := make([]*gridv1.EventCount, 0, len(counts))
	for _, c := range counts {
		out = append(out, &gridv1.EventCount{Layer: c.Layer, Category: c.Category, Events: int32(c.Events)})
	}
	return out
}

// searchVersion is the validator part that moves when search rows are
// rebuilt without a revision (a backfill).
func (g *GridServer) searchVersion(ctx context.Context) (string, error) {
	v, err := g.svc.Store.SearchVersion(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(".s%d", v), nil
}

// eventListWithSearch finishes ListEvents for a request with q or counts.
func (g *GridServer) eventListWithSearch(ctx context.Context, eq store.EventQuery, req *gridv1.ListEventsRequest, geomFmt geometryFormat) (*gridv1.EventList, error) {
	res, err := g.svc.Store.SearchEvents(ctx, eq)
	if err != nil {
		return nil, searchErr(ctx, err)
	}
	events := res.Items
	stripEventsIO(events, req.GetEnhancementIo())
	g.svc.overlayMeshActivity(ctx, events)
	applyGeometryFormat(ctx, geomFmt, eventGeometries(events)...)
	n := fitToBudget(events, g.pageBudget)
	out := &gridv1.EventList{
		Events: events[:n], NextPageToken: res.Next(n),
		TotalMatches: wrapperspb.Int32(int32(res.Total)),
		Counts:       eventCounts(res.Counts),
	}
	if eq.Search != nil {
		out.Search = g.searchInfo(ctx, eq.Search)
		out.Matches = make(map[string]*gridv1.SearchMatch, n)
		for _, ev := range events[:n] {
			out.Matches[ev.GetId()] = searchMatch(res.Snippets[ev.GetId()])
		}
	}
	return out, nil
}

// historyWithSearch finishes ListHistory for a request with q.
func (g *GridServer) historyWithSearch(ctx context.Context, hq store.HistoryQuery, collapse bool, keepIO bool, geomFmt geometryFormat) (*gridv1.EventRevisionList, error) {
	out := &gridv1.EventRevisionList{Search: g.searchInfo(ctx, hq.Search)}
	if !collapse {
		res, err := g.svc.Store.SearchHistory(ctx, hq)
		if err != nil {
			return nil, searchErr(ctx, err)
		}
		for i, rev := range res.Items {
			rev.Match = searchMatch(res.Snippets[i])
		}
		page := g.revisionPage(ctx, res.Page, keepIO, geomFmt)
		out.Revisions, out.NextPageToken = page.GetRevisions(), page.GetNextPageToken()
		out.TotalMatches = wrapperspb.Int32(int32(res.Total))
		return out, nil
	}
	res, err := g.svc.Store.SearchHistoryByEvent(ctx, hq)
	if err != nil {
		return nil, searchErr(ctx, err)
	}
	rows := make([]*gridv1.EventHistoryMatch, len(res.Items))
	firsts := make([]*gridv1.EventRevision, len(res.Items))
	for i, m := range res.Items {
		m.First.Match = searchMatch(m.Snippet)
		firsts[i] = m.First
		rows[i] = &gridv1.EventHistoryMatch{
			EventId: m.EventID, First: m.First, LastRevision: m.LastRevision,
			LastObservedAt:    timestamppb.New(time.Unix(m.LastObservedAt, 0)),
			MatchingRevisions: int32(m.Matching),
		}
	}
	stripRevisionsIO(firsts, keepIO)
	applyGeometryFormat(ctx, geomFmt, revisionGeometries(firsts)...)
	// Field 4 of EventRevisionList; fitToBudget's field-1 tag is the same size.
	n := fitToBudget(rows, g.pageBudget)
	out.Collapsed, out.NextPageToken = rows[:n], res.Next(n)
	out.TotalMatches = wrapperspb.Int32(int32(res.Total))
	return out, nil
}

// --- the finder (GET /api/v1/search) ---

const (
	finderDefaultLimit = 5
	finderMaxLimit     = 20
	// finderOverfetch is how many ranked events are read per hit wanted, so
	// that look-alikes can be dropped and the list still fill.
	finderOverfetch = 4
)

// finderKinds is the fixed order hits and totals are returned in: where a
// thing is, then what is happening there, then what to look at or listen to.
var finderKinds = []gridv1.SearchKind{
	gridv1.SearchKind_PLACE, gridv1.SearchKind_ROAD, gridv1.SearchKind_EVENT,
	gridv1.SearchKind_MESH_NODE, gridv1.SearchKind_CAMERA, gridv1.SearchKind_SCANNER,
}

func parseSearchKinds(vals []string) (map[gridv1.SearchKind]bool, error) {
	want := map[gridv1.SearchKind]bool{}
	for _, raw := range vals {
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			n, ok := gridv1.SearchKind_value[strings.ToUpper(v)]
			if !ok || n == int32(gridv1.SearchKind_SEARCH_KIND_UNSPECIFIED) {
				return nil, fmt.Errorf("unknown kind: %q", v)
			}
			want[gridv1.SearchKind(n)] = true
		}
	}
	if len(want) == 0 {
		for _, k := range finderKinds {
			want[k] = true
		}
	}
	return want, nil
}

// scored is a directory hit with its match score, for ranking a small list.
type scored struct {
	hit   *gridv1.SearchHit
	score int
}

// topScored sorts best first (ties keep list order, which is the directory's
// own) and returns the first limit hits and the count of all.
func topScored(all []scored, limit int) ([]*gridv1.SearchHit, int) {
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	out := make([]*gridv1.SearchHit, 0, min(limit, len(all)))
	for i := 0; i < len(all) && i < limit; i++ {
		out = append(out, all[i].hit)
	}
	return out, len(all)
}

// Search finds things by name or text across kinds.
func (g *GridServer) Search(ctx context.Context, req *gridv1.SearchRequest) (*gridv1.SearchResults, error) {
	q, err := parseSearch(req.GetQ(), req.GetQTyping())
	if err != nil {
		return nil, err
	}
	if q == nil {
		return nil, invalidErr(errors.New("q is required"))
	}
	want, err := parseSearchKinds(req.GetKind())
	if err != nil {
		return nil, invalidErr(err)
	}
	limit := int(req.GetLimit())
	switch {
	case limit < 0:
		return nil, invalidErr(errors.New("limit must not be negative"))
	case limit == 0:
		limit = finderDefaultLimit
	case limit > finderMaxLimit:
		limit = finderMaxLimit
	}

	var place *gridv1.Place
	if p := req.GetPlace(); p != "" {
		place, err = g.svc.Store.GetPlace(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundErr("unknown place: %q", p)
		}
		if err != nil {
			return nil, internalErr(ctx, err)
		}
	}

	out := &gridv1.SearchResults{Search: g.searchInfo(ctx, q)}
	hits := map[gridv1.SearchKind][]*gridv1.SearchHit{}
	totals := map[gridv1.SearchKind]int{}

	if want[gridv1.SearchKind_PLACE] {
		places, err := g.svc.Store.ListPlaces(ctx, gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED, "")
		if err != nil {
			return nil, internalErr(ctx, err)
		}
		var all []scored
		for _, p := range places {
			kind := humanKind(p.GetKind().String())
			if s, ok := q.Match(p.GetName(), p.GetSlug(), kind); ok {
				all = append(all, scored{&gridv1.SearchHit{
					Kind: gridv1.SearchKind_PLACE, Id: p.GetSlug(), Title: p.GetName(), Detail: kind,
					ApiPath: "/api/v1/places/" + p.GetSlug() + "/summary",
				}, s})
			}
		}
		hits[gridv1.SearchKind_PLACE], totals[gridv1.SearchKind_PLACE] = topScored(all, limit)
	}

	if want[gridv1.SearchKind_ROAD] && g.svc.Cfg != nil {
		var all []scored
		for _, r := range g.svc.Cfg.Roads.MonitoredRoads {
			if s, ok := q.Match(r.Name, r.Section, r.ID, strings.Join(r.LocationKeywords, " ")); ok {
				all = append(all, scored{&gridv1.SearchHit{
					Kind: gridv1.SearchKind_ROAD, Id: r.ID, Title: r.Name, Detail: r.Section,
					ApiPath: "/api/v1/roads/" + r.ID + "/history",
				}, s})
			}
		}
		hits[gridv1.SearchKind_ROAD], totals[gridv1.SearchKind_ROAD] = topScored(all, limit)
	}

	for _, k := range []gridv1.SearchKind{gridv1.SearchKind_EVENT, gridv1.SearchKind_MESH_NODE} {
		if !want[k] {
			continue
		}
		mesh := []gridv1.Layer{gridv1.Layer_MESH}
		var layers, exclude []gridv1.Layer
		if k == gridv1.SearchKind_MESH_NODE {
			layers = mesh
		} else {
			exclude = mesh
		}
		// Lane closures arrive as one event per work window, so a road's
		// best hits can be the same headline and place five times over. Ask
		// for more than the limit and keep the best of each look-alike; the
		// total still counts every event.
		ranked, total, err := g.svc.Store.SearchTopEvents(ctx, *q, place.GetId(), layers, exclude, min(limit*finderOverfetch, finderMaxLimit*finderOverfetch))
		if err != nil {
			return nil, searchErr(ctx, err)
		}
		totals[k] = total
		seen := map[string]bool{}
		for _, r := range ranked {
			ev := r.Event
			if len(hits[k]) == limit {
				break
			}
			if same := ev.GetHeadline() + "\x1f" + ev.GetAreaLabel(); seen[same] {
				continue
			} else {
				seen[same] = true
			}
			hit := &gridv1.SearchHit{
				Kind: k, Id: ev.GetId(), Title: ev.GetHeadline(), Detail: ev.GetAreaLabel(),
				ApiPath: "/api/v1/events/" + ev.GetId(), Url: ev.GetCanonicalUrl(),
				Event: &gridv1.SearchHitEvent{
					Layer: ev.GetLayer(), Category: ev.GetCategory(), Status: ev.GetStatus(),
					Severity: ev.GetSeverity(), ObservedAt: ev.GetObservedAt(),
				},
			}
			if len(r.Snippet.Parts) > 0 {
				hit.Match = searchMatch(r.Snippet)
			}
			hits[k] = append(hits[k], hit)
		}
	}

	if want[gridv1.SearchKind_CAMERA] {
		// The same cameras ListCameras serves. A directory that is not
		// configured or has never loaded was not searched: say so, rather
		// than let "no cameras" read as "none match".
		status := "UNAVAILABLE"
		var cams []rankedCamera
		if g.svc.Cameras != nil {
			var list []cwwp2.Camera
			list, status, _, _ = g.svc.Cameras.Cameras()
			if status != "UNAVAILABLE" {
				if cams, err = g.svc.camerasNear(ctx, list, place); err != nil {
					return nil, internalErr(ctx, err)
				}
			}
		}
		if status == "UNAVAILABLE" {
			out.Unavailable = append(out.Unavailable, gridv1.SearchKind_CAMERA)
		} else {
			var all []scored
			for _, rc := range cams {
				c := rc.cam
				where := joinNonEmpty(", ", c.Location.Route, c.Location.NearbyPlace)
				if s, ok := q.Match(c.Name, where, c.Location.County, c.Description); ok {
					all = append(all, scored{&gridv1.SearchHit{
						Kind: gridv1.SearchKind_CAMERA, Id: c.ID, Title: c.Name, Detail: where,
						ApiPath: "/api/v1/cameras", Url: c.ImageURL,
					}, s})
				}
			}
			hits[gridv1.SearchKind_CAMERA], totals[gridv1.SearchKind_CAMERA] = topScored(all, limit)
		}
	}

	if want[gridv1.SearchKind_SCANNER] && g.svc.Cfg != nil {
		var all []scored
		for _, f := range g.svc.allScannerFeeds() {
			if s, ok := q.Match(f.ChannelLabel, f.Agency, "scanner"); ok {
				all = append(all, scored{&gridv1.SearchHit{
					Kind: gridv1.SearchKind_SCANNER, Id: f.FeedID, Title: f.ChannelLabel, Detail: f.Agency,
					ApiPath: "/api/v1/scanners", Url: "https://www.broadcastify.com/listen/feed/" + f.FeedID,
				}, s})
			}
		}
		hits[gridv1.SearchKind_SCANNER], totals[gridv1.SearchKind_SCANNER] = topScored(all, limit)
	}

	unavailable := map[gridv1.SearchKind]bool{}
	for _, k := range out.Unavailable {
		unavailable[k] = true
	}
	for _, k := range finderKinds {
		if !want[k] {
			continue
		}
		out.Hits = append(out.Hits, hits[k]...)
		if !unavailable[k] {
			out.Totals = append(out.Totals, &gridv1.SearchKindTotal{Kind: k, Matches: int32(totals[k])})
		}
	}
	return out, nil
}

// humanKind renders an enum name for a reader: "EVAC_ZONE" -> "Evac zone".
func humanKind(name string) string {
	s := strings.ToLower(strings.ReplaceAll(name, "_", " "))
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func joinNonEmpty(sep string, vals ...string) string {
	var out []string
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, sep)
}
