# Architecture

Condensed from `docs/PLAN.md` (phase 1) with AMENDMENT A1 applied. `docs/PROTOCOL.md` is the
wire contract; this file explains the pieces, how they talk and which package may import which.

## Components

```
                 clients' sites (outbound only, D2)                         MSP side
 ┌──────────────────────────────────────────────┐         ┌─────────────────────────────────────┐
 │ Linux / Windows                               │  HTTPS  │ msp-server (one static Go binary)   │
 │   msp-agent (Go, service)                     │ ──────► │   /api/agent/v1  enroll, sync,      │
 │     metrics  /proc, x/sys/windows             │  sync   │                  result             │
 │     runner   exec | script | lock | ping |    │ ◄────── │   /api/v1        dashboard JSON API │
 │              message                          │ commands│   /               embedded SPA      │
 ├──────────────────────────────────────────────┤         │   /dl/            agent downloads   │
 │ Android                                       │  HTTPS  │   jobs            alert evaluator,  │
 │   MSP Agent (Kotlin, foreground service)      │ ──────► │                   lost sweeper,     │
 │     metrics  battery, memory, /data           │  sync   │                   pruner            │
 │     commands lock | ping | message            │ ◄────── │   store           SQLite (WAL)      │
 └──────────────────────────────────────────────┘         └──────────────┬──────────────────────┘
                                                                         │ session cookie + CSRF header
                                                          ┌──────────────┴──────────────────────┐
                                                          │ technician browser: Preact dashboard │
                                                          │ Fleet · Device · Alerts · Orgs ·     │
                                                          │ Audit · Users                        │
                                                          └─────────────────────────────────────┘
```

- **Server** (`cmd/msp-server`, `internal/server`, `internal/store`): Go 1.24, stdlib
  `net/http` routing, `database/sql` + `modernc.org/sqlite` (pure Go, WAL, busy_timeout 5000),
  bcrypt from `golang.org/x/crypto`, `log/slog`, `embed`. One binary serves the agent API, the
  dashboard API, the embedded SPA and `/dl/`.
- **Desktop agent** (`cmd/msp-agent`, `internal/agent`): Go, `golang.org/x/sys` only. Reads
  `/proc` on Linux and the Win32 API on Windows, runs commands without a shell, installs
  itself as a systemd unit or a Windows service.
- **Android agent** (`android/`): Kotlin with zero runtime dependencies, foreground service of
  type `specialUse`, speaks the same protocol by contract (fixtures), supports only the
  built-in kinds lock/ping/message.
- **Dashboard** (`web/`): Preact + TypeScript bundled by esbuild into `web/dist/app.js` +
  `app.css`, embedded via `package web`. `web/dist/index.html` is committed so the Go build
  never needs Node.
- **Ops** (`deploy/`, `scripts/`, `.github/`): Dockerfile, compose, Caddy example, systemd
  unit, installers, cross-compile script, e2e script, CI.

## Data flow

1. **Enrollment**: staff create an enrollment token for an org in the dashboard; the install
   one-liner runs `msp-agent install --server BASE --token et_...`. The agent POSTs
   `/api/agent/v1/enroll`, receives a device token (`dt_...`), writes it to its config file
   (0600) and starts the service.
2. **Sync / long-poll**: every `interval_s` (30 s desktop, 60 s Android) the agent POSTs
   `/api/agent/v1/sync` with metrics and `wait_s` (25 s desktop, 0 Android). The server records
   the heartbeat, claims queued commands for the device (`queued -> sent`) and, when there are
   none and `wait_s > 0`, parks the request on the device's notifier channel. A staff command
   wakes the parked request, so delivery takes under a second while the agent is parked and at
   most one interval otherwise.
3. **Execution**: the agent runs each command (max 2 concurrently; Android sequentially),
   caps output at 64 KiB per stream, kills the process group on timeout and POSTs
   `/api/agent/v1/commands/{id}/result`. The server moves the command to
   `done|failed|timeout` and audits `command.finished` with actor_type=device.
4. **Status**: computed at read time: `pending` (never seen), `online`
   (`now - last_seen_at <= 3*interval_s + 15 s`), else `offline`. The evaluator (every 15 s)
   opens/resolves `offline`, `disk_full`, `memory_high`, `battery_low` alerts and marks
   `sent` commands `lost` after `timeout_s + 5 min`. The pruner deletes heartbeats older than
   `MSP_RETENTION_DAYS` hourly.
5. **Dashboard**: polling only (5 s on Fleet, 1 s while a command runs, 10 s summary). Session
   cookie `SameSite=Strict` + `X-Requested-With: msp` header on every non-GET request.

## Package dependency rules

```
internal/protocol   <- imports: stdlib only
internal/store      <- protocol, database/sql, modernc.org/sqlite
internal/server     <- protocol, store, web, golang.org/x/crypto/bcrypt, github.com/coder/websocket
internal/agent      <- protocol, golang.org/x/sys, github.com/coder/websocket  (never store or server)
internal/e2e        <- server, store, agent, protocol      (tests only)
cmd/msp-server      <- server, store
cmd/msp-agent       <- agent
web                 <- embed only (package web; FS embed.FS)
```

- `protocol <- store <- server`: every SQL statement lives in `internal/store`; the server
  never touches `database/sql`. Postgres in phase 2 is a second store implementation.
- The agent imports `internal/protocol`, the standard library, `golang.org/x/sys` and, from
  RS1 (phase 2), `github.com/coder/websocket` to dial the remote-shell relay. It must never
  import `internal/store` or `internal/server`.
- `internal/server` is one Go package with two owners (WP2 core/agent API, WP3 dashboard
  files). Dashboard files register through `dashboardRoutes` in `init()` and use WP2's helper
  names (`writeJSON`, `writeError`, `readJSON`, `pathInt`, `clientIP`, `sessionAuth`,
  `requireRole`, `csrf`, `userFrom`, `audit`, `notifier`); new dashboard helpers are prefixed
  `dash`.
- `internal/agent` is one package split by build tags: Linux-buildable files (WP4) and
  `//go:build windows` / `//go:build !linux && !windows` files (WP5) implement the same
  unexported names (`Metrics`, `Facts`, `Uptime`, `osVersion`, `machineID`, `Install`,
  `Uninstall`, `IsService`, `RunAsService`, `setSysProcAttr`, `killProcess`, `shellArgv`).
- The Android app shares nothing with Go but the contract: `docs/PROTOCOL.md` and
  `internal/protocol/fixtures`, which both test suites round-trip.
- Phase 1 used only three third-party Go modules (`modernc.org/sqlite`,
  `golang.org/x/crypto`, `golang.org/x/sys`). Phase 2's RS1 adds one, `github.com/coder/websocket`
  (the remote-shell relay on both the server and the agent legs); nothing else may be added.

## Shared contract: internal/protocol

`types.go` is the single definition of every JSON shape (`EnrollRequest`, `EnrollResponse`,
`SyncRequest`, `SyncResponse`, `Command`, `CommandResult`, `Metrics`, `Facts`,
`ErrorResponse`, `PingResponse`), the constants (platforms, kinds, shells, intervals, limits)
and the validation helpers (`Command.Validate`, `KindSupportedOn`, `KindsFor`, `ShellsFor`,
`ClampInterval`, `ClampWait`, `ClampTimeout`). The server validates with it, the agent
dispatches on it, the fixtures are round-tripped through it.

## Command kinds (A1)

| kind    | linux | windows | android | server validation                                   |
|---------|-------|---------|---------|-----------------------------------------------------|
| exec    | yes   | yes     | no      | argv >= 1; android -> 400 unsupported_on_platform   |
| script  | yes   | yes     | no      | shell valid for platform, body; android -> 400      |
| lock    | yes   | yes     | yes     | no fields; extra audit device.lock_requested        |
| ping    | yes   | yes     | yes     | no fields                                           |
| message | yes   | yes     | yes     | text non-empty <= 4096 bytes                        |
| shell   | yes   | yes     | no      | optional shell; timeout = max session (RS1)         |
| patch   | yes   | yes     | no      | action/mode/reboot/ids; android -> 400 (PM1)        |
| deploy  | yes   | yes     | no      | source file (file_id/sha256/dest/run) or package; android -> 400 (SD1) |
| update  | yes   | yes     | no      | version/url/sha256 from the manifest; android -> 400 (AU1) |

Lock is only ever possible for a device enrolled under this server (it needs a valid device
token to receive the command) and visible to the technician (404 otherwise), never by free
text, always audited (D5).

## Security basics (phase 1)

bcrypt cost 12; sha256-at-rest device/enrollment/session tokens; `SameSite=Strict` cookie +
`X-Requested-With: msp` CSRF header; login and enroll rate limits per IP; CSP and the other
security headers; three roles (admin / operator / viewer); the agent refuses `http://` except
on loopback or with `--insecure-http` and has no skip-verify flag; argv-only exec or
script-to-0600-file with process-group kill; 64 KiB output caps; append-only audit with
`actor_type user|device|system` and script sha256. An operator account is root on every
client, so MFA, command signing and IP allowlisting are phase-2 preconditions for scale
(`docs/SECURITY.md`).

## Build

`make build` (static binaries), `make test`, `make race`, `make web` (Node 22), `make agents`
(cross-compile matrix), `make android` / `make android-test` (Gradle), `make e2e`
(`scripts/e2e.sh`), `make compose-check`, `make docker`. CI runs the same targets. See
`README.md` for the quickstart and `docs/OPS.md` for deployment.

## Reserved for later phases (names kept stable)

Phase 2: Postgres store, command ack, token rotation, MFA, per-org alert thresholds with
email/webhooks, agent self-update (remote shell over a WebSocket relay shipped as RS1, below).
Phase 3: Android
device-owner mode; remote desktop phase B (the branded RustDesk client,
`docs/RUSTDESK-BRANDING.md`).

## Remote desktop (RD1, D4 phase A)

Built on top of the pieces above without a new transport: `hbbs`/`hbbr`
(`rustdesk/rustdesk-server:1.1.16`, host networking) run next to the server in the compose
file; the server knows them as `Config.RustdeskHost`/`RustdeskKey` (both or neither) and
tells every agent in the `rustdesk` block of the enroll and sync responses; the desktop
agent keeps them in `agent.json`, reports `facts.rustdesk_id` (read with `rustdesk --get-id`
from the official client installed by the installers' `--with-rustdesk`) and offers
`msp-agent rustdesk setup`, which writes the client's relay options and permanent password
(never sent to the server); the store keeps the id in the reserved `devices.rustdesk_id`
column (no migration); the dashboard's device page shows the id and a Connect button that
POSTs `/api/v1/devices/{id}/remote-desktop` (audit `device.remote_desktop_opened`) and
opens the returned `rustdesk://connect/<id>` in the technician's own client. The screen
session runs client-to-client through hbbs/hbbr; the MSP server is never in that path.
Package rules are unchanged: `internal/agent` shells out to the RustDesk binary and imports
nothing new.

## Remote shell (RS1, D3 phase 2)

An interactive browser terminal over a server-brokered WebSocket relay, desktop only. Unlike
remote desktop it is a new transport inside `msp-server`, not an external one; the MSP server
is on the path and records the whole session.

```
 browser ──ws (cookie+ticket)──► msp-server relay ──ws (Bearer dt_)──► agent ──► PTY/ConPTY ── shell
   xterm.js  binary=keystrokes      counts bytes_in/out,     GET /api/agent/v1   /dev/ptmx (Linux)
             text =resize/exit      records transcript,      /shell/{id}         ConPTY (Windows)
                                    enforces idle/max/cap
```

- **Open**: the dashboard POSTs `/api/v1/devices/{id}/shell`; the server queues a `kind shell`
  command (`internal/store`, the sixth kind), reserves a single-use 60 s browser ticket, audits
  `shell.opened` and wakes the device's parked sync. The command's `timeout_s` is the max session
  duration (default 4 h, max 8 h), clamped with `ClampShellTimeout`.
- **Two legs**: the browser leg (`GET /api/v1/shell/{id}/ws?ticket=`, session cookie, Origin must
  equal Host) and the agent leg (`GET /api/agent/v1/shell/{id}`, `Authorization: Bearer dt_`).
  The relay copies binary frames (raw terminal bytes) verbatim between the legs and passes the
  text control frames through (`resize` browser→agent, `exit` agent→browser, `status`
  server→browser).
- **Server state** (`internal/server/shell.go`, `shell_relay.go`, `dashboard_shell.go`,
  `api_agent_shell.go`): live sessions are kept in RAM keyed by command id under a mutex, with a
  per-`Server` manager held in a package-level registry (no field added to `Server`). The store is
  touched only three times — `CreateShellSession`, `SetShellConnected`, `FinishShellSession` — and
  the agent→browser transcript is buffered in memory (cap 1 MiB) and flushed once on close. The
  relay enforces the four deadlines (agent-never-connected 60 s, idle 30 min, max duration
  `timeout_s`, server shutdown) and completes the command on close (`done` on a clean exit or a
  technician closing a live session, else `failed` with the close reason).
- **Agent** (`internal/agent/shell.go` + the build-tagged `pty_linux.go` / `pty_windows.go` /
  `pty_other.go`): a `kind shell` command is dispatched outside the two-slot command semaphore
  (it can live for hours) and guarded by an atomic single-shell flag. The PTY sits behind a small
  `ptySession` interface so the copy loop is platform-neutral and unit-testable with a fake; the
  agent never writes shell output to its logs. The agent posts a result only for its two refuse
  cases (a shell already running, or a PTY that will not open); otherwise the server completes the
  command when the relay closes.

## Per-technician org scoping (RS2, phase 2)

A second isolation layer on top of the existing org data model, with no new transport and no
change to the agent API. Every piece of data already belongs to one org; RS2 adds, per staff
user, a *visible org set* and enforces it on every read and write.

```
 session user ──► store.VisibleOrgIDs(userID) ──► (ids, all)
                  all = role==admin || users.all_orgs==1       ids = user_orgs rows otherwise
                      │                                             │
   list endpoints ────┘  WHERE org_id IN (ids)           single-resource ──► UserCanSeeOrg ──► 404 if not
   (devices, orgs, alerts, audit, summary)               (device, command, alert, token, org)
```

- **Model**: migration `0004_user_orgs` adds `user_orgs(user_id, org_id)` (cascading FKs) and
  `users.all_orgs INTEGER NOT NULL DEFAULT 1`, so every existing user keeps full visibility on
  upgrade. `0001_init.sql` is never edited. A user is unrestricted when `role == admin` or
  `all_orgs == 1`; otherwise they see exactly the `user_orgs` rows. Admins are always all-orgs and
  cannot be scoped (enforced in the store).
- **One rule, one place** (`internal/store/user_orgs.go`): `VisibleOrgIDs`, `UserCanSeeOrg`,
  `GetUserOrgs`, `SetUserOrgs`. The filter is pushed into the list queries (`orgInClause` → a SQL
  `org_id IN (…)` fragment on `ListOrgs`, `ListDevices`, `ListAlerts`, `ListAudit`, `Summary`), so
  large lists are never filtered in Go after the fact; single-resource loads check the loaded row's
  org against the caller.
- **Server** (`dashboard.go` helpers `dashVisibleOrgs`/`dashOrgVisible`): the caller's set is
  resolved once per request from the session user. The visibility check follows the **404-not-403
  rule** — a scoped user reaching a resource outside their set gets `404 not_found`, resolved
  **before** any side effect, role result or 409, so another org's resources are indistinguishable
  from ones that do not exist. Role (`403`) and visibility (`404`) stay independent checks.
- **Admin management**: `GET /users` exposes `all_orgs`/`org_ids`, `POST /users` can scope an
  operator/viewer at creation, `PUT /users/{id}/orgs` replaces the assignment (audit
  `user.orgs_changed`). A role change is reflected immediately by the resolution logic (a promoted
  scoped user becomes all-orgs; a demoted admin keeps all_orgs until scoped), so no role-change
  endpoint is needed.
- **Package rules unchanged**: this is store queries plus server handlers and a Preact dialog; no
  new dependency, and `internal/agent` is untouched.

## Alert notifications (NS1, phase 2)

Outbound delivery of alerts over email and webhooks, built on the existing evaluator and store
with no new transport and no new dependency (stdlib `net/smtp` and `net/http` only).

```
 evaluator open()/resolve() ──► EnqueueAlertNotifications ──► MatchRules + ActiveMaintenance
   (alerts.go, on a created/                  │                      │
    resolved alert)                           ▼                      ▼
                              notification_deliveries (pending | suppressed), idempotent on
                              (alert_id, rule_id, event)
                                              │
 Run ──► NotifyDispatcher.Run(jobsCtx) ──tick──► ClaimPendingDeliveries ──► render ──► stdSender
   (alongside the evaluator/pruner)               (lease via updated_at)     per kind    net/smtp
   stopped on shutdown                                                                   net/http
                                              │
                                   MarkDeliverySent | MarkDeliveryFailed (backoff, maxAttempts)
```

- **Model** (`internal/store/notify.go`, migration `0005_notifications`): four tables —
  `notify_channels`, `notify_rules`, `maintenance_windows`, `notification_deliveries` — each with
  a nullable `org_id` (null = global) so the RS2 `orgInClause` filter scopes staff visibility. A
  UNIQUE index on `(alert_id, rule_id, event)` makes enqueue idempotent; `updated_at` doubles as
  the dispatcher's retry lease. Channel secrets are stored in the clear (credentials in use) and
  tagged `json:"-"` so they never leave the store; callers expose `SecretSet`.
- **Enqueue** (`internal/server/alerts.go` → `EnqueueAlertNotifications`): the evaluator's
  `open()` (when `OpenAlert` reports created) and `resolve()` (fetching the row before it closes)
  hand the `store.Alert` to the enqueue, which finds matching enabled rules (`MatchRules`:
  org/severity/type/event) and upserts one `pending` delivery per rule — `suppressed` when the
  alert's org is inside an active maintenance window at enqueue time. It never sends.
- **Dispatcher** (`internal/server/notify_dispatch.go`): a background goroutine started in `Run`
  beside the other jobs on the jobs context (stopped on shutdown). Each tick claims a batch of due
  `pending` rows (the claim stamps `updated_at` inside an immediate write tx so two ticks never
  grab the same row), renders the message for the channel kind, sends it through the `Sender`
  interface, and marks `sent` or increments `attempts` with a redacted `last_error`, giving up at
  `maxAttempts`. The `Sender` and the clock (`s.now`) are injectable, so tests drive it with a
  fake clock and a fake sender, or exercise the real `stdSender` against a local fake SMTP server
  and `httptest`. `EvaluateNow`/`DispatchNotificationsNow` expose one synchronous pass each for
  the e2e test.
- **Renderers**: email is plain text (`net/smtp`, STARTTLS and PLAIN auth when configured);
  webhook is Slack (`{"text":…}`) or generic JSON, with an optional `X-MSP-Signature`
  HMAC-SHA256 over the body. URLs are redacted to `scheme://host` before any log or `detail_json`.
- **API + dashboard** (`internal/server/dashboard_notify.go`, wired by `notify_wire.go`): the
  `/api/v1/notify/*` CRUD + `/test` + delivery log, reached through a small `NotifyStore` adapter
  over `*store.Store` so the handler package builds and tests on its own; RS2 404-not-403 via the
  shared `dashVisibleOrgs`/`dashOrgVisible` helpers; write-only secrets; audit `notify.*`. The web
  dashboard adds the Notifications panels and the Delivery log with `web/src/lib/notify.ts` pure
  helpers.
- **Package rules unchanged**: store queries + server handlers + a dispatcher goroutine + Preact
  views; no new module (stdlib `net/smtp`/`net/http`), and `internal/agent` is untouched.

### NS2: SMS and multi-step escalation (phase 2)

NS2 extends this exact shape with no new module, no new dependency and (on the server/dispatcher)
no new goroutine:
- **SMS channel** is a third channel `kind`. Migration `0013_ns2` rebuilds `notify_channels`
  FK-safely (as 0007/0008/0012 do, backing up `notify_rules` first so the `channel_id` FK cannot
  cascade-delete it) to widen the `kind` CHECK to `('email','webhook','sms')`. The production sender
  gains `SendSMS` (Twilio Messages API form + HTTP basic auth, or a generic JSON gateway with a
  bearer token) over the same injectable `Sender` interface; the renderer is a short one-line
  summary with the provider token and any URL redacted, so no tokened link rides out in a text.
- **Escalation policies** are two new tables (`escalation_policies` + `escalation_steps`); a rule
  points at one with `notify_rules.escalation_policy_id` (a plain ADD COLUMN, no FK — `DeletePolicy`
  detaches referencing rules in a transaction). There is **no per-alert scheduler**: due steps are
  computed from the alert's own `opened_at` plus each step's `after_minutes` (`DueEscalations`),
  filtered to alerts still open and unacknowledged. The dispatcher runs `EscalateOnce` first on each
  tick, enqueuing one delivery per channel per due step; `notification_deliveries.step` widens the
  idempotency key to `(alert_id, rule_id, channel_id, event, step)` so a repeated tick never
  double-sends (NS1 rows keep step 0 and the old `(alert, rule, event)` guarantee).
- **API + dashboard**: `/api/v1/notify/policies` CRUD joins the same `NotifyStore` adapter and RS2
  helpers (audit `notify.policy_*`); the rule editor attaches a policy, the channel editor gains the
  SMS kind, and the delivery log shows the step (`web/src/lib/escalation.ts` pure helpers).

## Patch status and install (PM1, phase 2)

Per-device pending-OS-updates status and a technician-initiated install, reusing the facts/sync
channel for the report and the command queue for the install — no new transport, no new dependency.

```
 agent sync loop ──► PatchCollector.Report ──► SyncRequest.patches ──► handleSync/handleEnroll
   (every 6 h, first sync,    (apt / WU behind         (section 3.6)     │
    after a patch command)     an injectable executor)                   ▼
                                                            store.RecordPatches (devices columns +
                                                            patches_json; counts never understate)
 dashboard ──► POST /devices/{id}/patch ──► CreateCommand(kind patch) ──► ClaimQueuedCommands
   (Updates panel)              (store, migration 0007)          │           (emits action/mode/ids/reboot)
                                                                 ▼
                      agent PatchCollector.RunPatch ──► apt/WU install ──► CommandResult (log) ──► done
```

- **Protocol** (`internal/protocol/types.go`): `PatchReport` + `PatchUpdate` (fixture
  `sync_patches.json`), the optional `patches` field on the enroll/sync requests, the `patch`
  command kind with `Action`/`Mode`/`IDs`/`Reboot`, `KindsFor` (desktop only), the `ValidPatch*`
  helpers and `ClampPatchTimeout` (patch's own 14400 s ceiling). `Validate` bounds the report and
  the command shape.
- **Agent** (`internal/agent/patch*.go`): a `patchManager` interface (`Scan`/`Install`) chosen by
  platform — `aptManager` (linux), a Windows Update manager (windows, **NOT VERIFIED on a real
  host**), `noneManager` otherwise — over an injectable `commandExecutor` (default `os/exec`), so
  tests feed a fake apt / captured output and no test touches real packages. `PatchCollector` caches
  the last report, scans on the cadence, and runs `kind patch` commands. The Run loop attaches
  `Report` to each sync and dispatches `KindPatch` to `RunPatch`; the collector is behind a
  `newPatchReporter` seam so unit tests never run apt. Pure parsers (`parseAptSimulate`,
  `parseWindowsUpdates`) are unit-tested against captured fixtures.
- **Store** (`internal/store/patches.go`, `commands.go`, migration `0006_device_patches` +
  `0007_patch_commands`): `RecordPatches` stores the full report and the denormalised
  `pending_patches`/`security_patches`/`reboot_required`/`patches_checked_at`; the 0007 rebuild adds
  `patch` to the commands `kind` CHECK and the `action`/`mode`/`reboot`/`patch_ids_json` columns,
  preserving the `shell_sessions` child across the table rebuild. `CreateCommand`/`ClaimQueuedCommands`
  persist and emit the patch fields; `store.Command` exposes them (`omitempty`).
- **Server** (`internal/server/dashboard_patch.go`, `api_agent.go`, `dashboard_devices.go`): the
  sync/enroll handlers call `RecordPatches` when the request carries `patches`; `handleGetDevice`
  emits the full report as a sibling `patches` object; `POST /devices/{id}/patch` queues the command
  through the existing validate → org-visible (RS2) → revoked → platform → shape → ids-subset →
  online chain, audits `patch.scan_requested`/`patch.install_requested`, and wakes the device. The
  route self-registers via `dashboardRoutes` (like `dashboard_shell.go`), with `RegisterPatch` for a
  hand-assembled mux.
- **Dashboard** (`web/src/lib/patches.ts`, Device/Fleet views): the Updates panel and the fleet
  Updates column + "has pending security" filter, from pure helpers with a `node --test`.
- **Proof**: `internal/e2e/patch_test.go` and `scripts/e2e.sh` drive the real Linux agent against a
  fake `apt-get` on PATH; the Windows Update path is cross-built and parser-unit-tested only.

## Software deployment and file transfer (SD1, phase 2)

Push a file to a desktop device and optionally run it, or install a named package, through the
existing command queue and the agent's existing authenticated channel. No new transport.

```
 dashboard ──► POST /api/v1/files (multipart) ──► files table + <MSP_FILES_DIR>/<id>
 dashboard ──► POST /devices/{id}/deploy ──► CreateCommand(kind deploy) ──► ClaimQueuedCommands
                                              (store, migration 0008)        (emits source/file_id/sha256/dest/run/run_args/package)
 agent sync ──► KindDeploy ──► Deployer.RunDeploy ──► file: GET /api/agent/v1/files/{id}
                                                       (verify size+sha256 BEFORE write, temp+rename, optional argv run)
                                                     ──► package: apt-get -y install <name> (PM1 injectable executor)
```

- **Protocol** (`internal/protocol/types.go`, fixture `command_deploy.json`): `KindDeploy`, the
  deploy fields on `Command`, the `DeploySource*` constants, `ValidDeploySource`/`ValidPackageName`/
  `ValidSHA256Hex`/`ValidDeployDest`, `KindsFor` (desktop only), and the `Command.Validate` deploy
  arm (source required; file needs file_id+sha256+dest; package needs a valid name; run_args only
  with run; reject a non-absolute or `..` dest).
- **Agent** (`internal/agent/deploy.go`): a `Deployer` dispatched from the Run loop
  (`KindDeploy` → `RunDeploy`, beside `KindPatch`). The file source streams the library file through
  `(*Client).downloadFile` (the device token + the Client's TLS/CA transport, a ctx-bounded http
  client so a large download is not cut by the 40 s long-poll timeout), verifies the Content-Length
  and SHA-256 **before** writing, creates the parent 0755, refuses a symlinked parent, writes a temp
  file in the dest directory and renames it into place (0755 when run else 0644), then optionally runs
  dest with run_args via argv (no shell). The package source installs one validated name through the
  same injectable executor PM1 uses (`apt-get -y install`); the Windows installer path is NOT
  VERIFIED and fails cleanly. The deploy run path reuses `runProcess`/`makeCommandDir` from the
  runner; no new agent test seam is needed (the e2e test's fake `apt-get` on PATH drives package
  mode, a real Client drives file mode).
- **Store** (`internal/store/files.go`, `commands.go`, migration `0008_files_and_deploy`): the
  `files` table (`org_id` nullable FK → orgs ON DELETE CASCADE; index `files_org`) and the deploy
  command columns (`source`/`file_id`/`sha256`/`dest`/`run`/`run_args_json`/`package`).
  `CreateFile`/`GetFile`/`ListFiles` (RS2 `org_id IN (…)`, nil = all incl. global)/`DeleteFile`
  persist only the rows — the server owns the bytes on disk and the per-file org check.
  `CreateCommand`/`ClaimQueuedCommands` persist and emit the deploy fields; deploy uses the ordinary
  `ClampTimeout`.
- **Server** (`internal/server/dashboard_files.go`, `api_agent_files.go`, `dashboard_deploy.go`): the
  staff file endpoints (`POST`/`GET`/`DELETE /api/v1/files`, org-scoped like RS2, the upload exempt
  from the dashboard body limit and capped by `MSP_MAX_FILE_MB`), the device-token download
  (`GET /api/agent/v1/files/{id}`, global-or-own-org else 404, `X-MSP-SHA256` + Content-Length), and
  `POST /devices/{id}/deploy` (device 404 → org-visible RS2 404 → revoked → platform → shape →
  file-visibility 404 → online; the server fills the sha256 from the stored file; audit
  `file.uploaded`/`file.deleted`/`deploy.requested`). `MSP_FILES_DIR` is created on `serve` start.
  Routes self-register via `dashboardRoutes`, with `RegisterFiles`/`RegisterAgentFiles`/
  `RegisterDeploy` for a hand-assembled mux.
- **Dashboard** (`web/src/lib/deploy.ts`, the Software page + device Deploy dialog): a per-client file
  library and a two-mode deploy dialog, from pure helpers with a `node --test`.
- **Proof**: `internal/e2e/deploy_test.go` drives the real Linux agent against a local file server and
  the fake `apt-get` on PATH (never the public internet, never a real package); the Windows installer
  path is cross-built only.

## Installed-software inventory (SI1, phase 2)

Report the software installed on each desktop device, show it on the device page, and let a
technician search the whole fleet for a package (licensing and vulnerability hunting). This mirrors
the Android installed-apps inventory (`protocol.App` / `Facts.Apps`): it rides the facts the agent
already sends, with no new transport. A device reports `apps` (Android) or `software` (desktop),
never both.

```
 agent (facts cadence) ──► Facts.Software = SoftwareCollector.Collect()  (dpkg/rpm or uninstall registry; read-only)
 agent ──► enroll/sync facts.software ──► RecordSoftware ──► devices.software_json + software_count + software_checked_at
 dashboard ──► GET /devices/{id} ──► sibling "software" array (full list)
 dashboard ──► GET /api/v1/software?q= ──► SoftwareSearch (scan software_json in Go, RS2 org-scoped)
```

- **Protocol** (`internal/protocol/types.go`, fixture `sync_software.json`): `SoftwareItem{Name,
  Version, Publisher, Source}` and `Facts.Software []SoftwareItem` (`json:"software,omitempty"`); the
  `SoftwareSource*` constants (`dpkg`/`rpm`/`registry`) with `ValidSoftwareSource`; the bounds
  (`MaxSoftware` 2000, name/version/publisher length caps, `MaxSoftwareBytes` 192 KiB) enforced by
  `Facts.Validate` / `SoftwareItem.Validate`.
- **Agent** (`internal/agent/software.go` + per-platform `software_dpkg.go` / `software_windows.go` /
  `software_other.go`): a `SoftwareCollector` over an injectable `softwareLister` (the same executor
  seam PM1 uses). Linux parses `dpkg-query`, falling back to `rpm`; Windows decodes the uninstall
  registry keys. The pure parsers (`parseDpkg`/`parseRpm`/`parseRegistry`) are unit-tested against
  captured fixtures on Linux. `finalizeSoftware` trims/length-bounds each field, sorts by name, and
  caps at the entry and byte budgets; `Collect` returns nil on a host with none so `Facts.Software`
  is omitted. The Run loop builds the collector once and sets `f.Software = swCol.Collect(ctx)` in the
  facts block (and at `Enroll`), beside the metrics collector.
- **Store** (`internal/store/software.go`, migration `0011_device_software`): the `devices` columns
  `software_json`/`software_count`/`software_checked_at`; `RecordSoftware` writes all three (nil list
  stored as `[]`, `ErrNotFound` when the device is gone); `SoftwareSearch(visibleOrgs, query, limit)`
  scans each visible device's `software_json` in Go (a documented phase-1 choice, so a match is
  anchored to a package-name boundary rather than a JSON LIKE), returning `[]SoftwareHit` ordered by
  org/hostname/name, capped at `SoftwareSearchLimit` (500) with a `truncated` flag, org-scoped via
  `orgInClause` for RS2.
- **Server** (`internal/server/dashboard_software.go`): `RecordSoftware` called from the sync/enroll
  handler beside `RecordPatches` (a failure never fails the heartbeat/enroll); the device-detail
  handler emits the full list as the sibling `software` array (`dashJSONArray(dev.SoftwareJSON)`,
  like the PM1 `patches` sibling); and `GET /api/v1/software?q=&org_id=` (viewer+, read), `q` ≥ 2
  chars, an unseen `org_id` a 404 before the search (RS2). The route self-registers via
  `dashboardRoutes`, with `RegisterSoftware` for a hand-assembled mux.
- **Dashboard** (`web/src/lib/software.ts`, the device Installed-software panel + the Software-page
  fleet search): pure filter/sort/source-label helpers with a `node --test`.
- **Proof**: `internal/e2e/software_test.go` drives the real Linux agent against a fake `dpkg-query`
  on PATH (a captured inventory, never the container's own packages, never a mutation): the reported
  software shows on `GET /devices/{id}` with the right count and the full list, `GET
  /api/v1/software?q=openssl` returns the device, a sub-2-char `q` is 400, and an operator scoped to
  another org sees no cross-org rows (and a 404 on an explicit foreign `org_id`). The Windows
  registry path is parser-unit-tested and cross-built only, NOT VERIFIED on a real host.

## Agent self-update (AU1, phase 2)

Roll a new desktop-agent build out through the existing command queue and `/dl/` static server,
with the agent verifying and health-checking the binary before it swaps itself and rolling back on a
bad start. The server never pushes bytes: it advertises a manifest and queues an `update` command;
the agent pulls the binary over its own authenticated channel (reusing the SD1 download pattern).

```
 build ──► scripts/build-agents.sh ──► dist/agents.json + binaries + SHA256SUMS ──► MSP_AGENT_DIR (/dl/)
 server start/reload ──► manifestLibrary.load (mtime/size reload; absent/bad ⇒ self-update off)
 agent sync ──► agentUpdateFor(dev, reported_version) ──► SyncResponse.AgentUpdate (newer, platform-arch, not android/revoked)
 dashboard ──► POST /devices/{id}/update ──► CreateCommand(kind update, version/url/sha256 from manifest) ──► ClaimQueuedCommands
 agent ──► Updater.RunUpdate: download ─► verify sha256 ─► chmod ─► back up <exe>.bak ─► health-check <tmp> version ─► atomic swap ─► post result ─► exit
 agent restart ──► BootstrapRollback (restore <exe>.bak if the installed binary cannot run `version`) ──► FinalizeUpdate after next good sync
```

- **Protocol** (`internal/protocol/types.go`, fixtures `command_update.json`, `sync_response_update.json`):
  `KindUpdate`; `Command.Version`/`Command.URL` (the hash reuses `Command.SHA256`, shared with deploy);
  `AgentUpdate{Version,URL,SHA256}` with `Validate` and `SyncResponse.AgentUpdate`
  (`json:"agent_update,omitempty"`); `ValidAgentVersion` / `ValidUpdateURL`; `KindsFor` (desktop only),
  `KindSupportedOn` and the `Command.Validate` update arm.
- **Agent** (`internal/agent/selfupdate.go` + per-platform `selfupdate_windows.go` /
  `selfupdate_other.go`): an `Updater` with injectable download, health-check executor and
  resolve-executable seams. `RunUpdate` resolves the running exe (refusing a `go run` temp binary),
  downloads beside it via `(*Client).downloadUpdate`/`resolveUpdateURL` (same-host only, token in the
  header, never logged), verifies size + SHA-256 **before** anything, chmods, copies the current exe to
  `<exe>.bak`, health-checks `<tmp> version` against the target, then atomically swaps (Unix rename;
  Windows rename-aside). `BootstrapRollback` restores the backup at startup when the installed binary
  fails its `version` check; `FinalizeUpdate` clears the backup after the next good sync. The Run loop
  builds the updater once, runs `BootstrapRollback` before the loop, dispatches `KindUpdate` to
  `RunUpdate`, exits on `ShouldRestart` so the service restarts into the new binary, finalizes once, and
  auto-applies an advertised update when `cfg.AutoUpdateEnabled()` (config `auto_update`,
  `--no-auto-update`). The Windows swap path is cross-built and unit-tested on Linux, NOT VERIFIED on a
  real host.
- **Store** (`internal/store/commands.go`, migration `0012_update_command`): the `update` kind added to
  the `commands` kind CHECK and the `version`/`url` columns, rebuilt FK-safely preserving
  `shell_sessions` (the 0007/0008 pattern); `CreateCommand`/`ClaimQueuedCommands` persist and emit the
  update fields; `store.Command`/`CreateCommandParams` gain `Version`/`URL`.
- **Server** (`internal/server/manifest.go`, `dashboard_update.go`): `manifestLibrary` loads/reloads
  `MSP_AGENT_DIR/agents.json` (mtime/size, last-good kept on a bad reload, absent ⇒ off), wired into the
  `Server` struct (New/Run). `agentUpdateFor` attaches `AgentUpdate` to the sync response;
  `POST /api/v1/devices/{id}/update` (admin|operator; device 404 → org-visible RS2 404 → revoked →
  platform → `no_update_available` 409 → offline 409) fills the command from the manifest and audits
  `agent.update_requested`; `GET /api/v1/agent-release` serves the manifest for the dashboard. The routes
  self-register via `dashboardRoutes` (`RegisterUpdate` for a hand-assembled mux).
- **Build** (`scripts/build-agents.sh`): after the cross-compile it writes `dist/agents.json` with the
  real SHA-256s of the three desktop binaries, alongside `SHA256SUMS`, copied into `MSP_AGENT_DIR`.
- **Dashboard** (`web/src/lib/agentver.ts`, the device Update action + fleet agent-version column and
  outdated filter + released-version banner): pure version-compare/outdated/manifest helpers with a
  `node --test`.
- **Proof**: `internal/e2e/selfupdate_test.go` drives the real Linux agent with the swap targeting a
  temp exe (never the real test binary): a published fake "new" binary is downloaded over `/dl/`,
  hash-verified, health-checked and swapped (`done`, old→new, `<exe>.bak` kept, `agent.update_requested`
  audited); a SHA-256 mismatch and a health-check failure each fail with no swap and no dangling backup;
  the bootstrap rollback restores the backup over a broken binary; an RS2 operator scoped elsewhere gets
  404, android 400, a device already on the released version 409 `no_update_available`, and a sync
  carries `agent_update`. Plus `internal/agent/selfupdate_test.go`, `internal/store/update_test.go`,
  `internal/server/dashboard_update_test.go`, `web/src/lib/agentver.test.ts`.

## Two-factor authentication (TF1, phase 2)

A TOTP (RFC 6238) second factor for staff logins, with one-time recovery codes and an admin reset
for lockouts. No new dependency: the TOTP primitive is pure Go stdlib (`crypto/hmac`, `crypto/sha1`,
`encoding/base32`).

- **TOTP primitive** (`internal/server/totp.go`): HMAC-SHA1, 6 digits, 30 s step, ±1-step skew,
  constant-time compare; `newTOTPSecret` (20 random bytes base32), `totpNow`/`totpVerify` and
  `otpauthURI`. Unit-tested against the RFC 6238 Appendix B vectors (`totp_test.go`).
- **Model** (`internal/store/totp_store.go`, migration `0009_two_factor`): three columns on `users`
  (`totp_secret` write-only, `totp_enabled`, `totp_confirmed_at`) and a `recovery_codes` table
  (sha256-hashed, single-use, cascading FK). `SetTOTPSecret`/`EnableTOTP`/`DisableTOTP`/
  `ReplaceRecoveryCodes`/`ConsumeRecoveryCode` (atomic)/`TOTPStatus`; the user read exposes
  `totp_enabled` but never the secret.
- **Server** (`internal/server/auth.go`, `auth_totp.go`): the two-step login (`POST /api/v1/login`
  returns `{totp_required:true}` + a short-lived pending cookie when 2FA is on; `POST
  /api/v1/login/totp` verifies a TOTP or recovery code and mints the session), the self-service
  enrol/manage endpoints (`/me/totp/setup|confirm|disable|recovery/regenerate`), the `me` fields and
  the admin reset (`POST /api/v1/users/{id}/totp/reset`). The **pending-login token** is kept in an
  in-memory sha256-keyed store (`pendingTOTPStore`, 5-min TTL) — sufficient for the single-process,
  single-SQLite deployment and keeping migration 0009 to the three columns + one table. Routes
  self-register via `dashboardRoutes` (`registerTOTP`). Both login steps and confirm/disable/
  regenerate share the per-IP `loginLimit`.
- **Dashboard** (`web/src/lib/totp.ts` + `web/src/lib/qr.ts`, the Account 2FA section, the Login
  second step, the Users-page badge/reset): pure helpers with `node --test`; the QR is rendered in
  the browser from the pinned `qrcode-generator@1.4.4`, with the secret always shown as text too.
- **Proof**: `internal/server/auth_totp_test.go` and `internal/e2e/twofactor_test.go` prove the full
  flow (enrol, two-step login, wrong/stale/replayed codes, recovery single-use, disable, admin reset,
  rate limiting, RFC vectors, non-2FA login unchanged); everything is deterministic (codes computed
  in-test), no authenticator app or external call.

## Script library, bulk jobs and automations (AUT1, phase 2)

Reusable, scalable operations built entirely on the existing primitives — no new command kind, no
agent change, no new process. Migration `0014_automations` adds four pieces, all additive:

- `scripts` — the DB-backed, editable command library (distinct from the static `presets.json`
  console presets): `org_id` NULL = a global script, kind `exec` (argv) or `script` (shell + body),
  `platform` gating (`linux`/`windows`/`any`). `internal/store/scripts.go` is the CRUD;
  `internal/server/dashboard_scripts.go` the `/api/v1/scripts` endpoints.
- `jobs` — one fan-out of a script across a device set. A job stores only `origin`
  (`manual`/`schedule`/`trigger`), `script_id`, `automation_id` and `total`; progress is **derived**
  at read time by counting the job's commands by status (`JobProgress`). `internal/store/jobs.go`;
  `CreateCommandsForDevices` reuses `CreateCommand` with a `job_id`, skipping a device whose platform
  cannot run the script.
- `commands.job_id` — a plain `ADD COLUMN` (no CHECK change) tagging each command a fan-out creates.
  The agent wire COMMAND is unchanged; only the dashboard COMMAND JSON gains `job_id` (omitempty).
- `automations` — a saved rule that produces jobs: `trigger schedule` (`schedule_json`
  `{every_minutes}`) or `trigger alert` (`alert_json` `{alert_type?, min_severity?}`), with a
  `target_json` device filter resolved at run time. `internal/store/automations.go`;
  `internal/server/dashboard_automations.go` the CRUD; `internal/server/automations_run.go` the
  execution.

Two execution entry points, wired into the existing loops rather than a new one:

- `RunScheduledOnce` is a tick in the server jobs loop (`internal/server/jobs.go`, every 30 s,
  beside the evaluator/pruner), using `s.now()` so the fake clock drives it. It runs each due
  schedule automation, resolving the target against the automation's org scope, and advances
  `next_run_at` from `now` (missed ticks never stack).
- `TriggerAutomationsForAlert` is called from the alert-open hook (`internal/server/alerts.go`
  `s.open`, the same `OpenAlert`-returned-true path that enqueues notifications): it runs each
  matching alert automation on the alerting device, gated by a per-automation-per-device cooldown.

Target resolution funnels through one helper (`resolveTargetDevices`) and one RS2 gate
(`validateTargetForCaller`) shared by the bulk-run endpoint and both automation paths, so a target can
never reach a device outside the caller's / automation's visible org set. Everything is org-scoped like
RS2 and audited (`script.*`, `job.created`, `automation.*`). Proven end to end by
`internal/e2e/automations_test.go` (fake clock + the real Linux agent).
