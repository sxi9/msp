import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { when } from '../lib/format.ts';
import { accessOf, assignedOrgNames, summariseAccess, toPayload } from '../lib/orgscope.ts';
import type { Org, User } from '../types.ts';
import { ConfirmButton } from '../components/Confirm.tsx';
import { OrgChecklist } from '../components/OrgChecklist.tsx';

export function Users({ user }: { user: User }) {
  const users = usePoll(() => api.listUsers().then((r) => r.users), [], 0);
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0);
  const orgList = orgs.data ?? [];
  const knownOrgIds = orgList.map((o) => o.id);

  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState('operator');
  const [newAll, setNewAll] = useState(true);
  const [newSel, setNewSel] = useState<Set<number>>(new Set());
  const [error, setError] = useState('');
  const scopable = role === 'operator' || role === 'viewer';

  const toggleNew = (id: number, on: boolean) =>
    setNewSel((s) => { const n = new Set(s); if (on) n.add(id); else n.delete(id); return n; });

  const create = async (e: Event) => {
    e.preventDefault();
    setError('');
    try {
      const access = scopable ? toPayload(newAll, newSel, knownOrgIds) : undefined;
      await api.createUser(email.trim(), password, role, access);
      setEmail(''); setPassword(''); setNewAll(true); setNewSel(new Set());
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
      <form class="card" onSubmit={create}>
        <div class="form-row">
          <input type="email" placeholder="email" required autocomplete="off" value={email} onInput={(e) => setEmail(e.currentTarget.value)} />
          <input type="password" placeholder="password (10+ chars)" required minLength={10} autocomplete="new-password" value={password} onInput={(e) => setPassword(e.currentTarget.value)} />
          <select value={role} onChange={(e) => setRole(e.currentTarget.value)}>
            <option value="viewer">viewer</option><option value="operator">operator</option><option value="admin">admin</option>
          </select>
          <button type="submit" class="btn btn-primary">Create user</button>
          {error && <span class="error">{error}</span>}
        </div>
        {scopable && (
          <>
            <p class="muted" style="margin:8px 0 0">Client access</p>
            <OrgChecklist orgs={orgList} allOrgs={newAll} selected={newSel} onAllOrgs={setNewAll} onToggle={toggleNew} />
          </>
        )}
      </form>
      {orgs.error && <p class="error">Could not load clients: {orgs.error}</p>}
      {users.error && <p class="error">{users.error}</p>}
      {users.data && (
        <table class="grid">
          <thead><tr><th>Email</th><th>Role</th><th>Client access</th><th>Created</th><th>State</th><th></th></tr></thead>
          <tbody>
            {users.data.map((u) => (
              <UserRow
                key={u.id} u={u} self={u.id === user.id} orgs={orgList} knownOrgIds={knownOrgIds}
                onChanged={users.reload} onDisable={() => disable(u.id)}
              />
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function AccessCell({ u, orgs }: { u: User; orgs: Org[] }) {
  if (u.role === 'admin') return <span class="muted">All clients (admin)</span>;
  const a = accessOf(u);
  if (a.all) return <>All clients</>;
  if (a.orgIds.length === 0) return <span class="muted">No clients</span>;
  return <span class="access-names">{assignedOrgNames(a.orgIds, orgs).join(', ')}</span>;
}

function UserRow({ u, self, orgs, knownOrgIds, onChanged, onDisable }: {
  u: User; self: boolean; orgs: Org[]; knownOrgIds: number[];
  onChanged: () => void; onDisable: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [all, setAll] = useState(true);
  const [sel, setSel] = useState<Set<number>>(new Set());
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const editable = !u.disabled_at && u.role !== 'admin';

  const open = () => {
    const a = accessOf(u);
    setAll(a.all); setSel(new Set(a.orgIds)); setErr(''); setEditing(true);
  };
  const toggle = (id: number, on: boolean) =>
    setSel((s) => { const n = new Set(s); if (on) n.add(id); else n.delete(id); return n; });
  const save = async () => {
    setBusy(true); setErr('');
    try {
      await api.setUserOrgs(u.id, toPayload(all, sel, knownOrgIds));
      setEditing(false);
      onChanged();
    } catch (e) {
      setErr(errorText(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <tr class={u.disabled_at ? 'muted' : ''}>
        <td data-label="Email">{u.email}{self && <span class="chip">you</span>}</td>
        <td data-label="Role">{u.role}</td>
        <td data-label="Client access"><AccessCell u={u} orgs={orgs} /></td>
        <td data-label="Created">{when(u.created_at)}</td>
        <td data-label="State">{u.disabled_at ? `disabled ${when(u.disabled_at)}` : 'active'}</td>
        <td data-label="">
          <span class="row-actions">
            {editable && <button type="button" class="btn btn-sm" onClick={() => (editing ? setEditing(false) : open())}>{editing ? 'Close' : 'Edit access'}</button>}
            {!u.disabled_at && !self && <ConfirmButton label="Disable" question={`Disable ${u.email}?`} danger onConfirm={onDisable} />}
          </span>
        </td>
      </tr>
      {editing && (
        <tr class="expanded">
          <td colSpan={6}>
            <strong>Client access for {u.email}</strong>
            <OrgChecklist orgs={orgs} allOrgs={all} selected={sel} disabled={busy} onAllOrgs={setAll} onToggle={toggle} />
            <div class="form-row">
              <button type="button" class="btn btn-sm btn-primary" disabled={busy} onClick={save}>Save</button>
              <button type="button" class="btn btn-sm" disabled={busy} onClick={() => setEditing(false)}>Cancel</button>
              <span class="muted">Currently: {summariseAccess(u)}</span>
              {err && <span class="error">{err}</span>}
            </div>
          </td>
        </tr>
      )}
    </>
  );
}
