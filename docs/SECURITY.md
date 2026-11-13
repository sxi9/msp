# Security

What the system protects, what it does not, and what must be true before it grows.

## 1. Threat model, stated plainly

- **Any operator or admin account is root on every enrolled Linux machine, SYSTEM on every
  Windows machine, and can lock every enrolled phone.** Commands and scripts run with the
  agent's privileges by design; there is no sandbox between the dashboard and the endpoint.
  Protect staff accounts accordingly: unique strong passwords, no shared logins, disable
  accounts the day someone leaves (`POST /api/v1/users/{id}/disable`). Everyone changes their
  own password on the Account page (`POST /api/v1/me/password`, requires the current one and
  ends the user's other sessions); a forgotten or compromised password is reset on the server
  with `msp-server user set-password`, which ends every session of that user. Change the
  bootstrap admin password on day one.
- **Server compromise is fleet compromise.** Whoever controls the server (or its database file)
  can queue commands for every device. Keep the server patched, the database and backups
  private, and the dashboard reachable from as few networks as possible.
- A compromised **endpoint** can impersonate only itself: a device token lets it sync and post
  results for its own commands, nothing else. It cannot read other devices, orgs or users.
- A leaked **enrollment token** lets an attacker enroll rogue devices into one org until the
  token expires, is used up or revoked. Those devices then receive whatever operators queue for
  them, which is why rogue hostnames in the Fleet page deserve attention.
- Out of scope for phase 1: a malicious insider with operator rights, physical access to
  endpoints, and the network between agent and server when TLS is not used.

## 2. Secrets and hashing

| secret            | format                              | stored as                               | lifetime                        |
|-------------------|-------------------------------------|-----------------------------------------|---------------------------------|
| staff password    | >= 10 chars                         | bcrypt, cost 12                         | until changed                   |
| session cookie    | `ss_` + 32 random bytes (base64url) | sha256 hex                              | 12 h, deleted on logout         |
| enrollment token  | `et_` + 32 random bytes             | sha256 hex + first 10 chars for display | ttl (default 7 d), max uses, revocable |
| device token      | `dt_` + 32 random bytes             | sha256 hex in `devices.token_hash`      | until revoke or re-enrollment   |

Every token is generated with `crypto/rand`, shown exactly once, and only its hash is kept, so
the database alone does not let anyone authenticate. Revocation is immediate: a revoked device
token (`token_hash` NULL) gets `401 invalid_device_token` on the next request; re-enrollment
rotates the token and invalidates the old one. On endpoints the device token sits in
`/etc/msp-agent/agent.json` (root, 0600), `%ProgramData%\msp-agent\agent.json` (administrators)
or app-private SharedPreferences; anyone with root/admin on the endpoint can read it and
impersonate that one device. The published Android APK is the release build (not debuggable,
so `adb run-as` cannot read the preferences on an unrooted phone; signed with the MSP's own
key); the debug build is lab-only and never published (`docs/OPS.md` section 3).

## 3. Dashboard session, CSRF, rate limits, headers

- Session cookie `msp_session`: `HttpOnly`, `SameSite=Strict`, `Path=/`, `Secure` when serving
  TLS or `MSP_SECURE_COOKIES=1`. Run behind TLS in production; without it the cookie travels in
  clear.
- CSRF: every non-GET `/api/v1` request must carry `X-Requested-With: msp`, else `403 csrf`.
  Browsers do not add custom headers to cross-site requests, and `SameSite=Strict` keeps the
  cookie off cross-site requests entirely; the two together block CSRF even on older browsers.
- Rate limits (in-memory per client IP): login and password change 10 attempts/min,
  enrollment 60 requests/min (a client site behind one NAT address imaging a room of
  machines; the desktop agent retries a 429), all answering `429 rate_limited`. Failed logins
  and password changes are audited (`user.login_failed`, `user.password_change_failed`). Set
  `MSP_TRUST_PROXY=1` only behind a proxy you control, or the limits key on a spoofable header.
- Headers on every response: `Content-Security-Policy: default-src 'self'; style-src 'self'
  'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'`,
  `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
  and `Strict-Transport-Security: max-age=31536000` when serving TLS. The dashboard loads no
  third-party scripts, fonts or styles.
- Request bodies are capped (256 KiB agent, 512 KiB dashboard); command output is capped at
  64 KiB per stream by the agent.

## 4. Roles

`viewer` < `operator` < `admin`. Viewers read everything and change nothing. Operators also
create enrollment tokens, queue commands (exec, script, lock, ping, message) and acknowledge
alerts. Admins also manage orgs, users, and revoke or delete devices. All staff see all orgs
in phase 1; per-client logins are phase 2.

## 5. Agent transport policy

- `https://` always. `http://` is accepted only for loopback addresses or with the explicit
  `--insecure-http` flag (lab use). There is no "skip certificate verification" option;
  private CAs are added with `--ca-file`. Android release builds refuse cleartext at the
  platform level and trust only the system store.
- Agents only connect outbound; no port is opened on any client machine.
- A device token is bound to one device row; a result posted for a command that belongs to
  another device is `404`, so a command queued for one machine can never be reported, let
  alone executed, by another. Agents execute only commands delivered in their own sync response.

## 6. Command execution model

- `exec` runs an argv array through `exec.CommandContext`: no shell, metacharacters are
  literal (`["echo","a;id"]` prints `a;id`), PATH lookup only for `argv[0]`.
- `script` writes the body to a 0600 file in a fresh 0700 directory and runs it with a fixed
  interpreter argv (`sh`/`bash` on Linux; `powershell.exe -NoProfile -NonInteractive
  -ExecutionPolicy Bypass -File` or `cmd.exe /C` on Windows). The directory is removed afterwards.
- Timeouts: default 60 s, max 3600 s. On deadline Linux kills the whole process group
  (`Setpgid` + `kill(-pgid)`); Windows kills the direct child only (job objects are phase 2),
  so a PowerShell script that starts a long-running process can leave it behind.
- Output is capped at 64 KiB per stream (`truncated: true`), environment is the service's own
  plus `MSP_COMMAND_ID`, at most 2 commands run concurrently per device, and a command is
  claimed exactly once: a lost response leaves it `sent` until the sweeper marks it `lost`;
  nothing is ever executed twice.
- Every queued command is audited (`command.queued`) with the actor, the argv, or the shell
  plus the script's sha256 and its first 200 characters, or the message text; every result
  is audited (`command.finished`, actor the device) and stored in full in the command row.

## 7. Remote lock scope

Lock is a built-in command kind, never free text. A technician (operator or admin; never a
viewer) can lock only a device that is **enrolled under this server's tenant tree and visible
in their org set and holds a valid device token**; any other device id is `404`, and there is
no endpoint that can reach a device that is not enrolled here. On the endpoint the agent runs
only its fixed lock routine (`loginctl lock-sessions` / `LockWorkStation` in the console
session / `DevicePolicyManager.lockNow()` under device admin), with no parameters from the
request. Every lock writes two audit rows, `command.queued` and `device.lock_requested`,
with the requesting user, the device and the client IP, and the result is recorded like any
other command. The same scope rule applies to `ping` and `message`.

## 8. Remote desktop (RustDesk relay)

Phase A of D4 (feature RD1) adds a second channel beside the agent: the official RustDesk
client on the endpoint, talking only to a RustDesk ID/relay server (`hbbs`/`hbbr`) that the
MSP hosts. What that changes:

- **Relay exposure.** The relay host opens 21115/tcp, 21116/tcp+udp and 21117/tcp to the
  internet (21118/21119 stay closed; the web client is not used). hbbs/hbbr accept any
  client that knows the server's public key; the key is not a secret (every client carries
  it), so the relay is reachable by anyone who learns it. It carries only end-to-end
  encrypted RustDesk sessions and cannot start one: a session still needs the target id and
  its password. Keep the image pinned (`rustdesk/rustdesk-server:1.1.16`) and updated like
  any exposed service, and keep `deploy/rustdesk-data/id_ed25519` (the private key) with the
  database backups: whoever holds it can impersonate the relay.
- **Permanent password.** `msp-agent rustdesk setup` sets a 16-character random permanent
  password on the endpoint's client (or the line piped in with `--password-stdin`; there is
  no argv flag for it, because a process command line is readable by every local user for
  as long as it runs). It is printed once by the installer and stored in `rustdesk-password`
  next to `agent.json`: 0600 under a 0700 directory on Linux; on Windows, where Go's mode
  bits only toggle the read-only attribute, the agent itself sets a DACL granting SYSTEM and
  BUILTIN\Administrators only, inheritance disabled, on `%ProgramData%\msp-agent` and on the
  file (from `msp-agent install`, every config save and `rustdesk setup`; it is an error,
  not a warning, when that fails). The installer's `icacls` does the same earlier and is not
  what the guarantee rests on. The one exposure that cannot be avoided is the documented
  `rustdesk --password <pw>` call: the password is on that process's command line for the
  seconds it runs (`/proc/<pid>/cmdline`, WMI), so run setup on a quiescent machine.
  **It is never sent to or stored on the MSP server**, so a server or database compromise
  does not yield screen access; a compromised endpoint reveals only its own password, as
  with the device token. The MSP must keep the passwords in its own vault. Phase B
  (`docs/RUSTDESK-BRANDING.md`) and a per-session one-time password are the follow-ups.
- **Who can connect.** `POST /api/v1/devices/{id}/remote-desktop` is operator/admin only,
  CSRF-protected, and answers only for a device that is enrolled on this server and not
  revoked (404 otherwise: the same scope rule as lock). Staff accounts are not scoped to an
  org: every operator and admin of the MSP can reach every org's devices, as for every other
  action, until the org-scoped logins of section 11 exist. The MSP server never joins the
  session: it hands the technician's browser `rustdesk://connect/<id>` and the technician's
  own client, configured for the same relay and key, does the rest. Possession of a staff
  account plus the device password is what opens a screen; neither alone does.
- **The id is self-reported.** `facts.rustdesk_id` is whatever the agent asserts (digits
  only, validated for shape); the server cannot check that it belongs to that machine, and a
  reported id is kept until the agent reports another one (it is not cleared when RustDesk
  is removed). Whoever holds a device token, or the endpoint's OS, can therefore point the
  device page at a RustDesk id they control: the technician's session then lands on that
  machine and the password typed into it is captured, and the audit row names the spoofed
  id. Before entering the device's permanent password, compare the id in the RustDesk
  connection dialog with the one on the device page and with the one printed at install
  time, and treat a changed id like a changed SSH host key. The per-session one-time
  password above is the structural fix.
- **Audit.** Every answered request writes `device.remote_desktop_opened` (actor user,
  device, org, client IP, the RustDesk id and the relay host). Refused requests write nothing.
  Session duration and screen content are not recorded by the MSP server in phase A; the
  RustDesk clients keep their own connection logs.
- **Endpoint side.** The id is read with `rustdesk --get-id` and reported in facts as digits
  only (`<= 32`), validated on the server (400 otherwise); it is not a secret. A local admin
  on the endpoint can re-point the official client at another relay (phase B closes that).
  The relay settings the agent receives from the server are stored in `agent.json` (0600 /
  the Windows DACL above). Host and key are validated on both ends (`MSP_RUSTDESK_HOST` a
  bare hostname or IP, the key one base64 line) because the agent writes them verbatim into
  TOML literal strings in `RustDesk2.toml`: a value with a quote or newline could otherwise
  inject further `[options]` keys, and a `:port` would point the relay at the ID-server port.
  A malformed block from the server is ignored and logged, never stored.
- **The relay is opt-in.** `hbbs`/`hbbr` are in the compose profile `rustdesk`; a plain
  `up -d` starts neither, so a deployment that never configured remote desktop has nothing
  listening on 21115-21119.

## 9. Remote shell (RS1)

Feature RS1 (decision D3) adds an interactive terminal relayed through the server. Unlike remote
desktop, the MSP server is on the path, so the controls are the server's to enforce.

- **Who.** Only admin|operator may open a shell (viewer never), CSRF header required, and the
  target device must be in the caller's org set, desktop, online and not revoked. Android and
  revoked devices are refused (`unsupported_on_platform` / `unsupported_on_device`).
- **Two credentials, two legs.** The browser leg is authenticated by the staff **session cookie**
  plus a **single-use ticket**: a 43-character base64url value, bound to one command, consumed on
  the handshake and rejected once used or older than 60 s (constant-time compared). The agent leg
  is authenticated by the device bearer token (`dt_`) exactly like sync, and the server refuses a
  second agent socket for the same command (`409`).
- **Origin pinning.** The browser WebSocket handshake requires the `Origin` host to equal the
  request `Host` (a missing or cross-origin `Origin` is `403`), so a page on another origin cannot
  open a relay with a stolen cookie. The dashboard CSP's `connect-src 'self'` already confines the
  terminal's own connection to the same origin.
- **Single, bounded relay.** The server keeps at most 20 concurrent shells and one relay goroutine
  set per session (in memory, keyed by command id under a mutex). Each session ends on the first
  of: either leg closing, 30 min idle (no browser input), the `timeout_s` max duration (default
  4 h, max 8 h), or server shutdown; the agent enforces its own idle timeout as a backstop and
  runs a single shell at a time.
- **Transcript retention.** The server records the agent→browser output to a transcript capped at
  1 MiB (then `transcript_truncated` is set and recording stops while relaying continues), flushed
  once to the `shell_sessions` row on close and served by `GET /api/v1/shell/{id}/transcript` to
  admin|operator under the same org rule. Keystrokes (browser→agent) are counted but never
  recorded, so a password typed at a prompt is not stored. The transcript lives in the database
  and is removed with the command (cascade) and your retention of the DB; treat it as sensitive.
- **Output is never logged.** Neither the server nor the agent writes terminal bytes to its logs;
  the relay copies frames without logging their contents. Logs carry only metadata (command id,
  device, byte counts, close reason).
- **Completion and audit.** Every open writes `shell.opened` (actor user), every agent attach
  `shell.connected` (actor device) and every close `shell.closed` (actor the user for a
  technician-closed session, else system; detail duration, byte counts and close reason). The
  three rows bound the whole session in the append-only audit.

## 10. Audit

`audit_log` is append-only: the store exposes insert and list only, there is no update or
delete path, and it is never pruned. Each row names the actor type (`user`, `device`, `system`),
the actor, the org, the action, the target and the client IP. Actions: logins (success and
failure), user and org changes, token creation and revocation, enrollment, revocation and
deletion of devices, command queued/finished/lost, lock requests, remote desktop sessions
opened, remote shell opened/connected/closed, alert acknowledgements.
Export it with your backups; SIEM forwarding is phase 2.

## 11. Preconditions before scaling to many clients (phase 2)

The phase-1 controls are adequate for a pilot with a handful of trusted technicians. Before
the Nth client, in this order:

1. **MFA (TOTP)** on every staff account; operator rights without MFA are a fleet-wide risk.
2. **Command signing**: operator-held keys sign queued commands and agents verify them, so a
   server compromise alone cannot push commands.
3. **Device token rotation** with overlap, and Keystore wrapping on Android.
4. **Org-scoped logins** so a client's own staff see only their devices, and dashboard IP
   allowlisting at the proxy.
5. Windows job-object process-tree kill and a reviewed script library.

## 12. Reporting a vulnerability

Report privately to the maintainers of this repository (open a security advisory on the
repository, or e-mail the address listed in its README once published). Please include the
version (`msp-server version`), reproduction steps and impact; do not open a public issue
first. Fixes ship as a new version with a note in the release description.
