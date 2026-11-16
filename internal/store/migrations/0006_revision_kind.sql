-- Grid store schema version 6: two kinds of revision.
--
-- event_revisions.kind: 1 CHANGE (hashed content or status changed: every
-- revision before this column), 2 READING (a reading with content of its own
-- arrived and the state held; the content hash is unchanged).
-- events.readings: how many of the event's revisions are readings, so
-- revision - readings is its number of changes without counting rows.
--
-- Additive, with DEFAULTs: the previous release's writer inserts revisions
-- without kind and gets CHANGE, which is right, because it only writes changes.
ALTER TABLE event_revisions ADD COLUMN kind smallint NOT NULL DEFAULT 1;
ALTER TABLE events ADD COLUMN readings bigint NOT NULL DEFAULT 0;
