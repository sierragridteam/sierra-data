package gridapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
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
