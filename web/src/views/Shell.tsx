// The dashboard remote shell (RS1 D): a "Remote shell" button on the device
// page that, for admin|operator on an online desktop device, opens an xterm
// terminal wired to the same-origin WebSocket relay. Keystrokes go out as
// binary frames, fit-addon resizes go out as resize control frames, and
// incoming binary is written to the terminal. A status line and a Close button
// (and leaving the page) close the session as technician_closed. Finished shell
// commands in the history carry a Transcript link.
import { useEffect, useRef, useState } from 'preact/hooks';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { ApiError, MOCK, api, errorText } from '../api.ts';
import { mockShellSocket } from '../mock.ts';
import {
  SOCKET_OPEN, closeReasonText, encodeKeystroke, parseControl, resizeFrame, shellWsUrl, statusText,
  type ShellSocket, type ShellState,
} from '../lib/shell.ts';
import { COMMAND_DONE, canOperate, type Command, type Device as DeviceT, type User } from '../types.ts';

/** Only admin|operator see the button, and only for an online, non-revoked desktop device (RS1 D). */
const shellAvailable = (d: DeviceT, user: User): boolean =>
  canOperate(user) && d.platform !== 'android' && d.status === 'online' && !d.revoked;

export function RemoteShell({ device, user }: { device: DeviceT; user: User }) {
  const [open, setOpen] = useState(false);
  // Collapse the panel if the device is no longer eligible (e.g. went offline).
  useEffect(() => { if (open && !shellAvailable(device, user)) setOpen(false); }, [device.status, device.revoked]);
  if (!shellAvailable(device, user)) return null;
  const kind = device.platform === 'windows' ? 'PowerShell' : 'shell';
  return (
    <div class="card shell-card">
      <div class="form-row">
        <h2>Remote shell</h2>
        {!open && <button type="button" class="btn btn-primary" onClick={() => setOpen(true)}>Open remote shell</button>}
        {!open && <span class="muted">Opens an interactive {kind} on {device.hostname}. The session is audited and transcribed.</span>}
      </div>
      {open && <ShellPanel device={device} onClose={() => setOpen(false)} />}
    </div>
  );
}

const TERM_THEME = { background: '#0b0e12', foreground: '#e6e9ee', cursor: '#e6e9ee', selectionBackground: '#2a3139' };
const TERM_FONT = 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace';

function ShellPanel({ device, onClose }: { device: DeviceT; onClose: () => void }) {
  const host = useRef<HTMLDivElement>(null);
  const sockRef = useRef<ShellSocket | null>(null);
  const [status, setStatus] = useState<{ state: ShellState; reason: string }>({ state: 'waiting_for_agent', reason: '' });
  const [error, setError] = useState('');
  const [opening, setOpening] = useState(true);

  useEffect(() => {
    let disposed = false;
    let teardown: (() => void) | undefined;
    const term = new Terminal({ cursorBlink: true, fontFamily: TERM_FONT, fontSize: 13, scrollback: 5000, theme: TERM_THEME });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host.current!);
    try { fit.fit(); } catch { /* container may be hidden; default size is used */ }

    (async () => {
      try {
        const opened = await api.openShell(device.id, { cols: term.cols, rows: term.rows });
        if (disposed) return;
        const url = shellWsUrl(location, opened.ws_path, opened.ticket);
        const sock: ShellSocket = MOCK ? mockShellSocket(opened.command.shell || 'bash') : (new WebSocket(url) as unknown as ShellSocket);
        sock.binaryType = 'arraybuffer';
        sockRef.current = sock;
        setOpening(false);
        sock.onopen = () => { try { fit.fit(); } catch { /* ignore */ } sock.send(resizeFrame(term.cols, term.rows)); };
        sock.onmessage = (ev) => {
          if (typeof ev.data === 'string') {
            const c = parseControl(ev.data);
            if (!c) return;
            if (c.type === 'status') setStatus({ state: c.state, reason: c.reason });
            else term.write(`\r\n[process exited with code ${c.code}]\r\n`);
            return;
          }
          term.write(new Uint8Array(ev.data));
        };
        sock.onclose = () => { if (!disposed) setStatus((s) => (s.state === 'closed' ? s : { state: 'closed', reason: s.reason })); };
        sock.onerror = () => { if (!disposed) setError('The shell connection failed.'); };
        const dataSub = term.onData((d) => { const s = sockRef.current; if (s && s.readyState === SOCKET_OPEN) s.send(encodeKeystroke(d)); });
        const resizeSub = term.onResize(({ cols, rows }) => { const s = sockRef.current; if (s && s.readyState === SOCKET_OPEN) s.send(resizeFrame(cols, rows)); });
        const onWin = () => { try { fit.fit(); } catch { /* ignore */ } };
        window.addEventListener('resize', onWin);
        term.focus();
        teardown = () => { dataSub.dispose(); resizeSub.dispose(); window.removeEventListener('resize', onWin); };
      } catch (e) {
        if (!disposed) { setOpening(false); setError(shellOpenError(e)); }
      }
    })();

    return () => {
      disposed = true;
      teardown?.();
      try { sockRef.current?.close(1000, 'technician_closed'); } catch { /* ignore */ }
      try { term.dispose(); } catch { /* ignore */ }
    };
  }, [device.id]);

  return (
    <div class="shell-panel">
      <div class="shell-status">
        <span class={`shell-dot ${status.state}`} />
        <span>{opening ? 'Opening…' : statusText(status.state, status.reason)}</span>
        <span class="right"><button type="button" class="btn btn-sm" onClick={onClose}>Close</button></span>
      </div>
      {error && <p class="error">{error}</p>}
      <div class="shell-term" ref={host} />
    </div>
  );
}

/** Maps the POST /shell error codes (RS1 A/D) to the sentence shown in the panel. */
function shellOpenError(e: unknown): string {
  if (e instanceof ApiError) {
    switch (e.code) {
      case 'device_offline': return 'The device is offline; open a shell when it is back online.';
      case 'shell_busy': return 'A shell is already being opened for this device; wait for it to finish.';
      case 'too_many_shells': return 'The server is relaying its maximum number of shells; try again shortly.';
      case 'unsupported_on_platform': return 'Remote shell is only available on desktop devices.';
      case 'unsupported_on_device': return 'This device is revoked; re-enroll it first.';
      case 'forbidden': return 'Opening a remote shell needs the operator or admin role.';
      default: return e.detail ? `${e.code}: ${e.detail}` : e.code;
    }
  }
  return errorText(e);
}

/**
 * The shell-session summary shown under a kind `shell` command in the history
 * (RS1 A/D): bytes relayed and close reason, with a Transcript link once the
 * command has finished (GET /api/v1/shell/{id}/transcript).
 */
export function ShellResult({ command: c }: { command: Command }) {
  const s = c.shell_session;
  const done = COMMAND_DONE.has(c.status);
  return (
    <div class="shell-summary">
      {s && (
        <span class="muted">
          {s.bytes_in} B in / {s.bytes_out} B out
          {s.close_reason ? `, ${closeReasonText(s.close_reason)}` : ''}
          {s.transcript_truncated ? ' — transcript truncated at 1 MiB' : ''}
        </span>
      )}
      {done && <a href={api.transcriptUrl(c.id)} target="_blank" rel="noreferrer">Transcript</a>}
    </div>
  );
}
