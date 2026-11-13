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

## 8. Audit

`audit_log` is append-only: the store exposes insert and list only, there is no update or
delete path, and it is never pruned. Each row names the actor type (`user`, `device`, `system`),
the actor, the org, the action, the target and the client IP. Actions: logins (success and
failure), user and org changes, token creation and revocation, enrollment, revocation and
deletion of devices, command queued/finished/lost, lock requests, alert acknowledgements.
Export it with your backups; SIEM forwarding is phase 2.

## 9. Preconditions before scaling to many clients (phase 2)

The phase-1 controls are adequate for a pilot with a handful of trusted technicians. Before
the Nth client, in this order:

1. **MFA (TOTP)** on every staff account; operator rights without MFA are a fleet-wide risk.
2. **Command signing**: operator-held keys sign queued commands and agents verify them, so a
   server compromise alone cannot push commands.
3. **Device token rotation** with overlap, and Keystore wrapping on Android.
4. **Org-scoped logins** so a client's own staff see only their devices, and dashboard IP
   allowlisting at the proxy.
5. Windows job-object process-tree kill and a reviewed script library.

## 10. Reporting a vulnerability

Report privately to the maintainers of this repository (open a security advisory on the
repository, or e-mail the address listed in its README once published). Please include the
version (`msp-server version`), reproduction steps and impact; do not open a public issue
first. Fixes ship as a new version with a note in the release description.
