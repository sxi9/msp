# RS1 — Remote shell (phase 2, decision D3)

The normative contract for the interactive browser terminal. Every work package codes against
this exact wording: field names, routes, status strings, close reasons and limits are frozen here.
Research-verified library and API facts are injected by the build workflow as `facts.json` next to
this file; read it for the exact module versions and platform API names.

## A. Command kind `shell`

Desktop only. `android` → `400 {"error":"unsupported_on_platform"}`; revoked device →
`400 {"error":"unsupported_on_device"}`.

Agent-facing command JSON:

```
{"id":N,"kind":"shell","shell":"bash"|"sh"|"powershell"|"cmd","cols":120,"rows":32,"timeout_s":14400}
```

- `shell` optional. Default: `bash` if on PATH else `sh` (Linux); `powershell` (Windows).
- `cols`/`rows` optional, default 120×32, clamped 20..500 / 5..200.
- `timeout_s` = max session duration, default 14400, max 28800.

Dashboard open:

```
POST /api/v1/devices/{id}/shell  {"shell":"bash","cols":120,"rows":32}
```

- Auth: admin|operator, CSRF header, device in the caller's org set, platform desktop, not revoked.
- `409 {"error":"device_offline"}` if the device is not online.
- `409 {"error":"shell_busy"}` if another shell command for the device is in status `queued`|`sent`.
- `429 {"error":"too_many_shells"}` if the server already relays 20 concurrent shells.
- Success `201 {"command":COMMAND,"ticket":"<43-char base64url>","ws_path":"/api/v1/shell/{id}/ws"}`.
- Audit `shell.opened` (user, device, ip, shell). Then `Notify(device)` as for other commands.

`COMMAND` JSON for kind shell gains:

```
"shell_session": {"bytes_in":N,"bytes_out":N,
  "close_reason":"technician_closed|agent_closed|idle_timeout|max_duration|server_shutdown|agent_never_connected|",
  "transcript_truncated":bool,"connected_at":ts|null}
```

`null` until the session row exists.

Transcript:

```
GET /api/v1/shell/{id}/transcript   (admin|operator, same org rule)
```

→ `200 text/plain; charset=utf-8`, the recorded agent→browser bytes (cap 1 MiB; then
`transcript_truncated=true` and recording stops while relaying continues). `404` if not a shell command.

## B. WebSocket relay

Library from `facts.json` (prefer `github.com/coder/websocket`). Framing is identical on both legs:

- **binary** frames: raw terminal bytes. browser→agent = keystrokes to PTY stdin; agent→browser = PTY output.
- **text** frames: JSON control.
  - browser→agent: `{"type":"resize","cols":N,"rows":N}`.
  - agent→browser: `{"type":"exit","code":N}` just before it closes.
  - server→browser only: `{"type":"status","state":"waiting_for_agent"|"connected"|"closed","reason":"<close_reason>"}`.

**Agent leg**: `GET /api/agent/v1/shell/{command_id}` with `Authorization: Bearer dt_...` → 101.
Server checks: valid device token; command belongs to that device; kind shell; status `sent`; no agent
socket already attached (else 409). On attach: command stays `sent`; `shell_session.connected_at` set;
audit `shell.connected` (actor device); browser gets `status connected`.

**Browser leg**: `GET /api/v1/shell/{command_id}/ws?ticket=...` with the session cookie → 101.
Server checks: session auth (same role rule as POST); `Origin` header host must equal the request
`Host` (case-insensitive; missing Origin → 403); ticket matches the one issued for this command, is
unused and younger than 60 s (single use, consumed on handshake). The browser may connect before the
agent; it then receives `status waiting_for_agent`, and browser→agent bytes are dropped until the agent
attaches. If no agent attaches within 60 s of the POST: close the browser leg, complete the command
`failed` error "agent never connected", close_reason `agent_never_connected`.

**Relay**: copy frames verbatim between the legs; count `bytes_in` (browser→agent) and `bytes_out`
(agent→browser); append agent→browser bytes to the transcript buffer (cap 1 MiB); update last-activity
on browser input. Idle 30 min without browser input → close both, `idle_timeout`. Max duration
`timeout_s` → `max_duration`. Server shutdown → `server_shutdown`. Either leg closing closes the other:
`technician_closed` / `agent_closed`.

On close the server completes the command: status `done` when an exit code 0 arrived or the technician
closed a live session; `failed` otherwise; `exit_code` from the exit message or -1; stdout empty;
`error` = close reason when not done. It writes the `shell_sessions` row (bytes, transcript, truncated,
close_reason, connected_at, closed_at) and audit `shell.closed` (actor user for `technician_closed`,
else system; detail duration_s, bytes_in, bytes_out, reason). One relay goroutine set per session;
sessions in memory keyed by command id under a mutex; at most 20 concurrent shells per server.

CSP: the dashboard Content-Security-Policy must allow same-origin WebSocket (`connect-src 'self'`; add
explicit `ws:`/`wss:` only if needed). Keep all other directives unchanged.

## C. Agent

On a kind shell command: refuse (result exit -1, error "shell busy") if a shell is already running.
Otherwise dial the WebSocket at the agent's configured server base URL (http→ws, https→wss), with the
same TLS config / CA file and the `Authorization: Bearer dt_` header, path `/api/agent/v1/shell/{id}`.

- **Linux** (`internal/agent/pty_linux.go`, `x/sys/unix` only, names from facts.json): open `/dev/ptmx`,
  unlock (`TIOCSPTLCK`), get the slave number (`TIOCGPTN`), open the slave, start the shell with
  `SysProcAttr{Setsid:true, Setctty:true, Ctty:<slave fd index>}`, set the window size
  (`unix.IoctlSetWinsize`, `TIOCSWINSZ`), copy ptmx↔WebSocket, apply resize control frames, send the
  exit frame with the process exit code, close.
- **Windows** (`internal/agent/pty_windows.go`, `x/sys/windows`, names from facts.json): ConPTY via
  `CreatePseudoConsole` over a pipe pair, `ResizePseudoConsole` on resize, launch the shell with the
  pseudoconsole attribute, copy pipes↔WebSocket, exit frame, `ClosePseudoConsole`.
- **Other OS** (`pty_other.go`, build tag): shell unsupported → exit -1 error "shell not supported on this platform".

The PTY/ConPTY code goes behind a small interface so the sync loop is platform-neutral and the
interface can be unit-tested with a fake. The agent enforces its own idle and never writes shell output
to its logs.

## D. Dashboard

Device page "Remote shell" button (admin|operator; hidden for viewer and for offline/android devices).
Clicking it POSTs to open, then connects `@xterm/xterm` (version from facts.json) to
`ws_path?ticket=...` on the same origin. Keystrokes → binary frames; `onResize`/fit addon → resize
control frames; incoming binary → `term.write`. Show a status line (waiting for agent / connected /
closed with reason). A Close button and page navigation close the socket (`technician_closed`). The
terminal lives in a panel that does not break the 400 px layout (it may scroll inside its own box).
A "Transcript" link on finished shell commands in the history opens `GET …/transcript`. Keep `?mock=1`
working (a stub shell that echoes typed bytes is fine). Put pure helpers (framing encode/decode,
resize math) in `web/src/lib/shell.ts` with a `node --test` unit test. The xterm CSS is bundled into
`web/dist/app.css` (see facts.json for the build.mjs change).

## E. Store and migrations

New table `shell_sessions(command_id INTEGER PRIMARY KEY REFERENCES commands(id) ON DELETE CASCADE,
connected_at TEXT NULL, closed_at TEXT NULL, bytes_in INTEGER NOT NULL DEFAULT 0, bytes_out INTEGER NOT
NULL DEFAULT 0, transcript BLOB NOT NULL DEFAULT '', transcript_truncated INTEGER NOT NULL DEFAULT 0,
close_reason TEXT NOT NULL DEFAULT '')` via a new migration file (do not edit `0001_init.sql`). Store
methods: `CreateShellSession`, `SetShellConnected`, `FinishShellSession`, `GetShellSession`,
`AppendTranscript` (or an equivalent the server drives in memory and flushes once on close — pick one and
document it). `commands.kind` CHECK must allow `shell`: since SQLite CHECK can't be altered, add the new
migration that recreates the constraint, or (simpler, documented) store validates kind in Go and the
CHECK already lists shell — confirm what 0001 has and choose the minimal correct migration.

## F. Docs and status

Update `docs/PROTOCOL.md` (kind shell, both WS endpoints, control frames, transcript endpoint),
`docs/ARCHITECTURE.md` (the relay), `docs/SECURITY.md` (ticket + Origin + single relay + transcript
retention + that shell output is never logged), `docs/GUIDE.md` (a "Remote shell" section after Remote
desktop: how to open one, that it is audited and transcribed, limits), `docs/OPS.md` (proxy must allow
WebSocket upgrades and long-lived connections; the 20-shell cap), `docs/FEATURES.md`, `docs/STATUS.md`
(row WORKING once Linux e2e passes, Windows ConPTY NOT VERIFIED), `docs/ROADMAP.md`, `docs/CHECKLIST.md`.

## Proof (what the build must show here)

`CGO_ENABLED=0 go build ./...`, `go vet ./...`, `CGO_ENABLED=0 go test ./... -count=1`,
`GOOS=windows GOARCH=amd64 go build ./...`, `make web`, `scripts/e2e.sh`, plus a NEW in-process Go test
(`internal/e2e`) that starts the server, runs the real Linux agent, opens a shell over the real
WebSocket relay from a Go WebSocket client acting as the browser, runs `echo hello` (bash) or `printf`,
asserts the bytes come back, sends a resize, closes, and asserts the command is `done`, the audit has
`shell.opened`/`shell.connected`/`shell.closed`, and the transcript contains the output.
