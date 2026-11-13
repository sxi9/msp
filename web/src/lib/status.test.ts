import { test } from 'node:test';
import assert from 'node:assert/strict';
import { sortDevices, groupByOrg, filterDevices } from './status.ts';

const dev = (id: number, org_id: number, org_name: string, hostname: string, status: string, open_alerts = 0) =>
  ({ id, org_id, org_name, hostname, status, open_alerts });

const fleet = [
  dev(1, 2, 'Zed Legal', 'pc-b', 'online'),
  dev(2, 1, 'Acme Dental', 'ws-02', 'pending'),
  dev(3, 1, 'Acme Dental', 'ws-01', 'offline'),
  dev(4, 2, 'Zed Legal', 'pc-a', 'online', 1),
  dev(5, 1, 'Acme Dental', 'ws-03', 'online'),
];

test('sortDevices: alerting, offline, online, pending; then org, hostname', () => {
  const ids = sortDevices(fleet).map((d) => d.id);
  assert.deepEqual(ids, [4, 3, 5, 1, 2]);
  assert.equal(fleet[0].id, 1, 'input not mutated');
});

test('groupByOrg keeps device order inside orgs and sorts groups by name', () => {
  const groups = groupByOrg(sortDevices(fleet));
  assert.deepEqual(groups.map((g) => g.org_name), ['Acme Dental', 'Zed Legal']);
  assert.deepEqual(groups[0].devices.map((d) => d.id), [3, 5, 2]);
  assert.deepEqual(groups[1].devices.map((d) => d.id), [4, 1]);
});

test('filterDevices by substring, status and org', () => {
  assert.deepEqual(filterDevices(fleet, 'WS-0', '', 0).map((d) => d.id), [2, 3, 5]);
  assert.deepEqual(filterDevices(fleet, '', 'online', 0).map((d) => d.id), [1, 4, 5]);
  assert.deepEqual(filterDevices(fleet, '', '', 2).map((d) => d.id), [1, 4]);
  assert.deepEqual(filterDevices(fleet, 'pc', 'online', 2).map((d) => d.id), [1, 4]);
  assert.deepEqual(filterDevices(fleet, 'nothing', '', 0), []);
});
