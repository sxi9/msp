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
- [x] Software deployment and file transfer (SD1): a server-side file library (upload/list/delete, org-scoped like RS2), push a file to a desktop device and optionally run it, or install a named package, through the command queue; the agent downloads over its authenticated channel and verifies the SHA-256 before writing or running, destination paths are validated (absolute, no `..`, no symlink traversal), and upload/delete/deploy are role-gated, tenant-scoped and audited (`file.uploaded`, `file.deleted`, `deploy.requested`); the Linux path (real download + run, and apt package install) is proven by `internal/e2e` `TestDeployEndToEnd` against a fake `apt-get` and a local file server; **the Windows installer path is written and cross-built but NOT VERIFIED on a real Windows host**
- [ ] Installed-software inventory on desktops
- [ ] Service and process watchlists; event log and journal forwarding
- [ ] Agent self-update
- [ ] Notifications: SMS and multi-step escalation policies (NS2 follow-ons; the NS1 schema and channel `kind` enum reserve room for an SMS channel and an escalation table)
- [ ] Script library in the dashboard, scheduled and triggered automations, policy groups, bulk actions
- [ ] Reports: uptime, disk trends, patch compliance; exports
- [ ] Two-factor authentication; device token rotation; API keys; webhooks
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
