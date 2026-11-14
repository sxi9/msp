import { Fragment } from 'preact';
import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { when } from '../lib/format.ts';
import { channelTarget, describeRule, secretLabel, windowState } from '../lib/notify.ts';
import { emptyStep, formatDelay, policySummary, sortSteps, validateSteps } from '../lib/escalation.ts';
import { accessOf } from '../lib/orgscope.ts';
import {
  ALERT_TYPES, canOperate, type ChannelUpdate, type DeliveryStatus, type EmailConfig,
  type EscalationPolicy, type EscalationStep, type MaintenanceWindow, type NewChannel, type NewPolicy,
  type NewRule, type NewWindow, type NotifyChannel, type NotifyKind, type NotifyRule, type Org,
  type PolicyUpdate, type RuleUpdate, type Severity, type SmsConfig, type SmsProvider, type User,
  type WebhookConfig,
} from '../types.ts';
import { ConfirmButton } from '../components/Confirm.tsx';
import { DeliveryStatusChip } from '../components/StatusChip.tsx';

const SEVERITIES: Severity[] = ['low', 'medium', 'high'];

// A channel/rule/window org is null for a global row usable by every client.
const orgLabel = (orgId: number | null, orgs: Org[]): string =>
  orgId === null ? 'All clients' : orgs.find((o) => o.id === orgId)?.name ?? `#${orgId}`;

// The channel's address for the table: recipients for sms (no secret), else the
// shared channelTarget (email recipients / redacted webhook url).
const channelAddress = (c: NotifyChannel): string => {
  if (c.kind === 'sms') {
    const to = (c.config as SmsConfig).to;
    return Array.isArray(to) ? to.join(', ') : '';
  }
  return channelTarget(c);
};

// Select state: '' means the global (null) org, else the numeric id as a string.
const orgIdOf = (v: string): number | null => (v === '' ? null : Number(v));

function OrgSelect({ value, canGlobal, orgs, disabled, onChange }: {
  value: string; canGlobal: boolean; orgs: Org[]; disabled?: boolean; onChange: (v: string) => void;
}) {
  return (
    <select value={value} disabled={disabled} onChange={(e) => onChange(e.currentTarget.value)}>
      {canGlobal && <option value="">All clients (global)</option>}
      {orgs.map((o) => <option key={o.id} value={String(o.id)}>{o.name}</option>)}
    </select>
  );
}

export function Notifications({ user }: { user: User }) {
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0);
  const channels = usePoll(() => api.listNotifyChannels().then((r) => r.channels), [], 0);
  const rules = usePoll(() => api.listNotifyRules().then((r) => r.rules), [], 0);
  const policies = usePoll(() => api.listNotifyPolicies().then((r) => r.policies), [], 0);
  const windows = usePoll(() => api.listMaintenanceWindows().then((r) => r.windows), [], 20000);
  const orgList = orgs.data ?? [];
  const chList = channels.data ?? [];
  const polList = policies.data ?? [];
  const canGlobal = accessOf(user).all;
  const writable = canOperate(user);

  return (
    <section>
      <div class="toolbar">
        <h1>Notifications</h1>
        <a class="btn btn-sm" href="#/deliveries">Delivery log</a>
      </div>
      {!writable && <p class="muted">You have read-only access. Managing channels, rules and windows needs an operator role.</p>}

      <ChannelsPanel
        channels={channels.data} error={channels.error} orgs={orgList} canGlobal={canGlobal}
        writable={writable} onChanged={channels.reload}
      />
      <RulesPanel
        rules={rules.data} error={rules.error} orgs={orgList} channels={chList} policies={polList}
        canGlobal={canGlobal} writable={writable} onChanged={rules.reload}
      />
      <PoliciesPanel
        policies={policies.data} error={policies.error} orgs={orgList} channels={chList} canGlobal={canGlobal}
        writable={writable} onChanged={() => { policies.reload(); rules.reload(); }}
      />
      <WindowsPanel
        windows={windows.data} error={windows.error} orgs={orgList} canGlobal={canGlobal}
        writable={writable} onChanged={windows.reload}
      />
    </section>
  );
}

// --- channels ---------------------------------------------------------------

function ChannelsPanel({ channels, error, orgs, canGlobal, writable, onChanged }: {
  channels: NotifyChannel[] | null; error: string | null; orgs: Org[]; canGlobal: boolean;
  writable: boolean; onChanged: () => void;
}) {
  const [adding, setAdding] = useState(false);
  const [editId, setEditId] = useState(0);
  // Per-channel "Send test" result (a delivery status or an error message).
  const [tested, setTested] = useState<Record<number, { status: DeliveryStatus } | { error: string }>>({});

  const sendTest = async (id: number) => {
    setTested((t) => ({ ...t, [id]: { status: 'pending' } }));
    try {
      const r = await api.testNotifyChannel(id);
      setTested((t) => ({ ...t, [id]: { status: r.delivery.status } }));
    } catch (e) {
      setTested((t) => ({ ...t, [id]: { error: errorText(e) } }));
    }
  };
  const del = async (id: number) => { await api.deleteNotifyChannel(id); onChanged(); };

  return (
    <>
      <div class="toolbar">
        <h2>Channels</h2>
        {writable && <button type="button" class="btn btn-sm" onClick={() => { setAdding((a) => !a); setEditId(0); }}>{adding ? 'Close' : 'Add channel'}</button>}
      </div>
      {adding && (
        <ChannelForm orgs={orgs} canGlobal={canGlobal} onCancel={() => setAdding(false)} onDone={() => { setAdding(false); onChanged(); }} />
      )}
      {error && <p class="error">{error}</p>}
      {channels && channels.length === 0 && <p class="empty">No channels yet. Add an email or webhook channel to route alerts.</p>}
      {channels && channels.length > 0 && (
        <table class="grid">
          <thead><tr><th>Name</th><th>Kind</th><th>Client</th><th>Target</th><th>Secret</th><th>State</th><th></th></tr></thead>
          <tbody>
            {channels.map((c) => (
              <Fragment key={c.id}>
                <tr class={c.enabled ? '' : 'muted'}>
                  <td data-label="Name">{c.name}</td>
                  <td data-label="Kind"><span class="chip">{c.kind}</span></td>
                  <td data-label="Client">{orgLabel(c.org_id, orgs)}</td>
                  <td data-label="Target" class="access-names">{channelAddress(c) || '—'}</td>
                  <td data-label="Secret" class={c.secret_set ? '' : 'muted'}>{secretLabel(c.secret_set)}</td>
                  <td data-label="State">{c.enabled ? 'enabled' : 'disabled'}</td>
                  <td data-label="">
                    <span class="row-actions">
                      {writable && <button type="button" class="btn btn-sm" onClick={() => { setEditId(editId === c.id ? 0 : c.id); setAdding(false); }}>{editId === c.id ? 'Close' : 'Edit'}</button>}
                      {writable && <button type="button" class="btn btn-sm" onClick={() => sendTest(c.id)}>Send test</button>}
                      {tested[c.id] && ('status' in tested[c.id]
                        ? <DeliveryStatusChip status={(tested[c.id] as { status: DeliveryStatus }).status} />
                        : <span class="error">{(tested[c.id] as { error: string }).error}</span>)}
                      {writable && <ConfirmButton label="Delete" question={`Delete channel ${c.name}?`} danger onConfirm={() => del(c.id)} />}
                    </span>
                  </td>
                </tr>
                {editId === c.id && (
                  <tr class="expanded"><td colSpan={7}>
                    <ChannelForm channel={c} orgs={orgs} canGlobal={canGlobal} onCancel={() => setEditId(0)} onDone={() => { setEditId(0); onChanged(); }} />
                  </td></tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function ChannelForm({ channel, orgs, canGlobal, onDone, onCancel }: {
  channel?: NotifyChannel; orgs: Org[]; canGlobal: boolean; onDone: () => void; onCancel: () => void;
}) {
  const editing = !!channel;
  const cfg = (channel?.config ?? {}) as Partial<EmailConfig & WebhookConfig & SmsConfig>;
  const [kind, setKind] = useState<NotifyKind>(channel?.kind ?? 'email');
  const [name, setName] = useState(channel?.name ?? '');
  const [orgSel, setOrgSel] = useState(channel ? (channel.org_id === null ? '' : String(channel.org_id)) : (canGlobal ? '' : orgs[0] ? String(orgs[0].id) : ''));
  const [enabled, setEnabled] = useState(channel?.enabled ?? true);
  // email config
  const [host, setHost] = useState(cfg.host ?? '');
  const [port, setPort] = useState(String(cfg.port ?? 587));
  const [from, setFrom] = useState(cfg.from ?? '');
  const [to, setTo] = useState((cfg.to ?? []).join(', '));
  const [starttls, setStarttls] = useState(cfg.starttls ?? true);
  const [username, setUsername] = useState(cfg.username ?? '');
  // webhook config
  const [url, setUrl] = useState(cfg.url ?? '');
  const [flavor, setFlavor] = useState<WebhookConfig['flavor']>(cfg.flavor ?? 'slack');
  // sms config (NS2); `from`/`to`/`url` state is shared with the other kinds.
  const [provider, setProvider] = useState<SmsProvider>((cfg.provider as SmsProvider) ?? 'twilio');
  const [accountSid, setAccountSid] = useState(cfg.account_sid ?? '');
  // secret (write-only): on edit, revealed only when replacing
  const [replaceSecret, setReplaceSecret] = useState(!editing);
  const [secret, setSecret] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const toList = (s: string) => s.split(',').map((v) => v.trim()).filter(Boolean);
  const buildConfig = (): EmailConfig | WebhookConfig | SmsConfig => {
    if (kind === 'email') {
      return { host: host.trim(), port: Number(port) || 0, from: from.trim(), to: toList(to), starttls, username: username.trim() };
    }
    if (kind === 'webhook') return { url: url.trim(), flavor };
    const sms: SmsConfig = { provider, from: from.trim(), to: toList(to) };
    if (provider === 'twilio') sms.account_sid = accountSid.trim();
    else sms.url = url.trim();
    return sms;
  };

  const submit = async (e: Event) => {
    e.preventDefault();
    setError('');
    // The secret is write-only: send it only when the operator typed one.
    if (editing && replaceSecret && secret === '') { setError('Enter a new secret, or leave "replace secret" off to keep the stored one.'); return; }
    setBusy(true);
    try {
      const config = buildConfig();
      if (editing) {
        const body: ChannelUpdate = { kind, name: name.trim(), config, enabled };
        if (replaceSecret && secret !== '') body.secret = secret;
        await api.updateNotifyChannel(channel.id, body);
      } else {
        const body: NewChannel = { kind, name: name.trim(), org_id: orgIdOf(orgSel), config, enabled };
        if (secret !== '') body.secret = secret;
        await api.createNotifyChannel(body);
      }
      onDone();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card" onSubmit={submit}>
      <div class="form-row">
        <label>Kind
          {/* Kind is immutable once a channel exists (the server rejects a change),
              so the select is fixed while editing. */}
          <select value={kind} disabled={busy || editing} onChange={(e) => setKind(e.currentTarget.value as NotifyKind)}>
            <option value="email">email</option><option value="webhook">webhook</option><option value="sms">sms</option>
          </select>
        </label>
        <input type="text" placeholder="name" required value={name} disabled={busy} onInput={(e) => setName(e.currentTarget.value)} />
        {!editing && <label>Client <OrgSelect value={orgSel} canGlobal={canGlobal} orgs={orgs} disabled={busy} onChange={setOrgSel} /></label>}
        <label><input type="checkbox" checked={enabled} disabled={busy} onChange={(e) => setEnabled(e.currentTarget.checked)} /> enabled</label>
      </div>
      {kind === 'email' ? (
        <div class="form-row">
          <input type="text" placeholder="smtp host" required value={host} disabled={busy} onInput={(e) => setHost(e.currentTarget.value)} />
          <input type="number" placeholder="port" min={1} max={65535} value={port} disabled={busy} onInput={(e) => setPort(e.currentTarget.value)} />
          <input type="text" placeholder="from address" required value={from} disabled={busy} onInput={(e) => setFrom(e.currentTarget.value)} />
          <input type="text" placeholder="to (comma separated)" required value={to} disabled={busy} onInput={(e) => setTo(e.currentTarget.value)} />
          <input type="text" placeholder="username (optional)" value={username} disabled={busy} onInput={(e) => setUsername(e.currentTarget.value)} />
          <label><input type="checkbox" checked={starttls} disabled={busy} onChange={(e) => setStarttls(e.currentTarget.checked)} /> STARTTLS</label>
        </div>
      ) : kind === 'webhook' ? (
        <div class="form-row">
          <input type="url" placeholder="https://hooks.slack.com/…" required value={url} disabled={busy} onInput={(e) => setUrl(e.currentTarget.value)} />
          <label>Flavor
            <select value={flavor} disabled={busy} onChange={(e) => setFlavor(e.currentTarget.value as WebhookConfig['flavor'])}>
              <option value="slack">slack</option><option value="generic">generic JSON</option>
            </select>
          </label>
        </div>
      ) : (
        <div class="form-row">
          <label>Provider
            <select value={provider} disabled={busy} onChange={(e) => setProvider(e.currentTarget.value as SmsProvider)}>
              <option value="twilio">Twilio</option><option value="generic">generic JSON</option>
            </select>
          </label>
          <input type="text" placeholder="from (+15551234567)" required value={from} disabled={busy} onInput={(e) => setFrom(e.currentTarget.value)} />
          <input type="text" placeholder="to numbers (comma separated)" required value={to} disabled={busy} onInput={(e) => setTo(e.currentTarget.value)} />
          {provider === 'twilio'
            ? <input type="text" placeholder="account SID (AC…)" required value={accountSid} disabled={busy} onInput={(e) => setAccountSid(e.currentTarget.value)} />
            : <input type="url" placeholder="https://sms-gateway.example.com/send" required value={url} disabled={busy} onInput={(e) => setUrl(e.currentTarget.value)} />}
        </div>
      )}
      <div class="form-row">
        {editing
          ? <label><input type="checkbox" checked={replaceSecret} disabled={busy} onChange={(e) => { setReplaceSecret(e.currentTarget.checked); if (!e.currentTarget.checked) setSecret(''); }} /> replace secret (currently {secretLabel(channel!.secret_set)})</label>
          : <span class="muted">Secret</span>}
        {(replaceSecret || !editing) && (
          <input type="password" placeholder={kind === 'email' ? 'SMTP password (optional)' : kind === 'sms' ? 'provider auth token (optional)' : 'signing secret (optional)'} autocomplete="new-password" value={secret} disabled={busy} onInput={(e) => setSecret(e.currentTarget.value)} />
        )}
      </div>
      <div class="form-row">
        <button type="submit" class="btn btn-sm btn-primary" disabled={busy}>{editing ? 'Save channel' : 'Create channel'}</button>
        <button type="button" class="btn btn-sm" disabled={busy} onClick={onCancel}>Cancel</button>
        {error && <span class="error">{error}</span>}
      </div>
    </form>
  );
}

// --- rules ------------------------------------------------------------------

function RulesPanel({ rules, error, orgs, channels, policies, canGlobal, writable, onChanged }: {
  rules: NotifyRule[] | null; error: string | null; orgs: Org[]; channels: NotifyChannel[];
  policies: EscalationPolicy[]; canGlobal: boolean; writable: boolean; onChanged: () => void;
}) {
  const [adding, setAdding] = useState(false);
  const [editId, setEditId] = useState(0);
  const chName = (id: number) => channels.find((c) => c.id === id)?.name ?? `#${id}`;
  const polName = (id: number) => policies.find((p) => p.id === id)?.name ?? `#${id}`;
  const del = async (id: number) => { await api.deleteNotifyRule(id); onChanged(); };

  return (
    <>
      <div class="toolbar">
        <h2>Rules</h2>
        {writable && <button type="button" class="btn btn-sm" disabled={channels.length === 0} title={channels.length === 0 ? 'Add a channel first' : ''} onClick={() => { setAdding((a) => !a); setEditId(0); }}>{adding ? 'Close' : 'Add rule'}</button>}
      </div>
      {adding && <RuleForm orgs={orgs} channels={channels} policies={policies} canGlobal={canGlobal} onCancel={() => setAdding(false)} onDone={() => { setAdding(false); onChanged(); }} />}
      {error && <p class="error">{error}</p>}
      {rules && rules.length === 0 && <p class="empty">No rules yet. A rule routes matching alerts to a channel.</p>}
      {rules && rules.length > 0 && (
        <table class="grid">
          <thead><tr><th>Name</th><th>Client</th><th>Channel</th><th>Matches</th><th>State</th><th></th></tr></thead>
          <tbody>
            {rules.map((ru) => (
              <Fragment key={ru.id}>
                <tr class={ru.enabled ? '' : 'muted'}>
                  <td data-label="Name">{ru.name}</td>
                  <td data-label="Client">{orgLabel(ru.org_id, orgs)}</td>
                  <td data-label="Channel">{chName(ru.channel_id)}{ru.escalation_policy_id !== null && <span class="chip step-chip" title={`escalation policy: ${polName(ru.escalation_policy_id)}`}>+ escalates</span>}</td>
                  <td data-label="Matches" class="access-names">{describeRule(ru)}</td>
                  <td data-label="State">{ru.enabled ? 'enabled' : 'disabled'}</td>
                  <td data-label="">
                    <span class="row-actions">
                      {writable && <button type="button" class="btn btn-sm" onClick={() => { setEditId(editId === ru.id ? 0 : ru.id); setAdding(false); }}>{editId === ru.id ? 'Close' : 'Edit'}</button>}
                      {writable && <ConfirmButton label="Delete" question={`Delete rule ${ru.name}?`} danger onConfirm={() => del(ru.id)} />}
                    </span>
                  </td>
                </tr>
                {editId === ru.id && (
                  <tr class="expanded"><td colSpan={6}>
                    <RuleForm rule={ru} orgs={orgs} channels={channels} policies={policies} canGlobal={canGlobal} onCancel={() => setEditId(0)} onDone={() => { setEditId(0); onChanged(); }} />
                  </td></tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function RuleForm({ rule, orgs, channels, policies, canGlobal, onDone, onCancel }: {
  rule?: NotifyRule; orgs: Org[]; channels: NotifyChannel[]; policies: EscalationPolicy[];
  canGlobal: boolean; onDone: () => void; onCancel: () => void;
}) {
  const editing = !!rule;
  const [name, setName] = useState(rule?.name ?? '');
  const [orgSel, setOrgSel] = useState(rule ? (rule.org_id === null ? '' : String(rule.org_id)) : (canGlobal ? '' : orgs[0] ? String(orgs[0].id) : ''));
  const [minSeverity, setMinSeverity] = useState<Severity>(rule?.min_severity ?? 'medium');
  const [types, setTypes] = useState<Set<string>>(new Set(rule?.alert_types ?? []));
  const [onOpen, setOnOpen] = useState(rule?.on_open ?? true);
  const [onResolve, setOnResolve] = useState(rule?.on_resolve ?? false);
  const [channelId, setChannelId] = useState(rule?.channel_id ?? channels[0]?.id ?? 0);
  // '' = no escalation policy, else the policy id as a string.
  const [policySel, setPolicySel] = useState(rule?.escalation_policy_id != null ? String(rule.escalation_policy_id) : '');
  const [enabled, setEnabled] = useState(rule?.enabled ?? true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const toggleType = (t: string, on: boolean) =>
    setTypes((s) => { const n = new Set(s); if (on) n.add(t); else n.delete(t); return n; });

  const submit = async (e: Event) => {
    e.preventDefault();
    setError('');
    setBusy(true);
    try {
      const alertTypes = ALERT_TYPES.filter((t) => types.has(t));
      const policyId = policySel === '' ? null : Number(policySel);
      if (editing) {
        const body: RuleUpdate = { name: name.trim(), min_severity: minSeverity, alert_types: alertTypes, on_open: onOpen, on_resolve: onResolve, channel_id: channelId, escalation_policy_id: policyId, enabled };
        await api.updateNotifyRule(rule.id, body);
      } else {
        const body: NewRule = { name: name.trim(), org_id: orgIdOf(orgSel), min_severity: minSeverity, alert_types: alertTypes, on_open: onOpen, on_resolve: onResolve, channel_id: channelId, escalation_policy_id: policyId, enabled };
        await api.createNotifyRule(body);
      }
      onDone();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card" onSubmit={submit}>
      <div class="form-row">
        <input type="text" placeholder="rule name" required value={name} disabled={busy} onInput={(e) => setName(e.currentTarget.value)} />
        {!editing && <label>Client <OrgSelect value={orgSel} canGlobal={canGlobal} orgs={orgs} disabled={busy} onChange={setOrgSel} /></label>}
        <label>Channel
          <select value={channelId} disabled={busy} onChange={(e) => setChannelId(Number(e.currentTarget.value))}>
            {channels.map((c) => <option key={c.id} value={c.id}>{c.name} ({orgLabel(c.org_id, orgs)})</option>)}
          </select>
        </label>
        <label>Min severity
          <select value={minSeverity} disabled={busy} onChange={(e) => setMinSeverity(e.currentTarget.value as Severity)}>
            {SEVERITIES.map((s) => <option key={s} value={s}>{s}</option>)}
          </select>
        </label>
      </div>
      <div class="form-row">
        <label><input type="checkbox" checked={onOpen} disabled={busy} onChange={(e) => setOnOpen(e.currentTarget.checked)} /> on open</label>
        <label><input type="checkbox" checked={onResolve} disabled={busy} onChange={(e) => setOnResolve(e.currentTarget.checked)} /> on resolve</label>
        <span class="muted">Alert types (none = all):</span>
        {ALERT_TYPES.map((t) => (
          <label key={t}><input type="checkbox" checked={types.has(t)} disabled={busy} onChange={(e) => toggleType(t, e.currentTarget.checked)} /> {t}</label>
        ))}
      </div>
      <div class="form-row">
        <label>Escalation policy
          <select value={policySel} disabled={busy} onChange={(e) => setPolicySel(e.currentTarget.value)}>
            <option value="">none</option>
            {policies.filter((p) => p.org_id === null || p.org_id === orgIdOf(orgSel)).map((p) => (
              <option key={p.id} value={String(p.id)}>{p.name} ({policySummary(p)})</option>
            ))}
          </select>
        </label>
        <span class="muted">A policy adds timed escalation steps on open, on top of the channel above.</span>
      </div>
      <div class="form-row">
        <label><input type="checkbox" checked={enabled} disabled={busy} onChange={(e) => setEnabled(e.currentTarget.checked)} /> enabled</label>
        <span class="muted">Matches: {describeRule({ min_severity: minSeverity, alert_types: ALERT_TYPES.filter((t) => types.has(t)), on_open: onOpen, on_resolve: onResolve })}</span>
      </div>
      <div class="form-row">
        <button type="submit" class="btn btn-sm btn-primary" disabled={busy || channelId === 0}>{editing ? 'Save rule' : 'Create rule'}</button>
        <button type="button" class="btn btn-sm" disabled={busy} onClick={onCancel}>Cancel</button>
        {error && <span class="error">{error}</span>}
      </div>
    </form>
  );
}

// --- escalation policies (NS2) ----------------------------------------------

function PoliciesPanel({ policies, error, orgs, channels, canGlobal, writable, onChanged }: {
  policies: EscalationPolicy[] | null; error: string | null; orgs: Org[]; channels: NotifyChannel[];
  canGlobal: boolean; writable: boolean; onChanged: () => void;
}) {
  const [adding, setAdding] = useState(false);
  const [editId, setEditId] = useState(0);
  const del = async (id: number) => { await api.deleteNotifyPolicy(id); onChanged(); };

  return (
    <>
      <div class="toolbar">
        <h2>Escalation policies</h2>
        {writable && <button type="button" class="btn btn-sm" disabled={channels.length === 0} title={channels.length === 0 ? 'Add a channel first' : ''} onClick={() => { setAdding((a) => !a); setEditId(0); }}>{adding ? 'Close' : 'Add policy'}</button>}
      </div>
      {adding && <PolicyForm orgs={orgs} channels={channels} canGlobal={canGlobal} onCancel={() => setAdding(false)} onDone={() => { setAdding(false); onChanged(); }} />}
      {error && <p class="error">{error}</p>}
      {policies && policies.length === 0 && <p class="empty">No escalation policies. A policy fires extra channels over time while an alert stays open and unacknowledged; attach one to a rule.</p>}
      {policies && policies.length > 0 && (
        <table class="grid">
          <thead><tr><th>Name</th><th>Client</th><th>Steps</th><th></th></tr></thead>
          <tbody>
            {policies.map((p) => (
              <Fragment key={p.id}>
                <tr>
                  <td data-label="Name">{p.name}</td>
                  <td data-label="Client">{orgLabel(p.org_id, orgs)}</td>
                  <td data-label="Steps" class="access-names">{policySummary(p)}</td>
                  <td data-label="">
                    <span class="row-actions">
                      {writable && <button type="button" class="btn btn-sm" onClick={() => { setEditId(editId === p.id ? 0 : p.id); setAdding(false); }}>{editId === p.id ? 'Close' : 'Edit'}</button>}
                      {writable && <ConfirmButton label="Delete" question={`Delete policy ${p.name}?`} danger onConfirm={() => del(p.id)} />}
                    </span>
                  </td>
                </tr>
                {editId === p.id && (
                  <tr class="expanded"><td colSpan={4}>
                    <PolicyForm policy={p} orgs={orgs} channels={channels} canGlobal={canGlobal} onCancel={() => setEditId(0)} onDone={() => { setEditId(0); onChanged(); }} />
                  </td></tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function PolicyForm({ policy, orgs, channels, canGlobal, onDone, onCancel }: {
  policy?: EscalationPolicy; orgs: Org[]; channels: NotifyChannel[]; canGlobal: boolean; onDone: () => void; onCancel: () => void;
}) {
  const editing = !!policy;
  const [name, setName] = useState(policy?.name ?? '');
  const [orgSel, setOrgSel] = useState(policy ? (policy.org_id === null ? '' : String(policy.org_id)) : (canGlobal ? '' : orgs[0] ? String(orgs[0].id) : ''));
  const [steps, setSteps] = useState<EscalationStep[]>(
    policy ? policy.steps.map((s) => ({ after_minutes: s.after_minutes, channel_ids: [...s.channel_ids] })) : [emptyStep()],
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const policyOrg = orgIdOf(orgSel);
  // A policy's steps may only fire channels in its own client or global channels.
  const available = channels.filter((c) => c.org_id === null || c.org_id === policyOrg);

  const setStep = (i: number, patch: Partial<EscalationStep>) =>
    setSteps((ss) => ss.map((s, j) => (j === i ? { ...s, ...patch } : s)));
  const toggleStepChannel = (i: number, id: number, on: boolean) =>
    setSteps((ss) => ss.map((s, j) => (j === i ? { ...s, channel_ids: on ? [...s.channel_ids, id] : s.channel_ids.filter((x) => x !== id) } : s)));
  const addStep = () => setSteps((ss) => [...ss, emptyStep()]);
  const removeStep = (i: number) => setSteps((ss) => ss.filter((_, j) => j !== i));

  const submit = async (e: Event) => {
    e.preventDefault();
    setError('');
    const ordered = sortSteps(steps);
    const bad = validateSteps(ordered);
    if (bad) { setError(bad); return; }
    setBusy(true);
    try {
      if (editing) {
        await api.updateNotifyPolicy(policy.id, { name: name.trim(), steps: ordered } satisfies PolicyUpdate);
      } else {
        await api.createNotifyPolicy({ name: name.trim(), org_id: orgIdOf(orgSel), steps: ordered } satisfies NewPolicy);
      }
      onDone();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card" onSubmit={submit}>
      <div class="form-row">
        <input type="text" placeholder="policy name" required value={name} disabled={busy} onInput={(e) => setName(e.currentTarget.value)} />
        {!editing && <label>Client <OrgSelect value={orgSel} canGlobal={canGlobal} orgs={orgs} disabled={busy} onChange={(v) => { setOrgSel(v); setSteps((ss) => ss.map((s) => ({ ...s, channel_ids: [] }))); }} /></label>}
      </div>
      {steps.map((s, i) => (
        <div class="form-row step-row" key={i}>
          <span class="step-no">Step {i + 1}</span>
          <label>after
            <input type="number" min={0} max={40320} value={String(s.after_minutes)} disabled={busy} onInput={(e) => setStep(i, { after_minutes: Number(e.currentTarget.value) || 0 })} /> min
          </label>
          <span class="muted">({formatDelay(s.after_minutes)})</span>
          <span class="step-channels">
            {available.length === 0 && <span class="muted">no channels for this client</span>}
            {available.map((c) => (
              <label key={c.id}><input type="checkbox" checked={s.channel_ids.includes(c.id)} disabled={busy} onChange={(e) => toggleStepChannel(i, c.id, e.currentTarget.checked)} /> {c.name}</label>
            ))}
          </span>
          {steps.length > 1 && <button type="button" class="btn btn-sm" disabled={busy} onClick={() => removeStep(i)}>Remove</button>}
        </div>
      ))}
      <div class="form-row">
        <button type="button" class="btn btn-sm" disabled={busy || steps.length >= 10} onClick={addStep}>Add step</button>
        <span class="muted">{policySummary({ steps: sortSteps(steps) })}</span>
      </div>
      <div class="form-row">
        <button type="submit" class="btn btn-sm btn-primary" disabled={busy}>{editing ? 'Save policy' : 'Create policy'}</button>
        <button type="button" class="btn btn-sm" disabled={busy} onClick={onCancel}>Cancel</button>
        {error && <span class="error">{error}</span>}
      </div>
    </form>
  );
}

// --- maintenance windows ----------------------------------------------------

function WindowsPanel({ windows, error, orgs, canGlobal, writable, onChanged }: {
  windows: MaintenanceWindow[] | null; error: string | null; orgs: Org[]; canGlobal: boolean;
  writable: boolean; onChanged: () => void;
}) {
  const [adding, setAdding] = useState(false);
  const del = async (id: number) => { await api.deleteMaintenanceWindow(id); onChanged(); };

  return (
    <>
      <div class="toolbar">
        <h2>Maintenance windows</h2>
        {writable && <button type="button" class="btn btn-sm" onClick={() => setAdding((a) => !a)}>{adding ? 'Close' : 'Add window'}</button>}
      </div>
      {adding && <WindowForm orgs={orgs} canGlobal={canGlobal} onCancel={() => setAdding(false)} onDone={() => { setAdding(false); onChanged(); }} />}
      {error && <p class="error">{error}</p>}
      {windows && windows.length === 0 && <p class="empty">No maintenance windows. During a window, matching alerts are suppressed (logged, not sent).</p>}
      {windows && windows.length > 0 && (
        <table class="grid">
          <thead><tr><th>Client</th><th>Starts</th><th>Ends</th><th>Reason</th><th>State</th><th></th></tr></thead>
          <tbody>
            {windows.map((wnd) => {
              const st = windowState(wnd);
              return (
                <tr key={wnd.id}>
                  <td data-label="Client">{orgLabel(wnd.org_id, orgs)}</td>
                  <td data-label="Starts" title={wnd.starts_at}>{when(wnd.starts_at)}</td>
                  <td data-label="Ends" title={wnd.ends_at}>{when(wnd.ends_at)}</td>
                  <td data-label="Reason" class="access-names">{wnd.reason || '—'}</td>
                  <td data-label="State">
                    {wnd.active_now ? <span class="badge nst-suppressed">active now</span> : <span class="chip">{st}</span>}
                  </td>
                  <td data-label="">
                    {writable && <ConfirmButton label="Delete" question="Delete this maintenance window?" danger onConfirm={() => del(wnd.id)} />}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </>
  );
}

// Datetime-local value (local time, no zone) -> RFC3339 with the browser offset.
const toRFC3339 = (local: string): string => (local ? new Date(local).toISOString() : '');

function WindowForm({ orgs, canGlobal, onDone, onCancel }: {
  orgs: Org[]; canGlobal: boolean; onDone: () => void; onCancel: () => void;
}) {
  const [orgSel, setOrgSel] = useState(canGlobal ? '' : orgs[0] ? String(orgs[0].id) : '');
  const [starts, setStarts] = useState('');
  const [ends, setEnds] = useState('');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const submit = async (e: Event) => {
    e.preventDefault();
    setError('');
    const startsAt = toRFC3339(starts), endsAt = toRFC3339(ends);
    if (!startsAt || !endsAt) { setError('Set both a start and an end time.'); return; }
    if (new Date(endsAt) <= new Date(startsAt)) { setError('The end must be after the start.'); return; }
    setBusy(true);
    try {
      const body: NewWindow = { org_id: orgIdOf(orgSel), starts_at: startsAt, ends_at: endsAt, reason: reason.trim() };
      await api.createMaintenanceWindow(body);
      onDone();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card" onSubmit={submit}>
      <div class="form-row">
        <label>Client <OrgSelect value={orgSel} canGlobal={canGlobal} orgs={orgs} disabled={busy} onChange={setOrgSel} /></label>
        <label>Starts <input type="datetime-local" required value={starts} disabled={busy} onInput={(e) => setStarts(e.currentTarget.value)} /></label>
        <label>Ends <input type="datetime-local" required value={ends} disabled={busy} onInput={(e) => setEnds(e.currentTarget.value)} /></label>
      </div>
      <div class="form-row">
        <input type="text" placeholder="reason (optional)" value={reason} disabled={busy} onInput={(e) => setReason(e.currentTarget.value)} />
      </div>
      <div class="form-row">
        <button type="submit" class="btn btn-sm btn-primary" disabled={busy}>Create window</button>
        <button type="button" class="btn btn-sm" disabled={busy} onClick={onCancel}>Cancel</button>
        {error && <span class="error">{error}</span>}
      </div>
    </form>
  );
}
