import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  dueSteps, elapsedMinutes, formatDelay, MAX_STEP_MINUTES, MAX_STEPS,
  policyChannelCount, policySummary, sortSteps, validateSteps,
} from './escalation.ts';
import type { EscalationPolicy, EscalationStep } from '../types.ts';

const step = (after_minutes: number, channel_ids: number[]): EscalationStep => ({ after_minutes, channel_ids });
const policy = (steps: EscalationStep[]): EscalationPolicy => ({
  id: 1, name: 'p', org_id: null, steps, created_at: '2026-01-01T00:00:00Z', created_by: 1,
});

test('validateSteps: a valid ordered policy passes', () => {
  assert.equal(validateSteps([step(0, [1]), step(15, [2]), step(60, [1, 2])]), '');
  assert.equal(validateSteps([step(0, [1])]), '');
});

test('validateSteps: at least one step, at most MAX_STEPS', () => {
  assert.equal(validateSteps([]), 'Add at least one step.');
  const many = Array.from({ length: MAX_STEPS + 1 }, () => step(0, [1]));
  assert.equal(validateSteps(many), `A policy may have at most ${MAX_STEPS} steps.`);
});

test('validateSteps: delay must be a whole number of minutes >= 0 and within bounds', () => {
  assert.match(validateSteps([step(-1, [1])]), /Step 1: delay must be/);
  assert.match(validateSteps([step(1.5, [1])]), /Step 1: delay must be/);
  assert.match(validateSteps([step(MAX_STEP_MINUTES + 1, [1])]), /Step 1: delay is too far/);
  assert.equal(validateSteps([step(MAX_STEP_MINUTES, [1])]), '');
});

test('validateSteps: each step needs at least one channel', () => {
  assert.equal(validateSteps([step(0, [])]), 'Step 1: choose at least one channel.');
  assert.equal(validateSteps([step(0, [1]), step(5, [])]), 'Step 2: choose at least one channel.');
});

test('validateSteps: delays must be non-decreasing down the list', () => {
  assert.equal(validateSteps([step(15, [1]), step(5, [2])]), 'Steps must be ordered by increasing delay.');
  assert.equal(validateSteps([step(5, [1]), step(5, [2])]), ''); // equal is allowed
});

test('sortSteps: ascending by delay, stable, input untouched', () => {
  const input = [step(15, [2]), step(0, [1]), step(15, [3])];
  const sorted = sortSteps(input);
  assert.deepEqual(sorted.map((s) => s.after_minutes), [0, 15, 15]);
  // stable: the two 15-minute steps keep their relative order (channel 2 before 3)
  assert.deepEqual(sorted[1].channel_ids, [2]);
  assert.deepEqual(sorted[2].channel_ids, [3]);
  assert.deepEqual(input[0].channel_ids, [2]); // original array not reordered
});

test('formatDelay: at open, minutes, hours, days', () => {
  assert.equal(formatDelay(0), 'at open');
  assert.equal(formatDelay(15), '15m');
  assert.equal(formatDelay(90), '1h30m');
  assert.equal(formatDelay(120), '2h');
  assert.equal(formatDelay(1440), '1d');
  assert.equal(formatDelay(1530), '1d1h30m');
  assert.equal(formatDelay(-1), '?');
});

test('elapsedMinutes: floor of minutes since opened_at, clamped at 0, NaN on bad input', () => {
  const open = '2026-06-01T10:00:00Z';
  const at = (iso: string) => Date.parse(iso);
  assert.equal(elapsedMinutes(open, at('2026-06-01T10:15:30Z')), 15);
  assert.equal(elapsedMinutes(open, at('2026-06-01T09:00:00Z')), 0); // future open clamps to 0
  assert.ok(Number.isNaN(elapsedMinutes('nope', at('2026-06-01T10:15:00Z'))));
});

test('dueSteps: steps whose delay has elapsed (indices), mirrors the dispatcher', () => {
  const steps = [step(0, [1]), step(15, [2]), step(60, [3])];
  const open = '2026-06-01T10:00:00Z';
  const at = (iso: string) => Date.parse(iso);
  assert.deepEqual(dueSteps(steps, open, at('2026-06-01T10:00:00Z')), [0]);
  assert.deepEqual(dueSteps(steps, open, at('2026-06-01T10:14:59Z')), [0]);
  assert.deepEqual(dueSteps(steps, open, at('2026-06-01T10:15:00Z')), [0, 1]);
  assert.deepEqual(dueSteps(steps, open, at('2026-06-01T11:30:00Z')), [0, 1, 2]);
  assert.deepEqual(dueSteps(steps, 'bad', at('2026-06-01T11:30:00Z')), []);
});

test('policySummary: count and ordered delays', () => {
  assert.equal(policySummary(policy([step(0, [1]), step(15, [2])])), '2 steps · at open, 15m');
  assert.equal(policySummary(policy([step(5, [1])])), '1 step · 5m');
  // unordered input is summarised in time order
  assert.equal(policySummary(policy([step(15, [2]), step(0, [1])])), '2 steps · at open, 15m');
  assert.equal(policySummary(policy([])), 'no steps');
});

test('policyChannelCount: distinct channels across steps', () => {
  assert.equal(policyChannelCount(policy([step(0, [1, 2]), step(15, [2, 3])])), 3);
  assert.equal(policyChannelCount(policy([])), 0);
});
