# Phase 1 checklist

Verified on 2026-11-13 in the cloud build container (Go 1.24.7, Node 22.23.4, Gradle 8.14.3,
JDK in /usr/bin/java, Playwright 1.63.0 chromium). Commands run, in order: `CGO_ENABLED=0 go build ./...`,
`go vet ./...`, `CGO_ENABLED=0 go test ./... -count=1 -p 4` (all packages ok), `GOOS=windows GOARCH=amd64` and
`GOOS=linux GOARCH=arm64` builds of both binaries, `make web` (npm ci, 11 web unit tests, tsc, esbuild, server
rebuild), `scripts/e2e.sh` ("E2E OK"), `docker compose -f deploy/docker-compose.yml config`, and in `android/`
`gradle --no-daemon --console=plain --max-workers=1 clean :app:assembleDebug :app:testDebugUnitTest`
(APK 845 782 bytes, 12 JVM tests). Then a live run on 127.0.0.1:18091 with a fresh SQLite file, CLI bootstrap,
the real Linux agent at a 2 s interval and Playwright screenshots of every page at 1280 px and 400 px
(`docs/screenshots/`). Items marked "Not verified" could not be exercised here and stay unticked.

Updated as each item is built and verified in this environment. A ticked item has a passing
test or a screenshot in `docs/screenshots/`. Unticked items are not done.

## Server
- [x] Builds: `go build ./cmd/server` (the package is `./cmd/msp-server`; `go build ./...` and `make build` pass)
- [x] Unit tests pass: `go test ./...` (internal/agent, e2e, protocol, server, store all ok)
- [x] Starts with embedded SQLite, creates schema on first run (live run on a fresh `msp.db`; `TestOpenMigratesAndReopens`)
- [x] Admin user bootstrapped from env on first run — verified as the CLI bootstrap `msp-server user add --role admin --password-stdin` before `serve`; there is no env-based bootstrap in this design
- [x] Login / logout / session cookie (screenshot 01, live login; `TestLoginMeLogout`, `TestSessions`)
- [x] Roles enforced: admin, technician, read-only — the roles are named admin / operator / viewer; e2e "viewer cannot queue commands (403)", `TestDashboardAuthAndRoles`, `TestRequireRoleAndCSRF`
- [x] Create client org (CLI `org add` in the live run and e2e; screenshot 03; `TestDashboardOrgsAndTokens`)
- [x] Generate enrollment token (expiring, org-scoped) (CLI `token new --ttl 24h --max-uses 5` and the dashboard form, screenshot 04; `TestEnrollmentTokens`)
- [x] Agent enroll endpoint issues device token (live enroll "enrolled as device 1"; `TestEnroll`)
- [x] Heartbeat stores metrics and marks device online (screenshots 02, 05; `TestSyncMarksOnlineAndStoresSample`)
- [x] Offline sweeper marks device offline after 3 min — the window is 3 × interval + 15 s (`store.StatusOf`), i.e. 3 min 15 s at the default 60 s interval; verified at the 2 s interval (21 s) in e2e and in the live run (screenshot 13)
- [x] Inventory upload stored and shown — facts (OS, kernel, CPU, memory, IPs, model, serial) and disks are shown (screenshot 05). Not verified: installed software; it is out of phase 1 scope (docs/PLAN.md) and not collected
- [x] Command queue: create, deliver on heartbeat, result stored (screenshot 06, e2e `uname -a` round trip 13 ms; `TestCommandsLifecycle`, `TestSyncClaimsExactlyOnce`)
- [x] Built-in `lock` command type, allow-listed, technician+ role only (screenshot 07, audit `device.lock_requested`; e2e "lock on android is accepted (201)", viewer 403 on commands; `TestDashboardCommandValidation`). The lock itself on this headless host reports "no interactive session", see Desktop agent
- [ ] Tenant isolation: org A cannot see or command org B devices (test) — Not verified as written: phase 1 has no org-scoped logins (all staff see every org, docs/PLAN.md). What is tested is that tokens and devices are org-scoped (`TestDashboardOrgsAndTokens` "wrong org is 404", `TestUpsertDeviceRotatesToken` cross-org, `TestDashboardDevicesShape` org_id filter)
- [x] Alert rules evaluated on heartbeat; alert opens, acks, resolves, auto-resolves (open + ack in the live run, screenshots 11–12; `TestOfflineAlertOpensAndResolves` covers the auto-resolve when the device syncs again, `TestThresholdAlerts`, `TestDashboardAlertsAndAudit`)
- [x] Audit log records every user action (screenshot 09; e2e "audit trail (user vs device actors)"; `TestAuditAndSummary`)
- [x] Metrics retention job (`TestPruneOnce`; `MSP_RETENTION_DAYS`) — not exercised in the live run
- [x] docker-compose.yml and TLS reverse proxy example (`deploy/docker-compose.yml`, `deploy/Caddyfile.example`; `docker compose config` validates and `bash -n` passes on the scripts). Not verified: the image build and a real TLS deployment (no Docker daemon here)

## Desktop agent (Go)
- [x] Builds for linux/amd64, linux/arm64, windows/amd64 (`CGO_ENABLED=0 GOOS=… GOARCH=… go build ./cmd/msp-agent` all exit 0; `GOOS=windows go vet` clean)
- [x] `enroll` subcommand stores config (live run wrote `agent.json`; agent config tests)
- [x] Heartbeat with CPU, memory, disk, uptime, IPs (all shown on the device page, screenshot 05)
- [x] Inventory: OS, version, hardware, installed software — OS, version, kernel, CPU, memory, model/serial verified (screenshot 05). Not verified: installed software, not collected in phase 1 (docs/PLAN.md)
- [x] Runs commands without shell, captures output, enforces timeout (e2e "exec argv is literal (no shell)", "exec timeout"; runner tests)
- [ ] Lock: Windows LockWorkStation, Linux loginctl — Linux: the lock command was delivered and executed, `loginctl lock-sessions` ran and reported "no interactive session" because the build container has no desktop session (screenshot 07; `TestExecuteLock*`). Not verified: an actual screen lock on a Linux desktop, and Windows LockWorkStation (needs a Windows PC)
- [x] Reconnect with backoff (`TestRunBackoffOn500`, `TestRunBackoffCapsAt300s`) — not exercised in the live run
- [x] systemd unit install (`TestUnitFileMatchesDeploy`, `TestInstallAndUninstall` against a temp root). Not verified: an install on a real systemd host (the container is not booted with systemd)
- [ ] Windows service install command — Not verified: cross-compiles and vets only; needs a Windows PC
- [x] End-to-end on Linux in this container: enroll → appears online → command runs → output visible (`scripts/e2e.sh` E2E OK and screenshots 02, 05, 06)

## Android agent (Kotlin)
- [x] `gradle assembleDebug` produces APK in this container (`android/app/build/outputs/apk/debug/app-debug.apk`, 845 782 bytes, from a clean build)
- [ ] Enrollment screen (paste token + server URL) — Not verified: needs a device or emulator
- [ ] Foreground service heartbeat: battery, storage, RAM, network type, OS version — Not verified: needs a device or emulator
- [ ] Installed apps inventory — Not verified: needs a device or emulator (and no package-list code was found in `android/app/src/main`)
- [ ] Boot receiver restarts service — Not verified: needs a device or emulator
- [ ] Safe command set: ping, message, inventory, lock (device admin) — Not verified on a device; the server side accepts lock and refuses exec for android devices (e2e)
- [x] Unit tests for protocol client pass on JVM (`:app:testDebugUnitTest`: ProtocolTest 7, UrlPolicyTest 5, 0 failures)

## Dashboard
- [x] Login page (screenshot 01)
- [x] Clients list and client page with enrollment token button (screenshots 03, 04)
- [x] Device list with online/offline, OS, last seen, filters (screenshots 02, 13; the org/status/hostname filters are rendered, filtering logic covered by the web unit test `filterDevices by substring, status and org`)
- [x] Device page: metrics, inventory, software, command history, Run command, Lock (screenshots 05–07). Not verified: software list, not in phase 1
- [x] Alerts page with acknowledge / resolve (screenshots 08, 11, 12: open then acked; resolve is automatic when the device syncs again, `TestOfflineAlertOpensAndResolves`)
- [x] Audit log page (screenshot 09)
- [x] Command preset library: file loads, endpoint serves it, dashboard uses it, live reload (live run with `MSP_PRESETS=deploy/presets.example.json`: `GET /api/v1/presets` returned `source: file` with the 10 Linux + 10 Windows entries; picking "Failed services" filled argv and timeout and ran on the agent, screenshots 14, 15; an edit to the file showed up on the next request without restart, a broken file kept the last good set and was logged; `TestPresets*`, `web/src/lib/presets.test.ts`)
- [x] Phone width: no horizontal scroll (scrollWidth 400 at a 400 px viewport on all 15 pages)
- [x] Screenshots of every page in docs/screenshots/ (15 pages × desktop and phone, see its README)

## Docs
- [x] README: run, enroll, deploy (the Quickstart commands are the ones the live run used and they worked; deploy points to docs/OPS.md)
- [x] CI workflow: go build/test, gradle assembleDebug (`.github/workflows/ci.yml` runs the same commands that passed here). Not verified: an actual GitHub Actions run
