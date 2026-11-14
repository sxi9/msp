import { useEffect, useState } from 'preact/hooks';
import { ApiError, api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { navigate } from '../router.ts';
import { ago, duration, gb, pct, uptime, when } from '../lib/format.ts';
import { argvPreview, parseArgv } from '../lib/argv.ts';
import { loadPresets, presetFill, presetsFor } from '../lib/presets.ts';
import { notReportedHint, remoteDesktopError, technicianHint } from '../lib/rustdesk.ts';
import { appsOf, appsSummary, filterApps, type InstalledApp } from '../lib/apps.ts';
import { patchReportOf, rebootBadge, sortUpdates, summarisePatches } from '../lib/patches.ts';
import { destError, fileSize, filesForDevice, validPackageName } from '../lib/deploy.ts';
import { COMMAND_DONE, canOperate, isAdmin, type Command, type Device as DeviceT, type NewCommand, type NewDeploy, type NewPatch, type Preset, type RemoteDesktopOpened, type RustdeskInfo, type User } from '../types.ts';
import { RemoteShell, ShellResult } from './Shell.tsx';
import { StatusDot } from '../components/StatusDot.tsx';
import { Sparkline } from '../components/Sparkline.tsx';
import { CopyButton } from '../components/CopyBlock.tsx';
import { ConfirmButton } from '../components/Confirm.tsx';
import { Battery, PLATFORM_SHORT } from '../components/DeviceTable.tsx';

const SHELLS: Record<string, string[]> = { linux: ['sh', 'bash'], windows: ['powershell', 'cmd'] };

/** Readable labels for the protocol's facts keys; unknown keys show as-is. */
const FACT_LABELS: Record<string, string> = {
  kernel: 'Kernel', cpu_model: 'CPU', cpu_count: 'CPUs', mem_total_mb: 'Memory',
  manufacturer: 'Manufacturer', model: 'Model', serial: 'Serial', ips: 'IPs', rustdesk_id: 'RustDesk id',
};

/** One facts value as text: arrays joined, empty strings and nulls as an em dash, memory in MB. */
export function factText(key: string, v: unknown): string {
  if (v === null || v === undefined || v === '') return '—';
  if (Array.isArray(v)) return v.length ? v.join(', ') : '—';
  if (key === 'mem_total_mb' && typeof v === 'number') return `${v} MB`;
  return String(v);
}

/** Timeout field text -> seconds for the request: empty or 0 means the default 60, else clamped 1..3600. */
export function parseTimeout(text: string): number {
  const n = Math.floor(Number(text));
  if (!Number.isFinite(n) || n <= 0) return 60;
  return Math.min(3600, n);
}

/** One-line description of a command for the history table. */
export function describe(c: Command): string {
  switch (c.kind) {
    case 'exec': return argvPreview(c.argv);
    case 'script': return `${c.shell}: ${c.body.split('\n')[0].slice(0, 80)}`;
    case 'message': return c.text;
    case 'shell': return c.shell ? `interactive shell (${c.shell})` : 'interactive shell';
    case 'patch': {
      if (c.action === 'scan') return 'patch: scan';
      const mode = c.mode ?? 'security';
      return `patch: install ${mode}${mode === 'ids' && c.ids?.length ? ` (${c.ids.join(', ')})` : ''}`;
    }
    case 'deploy':
      if (c.source === 'package') return `deploy: install ${c.package ?? ''}`;
      return `deploy: ${c.dest ?? 'file'}${c.run ? ' (run)' : ''}`;
    default: return '';
  }
}

const KIND_CHIP: Record<string, string> = { exec: 'chip', script: 'chip', lock: 'chip chip-lock', ping: 'chip', message: 'chip', patch: 'chip' };
export const KindChip = ({ kind }: { kind: string }) => <span class={KIND_CHIP[kind] ?? 'chip'}>{kind}</span>;
export const StatusBadge = ({ status }: { status: string }) => <span class={`badge st-${status}`}>{status}</span>;

export function Device({ id, user }: { id: number; user: User }) {
  const detail = usePoll(() => api.getDevice(id), [id], 5000);
  const hb = usePoll(() => api.heartbeats(id, 120).then((r) => r.heartbeats), [id], 60000);
  const history = usePoll(() => api.listCommands(id, 50).then((r) => r.commands), [id], 5000);
  const [tracked, setTracked] = useState<Command | null>(null);
  const [error, setError] = useState('');

  const queue = async (c: NewCommand) => {
    setError('');
    try {
      setTracked((await api.createCommand(id, c)).command);
      history.reload();
    } catch (e) {
      const t = errorText(e);
      setError(t.startsWith('unsupported_on_platform') ? 'This command kind is not supported on this platform.' : t.startsWith('unsupported_on_device') ? 'This device is revoked; re-enroll it first.' : t);
    }
  };
  const queuePatch = async (p: NewPatch) => {
    setError('');
    try {
      setTracked((await api.queuePatch(id, p)).command);
      history.reload();
      detail.reload();
    } catch (e) {
      const t = errorText(e);
      setError(t.startsWith('device_offline') ? 'The device is offline; it must be online to patch.'
        : t.startsWith('unsupported_on_platform') ? 'Patching is not supported on this platform.'
        : t.startsWith('unsupported_on_device') ? 'This device is revoked; re-enroll it first.' : t);
    }
  };
  const queueDeploy = async (dreq: NewDeploy) => {
    setError('');
    try {
      setTracked((await api.deploy(id, dreq)).command);
      history.reload();
    } catch (e) {
      const t = errorText(e);
      setError(t.startsWith('device_offline') ? 'The device is offline; it must be online to deploy.'
        : t.startsWith('unsupported_on_platform') ? 'Software deployment is not supported on this platform.'
        : t.startsWith('unsupported_on_device') ? 'This device is revoked; re-enroll it first.'
        : t.startsWith('not_found') ? 'That file is not available for this device.' : t);
    }
  };
  // 1 s polling of the command just queued until it reaches a final status.
  useEffect(() => {
    if (!tracked || COMMAND_DONE.has(tracked.status)) return;
    const t = setInterval(async () => {
      try {
        const c = (await api.getCommand(tracked.id)).command;
        setTracked(c);
        if (COMMAND_DONE.has(c.status)) history.reload();
      } catch (e) { setError(errorText(e)); }
    }, 1000);
    return () => clearInterval(t);
  }, [tracked?.id, tracked?.status]);

  if (detail.error) return <p class="error">{detail.error}</p>;
  if (!detail.data) return <p class="muted">Loading…</p>;
  const d = detail.data.device;
  const facts = detail.data.facts ?? {};
  // The installed-apps inventory has its own panel; every other key is a facts row.
  const apps = appsOf(facts);
  const factRows = Object.entries(facts).filter(([k]) => k !== 'apps');
  const metrics = detail.data.metrics ?? {};
  const disks = (metrics.disks as { mount: string; total_gb: number; used_gb: number }[] | undefined) ?? [];
  const operator = canOperate(user);
  const builtins = operator && !d.revoked && d.status !== 'pending';
  const samples = hb.data ?? [];
  const lastBattery = samples.some((s) => s.battery_pct !== null) || d.battery_pct !== null;

  return (
    <section class="device">
      <div class="toolbar">
        <h1><StatusDot status={d.status} alerting={d.open_alerts > 0} /> {d.hostname} <span class="chip">{PLATFORM_SHORT[d.platform]}</span>{d.revoked && <span class="chip">revoked</span>}</h1>
        <a href={`#/orgs/${d.org_id}`}>{d.org_name}</a>
        {isAdmin(user) && (
          <span class="right">
            {!d.revoked && <ConfirmButton label="Revoke token" question="Revoke this device's token? The agent must be re-enrolled." danger onConfirm={async () => { await api.revokeDevice(id); detail.reload(); }} />}
            <ConfirmButton label="Delete device" question="Delete this device and all of its history?" danger onConfirm={async () => { await api.deleteDevice(id); navigate('#/'); }} />
          </span>
        )}
      </div>
      <dl class="kv header">
        <dt>Status</dt><dd>{d.status}, seen {ago(d.last_seen_at)}</dd>
        <dt>IP</dt><dd>{d.last_ip || '—'}</dd>
        <dt>OS</dt><dd>{d.os_version || '—'} ({d.arch})</dd>
        <dt>Agent</dt><dd>{d.agent_version || '—'}, every {d.interval_s} s</dd>
        <dt>Uptime</dt><dd>{uptime(d.uptime_s)}</dd>
        <dt>Enrolled</dt><dd>{when(d.enrolled_at)}</dd>
        <dt>Battery</dt><dd><Battery pct={d.battery_pct} charging={d.battery_charging} /></dd>
        <dt>Open alerts</dt><dd>{detail.data.alerts.length === 0 ? '0' : detail.data.alerts.map((a) => <span key={a.id} class={`badge sev-${a.severity}`}>{a.type}</span>)}</dd>
      </dl>

      <div class="sparks">
        <Sparkline label="CPU" values={samples.map((s) => s.cpu_pct)} last={pct(d.cpu_pct)} />
        <Sparkline label="Memory" values={samples.map((s) => s.mem_used_pct)} last={pct(d.mem_used_pct)} />
        <Sparkline label="Disk" values={samples.map((s) => s.disk_used_pct)} last={pct(d.disk_used_pct)} />
        {lastBattery && <Sparkline label="Battery" values={samples.map((s) => s.battery_pct)} last={pct(d.battery_pct)} />}
      </div>

      <div class="cols">
        <div>
          <h2>Facts</h2>
          <dl class="kv">
            {factRows.map(([k, v]) => [<dt key={`k${k}`} title={k}>{FACT_LABELS[k] ?? k}</dt>, <dd key={`v${k}`}>{factText(k, v)}</dd>])}
            {factRows.length === 0 && <dd class="muted">No facts reported yet.</dd>}
          </dl>
        </div>
        <div>
          <h2>Disks</h2>
          {disks.length === 0 ? <p class="muted">No disk sample yet.</p> : (
            <table class="grid"><thead><tr><th>Mount</th><th>Used</th><th>Total</th><th>%</th></tr></thead>
              <tbody>{disks.map((x) => <tr key={x.mount}><td data-label="Mount">{x.mount}</td><td data-label="Used">{gb(x.used_gb)}</td><td data-label="Total">{gb(x.total_gb)}</td><td data-label="%">{pct(x.total_gb ? (100 * x.used_gb) / x.total_gb : null)}</td></tr>)}</tbody>
            </table>
          )}
          {typeof metrics.mem_total_mb === 'number' && <p class="muted">Memory {Math.round(Number(metrics.mem_used_mb ?? 0))} / {Math.round(metrics.mem_total_mb)} MB</p>}
        </div>
      </div>

      {apps && <InstalledApps apps={apps} />}
      {d.platform !== 'android' && <Updates device={d} patches={detail.data.patches} canPatch={operator && !d.revoked && d.status === 'online'} onPatch={queuePatch} />}
      {builtins && <BuiltIns device={d} onQueue={queue} />}
      <RemoteShell device={d} user={user} />
      {!d.revoked && <RemoteDesktop device={d} operator={operator} />}
      {operator && d.can_run_commands && <Deploy device={d} onDeploy={queueDeploy} />}
      {operator && d.can_run_commands && <Console device={d} onQueue={queue} />}
      {error && <p class="error">{error}</p>}
      {tracked && <CommandResult command={tracked} />}

      <h2>Command history</h2>
      {history.error && <p class="error">{history.error}</p>}
      <History commands={history.data ?? []} />
    </section>
  );
}

/**
 * The installed-apps inventory an Android agent reports in facts.apps (docs/PROTOCOL.md):
 * count, a label/package search, a system-apps toggle (off by default: a phone lists a few
 * hundred of them) and a table inside its own scroll box so a long list or a long package
 * name never widens the page.
 */
function InstalledApps({ apps }: { apps: InstalledApp[] }) {
  const [query, setQuery] = useState('');
  const [showSystem, setShowSystem] = useState(false);
  const rows = filterApps(apps, query, showSystem);
  return (
    <div class="card apps">
      <div class="form-row">
        <h2>Installed apps <span class="muted">{appsSummary(apps)}</span></h2>
        <input type="search" placeholder="Search label or package" value={query} onInput={(e) => setQuery(e.currentTarget.value)} />
        <label><input type="checkbox" checked={showSystem} onChange={(e) => setShowSystem(e.currentTarget.checked)} /> Show system apps</label>
      </div>
      {apps.length === 0 ? <p class="muted">The agent reported no apps.</p> : rows.length === 0 ? <p class="muted">No app matches{showSystem ? '' : ' (system apps are hidden)'}.</p> : (
        <div class="apps-scroll">
          <table class="grid">
            <thead><tr><th>Label</th><th>Package</th><th>Version</th></tr></thead>
            <tbody>
              {rows.map((a) => (
                <tr key={a.package} class={a.system ? 'system' : ''}>
                  <td data-label="Label">{a.label}{a.system && <span class="chip">system</span>}</td>
                  <td data-label="Package"><code>{a.package}</code></td>
                  <td data-label="Version">{a.version || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p class="muted">{rows.length} of {apps.length} shown. The agent sends the inventory with its facts: on the first sync after start and every 120th sync, non-system apps first, at most 1000 entries.</p>
    </div>
  );
}

/**
 * Pending OS updates (PM1): last checked, a reboot-required badge, the counts
 * (pending, of which security), the update list (name, current -> new, a
 * "security" chip) and, for an operator on an online, non-revoked device, the
 * Install security / Install all / Rescan buttons (confirmed in-page). The queued
 * command's result shows in the command history/output below. The list scrolls in
 * its own box so a long list never widens the page.
 */
function Updates({ device, patches, canPatch, onPatch }: { device: DeviceT; patches?: Record<string, unknown>; canPatch: boolean; onPatch: (p: NewPatch) => Promise<void> }) {
  const report = patchReportOf(patches ?? {});
  const summary = report ? summarisePatches(report) : null;
  const updates = report ? sortUpdates(report.updates) : [];
  const badge = report ? rebootBadge(report) : (device.reboot_required ? 'Reboot required' : null);
  // Fall back to the denormalised DEVICE counts when the full report is absent
  // (e.g. the detail has only the columns, not the list yet).
  const pending = summary ? summary.pending : device.pending_patches;
  const security = summary ? summary.security : device.security_patches;
  return (
    <div class="card updates">
      <div class="form-row">
        <h2>Updates{summary?.unmanaged ? <span class="muted"> no package manager</span> : pending > 0 ? <span class="muted"> {pending} pending{security > 0 ? <>, <span class="warn">{security} security</span></> : ''}</span> : summary?.clean ? <span class="ok"> up to date</span> : ''}</h2>
        {badge && <span class="badge badge-amber">{badge}</span>}
        {canPatch && (
          <span class="right">
            <ConfirmButton label="Install security" question={`Install ${security} security update${security === 1 ? '' : 's'} on ${device.hostname}?`} disabled={security === 0} onConfirm={() => onPatch({ action: 'install', mode: 'security', reboot: 'never' })} />
            <ConfirmButton label="Install all" question={`Install all ${pending} pending update${pending === 1 ? '' : 's'} on ${device.hostname}? This may include non-security changes.`} danger disabled={pending === 0} onConfirm={() => onPatch({ action: 'install', mode: 'all', reboot: 'never' })} />
            <button type="button" class="btn btn-sm" onClick={() => onPatch({ action: 'scan' })}>Rescan</button>
          </span>
        )}
      </div>
      <dl class="kv">
        <dt>Last checked</dt><dd>{report?.checked_at ? ago(report.checked_at) : device.patches_checked_at ? ago(device.patches_checked_at) : <span class="muted">never</span>}</dd>
        <dt>Pending</dt><dd>{pending} {security > 0 && <span class="chip chip-lock">{security} security</span>}</dd>
        {summary && summary.manager !== 'none' && <><dt>Source</dt><dd class="muted">{summary.manager === 'apt' ? 'apt' : summary.manager === 'wu' ? 'Windows Update' : summary.manager}</dd></>}
      </dl>
      {report?.error && <p class="warn">Last scan failed: {report.error}. Showing the last good list.</p>}
      {!canPatch && device.status !== 'online' && <p class="muted">The device must be online to scan or install.</p>}
      {summary?.unmanaged ? <p class="muted">No supported package manager was found on this host.</p>
        : updates.length === 0 ? <p class="muted">{summary ? 'No pending updates.' : 'No patch report yet.'} {canPatch && 'Use Rescan to collect one.'}</p> : (
          <div class="apps-scroll">
            <table class="grid">
              <thead><tr><th>Package</th><th>Installed</th><th></th><th>Candidate</th><th></th></tr></thead>
              <tbody>
                {updates.map((u) => (
                  <tr key={u.name} class={u.security ? 'security' : ''}>
                    <td data-label="Package"><code>{u.name}</code>{u.security && <span class="chip chip-lock">security</span>}</td>
                    <td data-label="Installed" class="muted">{u.current || '—'}</td>
                    <td data-label="" class="muted">→</td>
                    <td data-label="Candidate">{u.new || '—'}</td>
                    <td data-label="Source" class="muted">{u.source || ''}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      {updates.length > 0 && <p class="muted">Installing is a role-gated, tenant-scoped, audited command; the result (the install log) appears in the command history below.</p>}
    </div>
  );
}

function BuiltIns({ device, onQueue }: { device: DeviceT; onQueue: (c: NewCommand) => Promise<void> }) {
  const [text, setText] = useState('');
  const send = async (e: Event) => {
    e.preventDefault();
    if (!text.trim()) return;
    await onQueue({ kind: 'message', text: text.trim(), timeout_s: 30 });
    setText('');
  };
  return (
    <div class="card builtins">
      <h2>Actions</h2>
      <div class="form-row">
        <ConfirmButton label="Lock device" question={`Lock ${device.hostname} now?`} danger onConfirm={() => onQueue({ kind: 'lock', timeout_s: 30 })} />
        <button type="button" class="btn btn-sm" onClick={() => onQueue({ kind: 'ping', timeout_s: 30 })}>Ping</button>
        <form class="form-row" onSubmit={send}>
          <input type="text" placeholder="Message to the logged-in user" maxLength={4096} value={text} onInput={(e) => setText(e.currentTarget.value)} />
          <button type="submit" class="btn btn-sm" disabled={!text.trim()}>Send message</button>
        </form>
      </div>
    </div>
  );
}

/**
 * Remote desktop through the MSP's own RustDesk relay (RD1). The server says
 * whether a relay is configured (GET /api/v1/rustdesk, fetched once per card);
 * the device carries the RustDesk id its agent reported. Connect POSTs
 * /remote-desktop (audited server side) and then navigates to the returned
 * rustdesk://connect/<id> URL, which the technician's own RustDesk client
 * handles; the same URL is also shown as a real link, and the id can be copied
 * for a manual connection.
 */
function RemoteDesktop({ device, operator }: { device: DeviceT; operator: boolean }) {
  const info = usePoll<RustdeskInfo>(() => api.rustdesk(), [], 0);
  const [opened, setOpened] = useState<RemoteDesktopOpened | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => { setOpened(null); setError(''); }, [device.id]);
  const connect = async () => {
    setBusy(true);
    setError('');
    try {
      const r = await api.remoteDesktop(device.id);
      setOpened(r);
      // The browser hands rustdesk:// to the registered client; the page stays.
      window.location.href = r.url;
    } catch (e) {
      setError(e instanceof ApiError ? remoteDesktopError(e.code, e.detail) : errorText(e));
    } finally { setBusy(false); }
  };
  if (info.error) return <div class="card rustdesk"><h2>Remote desktop</h2><p class="error">{info.error}</p></div>;
  if (!info.data) return <div class="card rustdesk"><h2>Remote desktop</h2><p class="muted">Loading…</p></div>;
  if (!info.data.enabled) {
    return (
      <div class="card rustdesk">
        <h2>Remote desktop</h2>
        <p class="muted">Remote desktop is not configured on this server. Set <code>MSP_RUSTDESK_HOST</code> and <code>MSP_RUSTDESK_KEY</code> to the self-hosted RustDesk relay: see <a href="https://github.com/sxi9/msp/blob/main/docs/OPS.md#10-remote-desktop-rustdesk" target="_blank" rel="noreferrer">docs/OPS.md, Remote desktop (RustDesk)</a>.</p>
      </div>
    );
  }
  const id = device.rustdesk_id;
  return (
    <div class="card rustdesk">
      <h2>Remote desktop</h2>
      <dl class="kv">
        <dt>RustDesk id</dt>
        <dd>{id ? <><code>{id}</code> <CopyButton text={id} /></> : <span class="muted">{notReportedHint(device.platform)}</span>}</dd>
        <dt>Relay</dt><dd><code>{info.data.host}</code></dd>
      </dl>
      {operator && id && (
        <div class="form-row">
          <button type="button" class="btn btn-primary" disabled={busy} onClick={connect}>Connect</button>
          {opened && <a href={opened.url}>Open <code>{opened.url}</code> in RustDesk</a>}
        </div>
      )}
      {!operator && <p class="muted">Viewers can see the id; connecting needs the operator role.</p>}
      {error && <p class="error">{error}</p>}
      {opened && <p class="muted">If nothing opened, the RustDesk client on this PC is not installed or not registered for <code>rustdesk://</code> links: start it, enter id <code>{opened.rustdesk_id}</code> and the device's permanent password. The session is audited as <code>device.remote_desktop_opened</code>.</p>}
      <p class="muted">{technicianHint(info.data.host)}</p>
    </div>
  );
}

function Console({ device, onQueue }: { device: DeviceT; onQueue: (c: NewCommand) => Promise<void> }) {
  const platform = device.platform === 'windows' ? 'windows' : 'linux';
  const shells = SHELLS[platform];
  const [mode, setMode] = useState<'exec' | 'script'>('exec');
  const [text, setText] = useState('');
  const [shell, setShell] = useState(shells[0]);
  // The raw field text is kept so Backspace can empty it; it is parsed on submit.
  const [timeoutText, setTimeoutText] = useState('60');
  const timeout = parseTimeout(timeoutText);
  const [busy, setBusy] = useState(false);
  const argv = parseArgv(text);
  // The preset library comes from GET /api/v1/presets (builtin set or the MSP_PRESETS
  // file), fetched once per app session; null while loading, [] when the fetch failed.
  const [presets, setPresets] = useState<Preset[] | null>(null);
  useEffect(() => {
    let alive = true;
    loadPresets(api.presets).then((lib) => { if (alive) setPresets(presetsFor(lib, platform)); });
    return () => { alive = false; };
  }, [platform]);
  const usePreset = (i: number) => {
    const p = presets?.[i];
    if (!p) return;
    const f = presetFill(p);
    setMode(f.mode);
    setText(f.text);
    if (f.mode === 'script') setShell(shells.includes(f.shell) ? f.shell : shells[0]);
    setTimeoutText(f.timeoutText);
  };
  const run = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    try {
      await onQueue(mode === 'exec' ? { kind: 'exec', argv, timeout_s: timeout } : { kind: 'script', shell, body: text.endsWith('\n') ? text : `${text}\n`, timeout_s: timeout });
    } finally { setBusy(false); }
  };
  const valid = mode === 'exec' ? argv.length > 0 : text.trim().length > 0;
  return (
    <form class="card console" onSubmit={run}>
      <div class="form-row">
        <h2>Run</h2>
        <div class="tabs">
          <button type="button" class={mode === 'exec' ? 'active' : ''} onClick={() => setMode('exec')}>Command</button>
          <button type="button" class={mode === 'script' ? 'active' : ''} onClick={() => setMode('script')}>Script</button>
        </div>
        {mode === 'script' && (
          <select value={shell} onChange={(e) => setShell(e.currentTarget.value)}>{shells.map((s) => <option key={s} value={s}>{s}</option>)}</select>
        )}
        <select value={-1} disabled={!presets || presets.length === 0} title={presets === null ? 'Loading presets' : presets.length === 0 ? 'No presets for this platform' : 'Fill the console from the preset library'} onChange={(e) => { usePreset(Number(e.currentTarget.value)); e.currentTarget.value = '-1'; }}>
          <option value={-1}>Presets…</option>
          {(presets ?? []).map((p, i) => <option key={p.label} value={i}>{p.label}</option>)}
        </select>
        <label>Timeout s<input type="number" min={1} max={3600} placeholder="60" value={timeoutText} onInput={(e) => setTimeoutText(e.currentTarget.value)} onBlur={() => setTimeoutText(String(timeout))} /></label>
      </div>
      <textarea rows={mode === 'exec' ? 4 : 8} spellcheck={false} placeholder={mode === 'exec' ? 'One argument per line, e.g.\nuname\n-a' : `Script body for ${shell}`} value={text} onInput={(e) => setText(e.currentTarget.value)} />
      {mode === 'exec' && <p class="muted">argv: <code>{argv.length ? argvPreview(argv) : '(empty)'}</code> — no shell is involved; metacharacters are literal.</p>}
      <button type="submit" class="btn btn-primary" disabled={busy || !valid}>Run on {device.hostname}</button>
    </form>
  );
}

/**
 * Software deployment (SD1): push a library file to the device (optionally running
 * it) or install a named OS package. The file picker is the library filtered to
 * this device's org plus global files; the dest is validated client-side (absolute,
 * no ".."). The queued command's result (download + install log) shows below.
 */
function Deploy({ device, onDeploy }: { device: DeviceT; onDeploy: (d: NewDeploy) => Promise<void> }) {
  const lib = usePoll(() => api.listFiles().then((r) => r.files), [device.org_id], 0);
  const files = filesForDevice(lib.data ?? [], device.org_id);
  const [mode, setMode] = useState<'file' | 'package'>('file');
  const [fileId, setFileId] = useState(0);
  const [dest, setDest] = useState('');
  const [run, setRun] = useState(false);
  const [argsText, setArgsText] = useState('');
  const [pkg, setPkg] = useState('');
  const [busy, setBusy] = useState(false);

  const destMsg = dest.trim() ? destError(dest.trim()) : null;
  const runArgs = argsText.split('\n').map((s) => s.trim()).filter(Boolean);
  const pkgTrim = pkg.trim();
  const fileValid = fileId > 0 && dest.trim() !== '' && destMsg === null;
  const pkgValid = validPackageName(pkgTrim);
  const valid = mode === 'file' ? fileValid : pkgValid;

  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    try {
      if (mode === 'file') {
        await onDeploy({ source: 'file', file_id: fileId, dest: dest.trim(), run, run_args: run ? runArgs : [] });
      } else {
        await onDeploy({ source: 'package', package: pkgTrim });
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card deploy" onSubmit={submit}>
      <div class="form-row">
        <h2>Deploy software</h2>
        <div class="tabs">
          <button type="button" class={mode === 'file' ? 'active' : ''} onClick={() => setMode('file')}>Deploy a file</button>
          <button type="button" class={mode === 'package' ? 'active' : ''} onClick={() => setMode('package')}>Install a package</button>
        </div>
      </div>
      {mode === 'file' ? (
        <>
          {lib.error && <p class="error">{lib.error}</p>}
          {!lib.data ? <p class="muted">Loading the library…</p> : files.length === 0 ? (
            <p class="muted">No files for this client yet. Upload one on the <a href="#/software">Software</a> page.</p>
          ) : (
            <>
              <div class="form-row">
                <label>File
                  <select value={fileId} onChange={(e) => setFileId(Number(e.currentTarget.value))}>
                    <option value={0}>Choose a file…</option>
                    {files.map((f) => (
                      <option key={f.id} value={f.id}>{f.name} ({fileSize(f.size)}{f.org_id === null ? ', all clients' : ''})</option>
                    ))}
                  </select>
                </label>
                <input type="text" class="deploy-dest" placeholder="Destination path, e.g. /opt/acme/installer.sh" value={dest} onInput={(e) => setDest(e.currentTarget.value)} />
                <label><input type="checkbox" checked={run} onChange={(e) => setRun(e.currentTarget.checked)} /> Run after copying</label>
              </div>
              {run && <textarea rows={3} spellcheck={false} placeholder={'Run arguments, one per line (optional), e.g.\n--silent'} value={argsText} onInput={(e) => setArgsText(e.currentTarget.value)} />}
              {dest.trim() && destMsg && <p class="warn">{destMsg}</p>}
              <button type="submit" class="btn btn-primary" disabled={busy || !valid}>Deploy to {device.hostname}</button>
              <p class="muted">The agent downloads the file, verifies its SHA-256, writes it to the destination{run ? ' and runs it with the arguments above (argv, no shell)' : ''}. A hash mismatch fails without writing.</p>
            </>
          )}
        </>
      ) : (
        <>
          <div class="form-row">
            <input type="text" class="deploy-dest" placeholder="Package name, e.g. htop" value={pkg} onInput={(e) => setPkg(e.currentTarget.value)} />
            <button type="submit" class="btn btn-primary" disabled={busy || !valid}>Install on {device.hostname}</button>
          </div>
          {pkgTrim && !pkgValid && <p class="warn">A package name is 1–256 characters of letters, digits and <code>. _ + -</code></p>}
          <p class="muted">Installs the package through the host's package manager ({device.platform === 'windows' ? 'the Windows installer path' : 'apt'}); the install log appears in the command history below.</p>
        </>
      )}
    </form>
  );
}

function Output({ label, text }: { label: string; text: string }) {
  if (!text) return null;
  return <div class="copyblock"><div class="copyblock-label">{label}</div><pre class="out">{text}</pre><CopyButton text={text} /></div>;
}

function CommandResult({ command: c }: { command: Command }) {
  const running = !COMMAND_DONE.has(c.status);
  return (
    <div class="card result">
      <div class="form-row">
        <KindChip kind={c.kind} /> <code>{describe(c)}</code>
        <StatusBadge status={c.status} />
        {running && <span class="muted">waiting for the agent…</span>}
        {!running && <span class="muted">exit {c.exit_code ?? '—'}, took {duration(c.started_at, c.finished_at)}</span>}
      </div>
      {c.error && <p class="error">{c.error}</p>}
      {c.truncated && <p class="warn">Output was truncated at 64 KiB per stream.</p>}
      {c.kind === 'shell' && <ShellResult command={c} />}
      <Output label="stdout" text={c.stdout} />
      <Output label="stderr" text={c.stderr} />
    </div>
  );
}

function History({ commands }: { commands: Command[] }) {
  const [open, setOpen] = useState<number | null>(null);
  const [full, setFull] = useState<Command | null>(null);
  const toggle = async (c: Command) => {
    if (open === c.id) { setOpen(null); return; }
    setOpen(c.id);
    setFull(null);
    try { setFull((await api.getCommand(c.id)).command); } catch { setFull(c); }
  };
  if (commands.length === 0) return <p class="empty">No commands yet.</p>;
  return (
    <table class="grid history">
      <thead><tr><th>When</th><th>Kind</th><th>Command</th><th>By</th><th>Status</th><th>Exit</th><th>Took</th></tr></thead>
      <tbody>
        {commands.map((c) => [
          <tr key={c.id} class="clickable" onClick={() => toggle(c)}>
            <td data-label="When" title={c.created_at}>{ago(c.created_at)}</td>
            <td data-label="Kind"><KindChip kind={c.kind} /></td>
            <td data-label="Command"><code>{describe(c)}</code></td>
            <td data-label="By" class="muted">{c.created_by_email}</td>
            <td data-label="Status"><StatusBadge status={c.status} /></td>
            <td data-label="Exit">{c.exit_code ?? '—'}</td>
            <td data-label="Took">{duration(c.started_at, c.finished_at)}</td>
          </tr>,
          open === c.id && (
            <tr key={`${c.id}-out`} class="expanded"><td colSpan={7}>
              {!full ? <span class="muted">Loading…</span> : <CommandResult command={full} />}
            </td></tr>
          ),
        ])}
      </tbody>
    </table>
  );
}
