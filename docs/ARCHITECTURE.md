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
internal/server     <- protocol, store, web, golang.org/x/crypto/bcrypt
internal/agent      <- protocol, golang.org/x/sys          (never store or server)
internal/e2e        <- server, store, agent, protocol      (tests only)
cmd/msp-server      <- server, store
cmd/msp-agent       <- agent
web                 <- embed only (package web; FS embed.FS)
```

- `protocol <- store <- server`: every SQL statement lives in `internal/store`; the server
  never touches `database/sql`. Postgres in phase 2 is a second store implementation.
- The agent imports only `internal/protocol`, the standard library and `golang.org/x/sys`.
  It must never import `internal/store` or `internal/server`.
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
- Only three third-party Go modules exist in the whole build (`modernc.org/sqlite`,
  `golang.org/x/crypto`, `golang.org/x/sys`); nothing else may be added in phase 1.

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

Phase 2: remote shell over a WebSocket relay, Postgres store, command ack, token rotation,
MFA, per-org alert thresholds with email/webhooks, agent self-update. Phase 3: remote desktop
through a self-hosted branded RustDesk relay (D4); `devices.rustdesk_id` is already a nullable
TEXT column so no migration is needed then; Android device-owner mode.
