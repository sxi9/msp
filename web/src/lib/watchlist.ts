// Service/process watchlists and forwarded logs (WL1): the pure logic behind the
// device page's "Watched services" and "Logs" panels, the Watchlists admin area
// and the fleet "stopped service" column. Nothing here touches the network — the
// views call api.ts and pass the parsed data in. Tested by watchlist.test.ts.
//
// The wire shapes (WatchlistItem, DeviceLog, ServiceState) live here, like
// lib/software.ts owns SoftwareItem/SoftwareHit, and are re-exported from types.ts.

/** A watchlist item (and a reported ServiceState) is a system service or a process. */
export type WatchKind = 'service' | 'process';
/** An item applies to a whole client org ('org') or one device ('device'). */
export type WatchScope = 'org' | 'device';
/** A forwarded log line comes from the Linux journal or the Windows Event Log. */
export type LogSource = 'journald' | 'eventlog';
/** The severity bucket a raw level string maps to (journald priority / Event Log level). */
export type Severity = 'error' | 'warning' | 'info' | 'debug' | 'other';

/**
 * One watched service/process's reported running state, as a desktop agent sends
 * it in facts.services ([{kind,name,running}]) for the device's applicable
 * watchlist. Parsed from the device-detail facts by servicesOf.
 */
export interface ServiceState { kind: WatchKind; name: string; running: boolean }

/**
 * A watchlist item exactly as GET /api/v1/watchlist returns it. org_id is null
 * only for a would-be global item (the server derives a device item's org from the
 * device, so in practice both carry an org); org_name/hostname are joined in for
 * the admin table; alert is whether a stopped item opens a service_down alert.
 */
export interface WatchlistItem {
  id: number; org_id: number | null; org_name: string; scope: WatchScope;
  device_id: number | null; hostname: string; kind: WatchKind; name: string;
  alert: boolean; created_at: string; created_by: number | null;
}

/** One forwarded log line as GET /api/v1/devices/{id}/logs returns it, newest first. */
export interface DeviceLog {
  id: number; device_id: number; org_id: number; at: string;
  source: string; unit: string; level: string; message: string;
}

/**
 * A watchlist item merged with its latest reported running state for the device
 * panel: running is true/false from facts.services, or null when the agent has not
 * reported this item's state yet (just added, or not yet synced).
 */
export interface WatchedService { kind: WatchKind; name: string; alert: boolean; running: boolean | null }

/** A minimal device shape the applicability/column helpers need (not the full wire row). */
export interface DeviceRef { id: number; org_id: number; platform?: string }

const key = (kind: string, name: string): string => `${kind}\u0000${name}`;

const byKindName = <T extends { kind: string; name: string }>(a: T, b: T): number =>
  a.kind.localeCompare(b.kind) || a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });

/**
 * A device's reported services as a typed array, or null when the facts carry no
 * services array at all (an Android device, or a desktop that has not reported
 * yet). Entries that are not objects with a non-empty string name are skipped;
 * kind coerces to service unless it is exactly "process"; running is strict true.
 */
export function servicesOf(raw: unknown): ServiceState[] | null {
  const arr = raw && typeof raw === 'object' && !Array.isArray(raw)
    ? (raw as Record<string, unknown>).services
    : raw;
  if (!Array.isArray(arr)) return null;
  const out: ServiceState[] = [];
  for (const e of arr) {
    if (!e || typeof e !== 'object') continue;
    const o = e as Record<string, unknown>;
    if (typeof o.name !== 'string' || o.name === '') continue;
    out.push({ kind: o.kind === 'process' ? 'process' : 'service', name: o.name, running: o.running === true });
  }
  return out;
}

/** running / stopped / unknown (null) counts of a watched-service list. */
export function serviceStateCounts(items: { running: boolean | null }[]): { total: number; running: number; stopped: number; unknown: number } {
  let running = 0, stopped = 0, unknown = 0;
  for (const s of items) {
    if (s.running === true) running++;
    else if (s.running === false) stopped++;
    else unknown++;
  }
  return { total: items.length, running, stopped, unknown };
}

/** A one-line summary for the panel heading: "No watched services" / "All 3 running" / "1 of 3 stopped". */
export function serviceSummary(items: { running: boolean | null }[]): string {
  const { total, stopped } = serviceStateCounts(items);
  if (total === 0) return 'No watched services';
  if (stopped === 0) return `All ${total} running`;
  return `${stopped} of ${total} stopped`;
}

/**
 * Whether a watchlist item applies to a device: an org-scoped item to every
 * (desktop) device in its org, a device-scoped item to that one device. Android
 * is never in a watchlist, so it matches nothing.
 */
export function appliesToDevice(item: WatchlistItem, device: DeviceRef): boolean {
  if (device.platform === 'android') return false;
  if (item.scope === 'org') return item.org_id === device.org_id;
  if (item.scope === 'device') return item.device_id === device.id;
  return false;
}

/** The items that apply to a device, ordered by kind then name; input not mutated. */
export function applicableItems(items: WatchlistItem[], device: DeviceRef): WatchlistItem[] {
  return items.filter((it) => appliesToDevice(it, device)).sort(byKindName);
}

/**
 * The device panel's rows: the applicable watchlist items, each with the running
 * state the agent last reported for it (null when not reported yet), ordered by
 * kind then name. Reported states that match no item are ignored. Input not
 * mutated.
 */
export function mergeWatched(items: WatchlistItem[], states: ServiceState[]): WatchedService[] {
  const state = new Map<string, boolean>();
  for (const s of states) state.set(key(s.kind, s.name), s.running);
  return [...items].sort(byKindName).map((it) => {
    const k = key(it.kind, it.name);
    return { kind: it.kind, name: it.name, alert: it.alert, running: state.has(k) ? state.get(k)! : null };
  });
}

const SEVERITY: Record<string, Severity> = {
  emerg: 'error', emergency: 'error', alert: 'error', crit: 'error', critical: 'error', err: 'error', error: 'error',
  warning: 'warning', warn: 'warning',
  notice: 'info', info: 'info', information: 'info', informational: 'info',
  debug: 'debug', verbose: 'debug', trace: 'debug',
};

/** The severity bucket a raw level maps to (journald priority or Event Log level), else "other". */
export function levelSeverity(level: string): Severity {
  return SEVERITY[level.trim().toLowerCase()] ?? 'other';
}

/** The CSS class for a level chip, e.g. "lvl-error" (styles.css colours each bucket). */
export function levelClass(level: string): string {
  return `lvl-${levelSeverity(level)}`;
}

/** The raw level upper-cased for display, or "—" when empty. */
export function levelLabel(level: string): string {
  const t = level.trim();
  return t === '' ? '—' : t.toUpperCase();
}

const SOURCE_LABELS: Record<string, string> = {
  journald: 'journald (Linux)',
  eventlog: 'Windows Event Log',
};

/** A readable label for a log source; the raw value (or "unknown") otherwise. */
export function logSourceLabel(source: string): string {
  return SOURCE_LABELS[source] ?? (source || 'unknown');
}

/** The distinct unit names present in a log list, sorted; blank units are skipped. */
export function distinctUnits(logs: DeviceLog[]): string[] {
  const s = new Set<string>();
  for (const l of logs) if (l.unit) s.add(l.unit);
  return [...s].sort((a, b) => a.localeCompare(b, undefined, { sensitivity: 'base' }));
}

/**
 * The log rows to show: those matching the exact unit (when set) and the severity
 * bucket (when set); order preserved (the server returns newest first). Input not
 * mutated.
 */
export function filterLogs(logs: DeviceLog[], f: { unit?: string; severity?: Severity | '' }): DeviceLog[] {
  const unit = (f.unit ?? '').trim();
  const sev = f.severity ?? '';
  return logs.filter((l) => (unit === '' || l.unit === unit) && (sev === '' || levelSeverity(l.level) === sev));
}

/** "24 entries, 3 errors" (or "1 entry", "No log entries") for the panel heading. */
export function logSummary(logs: DeviceLog[]): string {
  const n = logs.length;
  if (n === 0) return 'No log entries';
  const errors = logs.filter((l) => levelSeverity(l.level) === 'error').length;
  const base = `${n} ${n === 1 ? 'entry' : 'entries'}`;
  return errors === 0 ? base : `${base}, ${errors} error${errors === 1 ? '' : 's'}`;
}

/** The count of a device's stopped watched services (0 when unknown/unreported). */
export function stoppedServiceCount(d: { services_down?: number | null }): number {
  return typeof d.services_down === 'number' && d.services_down > 0 ? d.services_down : 0;
}

/** Whether a device has at least one stopped watched service (fleet column/filter). */
export function hasStoppedService(d: { services_down?: number | null }): boolean {
  return stoppedServiceCount(d) > 0;
}
