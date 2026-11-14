# NS2 — SMS notifications and multi-step escalation (phase 2)

Completes the notifications roadmap line by adding an **SMS** channel kind and **multi-step escalation
policies** on top of NS1 (email + webhook + routing + maintenance windows + delivery log + dispatcher).
Reuses the NS1 dispatcher, delivery log and org-scoping (RS2). Fully tested in-container: SMS goes to an
`httptest` provider, escalation is driven by the fake clock. No real SMS is ever sent.

## SMS channel

- Extend `notify_channels.kind` to allow `sms`. Migration `0013_ns2.sql` recreates `notify_channels`
  FK-safely (the `notify_rules.channel_id` FK references it) to widen the CHECK to
  `('email','webhook','sms')`, preserving all rows — same FK-safe recreate pattern as 0007/0008.
- SMS config (`config_json`): `{"provider":"twilio"|"generic","from":"+1555...","to":["+1555..."],
  "account_sid":"AC...","url":"https://..."}`. `secret` = the provider auth token (write-only, never
  returned/logged, as in NS1). `twilio`: POST to the Twilio Messages API shape with basic auth
  (account_sid + token); `generic`: POST a JSON `{to, text}` to `url` with the token as a bearer header.
  Validate: provider in the enum, at least one `to`, E.164-ish numbers, url required for generic.
- Dispatcher: a new renderer in the NS1 dispatcher for `kind sms` that sends a **short** message
  (`"[MSP] <severity> <type> on <hostname> (<org>)"`, trimmed to ~300 chars) via the provider, behind
  the existing injectable Sender interface (add `SendSMS`); the production Sender does the real HTTP POST,
  tests inject a fake. Redact the token and the provider URL's credentials from logs and `detail_json`.
- Dashboard: the channel editor gains the SMS kind (provider, from, to list, token write-only) and
  Send-test works for it. `?mock=1` gets an SMS channel.

## Multi-step escalation policies

- New tables: `escalation_policies(id, org_id INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE, name,
  created_at, created_by)` and `escalation_steps(id, policy_id INTEGER NOT NULL REFERENCES
  escalation_policies(id) ON DELETE CASCADE, step_order INTEGER NOT NULL, after_minutes INTEGER NOT NULL,
  channel_ids_json TEXT NOT NULL)`. A policy is an ordered list of steps; each step fires its channels
  `after_minutes` from the alert opening, but only while the alert is still **open and unacknowledged**.
- `notify_rules` gains `escalation_policy_id INTEGER NULL` (plain `ALTER TABLE ADD COLUMN`, no CHECK
  change). A rule may point at a policy; when it does and the rule matches a newly opened alert, the
  policy drives delivery over time **in addition to** the rule's own `on_open` channel (document the
  interaction: the immediate `on_open` channel still fires at open; the policy adds the timed steps).
- Scheduling: when an alert opens and matches a rule with a policy, record the policy + the alert's open
  time (reuse the alert row's `opened_at`; no new per-alert table needed — compute due steps from
  `opened_at` + `after_minutes`). The NS1 dispatcher tick (or the alert evaluator tick) does, for each
  **open, unacknowledged** alert whose matched rule has a policy: for each step whose
  `opened_at + after_minutes <= now` and which has **not already been delivered** (idempotent via the
  existing `notification_deliveries` unique key extended with the step, event `opened`), enqueue a
  delivery per channel in the step. Stop entirely once the alert is acknowledged or resolved (a resolve
  still honours `on_resolve` rules as in NS1). Respect maintenance windows (suppress like NS1).
  Escalation deliveries are org-scoped and appear in the delivery log with their step.
- Idempotency: extend the delivery uniqueness so a given (alert, rule, channel, step) delivers once;
  a restart or repeated tick never double-sends. Keep NS1's existing (alert, rule, event) behaviour for
  non-escalation rules.
- Store: `escalation` CRUD (create policy + steps, list, delete), `PoliciesForRule`, and the due-step
  query; all org-scoped (a policy's `org_id` and the rule's visibility).

## API and dashboard

- `GET/POST/PUT/DELETE /api/v1/notify/policies` (staff, CSRF, RS2 org-scoped) — a policy carries its
  ordered steps (each: after_minutes, channel_ids). `notify_rules` create/update accepts an optional
  `escalation_policy_id` (must be a visible policy, else 400/404). Audit `notify.policy_*`.
- Dashboard Notifications page gains an **Escalation policies** panel: create a policy, add ordered steps
  (delay + channels), attach a policy to a rule in the rule editor. The delivery log shows the step
  number for escalation deliveries. Pure helpers (step ordering/validation, due-step preview, policy
  summary) in `web/src/lib/escalation.ts` with a `node --test`. `?mock=1` gets a policy with two steps
  attached to a rule, and the SMS channel. Phone width intact.

## Docs

`docs/PROTOCOL.md` (sms channel config, the policy endpoints, the rule `escalation_policy_id`, the
delivery-log step), `docs/SECURITY.md` (SMS token write-only/redacted; escalation respects maintenance
windows and acks), `docs/OPS.md` (configuring an SMS provider; how escalation timing works), `docs/GUIDE.md`
(an "Escalation and SMS" subsection under Notifications), `docs/ARCHITECTURE.md`, `docs/FEATURES.md`,
`docs/STATUS.md` (NS2 row WORKING), `docs/ROADMAP.md` (tick the NS2 line — SMS + escalation now done),
`docs/CHECKLIST.md`.

## Proof (what the build must show here)

The six proof commands pass. A new `internal/e2e` test (or server tests) with a fake clock, an
`httptest` SMS provider and `httptest`/fake email+webhook:
- an SMS channel + a matching rule: an opened alert produces a `sent` SMS delivery; the fake provider
  received the short message; the token never appears in the delivery log or any response;
- a rule with a 2-step escalation policy (e.g. step 1 at 0 min to channel A, step 2 at 15 min to
  channel B): on open, step 1 delivers; advancing the fake clock past 15 min with the alert still open
  and unacked delivers step 2; acknowledging the alert before 15 min prevents step 2; a maintenance
  window suppresses the steps; the evaluator running twice never double-sends a step (idempotent);
- RS2: an operator scoped elsewhere cannot see or manage a policy/SMS channel in another org (404);
- every NS1 notification test still passes.

Verify stage screenshots the SMS channel + an escalation policy in the Notifications page and a
delivery-log entry showing an escalation step.
