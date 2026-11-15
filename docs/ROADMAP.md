# Roadmap and checklists

One line per deliverable. A ticked box means verified in the cloud build container with a test,
an end-to-end run or a labelled screenshot (see docs/CHECKLIST.md and docs/screenshots/).
An unticked box with a note is built but needs real hardware, or is not started.

## Phase 1 — Core RMM (done)

### Server
- [x] One Go binary, embedded SQLite, embedded dashboard
- [x] Multi-tenant model: MSP → client orgs → devices, tokens, alerts and audit scoped by org
- [x] Users and roles: admin, operator, viewer; sessions, CSRF header, login rate limit
- [x] Account page: change own password; admin reset via `user set-password`
- [x] Enrollment tokens: label, max uses, expiry, revoke; install one-liners per OS
- [x] Agent API: enroll, sync with 25 s long-poll, command results
- [x] Command queue: exec, script, lock, ping, message; lost-command sweeper
- [x] Alerts: offline, disk_full, memory_high, battery_low; acknowledge; auto-resolve
- [x] Audit log for every user, device and system action, never pruned
- [x] Heartbeat retention and hourly pruning
- [x] Agent downloads at /dl/ with SHA256SUMS
- [x] Graceful shutdown releases parked long-polls
- [x] Command preset library: presets.json, live reload, served to the dashboard
- [x] Per-technician org scoping (RS2): `user_orgs` + `users.all_orgs`, visible-org set in the store list queries, 404-not-403 at every org/device endpoint, `all_orgs`/`org_ids` on `GET /users`, `PUT /users/{id}/orgs`, audit `user.orgs_changed`; proven by `internal/e2e` `TestOrgScoping` and `TestDashboardOrgScoping`

### Desktop agent, Linux
- [x] Enroll, config with 0600 permissions, device token
- [x] Sync: CPU, memory, disks, uptime, IPs, battery
- [x] Facts: OS, kernel, CPU, memory, manufacturer, model, serial
- [x] exec with argv and no shell; script via sh or bash; timeout kills the process group
- [x] lock, ping, message; headless host reports "no interactive session"
- [x] systemd unit install and uninstall; reconnect with backoff; token-rejected handling
- [x] End-to-end: enroll → online → command → output → offline alert → ack
- [ ] Screen actually locks on an Ubuntu desktop — needs a real desktop to confirm

### Desktop agent, Windows
- [x] Cross-compiles and vets for windows/amd64
- [x] Windows service, LockWorkStation in the console session, PowerShell and cmd scripts, install.ps1 (code complete)
- [ ] Service install, online, command, lock on a real Windows 10/11 PC — needs a Windows PC

### Android agent
- [x] Debug APK builds; release signing wired to a keystore; JVM protocol tests pass
- [x] Enrollment screen, foreground service, boot receiver, device admin for lock, lock/ping/message handling (code complete)
- [x] Installed apps inventory: `AppInventory.kt` lists packages through PackageManager (`QUERY_ALL_PACKAGES`), `Apps.kt` shapes `facts.apps` (JVM-tested), the server validates and stores it, the device page shows it with search and a system-apps toggle (screenshots 18 and 19, a 40-app inventory sent through the real agent API). Device verification pending: the PackageManager read has not run on a real phone yet
- [ ] Enroll, sync, lock, message on a real phone — needs a phone

### Dashboard
- [x] Login, Fleet, Orgs, Org page with tokens, Device page, Alerts, Audit, Users, Account
- [x] Run console with presets, Lock, Ping, Message, Revoke, Delete
- [x] Phone width without horizontal scroll on every page
- [x] 30 labelled screenshots in docs/screenshots/

### Ops and docs
- [x] Dockerfile, docker-compose, Caddy TLS example, systemd unit, installers, CI workflow
- [x] End-to-end script with real binaries (29 checks)
- [x] PLAN, PROTOCOL, ARCHITECTURE, OPS, SECURITY, GUIDE, STATUS, FEATURES, DECISIONS, diagrams
- [ ] Docker image build and compose stack on a real host — no Docker daemon in the build container

## Phase 1A — Remote desktop via self-hosted RustDesk (in progress)
- [ ] RustDesk ID and relay services in docker-compose with key handling
- [ ] Server config MSP_RUSTDESK_HOST / MSP_RUSTDESK_KEY; relay settings sent to agents at enroll and sync
- [ ] Agent reports its RustDesk ID in facts; stored in devices.rustdesk_id
- [ ] `msp-agent rustdesk setup`: point the installed client at our relay only, set a permanent password
- [ ] Installers: --with-rustdesk deploys the client from /dl/
- [ ] Device page: Remote desktop card with ID and Connect button; audited
- [ ] Docs: OPS, GUIDE, SECURITY, PROTOCOL, branded-build recipe
- [ ] A real screen session between two machines — needs two real machines

## Phase 2 — Operations
- [x] Interactive remote shell in the browser over a WebSocket relay (PTY / ConPTY), audited and transcribed (RS1, decision D3; Linux verified end to end by `internal/e2e` `TestRemoteShellEndToEnd`; Windows ConPTY cross-builds but needs a Windows host)
- [x] Per-technician org scoping (RS2): scope an operator/viewer to a chosen set of client orgs, enforced in SQL with the 404-not-403 rule at every endpoint (client-facing self-service logins remain future work)
- [x] Alert notifications (NS1): email (SMTP) and webhook (Slack-compatible + generic JSON) delivery, per-org/severity/type routing rules, maintenance-window suppression, a retrying background dispatcher with a delivery log and a Send-test action, all org-scoped for staff like RS2; proven by `internal/e2e` `TestNotificationsDelivery`/`TestNotificationsOrgScoping`, the `internal/server` notify tests, and a live dashboard run with an email channel on a local fake SMTP server and a Slack channel on a local listener (screenshots 24 and 25: a `sent` and a `suppressed` delivery)
- [x] Patch status and install (PM1): per-device pending-updates report (apt on Linux, Windows Update on Windows) and role-gated, tenant-scoped, audited install (all / security / named ids) through the command queue; the Linux path is proven by `internal/e2e` `TestPatchEndToEnd` and `scripts/e2e.sh` against a fake `apt-get`, and by a live dashboard run with the real Linux agent against a fake `apt-get` and a `/var/run/reboot-required` flag (screenshots 26 and 27: the device Updates panel with the reboot badge, 3 pending / 2 security and the update list, and the Fleet Updates column emphasised for security); **the Windows Update path is written, cross-built and parser-unit-tested against a captured fixture but NOT VERIFIED on a real Windows host**
- [x] Software deployment and file transfer (SD1): a server-side file library (upload/list/delete, org-scoped like RS2), push a file to a desktop device and optionally run it, or install a named package, through the command queue; the agent downloads over its authenticated channel and verifies the SHA-256 before writing or running, destination paths are validated (absolute, no `..`, no symlink traversal), and upload/delete/deploy are role-gated, tenant-scoped and audited (`file.uploaded`, `file.deleted`, `deploy.requested`); the Linux path (real download + run, and apt package install) is proven by `internal/e2e` `TestDeployEndToEnd` against a fake `apt-get` and a local file server, and by a live dashboard run with the real Linux agent and a marker file uploaded over the API (screenshots 28 and 29: the Software library page with the file's size/sha256/client and the device Deploy dialog); **the Windows installer path is written and cross-built but NOT VERIFIED on a real Windows host**
- [x] Installed-software inventory on desktops (SI1): the desktop agent reports its installed software in the periodic facts (`dpkg-query`, falling back to `rpm`, on Linux; the uninstall registry on Windows; all read-only), stored by `RecordSoftware` on the `devices` table (migration `0011_device_software`), shown on the device page's Installed-software panel (count, last checked, search) and searchable fleet-wide at `GET /api/v1/software?q=` for licensing and vulnerability hunting, org-scoped like RS2 (an operator scoped elsewhere sees no cross-org rows); proven by `internal/e2e` `TestSoftwareEndToEnd` driving the real Linux agent against a fake `dpkg-query` on PATH (the inventory on `GET /devices/{id}` with the right count and full list, the fleet search returning the device, the RS2 404/no-row checks) and the agent parser unit tests against captured dpkg/rpm/registry fixtures; **the Windows registry path is written and cross-built but NOT VERIFIED on a real Windows host**
- [ ] Service and process watchlists; event log and journal forwarding
- [x] Agent self-update (AU1): the server publishes an agent release manifest (`agents.json` in `MSP_AGENT_DIR`, served at `/dl/`) and advertises a newer build to each desktop on its sync (`agent_update`); a role-gated, tenant-scoped, audited `POST /devices/{id}/update` forces it, and the agent downloads the binary over its authenticated channel, verifies the SHA-256, health-checks the new binary (runs its `version` subcommand), atomically swaps itself keeping a `<exe>.bak`, and rolls back on a bad post-swap start; auto-update is on by default and opts out with `--no-auto-update`; the Linux path (download + verify + health-check + swap, plus the hash-mismatch, health-check-failure and bootstrap-rollback cases) is proven by `internal/e2e` `TestSelfUpdateEndToEnd` against the real Linux agent with the swap targeting a temp exe, and by the `internal/agent` self-update unit tests; **the Windows service-swap path is written and cross-built but NOT VERIFIED on a real Windows host**
- [x] Notifications: SMS and multi-step escalation policies (NS2): an `sms` channel kind (Twilio form + HTTP basic auth, or a generic JSON gateway with a bearer token; the provider auth token is write-only and redacted, the short message carries no tokened URL) and escalation policies — an ordered list of steps that fire their channels `after_minutes` from an alert opening, but only while it stays open and unacknowledged, suppressed under maintenance windows, idempotent across ticks, in addition to the rule's own `on_open` channel; migration `0013_ns2` widens the channel `kind` CHECK FK-safely and adds `escalation_policies`/`escalation_steps`, `notify_rules.escalation_policy_id` and `notification_deliveries.step`; the escalation tick runs first in the production dispatcher and `/api/v1/notify/policies` is org-scoped (RS2) and audited (`notify.policy_*`); proven by `internal/e2e` `TestNS2Delivery` (httptest SMS provider on a fake clock: sent SMS with the short message and no token leak; step 1 on open, step 2 after the clock passes its delay, never after an ack, suppressed under a window, never double-sent) and `TestNS2OrgScoping`, with every NS1 test still passing, and a live dashboard run with the real Linux agent, an `oncall-sms` channel on a local HTTP SMS listener and a 2-step escalation policy attached to a rule — stopping the agent opened an `offline` alert whose `on_open` delivery (step 0) and both policy steps (STEP 1, STEP 2) were `sent` to the listener (screenshots 36 and 37). **A real SMS to a phone is NOT VERIFIED** (no outbound SMS in the build container)
- [x] Script library, bulk actions, scheduled and triggered automations (AUT1): a DB-backed, editable script library (exec/script, per-client or global, distinct from the static `presets.json` console presets); a bulk run that fans a script out across a target of visible devices as one tracked job with derived progress (queued/sent/done/failed), skipping android for exec/script with a per-device note; scheduled automations (every N minutes) run by a tick in the server jobs loop (fake-clock-injectable via `now()`, missed ticks never stack); and triggered automations that run a script on the alerting device when a matching alert opens (same `OpenAlert`-true hook as notifications), with a per-automation-per-device cooldown; all reuse the command queue (agent wire COMMAND unchanged, dashboard COMMAND gains `job_id`), are RS2 org-scoped (a target can never reach a device outside the caller's/automation's visible org set) and audited (`script.*`, `job.created`, `automation.*`, the two run actions as actor `system`); migration `0014_automations`; proven by `internal/e2e/automations_test.go` (fake clock + the real Linux agent: a 2-device bulk run the agent runs to `done` with progress reflecting it and android skipped, a scheduled automation that fires once when the clock advances past its due time without stacking missed ticks, a triggered automation that runs on an offline alert's device once with the cooldown blocking the re-run, and the RS2 scoping across two orgs) and the `internal/server` automations/scripts/jobs tests; policy groups remain future work
- [x] Reports: uptime, disk trends, patch compliance; exports (RP1): read-only, RS2 org-scoped aggregates over the data already stored (per-minute heartbeats + the denormalised device patch columns; no new migration, no agent change) at `GET /api/v1/reports/{uptime,disk,disk/top,patch-compliance}` with sibling `.csv` exports (attachment, CRLF, formula-injection-guarded text cells), the period bounded by `MSP_RETENTION_DAYS`; uptime is a check-in approximation (samples vs. interval-derived expected, prorated from enrolment) shown next to the device's OS `uptime_s`, patch buckets are up_to_date/pending/never_checked (partition) plus security_pending/reboot_required (overlapping flags); the dashboard Reports page renders all three (org + period toolbar, hand-written SVG disk chart, compliance tiles, Download-CSV) — proven by `internal/e2e` `TestReports` (seeded heartbeats + patch state: uptime_pct with mid-period proration, disk series min/max/latest, patch bucketing, injection-safe CSV with the attachment header, RS2 operator sees only its org and gets 404 on another, days beyond retention rejected), `internal/store` reports tests, `internal/server` `dashboard_reports_test.go`, `web/src/lib/reports.test.ts`, the six proof commands, and a live-browser run with the real Linux agent (screenshots 41 uptime table, 42 disk-trend chart, 43 patch-compliance summary; desktop and phone, phone `scrollWidth` 400)
- [x] Two-factor authentication (TF1): RFC 6238 TOTP second factor (native Go stdlib, no new dependency) for staff logins, with a two-step login (password returns `{totp_required}` and a 5-min pending cookie, then `POST /api/v1/login/totp` with a TOTP or recovery code mints the session), self-service enrolment (QR + manual secret, confirm, 10 single-use sha256-hashed recovery codes shown once), disable and regenerate, `totp_enabled`/`recovery_codes_remaining` on `me`, a 2FA badge and an admin `Reset 2FA` for lockouts, the secret write-only and both login steps rate-limited; proven by `internal/server/auth_totp_test.go` and `internal/e2e/twofactor_test.go` (enrol, two-step login, wrong/stale/replayed codes, recovery single-use, disable, admin reset, RFC 6238 vectors, non-2FA login unchanged), plus a live-browser run capturing the Account two-factor setup (QR + manual secret) and the login second-factor prompt (`docs/screenshots/30-account-2fa-setup-*`, `31-login-2fa-prompt-*`, phone `scrollWidth` 400) with a second user's 2FA enrolled over the API — all deterministic, no authenticator app or external call. **Making enrolment mandatory by policy** and **device token rotation; API keys; webhooks** remain
- [ ] Device token rotation; API keys; webhooks
- [ ] Postgres option
- [ ] Android: QR enrollment, location with consent, device-owner mode (wipe, app push, kiosk)
- [ ] Monitoring beyond agents: website and TCP checks, SNMP (printers, switches, UPS), network discovery probe
- [ ] Security posture: antivirus/EDR, firewall, disk encryption status; vulnerability and patch compliance
- [ ] Client portal with limited self-service view

## Phase 3 — Scale and business
- [ ] Branded RustDesk client build with our relay and key compiled in (docs/RUSTDESK-BRANDING.md)
- [ ] macOS and iOS agents; Windows on ARM
- [ ] Single sign-on; mutual TLS for agents
- [ ] Ticketing linked to devices and alerts; client contacts; time tracking; billing exports
- [ ] Knowledge base
- [ ] PSA integrations: Autotask, ConnectWise, HaloPSA
- [ ] Warranty and asset lifecycle
- [ ] Syslog collection and search
- [ ] Compliance packs (Cyber Essentials, SOC 2 evidence export)
- [ ] White-label branding per client; custom dashboards
