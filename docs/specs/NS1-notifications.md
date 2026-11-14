# NS1 — Alert notifications (phase 2)

Deliver alerts to the outside world: email and Slack/webhook, routed per client and severity,
suppressed during maintenance windows, with a retrying dispatcher and a delivery log. Everything
here is buildable and fully testable in the container: every channel send is SMTP or HTTP and is
tested against an in-process fake (a local SMTP listener; `httptest` for webhooks). No test performs
a real external send.

**In this slice (NS1):** email (SMTP), webhook (Slack-compatible and generic JSON), routing rules,
maintenance windows, the delivery queue + dispatcher + log, and a Send-test action.
**Deferred, names reserved (NS2):** SMS (another channel `kind` of the same shape, an HTTP provider),
and multi-step escalation policies. The schema and the `kind` enum leave room for both; this spec
says where they slot in. Do not build them now; note them in the docs as phase-2 follow-ons.

## Model (new tables, migration `000N_notifications.sql`, do not edit earlier migrations)

- `notify_channels(id, kind TEXT CHECK(kind IN ('email','webhook')), name TEXT, org_id INTEGER NULL
  REFERENCES orgs(id) ON DELETE CASCADE, config_json TEXT NOT NULL, secret TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1, created_at, created_by)`. `org_id NULL` = a global channel
  usable by any rule; a non-null `org_id` scopes the channel to one client. `config_json` holds the
  non-secret settings (email: host, port, from, to[], starttls bool, username; webhook: url, flavor
  'slack'|'generic'). `secret` holds the one secret (SMTP password or webhook signing secret); it is
  **write-only** over the API — never returned, shown as `"set":true|false`.
- `notify_rules(id, name, org_id INTEGER NULL, min_severity TEXT CHECK IN ('low','medium','high'),
  alert_types TEXT NOT NULL DEFAULT '', on_open INTEGER NOT NULL DEFAULT 1, on_resolve INTEGER NOT
  NULL DEFAULT 0, channel_id INTEGER NOT NULL REFERENCES notify_channels(id) ON DELETE CASCADE,
  enabled INTEGER NOT NULL DEFAULT 1, created_at, created_by)`. `alert_types` is a comma list
  (empty = all types). `org_id NULL` = matches every client; non-null = only that client.
- `maintenance_windows(id, org_id INTEGER NULL, starts_at TEXT NOT NULL, ends_at TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '', created_at, created_by)`. `org_id NULL` = all clients. One-shot
  (absolute times); recurring windows are out of scope.
- `notification_deliveries(id, alert_id INTEGER NULL REFERENCES alerts(id) ON DELETE SET NULL,
  rule_id INTEGER NULL, channel_id INTEGER NULL, org_id INTEGER NULL, event TEXT CHECK IN
  ('opened','resolved','test'), status TEXT CHECK IN ('pending','sent','failed','suppressed'),
  attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', detail_json TEXT NOT NULL
  DEFAULT '', created_at, updated_at)`. One row per (alert, rule, event); a UNIQUE index on
  `(alert_id, rule_id, event)` makes enqueue idempotent so the dispatcher never double-sends.

All five tables, channels, rules, windows and the delivery log are **org-scoped for staff visibility**
exactly like RS2: a scoped operator/viewer sees and manages only channels/rules/windows/deliveries
whose `org_id` is in their visible set (a global `org_id NULL` row is visible only to all-orgs users).
Admins see all. Reuse the RS2 visible-org helpers.

## Delivery

- **Enqueue**: when an alert is newly opened (`s.open` where `OpenAlert` returns true) or resolved
  (`s.resolve` that actually closed one), find enabled rules that match: org (rule.org_id NULL or ==
  alert org), severity (alert severity >= rule.min_severity), alert type (empty or listed), and the
  event (on_open / on_resolve). For each match, upsert a `notification_deliveries` row
  (status `pending`) keyed by (alert, rule, event). If the alert's org is inside an active
  maintenance window at enqueue time, create the row with status `suppressed` and do not send.
- **Dispatcher**: a background goroutine (started in `Run`, stopped on shutdown) that every few
  seconds claims `pending` rows, renders the message for the channel kind, sends it, and marks `sent`
  or increments `attempts` with `last_error`. Retry with backoff; give up at `maxAttempts` (e.g. 5)
  leaving status `failed`. Concurrency-safe claim (status pending -> an in-flight marker) so two
  dispatcher ticks never grab the same row. The dispatcher must be injectable with a fake clock and a
  fake sender for tests, and must use the server's `now()`.
- **Renderers**:
  - email: a plain-text message (subject `"[MSP] <severity> <type> on <hostname> (<org>)"`, body with
    the device, org, message, time, and a dashboard link if `BaseURL` is set) sent with `net/smtp`
    (STARTTLS when configured). The SMTP send is behind an interface; the test uses a local fake SMTP
    server, asserts the envelope and that the body names the device and alert.
  - webhook generic: `POST` the alert as JSON (`{alert_id, org, device, type, severity, message,
    event, at}`); if a secret is set, add an HMAC-SHA256 signature header `X-MSP-Signature`.
  - webhook slack: `POST {"text": "<same summary as the email subject + message>"}` (Slack incoming
    webhook shape). Tested against `httptest`, asserting the JSON body.
- Never log channel secrets or full webhook URLs with embedded tokens; never put a secret in the
  delivery `detail_json`. Redact.

## API (all staff endpoints under `/api/v1`, session auth, CSRF on writes, RS2 org-scoped)

- Channels: `GET /notify/channels` (list, org-scoped; secret shown as `"secret_set":bool`),
  `POST /notify/channels` (admin, or operator for an org they can see), `PUT /notify/channels/{id}`
  (same; omitting `secret` leaves it unchanged, sending `""` is rejected, sending a value replaces
  it), `DELETE /notify/channels/{id}`, `POST /notify/channels/{id}/test` (enqueue a `test` delivery
  immediately and return the created delivery id; the dispatcher sends it). Audit
  `notify.channel_created/updated/deleted/tested`.
- Rules: `GET/POST/PUT/DELETE /notify/rules` (same auth + org scope). Audit `notify.rule_*`.
- Maintenance windows: `GET/POST/DELETE /notify/windows`. Audit `notify.window_*`.
- Deliveries: `GET /notify/deliveries?status=&org_id=&limit=` (read, org-scoped) — the delivery log.
- Role: `viewer` is read-only on all of the above; `operator` may manage rows for orgs they can see;
  `admin` may manage global (`org_id NULL`) rows. Creating a global row requires all-orgs.

## Dashboard

- A **Notifications** section (new nav item for admin/operator; hidden for viewer-only? viewer sees
  the read-only log). Three panels: Channels (add/edit/delete, a "Send test" button that shows the
  resulting delivery status), Rules (add/edit/delete; severity, types, org, on-open/on-resolve,
  channel), Maintenance windows (add/delete; org, start, end, reason, with an "active now" badge).
- A **Delivery log** view: time, org, device/alert, channel, event, status chip
  (pending/sent/failed/suppressed), attempts, last error. Filter by status and org.
- Secrets are write-only in the UI: the field shows "set / not set" and a "replace" affordance.
- Pure helpers (severity compare, rule-match, window-active, redaction) in `web/src/lib/notify.ts`
  with a `node --test`. Keep `?mock=1` working with a couple of channels, a rule, a window and a few
  deliveries. Phone width intact (tables scroll in their own box).

## Docs

`docs/PROTOCOL.md` (all the endpoints + shapes + the write-only secret rule), `docs/SECURITY.md`
(secret storage: these are credentials that must be used so they are stored, not hashed; never
returned, never logged; HMAC signing; maintenance-window suppression is not a security control),
`docs/OPS.md` (configuring SMTP/Slack, the dispatcher, retry/backoff, that outbound SMTP/HTTP must be
allowed from the server), `docs/GUIDE.md` (a "Notifications" section: set up a channel, route alerts,
mute during maintenance, read the log), `docs/ARCHITECTURE.md`, `docs/FEATURES.md`, `docs/STATUS.md`,
`docs/ROADMAP.md` (tick email+Slack+routing+maintenance windows+delivery log; leave SMS and
escalation policies unticked as NS2), `docs/CHECKLIST.md`.

## Proof (what the build must show here)

`CGO_ENABLED=0 go build ./...`, `go vet ./...`, `CGO_ENABLED=0 go test ./... -count=1`,
`GOOS=windows GOARCH=amd64 go build ./...`, `make web`, `scripts/e2e.sh`.

A new `internal/e2e` test (or thorough server tests) that, with a fake clock, a local fake SMTP
server and an `httptest` webhook:
- an alert opening on a device in org A, with an email rule and a Slack rule matching, produces two
  `sent` deliveries; the fake SMTP received a message naming the device and alert; the webhook got
  the Slack JSON;
- a rule with `min_severity=high` does not fire on a `low` alert;
- an active maintenance window for org A makes the next opened alert's deliveries `suppressed`, none
  sent; outside the window they send;
- a failing sender increments attempts and lands `failed` after maxAttempts, and enqueue is
  idempotent (one row per alert+rule+event even if the evaluator runs twice);
- RS2 scoping: an operator scoped to A cannot GET/PUT/DELETE a channel/rule/window/delivery in org B
  (404), and cannot create a global row.

Verify stage also screenshots the Notifications settings (a channel + a rule + a maintenance window)
and the delivery log with a `sent` and a `suppressed` row.
