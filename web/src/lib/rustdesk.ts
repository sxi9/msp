// Pure helpers for the Remote desktop card (RD1). No DOM, so they are unit
// tested under node --test.

/** The URL scheme the official RustDesk client registers: rustdesk://connect/<id>. */
export const rustdeskUrl = (id: string): string => `rustdesk://connect/${id}`;

/** A RustDesk id as the protocol accepts it: 1..32 ASCII digits. */
export const isRustdeskId = (id: unknown): id is string => typeof id === 'string' && /^[0-9]{1,32}$/.test(id);

/** The relay as a technician types it into their own client (Settings > Network > ID server). */
export const relaySummary = (host: string): string => (host ? `ID server ${host}, relay ${host}` : 'no relay configured');

/**
 * Maps the API error codes of POST /devices/{id}/remote-desktop to the sentence
 * shown under the Connect button; anything else is passed through.
 */
export function remoteDesktopError(code: string, detail = ''): string {
  switch (code) {
    case 'rustdesk_disabled': return 'Remote desktop is not configured on the server (MSP_RUSTDESK_HOST and MSP_RUSTDESK_KEY).';
    case 'no_rustdesk_id': return 'This device has not reported a RustDesk id yet; install the RustDesk client with --with-rustdesk.';
    case 'not_found': return 'This device is not available for remote desktop (unknown or revoked).';
    case 'forbidden': return 'Only operators and admins can open a remote desktop session.';
    default: return detail ? `${code}: ${detail}` : code;
  }
}

/**
 * What the card says when a device has reported no id. Linux and Windows have an
 * installer flag and `msp-agent rustdesk setup`; neither exists for Android, where
 * the official app is installed and pointed at the relay by hand and the Connect
 * button is never offered (docs/GUIDE.md section 8).
 */
export const notReportedHint = (platform: string): string =>
  platform === 'android'
    ? 'not available for phones: the Android agent reports no RustDesk id. Install the official RustDesk app on the phone and point it at the relay by hand; connecting is not offered from the dashboard.'
    : 'not reported; install the RustDesk client with --with-rustdesk (or run msp-agent rustdesk setup)';

/** The one-line hint under the card: what the technician's own machine needs. */
export const technicianHint = (host: string): string =>
  `Your own PC needs the RustDesk client pointed at the same relay (${relaySummary(host)}, same key); the Connect button opens rustdesk://connect/<id> in it.`;
