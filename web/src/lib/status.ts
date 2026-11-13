// Fleet ordering, filtering and grouping. Pure; tested by status.test.ts.

/** The subset of DEVICE these helpers look at (the full type is in ../types.ts). */
export interface FleetDevice {
  id: number;
  org_id: number;
  org_name: string;
  hostname: string;
  status: string;
  open_alerts: number;
}

export function statusRank(d: FleetDevice): number {
  if (d.open_alerts > 0) return 0;
  switch (d.status) {
    case 'offline': return 1;
    case 'online': return 2;
    default: return 3; // pending
  }
}

/** Alerting first, then offline, online, pending; ties by org name then hostname. */
export function sortDevices<T extends FleetDevice>(devices: T[]): T[] {
  return [...devices].sort((a, b) =>
    statusRank(a) - statusRank(b) ||
    a.org_name.localeCompare(b.org_name) ||
    a.hostname.localeCompare(b.hostname) ||
    a.id - b.id);
}

export interface OrgGroup<T> { org_id: number; org_name: string; devices: T[] }

/** Groups (in org-name order) whose devices keep the incoming order. */
export function groupByOrg<T extends FleetDevice>(devices: T[]): OrgGroup<T>[] {
  const groups = new Map<number, OrgGroup<T>>();
  for (const d of devices) {
    let g = groups.get(d.org_id);
    if (!g) {
      g = { org_id: d.org_id, org_name: d.org_name, devices: [] };
      groups.set(d.org_id, g);
    }
    g.devices.push(d);
  }
  return [...groups.values()].sort((a, b) => a.org_name.localeCompare(b.org_name) || a.org_id - b.org_id);
}

/** q = case-insensitive hostname substring; status/orgId empty or 0 = any. */
export function filterDevices<T extends FleetDevice>(devices: T[], q: string, status: string, orgId: number): T[] {
  const needle = q.trim().toLowerCase();
  return devices.filter((d) =>
    (!needle || d.hostname.toLowerCase().includes(needle)) &&
    (!status || d.status === status) &&
    (!orgId || d.org_id === orgId));
}
