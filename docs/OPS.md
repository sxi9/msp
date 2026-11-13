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
  80 + 443 with Caddy) inbound and nothing else. Restrict the dashboard to the MSP's own
  addresses at the proxy if you can; phase 1 has no built-in IP allowlist.

## 9. Health

`GET /healthz` returns `200 {"ok":true}` when the database answers; the Docker image uses it
as the container health check. `GET /api/agent/v1/ping` is the unauthenticated reachability
check agents and installers use.
