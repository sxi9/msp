import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  bucketDevices, clampDays, csvCell, csvDocument, csvRow, diskCsv, expectedWindows, meanUptime,
  patchComplianceCsv, seriesStats, uptimeCsv, uptimePct,
  type DiskPoint, type PatchComplianceRow, type PatchStateInput, type UptimeRow,
} from './reports.ts';

test('clampDays: 1..max, default on junk', () => {
  assert.equal(clampDays(7), 7);
  assert.equal(clampDays(3), 3);
  assert.equal(clampDays(1), 1);
  assert.equal(clampDays(0), 7); // below 1 -> default
  assert.equal(clampDays(99, 7), 7); // above retention -> clamped
  assert.equal(clampDays('abc'), 7);
  assert.equal(clampDays(undefined), 7);
  assert.equal(clampDays(2.9), 2); // floored
  assert.equal(clampDays(30, 14, 14), 14);
});

test('uptimePct: 100*min(samples/expected,1), one decimal, zero-expected and negatives', () => {
  assert.equal(uptimePct(100, 100), 100);
  assert.equal(uptimePct(50, 100), 50);
  assert.equal(uptimePct(7190, 7200), 99.9);
  assert.equal(uptimePct(9999, 100), 100); // capped at 100
  assert.equal(uptimePct(0, 100), 0);
  assert.equal(uptimePct(5, 0), 0); // nothing expected -> 0
  assert.equal(uptimePct(5, -1), 0);
  assert.equal(uptimePct(-5, 100), 0); // floored at 0
  assert.equal(uptimePct(1, 3), 33.3); // rounding
});

test('expectedWindows: throttle floor, proration from enrollment, period clamp', () => {
  const now = Date.parse('2026-11-14T00:00:00Z');
  const dayMs = 86_400_000;
  // 7 days at a 30s interval -> throttled to 60s -> 7*1440 windows.
  assert.equal(expectedWindows(7, 30, now - 30 * dayMs, now), 7 * 1440);
  // A 120s interval over 1 day -> 720 windows.
  assert.equal(expectedWindows(1, 120, now - 30 * dayMs, now), 720);
  // Enrolled 2 days ago but a 7-day window -> prorated to 2 days.
  assert.equal(expectedWindows(7, 60, now - 2 * dayMs, now), 2 * 1440);
  // Enrolled before the window -> full window, not more.
  assert.equal(expectedWindows(3, 60, now - 30 * dayMs, now), 3 * 1440);
  // No enrollment given -> full window.
  assert.equal(expectedWindows(1, 60, null, now), 1440);
  // Enrolled in the future (clock skew) -> 0.
  assert.equal(expectedWindows(7, 60, now + dayMs, now), 0);
});

test('meanUptime: mean to one decimal, 0 for none', () => {
  assert.equal(meanUptime([]), 0);
  assert.equal(meanUptime([{ uptime_pct: 100 }, { uptime_pct: 50 }]), 75);
  assert.equal(meanUptime([{ uptime_pct: 100 }, { uptime_pct: 99 }, { uptime_pct: 98 }]), 99);
  assert.equal(meanUptime([{ uptime_pct: 33.3 }, { uptime_pct: 33.3 }, { uptime_pct: 33.4 }]), 33.3);
});

test('seriesStats: min/max/latest ignore nulls; latest is the last non-null', () => {
  const pts: DiskPoint[] = [
    { at: 'a', disk_used_pct: 40 },
    { at: 'b', disk_used_pct: null },
    { at: 'c', disk_used_pct: 55.5 },
    { at: 'd', disk_used_pct: 42 },
  ];
  assert.deepEqual(seriesStats(pts), { min: 40, max: 55.5, latest: 42 });
  // A trailing null does not change latest.
  assert.deepEqual(seriesStats([...pts, { at: 'e', disk_used_pct: null }]), { min: 40, max: 55.5, latest: 42 });
  // All null / empty -> all null.
  assert.deepEqual(seriesStats([{ at: 'a', disk_used_pct: null }]), { min: null, max: null, latest: null });
  assert.deepEqual(seriesStats([]), { min: null, max: null, latest: null });
  // A single point is min, max and latest.
  assert.deepEqual(seriesStats([{ at: 'a', disk_used_pct: 12 }]), { min: 12, max: 12, latest: 12 });
});

const dev = (extra: Partial<PatchStateInput>): PatchStateInput =>
  ({ pending_patches: 0, security_patches: 0, reboot_required: false, patches_checked_at: null, ...extra });

test('bucketDevices: partitions never_checked/up_to_date/pending, counts security + reboot', () => {
  const counts = bucketDevices([
    dev({ patches_checked_at: null }), // never checked
    dev({ patches_checked_at: '2026-11-14T00:00:00Z', pending_patches: 0 }), // up to date
    dev({ patches_checked_at: '2026-11-14T00:00:00Z', pending_patches: 3, security_patches: 1 }), // pending + security
    dev({ patches_checked_at: '2026-11-14T00:00:00Z', pending_patches: 2, security_patches: 0, reboot_required: true }), // pending + reboot
    dev({ patches_checked_at: '2026-11-14T00:00:00Z', pending_patches: 0, reboot_required: true }), // up to date but reboot pending
  ]);
  assert.deepEqual(counts, { total: 5, up_to_date: 2, pending: 2, security_pending: 1, reboot_required: 2, never_checked: 1 });
  // never_checked + up_to_date + pending == total (they partition).
  assert.equal(counts.never_checked + counts.up_to_date + counts.pending, counts.total);
});

test('bucketDevices: empty', () => {
  assert.deepEqual(bucketDevices([]), { total: 0, up_to_date: 0, pending: 0, security_pending: 0, reboot_required: 0, never_checked: 0 });
});

test('bucketDevices: a never-checked device with reboot_required set is not counted (parity with the server)', () => {
  // reboot_required and security_pending are flags on the checked set, so a device
  // that has never reported a patch scan contributes to neither even if its
  // reboot_required column is set. This mirrors the server bucketPatch helper
  // (internal/store/reports.go), which guards the reboot count on patches_checked_at.
  const counts = bucketDevices([
    dev({ patches_checked_at: null, reboot_required: true, security_patches: 2 }),
  ]);
  assert.deepEqual(counts, { total: 1, up_to_date: 0, pending: 0, security_pending: 0, reboot_required: 0, never_checked: 1 });
});

test('csvCell: quotes comma/quote/newline, doubles embedded quotes', () => {
  assert.equal(csvCell('plain'), 'plain');
  assert.equal(csvCell('a,b'), '"a,b"');
  assert.equal(csvCell('line1\nline2'), '"line1\nline2"');
  assert.equal(csvCell('say "hi"'), '"say ""hi"""');
  assert.equal(csvCell('has\rcr'), '"has\rcr"');
  assert.equal(csvCell(''), '');
  assert.equal(csvCell(null), '');
  assert.equal(csvCell(undefined), '');
});

test('csvCell: numbers and booleans are verbatim (never formula-guarded)', () => {
  assert.equal(csvCell(42), '42');
  assert.equal(csvCell(99.9), '99.9');
  assert.equal(csvCell(-5), '-5'); // a negative number is NOT turned into "'-5"
  assert.equal(csvCell(0), '0');
  assert.equal(csvCell(true), 'true');
  assert.equal(csvCell(false), 'false');
  assert.equal(csvCell(NaN), '');
  assert.equal(csvCell(Infinity), '');
});

test('csvCell: formula-injection guard on text cells', () => {
  assert.equal(csvCell('=1+2'), "'=1+2");
  assert.equal(csvCell('+1'), "'+1");
  assert.equal(csvCell('-cmd'), "'-cmd");
  assert.equal(csvCell('@SUM(A1)'), "'@SUM(A1)");
  assert.equal(csvCell('\t=1'), "'\t=1");
  // Guarded AND then quoted when it also contains a comma.
  assert.equal(csvCell('=HYPERLINK("x","y")'), '"\'=HYPERLINK(""x"",""y"")"');
  // A harmless leading char is untouched.
  assert.equal(csvCell('normal-text'), 'normal-text');
});

test('csvRow / csvDocument: CRLF lines, trailing CRLF', () => {
  assert.equal(csvRow(['a', 1, true]), 'a,1,true');
  assert.equal(csvDocument(['h1', 'h2'], [['a', 'b'], ['c', 'd']]), 'h1,h2\r\na,b\r\nc,d\r\n');
  assert.equal(csvDocument(['h'], []), 'h\r\n');
});

test('uptimeCsv: header + rows, OS uptime null -> empty cell', () => {
  const rows: UptimeRow[] = [
    { device_id: 1, hostname: 'ws-01', org_id: 1, org_name: 'Acme Dental', samples: 7190, expected: 7200, uptime_pct: 99.9, uptime_s: 123456 },
    { device_id: 2, hostname: 'ws,02', org_id: 1, org_name: 'Acme Dental', samples: 0, expected: 100, uptime_pct: 0, uptime_s: null },
  ];
  assert.equal(uptimeCsv(rows),
    'device_id,hostname,org_id,org_name,samples,expected,uptime_pct,os_uptime_s\r\n' +
    '1,ws-01,1,Acme Dental,7190,7200,99.9,123456\r\n' +
    '2,"ws,02",1,Acme Dental,0,100,0,\r\n');
});

test('diskCsv: at + disk_used_pct, null -> empty cell', () => {
  const pts: DiskPoint[] = [
    { at: '2026-11-14T00:00:00Z', disk_used_pct: 42.3 },
    { at: '2026-11-14T00:01:00Z', disk_used_pct: null },
  ];
  assert.equal(diskCsv(pts),
    'at,disk_used_pct\r\n2026-11-14T00:00:00Z,42.3\r\n2026-11-14T00:01:00Z,\r\n');
});

test('patchComplianceCsv: per-device rows, injection-safe hostname', () => {
  const rows: PatchComplianceRow[] = [
    { device_id: 1, hostname: 'ws-01', org_id: 1, org_name: 'Acme Dental', pending: 3, security: 1, reboot_required: false, checked_at: '2026-11-14T00:00:00Z' },
    // A hostile hostname that starts with '=' must be neutralised in the export.
    { device_id: 2, hostname: '=cmd|calc', org_id: 2, org_name: 'Zed Legal', pending: 0, security: 0, reboot_required: true, checked_at: null },
  ];
  assert.equal(patchComplianceCsv(rows),
    'device_id,hostname,org_id,org_name,pending,security,reboot_required,checked_at\r\n' +
    '1,ws-01,1,Acme Dental,3,1,false,2026-11-14T00:00:00Z\r\n' +
    "2,'=cmd|calc,2,Zed Legal,0,0,true,\r\n");
});
