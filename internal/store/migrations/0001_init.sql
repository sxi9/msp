-- 0001_init: the complete phase-1 schema (docs/PLAN.md DATA_MODEL + AMENDMENT A1).
-- All timestamps are TEXT RFC3339 UTC ("2026-11-13T10:00:01Z"). Secrets are stored
-- as sha256 hex only. Booleans are INTEGER 0/1.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE orgs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    email         TEXT NOT NULL UNIQUE,            -- lowercased
    password_hash TEXT NOT NULL,                   -- bcrypt cost 12
    role          TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'viewer')),
    created_at    TEXT NOT NULL,
    disabled_at   TEXT NULL
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    ip         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_expires ON sessions(expires_at);

CREATE TABLE enrollment_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id       INTEGER NOT NULL REFERENCES orgs(id),
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    max_uses     INTEGER NOT NULL,
    uses         INTEGER NOT NULL DEFAULT 0,
    expires_at   TEXT NOT NULL,
    created_by   INTEGER NULL REFERENCES users(id),
    created_at   TEXT NOT NULL,
    revoked_at   TEXT NULL
);
CREATE INDEX enrollment_tokens_org ON enrollment_tokens(org_id);

CREATE TABLE devices (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id            INTEGER NOT NULL REFERENCES orgs(id),
    device_uid        TEXT NOT NULL,
    hostname          TEXT NOT NULL,
    platform          TEXT NOT NULL CHECK (platform IN ('linux', 'windows', 'android')),
    arch              TEXT NOT NULL DEFAULT '',
    os_version        TEXT NOT NULL DEFAULT '',
    agent_version     TEXT NOT NULL DEFAULT '',
    token_hash        TEXT NULL UNIQUE,            -- NULL = revoked
    interval_s        INTEGER NOT NULL DEFAULT 30,
    enrolled_at       TEXT NOT NULL,
    last_seen_at      TEXT NULL,
    last_sample_at    TEXT NULL,
    last_ip           TEXT NOT NULL DEFAULT '',
    facts_json        TEXT NOT NULL DEFAULT '{}',
    last_metrics_json TEXT NOT NULL DEFAULT '{}',
    cpu_pct           REAL NULL,
    mem_used_pct      REAL NULL,
    disk_used_pct     REAL NULL,                   -- max over disks
    battery_pct       INTEGER NULL,
    battery_charging  INTEGER NULL,
    uptime_s          INTEGER NULL,
    rustdesk_id       TEXT NULL,                   -- reserved for phase 3 remote desktop (D4)
    UNIQUE (org_id, device_uid)
);
CREATE INDEX devices_org ON devices(org_id);
CREATE INDEX devices_last_seen ON devices(last_seen_at);

-- One row per device per >= 60 s; pruned after MSP_RETENTION_DAYS.
CREATE TABLE heartbeats (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id     INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    at            TEXT NOT NULL,
    cpu_pct       REAL NULL,
    mem_used_pct  REAL NULL,
    disk_used_pct REAL NULL,
    battery_pct   INTEGER NULL,
    uptime_s      INTEGER NULL
);
CREATE INDEX heartbeats_device_at ON heartbeats(device_id, at);

CREATE TABLE commands (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    org_id      INTEGER NOT NULL REFERENCES orgs(id),
    created_by  INTEGER NOT NULL REFERENCES users(id),
    kind        TEXT NOT NULL CHECK (kind IN ('exec', 'script', 'lock', 'ping', 'message')),
    argv_json   TEXT NULL,
    shell       TEXT NULL,
    body        TEXT NULL,
    body_sha256 TEXT NULL,
    text        TEXT NULL,                         -- kind message only (A1)
    timeout_s   INTEGER NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('queued', 'sent', 'done', 'failed', 'timeout', 'lost', 'cancelled')),
    created_at  TEXT NOT NULL,
    sent_at     TEXT NULL,
    started_at  TEXT NULL,
    finished_at TEXT NULL,
    exit_code   INTEGER NULL,
    stdout      TEXT NOT NULL DEFAULT '',
    stderr      TEXT NOT NULL DEFAULT '',
    truncated   INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX commands_device_status ON commands(device_id, status);
CREATE INDEX commands_status_sent ON commands(status, sent_at);
CREATE INDEX commands_created ON commands(created_at);

CREATE TABLE alerts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id          INTEGER NOT NULL REFERENCES orgs(id),
    device_id       INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    type            TEXT NOT NULL CHECK (type IN ('offline', 'disk_full', 'memory_high', 'battery_low')),
    severity        TEXT NOT NULL CHECK (severity IN ('low', 'medium', 'high')),
    message         TEXT NOT NULL,
    opened_at       TEXT NOT NULL,
    resolved_at     TEXT NULL,
    acknowledged_by INTEGER NULL REFERENCES users(id),
    acknowledged_at TEXT NULL
);
-- At most one open alert per (device, type).
CREATE UNIQUE INDEX alerts_open ON alerts(device_id, type) WHERE resolved_at IS NULL;
CREATE INDEX alerts_org_resolved ON alerts(org_id, resolved_at);

-- Append-only: the Store exposes only Audit and ListAudit.
CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          TEXT NOT NULL,
    actor_type  TEXT NOT NULL CHECK (actor_type IN ('user', 'device', 'system')),
    actor_id    INTEGER NULL,
    org_id      INTEGER NULL,
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id   INTEGER NULL,
    ip          TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_at ON audit_log(at);
CREATE INDEX audit_org_at ON audit_log(org_id, at);
