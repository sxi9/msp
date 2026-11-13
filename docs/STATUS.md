# Status board

Every component and feature carries one label. Updated whenever something changes state;
`docs/CHECKLIST.md` holds the item-by-item evidence and `docs/screenshots/` the proof.

| Label | Meaning |
|---|---|
| `WORKING` | Built and verified in the cloud build container: a passing test, a passing end-to-end run, or a screenshot in `docs/screenshots/`. |
| `BUILDING` | Being implemented right now. Code may be in the tree but is not yet verified. |
| `NOT VERIFIED` | Built, but cannot be exercised in the build container (needs a real Windows machine or Android phone). Needs a manual check; see the "Manual test" column. |
| `PLANNED` | In the plan (phase 2 or 3), not started. |

Last update: 2026-11-13 (fix pass after review). Server, Linux agent and dashboard verified in a live run (see docs/screenshots/); the rows below marked WORKING without a screenshot are covered by `go test ./...` (internal/store, internal/server, internal/e2e) and `scripts/e2e.sh` ("E2E OK").

## Components

| Component | Path | Label | Manual test needed |
|---|---|---|---|
| Server (Go, SQLite, embedded dashboard) | `cmd/msp-server`, `internal/server`, `internal/store` | `WORKING` | none; verified here |
| Desktop agent, Linux | `cmd/msp-agent`, `internal/agent` | `WORKING` | none; verified here |
| Desktop agent, Windows | `internal/agent/*_windows.go`, `deploy/install.ps1` | `NOT VERIFIED` | install on a Windows 10/11 PC, confirm service starts, device shows online, run a command, lock |
| Android agent | `android/` | `NOT VERIFIED` | build the signed release APK (`make android-release`, docs/OPS.md section 3), install it, enroll over https, grant device admin, confirm sync, lock, message |
| Web dashboard | `web/` | `WORKING` | none; screenshots taken here |
| Ops: Docker, compose, Caddy, CI | `deploy/`, `.github/workflows` | `WORKING` (`docker compose config` validates, `scripts/e2e.sh` and `scripts/build-agents.sh` pass here) / `NOT VERIFIED` (the image build itself: no Docker daemon in the build container) | `docker build -f deploy/Dockerfile .`, then bring the compose stack up on a real host with a domain |
| Docs: plan, protocol, decisions, diagrams | `docs/` | `WORKING` | none |

## Phase 1 features

| Feature | Component | Label |
|---|---|---|
| Multi-tenant: MSP → client orgs → devices | Server | `WORKING` |
| Users and roles: admin, operator, viewer | Server, Dashboard | `WORKING` |
| Login, session cookie, CSRF header, rate limits | Server, Dashboard | `WORKING` |
| Enrollment tokens: label, max uses, expiry, revoke | Server, Dashboard | `WORKING` |
| Agent enrollment, device token | Server, Agents | `WORKING` |
| Sync with metrics and long-poll command delivery | Server, Desktop agent | `WORKING` |
| Android sync every 60 s (battery, storage, RAM, apps) | Android agent | `NOT VERIFIED` |
| Online / offline / pending status | Server, Dashboard | `WORKING` |
| Facts: OS, kernel, CPU, memory, model, serial | Desktop agent | `WORKING` |
| Run command (exec, argv, no shell) | Desktop agent, Dashboard | `WORKING` |
| Run script (sh, bash, PowerShell, cmd) | Desktop agent, Dashboard | `WORKING` (sh/bash: `scripts/e2e.sh` script kind, runner tests) / `NOT VERIFIED` (PowerShell, cmd: needs Windows) |
| Remote lock, managed devices only, audited | All agents, Server, Dashboard | `WORKING` (queue, delivery, audit, role and tenant checks) / `NOT VERIFIED` (the lock itself: needs a real Ubuntu desktop, Windows PC or Android phone; on the headless build host it reports failed, see known issues) |
| Ping and message commands | All agents | `WORKING` |
| Command history with output, exit code, status | Server, Dashboard | `WORKING` |
| Command preset library (file-based) | Server, Dashboard | `WORKING` (`TestPresets*` loader, endpoint, mtime reload and last-good fallback; `web/src/lib/presets.test.ts`; live check with `MSP_PRESETS=deploy/presets.example.json`: file served, "Failed services" picked and run from the dashboard, edit picked up without restart, screenshots 14, 15) |
| Lost-command sweeper | Server | `WORKING` (`TestMarkLostCommands`, `TestLostCommands`) |
| Alerts: offline, disk_full, memory_high, battery_low | Server, Dashboard | `WORKING` |
| Alert acknowledge and auto-resolve | Server, Dashboard | `WORKING` |
| Audit log of every user, device and system action | Server, Dashboard | `WORKING` |
| Heartbeat retention and pruning | Server | `WORKING` (`TestPrune*`, `TestPruneOnce`; indexed, batched) |
| systemd unit install (Linux) | Desktop agent | `WORKING` (CI systemd round trip; service tests) |
| Windows service install | Desktop agent | `NOT VERIFIED` |
| Android foreground service, boot receiver, device admin | Android agent | `NOT VERIFIED` |
| Agent downloads and install one-liners at /dl/ | Server | `WORKING` (`/dl/install.sh`, `/dl/SHA256SUMS` served from `dist/` in the live run; `TestDashboardStatic`) |
| Password change (Account page) and CLI reset | Server, Dashboard | `WORKING` (`TestDashboardChangePassword`) |
| Dockerfile, docker-compose, Caddy TLS example | Ops | `WORKING` (compose config validates) / `NOT VERIFIED` (image build needs a Docker daemon) |
| CI: go build/test, web build, Android debug + release APK | Ops | `WORKING` (the same commands pass in the build container) |
| End-to-end proof script | Ops | `WORKING` (`scripts/e2e.sh` exits 0 with "E2E OK") |

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

## Known issues found in the live run

1. Fixed: lock on a Linux host with no desktop session now reports exit 0 "no interactive session" when loginctl says there is nothing to lock, and otherwise lists what every attempt said (`lock failed: loginctl: ...; xdg-screensaver: ...`).
2. CPU chart on the device page shows "no samples yet" until the second minute, because CPU needs two samples and heartbeat rows are stored once per minute. Cosmetic.
3. Fixed: the published Android APK was the debug build (cleartext allowed, debuggable, debug-signed). `scripts/build-agents.sh` now publishes only the signed release APK (`make android-release`, docs/OPS.md section 3).
4. Fixed: `disk_full` fired on small `/boot` partitions; `disk_used_pct` now ignores filesystems under 2 GB and the `/boot` family, and the alert names the mount.
5. Fixed: graceful shutdown waited the full 10 s and exited 1 while any agent was parked in a sync long-poll.
