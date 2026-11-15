-- 0015_watchlists_logs: service/process watchlists and forwarded logs (WL1).
--
-- Four changes:
--
-- (1) watchlist_items: service/process items to watch on desktops, either
--     org-scoped (scope='org', applies to every desktop in org_id) or
--     device-scoped (scope='device', one device_id). An item alerts by default
--     (alert=1); the server opens a 'service_down' alert for a watched item whose
--     latest reported state is not running. org_id is carried on both scopes so RS2
--     visibility filters by org for either; device_id is set for the device scope
--     only. name is the unit/service name or the process/executable name.
--
-- (2) devices gains the denormalised latest watchlist state: services_json (the
--     [{kind,name,running}] array RecordServices writes from facts.services, '[]'
--     until the first report) and services_checked_at (NULL until the first
--     report). Plain ADD COLUMN, like 0011's software columns.
--
-- (3) device_logs: forwarded journald / Event Log entries, one row per line, with
--     an index on (device_id, at) for the newest-first per-device viewer. org_id is
--     denormalised (NOT NULL, no FK, like notification_deliveries.rule_id) so a RS2
--     log read can scope by org without joining devices. Pruned after
--     MSP_LOG_RETENTION_DAYS by the hourly pruner, plus a per-device row cap
--     enforced in AppendLogs.
--
-- (4) alerts.type CHECK widens to add 'service_down'. SQLite cannot ALTER a CHECK
--     in place, so alerts is recreated with the enlarged constraint, preserving its
--     rows, the open-alert partial unique index (at most one open alert per
--     (device, type)) and the org/resolved index, and the AUTOINCREMENT sequence.
--     notification_deliveries references alerts(id) ON DELETE SET NULL (0005), and
--     the migration runner wraps each file in one foreign_keys=ON transaction, so a
--     plain DROP TABLE alerts would fire that action and null every delivery's
--     alert_id (losing the idempotency key and the delivery history). The deliveries
--     rows are backed up and the child dropped before alerts goes, then alerts is
--     rebuilt and notification_deliveries recreated with its current schema (0005
--     base + the 0013 'step' column) and indexes, and the rows restored against the
--     preserved alert ids (same FK-safe recreate as 0007/0008/0013).
--
-- All timestamps are TEXT RFC3339 UTC; booleans are INTEGER 0/1.

-- (1) Watchlist items.
CREATE TABLE watchlist_items (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id     INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,
    scope      TEXT NOT NULL CHECK (scope IN ('org', 'device')),
    device_id  INTEGER NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('service', 'process')),
    name       TEXT NOT NULL,
    alert      INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    created_by INTEGER NULL REFERENCES users(id)
);
CREATE INDEX watchlist_items_org ON watchlist_items(org_id);
CREATE INDEX watchlist_items_device ON watchlist_items(device_id);

-- (2) Latest watchlist state on the device (denormalised from facts.services).
ALTER TABLE devices ADD COLUMN services_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE devices ADD COLUMN services_checked_at TEXT NULL;

-- (3) Forwarded log entries.
CREATE TABLE device_logs (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    org_id    INTEGER NOT NULL,
    at        TEXT NOT NULL,
    source    TEXT NOT NULL CHECK (source IN ('journald', 'eventlog')),
    unit      TEXT NOT NULL DEFAULT '',
    level     TEXT NOT NULL DEFAULT '',
    message   TEXT NOT NULL
);
CREATE INDEX device_logs_device_at ON device_logs(device_id, at);

-- (4a) Back up notification_deliveries, then drop the child so dropping alerts
--      cannot fire its ON DELETE SET NULL action against it. CREATE TABLE AS copies
--      data only, no constraints.
CREATE TABLE notification_deliveries_backup AS SELECT * FROM notification_deliveries;
DROP TABLE notification_deliveries;

-- (4b) Rebuild alerts with 'service_down' in the type CHECK; preserve rows, the
--      open-alert partial unique index, the org/resolved index and the
--      AUTOINCREMENT sequence.
CREATE TABLE alerts_new (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id          INTEGER NOT NULL REFERENCES orgs(id),
    device_id       INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    type            TEXT NOT NULL CHECK (type IN ('offline', 'disk_full', 'memory_high', 'battery_low', 'service_down')),
    severity        TEXT NOT NULL CHECK (severity IN ('low', 'medium', 'high')),
    message         TEXT NOT NULL,
    opened_at       TEXT NOT NULL,
    resolved_at     TEXT NULL,
    acknowledged_by INTEGER NULL REFERENCES users(id),
    acknowledged_at TEXT NULL
);

INSERT INTO alerts_new (id, org_id, device_id, type, severity, message, opened_at, resolved_at, acknowledged_by, acknowledged_at)
    SELECT id, org_id, device_id, type, severity, message, opened_at, resolved_at, acknowledged_by, acknowledged_at
      FROM alerts;

DROP TABLE alerts;
ALTER TABLE alerts_new RENAME TO alerts;

CREATE UNIQUE INDEX alerts_open ON alerts(device_id, type) WHERE resolved_at IS NULL;
CREATE INDEX alerts_org_resolved ON alerts(org_id, resolved_at);

-- (4c) Recreate notification_deliveries with its current schema (0005 base + the
--      0013 'step' column) and indexes (alert ids were preserved above), restore the
--      rows, then drop the backup.
CREATE TABLE notification_deliveries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    alert_id    INTEGER NULL REFERENCES alerts(id) ON DELETE SET NULL,
    rule_id     INTEGER NULL,
    channel_id  INTEGER NULL,
    org_id      INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,
    event       TEXT NOT NULL CHECK (event IN ('opened', 'resolved', 'test')),
    status      TEXT NOT NULL CHECK (status IN ('pending', 'sent', 'failed', 'suppressed')),
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NULL,
    step        INTEGER NOT NULL DEFAULT 0
);

INSERT INTO notification_deliveries (id, alert_id, rule_id, channel_id, org_id, event, status, attempts, last_error, detail_json, created_at, updated_at, step)
    SELECT id, alert_id, rule_id, channel_id, org_id, event, status, attempts, last_error, detail_json, created_at, updated_at, step
      FROM notification_deliveries_backup;

DROP TABLE notification_deliveries_backup;
CREATE UNIQUE INDEX notification_deliveries_idem ON notification_deliveries(alert_id, rule_id, channel_id, event, step);
CREATE INDEX notification_deliveries_status ON notification_deliveries(status, updated_at);
CREATE INDEX notification_deliveries_org ON notification_deliveries(org_id);
