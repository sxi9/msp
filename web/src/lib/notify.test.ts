import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  channelTarget, compareSeverity, describeRule, redactUrl, ruleMatches,
  secretLabel, severityAtLeast, windowActive, windowState,
} from './notify.ts';
import type { NotifyRule } from '../types.ts';

const rule = (over: Partial<NotifyRule> = {}): NotifyRule => ({
  id: 1, name: 'r', org_id: null, min_severity: 'medium', alert_types: [],
  on_open: true, on_resolve: false, channel_id: 1, enabled: true,
  created_at: '2026-01-01T00:00:00Z', created_by: 1, ...over,
});

test('compareSeverity / severityAtLeast: low < medium < high', () => {
  assert.ok(compareSeverity('low', 'high') < 0);
  assert.ok(compareSeverity('high', 'low') > 0);
  assert.equal(compareSeverity('medium', 'medium'), 0);
  assert.equal(severityAtLeast('high', 'medium'), true);
  assert.equal(severityAtLeast('medium', 'medium'), true);
  assert.equal(severityAtLeast('low', 'medium'), false);
});

test('ruleMatches: org scope — null matches any org, a number only its own', () => {
  const base = { severity: 'high' as const, type: 'offline', event: 'opened' as const };
  assert.equal(ruleMatches(rule({ org_id: null }), { ...base, org_id: 7 }), true);
  assert.equal(ruleMatches(rule({ org_id: 7 }), { ...base, org_id: 7 }), true);
  assert.equal(ruleMatches(rule({ org_id: 7 }), { ...base, org_id: 8 }), false);
});

test('ruleMatches: severity threshold uses >= min_severity', () => {
  const a = { org_id: 1, type: 'offline', event: 'opened' as const };
  assert.equal(ruleMatches(rule({ min_severity: 'high' }), { ...a, severity: 'low' }), false);
  assert.equal(ruleMatches(rule({ min_severity: 'high' }), { ...a, severity: 'high' }), true);
  assert.equal(ruleMatches(rule({ min_severity: 'low' }), { ...a, severity: 'low' }), true);
});

test('ruleMatches: alert_types empty = all; otherwise must list the type', () => {
  const a = { org_id: 1, severity: 'high' as const, event: 'opened' as const };
  assert.equal(ruleMatches(rule({ alert_types: [] }), { ...a, type: 'battery_low' }), true);
  assert.equal(ruleMatches(rule({ alert_types: ['offline', 'disk_full'] }), { ...a, type: 'offline' }), true);
  assert.equal(ruleMatches(rule({ alert_types: ['offline'] }), { ...a, type: 'disk_full' }), false);
});

test('ruleMatches: event must be enabled, and a disabled rule never matches', () => {
  const a = { org_id: 1, severity: 'high' as const, type: 'offline' };
  assert.equal(ruleMatches(rule({ on_open: true, on_resolve: false }), { ...a, event: 'opened' }), true);
  assert.equal(ruleMatches(rule({ on_open: true, on_resolve: false }), { ...a, event: 'resolved' }), false);
  assert.equal(ruleMatches(rule({ on_open: false, on_resolve: true }), { ...a, event: 'resolved' }), true);
  assert.equal(ruleMatches(rule({ enabled: false }), { ...a, event: 'opened' }), false);
});

test('describeRule: severity, events and types in one line', () => {
  assert.equal(describeRule({ min_severity: 'high', alert_types: [], on_open: true, on_resolve: false }),
    'high and up · on open · all types');
  assert.equal(describeRule({ min_severity: 'medium', alert_types: ['offline', 'disk_full'], on_open: true, on_resolve: true }),
    'medium and up · on open + resolve · offline, disk_full');
  assert.equal(describeRule({ min_severity: 'low', alert_types: [], on_open: false, on_resolve: false }),
    'low and up · no events · all types');
});

test('windowActive / windowState: before, inside and after [start, end] (end inclusive)', () => {
  const w = { starts_at: '2026-06-01T10:00:00Z', ends_at: '2026-06-01T12:00:00Z' };
  const at = (iso: string) => Date.parse(iso);
  assert.equal(windowActive(w, at('2026-06-01T09:59:59Z')), false);
  assert.equal(windowActive(w, at('2026-06-01T10:00:00Z')), true); // start inclusive
  assert.equal(windowActive(w, at('2026-06-01T11:00:00Z')), true);
  // End is inclusive, matching the server's store.ActiveMaintenance so the badge
  // and the dispatcher agree at the boundary instant.
  assert.equal(windowActive(w, at('2026-06-01T12:00:00Z')), true);
  assert.equal(windowActive(w, at('2026-06-01T12:00:01Z')), false);
  assert.equal(windowState(w, at('2026-06-01T09:00:00Z')), 'upcoming');
  assert.equal(windowState(w, at('2026-06-01T11:00:00Z')), 'active');
  assert.equal(windowState(w, at('2026-06-01T12:00:00Z')), 'active'); // end inclusive
  assert.equal(windowState(w, at('2026-06-01T13:00:00Z')), 'ended');
});

test('windowState: bad or inverted times are invalid, and never active', () => {
  assert.equal(windowState({ starts_at: 'nope', ends_at: '2026-06-01T12:00:00Z' }), 'invalid');
  assert.equal(windowState({ starts_at: '2026-06-01T12:00:00Z', ends_at: '2026-06-01T10:00:00Z' }), 'invalid');
  assert.equal(windowActive({ starts_at: 'nope', ends_at: 'nope' }, 0), false);
});

test('secretLabel: write-only set / not set', () => {
  assert.equal(secretLabel(true), 'set');
  assert.equal(secretLabel(false), 'not set');
});

test('redactUrl: keeps scheme + host, drops path, query and userinfo', () => {
  assert.equal(redactUrl('https://hooks.slack.com/services/T000/B000/secretpath'), 'https://hooks.slack.com');
  assert.equal(redactUrl('https://user:pass@example.com/hook?token=abc'), 'https://example.com');
  assert.equal(redactUrl('http://10.0.0.1:9000/notify'), 'http://10.0.0.1:9000');
  assert.equal(redactUrl('not a url'), '[redacted-url]');
});

test('channelTarget: email shows recipients, webhook shows redacted url', () => {
  assert.equal(channelTarget({ kind: 'email', config: { to: ['a@x.com', 'b@x.com'] } }), 'a@x.com, b@x.com');
  assert.equal(channelTarget({ kind: 'webhook', config: { url: 'https://hooks.slack.com/services/T/B/x', flavor: 'slack' } }),
    'https://hooks.slack.com');
  assert.equal(channelTarget({ kind: 'email', config: {} }), '');
});
