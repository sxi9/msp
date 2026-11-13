import { test } from 'node:test';
import assert from 'node:assert/strict';
import { appsOf, appsSummary, filterApps, sortApps } from './apps.ts';

const app = (pkg: string, label: string, version = '1.0', system = false) => ({ package: pkg, label, version, system });

const inventory = [
  app('com.android.settings', 'Settings', '14', true),
  app('com.sxi9.msp.agent', 'MSP Agent', '0.1.0'),
  app('org.mozilla.firefox', 'Firefox', '131.0.3'),
  app('com.google.android.gms', 'Google Play services', '24.38.33', true),
  app('com.example.nolabel', 'com.example.nolabel', ''),
  app('com.example.b', 'Same'),
  app('com.example.a', 'Same'),
];

test('appsOf: null without an apps array, typed entries otherwise, bad entries skipped', () => {
  assert.equal(appsOf({ kernel: '6.8' }), null);
  assert.equal(appsOf({ apps: 'nope' }), null);
  assert.deepEqual(appsOf({ apps: [] }), []);
  assert.deepEqual(appsOf({ apps: [
    { package: 'com.a', label: 'A', version: '1', system: true },
    { package: 'com.b' },
    { package: '', label: 'ghost' },
    'junk', null, 7,
    { label: 'no package' },
  ] }), [
    { package: 'com.a', label: 'A', version: '1', system: true },
    { package: 'com.b', label: 'com.b', version: '', system: false },
  ]);
});

test('sortApps: non-system first, label case-insensitive, then package; input not mutated', () => {
  const copy = [...inventory];
  assert.deepEqual(sortApps(inventory).map((a) => a.package), [
    'com.example.nolabel', 'org.mozilla.firefox', 'com.sxi9.msp.agent', 'com.example.a', 'com.example.b',
    'com.google.android.gms', 'com.android.settings',
  ]);
  assert.deepEqual(inventory, copy);
});

test('filterApps: system toggle and case-insensitive label/package search', () => {
  assert.deepEqual(filterApps(inventory, '', false).map((a) => a.package),
    ['com.example.nolabel', 'org.mozilla.firefox', 'com.sxi9.msp.agent', 'com.example.a', 'com.example.b']);
  assert.equal(filterApps(inventory, '', true).length, 7);
  assert.deepEqual(filterApps(inventory, 'FIRE', true).map((a) => a.package), ['org.mozilla.firefox']);
  assert.deepEqual(filterApps(inventory, '  google  ', true).map((a) => a.package), ['com.google.android.gms']);
  assert.deepEqual(filterApps(inventory, 'google', false), []);
  assert.deepEqual(filterApps(inventory, 'com.example', false).map((a) => a.package), ['com.example.nolabel', 'com.example.a', 'com.example.b']);
  assert.deepEqual(filterApps(inventory, 'nothing', true), []);
});

test('appsSummary counts', () => {
  assert.equal(appsSummary([]), '0 apps');
  assert.equal(appsSummary([app('a', 'A')]), '1 app');
  assert.equal(appsSummary(inventory), '7 apps (2 system)');
});
