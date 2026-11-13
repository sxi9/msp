// Pure framing and sizing helpers for the Remote shell terminal (RS1 spec B).
// No DOM and no xterm here, so every function runs under `node --test`. The
// WebSocket relay carries two frame kinds, identical on both legs:
//   - binary frames: raw terminal bytes (keystrokes browser->agent; PTY output
//     agent->browser).
//   - text frames: JSON control (resize browser->agent; exit agent->browser;
//     status server->browser only).

/** Terminal size limits from RS1 A: cols clamped 20..500, rows 5..200. */
export const COLS_MIN = 20, COLS_MAX = 500, ROWS_MIN = 5, ROWS_MAX = 200;

export interface Dims { cols: number; rows: number }

const clamp = (n: number, lo: number, hi: number): number => {
  const i = Math.floor(n);
  if (!Number.isFinite(i)) return lo;
  return i < lo ? lo : i > hi ? hi : i;
};

export const clampCols = (n: number): number => clamp(n, COLS_MIN, COLS_MAX);
export const clampRows = (n: number): number => clamp(n, ROWS_MIN, ROWS_MAX);
export const clampDims = (d: Dims): Dims => ({ cols: clampCols(d.cols), rows: clampRows(d.rows) });

/** The resize control frame (a text frame) the browser sends; dims are clamped first. */
export function resizeFrame(cols: number, rows: number): string {
  const d = clampDims({ cols, rows });
  return JSON.stringify({ type: 'resize', cols: d.cols, rows: d.rows });
}

/** Keystrokes (the strings xterm's onData yields) encoded to binary-frame bytes. */
export function encodeKeystroke(data: string): Uint8Array {
  return new TextEncoder().encode(data);
}

export type ShellState = 'waiting_for_agent' | 'connected' | 'closed';

/** A parsed incoming text control frame. */
export type Control =
  | { type: 'status'; state: ShellState; reason: string }
  | { type: 'exit'; code: number };

const STATES: readonly string[] = ['waiting_for_agent', 'connected', 'closed'];

/** Parse an incoming text control frame; null when it is not a frame we handle. */
export function parseControl(text: string): Control | null {
  let v: unknown;
  try { v = JSON.parse(text); } catch { return null; }
  if (!v || typeof v !== 'object') return null;
  const o = v as Record<string, unknown>;
  if (o.type === 'status' && typeof o.state === 'string' && STATES.includes(o.state)) {
    return { type: 'status', state: o.state as ShellState, reason: typeof o.reason === 'string' ? o.reason : '' };
  }
  if (o.type === 'exit' && typeof o.code === 'number') {
    return { type: 'exit', code: o.code };
  }
  return null;
}

/** Human text for each close reason (RS1 A/B); "" means none recorded. */
const CLOSE_REASON: Record<string, string> = {
  technician_closed: 'closed by you',
  agent_closed: 'the shell on the device ended',
  idle_timeout: 'closed after 30 minutes idle',
  max_duration: 'closed at the maximum session duration',
  server_shutdown: 'the server is shutting down',
  agent_never_connected: 'the device never connected',
};

export const closeReasonText = (reason: string): string => CLOSE_REASON[reason] ?? (reason || 'closed');

/** The status-line text for a state (and its close reason when closed). */
export function statusText(state: ShellState, reason = ''): string {
  switch (state) {
    case 'waiting_for_agent': return 'Waiting for the device to connect…';
    case 'connected': return 'Connected';
    case 'closed': return `Closed — ${closeReasonText(reason)}`;
  }
}

/** Same-origin ws:// (or wss:// under TLS) URL for the browser leg, carrying the one-time ticket. */
export function shellWsUrl(loc: { protocol: string; host: string }, wsPath: string, ticket: string): string {
  const scheme = loc.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${loc.host}${wsPath}?ticket=${encodeURIComponent(ticket)}`;
}

/** WebSocket.OPEN; the one readyState value the panel checks before sending. */
export const SOCKET_OPEN = 1;

/**
 * The minimal socket shape the terminal panel drives. The real `WebSocket`
 * satisfies it structurally, and the `?mock=1` echo stub implements it by hand,
 * so the panel code is identical for both.
 */
export interface ShellSocket {
  binaryType: string;
  readyState: number;
  send(data: ArrayBufferView | string): void;
  close(code?: number, reason?: string): void;
  onopen: ((ev?: unknown) => void) | null;
  onmessage: ((ev: { data: ArrayBuffer | string }) => void) | null;
  onclose: ((ev?: unknown) => void) | null;
  onerror: ((ev?: unknown) => void) | null;
}
