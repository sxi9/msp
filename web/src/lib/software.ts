// Installed-software inventory (facts.software / the device's software list on
// desktop agents, SI1): parsing, filtering and ordering for the device page's
// "Installed software" panel, plus grouping the cross-fleet search results.
// Pure; tested by software.test.ts. Mirrors lib/apps.ts (the Android apps panel).

/** One entry of the device software list as docs/PROTOCOL.md defines it. */
export interface SoftwareItem { name: string; version: string; publisher: string; source: string }

/** One row of GET /api/v1/software: a matching package on one visible device. */
export interface SoftwareHit {
  device_id: number; hostname: string; org_id: number; org_name: string; name: string; version: string;
}

/** The matching packages on one device, for the grouped search-results table. */
export interface DeviceSoftwareGroup {
  device_id: number; hostname: string; org_id: number; org_name: string;
  packages: { name: string; version: string }[];
}

/**
 * A device's software list as a typed array, or null when it carries no software
 * array at all (an Android device, or a desktop that has not reported yet).
 * Entries that are not objects with a non-empty string name are skipped; missing
 * version/publisher/source fall back to "".
 */
export function softwareOf(raw: unknown): SoftwareItem[] | null {
  if (!Array.isArray(raw)) return null;
  const out: SoftwareItem[] = [];
  for (const e of raw) {
    if (!e || typeof e !== 'object') continue;
    const o = e as Record<string, unknown>;
    if (typeof o.name !== 'string' || o.name === '') continue;
    out.push({
      name: o.name,
      version: typeof o.version === 'string' ? o.version : '',
      publisher: typeof o.publisher === 'string' ? o.publisher : '',
      source: typeof o.source === 'string' ? o.source : '',
    });
  }
  return out;
}

/** By name (case-insensitive), then version, then publisher; input not mutated. */
export function sortSoftware(items: SoftwareItem[]): SoftwareItem[] {
  return [...items].sort((a, b) =>
    a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }) ||
    a.version.localeCompare(b.version) ||
    a.publisher.localeCompare(b.publisher, undefined, { sensitivity: 'base' }));
}

/**
 * The rows to show: those whose name or publisher contains the trimmed query
 * (case-insensitive); sorted with sortSoftware.
 */
export function filterSoftware(items: SoftwareItem[], query: string): SoftwareItem[] {
  const q = query.trim().toLowerCase();
  return sortSoftware(items.filter((a) =>
    q === '' || a.name.toLowerCase().includes(q) || a.publisher.toLowerCase().includes(q)));
}

/** "128 packages" (or "1 package") for the panel heading. */
export function softwareSummary(items: SoftwareItem[]): string {
  const n = items.length;
  return `${n} package${n === 1 ? '' : 's'}`;
}

const SOURCE_LABELS: Record<string, string> = {
  dpkg: 'dpkg (Debian/Ubuntu)',
  rpm: 'rpm (RHEL/Fedora)',
  registry: 'Windows registry',
};

/** A readable label for a software source; the raw value (or "unknown") otherwise. */
export function sourceLabel(source: string): string {
  return SOURCE_LABELS[source] ?? (source || 'unknown');
}

/**
 * Group cross-fleet search hits by device for the results table: one group per
 * device (keeping its hostname/org), each with its matching packages sorted by
 * name then version. Groups are ordered by client (org) name, then hostname,
 * then device id, so the table reads stably. The input is not mutated.
 */
export function groupHitsByDevice(hits: SoftwareHit[]): DeviceSoftwareGroup[] {
  const byDevice = new Map<number, DeviceSoftwareGroup>();
  for (const h of hits) {
    let g = byDevice.get(h.device_id);
    if (!g) {
      g = { device_id: h.device_id, hostname: h.hostname, org_id: h.org_id, org_name: h.org_name, packages: [] };
      byDevice.set(h.device_id, g);
    }
    g.packages.push({ name: h.name, version: h.version });
  }
  const groups = [...byDevice.values()];
  for (const g of groups) {
    g.packages.sort((a, b) =>
      a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }) || a.version.localeCompare(b.version));
  }
  groups.sort((a, b) =>
    a.org_name.localeCompare(b.org_name, undefined, { sensitivity: 'base' }) ||
    a.hostname.localeCompare(b.hostname, undefined, { sensitivity: 'base' }) ||
    a.device_id - b.device_id);
  return groups;
}

/** "5 packages on 3 devices" for the search-results heading. */
export function summariseHits(hits: SoftwareHit[]): string {
  const devices = new Set(hits.map((h) => h.device_id)).size;
  return `${hits.length} package${hits.length === 1 ? '' : 's'} on ${devices} device${devices === 1 ? '' : 's'}`;
}
