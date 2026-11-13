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
- `facts` is always an object (every key present, empty strings allowed), with one optional
  key **(RD1)**: `rustdesk_id`, the id of the locally installed RustDesk client, 1..32 ASCII
  digits, omitted when RustDesk is not installed (`400 invalid_request`, detail
  `facts.rustdesk_id must be 1..32 digits`, otherwise). The server stores it in
  `devices.rustdesk_id`; an enrollment without it keeps a previously stored id.

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
`command.finished` actor_type=device.

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

Command kinds are exactly `exec`, `script`, `lock`, `ping`, `message`. The Go form is
`protocol.Command{ID, Kind, Argv, Shell, Body, Text, TimeoutS}`; `Command.Validate()` checks
the shape, `KindSupportedOn(kind, platform)` checks the platform, `KindsFor(platform)` and
`ShellsFor(platform)` drive the UI.

| kind      | platforms               | fields                         | agent behaviour                                   |
|-----------|-------------------------|--------------------------------|---------------------------------------------------|
| `exec`    | linux, windows          | `argv` (len >= 1, each <= 4096 bytes) | `exec.CommandContext(argv[0], argv[1:]...)`, no shell |
| `script`  | linux, windows          | `shell`, `body` (non-empty, <= 262144 bytes) | body written to a 0600 file, run by the fixed interpreter argv |
| `lock`    | linux, windows, android | none                           | locks the interactive session(s)                   |
| `ping`    | linux, windows, android | none                           | exit 0, stdout `pong`                              |
| `message` | linux, windows, android | `text` (non-empty, <= 4096 bytes) | shows the text to the logged-in user            |

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

- `GET /api/v1/summary` -> `{"orgs":4,"devices":57,"online":51,"offline":5,"pending":1,"open_alerts":3}`
- `GET /api/v1/orgs` -> `{"orgs":[{"id":1,"name":"Acme Dental","created_at":"...","device_count":12,"online_count":11,"open_alerts":1}]}`
- `POST /api/v1/orgs {"name"}` (admin) -> `201 {"org":{...}}`; `409 {"error":"conflict"}` on
  duplicate. Audit `org.created`.
- `GET /api/v1/orgs/{id}` -> `{"org":{...},"tokens":[{"id":3,"org_id":1,"token_prefix":"et_Ab12cD3","label":"front desk","max_uses":10,"uses":2,"expires_at":"...","created_by":1,"created_at":"...","revoked_at":null}]}`
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

- `GET /api/v1/devices?org_id=&status=online|offline|pending&q=<hostname substring>` -> `{"devices":[DEVICE...]}`

```
DEVICE = {"id":42,"org_id":1,"org_name":"Acme Dental","device_uid":"f2c1...","hostname":"ws-01",
          "platform":"linux","arch":"amd64","os_version":"Ubuntu 24.04.1 LTS","agent_version":"0.1.0",
          "interval_s":30,"status":"online","enrolled_at":"...","last_seen_at":"...","last_ip":"10.0.0.12",
          "revoked":false,"cpu_pct":12.5,"mem_used_pct":50.8,"disk_used_pct":42.3,"battery_pct":null,
          "battery_charging":null,"uptime_s":86400,"open_alerts":0,"can_run_commands":true,
          "rustdesk_id":null}
```

`rustdesk_id` **(RD1)** is the id the agent last reported in its facts, `null` until then.

`can_run_commands` is true when the device is a desktop platform (`linux|windows`) and not
revoked: it gates the free-form exec/script console. **(A1)** The built-in kinds lock, ping and
message are available for every non-revoked device regardless of `can_run_commands`; the
dashboard decides from `platform` which controls to show.

- `GET /api/v1/devices/{id}` -> `{"device":DEVICE,"facts":{...facts object...},"metrics":{...last metrics object...},"alerts":[open ALERT...],"commands":[last 20 COMMAND without stdout/stderr]}`
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

```
COMMAND = {"id":7,"device_id":42,"org_id":1,"created_by":1,"created_by_email":"admin@example.com",
           "kind":"exec","argv":["uname","-a"],"shell":"","body":"","text":"","timeout_s":60,
           "status":"done","created_at":"...","sent_at":"...","started_at":"...","finished_at":"...",
           "exit_code":0,"stdout":"Linux ...","stderr":"","truncated":false,"error":""}
```

`status` in `queued|sent|done|failed|timeout|lost|cancelled`; nullable times are `null`;
`argv` is `[]`/`null`-free (an empty array) for non-exec kinds; `text` is `""` except for message.

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

- `GET /api/v1/alerts?state=open|all&org_id=` -> `{"alerts":[{"id":9,"org_id":1,"org_name":"...","device_id":42,"hostname":"ws-01","type":"offline","severity":"high","message":"no sync for 6m","opened_at":"...","resolved_at":null,"acknowledged_by":null,"acknowledged_at":null}]}` (default open; limit 500).
- `POST /api/v1/alerts/{id}/ack` (admin|operator) -> `204`. Audit `alert.ack`.
- `GET /api/v1/audit?limit=200&org_id=&before=<id>` -> `{"entries":[{"id":1,"at":"...","actor_type":"user","actor_id":1,"actor_label":"admin@example.com","org_id":1,"action":"command.queued","target_type":"command","target_id":7,"ip":"127.0.0.1","detail":{...}}]}` newest first; `actor_label` = user email, `"device:<id>"` or `"system"`.
- `GET /api/v1/users` (admin) -> `{"users":[{"id":1,"email":"...","role":"admin","created_at":"...","disabled_at":null}]}`
- `POST /api/v1/users {"email","password","role"}` (admin; password >= 10 chars; role in
  `admin|operator|viewer`) -> `201 {"user":{...}}`; `409 conflict`. Audit `user.created`.
- `POST /api/v1/users/{id}/disable` (admin) -> `204`. Audit `user.disabled`.

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

`user.login`, `user.login_failed`, `user.created`, `user.disabled`, `user.password_changed`
(actor the user, or `system` with detail `via: cli`), `user.password_change_failed`, `org.created`,
`token.created`, `token.revoked`, `device.enrolled`, `device.revoked`, `device.deleted`,
`device.lock_requested` **(A1)**, `device.remote_desktop_opened` **(RD1)**, `command.queued`,
`command.finished`, `command.lost`, `alert.ack`.

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
| `command_result.json`         | CommandResult     | exit 0, RFC3339 UTC times                                  |
| `error.json`                  | ErrorResponse     | `invalid_device_token`, no detail                          |

A change to any wire shape must update `types.go`, the fixture, `types_test.go` and this
document in one commit; the Android `ProtocolTest` reads the same fixtures and fails until
`Protocol.kt` follows.
