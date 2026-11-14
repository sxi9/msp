# SI1 — Installed-software inventory on desktops (phase 2)

Report the installed software on Windows and Linux devices, show it on the device page, and let a
technician search the whole fleet for a package (for license and vulnerability hunting). This mirrors
the Android installed-apps inventory already built (see `internal/agent` apps collector, `protocol.App`,
the dashboard "Installed apps" panel) — follow that pattern. Fully testable on Linux with real
`dpkg-query` (read-only, safe) and captured fixtures; the Windows registry path is fixture-tested and
cross-built, marked NOT VERIFIED until a real Windows host.

## Report

The agent collects installed software on the periodic facts cadence (first sync after start, and the
same refresh interval facts already use) and sends it in `facts` as `software`:

```
"software":[{"name":"openssl","version":"3.0.2","publisher":"Ubuntu","source":"dpkg"}]
```

- `source` is `dpkg` | `rpm` | `registry`. `publisher` may be empty. Capped at 2000 entries and a byte
  budget (~256 KiB, matching the Android apps cap approach) so the sync body stays under the agent body
  limit; sorted by name. Android keeps sending `apps` (phone apps); desktops send `software` (OS
  packages/programs). A device never sends both.
- `protocol`: add `SoftwareItem{Name, Version, Publisher, Source}` and `Facts.Software []SoftwareItem`
  `json:"software,omitempty"`; `Facts.Validate` bounds the list and field lengths (name ≤256,
  version ≤128, publisher ≤256, source in the enum). Fixture `sync_software.json`, extend types_test.

## Agent (`internal/agent`)

- `software.go`: a `softwareCollector` behind the injectable command executor (same seam as PM1 patch).
  - Linux (`software_dpkg.go`, build tag linux): `dpkg-query -W -f '${Package}\t${Version}\t${Maintainer}\n'`
    parsed; if dpkg is absent try `rpm -qa --qf '...'`; else empty. Read-only; safe to run for real.
  - Windows (`software_windows.go`, build tag windows): read the uninstall registry keys
    (`HKLM\...\Uninstall` and the WOW6432Node + HKCU variants) via `golang.org/x/sys/windows/registry`,
    emitting DisplayName/DisplayVersion/Publisher. Fixture-tested parser; runtime NOT VERIFIED.
  - `software_other.go`: empty.
  - Pure parsers unit-tested with captured `dpkg-query` output and a captured registry dump; cap/sort/
    byte-budget tested like the Android apps collector. Wire into the facts the sync loop already sends.

## Store

Migration `0011_device_software.sql`: add to `devices` the columns `software_json TEXT NOT NULL DEFAULT
'[]'`, `software_count INTEGER NOT NULL DEFAULT 0`, `software_checked_at TEXT NULL`. Store method
`RecordSoftware(deviceID, items, now)` (sets the json, count, checked-at), called from the sync/enroll
handler when the request carries `facts.software`. DEVICE JSON gains `software` (the list),
`software_count`, `software_checked_at`. The device list gains `software_count`.

Fleet search: `SoftwareSearch(ctx, visibleOrgs, query, limit)` → rows of `{device_id, hostname, org_id,
org_name, name, version}` for packages whose name matches (case-insensitive substring), org-scoped.
Implement by scanning each visible device's `software_json` in Go (phase-1 scale) or a LIKE over the
json — pick one and document; cap results (e.g. 500) and `log()` truncation.

## API and dashboard

- `GET /api/v1/software?q=<substring>&org_id=` (staff, session auth, RS2 org-scoped) → the matching
  rows across the caller's visible devices. `q` required, ≥2 chars. Used by the fleet software search.
- Device page "Installed software" panel: count, last checked, a search box filtering name/publisher,
  a table (Name, Version, Publisher, Source) in a scroll box. Mirror the Android apps panel; for an
  Android device keep showing the apps panel instead.
- A **Software inventory search** on the Software page (or Fleet): a query box → the cross-fleet results
  table (device → package/version), each device linking to its page. Org-scoped.
- Pure helpers (filter/sort, source label, search-result grouping) in `web/src/lib/software.ts` with a
  `node --test`. `?mock=1` gets desktop devices with software lists and the search working. Phone width
  intact.

## Docs

`docs/PROTOCOL.md` (the `software` facts field + the search endpoint + DEVICE fields), `docs/GUIDE.md`
(a "Software inventory" subsection: per-device list and the fleet search), `docs/ARCHITECTURE.md`,
`docs/FEATURES.md`, `docs/STATUS.md` (SI1 row: Linux WORKING, Windows registry NOT VERIFIED),
`docs/ROADMAP.md` (tick installed-software inventory on desktops), `docs/CHECKLIST.md`.

## Proof (what the build must show here)

The six proof commands pass. A new `internal/e2e` test (or server+agent tests):
- the real Linux agent reports installed software from real `dpkg-query` (or a fake dpkg fixture on
  PATH if real dpkg output is unstable), visible in `GET /api/v1/devices/{id}` with a plausible count;
- `GET /api/v1/software?q=<a package the fixture contains>` returns that device with the package;
- RS2: an operator scoped to another org sees neither the device nor its software in the search (no
  cross-org rows);
- the dpkg parser and the Windows-registry parser match their captured fixtures; cap/byte-budget hold.

Verify stage screenshots the device Installed-software panel and the fleet software-search results.
