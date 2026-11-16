-- Grid store schema version 5: mesh_telemetry.via, the gateway node (public
-- key, lowercase hex) a monitor took the read through. '' when the monitor
-- states none, and on every row written before this column existed.
--
-- Additive, with a DEFAULT: the previous release's writer inserts without it.
ALTER TABLE mesh_telemetry ADD COLUMN via text NOT NULL DEFAULT '';
