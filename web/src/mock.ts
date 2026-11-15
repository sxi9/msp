// In-memory stand-in for the server, active when location.search has mock=1
// (add &role=viewer|operator to try a weaker role). Lets the dashboard be
// developed and demoed without a backend. Nothing here ships to the server.
import type {
  AgentManifest, Alert, AuditEntry, Automation, Command, Device, EnrollmentToken, EscalationPolicy,
  Job, JobDeviceRow, LibraryFile, MaintenanceWindow, NewAutomation, NewCommand, NewDeploy, NewJob,
  NewPatch, NewScript, NewShell, NotificationDelivery, NotifyChannel, NotifyRule, Org, Preset, Script,
  TargetSpec, User,
} from './types.ts';
import type { ShellSocket } from './lib/shell.ts';
import type { SoftwareHit, SoftwareItem } from './lib/software.ts';
import { resolveTarget, scriptSkipNote } from './lib/automations.ts';
import { bucketDevices, expectedWindows, meanUptime, seriesStats, uptimePct } from './lib/reports.ts';
import type { DiskPoint, DiskTopRow, PatchComplianceRow, UptimeRow } from './lib/reports.ts';

interface Reply { status: number; body: unknown }

const iso = (msAgo: number) => new Date(Date.now() - msAgo).toISOString();
const randHex = (n: number) => Array.from({ length: n }, () => Math.floor(Math.random() * 16).toString(16)).join('');
const role = (new URLSearchParams(location.search).get('role') ?? 'admin') as User['role'];
// Add &totp=1 to the URL to sign in as a user with 2FA on: the login second step
// appears and the Account page shows the "on" management view (TF1). Without the
// flag, 2FA is off so the Account set-up flow is demoable.
const twofaOn = new URLSearchParams(location.search).get('totp') === '1';
// Unused recovery codes left for `me` (TF1); the /me response reports it.
let totpRemaining = twofaOn ? 8 : 0;
const me: User = { id: 1, email: 'admin@example.com', role, created_at: iso(86400e3 * 30), disabled_at: null, all_orgs: true, org_ids: [], totp_enabled: twofaOn };
const users: User[] = [
  me,
  // A scoped operator with 2FA on, so the Users page shows the "On" badge and the
  // admin "Reset 2FA" action (TF1). Sees only Acme Dental (org 1), so the
  // Edit-access editor opens with "All clients" off and that one org checked (RS2).
  { id: 2, email: 'tech@example.com', role: 'operator', created_at: iso(86400e3 * 9), disabled_at: null, all_orgs: false, org_ids: [1], totp_enabled: true },
  // An all-clients viewer, for contrast in the list (2FA off).
  { id: 3, email: 'viewer@example.com', role: 'viewer', created_at: iso(86400e3 * 4), disabled_at: null, all_orgs: true, org_ids: [], totp_enabled: false },
];
// A fixed 20-byte base32 secret for the mock set-up flow (TF1).
const mockSecret = 'JBSWY3DPEHPK3PXPJBSWY3DP';
const mockOtpauth = `otpauth://totp/MSP:${encodeURIComponent(me.email)}?secret=${mockSecret}&issuer=MSP&digits=6&period=30`;
const genRecoveryCodes = (): string[] => {
  const a = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  const grp = () => Array.from({ length: 5 }, () => a[Math.floor(Math.random() * a.length)]).join('');
  return Array.from({ length: 10 }, () => `${grp()}-${grp()}`);
};
// A typed code (TOTP 6-digit or a recovery code) the mock accepts for the login
// second step and the Account management actions; the real check is server-side.
const looksLikeFactor = (code: string): boolean => /^\d{6}$/.test(code) || /^[A-Za-z2-7]{5}-[A-Za-z2-7]{5}$/.test(code);
const isRecovery = (code: string): boolean => /^[A-Za-z2-7]{5}-[A-Za-z2-7]{5}$/.test(code);
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
  can_run_commands: platform !== 'android', rustdesk_id: null,
  pending_patches: 0, security_patches: 0, reboot_required: false, patches_checked_at: null,
  software_count: 0, software_checked_at: null, ...extra,
});
// The mock relay; add &rustdesk=0 to the URL to see the "not configured" card.
const rustdesk = new URLSearchParams(location.search).get('rustdesk') === '0'
  ? { enabled: false, host: '', key: '' }
  : { enabled: true, host: 'rmm.example.com', key: 'OeVuKk5nlHiXp+7qsR6CbcHBGxhU4PJkNbvoX3P5X0k=' };
// The agent release manifest (AU1): the single released version and the published
// desktop binaries per platform-arch. Add &selfupdate=0 to the URL to see the
// "self-update off" state (empty manifest). Most mock devices report 0.1.0 (older),
// so they show as outdated; ws-xray below runs 0.2.0 and shows as up to date.
const agentManifest: AgentManifest = new URLSearchParams(location.search).get('selfupdate') === '0'
  ? { version: '', files: {} }
  : {
    version: '0.2.0',
    files: {
      'linux-amd64': { name: 'msp-agent-linux-amd64', sha256: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855' },
      'linux-arm64': { name: 'msp-agent-linux-arm64', sha256: '1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014' },
      'windows-amd64': { name: 'msp-agent-windows-amd64.exe', sha256: '60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752' },
    },
  };
const devices: Device[] = [
  // A device with pending updates including a security one (PM1); agent 0.1.0, older
  // than the released 0.2.0, so it shows as an outdated agent (AU1).
  dev(1, 1, 'ws-reception-01', 'linux', 'online', { rustdesk_id: '123456789', pending_patches: 3, security_patches: 1, patches_checked_at: iso(3600e3) }),
  // An up-to-date agent (0.2.0): no update offered (AU1).
  dev(2, 1, 'ws-xray', 'windows', 'online', { disk_used_pct: 93.1, open_alerts: 1, rustdesk_id: '987654321', agent_version: '0.2.0' }),
  dev(3, 1, 'Pixel 8', 'android', 'online', { battery_pct: 12, battery_charging: false, open_alerts: 1 }),
  dev(4, 1, 'ws-backoffice', 'linux', 'offline', { open_alerts: 1 }),
  // A reboot-required device: everything installed, a reboot is pending (PM1).
  dev(5, 2, 'LAPTOP-7Q2', 'windows', 'online', { cpu_pct: 71, reboot_required: true, patches_checked_at: iso(7200e3) }),
  dev(6, 2, 'srv-files', 'linux', 'pending'),
];
// The full patches report keyed by device id (PM1); the device-detail response
// serves patchReports[id] ?? {} as its `patches` object. Devices absent here
// have never reported (the panel shows "No patch report yet").
const patchReports: Record<number, Record<string, unknown>> = {
  1: {
    checked_at: iso(3600e3), manager: 'apt', reboot_required: false, count: 3, security_count: 1, error: '',
    updates: [
      { name: 'openssl', current: '3.0.13-0ubuntu3.1', new: '3.0.13-0ubuntu3.4', security: true, source: 'noble-security' },
      { name: 'curl', current: '8.5.0-2ubuntu10.1', new: '8.5.0-2ubuntu10.6', security: false, source: 'noble-updates' },
      { name: 'vim', current: '2:9.1.0016-1ubuntu7', new: '2:9.1.0016-1ubuntu7.9', security: false, source: 'noble-updates' },
    ],
  },
  5: { checked_at: iso(7200e3), manager: 'wu', reboot_required: true, count: 0, security_count: 0, error: '', updates: [] },
};
// The installed-software inventory a desktop agent reports (SI1), keyed by device
// id: Linux dpkg packages and Windows registry programs. The device-detail
// response serves softwareInventory[id] as the device's `software` list, and the
// fleet software search below scans these. Android devices report apps, not
// software, so they are absent here. The device software_count is backfilled from
// these lengths just below.
const softwareInventory: Record<number, SoftwareItem[]> = {
  1: [
    { name: 'openssl', version: '3.0.13-0ubuntu3.4', publisher: 'Ubuntu Developers', source: 'dpkg' },
    { name: 'curl', version: '8.5.0-2ubuntu10.6', publisher: 'Ubuntu Developers', source: 'dpkg' },
    { name: 'nginx-core', version: '1.24.0-2ubuntu7', publisher: 'Ubuntu Developers', source: 'dpkg' },
    { name: 'openssh-server', version: '1:9.6p1-3ubuntu13', publisher: 'Ubuntu Developers', source: 'dpkg' },
    { name: 'python3', version: '3.12.3-0ubuntu2', publisher: 'Ubuntu Developers', source: 'dpkg' },
    { name: 'vim', version: '2:9.1.0016-1ubuntu7', publisher: 'Ubuntu Developers', source: 'dpkg' },
  ],
  2: [
    { name: '7-Zip 23.01 (x64)', version: '23.01', publisher: 'Igor Pavlov', source: 'registry' },
    { name: 'Google Chrome', version: '129.0.6668.70', publisher: 'Google LLC', source: 'registry' },
    { name: 'Mozilla Firefox (x64 en-US)', version: '131.0.3', publisher: 'Mozilla', source: 'registry' },
    { name: 'OpenSSL 3.0.13', version: '3.0.13', publisher: 'OpenSSL Project', source: 'registry' },
    { name: 'Microsoft Edge', version: '129.0.2792.65', publisher: 'Microsoft Corporation', source: 'registry' },
  ],
  4: [
    { name: 'openssl', version: '3.0.13-0ubuntu3.1', publisher: 'Ubuntu Developers', source: 'dpkg' },
    { name: 'curl', version: '8.5.0-2ubuntu10.1', publisher: 'Ubuntu Developers', source: 'dpkg' },
    { name: 'docker-ce', version: '5:27.3.1-1~ubuntu.24.04~noble', publisher: 'Docker Inc.', source: 'dpkg' },
  ],
  5: [
    { name: 'Acme VPN Client', version: '2.1.0', publisher: 'Acme Corp', source: 'registry' },
    { name: 'Google Chrome', version: '129.0.6668.70', publisher: 'Google LLC', source: 'registry' },
    { name: 'OpenSSL 3.0.13', version: '3.0.13', publisher: 'OpenSSL Project', source: 'registry' },
    { name: 'Microsoft Visual C++ 2015-2022 Redistributable (x64)', version: '14.40.33810', publisher: 'Microsoft Corporation', source: 'registry' },
  ],
};
for (const d of devices) {
  const inv = softwareInventory[d.id];
  if (inv) { d.software_count = inv.length; d.software_checked_at = iso(3600e3); }
}
// The server file library (SD1): one global artifact usable by any client and one
// scoped to Acme Dental (org 1). The Deploy dialog filters these to a device's org.
const files: LibraryFile[] = [
  { id: 70, org_id: null, name: 'bootstrap.sh', size: 4096, sha256: 'a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00', content_type: 'text/x-shellscript', created_at: iso(86400e3 * 2), created_by: 1, created_by_email: 'admin@example.com' },
  { id: 71, org_id: 1, name: 'acme-vpn-setup.exe', size: 18874368, sha256: '00ffeeddccbbaa998877665544332211009f8e7d6c5b4a39281706f5e4d3c2b1', content_type: 'application/vnd.microsoft.portable-executable', created_at: iso(86400e3), created_by: 1, created_by_email: 'admin@example.com' },
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
// NS1 alert notifications: two channels (one email with a stored secret, one
// Slack webhook), a rule, an active maintenance window and a few deliveries
// including a suppressed and a sent one. The secret is write-only, so a channel
// carries only secret_set.
const notifyChannels: NotifyChannel[] = [
  {
    id: 20, kind: 'email', name: 'On-call email', org_id: null,
    config: { host: 'smtp.example.com', port: 587, from: 'alerts@example.com', to: ['oncall@example.com', 'noc@example.com'], starttls: true, username: 'alerts@example.com' },
    secret_set: true, enabled: true, created_at: iso(86400e3 * 3), created_by: 1,
  },
  {
    id: 21, kind: 'webhook', name: 'Acme Slack', org_id: 1,
    config: { url: 'https://hooks.slack.com/services/T0000/B0000/xxxxxxxxxxxxxxxx', flavor: 'slack' },
    secret_set: false, enabled: true, created_at: iso(86400e3 * 2), created_by: 1,
  },
  {
    id: 22, kind: 'sms', name: 'On-call SMS', org_id: 1,
    config: { provider: 'twilio', from: '+15550001111', to: ['+15557654321'], account_sid: 'example-account-sid', url: '' },
    secret_set: true, enabled: true, created_at: iso(86400e3 * 2), created_by: 1,
  },
];
// A 2-step escalation policy (NS2): page Slack at open, then text on-call after 15 min.
const escalationPolicies: EscalationPolicy[] = [
  {
    id: 60, name: 'Acme critical escalation', org_id: 1,
    steps: [
      { after_minutes: 0, channel_ids: [21] },
      { after_minutes: 15, channel_ids: [22] },
    ],
    created_at: iso(86400e3 * 2), created_by: 1,
  },
];
const notifyRules: NotifyRule[] = [
  { id: 30, name: 'Acme high alerts to Slack', org_id: 1, min_severity: 'high', alert_types: [], on_open: true, on_resolve: false, channel_id: 21, escalation_policy_id: 60, enabled: true, created_at: iso(86400e3 * 2), created_by: 1 },
];
const maintenanceWindows: MaintenanceWindow[] = [
  // Started 1h ago, ends 1h from now -> active now (active_now computed on read).
  { id: 40, org_id: 1, starts_at: iso(3600e3), ends_at: iso(-3600e3), reason: 'Server room UPS replacement', active_now: true, created_at: iso(86400e3), created_by: 1 },
];
const deliveries: NotificationDelivery[] = [
  { id: 50, alert_id: 1, rule_id: 30, channel_id: 21, org_id: 1, event: 'opened', status: 'sent', attempts: 1, last_error: '', detail: { kind: 'webhook', flavor: 'slack', url: 'https://hooks.slack.com', signed: false }, step: 1, created_at: iso(3600e3 * 4), updated_at: iso(3600e3 * 4) },
  // Escalation step 2 fired 15 min after the alert opened and was still unacked.
  { id: 53, alert_id: 1, rule_id: 30, channel_id: 22, org_id: 1, event: 'opened', status: 'sent', attempts: 1, last_error: '', detail: { kind: 'sms', provider: 'twilio', to: ['+15557654321'] }, step: 2, created_at: iso(3600e3 * 4 - 900e3), updated_at: iso(3600e3 * 4 - 900e3) },
  { id: 51, alert_id: 2, rule_id: 30, channel_id: 21, org_id: 1, event: 'opened', status: 'suppressed', attempts: 0, last_error: '', detail: {}, step: null, created_at: iso(1800e3), updated_at: iso(1800e3) },
  { id: 52, alert_id: null, rule_id: null, channel_id: 20, org_id: null, event: 'test', status: 'failed', attempts: 5, last_error: 'dial tcp smtp.example.com:587: connect: connection refused', detail: { kind: 'email', to: ['oncall@example.com'], subject: '[MSP] test from On-call email' }, step: null, created_at: iso(900e3), updated_at: iso(700e3) },
];
const windowActiveNow = (w: MaintenanceWindow): boolean => {
  const s = Date.parse(w.starts_at), e = Date.parse(w.ends_at), now = Date.now();
  return !Number.isNaN(s) && !Number.isNaN(e) && now >= s && now < e;
};

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
// AUT1 script library, bulk jobs and automations. A couple of library scripts
// (one global, one Acme-scoped), a finished manual job with mixed statuses (done,
// failed, and an android device skipped), and two automations — a nightly
// schedule and an alert trigger. The job's per-device rows live in jobDeviceRows;
// GET /jobs/{id} derives progress from them (reading each live command's status
// when one exists, else the stored status for the seeded rows).
const scripts: Script[] = [
  { id: 80, org_id: null, name: 'Disk usage report', description: 'df -h on every desktop', platform: 'any', kind: 'script', argv: [], shell: 'sh', body: 'df -h\n', timeout_s: 120, created_at: iso(86400e3 * 3), created_by: 1 },
  { id: 81, org_id: 1, name: 'Flush DNS cache', description: 'Clear the systemd-resolved cache on Acme Linux desktops', platform: 'linux', kind: 'exec', argv: ['resolvectl', 'flush-caches'], shell: '', body: '', timeout_s: 60, created_at: iso(86400e3 * 2), created_by: 1 },
];
const jobs: Job[] = [
  { id: 90, org_id: 1, name: 'Disk usage report', origin: 'manual', script_id: 80, created_by: 1, automation_id: null, created_at: iso(3600e3 * 2), total: 3 },
];
const jobDeviceRows: Record<number, JobDeviceRow[]> = {
  90: [
    { device_id: 1, hostname: 'ws-reception-01', org_id: 1, org_name: 'Acme Dental', command_id: 501, status: 'done', skipped: false, note: '' },
    { device_id: 2, hostname: 'ws-xray', org_id: 1, org_name: 'Acme Dental', command_id: 502, status: 'failed', skipped: false, note: '' },
    { device_id: 4, hostname: 'ws-backoffice', org_id: 1, org_name: 'Acme Dental', command_id: 503, status: 'done', skipped: false, note: '' },
    { device_id: 3, hostname: 'Pixel 8', org_id: 1, org_name: 'Acme Dental', command_id: null, status: null, skipped: true, note: 'script commands are not supported on android' },
  ],
};
const automations: Automation[] = [
  // A nightly schedule: runs the global disk-usage script on Acme's Linux desktops.
  { id: 95, org_id: 1, name: 'Nightly disk report', enabled: true, trigger: 'schedule', script_id: 80, target: { org_id: 1, platform: 'linux' }, schedule: { every_minutes: 1440 }, alert: null, last_run_at: iso(3600e3 * 10), next_run_at: iso(-3600e3 * 14), created_at: iso(86400e3 * 3), created_by: 1 },
  // An alert trigger: flush DNS when a medium+ disk_full alert opens on the device.
  { id: 96, org_id: 1, name: 'Flush DNS on disk pressure', enabled: false, trigger: 'alert', script_id: 81, target: {}, schedule: null, alert: { alert_type: 'disk_full', min_severity: 'medium' }, last_run_at: null, next_run_at: null, created_at: iso(86400e3 * 2), created_by: 1 },
];
// The non-skipped device rows of a job, with each live command's current status
// folded in (a seeded row with no live command keeps its stored status).
const jobRowsLive = (jobId: number): JobDeviceRow[] =>
  (jobDeviceRows[jobId] ?? []).map((r) => {
    if (r.skipped || r.command_id === null) return r;
    const c = commands.find((c) => c.id === r.command_id);
    return c ? { ...r, status: c.status } : r;
  });
const jobProgressOf = (jobId: number, total: number) => {
  const p = { job_id: jobId, total, queued: 0, sent: 0, done: 0, failed: 0 };
  for (const r of jobRowsLive(jobId)) {
    if (r.skipped) continue;
    if (r.status === 'queued') p.queued++;
    else if (r.status === 'sent') p.sent++;
    else if (r.status === 'done') p.done++;
    else p.failed++; // failed, timeout, lost, cancelled
  }
  return p;
};

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
    else if (c.kind === 'patch') {
      c.status = 'done'; c.exit_code = 0;
      c.stdout = c.action === 'scan'
        ? 'Reading package lists...\nscan complete: 3 updates pending (1 security)\n'
        : `Reading package lists...\nThe following packages will be upgraded:\n  ${c.mode === 'security' ? 'openssl' : 'openssl curl vim'}\nSetting up openssl ...\ndone.\n`;
    }
    else if (c.kind === 'deploy') {
      c.status = 'done'; c.exit_code = 0;
      c.stdout = c.source === 'package'
        ? `Reading package lists...\nInstalling ${c.package} ...\nSetting up ${c.package} ...\ndone.\n`
        : `Downloaded ${c.dest} (sha256 ${c.sha256?.slice(0, 12)}… verified)\n` +
          (c.run ? `Running ${c.dest} ${(c.run_args ?? []).join(' ')}\nmarker written\n` : 'wrote file (not run)\n');
    }
    else if (c.kind === 'update') {
      c.status = 'done'; c.exit_code = 0;
      const d = devices.find((x) => x.id === c.device_id);
      const from = d?.agent_version ?? '?';
      c.stdout = `agent ${from} -> ${c.version}\n` +
        `Downloaded ${c.url} (sha256 ${c.sha256?.slice(0, 12)}… verified)\n` +
        `Health check: ${c.version} version OK\n` +
        `Swapped binary (backup kept at <exe>.bak)\n` +
        `Restarting into ${c.version} …\n`;
      // Reflect the successful swap so the device page shows the new version.
      if (d && c.version) d.agent_version = c.version;
    }
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
    if (me.totp_enabled) return ok({ totp_required: true }); // step 2 follows (TF1)
    return ok({ user: me });
  }
  if (p === '/api/v1/login/totp') {
    const code = String(b.code ?? '').trim();
    if (!looksLikeFactor(code)) return err(401, 'invalid_code');
    if (isRecovery(code) && totpRemaining > 0) totpRemaining--; // single-use
    log('user.login', 'user', me.id, null, { second_factor: isRecovery(code) ? 'recovery' : 'totp' });
    return ok({ user: me });
  }
  if (p === '/api/v1/logout') return ok(null, 204);
  if (p === '/api/v1/me') return ok({ user: { ...me, recovery_codes_remaining: me.totp_enabled ? totpRemaining : 0 } });
  if (p === '/api/v1/me/password') {
    const cur = String(b.current_password ?? ''), nw = String(b.new_password ?? '');
    if (cur.length < 4) return err(400, 'invalid_request', 'current password is incorrect');
    if (nw.length < 10) return err(400, 'invalid_request', 'password must be at least 10 characters');
    log('user.password_changed', 'user', me.id, null, { email: me.email });
    return ok(null, 204);
  }
  // --- TF1 two-factor management (self) ---
  if (p === '/api/v1/me/totp/setup') {
    if (me.totp_enabled) return err(409, 'conflict', 'two-factor is already enabled');
    return ok({ secret: mockSecret, otpauth_uri: mockOtpauth });
  }
  if (p === '/api/v1/me/totp/confirm') {
    if (!/^\d{6}$/.test(String(b.code ?? ''))) return err(400, 'invalid_code', 'that code is not correct');
    me.totp_enabled = true; totpRemaining = 10;
    log('user.totp_enabled', 'user', me.id, null, { email: me.email });
    return ok({ recovery_codes: genRecoveryCodes() });
  }
  if (p === '/api/v1/me/totp/disable') {
    if (!me.totp_enabled) return err(400, 'invalid_request', 'two-factor is not enabled');
    if (!looksLikeFactor(String(b.code ?? '').trim())) return err(400, 'invalid_code');
    me.totp_enabled = false; totpRemaining = 0;
    log('user.totp_disabled', 'user', me.id, null, { email: me.email });
    return ok(null, 204);
  }
  if (p === '/api/v1/me/totp/recovery/regenerate') {
    if (!me.totp_enabled) return err(400, 'invalid_request', 'two-factor is not enabled');
    if (!looksLikeFactor(String(b.code ?? '').trim())) return err(400, 'invalid_code');
    totpRemaining = 10;
    log('user.totp_recovery_regenerated', 'user', me.id, null, { email: me.email });
    return ok({ recovery_codes: genRecoveryCodes() });
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
      // The server stores the whole facts object (the installed-software array
      // included) in devices.facts_json and returns it here, so a real desktop
      // carries `software` inside facts; the Device view must exclude it from the
      // Facts panel (it has its own "Installed software" panel). Mirror that here.
      facts: d.status === 'pending' ? {} : d.platform === 'android' ? androidFacts(d) : { kernel: '6.8.0-45-generic', cpu_model: 'Intel(R) Core(TM) i5-1345U', cpu_count: 8, mem_total_mb: 16000, manufacturer: 'Dell Inc.', model: 'Latitude 5540', serial: 'ABC123', ips: [d.last_ip], software: softwareInventory[d.id] ?? [] },
      metrics: d.status === 'pending' ? {} : { cpu_pct: d.cpu_pct, mem_total_mb: 16000, mem_used_mb: 8120, disks: [{ mount: d.platform === 'windows' ? 'C:' : '/', total_gb: 476, used_gb: 201.3 }], battery_pct: d.battery_pct, ips: [d.last_ip] },
      alerts: alerts.filter((a) => a.device_id === d.id && !a.resolved_at),
      commands: commands.filter((c) => c.device_id === d.id).slice(0, 20).map((c) => ({ ...c, stdout: '', stderr: '' })),
      patches: patchReports[d.id] ?? {},
      // Installed-software inventory (SI1), a sibling like `patches`; Android
      // reports apps (in facts), not software.
      software: d.platform === 'android' ? [] : (softwareInventory[d.id] ?? []),
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
  if ((x = m(/^\/api\/v1\/devices\/(\d+)\/patch$/))) {
    const r = needRole('operator'); if (r) return r;
    const d = devices.find((d) => d.id === Number(x![1])); if (!d) return err(404, 'not_found');
    if (d.revoked) return err(400, 'unsupported_on_device');
    if (d.platform === 'android') return err(400, 'unsupported_on_platform');
    const np = b as unknown as NewPatch;
    const action = np.action ?? 'install';
    if (action !== 'scan' && action !== 'install') return err(400, 'invalid_request', 'patch action must be scan or install');
    const mode = action === 'scan' ? undefined : (np.mode ?? 'security');
    const reboot = action === 'scan' ? undefined : (np.reboot ?? 'never');
    const ids = action === 'scan' ? undefined : (np.ids ?? []);
    if (mode && !['all', 'security', 'ids'].includes(mode)) return err(400, 'invalid_request', 'patch mode must be all, security or ids');
    // Server-side ids subset check where a report exists (PM1).
    if (action === 'install' && mode === 'ids') {
      const rep = patchReports[d.id];
      const updates = (rep?.updates as { name: string }[] | undefined) ?? [];
      if (updates.length) {
        const pending = new Set(updates.map((u) => u.name));
        const unknown = (ids ?? []).filter((id) => !pending.has(id)).sort();
        if (unknown.length) return err(400, 'invalid_request', `unknown package(s) not in the pending list: ${unknown.join(', ')}`);
      }
    }
    if (d.status !== 'online') return err(409, 'device_offline');
    const c: Command = {
      id: nextId++, device_id: d.id, org_id: d.org_id, created_by: me.id, created_by_email: me.email, kind: 'patch',
      argv: [], shell: '', body: '', text: '', timeout_s: np.timeout_s || 3600, status: 'queued',
      created_at: iso(0), sent_at: null, started_at: null, finished_at: null, exit_code: null, stdout: '', stderr: '', truncated: false, error: '',
      action, mode, ids, reboot,
    };
    commands.unshift(c);
    log(action === 'scan' ? 'patch.scan_requested' : 'patch.install_requested', 'command', c.id, d.org_id,
      action === 'scan' ? { action, hostname: d.hostname } : { action, mode, reboot, hostname: d.hostname, ...(mode === 'ids' ? { ids } : {}) });
    setTimeout(() => finish(c), 400); // the device is online here (checked above)
    return ok({ command: c }, 201);
  }
  if ((x = m(/^\/api\/v1\/devices\/(\d+)\/deploy$/))) {
    const r = needRole('operator'); if (r) return r;
    const d = devices.find((d) => d.id === Number(x![1])); if (!d) return err(404, 'not_found');
    if (d.revoked) return err(400, 'unsupported_on_device');
    if (d.platform === 'android') return err(400, 'unsupported_on_platform');
    const nd = b as unknown as NewDeploy;
    if (nd.source !== 'file' && nd.source !== 'package') return err(400, 'invalid_request', 'deploy source must be one of file, package');
    let fileId = 0, sha = '';
    if (nd.source === 'file') {
      const f = files.find((f) => f.id === Number(nd.file_id));
      if (!f) return err(404, 'not_found');
      if (f.org_id !== null && f.org_id !== d.org_id) return err(404, 'not_found');
      const dest = String(nd.dest ?? '');
      const absolute = dest.startsWith('/') || dest.startsWith('\\\\') || /^[A-Za-z]:[/\\]/.test(dest);
      if (!dest || dest.replace(/\\/g, '/').split('/').includes('..') || !absolute) return err(400, 'invalid_request', 'deploy dest must be an absolute path with no ".."');
      fileId = f.id; sha = f.sha256;
    } else if (!/^[A-Za-z0-9._+-]{1,256}$/.test(String(nd.package ?? ''))) {
      return err(400, 'invalid_request', 'deploy package must be 1..256 characters of [A-Za-z0-9._+-]');
    }
    if (d.status !== 'online') return err(409, 'device_offline');
    const c: Command = {
      id: nextId++, device_id: d.id, org_id: d.org_id, created_by: me.id, created_by_email: me.email, kind: 'deploy',
      argv: [], shell: '', body: '', text: '', timeout_s: nd.timeout_s || 3600, status: 'queued',
      created_at: iso(0), sent_at: null, started_at: null, finished_at: null, exit_code: null, stdout: '', stderr: '', truncated: false, error: '',
      source: nd.source, file_id: fileId || undefined, sha256: sha || undefined, dest: nd.dest, run: nd.run, run_args: nd.run_args, package: nd.package,
    };
    commands.unshift(c);
    log('deploy.requested', 'command', c.id, d.org_id, nd.source === 'file'
      ? { source: 'file', file_id: fileId, dest: nd.dest, run: !!nd.run }
      : { source: 'package', package: nd.package });
    setTimeout(() => finish(c), 400); // the device is online here (checked above)
    return ok({ command: c }, 201);
  }
  if ((x = m(/^\/api\/v1\/devices\/(\d+)\/update$/))) {
    const r = needRole('operator'); if (r) return r;
    const d = devices.find((d) => d.id === Number(x![1])); if (!d) return err(404, 'not_found');
    if (d.revoked) return err(400, 'unsupported_on_device');
    if (d.platform === 'android') return err(400, 'unsupported_on_platform');
    const entry = agentManifest.files[`${d.platform}-${d.arch}`];
    if (!agentManifest.version || !entry || agentManifest.version === d.agent_version) return err(409, 'no_update_available');
    if (d.status !== 'online') return err(409, 'device_offline');
    const c: Command = {
      id: nextId++, device_id: d.id, org_id: d.org_id, created_by: me.id, created_by_email: me.email, kind: 'update',
      argv: [], shell: '', body: '', text: '', timeout_s: 600, status: 'queued',
      created_at: iso(0), sent_at: null, started_at: null, finished_at: null, exit_code: null, stdout: '', stderr: '', truncated: false, error: '',
      version: agentManifest.version, url: `/dl/${entry.name}`, sha256: entry.sha256,
    };
    commands.unshift(c);
    log('agent.update_requested', 'command', c.id, d.org_id, { from_version: d.agent_version, to_version: agentManifest.version, hostname: d.hostname });
    setTimeout(() => finish(c), 400); // the device is online here (checked above)
    return ok({ command: c }, 201);
  }
  if (p === '/api/v1/files' && method === 'GET') {
    const orgId = Number(q.get('org_id') ?? 0);
    return ok({ files: files.filter((f) => !orgId || f.org_id === orgId) });
  }
  if (p === '/api/v1/files' && method === 'POST') {
    const r = needRole('operator'); if (r) return r;
    const name = String(b.name ?? '').trim();
    if (!name) return err(400, 'invalid_request', 'a file name is required');
    const org_id = b.org_id === null || b.org_id === undefined ? null : Number(b.org_id);
    const size = Number(b.size ?? 0);
    const f: LibraryFile = {
      id: nextId++, org_id, name, size: Number.isFinite(size) && size >= 0 ? size : 0,
      sha256: randHex(64), content_type: '', created_at: iso(0), created_by: me.id, created_by_email: me.email,
    };
    files.unshift(f); log('file.uploaded', 'file', f.id, org_id, { name: f.name, size: f.size });
    return ok({ file: f }, 201);
  }
  if ((x = m(/^\/api\/v1\/files\/(\d+)$/))) {
    const f = files.find((f) => f.id === Number(x![1])); if (!f) return err(404, 'not_found');
    if (method === 'DELETE') {
      const r = needRole('operator'); if (r) return r;
      files.splice(files.indexOf(f), 1); log('file.deleted', 'file', f.id, f.org_id, { name: f.name });
      return ok(null, 204);
    }
    return ok({ file: f });
  }
  // Installed-software inventory search (SI1): the packages whose name matches the
  // substring q across the devices (org_id narrows to one client). The mock does
  // not model per-user org scoping (like the rest of the mock), so it honours only
  // the org_id filter; the real server scopes to the caller's visible orgs (RS2).
  if (p === '/api/v1/software') {
    const raw = (q.get('q') ?? '').trim();
    if (raw.length < 2) return err(400, 'invalid_request', 'q is required and must be at least 2 characters');
    const needle = raw.toLowerCase();
    const orgId = Number(q.get('org_id') ?? 0);
    const limit = 500;
    const results: SoftwareHit[] = [];
    for (const d of devices) {
      if (orgId && d.org_id !== orgId) continue;
      for (const s of softwareInventory[d.id] ?? []) {
        if (results.length >= limit) break;
        if (s.name.toLowerCase().includes(needle)) {
          results.push({ device_id: d.id, hostname: d.hostname, org_id: d.org_id, org_name: d.org_name, name: s.name, version: s.version });
        }
      }
    }
    return ok({ query: raw, results, truncated: results.length >= limit });
  }
  if (p === '/api/v1/agent-release') return ok(agentManifest);
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
  if ((x = m(/^\/api\/v1\/users\/(\d+)\/totp\/reset$/))) {
    const r = needRole('admin'); if (r) return r;
    const u = users.find((u) => u.id === Number(x![1])); if (!u) return err(404, 'not_found');
    u.totp_enabled = false;
    if (u.id === me.id) totpRemaining = 0;
    log('user.totp_reset', 'user', u.id, null, { email: u.email });
    return ok(null, 204);
  }

  // --- NS1 alert notifications ---
  const toOrgId = (v: unknown): number | null => (v === null || v === undefined ? null : Number(v));
  const toPolicyId = (v: unknown): number | null => (v === null || v === undefined ? null : Number(v));
  if (p === '/api/v1/notify/channels' && method === 'GET') return ok({ channels: notifyChannels });
  if (p === '/api/v1/notify/channels' && method === 'POST') {
    const rr = needRole('operator'); if (rr) return rr;
    const name = String(b.name ?? '').trim();
    const kind = String(b.kind ?? '');
    if (!name) return err(400, 'invalid_request', 'name is required');
    if (kind !== 'email' && kind !== 'webhook' && kind !== 'sms') return err(400, 'invalid_request', 'kind must be email, webhook or sms');
    const org_id = toOrgId(b.org_id);
    const ch: NotifyChannel = {
      id: nextId++, kind: kind as NotifyChannel['kind'], name, org_id,
      config: (b.config ?? {}) as NotifyChannel['config'],
      secret_set: typeof b.secret === 'string' && b.secret !== '', enabled: b.enabled !== false,
      created_at: iso(0), created_by: me.id,
    };
    notifyChannels.push(ch); log('notify.channel_created', 'notify_channel', ch.id, org_id, { kind, name });
    return ok({ channel: ch }, 201);
  }
  if ((x = m(/^\/api\/v1\/notify\/channels\/(\d+)$/))) {
    const rr = needRole('operator'); if (rr) return rr;
    const ch = notifyChannels.find((c) => c.id === Number(x![1])); if (!ch) return err(404, 'not_found');
    if (method === 'DELETE') {
      notifyChannels.splice(notifyChannels.indexOf(ch), 1); log('notify.channel_deleted', 'notify_channel', ch.id, ch.org_id);
      return ok(null, 204);
    }
    // PUT: omitting secret keeps it; "" is rejected; a value replaces it.
    const name = String(b.name ?? '').trim();
    const kind = String(b.kind ?? '');
    if (!name) return err(400, 'invalid_request', 'name is required');
    if (kind !== 'email' && kind !== 'webhook' && kind !== 'sms') return err(400, 'invalid_request', 'kind must be email, webhook or sms');
    // Kind is immutable on the real server (no kind column); reject a change here
    // too so the mock behaves the same (NS1 fix).
    if (kind !== ch.kind) return err(400, 'invalid_request', 'channel kind cannot be changed');
    if (b.secret !== undefined && b.secret !== null && String(b.secret) === '') return err(400, 'invalid_request', 'secret cannot be empty; omit it to keep the stored secret');
    ch.name = name; ch.config = (b.config ?? {}) as NotifyChannel['config'];
    ch.enabled = b.enabled !== false;
    if (typeof b.secret === 'string') ch.secret_set = true;
    log('notify.channel_updated', 'notify_channel', ch.id, ch.org_id, { kind, name, secret_replaced: typeof b.secret === 'string' });
    return ok({ channel: ch });
  }
  if ((x = m(/^\/api\/v1\/notify\/channels\/(\d+)\/test$/))) {
    const rr = needRole('operator'); if (rr) return rr;
    const ch = notifyChannels.find((c) => c.id === Number(x![1])); if (!ch) return err(404, 'not_found');
    // No dispatcher in the mock: simulate the send so the button shows a status.
    const del: NotificationDelivery = {
      id: nextId++, alert_id: null, rule_id: null, channel_id: ch.id, org_id: ch.org_id,
      event: 'test', status: ch.enabled ? 'sent' : 'failed', attempts: 1,
      last_error: ch.enabled ? '' : 'channel is disabled', detail: { kind: ch.kind, subject: `[MSP] test from ${ch.name}` },
      step: null, created_at: iso(0), updated_at: iso(0),
    };
    deliveries.unshift(del); log('notify.channel_tested', 'notify_channel', ch.id, ch.org_id, { delivery_id: del.id });
    return ok({ delivery: del }, 201);
  }
  if (p === '/api/v1/notify/rules' && method === 'GET') return ok({ rules: notifyRules });
  if (p === '/api/v1/notify/rules' && method === 'POST') {
    const rr = needRole('operator'); if (rr) return rr;
    const name = String(b.name ?? '').trim();
    const minSeverity = String(b.min_severity ?? '');
    const channelId = Number(b.channel_id ?? 0);
    if (!name) return err(400, 'invalid_request', 'name is required');
    if (!['low', 'medium', 'high'].includes(minSeverity)) return err(400, 'invalid_request', 'min_severity must be low, medium or high');
    if (channelId <= 0 || !notifyChannels.some((c) => c.id === channelId)) return err(400, 'invalid_request', 'channel_id not found');
    const policyId = toPolicyId(b.escalation_policy_id);
    if (policyId !== null && !escalationPolicies.some((pp) => pp.id === policyId)) return err(400, 'invalid_request', 'escalation_policy_id not found');
    const ru: NotifyRule = {
      id: nextId++, name, org_id: toOrgId(b.org_id), min_severity: minSeverity as NotifyRule['min_severity'],
      alert_types: Array.isArray(b.alert_types) ? b.alert_types.map(String) : [],
      on_open: b.on_open !== false, on_resolve: b.on_resolve === true, channel_id: channelId,
      escalation_policy_id: policyId, enabled: b.enabled !== false, created_at: iso(0), created_by: me.id,
    };
    notifyRules.push(ru); log('notify.rule_created', 'notify_rule', ru.id, ru.org_id, { name, channel_id: channelId });
    return ok({ rule: ru }, 201);
  }
  if ((x = m(/^\/api\/v1\/notify\/rules\/(\d+)$/))) {
    const rr = needRole('operator'); if (rr) return rr;
    const ru = notifyRules.find((r) => r.id === Number(x![1])); if (!ru) return err(404, 'not_found');
    if (method === 'DELETE') {
      notifyRules.splice(notifyRules.indexOf(ru), 1); log('notify.rule_deleted', 'notify_rule', ru.id, ru.org_id);
      return ok(null, 204);
    }
    const name = String(b.name ?? '').trim();
    const minSeverity = String(b.min_severity ?? '');
    const channelId = Number(b.channel_id ?? 0);
    if (!name) return err(400, 'invalid_request', 'name is required');
    if (!['low', 'medium', 'high'].includes(minSeverity)) return err(400, 'invalid_request', 'min_severity must be low, medium or high');
    if (channelId <= 0 || !notifyChannels.some((c) => c.id === channelId)) return err(400, 'invalid_request', 'channel_id not found');
    const policyId = toPolicyId(b.escalation_policy_id);
    if (policyId !== null && !escalationPolicies.some((pp) => pp.id === policyId)) return err(400, 'invalid_request', 'escalation_policy_id not found');
    ru.name = name; ru.min_severity = minSeverity as NotifyRule['min_severity'];
    ru.alert_types = Array.isArray(b.alert_types) ? b.alert_types.map(String) : [];
    ru.on_open = b.on_open !== false; ru.on_resolve = b.on_resolve === true; ru.channel_id = channelId;
    ru.escalation_policy_id = policyId; ru.enabled = b.enabled !== false;
    log('notify.rule_updated', 'notify_rule', ru.id, ru.org_id, { name, channel_id: channelId });
    return ok({ rule: ru });
  }
  // --- NS2 escalation policies ---
  const validateSteps = (raw: unknown): { steps: EscalationPolicy['steps'] } | Reply => {
    if (!Array.isArray(raw) || raw.length === 0) return err(400, 'invalid_request', 'a policy needs at least one step');
    if (raw.length > 10) return err(400, 'invalid_request', 'a policy may have at most 10 steps');
    const steps: EscalationPolicy['steps'] = [];
    let prev = -1;
    for (let i = 0; i < raw.length; i++) {
      const s = (raw[i] ?? {}) as Record<string, unknown>;
      const after = Number(s.after_minutes ?? 0);
      if (!Number.isInteger(after) || after < 0 || after > 40320) return err(400, 'invalid_request', `step ${i + 1}: after_minutes must be between 0 and 40320`);
      if (after < prev) return err(400, 'invalid_request', 'steps must be ordered by non-decreasing after_minutes');
      prev = after;
      const ids = Array.isArray(s.channel_ids) ? [...new Set(s.channel_ids.map(Number))] : [];
      if (ids.length === 0) return err(400, 'invalid_request', `step ${i + 1}: at least one channel is required`);
      if (!ids.every((id) => notifyChannels.some((c) => c.id === id))) return err(400, 'invalid_request', 'channel_id not found');
      steps.push({ after_minutes: after, channel_ids: ids });
    }
    return { steps };
  };
  if (p === '/api/v1/notify/policies' && method === 'GET') return ok({ policies: escalationPolicies });
  if (p === '/api/v1/notify/policies' && method === 'POST') {
    const rr = needRole('operator'); if (rr) return rr;
    const name = String(b.name ?? '').trim();
    if (!name) return err(400, 'invalid_request', 'name is required');
    const v = validateSteps(b.steps);
    if ('status' in v) return v;
    const pol: EscalationPolicy = { id: nextId++, name, org_id: toOrgId(b.org_id), steps: v.steps, created_at: iso(0), created_by: me.id };
    escalationPolicies.push(pol); log('notify.policy_created', 'escalation_policy', pol.id, pol.org_id, { name, steps: pol.steps.length });
    return ok({ policy: pol }, 201);
  }
  if ((x = m(/^\/api\/v1\/notify\/policies\/(\d+)$/))) {
    const rr = needRole('operator'); if (rr) return rr;
    const pol = escalationPolicies.find((pp) => pp.id === Number(x![1])); if (!pol) return err(404, 'not_found');
    if (method === 'DELETE') {
      escalationPolicies.splice(escalationPolicies.indexOf(pol), 1); log('notify.policy_deleted', 'escalation_policy', pol.id, pol.org_id);
      return ok(null, 204);
    }
    const name = String(b.name ?? '').trim();
    if (!name) return err(400, 'invalid_request', 'name is required');
    const v = validateSteps(b.steps);
    if ('status' in v) return v;
    pol.name = name; pol.steps = v.steps;
    log('notify.policy_updated', 'escalation_policy', pol.id, pol.org_id, { name, steps: pol.steps.length });
    return ok({ policy: pol });
  }
  if (p === '/api/v1/notify/windows' && method === 'GET') {
    return ok({ windows: maintenanceWindows.map((w) => ({ ...w, active_now: windowActiveNow(w) })) });
  }
  if (p === '/api/v1/notify/windows' && method === 'POST') {
    const rr = needRole('operator'); if (rr) return rr;
    const starts = String(b.starts_at ?? ''), ends = String(b.ends_at ?? '');
    if (Number.isNaN(Date.parse(starts))) return err(400, 'invalid_request', 'starts_at must be an RFC3339 timestamp');
    if (Number.isNaN(Date.parse(ends))) return err(400, 'invalid_request', 'ends_at must be an RFC3339 timestamp');
    if (Date.parse(ends) <= Date.parse(starts)) return err(400, 'invalid_request', 'ends_at must be after starts_at');
    const w: MaintenanceWindow = {
      id: nextId++, org_id: toOrgId(b.org_id), starts_at: new Date(starts).toISOString(), ends_at: new Date(ends).toISOString(),
      reason: String(b.reason ?? '').trim(), active_now: false, created_at: iso(0), created_by: me.id,
    };
    w.active_now = windowActiveNow(w);
    maintenanceWindows.push(w); log('notify.window_created', 'maintenance_window', w.id, w.org_id, { starts_at: w.starts_at, ends_at: w.ends_at });
    return ok({ window: w }, 201);
  }
  if ((x = m(/^\/api\/v1\/notify\/windows\/(\d+)$/)) && method === 'DELETE') {
    const rr = needRole('operator'); if (rr) return rr;
    const w = maintenanceWindows.find((w) => w.id === Number(x![1])); if (!w) return err(404, 'not_found');
    maintenanceWindows.splice(maintenanceWindows.indexOf(w), 1); log('notify.window_deleted', 'maintenance_window', w.id, w.org_id);
    return ok(null, 204);
  }
  if (p === '/api/v1/notify/deliveries') {
    const st = q.get('status') ?? '', orgId = Number(q.get('org_id') ?? 0), limit = Math.min(Number(q.get('limit') ?? 200), 1000);
    if (st && !['pending', 'sent', 'failed', 'suppressed'].includes(st)) return err(400, 'invalid_request', 'status must be pending, sent, failed or suppressed');
    return ok({ deliveries: deliveries.filter((d) => (!st || d.status === st) && (!orgId || d.org_id === orgId)).slice(0, limit) });
  }

  // --- AUT1 script library, bulk jobs and automations ---
  // The mock does not model per-user org scoping (like the rest of it); the real
  // server restricts every list to the caller's visible orgs and 404s a cross-org
  // id (RS2). A global (org_id null) script/automation needs all-orgs: the mock
  // enforces that for a non-admin role so &role=operator shows the gate.
  const needGlobalAdmin = (orgId: number | null): Reply | null =>
    orgId === null && me.role !== 'admin' ? err(403, 'forbidden', 'a global script needs access to all clients') : null;
  const scriptOrgId = (v: unknown): number | null => (v === null || v === undefined ? null : Number(v));
  if (p === '/api/v1/scripts' && method === 'GET') return ok({ scripts });
  if (p === '/api/v1/scripts' && method === 'POST') {
    const rr = needRole('operator'); if (rr) return rr;
    const org_id = scriptOrgId(b.org_id);
    const g = needGlobalAdmin(org_id); if (g) return g;
    const v = validateScriptBody(b); if ('status' in v) return v;
    const sc: Script = { id: nextId++, org_id, ...v.fields, created_at: iso(0), created_by: me.id };
    scripts.unshift(sc); log('script.created', 'script', sc.id, org_id, { name: sc.name, kind: sc.kind });
    return ok({ script: sc }, 201);
  }
  if ((x = m(/^\/api\/v1\/scripts\/(\d+)$/))) {
    const sc = scripts.find((s) => s.id === Number(x![1])); if (!sc) return err(404, 'not_found');
    if (method === 'DELETE') {
      const rr = needRole('operator'); if (rr) return rr;
      const g = needGlobalAdmin(sc.org_id); if (g) return g;
      scripts.splice(scripts.indexOf(sc), 1); log('script.deleted', 'script', sc.id, sc.org_id, { name: sc.name });
      return ok(null, 204);
    }
    const rr = needRole('operator'); if (rr) return rr;
    const org_id = scriptOrgId(b.org_id);
    const g = needGlobalAdmin(org_id) ?? needGlobalAdmin(sc.org_id); if (g) return g;
    const v = validateScriptBody(b); if ('status' in v) return v;
    sc.org_id = org_id; sc.name = v.fields.name; sc.description = v.fields.description;
    sc.platform = v.fields.platform; sc.kind = v.fields.kind; sc.argv = v.fields.argv;
    sc.shell = v.fields.shell; sc.body = v.fields.body; sc.timeout_s = v.fields.timeout_s;
    log('script.updated', 'script', sc.id, org_id, { name: sc.name, kind: sc.kind });
    return ok({ script: sc });
  }
  if (p === '/api/v1/jobs' && method === 'GET') {
    const orgId = Number(q.get('org_id') ?? 0);
    return ok({ jobs: jobs.filter((j) => !orgId || j.org_id === orgId) });
  }
  if (p === '/api/v1/jobs' && method === 'POST') {
    const rr = needRole('operator'); if (rr) return rr;
    const nj = (b ?? {}) as unknown as NewJob;
    const target = (nj.target ?? {}) as TargetSpec;
    let gate: { platform: string; kind: string };
    let fields: { kind: Command['kind']; argv: string[]; shell: string; body: string; timeout_s: number };
    let scriptId: number | null = null;
    let name = (nj.name ?? '').trim();
    if (nj.script_id) {
      const sc = scripts.find((s) => s.id === Number(nj.script_id)); if (!sc) return err(404, 'not_found');
      gate = { platform: sc.platform, kind: sc.kind };
      fields = { kind: sc.kind, argv: sc.argv, shell: sc.shell, body: sc.body, timeout_s: sc.timeout_s };
      scriptId = sc.id; if (!name) name = sc.name;
    } else {
      const kind = nj.kind === 'script' ? 'script' : 'exec';
      if (kind === 'exec' && !(Array.isArray(nj.argv) && nj.argv.length)) return err(400, 'invalid_request', 'exec needs argv with at least one element');
      if (kind === 'script' && !String(nj.body ?? '').trim()) return err(400, 'invalid_request', 'script needs a non-empty body');
      gate = { platform: 'any', kind };
      fields = { kind, argv: nj.argv ?? [], shell: nj.shell ?? '', body: nj.body ?? '', timeout_s: Number(nj.timeout_s) || 300 };
      if (!name) name = 'Bulk run';
    }
    const matched = resolveTarget(devices, target);
    if (matched.length === 0) return err(400, 'invalid_request', 'the target matches no visible device');
    const jobId = nextId++;
    const rows: JobDeviceRow[] = [];
    let total = 0;
    for (const d of matched) {
      const base = { device_id: d.id, hostname: d.hostname, org_id: d.org_id, org_name: d.org_name };
      if (d.revoked) { rows.push({ ...base, command_id: null, status: null, skipped: true, note: 'device is revoked' }); continue; }
      const note = scriptSkipNote(gate, d);
      if (note) { rows.push({ ...base, command_id: null, status: null, skipped: true, note }); continue; }
      const c: Command = {
        id: nextId++, device_id: d.id, org_id: d.org_id, created_by: me.id, created_by_email: me.email, kind: fields.kind,
        argv: fields.argv, shell: fields.shell, body: fields.body, text: '', timeout_s: fields.timeout_s, status: 'queued',
        created_at: iso(0), sent_at: null, started_at: null, finished_at: null, exit_code: null, stdout: '', stderr: '', truncated: false, error: '',
        job_id: jobId,
      };
      commands.unshift(c); total++;
      rows.push({ ...base, command_id: c.id, status: 'queued', skipped: false, note: '' });
      if (d.status !== 'offline') setTimeout(() => finish(c), 400); // offline stays queued
    }
    const org_id = target.org_id ?? null;
    const job: Job = { id: jobId, org_id, name, origin: 'manual', script_id: scriptId, created_by: me.id, automation_id: null, created_at: iso(0), total };
    jobs.unshift(job); jobDeviceRows[jobId] = rows;
    log('job.created', 'job', jobId, org_id, { origin: 'manual', script_id: scriptId, total });
    return ok({ job_id: jobId, total }, 201);
  }
  if ((x = m(/^\/api\/v1\/jobs\/(\d+)$/))) {
    const job = jobs.find((j) => j.id === Number(x![1])); if (!job) return err(404, 'not_found');
    return ok({ job, progress: jobProgressOf(job.id, job.total), devices: jobRowsLive(job.id) });
  }
  if (p === '/api/v1/automations' && method === 'GET') return ok({ automations });
  if (p === '/api/v1/automations' && method === 'POST') {
    const rr = needRole('operator'); if (rr) return rr;
    const org_id = scriptOrgId(b.org_id);
    const g = needGlobalAdmin(org_id); if (g) return g;
    const v = validateAutomationBody(b); if ('status' in v) return v;
    const a: Automation = {
      id: nextId++, org_id, name: v.fields.name, enabled: v.fields.enabled, trigger: v.fields.trigger,
      script_id: v.fields.script_id, target: v.fields.target, schedule: v.fields.schedule, alert: v.fields.alert,
      last_run_at: null, next_run_at: v.fields.trigger === 'schedule' && v.fields.schedule?.every_minutes ? iso(-v.fields.schedule.every_minutes * 60e3) : null,
      created_at: iso(0), created_by: me.id,
    };
    automations.unshift(a); log('automation.created', 'automation', a.id, org_id, { name: a.name, trigger: a.trigger });
    return ok({ automation: a }, 201);
  }
  if ((x = m(/^\/api\/v1\/automations\/(\d+)\/enable$/))) {
    const rr = needRole('operator'); if (rr) return rr;
    const a = automations.find((a) => a.id === Number(x![1])); if (!a) return err(404, 'not_found');
    const g = needGlobalAdmin(a.org_id); if (g) return g;
    a.enabled = b.enabled !== false;
    log('automation.enabled_changed', 'automation', a.id, a.org_id, { enabled: a.enabled });
    return ok(null, 204);
  }
  if ((x = m(/^\/api\/v1\/automations\/(\d+)$/))) {
    const a = automations.find((a) => a.id === Number(x![1])); if (!a) return err(404, 'not_found');
    if (method === 'DELETE') {
      const rr = needRole('operator'); if (rr) return rr;
      const g = needGlobalAdmin(a.org_id); if (g) return g;
      automations.splice(automations.indexOf(a), 1); log('automation.deleted', 'automation', a.id, a.org_id, { name: a.name });
      return ok(null, 204);
    }
    const rr = needRole('operator'); if (rr) return rr;
    const org_id = scriptOrgId(b.org_id);
    const g = needGlobalAdmin(org_id) ?? needGlobalAdmin(a.org_id); if (g) return g;
    const v = validateAutomationBody(b); if ('status' in v) return v;
    a.org_id = org_id; a.name = v.fields.name; a.enabled = v.fields.enabled; a.trigger = v.fields.trigger;
    a.script_id = v.fields.script_id; a.target = v.fields.target; a.schedule = v.fields.schedule; a.alert = v.fields.alert;
    a.next_run_at = v.fields.trigger === 'schedule' && v.fields.schedule?.every_minutes ? iso(-v.fields.schedule.every_minutes * 60e3) : null;
    log('automation.updated', 'automation', a.id, org_id, { name: a.name, trigger: a.trigger });
    return ok({ automation: a });
  }

  // --- RP1 reports and exports (read-only, RS2 org-scoped) ---
  // The mock does not model per-user org scoping (like the rest of it); the real
  // server restricts every report to the caller's visible orgs and 404s a cross-org
  // org_id/device_id (RS2). It honours the org_id filter and the retention-bounded
  // `days` so all three reports render under ?mock=1. The .csv exports are not routed
  // here: the dashboard builds them client-side from the loaded report when MOCK.
  const RETENTION_DAYS = 7;
  const reportDays = (): number | Reply => {
    const raw = q.get('days');
    if (raw === null || raw === '') return RETENTION_DAYS;
    const n = Number(raw);
    if (!Number.isInteger(n) || n < 1 || n > RETENTION_DAYS) return err(400, 'invalid_request', `days must be between 1 and ${RETENTION_DAYS}`);
    return n;
  };
  const orgExists = (orgId: number): boolean => !orgId || orgs.some((o) => o.id === orgId);
  if (p === '/api/v1/reports/uptime') {
    const days = reportDays(); if (typeof days !== 'number') return days;
    const orgId = Number(q.get('org_id') ?? 0);
    if (!orgExists(orgId)) return err(404, 'not_found');
    const rows: UptimeRow[] = devices
      .filter((d) => !orgId || d.org_id === orgId)
      .map((d) => {
        const expected = expectedWindows(days, d.interval_s, Date.parse(d.enrolled_at));
        // Seed plausible check-in coverage: online near-complete, offline partial,
        // pending (never synced) zero. (The 5-day-ago enrollment is prorated against
        // a longer window, so expected < days*windows demonstrates the proration.)
        const factor = d.status === 'online' ? 0.995 : d.status === 'offline' ? 0.3 : 0;
        const samples = Math.min(expected, Math.round(expected * factor));
        return {
          device_id: d.id, hostname: d.hostname, org_id: d.org_id, org_name: d.org_name,
          samples, expected, uptime_pct: uptimePct(samples, expected), uptime_s: d.uptime_s,
        };
      });
    return ok({ days, rows, mean_uptime_pct: meanUptime(rows), device_count: rows.length });
  }
  if (p === '/api/v1/reports/disk/top') {
    const days = reportDays(); if (typeof days !== 'number') return days;
    const orgId = Number(q.get('org_id') ?? 0);
    if (!orgExists(orgId)) return err(404, 'not_found');
    const limit = Math.min(Math.max(Number(q.get('limit') ?? 10) || 10, 1), 100);
    const rows: DiskTopRow[] = devices
      .filter((d) => (!orgId || d.org_id === orgId) && d.disk_used_pct !== null)
      .map((d) => ({ device_id: d.id, hostname: d.hostname, org_id: d.org_id, org_name: d.org_name, latest: d.disk_used_pct }))
      .sort((a, b) => (b.latest ?? 0) - (a.latest ?? 0))
      .slice(0, limit);
    return ok({ days, rows });
  }
  if (p === '/api/v1/reports/disk') {
    const days = reportDays(); if (typeof days !== 'number') return days;
    const d = devices.find((x) => x.id === Number(q.get('device_id') ?? 0));
    if (!d) return err(404, 'not_found');
    const n = d.status === 'pending' ? 0 : 120;
    const spanMs = days * 86400e3;
    const base = d.disk_used_pct ?? 40;
    // A gently rising series ending at the device's current disk_used_pct, oldest first.
    const series: DiskPoint[] = Array.from({ length: n }, (_, i) => ({
      at: iso(((n - i) / n) * spanMs),
      disk_used_pct: Math.round((base - (n - i) * 0.02 + 2 * Math.sin(i / 9)) * 10) / 10,
    }));
    return ok({ device_id: d.id, hostname: d.hostname, org_id: d.org_id, org_name: d.org_name, days, series, ...seriesStats(series) });
  }
  if (p === '/api/v1/reports/patch-compliance') {
    const orgId = Number(q.get('org_id') ?? 0);
    if (!orgExists(orgId)) return err(404, 'not_found');
    const scoped = devices.filter((d) => !orgId || d.org_id === orgId);
    const byOrg = new Map<number, Device[]>();
    for (const d of scoped) { const a = byOrg.get(d.org_id) ?? []; a.push(d); byOrg.set(d.org_id, a); }
    const orgRows = [...byOrg.entries()]
      .map(([oid, ds]) => ({ org_id: oid, org_name: ds[0].org_name, counts: bucketDevices(ds) }))
      .sort((a, b) => a.org_name.localeCompare(b.org_name));
    const deviceRows: PatchComplianceRow[] = scoped.map((d) => ({
      device_id: d.id, hostname: d.hostname, org_id: d.org_id, org_name: d.org_name,
      pending: d.pending_patches, security: d.security_patches, reboot_required: d.reboot_required, checked_at: d.patches_checked_at,
    }));
    return ok({ overall: bucketDevices(scoped), orgs: orgRows, devices: deviceRows });
  }

  return err(404, 'not_found');
}

// Validate a script create/update body (mock): the shape the server checks.
// Returns the normalised fields or an error Reply.
type ScriptFields = Omit<Script, 'id' | 'org_id' | 'created_at' | 'created_by'>;
function validateScriptBody(b: Record<string, unknown>): { fields: ScriptFields } | Reply {
  const nb = b as unknown as NewScript;
  const name = String(nb.name ?? '').trim();
  const platform = String(nb.platform ?? '');
  const kind = String(nb.kind ?? '');
  if (!name) return err(400, 'invalid_request', 'a name is required');
  if (!['linux', 'windows', 'any'].includes(platform)) return err(400, 'invalid_request', 'platform must be linux, windows or any');
  if (!['exec', 'script'].includes(kind)) return err(400, 'invalid_request', 'kind must be exec or script');
  const argv = Array.isArray(nb.argv) ? nb.argv.map(String).filter((a) => a.trim() !== '') : [];
  const shell = String(nb.shell ?? '');
  const body = String(nb.body ?? '');
  if (kind === 'exec' && argv.length === 0) return err(400, 'invalid_request', 'an exec script needs a non-empty argv');
  if (kind === 'script' && !shell.trim()) return err(400, 'invalid_request', 'a script needs a shell');
  if (kind === 'script' && !body.trim()) return err(400, 'invalid_request', 'a script needs a non-empty body');
  const timeout = Number(nb.timeout_s);
  if (!Number.isInteger(timeout) || timeout < 1 || timeout > 86400) return err(400, 'invalid_request', 'timeout_s must be 1..86400');
  return { fields: {
    name, description: String(nb.description ?? ''), platform: platform as Script['platform'], kind: kind as Script['kind'],
    argv: kind === 'exec' ? argv : [], shell: kind === 'script' ? shell : '', body: kind === 'script' ? body : '', timeout_s: timeout,
  } };
}

// Validate an automation create/update body (mock).
type AutomationFields = Pick<Automation, 'name' | 'enabled' | 'trigger' | 'script_id' | 'target' | 'schedule' | 'alert'>;
function validateAutomationBody(b: Record<string, unknown>): { fields: AutomationFields } | Reply {
  const nb = b as unknown as NewAutomation;
  const name = String(nb.name ?? '').trim();
  const trigger = String(nb.trigger ?? '');
  if (!name) return err(400, 'invalid_request', 'a name is required');
  if (!['schedule', 'alert'].includes(trigger)) return err(400, 'invalid_request', 'trigger must be schedule or alert');
  const scriptId = Number(nb.script_id ?? 0);
  if (!scriptId || !scripts.some((s) => s.id === scriptId)) return err(404, 'not_found');
  const target = (nb.target ?? {}) as TargetSpec;
  let schedule: Automation['schedule'] = null;
  let alert: Automation['alert'] = null;
  if (trigger === 'schedule') {
    const every = Number(nb.schedule?.every_minutes ?? 0);
    const cron = String(nb.schedule?.cron ?? '').trim();
    if (!cron) {
      if (!Number.isInteger(every) || every < 1 || every > 40320) return err(400, 'invalid_request', 'schedule every_minutes must be 1..40320');
      schedule = { every_minutes: every };
    } else {
      schedule = { cron };
    }
  } else {
    const at = String(nb.alert?.alert_type ?? '').trim();
    const sev = String(nb.alert?.min_severity ?? '').trim();
    if (sev && !['low', 'medium', 'high'].includes(sev)) return err(400, 'invalid_request', 'min_severity must be low, medium or high');
    alert = { ...(at ? { alert_type: at } : {}), ...(sev ? { min_severity: sev } : {}) } as NonNullable<Automation['alert']>;
  }
  return { fields: { name, enabled: nb.enabled !== false, trigger: trigger as Automation['trigger'], script_id: scriptId, target, schedule, alert } };
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
