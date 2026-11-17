package gridapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dpup/prefab/plugins/etag"
	"google.golang.org/protobuf/types/known/timestamppb"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// timelineRevisionCap bounds the revisions a timeline reads: a busy mesh
// node or a long fire can have hundreds, each a full snapshot. The timeline
// reads changes (the store's default kind), so the cap is 500 changes.
const timelineRevisionCap = 500

// timelineReadingCap bounds the READING revisions a timeline lists.
const timelineReadingCap = 50

// GetEventTimeline returns one event's timeline (see buildTimeline).
func (g *GridServer) GetEventTimeline(ctx context.Context, req *gridv1.GetEventTimelineRequest) (*gridv1.EventTimeline, error) {
	stamp, ok, err := g.svc.Store.EventVersion(ctx, req.GetId())
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	if !ok {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	prefix, err := g.validatorPrefix(ctx)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	tag := fmt.Sprintf("%s:timeline:%s:%d.%d", prefix, req.GetId(), stamp.Revision, stamp.Refreshes)
	var activity map[string]store.MeshActivity
	if native, ok := strings.CutPrefix(req.GetId(), meshActivityPrefix); ok {
		if activity, err = g.svc.Store.MeshActivityFor(ctx, []string{native}); err != nil {
			logError(ctx, "gridapi: reading mesh activity", err)
			activity = nil
		}
		tag += ":" + meshActivityTag(activity[strings.ToLower(native)])
	}
	if err := etag.Guard(ctx, etag.Weak(tag)); err != nil {
		return nil, err
	}
	ev, err := g.svc.Store.GetEvent(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr("unknown event: %q", req.GetId())
	}
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	applyMeshActivity([]*gridv1.Event{ev}, activity)

	var revs []*gridv1.EventRevision
	token, truncated := "", false
	for {
		// Never read past the cap: the last page is cut to fit it.
		size := min(store.MaxPageSize, timelineRevisionCap-len(revs))
		page, err := g.svc.Store.EventHistoryPage(ctx, req.GetId(), size, token)
		if err != nil {
			return nil, internalErr(ctx, err)
		}
		revs = append(revs, page.Items...)
		token = page.Next(len(page.Items))
		if token == "" {
			break
		}
		if len(revs) >= timelineRevisionCap {
			truncated = true
			break
		}
	}
	var first *gridv1.EventRevision
	if truncated {
		// FIRST_RECORDED always comes from revision 1, even past the cap.
		if p, err := g.svc.Store.EventRevisionAt(ctx, req.GetId(), 1); err == nil {
			first = p
		}
	}
	t := buildTimeline(ev, revs, first, truncated)
	{
		// Always asked, never gated on the event's own readings count: the
		// previous release's writer drops that field from the blob, and the
		// rows are the truth. An indexed read that is empty for most events.
		// Not fatal: the changes above do not depend on the readings.
		page, err := g.svc.Store.EventHistoryPage(ctx, req.GetId(), timelineReadingCap, "", gridv1.RevisionKind_READING)
		if err != nil {
			logError(ctx, "gridapi: reading the event's readings", err)
		} else {
			addReadings(t, page.Items)
		}
	}
	if native, ok := strings.CutPrefix(req.GetId(), meshActivityPrefix); ok {
		// Not fatal: the rest of the timeline does not depend on the archive.
		if err := g.addReadPathChanges(ctx, t, strings.ToLower(native)); err != nil {
			logError(ctx, "gridapi: reading mesh read-path changes", err)
		}
	}
	return t, nil
}

// readPathChangeCap bounds the read-path moves a mesh timeline lists.
const readPathChangeCap = 50

// addReadPathChanges adds a mesh node's read-path moves (the gateway a
// monitor reads it through changed) from the telemetry archive. They are on
// the monitor's clock and are not revisions: the gateway of a read is not
// hashed (see MeshAdminTelemetry.via).
func (g *GridServer) addReadPathChanges(ctx context.Context, t *gridv1.EventTimeline, pubkey string) error {
	changes, err := g.svc.Store.MeshReadPathChanges(ctx, pubkey, readPathChangeCap)
	if err != nil || len(changes) == 0 {
		return err
	}
	names := map[string]string{"": ""}
	name := func(key string) string {
		if n, ok := names[key]; ok {
			return n
		}
		n := key
		if len(n) > 12 {
			n = n[:12]
		}
		if gw, err := g.svc.Store.GetEvent(ctx, meshActivityPrefix+key); err == nil && gw.GetMesh().GetName() != "" {
			n = gw.GetMesh().GetName()
		}
		names[key] = n
		return n
	}
	for _, c := range changes {
		detail := c.Reporter
		if c.From != "" {
			detail = "was " + name(c.From)
		}
		t.Entries = append(t.Entries, &gridv1.TimelineEntry{
			At: timestamppb.New(c.At), Kind: gridv1.TimelineKind_TIMELINE_READ_PATH, Clock: gridv1.TimelineClock_TIMELINE_CLOCK_SOURCE,
			Label: "Read through " + name(c.To), Detail: detail, Field: "readVia", Before: name(c.From), After: name(c.To),
		})
	}
	sortTimeline(t)
	return nil
}

// addReadings adds one TIMELINE_READING entry per READING revision (newest
// first, as the store returns them), on the source's clock: the time of the
// reading itself, which a snapshot differs from the revision before it by.
func addReadings(t *gridv1.EventTimeline, readings []*gridv1.EventRevision) {
	for i, r := range readings {
		// The revision before this one, when it is on the page, tells which
		// line the reading came from; without it, the latest call wins.
		var prev *gridv1.Event
		if i+1 < len(readings) && readings[i+1].GetRevision()+1 == r.GetRevision() {
			prev = readings[i+1].GetEvent()
		}
		at, label, detail := readingOf(r.GetEvent(), prev)
		if at == nil {
			continue
		}
		t.Entries = append(t.Entries, &gridv1.TimelineEntry{
			At: at, Kind: gridv1.TimelineKind_TIMELINE_READING, Clock: gridv1.TimelineClock_TIMELINE_CLOCK_SOURCE,
			Label: label, Detail: detail, Revision: r.GetRevision(),
		})
	}
	sortTimeline(t)
}

// readingOf describes the reading a READING snapshot holds: when it was
// taken, a label and what it said. For burn status that is the line whose
// call differs from prev's (prev nil: the line called last).
func readingOf(ev, prev *gridv1.Event) (at *timestamppb.Timestamp, label, detail string) {
	before := map[string]*gridv1.BurnObservation{}
	for _, l := range prev.GetBurnStatus().GetBurnLines() {
		before[l.GetId()] = l.GetObservation()
	}
	var line *gridv1.BurnLineReading
	for _, l := range ev.GetBurnStatus().GetBurnLines() {
		o := l.GetObservation()
		if o.GetObservedAt() == nil {
			continue
		}
		if prev != nil {
			if b := before[l.GetId()]; b.GetObservedAt() != nil && !o.GetObservedAt().AsTime().After(b.GetObservedAt().AsTime()) {
				continue
			}
		}
		if line == nil || o.GetObservedAt().AsTime().After(line.GetObservation().GetObservedAt().AsTime()) {
			line = l
		}
	}
	if line == nil {
		return nil, "", ""
	}
	label = "Checked: " + nonEmptyOr(burnDayText(line.GetBurnDay()), "no answer")
	return line.GetObservation().GetObservedAt(), label, nonEmptyOr(line.GetObservation().GetMessage(), line.GetName())
}

// burnDayText renders a burn-day answer for a timeline line.
func burnDayText(d gridv1.BurnDay) string {
	switch d {
	case gridv1.BurnDay_BURN_DAY_YES:
		return "burn day"
	case gridv1.BurnDay_BURN_DAY_NO:
		return "no-burn day"
	case gridv1.BurnDay_BURN_DAY_MARGINAL:
		return "marginal burn day"
	}
	return ""
}

func burnPermissionText(p gridv1.BurnPermission) string {
	switch p {
	case gridv1.BurnPermission_BURN_PERMISSION_ALLOWED:
		return "allowed"
	case gridv1.BurnPermission_BURN_PERMISSION_PROHIBITED:
		return "prohibited"
	}
	return "unknown"
}

func calfireText(c gridv1.CalfireBurnStatus) string {
	switch c {
	case gridv1.CalfireBurnStatus_CALFIRE_BURNING_SUSPENDED:
		return "burning suspended"
	case gridv1.CalfireBurnStatus_CALFIRE_PERMIT_REQUIRED:
		return "permit required"
	case gridv1.CalfireBurnStatus_CALFIRE_NO_PERMIT_REQUIRED:
		return "no permit required"
	}
	return ""
}

// buildTimeline derives the timeline from the current event and its
// revisions (any order). first, when non-nil, is revision 1 fetched
// separately because revs was truncated.
//
// The rule that makes it worth having: SOURCE entries only ever come from
// fields the upstream stated, and GRID entries only from revision ingest
// times. A client used to guess a report time from whichever timestamp was
// earliest, which mixed the two clocks.
func buildTimeline(ev *gridv1.Event, revs []*gridv1.EventRevision, first *gridv1.EventRevision, truncated bool) *gridv1.EventTimeline {
	t := &gridv1.EventTimeline{EventId: ev.GetId(), Truncated: truncated}
	src := func(at *timestamppb.Timestamp, kind gridv1.TimelineKind, label, detail string) {
		if at == nil || at.AsTime().IsZero() || at.AsTime().Unix() <= 0 {
			return
		}
		t.Entries = append(t.Entries, &gridv1.TimelineEntry{At: at, Kind: kind, Clock: gridv1.TimelineClock_TIMELINE_CLOCK_SOURCE, Label: label, Detail: detail})
	}
	who := ev.GetProvenance().GetSourceName()

	switch ev.GetLayer() {
	case gridv1.Layer_ROAD_INCIDENT:
		ri := ev.GetRoadIncident()
		if c := ri.GetClosure(); c != nil {
			src(c.GetPlannedStart(), gridv1.TimelineKind_TIMELINE_PLANNED_START, "Planned start", who)
			src(c.GetSetUpAt(), gridv1.TimelineKind_TIMELINE_SET_UP, "Set up on the road", who)
			src(c.GetPlannedEnd(), gridv1.TimelineKind_TIMELINE_PLANNED_END, "Planned end", who)
		} else {
			src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "Reported", who)
		}
		for _, l := range ri.GetLog() {
			src(l.GetAt(), gridv1.TimelineKind_TIMELINE_LOG, l.GetText(), "Log line "+strconv.Itoa(int(l.GetNumber())))
		}
	case gridv1.Layer_WILDFIRE:
		src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "Started", who)
	case gridv1.Layer_EARTHQUAKE:
		src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "Occurred", who)
	case gridv1.Layer_EVACUATION, gridv1.Layer_WEATHER_ALERT:
		src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "In effect", who)
		src(ev.GetExpires(), gridv1.TimelineKind_TIMELINE_ENDS, "Ends", who)
	case gridv1.Layer_POWER:
		p := ev.GetPower()
		if p.GetDeEnergizationStart() != nil || p.GetDeEnergizationEnd() != nil {
			src(p.GetDeEnergizationStart(), gridv1.TimelineKind_TIMELINE_PLANNED_START, "De-energization starts", who)
			src(p.GetDeEnergizationEnd(), gridv1.TimelineKind_TIMELINE_PLANNED_END, "De-energization ends", who)
			src(p.GetAllClear(), gridv1.TimelineKind_TIMELINE_ALL_CLEAR, "All clear", who)
		} else {
			src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "Outage began", who)
		}
		src(p.GetEstimatedRestoration(), gridv1.TimelineKind_TIMELINE_ESTIMATE, "Estimated restoration", who+" estimate; often revised")
	case gridv1.Layer_MESH:
		tel := ev.GetMesh().GetTelemetry()
		src(tel.GetLastAdvertAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Last advert", "")
		src(tel.GetLastHeardAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Last heard", "")
		src(tel.GetLastRelayedAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Last relayed", "")
		a := tel.GetAdmin()
		src(a.GetLastSuccessAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Monitor last reached it", a.GetReporterId())
		if a.GetLastAttemptAt() != nil && !a.GetLastAttemptAt().AsTime().Equal(a.GetLastSuccessAt().AsTime()) {
			src(a.GetLastAttemptAt(), gridv1.TimelineKind_TIMELINE_ACTIVITY, "Monitor last tried", a.GetReporterId())
		}
	default:
		src(ev.GetEffective(), gridv1.TimelineKind_TIMELINE_BEGINS, "In effect", who)
	}

	ordered := append([]*gridv1.EventRevision(nil), revs...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].GetRevision() < ordered[j].GetRevision() })
	if first == nil && len(ordered) > 0 && ordered[0].GetRevision() == 1 {
		first = ordered[0]
	}
	if first != nil {
		// Changes since the first: readings are revisions too, and are not
		// what this line counts.
		detail := "No changes since"
		if n := int(ev.GetRevision()) - int(ev.GetReadings()) - 1; n > 0 {
			detail = plural(n, "change") + " since"
		}
		t.Entries = append(t.Entries, &gridv1.TimelineEntry{
			At: first.GetIngestedAt(), Kind: gridv1.TimelineKind_TIMELINE_FIRST_RECORDED, Clock: gridv1.TimelineClock_TIMELINE_CLOCK_GRID,
			Label: "First recorded by the Grid", Revision: 1, Detail: detail,
		})
	}
	for i := 1; i < len(ordered); i++ {
		// revs holds changes only, so their numbers skip wherever a reading
		// sits between two changes. That is not a gap: readings leave the
		// hashed content alone, so the change before is still the honest
		// "before". (A truncated read drops the oldest revisions as a block,
		// never one from the middle.)
		prev, cur := ordered[i-1], ordered[i]
		for _, c := range revisionChanges(prev.GetEvent(), cur.GetEvent()) {
			c.At, c.Clock, c.Revision = cur.GetIngestedAt(), gridv1.TimelineClock_TIMELINE_CLOCK_GRID, cur.GetRevision()
			t.Entries = append(t.Entries, c)
		}
	}

	sortTimeline(t)
	return t
}

func sortTimeline(t *gridv1.EventTimeline) {
	sort.SliceStable(t.Entries, func(i, j int) bool {
		a, b := t.Entries[i].GetAt().AsTime(), t.Entries[j].GetAt().AsTime()
		if !a.Equal(b) {
			return a.Before(b)
		}
		return t.Entries[i].GetClock() < t.Entries[j].GetClock() // SOURCE before GRID
	})
}

// revisionChanges lists the tracked values that differ between consecutive
// revisions: status, severity, and per layer the figures a reader follows.
func revisionChanges(a, b *gridv1.Event) []*gridv1.TimelineEntry {
	var out []*gridv1.TimelineEntry
	diff := func(kind gridv1.TimelineKind, field, label, before, after string) {
		if before == after {
			return
		}
		out = append(out, &gridv1.TimelineEntry{Kind: kind, Field: field, Label: label, Before: before, After: after, Detail: "was " + nonEmptyOr(before, "not given")})
	}
	if a.GetStatus() != b.GetStatus() {
		diff(gridv1.TimelineKind_TIMELINE_STATUS, "status", statusLabel(a.GetStatus(), b.GetStatus()), enumWord(a.GetStatus().String()), enumWord(b.GetStatus().String()))
	}
	diff(gridv1.TimelineKind_TIMELINE_CHANGED, "severity", "Severity "+enumWord(b.GetSeverity().String()), enumWord(a.GetSeverity().String()), enumWord(b.GetSeverity().String()))
	switch b.GetLayer() {
	case gridv1.Layer_WILDFIRE:
		fa, fb := a.GetWildfire(), b.GetWildfire()
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "acres", "Size "+acresText(fb.GetAcres()), acresText(fa.GetAcres()), acresText(fb.GetAcres()))
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "containment", "Containment "+pct(fb.GetContainment()), pct(fa.GetContainment()), pct(fb.GetContainment()))
	case gridv1.Layer_EVACUATION:
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "level", "Level "+enumWord(b.GetEvacuation().GetLevel()), enumWord(a.GetEvacuation().GetLevel()), enumWord(b.GetEvacuation().GetLevel()))
	case gridv1.Layer_BURN_STATUS:
		ba, bb := a.GetBurnStatus(), b.GetBurnStatus()
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "permission", "Burning "+burnPermissionText(bb.GetPermission()), burnPermissionText(ba.GetPermission()), burnPermissionText(bb.GetPermission()))
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "burnDay", "County line: "+nonEmptyOr(burnDayText(bb.GetBurnDay()), "no answer"), burnDayText(ba.GetBurnDay()), burnDayText(bb.GetBurnDay()))
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "calfireStatus", "CAL FIRE: "+nonEmptyOr(calfireText(bb.GetCalfireStatus()), "unknown"), calfireText(ba.GetCalfireStatus()), calfireText(bb.GetCalfireStatus()))
	case gridv1.Layer_ROAD_SEGMENT:
		ra, rb := a.GetRoadSegment(), b.GetRoadSegment()
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "roadStatus", roadStatusLabel(ra.GetStatus(), rb.GetStatus()), enumWord(ra.GetStatus()), enumWord(rb.GetStatus()))
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "congestion", "Traffic "+strings.ToLower(nonEmptyOr(rb.GetCongestion(), "unknown")), enumWord(ra.GetCongestion()), enumWord(rb.GetCongestion()))
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "beyond", nonEmptyOr(beyondNames(rb), "Seasonal closure lifted"), beyondNames(ra), beyondNames(rb))
	case gridv1.Layer_CHAIN_CONTROL:
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "extent", "Now "+chainExtent(b.GetChainControl()), chainExtent(a.GetChainControl()), chainExtent(b.GetChainControl()))
	case gridv1.Layer_SOURCE_HEALTH:
		sa, sb := a.GetSourceHealth().GetStatus(), b.GetSourceHealth().GetStatus()
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "sourceStatus", sourceStatusLabel(sb), sourceStatusWord(sa), sourceStatusWord(sb))
	case gridv1.Layer_POWER:
		ca, cb := a.GetPower().GetCustomersAffected(), b.GetPower().GetCustomersAffected()
		diff(gridv1.TimelineKind_TIMELINE_CHANGED, "customersAffected", countText(cb)+" customers out", countText(ca), countText(cb))
	}
	return out
}

// roadStatusLabel words a monitored road's change of status.
func roadStatusLabel(from, to string) string {
	switch to {
	case "OPEN":
		if from == "" {
			return "Open"
		}
		return "Reopened"
	case "CLOSED":
		return "Closed"
	case "RESTRICTED":
		return "Restricted"
	case "MAINTENANCE":
		return "Roadwork"
	}
	return nonEmptyOr(enumWord(to), "Status unknown")
}

// beyondNames lists the seasonal closures in effect past a road's ends.
func beyondNames(d *gridv1.RoadSegmentDetail) string {
	var names []string
	for _, b := range d.GetBeyond() {
		names = append(names, b.GetName()+" in effect")
	}
	return strings.Join(names, "; ")
}

// chainExtent is a chain-control stretch's two ends: "A to B", or "A".
func chainExtent(d *gridv1.ChainControlDetail) string {
	from, to := d.GetFrom().GetPlace(), d.GetTo().GetPlace()
	if from == "" || strings.EqualFold(from, to) {
		return nonEmptyOr(from, to)
	}
	return from + " to " + to
}

func sourceStatusWord(s gridv1.SourceStatus) string {
	switch s {
	case gridv1.SourceStatus_OK:
		return "OK"
	case gridv1.SourceStatus_STALE:
		return "stale"
	case gridv1.SourceStatus_UNAVAILABLE:
		return "unavailable"
	}
	return ""
}

// sourceStatusLabel words a source's change of status as what happened.
func sourceStatusLabel(to gridv1.SourceStatus) string {
	switch to {
	case gridv1.SourceStatus_OK:
		return "Readable again"
	case gridv1.SourceStatus_STALE:
		return "Read failed; serving the last good data"
	case gridv1.SourceStatus_UNAVAILABLE:
		return "Unavailable"
	}
	return "Not read yet"
}

func statusLabel(from, to gridv1.EventStatus) string {
	switch to {
	case gridv1.EventStatus_RESOLVED:
		return "Resolved"
	case gridv1.EventStatus_EXPIRED:
		return "Expired"
	case gridv1.EventStatus_ACTIVE:
		if from == gridv1.EventStatus_SCHEDULED {
			return "Now in effect"
		}
		return "Active again"
	}
	return enumWord(to.String())
}

// enumWord renders an enum name for display: "RESOLVED" -> "Resolved",
// "BURN_DAY_YES" stays readable as "Burn day yes".
func enumWord(s string) string {
	if s == "" || strings.HasSuffix(s, "UNSPECIFIED") {
		return ""
	}
	w := strings.ToLower(strings.ReplaceAll(s, "_", " "))
	return strings.ToUpper(w[:1]) + w[1:]
}

func nonEmptyOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func acresText(v float64) string {
	if v <= 0 {
		return ""
	}
	return strconv.FormatFloat(roundTo(v, 1), 'f', -1, 64) + " acres"
}

func pct(v int32) string { return strconv.Itoa(int(v)) + "%" }

func countText(v int32) string {
	if v <= 0 {
		return ""
	}
	return strconv.Itoa(int(v))
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}

func roundTo(v float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}
