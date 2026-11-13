import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { when } from '../lib/format.ts';
import type { User } from '../types.ts';
import { ConfirmButton } from '../components/Confirm.tsx';

export function Users({ user }: { user: User }) {
  const users = usePoll(() => api.listUsers().then((r) => r.users), [], 0);
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState('operator');
  const [error, setError] = useState('');
  const create = async (e: Event) => {
    e.preventDefault();
    setError('');
    try {
      await api.createUser(email.trim(), password, role);
      setEmail(''); setPassword('');
      users.reload();
    } catch (err) {
      setError(errorText(err) === 'conflict' ? 'A user with that email already exists.' : errorText(err));
    }
  };
  const disable = async (id: number) => {
    try { await api.disableUser(id); users.reload(); } catch (err) { setError(errorText(err)); }
  };
  return (
    <section>
      <div class="toolbar"><h1>Users</h1></div>
      <form class="card form-row" onSubmit={create}>
        <input type="email" placeholder="email" required autocomplete="off" value={email} onInput={(e) => setEmail(e.currentTarget.value)} />
        <input type="password" placeholder="password (10+ chars)" required minLength={10} autocomplete="new-password" value={password} onInput={(e) => setPassword(e.currentTarget.value)} />
        <select value={role} onChange={(e) => setRole(e.currentTarget.value)}>
          <option value="viewer">viewer</option><option value="operator">operator</option><option value="admin">admin</option>
        </select>
        <button type="submit" class="btn btn-primary">Create user</button>
        {error && <span class="error">{error}</span>}
      </form>
      {users.error && <p class="error">{users.error}</p>}
      {users.data && (
        <table class="grid">
          <thead><tr><th>Email</th><th>Role</th><th>Created</th><th>State</th><th></th></tr></thead>
          <tbody>
            {users.data.map((u) => (
              <tr key={u.id} class={u.disabled_at ? 'muted' : ''}>
                <td data-label="Email">{u.email}{u.id === user.id && <span class="chip">you</span>}</td>
                <td data-label="Role">{u.role}</td>
                <td data-label="Created">{when(u.created_at)}</td>
                <td data-label="State">{u.disabled_at ? `disabled ${when(u.disabled_at)}` : 'active'}</td>
                <td data-label="">{!u.disabled_at && u.id !== user.id && <ConfirmButton label="Disable" question={`Disable ${u.email}?`} danger onConfirm={() => disable(u.id)} />}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
