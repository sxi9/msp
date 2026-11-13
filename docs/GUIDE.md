# MSP platform guide

Everything in one place: what the platform does, how to install the server and the agents on
Windows, Linux and Android, how to enroll a client's devices, and how to use remote commands,
lock, alerts and the audit log day to day. Reference docs: `docs/PROTOCOL.md` (wire contract),
`docs/OPS.md` (operations detail), `docs/SECURITY.md` (threat model), `docs/STATUS.md` (what is
verified), `docs/screenshots/` (what each page looks like).

## 1. What you get

One server binary with SQLite inside and the web dashboard embedded. One desktop agent binary
for Linux and Windows. One Kotlin app for Android. Agents only ever connect **outbound** over
HTTPS to the server, so client sites need no firewall changes; commands ride back on the
agent's own sync request.

Phase 1 features (built and verified on Linux; Windows and Android runtime need a real device):

| Area | Features |
|---|---|
| Clients | One organisation per customer; devices, tokens, alerts and audit are scoped to it |
| Users | Roles `viewer` (see), `operator` (act), `admin` (manage users, clients, delete devices) |
| Enrollment | Per-client tokens with label, max uses, expiry and revoke; install one-liners per OS |
| Fleet | Every device with online/offline/pending, OS, agent version, last seen, CPU, memory, disk, battery, open alerts; filter by client, status, hostname |
| Device page | Status, metrics, facts (CPU model, memory, kernel, model, serial), disks, command console, command history, Lock, Ping, Message, Revoke, Delete |
| Remote commands | `exec` (argv, no shell), `script` (sh/bash on Linux, PowerShell/cmd on Windows), presets, timeout, output capped at 64 KiB |
| Remote lock | Built-in command; only for devices enrolled under your server; operator or admin; audited twice |
| Alerts | `offline`, `disk_full` (≥ 90 %), `memory_high` (≥ 95 %), `battery_low` (≤ 15 %, not charging); acknowledge; auto-resolve |
| Audit | Every login, token, enrollment, command, lock, alert ack and user change, with actor and IP; never pruned |
| Ops | Docker compose, Caddy TLS example, systemd unit, Windows service, Android foreground service, health endpoint |

Later phases (planned, not built): interactive remote shell in the browser, remote desktop
through a self-hosted branded RustDesk relay, patching, software deployment, notifications
by email/SMS/Slack, reports, Android device-owner mode (wipe, kiosk, app push), macOS/iOS
agents, ticketing. See `docs/FEATURES.md` and `docs/STATUS.md`.

## 2. How it fits together

```
client site (outbound HTTPS only)             MSP server (one binary)              technician
Windows agent  ─┐                             /api/agent/v1/*  agent API           browser
Linux agent    ─┼─ POST /api/agent/v1/sync ─▶ queue, alerts, SQLite, audit  ◀───  /api/v1/*
Android agent  ─┘   every 30 s (Android 60 s)   dashboard embedded at /            login + role
```

- Desktop agents sync every 30 s and the server holds the request up to 25 s, so a queued
  command reaches the device in under a second. Android syncs every 60 s without waiting.
- A device is `online` while its last sync is newer than 3 × interval + 15 s (105 s desktop,
  195 s Android), `offline` after that, `pending` if it has never synced.
- Diagrams with every arrow labelled are in `docs/diagrams/`.

## 3. Install the server

### 3a. Docker (recommended)

```sh
git clone https://github.com/sxi9/msp && cd msp
mkdir -p deploy/data && sudo chown 10001:10001 deploy/data
docker compose -f deploy/docker-compose.yml build
docker compose -f deploy/docker-compose.yml run --rm server user add --email admin@example.com --role admin --password-stdin
docker compose -f deploy/docker-compose.yml run --rm server org add --name "Acme Dental"        # prints the org id
docker compose -f deploy/docker-compose.yml up -d
```

The image already contains the desktop agent binaries and installers at `/agents`, served
at `https://your-host/dl/`. Build the Android APK separately (section 5c) and drop it into
the same directory.

For HTTPS, uncomment the `caddy` service in `deploy/docker-compose.yml`, copy
`deploy/Caddyfile.example` to `deploy/Caddyfile` with your hostname, point DNS at the host,
open ports 80 and 443, and set in the server's environment:

```
MSP_BASE_URL=https://rmm.example.com
MSP_SECURE_COOKIES=1
MSP_TRUST_PROXY=1
```

Then remove the server's own `ports:` mapping so only Caddy is reachable. Certificates renew
automatically.

### 3b. Bare binary

```sh
make build                                   # bin/msp-server, bin/msp-agent
export MSP_DB=/var/lib/msp/msp.db MSP_LISTEN=:8080 MSP_BASE_URL=https://rmm.example.com MSP_AGENT_DIR=/var/lib/msp/agents
echo 'change-me-now' | bin/msp-server user add --email admin@example.com --role admin --password-stdin
bin/msp-server org add --name "Acme Dental"
bin/msp-server serve
make agents                                  # fills MSP_AGENT_DIR with binaries, SHA256SUMS, installers
```

Direct TLS without a proxy: set `MSP_TLS_CERT` and `MSP_TLS_KEY` to a PEM pair and
`MSP_LISTEN=:443`.

| variable | default | meaning |
|---|---|---|
| `MSP_DB` | `./msp.db` | SQLite file |
| `MSP_LISTEN` | `:8080` | listen address |
| `MSP_BASE_URL` | empty | public URL printed into install one-liners |
| `MSP_AGENT_DIR` | empty | directory served at `/dl/` |
| `MSP_RETENTION_DAYS` | `7` | heartbeat samples older than this are pruned |
| `MSP_SECURE_COOKIES` | `0` | `1` behind TLS |
| `MSP_TRUST_PROXY` | `0` | `1` only when a proxy you control is the only way in |

Health check: `GET /healthz` returns `{"ok":true}`.

## 4. Add a client and get an enrollment token

1. Log in at your server URL with the admin account.
2. **Orgs** → **New org** → name the client (for example "Acme Dental").
3. Open the org → **New token** → label it for the rollout (for example "front desk, Nov"),
   set max uses to the number of machines plus a few, and a short expiry (a day for a site
   visit, a week at most).
4. The token is shown **once**, together with the install one-liners for Linux, Windows and
   the APK link. Copy what you need now.
5. When the batch is enrolled, **Revoke** the token.

You can also do this from the server CLI:

```sh
msp-server token new --org 1 --label "front desk" --max-uses 10 --ttl 168h    # prints et_...
```

## 5. Install the agents

### 5a. Ubuntu / Linux (systemd)

Run as root on the client machine. The installer verifies `SHA256SUMS` before installing.

```sh
curl -fsSLo /tmp/msp-install.sh https://rmm.example.com/dl/install.sh \
  && sudo bash /tmp/msp-install.sh --server https://rmm.example.com --token et_...
```

Manual equivalent (private CA or custom interval):

```sh
sudo install -m 0755 msp-agent-linux-amd64 /usr/local/bin/msp-agent
sudo msp-agent install --server https://rmm.example.com --token et_... [--ca-file /etc/msp-agent/ca.pem] [--interval 30]
msp-agent status
```

This enrolls, writes `/etc/msp-agent/agent.json`, installs and starts the `msp-agent`
systemd service (runs as root so remote scripts can touch `/etc` and `/usr`). Logs:
`journalctl -u msp-agent -f`. Remove: `sudo msp-agent uninstall`.

### 5b. Windows 10 / 11

From an **administrator** PowerShell:

```powershell
irm https://rmm.example.com/dl/install.ps1 -OutFile $env:TEMP\msp-install.ps1; & $env:TEMP\msp-install.ps1 -Server https://rmm.example.com -Token et_...
```

The script downloads `msp-agent-windows-amd64.exe` to `%ProgramData%\msp-agent\`, checks its
hash, enrolls and registers the **MSP Agent** service (automatic start, restart on failure).
Commands run as LocalSystem; lock and message reach the logged-in user's session. Logs: Event
Viewer → Windows Logs → Application, source `msp-agent`. Remove: `msp-agent.exe uninstall`.

### 5c. Android

Build the APK once (`make android`, needs the Android SDK) and publish it at `/dl/msp-agent.apk`.
On the phone:

1. Open `https://rmm.example.com/dl/msp-agent.apk` in the browser and allow installing from
   that browser, or `adb install msp-agent.apk`.
2. Open **MSP Agent**, enter the **Server URL** and the **Enrollment token**, tap **Enroll**.
3. Tap **Allow background** and accept the battery-optimisation exemption.
4. Tap **Grant device admin** and accept. This is what makes remote lock work.

The app runs as a foreground service with a permanent "Managed by your IT provider"
notification, syncs every 60 s, and shows offline after 195 s without a sync. Phone battery
managers can delay syncs; a few false offline alerts on phones are expected in phase 1.
The phase 1 APK is a debug build that also accepts plain `http://` for lab servers.

### 5d. Check it worked

The device appears on **Fleet** under its client within one sync, with a green dot. On the
device page the **Facts** panel fills on the first sync and metrics update every sync.

## 6. Remote commands

Open the device page. Desktop devices show the **Run** console; Android devices show only the
Actions row (lock, ping, message), because Android cannot run shell commands.

**Command mode** (`exec`): one argument per line. The agent runs exactly that argv with no
shell, so `;`, `|`, `$VAR` and `*` are literal characters, never interpreted. Example:

```
uname
-a
```

**Script mode** (`script`): a whole script body run by `sh` or `bash` on Linux, `powershell`
or `cmd` on Windows. The agent writes it to a private temp file and runs that file. Use this
when you need pipes, variables or several steps.

**Presets** fill the console with common tasks: Uptime, Disk usage, Top processes, Reboot on
Linux; System info, Drives, Top processes, Reboot on Windows.

**Timeout**: 1 to 3600 s (default 60). On timeout the agent kills the whole process group and
reports `timeout` with exit code -1.

What happens after **Run on <host>**:

| Status | Meaning |
|---|---|
| `queued` | Saved and audited; the server wakes the device's parked sync immediately |
| `sent` | Delivered to the agent; it is running |
| `done` | Exit code 0; stdout and stderr shown with a Copy button |
| `failed` | Non-zero exit, or could not start |
| `timeout` | Killed at the timeout |
| `lost` | Agent took the command but never reported back within timeout + 5 min |

Output is capped at 64 KiB per stream (`truncated` is shown when it hits the cap). The
history lists the last commands with kind, who ran it, status, exit code and duration; the
full command, script hash and output are kept in the database and never pruned.

Rules enforced by the server: only `operator` and `admin` can run anything; a command can
only target a device inside the technician's own client set; `exec` and `script` are refused
for Android devices.

## 7. Lock, ping, message

The **Actions** row on every device page:

- **Lock device**: asks for confirmation in the page, then queues the built-in `lock`
  command. Linux runs `loginctl lock-sessions` (falls back to `xdg-screensaver lock`);
  Windows locks the active console session; Android calls `DevicePolicyManager.lockNow()`
  and needs device admin granted in the app. The result appears in the history like any
  command. Lock cannot be sent to a device that is not enrolled under your server: the
  dashboard only lists enrolled devices, the API returns 404 for anything else, and the agent
  only accepts commands on its own device token. Each lock writes two audit rows,
  `command.queued` and `device.lock_requested`.
- **Ping**: round-trip check; the agent answers `pong` with exit 0.
- **Send message**: shows a text to the logged-in user (desktop notification on Linux via
  `notify-send`, `msg.exe` on Windows, a high-priority notification on Android).

## 8. Alerts

Alerts are evaluated by the server on every sync and by a sweeper every 15 s. No separate
monitoring process.

| Type | Severity | Opens when | Resolves when |
|---|---|---|---|
| `offline` | high | no sync for 3 × interval + 15 s | the device syncs again |
| `disk_full` | medium | any disk ≥ 90 % used | back under |
| `memory_high` | low | memory ≥ 95 % used | back under |
| `battery_low` | low | battery ≤ 15 % and not charging | charging or above |

There is one open alert per device and type. **Alerts** page: Open or All, filter by client,
**Ack** to acknowledge (recorded with your user and time). Thresholds are server config
(`DiskPct`, `MemPct`, `BatteryPct` in `internal/server`). Email, SMS and Slack notifications
are phase 2.

## 9. Users, roles, audit

- **Users** (admin only): add technicians with email, password (10+ characters) and role;
  disable them when they leave. Sessions are cookies; the dashboard sends a CSRF header on
  every write. Login is rate limited to 10 attempts per minute per IP.
- **Audit** page: every action with time, actor (user email, `device:<id>` or `system`),
  action name, client and device, and the client IP. Append-only, never pruned.

## 10. Housekeeping

- **Token hygiene**: one token per rollout, short expiry, revoke after use. The server keeps
  only a hash and the first 10 characters.
- **Revoke a device** (admin): its token stops working; the agent logs "token rejected" and
  waits. **Re-enroll** by running the installer again with a new token; the same machine
  reuses its device row. **Delete** removes the device and all its history.
- **Backups**: one SQLite file. `sqlite3 msp.db ".backup backup.db"` while running, or stop
  the server and copy `msp.db` with its `-wal` and `-shm` files.
- **Upgrades**: replace the binary or pull the image and restart; migrations run at start.
  Agents are not self-updating in phase 1; re-run the installer to update them.
- **Firewall**: agents need outbound HTTPS to the server only. The server needs 443 inbound
  (80 + 443 with Caddy). Restrict the dashboard to your own addresses at the proxy.

## 11. Troubleshooting

| Symptom | Check |
|---|---|
| Device stays `pending` | Agent never synced: `msp-agent status`, `journalctl -u msp-agent`; is the server URL reachable from the site; is TLS trusted (private CA needs `--ca-file`) |
| Device `offline` but machine is up | Service stopped, or proxy idle timeout under 40 s cutting the long-poll; Android battery manager (re-check **Allow background**) |
| Agent logs "token rejected" | Device was revoked, or the server database was replaced; re-run the installer with a fresh token |
| Command stuck in `sent` then `lost` | Agent died mid-command or lost network; it reports on reconnect if it can, otherwise re-run |
| Lock reports `failed` on Linux | No desktop session to lock (headless machine or no logind). Expected on servers; verify on a desktop |
| Lock reports "device admin not granted" on Android | Open the app and tap **Grant device admin** |
| Installer refuses to install | `SHA256SUMS` in `/dl/` does not match the binaries; rebuild with `make agents` |
| Dashboard says "bundle not built" | Run `make web` (needs Node 22) and restart the server |

## 12. Where to look in the repository

| Path | What |
|---|---|
| `cmd/msp-server`, `internal/server`, `internal/store` | server, APIs, alerts, SQLite |
| `cmd/msp-agent`, `internal/agent` | desktop agent, command runner, service install |
| `android/` | Android agent app |
| `web/` | dashboard (Preact + TypeScript) |
| `deploy/` | Dockerfile, docker-compose, Caddyfile example, systemd unit, installers |
| `scripts/e2e.sh` | end-to-end proof with real binaries |
| `docs/screenshots/`, `docs/diagrams/` | labelled screenshots and diagrams |
| `docs/STATUS.md`, `docs/CHECKLIST.md` | what is verified and what is not |
