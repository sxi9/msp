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
- P2 Patch status and install (Windows Update, apt)
- P2 Software deployment, file transfer
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
- P3 White-label branding per client, custom dashboards

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
