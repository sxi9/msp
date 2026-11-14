// Pending OS updates (the patches report, PM1): parsing, summarising, the
// security-only subset and the reboot badge for the device page's "Updates"
// panel and the fleet column. Pure; tested by patches.test.ts.

/** One pending update as docs/PROTOCOL.md defines it (protocol.PatchUpdate). */
export interface PatchUpdate { name: string; current: string; new: string; security: boolean; source: string }

/**
 * The patches report a device last sent (protocol.PatchReport). `manager` is
 * apt, wu or none (none = no supported package manager); `error` non-empty means
 * the last scan failed and the list is the last good one.
 */
export interface PatchReport {
  checked_at: string; manager: string; reboot_required: boolean;
  count: number; security_count: number; error: string; updates: PatchUpdate[];
}

/**
 * The device-detail `patches` object as a typed report, or null when the device
 * has never reported patches (the server sends {} then, or omits the key — e.g.
 * an Android device). Bad update entries are skipped; a missing numeric field
 * falls back to 0 and a missing string to "".
 */
export function patchReportOf(raw: unknown): PatchReport | null {
  if (!raw || typeof raw !== 'object') return null;
  const o = raw as Record<string, unknown>;
  // {} (never reported) has no manager; treat it as "no report".
  if (typeof o.manager !== 'string' || o.manager === '') return null;
  const updates: PatchUpdate[] = [];
  if (Array.isArray(o.updates)) {
    for (const e of o.updates) {
      if (!e || typeof e !== 'object') continue;
      const u = e as Record<string, unknown>;
      if (typeof u.name !== 'string' || u.name === '') continue;
      updates.push({
        name: u.name,
        current: typeof u.current === 'string' ? u.current : '',
        new: typeof u.new === 'string' ? u.new : '',
        security: u.security === true,
        source: typeof u.source === 'string' ? u.source : '',
      });
    }
  }
  return {
    checked_at: typeof o.checked_at === 'string' ? o.checked_at : '',
    manager: o.manager,
    reboot_required: o.reboot_required === true,
    count: typeof o.count === 'number' ? o.count : 0,
    security_count: typeof o.security_count === 'number' ? o.security_count : 0,
    error: typeof o.error === 'string' ? o.error : '',
    updates,
  };
}

/** The security-only subset of a report's updates; input not mutated. */
export function securityUpdates(updates: PatchUpdate[]): PatchUpdate[] {
  return updates.filter((u) => u.security);
}

/** Security updates first, then by name (case-insensitive); input not mutated. */
export function sortUpdates(updates: PatchUpdate[]): PatchUpdate[] {
  return [...updates].sort((a, b) =>
    Number(b.security) - Number(a.security) ||
    a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }));
}

export interface PatchSummary {
  manager: string;
  /** No supported package manager was found on the host. */
  unmanaged: boolean;
  /** Total pending and, of those, security; never below the agent's own totals,
   *  which can exceed the shown list when it was capped at 500. */
  pending: number;
  security: number;
  nonSecurity: number;
  rebootRequired: boolean;
  checkedAt: string;
  error: string;
  /** Up to date: a managed host with a clean, successful, pending-free report. */
  clean: boolean;
}

/**
 * Counts and flags for the panel heading. pending/security never understate the
 * agent's own totals (its list is capped at 500, so count/security_count win
 * when larger), mirroring the server's RecordPatches.
 */
export function summarisePatches(r: PatchReport): PatchSummary {
  const secList = securityUpdates(r.updates).length;
  const pending = Math.max(r.updates.length, r.count);
  const security = Math.max(secList, r.security_count);
  const unmanaged = r.manager === 'none';
  return {
    manager: r.manager,
    unmanaged,
    pending,
    security,
    nonSecurity: Math.max(0, pending - security),
    rebootRequired: r.reboot_required,
    checkedAt: r.checked_at,
    error: r.error,
    clean: !unmanaged && r.error === '' && pending === 0,
  };
}

/** The reboot-required badge label, or null when no reboot is pending. */
export function rebootBadge(r: Pick<PatchReport, 'reboot_required'>): string | null {
  return r.reboot_required ? 'Reboot required' : null;
}

/** Fleet filter predicate (RS2-style): the device has at least one pending security update. */
export function hasPendingSecurity(d: { security_patches: number }): boolean {
  return d.security_patches > 0;
}
