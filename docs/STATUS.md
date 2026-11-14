# Status board

Every component and feature carries one label. Updated whenever something changes state;
`docs/CHECKLIST.md` holds the item-by-item evidence and `docs/screenshots/` the proof.

| Label | Meaning |
|---|---|
| `WORKING` | Built and verified in the cloud build container: a passing test, a passing end-to-end run, or a screenshot in `docs/screenshots/`. |
| `BUILDING` | Being implemented right now. Code may be in the tree but is not yet verified. |
| `NOT VERIFIED` | Built, but cannot be exercised in the build container (needs a real Windows machine or Android phone). Needs a manual check; see the "Manual test" column. |
| `PLANNED` | In the plan (phase 2 or 3), not started. |

Last update: 2026-11-14 (PM1 patch status and install: migrations `0006_device_patches` (device patch columns) and `0007_patch_commands` (the `patch` command kind + action/mode/reboot/ids, rebuilt preserving `shell_sessions`), the agent `PatchCollector` over an apt/Windows-Update `patchManager` behind an injectable executor wired into the sync loop (periodic report + `KindPatch` dispatch), `RecordPatches` called from the sync/enroll handler, the device-detail `patches` sibling, the `POST /api/v1/devices/{id}/patch` endpoint (role-gated, RS2-scoped, audited), and the Updates panel + fleet column — proven by `internal/e2e` `TestPatchEndToEnd` and `scripts/e2e.sh` driving the real Linux agent against a fake `apt-get` on PATH (report with counts incl. a security update on `GET /devices/{id}`, install-security completes `done` with the log and audits `patch.install_requested`, unknown-id install is 400, RS2 404 with no command, android 400 unsupported_on_platform), the `internal/agent` patch tests (fake executor + captured apt/WU fixtures), `internal/store` `TestCreatePatchCommandRoundTrip`/`TestMigration0007PreservesShellSessions`/`TestRecordPatches`, `internal/server` `TestPatchEndpointRejections`, `web/src/lib/patches.test.ts`, plus `go test ./...`, `go vet ./...`, `GOOS=windows go build ./...`, `make web` and `scripts/e2e.sh`; **the Windows Update path is cross-built and parser-unit-tested only, NOT VERIFIED on a real host**. Before that: NS1 alert notifications: migration `0005_notifications` (notify_channels, notify_rules, maintenance_windows, notification_deliveries), the enqueue wired into the alert evaluator, a retrying background dispatcher started in `Run` with net/smtp + net/http senders, the `/api/v1/notify/*` CRUD + delivery log org-scoped like RS2 with write-only secrets, and the Notifications + Delivery-log dashboard views — proven by `internal/e2e` `TestNotificationsDelivery` (fake clock, local fake SMTP, httptest webhooks: fan-out, severity gate, maintenance suppression, failing-sender retry to `failed`, idempotent enqueue across two evaluator ticks) and `TestNotificationsOrgScoping` (RS2 404-not-403 on channel/rule/window/delivery and the global-row rule), the `internal/server` notify tests (`TestDispatch*`, `TestNotifyChannelSecretWriteOnly`, `TestNotifyOrgScoping`, `TestStdSenderEmailAgainstFakeSMTP`), `web/src/lib/notify.test.ts`, plus `go test ./...`, `make web` and `scripts/e2e.sh`, and a live dashboard run with the real Linux agent, an email channel on a local fake SMTP server and a Slack channel on a local listener (Send-test `sent` on both, an `offline` alert `suppressed` by an active maintenance window; screenshots 24 and 25). Before that: RS2 per-technician org scoping: a `user_orgs` table + `users.all_orgs` (migration `0004_user_orgs`), the visible-org set resolved per request and pushed into the store list queries, the 404-not-403 rule at every org/device endpoint, `all_orgs`/`org_ids` on `GET /users`, scoping on `POST /users`, `PUT /users/{id}/orgs`, audit `user.orgs_changed`, and the Users-page "Edit access" dialog — proven by `internal/e2e` `TestOrgScoping`, `internal/server` `TestDashboardOrgScoping`/`TestDashboardUserOrgsAdmin`, `internal/store` `user_orgs_test.go`, `web/src/lib/orgscope.test.ts`, plus `go test ./...`, `make web`, `scripts/e2e.sh` and a live run with two
orgs and a scoped operator (screenshots 22 and 23). Before that: RS1 interactive remote shell, decision D3: the WebSocket relay, the Linux PTY agent, the xterm.js dashboard terminal and the store migration, proven end to end by `internal/e2e` `TestRemoteShellEndToEnd` — real server + real Linux agent + a real browser WebSocket client — plus `go test ./...` and `scripts/e2e.sh`; Windows ConPTY cross-builds but is NOT VERIFIED here. Before that: Android installed-apps inventory `facts.apps`, screenshots 18 and 19; RD1 remote desktop, phase A of D4, screenshots 16 and 17). Server, Linux agent and dashboard verified in a live run (see docs/screenshots/); the rows below marked WORKING without a screenshot are covered by `go test ./...` (internal/store, internal/server, internal/e2e) and `scripts/e2e.sh` ("E2E OK").

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
| Android installed apps inventory (`facts.apps`: label, package, version, system flag; non-system first; cap 1000) | Android agent, Server, Dashboard | `WORKING` (app code `Apps.kt` + `AppInventory.kt`, JVM tests `AppsTest` 7 + `ProtocolTest.syncWithAppsMatchesFixture`; server `TestAppsInventoryStoredAndServed`, `TestAppsInventoryValidation`, fixture `sync_android_apps.json`; dashboard "Installed apps" panel, `web/src/lib/apps.test.ts`, screenshots 18 and 19: 40-app inventory, default view and search filter, lock/ping/message only in Actions) / `NOT VERIFIED` (the PackageManager read on a real phone) |
| Online / offline / pending status | Server, Dashboard | `WORKING` |
| Facts: OS, kernel, CPU, memory, model, serial | Desktop agent | `WORKING` |
| Run command (exec, argv, no shell) | Desktop agent, Dashboard | `WORKING` |
| Run script (sh, bash, PowerShell, cmd) | Desktop agent, Dashboard | `WORKING` (sh/bash: `scripts/e2e.sh` script kind, runner tests) / `NOT VERIFIED` (PowerShell, cmd: needs Windows) |
| Remote lock, managed devices only, audited | All agents, Server, Dashboard | `WORKING` (queue, delivery, audit, role and tenant checks) / `NOT VERIFIED` (the lock itself: needs a real Ubuntu desktop, Windows PC or Android phone; on the headless build host it reports done with "no interactive session", screenshot 07) |
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

## RS1: interactive remote shell over a WebSocket relay (decision D3)

| Part | Component | Label |
|---|---|---|
| Store: `kind shell` accepted (`CreateCommand`, `ClampShellTimeout`), migration `0003_shell_sessions` (kind CHECK + `shell_sessions` table), `CreateShellSession`/`SetShellConnected`/`FinishShellSession`/`GetShellSession` | Store | `WORKING` (`internal/store` shell tests; `go test ./...`) |
| Server relay: `POST /devices/{id}/shell` (role, CSRF, validation order, ticket, audit `shell.opened`), browser leg `GET /shell/{id}/ws` (cookie, Origin, single-use ticket), agent leg `GET /api/agent/v1/shell/{id}` (Bearer, 409 on second agent), verbatim relay with byte counts, 1 MiB transcript, idle/max/agent-connect/shutdown deadlines, `GET /shell/{id}/transcript` | Server | `WORKING` (`internal/server` shell tests incl. `TestShellEndToEnd`, `TestShellBusy`) |
| Protocol: `KindShell`, cols/rows/timeout clamps, control frames (`resize`/`exit`/`status`), close reasons, `ShellSession` block | Protocol | `WORKING` (`internal/protocol` tests) |
| Agent, Linux: PTY via `/dev/ptmx` (`golang.org/x/sys/unix` ioctls), platform-neutral relay behind the `ptySession` interface, single-shell atomic guard, resize + exit frame, output never logged | Desktop agent | `WORKING` (`internal/agent` shell tests; `TestRemoteShellEndToEnd` runs a real bash PTY) |
| Agent, Windows: ConPTY (`CreatePseudoConsole`/`ResizePseudoConsole`, `golang.org/x/sys/windows`) | Desktop agent | `NOT VERIFIED` (`GOOS=windows GOARCH=amd64 go build ./...` passes; no Windows host to run it) |
| Dashboard: Remote shell button, `@xterm/xterm` terminal + fit/resize, status line, Close, Transcript link on finished shell commands, `?mock=1` echo shell, xterm CSS bundled into `app.css` | Dashboard | `WORKING` (`web/src/lib/shell.test.ts`; `make web` typecheck + build; live run with the real Linux agent: logged in, opened the terminal, typed `echo RS1-SHELL-OK` over the WebSocket relay and saw the output, closed, the audit `shell.opened`/`connected`/`closed` and `GET …/transcript` confirm it — `docs/screenshots/20-device-remote-shell-{desktop,phone}.png`, `21-audit-remote-shell-{desktop,phone}.png`) |
| End-to-end: real server + real Linux agent + real browser WebSocket client, `echo RS1OK`, resize, close, command `done`, audit `shell.opened`/`connected`/`closed`, transcript contains the output | All | `WORKING` (`internal/e2e` `TestRemoteShellEndToEnd`) |

## RS2: per-technician org scoping (phase 2)

| Part | Component | Label |
|---|---|---|
| Store: migration `0004_user_orgs` (`user_orgs` table, `users.all_orgs` DEFAULT 1), `VisibleOrgIDs`/`UserCanSeeOrg`/`GetUserOrgs`/`SetUserOrgs` (admin forced all-orgs), `orgInClause` pushed into `ListOrgs`/`ListDevices`/`ListAlerts`/`ListAudit`/`Summary`, `OrgExists`/`GetAlert` for single-resource checks | Store | `WORKING` (`internal/store` `user_orgs_test.go`; `store_test.go` migrationCount 4; `go test ./...`) |
| Server: visible-org set resolved per request (`dashVisibleOrgs`/`dashOrgVisible`), 404-not-403 at every org/device/alert/command/shell endpoint before any side effect, `all_orgs`/`org_ids` on `GET /users`, scoping fields on `POST /users`, `PUT /users/{id}/orgs` (reject scoping an admin, 404 missing user, 400 bad org), audit `user.orgs_changed` | Server | `WORKING` (`internal/server` `TestDashboardOrgScoping`, `TestDashboardUserOrgsAdmin`) |
| Dashboard: Users-page "Edit access" dialog (All-clients toggle + per-client checklist) and create-user control, "Add client" hidden for non-admins, `OrgChecklist` component, `accessOf` helper, `?mock=1` scoped operator | Dashboard | `WORKING` (`web/src/lib/orgscope.test.ts`; `make web` typecheck + build + 38 web tests) |
| End-to-end: real server through httptest, two orgs A/B each with a device, alert and audit history, an operator scoped to A — sees only A in orgs/devices/alerts/audit/summary, 404 with no side effect on every B endpoint, admin `PUT …/orgs` grants B and the operator then sees B | All | `WORKING` (`internal/e2e` `TestOrgScoping`) |
| Screenshots: Edit-access dialog and a scoped operator's reduced Fleet | Dashboard | `WORKING` (live run with two orgs "Acme Dental"/"Riverside Clinic", the real Linux agent on Acme and a curl-enrolled device on Riverside, and an operator scoped to Acme: the admin's Users-page Edit-access dialog with the client checklist in `docs/screenshots/22-user-client-access-{desktop,phone}.png`, and the same operator's Fleet showing only Acme Dental in `23-scoped-operator-fleet-{desktop,phone}.png`; phone `scrollWidth` 400) |

## NS1: alert notifications (phase 2)

| Part | Component | Label |
|---|---|---|
| Store: migration `0005_notifications` (notify_channels, notify_rules, maintenance_windows, notification_deliveries; unique `(alert_id, rule_id, event)` for idempotent enqueue), `CreateChannel`/`GetChannel`/`ListChannels`/`UpdateChannel`/`DeleteChannel`, `CreateRule`/`.../MatchRules`, `CreateWindow`/`ListWindows`/`ActiveMaintenance`, `EnqueueDelivery`/`ClaimPendingDeliveries`/`MarkDeliverySent`/`MarkDeliveryFailed`/`ListDeliveries`, `GetOpenAlert`; secrets stored in the clear, `org_id IN (…)` RS2 filter | Store | `WORKING` (`internal/store` `notify_test.go`; `store_test.go` migrationCount 5) |
| Server: enqueue wired into the evaluator (`open`/`resolve`), dispatcher started in `Run` and stopped on shutdown, net/smtp + net/http senders (STARTTLS, PLAIN auth, HMAC-SHA256 signature, redaction), renderers (email subject/body, Slack `{"text"}`, generic JSON), `/api/v1/notify/*` CRUD + `/test` + delivery log, RS2 404-not-403, write-only secrets (`secret_set`), audit `notify.*` | Server | `WORKING` (`internal/server` `TestDispatch*`, `TestEnqueueIdempotentAtServer`, `TestStdSender*`, `TestRenderers`, `TestRedaction`, `TestNotifyChannelSecretWriteOnly`, `TestNotifyTestSendEnqueues`, `TestNotifyRoles`, `TestNotifyOrgScoping`) |
| Dashboard: Notifications section (Channels, Rules, Maintenance windows with an "active now" badge, Send test), Delivery log (status chips, status/org filters), write-only secret fields, `web/src/lib/notify.ts` pure helpers, `?mock=1` seeded | Dashboard | `WORKING` (`web/src/lib/notify.test.ts`; `make web` typecheck + build; live run with the real Linux agent, an Ops email channel on a local fake SMTP server and an Ops Slack channel on a local listener: the Notifications settings page with both channels, a routing rule and an active-now maintenance window — `docs/screenshots/24-notifications-settings-{desktop,phone}.png` — and the Delivery log with two `sent` test rows and a `suppressed` offline-alert row — `25-notification-deliveries-{desktop,phone}.png`; phone `scrollWidth` 400) |
| End-to-end: real evaluator + production dispatcher (net/smtp + net/http) against a local fake SMTP server and httptest webhooks on a fake clock — opened-alert fan-out (email + Slack both `sent`, SMTP names the device/alert, webhook gets Slack JSON), `min_severity=high` does not fire on a low alert, active maintenance window suppresses (nothing sent) while outside it sends, a failing sender reaches `failed` after `maxAttempts` and enqueue is idempotent across two evaluator ticks; and the RS2 404-not-403 isolation through the real HTTP API | All | `WORKING` (`internal/e2e` `TestNotificationsDelivery`, `TestNotificationsOrgScoping`) |
| An actual email to a real inbox / a post to a real Slack workspace | — | `NOT VERIFIED` (no outbound mail or internet in the build container; the senders are proven against a local fake SMTP listener and httptest, never a real external send) |
| NS2: SMS channel kind and multi-step escalation policies | — | `PLANNED` (schema and the `kind` enum reserve room; no table added) |

## PM1: patch status and install (phase 2)

| Part | Component | Label |
|---|---|---|
| Protocol: `PatchReport`/`PatchUpdate`, the optional `patches` field on enroll/sync, the `patch` command kind (`Action`/`Mode`/`IDs`/`Reboot`), `KindsFor`/`ValidPatch*`/`ClampPatchTimeout`, `Validate`; fixture `sync_patches.json` | Protocol | `WORKING` (`internal/protocol` `types_test.go`) |
| Agent: `patchManager` (apt / Windows Update / none) over an injectable `commandExecutor`, `PatchCollector` (cadence scan + `RunPatch`), wired into the Run loop (periodic report + `KindPatch` dispatch); pure parsers for apt and WU | Agent | `WORKING` (Linux: `internal/agent` patch tests with a fake executor and captured apt/WU fixtures) / `NOT VERIFIED` (Windows Update on a real host) |
| Store: migration `0006_device_patches` (device columns) + `0007_patch_commands` (the `patch` kind + action/mode/reboot/patch_ids_json, rebuilt preserving `shell_sessions`), `RecordPatches`, `CreateCommand`/`ClaimQueuedCommands` patch fields, `store.Command` patch fields | Store | `WORKING` (`internal/store` `TestRecordPatches`, `TestCreatePatchCommandRoundTrip`, `TestMigration0007PreservesShellSessions`; migrationCount 7) |
| Server: sync/enroll `RecordPatches`, device-detail `patches` sibling, `POST /api/v1/devices/{id}/patch` (RS2 404-not-403, revoked, android, shape, ids-subset, offline order; audit `patch.scan_requested`/`patch.install_requested`), route self-registered via `dashboardRoutes` | Server | `WORKING` (`internal/server` `TestPatchEndpointRejections`, `TestUnknownPatchIDs`; device-detail/COMMAND shape tests) |
| Dashboard: Updates panel (last checked, reboot badge, counts, list, Install security/all + Rescan), fleet Updates column + "has pending security" filter, `web/src/lib/patches.ts` pure helpers, `?mock=1` seeded | Dashboard | `WORKING` (`web/src/lib/patches.test.ts`; `make web`) |
| End-to-end: the real Linux agent against a fake `apt-get` on PATH — the report appears on `GET /devices/{id}` with the right counts incl. a security update, install-security queues/installs/completes `done` with the log and audits `patch.install_requested`, unknown-id install is 400, RS2 404 with no command, android 400 | All | `WORKING` (`internal/e2e` `TestPatchEndToEnd`; `scripts/e2e.sh`) |
| Windows Update install/scan on a real Windows host | — | `NOT VERIFIED` (no Windows host in the build container; the COM/PowerShell path is cross-built and the JSON parser is unit-tested against a captured fixture) |

## Later phases

| Feature | Phase | Label |
|---|---|---|
| Interactive remote shell over WebSocket relay | 2 | `WORKING` (Linux, RS1 table above) / `NOT VERIFIED` (Windows ConPTY) |
| Per-technician org scoping | 2 | `WORKING` (RS2 table above) |
| Patch status and install (apt / Windows Update) | 2 | `WORKING` (PM1 table above; Linux) / `NOT VERIFIED` (Windows Update) |
| Software deployment, file transfer | 2 | `PLANNED` |
| Alert notifications: email + Slack/webhook, routing, maintenance windows, delivery log | 2 | `WORKING` (NS1 table above) / `NOT VERIFIED` (a real external send) |
| Notifications: SMS and escalation policies | 2 | `PLANNED` (NS2) |
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

1. Fixed: lock on a Linux host with no desktop session now reports done with "no interactive session" (was `failed`).
2. CPU chart on the device page shows "no samples yet" until the second minute, because CPU needs two samples and heartbeat rows are stored once per minute. Cosmetic.
