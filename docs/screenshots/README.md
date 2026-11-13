# Screenshots

Taken 2026-11-13 in the cloud build container from a real run: `bin/msp-server serve` on
127.0.0.1:18091 with a fresh SQLite file, an admin user, one client org ("Acme Dental") and one
enrollment token created through the CLI (`user add`, `org add`, `token new`), and the real
`bin/msp-agent` enrolled on the Linux build host (hostname `vm`, sync interval 2 s). Captured with
Playwright (chromium, deviceScaleFactor 2, light theme) as full-page images; every page exists at
desktop width (1280 px, `*-desktop.png`) and phone width (400 px, `*-phone.png`). At phone width
`document.documentElement.scrollWidth` was 400 on every page, i.e. no horizontal scroll.

The only secret visible is the demo enrollment token `et_SlePP8s…` created on the org page for the
screenshot; the database it belonged to was deleted after the run.

| Page | Desktop | Phone | What it shows |
|---|---|---|---|
| Login | [01-login-desktop.png](01-login-desktop.png) | [01-login-phone.png](01-login-phone.png) | Sign-in form with the admin email filled in (password masked). |
| Fleet | [02-fleet-desktop.png](02-fleet-desktop.png) | [02-fleet-phone.png](02-fleet-phone.png) | Device list grouped by org: online dot, OS, arch, last seen, CPU/memory/disk bars, battery, open alerts, agent version; org/status/hostname filters; header counts "1 online 0 offline 0 pending". |
| Orgs | [03-orgs-desktop.png](03-orgs-desktop.png) | [03-orgs-phone.png](03-orgs-phone.png) | Client org list with device, online and open-alert counts and the "Add client" form. |
| Org page with enrollment token | [04-org-enrollment-token-desktop.png](04-org-enrollment-token-desktop.png) | [04-org-enrollment-token-phone.png](04-org-enrollment-token-phone.png) | Acme Dental: its devices, the "New enrollment token" form, the token just created shown once with the Linux / Windows / Android install one-liners, and the token table (the CLI token `lab` 1/5 used, the new `front desk` 0/10) with Revoke buttons. |
| Device page | [05-device-desktop.png](05-device-desktop.png) | [05-device-phone.png](05-device-phone.png) | Status online, IP, OS, agent version and interval, uptime, enrolled time; CPU / memory / disk sparklines; facts (CPUs, CPU model, IPs, kernel, memory, model, serial); disks table; Actions (Lock device, Ping, Send message); Run command/script console; empty command history. |
| Command run with output | [06-device-command-output-desktop.png](06-device-command-output-desktop.png) | [06-device-command-output-phone.png](06-device-command-output-phone.png) | `uname -a` typed as argv, queued from the dashboard, delivered on the agent's sync, result card with exit 0 and the stdout `Linux vm 6.18.44-fc-v445 …`, and the `done` row in the command history. |
| Lock request in the history | [07-device-lock-history-desktop.png](07-device-lock-history-desktop.png) | [07-device-lock-history-phone.png](07-device-lock-history-phone.png) | "Lock device" confirmed in page; the `lock` command was delivered and executed by the agent (`loginctl lock-sessions`) and shows `done`, exit 0, stdout "no interactive session" with loginctl's stderr, because the headless build container has no desktop session to lock. The history now lists lock and exec. |
| Alerts (healthy) | [08-alerts-empty-desktop.png](08-alerts-empty-desktop.png) | [08-alerts-empty-phone.png](08-alerts-empty-phone.png) | Alerts page with the Open/All tabs and org filter while the device is online: "No open alerts." |
| Audit log | [09-audit-desktop.png](09-audit-desktop.png) | [09-audit-phone.png](09-audit-phone.png) | Every action of the run with actor type (user vs device), action, target, IP and JSON detail: device.enrolled, user.login, token.created, command.queued, command.finished, device.lock_requested. |
| Users | [10-users-desktop.png](10-users-desktop.png) | [10-users-phone.png](10-users-phone.png) | Users page (admin only): the admin account (marked YOU) and the "Create user" form with a role select (operator default). |
| Alert opened | [11-alerts-offline-open-desktop.png](11-alerts-offline-open-desktop.png) | [11-alerts-offline-open-phone.png](11-alerts-offline-open-phone.png) | The agent process was stopped; after the offline window (3 × 2 s + 15 s) and the evaluator tick the server opened an `offline` alert, severity high, "no sync for 23s"; the nav shows the Alerts badge and "1 offline". |
| Alert acknowledged | [12-alerts-offline-acked-desktop.png](12-alerts-offline-acked-desktop.png) | [12-alerts-offline-acked-phone.png](12-alerts-offline-acked-phone.png) | The same alert after clicking Ack: state "acked 0 s ago", Ack button gone. |
| Fleet with the device offline | [13-fleet-offline-desktop.png](13-fleet-offline-desktop.png) | [13-fleet-offline-phone.png](13-fleet-offline-phone.png) | Fleet after the agent stopped: amber dot, last seen 27 s ago, "0 online 1 offline", open alert count 1 on the row. |
| Device page with a preset picked | [14-device-presets-desktop.png](14-device-presets-desktop.png) | [14-device-presets-phone.png](14-device-presets-phone.png) | Second run (127.0.0.1:18093, fresh database, server started with `MSP_PRESETS=deploy/presets.example.json`): the Presets dropdown in the Run console is filled from `GET /api/v1/presets` (`source: file`, 10 Linux entries). "Failed services" was picked, which put `systemctl` / `--failed` / `--no-pager` into the argv box and 60 into Timeout. Headless Chromium draws the native dropdown popup outside the page, so the picked state is shown instead of the open list. The phone image was taken after the desktop run, so its history already lists that run. |
| Preset run with output | [15-device-preset-run-desktop.png](15-device-preset-run-desktop.png) | [15-device-preset-run-phone.png](15-device-preset-run-phone.png) | "Run on vm" with the preset's argv: delivered on the agent's sync, result card `failed`, exit 1, took 3 ms, stderr "System has not been booted with systemd as init system (PID 1)" because the build container has no systemd; the row appears in the command history. |

Screenshots 14 and 15 come from a second run on 127.0.0.1:18093 with its own fresh database (also deleted
afterwards); the enrollment token for that run was created through the CLI and is not shown.

Not shown because they cannot run here: the Windows service and Windows lock (cross-compiled only),
the Android app (the debug APK builds and its JVM tests pass; it needs a phone), and the Docker image
(`docker compose config` validates; no daemon in the container to build it).
