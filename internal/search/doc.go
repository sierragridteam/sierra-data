// Package search builds the text the store indexes for an event and parses
// what a person or an agent types into a query. Both are pure: the store turns
// a Doc into a tsvector and a Query into a tsquery (internal/store/search.go).
//
// The proto blob stays canonical. A Doc is derived from an event and can be
// rebuilt from it at any time, so the search tables hold nothing that exists
// nowhere else. Bump DocVersion when Build's output changes for the same
// event: stored rows record the version they were built with and the writer
// rebuilds the stale ones.
package search

import (
	"sort"
	"strings"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
)

// DocVersion identifies Build's rules. Raise it with any change that makes
// Build return different text for an unchanged event.
const DocVersion = 1

// Doc is an event's searchable text in four weights, most important first.
// The store ranks a match in A above one in B, and so on.
type Doc struct {
	// A is what names the event: headline, where it is, and identifiers.
	A string
	// B describes it in plain words: the summary, category and typed facts.
	B string
	// C is the source's own text: the verbatim description and incident log.
	C string
	// D is who reported it.
	D string
	// Category is the event's category, kept beside the text so the store
	// can count matches by layer and category without reading blobs.
	Category string
	// Title and Prose are what a match snippet is cut from: the headline and
	// location, and the sentences a person would read (summary, description,
	// log, instructions). They leave out the identifiers and typed facts that
	// A and B also index, which read as noise in an excerpt.
	Title, Prose string
}

// TitleSeparator joins the headline and the location in Doc.Title, so a
// snippet of both reads as two things and not as one run-on line.
const TitleSeparator = " · "

// Indexed returns the four weights as the store indexes them: each with one
// canonical token per road it mentions appended. Roads are written many ways
// ("SR 108", "Sr108", "State Route 108"), and ParseQuery rewrites a road the
// same way, so any spelling finds every spelling.
func (d Doc) Indexed() (a, b, c, dd string) {
	withRoutes := func(s string) string { return joinNonEmpty(" ", s, strings.Join(RouteTokens(s), " ")) }
	return withRoutes(d.A), withRoutes(d.B), withRoutes(d.C), d.D
}

// Text is everything the index depends on, for hashing: the store skips a
// write whose text did not change (a mesh telemetry refresh, a place
// re-attachment).
func (d Doc) Text() string {
	a, b, c, dd := d.Indexed()
	return strings.Join([]string{a, b, c, dd, d.Category}, "\x1f")
}

// Build returns the search document for ev.
//
// Left out on purpose: the AI enhancement's raw request and response (they
// repeat the summary at length), geometry, and mesh telemetry (it changes on
// every packet without a revision, and nobody searches for an RSSI).
func Build(ev *gridv1.Event) Doc {
	var a, b, c, d parts
	a.add(ev.GetHeadline(), ev.GetAreaLabel())
	b.add(ev.GetSummary(), ev.GetCategory())
	c.add(ev.GetDescription())
	d.add(ev.GetProvenance().GetSourceName(), ev.GetProvenance().GetAttribution())

	if w := ev.GetWildfire(); w != nil {
		b.add(w.GetCounty(), w.GetCause())
	}
	if e := ev.GetEvacuation(); e != nil {
		a.add(e.GetZoneId())
		b.add(e.GetLevel(), e.GetEventType(), e.GetCounty())
	}
	if w := ev.GetWeatherAlert(); w != nil {
		b.add(w.GetAreaDesc())
		c.add(w.GetInstruction())
	}
	if r := ev.GetRoadIncident(); r != nil {
		b.add(r.GetImpact())
		// The model's structured facts ("traffic collision", "all lanes"), in
		// key order so the text is the same on every build.
		keys := make([]string, 0, len(r.GetMetadata()))
		for k := range r.GetMetadata() {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.add(r.GetMetadata()[k])
		}
		a.add(r.GetLogNumber())
		for _, l := range r.GetLog() {
			c.add(l.GetText())
		}
		if cl := r.GetClosure(); cl != nil {
			a.add(cl.GetRoute(), cl.GetBeginLocation(), cl.GetEndLocation(), cl.GetClosureId())
			b.add(cl.GetClosureType(), cl.GetWorkType(), cl.GetDirection(), cl.GetFacility(), cl.GetClosureDuration())
		}
	}
	if m := ev.GetMesh(); m != nil {
		a.add(m.GetName(), m.GetPublicKey())
		b.add(m.GetNodeType())
	}
	if p := ev.GetPower(); p != nil {
		a.add(p.GetOutageId(), p.GetEventName(), p.GetEventId())
		b.add(p.GetCause(), p.GetCrewStatus(), p.GetStage(), p.GetTimePeriod())
	}
	if bs := ev.GetBurnStatus(); bs != nil {
		b.add(bs.GetCalfireArea())
		for _, l := range bs.GetBurnLines() {
			b.add(l.GetName())
		}
	}

	var prose parts
	prose.add(ev.GetSummary(), ev.GetDescription())
	for _, l := range ev.GetRoadIncident().GetLog() {
		prose.add(l.GetText())
	}
	prose.add(ev.GetWeatherAlert().GetInstruction())
	return Doc{
		A: a.String(), B: b.String(), C: c.String(), D: d.String(), Category: ev.GetCategory(),
		Title: joinNonEmpty(TitleSeparator, strings.TrimSpace(ev.GetHeadline()), strings.TrimSpace(ev.GetAreaLabel())),
		Prose: prose.String(),
	}
}

type parts []string

func (p *parts) add(vals ...string) {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			*p = append(*p, v)
		}
	}
}

func (p parts) String() string { return strings.Join(p, "\n") }

func joinNonEmpty(sep string, vals ...string) string {
	out := vals[:0:0]
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, sep)
}
