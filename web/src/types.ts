// Wire types of the dashboard API (docs/PROTOCOL.md section 6). Field names
// are the JSON names exactly; nullable columns are `| null`.

export type Role = 'admin' | 'operator' | 'viewer';
export type Platform = 'linux' | 'windows' | 'android';
export type DeviceStatus = 'online' | 'offline' | 'pending';
export type CommandKind = 'exec' | 'script' | 'lock' | 'ping' | 'message' | 'shell' | 'patch' | 'deploy';
/** Deploy command source (SD1): push a library file, or install an OS package. */
export type DeploySource = 'file' | 'package';
export type CommandStatus = 'queued' | 'sent' | 'done' | 'failed' | 'timeout' | 'lost' | 'cancelled';
/** Patch command fields (PM1). */
export type PatchAction = 'scan' | 'install';
export type PatchMode = 'all' | 'security' | 'ids';
export type PatchReboot = 'never' | 'if_required';
export type Severity = 'low' | 'medium' | 'high';

export interface User {
  id: number; email: string; role: Role; created_at: string; disabled_at: string | null;
  /**
   * Per-technician client access (RS2). Present on the admin users list
   * (GET /api/v1/users); absent on /me and /login, which carry only the session
   * identity. `true` = this user sees every client org; admins are always `true`.
   */
  all_orgs?: boolean;
  /** The user's assigned client org ids; `[]` (or absent) when all_orgs (RS2). */
  org_ids?: number[];
}

/** The PUT /api/v1/users/{id}/orgs body and the create-user access block (RS2). */
export interface OrgAssignment { all_orgs: boolean; org_ids: number[] }

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
  /** RustDesk id the agent last reported in its facts; null until the RustDesk client is installed (RD1). */
  rustdesk_id: string | null;
  /**
   * Patch status denormalised from the last patches report (PM1): the pending
   * count and, of those, the security count drive the fleet Updates column;
   * reboot_required and patches_checked_at round out the device page. The full
   * update list is the `patches` object on the device-detail response, not here.
   */
  pending_patches: number; security_patches: number; reboot_required: boolean;
  patches_checked_at: string | null;
}

/** Body of GET /api/v1/rustdesk: the self-hosted relay, enabled only when the server has host and key. */
export interface RustdeskInfo { enabled: boolean; host: string; key: string }

/** Body of POST /api/v1/devices/{id}/remote-desktop: the rustdesk://connect URL plus what the technician's own client needs. */
export interface RemoteDesktopOpened { url: string; rustdesk_id: string; host: string; key: string }

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
  /** Present only for kind `shell` once the relay session row exists; null before that (RS1 A). */
  shell_session?: ShellSession | null;
  /** Present only for kind `patch` (PM1): what the command asked the agent to do. */
  action?: PatchAction; mode?: PatchMode; ids?: string[]; reboot?: PatchReboot;
  /**
   * Present only for kind `deploy` (SD1): the source (file/package) and its fields.
   * A file deploy carries file_id + sha256 + dest and optionally run + run_args; a
   * package deploy carries the package name.
   */
  source?: DeploySource; file_id?: number; sha256?: string; dest?: string; run?: boolean;
  run_args?: string[]; package?: string;
}

/**
 * A server-side file-library entry (SD1): the metadata the staff endpoints return;
 * the bytes live on the server and are streamed to agents, never to the browser.
 * `org_id` is null for a global artifact usable by any client, or a client org id.
 */
export interface LibraryFile {
  id: number; org_id: number | null; name: string; size: number; sha256: string;
  content_type: string; created_at: string; created_by: number | null; created_by_email: string;
}

/**
 * Body of POST /api/v1/devices/{id}/deploy (SD1). A file deploy sets file_id, dest
 * and optionally run + run_args; a package deploy sets package. The server fills
 * the sha256 from the library file, so the client never sends it.
 */
export interface NewDeploy {
  source: DeploySource;
  file_id?: number; dest?: string; run?: boolean; run_args?: string[];
  package?: string;
  timeout_s?: number;
}

/** The shell_session block a kind `shell` command carries (RS1 A). */
export interface ShellSession {
  bytes_in: number; bytes_out: number;
  /** "" | technician_closed | agent_closed | idle_timeout | max_duration | server_shutdown | agent_never_connected */
  close_reason: string;
  transcript_truncated: boolean;
  connected_at: string | null;
}

/** Body of POST /api/v1/devices/{id}/shell: shell optional, cols/rows default 120x32 (RS1 A). */
export interface NewShell { shell?: string; cols: number; rows: number }

/** Body of POST /api/v1/devices/{id}/shell on success (201): the command, the one-time ticket and the browser WS path. */
export interface ShellOpened { command: Command; ticket: string; ws_path: string }

/** Body of POST /api/v1/devices/{id}/commands; only the keys of the kind are sent. */
export interface NewCommand { kind: CommandKind; argv?: string[]; shell?: string; body?: string; text?: string; timeout_s: number }

/**
 * Body of POST /api/v1/devices/{id}/patch (PM1): every field optional. action
 * defaults to install, mode to security, reboot to never. A scan ignores
 * mode/ids/reboot; ids is the package list for mode `ids`.
 */
export interface NewPatch { action?: PatchAction; mode?: PatchMode; ids?: string[]; reboot?: PatchReboot; timeout_s?: number }

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
  /** The full patches report (PM1); {} or absent when the device has never reported. Parse with lib/patches.patchReportOf. */
  patches?: Record<string, unknown>;
}

// --- NS1 alert notifications -------------------------------------------------
// Wire types for /api/v1/notify (docs/PROTOCOL.md). The channel secret (SMTP
// password / webhook signing secret) is write-only: it is never returned, only
// `secret_set` tells whether one is stored.

export type NotifyKind = 'email' | 'webhook';
export type WebhookFlavor = 'slack' | 'generic';
export type DeliveryEvent = 'opened' | 'resolved' | 'test';
export type DeliveryStatus = 'pending' | 'sent' | 'failed' | 'suppressed';

/** The four alert types the evaluator opens (internal/server/alerts.go). */
export const ALERT_TYPES = ['offline', 'disk_full', 'memory_high', 'battery_low'] as const;

/** Non-secret settings of an email channel (config of a kind `email` channel). */
export interface EmailConfig { host: string; port: number; from: string; to: string[]; starttls: boolean; username: string }
/** Non-secret settings of a webhook channel (config of a kind `webhook` channel). */
export interface WebhookConfig { url: string; flavor: WebhookFlavor }

export interface NotifyChannel {
  id: number; kind: NotifyKind; name: string;
  /** `null` = a global channel usable by any rule; a number scopes it to one client. */
  org_id: number | null;
  /** Kind-specific non-secret settings; shape is EmailConfig or WebhookConfig. */
  config: EmailConfig | WebhookConfig | Record<string, unknown>;
  /** Whether a secret is stored. The secret itself is never returned (write-only). */
  secret_set: boolean;
  enabled: boolean; created_at: string; created_by: number | null;
}

export interface NotifyRule {
  id: number; name: string; org_id: number | null; min_severity: Severity;
  /** Alert types the rule matches; `[]` = all types. */
  alert_types: string[];
  on_open: boolean; on_resolve: boolean; channel_id: number;
  enabled: boolean; created_at: string; created_by: number | null;
}

export interface MaintenanceWindow {
  id: number; org_id: number | null; starts_at: string; ends_at: string; reason: string;
  /** Server-computed: `now` is within [starts_at, ends_at). */
  active_now: boolean;
  created_at: string; created_by: number | null;
}

export interface NotificationDelivery {
  id: number; alert_id: number | null; rule_id: number | null; channel_id: number | null;
  org_id: number | null; event: DeliveryEvent; status: DeliveryStatus; attempts: number;
  /** Redacted — never carries a secret or a tokened URL. */
  last_error: string; detail: Record<string, unknown>;
  created_at: string; updated_at: string;
}

/** POST /notify/channels body. `secret`/`enabled` omitted take the server defaults. */
export interface NewChannel {
  kind: NotifyKind; name: string; org_id: number | null;
  config: EmailConfig | WebhookConfig; secret?: string; enabled?: boolean;
}
/** PUT /notify/channels/{id} body. Omit `secret` to keep the stored one; `""` is rejected. */
export interface ChannelUpdate {
  kind: NotifyKind; name: string; config: EmailConfig | WebhookConfig; secret?: string; enabled?: boolean;
}
/** POST /notify/rules body. */
export interface NewRule {
  name: string; org_id: number | null; min_severity: Severity; alert_types: string[];
  on_open: boolean; on_resolve: boolean; channel_id: number; enabled?: boolean;
}
/** PUT /notify/rules/{id} body (org cannot be changed). */
export interface RuleUpdate {
  name: string; min_severity: Severity; alert_types: string[];
  on_open: boolean; on_resolve: boolean; channel_id: number; enabled?: boolean;
}
/** POST /notify/windows body. `starts_at`/`ends_at` are RFC3339 timestamps. */
export interface NewWindow { org_id: number | null; starts_at: string; ends_at: string; reason: string }

export const ROLE_RANK: Record<Role, number> = { viewer: 0, operator: 1, admin: 2 };
export const canOperate = (u: User | null): boolean => !!u && ROLE_RANK[u.role] >= ROLE_RANK.operator;
export const isAdmin = (u: User | null): boolean => !!u && u.role === 'admin';
export const COMMAND_DONE = new Set<CommandStatus>(['done', 'failed', 'timeout', 'lost', 'cancelled']);
