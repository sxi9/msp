// NS2 escalation policies: the pure logic the Notifications and Delivery-log
// views use — step validation and ordering, the human policy summary, the
// delay label, and the "which steps are due" preview. No DOM, no network;
// tested by escalation.test.ts with `node --test`. These mirror the server's
// rules (internal/server/dashboard_notify.go validatePolicy and the dispatcher's
// due-step query) so the dashboard preview matches what the dispatcher does.

import type { EscalationPolicy, EscalationStep } from '../types.ts';

/** Most steps a policy may hold (server: notifyMaxSteps). */
export const MAX_STEPS = 10;
/** Furthest a step may be delayed, in minutes (server: notifyMaxStepMinutes, 4 weeks). */
export const MAX_STEP_MINUTES = 40320;

/** The two fields of a step the UI edits and the server validates. */
export interface StepLike {
  after_minutes: number;
  channel_ids: number[];
}

/**
 * Sort steps by ascending delay, stably (a copy; the input is untouched). The
 * UI sorts before submit so the stored step_order matches the firing order, and
 * the summary reads in time order regardless of entry order.
 */
export function sortSteps<T extends StepLike>(steps: readonly T[]): T[] {
  return steps.map((s, i) => [s, i] as const)
    .sort((a, b) => a[0].after_minutes - b[0].after_minutes || a[1] - b[1])
    .map(([s]) => s);
}

/**
 * Validate a policy's ordered steps, mirroring the server's validatePolicy: at
 * least one step and at most MAX_STEPS; each delay a whole number of minutes in
 * [0, MAX_STEP_MINUTES]; delays non-decreasing down the list; each step with at
 * least one channel. Returns '' when valid, else a one-line human reason.
 */
export function validateSteps(steps: readonly StepLike[]): string {
  if (steps.length === 0) return 'Add at least one step.';
  if (steps.length > MAX_STEPS) return `A policy may have at most ${MAX_STEPS} steps.`;
  let prev = -1;
  for (let i = 0; i < steps.length; i++) {
    const s = steps[i];
    const n = i + 1;
    if (!Number.isInteger(s.after_minutes) || s.after_minutes < 0) {
      return `Step ${n}: delay must be a whole number of minutes (0 or more).`;
    }
    if (s.after_minutes > MAX_STEP_MINUTES) {
      return `Step ${n}: delay is too far in the future.`;
    }
    if (!s.channel_ids || s.channel_ids.length === 0) {
      return `Step ${n}: choose at least one channel.`;
    }
    if (s.after_minutes < prev) {
      return 'Steps must be ordered by increasing delay.';
    }
    prev = s.after_minutes;
  }
  return '';
}

/**
 * A compact label for a step delay: "at open" at 0, else the largest units,
 * e.g. "15m", "1h30m", "2h", "1d", "1d6h".
 */
export function formatDelay(mins: number): string {
  if (!Number.isFinite(mins) || mins < 0) return '?';
  if (mins === 0) return 'at open';
  const d = Math.floor(mins / 1440);
  const h = Math.floor((mins % 1440) / 60);
  const m = mins % 60;
  const parts: string[] = [];
  if (d) parts.push(`${d}d`);
  if (h) parts.push(`${h}h`);
  if (m) parts.push(`${m}m`);
  return parts.join('');
}

/**
 * Whole minutes elapsed between an alert's opened_at (RFC3339) and `now` (ms).
 * NaN when opened_at is unparseable; negative is clamped to 0 (a future open has
 * no due steps yet).
 */
export function elapsedMinutes(openedAt: string, now: number = Date.now()): number {
  const o = Date.parse(openedAt);
  if (Number.isNaN(o)) return NaN;
  return Math.max(0, Math.floor((now - o) / 60000));
}

/**
 * The indices (0-based) of the steps whose delay has elapsed for an alert opened
 * at openedAt — the dispatcher's due-step set while the alert is still open and
 * unacknowledged. Empty when openedAt is unparseable.
 */
export function dueSteps(steps: readonly StepLike[], openedAt: string, now: number = Date.now()): number[] {
  const mins = elapsedMinutes(openedAt, now);
  if (Number.isNaN(mins)) return [];
  const out: number[] = [];
  steps.forEach((s, i) => {
    if (mins >= s.after_minutes) out.push(i);
  });
  return out;
}

/**
 * A one-line summary of a policy for the Rules panel and the policies list, e.g.
 * "2 steps · at open, 15m" or "1 step · 5m". "no steps" when empty.
 */
export function policySummary(p: Pick<EscalationPolicy, 'steps'>): string {
  const n = p.steps.length;
  if (n === 0) return 'no steps';
  const times = sortSteps(p.steps).map((s) => formatDelay(s.after_minutes)).join(', ');
  return `${n} step${n === 1 ? '' : 's'} · ${times}`;
}

/** Total distinct channels a policy fires across all its steps. */
export function policyChannelCount(p: Pick<EscalationPolicy, 'steps'>): number {
  const ids = new Set<number>();
  for (const s of p.steps) for (const id of s.channel_ids) ids.add(id);
  return ids.size;
}

/** An empty step for the editor: fires at open with no channels chosen yet. */
export function emptyStep(): EscalationStep {
  return { after_minutes: 0, channel_ids: [] };
}
