-- 0014_automations: script library, bulk-run jobs, and scheduled/triggered
-- automations (AUT1).
--
-- Four changes, all additive:
--
-- (1) scripts: the DB-backed, editable command library (distinct from the static
--     presets.json console presets). org_id NULL = a global script usable for any
--     client (RS2 visibility, exactly like a global file/channel). kind is 'exec'
--     (argv_json) or 'script' (shell + body); platform gates which devices it may
--     run on ('any' = linux and windows). timeout_s defaults to 300.
--
-- (2) jobs: one fan-out of a script across a device set. A job groups the
--     per-device commands created in that fan-out; progress is derived at read
--     time by counting its commands by status (JobProgress), so jobs stores only
--     total (the number of commands actually created). origin records how the job
--     was started (manual bulk run, a schedule, or an alert trigger). script_id is
--     ON DELETE SET NULL so a job survives its script's deletion; automation_id is
--     a plain column (no FK) so a job survives its automation's deletion and the
--     cooldown/last-run lookups keep working.
--
-- (3) commands gains job_id (plain ADD COLUMN, no CHECK change): a job's fan-out
--     tags each command it creates, and JobProgress counts them by status.
--     ON DELETE SET NULL keeps the command (and its audit trail) when the job is
--     removed. The agent wire COMMAND is unchanged; the dashboard COMMAND JSON
--     gains job_id (omitempty).
--
-- (4) automations: a saved rule that produces jobs. trigger 'schedule' uses
--     schedule_json ({every_minutes} or {cron}) and the server scheduler
--     (DueAutomations); trigger 'alert' uses alert_json ({alert_type?,
--     min_severity?}) and fires from the alert-open hook on the alerting device.
--     target_json is a device filter ({org_id?, status?, platform?, hostname_q?})
--     resolved to the automation's visible devices at run time. script_id is
--     ON DELETE CASCADE (an automation without a script is meaningless).
--
-- All timestamps are TEXT RFC3339 UTC; booleans are INTEGER 0/1. "trigger" is a
-- SQL keyword, so it is always quoted.

CREATE TABLE scripts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id      INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,  -- NULL = global
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    platform    TEXT NOT NULL CHECK (platform IN ('linux', 'windows', 'any')),
    kind        TEXT NOT NULL CHECK (kind IN ('exec', 'script')),
    argv_json   TEXT NULL,   -- kind exec: JSON array of argv
    shell       TEXT NULL,   -- kind script: interpreter ("" = platform default)
    body        TEXT NULL,   -- kind script: the script body
    timeout_s   INTEGER NOT NULL DEFAULT 300,
    created_at  TEXT NOT NULL,
    created_by  INTEGER NULL REFERENCES users(id)
);
CREATE INDEX scripts_org ON scripts(org_id);

CREATE TABLE jobs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id        INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,  -- NULL = spans visible orgs (global script)
    name          TEXT NOT NULL,
    origin        TEXT NOT NULL CHECK (origin IN ('manual', 'schedule', 'trigger')),
    script_id     INTEGER NULL REFERENCES scripts(id) ON DELETE SET NULL,
    created_by    INTEGER NULL REFERENCES users(id),
    automation_id INTEGER NULL,   -- no FK: a job outlives its automation (cooldown/last-run lookups)
    created_at    TEXT NOT NULL,
    total         INTEGER NOT NULL DEFAULT 0   -- commands actually created in the fan-out
);
CREATE INDEX jobs_org ON jobs(org_id);
CREATE INDEX jobs_automation ON jobs(automation_id);

CREATE TABLE automations (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id        INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,  -- NULL = global (all-orgs only)
    name          TEXT NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    "trigger"     TEXT NOT NULL CHECK ("trigger" IN ('schedule', 'alert')),
    script_id     INTEGER NOT NULL REFERENCES scripts(id) ON DELETE CASCADE,
    target_json   TEXT NOT NULL,          -- {org_id?, status?, platform?, hostname_q?}
    schedule_json TEXT NULL,              -- trigger schedule: {every_minutes} or {cron}
    alert_json    TEXT NULL,              -- trigger alert: {alert_type?, min_severity?}
    last_run_at   TEXT NULL,
    next_run_at   TEXT NULL,              -- trigger schedule: the next due time
    created_at    TEXT NOT NULL,
    created_by    INTEGER NULL REFERENCES users(id)
);
CREATE INDEX automations_org ON automations(org_id);
-- Covers the scheduler's DueAutomations scan (enabled, trigger, next_run_at).
CREATE INDEX automations_due ON automations(enabled, "trigger", next_run_at);

-- (3) commands.job_id: plain ADD COLUMN with a NULL default (SQLite permits a
--     REFERENCES clause on ADD COLUMN only when the default is NULL).
ALTER TABLE commands ADD COLUMN job_id INTEGER NULL REFERENCES jobs(id) ON DELETE SET NULL;
CREATE INDEX commands_job ON commands(job_id);
