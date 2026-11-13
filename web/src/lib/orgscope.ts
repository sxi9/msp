// Per-technician client access (RS2 org scoping): the pure logic the Users page
// uses to read a user's visible-org set, summarise it and build the PUT /orgs
// payload. No DOM, no network; tested by orgscope.test.ts.

import type { Org, OrgAssignment, Role } from '../types.ts';

/** A user's effective visible-org set: every org, or a specific list of ids. */
export interface OrgAccess { all: boolean; orgIds: number[] }

/** A user as far as access is concerned (the admin users list, or the session user). */
type Scopable = { role?: Role; all_orgs?: boolean; org_ids?: number[] };

/** Dedupe and sort ids ascending; input not mutated. */
function norm(ids: number[]): number[] {
  return [...new Set(ids)].sort((a, b) => a - b);
}

/**
 * A user's effective client access. admin is always all-clients — the server
 * forces all_orgs=1 for admins and ignores any user_orgs rows — and a user whose
 * all_orgs field is absent (the session user from /me, or pre-upgrade data) is
 * treated as all-clients too, matching the server's DEFAULT 1.
 */
export function accessOf(u: Scopable): OrgAccess {
  if (u.role === 'admin' || u.all_orgs !== false) return { all: true, orgIds: [] };
  return { all: false, orgIds: norm(u.org_ids ?? []) };
}

/**
 * The one-line access summary for a user row: "All clients (admin)" for an admin,
 * "All clients" for an all-orgs operator/viewer, "No clients" for a scoped user
 * with an empty set (sees nothing yet), else "N clients".
 */
export function summariseAccess(u: Scopable): string {
  if (u.role === 'admin') return 'All clients (admin)';
  const a = accessOf(u);
  if (a.all) return 'All clients';
  const n = a.orgIds.length;
  return n === 0 ? 'No clients' : `${n} client${n === 1 ? '' : 's'}`;
}

/**
 * The names of an assigned-org set, ordered as `orgs` is. An assigned id with no
 * matching org (the admin's org list did not include it) falls back to "#<id>", so
 * the UI never assumes it can name an org the API did not return.
 */
export function assignedOrgNames(orgIds: number[], orgs: Pick<Org, 'id' | 'name'>[]): string[] {
  const wanted = new Set(orgIds);
  const named = orgs.filter((o) => wanted.has(o.id)).map((o) => o.name);
  const seen = new Set(orgs.map((o) => o.id));
  const unknown = norm([...wanted].filter((id) => !seen.has(id))).map((id) => `#${id}`);
  return [...named, ...unknown];
}

/**
 * The PUT /orgs (or create-user) payload for a toggle + checklist selection.
 * all=true clears the list; otherwise only ids that exist in `knownOrgIds` survive
 * — the dashboard never sends an org it was not shown — deduped and sorted.
 */
export function toPayload(all: boolean, checked: Iterable<number>, knownOrgIds: number[]): OrgAssignment {
  if (all) return { all_orgs: true, org_ids: [] };
  const known = new Set(knownOrgIds);
  return { all_orgs: false, org_ids: norm([...checked].filter((id) => known.has(id))) };
}

/** Whether two access payloads describe the same visibility (list order ignored). */
export function sameAccess(a: OrgAssignment, b: OrgAssignment): boolean {
  if (a.all_orgs || b.all_orgs) return a.all_orgs === b.all_orgs;
  const x = norm(a.org_ids), y = norm(b.org_ids);
  return x.length === y.length && x.every((v, i) => v === y[i]);
}
