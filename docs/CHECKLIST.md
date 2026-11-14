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
- [x] Tenant isolation: a scoped technician cannot see or command another client's devices — now verified **(RS2, section below)**: `internal/e2e` `TestOrgScoping` and `internal/server` `TestDashboardOrgScoping` prove a scoped operator sees only org A and gets a 404 with no side effect on every org-B endpoint. Data ownership is still also tested directly (`TestDashboardOrgsAndTokens` "wrong org is 404", `TestUpsertDeviceRotatesToken` cross-org, `TestDashboardDevicesShape` org_id filter)
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

## RS1: interactive remote shell over a WebSocket relay (added 2026-11-13, decision D3)

Verified in the cloud build container (no display, no Windows host, no Docker daemon) with, in order:
`CGO_ENABLED=0 go build ./...` (exit 0), `go vet ./...` (exit 0), `CGO_ENABLED=0 go test ./... -count=1`
(all packages ok: agent, e2e, protocol, server, store; the previously skipped `TestShellBusy` and
`TestShellEndToEnd` now run and pass, and the new `internal/e2e` `TestRemoteShellEndToEnd` passes,
also under `go test -race`), `GOOS=windows GOARCH=amd64 go build ./...` (exit 0, ConPTY path cross-builds),
`make web` (npm ci, 33 web unit tests incl. `web/src/lib/shell.test.ts`, `tsc --noEmit`, esbuild `app.js`
+ `app.css` 13.8 kB with the xterm stylesheet bundled in, server rebuilt), and `scripts/e2e.sh` ("E2E OK").

- [x] Protocol: `KindShell`, cols/rows/timeout clamps (`ClampShellCols`/`Rows`/`Timeout`), control frames (`resize`/`exit`/`status` via `ControlFrame`/`ParseControlFrame`), close reasons, `ShellSession` block (`internal/protocol` tests)
- [x] Store: `CreateCommand` accepts `kind shell` (shell persisted, `ClampShellTimeout` for the max-duration scale), migration `0003_shell_sessions` (kind CHECK rebuilt to include `shell`, `shell_sessions` table), `CreateShellSession` / `SetShellConnected` / `FinishShellSession` / `GetShellSession` (`internal/store` shell tests)
- [x] Server: `POST /api/v1/devices/{id}/shell` (role, CSRF, order device→revoked→platform→shape→offline→busy→cap, single-use 43-char ticket, audit `shell.opened`), browser leg `GET /api/v1/shell/{id}/ws` (session cookie, Origin==Host, ticket consumed once, 60 s), agent leg `GET /api/agent/v1/shell/{id}` (Bearer `dt_`, kind+status checks, 409 second agent), verbatim relay with `bytes_in`/`bytes_out`, 1 MiB transcript, idle/max/agent-connect/shutdown deadlines, completion on close, `GET /api/v1/shell/{id}/transcript` (`TestShell*` incl. `TestShellEndToEnd`, `TestShellBusy`, origin/ticket/cap/relay tests)
- [x] Command JSON carries the `shell_session` block for shell commands, null until the session row exists (`enrichCommand` on `GET /commands/{id}` and the device command list)
- [x] Agent dispatch: a `kind shell` command is handled outside the two-slot semaphore and guarded by an atomic single-shell flag; `RunShell` posts a result only on its two refuse cases, else the server completes the command on relay close (`internal/agent` shell tests; `agent.go` Run loop)
- [x] Agent Linux PTY: `/dev/ptmx` open/unlock/getptn (`golang.org/x/sys/unix` ioctls, `Setsid`/`Setctty`/`Ctty`), window size, resize frames, exit frame; platform-neutral copy loop behind the `ptySession` interface; shell output never logged (`pty_linux.go`, exercised by `TestRemoteShellEndToEnd` against a real bash)
- [ ] Agent Windows ConPTY (`CreatePseudoConsole`/`ResizePseudoConsole`, `golang.org/x/sys/windows`) — Not verified: cross-builds only (`GOOS=windows GOARCH=amd64 go build ./...`); needs a Windows host (docs/STATUS.md)
- [x] Dashboard: Remote shell button (operator+, hidden for viewer/offline/android), `@xterm/xterm` terminal with fit/resize and a status line, Close, Transcript link on finished shell commands, `?mock=1` echo shell, pure helpers in `web/src/lib/shell.ts` with `node --test`; xterm CSS routed into `dist/app.css` via `styles.css` (`shell.test.ts`, `make web`); live browser run with the real Linux agent: logged in, opened the terminal, ran `echo RS1-SHELL-OK` over the WebSocket relay and saw the output, closed, and the audit `shell.opened`/`connected`/`closed` and `GET …/transcript` confirm it (screenshots 20 and 21)
- [x] CSP: `connect-src 'self'` already permits the same-origin WebSocket; no directive change needed (`middleware.go`; asserted by the e2e CSP check)
- [x] End-to-end proof: `internal/e2e/shell_test.go` `TestRemoteShellEndToEnd` — real server + real Linux agent + a real coder/websocket browser client opens a bash shell, relays `echo RS1OK`, sends a resize, closes, and asserts the command is `done`, the audit has `shell.opened`/`shell.connected`/`shell.closed` and `GET …/transcript` contains `RS1OK` (passes under `-count=3` and `-race`)
- [x] Docs: PROTOCOL (kind, both WS legs, control frames, transcript, audit actions), ARCHITECTURE (relay + dependency rule for `github.com/coder/websocket`), SECURITY (ticket + Origin + single relay + transcript retention + output never logged), GUIDE (Remote shell section), OPS (proxy WebSocket upgrades + long-lived + 20-shell cap), FEATURES, STATUS (WORKING Linux / NOT VERIFIED Windows ConPTY), ROADMAP

## RS2: per-technician org scoping (added 2026-11-13, phase 2)

Verified in the cloud build container (no display) with, in order: `CGO_ENABLED=0 go build ./...`
(exit 0), `go vet ./...` (exit 0), `CGO_ENABLED=0 go test ./... -count=1` (all packages ok: agent,
e2e, protocol, server, store — including the new `internal/e2e` `TestOrgScoping`, `internal/server`
`TestDashboardOrgScoping`/`TestDashboardUserOrgsAdmin` and `internal/store` `user_orgs_test.go`),
`GOOS=windows GOARCH=amd64 go build ./...` (exit 0), `make web` (npm ci, 38 web unit tests incl.
`web/src/lib/orgscope.test.ts`, `tsc --noEmit`, esbuild `app.js` + `app.css`, server rebuilt), and
`scripts/e2e.sh` ("E2E OK").

- [x] Model: migration `0004_user_orgs` — `user_orgs(user_id, org_id)` with cascading FKs and an `org_id` index, `users.all_orgs INTEGER NOT NULL DEFAULT 1` so every existing user keeps full visibility on upgrade; `0001_init.sql` untouched (`store_test.go` migrationCount 4)
- [x] Store helpers (single definition of the visible-org rule): `GetUserOrgs`, `VisibleOrgIDs(ctx,userID) (ids, all)`, `UserCanSeeOrg`, `SetUserOrgs` (admin forced all-orgs, org ids validated, dedup, one transaction), `OrgExists`, `GetAlert`; `orgInClause` fragment pushed into `ListOrgs`/`ListDevices`/`ListAlerts`/`ListAudit`/`Summary` so lists filter in SQL, not in Go (`internal/store` `user_orgs_test.go`)
- [x] Server: visible-org set resolved once per request (`dashVisibleOrgs`), single-resource check `dashOrgVisible` returning `404 not_found` before any side effect, role result or 409 at every org/device/command/shell/transcript/remote-desktop/alert endpoint; agent API unchanged (`TestDashboardOrgScoping` asserts 404 with no new command/alert/audit row in org B)
- [x] Admin management: `GET /users` adds `all_orgs`/`org_ids`, `POST /users` scopes operator/viewer (validates org ids, ignores scoping for admin), `PUT /users/{id}/orgs` (400 when scoping an admin, 404 missing user, 400 bad org id), audit `user.orgs_changed` (`TestDashboardUserOrgsAdmin`)
- [x] Role-change edge: a scoped user promoted to admin becomes all-orgs at once (the rule keys off role); a demoted admin keeps `all_orgs=1` until an admin scopes them — handled by the resolution logic (no role-change endpoint in this codebase), covered by a store test that flips role via SQL
- [x] Dashboard: Users-page "Edit access" dialog (All-clients toggle + per-client checklist, `OrgChecklist` component), the same control on the create-user form, "Add client" hidden for non-admins, `accessOf` helper, `?mock=1` scoped operator (`web/src/lib/orgscope.test.ts`; `make web` typecheck + build)
- [x] End-to-end proof: `internal/e2e/orgscope_test.go` `TestOrgScoping` — real server over httptest, two orgs A/B each with a device, an open alert and audit history, an operator scoped to A: sees only A in `GET /orgs`/`/devices`/`/alerts`/`/audit`/`/summary`; 404 (no new command/alert/audit row, alert still unacked) on `GET /orgs/{B}`, `GET /devices/{Bdev}`, `POST …/commands`, `POST …/shell`, `POST …/lock` (command), `POST …/remote-desktop`, `GET …/transcript`, `POST /alerts/{Balert}/ack`; admin `PUT /users/{id}/orgs` grants B and the operator then sees B; the admin is unaffected
- [x] Screenshots: the Edit-access dialog and a scoped operator's reduced Fleet — produced in a live run (two orgs "Acme Dental" and "Riverside Clinic", the real Linux agent on Acme and a curl-enrolled device on Riverside, an operator `tech@example.com` scoped to Acme only): the admin's Users-page Edit-access dialog with the "All clients" toggle off and the Acme-ticked / Riverside-unticked checklist (`docs/screenshots/22-user-client-access-{desktop,phone}.png`), and the same operator's Fleet listing only Acme Dental (org filter and counts reduced, Riverside and its device absent) (`23-scoped-operator-fleet-{desktop,phone}.png`); the label banners were prepended before capture and phone `scrollWidth` was 400 on both pages
- [x] Docs: PROTOCOL (`all_orgs`/`org_ids`, `PUT /users/{id}/orgs`, the visible-org rule, device-scoped 404-across-orgs, `user.orgs_changed`), SECURITY (two-layer tenant isolation, the 404-not-403 rule, section 11 item 4 done), GUIDE ("Client access per technician"), ARCHITECTURE, FEATURES, STATUS (RS2 table), ROADMAP, CHECKLIST

### Final verification pass (2026-11-13)

Re-run in the cloud build container (Go 1.24, Node 22, no display): `CGO_ENABLED=0 go build ./...` ok, `go vet ./...`
ok, `CGO_ENABLED=0 go test ./... -count=1` ok (agent 3.2 s, e2e 9.95 s incl. `TestOrgScoping`, protocol, server 47.4 s,
store 0.33 s), `GOOS=windows GOARCH=amd64 go build ./...` ok, `make web` (npm ci, 38 web unit tests pass, `tsc --noEmit`,
esbuild `app.js` 418.3 kB + `app.css` 14.2 kB, server rebuilt), `scripts/e2e.sh` ends with "E2E OK" (36 PASS, 0 FAIL).
Then a live run on 127.0.0.1:18110 with a fresh SQLite file: admin, two orgs "Acme Dental" and "Riverside Clinic"
and a token each via the CLI; the real `bin/msp-agent` enrolled on Acme (device `vm`, interval 2 s) and a
curl-enrolled fake device on Riverside (`reception-pc`); an operator `tech@example.com` created scoped to Acme only
(`POST /api/v1/users` with `all_orgs:false, org_ids:[1]`). Playwright (chromium, deviceScaleFactor 2, light theme)
then captured screenshots 22 (admin Users Edit-access dialog) and 23 (the scoped operator's Fleet, showing only
Acme Dental) at 1280 px and 400 px; the operator's org filter listed only "All orgs" and "Acme Dental" and its Fleet
held no Riverside device, confirming the isolation. The server and agent were stopped and the live database deleted.

- [x] Live run confirms a scoped operator sees only their assigned client in the real dashboard: Fleet, org filter and header counts reduced to Acme Dental, Riverside (visible to the admin) absent; admin Edit-access dialog edits the per-client checklist; phone `scrollWidth` 400 on all four pages; database deleted afterwards

## NS1: alert notifications (phase 2)

- [x] Model: migration `0005_notifications` — `notify_channels`, `notify_rules`, `maintenance_windows`, `notification_deliveries`, each with a nullable `org_id` (null = global) and RS2 org indexes; UNIQUE `(alert_id, rule_id, event)` makes enqueue idempotent; `updated_at` doubles as the retry lease; channel `secret` stored in the clear but `json:"-"` (`store_test.go` migrationCount 5)
- [x] Store: channel/rule/window/delivery CRUD + `MatchRules` (org/severity/type/event), `ActiveMaintenance`, `EnqueueDelivery` (ON CONFLICT DO NOTHING), `ClaimPendingDeliveries` (immediate-tx lease), `MarkDeliverySent`/`MarkDeliveryFailed`, `ListDeliveries` with the RS2 `org_id IN (…)` filter, `GetOpenAlert` for the enqueue wiring (`internal/store` `notify_test.go`)
- [x] Enqueue wired into the evaluator: `open()` enqueues on a newly created alert, `resolve()` fetches the row before closing it and enqueues a resolved event; idempotent across two evaluator ticks (`internal/server` `TestEnqueueIdempotentAtServer`, e2e idempotence assertion)
- [x] Dispatcher started in `Run` beside the evaluator/pruner on the jobs context and stopped on shutdown; claims pending rows, renders per kind, sends through the `Sender`, retries with backoff, gives up at `maxAttempts` → `failed` (`TestDispatchFailingSenderReachesFailed`)
- [x] Renderers + real sender: email plain-text subject/body naming the device and alert (`net/smtp`, STARTTLS, PLAIN auth), Slack `{"text"}`, generic JSON, `X-MSP-Signature` HMAC-SHA256; URLs redacted to `scheme://host` (`TestRenderers`, `TestRedaction`, `TestStdSenderEmailAgainstFakeSMTP`, `TestStdSenderWebhookGenericAndSignature`, `TestStdSenderWebhookNon2xxFails`)
- [x] API: `/api/v1/notify/channels|rules|windows|deliveries` + `/channels/{id}/test`, viewer read-only / operator write, CSRF on writes, write-only secret (`secret_set`; omit keeps, `""` rejected, value replaces), audit `notify.*` (`TestNotifyChannelSecretWriteOnly`, `TestNotifyTestSendEnqueues`, `TestNotifyRoles`)
- [x] RS2 scoping on every notify resource: a scoped operator lists only their orgs' rows (no global), gets `404 not_found` with no side effect on a channel/rule/window in another org, sees only their deliveries, and cannot create a global (`org_id` null) row (`403`); admin unaffected; a grant lets them manage the new org (`internal/server` `TestNotifyOrgScoping`, `internal/e2e` `TestNotificationsOrgScoping`)
- [x] Dashboard: Notifications section (Channels, Rules, Maintenance windows with "active now", Send test), Delivery log (status chips, status/org filters), write-only secret fields, `web/src/lib/notify.ts` helpers + `web/src/lib/notify.test.ts`, `?mock=1` seeded (`make web` typecheck + build)
- [x] End-to-end: `internal/e2e/notify_test.go` — real evaluator + production dispatcher (net/smtp + net/http) against a local fake SMTP server and httptest webhooks on a fake clock: an opened alert in org A with an email rule and a Slack rule yields two `sent` deliveries (SMTP names the device + "no sync for", webhook gets `{"text":…}` naming the device); a `min_severity=high` rule does not fire on a low alert; an active maintenance window suppresses (nothing sent) while another org outside a window sends; a failing webhook reaches `failed` after `maxAttempts` and enqueue is idempotent across two ticks; and the RS2 404-not-403 isolation + global-row rule through the real HTTP API
- [x] Live dashboard run: the real Linux agent under **Acme Dental**, an **Ops email** channel on a local fake SMTP server and an **Ops Slack** webhook channel on a local HTTP listener (reached via an `/etc/hosts` name that passes the deterministic webhook SSRF guard), a routing rule "low and up · on open · all types" and an active maintenance window. **Send test** on both channels landed `sent` (the fake SMTP received the message, the listener received the Slack `{"text":…}` post); stopping the agent opened an `offline` alert that the active window **suppressed**. Shown on the Notifications settings page (two channels, the rule, the active-now window; screenshot 24) and the Delivery log (two sent rows + one suppressed row; screenshot 25); phone `scrollWidth` 400 on all four pages; the `/etc/hosts` entry and the live database were removed afterwards
- [ ] A real external send (an email to an inbox, a post to a real Slack workspace) — not verified: no outbound mail or internet in the build container; the senders are proven only against a local fake SMTP listener and httptest
- [x] Docs: PROTOCOL (the `/notify/*` endpoints + shapes + the write-only secret rule + `notify.*` audit actions), SECURITY (channel-secret storage, HMAC signing, maintenance-window suppression is not a security control — section 12), OPS (section 11: configuring SMTP/Slack, the dispatcher, retry/backoff, outbound egress), GUIDE (Notifications under Alerts), ARCHITECTURE (NS1 section), FEATURES, STATUS (NS1 table), ROADMAP (NS1 ticked, SMS/escalation left as NS2), CHECKLIST

### Final verification pass (2026-11-14, NS1)

Re-run in the cloud build container (Go 1.24.7, Node 22.23.4, no display, no outbound mail):
`CGO_ENABLED=0 go build ./...` ok, `go vet ./...` ok, `CGO_ENABLED=0 go test ./... -count=1` ok
(agent 3.21 s, e2e 11.77 s incl. `TestNotificationsDelivery` + `TestNotificationsOrgScoping`,
protocol 0.01 s, server 57.67 s, store 0.60 s), `GOOS=windows GOARCH=amd64 go build ./...` ok,
`make web` (npm ci, 49 web unit tests incl. `web/src/lib/notify.test.ts`, `tsc --noEmit`, esbuild
`app.js` 443.5 kB + `app.css` 14.4 kB, server rebuilt), `scripts/e2e.sh` ends with "E2E OK"
(36 PASS, 0 FAIL). The notification senders are exercised against a local fake SMTP listener and
`httptest` webhooks; a real external send is the one `NOT VERIFIED` item (no egress here).

The first full `go test ./...` of this pass flaked once on the RS1 remote-shell timing test
`TestShellRelayPipesBytesAndCloses` ("shell.closed audit = []", a race between the close audit write
and the assertion under full-package parallelism); it passed three times in isolation and on a clean
full re-run. It is unrelated to NS1.

Live NS1 dashboard run (fresh SQLite on a free port, deleted afterwards; admin / org / token from the
CLI; the real Linux agent at a 2 s interval): an Ops email channel pointed at a local fake SMTP server
and an Ops Slack webhook channel pointed at a local HTTP listener, a routing rule and an active
maintenance window, all created through the API. `POST /notify/channels/{id}/test` on each channel
produced two `sent` deliveries (the fake SMTP received one message `alerts@msp.example ->
ops@acme.example`; the listener received one Slack post); stopping the agent opened an `offline` alert
that the active window recorded as `suppressed`. Captured with Playwright (chromium, deviceScaleFactor
2, light theme) the Notifications settings page (screenshot 24) and the Delivery log (screenshot 25) at
1280 px and 400 px with the label banners prepended; phone `scrollWidth` was 400 on all four. The
`/etc/hosts` helper name and the live database were removed afterwards.

## PM1: patch status and install (phase 2)

- [x] Protocol: `PatchReport`/`PatchUpdate` with the documented json tags, the optional `patches` field on `EnrollRequest`/`SyncRequest` (`omitempty`), the `patch` command kind with `Action`/`Mode`/`IDs`/`Reboot`, `KindsFor` (desktop only — android `patch` is 400 unsupported_on_platform), `ValidPatchManager`/`ValidPatchAction`/`ValidPatchMode`/`ValidPatchReboot`, `ClampPatchTimeout` (0 → 3600, max 14400), `PatchReport.Validate` + `Command.Validate` patch arm; fixture `sync_patches.json` round-tripped (`internal/protocol` `types_test.go`)
- [x] Agent: a `patchManager` interface (`Scan`/`Install`) chosen by platform — `aptManager` (linux), Windows Update manager (windows), `noneManager` otherwise — over an injectable `commandExecutor` (default `os/exec`); `PatchCollector` caches the last report, scans on the first call / every 6 h / after a command, and runs `kind patch` commands; pure parsers `parseAptSimulate` and `parseWindowsUpdates`; the full package list is never logged at info level (`internal/agent` patch tests with a fake executor and captured `testdata/apt-simulate.txt` + `testdata/windows-update.json`)
- [x] Agent integration: the Run loop attaches `PatchCollector.Report` to each sync and dispatches `KindPatch` to `RunPatch`, behind a `newPatchReporter` seam so unit tests use a no-op reporter and never run apt (`internal/agent` Run tests; `agent.go`)
- [x] Store: migration `0006_device_patches` (device columns) + `0007_patch_commands` (the `patch` kind in the CHECK + `action`/`mode`/`reboot`/`patch_ids_json`, rebuilt copying `shell_sessions` out and back so its rows + FK survive the FK-ON transaction); `RecordPatches` (full report + denormalised counts that never understate the agent's totals); `CreateCommand`/`ClaimQueuedCommands` persist and emit the patch fields with the patch timeout scale; `store.Command` exposes them `omitempty` (`internal/store` `TestRecordPatches`, `TestRecordPatchesCountFallback`, `TestCreatePatchCommandRoundTrip`, `TestMigration0007PreservesShellSessions`; migrationCount 7)
- [x] Server: the sync/enroll handlers `Validate` and `RecordPatches` when the request carries `patches`; `handleGetDevice` emits the full report as a sibling `patches` object (device-detail keys `alerts,commands,device,facts,metrics,patches`); the denormalised counts on every DEVICE row; `POST /api/v1/devices/{id}/patch` with the validate → org-visible (RS2 404) → revoked → platform → shape → ids-subset → online order, audit `patch.scan_requested`/`patch.install_requested`, then `Notify`; the route self-registers via `dashboardRoutes` (`internal/server` `TestPatchEndpointRejections`, `TestUnknownPatchIDs`, device-detail/COMMAND shape tests)
- [x] Dashboard: the device **Updates** panel (last checked, reboot badge, pending/security counts, the update list with a security chip, Install security / Install all / Rescan for operator+admin) and the fleet **Updates** column + "has pending security updates" filter, from `web/src/lib/patches.ts` pure helpers with `web/src/lib/patches.test.ts`; `?mock=1` seeds a device with updates incl. a security one and a reboot-required device (`make web`); live-browser proof with the real Linux agent against a fake `apt-get` and a `/var/run/reboot-required` flag — the device page Updates panel (Last checked, the **Reboot required** badge, "3 pending, 2 security", the libssl3/openssl SECURITY + curl list, Install security / Install all / Rescan) in `docs/screenshots/26-device-updates-{desktop,phone}.png` and the Fleet with the amber **Updates** badge "3" and the "Pending security updates" filter in `27-fleet-updates-column-{desktop,phone}.png`; phone `scrollWidth` 400 on both
- [x] End-to-end: `internal/e2e/patch_test.go` and `scripts/e2e.sh` run the real Linux agent against a fake `apt-get` on PATH (never real apt): the agent reports pending updates incl. a security one, visible on `GET /devices/{id}` with pending 3 / security 2 and the full report; `POST …/patch {install,security}` queues a command the agent installs via the fake apt, completing `done` with the log, and `patch.install_requested` is audited; `{install,ids}` with an unknown id is `400` naming it with no command created; an operator scoped to another org gets `404` with no command; an android device is `400 unsupported_on_platform`
- [ ] Windows Update scan/install on a real Windows host — not verified: no Windows host in the build container; the COM/PowerShell path is cross-built (`GOOS=windows go build ./...`) and the JSON parser is unit-tested against a captured fixture
- [x] Docs: PROTOCOL (the `patches` report §3.6, the `patch` kind §5, the endpoint + DEVICE fields + device-detail sibling §6, `patch.*` audit actions §7, the fixture), GUIDE (§10 Patching), OPS (§12 Patching: apt root, Windows Update prereqs, reboots, auto-patching is automations), SECURITY (§13: role-gated, tenant-scoped, audited, ids a subset of pending, no arbitrary OS input), ARCHITECTURE (PM1 section + command-kinds table), FEATURES, STATUS (PM1 table), ROADMAP (patch ticked, Windows NOT VERIFIED), CHECKLIST

### Final verification pass (2026-11-14, PM1)

Re-run in the cloud build container (Go 1.24.7, Node 22, no display): `CGO_ENABLED=0 go build ./...` ok,
`go vet ./...` ok, `CGO_ENABLED=0 go test ./... -count=1` ok (agent 3.14 s, e2e 13.02 s incl.
`TestPatchEndToEnd`, protocol 0.005 s, server 49.49 s, store 0.60 s), `GOOS=windows GOARCH=amd64 go
build ./...` ok, `make web` (56 web unit tests incl. `web/src/lib/patches.test.ts`,
`tsc --noEmit`, esbuild `app.js` 451.5 kB + `app.css` 14.6 kB, server rebuilt), `scripts/e2e.sh` ends
with "E2E OK" (all steps PASS incl. the patch steps against the fake `apt-get`). The Windows
Update path is the one `NOT VERIFIED` item (no Windows host here).

Live run on a free port with a fresh SQLite file (deleted afterwards): admin + org "Acme Dental" +
the real Linux agent `vm` (2 s interval) enrolled, run with a fake `apt-get` on PATH and a
`/var/run/reboot-required` flag (both removed afterwards; no real apt ever ran). The agent's periodic
patch report reached the server on its first sync — `GET /api/v1/devices/{id}` showed `pending_patches`
3, `security_patches` 2, `reboot_required` true, `patches.manager` apt, and the three updates
(libssl3 + openssl from `noble-security` with `security:true`, curl from `noble-updates`). Playwright
(chromium, deviceScaleFactor 2, light theme) captured the device Updates panel and the Fleet Updates
column at 1280 and 400 px (`docs/screenshots/26-device-updates-*`, `27-fleet-updates-column-*`); phone
`scrollWidth` was 400 on both.
