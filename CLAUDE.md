# CLAUDE.md — project memory for the MSP platform

Read this first. It is the durable memory for anyone (human or agent) continuing this repository.
The full chronological history of how it was built is in `docs/SESSION-LOG.md`.

## What this is

A remote monitoring and management (RMM) platform for an IT company becoming a managed service
provider (MSP). One central server with a web dashboard; agents for Windows, Linux (Ubuntu) and
Android that report in and take commands. Built from scratch in this repo (`sxi9/msp`), which started
empty.

## Architecture (frozen; do not drift)

- **Server**: one Go binary, `github.com/sxi9/msp`, embedded SQLite (WAL, pure-Go `modernc.org/sqlite`,
  `CGO_ENABLED=0` everywhere), the Preact dashboard embedded via `go:embed`. Listens `:8080`, behind a
  TLS reverse proxy in production. Packages: `cmd/msp-server`, `internal/server`, `internal/store`,
  `internal/protocol` (the shared wire contract).
- **Desktop agent**: the same Go module, one binary cross-compiled for linux/amd64, linux/arm64,
  windows/amd64. `cmd/msp-agent`, `internal/agent`. Only third-party dep is `golang.org/x/sys`.
- **Android agent**: `android/`, Kotlin, dependency-free, foreground service.
- **Dashboard**: `web/`, Preact + TypeScript, bundled by esbuild into `web/dist/app.js`+`app.css`,
  embedded in the server. Pure logic lives in `web/src/lib/*.ts` with `node --test` unit tests.
- **Transport**: agents connect **outbound only** over HTTPS. Commands ride back on the agent's `sync`
  long-poll (`/api/agent/v1/sync`, held up to 25 s). No inbound ports at client sites. Staff use
  `/api/v1/*` (cookie session). Device-token auth (`Bearer dt_`) for the agent API.

## Non-negotiable conventions

- **Go module path** `github.com/sxi9/msp`, go directive `1.24` (do NOT bump; the pinned deps'
  `go 1.26` releases would force a toolchain switch — see `go.mod` notes and `docs/SESSION-LOG.md`).
  Pinned deps: `modernc.org/sqlite v1.45.0`, `golang.org/x/crypto v0.48.0`, `golang.org/x/sys v0.41.0`.
  Added for RS1: `github.com/coder/websocket v1.8.15`. Web: `@xterm/xterm 6.0.0` + `@xterm/addon-fit`.
- **Migrations** are numbered files in `internal/store/migrations/`. Current highest is `0008`; the
  **next is 0009**. Never edit an existing migration. SQLite can't ALTER a CHECK, so adding a command
  kind recreates the `commands` table FK-safely (see 0007/0008 for the pattern that preserves
  `shell_sessions`).
- **Command kinds**: `exec, script, lock, ping, message, shell, patch, deploy`. `KindsFor(platform)`
  gates them (android gets only lock/ping/message). Add new kinds in `internal/protocol` with
  `ValidKind`, `KindSupportedOn`, `Command.Validate`, a fixture, and a `store.CreateCommand` case.
- **Tenant isolation (RS2)**: every org-scoped endpoint checks the caller's visible org set and returns
  **404 not_found** (never 403) for anything outside it, **before any side effect**. Use the existing
  `dashOrgVisible` / visible-org helpers. A scoped operator/viewer must not be able to tell a resource
  in another client exists. This is enforced and tested; keep it that way for every new endpoint.
- **Security defaults**: secrets (SMTP password, webhook secret, RustDesk key, device/session tokens)
  are stored only as needed, **write-only over the API** (shown as `set`/`not set`), never logged.
  Commands use **argv, never a shell**. Files are **sha256-verified before run**. Paths are
  traversal-checked. Webhook URLs are SSRF-guarded. Every staff action is audited.
- **Roles**: `admin` (all clients, manages users/orgs), `operator` (acts, scoped), `viewer`
  (read-only, scoped). Role + CSRF header on every write.
- **Branch**: `main`. Push there.
- **Commit attribution** (current): end messages with
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01NZxfDjZXRbBPEoH3WZzJ8d`.
  (Earlier commits used Claude Fable 5.1 — the model changed mid-session; follow the system reminder
  for the current lines.)

## How features are built here (the method, keep using it)

1. Write a contract spec in `docs/specs/<CODE>-<name>.md` (exact routes, JSON shapes, tables, limits,
   and a "Proof" section listing what the build must demonstrate in-container). Commit it.
2. Run a `Workflow` with stages: scaffold → implement (parallel, disjoint paths) → integrate (wire +
   a real `internal/e2e` test + docs) → review (two adversarial lenses: security + correctness) →
   refute each finding → fix (with a regression test per confirmed finding) → verify (full proof
   commands + labelled screenshots). Commit the result.
3. **Everything is tested in-container with fakes for anything external**: a fake `apt-get` stub on
   PATH (never mutate real packages), a local fake SMTP server, `httptest` webhooks, a local file
   server. No test fetches the internet, sends real mail, or installs real packages.
4. During long workflows, schedule checkpoint commits (`send_later`) so a container restart doesn't
   lose work; workflows resume from cache via `resumeFromRunId`.

## The six proof commands (must pass before any feature is "done")

```
CGO_ENABLED=0 go build ./...
go vet ./...
CGO_ENABLED=0 go test ./... -count=1        # includes internal/e2e
GOOS=windows GOARCH=amd64 go build ./...
make web                                     # npm ci, web unit tests, tsc, esbuild, rebuild binaries
scripts/e2e.sh                               # real binaries, ends "E2E OK"
```

Toolchain in the container: Go 1.24, Node 22, Android SDK at `/opt/android-sdk` (platform 35,
build-tools 35), `apt-get`/`dpkg` present, gofmt must be clean. No Docker daemon (compose is validated
with `docker compose config`). No display (Playwright screenshots run headless, deviceScaleFactor 2).

## What is done vs what needs real hardware

- **Verified end to end on Linux in this container** (passing `internal/e2e` tests): enrollment,
  sync/metrics, commands, lock, remote shell (RS1), org scoping (RS2), notifications (NS1), patching
  (PM1), software deployment (SD1).
- **NOT VERIFIED — needs real hardware** (code complete + cross-built only; listed with manual steps
  in `docs/STATUS.md`): the Windows agent on a PC, the Android app on a phone, an actual desktop
  screen lock, a real RustDesk screen session between two machines, the Docker image build, and the
  Windows Update / Windows installer paths.

## Where things live

- `docs/ROADMAP.md` — the phased checklist (what's done, what's left). **Start here when picking the
  next task.**
- `docs/STATUS.md` — per-component WORKING / NOT VERIFIED labels and the manual tests.
- `docs/GUIDE.md` — the end-user guide (install, enroll, every feature).
- `docs/PROTOCOL.md` — the normative wire + CLI contract. `docs/SECURITY.md` — the threat model.
- `docs/DECISIONS.md` — D1–D5 (incl. RustDesk for remote desktop, remote lock managed-only).
- `docs/specs/` — the per-feature contracts (RS1, RS2, NS1, PM1, SD1). Write the next one here.
- `docs/screenshots/` — labelled proof screenshots (teal banner), numbered; add the next in sequence.
- `docs/SESSION-LOG.md` — the full build history of this project.

## What's next (from the roadmap, phase 2 remainder)

Installed-software inventory on desktops; service/process watchlists + event-log/journal forwarding;
agent self-update; NS2 (SMS channel + multi-step escalation policies — schema already reserves room);
script library + scheduled/triggered automations + bulk actions; reports (uptime, disk trends, patch
compliance, exports); two-factor auth + device-token rotation + API keys; Postgres option; Android
device-owner mode (wipe, app push, kiosk) + QR enrollment + location; monitoring beyond agents
(website/TCP checks, SNMP, network discovery). Then phase 3 (branded RustDesk client, macOS/iOS
agents, SSO/mTLS, ticketing/billing/PSA, syslog, compliance packs, white-labelling).

Highest-value next steps before a real pilot: two-factor auth (before any internet-facing deploy) and
a real-hardware pass on the NOT VERIFIED items.
