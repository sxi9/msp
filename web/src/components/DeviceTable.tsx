import type { Device } from '../types.ts';
import { groupByOrg, sortDevices } from '../lib/status.ts';
import { hasPendingSecurity } from '../lib/patches.ts';
import { ago, pct } from '../lib/format.ts';
import { StatusDot } from './StatusDot.tsx';
import { Bar } from './Bar.tsx';

export const PLATFORM_SHORT: Record<string, string> = { linux: 'LIN', windows: 'WIN', android: 'AND' };

export function Battery({ pct: p, charging }: { pct: number | null; charging: boolean | null }) {
  if (p === null) return <span class="muted">{pct(null)}</span>;
  return <span class={p <= 15 && !charging ? 'warn' : ''}>{p}%{charging ? ' ⚡' : ''}</span>;
}

/** Fleet Updates cell: the pending count, emphasised when any is a security update (PM1). */
export function Updates({ device: d }: { device: Device }) {
  if (d.pending_patches <= 0) return <span class="muted">0</span>;
  const title = `${d.pending_patches} pending${d.security_patches > 0 ? `, ${d.security_patches} security` : ''}`;
  if (hasPendingSecurity(d)) return <span class="badge badge-amber" title={title}>{d.pending_patches}</span>;
  return <span title={title}>{d.pending_patches}</span>;
}

/** Fleet table: sorted by urgency, grouped under org header rows (the Org page hides them). */
export function DeviceTable({ devices, grouped = true, empty }: { devices: Device[]; grouped?: boolean; empty: string }) {
  if (devices.length === 0) return <p class="empty">{empty}</p>;
  const sorted = sortDevices(devices);
  const groups = grouped ? groupByOrg(sorted) : [{ org_id: 0, org_name: '', devices: sorted }];
  return (
    <table class="grid">
      <thead>
        <tr><th></th><th>Hostname</th><th>OS</th><th>Arch</th><th>Last seen</th><th>CPU</th><th>Memory</th><th>Disk</th><th>Battery</th><th>Alerts</th><th>Updates</th><th>Agent</th></tr>
      </thead>
      <tbody>
        {groups.map((g) => [
          grouped && <tr class="orghead" key={`org-${g.org_id}`}><td colSpan={12}><a href={`#/orgs/${g.org_id}`}>{g.org_name}</a> <span class="muted">{g.devices.length}</span></td></tr>,
          ...g.devices.map((d) => (
            <tr key={d.id} class={d.revoked ? 'revoked' : ''}>
              <td data-label="Status"><StatusDot status={d.status} alerting={d.open_alerts > 0} /></td>
              <td data-label="Hostname"><a href={`#/devices/${d.id}`}>{d.hostname}</a>{d.revoked && <span class="chip">revoked</span>}</td>
              <td data-label="OS"><span class="chip">{PLATFORM_SHORT[d.platform] ?? d.platform}</span> {d.os_version}</td>
              <td data-label="Arch" class="muted">{d.arch}</td>
              <td data-label="Last seen">{ago(d.last_seen_at)}</td>
              <td data-label="CPU"><Bar value={d.cpu_pct} /></td>
              <td data-label="Memory"><Bar value={d.mem_used_pct} /></td>
              <td data-label="Disk"><Bar value={d.disk_used_pct} /></td>
              <td data-label="Battery"><Battery pct={d.battery_pct} charging={d.battery_charging} /></td>
              <td data-label="Alerts">{d.open_alerts > 0 ? <span class="badge badge-amber">{d.open_alerts}</span> : <span class="muted">0</span>}</td>
              <td data-label="Updates"><Updates device={d} /></td>
              <td data-label="Agent" class="muted">{d.agent_version || '—'}</td>
            </tr>
          )),
        ])}
      </tbody>
    </table>
  );
}
