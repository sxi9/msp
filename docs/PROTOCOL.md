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
  `apps`, the installed-apps inventory (section 3.2a), sent by the Android agent and
  omitted by the desktop agents in phase 1.

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
| `apps`         | [App]    | optional: installed apps, Android only in phase 1 (3.2a)     |

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
- Desktop agents do not send `apps` in phase 1 (installed-software inventory on desktops is
  on the roadmap).

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
  `sync_android_apps.json`; an invalid entry -> `400 invalid_request` naming the index).
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
  "count":2,"security_count":1,"error":"",
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
`patch` **(PM1)**.
The Go form is `protocol.Command{ID, Kind, Argv, Shell, Body, Text, Action, Mode, IDs, Reboot,
TimeoutS, Cols, Rows, ShellSession}` (`Action`/`Mode`/`IDs`/`Reboot` belong to `patch`, the last
three to `shell`); `Command.Validate()` checks the shape,
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
  `user.login` / `user.login_failed`.
- `POST /api/v1/logout` -> `204`.
- `GET /api/v1/me` -> `{"user":{...}}`.
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
          "patches_checked_at":"2026-11-14T10:00:00Z"}
```

`rustdesk_id` **(RD1)** is the id the agent last reported in its facts, `null` until then.

`pending_patches`, `security_patches`, `reboot_required` and `patches_checked_at` **(PM1)** are
denormalised from the device's last patch report (section 3.6) by `RecordPatches`; they are on every
DEVICE row (so the list carries the fleet "Updates" column) and `patches_checked_at` is `null` until
the first report. The full report is **not** in DEVICE — it is a sibling `patches` object on the
device-detail response (below).

`can_run_commands` is true when the device is a desktop platform (`linux|windows`) and not
revoked: it gates the free-form exec/script console. **(A1)** The built-in kinds lock, ping and
message are available for every non-revoked device regardless of `can_run_commands`; the
dashboard decides from `platform` which controls to show.

- `GET /api/v1/devices/{id}` -> `{"device":DEVICE,"facts":{...facts object...},"metrics":{...last metrics object...},"patches":{...patch report...},"alerts":[open ALERT...],"commands":[last 20 COMMAND without stdout/stderr]}`.
  `facts` is the last facts object the agent sent, verbatim, so for an Android device it
  carries the `apps` array (section 3.2a); the dashboard shows it as the "Installed apps" panel.
  `patches` **(PM1)** is the full `protocol.PatchReport` the agent last sent (section 3.6), `{}` for
  a device that never reported; the dashboard's "Updates" panel reads it.
- `GET /api/v1/devices/{id}/heartbeats?limit=120` -> `{"heartbeats":[{"at":"...","cpu_pct":12.5,"mem_used_pct":50.8,"disk_used_pct":42.3,"battery_pct":null,"uptime_s":86400}]}` oldest first.
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

### Alerts, audit, users

- `GET /api/v1/alerts?state=open|all&org_id=` -> `{"alerts":[{"id":9,"org_id":1,"org_name":"...","device_id":42,"hostname":"ws-01","type":"offline","severity":"high","message":"no sync for 6m","opened_at":"...","resolved_at":null,"acknowledged_by":null,"acknowledged_at":null}]}` (default open; limit 500). Filtered to the caller's visible orgs **(RS2)**; an explicit `?org_id=` outside the visible set is `404 not_found`.
- `POST /api/v1/alerts/{id}/ack` (admin|operator) -> `204`. Audit `alert.ack`. `404 not_found`
  when the alert's org is not visible **(RS2)**, resolved before the ack so there is no side effect.
- `GET /api/v1/audit?limit=200&org_id=&before=<id>` -> `{"entries":[{"id":1,"at":"...","actor_type":"user","actor_id":1,"actor_label":"admin@example.com","org_id":1,"action":"command.queued","target_type":"command","target_id":7,"ip":"127.0.0.1","detail":{...}}]}` newest first; `actor_label` = user email, `"device:<id>"` or `"system"`. Filtered to the caller's visible orgs **(RS2)**; rows with `org_id` null (system actions not tied to an org) are visible only to all-orgs users.
- `GET /api/v1/users` (admin) -> `{"users":[{"id":1,"email":"...","role":"admin","created_at":"...","disabled_at":null,"all_orgs":true,"org_ids":[]}]}`.
  `all_orgs` **(RS2)** is true when the user sees every org; `org_ids` lists the assigned orgs
  and is empty when `all_orgs`. An admin row is always `all_orgs:true` with an empty `org_ids`.
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

### Notifications (NS1)

Alert notifications: email (SMTP) and webhook (Slack-compatible + generic JSON), routed per
client and severity, suppressed during maintenance windows, with a retrying dispatcher and a
delivery log. All four resources are **org-scoped for staff visibility exactly like RS2**: a
scoped operator/viewer sees and manages only channels/rules/windows/deliveries whose `org_id`
is in their visible set; a global `org_id null` row is visible only to all-orgs users; a
cross-org single resource is `404 not_found`, resolved before any side effect (docs/SECURITY.md
section 4). GETs require `viewer`; writes require `operator`; a global (`org_id null`) row may be
created only by an all-orgs caller (`403 forbidden` otherwise). CSRF header on every write.

**Channel secret is write-only.** A channel's `secret` (SMTP password or webhook signing
secret) is never returned; a channel is shown with `"secret_set":true|false`. On `PUT`, omitting
`secret` leaves the stored value unchanged, sending `""` is rejected (`400 invalid_request`), and
sending a value replaces it.

- `GET /api/v1/notify/channels` -> `{"channels":[CHANNEL...]}` (org-scoped).

```
CHANNEL = {"id":3,"kind":"email"|"webhook","name":"Ops email","org_id":1|null,
           "config":{...},"secret_set":true,"enabled":true,"created_at":"...","created_by":1}
```

`config` for email: `{"host","port","from","to":[...],"starttls":bool,"username"}`; for webhook:
`{"url","flavor":"slack"|"generic"}`.
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
        "channel_id":3,"enabled":true,"created_at":"...","created_by":1}
```

`alert_types` empty = all types. A rule fires when org matches (rule `org_id` null or == the
alert's org), `alert severity >= min_severity`, the type is listed (or the list is empty), and
the event matches (`on_open` for opened, `on_resolve` for resolved).
- `POST /api/v1/notify/rules`, `PUT /api/v1/notify/rules/{id}` (`org_id` immutable on update),
  `DELETE /api/v1/notify/rules/{id}` (same auth + org scope). `channel_id` must reference a
  channel the caller can see (`400 invalid_request` otherwise, not revealing another org's
  channel). Audit `notify.rule_created`/`updated`/`deleted`.
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
            "attempts":0,"last_error":"","detail":{...redacted...},"created_at":"...","updated_at":"..."}
```

One row per `(alert_id, rule_id, event)` (a UNIQUE index makes enqueue idempotent so the
dispatcher never double-sends; a `test` row has `alert_id` null and is always its own row). An
alert newly opened (`OpenAlert` returned created) or resolved enqueues one `pending` row per
matching rule, or `suppressed` when the alert's org is inside an active maintenance window at
enqueue time. The dispatcher retries with backoff and lands `failed` after `maxAttempts`.
`detail` and `last_error` are redacted of any secret or tokened URL (docs/SECURITY.md).

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
`patch.scan_requested`, `patch.install_requested` **(PM1)**, `command.queued`,
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
malformed value exits 2 with the reason).

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
| `sync_patches.json`           | SyncRequest       | like `sync_linux.json` with a `patches` report (PM1): apt, reboot required, two updates incl. a security one |
| `error.json`                  | ErrorResponse     | `invalid_device_token`, no detail                          |

A change to any wire shape must update `types.go`, the fixture, `types_test.go` and this
document in one commit; the Android `ProtocolTest` reads the same fixtures and fails until
`Protocol.kt` follows.
