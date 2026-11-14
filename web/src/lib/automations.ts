// AUT1 script library, bulk jobs and automations: the pure logic the Scripts,
// Jobs and Automations views share — the target-filter summary and resolution,
// the schedule / next-run preview, the job-progress math, and script
// platform/kind validation. No DOM, no network; tested by automations.test.ts
// with `node --test`. These mirror the server's rules (internal/store scriptArgs
// / scriptSkipNote, DueAutomations' next = now + every_minutes, and JobProgress'
// status folding) so the dashboard preview matches what the server does.

import type { Org, ScheduleSpec, TargetSpec } from '../types.ts';

/** Longest a script may run, in seconds (24 h); the editor caps timeout here. */
export const MAX_TIMEOUT_S = 86400;
/** Interval bounds for a schedule automation, in minutes (1 min .. 4 weeks). */
export const MIN_INTERVAL_MINUTES = 1;
export const MAX_INTERVAL_MINUTES = 40320;

// --- platform / kind validation ---------------------------------------------

/** The fields of the script editor that validateScript checks (argv already parsed). */
export interface ScriptDraft {
  name: string;
  platform: string;
  kind: string;
  argv: string[];
  shell: string;
  body: string;
  timeout_s: number;
}

/**
 * Validate a script-library draft, mirroring the server's shape check: a name;
 * platform one of linux/windows/any; kind one of exec/script; an exec carries a
 * non-empty argv; a script carries a shell and a non-empty body; the timeout a
 * whole number of seconds in [1, MAX_TIMEOUT_S]. Returns '' when valid, else a
 * one-line human reason.
 */
export function validateScript(d: ScriptDraft): string {
  if (!d.name.trim()) return 'A name is required.';
  if (d.platform !== 'linux' && d.platform !== 'windows' && d.platform !== 'any') {
    return 'Platform must be Linux, Windows or Any.';
  }
  if (d.kind !== 'exec' && d.kind !== 'script') return 'Kind must be exec or script.';
  if (d.kind === 'exec') {
    if (d.argv.filter((a) => a.trim() !== '').length === 0) return 'An exec script needs at least one argument.';
  } else {
    if (!d.shell.trim()) return 'A script needs a shell (e.g. sh, bash, powershell).';
    if (!d.body.trim()) return 'A script needs a non-empty body.';
  }
  if (!Number.isInteger(d.timeout_s) || d.timeout_s < 1 || d.timeout_s > MAX_TIMEOUT_S) {
    return `Timeout must be a whole number of seconds between 1 and ${MAX_TIMEOUT_S}.`;
  }
  return '';
}

/** A device as far as a script fan-out is concerned. */
export interface TargetDevice {
  id: number;
  org_id: number;
  status: string;
  platform: string;
  hostname: string;
}

/** A script's platform + kind, the two axes that decide whether a device can run it. */
export interface ScriptGate {
  platform: string;
  kind: string;
}

/**
 * Why a script cannot run on a device, or '' when it can — mirrors the server's
 * scriptSkipNote. exec/script are desktop kinds, so android is always skipped;
 * a linux/windows script is skipped on the other platform. 'any' runs on both
 * desktops.
 */
export function scriptSkipNote(script: ScriptGate, device: { platform: string }): string {
  if (device.platform === 'android') return `${script.kind} commands are not supported on android`;
  if (script.platform !== 'any' && script.platform !== device.platform) {
    return `script targets ${script.platform}, device is ${device.platform}`;
  }
  return '';
}

/** Whether a script can run on a device (no skip note). */
export function canRunScriptOn(script: ScriptGate, device: { platform: string }): boolean {
  return scriptSkipNote(script, device) === '';
}

// --- target filter -----------------------------------------------------------

/**
 * Whether a device matches a target filter. An explicit device_ids list pins the
 * set (the other axes are ignored); otherwise org_id/status/platform narrow it and
 * hostname_q is a case-insensitive substring. An empty / unset axis does not
 * narrow.
 */
export function matchesTarget(d: TargetDevice, t: TargetSpec): boolean {
  if (t.device_ids && t.device_ids.length > 0) return t.device_ids.includes(d.id);
  if (t.org_id !== null && t.org_id !== undefined && d.org_id !== t.org_id) return false;
  if (t.status && d.status !== t.status) return false;
  if (t.platform && d.platform !== t.platform) return false;
  if (t.hostname_q && !d.hostname.toLowerCase().includes(t.hostname_q.trim().toLowerCase())) return false;
  return true;
}

/** The devices a target resolves to, preserving input order. */
export function resolveTarget<T extends TargetDevice>(devices: T[], t: TargetSpec): T[] {
  return devices.filter((d) => matchesTarget(d, t));
}

/**
 * A one-line human summary of a target filter for the picker and automation rows,
 * e.g. "All clients", "Acme Dental · online · linux", "matching “ws”", or
 * "3 selected devices" when an explicit device set is pinned.
 */
export function targetSummary(t: TargetSpec, orgs: Pick<Org, 'id' | 'name'>[] = []): string {
  if (t.device_ids && t.device_ids.length > 0) {
    const n = t.device_ids.length;
    return `${n} selected device${n === 1 ? '' : 's'}`;
  }
  const parts: string[] = [];
  if (t.org_id !== null && t.org_id !== undefined) {
    const o = orgs.find((o) => o.id === t.org_id);
    parts.push(o ? o.name : `org #${t.org_id}`);
  } else {
    parts.push('All clients');
  }
  if (t.status) parts.push(t.status);
  if (t.platform) parts.push(t.platform);
  if (t.hostname_q && t.hostname_q.trim()) parts.push(`matching “${t.hostname_q.trim()}”`);
  return parts.join(' · ');
}

// --- schedule / next-run preview --------------------------------------------

/**
 * A compact interval label: "every 15 minutes", "every 1 hour", "every 2 days".
 * Uses whole days/hours when the interval divides evenly, else minutes. '?' for a
 * non-positive or non-finite value.
 */
export function formatInterval(mins: number): string {
  if (!Number.isFinite(mins) || mins <= 0) return '?';
  if (mins % 1440 === 0) { const d = mins / 1440; return `every ${d} day${d === 1 ? '' : 's'}`; }
  if (mins % 60 === 0) { const h = mins / 60; return `every ${h} hour${h === 1 ? '' : 's'}`; }
  return `every ${mins} minute${mins === 1 ? '' : 's'}`;
}

/**
 * A schedule's one-line summary for the Automations list: the interval, or a cron
 * string when one is set, or '—' for an empty / missing schedule.
 */
export function scheduleSummary(spec: ScheduleSpec | null | undefined): string {
  if (!spec) return '—';
  if (spec.cron && spec.cron.trim()) return `cron: ${spec.cron.trim()}`;
  if (spec.every_minutes && spec.every_minutes > 0) return formatInterval(spec.every_minutes);
  return '—';
}

/**
 * The next run time (ms since epoch) for an interval schedule, computed from `now`
 * — never by advancing a stale schedule — so missed ticks do not stack, matching
 * the server scheduler (next = now + every_minutes).
 */
export function nextRunFrom(everyMinutes: number, now: number = Date.now()): number {
  return now + everyMinutes * 60000;
}

/**
 * An ISO preview of the next run for the schedule editor, or '—' when the interval
 * is not a positive whole number of minutes.
 */
export function nextRunPreview(everyMinutes: number, now: number = Date.now()): string {
  if (!Number.isInteger(everyMinutes) || everyMinutes <= 0) return '—';
  return new Date(nextRunFrom(everyMinutes, now)).toISOString();
}

/** Validate a schedule interval, matching the editor's bounds. '' when valid. */
export function validateSchedule(everyMinutes: number): string {
  if (!Number.isInteger(everyMinutes) || everyMinutes < MIN_INTERVAL_MINUTES) {
    return 'Run every must be a whole number of minutes (1 or more).';
  }
  if (everyMinutes > MAX_INTERVAL_MINUTES) return 'Run every is too large (at most 4 weeks).';
  return '';
}

// --- job progress math -------------------------------------------------------

/** The four status counts a job's progress carries (queued/sent in flight, done/failed terminal). */
export interface ProgressLike {
  total: number;
  queued: number;
  sent: number;
  done: number;
  failed: number;
}

/** Commands still in flight (queued + sent). Negative counts are clamped to 0. */
export function jobInFlight(p: ProgressLike): number {
  return Math.max(0, p.queued) + Math.max(0, p.sent);
}

/** Commands in a terminal state (done + failed). Negative counts are clamped to 0. */
export function jobTerminal(p: ProgressLike): number {
  return Math.max(0, p.done) + Math.max(0, p.failed);
}

/** Whether every command has reached a terminal state (nothing in flight). */
export function jobComplete(p: ProgressLike): boolean {
  return p.total > 0 && jobInFlight(p) === 0;
}

/** Terminal fraction as a 0..100 integer percent; 0 for an empty job. */
export function jobProgressPct(p: ProgressLike): number {
  if (p.total <= 0) return 0;
  return Math.min(100, Math.max(0, Math.round((jobTerminal(p) / p.total) * 100)));
}

/** The "n of N done" label for the job progress view. */
export function jobProgressLabel(p: ProgressLike): string {
  return `${Math.max(0, p.done)} of ${Math.max(0, p.total)} done`;
}

/**
 * A short status word for a job: "no devices" for an empty job, "running" while
 * anything is still in flight, "done" when all succeeded, or "done, N failed"
 * when some failed.
 */
export function jobStatusText(p: ProgressLike): string {
  if (p.total <= 0) return 'no devices';
  if (!jobComplete(p)) return 'running';
  return p.failed > 0 ? `done, ${p.failed} failed` : 'done';
}
