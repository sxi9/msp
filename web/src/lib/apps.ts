// Installed-apps inventory (facts.apps, Android agents): parsing, filtering and
// ordering for the device page's "Installed apps" panel. Pure; tested by apps.test.ts.

/** One entry of facts.apps as docs/PROTOCOL.md defines it. */
export interface InstalledApp { package: string; label: string; version: string; system: boolean }

/**
 * facts.apps as a typed list, or null when the facts carry no apps array at all
 * (desktop agents in phase 1). Entries that are not objects with a string package
 * are skipped; a missing label falls back to the package, a missing version to "".
 */
export function appsOf(facts: Record<string, unknown>): InstalledApp[] | null {
  const raw = facts.apps;
  if (!Array.isArray(raw)) return null;
  const out: InstalledApp[] = [];
  for (const e of raw) {
    if (!e || typeof e !== 'object') continue;
    const o = e as Record<string, unknown>;
    if (typeof o.package !== 'string' || o.package === '') continue;
    out.push({
      package: o.package,
      label: typeof o.label === 'string' && o.label !== '' ? o.label : o.package,
      version: typeof o.version === 'string' ? o.version : '',
      system: o.system === true,
    });
  }
  return out;
}

/** Non-system apps first, then by label (case-insensitive), then by package; input not mutated. */
export function sortApps(apps: InstalledApp[]): InstalledApp[] {
  return [...apps].sort((a, b) =>
    Number(a.system) - Number(b.system) ||
    a.label.localeCompare(b.label, undefined, { sensitivity: 'base' }) ||
    a.package.localeCompare(b.package));
}

/**
 * The rows to show: system apps only when showSystem, and only those whose label or
 * package contains the trimmed query (case-insensitive); sorted with sortApps.
 */
export function filterApps(apps: InstalledApp[], query: string, showSystem: boolean): InstalledApp[] {
  const q = query.trim().toLowerCase();
  return sortApps(apps.filter((a) =>
    (showSystem || !a.system) &&
    (q === '' || a.label.toLowerCase().includes(q) || a.package.toLowerCase().includes(q))));
}

/** "12 apps (3 system)" for the panel heading. */
export function appsSummary(apps: InstalledApp[]): string {
  const system = apps.filter((a) => a.system).length;
  const n = apps.length;
  return `${n} app${n === 1 ? '' : 's'}${system ? ` (${system} system)` : ''}`;
}
