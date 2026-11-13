import { useEffect, useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { navigate } from '../router.ts';
import { ago, duration, gb, pct, uptime, when } from '../lib/format.ts';
import { argvPreview, parseArgv } from '../lib/argv.ts';
import { COMMAND_DONE, canOperate, isAdmin, type Command, type Device as DeviceT, type NewCommand, type User } from '../types.ts';
import { StatusDot } from '../components/StatusDot.tsx';
import { Sparkline } from '../components/Sparkline.tsx';
import { CopyButton } from '../components/CopyBlock.tsx';
import { ConfirmButton } from '../components/Confirm.tsx';
import { Battery, PLATFORM_SHORT } from '../components/DeviceTable.tsx';

interface Preset { label: string; cmd: Omit<NewCommand, 'timeout_s'> }
const PRESETS: Record<'linux' | 'windows', Preset[]> = {
  linux: [
    { label: 'Uptime', cmd: { kind: 'exec', argv: ['uptime'] } },
    { label: 'Disk usage', cmd: { kind: 'script', shell: 'sh', body: 'df -h\n' } },
    { label: 'Top processes', cmd: { kind: 'script', shell: 'sh', body: 'ps aux --sort=-%cpu | head -20\n' } },
    { label: 'Reboot', cmd: { kind: 'exec', argv: ['systemctl', 'reboot'] } },
  ],
  windows: [
    { label: 'System info', cmd: { kind: 'script', shell: 'powershell', body: 'Get-ComputerInfo | Select-Object OsName,OsUptime\n' } },
    { label: 'Drives', cmd: { kind: 'script', shell: 'powershell', body: 'Get-PSDrive -PSProvider FileSystem\n' } },
    { label: 'Top processes', cmd: { kind: 'script', shell: 'powershell', body: 'Get-Process | Sort-Object CPU -Descending | Select-Object -First 20\n' } },
    { label: 'Reboot', cmd: { kind: 'exec', argv: ['shutdown', '/r', '/t', '0'] } },
  ],
};
const SHELLS: Record<string, string[]> = { linux: ['sh', 'bash'], windows: ['powershell', 'cmd'] };

/** Readable labels for the protocol's facts keys; unknown keys show as-is. */
const FACT_LABELS: Record<string, string> = {
  kernel: 'Kernel', cpu_model: 'CPU', cpu_count: 'CPUs', mem_total_mb: 'Memory',
  manufacturer: 'Manufacturer', model: 'Model', serial: 'Serial', ips: 'IPs',
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
    default: return '';
  }
}

const KIND_CHIP: Record<string, string> = { exec: 'chip', script: 'chip', lock: 'chip chip-lock', ping: 'chip', message: 'chip' };
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
            {Object.entries(facts).map(([k, v]) => [<dt key={`k${k}`} title={k}>{FACT_LABELS[k] ?? k}</dt>, <dd key={`v${k}`}>{factText(k, v)}</dd>])}
            {Object.keys(facts).length === 0 && <dd class="muted">No facts reported yet.</dd>}
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

      {builtins && <BuiltIns device={d} onQueue={queue} />}
      {operator && d.can_run_commands && <Console device={d} onQueue={queue} />}
      {error && <p class="error">{error}</p>}
      {tracked && <CommandResult command={tracked} />}

      <h2>Command history</h2>
      {history.error && <p class="error">{history.error}</p>}
      <History commands={history.data ?? []} />
    </section>
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
  const usePreset = (i: number) => {
    const p = PRESETS[platform][i];
    if (!p) return;
    setMode(p.cmd.kind as 'exec' | 'script');
    if (p.cmd.kind === 'exec') setText((p.cmd.argv ?? []).join('\n'));
    else { setText(p.cmd.body ?? ''); setShell(p.cmd.shell ?? shells[0]); }
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
        <select value={-1} onChange={(e) => { usePreset(Number(e.currentTarget.value)); e.currentTarget.value = '-1'; }}>
          <option value={-1}>Presets…</option>
          {PRESETS[platform].map((p, i) => <option key={p.label} value={i}>{p.label}</option>)}
        </select>
        <label>Timeout s<input type="number" min={1} max={3600} placeholder="60" value={timeoutText} onInput={(e) => setTimeoutText(e.currentTarget.value)} onBlur={() => setTimeoutText(String(timeout))} /></label>
      </div>
      <textarea rows={mode === 'exec' ? 4 : 8} spellcheck={false} placeholder={mode === 'exec' ? 'One argument per line, e.g.\nuname\n-a' : `Script body for ${shell}`} value={text} onInput={(e) => setText(e.currentTarget.value)} />
      {mode === 'exec' && <p class="muted">argv: <code>{argv.length ? argvPreview(argv) : '(empty)'}</code> — no shell is involved; metacharacters are literal.</p>}
      <button type="submit" class="btn btn-primary" disabled={busy || !valid}>Run on {device.hostname}</button>
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
