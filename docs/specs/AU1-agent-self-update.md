# AU1 — Agent self-update (phase 2)

Let the server roll a new agent version out to desktops: the agent downloads the new binary, verifies
its hash, swaps itself atomically, restarts, and **rolls back automatically if the new binary fails to
start**. Self-update is the highest-risk feature in the platform — a bad push could brick every managed
device — so safety (hash verify, kept backup, health-checked rollback, opt-out) is the point, not just
the swap. Linux is verified in-container with a fake "new" binary; the Windows service-swap path is
written, cross-built and unit-tested against the swap logic, marked NOT VERIFIED until a real host.

## Release manifest (server)

- The MSP publishes agent binaries to `MSP_AGENT_DIR` (served at `/dl/`) together with a manifest
  `agents.json`:
  ```
  {"version":"0.2.0","files":{
     "linux-amd64":{"name":"msp-agent-linux-amd64","sha256":"<hex>"},
     "linux-arm64":{"name":"msp-agent-linux-arm64","sha256":"<hex>"},
     "windows-amd64":{"name":"msp-agent-windows-amd64.exe","sha256":"<hex>"}}}
  ```
- The server loads `agents.json` from `MSP_AGENT_DIR` on start and re-reads it when its mtime changes
  (like the preset library). If absent, self-update is simply off (no update ever advertised). Store the
  parsed manifest in memory; expose the current released version in the dashboard.
- `scripts/build-agents.sh` writes `agents.json` with the real sha256s alongside the binaries and
  `SHA256SUMS` (extend the existing script).

## What the server advertises

- The sync response gains an optional `agent_update` object, sent only when the manifest has an entry
  for the device's `platform-arch` **and** that entry's version differs from the device's reported
  `agent_version`:
  ```
  "agent_update":{"version":"0.2.0","url":"/dl/msp-agent-linux-amd64","sha256":"<hex>"}
  ```
  `url` is relative to the server base. Android never gets it.
- An agent applies an advertised update automatically **only when auto-update is enabled** for it
  (agent config `auto_update`, default true; `msp-agent` flag `--no-auto-update` / config to disable per
  device). Regardless of auto-update, a dashboard `update` command forces it now.

## Command kind `update`

Desktop only (`KindsFor` adds `update` for linux/windows; android → 400 unsupported_on_platform;
revoked → unsupported_on_device). Agent-facing JSON `{"id":N,"kind":"update","version":"0.2.0",
"url":"/dl/...","sha256":"<hex>","timeout_s":600}` — the server fills version/url/sha256 from the
manifest for the device's platform at queue time (the technician does not type them). `protocol` adds
`KindUpdate` + fields, `ValidKind`/`KindSupportedOn`/`Command.Validate`, a fixture, a
`store.CreateCommand` case. Migration `0012_update_command.sql` recreates `commands` to add `update` to
the kind CHECK (same FK-safe pattern as 0007/0008 that preserves `shell_sessions`). Reuses the queue,
result and history; the result holds the update log (old→new version, outcome, rollback if any).

Dashboard open: `POST /api/v1/devices/{id}/update` (admin|operator, CSRF, RS2 device, desktop, not
revoked, online else 409; 409 `no_update_available` when the manifest has nothing newer for the
device's platform). Audit `agent.update_requested` (from_version, to_version).

## Agent self-update (`internal/agent/selfupdate.go`)

The swap must be safe and reversible:
1. Resolve the running executable path (`os.Executable`, resolve symlinks). Refuse if it is not a
   regular file we can replace (e.g. running from `go run`) → result "cannot self-update in this
   environment".
2. Download `url` with the device token (reuse the SD1 streaming download helper) to a temp file beside
   the executable; verify size and **sha256 before** doing anything else. Mismatch → fail, no change.
3. `chmod 0755`; **back up** the current executable to `<exe>.bak` (copy, not move, so the running image
   is untouched).
4. **Health-check the new binary** before committing: run `<tempfile> version` (or `--version`) with a
   short timeout and confirm it prints the expected new version. If it does not, delete the temp file,
   fail "new binary failed health check", no swap.
5. Atomic replace: `rename(tempfile, exe)` (Linux allows replacing a running binary's file; on Windows
   the service path renames the current exe aside first, then moves the new one in — documented and
   build-tagged).
6. Post the command result (success, old→new version), then restart: on Linux under systemd, exit 0 so
   the unit restarts into the new binary; the Windows service requests a restart. On the next start, if
   the agent cannot reach the server or its own `version` subcommand fails at boot, a tiny bootstrap
   check restores `<exe>.bak` and restarts (rollback). Keep `<exe>.bak` until the next successful sync,
   then remove it.
- All of this is behind an interface with an injectable downloader, executor and filesystem root so the
  **e2e test runs the whole path against a fake "new" binary** (a tiny script that prints the new
  version) without replacing the real test binary — the swap targets a temp "exe" path in the test.
- Never log the token or the full URL with embedded secrets. Rate-limit: ignore a repeated identical
  advertised version once an update for it is in flight or just applied.

## Dashboard

- Device page: show the agent version; when an update is available show an "Update agent" button
  (admin/operator) and the target version; the result (and any rollback) appears in command history.
- Fleet: an "Agent version" column and a filter "outdated agent" (version != manifest version for the
  platform). A small banner on the Software/Fleet page showing the current released version from the
  manifest.
- Pure helpers (version compare, outdated check, manifest-for-platform) in `web/src/lib/agentver.ts`
  with a `node --test`. `?mock=1` gets a manifest, one up-to-date and one outdated device, and the
  update action. Phone width intact.

## Docs

`docs/PROTOCOL.md` (the manifest, the `agent_update` sync field, the `update` kind + endpoint),
`docs/OPS.md` (publish a new agent version: build, drop binaries + `agents.json` in `MSP_AGENT_DIR`,
auto-update vs forced, rollback behaviour, the `--no-auto-update` opt-out), `docs/SECURITY.md`
(hash-verified, health-checked, backup+rollback, token-authed download, the blast-radius note and why
rollback exists), `docs/GUIDE.md` (an "Agent updates" subsection), `docs/ARCHITECTURE.md`,
`docs/FEATURES.md`, `docs/STATUS.md` (AU1 row: Linux WORKING incl. rollback, Windows swap NOT
VERIFIED), `docs/ROADMAP.md` (tick agent self-update), `docs/CHECKLIST.md`.

## Proof (what the build must show here)

The six proof commands pass. A new `internal/e2e` test and agent unit tests:
- with a manifest advertising a newer version, the device's sync response carries `agent_update`;
- `POST /api/v1/devices/{id}/update` queues an `update` command; the agent (with an injected fake
  downloader serving a fake "new" binary that prints the new version) downloads, verifies the hash,
  health-checks, swaps the temp "exe", and reports `done` with old→new; `agent.update_requested` audited;
- a sha256 mismatch fails with no swap and no backup left dangling;
- the health check failing (fake binary prints the wrong version) aborts the swap with the old binary
  intact;
- a simulated bad post-swap start restores the `.bak` (rollback) — unit-tested on the bootstrap check;
- RS2: an operator scoped elsewhere gets 404 on the update endpoint; android → 400
  unsupported_on_platform; `no_update_available` when the manifest matches the device version.

Verify stage screenshots the device "Update agent" action and the fleet agent-version column with an
outdated device.
