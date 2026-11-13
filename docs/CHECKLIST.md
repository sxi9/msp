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
- [x] Installed apps inventory — `AppInventory.kt` (PackageManager, `QUERY_ALL_PACKAGES` in the manifest) + pure `Apps.kt` (`facts.apps`: label / package / version / system, non-system first then by label, cap 1000 entries and 192 KiB, label fallback to package, missing version `""`): JVM `AppsTest` (7 tests: fixture keys, empty inventory, ordering, cap, field limits in bytes, byte budget, fallbacks) and `ProtocolTest.syncWithAppsMatchesFixture` against `sync_android_apps.json`; server `TestAppsInventoryStoredAndServed` / `TestAppsInventoryValidation`; dashboard panel in screenshots 18 and 19 (android devices enrolled through the real agent API: the fixture facts, then a 40-app inventory with the search filter). Not verified: the PackageManager read on a real phone or emulator
- [ ] Boot receiver restarts service — Not verified: needs a device or emulator
- [ ] Safe command set: ping, message, inventory, lock (device admin) — Not verified on a device; the server side accepts lock and refuses exec for android devices (e2e)
- [x] Unit tests for protocol client pass on JVM (`:app:testDebugUnitTest`: ProtocolTest 8, UrlPolicyTest 5, AppsTest 7, 0 failures — 20 tests, see the installed-apps section below)

## Dashboard
- [x] Login page (screenshot 01)
- [x] Clients list and client page with enrollment token button (screenshots 03, 04)
- [x] Device list with online/offline, OS, last seen, filters (screenshots 02, 13; the org/status/hostname filters are rendered, filtering logic covered by the web unit test `filterDevices by substring, status and org`)
- [x] Device page: metrics, inventory, software, command history, Run command, Lock (screenshots 05–07; Android "Installed apps" panel with count, search and system toggle in screenshots 18 and 19, `web/src/lib/apps.test.ts`). Not verified: a desktop software list, not in phase 1
- [x] Alerts page with acknowledge / resolve (screenshots 08, 11, 12: open then acked; resolve is automatic when the device syncs again, `TestOfflineAlertOpensAndResolves`)
- [x] Audit log page (screenshot 09)
- [x] Command preset library: file loads, endpoint serves it, dashboard uses it, live reload (live run with `MSP_PRESETS=deploy/presets.example.json`: `GET /api/v1/presets` returned `source: file` with the 10 Linux + 10 Windows entries; picking "Failed services" filled argv and timeout and ran on the agent, screenshots 14, 15; an edit to the file showed up on the next request without restart, a broken file kept the last good set and was logged; `TestPresets*`, `web/src/lib/presets.test.ts`)
- [x] Phone width: no horizontal scroll (scrollWidth 400 at a 400 px viewport on all 15 pages)
- [x] Screenshots of every page in docs/screenshots/ (15 pages × desktop and phone, see its README)

## Docs
- [x] README: run, enroll, deploy (the Quickstart commands are the ones the live run used and they worked; deploy points to docs/OPS.md)
- [x] CI workflow: go build/test, gradle assembleDebug (`.github/workflows/ci.yml` runs the same commands that passed here). Not verified: an actual GitHub Actions run

## Android installed apps inventory (added 2026-11-13)

Verified in the same container with: `CGO_ENABLED=0 go build ./...`, `go vet ./...`, `CGO_ENABLED=0 go test ./... -count=1`
(agent, e2e, protocol, server, store ok), `make web` (25 web unit tests, tsc, esbuild, server rebuild), `scripts/e2e.sh`
("E2E OK"), and in `android/` `gradle --no-daemon --max-workers=1 :app:assembleDebug :app:testDebugUnitTest`
(BUILD SUCCESSFUL, APK 877 055 bytes, 20 JVM tests: AppsTest 7, ProtocolTest 8, UrlPolicyTest 5, 0 failures), then a live
run for screenshot 18.

- [x] Protocol: `Facts.Apps []App` (`apps,omitempty`), `App{Package, Label, Version, System}`, validation (<= 1000 entries, package 1..256, label <= 256, version <= 64, detail names the index), fixture `sync_android_apps.json` round-trips on both sides (`TestFixturesRoundTrip`, `TestFixtureSemantics`, `TestFactsAppsValidate`; Android `ProtocolTest.syncWithAppsMatchesFixture`); docs/PROTOCOL.md section 3.2a
- [x] Android app: `AppInventory.kt` reads PackageManager (manifest `QUERY_ALL_PACKAGES` with the reason), pure `Apps.kt` sorts (non-system first, label case-insensitive), caps (1000 entries, 192 KiB), falls back (label -> package, version -> ""), cuts fields to the byte limits; sent inside facts on the existing cadence (`Metrics.facts`); `AppsTest` 6 tests. Not verified: on a real phone (the PackageManager call cannot run on the JVM)
- [x] Server: apps stored verbatim at enroll and sync, served in `GET /api/v1/devices/{id}` facts, untouched by a sync without facts, invalid inventories 400 on both endpoints, a 1000-entry inventory fits the 256 KiB body limit (`TestAppsInventoryStoredAndServed`, `TestAppsInventoryValidation`)
- [x] Dashboard: "Installed apps" card (count, search, system toggle off by default, Label / Package / Version in a scroll box), `apps` excluded from the facts rows, `?mock=1` android device carries apps (`web/src/lib/apps.test.ts`: appsOf, sortApps, filterApps, appsSummary; screenshots 18 and 19, scrollWidth 400 at phone width)

### Final verification pass (2026-11-13)

Re-run in the same container: `CGO_ENABLED=0 go build ./...` ok, `go vet ./...` ok, `CGO_ENABLED=0 go test ./... -count=1` ok
(agent 3.3 s, e2e 7.4 s, protocol, server 27.6 s, store), `GOOS=windows GOARCH=amd64 go build ./...` ok, `make web` (npm ci,
25 web unit tests pass, `tsc --noEmit`, esbuild `app.js` 69.4 kB, server rebuilt), `scripts/e2e.sh` ends with "E2E OK" (36 PASS
steps, no FAIL), and in `android/` `gradle --no-daemon --max-workers=1 :app:assembleDebug :app:testDebugUnitTest` BUILD
SUCCESSFUL (APK 877 055 bytes; the JVM tests re-executed with `--rerun-tasks`: AppsTest 7, ProtocolTest 8, UrlPolicyTest 5,
20 tests, 0 failures). Then a live run on 127.0.0.1:18099 with a fresh SQLite file, admin/org/token bootstrapped through
the CLI, and a fake Android device sent with curl (no phone here): enrollment `pixel-7-reception`, platform android,
arch arm64, Android 14 (34), facts with 40 apps (26 user, 14 system), then one sync with battery 64 % not charging and one
disk `/data`; the database was deleted afterwards.

- [x] Enroll answered 201 and the sync 200 `{"poll_interval_s":60,"commands":[]}`; `GET /api/v1/devices/1` showed status online, `battery_pct` 64, `battery_charging` false, one `/data` disk, `can_run_commands` false and 40 entries in `facts.apps` (14 system)
- [x] Device page: "Installed apps 40 apps (14 system)", 26 rows with the system toggle off (default), 40 with it on (14 marked system), 26 again after unticking; `apps` not rendered as a facts row (facts list: CPUs, CPU, IPs, Kernel, Manufacturer, Memory, Model, Serial); Battery 64 %; `/data` in the disks table (screenshot 18, desktop 1280 px and phone 400 px)
- [x] Search: `chrome` typed into the box leaves one row (Chrome, `com.android.chrome`, 130.0.6723.102), footer "1 of 40 shown"; still 1 with system apps shown (screenshot 19, desktop and phone)
- [x] Actions row for the android device offers exactly Lock device, Ping and Send message (with the message input); no Run command/script console, no "Run on" button and no preset select on the page
- [x] Phone width: `document.documentElement.scrollWidth` 400 on both pages; no browser console errors
- [x] Nothing left behind: server stopped, the live database deleted

## RD1: remote desktop through our own RustDesk relay (added 2026-11-13, D4 phase A)

Verified in the same container with: `CGO_ENABLED=0 go build ./...`, `go vet ./...`, `CGO_ENABLED=0 go test ./... -count=1`
(agent, e2e, protocol, server, store ok), `GOOS=windows GOARCH=amd64 go build ./...` + `GOOS=windows go vet`, `make web`
(20 web unit tests, tsc, esbuild, server rebuild), `scripts/e2e.sh` ("E2E OK", now with the RustDesk steps),
`docker compose -f deploy/docker-compose.yml config`, `bash -n` / `sh -n` on the scripts, `gradle :app:testDebugUnitTest`
(12 JVM tests, fixtures unchanged for Android), and a live run for screenshot 16.

- [x] Compose: `hbbs` + `hbbr` from `rustdesk/rustdesk-server:1.1.16`, host networking, `./rustdesk-data:/root`, `-r <host>` (`docker compose config` validates). Not verified: running them (no Docker daemon here)
- [x] Server: `MSP_RUSTDESK_HOST` + `MSP_RUSTDESK_KEY` / `--rustdesk-host` / `--rustdesk-key`, both or neither (`TestConfigValidateRustdesk`; `msp-server serve --rustdesk-host x` exits 2 with the message)
- [x] `GET /api/v1/rustdesk` and the `rustdesk` block in enroll/sync responses, omitted when disabled (`TestRustdeskInfoAndAgentResponses`; e2e "rustdesk info is enabled", "agent config holds the relay from the enroll response")
- [x] `facts.rustdesk_id` (digits, <= 32) validated, stored on enroll and sync, exposed as `rustdesk_id` in DEVICE JSON (`TestRustdeskIDFromFacts`, `TestRustdeskIDStoredFromFacts`; e2e "device reports rustdesk_id from the stub client" with the real agent and a stub `rustdesk --get-id` on its PATH)
- [x] `POST /api/v1/devices/{id}/remote-desktop`: operator+, CSRF, 404 unknown/revoked, 409 `rustdesk_disabled`, 409 `no_rustdesk_id`, 200 with `rustdesk://connect/<id>`, audit `device.remote_desktop_opened` with user, device and IP (`TestRemoteDesktop`; e2e steps "remote desktop answers rustdesk://connect/<id> (200)" through "audit has exactly one device.remote_desktop_opened")
- [x] Agent: relay kept in `agent.json`, saved only on change (`TestRunSavesRustdeskRelayFromSync`); `rustdesk_id` via `--get-id` with `RustDesk.toml` fallback (`TestRustdeskIDFromCLIAndTOML`)
- [x] Agent: `msp-agent rustdesk setup` against a stub client: RustDesk2.toml `[options]` merge, `--password`, restart hook, `--get-id`, `rustdesk-password` 0600, `--config-string` path, error paths (`TestRustdeskSetup`, `TestMergeRustdeskOptions`)
- [ ] Agent: `rustdesk setup` against the real RustDesk client on Ubuntu and Windows — Not verified: the conventional RustDesk2.toml keys/paths cannot be exercised here (docs/STATUS.md manual test 3 and 4)
- [x] Installers: `--with-rustdesk` / `-WithRustDesk` download from `/dl/`, verify against SHA256SUMS, install silently, run setup, restart the agent; absent asset prints how to publish it (`bash -n`, `sh -n`; the messages reviewed). Not verified: a real install
- [x] `scripts/build-agents.sh --sums-only` regenerates SHA256SUMS over `MSP_AGENT_DIR` without building or downloading (exercised on a scratch directory; `sha256sum -c` passes)
- [x] Dashboard: Remote desktop card (id + Copy, Connect, fallback link, viewer without button, not-configured, not-reported), `?mock=1` and `?mock=1&rustdesk=0` (`web/src/lib/rustdesk.test.ts`; typecheck; screenshots 16, 17)
- [ ] An actual screen session from a technician PC — Not verified: RustDesk cannot run in the build container (docs/STATUS.md manual test 5)
- [x] Docs: OPS section 10, GUIDE section 8, SECURITY section 8, PROTOCOL (fields, endpoints, audit action, fixtures, CLI), DECISIONS D4 phases, RUSTDESK-BRANDING.md, FEATURES, STATUS, README, ARCHITECTURE

## Remote desktop (RustDesk, phase A)

Final verification pass, 2026-11-13, in the cloud build container (no display, no Docker daemon, no RustDesk
client). Commands and their results: `CGO_ENABLED=0 go build ./...` ok, `go vet ./...` ok, `CGO_ENABLED=0 go test ./... -count=1`
ok (agent 3.1 s, e2e 7.4 s, protocol, server 27.0 s, store), `GOOS=windows GOARCH=amd64 go build ./...` ok, `make web`
(npm ci, 21 web unit tests pass, `tsc --noEmit`, esbuild, server rebuilt), `scripts/e2e.sh` ends with "E2E OK" (every step
PASS, including the eight RD1 steps), `docker compose -f deploy/docker-compose.yml config` exit 0, `bash -n deploy/install.sh`
exit 0. Then a live run on 127.0.0.1:18095 with a fresh SQLite file, `MSP_RUSTDESK_HOST=rd.example.com`,
`MSP_RUSTDESK_KEY=EXAMPLEKEY123`, admin/org/token bootstrapped through the CLI, a fake `/root/.config/rustdesk/RustDesk.toml`
with `id = '123456789'`, and the real `bin/msp-agent` enrolled and running with a 2 s interval; the database and the fake
config file were deleted afterwards.

- [x] Server accepts the relay from the environment: `GET /api/v1/rustdesk` -> `{"enabled":true,"host":"rd.example.com","key":"EXAMPLEKEY123"}`
- [x] Agent learns the relay on enroll: `agent.json` holds `"rustdesk_host": "rd.example.com"` right after `msp-agent enroll`
- [x] Agent reports the id from the config-file fallback: with no `rustdesk` binary on PATH the facts reader read `RustDesk.toml`; `GET /api/v1/devices/1` showed `device.rustdesk_id` and `facts.rustdesk_id` = `123456789` within 5 s of the agent starting
- [x] Device page shows the Remote desktop card with the id, Copy, relay `rd.example.com` and the Connect button; "RustDesk id" also appears in the facts list (screenshot 16, desktop 1280 px and phone 400 px, phone `scrollWidth` 400)
- [x] Connect: Playwright clicked the button; `POST /api/v1/devices/1/remote-desktop` returned 200 with `{"url":"rustdesk://connect/123456789","rustdesk_id":"123456789","host":"rd.example.com","key":"EXAMPLEKEY123"}`; the card then showed the `Open rustdesk://connect/123456789 in RustDesk` fallback link and the "if nothing opened" hint (headless Chromium has no handler for `rustdesk://`, the page stayed)
- [x] Audit: `GET /api/v1/audit` lists exactly one `device.remote_desktop_opened`, actor `user` `admin@example.com`, target `device` #1, IP `127.0.0.1`, detail `{"host":"rd.example.com","hostname":"vm","platform":"linux","rustdesk_id":"123456789"}`; the Audit page renders it (screenshot 17, desktop and phone, phone `scrollWidth` 400)
- [x] Nothing left behind: server and agent stopped, the live database and `/root/.config/rustdesk/RustDesk.toml` removed
- [ ] An actual screen session technician -> device — Not verified: RustDesk cannot run in the build container (no display, no client); needs a technician PC with the official client and a device with the client installed (docs/STATUS.md manual test 5)
- [ ] The relay containers `hbbs`/`hbbr` running — Not verified: no Docker daemon here; only `docker compose config` validates the two services (docs/STATUS.md manual test 1)
- [ ] The installers `--with-rustdesk` / `-WithRustDesk` on a real Ubuntu desktop and Windows PC — Not verified: only `bash -n` here and the stub-client tests; needs the published packages and a real machine (docs/STATUS.md manual tests 2 to 4)
