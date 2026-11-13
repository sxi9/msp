import { useEffect, useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { when } from '../lib/format.ts';
import type { AuditEntry } from '../types.ts';

const LIMIT = 200;

export function Audit() {
  const [orgId, setOrgId] = useState(0);
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0).data ?? [];
  const load = async (before?: number) => {
    setBusy(true);
    setError('');
    try {
      const r = await api.listAudit({ limit: LIMIT, org_id: orgId || undefined, before });
      setEntries((prev) => (before ? [...prev, ...r.entries] : r.entries));
      setDone(r.entries.length < LIMIT);
    } catch (e) { setError(errorText(e)); } finally { setBusy(false); }
  };
  useEffect(() => { void load(); }, [orgId]);
  const last = entries[entries.length - 1];
  return (
    <section>
      <div class="toolbar">
        <h1>Audit log</h1>
        <select value={orgId} onChange={(e) => setOrgId(Number(e.currentTarget.value))}>
          <option value={0}>All orgs</option>
          {orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}
        </select>
        <button type="button" class="btn btn-sm" disabled={busy} onClick={() => load()}>Refresh</button>
      </div>
      {error && <p class="error">{error}</p>}
      {entries.length === 0 && !busy && <p class="empty">No audit entries.</p>}
      {entries.length > 0 && (
        <table class="grid audit">
          <thead><tr><th>At</th><th>Actor</th><th>Action</th><th>Target</th><th>IP</th><th>Detail</th></tr></thead>
          <tbody>
            {entries.map((e) => (
              <tr key={e.id}>
                <td data-label="At" title={e.at}>{when(e.at)}</td>
                <td data-label="Actor"><span class="chip">{e.actor_type}</span> {e.actor_label}</td>
                <td data-label="Action"><code>{e.action}</code></td>
                <td data-label="Target">{e.target_type}{e.target_id !== null && ` #${e.target_id}`}</td>
                <td data-label="IP" class="muted">{e.ip || '—'}</td>
                <td data-label="Detail"><code class="detail">{JSON.stringify(e.detail)}</code></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {entries.length > 0 && !done && last && <button type="button" class="btn" disabled={busy} onClick={() => load(last.id)}>Load older</button>}
    </section>
  );
}
