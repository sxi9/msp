# Status board

Every component and feature carries one label. Updated whenever something changes state;
`docs/CHECKLIST.md` holds the item-by-item evidence and `docs/screenshots/` the proof.

| Label | Meaning |
|---|---|
| `WORKING` | Built and verified in the cloud build container: a passing test, a passing end-to-end run, or a screenshot in `docs/screenshots/`. |
| `BUILDING` | Being implemented right now. Code may be in the tree but is not yet verified. |
| `NOT VERIFIED` | Built, but cannot be exercised in the build container (needs a real Windows machine or Android phone). Needs a manual check; see the "Manual test" column. |
| `PLANNED` | In the plan (phase 2 or 3), not started. |

Last update: 2026-11-13 (RD1 remote desktop, phase A of D4: final verification pass, screenshots 16 and 17, docs/CHECKLIST.md "Remote desktop (RustDesk, phase A)"). Server, Linux agent and dashboard verified in a live run (see docs/screenshots/); the rows below marked WORKING without a screenshot are covered by `go test ./...` (internal/store, internal/server, internal/e2e) and `scripts/e2e.sh` ("E2E OK").

## Components

| Component | Path | Label | Manual test needed |
|---|---|---|---|
| Server (Go, SQLite, embedded dashboard) | `cmd/msp-server`, `internal/server`, `internal/store` | `WORKING` | none; verified here |
| Desktop agent, Linux | `cmd/msp-agent`, `internal/agent` | `WORKING` | none; verified here |
| Desktop agent, Windows | `internal/agent/*_windows.go`, `deploy/install.ps1` | `NOT VERIFIED` | install on a Windows 10/11 PC, confirm service starts, device shows online, run a command, lock |
| Android agent | `android/` | `NOT VERIFIED` | build the signed release APK (`make android-release`, docs/OPS.md section 3), install it, enroll over https, grant device admin, confirm sync, lock, message |
| Web dashboard | `web/` | `WORKING` | none; screenshots taken here |
| RustDesk relay (hbbs/hbbr) in compose | `deploy/docker-compose.yml` | `WORKING` (`docker compose config` validates the two services) / `NOT VERIFIED` (running them: no Docker daemon here) | bring the stack up on a real host, `cat deploy/rustdesk-data/id_ed25519.pub`, set `MSP_RUSTDESK_HOST`/`KEY`, open 21115-21117/tcp + 21116/udp |
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

## RD1: remote desktop through our own RustDesk relay (D4 phase A)

| Part | Component | Label |
|---|---|---|
| Server: `MSP_RUSTDESK_HOST`/`KEY` (both or neither; host a bare hostname/IP, key one base64 line), `GET /api/v1/rustdesk`, `rustdesk` block in enroll/sync responses | Server | `WORKING` (`TestConfigValidateRustdesk`, `TestRustdeskInfoAndAgentResponses`) |
| Server: `facts.rustdesk_id` validated and stored in `devices.rustdesk_id` on enroll and sync; `rustdesk_id` in DEVICE JSON | Server, Store | `WORKING` (`TestRustdeskIDFromFacts`, `TestRustdeskIDStoredFromFacts`, `TestDeviceJSONShape`) |
| Server: `POST /devices/{id}/remote-desktop` (role, CSRF, 404 scope/revoked, 409 disabled, 409 no id, audit `device.remote_desktop_opened`) | Server | `WORKING` (`TestRemoteDesktop`) |
| Protocol: `Rustdesk` type and `Validate` (`ValidRustdeskHost`, `ValidRustdeskKey`), `Facts.RustdeskID`, validators, `rustdesk://connect/<id>`, two fixtures | Protocol | `WORKING` (`TestFixturesRoundTrip`, `TestRustdesk`) |
| Dashboard: Remote desktop card (id + Copy, Connect, link fallback, not-configured and not-reported states, viewer sees no button), mock mode | Dashboard | `WORKING` (`web/src/lib/rustdesk.test.ts`; typecheck + build; live run with the real agent reading a fake `RustDesk.toml`, Connect clicked, POST 200 with the `rustdesk://` URL, audit row rendered: `docs/screenshots/16-device-remote-desktop-{desktop,phone}.png`, `17-audit-remote-desktop-{desktop,phone}.png`; `?mock=1` and `?mock=1&rustdesk=0`) / `NOT VERIFIED` (the browser handing `rustdesk://` to a client: needs a technician PC with RustDesk) |
| Agent: relay stored from enroll/sync (saved only on change; a malformed block is ignored), `rustdesk_id` in facts via `rustdesk --get-id` with a `RustDesk.toml` fallback | Desktop agent | `WORKING` (`TestRunSavesRustdeskRelayFromSync`, `TestRustdeskIDFromCLIAndTOML`, `TestConfigSetRustdeskAndStatus`; `scripts/e2e.sh` with a stub binary; live run with the TOML fallback, screenshot 16) |
| Agent: `msp-agent rustdesk setup` (relay re-validated, RustDesk2.toml `[options]` merge, `--password`, restart, `--get-id`, password file 0600, `--password-stdin`, `--config-string`) | Desktop agent | `WORKING` against a stub client (`TestRustdeskSetup`, `TestMergeRustdeskOptions`) / `NOT VERIFIED` against the real client (see manual test) |
| Agent: Windows DACL (SYSTEM + Administrators, inheritance off) on `%ProgramData%\msp-agent`, `agent.json` and `rustdesk-password`, set from Go (`restrictToPrivileged`, `golang.org/x/sys/windows`) by `install`/`Save` and `rustdesk setup` | Desktop agent | `WORKING` (compiles with `GOOS=windows`; no-op stub on Linux is what the tests run) / `NOT VERIFIED` (the ACL on a real Windows host: `icacls %ProgramData%\msp-agent\rustdesk-password` must list only SYSTEM and Administrators, no `BUILTIN\Users`) |
| Installers `--with-rustdesk` / `-WithRustDesk`; `scripts/build-agents.sh --sums-only` | Ops | `WORKING` (`bash -n`, `sh -n`, `--sums-only` exercised on a scratch directory) / `NOT VERIFIED` (a real install: needs the published `.deb`/`.exe` and a desktop) |
| An actual screen session technician -> device | All | `NOT VERIFIED`: RustDesk cannot run in the build container (no display, no client, no Docker daemon for hbbs/hbbr); the live run proves only the Connect endpoint, the URL and the audit entry (screenshot 17) |
| Phase B branded client | — | `PLANNED` (`docs/RUSTDESK-BRANDING.md`) |

Manual test for the `NOT VERIFIED` rows, in order: (1) on a host with Docker, first confirm that a plain `docker compose -f deploy/docker-compose.yml up -d` starts only `server` (`docker compose ps` lists no hbbs/hbbr, nothing listens on 21116), then `MSP_RUSTDESK_HOST=<public name> docker compose -f deploy/docker-compose.yml --profile rustdesk up -d`, confirm `deploy/rustdesk-data/id_ed25519.pub` exists and ports 21115-21117/tcp and 21116/udp are open; set `MSP_RUSTDESK_HOST`/`MSP_RUSTDESK_KEY`, restart the server, `GET /api/v1/rustdesk` says `enabled:true`. (2) Publish `rustdesk-1.5.3-x86_64.deb` and `rustdesk-1.5.3-x86_64.exe` at `/dl/` (`scripts/build-agents.sh --sums-only`). (3) On an Ubuntu desktop run the install one-liner with `--with-rustdesk`: expect the `.deb` installed, `/root/.config/rustdesk/RustDesk2.toml` holding the relay keys, the password printed, the id printed, and within a minute the device page showing the id (if the relay does not take in the RustDesk UI, re-run `msp-agent rustdesk setup --config-string <exported from Settings > Network>`). (4) Same on a Windows 10/11 PC with `-WithRustDesk` (check `%ProgramFiles%\RustDesk\rustdesk.exe`, the service, the LocalService `RustDesk2.toml`), and once more on a second PC with `msp-agent.exe install` + `msp-agent.exe rustdesk setup` by hand from an elevated prompt: `icacls %ProgramData%\msp-agent %ProgramData%\msp-agent\rustdesk-password` must show SYSTEM and Administrators only, and `type` of the file as a standard user must be denied. (5) On a technician PC with the official client set to the same host and key, press **Connect** on the device page: the browser hands `rustdesk://connect/<id>` to the client, the client asks the permanent password, the screen appears; the Audit page shows `device.remote_desktop_opened` with your user and IP. (6) Disable: unset both variables, restart; the card says not configured and Connect is gone.

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
| Remote desktop phase A: official client + our relay (RD1) | 3 | `WORKING` / `NOT VERIFIED` (see the RD1 table above) |
| Remote desktop phase B: our branded RustDesk build | 3 | `PLANNED` (`docs/RUSTDESK-BRANDING.md`) |
| macOS and iOS agents | 3 | `PLANNED` |
| Service desk, billing, PSA integrations | 3 | `PLANNED` |
| SSO, mutual TLS, compliance packs | 3 | `PLANNED` |

## Known issues found in the live run

1. Fixed: lock on a Linux host with no desktop session now reports exit 0 "no interactive session" when loginctl says there is nothing to lock, and otherwise lists what every attempt said (`lock failed: loginctl: ...; xdg-screensaver: ...`).
2. CPU chart on the device page shows "no samples yet" until the second minute, because CPU needs two samples and heartbeat rows are stored once per minute. Cosmetic.
3. Fixed: the published Android APK was the debug build (cleartext allowed, debuggable, debug-signed). `scripts/build-agents.sh` now publishes only the signed release APK (`make android-release`, docs/OPS.md section 3).
4. Fixed: `disk_full` fired on small `/boot` partitions; `disk_used_pct` now ignores filesystems under 2 GB and the `/boot` family, and the alert names the mount.
5. Fixed: graceful shutdown waited the full 10 s and exited 1 while any agent was parked in a sync long-poll.
