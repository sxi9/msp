-- 0005_notifications: alert notifications (NS1).
--
-- Email (SMTP) and webhook (Slack-compatible + generic JSON) delivery of alerts,
-- routed per client org and severity, suppressed during maintenance windows, with
-- a retrying dispatcher and a delivery log. Every table is org-scoped for staff
-- visibility exactly like RS2 (org_id NULL = global, visible only to all-orgs
-- staff; a non-null org_id scopes the row to one client). Phase-2 follow-ons
-- (NS2: SMS as another channel kind, multi-step escalation) reuse this shape and
-- add no table here.
--
-- All timestamps are TEXT RFC3339 UTC; booleans are INTEGER 0/1. Channel secrets
-- (SMTP password or webhook signing secret) are stored in the clear because they
-- must be used to authenticate/sign outbound sends -- they are credentials in
-- use, not password hashes (docs/SECURITY.md). They are write-only over the API
-- and never logged or placed in a delivery's detail_json.

CREATE TABLE notify_channels (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL CHECK (kind IN ('email', 'webhook')),
    name        TEXT NOT NULL,
    org_id      INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,   -- NULL = global channel
    config_json TEXT NOT NULL,                                        -- non-secret settings
    secret      TEXT NOT NULL DEFAULT '',                             -- SMTP password / webhook signing secret
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    created_by  INTEGER NULL REFERENCES users(id)
);
CREATE INDEX notify_channels_org ON notify_channels(org_id);

CREATE TABLE notify_rules (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    org_id       INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,  -- NULL = matches every client
    min_severity TEXT NOT NULL CHECK (min_severity IN ('low', 'medium', 'high')),
    alert_types  TEXT NOT NULL DEFAULT '',                            -- comma list; empty = all types
    on_open      INTEGER NOT NULL DEFAULT 1,
    on_resolve   INTEGER NOT NULL DEFAULT 0,
    channel_id   INTEGER NOT NULL REFERENCES notify_channels(id) ON DELETE CASCADE,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    created_by   INTEGER NULL REFERENCES users(id)
);
CREATE INDEX notify_rules_org ON notify_rules(org_id);
CREATE INDEX notify_rules_channel ON notify_rules(channel_id);

CREATE TABLE maintenance_windows (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id     INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,    -- NULL = all clients
    starts_at  TEXT NOT NULL,
    ends_at    TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    created_by INTEGER NULL REFERENCES users(id)
);
CREATE INDEX maintenance_windows_org ON maintenance_windows(org_id);

CREATE TABLE notification_deliveries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    alert_id    INTEGER NULL REFERENCES alerts(id) ON DELETE SET NULL,
    rule_id     INTEGER NULL,                                         -- kept after a rule is deleted (log survives)
    channel_id  INTEGER NULL,                                         -- kept after a channel is deleted (log survives)
    org_id      INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,
    event       TEXT NOT NULL CHECK (event IN ('opened', 'resolved', 'test')),
    status      TEXT NOT NULL CHECK (status IN ('pending', 'sent', 'failed', 'suppressed')),
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NULL                                             -- NULL until first claimed; doubles as the retry lease
);

-- One row per (alert, rule, event): makes EnqueueDelivery idempotent so the
-- evaluator running twice never double-sends. A 'test' delivery has alert_id NULL;
-- NULLs are distinct in a UNIQUE index, so every test enqueue is its own row.
CREATE UNIQUE INDEX notification_deliveries_idem ON notification_deliveries(alert_id, rule_id, event);
CREATE INDEX notification_deliveries_status ON notification_deliveries(status, updated_at);
CREATE INDEX notification_deliveries_org ON notification_deliveries(org_id);
