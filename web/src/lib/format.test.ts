import { test } from 'node:test';
import assert from 'node:assert/strict';
import { pct, gb, ago, duration, uptime } from './format.ts';

test('pct rounds and dashes null', () => {
  assert.equal(pct(50.8), '51%');
  assert.equal(pct(0), '0%');
  assert.equal(pct(null), '—');
  assert.equal(pct(undefined), '—');
});

test('gb one decimal', () => {
  assert.equal(gb(201.3), '201.3 GB');
  assert.equal(gb(476), '476.0 GB');
  assert.equal(gb(null), '—');
});

test('ago buckets', () => {
  const now = Date.parse('2026-11-13T10:00:00Z');
  assert.equal(ago(null), 'never');
  assert.equal(ago('garbage'), 'never');
  assert.equal(ago('2026-11-13T09:59:48Z', now), '12 s ago');
  assert.equal(ago('2026-11-13T09:55:00Z', now), '5 min ago');
  assert.equal(ago('2026-11-13T07:00:00Z', now), '3 h ago');
  assert.equal(ago('2026-11-11T10:00:00Z', now), '2 d ago');
  assert.equal(ago('2026-11-13T10:00:05Z', now), '0 s ago');
});

test('duration', () => {
  assert.equal(duration('2026-11-13T10:00:00.000Z', '2026-11-13T10:00:00.340Z'), '340 ms');
  assert.equal(duration('2026-11-13T10:00:00Z', '2026-11-13T10:00:02.5Z'), '2.5 s');
  assert.equal(duration('2026-11-13T10:00:00Z', '2026-11-13T10:03:04Z'), '3 min 4 s');
  assert.equal(duration('2026-11-13T10:00:00Z', null), '—');
});

test('uptime', () => {
  assert.equal(uptime(86400 * 3 + 3600 * 4 + 7), '3 d 4 h');
  assert.equal(uptime(3600 * 2 + 300), '2 h 5 min');
  assert.equal(uptime(420), '7 min');
  assert.equal(uptime(null), '—');
});
