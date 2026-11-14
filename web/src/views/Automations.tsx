import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { ago, fromNow, when } from '../lib/format.ts';
import { nextRunPreview, scheduleSummary, targetSummary, validateSchedule } from '../lib/automations.ts';
import type {
  AlertTriggerSpec, Automation, AutomationTrigger, NewAutomation, Org, Script, Severity, TargetSpec, User,
} from '../types.ts';
import { ALERT_TYPES, isAdmin } from '../types.ts';
import { ConfirmButton } from '../components/Confirm.tsx';

const SEVERITIES: Severity[] = ['low', 'medium', 'high'];

/** The name of a client org by id. */
function orgName(orgs: Org[], id: number | null): string {
  if (id === null) return 'All clients';
  return orgs.find((o) => o.id === id)?.name ?? `#${id}`;
}

/** The one-line trigger summary for a row: the interval, or the alert matcher. */
function triggerSummary(a: Automation): string {
  if (a.trigger === 'schedule') return scheduleSummary(a.schedule);
  const sev = a.alert?.min_severity ? `${a.alert.min_severity}+` : 'any severity';
  const type = a.alert?.alert_type || 'any alert';
  return `on ${type} · ${sev}`;
}

/**
 * The Automations page (AUT1, admin/operator): CRUD rules that run a library
 * script on a target — on a schedule (every N minutes) or when a matching alert
 * opens — with enable/disable and the last/next run. Each run produces a job,
 * reachable from the Jobs page. Org-scoped; a global automation needs all clients.
 */
export function Automations({ user }: { user: User }) {
  const automations = usePoll(() => api.listAutomations().then((r) => r.automations), [], 15000);
  const scripts = usePoll(() => api.listScripts().then((r) => r.scripts), [], 0);
  const orgs = usePoll(() => api.listOrgs().then((r) => r.orgs), [], 0);
  const [editing, setEditing] = useState<Automation | 'new' | null>(null);
  const [error, setError] = useState('');

  const orgList = orgs.data ?? [];
  const scriptList = scripts.data ?? [];
  const scriptName = (id: number) => scriptList.find((s) => s.id === id)?.name ?? `script #${id}`;

  const toggle = async (a: Automation) => {
    setError('');
    try { await api.setAutomationEnabled(a.id, !a.enabled); automations.reload(); } catch (e) { setError(errorText(e)); }
  };
  const remove = async (a: Automation) => {
    setError('');
    try { await api.deleteAutomation(a.id); automations.reload(); } catch (e) { setError(errorText(e)); }
  };

  return (
    <section>
      <div class="toolbar">
        <h1>Automations</h1>
        <button type="button" class="btn btn-primary" disabled={scriptList.length === 0} onClick={() => setEditing('new')}>New automation</button>
        <a class="btn" href="#/scripts">Scripts</a>
      </div>
      <p class="muted">
        Run a library script automatically — on a schedule (every N minutes) or when a matching alert opens. Each run
        produces a job you can follow on the Jobs page. A missed schedule tick does not stack; an alert trigger runs on
        the alerting device with a short per-device cooldown.
      </p>
      {scriptList.length === 0 && <p class="muted">Create a <a href="#/scripts">script</a> first — an automation runs a saved script.</p>}
      {error && <p class="error">{error}</p>}
      {automations.error && <p class="error">{automations.error}</p>}

      {editing && (
        <AutomationForm user={user} orgs={orgList} scripts={scriptList}
          automation={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)} onSaved={() => { setEditing(null); automations.reload(); }} />
      )}

      <h2>Rules</h2>
      {!automations.data ? <p class="muted">Loading…</p> : automations.data.length === 0 ? (
        <p class="empty">No automations yet. Create one above.</p>
      ) : (
        <div class="log-scroll">
          <table class="grid">
            <thead>
              <tr><th>Name</th><th>Trigger</th><th>Script</th><th>Target</th><th>Client</th><th>Last run</th><th>Next run</th><th></th></tr>
            </thead>
            <tbody>
              {automations.data.map((a) => (
                <tr key={a.id} class={a.enabled ? '' : 'revoked'}>
                  <td data-label="Name">{a.name}{!a.enabled && <span class="chip">disabled</span>}</td>
                  <td data-label="Trigger"><span class="chip">{a.trigger}</span> {triggerSummary(a)}</td>
                  <td data-label="Script">{scriptName(a.script_id)}</td>
                  <td data-label="Target" class="muted">{targetSummary(a.target, orgList)}</td>
                  <td data-label="Client">{orgName(orgList, a.org_id)}</td>
                  <td data-label="Last run" class="muted" title={a.last_run_at ? when(a.last_run_at) : ''}>{a.last_run_at ? ago(a.last_run_at) : 'never'}</td>
                  <td data-label="Next run" class="muted" title={a.next_run_at ? when(a.next_run_at) : ''}>{a.trigger === 'alert' ? 'on alert' : a.next_run_at ? fromNow(a.next_run_at) : '—'}</td>
                  <td data-label="">
                    <div class="row-actions">
                      <button type="button" class="btn btn-sm" onClick={() => toggle(a)}>{a.enabled ? 'Disable' : 'Enable'}</button>
                      <button type="button" class="btn btn-sm" onClick={() => setEditing(a)}>Edit</button>
                      <ConfirmButton label="Delete" question={`Delete ${a.name}? Jobs it produced keep their history.`} danger onConfirm={() => remove(a)} />
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

/** The create/edit form for an automation. */
function AutomationForm({ user, orgs, scripts, automation, onClose, onSaved }: {
  user: User; orgs: Org[]; scripts: Script[]; automation: Automation | null; onClose: () => void; onSaved: () => void;
}) {
  const [name, setName] = useState(automation?.name ?? '');
  const [orgId, setOrgId] = useState(automation ? (automation.org_id === null ? '' : String(automation.org_id)) : '');
  const [trigger, setTrigger] = useState<AutomationTrigger>(automation?.trigger ?? 'schedule');
  const [scriptId, setScriptId] = useState(String(automation?.script_id ?? scripts[0]?.id ?? ''));
  const [enabled, setEnabled] = useState(automation?.enabled ?? true);
  // target filter
  const t = automation?.target ?? {};
  const [tOrg, setTOrg] = useState(t.org_id === null || t.org_id === undefined ? '' : String(t.org_id));
  const [tStatus, setTStatus] = useState(t.status ?? '');
  const [tPlatform, setTPlatform] = useState(t.platform ?? '');
  const [tHost, setTHost] = useState(t.hostname_q ?? '');
  // schedule / alert
  const [everyMinutes, setEveryMinutes] = useState(String(automation?.schedule?.every_minutes ?? 60));
  const [alertType, setAlertType] = useState(automation?.alert?.alert_type ?? '');
  const [minSeverity, setMinSeverity] = useState<string>(automation?.alert?.min_severity ?? '');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const scheduleError = trigger === 'schedule' ? validateSchedule(Number(everyMinutes)) : '';

  const buildTarget = (): TargetSpec => ({
    ...(tOrg === '' ? {} : { org_id: Number(tOrg) }),
    ...(tStatus ? { status: tStatus } : {}),
    ...(tPlatform ? { platform: tPlatform } : {}),
    ...(tHost.trim() ? { hostname_q: tHost.trim() } : {}),
  });

  const submit = async (e: Event) => {
    e.preventDefault();
    if (!name.trim()) { setError('A name is required.'); return; }
    if (!scriptId) { setError('Choose a script.'); return; }
    if (trigger === 'schedule' && scheduleError) { setError(scheduleError); return; }
    setBusy(true);
    setError('');
    const alert: AlertTriggerSpec = {
      ...(alertType ? { alert_type: alertType } : {}),
      ...(minSeverity ? { min_severity: minSeverity as Severity } : {}),
    };
    const payload: NewAutomation = {
      org_id: orgId === '' ? null : Number(orgId), name: name.trim(), enabled, trigger, script_id: Number(scriptId),
      target: buildTarget(),
      ...(trigger === 'schedule' ? { schedule: { every_minutes: Number(everyMinutes) } } : { alert }),
    };
    try {
      if (automation) await api.updateAutomation(automation.id, payload);
      else await api.createAutomation(payload);
      onSaved();
    } catch (err) {
      const msg = errorText(err);
      setError(msg.startsWith('forbidden') ? 'A global automation needs access to all clients; pick a client instead.' : msg);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card aut-form" onSubmit={submit}>
      <h2>{automation ? 'Edit automation' : 'New automation'}</h2>
      <div class="form-row">
        <input type="text" placeholder="Name" value={name} maxLength={120} onInput={(e) => setName(e.currentTarget.value)} />
        <label>Client
          <select value={orgId} onChange={(e) => setOrgId(e.currentTarget.value)}>
            <option value="">All clients (global){isAdmin(user) ? '' : ' — needs all clients'}</option>
            {orgs.map((o) => <option key={o.id} value={String(o.id)}>{o.name}</option>)}
          </select>
        </label>
        <label>Script
          <select value={scriptId} onChange={(e) => setScriptId(e.currentTarget.value)}>
            {scripts.map((s) => <option key={s.id} value={String(s.id)}>{s.name} ({s.platform}/{s.kind})</option>)}
          </select>
        </label>
        <label><input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.currentTarget.checked)} /> Enabled</label>
      </div>

      <div class="form-row">
        <span class="muted">Trigger:</span>
        <label><input type="radio" name="trigger" checked={trigger === 'schedule'} onChange={() => setTrigger('schedule')} /> Schedule</label>
        <label><input type="radio" name="trigger" checked={trigger === 'alert'} onChange={() => setTrigger('alert')} /> On alert</label>
      </div>

      {trigger === 'schedule' ? (
        <div class="form-row">
          <label>Run every (minutes)<input type="number" min={1} max={40320} value={everyMinutes} onInput={(e) => setEveryMinutes(e.currentTarget.value)} /></label>
          <span class="muted">{scheduleError ? scheduleError : `next run ${fromNow(nextRunPreview(Number(everyMinutes)))}`}</span>
        </div>
      ) : (
        <div class="form-row">
          <label>Alert type
            <select value={alertType} onChange={(e) => setAlertType(e.currentTarget.value)}>
              <option value="">Any type</option>
              {ALERT_TYPES.map((at) => <option key={at} value={at}>{at}</option>)}
            </select>
          </label>
          <label>Minimum severity
            <select value={minSeverity} onChange={(e) => setMinSeverity(e.currentTarget.value)}>
              <option value="">Any</option>
              {SEVERITIES.map((s) => <option key={s} value={s}>{s}</option>)}
            </select>
          </label>
          <span class="muted">Runs the script on the alerting device.</span>
        </div>
      )}

      <h3>Target</h3>
      {trigger === 'alert' ? (
        <p class="muted">An alert trigger always runs on the alerting device; the filter below further limits which alerts count by client.</p>
      ) : (
        <p class="muted">The devices the script runs on each tick (resolved at run time; android is skipped for exec/script).</p>
      )}
      <div class="form-row">
        <label>Client
          <select value={tOrg} onChange={(e) => setTOrg(e.currentTarget.value)}>
            <option value="">All visible</option>
            {orgs.map((o) => <option key={o.id} value={String(o.id)}>{o.name}</option>)}
          </select>
        </label>
        <label>Status
          <select value={tStatus} onChange={(e) => setTStatus(e.currentTarget.value)}>
            <option value="">Any</option><option value="online">Online</option><option value="offline">Offline</option><option value="pending">Pending</option>
          </select>
        </label>
        <label>Platform
          <select value={tPlatform} onChange={(e) => setTPlatform(e.currentTarget.value)}>
            <option value="">Any</option><option value="linux">Linux</option><option value="windows">Windows</option>
          </select>
        </label>
        <input type="search" placeholder="Hostname contains…" value={tHost} onInput={(e) => setTHost(e.currentTarget.value)} />
      </div>
      <p class="muted">Target: {targetSummary(buildTarget(), orgs)}</p>

      {error && <p class="error">{error}</p>}
      <div class="form-row">
        <button type="submit" class="btn btn-primary" disabled={busy || (trigger === 'schedule' && scheduleError !== '')}>{busy ? 'Saving…' : automation ? 'Save' : 'Create'}</button>
        <button type="button" class="btn" onClick={onClose}>Cancel</button>
      </div>
    </form>
  );
}
