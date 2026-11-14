import { api } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { ago, when } from '../lib/format.ts';
import { jobComplete, jobProgressLabel, jobProgressPct, jobStatusText } from '../lib/automations.ts';
import type { Job, JobDeviceRow } from '../types.ts';

const ORIGIN_LABEL: Record<Job['origin'], string> = { manual: 'Bulk run', schedule: 'Scheduled', trigger: 'Triggered' };

/** A command-status badge reusing the console's status colours (st-*). */
function StatusCell({ row }: { row: JobDeviceRow }) {
  if (row.skipped) return <span class="muted" title={row.note}>skipped</span>;
  if (row.status === null) return <span class="muted">—</span>;
  return <span class={`badge st-${row.status}`}>{row.status}</span>;
}

/** The jobs list (AUT1): every fan-out a bulk run or automation produced, newest first. */
export function Jobs() {
  const jobs = usePoll(() => api.listJobs().then((r) => r.jobs), [], 10000);
  return (
    <section>
      <div class="toolbar">
        <h1>Jobs</h1>
        <a class="btn" href="#/scripts">Scripts</a>
      </div>
      <p class="muted">A job is one run of a script across a set of devices — a bulk run, or a run an automation produced. Open one for its live per-device progress.</p>
      {jobs.error && <p class="error">{jobs.error}</p>}
      {!jobs.data ? <p class="muted">Loading…</p> : jobs.data.length === 0 ? (
        <p class="empty">No jobs yet. Run a script on devices from the Scripts page.</p>
      ) : (
        <div class="log-scroll">
          <table class="grid">
            <thead><tr><th>Name</th><th>Origin</th><th>Devices</th><th>Created</th><th></th></tr></thead>
            <tbody>
              {jobs.data.map((j) => (
                <tr key={j.id}>
                  <td data-label="Name"><a href={`#/jobs/${j.id}`}>{j.name}</a></td>
                  <td data-label="Origin"><span class="chip">{ORIGIN_LABEL[j.origin]}</span></td>
                  <td data-label="Devices">{j.total}</td>
                  <td data-label="Created" class="muted" title={when(j.created_at)}>{ago(j.created_at)}</td>
                  <td data-label=""><a class="btn btn-sm" href={`#/jobs/${j.id}`}>Progress</a></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

/** One job's progress view (AUT1): n done / total, the bar, and per-device status. */
export function JobView({ id }: { id: number }) {
  // Poll while the job is running; usePoll keeps the last data between ticks.
  const job = usePoll(() => api.getJob(id), [id], 3000);
  const d = job.data;
  const complete = d ? jobComplete(d.progress) : false;
  const pctClass = d && d.progress.failed > 0 ? ' bar-crit' : '';

  return (
    <section>
      <div class="toolbar">
        <h1>Job</h1>
        <a class="btn" href="#/jobs">All jobs</a>
      </div>
      {job.error && <p class="error">{job.error}</p>}
      {!d ? <p class="muted">Loading…</p> : (
        <>
          <div class="card">
            <h2>{d.job.name}</h2>
            <p class="muted">
              {ORIGIN_LABEL[d.job.origin]} · {d.job.total} device{d.job.total === 1 ? '' : 's'} · created {when(d.job.created_at)}
              {d.job.automation_id !== null && <> · <a href="#/automations">from an automation</a></>}
            </p>
            <div class="form-row">
              <span class={`bar${pctClass}`} style={{ width: '200px' }}><span class="bar-fill" style={{ width: `${jobProgressPct(d.progress)}%` }} /></span>
              <strong>{jobProgressLabel(d.progress)}</strong>
              <span class={complete ? (d.progress.failed > 0 ? 'warn' : 'ok') : 'muted'}>{jobStatusText(d.progress)}</span>
            </div>
            <dl class="kv header">
              <dt>Queued</dt><dd>{d.progress.queued}</dd>
              <dt>Sent</dt><dd>{d.progress.sent}</dd>
              <dt>Done</dt><dd class="ok">{d.progress.done}</dd>
              <dt>Failed</dt><dd class={d.progress.failed > 0 ? 'warn' : ''}>{d.progress.failed}</dd>
            </dl>
          </div>

          <h2>Devices</h2>
          {d.devices.length === 0 ? <p class="empty">This job queued no commands.</p> : (
            <div class="log-scroll">
              <table class="grid">
                <thead><tr><th>Device</th><th>Client</th><th>Status</th><th>Detail</th></tr></thead>
                <tbody>
                  {d.devices.map((row) => (
                    <tr key={row.device_id}>
                      <td data-label="Device"><a href={`#/devices/${row.device_id}`}>{row.hostname}</a></td>
                      <td data-label="Client" class="muted">{row.org_name}</td>
                      <td data-label="Status"><StatusCell row={row} /></td>
                      <td data-label="Detail" class="muted">{row.skipped ? row.note : row.command_id !== null ? <a href={`#/devices/${row.device_id}`}>command #{row.command_id}</a> : '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  );
}
