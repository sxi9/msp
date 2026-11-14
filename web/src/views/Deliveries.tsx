import { useState } from 'preact/hooks';
import { api } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { when } from '../lib/format.ts';
import type { DeliveryStatus, NotificationDelivery, Org } from '../types.ts';
import { DeliveryStatusChip } from '../components/StatusChip.tsx';

const STATUSES: DeliveryStatus[] = ['pending', 'sent', 'failed', 'suppressed'];

const orgLabel = (orgId: number | null, orgs: Org[]): string =>
  orgId === null ? 'All clients' : orgs.find((o) => o.id === orgId)?.name ?? `#${orgId}`;

// What the alert/device cell shows: the rendered subject names the device and
// org; otherwise the alert id, or "test" for a channel test.
const subjectOf = (d: NotificationDelivery): string => {
  const subj = d.detail['subject'];
  if (typeof subj === 'string' && subj) return subj;
  if (d.alert_id !== null) return `alert #${d.alert_id}`;
  return d.event === 'test' ? 'test' : '—';
};

export function Deliveries() {
  const [status, setStatus] = useState('');
  const [orgId, setOrgId] = useState(0);
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0).data ?? [];
  // Channel names for the log; viewers may read channels (GET needs viewer).
  const channels = usePoll(() => api.listNotifyChannels().then((r) => r.channels), [], 0).data ?? [];
  const chName = (id: number | null) => (id === null ? '—' : channels.find((c) => c.id === id)?.name ?? `#${id}`);
  const deliveries = usePoll(
    () => api.listDeliveries({ status: status || undefined, org_id: orgId || undefined }).then((r) => r.deliveries),
    [status, orgId], 10000,
  );

  return (
    <section>
      <div class="toolbar">
        <h1>Delivery log</h1>
        <a class="btn btn-sm" href="#/notify">Notification settings</a>
        <div class="tabs">
          <button type="button" class={status === '' ? 'active' : ''} onClick={() => setStatus('')}>All</button>
          {STATUSES.map((s) => (
            <button type="button" key={s} class={status === s ? 'active' : ''} onClick={() => setStatus(s)}>{s}</button>
          ))}
        </div>
        <select value={orgId} onChange={(e) => setOrgId(Number(e.currentTarget.value))}>
          <option value={0}>All orgs</option>
          {orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}
        </select>
        <button type="button" class="btn btn-sm" onClick={() => deliveries.reload()}>Refresh</button>
      </div>
      {deliveries.error && <p class="error">{deliveries.error}</p>}
      {deliveries.data && deliveries.data.length === 0 && <p class="empty">No deliveries{status ? ` with status ${status}` : ''}.</p>}
      {deliveries.data && deliveries.data.length > 0 && (
        <div class="log-scroll">
          <table class="grid">
            <thead><tr><th>Time</th><th>Org</th><th>Alert / device</th><th>Channel</th><th>Event</th><th>Status</th><th>Tries</th><th>Last error</th></tr></thead>
            <tbody>
              {deliveries.data.map((d) => (
                <tr key={d.id}>
                  <td data-label="Time" title={d.updated_at}>{when(d.updated_at)}</td>
                  <td data-label="Org">{orgLabel(d.org_id, orgs)}</td>
                  <td data-label="Alert / device" class="access-names">{subjectOf(d)}</td>
                  <td data-label="Channel">{chName(d.channel_id)}</td>
                  <td data-label="Event"><span class="chip">{d.event}</span></td>
                  <td data-label="Status"><DeliveryStatusChip status={d.status} /></td>
                  <td data-label="Tries">{d.attempts}</td>
                  <td data-label="Last error" class="access-names">{d.last_error ? <code class="detail">{d.last_error}</code> : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
