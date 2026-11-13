import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { ago } from '../lib/format.ts';
import { canOperate, type User } from '../types.ts';

export function Alerts({ user }: { user: User }) {
  const [state, setState] = useState<'open' | 'all'>('open');
  const [orgId, setOrgId] = useState(0);
  const [error, setError] = useState('');
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0).data ?? [];
  const alerts = usePoll(() => api.listAlerts(state, orgId || undefined).then((r) => r.alerts), [state, orgId], 10000);
  const ack = async (id: number) => {
    try { await api.ackAlert(id); alerts.reload(); } catch (e) { setError(errorText(e)); }
  };
  return (
    <section>
      <div class="toolbar">
        <h1>Alerts</h1>
        <div class="tabs">
          <button type="button" class={state === 'open' ? 'active' : ''} onClick={() => setState('open')}>Open</button>
          <button type="button" class={state === 'all' ? 'active' : ''} onClick={() => setState('all')}>All</button>
        </div>
        <select value={orgId} onChange={(e) => setOrgId(Number(e.currentTarget.value))}>
          <option value={0}>All orgs</option>
          {orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}
        </select>
      </div>
      {(error || alerts.error) && <p class="error">{error || alerts.error}</p>}
      {alerts.data && alerts.data.length === 0 && <p class="empty">No {state === 'open' ? 'open ' : ''}alerts.</p>}
      {alerts.data && alerts.data.length > 0 && (
        <table class="grid">
          <thead><tr><th>Severity</th><th>Type</th><th>Device</th><th>Org</th><th>Message</th><th>Opened</th><th>State</th><th></th></tr></thead>
          <tbody>
            {alerts.data.map((a) => (
              <tr key={a.id} class={a.resolved_at ? 'muted' : ''}>
                <td data-label="Severity"><span class={`badge sev-${a.severity}`}>{a.severity}</span></td>
                <td data-label="Type">{a.type}</td>
                <td data-label="Device"><a href={`#/devices/${a.device_id}`}>{a.hostname}</a></td>
                <td data-label="Org">{a.org_name}</td>
                <td data-label="Message">{a.message}</td>
                <td data-label="Opened" title={a.opened_at}>{ago(a.opened_at)}</td>
                <td data-label="State">{a.resolved_at ? `resolved ${ago(a.resolved_at)}` : a.acknowledged_at ? `acked ${ago(a.acknowledged_at)}` : 'open'}</td>
                <td data-label="">
                  {canOperate(user) && !a.resolved_at && !a.acknowledged_at && <button type="button" class="btn btn-sm" onClick={() => ack(a.id)}>Ack</button>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
