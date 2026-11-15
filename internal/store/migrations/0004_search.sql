-- Grid store schema version 4: full-text search over events and revisions.
--
-- Two derived tables, like event_lite and event_places: every row can be
-- rebuilt from the proto blob it describes (internal/search.Build), and
-- nothing here is the only home of a value. They are separate tables, not
-- columns on events/event_revisions, because events is updated constantly for
-- reasons that have nothing to do with text (last_seen_at, telemetry
-- refreshes, status and observed_at, the last two indexed): each such update
-- that is not heap-only would re-insert every word of the event into a GIN
-- index on the same table. Here a row is written only when its text changes.
--
--   * event_search: one row per event, its CURRENT text. category sits beside
--     the vector so matches can be counted by layer and category without
--     reading blobs. revision and refreshes are the event's stamp (its
--     revision, and its count of in-place rewrites) when the row was built,
--     and doc_version the builder's rules (search.DocVersion): the writer's
--     backfill rebuilds a row whose stamp or version differs from its event's
--     (an older release's writer, which does not know these tables, or a rule
--     change).
--   * event_revision_search: one row per stored revision, append-only like
--     event_revisions. observed_at is copied so a window over matches needs
--     no join.
--
-- tsv is built by the writer (weights A-D, English stemming, plus an
-- unstemmed copy of the title and summary for prefix matching while typing);
-- the expression lives in internal/store/search.go.
--
-- Backwards compatible: new tables only. A release that does not know them
-- neither reads nor writes them, and the backfill catches up what it wrote.

CREATE TABLE event_search (
  event_id    text PRIMARY KEY,
  category    text NOT NULL DEFAULT '',
  tsv         tsvector NOT NULL,
  text_hash   text NOT NULL,
  doc_version integer NOT NULL,
  revision    bigint NOT NULL,
  refreshes   bigint NOT NULL DEFAULT 0
);
CREATE INDEX idx_event_search_tsv ON event_search USING gin (tsv);

CREATE TABLE event_revision_search (
  event_id    text NOT NULL,
  revision    bigint NOT NULL,
  observed_at bigint NOT NULL,
  tsv         tsvector NOT NULL,
  doc_version integer NOT NULL,
  PRIMARY KEY (event_id, revision)
);
CREATE INDEX idx_event_revision_search_tsv ON event_revision_search USING gin (tsv);
