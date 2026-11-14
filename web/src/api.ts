// fetch() wrapper for /api/v1 plus one typed function per endpoint. With
// ?mock=1 in the URL every call is answered in-memory by mock.ts instead.
import type {
  Alert, AuditEntry, ChannelUpdate, Command, Device, DeviceDetail, EnrollmentToken, Heartbeat,
  MaintenanceWindow, NewChannel, NewCommand, NewRule, NewShell, NewWindow, NotificationDelivery,
  NotifyChannel, NotifyRule, Org, OrgAssignment, PresetLibrary, RemoteDesktopOpened, RuleUpdate,
  RustdeskInfo, ShellOpened, Summary, TokenCreated, User,
} from './types.ts';
import { mockRequest } from './mock.ts';

export const MOCK = typeof location !== 'undefined' && /(^|[?&])mock=1(&|$)/.test(location.search);

export class ApiError extends Error {
  status: number;
  code: string;
  detail: string;
  constructor(status: number, code: string, detail = '') {
    super(detail ? `${code}: ${detail}` : code);
    this.status = status;
    this.code = code;
    this.detail = detail;
  }
}

export const errorText = (e: unknown): string =>
  e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e);

export async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  let status: number;
  let data: { error?: string; detail?: string } | null = null;
  if (MOCK) {
    const r = await mockRequest(method, path, body);
    status = r.status;
    data = r.body as typeof data;
  } else {
    const headers: Record<string, string> = { 'X-Requested-With': 'msp' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    let res: Response;
    try {
      res = await fetch(path, { method, headers, credentials: 'same-origin', body: body === undefined ? undefined : JSON.stringify(body) });
    } catch {
      throw new ApiError(0, 'network_error', 'server unreachable');
    }
    status = res.status;
    if (status !== 204) {
      try { data = await res.json(); } catch {
        if (res.ok) throw new ApiError(status, 'bad_response', 'expected a JSON body');
        data = null;
      }
    }
  }
  if (status >= 200 && status < 300) return data as T;
  if (status === 401) location.hash = '#/login';
  throw new ApiError(status, data?.error ?? `http_${status}`, data?.detail ?? '');
}

const qs = (params: Record<string, string | number | undefined>): string => {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '' && v !== 0) p.set(k, String(v));
  const s = p.toString();
  return s ? `?${s}` : '';
};

export const api = {
  login: (email: string, password: string) => request<{ user: User }>('POST', '/api/v1/login', { email, password }),
  logout: () => request<void>('POST', '/api/v1/logout'),
  me: () => request<{ user: User }>('GET', '/api/v1/me'),
  changePassword: (current_password: string, new_password: string) =>
    request<void>('POST', '/api/v1/me/password', { current_password, new_password }),
  summary: () => request<Summary>('GET', '/api/v1/summary'),
  listOrgs: () => request<{ orgs: Org[] }>('GET', '/api/v1/orgs'),
  createOrg: (name: string) => request<{ org: Org }>('POST', '/api/v1/orgs', { name }),
  getOrg: (id: number) => request<{ org: Org; tokens: EnrollmentToken[] }>('GET', `/api/v1/orgs/${id}`),
  createToken: (orgId: number, label: string, max_uses: number, ttl_hours: number) =>
    request<TokenCreated>('POST', `/api/v1/orgs/${orgId}/tokens`, { label, max_uses, ttl_hours }),
  revokeToken: (orgId: number, id: number) => request<void>('POST', `/api/v1/orgs/${orgId}/tokens/${id}/revoke`),
  listDevices: (f: { org_id?: number; status?: string; q?: string } = {}) =>
    request<{ devices: Device[] }>('GET', `/api/v1/devices${qs(f)}`),
  getDevice: (id: number) => request<DeviceDetail>('GET', `/api/v1/devices/${id}`),
  heartbeats: (id: number, limit = 120) => request<{ heartbeats: Heartbeat[] }>('GET', `/api/v1/devices/${id}/heartbeats?limit=${limit}`),
  createCommand: (deviceId: number, c: NewCommand) => request<{ command: Command }>('POST', `/api/v1/devices/${deviceId}/commands`, c),
  listCommands: (deviceId: number, limit = 50) => request<{ commands: Command[] }>('GET', `/api/v1/devices/${deviceId}/commands?limit=${limit}`),
  getCommand: (id: number) => request<{ command: Command }>('GET', `/api/v1/commands/${id}`),
  // Opens an interactive shell relay (RS1 D): 201 with the command, a one-time
  // ticket and the browser WS path; the panel then connects ws_path?ticket=.
  openShell: (deviceId: number, s: NewShell) => request<ShellOpened>('POST', `/api/v1/devices/${deviceId}/shell`, s),
  // The recorded agent->browser transcript of a finished shell command
  // (text/plain). Opened as a link, not fetched through request().
  transcriptUrl: (commandId: number) => `/api/v1/shell/${commandId}/transcript`,
  presets: () => request<PresetLibrary>('GET', '/api/v1/presets'),
  rustdesk: () => request<RustdeskInfo>('GET', '/api/v1/rustdesk'),
  remoteDesktop: (deviceId: number) => request<RemoteDesktopOpened>('POST', `/api/v1/devices/${deviceId}/remote-desktop`),
  revokeDevice: (id: number) => request<void>('POST', `/api/v1/devices/${id}/revoke`),
  deleteDevice: (id: number) => request<void>('DELETE', `/api/v1/devices/${id}`),
  listAlerts: (state: 'open' | 'all', org_id?: number) => request<{ alerts: Alert[] }>('GET', `/api/v1/alerts${qs({ state, org_id })}`),
  ackAlert: (id: number) => request<void>('POST', `/api/v1/alerts/${id}/ack`),
  listAudit: (f: { limit?: number; org_id?: number; before?: number }) => request<{ entries: AuditEntry[] }>('GET', `/api/v1/audit${qs(f)}`),
  listUsers: () => request<{ users: User[] }>('GET', '/api/v1/users'),
  // `access` scopes an operator/viewer to a set of client orgs; omitted (and
  // ignored by the server) for admins, who always see every org (RS2).
  createUser: (email: string, password: string, role: string, access?: OrgAssignment) =>
    request<{ user: User }>('POST', '/api/v1/users', { email, password, role, ...(access ?? {}) }),
  // Replaces a user's client access (RS2); 400 if the target is an admin and
  // all_orgs is false (admins cannot be scoped).
  setUserOrgs: (id: number, access: OrgAssignment) => request<void>('PUT', `/api/v1/users/${id}/orgs`, access),
  disableUser: (id: number) => request<void>('POST', `/api/v1/users/${id}/disable`),

  // --- NS1 alert notifications (/api/v1/notify) ---
  // Channels. GET needs viewer; writes need operator (admin for a global
  // org_id-null row). The secret is write-only: it is sent on create/update when
  // present and never returned — a channel row carries only `secret_set`. On
  // update, omitting `secret` keeps the stored one; sending `""` is rejected.
  listNotifyChannels: () => request<{ channels: NotifyChannel[] }>('GET', '/api/v1/notify/channels'),
  createNotifyChannel: (c: NewChannel) => request<{ channel: NotifyChannel }>('POST', '/api/v1/notify/channels', c),
  updateNotifyChannel: (id: number, c: ChannelUpdate) => request<{ channel: NotifyChannel }>('PUT', `/api/v1/notify/channels/${id}`, c),
  deleteNotifyChannel: (id: number) => request<void>('DELETE', `/api/v1/notify/channels/${id}`),
  // Enqueues a `test` delivery immediately and returns it; the dispatcher sends it.
  testNotifyChannel: (id: number) => request<{ delivery: NotificationDelivery }>('POST', `/api/v1/notify/channels/${id}/test`),
  // Rules.
  listNotifyRules: () => request<{ rules: NotifyRule[] }>('GET', '/api/v1/notify/rules'),
  createNotifyRule: (r: NewRule) => request<{ rule: NotifyRule }>('POST', '/api/v1/notify/rules', r),
  updateNotifyRule: (id: number, r: RuleUpdate) => request<{ rule: NotifyRule }>('PUT', `/api/v1/notify/rules/${id}`, r),
  deleteNotifyRule: (id: number) => request<void>('DELETE', `/api/v1/notify/rules/${id}`),
  // Maintenance windows (one-shot; add/delete only).
  listMaintenanceWindows: () => request<{ windows: MaintenanceWindow[] }>('GET', '/api/v1/notify/windows'),
  createMaintenanceWindow: (w: NewWindow) => request<{ window: MaintenanceWindow }>('POST', '/api/v1/notify/windows', w),
  deleteMaintenanceWindow: (id: number) => request<void>('DELETE', `/api/v1/notify/windows/${id}`),
  // Delivery log (read-only, org-scoped; viewer may read).
  listDeliveries: (f: { status?: string; org_id?: number; limit?: number } = {}) =>
    request<{ deliveries: NotificationDelivery[] }>('GET', `/api/v1/notify/deliveries${qs(f)}`),
};
