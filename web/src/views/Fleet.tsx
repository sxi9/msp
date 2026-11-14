import { useState } from 'preact/hooks';
import { api } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { filterDevices } from '../lib/status.ts';
import { hasPendingSecurity } from '../lib/patches.ts';
import { isOutdated, outdatedCount, selfUpdateEnabled } from '../lib/agentver.ts';
import { DeviceTable } from '../components/DeviceTable.tsx';

export function Fleet() {
  const [q, setQ] = useState('');
  const [status, setStatus] = useState('');
  const [orgId, setOrgId] = useState(0);
  const [secOnly, setSecOnly] = useState(false);
  const [outdatedOnly, setOutdatedOnly] = useState(false);
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 60000).data ?? [];
  const devices = usePoll(() => api.listDevices().then((r) => r.devices), [], 5000);
  // The agent release manifest (AU1): drives the Agent column's "outdated" flag,
  // the outdated filter and the released-version banner. Fetched once per session.
  const manifest = usePoll(() => api.agentRelease(), [], 0).data ?? null;
  const all = devices.data ?? [];
  let shown = filterDevices(all, q, status, orgId);
  if (secOnly) shown = shown.filter(hasPendingSecurity);
  if (outdatedOnly) shown = shown.filter((d) => isOutdated(d, manifest));
  const outdated = outdatedCount(all, manifest);
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
        <label><input type="checkbox" checked={secOnly} onChange={(e) => setSecOnly(e.currentTarget.checked)} /> Pending security updates</label>
        <label><input type="checkbox" checked={outdatedOnly} onChange={(e) => setOutdatedOnly(e.currentTarget.checked)} /> Outdated agent</label>
        <span class="muted">{shown.length} of {all.length}</span>
      </div>
      {selfUpdateEnabled(manifest) && (
        <p class="banner">Released agent version <strong>{manifest!.version}</strong>.{' '}
          {outdated > 0
            ? <>{outdated} device{outdated === 1 ? '' : 's'} running an older agent — open a device to update it.</>
            : <span class="ok">Every device is up to date.</span>}
        </p>
      )}
      {devices.error && <p class="error">{devices.error}</p>}
      {devices.data && (
        <DeviceTable devices={shown} manifest={manifest} empty={all.length === 0 ? 'No devices yet. Add a client under Orgs and run the install one-liner.' : 'No devices match the filter.'} />
      )}
    </section>
  );
}
