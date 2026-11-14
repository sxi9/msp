-- 0011_device_software: per-device installed-software inventory (SI1).
--
-- The desktop agent sends its installed software (protocol.Facts.Software) in
-- the periodic facts, beside apps/rustdesk_id; RecordSoftware
-- (internal/store/software.go) stores the full list as JSON and the two
-- denormalised columns the fleet and device pages read. software_json defaults
-- to '[]' so a device that has never reported software round-trips as an empty
-- array; software_count defaults to 0 and software_checked_at is NULL until the
-- first report. Android keeps reporting apps (in facts_json); a device reports
-- one or the other, never both.

ALTER TABLE devices ADD COLUMN software_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE devices ADD COLUMN software_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN software_checked_at TEXT NULL;
