import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  canRunScriptOn, formatInterval, jobComplete, jobInFlight, jobProgressLabel, jobProgressPct,
  jobStatusText, jobTerminal, matchesTarget, MAX_INTERVAL_MINUTES, MAX_TIMEOUT_S, nextRunFrom,
  nextRunPreview, resolveTarget, scheduleSummary, scriptSkipNote, targetSummary, validateSchedule,
  validateScript, type ProgressLike, type ScriptDraft, type TargetDevice,
} from './automations.ts';
import type { Org, ScheduleSpec, TargetSpec } from '../types.ts';

const draft = (o: Partial<ScriptDraft> = {}): ScriptDraft => ({
  name: 'Uptime', platform: 'linux', kind: 'exec', argv: ['uptime'], shell: '', body: '', timeout_s: 60, ...o,
});

test('validateScript: a valid exec and a valid script pass', () => {
  assert.equal(validateScript(draft()), '');
  assert.equal(validateScript(draft({ kind: 'script', argv: [], shell: 'sh', body: 'df -h\n' })), '');
  assert.equal(validateScript(draft({ platform: 'any' })), '');
  assert.equal(validateScript(draft({ timeout_s: MAX_TIMEOUT_S })), '');
});

test('validateScript: name, platform and kind are checked', () => {
  assert.equal(validateScript(draft({ name: '  ' })), 'A name is required.');
  assert.match(validateScript(draft({ platform: 'android' })), /Platform must be/);
  assert.match(validateScript(draft({ kind: 'lock' })), /Kind must be/);
});

test('validateScript: exec needs argv, script needs shell and body', () => {
  assert.match(validateScript(draft({ kind: 'exec', argv: [] })), /exec script needs/);
  assert.match(validateScript(draft({ kind: 'exec', argv: ['  '] })), /exec script needs/);
  assert.match(validateScript(draft({ kind: 'script', argv: [], shell: '', body: 'x' })), /needs a shell/);
  assert.match(validateScript(draft({ kind: 'script', argv: [], shell: 'sh', body: '  ' })), /non-empty body/);
});

test('validateScript: timeout must be a whole number of seconds in range', () => {
  assert.match(validateScript(draft({ timeout_s: 0 })), /Timeout must be/);
  assert.match(validateScript(draft({ timeout_s: 1.5 })), /Timeout must be/);
  assert.match(validateScript(draft({ timeout_s: MAX_TIMEOUT_S + 1 })), /Timeout must be/);
});

test('scriptSkipNote / canRunScriptOn: android always skipped, platform must match', () => {
  assert.equal(scriptSkipNote({ platform: 'any', kind: 'exec' }, { platform: 'android' }), 'exec commands are not supported on android');
  assert.equal(scriptSkipNote({ platform: 'linux', kind: 'script' }, { platform: 'windows' }), 'script targets linux, device is windows');
  assert.equal(scriptSkipNote({ platform: 'linux', kind: 'exec' }, { platform: 'linux' }), '');
  assert.equal(scriptSkipNote({ platform: 'any', kind: 'exec' }, { platform: 'windows' }), '');
  assert.equal(canRunScriptOn({ platform: 'any', kind: 'exec' }, { platform: 'linux' }), true);
  assert.equal(canRunScriptOn({ platform: 'any', kind: 'exec' }, { platform: 'android' }), false);
});

const devs: TargetDevice[] = [
  { id: 1, org_id: 1, status: 'online', platform: 'linux', hostname: 'ws-reception-01' },
  { id: 2, org_id: 1, status: 'online', platform: 'windows', hostname: 'ws-xray' },
  { id: 3, org_id: 1, status: 'offline', platform: 'linux', hostname: 'ws-backoffice' },
  { id: 4, org_id: 2, status: 'online', platform: 'linux', hostname: 'srv-files' },
];

test('matchesTarget / resolveTarget: filter axes and explicit device_ids', () => {
  assert.deepEqual(resolveTarget(devs, {}).map((d) => d.id), [1, 2, 3, 4]);
  assert.deepEqual(resolveTarget(devs, { org_id: 1 }).map((d) => d.id), [1, 2, 3]);
  assert.deepEqual(resolveTarget(devs, { org_id: 1, platform: 'linux' }).map((d) => d.id), [1, 3]);
  assert.deepEqual(resolveTarget(devs, { status: 'online' }).map((d) => d.id), [1, 2, 4]);
  assert.deepEqual(resolveTarget(devs, { hostname_q: 'WS-' }).map((d) => d.id), [1, 2, 3]);
  // device_ids pins the set and ignores the other axes
  assert.deepEqual(resolveTarget(devs, { org_id: 2, device_ids: [1, 3] }).map((d) => d.id), [1, 3]);
  assert.equal(matchesTarget(devs[0], { org_id: 2 }), false);
});

test('targetSummary: filter axes, all-clients, named org, device set', () => {
  const orgs: Pick<Org, 'id' | 'name'>[] = [{ id: 1, name: 'Acme Dental' }, { id: 2, name: 'Zed Legal' }];
  assert.equal(targetSummary({}, orgs), 'All clients');
  assert.equal(targetSummary({ org_id: 1, status: 'online', platform: 'linux' }, orgs), 'Acme Dental · online · linux');
  assert.equal(targetSummary({ hostname_q: 'ws' }, orgs), 'All clients · matching “ws”');
  assert.equal(targetSummary({ org_id: 9 }, orgs), 'org #9');
  assert.equal(targetSummary({ device_ids: [1, 2, 3] }, orgs), '3 selected devices');
  assert.equal(targetSummary({ device_ids: [7] }, orgs), '1 selected device');
});

test('formatInterval: minutes, hours, days, plurals', () => {
  assert.equal(formatInterval(1), 'every 1 minute');
  assert.equal(formatInterval(15), 'every 15 minutes');
  assert.equal(formatInterval(60), 'every 1 hour');
  assert.equal(formatInterval(120), 'every 2 hours');
  assert.equal(formatInterval(1440), 'every 1 day');
  assert.equal(formatInterval(2880), 'every 2 days');
  assert.equal(formatInterval(90), 'every 90 minutes');
  assert.equal(formatInterval(0), '?');
});

test('scheduleSummary: interval, cron, empty', () => {
  assert.equal(scheduleSummary({ every_minutes: 30 }), 'every 30 minutes');
  assert.equal(scheduleSummary({ cron: '0 * * * *' } as ScheduleSpec), 'cron: 0 * * * *');
  assert.equal(scheduleSummary(null), '—');
  assert.equal(scheduleSummary({}), '—');
});

test('nextRunFrom / nextRunPreview: next = now + interval (missed ticks do not stack)', () => {
  const now = Date.parse('2026-06-01T10:00:00Z');
  assert.equal(nextRunFrom(15, now), Date.parse('2026-06-01T10:15:00Z'));
  assert.equal(nextRunPreview(15, now), '2026-06-01T10:15:00.000Z');
  assert.equal(nextRunPreview(0, now), '—');
  assert.equal(nextRunPreview(1.5, now), '—');
});

test('validateSchedule: whole minutes within bounds', () => {
  assert.equal(validateSchedule(1), '');
  assert.equal(validateSchedule(MAX_INTERVAL_MINUTES), '');
  assert.match(validateSchedule(0), /whole number of minutes/);
  assert.match(validateSchedule(1.5), /whole number of minutes/);
  assert.match(validateSchedule(MAX_INTERVAL_MINUTES + 1), /too large/);
});

const prog = (o: Partial<ProgressLike> = {}): ProgressLike => ({ total: 0, queued: 0, sent: 0, done: 0, failed: 0, ...o });

test('job progress math: in flight, terminal, complete', () => {
  const running = prog({ total: 4, queued: 1, sent: 1, done: 2, failed: 0 });
  assert.equal(jobInFlight(running), 2);
  assert.equal(jobTerminal(running), 2);
  assert.equal(jobComplete(running), false);
  const finished = prog({ total: 4, done: 3, failed: 1 });
  assert.equal(jobInFlight(finished), 0);
  assert.equal(jobComplete(finished), true);
  assert.equal(jobComplete(prog()), false); // empty job is not "complete"
});

test('jobProgressPct: terminal fraction, failures still count toward 100%', () => {
  assert.equal(jobProgressPct(prog({ total: 4, done: 1, failed: 0, queued: 3 })), 25);
  assert.equal(jobProgressPct(prog({ total: 4, done: 3, failed: 1 })), 100);
  assert.equal(jobProgressPct(prog({ total: 0 })), 0);
  assert.equal(jobProgressPct(prog({ total: 3, done: 1 })), 33);
});

test('jobProgressLabel / jobStatusText', () => {
  assert.equal(jobProgressLabel(prog({ total: 4, done: 2 })), '2 of 4 done');
  assert.equal(jobStatusText(prog({ total: 0 })), 'no devices');
  assert.equal(jobStatusText(prog({ total: 4, sent: 2, done: 2 })), 'running');
  assert.equal(jobStatusText(prog({ total: 4, done: 4 })), 'done');
  assert.equal(jobStatusText(prog({ total: 4, done: 3, failed: 1 })), 'done, 1 failed');
});
