// RP1 reports: the pure math behind the Reports dashboard — device uptime from
// heartbeat samples vs. expected windows, the org mean, disk-series min/max/latest,
// patch-compliance bucketing, and a formula-injection-safe CSV builder used for the
// client-side previews/exports under ?mock=1. No DOM, no network; tested by
// reports.test.ts and mirrored by the server (docs/PROTOCOL.md). The row/point
// shapes defined here are re-exported as the report wire types from types.ts, the
// way lib/software.ts feeds SoftwareItem/SoftwareHit.

/** One device row of the uptime report. `uptime_pct = 100*min(samples/expected,1)`. */
export interface UptimeRow {
  device_id: number; hostname: string; org_id: number; org_name: string;
  /** Heartbeat rows seen over the period. */
  samples: number;
  /** Heartbeat windows expected over the period (prorated from enrollment). */
  expected: number;
  uptime_pct: number;
  /** The OS uptime the device last reported (heartbeat `uptime_s`); null if never. */
  uptime_s: number | null;
}

/** One point of a device's disk-usage time series. */
export interface DiskPoint { at: string; disk_used_pct: number | null }

/** One device row of the top-disk fleet view: its latest disk usage. */
export interface DiskTopRow { device_id: number; hostname: string; org_id: number; org_name: string; latest: number | null }

/** One device row of the patch-compliance breakdown. */
export interface PatchComplianceRow {
  device_id: number; hostname: string; org_id: number; org_name: string;
  pending: number; security: number; reboot_required: boolean; checked_at: string | null;
}

/**
 * Patch-compliance device counts by state. `never_checked`, `up_to_date` and
 * `pending` partition `total` (every device is exactly one of them);
 * `security_pending` is the subset of `pending` with a security update, and
 * `reboot_required` is an independent flag — both can overlap `pending`.
 */
export interface PatchCounts {
  total: number; up_to_date: number; pending: number; security_pending: number; reboot_required: number; never_checked: number;
}

const MS_PER_DAY = 86_400_000;
/** The heartbeat throttle: at most one heartbeat per minute, whatever the interval. */
export const MIN_INTERVAL_S = 60;

/** Clamp a requested report period to 1..maxDays (default retention 7); non-finite -> def. */
export function clampDays(days: unknown, maxDays = 7, def = 7): number {
  const n = Math.floor(Number(days));
  if (!Number.isFinite(n) || n < 1) return def;
  return Math.min(n, Math.max(1, maxDays));
}

/**
 * The uptime percentage for a device: `100 * min(samples/expected, 1)`, rounded to
 * one decimal. 0 when no window was expected (e.g. a device enrolled just now) and
 * negative samples/expected are floored at 0.
 */
export function uptimePct(samples: number, expected: number): number {
  if (!(expected > 0)) return 0;
  const ratio = Math.max(0, samples) / expected;
  return Math.round(Math.min(ratio, 1) * 1000) / 10;
}

/**
 * The number of heartbeat windows expected over `days`, one per effective interval
 * (the agent interval floored at the 1/min throttle). A device enrolled mid-period
 * counts only from enrollment, so its expected total is prorated.
 */
export function expectedWindows(days: number, intervalS: number, enrolledAtMs?: number | null, nowMs = Date.now()): number {
  const periodStart = nowMs - clampDays(days) * MS_PER_DAY;
  const start = enrolledAtMs != null && enrolledAtMs > periodStart ? enrolledAtMs : periodStart;
  const spanS = Math.max(0, nowMs - start) / 1000;
  const eff = Math.max(MIN_INTERVAL_S, Math.floor(intervalS) || MIN_INTERVAL_S);
  return Math.floor(spanS / eff);
}

/** The mean of the rows' `uptime_pct` (rounded to one decimal); 0 for no rows. */
export function meanUptime(rows: { uptime_pct: number }[]): number {
  if (rows.length === 0) return 0;
  const sum = rows.reduce((s, r) => s + r.uptime_pct, 0);
  return Math.round((sum / rows.length) * 10) / 10;
}

/** The min, max and latest of a disk series, ignoring nulls; latest = last non-null. */
export function seriesStats(points: DiskPoint[]): { min: number | null; max: number | null; latest: number | null } {
  let min: number | null = null, max: number | null = null, latest: number | null = null;
  for (const p of points) {
    const v = p.disk_used_pct;
    if (v === null || v === undefined || Number.isNaN(v)) continue;
    if (min === null || v < min) min = v;
    if (max === null || v > max) max = v;
    latest = v;
  }
  return { min, max, latest };
}

/** A device as far as patch compliance is concerned (the denormalised patch columns). */
export interface PatchStateInput {
  pending_patches: number; security_patches: number; reboot_required: boolean; patches_checked_at: string | null;
}

/**
 * Bucket devices into the compliance counts. A device with no `patches_checked_at`
 * is `never_checked`; otherwise it is `up_to_date` when nothing is pending, else
 * `pending`. `security_pending` and `reboot_required` are counted independently.
 */
export function bucketDevices(devices: PatchStateInput[]): PatchCounts {
  const c: PatchCounts = { total: 0, up_to_date: 0, pending: 0, security_pending: 0, reboot_required: 0, never_checked: 0 };
  for (const d of devices) {
    c.total++;
    if (!d.patches_checked_at) { c.never_checked++; continue; }
    if (d.pending_patches > 0) c.pending++; else c.up_to_date++;
    if (d.security_patches > 0) c.security_pending++;
    if (d.reboot_required) c.reboot_required++;
  }
  return c;
}

// --- CSV --------------------------------------------------------------------
// A small CSV builder that mirrors the server's exporter: text cells with a
// leading =,+,-,@ (or a control char) get an apostrophe so a spreadsheet never
// evaluates them as a formula, and any cell containing a comma, quote or newline
// is wrapped in double quotes with embedded quotes doubled. Numbers and booleans
// are emitted verbatim (never formula-guarded, so a negative number stays a number).

type Cell = string | number | boolean | null | undefined;

/** Escape one CSV cell (see module note). */
export function csvCell(value: Cell): string {
  if (typeof value === 'number') return Number.isFinite(value) ? String(value) : '';
  if (typeof value === 'boolean') return value ? 'true' : 'false';
  let s = value === null || value === undefined ? '' : String(value);
  if (s !== '' && /^[=+\-@\t\r]/.test(s)) s = `'${s}`; // formula-injection guard (text cells only)
  if (/[",\n\r]/.test(s)) s = `"${s.replace(/"/g, '""')}"`;
  return s;
}

/** Join one row of values into a CSV line. */
export function csvRow(cells: Cell[]): string {
  return cells.map(csvCell).join(',');
}

/** A CSV document: a header line then the rows, CRLF-separated with a trailing CRLF. */
export function csvDocument(header: string[], rows: Cell[][]): string {
  return [csvRow(header), ...rows.map(csvRow)].join('\r\n') + '\r\n';
}

/** The uptime report as CSV (same rows as the JSON report). */
export function uptimeCsv(rows: UptimeRow[]): string {
  return csvDocument(
    ['device_id', 'hostname', 'org_id', 'org_name', 'samples', 'expected', 'uptime_pct', 'os_uptime_s'],
    rows.map((r) => [r.device_id, r.hostname, r.org_id, r.org_name, r.samples, r.expected, r.uptime_pct, r.uptime_s ?? '']),
  );
}

/** One device's disk series as CSV. */
export function diskCsv(points: DiskPoint[]): string {
  return csvDocument(['at', 'disk_used_pct'], points.map((p) => [p.at, p.disk_used_pct ?? '']));
}

/** The patch-compliance per-device breakdown as CSV. */
export function patchComplianceCsv(rows: PatchComplianceRow[]): string {
  return csvDocument(
    ['device_id', 'hostname', 'org_id', 'org_name', 'pending', 'security', 'reboot_required', 'checked_at'],
    rows.map((r) => [r.device_id, r.hostname, r.org_id, r.org_name, r.pending, r.security, r.reboot_required, r.checked_at ?? '']),
  );
}
