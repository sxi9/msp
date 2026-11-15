import { useState } from 'preact/hooks';
import { MOCK, api } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { ago, pct, uptime as osUptime } from '../lib/format.ts';
import { diskCsv, patchComplianceCsv, uptimeCsv, type DiskPoint } from '../lib/reports.ts';
import type { User } from '../types.ts';

// The period is bounded by the server's retention (MSP_RETENTION_DAYS, default 7);
// a longer window is rejected server-side, so the picker never offers one.
const PERIODS: { days: number; label: string }[] = [
  { days: 1, label: 'Last 24 h' },
  { days: 3, label: 'Last 3 days' },
  { days: 7, label: 'Last 7 days' },
];

/**
 * The Reports page (RP1, admin/operator/viewer — reports are read-only): device
 * uptime, disk-usage trends and patch compliance over the data already collected,
 * each org-scoped (RS2) and exportable as CSV. A single Client + Period toolbar
 * drives all three sections (patch compliance ignores the period).
 */
export function Reports({ user: _user }: { user: User }) {
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0).data ?? [];
  const [orgId, setOrgId] = useState(0);
  const [days, setDays] = useState(7);
  return (
    <section>
      <div class="toolbar">
        <h1>Reports</h1>
        <label>Client
          <select value={orgId} onChange={(e) => setOrgId(Number(e.currentTarget.value))}>
            <option value={0}>All clients</option>
            {orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}
          </select>
        </label>
        <label>Period
          <select value={days} onChange={(e) => setDays(Number(e.currentTarget.value))}>
            {PERIODS.map((pd) => <option key={pd.days} value={pd.days}>{pd.label}</option>)}
          </select>
        </label>
      </div>
      <p class="muted">
        Read-only reports over the data already collected, scoped to the clients you can see. The period is
        bounded by the server's retention (default 7 days). Each report exports to CSV.
      </p>
      <UptimeSection orgId={orgId} days={days} />
      <DiskSection orgId={orgId} days={days} />
      <PatchSection orgId={orgId} />
    </section>
  );
}

// --- Uptime ------------------------------------------------------------------

function UptimeSection({ orgId, days }: { orgId: number; days: number }) {
  const rep = usePoll(() => api.reportUptime({ org_id: orgId || undefined, days }), [orgId, days], 0);
  const data = rep.data;
  return (
    <>
      <div class="toolbar section-head">
        <h2>Device uptime</h2>
        {data && data.rows.length > 0 && (
          <span class="muted">Org mean {data.mean_uptime_pct.toFixed(1)}% across {data.device_count} device{data.device_count === 1 ? '' : 's'}</span>
        )}
        <div class="right">
          {data && data.rows.length > 0 && (
            <CsvDownload name="uptime" url={api.reportUptimeCsvUrl({ org_id: orgId || undefined, days })} build={() => uptimeCsv(data.rows)} />
          )}
        </div>
      </div>
      <p class="muted">
        Uptime here means “did the device check in”: the share of the expected heartbeat windows seen over the
        period (a device enrolled mid-period counts only from enrollment). The OS uptime the device last reported
        is shown alongside.
      </p>
      {rep.error && <p class="error">{rep.error}</p>}
      {data && data.rows.length === 0 && <p class="empty">No devices in range.</p>}
      {data && data.rows.length > 0 && (
        <div class="log-scroll">
          <table class="grid">
            <thead><tr><th>Device</th><th>Client</th><th>Uptime</th><th>Samples / expected</th><th>OS uptime</th></tr></thead>
            <tbody>
              {data.rows.map((r) => (
                <tr key={r.device_id}>
                  <td data-label="Device"><a href={`#/devices/${r.device_id}`}>{r.hostname}</a></td>
                  <td data-label="Client" class="muted">{r.org_name}</td>
                  <td data-label="Uptime"><UptimeBar value={r.uptime_pct} /></td>
                  <td data-label="Samples / expected" class="muted">{r.samples} / {r.expected}</td>
                  <td data-label="OS uptime" class="muted">{osUptime(r.uptime_s)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

/** A check-in-coverage bar: high is good (accent), amber below 99%, red below 95%. */
function UptimeBar({ value }: { value: number }) {
  const v = Math.max(0, Math.min(100, value));
  const level = v >= 99 ? '' : v >= 95 ? ' bar-warn' : ' bar-crit';
  return (
    <span class="barwrap" title={`${v.toFixed(1)}%`}>
      <span class={`bar${level}`}><span class="bar-fill" style={`width:${v}%`} /></span>
      <span class="bar-label">{v.toFixed(1)}%</span>
    </span>
  );
}

// --- Disk trends -------------------------------------------------------------

function DiskSection({ orgId, days }: { orgId: number; days: number }) {
  const top = usePoll(() => api.reportDiskTop({ org_id: orgId || undefined, days, limit: 20 }), [orgId, days], 0);
  const [picked, setPicked] = useState(0);
  const rows = top.data?.rows ?? [];
  // Default the picker to the device with the highest latest usage; keep the user's
  // choice while it is still in range, otherwise fall back to the top device.
  const selected = picked && rows.some((r) => r.device_id === picked) ? picked : (rows[0]?.device_id ?? 0);
  const rep = usePoll(() => (selected ? api.reportDisk(selected, days) : Promise.resolve(null)), [selected, days], 0);
  const data = rep.data;
  return (
    <>
      <div class="toolbar section-head">
        <h2>Disk-usage trends</h2>
        {rows.length > 0 && (
          <label>Device
            <select value={selected} onChange={(e) => setPicked(Number(e.currentTarget.value))}>
              {rows.map((r) => <option key={r.device_id} value={r.device_id}>{r.hostname} — {pct(r.latest)}</option>)}
            </select>
          </label>
        )}
        <div class="right">
          {data && data.series.length > 0 && (
            <CsvDownload name={`disk-${data.hostname}`} url={api.reportDiskCsvUrl(selected, days)} build={() => diskCsv(data.series)} />
          )}
        </div>
      </div>
      {(top.error || rep.error) && <p class="error">{top.error || rep.error}</p>}
      {top.data && rows.length === 0 && <p class="empty">No devices have reported disk usage in range.</p>}
      {data && (
        data.series.length === 0 ? (
          <p class="empty">No disk samples for {data.hostname} in range.</p>
        ) : (
          <>
            <div class="report-stats">
              <StatTile label="Latest" value={pct(data.latest)} tone={tone(data.latest)} />
              <StatTile label="Minimum" value={pct(data.min)} />
              <StatTile label="Maximum" value={pct(data.max)} tone={tone(data.max)} />
            </div>
            <DiskChart series={data.series} />
          </>
        )
      )}
    </>
  );
}

/** Disk-usage tone: amber at 80%, red at 90% (matching the fleet Bar thresholds). */
function tone(v: number | null): 'warn' | 'bad' | undefined {
  if (v === null) return undefined;
  return v >= 90 ? 'bad' : v >= 80 ? 'warn' : undefined;
}

/**
 * A hand-written SVG line/area chart of disk_used_pct over the period: a 0–100%
 * grid with labelled axes, theme-aware via CSS variables (following the device
 * sparkline style). Nulls break the line, and the area is drawn per contiguous run
 * so a gap is not filled across. Marks are clamped within the plot bounds.
 */
function DiskChart({ series }: { series: DiskPoint[] }) {
  const W = 680, H = 240, ml = 40, mr = 14, mt = 12, mb = 28;
  const pw = W - ml - mr, ph = H - mt - mb, baseline = mt + ph;
  const n = series.length;
  const x = (i: number) => ml + (n <= 1 ? pw / 2 : (i / (n - 1)) * pw);
  const y = (v: number) => mt + (1 - Math.max(0, Math.min(100, v)) / 100) * ph;

  const lineSegs: string[] = [];
  const areaSegs: string[] = [];
  let run: { i: number; v: number }[] = [];
  const flush = () => {
    if (run.length === 0) return;
    lineSegs.push(run.map((p, k) => `${k ? 'L' : 'M'}${x(p.i).toFixed(1)},${y(p.v).toFixed(1)}`).join(' '));
    areaSegs.push(
      `M${x(run[0].i).toFixed(1)},${baseline.toFixed(1)} ` +
      run.map((p) => `L${x(p.i).toFixed(1)},${y(p.v).toFixed(1)}`).join(' ') +
      ` L${x(run[run.length - 1].i).toFixed(1)},${baseline.toFixed(1)} Z`,
    );
    run = [];
  };
  series.forEach((p, i) => {
    if (p.disk_used_pct === null || p.disk_used_pct === undefined || Number.isNaN(p.disk_used_pct)) flush();
    else run.push({ i, v: p.disk_used_pct });
  });
  flush();

  const firstAt = series.find((p) => p.disk_used_pct !== null)?.at ?? series[0]?.at;
  const lastAt = [...series].reverse().find((p) => p.disk_used_pct !== null)?.at ?? series[n - 1]?.at;
  return (
    <div class="chart">
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="xMidYMid meet" role="img" aria-label="Disk usage over the selected period">
        {[0, 25, 50, 75, 100].map((g) => (
          <g key={g}>
            <line x1={ml} y1={y(g)} x2={W - mr} y2={y(g)} class="chart-grid" />
            <text x={ml - 6} y={y(g) + 3} class="chart-axis chart-axis-y">{g}%</text>
          </g>
        ))}
        {areaSegs.map((d, i) => <path key={`a${i}`} d={d} class="chart-area" />)}
        {lineSegs.map((d, i) => <path key={`l${i}`} d={d} class="chart-line" />)}
        <line x1={ml} y1={baseline} x2={W - mr} y2={baseline} class="chart-axis-line" />
        <text x={ml} y={H - 8} class="chart-axis">{axisTime(firstAt)}</text>
        <text x={W - mr} y={H - 8} class="chart-axis chart-axis-x-end">{axisTime(lastAt)}</text>
      </svg>
    </div>
  );
}

/** Compact axis timestamp "MM-DD HH:MM" in local time; "" for a bad/missing stamp. */
function axisTime(s: string | undefined): string {
  if (!s) return '';
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return '';
  const p = (x: number) => String(x).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

// --- Patch compliance --------------------------------------------------------

function PatchSection({ orgId }: { orgId: number }) {
  const rep = usePoll(() => api.reportPatchCompliance(orgId || undefined), [orgId], 0);
  const data = rep.data;
  return (
    <>
      <div class="toolbar section-head">
        <h2>Patch compliance</h2>
        <div class="right">
          {data && data.devices.length > 0 && (
            <CsvDownload name="patch-compliance" url={api.reportPatchComplianceCsvUrl(orgId || undefined)} build={() => patchComplianceCsv(data.devices)} />
          )}
        </div>
      </div>
      <p class="muted">
        Device counts from the last patch report each device sent: “up to date” means checked with nothing pending.
        Security-pending and reboot-required are counted among the pending ones.
      </p>
      {rep.error && <p class="error">{rep.error}</p>}
      {data && data.devices.length === 0 && <p class="empty">No devices in range.</p>}
      {data && data.devices.length > 0 && (
        <>
          <div class="report-tiles">
            <StatTile label="Devices" value={String(data.overall.total)} />
            <StatTile label="Up to date" value={String(data.overall.up_to_date)} tone="ok" />
            <StatTile label="Updates pending" value={String(data.overall.pending)} tone={data.overall.pending ? 'warn' : undefined} />
            <StatTile label="Security pending" value={String(data.overall.security_pending)} tone={data.overall.security_pending ? 'bad' : undefined} />
            <StatTile label="Reboot required" value={String(data.overall.reboot_required)} tone={data.overall.reboot_required ? 'warn' : undefined} />
            <StatTile label="Never checked" value={String(data.overall.never_checked)} tone={data.overall.never_checked ? 'muted' : undefined} />
          </div>
          {data.orgs.length > 1 && (
            <div class="log-scroll">
              <table class="grid">
                <thead><tr><th>Client</th><th>Devices</th><th>Up to date</th><th>Pending</th><th>Security</th><th>Reboot</th><th>Never checked</th></tr></thead>
                <tbody>
                  {data.orgs.map((o) => (
                    <tr key={o.org_id}>
                      <td data-label="Client">{o.org_name}</td>
                      <td data-label="Devices">{o.counts.total}</td>
                      <td data-label="Up to date">{o.counts.up_to_date}</td>
                      <td data-label="Pending">{o.counts.pending}</td>
                      <td data-label="Security">{o.counts.security_pending}</td>
                      <td data-label="Reboot">{o.counts.reboot_required}</td>
                      <td data-label="Never checked">{o.counts.never_checked}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <h3 class="report-subhead">Per device</h3>
          <div class="log-scroll">
            <table class="grid">
              <thead><tr><th>Device</th><th>Client</th><th>Pending</th><th>Security</th><th>Reboot</th><th>Checked</th></tr></thead>
              <tbody>
                {data.devices.map((r) => (
                  <tr key={r.device_id}>
                    <td data-label="Device"><a href={`#/devices/${r.device_id}`}>{r.hostname}</a></td>
                    <td data-label="Client" class="muted">{r.org_name}</td>
                    <td data-label="Pending">{r.pending}</td>
                    <td data-label="Security">{r.security > 0 ? <span class="badge sev-high">{r.security}</span> : '0'}</td>
                    <td data-label="Reboot">{r.reboot_required ? <span class="badge sev-medium">reboot</span> : '—'}</td>
                    <td data-label="Checked" class="muted" title={r.checked_at ?? ''}>{r.checked_at ? ago(r.checked_at) : 'never'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </>
  );
}

// --- shared ------------------------------------------------------------------

function StatTile({ label, value, tone }: { label: string; value: string; tone?: 'ok' | 'warn' | 'bad' | 'muted' }) {
  return (
    <div class="stat-tile">
      <div class={`stat-value${tone ? ` ${tone}` : ''}`}>{value}</div>
      <div class="stat-label">{label}</div>
    </div>
  );
}

/**
 * A Download CSV control. In a real browser it is an <a download> pointing at the
 * server's .csv endpoint, so the server names and streams the file. Under ?mock=1
 * there is no server, so it builds the CSV client-side (lib/reports.ts) from the
 * already-loaded report and downloads a blob instead.
 */
function CsvDownload({ name, url, build }: { name: string; url: string; build: () => string }) {
  const filename = `msp-${name}-${new Date().toISOString().slice(0, 10)}.csv`;
  if (MOCK) {
    return <button type="button" class="btn btn-sm" onClick={() => downloadText(filename, build())}>Download CSV</button>;
  }
  return <a class="btn btn-sm" href={url} download={filename}>Download CSV</a>;
}

function downloadText(filename: string, text: string): void {
  const blob = new Blob([text], { type: 'text/csv;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
