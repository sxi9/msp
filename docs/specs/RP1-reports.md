# RP1 — Reports and exports (phase 2)

Turn the data the platform already stores into reports a technician and a client can read: device
**uptime**, **disk-usage trends**, and **patch compliance**, each viewable in the dashboard and
exportable as CSV. Read-only over existing tables (heartbeats, devices, patches, alerts) — **no new
migration, no agent change, no new command kind**. Fully testable in-container by seeding the store and
asserting the aggregates. All RS2 org-scoped.

## Data sources (already present)

- `heartbeats(device_id, at, cpu_pct, mem_used_pct, disk_used_pct, battery_pct, uptime_s)` — one row per
  device per minute, pruned after `MSP_RETENTION_DAYS` (default 7). Basis for uptime and disk trends.
- `devices` — `last_seen_at`, `interval_s`, `status`, the denormalized `disk_used_pct`, and the patch
  columns (`pending_patches`, `security_patches`, `reboot_required`, `patches_checked_at`).
- Period is bounded by retention: a report accepts `days` in 1..MSP_RETENTION_DAYS (reject longer with a
  clear message; default 7).

## Reports (server, staff session, RS2 org-scoped)

- `GET /api/v1/reports/uptime?org_id=&days=N` → per device: `{device_id, hostname, org_id, org_name,
  samples, expected, uptime_pct}`. `expected` = the number of heartbeat windows expected over the period
  given the device's `interval_s` (and the heartbeat ≤1/min throttle), `samples` = heartbeat rows seen;
  `uptime_pct = 100 * min(samples/expected, 1)`. A device enrolled mid-period counts only from
  enrollment. Also return an org summary (mean uptime). Document the approximation honestly (it measures
  "did the device check in", not OS uptime; the `uptime_s` column is the OS figure and is shown too).
- `GET /api/v1/reports/disk?device_id=&days=N` → a time series `[{at, disk_used_pct}]` for one device
  from heartbeats (the device must be RS2-visible), plus min/max/latest. `GET /api/v1/reports/disk/top?
  org_id=&days=N&limit=` → the devices with the highest latest disk usage (for a fleet view).
- `GET /api/v1/reports/patch-compliance?org_id=` → per org and overall: device counts by state
  `{total, up_to_date, pending, security_pending, reboot_required, never_checked}` derived from the
  device patch columns; and a per-device breakdown (hostname, pending, security, reboot_required,
  checked_at). "up_to_date" = checked and pending==0.
- All three are read-only, use `now()` for the window, and filter to the caller's visible org set in SQL
  (reuse the RS2 visible-org helpers); an explicit `org_id` outside the set → 404.

## Exports (CSV)

- `GET /api/v1/reports/{uptime|patch-compliance}.csv?org_id=&days=N` and
  `GET /api/v1/reports/disk.csv?device_id=&days=N` → `text/csv; charset=utf-8` with
  `Content-Disposition: attachment; filename="msp-<report>-<date>.csv"`, the same rows as the JSON
  report, properly CSV-quoted (quote fields containing comma/quote/newline; never allow CSV/formula
  injection — prefix a leading `=`,`+`,`-`,`@` in a text cell with a quote). RS2-scoped like the JSON
  endpoints. (Note: CSV download works in a real browser; it is not exercised by any artifact sandbox.)

## Dashboard

- A **Reports** page (nav entry; admin/operator/viewer — reports are read-only so viewer may see them):
  - Uptime: an org selector + period, a table of devices with an uptime bar/percentage and the org mean,
    and a "Download CSV" button.
  - Disk trends: a device picker (or the top-N fleet view) with a line/area chart of `disk_used_pct`
    over the period (reuse the existing dashboard sparkline/chart style; label axes; theme-aware), and a
    "Download CSV".
  - Patch compliance: the per-org summary tiles (up to date / pending / security / reboot required /
    never checked) and a per-device table, with a "Download CSV".
- Pure helpers (uptime math from samples/expected, series min/max/latest, compliance bucketing, CSV cell
  escaping mirrored for any client-side preview) in `web/src/lib/reports.ts` with a `node --test`.
  `?mock=1` seeds heartbeats/patch states so all three reports render. Phone width intact (charts and
  tables scroll/stack; no horizontal page scroll at 400 px).
- Charts: follow the dataviz guidance already used for the device sparklines — readable in light and
  dark, labelled, marks within bounds. Keep them simple SVG, no new chart dependency.

## Docs

`docs/PROTOCOL.md` (the report + CSV endpoints and shapes, the uptime approximation note),
`docs/GUIDE.md` (a "Reports" section: uptime, disk trends, patch compliance, exporting CSV, the
retention-bounded period), `docs/OPS.md` (reports are bounded by `MSP_RETENTION_DAYS`; longer history
needs a longer retention or the phase-3 rollups), `docs/SECURITY.md` (RS2-scoped, CSV-injection guard),
`docs/ARCHITECTURE.md`, `docs/FEATURES.md`, `docs/STATUS.md` (RP1 row WORKING), `docs/ROADMAP.md` (tick
reports: uptime/disk trends/patch compliance/exports), `docs/CHECKLIST.md`.

## Proof (what the build must show here)

The six proof commands pass. A new `internal/e2e` or server test that seeds a device with a known
heartbeat history and patch state, then:
- `GET /reports/uptime` returns the expected uptime_pct for the seeded samples/expected (and a device
  enrolled mid-period is prorated);
- `GET /reports/disk` returns the seeded series with correct min/max/latest;
- `GET /reports/patch-compliance` buckets devices correctly (up_to_date / security_pending / never
  checked);
- the `.csv` endpoints return well-formed, injection-safe CSV with the attachment header and the same
  data;
- RS2: an operator scoped to org A sees only A's devices in every report and gets 404 on an explicit
  org_id/device in B; a `days` beyond retention is rejected.

Verify stage screenshots the Reports page: the uptime table, a disk-trend chart, and the patch-
compliance summary.
