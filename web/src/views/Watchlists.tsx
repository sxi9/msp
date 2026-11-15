import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { when } from '../lib/format.ts';
import type { Device, NewWatchlistItem, Org, User, WatchKind, WatchlistItem, WatchScope } from '../types.ts';
import { ConfirmButton } from '../components/Confirm.tsx';

const KIND_LABEL: Record<WatchKind, string> = { service: 'service', process: 'process' };

/** A one-line label for an item's target: a client (org item) or a device (device item). */
function targetLabel(it: WatchlistItem): string {
  return it.scope === 'org' ? `${it.org_name || `#${it.org_id}`} — all desktops` : `${it.hostname || `device #${it.device_id}`} (${it.org_name})`;
}

/**
 * The Watchlists admin area (WL1, admin/operator): the service/process items the
 * fleet watches. An org item applies to every desktop in a client; a device item to
 * one device. A stopped item with alerts on opens a service_down alert (and feeds
 * notifications/automations). Android is never watched. RS2 org-scoped by the server.
 */
export function Watchlists({ user }: { user: User }) {
  const items = usePoll(() => api.listWatchlist().then((r) => r.items), [], 15000);
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0);
  const devices = usePoll(() => api.listDevices().then((r) => r.devices), [], 0);
  const [editing, setEditing] = useState<WatchlistItem | 'new' | null>(null);
  const [error, setError] = useState('');

  const orgList = orgs.data ?? [];
  const deviceList = (devices.data ?? []).filter((d) => d.platform !== 'android');
  const remove = async (it: WatchlistItem) => {
    setError('');
    try { await api.deleteWatchlistItem(it.id); items.reload(); } catch (e) { setError(errorText(e)); }
  };

  return (
    <section>
      <div class="toolbar">
        <h1>Watchlists</h1>
        <button type="button" class="btn btn-primary" onClick={() => setEditing('new')}>New item</button>
      </div>
      <p class="muted">
        Watch a service or process on desktops and alert when it stops. A client item applies to every desktop in that
        client; a device item to one device. A stopped item with alerts on opens a <code>service_down</code> alert, which
        flows to your notification rules and triggered automations. Android devices are never watched.
      </p>
      {error && <p class="error">{error}</p>}
      {items.error && <p class="error">{items.error}</p>}

      {editing && (
        <WatchlistForm user={user} orgs={orgList} devices={deviceList} item={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)} onSaved={() => { setEditing(null); items.reload(); }} />
      )}

      <h2>Items</h2>
      {!items.data ? <p class="muted">Loading…</p> : items.data.length === 0 ? (
        <p class="empty">No watchlist items yet. Create one above.</p>
      ) : (
        <div class="log-scroll">
          <table class="grid">
            <thead>
              <tr><th>Name</th><th>Kind</th><th>Scope</th><th>Target</th><th>Alerts</th><th>Added</th><th></th></tr>
            </thead>
            <tbody>
              {items.data.map((it) => (
                <tr key={it.id}>
                  <td data-label="Name"><code>{it.name}</code></td>
                  <td data-label="Kind"><span class="chip">{KIND_LABEL[it.kind]}</span></td>
                  <td data-label="Scope"><span class="chip">{it.scope === 'org' ? 'client' : 'device'}</span></td>
                  <td data-label="Target">{it.scope === 'device' && it.device_id ? <a href={`#/devices/${it.device_id}`}>{targetLabel(it)}</a> : targetLabel(it)}</td>
                  <td data-label="Alerts">{it.alert ? <span class="ok">on</span> : <span class="muted">off</span>}</td>
                  <td data-label="Added" class="muted">{when(it.created_at)}</td>
                  <td data-label="">
                    <div class="row-actions">
                      <button type="button" class="btn btn-sm" onClick={() => setEditing(it)}>Edit</button>
                      <ConfirmButton label="Delete" question={`Delete the watch on ${it.name}? Any open service_down alert it caused is resolved.`} danger onConfirm={() => remove(it)} />
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

/**
 * Create/edit form for a watchlist item. On create, scope selects a client (org
 * item) or a device (device item); on edit, the scope/target are fixed and only
 * kind/name/alert change (the server ignores a changed target).
 */
function WatchlistForm({ user: _user, orgs, devices, item, onClose, onSaved }: {
  user: User; orgs: Org[]; devices: Device[]; item: WatchlistItem | null; onClose: () => void; onSaved: () => void;
}) {
  const editing = item !== null;
  const [scope, setScope] = useState<WatchScope>(item?.scope ?? 'org');
  const [orgId, setOrgId] = useState(item?.scope === 'org' && item.org_id !== null ? String(item.org_id) : '');
  const [deviceId, setDeviceId] = useState(item?.scope === 'device' && item.device_id !== null ? String(item.device_id) : '');
  const [kind, setKind] = useState<WatchKind>(item?.kind ?? 'service');
  const [name, setName] = useState(item?.name ?? '');
  const [alert, setAlert] = useState(item?.alert ?? true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const trimmed = name.trim();
  const invalid = trimmed === '' ? 'Name is required.'
    : trimmed.length > 256 ? 'Name is too long.'
    : !editing && scope === 'org' && orgId === '' ? 'Pick a client.'
    : !editing && scope === 'device' && deviceId === '' ? 'Pick a device.'
    : '';

  const submit = async (e: Event) => {
    e.preventDefault();
    if (invalid) { setError(invalid); return; }
    setBusy(true);
    setError('');
    try {
      if (editing) {
        await api.updateWatchlistItem(item.id, { kind, name: trimmed, alert });
      } else {
        const payload: NewWatchlistItem = scope === 'org'
          ? { scope: 'org', org_id: Number(orgId), kind, name: trimmed, alert }
          : { scope: 'device', device_id: Number(deviceId), kind, name: trimmed, alert };
        await api.createWatchlistItem(payload);
      }
      onSaved();
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card aut-form" onSubmit={submit}>
      <h2>{editing ? 'Edit watchlist item' : 'New watchlist item'}</h2>
      <div class="form-row">
        {editing ? (
          <label>Scope<input type="text" value={scope === 'org' ? 'client' : 'device'} disabled /></label>
        ) : (
          <label>Scope
            <select value={scope} onChange={(e) => setScope(e.currentTarget.value as WatchScope)}>
              <option value="org">A client (every desktop)</option>
              <option value="device">One device</option>
            </select>
          </label>
        )}
        {editing ? (
          <label>Target<input type="text" value={item.scope === 'org' ? `${item.org_name} — all desktops` : `${item.hostname} (${item.org_name})`} disabled /></label>
        ) : scope === 'org' ? (
          <label>Client
            <select value={orgId} onChange={(e) => setOrgId(e.currentTarget.value)}>
              <option value="">Pick a client…</option>
              {orgs.map((o) => <option key={o.id} value={String(o.id)}>{o.name}</option>)}
            </select>
          </label>
        ) : (
          <label>Device
            <select value={deviceId} onChange={(e) => setDeviceId(e.currentTarget.value)}>
              <option value="">Pick a device…</option>
              {devices.map((d) => <option key={d.id} value={String(d.id)}>{d.hostname} — {d.org_name} ({d.platform})</option>)}
            </select>
          </label>
        )}
        <label>Kind
          <select value={kind} onChange={(e) => setKind(e.currentTarget.value as WatchKind)}>
            <option value="service">service</option><option value="process">process</option>
          </select>
        </label>
      </div>
      <input type="text" class="full" placeholder={kind === 'service' ? 'Unit name, e.g. ssh.service or Spooler' : 'Process/executable name, e.g. cron or nginx'}
        value={name} maxLength={256} onInput={(e) => setName(e.currentTarget.value)} />
      <label class="inline"><input type="checkbox" checked={alert} onChange={(e) => setAlert(e.currentTarget.checked)} /> Open a <code>service_down</code> alert when it is not running</label>
      {error && <p class="error">{error}</p>}
      <div class="form-row">
        <button type="submit" class="btn btn-primary" disabled={busy || invalid !== ''}>{busy ? 'Saving…' : editing ? 'Save' : 'Create'}</button>
        <button type="button" class="btn" onClick={onClose}>Cancel</button>
        {invalid && <span class="muted">{invalid}</span>}
      </div>
    </form>
  );
}
