import { useEffect, useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { navigate } from '../router.ts';
import { argvPreview, parseArgv } from '../lib/argv.ts';
import { filterDevices } from '../lib/status.ts';
import {
  canRunScriptOn, scriptSkipNote, targetSummary, validateScript, type ScriptDraft,
} from '../lib/automations.ts';
import type { NewScript, Org, Script, ScriptKind, ScriptPlatform, User } from '../types.ts';
import { isAdmin } from '../types.ts';
import { ConfirmButton } from '../components/Confirm.tsx';

const PLATFORM_LABEL: Record<ScriptPlatform, string> = { linux: 'Linux', windows: 'Windows', any: 'Any desktop' };

/** The name of a client org by id, from the loaded list; a fallback when unknown. */
function orgName(orgs: Org[], id: number | null): string {
  if (id === null) return 'All clients';
  return orgs.find((o) => o.id === id)?.name ?? `#${id}`;
}

/** A compact one-line view of what a script runs. */
function commandOf(s: Pick<Script, 'kind' | 'argv' | 'shell' | 'body'>): string {
  return s.kind === 'exec' ? argvPreview(s.argv) : `${s.shell}: ${s.body.split('\n')[0]}${s.body.includes('\n') ? ' …' : ''}`;
}

/**
 * The Scripts library page (AUT1, admin/operator): CRUD the saved, reusable
 * scripts, and run one across many devices as a tracked job. Org-scoped by the
 * server; a global script ("All clients") is usable for any client. Jobs are
 * reachable from here (recent jobs link, and each run opens its progress view).
 */
export function Scripts({ user }: { user: User }) {
  const scripts = usePoll(() => api.listScripts().then((r) => r.scripts), [], 15000);
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0);
  const [editing, setEditing] = useState<Script | 'new' | null>(null);
  const [running, setRunning] = useState<Script | null>(null);
  const [error, setError] = useState('');

  const orgList = orgs.data ?? [];
  const remove = async (s: Script) => {
    setError('');
    try { await api.deleteScript(s.id); scripts.reload(); } catch (e) { setError(errorText(e)); }
  };

  return (
    <section>
      <div class="toolbar">
        <h1>Scripts</h1>
        <button type="button" class="btn btn-primary" onClick={() => setEditing('new')}>New script</button>
        <a class="btn" href="#/jobs">Recent jobs</a>
      </div>
      <p class="muted">
        A saved, editable command you can run on one device or bulk-run across many as a tracked job, and attach to a
        scheduled or alert-triggered automation. Commands run as argv, never a shell; android desktops are skipped for
        exec/script runs. A global script is usable for any client.
      </p>
      {error && <p class="error">{error}</p>}
      {scripts.error && <p class="error">{scripts.error}</p>}

      {editing && (
        <ScriptForm user={user} orgs={orgList} script={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)} onSaved={() => { setEditing(null); scripts.reload(); }} />
      )}
      {running && (
        <RunDialog script={running} orgs={orgList} onClose={() => setRunning(null)}
          onRan={(jobId) => { setRunning(null); navigate(`#/jobs/${jobId}`); }} />
      )}

      <h2>Library</h2>
      {!scripts.data ? <p class="muted">Loading…</p> : scripts.data.length === 0 ? (
        <p class="empty">No scripts yet. Create one above.</p>
      ) : (
        <div class="log-scroll">
          <table class="grid">
            <thead>
              <tr><th>Name</th><th>Platform</th><th>Kind</th><th>Command</th><th>Timeout</th><th>Client</th><th></th></tr>
            </thead>
            <tbody>
              {scripts.data.map((s) => (
                <tr key={s.id}>
                  <td data-label="Name">{s.name}{s.description && <div class="muted">{s.description}</div>}</td>
                  <td data-label="Platform"><span class="chip">{PLATFORM_LABEL[s.platform]}</span></td>
                  <td data-label="Kind"><span class="chip">{s.kind}</span></td>
                  <td data-label="Command"><code>{commandOf(s)}</code></td>
                  <td data-label="Timeout" class="muted">{s.timeout_s}s</td>
                  <td data-label="Client">{s.org_id === null ? <span class="chip">All clients</span> : orgName(orgList, s.org_id)}</td>
                  <td data-label="">
                    <div class="row-actions">
                      <button type="button" class="btn btn-sm btn-primary" onClick={() => setRunning(s)}>Run on devices</button>
                      <button type="button" class="btn btn-sm" onClick={() => setEditing(s)}>Edit</button>
                      <ConfirmButton label="Delete" question={`Delete ${s.name}? Automations using it are removed; jobs it ran keep their history.`} danger onConfirm={() => remove(s)} />
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

/** The create/edit form for a library script. */
function ScriptForm({ user, orgs, script, onClose, onSaved }: {
  user: User; orgs: Org[]; script: Script | null; onClose: () => void; onSaved: () => void;
}) {
  const [name, setName] = useState(script?.name ?? '');
  const [description, setDescription] = useState(script?.description ?? '');
  const [orgId, setOrgId] = useState(script ? (script.org_id === null ? '' : String(script.org_id)) : '');
  const [platform, setPlatform] = useState<ScriptPlatform>(script?.platform ?? 'any');
  const [kind, setKind] = useState<ScriptKind>(script?.kind ?? 'script');
  const [argvText, setArgvText] = useState(script && script.kind === 'exec' ? script.argv.join('\n') : '');
  const [shell, setShell] = useState(script?.shell || 'sh');
  const [body, setBody] = useState(script?.body ?? '');
  const [timeout, setTimeout] = useState(String(script?.timeout_s ?? 300));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const argv = parseArgv(argvText);
  const draft: ScriptDraft = { name, platform, kind, argv, shell, body, timeout_s: Number(timeout) };
  const invalid = validateScript(draft);

  const submit = async (e: Event) => {
    e.preventDefault();
    const bad = validateScript(draft);
    if (bad) { setError(bad); return; }
    setBusy(true);
    setError('');
    const payload: NewScript = {
      org_id: orgId === '' ? null : Number(orgId), name: name.trim(), description: description.trim(),
      platform, kind, timeout_s: Number(timeout),
      ...(kind === 'exec' ? { argv } : { shell: shell.trim(), body }),
    };
    try {
      if (script) await api.updateScript(script.id, payload);
      else await api.createScript(payload);
      onSaved();
    } catch (err) {
      const t = errorText(err);
      setError(t.startsWith('forbidden') ? 'A global script needs access to all clients; pick a client instead.' : t);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card aut-form" onSubmit={submit}>
      <h2>{script ? 'Edit script' : 'New script'}</h2>
      <div class="form-row">
        <input type="text" placeholder="Name" value={name} maxLength={120} onInput={(e) => setName(e.currentTarget.value)} />
        <label>Client
          <select value={orgId} onChange={(e) => setOrgId(e.currentTarget.value)}>
            <option value="">All clients (global){isAdmin(user) ? '' : ' — needs all clients'}</option>
            {orgs.map((o) => <option key={o.id} value={String(o.id)}>{o.name}</option>)}
          </select>
        </label>
        <label>Platform
          <select value={platform} onChange={(e) => setPlatform(e.currentTarget.value as ScriptPlatform)}>
            <option value="any">Any desktop</option><option value="linux">Linux</option><option value="windows">Windows</option>
          </select>
        </label>
        <label>Kind
          <select value={kind} onChange={(e) => setKind(e.currentTarget.value as ScriptKind)}>
            <option value="script">script (shell + body)</option><option value="exec">exec (argv)</option>
          </select>
        </label>
        <label>Timeout (s)<input type="number" min={1} max={86400} value={timeout} onInput={(e) => setTimeout(e.currentTarget.value)} /></label>
      </div>
      <input type="text" class="full" placeholder="Description (optional)" value={description} maxLength={240} onInput={(e) => setDescription(e.currentTarget.value)} />
      {kind === 'exec' ? (
        <>
          <p class="muted">One argument per line (argv, run without a shell).</p>
          <textarea rows={4} value={argvText} placeholder={'uptime'} onInput={(e) => setArgvText(e.currentTarget.value)} />
          {argv.length > 0 && <p class="muted">Runs: <code>{argvPreview(argv)}</code></p>}
        </>
      ) : (
        <>
          <div class="form-row">
            <label>Shell<input type="text" value={shell} placeholder="sh / bash / powershell" onInput={(e) => setShell(e.currentTarget.value)} /></label>
          </div>
          <textarea rows={6} value={body} placeholder={'df -h\n'} onInput={(e) => setBody(e.currentTarget.value)} />
        </>
      )}
      {error && <p class="error">{error}</p>}
      <div class="form-row">
        <button type="submit" class="btn btn-primary" disabled={busy || invalid !== ''}>{busy ? 'Saving…' : script ? 'Save' : 'Create'}</button>
        <button type="button" class="btn" onClick={onClose}>Cancel</button>
        {invalid && <span class="muted">{invalid}</span>}
      </div>
    </form>
  );
}

/**
 * The Run-on-devices target picker: pick a client / status / platform filter and
 * tick the devices to run on. Shows how many will run vs be skipped (android /
 * wrong platform) using the same gate the server applies, then creates a job and
 * opens its progress view.
 */
function RunDialog({ script, orgs, onClose, onRan }: {
  script: Script; orgs: Org[]; onClose: () => void; onRan: (jobId: number) => void;
}) {
  const devices = usePoll(() => api.listDevices().then((r) => r.devices), [], 0);
  const [orgId, setOrgId] = useState(0);
  const [status, setStatus] = useState('');
  const [platform, setPlatform] = useState('');
  const [q, setQ] = useState('');
  const [checked, setChecked] = useState<Set<number>>(new Set());
  const [touched, setTouched] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const all = devices.data ?? [];
  let shown = filterDevices(all, q, status, orgId);
  if (platform) shown = shown.filter((d) => d.platform === platform);

  // Pre-check every shown device the first time the list loads (before the user
  // touches a box), so the common "run on all matching" case is one click.
  useEffect(() => {
    if (!touched && devices.data) setChecked(new Set(shown.map((d) => d.id)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [devices.data, q, status, platform, orgId]);

  const toggle = (id: number, on: boolean) => {
    setTouched(true);
    setChecked((prev) => { const n = new Set(prev); if (on) n.add(id); else n.delete(id); return n; });
  };
  const setAll = (on: boolean) => { setTouched(true); setChecked(on ? new Set(shown.map((d) => d.id)) : new Set()); };

  const selected = shown.filter((d) => checked.has(d.id));
  const runnable = selected.filter((d) => canRunScriptOn(script, d));
  const skipped = selected.length - runnable.length;

  const run = async () => {
    setBusy(true);
    setError('');
    try {
      const r = await api.createJob({ name: script.name, script_id: script.id, target: { device_ids: selected.map((d) => d.id) } });
      onRan(r.job_id);
    } catch (e) {
      setError(errorText(e));
      setBusy(false);
    }
  };

  return (
    <div class="card">
      <h2>Run “{script.name}” on devices</h2>
      <p class="muted">
        {script.kind} · {PLATFORM_LABEL[script.platform]} · {script.timeout_s}s timeout. Target:{' '}
        {targetSummary({ device_ids: selected.map((d) => d.id) }, orgs)}.
      </p>
      <div class="form-row">
        <select value={orgId} onChange={(e) => setOrgId(Number(e.currentTarget.value))}>
          <option value={0}>All clients</option>
          {orgs.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}
        </select>
        <select value={status} onChange={(e) => setStatus(e.currentTarget.value)}>
          <option value="">Any status</option><option value="online">Online</option><option value="offline">Offline</option><option value="pending">Pending</option>
        </select>
        <select value={platform} onChange={(e) => setPlatform(e.currentTarget.value)}>
          <option value="">Any platform</option><option value="linux">Linux</option><option value="windows">Windows</option><option value="android">Android</option>
        </select>
        <input type="search" placeholder="Search hostname" value={q} onInput={(e) => setQ(e.currentTarget.value)} />
        <button type="button" class="btn btn-sm" onClick={() => setAll(true)}>Select all</button>
        <button type="button" class="btn btn-sm" onClick={() => setAll(false)}>None</button>
      </div>
      {devices.error && <p class="error">{devices.error}</p>}
      {!devices.data ? <p class="muted">Loading devices…</p> : shown.length === 0 ? (
        <p class="empty">No devices match the filter.</p>
      ) : (
        <div class="apps-scroll">
          <table class="grid">
            <thead><tr><th></th><th>Hostname</th><th>Client</th><th>Platform</th><th>Status</th><th>Will run?</th></tr></thead>
            <tbody>
              {shown.map((d) => {
                const note = scriptSkipNote(script, d);
                return (
                  <tr key={d.id}>
                    <td data-label=""><input type="checkbox" checked={checked.has(d.id)} onChange={(e) => toggle(d.id, e.currentTarget.checked)} /></td>
                    <td data-label="Hostname"><a href={`#/devices/${d.id}`}>{d.hostname}</a></td>
                    <td data-label="Client" class="muted">{d.org_name}</td>
                    <td data-label="Platform">{d.platform}</td>
                    <td data-label="Status">{d.status}</td>
                    <td data-label="Will run?">{note ? <span class="warn" title={note}>skipped</span> : d.status === 'offline' ? <span class="muted">when online</span> : <span class="ok">yes</span>}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {error && <p class="error">{error}</p>}
      <div class="form-row">
        <button type="button" class="btn btn-primary" disabled={busy || runnable.length === 0} onClick={run}>
          {busy ? 'Queuing…' : `Run on ${runnable.length} device${runnable.length === 1 ? '' : 's'}`}
        </button>
        <button type="button" class="btn" onClick={onClose}>Cancel</button>
        <span class="muted">{selected.length} selected{skipped > 0 ? `, ${skipped} skipped` : ''}</span>
      </div>
    </div>
  );
}
