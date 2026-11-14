// NS1 alert notifications: the pure logic the Notifications and Delivery-log
// views use — severity comparison, the rule-match preview, maintenance-window
// state and secret redaction/display. No DOM, no network; tested by
// notify.test.ts with `node --test`. These mirror the server's rules
// (internal/server/dashboard_notify.go, notify_dispatch.go) so the dashboard's
// preview matches what the dispatcher will actually do.

import type { MaintenanceWindow, NotifyChannel, NotifyRule, Severity } from '../types.ts';

// --- severity ---------------------------------------------------------------

/** Order of the three severities; a higher rank is more severe. */
export const SEVERITY_RANK: Record<Severity, number> = { low: 0, medium: 1, high: 2 };

/** < 0, 0 or > 0 as `a` is less, equally or more severe than `b`. */
export function compareSeverity(a: Severity, b: Severity): number {
  return SEVERITY_RANK[a] - SEVERITY_RANK[b];
}

/** Whether `sev` meets or exceeds `min` — the dispatcher's `severity >= min_severity` test. */
export function severityAtLeast(sev: Severity, min: Severity): boolean {
  return SEVERITY_RANK[sev] >= SEVERITY_RANK[min];
}

// --- rule matching (preview) ------------------------------------------------

/** The parts of an alert event a rule is matched against. */
export interface AlertLike {
  org_id: number;
  severity: Severity;
  type: string;
  /** The delivery event this would be: an open fires on_open rules, a resolve on_resolve. */
  event: 'opened' | 'resolved';
}

/**
 * Whether a rule would route the given alert event, mirroring the server's
 * MatchRules: the rule must be enabled; its org (null = every client, else the
 * alert's org); severity at least the rule's min; the alert type listed (empty
 * list = all types); and the event enabled (on_open / on_resolve).
 */
export function ruleMatches(rule: NotifyRule, alert: AlertLike): boolean {
  if (!rule.enabled) return false;
  if (rule.org_id !== null && rule.org_id !== alert.org_id) return false;
  if (!severityAtLeast(alert.severity, rule.min_severity)) return false;
  if (rule.alert_types.length > 0 && !rule.alert_types.includes(alert.type)) return false;
  if (alert.event === 'opened' && !rule.on_open) return false;
  if (alert.event === 'resolved' && !rule.on_resolve) return false;
  return true;
}

/**
 * A one-line human preview of what a rule matches, for the Rules panel:
 * e.g. "high and up · on open · all types" or
 * "medium and up · on open + resolve · offline, disk_full".
 */
export function describeRule(rule: Pick<NotifyRule, 'min_severity' | 'alert_types' | 'on_open' | 'on_resolve'>): string {
  const events = [rule.on_open && 'open', rule.on_resolve && 'resolve'].filter(Boolean).join(' + ');
  const types = rule.alert_types.length === 0 ? 'all types' : rule.alert_types.join(', ');
  return `${rule.min_severity} and up · ${events ? `on ${events}` : 'no events'} · ${types}`;
}

// --- maintenance windows ----------------------------------------------------

export type WindowState = 'upcoming' | 'active' | 'ended' | 'invalid';

/** Parse an RFC3339 stamp to epoch ms, or NaN. */
function ms(iso: string): number {
  return Date.parse(iso);
}

/** Whether `now` falls inside [starts_at, ends_at) — the suppression test. */
export function windowActive(w: Pick<MaintenanceWindow, 'starts_at' | 'ends_at'>, now: number = Date.now()): boolean {
  const s = ms(w.starts_at), e = ms(w.ends_at);
  if (Number.isNaN(s) || Number.isNaN(e)) return false;
  return now >= s && now < e;
}

/** A window's state relative to `now`: upcoming, active, ended (or invalid times). */
export function windowState(w: Pick<MaintenanceWindow, 'starts_at' | 'ends_at'>, now: number = Date.now()): WindowState {
  const s = ms(w.starts_at), e = ms(w.ends_at);
  if (Number.isNaN(s) || Number.isNaN(e) || e <= s) return 'invalid';
  if (now < s) return 'upcoming';
  if (now < e) return 'active';
  return 'ended';
}

// --- secrets ----------------------------------------------------------------

/** The write-only secret's display label: "set" when one is stored, else "not set". */
export function secretLabel(secretSet: boolean): string {
  return secretSet ? 'set' : 'not set';
}

/**
 * Reduce a webhook URL to scheme://host for display, dropping userinfo, path and
 * query so an embedded token (a Slack incoming-webhook path, a token query
 * parameter) is never shown. Mirrors the server's redactURL; an unparseable URL
 * becomes "[redacted-url]".
 */
export function redactUrl(raw: string): string {
  try {
    const u = new URL(raw);
    return u.host ? `${u.protocol}//${u.host}` : '[redacted-url]';
  } catch {
    return '[redacted-url]';
  }
}

/**
 * A channel's address for display, already redacted: the recipient list for an
 * email channel, the scheme://host for a webhook. Used in the channels table and
 * the delivery log so no secret or tokened URL is ever rendered.
 */
export function channelTarget(c: Pick<NotifyChannel, 'kind' | 'config'>): string {
  const cfg = c.config as Record<string, unknown>;
  if (c.kind === 'email') {
    const to = Array.isArray(cfg.to) ? (cfg.to as unknown[]).map(String) : [];
    return to.join(', ');
  }
  if (c.kind === 'webhook') {
    return typeof cfg.url === 'string' ? redactUrl(cfg.url) : '';
  }
  return '';
}
