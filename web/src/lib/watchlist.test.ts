import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  appliesToDevice, applicableItems, distinctUnits, filterLogs, hasStoppedService, levelClass,
  levelLabel, levelSeverity, logSourceLabel, logSummary, mergeWatched, serviceStateCounts,
  serviceSummary, servicesOf, stoppedServiceCount, type DeviceLog, type ServiceState, type WatchlistItem,
} from './watchlist.ts';

const item = (o: Partial<WatchlistItem>): WatchlistItem => ({
  id: 1, org_id: 1, org_name: 'Acme', scope: 'org', device_id: null, hostname: '',
  kind: 'service', name: 'ssh.service', alert: true, created_at: '2025-01-01T00:00:00Z', created_by: 1, ...o,
});

test('servicesOf: null without an array, typed entries, bad entries skipped, services key unwrapped', () => {
  assert.equal(servicesOf({ kernel: '6.8' }), null);
  assert.equal(servicesOf('nope'), null);
  assert.equal(servicesOf(undefined), null);
  assert.deepEqual(servicesOf([]), []);
  // Accepts a bare array or a facts object carrying a `services` array.
  const fromFacts = servicesOf({ kernel: '6.8', services: [{ kind: 'service', name: 'ssh.service', running: true }] });
  assert.deepEqual(fromFacts, [{ kind: 'service', name: 'ssh.service', running: true }]);
  assert.deepEqual(servicesOf([
    { kind: 'service', name: 'ssh.service', running: true },
    { kind: 'process', name: 'nginx', running: false },
    { kind: 'daemon', name: 'weird' }, // unknown kind -> service, running defaults false
    { name: '' }, 'junk', null, 7, { running: true }, // all skipped (no valid name)
  ]), [
    { kind: 'service', name: 'ssh.service', running: true },
    { kind: 'process', name: 'nginx', running: false },
    { kind: 'service', name: 'weird', running: false },
  ]);
});

test('serviceStateCounts and serviceSummary', () => {
  assert.deepEqual(serviceStateCounts([]), { total: 0, running: 0, stopped: 0, unknown: 0 });
  const mix: { running: boolean | null }[] = [{ running: true }, { running: false }, { running: null }, { running: true }];
  assert.deepEqual(serviceStateCounts(mix), { total: 4, running: 2, stopped: 1, unknown: 1 });
  assert.equal(serviceSummary([]), 'No watched services');
  assert.equal(serviceSummary([{ running: true }, { running: true }]), 'All 2 running');
  assert.equal(serviceSummary(mix), '1 of 4 stopped');
});

const device = { id: 7, org_id: 1, platform: 'linux' };
const items: WatchlistItem[] = [
  item({ id: 10, scope: 'org', org_id: 1, kind: 'service', name: 'ssh.service', alert: true }),
  item({ id: 11, scope: 'org', org_id: 2, kind: 'service', name: 'nginx.service', alert: true }), // other org
  item({ id: 12, scope: 'device', org_id: 1, device_id: 7, kind: 'process', name: 'cron', alert: false }),
  item({ id: 13, scope: 'device', org_id: 1, device_id: 8, kind: 'service', name: 'mysql', alert: true }), // other device
];

test('appliesToDevice / applicableItems: org by org, device by id, android never, sorted', () => {
  assert.equal(appliesToDevice(items[0], device), true);
  assert.equal(appliesToDevice(items[1], device), false); // org 2
  assert.equal(appliesToDevice(items[2], device), true);
  assert.equal(appliesToDevice(items[3], device), false); // device 8
  assert.equal(appliesToDevice(items[0], { id: 7, org_id: 1, platform: 'android' }), false);
  // kind then name: process/cron before service/ssh.service.
  assert.deepEqual(applicableItems(items, device).map((i) => i.id), [12, 10]);
  const copy = [...items];
  applicableItems(items, device);
  assert.deepEqual(items, copy); // not mutated
});

test('mergeWatched: overlays reported running, null when unreported, ignores unmatched state', () => {
  const states: ServiceState[] = [
    { kind: 'service', name: 'ssh.service', running: false },
    { kind: 'service', name: 'ghost', running: true }, // not an item: ignored
  ];
  const rows = mergeWatched(applicableItems(items, device), states);
  assert.deepEqual(rows, [
    { kind: 'process', name: 'cron', alert: false, running: null }, // no state reported
    { kind: 'service', name: 'ssh.service', alert: true, running: false },
  ]);
});

test('levelSeverity / levelClass / levelLabel across journald + eventlog', () => {
  assert.equal(levelSeverity('err'), 'error');
  assert.equal(levelSeverity('CRITICAL'), 'error');
  assert.equal(levelSeverity('  Warning '), 'warning');
  assert.equal(levelSeverity('Information'), 'info');
  assert.equal(levelSeverity('notice'), 'info');
  assert.equal(levelSeverity('debug'), 'debug');
  assert.equal(levelSeverity('bogus'), 'other');
  assert.equal(levelClass('err'), 'lvl-error');
  assert.equal(levelClass(''), 'lvl-other');
  assert.equal(levelLabel('warning'), 'WARNING');
  assert.equal(levelLabel('  '), '—');
});

test('logSourceLabel', () => {
  assert.equal(logSourceLabel('journald'), 'journald (Linux)');
  assert.equal(logSourceLabel('eventlog'), 'Windows Event Log');
  assert.equal(logSourceLabel('syslog'), 'syslog');
  assert.equal(logSourceLabel(''), 'unknown');
});

const log = (o: Partial<DeviceLog>): DeviceLog => ({
  id: 1, device_id: 7, org_id: 1, at: '2025-01-01T00:00:00Z', source: 'journald', unit: '', level: 'warning', message: 'm', ...o,
});
const logs: DeviceLog[] = [
  log({ id: 1, unit: 'ssh.service', level: 'err', message: 'auth failed' }),
  log({ id: 2, unit: 'cron.service', level: 'warning', message: 'slow run' }),
  log({ id: 3, unit: 'ssh.service', level: 'crit', message: 'segfault' }),
  log({ id: 4, unit: '', level: 'notice', message: 'started' }),
];

test('distinctUnits: sorted, blanks dropped', () => {
  assert.deepEqual(distinctUnits(logs), ['cron.service', 'ssh.service']);
  assert.deepEqual(distinctUnits([]), []);
});

test('filterLogs: by unit and severity bucket, order preserved, input not mutated', () => {
  assert.deepEqual(filterLogs(logs, {}).map((l) => l.id), [1, 2, 3, 4]);
  assert.deepEqual(filterLogs(logs, { unit: 'ssh.service' }).map((l) => l.id), [1, 3]);
  assert.deepEqual(filterLogs(logs, { severity: 'error' }).map((l) => l.id), [1, 3]); // err + crit
  assert.deepEqual(filterLogs(logs, { unit: 'ssh.service', severity: 'warning' }), []);
  const copy = [...logs];
  filterLogs(logs, { severity: 'error' });
  assert.deepEqual(logs, copy);
});

test('logSummary counts entries and errors', () => {
  assert.equal(logSummary([]), 'No log entries');
  assert.equal(logSummary([logs[1]]), '1 entry');
  assert.equal(logSummary(logs), '4 entries, 2 errors');
});

test('stoppedServiceCount / hasStoppedService tolerate missing field', () => {
  assert.equal(stoppedServiceCount({}), 0);
  assert.equal(stoppedServiceCount({ services_down: 0 }), 0);
  assert.equal(stoppedServiceCount({ services_down: 3 }), 3);
  assert.equal(stoppedServiceCount({ services_down: null }), 0);
  assert.equal(hasStoppedService({ services_down: 2 }), true);
  assert.equal(hasStoppedService({}), false);
});
