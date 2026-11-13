// Wire types of the dashboard API (docs/PROTOCOL.md section 6). Field names
// are the JSON names exactly; nullable columns are `| null`.

export type Role = 'admin' | 'operator' | 'viewer';
export type Platform = 'linux' | 'windows' | 'android';
export type DeviceStatus = 'online' | 'offline' | 'pending';
export type CommandKind = 'exec' | 'script' | 'lock' | 'ping' | 'message';
export type CommandStatus = 'queued' | 'sent' | 'done' | 'failed' | 'timeout' | 'lost' | 'cancelled';
export type Severity = 'low' | 'medium' | 'high';

export interface User { id: number; email: string; role: Role; created_at: string; disabled_at: string | null }

export interface Summary { orgs: number; devices: number; online: number; offline: number; pending: number; open_alerts: number }

export interface Org { id: number; name: string; created_at: string; device_count: number; online_count: number; open_alerts: number }

export interface EnrollmentToken {
  id: number; org_id: number; token_prefix: string; label: string; max_uses: number; uses: number;
  expires_at: string; created_by: number | null; created_at: string; revoked_at: string | null;
}

export interface TokenCreated {
  token: EnrollmentToken;
  plaintext: string;
  install: { linux: string; windows: string; android: string };
}

export interface Device {
  id: number; org_id: number; org_name: string; device_uid: string; hostname: string;
  platform: Platform; arch: string; os_version: string; agent_version: string; interval_s: number;
  status: DeviceStatus; enrolled_at: string; last_seen_at: string | null; last_ip: string; revoked: boolean;
  cpu_pct: number | null; mem_used_pct: number | null; disk_used_pct: number | null;
  battery_pct: number | null; battery_charging: boolean | null; uptime_s: number | null;
  open_alerts: number; can_run_commands: boolean;
}

export interface Heartbeat {
  at: string; cpu_pct: number | null; mem_used_pct: number | null; disk_used_pct: number | null;
  battery_pct: number | null; uptime_s: number | null;
}

export interface Command {
  id: number; device_id: number; org_id: number; created_by: number; created_by_email: string;
  kind: CommandKind; argv: string[]; shell: string; body: string; text: string; timeout_s: number;
  status: CommandStatus; created_at: string; sent_at: string | null; started_at: string | null;
  finished_at: string | null; exit_code: number | null; stdout: string; stderr: string;
  truncated: boolean; error: string;
}

/** Body of POST /api/v1/devices/{id}/commands; only the keys of the kind are sent. */
export interface NewCommand { kind: CommandKind; argv?: string[]; shell?: string; body?: string; text?: string; timeout_s: number }

/** One entry of GET /api/v1/presets: argv for exec, shell + body for script; timeout_s always filled in. */
export interface Preset { label: string; kind: 'exec' | 'script'; argv?: string[]; shell?: string; body?: string; timeout_s: number }

/** Body of GET /api/v1/presets: the builtin set or the MSP_PRESETS file, in file order. */
export interface PresetLibrary { presets: { linux: Preset[]; windows: Preset[] }; source: 'builtin' | 'file'; path: string }

export interface Alert {
  id: number; org_id: number; org_name: string; device_id: number; hostname: string; type: string;
  severity: Severity; message: string; opened_at: string; resolved_at: string | null;
  acknowledged_by: number | null; acknowledged_at: string | null;
}

export interface AuditEntry {
  id: number; at: string; actor_type: 'user' | 'device' | 'system'; actor_id: number | null; actor_label: string;
  org_id: number | null; action: string; target_type: string; target_id: number | null; ip: string;
  detail: Record<string, unknown>;
}

export interface DeviceDetail {
  device: Device;
  facts: Record<string, unknown>;
  metrics: Record<string, unknown>;
  alerts: Alert[];
  commands: Command[];
}

export const ROLE_RANK: Record<Role, number> = { viewer: 0, operator: 1, admin: 2 };
export const canOperate = (u: User | null): boolean => !!u && ROLE_RANK[u.role] >= ROLE_RANK.operator;
export const isAdmin = (u: User | null): boolean => !!u && u.role === 'admin';
export const COMMAND_DONE = new Set<CommandStatus>(['done', 'failed', 'timeout', 'lost', 'cancelled']);
