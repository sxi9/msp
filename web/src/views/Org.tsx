import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { ago, fromNow, when } from '../lib/format.ts';
import { canOperate, type TokenCreated, type User } from '../types.ts';
import { DeviceTable } from '../components/DeviceTable.tsx';
import { CopyBlock } from '../components/CopyBlock.tsx';
import { ConfirmButton } from '../components/Confirm.tsx';

export function Org({ id, user }: { id: number; user: User }) {
  const org = usePoll(() => api.getOrg(id), [id], 15000);
  const devices = usePoll(() => api.listDevices({ org_id: id }).then((r) => r.devices), [id], 5000);
  const [label, setLabel] = useState('');
  const [maxUses, setMaxUses] = useState(10);
  const [ttl, setTtl] = useState(168);
  const [created, setCreated] = useState<TokenCreated | null>(null);
  const [error, setError] = useState('');
  const create = async (e: Event) => {
    e.preventDefault();
    setError('');
    try {
      setCreated(await api.createToken(id, label.trim(), maxUses, ttl));
      setLabel('');
      org.reload();
    } catch (err) { setError(errorText(err)); }
  };
  const revoke = async (tid: number) => {
    try { await api.revokeToken(id, tid); org.reload(); } catch (err) { setError(errorText(err)); }
  };
  if (org.error) return <p class="error">{org.error}</p>;
  if (!org.data) return <p class="muted">Loading…</p>;
  const now = Date.now();
  return (
    <section>
      <div class="toolbar">
        <h1>{org.data.org.name}</h1>
        <span class="muted">{org.data.org.device_count} devices, {org.data.org.online_count} online, {org.data.org.open_alerts} open alerts</span>
      </div>
      <h2>Devices</h2>
      {devices.error && <p class="error">{devices.error}</p>}
      {devices.data && <DeviceTable devices={devices.data} grouped={false} empty="No devices enrolled yet. Create an enrollment token below and run the install one-liner on the client machine." />}

      <h2>Enrollment tokens</h2>
      {canOperate(user) && (
        <form class="card form-row" onSubmit={create}>
          <input type="text" placeholder="Label (e.g. front desk)" value={label} onInput={(e) => setLabel(e.currentTarget.value)} />
          <label>Max uses<input type="number" min={1} max={1000} value={maxUses} onInput={(e) => setMaxUses(Number(e.currentTarget.value))} /></label>
          <label>TTL hours<input type="number" min={1} max={8760} value={ttl} onInput={(e) => setTtl(Number(e.currentTarget.value))} /></label>
          <button type="submit" class="btn btn-primary">New enrollment token</button>
          {error && <span class="error">{error}</span>}
        </form>
      )}
      {created && (
        <div class="card">
          <p><strong>Token created.</strong> It is shown once; copy it now. It expires {fromNow(created.token.expires_at)} ({when(created.token.expires_at)}) or after {created.token.max_uses} uses.</p>
          <CopyBlock label="Enrollment token" text={created.plaintext} highlight />
          <CopyBlock label="Linux (run as root)" text={created.install.linux} />
          <CopyBlock label="Windows (admin PowerShell)" text={created.install.windows} />
          <CopyBlock label="Android" text={created.install.android} />
          <button type="button" class="btn btn-sm" onClick={() => setCreated(null)}>Dismiss</button>
        </div>
      )}
      {org.data.tokens.length === 0 ? <p class="empty">No tokens yet.</p> : (
        <table class="grid">
          <thead><tr><th>Prefix</th><th>Label</th><th>Uses</th><th>Expires</th><th>Created</th><th>State</th><th></th></tr></thead>
          <tbody>
            {org.data.tokens.map((t) => {
              const expired = Date.parse(t.expires_at) < now;
              const state = t.revoked_at ? 'revoked' : expired ? 'expired' : t.uses >= t.max_uses ? 'used up' : 'active';
              return (
                <tr key={t.id} class={state === 'active' ? '' : 'muted'}>
                  <td data-label="Prefix"><code>{t.token_prefix}…</code></td>
                  <td data-label="Label">{t.label || '—'}</td>
                  <td data-label="Uses">{t.uses} / {t.max_uses}</td>
                  <td data-label="Expires" title={t.expires_at}>{when(t.expires_at)}</td>
                  <td data-label="Created">{ago(t.created_at)}</td>
                  <td data-label="State">{state}</td>
                  <td data-label="">{canOperate(user) && state === 'active' && <ConfirmButton label="Revoke" question="Revoke this token?" danger onConfirm={() => revoke(t.id)} />}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </section>
  );
}
