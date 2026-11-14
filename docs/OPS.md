# Operations

How to run the MSP server, publish and install agents, and keep the installation healthy.
The wire and CLI contract is in `docs/PROTOCOL.md`; the threat model is in `docs/SECURITY.md`.

## 1. Running the server

One static binary (`msp-server`), one SQLite file, no other services. Configuration is by
environment variables; a flag of the same name overrides the variable.

| variable             | default      | meaning                                                                 |
|----------------------|--------------|-------------------------------------------------------------------------|
| `MSP_DB`             | `./msp.db`   | SQLite file (WAL mode; `-wal` and `-shm` files live next to it)         |
| `MSP_LISTEN`         | `:8080`      | listen address                                                           |
| `MSP_BASE_URL`       | `""`         | public URL printed into install one-liners (`https://rmm.example.com`)   |
| `MSP_TLS_CERT` / `MSP_TLS_KEY` | unset | serve TLS directly when both are set                                 |
| `MSP_SECURE_COOKIES` | `0`          | `1` marks the session cookie `Secure` (automatic when serving TLS)       |
| `MSP_TRUST_PROXY`    | `0`          | `1` honours the first `X-Forwarded-For` hop (only behind your own proxy) |
| `MSP_AGENT_DIR`      | `""`         | directory served at `/dl/` (agent binaries, `SHA256SUMS`, installers)    |
| `MSP_RETENTION_DAYS` | `7`          | heartbeat samples older than this are pruned hourly                      |
| `MSP_PRESETS`        | `""`         | JSON command preset library for the device console; unset = the builtin set (see below) |
| `MSP_RUSTDESK_HOST`  | `""`         | self-hosted RustDesk ID/relay server as clients reach it, bare hostname or IP (section 10) |
| `MSP_RUSTDESK_KEY`   | `""`         | that server's public key (content of `id_ed25519.pub`); set with `MSP_RUSTDESK_HOST` or not at all: the server refuses to start with only one |

Bootstrap (first admin, first org, first enrollment token), then serve:

```sh
echo 'change-me-now' | msp-server user add --email admin@example.com --role admin --password-stdin
msp-server org add --name "Acme Dental"                      # prints the org id
msp-server token new --org 1 --label rollout --max-uses 10 --ttl 168h   # prints et_...
msp-server serve
```

Change the bootstrap password right away: sign in, open **Account** (click your email in the
top bar) and set a new one, or from the server `echo 'new-pass' | msp-server user set-password
--email admin@example.com --password-stdin` (the CLI path also ends every session of that user;
use it when a password is forgotten or compromised).

With Docker the same commands run as `docker compose -f deploy/docker-compose.yml run --rm server user add ...`
(the compose file's header lists them). The image runs as uid 10001 and stores the database
in the `/data` volume (`./deploy/data` on the host; `chown 10001:10001` it before first start).
The image already contains the desktop agent binaries at `/agents` and sets
`MSP_AGENT_DIR=/agents`, so `/dl/` works out of the box; the Android APK is not in the image
(see section 3).

## 2. TLS

Agents refuse `http://` except to loopback or with `--insecure-http`, Android release builds
refuse cleartext entirely, and the session cookie is only safe over TLS. Pick one:

- **Caddy in front (recommended).** Uncomment the `caddy` service in
  `deploy/docker-compose.yml`, copy `deploy/Caddyfile.example` to `deploy/Caddyfile` with your
  hostname, point public DNS at the host and open 80/443. Set
  `MSP_BASE_URL=https://rmm.example.com`, `MSP_SECURE_COOKIES=1`, `MSP_TRUST_PROXY=1` and remove
  the server's `ports:` mapping so only Caddy is reachable. Let's Encrypt renewal is automatic.
- **Direct TLS.** Set `MSP_TLS_CERT` and `MSP_TLS_KEY` to a PEM pair and `MSP_LISTEN=:443`;
  `Secure` cookies and HSTS switch on by themselves. You renew the certificate yourself.
- **Private CA.** Works for desktop agents (`--ca-file ca.pem` at enroll/install time; the
  installers do not pass it, so use the manual steps below). Android trusts only the system
  store, so a private CA must be installed as a user certificate on each phone; a public
  certificate is far less work.

Any proxy between agents and server must allow request bodies to sit idle for 25 s (the sync
long-poll) and should have an idle timeout above 40 s (the agent's HTTP timeout). Caddy's
defaults are fine. Set `MSP_TRUST_PROXY=1` only when the server port is not reachable except
through that proxy, otherwise clients can spoof their IP for rate limits and the audit log.

A proxy in front of the **dashboard** must additionally allow **WebSocket upgrades** and
**long-lived connections** for the remote shell (RS1): both relay legs are WebSockets under
`/api/v1/shell/{id}/ws` (browser) and `/api/agent/v1/shell/{command_id}` (agent), and a session
may stay open up to the per-session maximum (default 4 h, up to 8 h). Caddy's reverse proxy
passes the `Upgrade`/`Connection` headers and holds the connection open by default; if you set an
explicit read/idle timeout on the proxy, keep it above the shell's max duration or add a
longer-timeout rule for the two `/shell/` paths, otherwise the proxy cuts active terminals. The
server itself caps concurrent shells at **20 per server** (further opens get `429 too_many_shells`);
raise capacity by running more server replicas behind the proxy rather than lifting the cap.

## 3. Publishing agents

The dashboard prints an install one-liner next to every enrollment token; it downloads from
`BASE/dl/`. To populate that directory:

- `make agents` (= `scripts/build-agents.sh`) cross-compiles `msp-agent-linux-amd64`,
  `msp-agent-linux-arm64`, `msp-agent-windows-amd64.exe`, `msp-server-linux-amd64`, writes
  `SHA256SUMS`, copies `install.sh` / `install.ps1`, and copies the **signed release APK** to
  `msp-agent.apk` when one has been built (`make android-release`, below). With
  `MSP_AGENT_DIR` set it copies everything there.
- The Docker image ships the same set (minus the APK) at `/agents`. To add the APK, mount a
  host directory over `/agents` or bind `MSP_AGENT_DIR` to a directory you maintain.

**Android release signing.** The published APK is always the release build: it refuses
cleartext `http://` at the platform level, is not debuggable, and is signed with your own key
so phones can verify updates come from you. The debug APK (`make android`) permits cleartext,
is debuggable (`adb run-as` can read the device token on an unrooted phone) and is signed with
the public Android debug key; it is for lab use only and `build-agents.sh` never publishes it.
Create the key once and keep it with your backups (losing it means phones must uninstall and
re-enroll to take an update):

```sh
keytool -genkeypair -v -keystore /secure/msp-agent.jks -alias msp-agent \
  -keyalg RSA -keysize 4096 -validity 10000 -dname "CN=MSP Agent, O=Your MSP"
export MSP_ANDROID_KEYSTORE=/secure/msp-agent.jks MSP_ANDROID_KEYSTORE_PASSWORD=...   # optional: MSP_ANDROID_KEY_ALIAS, MSP_ANDROID_KEY_PASSWORD
make android-release && make agents
```

`*.jks`, `*.keystore` and `*.p12` are git-ignored. `build-agents.sh` refuses to copy an APK
that is debuggable or debug-signed, and `SHA256SUMS` is regenerated on every run.

`/dl/` serves single files only (no listing). Rebuild and republish after every upgrade so
`SHA256SUMS` matches the binaries, or the installers refuse to install.

## 4. Installing agents

### Ubuntu / Linux (systemd)

One-liner from the token dialog (run as root; it verifies `SHA256SUMS` before installing):

```sh
curl -fsSLo /tmp/msp-install.sh https://rmm.example.com/dl/install.sh \
  && sudo bash /tmp/msp-install.sh --server https://rmm.example.com --token et_...
```

Manual equivalent (also the path for a private CA or a custom interval):

```sh
sudo install -m 0755 msp-agent-linux-amd64 /usr/local/bin/msp-agent
sudo msp-agent install --server https://rmm.example.com --token et_... [--ca-file /etc/msp-agent/ca.pem] [--interval 30]
msp-agent status            # config path, device id, server
```

`install` enrolls, writes `/etc/msp-agent/agent.json` (dir 0700, file 0600, root), installs
`/etc/systemd/system/msp-agent.service` (the unit in `deploy/msp-agent.service`) and starts it.
The unit runs as root on purpose: remote scripts must be able to touch `/etc` and `/usr`.
`sudo msp-agent uninstall` stops, disables and removes the unit; the config file and the binary
stay in place (delete `/etc/msp-agent` and `/usr/local/bin/msp-agent` yourself if the machine
leaves management for good). Logs: `journalctl -u msp-agent -f`.

### Windows 10/11

From an **administrator** PowerShell:

```powershell
irm https://rmm.example.com/dl/install.ps1 -OutFile $env:TEMP\msp-install.ps1; & $env:TEMP\msp-install.ps1 -Server https://rmm.example.com -Token et_...
```

The script downloads `msp-agent-windows-amd64.exe` to `%ProgramData%\msp-agent\`, checks its
hash against `SHA256SUMS`, then runs `msp-agent.exe install`, which enrolls, writes
`%ProgramData%\msp-agent\agent.json` and registers the `msp-agent` service ("MSP Agent",
automatic start, restart on failure). Uninstall: `msp-agent.exe uninstall` (admin). Logs: Event
Viewer, Windows Logs > Application, source `msp-agent`. Commands run as LocalSystem; lock and
message reach the active console session.

### Android

Sideload `BASE/dl/msp-agent.apk` (enable "install unknown apps" for the browser or use
`adb install`). Open MSP Agent, enter the server URL and the enrollment token, tap Enroll,
then **Allow background** (battery optimisation exemption) and **Grant device admin** (needed
for remote lock). The agent is a foreground service with a permanent notification; it syncs
every 60 s and is shown offline after 195 s without a sync. Doze and OEM battery managers can
still delay syncs; a few false offline alerts on phones are expected in phase 1.

The published APK is the release build: it refuses cleartext and must talk to a server with
a certificate the phone trusts, and its enrollment screen rejects `http://` except to
localhost. The debug APK (`make android`) permits cleartext `http://` so a lab server without
TLS can be used; it is never published (section 3).

## 5. Enrollment-token hygiene

- One token per rollout batch, short `ttl_hours` (a day for a site visit, a week at most) and
  `max_uses` equal to the number of machines plus a small margin.
- Revoke the token (org page) as soon as the batch is enrolled; a leaked token can enroll rogue
  devices into that org until it expires or runs out of uses.
- Tokens are shown once; the server keeps only a hash and the first 10 characters.

## 6. Devices: revoke, re-enroll, delete

- **Revoke** (admin) nulls the device token: the agent gets `401`, logs
  "token rejected, re-enroll with msp-agent install" and retries every 5 min without exiting.
  History, commands and alerts stay.
- **Re-enroll** by running `msp-agent install` (or the installer) again with a fresh token;
  the same `(org, machine-id)` reuses the device row and rotates the token.
- **Cloned machines.** The Linux device id is `/etc/machine-id`; VM templates and imaged
  workstations often share one. Two such machines enrolled in the same org take turns owning
  one device row and cut each other off (the earlier agent gets `401` and logs "token
  rejected"). The audit row `device.enrolled` then carries `re_enrolled`, `previous_hostname`
  and a `warning`, and the server logs "device re-enrolled from a different host". Fix the
  image: `rm /etc/machine-id /var/lib/dbus/machine-id && systemd-machine-id-setup` (then
  re-enroll), and regenerate the id in your template before cloning.
- **Wrong org.** Enrolling a machine first with org A's token and then with org B's leaves a
  row in A that goes offline and alerts; revoke and delete that row.
- **Delete** (admin) removes the device and everything attached to it.

## 7. Backups and upgrades

Everything lives in one SQLite file. Back it up while the server runs with the online backup
API (consistent even in WAL mode; the `sqlite3` CLI runs on the host against the bind-mounted
file, the alpine image does not ship it):

```sh
sqlite3 deploy/data/msp.db ".backup deploy/data/backup-$(date +%F).db"
```

or stop the server and copy `msp.db` **together with** `msp.db-wal` and `msp.db-shm`. Restore by
putting the file back (and deleting any stale `-wal`/`-shm`) before starting the server.

Upgrade = replace the binary or pull the new image and restart; migrations run at start and
are recorded in `schema_migrations`. Downgrades are not supported: restore the backup taken
before the upgrade instead. Agents keep working across server upgrades as long as
`docs/PROTOCOL.md` does not change; agents are not self-updating in phase 1, re-run the
installer to update them.

**Command presets.** The "Presets…" dropdown of the device console is a library the server
serves at `GET /api/v1/presets`. Without `MSP_PRESETS` it is the builtin set (four entries
per platform, compiled into `internal/server/presets.go`). To run your own, copy
`deploy/presets.example.json` (ten entries per platform: updates, services, logs, Defender
scan, temp cleanup, ...) somewhere the server can read it, edit it and point `MSP_PRESETS`
(or `--presets`) at it; the file then replaces the builtin set entirely. Format and rules are
in `docs/PROTOCOL.md` section 6 ("Presets"): one `linux` and one `windows` list, each entry
`label` (unique per platform) + `kind` `exec` (`argv`) or `script` (`shell`, `body`) +
optional `timeout_s` (1..3600, default 60); unknown keys are refused. Edit the file in place
at any time: the server re-reads it when its mtime changes, on the next `GET /api/v1/presets`,
with no restart; the dashboard fetches the library once per sign-in, so a technician sees
the new list after a browser reload (F5) or a fresh login, not by merely switching pages. A file that does not load at start makes `msp-server serve` exit with a
message naming the file, the platform, the preset and the problem; a bad edit while running
logs a `WARN presets reload failed, keeping the last good set` line with the same detail
and the previous set stays in service until the file is fixed. With Docker, mount the file
(`./presets.json:/data/presets.json:ro`) and set `MSP_PRESETS=/data/presets.json`; the
compose file carries the commented lines.

## 8. Logs, retention, firewall

- Server: structured logs on stderr (`docker compose logs -f server`, or your supervisor).
  Every request is logged with method, path, status, duration and client IP.
- Agent: `journalctl -u msp-agent` on Linux, Event Log source `msp-agent` on Windows, the
  notification text on Android.
- Audit: dashboard Audit page (`/api/v1/audit`), append-only, never pruned.
- Retention: heartbeat samples are pruned hourly after `MSP_RETENTION_DAYS` (indexed, in
  batches of 5000 rows so syncs interleave); commands (with their
  captured output), alerts and audit rows are kept indefinitely. Watch the database size and
  run `VACUUM` after large deletions if needed.
- Firewall: agents need **outbound** HTTPS (443, or whatever port the server listens on)
  to the server only; nothing is opened inbound at client sites. The server needs 443 (or
  80 + 443 with Caddy) inbound and nothing else, plus the RustDesk relay ports of section 10
  when remote desktop is on. Restrict the dashboard to the MSP's own addresses at the proxy
  if you can; phase 1 has no built-in IP allowlist.

## 9. Health

`GET /healthz` returns `200 {"ok":true}` when the database answers; the Docker image uses it
as the container health check. `GET /api/agent/v1/ping` is the unauthenticated reachability
check agents and installers use.

## 10. Remote desktop (RustDesk)

Decision D4, phase A (feature RD1): the official RustDesk client on the endpoints and on
the technicians' PCs, talking only through a RustDesk ID/relay server (`hbbs`/`hbbr`) that
you host next to the MSP server. The MSP server stores each device's RustDesk id, shows it
on the device page with a **Connect** button, and audits every connection; the screen
session itself runs between the two RustDesk clients through your relay and never touches
the MSP server. Phase B (a branded client with the relay compiled in) is documented in
`docs/RUSTDESK-BRANDING.md` and not built. Facts below are from the RustDesk documentation
and source as checked on 2026-11-13 (`docs/RUSTDESK-BRANDING.md` lists the sources).

### 10a. The relay: hbbs + hbbr in compose

`deploy/docker-compose.yml` carries two services, both from the image
`rustdesk/rustdesk-server:1.1.16` (the latest release, 2026-07-20; pin
`1.1.16-amd64` on an x86_64-only host), with `network_mode: "host"` as in the official
compose example and `restart: unless-stopped`, **both in the compose profile `rustdesk`**:
a plain `docker compose ... up -d` (section 3, README) never starts them and nothing
listens on the RustDesk ports until you pass `--profile rustdesk`. With host networking
there is no port mapping to leave out, so the profile is the opt-in; keep passing it to
every later `up`/`restart` once you use remote desktop, or the two containers are left
alone (compose does not stop them either).

- `hbbs`, command `["hbbs", "-r", "<MSP_RUSTDESK_HOST>"]`: the ID/rendezvous server. `-r`
  names the relay the clients are told to use (hbbr on the same host, port 21117), so it
  takes the bare hostname. The value is passed as one argument whatever it contains, and
  when the variable is unset hbbs gets `-r ""` and advertises no default relay (the clients
  that `msp-agent rustdesk setup` configures carry `relay-server` themselves) rather than a
  placeholder hostname.
- `hbbr`, command `hbbr`: the relay.

Both bind-mount `./rustdesk-data` (next to the compose file, git-ignored) at `/root`. On
its first start `hbbs` generates an Ed25519 key pair there: `id_ed25519` (private: keep it,
back it up, never publish it) and `id_ed25519.pub` (public: one base64 line that every
client must know). To use a key of your own instead, pass `-k <key>` to both commands or set
`KEY` in their environment.

```sh
MSP_RUSTDESK_HOST=rmm.example.com docker compose -f deploy/docker-compose.yml --profile rustdesk up -d
cat deploy/rustdesk-data/id_ed25519.pub        # -> OeVuKk5nlHiXp+7qsR6CbcHBGxhU4PJkNbvoX3P5X0k=
```

Put both values in the `.env` file next to the compose file (or the server's environment):

```
MSP_RUSTDESK_HOST=rmm.example.com
MSP_RUSTDESK_KEY=OeVuKk5nlHiXp+7qsR6CbcHBGxhU4PJkNbvoX3P5X0k=
```

and `docker compose ... --profile rustdesk up -d` again. The host is the **bare** public DNS
name or IP as RustDesk clients reach it: no port, no scheme. hbbs is reached on 21116 and
hbbr on 21117, the defaults of both the server and the client, and the same string is
written as ID server and as relay server into every client, so a `:port` would point the
relay at the ID-server port. The key is the one base64 line of `id_ed25519.pub` (or the
`-k` value you gave both servers). With a bare binary the same two variables (or
`--rustdesk-host` / `--rustdesk-key`) go on `msp-server serve`. Both or neither: with only
one the server exits with "MSP_RUSTDESK_HOST and MSP_RUSTDESK_KEY must be set together",
and with a host that is not a bare hostname/IP or a key that is not one base64 line it
exits with the reason (the values go verbatim into the agents' `RustDesk2.toml`, so nothing
that would need quoting there is accepted; the agents refuse such values too).
`GET /api/v1/rustdesk` answers `{"enabled":true,...}` once it is on, and every agent learns
the host and key in its next sync response (no reinstall).

**Firewall** (inbound, on the relay host; host networking means the host firewall is what
opens them):

| port | proto | service | needed for |
|---|---|---|---|
| 21115 | tcp | hbbs | NAT type test |
| 21116 | tcp **and** udp | hbbs | ID registration, rendezvous, hole punching |
| 21117 | tcp | hbbr | relayed sessions |
| 21118 | tcp | hbbs | web client only; keep closed otherwise |
| 21119 | tcp | hbbr | web client only; keep closed otherwise |
| 21114 | tcp | hbbs | Pro web console only (not used) |

The official UFW example is `ufw allow 21114:21119/tcp` and `ufw allow 21116/udp`; with the
web client unused, `21115:21117/tcp` plus `21116/udp` is enough. Endpoints and technician
PCs need **outbound** access to those ports only.

### 10b. Publishing the client installers at /dl/

The agent installers can install the official RustDesk client, but the MSP server never
downloads it and `scripts/build-agents.sh` never does either: you fetch the release assets
once and publish them yourself. Current client release: **1.5.3** (2026-11-09); the assets the
installers look for are `rustdesk-1.5.3-x86_64.deb` (Linux amd64),
`rustdesk-1.5.3-aarch64.deb` (Linux arm64) and `rustdesk-1.5.3-x86_64.exe` (Windows), from
`https://github.com/rustdesk/rustdesk/releases/download/1.5.3/<asset>` (the always-current
page is `https://github.com/rustdesk/rustdesk/releases/latest`; an `.msi` also exists but the
installers use the `.exe`, whose `--silent-install` is the documented unattended path).

```sh
cd "$MSP_AGENT_DIR"     # the directory served at /dl/ (/agents in the Docker image, see section 3)
curl -fsSLO https://github.com/rustdesk/rustdesk/releases/download/1.5.3/rustdesk-1.5.3-x86_64.deb
curl -fsSLO https://github.com/rustdesk/rustdesk/releases/download/1.5.3/rustdesk-1.5.3-x86_64.exe
cd - && MSP_AGENT_DIR=... scripts/build-agents.sh --sums-only     # rewrites SHA256SUMS over the whole directory
```

`--sums-only` compiles nothing and copies nothing; it only regenerates `SHA256SUMS` so the
installers can verify the packages (they refuse an unlisted or mismatching file). A full
`make agents` run with `MSP_AGENT_DIR` set also re-sums the directory, so the packages stay
listed across agent upgrades. To publish a newer client, drop in the new assets and run the
installers with `RUSTDESK_VERSION=<v>` (Linux) / `-RustDeskVersion <v>` (Windows).

### 10c. Installing the client on endpoints

- Linux: `install.sh ... --with-rustdesk` downloads the `.deb` from `/dl/`, verifies it,
  installs it (`apt-get install -y ./file.deb`, or `dpkg -i`), then runs
  `msp-agent rustdesk setup` and restarts the agent so the id is reported at once.
- Windows: `install.ps1 ... -WithRustDesk` downloads the `.exe`, verifies it, runs
  `rustdesk.exe --silent-install` and `rustdesk.exe --install-service` (the documented
  unattended sequence), then `msp-agent.exe rustdesk setup` and restarts the agent service.
- Already enrolled machines: install RustDesk any way you like (or re-run the installer with
  the flag) and run `msp-agent rustdesk setup` as root/Administrator.

`msp-agent rustdesk setup [--password-stdin] [--config-string S]` does, in order: read the
relay host and key from `agent.json` (refuses when the server has not reported one yet, and
when the stored host is not a bare hostname/IP or the key not one base64 line); find the
client (`rustdesk` on PATH or `/usr/bin/rustdesk`; `%ProgramFiles%\RustDesk\rustdesk.exe`;
override with `MSP_RUSTDESK_BIN`); write the `[options]` block of `RustDesk2.toml`
(`custom-rendezvous-server`, `relay-server`, `key`) into the service account's RustDesk
config directory (`/root/.config/rustdesk`; on Windows the LocalService profile and the
invoking user's `%APPDATA%\RustDesk\config`), keeping other keys; set the permanent password
with the documented `rustdesk --password <pw>` (a random 16-character one, or the line piped
in with `--password-stdin`: there is no `--password P` flag, so the password never sits on
msp-agent's own command line); restart the RustDesk service (`systemctl restart rustdesk` /
the `RustDesk` Windows service, best effort); read the id with `rustdesk --get-id`; and write
the password to `rustdesk-password` next to `agent.json`, 0600 on Linux and, on Windows where
mode bits mean nothing, with a DACL for SYSTEM and Administrators only that the agent sets
on the directory and the file itself (also from `msp-agent install`; the installer's `icacls`
is only a belt and braces). The one unavoidable exposure is `rustdesk --password <pw>`, the
only documented way to set it: the password is on that process's command line for the
seconds it runs, so run setup on a machine nobody else is logged into
(`docs/SECURITY.md` section 8). The password is printed once and **never sent to the MSP
server**; a technician on the box can read it from that file. If you prefer
the fully documented configuration path, export the config string from any configured
client's Settings > Network and pass it with `--config-string`: it is applied with
`rustdesk --config <string>` instead of the TOML write. `msp-agent rustdesk id` prints the
local id.

The id reaches the server with the agent's facts (at enrollment, the first sync after a
start and every 120th sync), so after a manual setup restart the agent (`systemctl restart
msp-agent` / `Restart-Service msp-agent`) or wait up to an hour.

What is verified here and what is not: the CLI flags (`--get-id`, `--password`, `--config`,
`--silent-install`, `--install-service`) and `systemctl restart rustdesk` are from the
official client-deployment page; the `RustDesk2.toml` key names other than
`custom-rendezvous-server` and the per-OS config directories are the conventional ones and
could not be confirmed from primary documentation, so the first install on a real Linux
desktop and a real Windows PC is a manual test (`docs/STATUS.md`). If the relay does not
take, use `--config-string`.

### 10d. The technician's own PC

Install the same official client on each technician's PC, open Settings > Network and set
**ID server** and **Relay server** to `MSP_RUSTDESK_HOST` and **Key** to `MSP_RUSTDESK_KEY`
(the device page shows the relay; `GET /api/v1/rustdesk` returns both). Keep the client
running: the **Connect** button opens `rustdesk://connect/<id>`, which the client registers
as a URL scheme at install time and turns into a session to that id; the client asks for the
device's permanent password (from the installer output or `rustdesk-password` on the
device). If the link does nothing, the client is not installed or not registered for the
scheme: start it and enter the id by hand (the page shows it with a Copy button). The
technician's client must use the **same** host and key, or the id is unknown to it.

### 10e. Disabling and rotating

Unset both variables to turn the feature off: the card on every device page says it is not
configured, `POST .../remote-desktop` answers `409 rustdesk_disabled`, and the agents keep
their last known relay (harmless). To rotate the key, stop `hbbs`, delete `id_ed25519*`,
start it, put the new `.pub` line into `MSP_RUSTDESK_KEY`, restart the server, and re-run
`msp-agent rustdesk setup` on every endpoint and the client settings on every technician PC
(the agents receive the new key in their next sync but only `setup` applies it to RustDesk).

## 11. Notifications (email and Slack/webhook) (NS1)

Alert notifications send an email or an HTTP webhook when an alert opens or resolves, routed
per client org and severity, muted during maintenance windows. There is nothing to configure
in the environment: channels, routing rules and maintenance windows are created in the
dashboard (Notifications section) or over `/api/v1/notify/*` (docs/PROTOCOL.md). The dispatcher
is always running.

### 11a. The dispatcher

A background goroutine started in `Run` alongside the alert evaluator and the pruner (stopped
on shutdown). Every few seconds it claims the `pending` deliveries whose retry lease has
expired, renders the message for the channel kind, sends it through the real sender
(`net/smtp` for email, `net/http` for webhooks), and marks the row `sent` or records the
failure. Retries use a per-delivery backoff (the claim stamps `updated_at`, which both leases
the row so two ticks never grab it and schedules the next attempt); after `maxAttempts` (5) a
delivery is left `failed`. Enqueue is idempotent — one row per `(alert, rule, event)` — so the
evaluator running twice never double-sends. Nothing is tunable by env in NS1; the constants
live in `internal/server/notify_dispatch.go`.

### 11b. Channels

- **Email**: `config` = `{"host","port","from","to":[...],"starttls":true,"username":"..."}`,
  plus a write-only `secret` (the SMTP password, used only when `username` is set). STARTTLS is
  used when `starttls` is true and the server advertises it; the default port is 587. The
  message is plain text with the subject `[MSP] <severity> <type> on <hostname> (<org>)` and a
  dashboard link when `MSP_BASE_URL` is set.
- **Webhook**: `config` = `{"url","flavor":"slack"|"generic"}`. `slack` posts
  `{"text":"<subject> — <message>"}` (a Slack incoming webhook). `generic` posts
  `{alert_id, org, device, type, severity, message, event, at}`; if a `secret` is set it adds
  `X-MSP-Signature: sha256=<hmac of the body>` so the receiver can verify authenticity.

Use **Send test** on a channel to enqueue a `test` delivery immediately and watch it in the
delivery log; the dispatcher sends it on its next tick.

### 11c. Routing rules and maintenance windows

A rule routes matching alerts to one channel: it matches by org (a rule scoped to an org, or a
global rule), `min_severity` (the alert's severity must be at least this), an optional alert-type
list (empty = all of `offline`, `disk_full`, `memory_high`, `battery_low`), and the event
(on-open / on-resolve). A maintenance window mutes an org's (or all orgs') deliveries between an
absolute start and end: alerts still open, but their deliveries are recorded `suppressed` and
nothing is sent. Suppression is an operational mute, not a security control (docs/SECURITY.md
section 12).

### 11d. Firewall and secrets

The server makes **outbound** connections to the SMTP host/port and to each webhook URL, so the
server's egress must allow them (a locked-down host that only accepts inbound 443 will see every
delivery fail and land `failed` after the retries). Channel secrets are stored in the database in
the clear because they are credentials in use (docs/SECURITY.md section 2 and 13); they are
write-only over the API, never logged, and never placed in a delivery's `detail_json`, and
tokened URLs are redacted to `scheme://host` in logs and the delivery log. Keep the database and
backups private accordingly.

### 11e. Reading the delivery log

`GET /api/v1/notify/deliveries?status=&org_id=&limit=` (the Delivery log view) shows every
attempt newest first: time, org, device/alert, channel, event, a status chip
(`pending`/`sent`/`failed`/`suppressed`), the attempt count and the last error. A row stuck
`pending` with rising attempts points at an unreachable SMTP host or webhook (check egress and
the channel config); a `failed` row has given up after `maxAttempts`. All of it is org-scoped
for staff exactly like the rest of the dashboard.

### 11f. Not configured in the build container

No outbound mail or internet is available in the cloud build container, so the senders are
tested against a local fake SMTP listener and `httptest` webhooks (`internal/e2e`
`TestNotificationsDelivery`, `internal/server` `TestStdSenderEmailAgainstFakeSMTP`); a real
email to an inbox or a post to a real Slack workspace must be confirmed on a deployed host with
egress.
