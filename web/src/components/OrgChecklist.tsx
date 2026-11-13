import type { Org } from '../types.ts';

/**
 * The All-clients toggle plus, when it is off, a checklist of every client org
 * (RS2). Shared by the create-user form and the Edit-access dialog. Purely
 * presentational: the parent owns the `allOrgs` flag and the `selected` id set.
 */
export function OrgChecklist({ orgs, allOrgs, selected, disabled = false, onAllOrgs, onToggle }: {
  orgs: Org[]; allOrgs: boolean; selected: Set<number>; disabled?: boolean;
  onAllOrgs: (v: boolean) => void; onToggle: (id: number, on: boolean) => void;
}) {
  return (
    <div class="orgscope">
      <label class="orgscope-all">
        <input type="checkbox" checked={allOrgs} disabled={disabled} onChange={(e) => onAllOrgs(e.currentTarget.checked)} />
        All clients
      </label>
      {!allOrgs && (orgs.length === 0
        ? <p class="muted">No clients exist yet.</p>
        : <ul class="orgscope-list">
            {orgs.map((o) => (
              <li key={o.id}>
                <label>
                  <input type="checkbox" checked={selected.has(o.id)} disabled={disabled} onChange={(e) => onToggle(o.id, e.currentTarget.checked)} />
                  {o.name}
                </label>
              </li>
            ))}
          </ul>
      )}
    </div>
  );
}
