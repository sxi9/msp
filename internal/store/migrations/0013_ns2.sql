-- 0013_ns2: SMS channel kind and multi-step escalation policies (NS2).
--
-- Three changes, all additive to NS1 (0005):
--
-- (1) notify_channels.kind must accept 'sms' alongside 'email' and 'webhook'.
--     SQLite cannot ALTER a CHECK in place, so notify_channels is recreated with
--     the widened constraint, preserving its rows, indexes and AUTOINCREMENT
--     sequence. notify_rules references notify_channels(id) ON DELETE CASCADE, and
--     the migration runner wraps each file in one foreign_keys=ON transaction, so a
--     plain DROP TABLE notify_channels would cascade-delete every notify_rules row.
--     The notify_rules rows are backed up and the child dropped before
--     notify_channels goes, then notify_rules is recreated with its original schema
--     and the rows restored (same FK-safe recreate as 0007/0008/0012).
--
-- (2) escalation_policies + escalation_steps: an ordered list of steps per policy;
--     each step fires its channels after_minutes from an alert opening, while the
--     alert is still open and unacknowledged. notify_rules gains an optional
--     escalation_policy_id (plain ADD COLUMN, no CHECK change, no FK so a deleted
--     policy is detached in the store rather than cascading the rule away).
--
-- (3) notification_deliveries gains a 'step' column (0 = a non-escalation NS1
--     delivery; >=1 = an escalation step number) and the idempotency unique index
--     is widened from (alert_id, rule_id, event) to
--     (alert_id, rule_id, channel_id, event, step) so a given
--     (alert, rule, channel, step) delivers exactly once. For an NS1 row channel_id
--     is the rule's single channel and step is 0, so the old (alert, rule, event)
--     guarantee is preserved unchanged.
--
-- All timestamps are TEXT RFC3339 UTC; booleans are INTEGER 0/1. SMS channel
-- secrets (the provider auth token) are stored like NS1 channel secrets: in the
-- clear because they authenticate outbound sends, write-only over the API, never
-- logged or placed in a delivery's detail_json (docs/SECURITY.md).

-- (2a) Escalation policies and their ordered steps. A step's channel_ids_json is a
--      JSON array of notify_channels ids; it is not an FK (the delivery log and the
--      step both survive a channel deletion, matching notification_deliveries).
CREATE TABLE escalation_policies (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id     INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,  -- NULL = global policy
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    created_by INTEGER NULL REFERENCES users(id)
);
CREATE INDEX escalation_policies_org ON escalation_policies(org_id);

CREATE TABLE escalation_steps (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    policy_id        INTEGER NOT NULL REFERENCES escalation_policies(id) ON DELETE CASCADE,
    step_order       INTEGER NOT NULL,   -- 1-based; distinct from the NS1 delivery step 0
    after_minutes    INTEGER NOT NULL,   -- minutes from opened_at before this step is due
    channel_ids_json TEXT NOT NULL       -- JSON array of notify_channels ids
);
CREATE INDEX escalation_steps_policy ON escalation_steps(policy_id, step_order);

-- (1a) Back up notify_rules, then drop the child so dropping notify_channels
--      cannot cascade into it. CREATE TABLE AS copies data only, no constraints.
CREATE TABLE notify_rules_backup AS SELECT * FROM notify_rules;
DROP TABLE notify_rules;

-- (1b) Rebuild notify_channels with 'sms' in the kind CHECK; preserve rows, the
--      index and the AUTOINCREMENT sequence.
CREATE TABLE notify_channels_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL CHECK (kind IN ('email', 'webhook', 'sms')),
    name        TEXT NOT NULL,
    org_id      INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,
    config_json TEXT NOT NULL,
    secret      TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    created_by  INTEGER NULL REFERENCES users(id)
);

INSERT INTO notify_channels_new (id, kind, name, org_id, config_json, secret, enabled, created_at, created_by)
    SELECT id, kind, name, org_id, config_json, secret, enabled, created_at, created_by
      FROM notify_channels;

DROP TABLE notify_channels;
ALTER TABLE notify_channels_new RENAME TO notify_channels;
CREATE INDEX notify_channels_org ON notify_channels(org_id);

-- (1c) Recreate notify_rules with its original 0005 schema (channel ids were
--      preserved by the rebuild above), restore the rows, then drop the backup.
CREATE TABLE notify_rules (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    org_id       INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE,
    min_severity TEXT NOT NULL CHECK (min_severity IN ('low', 'medium', 'high')),
    alert_types  TEXT NOT NULL DEFAULT '',
    on_open      INTEGER NOT NULL DEFAULT 1,
    on_resolve   INTEGER NOT NULL DEFAULT 0,
    channel_id   INTEGER NOT NULL REFERENCES notify_channels(id) ON DELETE CASCADE,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    created_by   INTEGER NULL REFERENCES users(id)
);

INSERT INTO notify_rules (id, name, org_id, min_severity, alert_types, on_open, on_resolve, channel_id, enabled, created_at, created_by)
    SELECT id, name, org_id, min_severity, alert_types, on_open, on_resolve, channel_id, enabled, created_at, created_by
      FROM notify_rules_backup;

DROP TABLE notify_rules_backup;
CREATE INDEX notify_rules_org ON notify_rules(org_id);
CREATE INDEX notify_rules_channel ON notify_rules(channel_id);

-- (2b) A rule may point at an escalation policy (plain ADD COLUMN; no FK so
--      DeletePolicy detaches referencing rules in the store).
ALTER TABLE notify_rules ADD COLUMN escalation_policy_id INTEGER NULL;
CREATE INDEX notify_rules_policy ON notify_rules(escalation_policy_id);

-- (3) Widen the delivery idempotency key with the escalation step. step 0 is a
--     non-escalation NS1 delivery; >=1 is an escalation step number.
ALTER TABLE notification_deliveries ADD COLUMN step INTEGER NOT NULL DEFAULT 0;
DROP INDEX notification_deliveries_idem;
CREATE UNIQUE INDEX notification_deliveries_idem
    ON notification_deliveries(alert_id, rule_id, channel_id, event, step);
