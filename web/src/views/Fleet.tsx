import { useState } from 'preact/hooks';
import { api } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { filterDevices } from '../lib/status.ts';
import { DeviceTable } from '../components/DeviceTable.tsx';

export function Fleet() {
  const [q, setQ] = useState('');
  const [status, setStatus] = useState('');
  const [orgId, setOrgId] = useState(0);
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 60000).data ?? [];
  const devices = usePoll(() => api.listDevices().then((r) => r.devices), [], 5000);
  const shown = filterDevices(devices.data ?? [], q, status, orgId);
  return (
    <section>
      <div class="toolbar">
        <h1>Fleet</h1>
        <select value={orgId} onChange={(e) => setOrgId(Number(e.currentTarget.value))}>
          <option value={0}>All orgs</option>
          {orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}
        </select>
        <select value={status} onChange={(e) => setStatus(e.currentTarget.value)}>
          <option value="">Any status</option><option value="online">Online</option><option value="offline">Offline</option><option value="pending">Pending</option>
        </select>
        <input type="search" placeholder="Search hostname" value={q} onInput={(e) => setQ(e.currentTarget.value)} />
        <span class="muted">{shown.length} of {devices.data?.length ?? 0}</span>
      </div>
      {devices.error && <p class="error">{devices.error}</p>}
      {devices.data && (
        <DeviceTable devices={shown} empty={devices.data.length === 0 ? 'No devices yet. Add a client under Orgs and run the install one-liner.' : 'No devices match the filter.'} />
      )}
    </section>
  );
}
