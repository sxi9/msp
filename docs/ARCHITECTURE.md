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
  views; no new module (stdlib `net/smtp`/`net/http`), and `internal/agent` is untouched. NS2 (SMS
  as another channel `kind`, multi-step escalation) reuses this shape and adds no table.

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
