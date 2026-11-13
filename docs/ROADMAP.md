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
- [ ] Per-technician org scoping (all staff see all clients in phase 1) — moved to phase 2

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
- [ ] Installed apps inventory — NOT BUILT: no package-listing code in the Android app yet (queued as a fix)
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
- [ ] Interactive remote shell in the browser over a WebSocket relay (PTY / ConPTY), audited
- [ ] Per-technician org scoping and client-level permissions
- [ ] Patch status and install: Windows Update, apt
- [ ] Software deployment and file transfer
- [ ] Installed-software inventory on desktops
- [ ] Service and process watchlists; event log and journal forwarding
- [ ] Agent self-update
- [ ] Notifications: email, SMS, Slack; escalation policies; maintenance windows
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
