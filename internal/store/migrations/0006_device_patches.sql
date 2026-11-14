-- 0006_device_patches: per-device patch status (PM1).
--
-- The agent sends an optional patch report (protocol.PatchReport) beside facts
-- on the sync and enroll request; RecordPatches (internal/store/patches.go)
-- stores the full report as JSON and derives the denormalised columns the
-- fleet and device pages read. patches_json defaults to '{}' so a device that
-- has never reported patches round-trips as an empty object; the counts default
-- to 0 and reboot_required to 0 (false). patches_checked_at is NULL until the
-- first report.

ALTER TABLE devices ADD COLUMN patches_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE devices ADD COLUMN patches_checked_at TEXT NULL;
ALTER TABLE devices ADD COLUMN pending_patches INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN security_patches INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN reboot_required INTEGER NOT NULL DEFAULT 0;
