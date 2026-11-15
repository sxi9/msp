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
| channel secret **(NS1)** | SMTP password / webhook signing secret | **plaintext** in `notify_channels.secret` | until changed or the channel is deleted |
| TOTP secret **(TF1)** | 20 random bytes, base32 | **plaintext** in `users.totp_secret` | until 2FA is disabled/reset (then cleared) |
| recovery code **(TF1)** | `xxxxx-xxxxx` base32 (10 per user) | sha256 hex in `recovery_codes.code_hash` | single-use; replaced on regenerate, deleted on disable |
| pending-login token **(TF1)** | `pt_` + 32 random bytes | sha256 hex, in-memory only | 5 min, consumed on the second login step |

The channel secret is the one exception to hashing: it is a credential the server must *use* to
authenticate an SMTP session or to compute an HMAC, not a value it verifies, so it is stored in
the clear (like any outbound password a mailer keeps). It is **write-only over the API** — never
returned (`"secret_set":bool` only), never logged, and never placed in a delivery's `detail_json`;
see section 12. The TOTP secret is the same kind of exception: the server must recompute the code
to verify it, so it is kept (like the channel secret) in the clear, but it is **write-only** —
handed back exactly once by `setup` so the owner can enrol, then never returned by `me`, the users
list or any log, and cleared when 2FA is disabled or reset. See section 13.

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
  machines; the desktop agent retries a 429), all answering `429 rate_limited`. The 2FA second
  login step (`/login/totp`) and the TOTP confirm/disable/regenerate endpoints share the same
  login budget (TF1), so guessing a 6-digit code is throttled to the login rate. Failed logins
  and password changes are audited (`user.login_failed`, `user.password_change_failed`,
  `user.login_totp_failed`). Set `MSP_TRUST_PROXY=1` only behind a proxy you control, or the
  limits key on a spoofable header.
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
alerts. Admins also manage orgs, users, and revoke or delete devices.

**Tenant isolation now has two layers (RS2).** The first is data ownership: every device,
command, alert, token and audit row belongs to exactly one org, and the server never joins
across the tenant tree. The second is **per-user visibility**: an operator or viewer can be
scoped to a chosen set of orgs (`all_orgs = 0` plus the `user_orgs` rows), and then sees and
acts on nothing outside that set. A user's visible org set is **all orgs** when the role is
admin or `all_orgs = 1`, otherwise exactly the `user_orgs` rows; admins are always all-orgs and
cannot be scoped. The set is resolved once per request from the session user and pushed into the
store queries (`org_id IN (…)`) so lists are filtered in SQL, not after the fact.

The visibility layer follows a strict **404-not-403 rule**: a scoped user who reaches a
resource outside their set gets the **same response an unrelated resource would give** —
`404 not_found`, never a distinguishable "exists but forbidden". Every org- and device-scoped
endpoint resolves the resource, then returns `404` **before** any side effect, role-check result
or `409`, so a scoped user cannot tell that a device, command, alert or org in another org even
exists. Role (`403`) and visibility (`404`) are independent checks; visibility is enforced first.
The agent API (`/api/agent/v1/*`) is device-token auth and is unchanged: scoping is a
staff-visibility feature only.

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
  revoked (404 otherwise: the same scope rule as lock). The same per-user visibility layer
  applies **(RS2, section 4)**: a scoped operator reaching a device outside their org set gets
  `404 not_found`, resolved before the request has any effect; an all-orgs operator or admin
  reaches every org's devices as before. The MSP server never joins the
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
opened, remote shell opened/connected/closed, alert acknowledgements, and notification
channel/rule/window create/update/delete and channel test **(NS1)**.
Export it with your backups; SIEM forwarding is phase 2.

## 11. Preconditions before scaling to many clients (phase 2)

The phase-1 controls are adequate for a pilot with a handful of trusted technicians. Before
the Nth client, in this order:

1. **MFA (TOTP)** on every staff account; operator rights without MFA are a fleet-wide risk.
   The mechanism is **done (TF1, section 15)** — each staff user can enrol a TOTP second factor
   with recovery codes, and an admin can reset a lockout. Making it *mandatory* (a policy that
   forces enrolment before a session is usable) is the remaining step.
2. **Command signing**: operator-held keys sign queued commands and agents verify them, so a
   server compromise alone cannot push commands.
3. **Device token rotation** with overlap, and Keystore wrapping on Android.
4. **Per-technician org scoping** — done **(RS2)**: an operator or viewer can be restricted to a
   chosen set of client orgs (section 4), enforced in SQL with the 404-not-403 rule at every
   endpoint. Still outstanding here: client-facing self-service logins and dashboard IP
   allowlisting at the proxy.
5. Windows job-object process-tree kill and a reviewed script library.

## 12. Notification secrets and suppression (NS1 + NS2)

Alert notifications add outbound SMTP and HTTP from the server and one new kind of stored
credential. NS2 adds an **SMS** channel (its provider auth token is a fourth such credential) and
**multi-step escalation policies** (timed re-notification while an alert stays open). What that
changes:

- **Secrets are credentials in use, not hashes.** A channel's `secret` (the SMTP password, the
  webhook signing secret, or the **SMS provider auth token** — Twilio token or the generic
  gateway's bearer token) must be presented to the provider, so it is stored
  in the clear in `notify_channels.secret` (section 2). Protect it exactly like the device tokens:
  whoever holds the database holds these secrets. It is **write-only over the API** — the channel
  JSON carries only `"secret_set":true|false`; `PUT` with the field omitted keeps the stored value,
  `""` is rejected, a value replaces it. It is never written to a log line and never placed in a
  delivery's `detail_json`.
- **Redaction.** The server never logs a channel secret, and it reduces any `http(s)` URL that
  appears in a log line, a `last_error`, or a delivery `detail_json` to `scheme://host` — dropping
  userinfo, path and query — so a Slack incoming-webhook path or a `?token=` is not leaked through
  the delivery log or the server log. Webhook signing uses HMAC-SHA256 over the exact request body,
  sent as `X-MSP-Signature: sha256=<hex>`; the receiver verifies it with the shared secret.
- **Maintenance-window suppression is not a security control.** A window mutes deliveries for an
  org (or all orgs) between two absolute times; it is an operational convenience so a planned
  outage does not page anyone. It does not stop alerts being opened, gate any access, or hide data —
  anyone who can see the org still sees the alert and the suppressed delivery row. Do not rely on it
  to withhold information.
- **Outbound reachability.** The dispatcher connects out to the configured SMTP host/port and
  webhook URLs; those egress paths must be allowed from the server (docs/OPS.md). Like every other
  server-initiated connection it is subject to the server's own network position; a server
  compromise can both read these secrets and send as these channels, which is the same blast radius
  as the rest of the server (section 1).
- **Webhook targets are validated against SSRF.** A webhook `config.url` is rejected — at create,
  at update, and again before every send — when its host is a loopback, private (RFC1918), ULA,
  link-local (169.254.0.0/16, fe80::/10), unspecified or multicast IP literal, or a name that
  points at the loopback or the cloud metadata service by convention (`localhost`, `*.localhost`,
  `metadata`, `metadata.google.internal`). This stops an org-scoped operator from pointing a
  channel at the server's own network (e.g. `http://169.254.169.254/` or `http://127.0.0.1:<port>/`)
  and probing it with a Send-test. The check is deterministic (no DNS), so a hostname that passes
  validation but only *later* resolves to an internal address (DNS rebinding) is a known residual;
  a non-public webhook endpoint should still be fronted by an allowlisting egress proxy.
- **Notification mail cannot be used for header injection.** The alert hostname flows into the email
  Subject. A hostname is rejected at enrollment if it contains any control character, and every
  value interpolated into an email header (Subject, From, To) has its CR/LF collapsed to spaces
  before the message is written, so an agent operator cannot inject an extra header (a Bcc) or an
  arbitrary body into the notification mail. The `config.from`/`config.to` addresses are likewise
  rejected if they contain line breaks.
- **Redaction strips the secret and tokened URLs.** Before a `last_error` is stored or logged, the
  channel secret (when known) is removed and any `http(s)` URL is reduced to `scheme://host`
  (dropping userinfo, path and query). The error strings the dispatcher builds never embed the
  secret; the explicit strip keeps the guarantee true even if an underlying SMTP/HTTP library error
  ever echoed the credential.
- **SMS token write-only and redacted (NS2).** The SMS provider token is handled exactly like the
  other channel secrets: write-only over the API (`"secret_set"` only), never logged, never placed
  in a delivery's `detail_json` or `last_error`. It rides only in the provider request's
  `Authorization` header (Twilio HTTP basic auth, or a `Bearer` header for the generic gateway),
  never in a URL. The SMS body is a short one-line summary built only from alert fields with any
  URL reduced to `scheme://host`, so no tokened link can ride out in a text message. An SMS
  `config.url` (generic gateway, or a Twilio override) is SSRF-validated exactly like a webhook URL
  — at create, at update, and again before every send (so a channel stored out of band still cannot
  reach an internal address) — with the same deterministic-check / DNS-rebinding residual noted
  above; a generic SMS gateway should still be a trusted, operator-controlled endpoint (docs/OPS.md).
- **Escalation respects maintenance windows and acknowledgements (NS2).** An escalation step is
  enqueued only while the alert is **open and unacknowledged**; acknowledging or resolving the
  alert stops every further step at once — both by not enqueuing new steps and by suppressing an
  already-queued step at send time (so a step left `pending` by a transient provider failure is not
  re-claimed and paged after the operator has taken the alert). A due step whose org is inside an
  active maintenance window is enqueued `suppressed` and sent nothing, exactly like an NS1
  open/resolve delivery — the same operational mute, with the same caveat that it is not a security
  control. The escalation tick is idempotent (one delivery per `(alert, rule, channel, step)`), so a
  repeated tick or a restart never re-pages.
- **Escalation and routing references are tenant-consistent (NS2/RS2).** A rule or escalation
  policy may reference only a channel (and a rule only a policy) that is **global or in its own
  client** — enforced against the rule/policy's own `org_id`, not merely the caller's visibility, so
  not even an all-orgs admin can wire an org-A rule/policy to an org-B channel and leak one client's
  alert content to another tenant. A cross-org reference is a `400` that does not reveal whether the
  resource exists.
- **Org scoping (RS2).** Channels, rules, **escalation policies**, windows and the delivery log
  follow the same two-layer tenant isolation and 404-not-403 rule as every other staff resource
  (section 4): a scoped operator manages only their orgs' rows, a global `org_id null` row is
  visible only to all-orgs staff, and a cross-org resource is indistinguishable from one that does
  not exist.

## 13. Patch install (PM1)

- **Role-gated and tenant-scoped.** `POST /api/v1/devices/{id}/patch` requires the operator or
  admin role (never viewer) and the CSRF header, and a technician can patch only a device in an org
  they can see: a device outside the visible org set is `404 not_found`, resolved before any side
  effect, exactly like running a command (section 4). A revoked device is `unsupported_on_device`;
  android (no desktop package management) is `unsupported_on_platform`.
- **No arbitrary input to the OS.** The command carries only a bounded, validated shape
  (`action` scan|install, `mode` all|security|ids, `reboot` never|if_required, an `ids` list of
  package names). The agent runs only fixed package-manager invocations behind an injectable command
  executor — `apt-get -s dist-upgrade` to scan (a simulation that changes nothing) and `apt-get -y`
  to install, or the Windows Update COM API — never a shell or an argv from the request.
- **Named installs are constrained to what is pending.** `mode: ids` may name only packages already
  in the device's pending list: the server rejects unknown ids (`400 invalid_request` naming them)
  when it has a stored report, and the agent re-checks against a fresh scan and fails the command
  otherwise, so a technician cannot use the install path to pull in an arbitrary package.
- **Audited.** Every scan or install writes an audit row (`patch.scan_requested` /
  `patch.install_requested`) with the actor, device, mode and named packages; the install log is
  stored on the command result (capped 64 KiB) like exec/script. The full package list is never
  written to the agent or server logs at info level.

## 14. Software deployment (SD1)

- **Hash verification before use.** A file deploy carries the library file's SHA-256, filled by
  the server from the stored row (the caller never supplies it). The agent downloads the file over
  its authenticated channel, verifies the response `Content-Length` and recomputes the SHA-256
  **before** it writes or runs anything; a mismatch fails the command without writing the
  destination. The download is also bounded to the declared length, so a server that streamed more
  than it promised cannot fill the disk.
- **Destination path validation.** `dest` must be absolute, cleaned and free of any `..` component
  (on either separator); the server rejects a bad path with `400 invalid_request` and the agent
  re-validates. The agent additionally resolves the parent directory and refuses to write through a
  symlinked parent, then writes to a temp file in that directory and renames it into place (0755 if
  it will run, else 0644), so a deploy cannot be redirected outside its intended directory.
- **Org-scoped file access by agents.** The agent-download endpoint
  (`GET /api/agent/v1/files/{id}`) serves a file only when its `org_id` is the device's own org or
  NULL (a global artifact); any other org is `404 not_found`, indistinguishable from a missing file,
  so a device token cannot enumerate or pull another client's library (section 4). Staff file access
  is org-scoped exactly like the rest of RS2.
- **Size caps.** Uploads are capped at `MSP_MAX_FILE_MB` (default 200), enforced while streaming
  (the upload is exempt from the generic 512 KiB dashboard body limit but applies its own limit),
  over it `413 file_too_large`.
- **Package names are argv, never a shell.** A package deploy's name is a single token of
  `[A-Za-z0-9._+-]` (≤ 256 bytes), validated on the server and again on the agent, and passed as one
  argv element to `apt-get -y install` behind the same injectable executor PM1 uses — never through a
  shell, so it cannot split, inject or add flags. Run arguments for a file deploy are likewise argv
  (each ≤ 4096 bytes), only accepted with `run`, executed with no shell.
- **Role-gated, tenant-scoped and audited.** Upload, delete and deploy require the operator or admin
  role (never viewer) and the CSRF header; deploy to a device outside the visible org set, or for a
  file not visible for that device, is `404 not_found` resolved before any side effect. Every upload,
  delete and deploy writes an audit row (`file.uploaded`, `file.deleted`, `deploy.requested`); the
  download/install/run log is stored on the command result (capped 64 KiB) like exec/script.

## 15. Two-factor authentication (TF1)

- **TOTP, native and standard.** The second factor is RFC 6238 TOTP (HMAC-SHA1, 6 digits, 30 s
  step), implemented with the Go standard library only — no new dependency and no external service.
  Verification accepts the current 30 s step and ±1 (clock skew) and compares in **constant time**;
  a stale code (two or more steps away) is rejected. The primitive is tested against the RFC 6238
  Appendix B vectors.
- **An accepted login code is one-time (RFC 6238 §5.2).** Beyond the pending cookie being single-use,
  the login second factor records the highest TOTP time step a user has authenticated with
  (`users.totp_last_step`, migration 0010) and refuses any code whose step is not strictly greater.
  A still-valid code captured within its ±1 skew window therefore cannot be replayed to mint a second
  session even with a freshly minted pending token; the burn is one atomic guarded `UPDATE` (SQLite
  serialises writers), so concurrent logins cannot both spend the same step. The counter is reset when
  2FA is disabled or admin-reset, so a re-enrolled user starts clean. A legitimate later login at the
  next step is unaffected.
- **The secret is write-only.** It is 20 random bytes base32-encoded (`users.totp_secret`), handed
  back once by `setup` so the owner can enrol, and after that never returned by `me`, the users list
  or any log line (section 2). It is cleared when 2FA is disabled or an admin resets it.
- **Recovery codes are hashed and single-use.** Ten per user, format `xxxxx-xxxxx`, stored only as a
  sha256 hash (`recovery_codes.code_hash`) and shown once at enrol/regenerate. Consuming one marks
  its row used atomically, so a code logs in exactly once; `me` reports `recovery_codes_remaining`.
  Disabling 2FA deletes them all.
- **The login is two-step and the pending token is not a session.** With 2FA on, `POST
  /api/v1/login` verifies the password, then returns `{"totp_required":true}` with **only** a
  short-lived (5 min) pending cookie bound to the user and a nonce — never a full session.
  `POST /api/v1/login/totp` verifies a TOTP or recovery code and only then mints the session. A wrong
  password never reaches the second step, so the response never reveals whether 2FA is on. The
  pending token is kept server-side as a sha256 key in memory (single process, single SQLite DB), is
  consumed on success, and is not accepted anywhere a session is required.
- **Both steps are rate-limited and failures audited.** `/login/totp` and the confirm/disable/
  regenerate endpoints share the per-IP login budget (10/min; section 3), throttling code guessing.
  A bad second factor is audited `user.login_totp_failed`; a successful two-step login records
  `user.login` with `detail.second_factor` = `totp` or `recovery_code`.
- **Admin reset for lockouts.** `POST /api/v1/users/{id}/totp/reset` (admin only) disables a target
  user's 2FA so a user who lost their authenticator and recovery codes can re-enrol. It is audited
  `user.totp_reset` with the acting admin as actor and the target as target, and returns `404` for a
  user that does not exist (the check is kept uniform even though admins are all-orgs).
- **Residual.** Enrolment is per-user opt-in, not yet enforced as org policy (precondition 1). An
  accepted login code is one-time (see above), so a replay within its window is rejected; the ±1-step
  window plus the shared rate limit bound any remaining guessing exposure.

## 16. Agent self-update (AU1)

Self-update is the highest-blast-radius action in the platform: a bad push could brick every
managed desktop at once. The design is therefore built to be verified and reversible, not just to
swap a file.

- **Hash-verified before use.** The update command carries the release binary's SHA-256, filled by
  the server from `agents.json` (the operator never types it). The agent downloads the binary from
  the server's `/dl/` over TLS, bounds the body to the response `Content-Length`, and recomputes the
  SHA-256 **before touching anything**. A mismatch aborts with the running binary untouched and no
  backup left behind — a tampered or corrupted download can never be executed. Integrity rests on TLS
  plus this manifest-SHA-256 re-check, not on authenticating the download: `/dl/` serves the agent
  binaries publicly (the install one-liners fetch them without a token), so the device token the agent
  sends on the update fetch is not required or enforced there.
- **Health-checked before the swap.** After the hash passes, the agent runs the new binary's
  `version` subcommand with a short timeout and confirms it prints the expected version. A binary
  that cannot start, or reports the wrong version, aborts the swap with the old binary still in
  place and the backup removed. Only a verified, runnable binary of the expected version is swapped
  in.
- **Backed up and rolled back, two ways.** The current executable is copied to `<exe>.bak` before the
  atomic rename (the running image is never disturbed). The backup is cleared only after the next
  successful sync on the new version, and until then **two** checks can roll it back: (1) on the next
  start, if the installed binary cannot run its own `version` subcommand, a bootstrap check restores
  `<exe>.bak` and restarts; and (2) a binary that *does* answer `version` but cannot reach or sync with
  the server is put on probation — if it fails to complete one successful sync within
  ~10 minutes / 8 consecutive failures it restores `<exe>.bak` and restarts. This closes the brick a
  `version`-but-no-sync release (a broken request shape, a TLS/CA regression) would otherwise cause.
  Rolling back is the safe direction: the restored binary is the known-good previous release, and if
  the new one was in fact fine the server simply re-advertises it — so a release that is genuinely bad
  should also be un-published from `MSP_AGENT_DIR` to stop the re-advertise/rollback churn.
- **No secret leak on the download URL.** The download URL is resolved against the agent's own server
  base; an absolute URL on a different host, or one that downgrades the scheme (http on an https
  server), is refused, so the device token (sent in the `Authorization` header, never the URL, never
  logged) cannot be exfiltrated or put on the wire in cleartext by a hostile or mistaken manifest.
- **Desktop only, role-gated, tenant-scoped, audited.** `update` is rejected on Android
  (`unsupported_on_platform`) and on a revoked device. Forcing an update needs the operator or admin
  role and the CSRF header; a device outside the caller's visible org set returns `404 not_found`
  before any side effect (RS2), and every forced update writes `agent.update_requested` (from/to
  version). A repeated identical advertised version is ignored while an update for it is in flight or
  a restart is pending.
- **Opt-out.** Auto-update defaults on but can be disabled per device (`--no-auto-update`,
  persisted), so an operator can stage a release on a pilot group and hold the fleet. A forced
  dashboard command still applies to an opted-out device.
- **Residual.** The Windows service-swap path (rename-aside) is written and cross-built but NOT
  VERIFIED on a real Windows host; the manifest and its binaries must be published over TLS and the
  `MSP_AGENT_DIR` kept writable only by the MSP, since whoever can write `agents.json` chooses what
  every desktop runs.

## 17. Script library, bulk jobs and automations (AUT1)

- **Same execution model.** A script runs as an ordinary queued command, so every command guarantee
  holds unchanged: argv is executed directly, never through a shell (section 6); a script body runs
  under an explicit interpreter; paths are traversal-checked; the agent wire COMMAND is unchanged (a
  job only tags its commands with a `job_id` in the dashboard view).
- **Org-scoped targets (RS2).** Scripts, jobs and automations are org-scoped exactly like RS2: a
  scoped operator/viewer sees only rows whose `org_id` is in their visible set, a global
  (`org_id` NULL) script/automation is visible only to all-orgs users, and a cross-org single row is
  `404 not_found`. A bulk or automation target can **never** reach a device outside the caller's (for
  a bulk run) or the automation's visible org set: a target `org_id` or `device_id` outside scope is
  rejected `404`/`400` **before any command is queued**, and the fan-out resolver drops any
  out-of-scope device. A global script/automation requires all-orgs to create.
- **Roles.** Viewer is read-only over all of it; create/update/delete/run-on-many require operator or
  admin, with the CSRF header on every write.
- **Cooldown against flapping.** A triggered automation re-runs on the same device only after a
  per-automation-per-device cooldown (10 min), so an alert that flaps open/closed cannot hammer a
  device with repeated commands.
- **Attribution.** Every bulk run (`job.created`) and automation run (`automation.ran`,
  `automation.triggered`, the latter two as actor `system`) is audited, and each resulting command is
  audited as usual. A scheduled/triggered command is attributed to the automation's owner
  (`automations.created_by`), so a run always traces back to an accountable user.
- **Android.** exec/script are desktop kinds, so a fan-out skips android with a per-device note
  rather than queueing a command the device cannot run.

## 18. Reports and exports (RP1)

- **Read-only.** Every report is a `GET` over data the platform already stores (the `heartbeats`
  table and the denormalised device patch columns). There is no new table, no new command kind and
  no agent change; a report can only read, never act, so a viewer may read them.
- **Org scoping (RS2).** Each report filters to the caller's visible org set **in SQL** (the shared
  visible-org helpers), and an explicit `org_id` (fleet reports) or `device_id` (the disk series)
  outside that set is `404 not_found` — never a distinguishable 403, and resolved **before any work**,
  so a scoped operator cannot even confirm that a client or device in another tenant exists. A scoped
  operator's uptime/disk-top/patch reports contain only their own orgs' rows.
- **CSV / formula injection.** The CSV exports neutralise spreadsheet formula injection: any text cell
  (hostname, org name, platform) whose first character is `=`, `+`, `-`, `@`, TAB or CR is prefixed
  with a single quote, so a crafted hostname like `=cmd|'/c calc'!A1` is rendered as literal text and
  never evaluated when the file is opened in Excel/Sheets. Comma/quote/newline quoting is handled by
  the standard CSV encoder; numeric and boolean cells are emitted verbatim. The same guard is mirrored
  in the client-side CSV builder used for the dashboard preview. The export carries
  `Content-Disposition: attachment` so a browser downloads rather than renders it.
- **Retention bound.** The period is clamped to 1..`MSP_RETENTION_DAYS`; a longer `days` is rejected
  `400`, so a report cannot be used to probe for data beyond what retention keeps.

## 19. Reporting a vulnerability

Report privately to the maintainers of this repository (open a security advisory on the
repository, or e-mail the address listed in its README once published). Please include the
version (`msp-server version`), reproduction steps and impact; do not open a public issue
first. Fixes ship as a new version with a note in the release description.
