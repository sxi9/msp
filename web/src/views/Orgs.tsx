import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { navigate } from '../router.ts';
import { isAdmin, type User } from '../types.ts';

export function Orgs({ user }: { user: User }) {
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 10000);
  const [name, setName] = useState('');
  const [error, setError] = useState('');
  const add = async (e: Event) => {
    e.preventDefault();
    setError('');
    try {
      const r = await api.createOrg(name.trim());
      navigate(`#/orgs/${r.org.id}`);
    } catch (err) {
      setError(errorText(err) === 'conflict' ? 'An org with that name already exists.' : errorText(err));
    }
  };
  return (
    <section>
      <div class="toolbar"><h1>Orgs</h1></div>
      {isAdmin(user) && (
        <form class="card form-row" onSubmit={add}>
          <input type="text" placeholder="New client name" required minLength={1} value={name} onInput={(e) => setName(e.currentTarget.value)} />
          <button type="submit" class="btn btn-primary">Add client</button>
          {error && <span class="error">{error}</span>}
        </form>
      )}
      {orgs.error && <p class="error">{orgs.error}</p>}
      {orgs.data && orgs.data.length === 0 && <p class="empty">No clients yet.</p>}
      {orgs.data && orgs.data.length > 0 && (
        <table class="grid">
          <thead><tr><th>Name</th><th>Devices</th><th>Online</th><th>Open alerts</th></tr></thead>
          <tbody>
            {orgs.data.map((o) => (
              <tr key={o.id}>
                <td data-label="Name"><a href={`#/orgs/${o.id}`}>{o.name}</a></td>
                <td data-label="Devices">{o.device_count}</td>
                <td data-label="Online">{o.online_count}</td>
                <td data-label="Open alerts">{o.open_alerts > 0 ? <span class="badge badge-amber">{o.open_alerts}</span> : 0}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
