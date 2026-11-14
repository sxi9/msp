import { Fragment } from 'preact';
import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { when } from '../lib/format.ts';
import { channelTarget, describeRule, secretLabel, windowState } from '../lib/notify.ts';
import { accessOf } from '../lib/orgscope.ts';
import {
  ALERT_TYPES, canOperate, type ChannelUpdate, type DeliveryStatus, type EmailConfig,
  type MaintenanceWindow, type NewChannel, type NewRule, type NewWindow, type NotifyChannel,
  type NotifyKind, type NotifyRule, type Org, type RuleUpdate, type Severity, type User, type WebhookConfig,
} from '../types.ts';
import { ConfirmButton } from '../components/Confirm.tsx';
import { DeliveryStatusChip } from '../components/StatusChip.tsx';

const SEVERITIES: Severity[] = ['low', 'medium', 'high'];

// A channel/rule/window org is null for a global row usable by every client.
const orgLabel = (orgId: number | null, orgs: Org[]): string =>
  orgId === null ? 'All clients' : orgs.find((o) => o.id === orgId)?.name ?? `#${orgId}`;

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
  const windows = usePoll(() => api.listMaintenanceWindows().then((r) => r.windows), [], 20000);
  const orgList = orgs.data ?? [];
  const chList = channels.data ?? [];
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
        rules={rules.data} error={rules.error} orgs={orgList} channels={chList} canGlobal={canGlobal}
        writable={writable} onChanged={rules.reload}
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
                  <td data-label="Target" class="access-names">{channelTarget(c) || '—'}</td>
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
  const cfg = (channel?.config ?? {}) as Partial<EmailConfig & WebhookConfig>;
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
  // secret (write-only): on edit, revealed only when replacing
  const [replaceSecret, setReplaceSecret] = useState(!editing);
  const [secret, setSecret] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const buildConfig = (): EmailConfig | WebhookConfig =>
    kind === 'email'
      ? { host: host.trim(), port: Number(port) || 0, from: from.trim(), to: to.split(',').map((s) => s.trim()).filter(Boolean), starttls, username: username.trim() }
      : { url: url.trim(), flavor };

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
          <select value={kind} disabled={busy} onChange={(e) => setKind(e.currentTarget.value as NotifyKind)}>
            <option value="email">email</option><option value="webhook">webhook</option>
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
      ) : (
        <div class="form-row">
          <input type="url" placeholder="https://hooks.slack.com/…" required value={url} disabled={busy} onInput={(e) => setUrl(e.currentTarget.value)} />
          <label>Flavor
            <select value={flavor} disabled={busy} onChange={(e) => setFlavor(e.currentTarget.value as WebhookConfig['flavor'])}>
              <option value="slack">slack</option><option value="generic">generic JSON</option>
            </select>
          </label>
        </div>
      )}
      <div class="form-row">
        {editing
          ? <label><input type="checkbox" checked={replaceSecret} disabled={busy} onChange={(e) => { setReplaceSecret(e.currentTarget.checked); if (!e.currentTarget.checked) setSecret(''); }} /> replace secret (currently {secretLabel(channel!.secret_set)})</label>
          : <span class="muted">Secret</span>}
        {(replaceSecret || !editing) && (
          <input type="password" placeholder={kind === 'email' ? 'SMTP password (optional)' : 'signing secret (optional)'} autocomplete="new-password" value={secret} disabled={busy} onInput={(e) => setSecret(e.currentTarget.value)} />
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

function RulesPanel({ rules, error, orgs, channels, canGlobal, writable, onChanged }: {
  rules: NotifyRule[] | null; error: string | null; orgs: Org[]; channels: NotifyChannel[];
  canGlobal: boolean; writable: boolean; onChanged: () => void;
}) {
  const [adding, setAdding] = useState(false);
  const [editId, setEditId] = useState(0);
  const chName = (id: number) => channels.find((c) => c.id === id)?.name ?? `#${id}`;
  const del = async (id: number) => { await api.deleteNotifyRule(id); onChanged(); };

  return (
    <>
      <div class="toolbar">
        <h2>Rules</h2>
        {writable && <button type="button" class="btn btn-sm" disabled={channels.length === 0} title={channels.length === 0 ? 'Add a channel first' : ''} onClick={() => { setAdding((a) => !a); setEditId(0); }}>{adding ? 'Close' : 'Add rule'}</button>}
      </div>
      {adding && <RuleForm orgs={orgs} channels={channels} canGlobal={canGlobal} onCancel={() => setAdding(false)} onDone={() => { setAdding(false); onChanged(); }} />}
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
                  <td data-label="Channel">{chName(ru.channel_id)}</td>
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
                    <RuleForm rule={ru} orgs={orgs} channels={channels} canGlobal={canGlobal} onCancel={() => setEditId(0)} onDone={() => { setEditId(0); onChanged(); }} />
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

function RuleForm({ rule, orgs, channels, canGlobal, onDone, onCancel }: {
  rule?: NotifyRule; orgs: Org[]; channels: NotifyChannel[]; canGlobal: boolean; onDone: () => void; onCancel: () => void;
}) {
  const editing = !!rule;
  const [name, setName] = useState(rule?.name ?? '');
  const [orgSel, setOrgSel] = useState(rule ? (rule.org_id === null ? '' : String(rule.org_id)) : (canGlobal ? '' : orgs[0] ? String(orgs[0].id) : ''));
  const [minSeverity, setMinSeverity] = useState<Severity>(rule?.min_severity ?? 'medium');
  const [types, setTypes] = useState<Set<string>>(new Set(rule?.alert_types ?? []));
  const [onOpen, setOnOpen] = useState(rule?.on_open ?? true);
  const [onResolve, setOnResolve] = useState(rule?.on_resolve ?? false);
  const [channelId, setChannelId] = useState(rule?.channel_id ?? channels[0]?.id ?? 0);
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
      if (editing) {
        const body: RuleUpdate = { name: name.trim(), min_severity: minSeverity, alert_types: alertTypes, on_open: onOpen, on_resolve: onResolve, channel_id: channelId, enabled };
        await api.updateNotifyRule(rule.id, body);
      } else {
        const body: NewRule = { name: name.trim(), org_id: orgIdOf(orgSel), min_severity: minSeverity, alert_types: alertTypes, on_open: onOpen, on_resolve: onResolve, channel_id: channelId, enabled };
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
