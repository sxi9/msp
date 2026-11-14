import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  hasPendingSecurity, patchReportOf, rebootBadge, securityUpdates, sortUpdates, summarisePatches,
  type PatchUpdate,
} from './patches.ts';

const upd = (name: string, security = false, cur = '1.0', nw = '1.1'): PatchUpdate =>
  ({ name, current: cur, new: nw, security, source: 'noble-security' });

const report = {
  checked_at: '2026-11-14T09:00:00Z', manager: 'apt', reboot_required: true, count: 3, security_count: 1, error: '',
  updates: [upd('openssl', true), upd('curl'), upd('vim')],
};

test('patchReportOf: null without a usable report, typed otherwise, bad entries skipped', () => {
  assert.equal(patchReportOf(undefined), null);
  assert.equal(patchReportOf({}), null); // never reported (server sends {})
  assert.equal(patchReportOf({ manager: '' }), null);
  assert.equal(patchReportOf('nope'), null);
  const r = patchReportOf({
    checked_at: '2026-11-14T09:00:00Z', manager: 'apt', reboot_required: true, count: 2, security_count: 1, error: '',
    updates: [
      { name: 'openssl', current: '3.0.1', new: '3.0.2', security: true, source: 's' },
      { name: 'curl' },
      { name: '', label: 'ghost' },
      'junk', null, 7,
      { current: '1.0' },
    ],
  });
  assert.ok(r);
  assert.equal(r!.manager, 'apt');
  assert.equal(r!.reboot_required, true);
  assert.deepEqual(r!.updates, [
    { name: 'openssl', current: '3.0.1', new: '3.0.2', security: true, source: 's' },
    { name: 'curl', current: '', new: '', security: false, source: '' },
  ]);
});

test('patchReportOf: none manager with no updates is still a report (unmanaged host)', () => {
  const r = patchReportOf({ checked_at: '', manager: 'none', reboot_required: false, count: 0, security_count: 0, error: '', updates: [] });
  assert.ok(r);
  assert.equal(r!.manager, 'none');
  assert.deepEqual(r!.updates, []);
});

test('securityUpdates and sortUpdates: security first then name, input not mutated', () => {
  const list = [upd('zlib'), upd('openssl', true), upd('apache', true), upd('curl')];
  const copy = [...list];
  assert.deepEqual(securityUpdates(list).map((u) => u.name), ['openssl', 'apache']);
  assert.deepEqual(sortUpdates(list).map((u) => u.name), ['apache', 'openssl', 'curl', 'zlib']);
  assert.deepEqual(list, copy);
});

test('summarisePatches: counts, and the agent totals win when its list was capped', () => {
  const s = summarisePatches(patchReportOf(report)!);
  assert.equal(s.pending, 3);
  assert.equal(s.security, 1);
  assert.equal(s.nonSecurity, 2);
  assert.equal(s.rebootRequired, true);
  assert.equal(s.unmanaged, false);
  assert.equal(s.clean, false);

  // Agent reported more than the capped list it sent.
  const capped = summarisePatches(patchReportOf({ ...report, count: 540, security_count: 120 })!);
  assert.equal(capped.pending, 540);
  assert.equal(capped.security, 120);
  assert.equal(capped.nonSecurity, 420);
});

test('summarisePatches: a clean managed host, and an unmanaged one', () => {
  const clean = summarisePatches(patchReportOf({ checked_at: 'x', manager: 'apt', reboot_required: false, count: 0, security_count: 0, error: '', updates: [] })!);
  assert.equal(clean.clean, true);
  assert.equal(clean.pending, 0);

  const errored = summarisePatches(patchReportOf({ checked_at: 'x', manager: 'apt', reboot_required: false, count: 0, security_count: 0, error: 'apt lock held', updates: [] })!);
  assert.equal(errored.clean, false);
  assert.equal(errored.error, 'apt lock held');

  const unmanaged = summarisePatches(patchReportOf({ checked_at: 'x', manager: 'none', reboot_required: false, count: 0, security_count: 0, error: '', updates: [] })!);
  assert.equal(unmanaged.unmanaged, true);
  assert.equal(unmanaged.clean, false);
});

test('rebootBadge', () => {
  assert.equal(rebootBadge({ reboot_required: true }), 'Reboot required');
  assert.equal(rebootBadge({ reboot_required: false }), null);
});

test('hasPendingSecurity: fleet filter predicate', () => {
  assert.equal(hasPendingSecurity({ security_patches: 0 }), false);
  assert.equal(hasPendingSecurity({ security_patches: 2 }), true);
});
