# PM1 — Patch status and install (phase 2)

Report pending OS updates per device and let a technician install them. Linux uses `apt`; Windows
uses Windows Update. Fully testable in the container on Linux with a **fake** package manager (a stub
`apt-get` on PATH); **no test ever mutates the real container's packages**. Windows code is written,
cross-built and parser-unit-tested against a captured fixture, but marked NOT VERIFIED until run on a
real Windows host.

## Report

The agent collects a patch report periodically (on the first sync after start and every 6 h, and
immediately after an install or a scan command) and sends it as an optional top-level field on the
sync and enroll request, beside `facts`:

```
"patches": {"checked_at":ts,"manager":"apt"|"wu"|"none","reboot_required":bool,
  "count":N,"security_count":N,"error":"",
  "updates":[{"name":"openssl","current":"3.0.1","new":"3.0.2","security":true,"source":"..."}]}
```

- `manager` `none` = no supported package manager found (the report still carries `checked_at` and an
  empty list). `error` non-empty = the scan failed (e.g. apt lock held); keep the last good list.
- `updates` capped at 500 entries, security first then name; each field length-bounded (name<=256,
  version<=128, source<=256). Android never sends `patches` (no desktop package management).
- Protocol: `protocol.PatchReport` + `PatchUpdate` with those json tags; `Facts` is unchanged (patches
  are separate, they scan slower). Add `PatchReport *PatchReport json:"patches,omitempty"` to the sync
  and enroll request structs. `Validate` bounds the list and field lengths (-> 400 invalid_request
  naming the index). Add a fixture `sync_patches.json` and extend types_test.

## Command kind `patch`

Desktop only (`KindsFor` adds `patch` for linux/windows, not android; android -> 400
unsupported_on_platform; revoked -> unsupported_on_device). Agent-facing JSON:

```
{"id":N,"kind":"patch","action":"scan"|"install","mode":"all"|"security"|"ids","ids":["openssl"],
 "reboot":"never"|"if_required","timeout_s":3600}
```

- `action:"scan"` ignores mode/ids/reboot: the agent re-collects the report immediately and sends it on
  its next sync; the command result is a short summary (counts). `action:"install"` applies updates:
  `mode:"all"` every update, `"security"` security only, `"ids"` the named packages (must be a subset of
  the pending list; unknown ids -> the command fails with that detail). `reboot:"if_required"` reboots
  only when the post-install state needs it; `"never"` leaves `reboot_required` true for a later
  manual/scheduled reboot. Default action install, mode security, reboot never, timeout 3600 (max 14400).
- `KindSupportedOn`/`ValidKind` updated; `protocol.Command.Validate` covers the patch fields.

Dashboard open: `POST /api/v1/devices/{id}/patch {"action":"install","mode":"security","reboot":"never"}`
(admin|operator, CSRF, RS2 org-visible device, desktop, not revoked, online else 409 device_offline).
Audit `patch.scan_requested` / `patch.install_requested` with mode/ids/reboot. Reuses the command queue,
so delivery, result, history and the lost-command sweeper all apply; the command result holds the
install log (stdout/stderr, capped 64 KiB) exactly like exec/script.

## Agent

- `internal/agent/patch.go`: a `patchManager` interface `{Scan(ctx) (protocol.PatchReport, error);
  Install(ctx, mode, ids, reboot) (output string, rebooted bool, err error)}` chosen by platform, with
  the OS calls behind an **injectable command executor** (default: os/exec) so tests feed canned output.
- `patch_apt.go` (build tag linux): Scan via `apt-get -s dist-upgrade` (simulate) or
  `apt list --upgradable` parsed; security detection from the origin/`-security` suite. Install via
  `DEBIAN_FRONTEND=noninteractive apt-get -y --only-upgrade install <names>` (ids),
  `... apt-get -y upgrade` (all), or the security subset; `reboot_required` from
  `/var/run/reboot-required`. `if_required` reboot via `systemctl reboot` only when that file exists.
- `patch_windows.go` (build tag windows): Scan + install via the Windows Update COM API driven through
  PowerShell (`Microsoft.Update.Session`), output parsed from a JSON the script emits; `reboot_required`
  from the WU reboot-required API. Behind the same executor interface.
- `patch_other.go`: manager `none`.
- Wire into the sync loop: the collector adds the periodic patch report to the request; the runner
  dispatches `KindPatch` to the manager. Parsers are pure functions unit-tested with captured
  apt output and a captured WU JSON fixture (checked in under testdata). The agent never logs the full
  package list at info level.

## Store

Migration `0006_device_patches.sql`: add to `devices` the columns `patches_json TEXT NOT NULL DEFAULT
'{}'`, `patches_checked_at TEXT NULL`, `pending_patches INTEGER NOT NULL DEFAULT 0`,
`security_patches INTEGER NOT NULL DEFAULT 0`, `reboot_required INTEGER NOT NULL DEFAULT 0`. Store
method `RecordPatches(deviceID, report)` (updates all five, derives the counts) called from the
sync/enroll handler when the request carries `patches`. DEVICE JSON gains `patches` (the full report),
`pending_patches`, `security_patches`, `reboot_required`, `patches_checked_at`. The device list gains
`pending_patches` and `security_patches` so the fleet can show a column.

## Dashboard

- Device page "Updates" panel: last checked (relative), a reboot-required badge, counts (pending, of
  which security), the update list (name, current -> new, a "security" chip), and buttons **Install
  security**, **Install all** (admin/operator; confirm in-page) and **Rescan** (queues the patch
  command). The install result shows in the existing command history/output.
- Fleet: a "Updates" column showing the pending count, emphasised when `security_patches > 0`; a filter
  "has pending security updates".
- Pure helpers (summarise a report, security-only subset, reboot badge) in `web/src/lib/patches.ts`
  with a `node --test`. `?mock=1` gets a device with a few updates incl. a security one and a
  reboot-required device. Phone width intact (the list scrolls in its own box).

## Docs

`docs/PROTOCOL.md` (the patches report, the patch kind, the endpoint), `docs/GUIDE.md` (a "Patching"
section: see pending updates, install security vs all, reboots, that it is audited), `docs/OPS.md`
(apt needs the agent to run as root — it already does; Windows Update prerequisites; that scheduled
auto-patching is the automations item, PM1 is status + manual install), `docs/SECURITY.md` (install is
a role-gated, tenant-scoped, audited command; ids must be a subset of pending), `docs/ARCHITECTURE.md`,
`docs/FEATURES.md`, `docs/STATUS.md`, `docs/ROADMAP.md` (tick patch status+install; note Windows
NOT VERIFIED), `docs/CHECKLIST.md`.

## Proof (what the build must show here)

`CGO_ENABLED=0 go build ./...`, `go vet ./...`, `CGO_ENABLED=0 go test ./... -count=1`,
`GOOS=windows GOARCH=amd64 go build ./...`, `make web`, `scripts/e2e.sh`.

A new `internal/e2e` test that puts a **fake `apt-get`** (a small script written to a temp dir
prepended to PATH, emitting canned `--upgradable`/simulate output for scan and a success log for
install) so the real agent patch path runs without touching the container:
- the agent reports pending updates (incl. a security one) which appear in `GET /api/v1/devices/{id}`
  with the right counts;
- a technician `POST …/patch {action:install, mode:security}` queues a command, the agent installs via
  the fake apt, the command completes `done` with the log, and `patch.install_requested` is audited;
- `action:install, mode:ids` with an id not in the pending list fails with that detail;
- RS2: an operator scoped to another org gets 404 on the patch endpoint with no command created;
- android device -> 400 unsupported_on_platform.
Plus `internal/agent` unit tests parsing the captured apt output and the captured WU JSON fixture.

Verify stage screenshots the Updates panel (pending + security + reboot-required) and the fleet Updates
column.
