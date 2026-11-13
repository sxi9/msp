# Status board

Every component and feature carries one label. Updated whenever something changes state;
`docs/CHECKLIST.md` holds the item-by-item evidence and `docs/screenshots/` the proof.

| Label | Meaning |
|---|---|
| `WORKING` | Built and verified in the cloud build container: a passing test, a passing end-to-end run, or a screenshot in `docs/screenshots/`. |
| `BUILDING` | Being implemented right now. Code may be in the tree but is not yet verified. |
| `NOT VERIFIED` | Built, but cannot be exercised in the build container (needs a real Windows machine or Android phone). Needs a manual check; see the "Manual test" column. |
| `PLANNED` | In the plan (phase 2 or 3), not started. |

Last update: 2026-11-13 (build in progress, nothing verified yet).

## Components

| Component | Path | Label | Manual test needed |
|---|---|---|---|
| Server (Go, SQLite, embedded dashboard) | `cmd/msp-server`, `internal/server`, `internal/store` | `BUILDING` | none; verified here |
| Desktop agent, Linux | `cmd/msp-agent`, `internal/agent` | `BUILDING` | none; verified here |
| Desktop agent, Windows | `internal/agent/*_windows.go`, `deploy/install.ps1` | `BUILDING` | install on a Windows 10/11 PC, confirm service starts, device shows online, run a command, lock |
| Android agent | `android/` | `BUILDING` | install the debug APK, enroll, grant device admin, confirm sync, lock, message |
| Web dashboard | `web/` | `BUILDING` | none; screenshots taken here |
| Ops: Docker, compose, Caddy, CI | `deploy/`, `.github/workflows` | `BUILDING` | bring the compose stack up on a real host with a domain |
| Docs: plan, protocol, decisions, diagrams | `docs/` | `WORKING` | none |

## Phase 1 features

| Feature | Component | Label |
|---|---|---|
| Multi-tenant: MSP → client orgs → devices | Server | `BUILDING` |
| Users and roles: admin, operator, viewer | Server, Dashboard | `BUILDING` |
| Login, session cookie, CSRF header, rate limits | Server, Dashboard | `BUILDING` |
| Enrollment tokens: label, max uses, expiry, revoke | Server, Dashboard | `BUILDING` |
| Agent enrollment, device token | Server, Agents | `BUILDING` |
| Sync with metrics and long-poll command delivery | Server, Desktop agent | `BUILDING` |
| Android sync every 60 s (battery, storage, RAM, apps) | Android agent | `BUILDING` |
| Online / offline / pending status | Server, Dashboard | `BUILDING` |
| Facts: OS, kernel, CPU, memory, model, serial | Desktop agent | `BUILDING` |
| Run command (exec, argv, no shell) | Desktop agent, Dashboard | `BUILDING` |
| Run script (sh, bash, PowerShell, cmd) | Desktop agent, Dashboard | `BUILDING` |
| Remote lock, managed devices only, audited | All agents, Server, Dashboard | `BUILDING` |
| Ping and message commands | All agents | `BUILDING` |
| Command history with output, exit code, status | Server, Dashboard | `BUILDING` |
| Lost-command sweeper | Server | `BUILDING` |
| Alerts: offline, disk_full, memory_high, battery_low | Server, Dashboard | `BUILDING` |
| Alert acknowledge and auto-resolve | Server, Dashboard | `BUILDING` |
| Audit log of every user, device and system action | Server, Dashboard | `BUILDING` |
| Heartbeat retention and pruning | Server | `BUILDING` |
| systemd unit install (Linux) | Desktop agent | `BUILDING` |
| Windows service install | Desktop agent | `BUILDING` |
| Android foreground service, boot receiver, device admin | Android agent | `BUILDING` |
| Agent downloads and install one-liners at /dl/ | Server | `BUILDING` |
| Dockerfile, docker-compose, Caddy TLS example | Ops | `BUILDING` |
| CI: go build/test, web build, Android APK | Ops | `BUILDING` |
| End-to-end proof script | Ops | `BUILDING` |

## Later phases

| Feature | Phase | Label |
|---|---|---|
| Interactive remote shell over WebSocket relay | 2 | `PLANNED` |
| Patch management, software deployment, file transfer | 2 | `PLANNED` |
| Notifications: email, SMS, Slack; escalation; maintenance windows | 2 | `PLANNED` |
| Script library, automations, policy groups, bulk actions | 2 | `PLANNED` |
| Reports and exports | 2 | `PLANNED` |
| Two-factor auth, device token rotation, Postgres, API keys | 2 | `PLANNED` |
| Android device-owner mode: wipe, app push, kiosk; QR enrollment; location | 2 | `PLANNED` |
| Uptime checks, SNMP, network discovery | 2 | `PLANNED` |
| Remote desktop via our own RustDesk build and relay | 3 | `PLANNED` |
| macOS and iOS agents | 3 | `PLANNED` |
| Service desk, billing, PSA integrations | 3 | `PLANNED` |
| SSO, mutual TLS, compliance packs | 3 | `PLANNED` |
