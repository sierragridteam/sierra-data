# Recorded State: Changes and Readings — Technical Design

Status: **Proposed** · Owner: The Grid (S.I.E.R.R.A) · Last updated: 2026-11-16

## 1. Summary

The Grid records history for one thing: a change in an event's hashed content
or status. Everything else is either overwritten in place or never stored. Now
that the store is PostgreSQL, the constraints that shaped that are gone, and
the gaps are the main obstacle to longitudinal analysis.

This design keeps the event as the single model and extends it in two ways:

1. **Two kinds of revision.** A *change* is what a revision is today: the
   hashed content or the status changed. A *reading* is new: we looked again, what
   we got carries content of its own, and the state held. Both are full
   snapshots in `event_revisions`. History defaults to changes, so existing
   consumers see what they see now.
2. **More state modeled as long-lived events.** Source health and chain control
   become event layers, each revised when its state changes, the way burn
   status already works. Weather readings are a candidate for the same
   treatment.

No new history table is added. A generic `samples` table was considered and
rejected (section 11).

## 2. Background: what is recorded and what is lost

Production holds 12,318 events and 75,721 revisions since 2026-07-07
(`/api/v1/stats`, 2026-11-16). Storage is not a constraint at this scale.

| Data | Where it lives today | History kept |
| --- | --- | --- |
| Hashed event content, status | `events`, `event_revisions` | Every change, forever |
| Burn-line call (message, transcript, confidence, call time) | Hash-excluded block in the event blob; `burn_readings` keyed by `line_id` | **None.** Each call replaces the last in both places |
| Source health | `sources` row, rewritten by `RecordAttempt` | **None** |
| Chain control | In-memory cache, projected live as the `chain_control` map layer | **None** |
| Weather readings | In-memory cache, served by `/api/v1/conditions` | **None** |
| Message signs | In-memory cache | **None** |
| Road segment status and travel time | `road_samples` (since 2026-11-10) | 400 days |
| Mesh monitor reads | `mesh_telemetry` | 1 year |
| Mesh receptions | `mesh_observations`, rolled up per link per day | 48 hours raw; links 2 years; nothing per node |

Two consequences matter most:

- **A quiet stretch of history is ambiguous.** Nothing records when a source
  was down, so "no incidents" and "feed unavailable" look the same afterwards.
  The service refuses to present absence as safety in the live API; the
  archive cannot make that distinction at all.
- **Burn status cannot show its readings.** The revision history is described
  as the product of that layer, but a call that repeats yesterday's answer
  leaves no trace. "Still a burn day" and "the dialer did not run" are
  indistinguishable.

### How the store got here

The rule in `internal/store/AGENTS.md` is "every hash-excluded field needs a
way back into the blob". It exists so a field left out of the content hash is
still persisted: the hash-equal path (`refreshEventPlaces`) rewrites the blob
in place, bumps `events.refreshes` and the layer's `refresh:<LAYER>` version so
ETags move, and the poller lists the event in `PollResult.ForceWrite` so the
scheduler does not skip it.

That rule keeps the *current* value correct. It says nothing about history,
and it is fragile: burn status shipped without it and served its first
reading from 2026-10-09 to 10-12 while daily calls landed.

## 3. Goals and non-goals

**Goals**

- Record every burn-line call, every source outage and every chain-control
  requirement, starting as soon as possible. None of this can be backfilled.
- One access path for history: `event_revisions`, through the existing history
  endpoints.
- Keep "what changed" readable. Readings must not bury changes.
- Additive schema and API. No existing response changes shape by default.

**Non-goals**

- Changing `ContentHash`. The hash still defines a change.
- Moving `mesh_telemetry`, `road_samples` or the mesh reception tables into the
  event model (section 8).
- Extending mesh reception retention or adding a per-node rollup. Worth doing,
  separate change.
- Making stored blobs queryable in SQL (a `jsonb` projection) or converting
  timestamps to `timestamptz`. Separate changes.

## 4. The model

> Anything with a state someone would ask "since when?" about is an event. It
> lives as long as the thing it describes and is revised when that state
> changes.

Burn status is the pattern: one event per county, permanently ACTIVE, revised
when the answer changes. The call to the burn line is not a separate event; it
is a reading of the long-lived one.

A revision is one of two kinds:

| Kind | Written when | Content hash | Shown by default |
| --- | --- | --- | --- |
| `CHANGE` | hashed content or status changed | changes | yes |
| `READING` | a reading with its own content arrived and the state held | unchanged | no |

### Which readings are recorded

**A reading is recorded as a revision when it carries content of its own:**
a transcript, a confidence score, a measured value.

A poll that merely confirms what was already stored does not. Wildfire is
polled every five minutes; a revision per poll would be noise. That case is
covered differently: the source was healthy, and section 6.1 records when it
was not. Together the two answer "did we look, and what did we see" for every
moment in the archive.

Applied to today's hash-excluded fields:

| Field | Treatment | Why |
| --- | --- | --- |
| Burn-line observation | **`READING` revision** per call | The call has content: message, transcript, confidence |
| CAL FIRE observed-at stamp | Refresh path, as today | A stamp, no content. Outages show as source health |
| Mesh telemetry | Refresh path, as today | Moves every packet; `mesh_telemetry` is its archive |
| AI enhancement, summary | Refresh path, as today | Derived text. Revisit separately |
| `place_ids` | Refresh path, as today | Derived at ingest, not upstream content |

### What a reading revision is not

It is not a sample table. Each one is a full snapshot of the event, so "what
was the state when this reading was taken" needs no join: it is the row.

### Vocabulary

This design adds states to a model whose words already carry several meanings.
The names below are part of the design, not a follow-up.

**Event stays, and gets an accurate definition.** `Event` is the public noun
(`/api/v1/events`, `grid.v1.Event`, the tables, the MCP tools), and its use
matches the domain: in CAP alerts and USGS feeds an event is a long-lived thing
that is updated. What is wrong is the glossary, which calls it "a single
occurrence" and then lists a mesh node. `CONCEPTS.md` now reads: a thing the
Grid tracks, with a state and a history, that lasts as long as what it
describes.

**`CHANGE` and `READING`, not transition and observation.**

| Rejected | Why |
| --- | --- |
| `OBSERVATION` | It would be a fourth meaning, after `observedAt` on every event, the `mesh_observations` reception table and the `observation` block on a burn line |
| `TRANSITION` | The store's `TransitionEvents` means a status change only; this kind also covers content changes |

Burn already names its message `BurnLineReading`. The burn line's
`observation` field keeps its wire name.

**Ambient means "not a hazard", nothing more.** An ambient layer is left out of
the summary's hazard rollups. It does not mean permanent: mesh nodes are
ambient and expire, burn status is ambient and never ends.

**`observedAt` is when this revision's content was seen.** For a `CHANGE`,
when the change was observed; for a `READING`, when the reading was taken. It
is never "last polled" or "last heard", which is how it was misread for mesh
nodes (`docs/solutions/logic-errors/mesh-observedat-read-as-last-heard.md`).
Today the store notes carry an exception: `observed_at` moves only with a
revision, "except burn status, where it follows each new reading". Once a
reading is a revision, the exception is the rule and can be deleted.

**`burn-line` and `burnline` are one hyphen apart.** The first is the CI
dialer's reporter row, the second the freshness of the burn-day data. Section
6.1 puts both in history side by side. Rename the reporter to `burn-dialer`
before that ships. A reporter row is health only and owns no events, so the
change is the config entry and a reseeded `sources` row; the feed id stays. To
verify first: that nothing beyond `prefab.yaml` and a staged reading's
`reporter_id` refers to the reporter id.

Left alone: "layer" (event layer and map layer) and "status" (lifecycle,
source, road). Both are overloaded, and consumers depend on both.

## 5. Revision kind

### 5.1 Schema (migration 0006)

```sql
ALTER TABLE event_revisions ADD COLUMN kind smallint NOT NULL DEFAULT 1; -- 1 CHANGE, 2 READING
ALTER TABLE events ADD COLUMN readings bigint NOT NULL DEFAULT 0;        -- count of kind-2 revisions

-- History defaults to changes; keep both walks on an index.
CREATE INDEX idx_revisions_changes
  ON event_revisions (observed_at DESC, event_id ASC, revision DESC) WHERE kind = 1;
CREATE INDEX idx_revisions_event_changes
  ON event_revisions (event_id, revision DESC) WHERE kind = 1;
```

Both columns have defaults, as the migration rules require. The previous
release's writer inserts revisions without `kind` and gets `CHANGE`, which
is correct: it only ever writes changes. It never touches `readings`,
and never needs to.

`events.revision` keeps counting every revision, so
`changes = revision - readings` with no second counter to drift.

### 5.2 Write path

`UpsertEvent` gains a third outcome on the hash-equal branch:

1. Hash differs or the event is new: a `CHANGE` revision, exactly as today.
2. Hash equal, and the poller listed the event in `PollResult.Readings`, and
   its reading clock advanced past the stored one: an `READING`
   revision. The stored blob takes the incoming hash-excluded block, `revision`
   and `readings` increment, `content_hash` stays, and a snapshot row is
   inserted with `kind = 2`.
3. Otherwise: the existing refresh path.

The reading clock is named by the poller per layer. For burn status it is
each line's call time: a tick that re-offers the same call writes nothing. The
burn poller ticks hourly and a line is called once a day, so this guard is
what keeps it to one revision per call rather than twenty-four.

A reading revision is an ordinary revision to everything downstream:

- It bumps the `revisions` version, so list and detail ETags move. It does not
  need `refreshes` or `refresh:<LAYER>`.
- `event_lite` and `event_places` are written as for any revision.
- `event_revision_search`: the row is written like any other. A reading's
  content is outside the search text, so it repeats the previous revision's
  vector; skipping it would make the search backfill treat it as missing.
- `TransitionEvents` is unaffected.

The single-writer rule covers the new read-modify-write step like the others.

### 5.3 Compatibility during a deploy

- **Old writer, new schema:** writes changes only; defaults are right.
- **Old reader, new rows:** an older build serving history shows reading
  revisions unfiltered, for the length of a blue/green. They are valid
  snapshots, so this is cosmetic. To avoid it entirely, gate reading
  writing behind `grid.readings.enabled` and turn it on after the deploy.
- **Rollback:** the older build treats reading revisions as revisions. No
  data is harmed. The burn poller's refresh path still exists in that build.

## 6. New layers

### 6.1 `SOURCE_HEALTH`

One long-lived event per row in `sources`, id `source:<id>`, permanently
ACTIVE, always INFO. Its detail carries the source's status (`OK`, `STALE`,
`UNAVAILABLE`), when that status began, and the last error.

- **Change:** the status changed. The error text is excluded from the
  hash, since it varies per attempt, but the snapshot keeps the error that was
  current at the change.
- **No reading revisions.** A successful poll has no content of its own.
- **Writer:** the scheduler, where it already calls `RecordAttempt`, when the
  resulting status differs from the stored one.
- **`sources` stays** as the registry and the current health. The event is its
  history.
- **Places:** none. The place summary already carries a `sources[]` sidecar.

This is what lets an analysis qualify a gap: join an event's `source_id` to
`source:<id>` history and a quiet stretch reads as quiet or as blind.

`events.source_id` is a foreign key, so these events need a source row of
their own for the Grid itself.

### 6.2 `CHAIN_CONTROL`

A chain requirement on a stretch of highway starts, holds and ends. It is an
event by any definition; it is a condition layer today only because the
hazards layers predate the store.

The identity already exists. `chainStretchID` produces `cc:hwy4:r2`, stable
while a stretch grows, shrinks or a sign flaps, and deliberately new when the
level changes (`cc:hwy4:r3` is a different requirement). So:

- A stretch appearing is revision 1, ACTIVE.
- A stretch missing from a successful, complete poll is RESOLVED
  (`disappearance: resolve`), under the existing fail-loud sweep rules.
- A level change resolves one event and opens another. The sequence of events
  on a highway is the storm's history.

Details:

- **Source id:** its own (`caltrans-cc`), not `caltrans`. The sweep runs per
  source id, and chain controls must not be swept by the lane-closure poll or
  the reverse.
- **Not ambient.** Unlike burn status this is a real hazard. But the place
  summary's roads domain already reads the live condition layer, so these
  events stay out of the summary rollups until that domain is cut over to
  them, or they would be counted twice.
- **The map layer keeps its live projection** in the first step. Recording
  comes first; switching `chain_control.geojson` to project from the store is
  a follow-up, after the two have been compared through a storm.
- **Known limit:** a stretch that splits, merges or gains a neighbour before
  it on the road renumbers (`cc:hwy88:r2:2`), which reads as one event
  resolving and another opening.

### 6.3 Weather readings (to decide)

One long-lived event per configured location, detail carrying current
conditions, with every refresh a reading revision. A reading has content
of its own, so it fits the rule.

The cost is volume: 7 locations at a 15-minute refresh is about 670 revisions
a day, roughly 245,000 a year. The whole store writes about 210,000 a year
today (75,721 in 132 days). That is affordable in PostgreSQL and the partial indexes keep it out of
the default history walks, but it doubles revision growth for a series of
numbers.

Recommendation: do it, for one access path, after sections 5 and 6.1 are in
production and their cost is measured. It needs a scheduler tick that reads
the weather service, as the road sampler does.

### 6.4 Ambient layers as a declared property

`MESH` and `BURN_STATUS` are each excluded from the summary's `totalActive`,
`severityCounts`, `topEvents` and `mode` by a hand-written layer check
(`internal/gridapi/summary.go`). `SOURCE_HEALTH` and weather would add two
more.

Replace the checks with one declared property: a single function that says
whether a layer is ambient (not a hazard; see Vocabulary in section 4), read
by the summary and anything else that needs it. `CHAIN_CONTROL` is not
ambient; its temporary summary exclusion is a separate, explicitly temporary
rule.

## 7. API changes

All additive. Each needs a `CHANGELOG.md` entry, OpenAPI descriptions and
examples, and fixtures for the field-presence contract tests.

- `EventRevision.kind` (`CHANGE` | `READING`).
- `kinds` filter on `/api/v1/events/{id}/history` and `/api/v1/history`,
  default `CHANGE`. With the default, responses are what they are today.
- `/api/v1/stats`: `changes` and `readings` beside `revisions`, overall
  and per layer.
- Layers `SOURCE_HEALTH` and `CHAIN_CONTROL` (and their detail messages).
- `/api/v1/events/{id}/timeline`: reading entries.

Two behaviours to call out in the changelog, neither a shape change:

- **Revision numbers in default history can skip.** Readings consume
  revision numbers, so a changes-only page may read 3, 7, 12.
- **`Event.revision` on a burn event now moves with each call.** A consumer
  using it as "number of changes" should use the new counts. burnday.ersn.net
  reads `observation.observedAt` and is unaffected.

## 8. What stays outside the event model

| Table | Why it stays |
| --- | --- |
| `mesh_observations`, `mesh_link_rollup`, `mesh_meta` | A firehose, and a link is a pair of nodes rather than a state of one subject |
| `mesh_telemetry` | A dense numeric series with a typed archive that works. A full event snapshot per read adds nothing |
| `road_samples` | Same. A road segment's status could become a long-lived event later; travel time would stay here |
| `mesh_activity` | A current-value overlay that moves every few seconds |
| `burn_readings` | The inbox between the push handler and the single writer. No longer the only home of a reading once section 5 ships |

The line: a table outside the event model is justified by volume or by a
relationship the event cannot hold, not by which source the data came from.

## 9. What this removes

Once burn status writes reading revisions, burn no longer needs:

- the burn branch of `refreshEventPlaces`;
- `ForceWrite` in the burn poller;
- the `refresh:BURN_STATUS` version.

They stay for one release, for rollback, then go. The refresh path itself
remains for mesh telemetry, enhancement and place ids.

The store rule changes from "every hash-excluded field needs a way back into
the blob" to: **every hash-excluded field is either a reading, with its
history in `event_revisions`, or is named in the store notes with the reason
its history is not kept.**

## 10. Rollout

1. **Revision kind and burn readings.** Migration 0006, the store's third
   outcome, the burn poller's reading clock, the API fields and filter,
   stats. Ships dark behind `grid.readings.enabled`, turned on after the
   deploy.
2. **`SOURCE_HEALTH`**, with the ambient-layer property, after the
   `burn-line` reporter is renamed `burn-dialer`.
3. **`CHAIN_CONTROL`**, recording only. Compare against the live map layer
   through the next storm.
4. **Cut over** the `chain_control` map layer and the summary's roads domain
   to the stored events.
5. **Weather**, if section 6.3 is accepted.
6. **Contract:** remove the burn refresh branch, a release after step 1.

Steps 1 to 3 are the ones that stop data loss. Every day before them is a day
of calls, outages and chain requirements that cannot be recovered.

Tests to add with step 1: a reading revision leaves `content_hash`
unchanged; a re-offered reading writes nothing; default history returns
changes only and stays on the partial indexes (`plans_test.go`); the
previous release's writer path (no `kind`) reads back as `CHANGE`.

## 11. Alternatives considered

- **A generic `samples` table** (`subject`, `series`, `at`, `data jsonb`). One
  shape for every reading, but a second history model beside revisions, joined
  to events only by time and a naming convention, with its own write path,
  retention and endpoint. Three of the four gaps turned out to be state with
  changes, which the event model already handles.
- **A typed table per source** (a burn log, a source status log, chain-control
  samples). The existing pattern, and the reason there are six source-specific
  tables with private keys and no common read path.
- **Hash the reading.** Every call becomes an ordinary revision. This is
  what the hash exclusion was added to prevent: a revision a day buries the few
  real changes the layer exists to record. The kind column gives the same
  record without that cost.
- **A reading revision per poll, for every event.** Complete, and almost
  entirely redundant: wildfire alone would write hundreds a day per event that
  say nothing. Source health carries the same information in a handful of rows.
- **An event per source outage** rather than one long-lived event per source.
  Lists naturally, but it is the opposite of the burn pattern, and "the
  history of this source" becomes a query instead of one event's revisions.

## 12. Open questions

1. **Reporter rows.** `sources` includes one health row per push reporter
   (`alanpi`, `relay`, `burn-line`), and mesh reporter health is held in
   memory, so it reads "no report received yet" after every deploy. Should
   reporters get `SOURCE_HEALTH` events, and should that reset be fixed first
   so it does not write a false outage on every deploy?
2. **CAL FIRE re-checks.** Section 4 leaves the CAL FIRE stamp on the refresh
   path. If the twice-yearly suspension change needs a record of each scrape
   that confirmed it, it becomes a reading clock too.
3. **Message signs.** Not covered. The text is mostly campaign boilerplate; a
   sign's message could be a long-lived event revised when the text changes.
4. **Exposing ambient in the API.** Section 6.4 is internal. A consumer filter
   for "hazards only" would need it on the wire.
5. **Geometry in snapshots.** Every revision repeats the event's geometry.
   Burn and source health have none worth counting; a content-addressed
   geometry table would make the question moot and retire `event_lite`.
