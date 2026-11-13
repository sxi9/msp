import { test } from 'node:test';
import assert from 'node:assert/strict';
import { accessOf, assignedOrgNames, sameAccess, summariseAccess, toPayload } from './orgscope.ts';

const orgs = [
  { id: 1, name: 'Acme Dental' },
  { id: 2, name: 'Zed Legal' },
  { id: 3, name: 'Beam Clinic' },
];

test('accessOf: admins and all_orgs users are all-clients; absent all_orgs defaults to all', () => {
  assert.deepEqual(accessOf({ role: 'admin', all_orgs: false, org_ids: [1, 2] }), { all: true, orgIds: [] });
  assert.deepEqual(accessOf({ role: 'operator', all_orgs: true, org_ids: [] }), { all: true, orgIds: [] });
  assert.deepEqual(accessOf({ role: 'viewer' }), { all: true, orgIds: [] });
  assert.deepEqual(accessOf({ role: 'operator', all_orgs: false, org_ids: [2, 1, 2] }), { all: false, orgIds: [1, 2] });
  assert.deepEqual(accessOf({ role: 'viewer', all_orgs: false }), { all: false, orgIds: [] });
});

test('summariseAccess: admin tag, all clients, empty set and counts', () => {
  assert.equal(summariseAccess({ role: 'admin', all_orgs: false, org_ids: [1] }), 'All clients (admin)');
  assert.equal(summariseAccess({ role: 'operator', all_orgs: true, org_ids: [] }), 'All clients');
  assert.equal(summariseAccess({ role: 'viewer' }), 'All clients');
  assert.equal(summariseAccess({ role: 'operator', all_orgs: false, org_ids: [] }), 'No clients');
  assert.equal(summariseAccess({ role: 'operator', all_orgs: false, org_ids: [1] }), '1 client');
  assert.equal(summariseAccess({ role: 'viewer', all_orgs: false, org_ids: [3, 1, 2] }), '3 clients');
});

test('assignedOrgNames: ordered by the org list, unknown ids fall back to #id', () => {
  assert.deepEqual(assignedOrgNames([2, 1], orgs), ['Acme Dental', 'Zed Legal']);
  assert.deepEqual(assignedOrgNames([3], orgs), ['Beam Clinic']);
  assert.deepEqual(assignedOrgNames([], orgs), []);
  // An id the admin's org list did not include is named "#id", never dropped silently.
  assert.deepEqual(assignedOrgNames([1, 9, 7], orgs), ['Acme Dental', '#7', '#9']);
});

test('toPayload: all clears the list; scoped keeps only known ids, deduped and sorted', () => {
  assert.deepEqual(toPayload(true, [1, 2], [1, 2, 3]), { all_orgs: true, org_ids: [] });
  assert.deepEqual(toPayload(false, new Set([3, 1, 1]), [1, 2, 3]), { all_orgs: false, org_ids: [1, 3] });
  // Ids not in knownOrgIds are dropped: the dashboard never sends an org it was not shown.
  assert.deepEqual(toPayload(false, [1, 99], [1, 2, 3]), { all_orgs: false, org_ids: [1] });
  assert.deepEqual(toPayload(false, [], [1, 2, 3]), { all_orgs: false, org_ids: [] });
});

test('sameAccess: all_orgs matches ignore the list; scoped compares the sets order-free', () => {
  assert.equal(sameAccess({ all_orgs: true, org_ids: [] }, { all_orgs: true, org_ids: [1] }), true);
  assert.equal(sameAccess({ all_orgs: true, org_ids: [] }, { all_orgs: false, org_ids: [] }), false);
  assert.equal(sameAccess({ all_orgs: false, org_ids: [1, 2] }, { all_orgs: false, org_ids: [2, 1] }), true);
  assert.equal(sameAccess({ all_orgs: false, org_ids: [1] }, { all_orgs: false, org_ids: [1, 2] }), false);
  assert.equal(sameAccess({ all_orgs: false, org_ids: [] }, { all_orgs: false, org_ids: [] }), true);
});
