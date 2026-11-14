import { test } from 'node:test';
import assert from 'node:assert/strict';
import { INVENTORY_FACT_KEYS, factRows } from './facts.ts';

test('factRows keeps ordinary facts and drops the inventory arrays (apps + software)', () => {
  // A real desktop device's facts object: the server stores the whole facts
  // object in facts_json, so `software` is present alongside the scalar facts.
  const facts = {
    kernel: '6.8.0-45-generic',
    cpu_model: 'Intel(R) Core(TM) i5-1345U',
    cpu_count: 8,
    ips: ['10.0.0.5'],
    software: [
      { name: 'openssl', version: '3.0.13', publisher: 'Ubuntu', source: 'dpkg' },
      { name: 'curl', version: '8.5.0', publisher: 'Ubuntu', source: 'dpkg' },
    ],
  };
  const keys = factRows(facts).map(([k]) => k);
  assert.deepEqual(keys, ['kernel', 'cpu_model', 'cpu_count', 'ips']);
  assert.ok(!keys.includes('software'), 'software must never be a Facts row');
  assert.ok(!keys.includes('apps'), 'apps must never be a Facts row');
});

test('factRows drops apps for an Android device', () => {
  const facts = {
    kernel: 'android-6.1',
    apps: [{ package: 'com.example', label: 'Example', version: '1.0', system: false }],
  };
  assert.deepEqual(factRows(facts).map(([k]) => k), ['kernel']);
});

test('factRows preserves values and handles an empty facts object', () => {
  assert.deepEqual(factRows({}), []);
  assert.deepEqual(factRows({ serial: 'ABC123', model: '' }), [['serial', 'ABC123'], ['model', '']]);
});

test('INVENTORY_FACT_KEYS lists exactly the keys with their own panels', () => {
  assert.deepEqual([...INVENTORY_FACT_KEYS].sort(), ['apps', 'software']);
});
