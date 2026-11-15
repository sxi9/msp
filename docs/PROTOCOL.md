# MSP protocol (normative)

This document is the contract between the server (`internal/server`), the desktop agent
(`internal/agent`), the Android agent (`android/`) and the dashboard (`web/`). The Go form
of every wire structure is `internal/protocol/types.go`; the canonical JSON samples are
`internal/protocol/fixtures/*.json` and are round-tripped by both the Go test suite and the
Android JVM test. The Android agent codes only from this document and those fixtures.

It is the PROTOCOL section of `docs/PLAN.md` with AMENDMENT A1 (five command kinds,
Android built-in commands, remote lock) applied in place. Where A1 and the original text
conflicted, A1 wins and the resolution is marked **(A1)**.

## 1. Transport

- HTTPS, or HTTP behind a TLS reverse proxy / on loopback in development.
- JSON bodies, `Content-Type: application/json`. All timestamps are RFC3339 UTC strings
  (`2026-11-13T10:00:01Z`).
- Server listens on `MSP_LISTEN` (default `:8080`; e2e uses `127.0.0.1:18080`).
- Path families: `/api/agent/v1/*` (device bearer auth), `/api/v1/*` (staff session auth),
  `/healthz`, `/dl/*`, `/` (SPA).
- Body limits: 256 KiB agent, 512 KiB dashboard (`http.MaxBytesReader`).
- Error body everywhere: `{"error":"<code>","detail":"<optional text>"}`.
- Agent `User-Agent`: `msp-agent/<version> (<linux|windows|android>/<arch>)`.

## 2. Agent authentication

**Enrollment token**: `et_` + 43 chars base64url (32 random bytes). Per org, created by
admin|operator; plaintext returned once; stored as sha256 hex with `token_prefix` (first 10
chars) for display; `max_uses`, `expires_at`, `revoked_at`. Used only by `POST /enroll`.

**Device token**: `dt_` + 43 chars base64url, returned once at enrollment, stored on the
server as sha256 hex in `devices.token_hash`; sent as `Authorization: Bearer dt_...`. One
token per device. Re-enrolling the same `(org_id, device_uid)` reuses the device row (history
kept) and rotates the token. Revoke = `token_hash` set NULL -> next request
`401 {"error":"invalid_device_token"}`; the agent logs "token rejected, re-enroll with
msp-agent install" and retries every 5 min (it never exits).

**Agent URL policy**: `https://` always; `http://` only when the host is
localhost / 127.0.0.0/8 / ::1 or config `insecure_http=true` (`--insecure-http`).
`--ca-file PEM` adds a private CA. There is no skip-verify option.

A device token authorises only that device's own `/sync` and `/commands/{id}/result` calls.
A command queued for one device is delivered only in that device's sync response and its
result is accepted only from that device's token (D5).

## 3. Agent endpoints

### 3.1 `GET /api/agent/v1/ping` (no auth)

`200 {"ok":true,"version":"0.1.0"}`

### 3.2 `POST /api/agent/v1/enroll` (no auth; rate limit 60 requests/min/IP, refilled continuously -> `429 {"error":"rate_limited"}`; the desktop agent retries a 429 up to six times 10 s apart)

Request (fixtures `enroll_linux.json`, `enroll_android.json`):

```json
{"enrollment_token":"et_...","device_uid":"f2c1...","hostname":"ws-reception-01",
 "platform":"linux","arch":"amd64","os_version":"Ubuntu 24.04.1 LTS","agent_version":"0.1.0","interval_s":30,
 "facts":{"kernel":"6.8.0-45-generic","cpu_model":"Intel(R) Core(TM) i5","cpu_count":8,"mem_total_mb":16000,
          "manufacturer":"Dell Inc.","model":"Latitude 5540","serial":"ABC123","ips":["10.0.0.12"]}}
```

- `device_uid`: `/etc/machine-id` | `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid` |
  `Settings.Secure.ANDROID_ID`.
- `platform` in `linux|windows|android`; `arch` in `amd64|arm64|386|arm`.
- `interval_s` clamped 1..3600 (0 -> 30, or 60 for android).
- Android: `hostname` = `Settings.Global "device_name"` or `Build.MODEL`; `os_version`
  `"Android 14 (34)"`; `facts.kernel = System.getProperty("os.version")`,
  `cpu_model = Build.HARDWARE`, `serial = ""`.
- `facts` is always an object (every key present, empty strings allowed), with two optional
  keys. **(RD1)** `rustdesk_id`, the id of the locally installed RustDesk client, 1..32 ASCII
  digits, omitted when RustDesk is not installed (`400 invalid_request`, detail
  `facts.rustdesk_id must be 1..32 digits`, otherwise). The server stores it in
  `devices.rustdesk_id`; an enrollment without it keeps a previously stored id.
  `apps`, the installed-apps inventory (section 3.2a), sent by the Android agent, and
  `software`, the installed-software inventory (section 3.2b), sent by the desktop agents.
  A device sends `apps` **or** `software`, never both.

| facts key      | type     | source (desktop / Android)                                   |
|----------------|----------|--------------------------------------------------------------|
| `kernel`       | string   | `uname -r` / `System.getProperty("os.version")`              |
| `cpu_model`    | string   | `/proc/cpuinfo`, registry / `Build.HARDWARE`                 |
| `cpu_count`    | int      | logical CPUs                                                 |
| `mem_total_mb` | int      | total RAM                                                    |
| `manufacturer` | string   | DMI / `Build.MANUFACTURER`                                   |
| `model`        | string   | DMI / `Build.MODEL`                                          |
| `serial`       | string   | DMI serial / `""` on Android                                 |
| `ips`          | [string] | non-loopback IPv4 addresses                                  |
| `rustdesk_id`  | string   | optional (RD1): local RustDesk client id, digits, <= 32      |
| `apps`         | [App]    | optional: installed apps, Android only (3.2a)                |
| `software`     | [Software] | optional: installed software, desktop only (SI1, 3.2b)     |

### 3.2a `facts.apps`: installed-apps inventory

```json
"apps":[{"package":"org.mozilla.firefox","label":"Firefox","version":"131.0.3","system":false},
        {"package":"com.android.settings","label":"Settings","version":"14","system":true}]
```

Go form `protocol.App{Package, Label, Version string; System bool}`, fixture
`sync_android_apps.json`.

- One entry per installed package from `PackageManager.getInstalledPackages` (the app
  declares `QUERY_ALL_PACKAGES`, which Android 11+ requires to list every package).
  `label` is the application label, or the package id when the app has none; `version` is
  `versionName`, `""` when the package declares none; `system` is
  `ApplicationInfo.FLAG_SYSTEM`.
- Order: non-system apps first, each group sorted by label (case-insensitive, then by
  package). The dashboard sorts again, so the order is a convenience, not a contract.
- Cap: at most **1000 entries** (`protocol.MaxApps`); entries past the cap are dropped from
  the end, so system apps go first. The Android agent additionally stops at 192 KiB of
  serialised apps JSON: with every field at its maximum length 1000 entries would be ~630 KiB,
  over the 256 KiB agent body limit, whereas typical entries are ~100 bytes (1000 of them
  ~100 KiB). Validation: `package` 1..256 bytes, `label` <= 256 bytes, `version` <= 64 bytes,
  at most 1000 entries; otherwise `400 invalid_request` with a detail naming the index
  (`facts.apps[3].package must be 1..256 bytes`, `facts.apps has 1001 entries, at most
  1000 are accepted`). The agent cuts its fields to the same limits before sending.
- Sent whenever facts are sent (enrollment, the first sync after start, every 120th sync);
  stored verbatim inside `facts_json`, returned inside `facts` by `GET /api/v1/devices/{id}`
  (no separate endpoint); facts without the key replace a stored inventory with none. The
  Android agent sends an empty array (`"apps":[]`) when it has nothing to report or the
  PackageManager read failed; the server re-marshals facts, so an empty array is stored as
  no key at all (`omitempty`) and the dashboard shows no Installed apps panel until the
  next facts sync carries entries.
- Desktop agents do not send `apps`; they send `software` (section 3.2b) instead.

### 3.2b `facts.software`: installed-software inventory **(SI1)**

```json
"software":[{"name":"openssl","version":"3.0.13-0ubuntu3.4","publisher":"Ubuntu Developers","source":"dpkg"},
            {"name":"7-Zip 24.08 (x64)","version":"24.08","publisher":"Igor Pavlov","source":"registry"}]
```

Go form `protocol.SoftwareItem{Name, Version, Publisher, Source string}`, fixture
`sync_software.json`.

- One entry per installed OS package/program. Linux reads `dpkg-query -W -f
  '${Package}\t${Version}\t${Maintainer}\n'` (`source:dpkg`), falling back to `rpm -qa --qf
  '%{NAME}\t%{VERSION}-%{RELEASE}\t%{VENDOR}\n'` (`source:rpm`) when dpkg is absent; both are
  read-only queries. Windows reads the uninstall registry keys (`HKLM\...\Uninstall`, the
  WOW6432Node and HKCU variants) and emits DisplayName/DisplayVersion/Publisher
  (`source:registry`), keeping only what Programs-and-Features shows (a DisplayName is
  required; SystemComponent, child entries with a ParentKeyName, and update/hotfix
  ReleaseType rows are dropped). `publisher` may be empty; the dpkg maintainer email is
  stripped and an rpm `(none)` vendor becomes empty.
- `source` is one of `dpkg` | `rpm` | `registry`. Order: by name (case-insensitive, then
  version); the dashboard sorts again, so the order is a convenience, not a contract.
- Cap: at most **2000 entries** (`protocol.MaxSoftware`); entries past the cap are dropped.
  The agent additionally stops at 192 KiB of serialised software JSON (`MaxSoftwareBytes`) so
  the sync body stays under the 256 KiB agent body limit. Validation: `name` 1..256 bytes,
  `version` <= 128 bytes, `publisher` <= 256 bytes, `source` in the enum, at most 2000
  entries; otherwise `400 invalid_request` with a detail naming the index
  (`facts.software[3].name must be 1..256 bytes`, `facts.software has 2001 entries, at most
  2000 are accepted`). The agent cuts its fields to the same limits before sending.
- Sent whenever facts are sent (enrollment, the first sync after start, every 120th sync);
  `RecordSoftware` stores the list in `devices.software_json` and refreshes `software_count`
  and `software_checked_at`. Returned as the sibling `software` array on `GET
  /api/v1/devices/{id}` (and searchable fleet-wide at `GET /api/v1/software`, section 6). A
  desktop agent omits the key on a host with no inventory (`omitempty`), which leaves the
  last stored list untouched.

Response `201 {"device_id":42,"device_token":"dt_...","poll_interval_s":30}`, plus
`"rustdesk":{"host":"rmm.example.com","key":"<id_ed25519.pub line>"}` **(RD1)** when the
server has a relay configured (`MSP_RUSTDESK_HOST` + `MSP_RUSTDESK_KEY`); the key is
omitted entirely when it has none. The desktop agent stores host and key in its config.

Errors: `400 {"error":"invalid_request","detail":"..."}`;
`401 {"error":"invalid_enrollment_token"}` (unknown, expired, revoked, uses >= max_uses).

Side effects: `enrollment_tokens.uses += 1` atomically; devices upsert; audit
`device.enrolled` actor_type=device.

### 3.3 `POST /api/agent/v1/sync` (Bearer `dt_`) — heartbeat AND command channel

Request (fixtures `sync_linux.json`, `sync_android.json`):

```json
{"agent_version":"0.1.0","interval_s":30,"wait_s":25,"uptime_s":86400,
 "metrics":{"cpu_pct":12.5,"mem_total_mb":16000,"mem_used_mb":8120,
            "disks":[{"mount":"/","total_gb":476.0,"used_gb":201.3}],
            "battery_pct":87,"battery_charging":false,"ips":["10.0.0.12"]},
 "facts":{...same shape as enroll...}}
```

- `cpu_pct` is `null` on Android and on the agent's first sample (the key is always present).
- `battery_pct` / `battery_charging` are absent when there is no battery.
- Android sends one disk `{"mount":"/data",...}`.
- `facts` is present only on the first sync after start and every 120th sync (the server
  replaces `facts_json` when present, and `devices.rustdesk_id` when the facts carry
  `rustdesk_id`; invalid `rustdesk_id` -> `400 invalid_request`, fixture `sync_rustdesk.json`).
  The Android agent's facts carry the installed-apps inventory `apps` (section 3.2a, fixture
  `sync_android_apps.json`; an invalid entry -> `400 invalid_request` naming the index); the
  desktop agents' facts carry the installed-software inventory `software` instead (section
  3.2b, fixture `sync_software.json`), stored by `RecordSoftware`.
- `interval_s` = the agent's configured interval (desktop 30, android 60, e2e 2); clamped 1..3600.
- `wait_s` = how long the server may hold the request; the server clamps it to 0..25.
  Android sends 0.

Server: deviceAuth -> `RecordSync` (`last_seen_at`, `last_ip`, `agent_version`, `interval_s`,
`last_metrics_json`, denormalized cpu/mem/disk/battery/uptime columns; a heartbeats row only
if `last_sample_at` is older than 60 s) -> subscribe to the device's notifier ->
`ClaimQueuedCommands(device, 10)` (queued -> sent, `sent_at = now`); if none and `wait_s > 0`
block until `Notify(device)` or `wait_s` elapses or the client disconnects, then claim again
-> respond.

**(A1)** Android is NOT answered with an empty list any more: with `wait_s` 0 it is served
the same `ClaimQueuedCommands(device, 10)` without blocking, so queued lock/ping/message
commands reach it on its next sync. The result endpoint works identically for every platform.

Response `200` (fixtures `sync_response.json`, `sync_response_android.json`):

```json
{"poll_interval_s":30,
 "commands":[{"id":7,"kind":"exec","argv":["uname","-a"],"timeout_s":60},
             {"id":8,"kind":"script","shell":"sh","body":"df -h\nuptime\n","timeout_s":120},
             {"id":9,"kind":"lock","timeout_s":30},
             {"id":10,"kind":"ping","timeout_s":30},
             {"id":11,"kind":"message","text":"Please call the helpdesk","timeout_s":30}]}
```

- `poll_interval_s` = 5 while more queued commands remain for the device (fallback for agents
  whose long-poll is cut by a proxy), else the device's `interval_s`.
- `commands` is `[]` (never `null`).
- Each command carries only the keys of its kind: `argv` for exec; `shell` + `body` for
  script; `text` for message; nothing but `id`, `kind`, `timeout_s` for lock and ping.
- `rustdesk` **(RD1)**: `{"host":"...","key":"..."}` when the server has a relay configured
  (fixture `sync_response_rustdesk.json`), absent otherwise. Agents update their stored
  relay when it is present and differs (the desktop agent rewrites its config file);
  an absent key leaves the stored relay untouched. Android ignores it.
- `agent_update` **(AU1)**: `{"version":"0.2.0","url":"/dl/msp-agent-linux-amd64","sha256":"<64 hex>"}`
  (fixture `sync_response_update.json`), sent only when the release manifest has an entry for the
  device's `platform-arch` whose version differs from the agent's reported `agent_version`;
  absent otherwise, and never sent to Android or a revoked device. When auto-update is enabled the
  agent applies it (download → verify → health-check → swap); it is otherwise advisory (the
  dashboard greys/enables the "Update agent" button). See section 5, "update".

Agent loop (desktop): sync -> for each command spawn a goroutine (max 2 concurrent per
device) that executes and POSTs the result -> if the response contained commands sync again
immediately, else sleep `max(0, poll_interval_s - elapsed)` where `poll_interval_s` is clamped
to `1..interval_s` -> loop. Network/5xx error: exponential backoff 5 s doubling to 300 s,
reset on success. 401: sleep 5 min, retry.

Agent loop (Android): sync with `wait_s` 0 -> process returned commands sequentially and POST
each result before the next sync -> schedule the next sync after `poll_interval_s` clamped
15..600. IOException -> backoff 30 s doubling to 600 s. 401 -> notification "Token rejected —
open app to re-enroll" and retry every 5 min.

Errors: `400 invalid_request`, `401 invalid_device_token`.

### 3.4 `POST /api/agent/v1/commands/{id}/result` (Bearer `dt_`; the command must belong to this device and be in status `sent`)

Request (fixture `command_result.json`):

```json
{"exit_code":0,"stdout":"Linux ws-01 6.8...\n","stderr":"","started_at":"2026-11-13T10:00:01Z",
 "finished_at":"2026-11-13T10:00:01Z","truncated":false,"error":""}
```

- `error` is `""` on normal completion; `"timeout after 60s"` when the deadline hit
  (`exit_code -1`); `"spawn: <reason>"` when the process could not start (`exit_code -1`);
  `"unsupported kind: X"` when the agent does not implement the kind (`exit_code -1`).
- `stdout` / `stderr` each capped at 65536 bytes by the agent (`truncated=true`).

Response `204`. Status becomes `timeout` (error starts with "timeout"), `failed` (error
non-empty or `exit_code != 0`), else `done`. `404 {"error":"not_found"}` when the command does
not exist or belongs to another device; `409 {"error":"conflict"}` when it is this device's
command but not in status `sent` (already finished, lost, or never delivered). Agents treat
both as final. `started_at` / `finished_at` are stored with millisecond precision. Audit
`command.finished` actor_type=device. A `kind shell` command (RS1) is the exception: the agent
posts a result here only for its two refuse cases; otherwise the server completes it when the
relay closes.

### 3.5 `GET /api/agent/v1/shell/{command_id}` (Bearer `dt_`) — remote-shell agent leg **(RS1)**

The agent leg of the interactive remote-shell relay: a WebSocket upgrade (`101`) the agent dials
after it claims a `kind shell` command. Checks and framing are specified in section 6, "Remote
shell"; the PTY/ConPTY side is in section 5, "shell".

### 3.6 Patch report (`patches`) **(PM1)**

The desktop agent collects a pending-OS-updates report periodically (on the first sync after
start, every 6 h, and immediately after a patch command) and sends it as an optional top-level
field beside `facts` on the enroll (3.2) and sync (3.3) requests. Android never sends it (no
desktop package management), and a sync between scans omits it (the field is `omitempty`).

```json
"patches":{"checked_at":"2026-11-14T10:00:00Z","manager":"apt","reboot_required":true,
  "count":1,"security_count":1,"error":"",
  "updates":[{"name":"openssl","current":"3.0.1","new":"3.0.2","security":true,"source":"Ubuntu:24.04/noble-security"}]}
```

- `manager` is `apt` (Linux), `wu` (Windows Update) or `none` (no supported package manager — the
  report still carries `checked_at` and an empty `updates`). `error` non-empty means the scan
  failed (e.g. apt lock held); the server keeps the stored report's list and the last good counts.
- `updates` is capped at 500 entries (`protocol.MaxPatchUpdates`), security first then name; each
  field is length-bounded (name <= 256, versions <= 128, source <= 256). `count`/`security_count`
  are the agent's own totals and may exceed the (capped) list.
- Go form `protocol.PatchReport` + `PatchUpdate`; `PatchReport.Validate()` bounds the list and the
  field lengths (`400 invalid_request` naming the offending index). On a valid report the server
  calls `RecordPatches`: it stores the full report and refreshes the device's denormalised
  `pending_patches`, `security_patches`, `reboot_required` and `patches_checked_at` (the counts are
  derived from the list but never fall below the agent's own totals). Fixture `sync_patches.json`.

### 3.7 `GET /api/agent/v1/files/{id}` (Bearer `dt_`) — library file download **(SD1)**

Streams a file-library artifact (section 6, "File library") to an authenticated device so a
`kind deploy` file command can fetch it. Device-token auth, never a session; no CSRF.

- Allowed only when the file's `org_id` is the device's own org or NULL (a global artifact);
  any other org is `404 {"error":"not_found"}`, indistinguishable from a missing file, so a
  device token cannot probe another client's library.
- `200` streams the bytes with `Content-Type: application/octet-stream`, a `Content-Length` and
  an `X-MSP-SHA256` header (the stored hash). The agent re-verifies the length and SHA-256 after
  download and before it writes or runs anything; a mismatch fails the command.
- `404 not_found` when the file does not exist, is not visible to the device, or its bytes are
  missing on disk; `401 invalid_device_token` without a valid token.

## 4. Status and timing

- Desktop interval 30 s, Android 60 s, `wait_s` 25 (agent HTTP timeout 40 s).
- Device status is computed at read time from `devices.last_seen_at` and `interval_s`:
  `pending` if never seen, `online` if `now - last_seen_at <= 3*interval_s + 15 s`, else
  `offline` (30 s -> 105 s; 60 s -> 195 s; e2e 2 s -> 21 s).
- Evaluator runs every 15 s: opens alert type `offline` (severity high) for offline,
  non-revoked devices and resolves it when online again; marks commands `lost` when
  `status=sent` and `sent_at + timeout_s + 5 min < now` (audit `command.lost`
  actor_type=system).
- Command latency: < 1 s while the agent is parked in a sync, <= `interval_s` otherwise.

## 5. Commands **(A1)**

Command kinds are exactly `exec`, `script`, `lock`, `ping`, `message`, `shell` **(RS1, D3)**,
`patch` **(PM1)**, `deploy` **(SD1)**, `update` **(AU1)**.
The Go form is `protocol.Command{ID, Kind, Argv, Shell, Body, Text, Action, Mode, IDs, Reboot,
Source, FileID, SHA256, Dest, Run, RunArgs, Package, Version, URL,
TimeoutS, Cols, Rows, ShellSession}` (`Action`/`Mode`/`IDs`/`Reboot` belong to `patch`,
`Source`/`FileID`/`SHA256`/`Dest`/`Run`/`RunArgs`/`Package` to `deploy`, `Version`/`URL` (with
`SHA256`, shared with `deploy`) to `update`, the last three to
`shell`); `Command.Validate()` checks the shape,
`KindSupportedOn(kind, platform)` checks the platform, `KindsFor(platform)` and
`ShellsFor(platform)` drive the UI.

| kind      | platforms               | fields                         | agent behaviour                                   |
|-----------|-------------------------|--------------------------------|---------------------------------------------------|
| `exec`    | linux, windows          | `argv` (len >= 1, each <= 4096 bytes) | `exec.CommandContext(argv[0], argv[1:]...)`, no shell |
| `script`  | linux, windows          | `shell`, `body` (non-empty, <= 262144 bytes) | body written to a 0600 file, run by the fixed interpreter argv |
| `lock`    | linux, windows, android | none                           | locks the interactive session(s)                   |
| `ping`    | linux, windows, android | none                           | exit 0, stdout `pong`                              |
| `message` | linux, windows, android | `text` (non-empty, <= 4096 bytes) | shows the text to the logged-in user            |
| `shell`   | linux, windows          | `shell` (optional), `cols`/`rows` (optional), `timeout_s` = max duration | opens a PTY/ConPTY and relays it over a WebSocket (section 6, "Remote shell") |
| `patch`   | linux, windows          | `action`, `mode`, `ids`, `reboot`, `timeout_s` (max 14400) | scans or installs OS updates via apt / Windows Update (section 5, "patch", PM1) |
| `deploy`  | linux, windows          | `source`, and for file `file_id`/`sha256`/`dest`/`run`/`run_args`, for package `package` | downloads a library file and optionally runs it, or installs a named package (section 5, "deploy", SD1) |
| `update`  | linux, windows          | `version`, `url`, `sha256`, `timeout_s` (default 600) | downloads the agent release, verifies the hash, health-checks it and swaps itself, rolling back on a bad start (section 5, "update", AU1) |

Common rules:

- `timeout_s`: 0 means the default 60; otherwise it must be 1..3600 (the dashboard API answers
  `400 invalid_request` outside that range; agents clamp defensively). On deadline the process
  group is killed (Linux `Setpgid` + `kill(-pgid)`; Windows `Process.Kill`, tree kill is phase 2).
- Only admin|operator can queue commands; viewer never can.
- Every queued command writes audit `command.queued` with `argv`, or `shell` + body sha256 +
  first 200 chars, or `text`. `lock` additionally writes audit `device.lock_requested` with the
  actor user id.
- Unknown kind on any agent -> result `exit_code -1`, `error "unsupported kind: X"`.
- An agent executes only commands delivered in its own sync response; results are accepted
  only with the same device token (see section 2).

### exec (desktop only)

`exec.CommandContext(ctx, argv[0], argv[1:]...)`: PATH lookup allowed, no shell, shell
metacharacters are literal (`["echo","a;id"]` prints `a;id`). `cwd` = a per-command temp dir;
`env` = the service's environment unchanged plus `MSP_COMMAND_ID=<id>`.

### script (desktop only)

`shell` in `sh|bash` (Linux) or `powershell|cmd` (Windows). The agent writes `body` to
`<tmpdir>/msp-cmd-<id>/script.(sh|ps1|cmd)` (dir 0700, file 0600) and runs
`[sh|bash, file]` / `[powershell.exe, -NoProfile, -NonInteractive, -ExecutionPolicy, Bypass, -File, file]`
/ `[cmd.exe, /C, file]`. Wrong shell for the platform -> `error "spawn: shell <x> not available on <platform>"`.
The dir is removed afterwards.

### lock (all platforms)

- Linux: run `["loginctl","lock-sessions"]`; if that exits non-zero try
  `["xdg-screensaver","lock"]`; report the first that succeeds. When loginctl reports that
  there is no session to lock (headless host, container) the result is exit 0 with stdout
  `no interactive session`, like Windows. Otherwise `failed` with `error` listing every
  attempt (`lock failed: loginctl: exit 1: <stderr>; xdg-screensaver: spawn: ...`).
- Windows: the agent runs as a service in session 0, where `LockWorkStation` does nothing.
  Find the active console session (`WTSGetActiveConsoleSessionId`), `WTSQueryUserToken` for
  it, `CreateProcessAsUser` running `rundll32.exe user32.dll,LockWorkStation` with
  `CREATE_NO_WINDOW`; if no user is logged on return exit 0 with stdout
  `no interactive session`. When not running as a service (dev mode) call user32
  `LockWorkStation` directly. Uses `golang.org/x/sys/windows` only.
- Android: `DevicePolicyManager.lockNow()` through a `DeviceAdminReceiver`
  (`android.app.action.DEVICE_ADMIN_ENABLED`, `res/xml/device_admin.xml` with `<force-lock/>`).
  If lock is requested without device admin granted: `exit_code 1`,
  `error "device admin not granted"`.
- Scope (D5): only enrolled devices (holding a valid device token) in the technician's org
  tree can be locked; there is no endpoint that can lock anything else. Lock is a built-in
  kind, never free text, and is always audited.

### ping (all platforms)

Reply `exit_code 0`, `stdout "pong"`, `stderr ""`, `error ""`.

### message (all platforms)

- Linux: run `notify-send` with the text if present on PATH, else write the text to the
  result stdout with exit 0.
- Windows: `msg.exe` via the console session (best effort); exit 0 with the text in stdout
  when no session is available.
- Android: a high-importance notification with the text; result exit 0. When notifications
  are disabled for the app the text goes to stdout with stderr `notifications disabled`, still
  exit 0 (same as the desktop agents without a notifier).

### shell (desktop only, RS1 — interactive relay)

An interactive remote terminal, not a one-shot result. The agent-facing command JSON is
`{"id":N,"kind":"shell","shell":"bash"|"sh"|"powershell"|"cmd","cols":120,"rows":32,"timeout_s":14400}`:

- `shell` optional; default `bash` if on PATH else `sh` (Linux), `powershell` (Windows).
- `cols`/`rows` optional, default 120×32, clamped 20..500 / 5..200. They are the initial PTY
  size only; the browser's first `resize` control frame (section 6, "Remote shell") is
  authoritative, so they are not persisted by the store and may be omitted from the command JSON.
- `timeout_s` is the **maximum session duration** here (not a one-shot deadline): default 14400
  (4 h), max 28800 (8 h), clamped with `ClampShellTimeout` — the single exception to the 1..3600
  rule above.

The agent does not post a `CommandResult` on the normal path: the server completes the command
when the relay closes (section 6, "Remote shell"). The agent posts a result only for its two
refuse cases — a shell is already running (`exit_code -1`, `error "shell busy"`) or the PTY/ConPTY
will not open (`exit_code -1`, the platform reason; on an unsupported OS `"shell not supported on
this platform"`). The relay transport, both WebSocket legs and the agent leg endpoint
`GET /api/agent/v1/shell/{command_id}` are specified under section 6, "Remote shell".

### patch (desktop only, RS1 excepted — PM1)

Applies or scans OS updates. The agent-facing command JSON is
`{"id":N,"kind":"patch","action":"scan"|"install","mode":"all"|"security"|"ids","ids":["openssl"],"reboot":"never"|"if_required","timeout_s":3600}`:

- `action:"scan"` ignores `mode`/`ids`/`reboot`: the agent re-collects the report immediately (so
  it rides the next sync, section 3.6) and the command result is a short counts summary.
- `action:"install"` applies updates — `mode:"all"` every update, `"security"` the security subset,
  `"ids"` the named packages (which must be a subset of the pending list; unknown ids fail the
  command with that detail). `reboot:"if_required"` reboots only when the post-install state needs
  it (Linux `/var/run/reboot-required`), `"never"` leaves `reboot_required` set for a later reboot.
  The result holds the install log in `stdout` (capped 64 KiB) exactly like exec/script.
- Defaults: action `install`, mode `security`, reboot `never`. `timeout_s` is 0 (default 3600) or
  1..14400 — the patch exception to the 1..3600 rule (an install can be slow), clamped with
  `ClampPatchTimeout`. Linux drives apt (`apt-get -s dist-upgrade` to scan, `apt-get -y ...` to
  install); Windows drives Windows Update. Both go through an injectable command executor so tests
  feed a fake apt / captured output; the agent never logs the full package list at info level.

### deploy (desktop only — SD1)

Pushes a file to a device and optionally runs it, or installs a named OS package. The
agent-facing command JSON carries only the keys of its source:

```json
{"id":N,"kind":"deploy","source":"file","file_id":M,"sha256":"<64 hex>",
 "dest":"/opt/acme/installer.sh","run":true,"run_args":["--silent"],"timeout_s":3600}
{"id":N,"kind":"deploy","source":"package","package":"htop","timeout_s":3600}
```

- `source:"file"`: `file_id` + `sha256` name a server-side library file (section 6, "File
  library"); `dest` is where it is written (absolute, cleaned, no `..`, not through a symlinked
  parent; the parent is created 0755 if missing). The agent downloads
  `GET /api/agent/v1/files/{file_id}`, verifies the response `Content-Length` and the SHA-256
  **before** writing, writes a temp file in the dest directory and renames it into `dest` (0755
  when `run` else 0644), and when `run` is set executes `dest` with `run_args` via argv (no
  shell), capturing output like exec/script. A hash mismatch fails the command without writing
  dest. `run_args` is allowed only with `run`; each element ≤ 4096 bytes.
- `source:"package"`: `package` is one name (charset `[A-Za-z0-9._+-]`, ≤ 256 bytes), installed
  through the same injectable executor PM1 uses:
  `DEBIAN_FRONTEND=noninteractive apt-get -y install <pkg>` on Linux (argv, never a shell); the
  Windows installer path is NOT VERIFIED and a package deploy on Windows fails cleanly. The
  captured install log comes back in the command result.
- `timeout_s` uses the ordinary 0 (default 60) or 1..3600 rule.
- `Command.Validate()`: `source` required; a file source needs `file_id` + a 64-hex `sha256` +
  a valid `dest`; a package source needs a valid package name and must not set the file fields;
  `run_args` only with `run`. Fixture `command_deploy.json`.

### update (desktop only — AU1)

Replaces the running agent binary with a newer release. The server fills `version`, `url` and
`sha256` from the release manifest (section 6, "Agent releases") at queue time; the technician
never types them. The agent-facing command JSON is:

```json
{"id":N,"kind":"update","version":"0.2.0","url":"/dl/msp-agent-linux-amd64","sha256":"<64 hex>","timeout_s":600}
```

- `url` is relative to the server base; the agent joins it to its own base and refuses a URL on a
  different host **or one that downgrades the scheme** (http on an https server), so the device
  token is never sent off the server or in cleartext.
- The agent downloads `url` into a temp file **beside** the running executable, verifies the size
  and `sha256` **before** anything else (a mismatch aborts with no change and no backup),
  `chmod 0755`, backs the current executable up to `<exe>.bak` (a copy), **health-checks** the new
  binary by running `<tmp> version` and confirming it prints `version` (a failure aborts, old binary
  intact, backup removed), then atomically renames the verified binary into place. The result reports
  `self-update applied: <old> -> <new>`. (The agent sends its device token on the `/dl/` fetch, but
  `/dl/` serves the agent binaries publicly and does not require it; integrity rests on TLS plus the
  manifest `sha256` re-check.)
- After posting the result the agent exits so the service manager restarts into the new binary. Two
  rollback checks protect the fleet while `<exe>.bak` exists (until the next successful sync clears
  it): on the next start a bootstrap check restores `<exe>.bak` if the installed binary cannot run its
  own `version` subcommand; and a binary that answers `version` but cannot complete a successful sync
  within ~10 minutes / 8 consecutive failures restores `<exe>.bak` and restarts, so a release that
  starts but cannot reach the server does not brick the device.
- Self-update is applied automatically only when auto-update is enabled for the agent (config
  `auto_update`, default on; `msp-agent run --no-auto-update` / enroll `--no-auto-update`
  disables it). A dashboard `update` command forces it regardless.
- `Command.Validate()`: `version` (1..64 of `[A-Za-z0-9.+_-]`), `url` (1..2048, no spaces or
  control characters) and a 64-hex `sha256` are required; no deploy/patch/exec field may be set.
  Fixture `command_update.json`.

### Android (A1)

Android supports exactly `lock`, `ping`, `message` and rejects everything else with
`exit_code -1`, `error "unsupported kind"`. Each sync processes returned commands sequentially
and POSTs results before the next sync.

## 6. Dashboard API (`/api/v1`)

Session cookie `msp_session`: `HttpOnly`, `SameSite=Strict`, `Path=/`, `Secure` when TLS or
`MSP_SECURE_COOKIES=1`, 12 h; cookie value `"ss_"+random` stored as sha256. Every non-GET
request must carry header `X-Requested-With: msp` else `403 {"error":"csrf"}`.
`401 {"error":"unauthorized"}` without a valid session; `403 {"error":"forbidden"}` for an
insufficient role. Roles: `viewer < operator < admin`.

### Auth

- `POST /api/v1/login {"email","password"}` -> `200 {"user":{"id":1,"email":"...","role":"admin"}}`
  + `Set-Cookie`; `401 {"error":"invalid_credentials"}`; `429` after 10 attempts/min/IP. Audit
  `user.login` / `user.login_failed`. **Two-step (TF1):** when the user has 2FA enabled the
  password is only the first factor — the response is `200 {"totp_required":true}` with **no**
  session cookie, only a short-lived (5 min) **pending** cookie (`msp_pending_totp`) bound to the
  user and a nonce. The pending cookie is not a session. A wrong password never reveals whether
  2FA is on (the same bcrypt time is spent either way).
- `POST /api/v1/login/totp {"code"}` (TF1; reads the pending cookie) — second step: verifies a
  6-digit TOTP code **or** a one-time recovery code, consumes the pending token, creates the real
  session and returns `200 {"user":{...}}` + `Set-Cookie`. `401 {"error":"invalid_code"}` on a wrong
  code (rate-limited, 10/min/IP, audit `user.login_totp_failed`); `401 {"error":"pending_expired"}`
  when the pending cookie is missing/expired or 2FA was turned off in the window. On success audit
  `user.login` with `detail.second_factor` = `totp` or `recovery_code`.
- `POST /api/v1/logout` -> `204`.
- `GET /api/v1/me` -> `{"user":{...}}`. The user object carries `totp_enabled` (bool) and
  `recovery_codes_remaining` (int, unused codes left; 0 when 2FA is off). The TOTP secret is never
  returned.

#### Two-factor enrolment and management (TF1)

TOTP is RFC 6238 (HMAC-SHA1, 6 digits, 30 s step, ±1 step skew), native to the Go stdlib. The
secret is 20 random bytes base32-encoded, write-only over the API and never logged. Recovery codes
(10 per user, `xxxxx-xxxxx`) are stored only as a sha256 hash and are single-use.

- `POST /api/v1/me/totp/setup` (session + CSRF, any role) -> `200 {"secret","otpauth_uri"}` —
  generates a pending (not-yet-enabled) secret and its `otpauth://totp/MSP:<email>?...` URI. Re-running
  before confirm replaces the pending secret. `409 {"error":"conflict"}` if 2FA is already enabled
  (disable first).
- `POST /api/v1/me/totp/confirm {"code"}` -> `200 {"recovery_codes":[...]}` — verifies the code
  against the pending secret, enables 2FA and returns the 10 recovery codes **once** (plaintext).
  `400 {"error":"invalid_code"}` on a wrong code. Audit `user.totp_enabled`.
- `POST /api/v1/me/totp/disable {"code"}` -> `204` — a valid current TOTP code or a recovery code
  turns 2FA off, clearing the secret and deleting the recovery codes. `400 invalid_code`. Audit
  `user.totp_disabled`.
- `POST /api/v1/me/totp/recovery/regenerate {"code"}` -> `200 {"recovery_codes":[...]}` — with a
  valid current TOTP code, replaces the recovery codes and returns the new set once. `400
  invalid_code`. Audit `user.totp_recovery_regenerated`.
- `POST /api/v1/users/{id}/totp/reset` (admin) -> `204` — disables the target user's 2FA so a
  locked-out user can re-enrol. `404 not_found` for a missing user. Audit `user.totp_reset` with the
  acting admin as actor and the target as target.
- All five of the verifying endpoints above (confirm/disable/regenerate and both login steps) share
  the per-IP login rate limiter (10/min).
- `POST /api/v1/me/password {"current_password","new_password"}` (any role, own account;
  `new_password` >= 10 chars and different from the current one) -> `204`; the user's other
  sessions are deleted, the calling session stays valid. `400 invalid_request` with a detail
  (`current password is incorrect`, ...); `429` under the login rate limit (10/min/IP). Audit
  `user.password_changed` / `user.password_change_failed`. Recovery without the current
  password is the CLI `msp-server user set-password` (section 9).

### Summary and orgs

Every list and counter below is scoped to the **visible org set** of the caller
**(RS2)**: a user whose `all_orgs` is false sees only the orgs assigned to them
(section 6, Users), and an admin or an `all_orgs` user sees every org. A request
for one resource in an org the caller cannot see returns `404 not_found` — the
same response an unrelated resource gives, never a distinguishable `403` (see
docs/SECURITY.md section 4).

- `GET /api/v1/summary` -> `{"orgs":4,"devices":57,"online":51,"offline":5,"pending":1,"open_alerts":3}`
  — every counter is computed over the caller's visible orgs only.
- `GET /api/v1/orgs` -> `{"orgs":[{"id":1,"name":"Acme Dental","created_at":"...","device_count":12,"online_count":11,"open_alerts":1}]}`
  — only the caller's visible orgs.
- `POST /api/v1/orgs {"name"}` (admin) -> `201 {"org":{...}}`; `409 {"error":"conflict"}` on
  duplicate. Audit `org.created`. Org creation is admin-only; the dashboard hides "Add client" for non-admins.
- `GET /api/v1/orgs/{id}` -> `{"org":{...},"tokens":[{"id":3,"org_id":1,"token_prefix":"et_Ab12cD3","label":"front desk","max_uses":10,"uses":2,"expires_at":"...","created_by":1,"created_at":"...","revoked_at":null}]}`.
  `404 not_found` when the org is not in the caller's visible set **(RS2)**; token
  create/revoke under an org is likewise `404` when the org is not visible, checked before the action.
- `POST /api/v1/orgs/{id}/tokens {"label":"front desk","max_uses":10,"ttl_hours":168}`
  (admin|operator; defaults 10 and 168; `max_uses` 1..1000, `ttl_hours` 1..8760) ->
  `201 {"token":{...row...},"plaintext":"et_...","install":{"linux":"curl -fsSLo /tmp/msp-install.sh BASE/dl/install.sh && sudo bash /tmp/msp-install.sh --server BASE --token et_...","windows":"irm BASE/dl/install.ps1 -OutFile $env:TEMP\\msp-install.ps1; & $env:TEMP\\msp-install.ps1 -Server BASE -Token et_...","android":"Install BASE/dl/msp-agent.apk, open MSP Agent, enter BASE and the token et_..."}}`
  where `BASE` = `MSP_BASE_URL` or request scheme+host. Audit `token.created`.
- `POST /api/v1/orgs/{id}/tokens/{tid}/revoke` (admin|operator) -> `204`. Audit `token.revoked`.

`device.enrolled` for an existing `(org_id, device_uid)` carries `re_enrolled: true`,
`previous_hostname` and `previous_status`; when the hostname differs and the row was not
revoked it also carries a `warning` (and the server logs one): that is a cloned machine-id,
the previous agent's token is now invalid.

### Devices

Every device-scoped endpoint below (list, detail, heartbeats, commands, shell, transcript,
remote-desktop, lock/ping/message, revoke, delete) is filtered or gated by the caller's **visible
org set (RS2)**: the list returns only devices whose org is visible, an explicit `?org_id=` outside
the set is `404 not_found`, and a single device/command the caller cannot see returns `404
not_found` **before** any side effect, role result or `409` — so a scoped user cannot tell a device
in another org exists (docs/SECURITY.md section 4).

- `GET /api/v1/devices?org_id=&status=online|offline|pending&q=<hostname substring>` -> `{"devices":[DEVICE...]}`

```
DEVICE = {"id":42,"org_id":1,"org_name":"Acme Dental","device_uid":"f2c1...","hostname":"ws-01",
          "platform":"linux","arch":"amd64","os_version":"Ubuntu 24.04.1 LTS","agent_version":"0.1.0",
          "interval_s":30,"status":"online","enrolled_at":"...","last_seen_at":"...","last_ip":"10.0.0.12",
          "revoked":false,"cpu_pct":12.5,"mem_used_pct":50.8,"disk_used_pct":42.3,"battery_pct":null,
          "battery_charging":null,"uptime_s":86400,"open_alerts":0,"can_run_commands":true,
          "rustdesk_id":null,"pending_patches":2,"security_patches":1,"reboot_required":false,
          "patches_checked_at":"2026-11-14T10:00:00Z","software_count":214,
          "software_checked_at":"2026-11-14T10:00:00Z"}
```

`rustdesk_id` **(RD1)** is the id the agent last reported in its facts, `null` until then.

`pending_patches`, `security_patches`, `reboot_required` and `patches_checked_at` **(PM1)** are
denormalised from the device's last patch report (section 3.6) by `RecordPatches`; they are on every
DEVICE row (so the list carries the fleet "Updates" column) and `patches_checked_at` is `null` until
the first report. The full report is **not** in DEVICE — it is a sibling `patches` object on the
device-detail response (below).

`software_count` and `software_checked_at` **(SI1)** are denormalised from the device's last
installed-software inventory (section 3.2b) by `RecordSoftware`; they are on every DEVICE row (so the
list carries a fleet "Software" count) and `software_checked_at` is `null` until the first report. The
full list is **not** in DEVICE — it is a sibling `software` array on the device-detail response (below).

`can_run_commands` is true when the device is a desktop platform (`linux|windows`) and not
revoked: it gates the free-form exec/script console. **(A1)** The built-in kinds lock, ping and
message are available for every non-revoked device regardless of `can_run_commands`; the
dashboard decides from `platform` which controls to show.

- `GET /api/v1/devices/{id}` -> `{"device":DEVICE,"facts":{...facts object...},"metrics":{...last metrics object...},"patches":{...patch report...},"software":[...SoftwareItem...],"alerts":[open ALERT...],"commands":[last 20 COMMAND without stdout/stderr]}`.
  `facts` is the last facts object the agent sent, verbatim, so for an Android device it
  carries the `apps` array (section 3.2a); the dashboard shows it as the "Installed apps" panel.
  `patches` **(PM1)** is the full `protocol.PatchReport` the agent last sent (section 3.6), `{}` for
  a device that never reported; the dashboard's "Updates" panel reads it.
  `software` **(SI1)** is the full installed-software list the desktop agent last sent (section
  3.2b), `[]` for a device that never reported; the dashboard's "Installed software" panel reads it.
- `GET /api/v1/devices/{id}/heartbeats?limit=120` -> `{"heartbeats":[{"at":"...","cpu_pct":12.5,"mem_used_pct":50.8,"disk_used_pct":42.3,"battery_pct":null,"uptime_s":86400}]}` oldest first.
- `GET /api/v1/software?q=<substring>&org_id=` (admin|operator|viewer; read) **(SI1)** -> the
  fleet-wide installed-software search: `{"query":"openssl","results":[{"device_id":42,
  "hostname":"ws-01","org_id":1,"org_name":"Acme Dental","name":"openssl","version":"3.0.13-0ubuntu3.4"}],
  "truncated":false}`. `q` is required and must be at least **2 characters** (`400
  invalid_request` otherwise); the match is a case-insensitive substring of the package name
  across the caller's visible devices. An optional `org_id` narrows the search to that one org;
  an `org_id` the caller cannot see is `404 not_found` (RS2), before the search. Results are
  ordered by org name, hostname then package name and capped at 500 rows (`truncated:true` when
  the cap cut the result).
- `POST /api/v1/devices/{id}/commands` (admin|operator) with one of
  `{"kind":"exec","argv":["uname","-a"],"timeout_s":60}`,
  `{"kind":"script","shell":"sh","body":"df -h\n","timeout_s":120}`,
  `{"kind":"lock","timeout_s":30}`, `{"kind":"ping","timeout_s":30}`,
  `{"kind":"message","text":"Please call the helpdesk","timeout_s":30}`
  -> `201 {"command":COMMAND}`.
  Errors **(A1)**: `404 {"error":"not_found"}` when the device is not visible to the
  technician (tenant isolation; a lock on a device outside the visible org set is 404 like any
  other access); `400 {"error":"unsupported_on_platform"}` for exec/script on an android
  device; `400 {"error":"unsupported_on_device"}` for any kind on a revoked device;
  `400 {"error":"invalid_request","detail":"..."}` on validation failure (`Command.Validate()`
  text, e.g. shell not available on the platform); `403 forbidden` for viewer.
  Audit `command.queued` (plus `device.lock_requested` for lock), then `Notify(device)`.
- `GET /api/v1/devices/{id}/commands?limit=50` -> `{"commands":[COMMAND...]}` newest first.
- `GET /api/v1/commands/{id}` -> `{"command":COMMAND}`.
- `POST /api/v1/devices/{id}/patch` (admin|operator; CSRF) **(PM1)** with
  `{"action":"install","mode":"security","ids":["openssl"],"reboot":"never","timeout_s":3600}`
  (every field optional: defaults install / security / never / 3600; a scan ignores mode/ids/reboot)
  -> `201 {"command":COMMAND}` (a queued `kind patch` command).
  Validation order (mirrors the shell open): device exists (`404`) -> org visible (`404` across
  orgs, RS2) -> not revoked (`400 unsupported_on_device`) -> desktop platform
  (`400 unsupported_on_platform`, i.e. android) -> body shape (`400 invalid_request`, via
  `Command.Validate`) -> for `mode:ids`, every id a package in the device's stored report where the
  server has one (`400 invalid_request` naming the unknown ids; without a report the agent checks)
  -> online (`409 device_offline`). Then the command is queued, audited
  (`patch.scan_requested` / `patch.install_requested` with mode/ids/reboot) and the device's parked
  sync woken. `403 forbidden` for viewer. The install log comes back on the command result like
  exec/script, visible in the command history.

```
COMMAND = {"id":7,"device_id":42,"org_id":1,"created_by":1,"created_by_email":"admin@example.com",
           "kind":"exec","argv":["uname","-a"],"shell":"","body":"","text":"","timeout_s":60,
           "status":"done","created_at":"...","sent_at":"...","started_at":"...","finished_at":"...",
           "exit_code":0,"stdout":"Linux ...","stderr":"","truncated":false,"error":""}
```

`status` in `queued|sent|done|failed|timeout|lost|cancelled`; nullable times are `null`;
`argv` is `[]`/`null`-free (an empty array) for non-exec kinds; `text` is `""` except for message.
A `kind patch` command **(PM1)** additionally carries `action`, `mode`, `reboot` and `ids`
(`omitempty`, so they appear only on a patch COMMAND); every other kind omits them.

- `POST /api/v1/devices/{id}/revoke` (admin) -> `204` (`token_hash` NULL; audit `device.revoked`).
- `DELETE /api/v1/devices/{id}` (admin) -> `204` (cascade; audit `device.deleted`).

### Remote desktop (RD1)

- `GET /api/v1/rustdesk` (any role) -> `{"enabled":true,"host":"rmm.example.com","key":"<id_ed25519.pub line>"}`;
  `{"enabled":false,"host":"","key":""}` when the server has no relay. The dashboard's
  Remote desktop card and the technician's own client setup read it.
- `POST /api/v1/devices/{id}/remote-desktop` (admin|operator; CSRF header; empty body) ->
  `200 {"url":"rustdesk://connect/123456789","rustdesk_id":"123456789","host":"rmm.example.com","key":"..."}`.
  `url` is the RustDesk URL scheme for a remote-control session (`rustdesk://connect/<id>`,
  from the client source: `get_uri_prefix()` + the `connect` authority); the dashboard
  navigates to it so the technician's installed client opens the session.
  Errors: `404 {"error":"not_found"}` when the device is unknown or revoked (same scope
  rule as lock: staff accounts see every org, so there is no per-technician org set;
  nothing else is distinguishable);
  `409 {"error":"rustdesk_disabled"}` when the server has no relay configured;
  `409 {"error":"no_rustdesk_id"}` when the agent has not reported an id; `403 forbidden`
  for viewer. A `200` writes audit `device.remote_desktop_opened` (actor the user, target the
  device, org, client IP, detail `hostname`, `platform`, `rustdesk_id`, `host`); refused
  requests write nothing.

### Remote shell (RS1, D3)

An interactive browser terminal brokered by the server over a WebSocket relay (desktop only).

- `POST /api/v1/devices/{id}/shell` (admin|operator; CSRF header; body `{"shell":"bash","cols":120,"rows":32}`,
  every field optional) -> `201 {"command":COMMAND,"ticket":"<43-char base64url>","ws_path":"/api/v1/shell/{id}/ws"}`.
  The command is queued as `kind shell`; `ticket` is a single-use browser credential valid 60 s.
  Validation order: device exists (`404`) -> not revoked (`400 unsupported_on_device`) ->
  desktop platform (`400 unsupported_on_platform`, i.e. android) -> body shape
  (`400 invalid_request`) -> online (`409 device_offline`) -> no other `queued|sent` shell for the
  device (`409 shell_busy`) -> under the 20-shell server cap (`429 too_many_shells`). Audit
  `shell.opened` (actor user; detail hostname, platform, shell, cols, rows, command_id), then
  `Notify(device)`.
- The `COMMAND` JSON for a shell command gains a `shell_session` block (null until the relay's
  session row exists): `{"bytes_in":N,"bytes_out":N,"close_reason":"<reason>|","transcript_truncated":bool,"connected_at":ts|null}`.
- `GET /api/v1/shell/{id}/ws?ticket=...` (**browser leg**; session cookie, same role rule as the
  POST) -> `101`. The `Origin` header host must equal `Host` (case-insensitive; missing Origin ->
  `403`); the ticket must match the one issued for this command, be unused and younger than 60 s
  (consumed on handshake). The browser may connect before the agent and then receives
  `status waiting_for_agent`; its bytes are dropped until the agent attaches.
- `GET /api/agent/v1/shell/{command_id}` (**agent leg**; `Authorization: Bearer dt_...`, no cookie,
  no CSRF) -> `101`. The server checks a valid device token, the command belongs to that device,
  is `kind shell` and status `sent`, and no agent socket is already attached (else `409`). On
  attach the command stays `sent`, `connected_at` is set, audit `shell.connected` (actor device),
  and a waiting browser gets `status connected`.
- **Framing** (identical on both legs): **binary** frames are raw terminal bytes
  (browser→agent = keystrokes to PTY stdin; agent→browser = PTY output); **text** frames are JSON
  control — browser→agent `{"type":"resize","cols":N,"rows":N}`; agent→browser `{"type":"exit","code":N}`
  just before it closes; server→browser only `{"type":"status","state":"waiting_for_agent"|"connected"|"closed","reason":"<close_reason>"}`.
- **Relay**: frames are copied verbatim; the server counts `bytes_in` (browser→agent) and
  `bytes_out` (agent→browser), appends agent→browser bytes to the transcript (cap 1 MiB, then
  `transcript_truncated=true` and recording stops while relaying continues), and resets the idle
  timer on browser input. Close reasons: `technician_closed` / `agent_closed` (a leg closed),
  `idle_timeout` (30 min no browser input), `max_duration` (`timeout_s`), `server_shutdown`,
  `agent_never_connected` (no agent within 60 s of the POST). On close the server completes the
  command `done` (an exit 0 arrived, or the technician closed a live session) else `failed`
  (`exit_code` from the exit frame or -1, `error` = close reason), writes the `shell_sessions` row
  and audits `shell.closed` (actor user for `technician_closed`, else system; detail duration_s,
  bytes_in, bytes_out, reason). At most 20 concurrent shells per server.
- `GET /api/v1/shell/{id}/transcript` (admin|operator, same org rule) -> `200 text/plain; charset=utf-8`,
  the recorded agent→browser bytes (served from the live buffer while open, from the flushed row
  after close). `404` when the command is not a shell command.

The dashboard CSP (`connect-src 'self'`, section 6 "Other paths and headers") already permits the
same-origin WebSocket; no `ws:`/`wss:` source is added.

### Presets

- `GET /api/v1/presets` (any role) ->
  `{"presets":{"linux":[PRESET...],"windows":[PRESET...]},"source":"builtin"|"file","path":"<MSP_PRESETS or empty>"}`

```
PRESET = {"label":"Disk usage","kind":"script","shell":"sh","body":"df -h\n","timeout_s":60}
       | {"label":"Uptime","kind":"exec","argv":["uptime"],"timeout_s":60}
```

The library behind the device console's "Presets…" dropdown. Entries are returned in file
order with only the keys of their kind (`argv` for exec; `shell` + `body` for script) and
`timeout_s` always filled in. Both arrays are always present (`[]` when empty).

`source` is `builtin` (the set compiled into `internal/server/presets.go`: Uptime, Disk
usage, Top processes, Reboot; System info, Drives, Top processes, Reboot) unless `MSP_PRESETS`
names a file, which replaces the builtin set entirely. File format (template
`deploy/presets.example.json`):

```json
{"linux":[PRESET...],"windows":[PRESET...]}
```

- `label`: 1..60 characters, unique within its platform.
- `kind`: `exec` or `script` only (lock, ping and message are actions, never presets).
- `argv` (exec only): `len >= 1`, each element <= 4096 bytes, as `Command.Validate`, and
  each element a single trimmed non-empty line (no newline or carriage return, no leading or
  trailing whitespace): the console shows argv one element per line and sends it back that
  way, so an element that cannot survive that round trip is refused by the loader.
- `shell` (script only): `sh|bash` under `linux`, `powershell|cmd` under `windows`.
- `body` (script only): non-empty, <= 262144 bytes; a body without a trailing newline gets
  one appended by the loader.
- `timeout_s`: optional, 1..3600; omitted = 60. `0` is an error (unlike the command API).
- Unknown keys anywhere are an error (`DisallowUnknownFields`); a platform key may be
  omitted (then that platform has no presets).

Loading: at startup an unreadable or invalid file makes `msp-server serve` exit 1 with
`presets file <path>: <platform>[<index>] "<label>": <problem>`. At runtime every GET
re-reads the file when its mtime or size changed; an invalid rewrite is logged once as
`WARN presets reload failed, keeping the last good set` with the same detail and the last
good set keeps being served.

### File library and deployment (SD1)

A server-side file library holds deployable artifacts; agents download them over the
device-token endpoint (section 3.7) and a `kind deploy` command pushes one to a device or installs
a named package. Metadata lives in a `files` table, the bytes on disk under `MSP_FILES_DIR/<id>`
(created on start). The four staff endpoints are org-scoped exactly like RS2: a scoped
operator/viewer sees only files whose `org_id` is in their visible set, a global (`org_id` NULL)
file is visible only to all-orgs users, and a cross-org single file is `404 not_found`. Size cap
`MSP_MAX_FILE_MB` (default 200), over it `413 file_too_large`.

- `POST /api/v1/files` (admin|operator; CSRF; `multipart/form-data`: a `file` part plus `name`
  and optional `org_id`) -> `201 {"file":FILE}`. Streams the part to a temp file in
  `MSP_FILES_DIR` while computing its sha256 and size, then inserts the row and renames the bytes
  into `MSP_FILES_DIR/<id>`. Admin, or an operator for an org they can see; a global file
  (`org_id` omitted) requires all-orgs (`403 forbidden` otherwise). Exempt from the 512 KiB
  dashboard body limit (it enforces its own `MSP_MAX_FILE_MB` cap). Audit `file.uploaded` (name,
  size, sha256).

```
FILE = {"id":12,"org_id":1,"name":"installer.sh","size":1024,"sha256":"<64 hex>",
        "content_type":"application/x-sh","created_at":"...","created_by":1,
        "created_by_email":"admin@example.com"}
```

`org_id` is `null` for a global artifact usable by any client; `created_by`/`created_by_email`
name the uploader (`null`/`""` when unknown).

- `GET /api/v1/files?org_id=` (viewer) -> `{"files":[FILE...]}` newest first, org-scoped; never
  streams the bytes.
- `GET /api/v1/files/{id}` (viewer) -> `{"file":FILE}`; `404 not_found` when missing or not
  visible (RS2).
- `DELETE /api/v1/files/{id}` (admin|operator) -> `204`; removes the row and the disk file.
  `404 not_found` across orgs before the delete. Audit `file.deleted`.

- `POST /api/v1/devices/{id}/deploy` (admin|operator; CSRF) with one of
  `{"source":"file","file_id":12,"dest":"/opt/acme/installer.sh","run":true,"run_args":["--silent"],"timeout_s":3600}`
  or `{"source":"package","package":"htop","timeout_s":3600}`
  -> `201 {"command":COMMAND}` (a queued `kind deploy` command). The caller never supplies the
  `sha256`: the server looks the library file up and fills the deploy command with the stored
  hash, so a deploy cannot be pointed at a forged digest.
  Validation order (mirrors the patch open): device exists (`404`) -> org visible (`404` across
  orgs, RS2) -> not revoked (`400 unsupported_on_device`) -> desktop platform
  (`400 unsupported_on_platform`, i.e. android) -> body shape (`400 invalid_request`, via
  `Command.Validate`) -> for a file source, the file must be visible for the device's org or
  global (`404 not_found` otherwise) -> online (`409 device_offline`). Then the command is
  queued, audited (`deploy.requested` with source, file_id/package, dest, run) and the device's
  parked sync woken. `403 forbidden` for viewer. The download + install/run log comes back on the
  command result like exec/script, visible in the command history. A `kind deploy` COMMAND
  additionally carries `source`, `file_id`, `sha256`, `dest`, `run`, `run_args` and `package`
  (`omitempty`, so they appear only on a deploy COMMAND).

### Agent releases and self-update (AU1)

The MSP publishes the cross-built agent binaries to `MSP_AGENT_DIR` (served at `/dl/`) together
with a manifest `agents.json`:

```json
{"version":"0.2.0","files":{
   "linux-amd64":{"name":"msp-agent-linux-amd64","sha256":"<hex>"},
   "linux-arm64":{"name":"msp-agent-linux-arm64","sha256":"<hex>"},
   "windows-amd64":{"name":"msp-agent-windows-amd64.exe","sha256":"<hex>"}}}
```

`version` is the single released version; each entry's `name` is the file served at `/dl/<name>`
and `sha256` is its digest (the agent re-verifies it after download). The server loads the file on
start and re-reads it when its mtime/size changes (like the preset library); an absent or
malformed file simply turns self-update off (no update advertised, `version:""`). `scripts/build-agents.sh`
writes `agents.json` with the real digests alongside the binaries and `SHA256SUMS`.

- `GET /api/v1/agent-release` (viewer) -> the current manifest `{"version":"0.2.0","files":{...}}`,
  or `{"version":"","files":{}}` when self-update is off. Drives the released-version banner and the
  fleet "outdated agent" filter.
- `POST /api/v1/devices/{id}/update` (admin|operator; CSRF), optional body `{"timeout_s":600}`
  -> `201 {"command":COMMAND}` (a queued `kind update` command). The caller never supplies the
  `version`/`url`/`sha256`: the server fills them from the manifest for the device's `platform-arch`.
  Validation order (mirrors the deploy open): device exists (`404`) -> org visible (`404` across
  orgs, RS2, before any side effect) -> not revoked (`400 unsupported_on_device`) -> desktop
  platform (`400 unsupported_on_platform`, i.e. android) -> a newer release exists for the device's
  platform-arch (`409 no_update_available` when the manifest has nothing, or the device already runs
  the released version) -> online (`409 device_offline`). Then the command is queued, audited
  (`agent.update_requested` with `from_version`, `to_version`) and the device's parked sync woken.
  `403 forbidden` for viewer. The update log (old→new, outcome, any rollback) comes back on the
  command result.

### Script library, bulk jobs and automations (AUT1)

A DB-backed, editable **script library** (distinct from the static `presets.json` console presets),
a **bulk run** that fans a script out across many devices as one tracked **job**, and **automations**
that produce jobs on a schedule or when an alert opens. Everything reuses the existing command queue
(the agent runs the resulting commands unchanged — the agent wire COMMAND is unchanged, only the
dashboard COMMAND JSON gains `job_id`), RS2 org-scoping and audit. A command created by a job carries
`job_id` (`omitempty`) in the dashboard COMMAND.

```
SCRIPT = {"id":5,"org_id":1,"name":"restart-nginx","description":"","platform":"linux",
          "kind":"exec","argv":["systemctl","restart","nginx"],"shell":"","body":"",
          "timeout_s":300,"created_at":"...","created_by":1}
```

`org_id` is `null` for a global script usable by any client (visible to all-orgs users only, exactly
like a global file/channel). `platform` is `linux`, `windows` or `any`; `kind` is `exec` (an `argv`)
or `script` (a `shell` + `body`). exec/script are desktop kinds, so a fan-out skips android.

- `GET /api/v1/scripts` (viewer) -> `{"scripts":[SCRIPT...]}` newest first, org-scoped (RS2).
- `POST /api/v1/scripts` (admin|operator; CSRF) body `{name,description,platform,kind,argv|shell+body,
  timeout_s,org_id?}` -> `201 {"script":SCRIPT}`. A global script (`org_id` omitted) requires all-orgs.
  Audit `script.created`.
- `GET /api/v1/scripts/{id}` (viewer) -> `{"script":SCRIPT}`; `404 not_found` across orgs (RS2).
- `PUT /api/v1/scripts/{id}` (admin|operator) -> `{"script":SCRIPT}`; `org_id` is immutable. Audit
  `script.updated`.
- `DELETE /api/v1/scripts/{id}` (admin|operator) -> `204`; a job that used it keeps its rows
  (`script_id ON DELETE SET NULL`); an automation that referenced it is removed
  (`script_id ON DELETE CASCADE`). Audit `script.deleted`.

A **job** groups the per-device commands created in one fan-out; its progress is derived by counting
those commands by status (the four buckets sum to `total`).

```
JOB      = {"id":7,"org_id":1,"name":"restart-nginx","origin":"manual","script_id":5,
            "created_by":1,"automation_id":null,"created_at":"...","total":2,
            "progress":{"job_id":7,"total":2,"queued":0,"sent":0,"done":2,"failed":0}}
PROGRESS = {"job_id":7,"total":2,"queued":0,"sent":0,"done":2,"failed":0}
```

`origin` is `manual` (a bulk run), `schedule` or `trigger` (an automation). `progress.failed` folds
in every terminal non-success status (failed/timeout/lost/cancelled) so the four columns sum to
`total`. A job's `org_id` is `null` when it spans several visible orgs (a global script run across
them); a scoped user then does not see it.

- `POST /api/v1/jobs` (admin|operator; CSRF) body
  `{"script_id":5 | "command":{platform,kind,argv|shell+body,timeout_s}, "target":{org_id?,status?,
  platform?,hostname_q?,device_ids?}, "name"?}` -> `201 {"job_id":7,"total":2,"devices":[JC...]}`.
  Resolves the caller's **visible** devices matching the target (RS2: a device outside the caller's
  org set, or a target `org_id`/`device_ids` outside the script's client, is `404 not_found` before
  any side effect), creates a job and one command per device. A device whose platform cannot run the
  script (android for exec/script, or a platform-specific script on another platform) is **skipped**
  with a per-device note and no command; `total` counts only the commands actually created. Audit
  `job.created` (origin, name, total, skipped, script_id/kind). `JC` is a per-device fan-out row:
  `{"device_id":42,"hostname":"ws-01","org_id":1,"command_id":9,"skipped":false,"note":""}`
  (`command_id` omitted on a skip; `note` carries the reason).
- `GET /api/v1/jobs?org_id=` (viewer) -> `{"jobs":[JOB...]}` newest first, org-scoped (RS2); each
  row also carries a derived `progress`.
- `GET /api/v1/jobs/{id}` (viewer) -> `{"job":JOB, "progress":{job_id,total,queued,sent,done,failed},
  "devices":[DR...]}`; `404 not_found` across orgs. `progress` and `devices` are **top-level**, not
  nested in `job`. `DR` is a per-device row for each command the job queued:
  `{"device_id":42,"hostname":"ws-01","org_id":1,"org_name":"Acme","command_id":9,"status":"done",
  "skipped":false,"note":""}` (platform/android skips queue no command and so appear only in the
  `POST /jobs` fan-out breakdown, not here).

An **automation** is a saved rule that produces jobs: `trigger:"schedule"` (run by the server
scheduler) or `trigger:"alert"` (run from the alert-open hook on the alerting device).

```
AUTOMATION = {"id":3,"org_id":1,"name":"nightly-reboot-check","enabled":true,"trigger":"schedule",
              "script_id":5,"target":{"platform":"linux"},"schedule":{"every_minutes":1440},
              "alert":null,"last_run_at":null,"next_run_at":"...","created_at":"...","created_by":1}
```

`target`/`schedule`/`alert` are emitted as **parsed objects** (the server stores them as JSON strings
internally but every HTTP response parses them for the dashboard). `schedule` is set for
`trigger:"schedule"` and `alert` is set for `trigger:"alert"`; the other is `null`.

- `GET /api/v1/automations` (viewer) -> `{"automations":[AUTOMATION...]}` org-scoped (RS2).
- `POST /api/v1/automations` (admin|operator; CSRF) body `{name,trigger,script_id,org_id?,
  target:{...}, schedule:{every_minutes} | alert:{alert_type?,min_severity?}, enabled?}` ->
  `201 {"automation":AUTOMATION}`. A schedule automation needs `schedule.every_minutes >= 1` and no
  alert matcher; an alert automation needs no schedule. A global automation requires all-orgs; the
  script and target must stay inside the automation's client (else `404`/`400`). The first
  `next_run_at` is seeded to `now + every_minutes` for an enabled schedule. Audit `automation.created`.
- `PUT /api/v1/automations/{id}` (admin|operator) -> `{"automation":AUTOMATION}`; `org_id` immutable.
  Audit `automation.updated`.
- `POST /api/v1/automations/{id}/enable` (admin|operator) body `{"enabled":true|false}` ->
  `{"automation":AUTOMATION}`; re-seeds `next_run_at` on enable, clears it on disable (keeps
  `last_run_at`). Audit `automation.enabled` / `automation.disabled`.
- `DELETE /api/v1/automations/{id}` (admin|operator) -> `204`; a job it produced survives
  (`automation_id` is a plain column, no FK). Audit `automation.deleted`.

The **scheduler** is a job in the server jobs loop (every 30 s, fake-clock-injectable via `now()`):
each tick it runs every enabled schedule automation whose `next_run_at <= now`, resolving the target
to the automation's org-scoped visible devices, fanning the script out under a job (origin schedule),
and advancing `next_run_at = now + every_minutes` (computed from `now`, so **missed ticks do not
stack**). Audit `automation.ran` (actor `system`). A **triggered** automation fires from the
alert-open hook (the same `OpenAlert`-returned-true path notifications use): on a newly opened alert,
each enabled alert automation matching the alert's org, `alert_type` and `min_severity` runs its
script on the **alerting device** under a job (origin trigger), subject to a per-automation-per-device
**cooldown** (10 min) so a flapping alert cannot hammer a device. Audit `automation.triggered` (actor
`system`). Both run actions attribute the resulting commands to the automation's owner
(`automations.created_by`; `commands.created_by` is NOT NULL).

### Alerts, audit, users

- `GET /api/v1/alerts?state=open|all&org_id=` -> `{"alerts":[{"id":9,"org_id":1,"org_name":"...","device_id":42,"hostname":"ws-01","type":"offline","severity":"high","message":"no sync for 6m","opened_at":"...","resolved_at":null,"acknowledged_by":null,"acknowledged_at":null}]}` (default open; limit 500). Filtered to the caller's visible orgs **(RS2)**; an explicit `?org_id=` outside the visible set is `404 not_found`.
- `POST /api/v1/alerts/{id}/ack` (admin|operator) -> `204`. Audit `alert.ack`. `404 not_found`
  when the alert's org is not visible **(RS2)**, resolved before the ack so there is no side effect.
- `GET /api/v1/audit?limit=200&org_id=&before=<id>` -> `{"entries":[{"id":1,"at":"...","actor_type":"user","actor_id":1,"actor_label":"admin@example.com","org_id":1,"action":"command.queued","target_type":"command","target_id":7,"ip":"127.0.0.1","detail":{...}}]}` newest first; `actor_label` = user email, `"device:<id>"` or `"system"`. Filtered to the caller's visible orgs **(RS2)**; rows with `org_id` null (system actions not tied to an org) are visible only to all-orgs users.
- `GET /api/v1/users` (admin) -> `{"users":[{"id":1,"email":"...","role":"admin","created_at":"...","disabled_at":null,"all_orgs":true,"org_ids":[]}]}`.
  `all_orgs` **(RS2)** is true when the user sees every org; `org_ids` lists the assigned orgs
  and is empty when `all_orgs`. An admin row is always `all_orgs:true` with an empty `org_ids`.
  Each row also carries `totp_enabled` (TF1), so the Users page can badge who has 2FA on; the
  secret is never included.
- `POST /api/v1/users {"email","password","role"}` (admin; password >= 10 chars; role in
  `admin|operator|viewer`) -> `201 {"user":{...}}`; `409 conflict`. Audit `user.created`.
  For an operator or viewer the body may add `"all_orgs":false,"org_ids":[1,2]` to scope the new
  user **(RS2)** (every id must exist, else `400 invalid_request`; an empty list with `all_orgs:false`
  is allowed and means the user sees nothing yet). The scoping fields are ignored for the admin role
  (admins are always all-orgs); a missing `all_orgs` keeps today's full visibility.
- `PUT /api/v1/users/{id}/orgs {"all_orgs":bool,"org_ids":[...]}` (admin) -> `200 {"user":{...}}`
  **(RS2)**: replaces the user's org assignment. `404 not_found` when the user does not exist;
  `400 invalid_request` when the target is an admin and `all_orgs` is false (admins cannot be
  scoped) or an `org_id` does not exist. Audit `user.orgs_changed` with the new set.
- `POST /api/v1/users/{id}/disable` (admin) -> `204`. Audit `user.disabled`.

A scoped user who is later **promoted to admin** becomes all-orgs at once (the visible-org rule
keys off role); **demoting an admin** to operator keeps `all_orgs:true` until an admin scopes them
through `PUT …/orgs`.

### Notifications (NS1 + NS2)

Alert notifications: email (SMTP), webhook (Slack-compatible + generic JSON) and **SMS**
(Twilio or a generic JSON gateway), routed per client and severity, suppressed during
maintenance windows, with a retrying dispatcher and a delivery log. NS2 adds the `sms` channel
kind and **multi-step escalation policies** (`/api/v1/notify/policies`), both org-scoped exactly
like the NS1 resources. Every resource is **org-scoped for staff visibility exactly like RS2**: a
scoped operator/viewer sees and manages only channels/rules/policies/windows/deliveries whose
`org_id` is in their visible set; a global `org_id null` row is visible only to all-orgs users; a
cross-org single resource is `404 not_found`, resolved before any side effect (docs/SECURITY.md
section 4). GETs require `viewer`; writes require `operator`; a global (`org_id null`) row may be
created only by an all-orgs caller (`403 forbidden` otherwise). CSRF header on every write.

**Channel secret is write-only.** A channel's `secret` (SMTP password or webhook signing
secret) is never returned; a channel is shown with `"secret_set":true|false`. On `PUT`, omitting
`secret` leaves the stored value unchanged, sending `""` is rejected (`400 invalid_request`), and
sending a value replaces it.

- `GET /api/v1/notify/channels` -> `{"channels":[CHANNEL...]}` (org-scoped).

```
CHANNEL = {"id":3,"kind":"email"|"webhook"|"sms","name":"Ops email","org_id":1|null,
           "config":{...},"secret_set":true,"enabled":true,"created_at":"...","created_by":1}
```

`config` for email: `{"host","port","from","to":[...],"starttls":bool,"username"}`; for webhook:
`{"url","flavor":"slack"|"generic"}`; for **sms** (NS2):
`{"provider":"twilio"|"generic","from":"+1555...","to":["+1555..."],"account_sid":"AC...","url":"https://..."}`.
The channel `secret` is the provider auth token (write-only, as for the other kinds). `twilio`
POSTs the Messages API form (From/To/Body) with HTTP basic auth (`account_sid` : token); `url` is
an optional self-hosted/test override (the dashboard drops it, addressing the account by
`account_sid`). `generic` POSTs `{to,text}` JSON to `url` with the token as a `Bearer` header
(`account_sid` is dropped). The SMS body is a short one-line summary
(`"[MSP] <severity> <type> on <hostname> (<org>)"`, trimmed to ~300 chars, any URL reduced to
`scheme://host`); a tokened link can never ride out in a text. Validation: provider in the enum,
a `from` and at least one `to` E.164-ish number, `account_sid` for twilio, `url` for generic.
- `POST /api/v1/notify/channels {"kind","name","org_id":1|null,"config":{...},"secret":"...","enabled":true}`
  (admin, or operator for an org they can see) -> `201 {"channel":CHANNEL}`. Validates the
  kind-specific config (`400 invalid_request` with a detail). Audit `notify.channel_created`.
- `PUT /api/v1/notify/channels/{id}` (same auth + org scope; `org_id` and `kind` are immutable) ->
  `200 {"channel":CHANNEL}`. `404 not_found` across orgs. Audit `notify.channel_updated`.
- `DELETE /api/v1/notify/channels/{id}` -> `204` (rules cascade; the delivery log keeps the
  dangling `channel_id`). Audit `notify.channel_deleted`.
- `POST /api/v1/notify/channels/{id}/test` -> `201 {"delivery":DELIVERY}`: enqueues a `test`
  delivery immediately (status `pending`); the dispatcher sends it on its next tick. Audit
  `notify.channel_tested`.
- `GET /api/v1/notify/rules` -> `{"rules":[RULE...]}`.

```
RULE = {"id":5,"name":"High to Slack","org_id":1|null,"min_severity":"low"|"medium"|"high",
        "alert_types":["offline","disk_full"],"on_open":true,"on_resolve":false,
        "channel_id":3,"escalation_policy_id":7|null,"enabled":true,"created_at":"...","created_by":1}
```

`alert_types` empty = all types. A rule fires when org matches (rule `org_id` null or == the
alert's org), `alert severity >= min_severity`, the type is listed (or the list is empty), and
the event matches (`on_open` for opened, `on_resolve` for resolved). `escalation_policy_id` (NS2,
optional) attaches a policy that drives timed steps **in addition to** the rule's own `on_open`
channel: the immediate `on_open` channel still fires at open; the policy adds the later steps.
- `POST /api/v1/notify/rules`, `PUT /api/v1/notify/rules/{id}` (`org_id` immutable on update),
  `DELETE /api/v1/notify/rules/{id}` (same auth + org scope). `channel_id` and (when set)
  `escalation_policy_id` must reference a channel/policy the caller can see (`400 invalid_request`
  otherwise, not revealing another org's resource). Audit `notify.rule_created`/`updated`/`deleted`.

#### Escalation policies (NS2)

A policy is an ordered list of steps; each step fires its channels `after_minutes` from the
alert opening, but only while the alert is still **open and unacknowledged**. Acknowledging or
resolving the alert stops escalation at once; a resolve still honours `on_resolve` rules.
- `GET /api/v1/notify/policies` -> `{"policies":[POLICY...]}` (org-scoped).

```
POLICY = {"id":7,"name":"Critical escalation","org_id":1|null,
          "steps":[{"after_minutes":0,"channel_ids":[21]},{"after_minutes":15,"channel_ids":[22]}],
          "created_at":"...","created_by":1}
```

- `POST /api/v1/notify/policies {"name","org_id":1|null,"steps":[{"after_minutes","channel_ids":[...]}]}`
  -> `201 {"policy":POLICY}`. At least one step, at most 10; `after_minutes` 0..40320 (4 weeks) and
  non-decreasing across steps; each step needs >=1 visible channel (`400 invalid_request`
  otherwise). Audit `notify.policy_created`.
- `PUT /api/v1/notify/policies/{id}` (`org_id` immutable; replaces name + full step list) ->
  `200 {"policy":POLICY}`. `404 not_found` across orgs. Audit `notify.policy_updated`.
- `DELETE /api/v1/notify/policies/{id}` -> `204` (steps cascade; any rule pointing at it is
  detached, `escalation_policy_id` set null). Audit `notify.policy_deleted`.
- `GET /api/v1/notify/windows` -> `{"windows":[WINDOW...]}`.

```
WINDOW = {"id":2,"org_id":1|null,"starts_at":"...","ends_at":"...","reason":"patching",
          "active_now":true,"created_at":"...","created_by":1}
```

One-shot absolute times; `active_now` is computed server-side (`starts_at <= now < ends_at`).
- `POST /api/v1/notify/windows {"org_id":1|null,"starts_at","ends_at","reason"}`
  (`ends_at` must be after `starts_at`), `DELETE /api/v1/notify/windows/{id}`. Audit
  `notify.window_created`/`deleted`.
- `GET /api/v1/notify/deliveries?status=pending|sent|failed|suppressed&org_id=&limit=` ->
  `{"deliveries":[DELIVERY...]}` newest first (read, org-scoped). The delivery log.

```
DELIVERY = {"id":9,"alert_id":9|null,"rule_id":5|null,"channel_id":3|null,"org_id":1|null,
            "event":"opened"|"resolved"|"test","status":"pending"|"sent"|"failed"|"suppressed",
            "attempts":0,"last_error":"","detail":{...redacted...},"step":2|null,
            "created_at":"...","updated_at":"..."}
```

One row per `(alert_id, rule_id, channel_id, event, step)` (a UNIQUE index makes enqueue
idempotent so the dispatcher, or the escalation tick, never double-sends; a `test` row has
`alert_id` null and is always its own row). `step` (NS2) is the 1-based escalation step number for
an escalation delivery and `null` for an ordinary open/resolve/test delivery (stored as `0`); for
an NS1 row `channel_id` is the rule's single channel and the key reduces to the old
`(alert, rule, event)` guarantee. An alert newly opened (`OpenAlert` returned created) or resolved
enqueues one `pending` row per matching rule; the escalation tick enqueues one row per channel per
due step of a matched policy-bearing rule while the alert is open+unacked; each is `suppressed`
when the alert's org is inside an active maintenance window at enqueue time. The dispatcher retries
with backoff and lands `failed` after `maxAttempts`. `detail` and `last_error` are redacted of any
secret or tokened URL (docs/SECURITY.md).

### Reports and exports (RP1)

Read-only, RS2 org-scoped aggregates over the data the platform already stores (heartbeats and the
denormalised device patch columns); no new table, no agent change. All are `GET`, admin|operator|viewer
(reports are read-only, so a viewer may read them). The period `days` defaults to `7`, must be a positive
integer and is rejected (`400 invalid_request`) when non-numeric, non-positive or greater than
`MSP_RETENTION_DAYS` (default 7) — a report can only cover what retention keeps. A malformed `org_id` is
`400`; an explicit `org_id`/`device_id` outside the caller's visible org set is `404 not_found` (never a
distinguishable 403), resolved before any work. **`uptime_pct` is a check-in approximation** ("did the
agent report in"), not OS uptime: `expected` is the number of throttled heartbeat windows the device's
`interval_s` (floored at the 1/min sample throttle) would produce over the window, prorated from
`enrolled_at`; `samples` is the heartbeat rows seen; `uptime_pct = 100 * min(samples/expected, 1)`. The
device's own OS uptime is the unrelated `uptime_s` (the last heartbeat's figure), shown alongside.

- `GET /api/v1/reports/uptime?org_id=&days=N` ->
  `{"days", "since", "mean_uptime_pct", "device_count",
    "rows":[{"device_id","hostname","org_id","org_name","samples","expected","uptime_pct","uptime_s"}]}`.
  `mean_uptime_pct` is the unweighted mean of the rows' `uptime_pct`.
- `GET /api/v1/reports/disk?device_id=&days=N` -> one RS2-visible device's disk series:
  `{"device_id","hostname","org_id","org_name","days","since",
    "series":[{"at","disk_used_pct"}], "min","max","latest"}`. Heartbeats with a NULL disk reading are
  skipped; `min`/`max`/`latest` are `null` when the series is empty. `device_id` is required (missing or
  invalid -> `400`; unknown or invisible device -> `404`).
- `GET /api/v1/reports/disk/top?org_id=&days=N&limit=` ->
  `{"days","rows":[{"device_id","hostname","org_id","org_name","latest"}]}` — the devices with the
  highest latest in-window disk usage, highest first (`limit` default 10, max 100; only devices with an
  in-window disk sample).
- `GET /api/v1/reports/patch-compliance?org_id=` ->
  `{"overall":<counts>, "orgs":[{"org_id","org_name","counts":<counts>}],
    "devices":[{"device_id","hostname","org_id","org_name","pending","security","reboot_required","checked_at"}]}`
  where `<counts>` is `{"total","up_to_date","pending","security_pending","reboot_required","never_checked"}`.
  `up_to_date`/`pending`/`never_checked` partition `total` (`up_to_date` = checked with 0 pending);
  `security_pending` and `reboot_required` are additional overlapping flags on the checked set.
  `never_checked` includes platforms that never scan (e.g. android) — this is the current denormalised
  state, not windowed.
- CSV exports mirror the JSON rows: `GET /api/v1/reports/uptime.csv?org_id=&days=N`,
  `GET /api/v1/reports/disk.csv?device_id=&days=N`, `GET /api/v1/reports/patch-compliance.csv?org_id=`.
  `Content-Type: text/csv; charset=utf-8`, `Content-Disposition: attachment; filename="msp-<report>-<YYYY-MM-DD>.csv"`,
  CRLF-framed. Columns: uptime = `device_id,hostname,org_id,org_name,samples,expected,uptime_pct,os_uptime_s`;
  disk = `at,disk_used_pct`; patch-compliance = `device_id,hostname,org_id,org_name,pending,security,reboot_required,checked_at`.
  Text cells (hostname, org_name) are formula-injection-guarded: a leading `=`,`+`,`-`,`@`,TAB or CR is
  prefixed with a single quote so a spreadsheet renders it as text (docs/SECURITY.md).

### Other paths and headers

- `GET /healthz` -> `200 {"ok":true}` when the DB answers `SELECT 1`, else `503`.
- `GET /` and any path not under `/api` or `/dl` -> embedded SPA (`index.html` fallback,
  `Cache-Control: no-cache`).
- `GET /dl/<file>` -> static file from `MSP_AGENT_DIR` (no directory listing; 404 when unset
  or missing).
- Security headers on every response:
  `Content-Security-Policy: default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'`,
  `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
  `Strict-Transport-Security: max-age=31536000` when serving TLS.

## 7. Audit actions (`audit_log.action`)

`user.login`, `user.login_failed`, `user.created`, `user.disabled`, `user.orgs_changed` **(RS2)**,
`user.password_changed`
(actor the user, or `system` with detail `via: cli`), `user.password_change_failed`, `org.created`,
`token.created`, `token.revoked`, `device.enrolled`, `device.revoked`, `device.deleted`,
`device.lock_requested` **(A1)**, `device.remote_desktop_opened` **(RD1)**,
`shell.opened`, `shell.connected`, `shell.closed` **(RS1)**,
`patch.scan_requested`, `patch.install_requested` **(PM1)**,
`file.uploaded`, `file.deleted`, `deploy.requested` **(SD1)**, `agent.update_requested` **(AU1)**,
`script.created`, `script.updated`, `script.deleted`, `job.created`, `automation.created`,
`automation.updated`, `automation.enabled`, `automation.disabled`, `automation.deleted`,
`automation.ran`, `automation.triggered` **(AUT1**; the two run actions are actor `system`**)**, `command.queued`,
`command.finished`, `command.lost`, `alert.ack`,
`notify.channel_created`, `notify.channel_updated`, `notify.channel_deleted`,
`notify.channel_tested`, `notify.rule_created`, `notify.rule_updated`, `notify.rule_deleted`,
`notify.window_created`, `notify.window_deleted` **(NS1)**.

## 8. Alert thresholds

`offline` = status offline (`3*interval_s + 15 s`), severity high; `disk_full` =
`disk_used_pct >= 90`, medium, message `disk 92.5% used on /data`; `memory_high` =
`mem_used_pct >= 95`, low; `battery_low` = `battery_pct <= 15` and not charging, low. Each type
has at most one open row per device and auto-resolves when the condition clears; ack is a flag
on the open row.

`disk_used_pct` (Fleet Disk column, heartbeats, `disk_full`) is the fullest filesystem among
those that count: at least 2 GB and not `/boot`, `/boot/*` or `/efi` (a separate boot
partition is small and routinely 90 % full on a healthy host). When no reported filesystem
counts, the fullest of all of them is used. Every filesystem the agent reports stays visible
on the device page.

## 9. CLI contracts

**Server** (`cmd/msp-server`): `msp-server serve`;
`msp-server user add --email E --role admin|operator|viewer --password-stdin` (reads the
password from stdin); `msp-server user set-password --email E --password-stdin` (replaces the
password and ends every session of that user; audit `user.password_changed` actor system);
`msp-server org add --name N` (prints the new org id as a bare integer);
`msp-server token new --org ID --label L --max-uses N --ttl 24h` (prints only the plaintext
token); `msp-server version`.
Env (flags of the same name override): `MSP_DB` (`./msp.db`), `MSP_LISTEN` (`:8080`),
`MSP_BASE_URL` (`""`), `MSP_TLS_CERT` + `MSP_TLS_KEY` (serve TLS when both set),
`MSP_SECURE_COOKIES` (0/1), `MSP_TRUST_PROXY` (0/1: honour the first `X-Forwarded-For`),
`MSP_AGENT_DIR` (`""`), `MSP_RETENTION_DAYS` (7), `MSP_PRESETS` (`""`: JSON command preset
library, section 6 "Presets"; `--presets`), `MSP_RUSTDESK_HOST` + `MSP_RUSTDESK_KEY` (`""`;
`--rustdesk-host` / `--rustdesk-key`: the self-hosted RustDesk relay as a bare hostname or IP
(no port: 21116/21117 are implied) and its public key, one base64 line; both enable remote
desktop, neither disables it, one alone makes `serve` exit 2 with "must be set together", a
malformed value exits 2 with the reason);
`MSP_FILES_DIR` (`""` -> `<dir of MSP_DB>/files`; `--files-dir`: the file-library directory,
created on `serve` start; artifacts live at `<MSP_FILES_DIR>/<id>`) **(SD1)**;
`MSP_MAX_FILE_MB` (200; `--max-file-mb`: the upload size cap in MiB, must be ≥ 1) **(SD1)**.

**Agent** (`cmd/msp-agent`):
`msp-agent enroll --server URL --token et_... [--config PATH] [--ca-file PEM] [--interval 30] [--insecure-http]`
(enrolls, writes config, exits); `msp-agent run [--config PATH] [--interval N]` (foreground;
under the Windows SCM it runs as the service);
`msp-agent install --server URL --token et_... [--ca-file] [--interval] [--insecure-http]`
(enroll + write config + register and start the systemd unit / Windows service;
root/Administrator); `msp-agent uninstall`; `msp-agent status` (prints config path, device id,
server, relay host when known); `msp-agent rustdesk setup [--password-stdin]
[--config-string S] [--config PATH]` **(RD1)** (root/Administrator: points the installed
RustDesk client at the relay from the config, sets the permanent password with
`rustdesk --password`, generated 16 characters unless one line of at least 8 is piped in
with `--password-stdin`, prints it once and writes it 0600 (Windows: SYSTEM + Administrators
DACL) to `rustdesk-password` next to the config, restarts the client service, prints the id
from `rustdesk --get-id`; exit 2 when the config has no relay or a malformed one, or no
client is installed);
`msp-agent rustdesk id` (prints the local id, exit 1 when none); `msp-agent version`.
Config file JSON `{"server_url","device_token","device_id","interval_s","ca_file","insecure_http","rustdesk_host","rustdesk_key"}`
(the last two optional, written when the server reports a relay) at `/etc/msp-agent/agent.json`
(Linux) or `%ProgramData%\msp-agent\agent.json` (Windows), dir 0700 file 0600. Exit codes:
0 ok, 1 runtime error, 2 bad arguments/config.

## 10. Fixtures

| file                          | type              | notes                                                      |
|-------------------------------|-------------------|------------------------------------------------------------|
| `enroll_linux.json`           | EnrollRequest     | amd64, interval 30, full facts                             |
| `enroll_android.json`         | EnrollRequest     | arm64, interval 60, `serial` empty                         |
| `sync_linux.json`             | SyncRequest       | wait_s 25, battery absent, facts present                   |
| `sync_android.json`           | SyncRequest       | cpu_pct null, one `/data` disk, battery 87 / false, wait_s 0, no facts |
| `sync_response.json`          | SyncResponse      | poll 30; exec `uname -a` (60 s) + script sh `df -h\nuptime\n` (120 s) |
| `sync_response_android.json`  | SyncResponse      | poll 60; lock, ping, message (A1 kinds)                    |
| `sync_rustdesk.json`          | SyncRequest       | like `sync_linux.json` with `facts.rustdesk_id` (RD1)      |
| `sync_response_rustdesk.json` | SyncResponse      | poll 30, no commands, `rustdesk` host + key (RD1)          |
| `sync_android_apps.json`      | SyncRequest       | like `sync_android.json` with facts and `facts.apps` (5 entries: 3 user apps incl. a label fallback and an empty version, then 2 system apps) |
| `command_result.json`         | CommandResult     | exit 0, RFC3339 UTC times                                  |
| `sync_patches.json`           | SyncRequest       | like `sync_linux.json` with a `patches` report (PM1): apt, reboot required, three updates incl. a security one |
| `command_deploy.json`         | Command           | kind `deploy`, source `file` (SD1): file_id, sha256, dest, run, run_args |
| `command_update.json`         | Command           | kind `update` (AU1): version, url, sha256, timeout_s        |
| `sync_response_update.json`   | SyncResponse      | poll 30, no commands, `agent_update` version/url/sha256 (AU1) |
| `error.json`                  | ErrorResponse     | `invalid_device_token`, no detail                          |

A change to any wire shape must update `types.go`, the fixture, `types_test.go` and this
document in one commit; the Android `ProtocolTest` reads the same fixtures and fails until
`Protocol.kt` follows.
