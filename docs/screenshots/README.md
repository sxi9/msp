# Screenshots

Taken 2026-11-13 in the cloud build container from a real run: `msp-server serve` with a fresh
SQLite file, an admin user, one client org ("Acme Dental"), one enrollment token, and a real
`msp-agent` enrolled on the Linux build host (sync interval 2 s). Each image carries its label
in the top banner. Desktop width 1280 px unless marked phone (400 px). Light theme.

| Image | What it shows | Result |
|---|---|---|
| ![](01-login.png) | Login page | WORKING |
| ![](02-fleet.png) | Fleet: every device across all clients, grouped by org; online dot, OS, version, last seen, CPU/memory/disk bars, battery, open alerts | WORKING |
| ![](03-orgs.png) | Clients list with device and alert counts | WORKING |
| ![](04-org-tokens.png) | Client page: enrollment tokens (label, uses, expiry, revoke) and per-OS install one-liners | WORKING |
| ![](05-device.png) | Device page: status, metrics, facts, disks, Actions (Lock, Ping, Message), Run command/script | WORKING |
| ![](06-command-output.png) | `uname -a` queued from the dashboard, delivered on the agent's sync, output and exit code shown, history row | WORKING |
| ![](07-lock.png) | Lock requested on a managed device: queued, delivered, executed, audited. On this headless build container it reports `failed` because there is no desktop session to lock (no logind, no xdg-screensaver). | WORKING (delivery); NOT VERIFIED (actual lock, needs a real Ubuntu desktop or Windows PC) |
| ![](08-ping-history.png) | Command history with kind chips and status: exec done, lock failed, ping done | WORKING |
| ![](09-audit.png) | Audit log: login, org created, token created, device enrolled, commands queued and finished, lock requested | WORKING |
| ![](10-users.png) | Users page (admin only) | WORKING |
| ![](11-alerts-empty.png) | Alerts page while the device is healthy | WORKING |
| ![](12-fleet-phone.png) | Fleet at phone width, no horizontal scroll (scrollWidth 400) | WORKING |
| ![](13-device-phone.png) | Device page at phone width | WORKING |
| ![](14-alert-offline.png) | Agent stopped; after 3 × interval + 15 s the evaluator opened an `offline` alert (severity high) | WORKING |
| ![](15-alert-acked.png) | Alert acknowledged by the technician | WORKING |
| ![](16-fleet-offline.png) | Fleet now shows the device offline | WORKING |

Not shown because they cannot run here: the Windows service and Windows lock (cross-compiled only),
the Android app (APK builds; needs a phone), and the Docker image (no Docker daemon in the container).
