// In-memory stand-in for the server, active when location.search has mock=1
// (add &role=viewer|operator to try a weaker role). Lets the dashboard be
// developed and demoed without a backend. Nothing here ships to the server.
import type { Alert, AuditEntry, Command, Device, EnrollmentToken, NewCommand, NewShell, Org, Preset, User } from './types.ts';
import type { ShellSocket } from './lib/shell.ts';

interface Reply { status: number; body: unknown }

const iso = (msAgo: number) => new Date(Date.now() - msAgo).toISOString();
const role = (new URLSearchParams(location.search).get('role') ?? 'admin') as User['role'];
const me: User = { id: 1, email: 'admin@example.com', role, created_at: iso(86400e3 * 30), disabled_at: null, all_orgs: true, org_ids: [] };
const users: User[] = [
  me,
  // A scoped operator: sees only Acme Dental (org 1), so the Edit-access editor
  // opens with "All clients" off and that one org checked (RS2).
  { id: 2, email: 'tech@example.com', role: 'operator', created_at: iso(86400e3 * 9), disabled_at: null, all_orgs: false, org_ids: [1] },
  // An all-clients viewer, for contrast in the list.
  { id: 3, email: 'viewer@example.com', role: 'viewer', created_at: iso(86400e3 * 4), disabled_at: null, all_orgs: true, org_ids: [] },
];
const orgs: Org[] = [
  { id: 1, name: 'Acme Dental', created_at: iso(86400e3 * 20), device_count: 0, online_count: 0, open_alerts: 0 },
  { id: 2, name: 'Zed Legal', created_at: iso(86400e3 * 12), device_count: 0, online_count: 0, open_alerts: 0 },
];
const dev = (id: number, org_id: number, hostname: string, platform: Device['platform'], status: Device['status'], extra: Partial<Device> = {}): Device => ({
  id, org_id, org_name: orgs.find((o) => o.id === org_id)!.name, device_uid: `uid-${id}`, hostname, platform,
  arch: platform === 'android' ? 'arm64' : 'amd64',
  os_version: platform === 'linux' ? 'Ubuntu 24.04.1 LTS' : platform === 'windows' ? 'Windows 11 Pro 23H2' : 'Android 14 (34)',
  agent_version: '0.1.0', interval_s: platform === 'android' ? 60 : 30, status, enrolled_at: iso(86400e3 * 5),
  last_seen_at: status === 'pending' ? null : iso(status === 'online' ? 12e3 : 3600e3 * 6),
  last_ip: status === 'pending' ? '' : `10.0.${org_id}.${id}`, revoked: false,
  cpu_pct: status === 'pending' ? null : 12.5, mem_used_pct: status === 'pending' ? null : 50.8, disk_used_pct: status === 'pending' ? null : 42.3,
  battery_pct: null, battery_charging: null, uptime_s: status === 'pending' ? null : 86400 * 3 + 4000, open_alerts: 0,
  can_run_commands: platform !== 'android', rustdesk_id: null, ...extra,
});
// The mock relay; add &rustdesk=0 to the URL to see the "not configured" card.
const rustdesk = new URLSearchParams(location.search).get('rustdesk') === '0'
  ? { enabled: false, host: '', key: '' }
  : { enabled: true, host: 'rmm.example.com', key: 'OeVuKk5nlHiXp+7qsR6CbcHBGxhU4PJkNbvoX3P5X0k=' };
const devices: Device[] = [
  dev(1, 1, 'ws-reception-01', 'linux', 'online', { rustdesk_id: '123456789' }),
  dev(2, 1, 'ws-xray', 'windows', 'online', { disk_used_pct: 93.1, open_alerts: 1, rustdesk_id: '987654321' }),
  dev(3, 1, 'Pixel 8', 'android', 'online', { battery_pct: 12, battery_charging: false, open_alerts: 1 }),
  dev(4, 1, 'ws-backoffice', 'linux', 'offline', { open_alerts: 1 }),
  dev(5, 2, 'LAPTOP-7Q2', 'windows', 'online', { cpu_pct: 71 }),
  dev(6, 2, 'srv-files', 'linux', 'pending'),
];
const alerts: Alert[] = [
  { id: 1, org_id: 1, org_name: 'Acme Dental', device_id: 4, hostname: 'ws-backoffice', type: 'offline', severity: 'high', message: 'no sync for 6h', opened_at: iso(3600e3 * 5), resolved_at: null, acknowledged_by: null, acknowledged_at: null },
  { id: 2, org_id: 1, org_name: 'Acme Dental', device_id: 2, hostname: 'ws-xray', type: 'disk_full', severity: 'medium', message: 'disk 93% used', opened_at: iso(3600e3), resolved_at: null, acknowledged_by: null, acknowledged_at: null },
  { id: 3, org_id: 1, org_name: 'Acme Dental', device_id: 3, hostname: 'Pixel 8', type: 'battery_low', severity: 'low', message: 'battery 12% and not charging', opened_at: iso(600e3), resolved_at: null, acknowledged_by: null, acknowledged_at: null },
  { id: 4, org_id: 2, org_name: 'Zed Legal', device_id: 5, hostname: 'LAPTOP-7Q2', type: 'offline', severity: 'high', message: 'no sync for 2m', opened_at: iso(86400e3), resolved_at: iso(80000e3), acknowledged_by: 1, acknowledged_at: iso(85000e3) },
];
const tokens: EnrollmentToken[] = [
  { id: 1, org_id: 1, token_prefix: 'et_Ab12cD3', label: 'front desk', max_uses: 10, uses: 2, expires_at: iso(-86400e3 * 6), created_by: 1, created_at: iso(86400e3), revoked_at: null },
  { id: 2, org_id: 1, token_prefix: 'et_Zz98yX7', label: 'old', max_uses: 1, uses: 1, expires_at: iso(86400e3), created_by: 1, created_at: iso(86400e3 * 8), revoked_at: iso(86400e3 * 2) },
];
const commands: Command[] = [{
  id: 1, device_id: 1, org_id: 1, created_by: 1, created_by_email: me.email, kind: 'exec', argv: ['uname', '-a'], shell: '', body: '', text: '',
  timeout_s: 60, status: 'done', created_at: iso(900e3), sent_at: iso(899.8e3), started_at: iso(899.7e3), finished_at: iso(899.5e3),
  exit_code: 0, stdout: 'Linux ws-reception-01 6.8.0-45-generic #45-Ubuntu SMP x86_64 GNU/Linux\n', stderr: '', truncated: false, error: '',
}];
const audit: AuditEntry[] = [
  { id: 2, at: iso(899.5e3), actor_type: 'device', actor_id: 1, actor_label: 'device:1', org_id: 1, action: 'command.finished', target_type: 'command', target_id: 1, ip: '10.0.1.1', detail: { status: 'done', exit_code: 0 } },
  { id: 1, at: iso(900e3), actor_type: 'user', actor_id: 1, actor_label: me.email, org_id: 1, action: 'command.queued', target_type: 'command', target_id: 1, ip: '127.0.0.1', detail: { kind: 'exec', argv: ['uname', '-a'] } },
];
// The server's builtin preset library (internal/server/presets.go), as GET /api/v1/presets serves it.
const presets: { linux: Preset[]; windows: Preset[] } = {
  linux: [
    { label: 'Uptime', kind: 'exec', argv: ['uptime'], timeout_s: 60 },
    { label: 'Disk usage', kind: 'script', shell: 'sh', body: 'df -h\n', timeout_s: 60 },
    { label: 'Top processes', kind: 'script', shell: 'sh', body: 'ps aux --sort=-%cpu | head -20\n', timeout_s: 60 },
    { label: 'Reboot', kind: 'exec', argv: ['systemctl', 'reboot'], timeout_s: 60 },
  ],
  windows: [
    { label: 'System info', kind: 'script', shell: 'powershell', body: 'Get-ComputerInfo | Select-Object OsName,OsUptime\n', timeout_s: 60 },
    { label: 'Drives', kind: 'script', shell: 'powershell', body: 'Get-PSDrive -PSProvider FileSystem\n', timeout_s: 60 },
    { label: 'Top processes', kind: 'script', shell: 'powershell', body: 'Get-Process | Sort-Object CPU -Descending | Select-Object -First 20\n', timeout_s: 60 },
    { label: 'Reboot', kind: 'exec', argv: ['shutdown', '/r', '/t', '0'], timeout_s: 60 },
  ],
};
// An Android agent's facts carry the installed-apps inventory (facts.apps, non-system first,
// then by label), which the device page shows in its own panel.
const androidFacts = (d: Device) => ({
  kernel: '5.15.110-android14-11', cpu_model: 'shiba', cpu_count: 9, mem_total_mb: 7634, manufacturer: 'Google', model: d.hostname, serial: '', ips: [d.last_ip],
  apps: [
    { package: 'com.example.nolabel', label: 'com.example.nolabel', version: '', system: false },
    { package: 'org.mozilla.firefox', label: 'Firefox', version: '131.0.3', system: false },
    { package: 'com.google.android.apps.maps', label: 'Maps', version: '11.145.0101', system: false },
    { package: 'com.sxi9.msp.agent', label: 'MSP Agent', version: '0.1.0', system: false },
    { package: 'com.whatsapp', label: 'WhatsApp', version: '2.24.20.81', system: false },
    { package: 'com.android.chrome', label: 'Chrome', version: '129.0.6668.70', system: true },
    { package: 'com.google.android.gms', label: 'Google Play services', version: '24.38.33 (190400-676000000)', system: true },
    { package: 'com.android.settings', label: 'Settings', version: '14', system: true },
  ],
});
let nextId = 100;
const log = (action: string, target_type: string, target_id: number | null, org_id: number | null, detail: Record<string, unknown> = {}) =>
  audit.unshift({ id: nextId++, at: iso(0), actor_type: 'user', actor_id: me.id, actor_label: me.email, org_id, action, target_type, target_id, ip: '127.0.0.1', detail });

// Dedupe + sort the org_ids of an RS2 access payload, dropping non-integers.
const normIds = (raw: unknown): number[] =>
  Array.isArray(raw) ? [...new Set(raw.map(Number).filter((n) => Number.isInteger(n)))].sort((a, b) => a - b) : [];

const ok = (body: unknown, status = 200): Reply => ({ status, body });
const err = (status: number, error: string, detail = ''): Reply => ({ status, body: { error, detail } });
const needRole = (min: 'operator' | 'admin'): Reply | null =>
  (me.role === 'admin' || (min === 'operator' && me.role === 'operator')) ? null : err(403, 'forbidden');

function withCounts(o: Org): Org {
  const ds = devices.filter((d) => d.org_id === o.id);
  return { ...o, device_count: ds.length, online_count: ds.filter((d) => d.status === 'online').length, open_alerts: alerts.filter((a) => a.org_id === o.id && !a.resolved_at).length };
}

function finish(c: Command) {
  c.status = 'sent'; c.sent_at = iso(0);
  setTimeout(() => {
    c.started_at = iso(0);
    const out: Record<string, string> = { exec: `${c.argv.join(' ')}: simulated output\n`, script: 'simulated script output\n', ping: 'pong', lock: 'locked 1 session', message: 'shown' };
    if (c.kind === 'exec' && c.argv[0] === 'sleep') { c.status = 'timeout'; c.exit_code = -1; c.error = `timeout after ${c.timeout_s}s`; }
    else if (c.kind === 'exec' && c.argv[0] === 'false') { c.status = 'failed'; c.exit_code = 1; c.stderr = 'simulated failure\n'; }
    else { c.status = 'done'; c.exit_code = 0; c.stdout = out[c.kind]; }
    c.finished_at = iso(-700);
  }, 1500);
}

function route(method: string, path: string, body: unknown): Reply {
  const u = new URL(path, location.origin);
  const p = u.pathname;
  const q = u.searchParams;
  const m = (re: RegExp) => re.exec(p);
  const b = (body ?? {}) as Record<string, unknown>;
  let x: RegExpExecArray | null;
  if (p === '/api/v1/login') {
    if (typeof b.password !== 'string' || b.password.length < 4) return err(401, 'invalid_credentials');
    return ok({ user: me });
  }
  if (p === '/api/v1/logout') return ok(null, 204);
  if (p === '/api/v1/me') return ok({ user: me });
  if (p === '/api/v1/me/password') {
    const cur = String(b.current_password ?? ''), nw = String(b.new_password ?? '');
    if (cur.length < 4) return err(400, 'invalid_request', 'current password is incorrect');
    if (nw.length < 10) return err(400, 'invalid_request', 'password must be at least 10 characters');
    log('user.password_changed', 'user', me.id, null, { email: me.email });
    return ok(null, 204);
  }
  if (p === '/api/v1/summary') return ok({
    orgs: orgs.length, devices: devices.length, online: devices.filter((d) => d.status === 'online').length,
    offline: devices.filter((d) => d.status === 'offline').length, pending: devices.filter((d) => d.status === 'pending').length,
    open_alerts: alerts.filter((a) => !a.resolved_at).length,
  });
  if (p === '/api/v1/orgs' && method === 'GET') return ok({ orgs: orgs.map(withCounts) });
  if (p === '/api/v1/orgs' && method === 'POST') {
    const r = needRole('admin'); if (r) return r;
    const name = String(b.name ?? '').trim();
    if (!name) return err(400, 'invalid_request', 'name required');
    if (orgs.some((o) => o.name.toLowerCase() === name.toLowerCase())) return err(409, 'conflict');
    const org: Org = { id: nextId++, name, created_at: iso(0), device_count: 0, online_count: 0, open_alerts: 0 };
    orgs.push(org); log('org.created', 'org', org.id, org.id, { name });
    return ok({ org }, 201);
  }
  if ((x = m(/^\/api\/v1\/orgs\/(\d+)$/))) {
    const org = orgs.find((o) => o.id === Number(x![1]));
    return org ? ok({ org: withCounts(org), tokens: tokens.filter((t) => t.org_id === org.id) }) : err(404, 'not_found');
  }
  if ((x = m(/^\/api\/v1\/orgs\/(\d+)\/tokens$/))) {
    const r = needRole('operator'); if (r) return r;
    const org = orgs.find((o) => o.id === Number(x![1])); if (!org) return err(404, 'not_found');
    const max_uses = Number(b.max_uses ?? 10), ttl = Number(b.ttl_hours ?? 168);
    if (max_uses < 1 || max_uses > 1000 || ttl < 1 || ttl > 8760) return err(400, 'invalid_request', 'max_uses 1..1000, ttl_hours 1..8760');
    const plain = 'et_' + 'MockToken0123456789abcdefghijklmnopqrstuvwxyz'.slice(0, 43);
    const t: EnrollmentToken = { id: nextId++, org_id: org.id, token_prefix: plain.slice(0, 10), label: String(b.label ?? ''), max_uses, uses: 0, expires_at: iso(-ttl * 3600e3), created_by: me.id, created_at: iso(0), revoked_at: null };
    tokens.unshift(t); log('token.created', 'enrollment_token', t.id, org.id, { label: t.label });
    const base = location.origin;
    return ok({ token: t, plaintext: plain, install: {
      linux: `curl -fsSLo /tmp/msp-install.sh ${base}/dl/install.sh && sudo bash /tmp/msp-install.sh --server ${base} --token ${plain}`,
      windows: `irm ${base}/dl/install.ps1 -OutFile $env:TEMP\\msp-install.ps1; & $env:TEMP\\msp-install.ps1 -Server ${base} -Token ${plain}`,
      android: `Install ${base}/dl/msp-agent.apk, open MSP Agent, enter ${base} and the token ${plain}`,
    } }, 201);
  }
  if ((x = m(/^\/api\/v1\/orgs\/(\d+)\/tokens\/(\d+)\/revoke$/))) {
    const r = needRole('operator'); if (r) return r;
    const t = tokens.find((t) => t.org_id === Number(x![1]) && t.id === Number(x![2])); if (!t) return err(404, 'not_found');
    t.revoked_at = iso(0); log('token.revoked', 'enrollment_token', t.id, t.org_id);
    return ok(null, 204);
  }
  if (p === '/api/v1/devices') {
    const orgId = Number(q.get('org_id') ?? 0), st = q.get('status') ?? '', needle = (q.get('q') ?? '').toLowerCase();
    return ok({ devices: devices.filter((d) => (!orgId || d.org_id === orgId) && (!st || d.status === st) && (!needle || d.hostname.toLowerCase().includes(needle))) });
  }
  if ((x = m(/^\/api\/v1\/devices\/(\d+)$/))) {
    const d = devices.find((d) => d.id === Number(x![1])); if (!d) return err(404, 'not_found');
    if (method === 'DELETE') {
      const r = needRole('admin'); if (r) return r;
      devices.splice(devices.indexOf(d), 1); log('device.deleted', 'device', d.id, d.org_id, { hostname: d.hostname });
      return ok(null, 204);
    }
    return ok({
      device: d,
      facts: d.status === 'pending' ? {} : d.platform === 'android' ? androidFacts(d) : { kernel: '6.8.0-45-generic', cpu_model: 'Intel(R) Core(TM) i5-1345U', cpu_count: 8, mem_total_mb: 16000, manufacturer: 'Dell Inc.', model: 'Latitude 5540', serial: 'ABC123', ips: [d.last_ip] },
      metrics: d.status === 'pending' ? {} : { cpu_pct: d.cpu_pct, mem_total_mb: 16000, mem_used_mb: 8120, disks: [{ mount: d.platform === 'windows' ? 'C:' : '/', total_gb: 476, used_gb: 201.3 }], battery_pct: d.battery_pct, ips: [d.last_ip] },
      alerts: alerts.filter((a) => a.device_id === d.id && !a.resolved_at),
      commands: commands.filter((c) => c.device_id === d.id).slice(0, 20).map((c) => ({ ...c, stdout: '', stderr: '' })),
    });
  }
  if ((x = m(/^\/api\/v1\/devices\/(\d+)\/heartbeats$/))) {
    const d = devices.find((d) => d.id === Number(x![1])); if (!d) return err(404, 'not_found');
    const n = Math.min(Number(q.get('limit') ?? 120), 120);
    const hb = d.status === 'pending' ? [] : Array.from({ length: n }, (_, i) => ({
      at: iso((n - i) * 60e3), cpu_pct: 10 + 30 * Math.abs(Math.sin(i / 7)), mem_used_pct: 45 + 10 * Math.sin(i / 11),
      disk_used_pct: (d.disk_used_pct ?? 40) - (n - i) * 0.01, battery_pct: d.battery_pct === null ? null : Math.max(5, d.battery_pct + (n - i) * 0.3), uptime_s: 86400 * 3 + i * 60,
    }));
    return ok({ heartbeats: hb });
  }
  if (p === '/api/v1/rustdesk') return ok(rustdesk);
  if ((x = m(/^\/api\/v1\/devices\/(\d+)\/remote-desktop$/))) {
    const r = needRole('operator'); if (r) return r;
    const d = devices.find((d) => d.id === Number(x![1])); if (!d || d.revoked) return err(404, 'not_found');
    if (!rustdesk.enabled) return err(409, 'rustdesk_disabled', 'set MSP_RUSTDESK_HOST and MSP_RUSTDESK_KEY on the server');
    if (!d.rustdesk_id) return err(409, 'no_rustdesk_id', 'the agent has not reported a RustDesk id');
    log('device.remote_desktop_opened', 'device', d.id, d.org_id, { hostname: d.hostname, rustdesk_id: d.rustdesk_id, host: rustdesk.host });
    return ok({ url: `rustdesk://connect/${d.rustdesk_id}`, rustdesk_id: d.rustdesk_id, host: rustdesk.host, key: rustdesk.key });
  }
  if ((x = m(/^\/api\/v1\/devices\/(\d+)\/shell$/))) {
    const r = needRole('operator'); if (r) return r;
    const d = devices.find((d) => d.id === Number(x![1])); if (!d) return err(404, 'not_found');
    if (d.platform === 'android') return err(400, 'unsupported_on_platform');
    if (d.revoked) return err(400, 'unsupported_on_device');
    if (d.status !== 'online') return err(409, 'device_offline');
    const ns = b as unknown as NewShell;
    const shell = String(ns.shell || (d.platform === 'windows' ? 'powershell' : 'bash'));
    const c: Command = {
      id: nextId++, device_id: d.id, org_id: d.org_id, created_by: me.id, created_by_email: me.email, kind: 'shell',
      argv: [], shell, body: '', text: '', timeout_s: 14400, status: 'sent', created_at: iso(0), sent_at: iso(0),
      started_at: null, finished_at: null, exit_code: null, stdout: '', stderr: '', truncated: false, error: '',
      shell_session: { bytes_in: 0, bytes_out: 0, close_reason: '', transcript_truncated: false, connected_at: null },
    };
    commands.unshift(c); log('shell.opened', 'command', c.id, d.org_id, { shell });
    // A 43-char base64url-shaped stand-in; the real server issues a random one.
    const ticket = ('Mock' + 'Ticket0123456789abcdefghijklmnopqrstuvwxyz').slice(0, 43);
    return ok({ command: c, ticket, ws_path: `/api/v1/shell/${c.id}/ws` }, 201);
  }
  if ((x = m(/^\/api\/v1\/devices\/(\d+)\/revoke$/))) {
    const r = needRole('admin'); if (r) return r;
    const d = devices.find((d) => d.id === Number(x![1])); if (!d) return err(404, 'not_found');
    d.revoked = true; d.can_run_commands = false; log('device.revoked', 'device', d.id, d.org_id);
    return ok(null, 204);
  }
  if ((x = m(/^\/api\/v1\/devices\/(\d+)\/commands$/))) {
    const d = devices.find((d) => d.id === Number(x![1])); if (!d) return err(404, 'not_found');
    if (method === 'GET') return ok({ commands: commands.filter((c) => c.device_id === d.id).slice(0, Number(q.get('limit') ?? 50)) });
    const r = needRole('operator'); if (r) return r;
    const nc = b as unknown as NewCommand;
    if (d.revoked) return err(400, 'unsupported_on_device');
    if ((nc.kind === 'exec' || nc.kind === 'script') && d.platform === 'android') return err(400, 'unsupported_on_platform');
    if (nc.kind === 'exec' && !(nc.argv && nc.argv.length)) return err(400, 'invalid_request', 'exec needs argv with at least one element');
    if (nc.kind === 'script' && !nc.body) return err(400, 'invalid_request', 'script needs a non-empty body');
    if (nc.kind === 'message' && !nc.text) return err(400, 'invalid_request', 'message needs text');
    const c: Command = {
      id: nextId++, device_id: d.id, org_id: d.org_id, created_by: me.id, created_by_email: me.email, kind: nc.kind,
      argv: nc.argv ?? [], shell: nc.shell ?? '', body: nc.body ?? '', text: nc.text ?? '', timeout_s: nc.timeout_s || 60, status: 'queued',
      created_at: iso(0), sent_at: null, started_at: null, finished_at: null, exit_code: null, stdout: '', stderr: '', truncated: false, error: '',
    };
    commands.unshift(c); log('command.queued', 'command', c.id, d.org_id, { kind: c.kind });
    if (c.kind === 'lock') log('device.lock_requested', 'device', d.id, d.org_id);
    if (d.status !== 'offline') setTimeout(() => finish(c), 400);
    return ok({ command: c }, 201);
  }
  if (p === '/api/v1/presets') return ok({ presets, source: 'builtin', path: '' });
  if ((x = m(/^\/api\/v1\/commands\/(\d+)$/))) {
    const c = commands.find((c) => c.id === Number(x![1]));
    return c ? ok({ command: c }) : err(404, 'not_found');
  }
  if (p === '/api/v1/alerts') {
    const orgId = Number(q.get('org_id') ?? 0), all = q.get('state') === 'all';
    return ok({ alerts: alerts.filter((a) => (all || !a.resolved_at) && (!orgId || a.org_id === orgId)) });
  }
  if ((x = m(/^\/api\/v1\/alerts\/(\d+)\/ack$/))) {
    const r = needRole('operator'); if (r) return r;
    const a = alerts.find((a) => a.id === Number(x![1])); if (!a) return err(404, 'not_found');
    a.acknowledged_by = me.id; a.acknowledged_at = iso(0); log('alert.ack', 'alert', a.id, a.org_id);
    return ok(null, 204);
  }
  if (p === '/api/v1/audit') {
    const orgId = Number(q.get('org_id') ?? 0), before = Number(q.get('before') ?? 0), limit = Number(q.get('limit') ?? 200);
    return ok({ entries: audit.filter((e) => (!orgId || e.org_id === orgId) && (!before || e.id < before)).slice(0, limit) });
  }
  if (p === '/api/v1/users') {
    const r = needRole('admin'); if (r) return r;
    if (method === 'GET') return ok({ users });
    const email = String(b.email ?? '').trim().toLowerCase(), password = String(b.password ?? ''), rl = String(b.role ?? '');
    if (!email.includes('@') || password.length < 10 || !['admin', 'operator', 'viewer'].includes(rl)) return err(400, 'invalid_request', 'email, password >= 10 chars and a valid role are required');
    if (users.some((u) => u.email === email)) return err(409, 'conflict');
    // admins are always all-orgs; operator/viewer may carry all_orgs/org_ids (RS2).
    const scopable = rl === 'operator' || rl === 'viewer';
    const wantAll = b.all_orgs === undefined ? true : b.all_orgs === true;
    const ids = normIds(b.org_ids);
    if (scopable && !wantAll) { for (const id of ids) if (!orgs.some((o) => o.id === id)) return err(400, 'invalid_request', `org ${id} does not exist`); }
    const user: User = {
      id: nextId++, email, role: rl as User['role'], created_at: iso(0), disabled_at: null,
      all_orgs: scopable ? wantAll : true, org_ids: scopable && !wantAll ? ids : [],
    };
    users.push(user); log('user.created', 'user', user.id, null, { email, role: rl });
    return ok({ user }, 201);
  }
  if ((x = m(/^\/api\/v1\/users\/(\d+)\/orgs$/))) {
    const r = needRole('admin'); if (r) return r;
    const u = users.find((u) => u.id === Number(x![1])); if (!u) return err(404, 'not_found');
    const wantAll = b.all_orgs === true;
    if (u.role === 'admin' && !wantAll) return err(400, 'invalid_request', 'admins cannot be scoped');
    const ids = normIds(b.org_ids);
    if (!wantAll) { for (const id of ids) if (!orgs.some((o) => o.id === id)) return err(400, 'invalid_request', `org ${id} does not exist`); }
    u.all_orgs = u.role === 'admin' ? true : wantAll;
    u.org_ids = u.role === 'admin' || wantAll ? [] : ids;
    log('user.orgs_changed', 'user', u.id, null, { all_orgs: u.all_orgs, org_ids: u.org_ids });
    return ok(null, 204);
  }
  if ((x = m(/^\/api\/v1\/users\/(\d+)\/disable$/))) {
    const r = needRole('admin'); if (r) return r;
    const u = users.find((u) => u.id === Number(x![1])); if (!u) return err(404, 'not_found');
    u.disabled_at = iso(0); log('user.disabled', 'user', u.id, null);
    return ok(null, 204);
  }
  return err(404, 'not_found');
}

export function mockRequest(method: string, path: string, body?: unknown): Promise<Reply> {
  // Structured clone so views never hold references into the mock state.
  return new Promise((resolve) => setTimeout(() => {
    const r = route(method, path, body);
    resolve({ status: r.status, body: r.body === null ? null : JSON.parse(JSON.stringify(r.body)) });
  }, 60));
}

/**
 * A stub shell for ?mock=1: no server, no WebSocket. It implements ShellSocket,
 * announces waiting_for_agent then connected, prints a banner, and echoes typed
 * bytes back as PTY output (Enter -> CRLF) so the terminal panel is usable
 * offline. Resize control frames are accepted and ignored.
 */
export function mockShellSocket(shell: string): ShellSocket {
  const emit = (s: string) => sock.onmessage?.({ data: new TextEncoder().encode(s).buffer });
  const status = (state: string, reason = '') => sock.onmessage?.({ data: JSON.stringify({ type: 'status', state, reason }) });
  const sock: ShellSocket = {
    binaryType: 'arraybuffer', readyState: 0,
    onopen: null, onmessage: null, onclose: null, onerror: null,
    send(data) {
      if (sock.readyState !== 1 || typeof data === 'string') return; // ignore resize frames
      const bytes = data instanceof Uint8Array ? data : new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
      let out = '';
      for (const byte of bytes) out += byte === 13 ? '\r\n' : String.fromCharCode(byte);
      emit(out);
    },
    close() {
      if (sock.readyState === 3) return;
      sock.readyState = 3;
      status('closed', 'technician_closed');
      sock.onclose?.();
    },
  };
  setTimeout(() => {
    sock.readyState = 1;
    sock.onopen?.();
    status('waiting_for_agent');
    setTimeout(() => {
      status('connected');
      emit(`mock ${shell} (echo) — typed keystrokes are echoed back; this is a demo with no real device.\r\n$ `);
    }, 300);
  }, 60);
  return sock;
}
