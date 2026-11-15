# WL1 — Service/process watchlists and log forwarding (phase 2)

Watch specific services and processes on desktops and alert when one is down, and forward selected
log entries (journald on Linux, Event Log on Windows) to the server for a per-device viewer. Linux is
verified in-container with a fake `systemctl`/`journalctl` (read-only; the container has no systemd
PID 1, so real calls are not used in tests); the Windows service-query and Event-Log paths are
fixture-tested and cross-built, marked NOT VERIFIED until a real host. No real log source is tailed in
tests. All RS2 org-scoped.

## Model (migration `0015_watchlists_logs.sql`)

- `watchlist_items(id, org_id INTEGER NULL REFERENCES orgs(id) ON DELETE CASCADE, scope TEXT CHECK(scope
  IN ('org','device')), device_id INTEGER NULL REFERENCES devices(id) ON DELETE CASCADE, kind TEXT
  CHECK(kind IN ('service','process')), name TEXT NOT NULL, alert INTEGER NOT NULL DEFAULT 1,
  created_at, created_by)`. An org-scoped item applies to every desktop in the org; a device-scoped item
  to one device. `name` is the unit/service name or the process/executable name.
- Device service/process state is reported in `facts` and stored on the device:
  `devices` gains `services_json TEXT NOT NULL DEFAULT '[]'` (latest `[{kind,name,running}]` for the
  device's applicable watchlist) and `services_checked_at TEXT NULL`.
- `device_logs(id, device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE, org_id INTEGER
  NOT NULL, at TEXT NOT NULL, source TEXT CHECK(source IN ('journald','eventlog')), unit TEXT NOT NULL
  DEFAULT '', level TEXT NOT NULL DEFAULT '', message TEXT NOT NULL)` with an index on
  `(device_id, at)`. Retention: pruned after `MSP_LOG_RETENTION_DAYS` (new config, default 3) by the
  existing hourly pruner; a global row cap per device (e.g. keep the newest 5000) to bound growth.
- `alerts.type` CHECK widens to add `service_down` — recreate the `alerts` table FK-safely (same pattern
  as 0007/0008), preserving rows and the open-alert unique index.
- Store: watchlist CRUD (org-scoped), `WatchlistForDevice(device)` (org + device items for the device's
  platform), `RecordServices(deviceID, items, now)`, `AppendLogs(deviceID, orgID, entries)` (capped),
  `ListLogs(deviceID, filter, limit)`, prune.

## Agent

- `services.go` + `services_linux.go` (build tag linux) / `services_windows.go` (windows) /
  `services_other.go`: given the device's watchlist (delivered to the agent — see below), report each
  item's running state. Linux: `systemctl is-active <unit>` for services (fake in tests) and a
  `/proc` scan (or `pgrep`) for processes; Windows: the service control manager query + a process
  snapshot. Behind the injectable executor. Pure parsers unit-tested with fixtures.
- Delivering the watchlist to the agent: the sync response gains an optional `watch` list
  `[{kind,name}]` (the device's applicable items); the agent reports their state in the next sync's
  `facts.services`. Keep it small (cap ~200 items).
- `logs.go` + platform collectors: forward log entries matching a filter — **bounded**: only the device's
  watchlist units (or a configured allowlist) at `level >= warning`, since a cursor the agent keeps,
  at most N entries and ~128 KiB per sync. Linux: `journalctl --since <cursor> -o json -p warning`
  (fake in tests); Windows: Event Log query. Sent in the sync request as `logs:[{at,source,unit,level,
  message}]`; the agent advances its cursor only after the server acks (the sync 200). Never log the
  forwarded content itself to the agent's own logs.
- Protocol: add `Facts.Services []ServiceState`, a `SyncRequest.Logs []LogEntry`, and
  `SyncResponse.Watch []WatchItem`; `Validate` bounds all three; fixtures + types_test. Facts byte-budget
  (the AU-era `boundSyncBody`) must account for services + logs too — trim logs first, then software.

## Server

- On sync/enroll: `RecordServices` from `facts.services`; `AppendLogs` from `req.Logs`; compute and send
  the device's `watch` list in the response. The alert evaluator opens a `service_down` alert (severity
  medium, configurable) for any watchlist item with `alert=1` whose latest state is not running, and
  resolves it when running again (same open/resolve machinery as the other alert types; it also feeds
  notifications + triggered automations already built).
- Endpoints (staff, RS2): `GET/POST/PUT/DELETE /api/v1/watchlist` (items, org/device scoped, audit
  `watchlist.*`); `GET /api/v1/devices/{id}/logs?unit=&level=&limit=` (the device's forwarded logs,
  RS2-visible device). `MSP_LOG_RETENTION_DAYS` in config + docs.

## Dashboard

- Device page: a **Watched services** panel (each item: name, kind, running/stopped chip, whether it
  alerts) and a **Logs** panel (recent forwarded entries with unit/level filter, newest first, in a
  scroll box). A **Watchlists** admin area (org and device items CRUD).
- Fleet/alerts: `service_down` alerts show like any alert; a column/filter for devices with a stopped
  watched service.
- Pure helpers (service-state summary, log-level filter/format, watchlist applicability for a device) in
  `web/src/lib/watchlist.ts` with a `node --test`. `?mock=1` seeds a watchlist, a stopped service (open
  `service_down` alert) and some log lines. Phone width intact (logs/table scroll in their own box).

## Docs

`docs/PROTOCOL.md` (facts.services, sync logs + watch, the watchlist + logs endpoints, the
`service_down` alert, `MSP_LOG_RETENTION_DAYS`), `docs/GUIDE.md` (a "Watchlists and logs" section),
`docs/OPS.md` (log retention + the per-device cap, that forwarding is bounded to watched units at
warning+), `docs/SECURITY.md` (RS2 scope, forwarded logs may contain sensitive text — retention + who
can read them), `docs/ARCHITECTURE.md`, `docs/FEATURES.md`, `docs/STATUS.md` (WL1 row: Linux WORKING,
Windows NOT VERIFIED), `docs/ROADMAP.md` (tick service/process watchlists + event-log/journal
forwarding), `docs/CHECKLIST.md`.

## Proof (what the build must show here)

The six proof commands pass. A new `internal/e2e` test with the real Linux agent + a fake
`systemctl`/`journalctl` on PATH:
- define a watchlist service item; the agent reports it stopped (fake systemctl), the server opens a
  `service_down` alert, and reports it running resolves it; `watchlist.*` audited;
- the agent forwards warning+ journal entries (fake journalctl json) which appear in
  `GET /api/v1/devices/{id}/logs` with the right unit/level, bounded to the cap, cursor advancing so the
  same lines are not re-sent;
- RS2: an operator scoped to org A cannot see/manage another org's watchlist or read another device's
  logs (404);
- the services + logs byte budget keeps the sync body under the limit (trim logs first), and android is
  excluded from watchlists/log forwarding.

Verify stage screenshots the Watched-services panel (with a stopped service), the device Logs panel, and
the Watchlists admin area.
