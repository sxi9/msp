# Feature catalogue

P1 = first build. P2 = next. P3 = after that.

## Desktop agent (Windows, Linux/Ubuntu)
- P1 Enroll with client token, store device token
- P1 Install as Windows service / systemd unit, auto-start
- P1 Heartbeat: CPU, memory, disk, uptime, IPs
- P1 Inventory: OS, version, hardware, installed software
- P1 Run commands and scripts, capture output, timeout
- P1 Remote lock (built-in command, managed devices only)
- P1 Reconnect with backoff, offline tolerant
- RS1 (built) Interactive remote shell over WebSocket relay (PTY / ConPTY), audited and transcribed; Windows ConPTY cross-builds but is not runtime-verified
- PM1 (built) Patch status and install: periodic pending-updates report (apt on Linux, Windows Update on Windows, behind an injectable command executor), and install all / security / named-ids through the command queue with a reboot policy; Windows Update path NOT VERIFIED on a real host
- SD1 (built) Software deployment and file transfer: a server-side file library, push a file to a desktop device and optionally run it, or install a named package (apt on Linux), hash-verified on the device and audited; Windows installer path NOT VERIFIED on a real host
- P2 Service and process watchlists, event log forwarding
- P2 Agent self-update
- RD1 (built) Remote desktop phase A: official RustDesk client installed by the installer (`--with-rustdesk`), pointed at our relay by `msp-agent rustdesk setup`, id reported in facts
- P3 Remote desktop phase B: our branded RustDesk build with the relay compiled in (`docs/RUSTDESK-BRANDING.md`)
- P3 macOS agent

## Android agent
- P1 Enroll by pasting or scanning token
- P1 Heartbeat: battery, storage, RAM, network type, OS version
- P1 Installed apps inventory (built: `facts.apps`, label / package / version / system flag, non-system first, capped at 1000; shown on the device page; not yet verified on a real phone)
- P1 Survives reboot, battery-optimisation guidance screen
- P1 Safe command set only: ping, show message, request inventory, lock (device admin)
- P2 Location reporting with consent
- P2 Device-owner mode: wipe, app push, kiosk
- P3 Remote screen via our RustDesk build (MediaProjection consent, accessibility control); phase A: the official app by hand, not offered from the dashboard
- P3 iOS agent

## Server core and API
- P1 Multi-tenant: MSP → client orgs → devices
- P1 Users with roles: admin, technician, read-only
- P1 Enrollment tokens per org, expiring
- P1 Device API: enroll, heartbeat, inventory, command results
- P1 Command queue with full audit log
- P1 Alert rules and alert state machine
- P1 Metrics retention (raw 7 days, hourly 90 days)
- P1 Docker compose, TLS via reverse proxy, backups by file copy
- RS1 (built) WebSocket relay for remote shell (single-use ticket + Origin check, 20-shell cap, 1 MiB transcript, `shell.opened`/`connected`/`closed` audit)
- P2 Postgres option, API keys, webhooks
- P2 Email, SMS, Slack notifications, escalation, maintenance windows
- P2 Two-factor auth, device token rotation
- RD1 (built) RustDesk hbbs/hbbr in compose, `MSP_RUSTDESK_HOST`/`KEY`, device RustDesk id registry (`devices.rustdesk_id`), `GET /api/v1/rustdesk`, `POST /devices/{id}/remote-desktop`, audit `device.remote_desktop_opened`
- RS2 (built) Per-technician org scoping: `user_orgs` table + `users.all_orgs`, visible-org set resolved per request and pushed into the store list queries, 404-not-403 at every org/device endpoint, `all_orgs`/`org_ids` on `GET /users`, scoping on `POST /users`, `PUT /users/{id}/orgs`, audit `user.orgs_changed`
- NS1 (built) Alert notifications: email (SMTP, STARTTLS) and webhook (Slack incoming-webhook + generic JSON with optional HMAC-SHA256 signature) channels, per-org/severity/type routing rules, one-shot maintenance windows that suppress, a retrying background dispatcher (backoff, `maxAttempts`, concurrency-safe claim) with an idempotent enqueue and a delivery log, a Send-test action, all org-scoped for staff like RS2; secrets are write-only (`secret_set`), never logged or stored in `detail_json`
- P2 Notifications NS2: SMS (another channel `kind`, an HTTP provider) and multi-step escalation policies — reserved, not built
- P3 Session length / recording audit for remote desktop
- P3 Single sign-on, mutual TLS

## Technician dashboard
- P1 Login, session, role-aware menus
- P1 Clients list, client page with enrollment token button
- P1 Device list: online/offline, OS, last seen, filters
- P1 Device page: live metrics, inventory, software (Android installed apps panel with search and system toggle; desktop software list is P2), command history, Lock button
- P1 Run command / saved script on one device or a group
- P1 Command preset library: server-served dropdown, editable JSON file (`MSP_PRESETS`) with live reload, builtin fallback
- P1 Alerts page: acknowledge, resolve
- P1 Audit log view
- RS1 (built) Browser terminal for remote shell (`@xterm/xterm`, resize, status line, Transcript link on finished shell commands)
- P2 Script library, scheduled automations, policy groups
- P2 Reports: uptime, disk trends, patch compliance, exports
- P2 Client portal with limited self-service view
- RD1 (built) Remote desktop card on the device page: id, Copy, Connect button (opens `rustdesk://connect/<id>`), not-configured / not-reported states
- RS2 (built) Users page "Edit access" dialog (All-clients toggle + per-client checklist) and the same control on the create-user form; "Add client" hidden for non-admins; a scoped technician's Fleet/Orgs/Alerts/Audit simply show less
- NS1 (built) Notifications section (Channels, Rules, Maintenance windows) for admin/operator with a "Send test" button, a Delivery log view (time, org, device/alert, channel, event, status chip, attempts, last error) with status/org filters, write-only secret fields ("set / not set" + replace); viewer sees the read-only log
- P3 White-label branding per client, custom dashboards

## Notifications (NS1 built; NS2 reserved)
- NS1 (built) Email channel: SMTP with STARTTLS and optional PLAIN auth, plain-text message (subject `[MSP] <sev> <type> on <host> (<org>)`, body with device/org/message/time and a dashboard link when `MSP_BASE_URL` is set)
- NS1 (built) Webhook channel: Slack incoming-webhook (`{"text":...}`) and generic JSON (`{alert_id, org, device, type, severity, message, event, at}`) with an optional `X-MSP-Signature: sha256=…` HMAC when a signing secret is set
- NS1 (built) Routing rules: per org (or global), `min_severity`, alert-type list (empty = all), on-open / on-resolve, to one channel
- NS1 (built) Maintenance windows: one-shot absolute start/end per org (or all), suppress deliveries while active (an operational mute, not a security control)
- NS1 (built) Dispatcher: background goroutine, claims pending rows, retries with backoff, gives up at `maxAttempts` → `failed`, concurrency-safe lease, idempotent enqueue, injectable clock + sender for tests
- NS2 (reserved) SMS channel kind (HTTP provider) and multi-step escalation policies — schema leaves room, not built

## Monitoring beyond agents
- P2 Website and TCP uptime checks
- P2 SNMP: printers, switches, routers, UPS
- P2 Network discovery per client site via a probe agent
- P3 Syslog collection and search

## Security and compliance reporting
- P1 Hashed passwords, per-device tokens, tenant check on every query
- P1 No shell injection: argv exec, allow-listed Android commands
- P2 Antivirus / EDR, firewall, disk encryption status
- P2 Vulnerability and patch compliance reports
- P3 Compliance packs (Cyber Essentials, SOC 2 evidence export)

## Service desk and business
- P3 Ticketing linked to devices and alerts
- P3 Client contacts, time tracking, billing exports
- P3 Knowledge base
- P3 PSA integrations: Autotask, ConnectWise, HaloPSA
- P3 Warranty and asset lifecycle
