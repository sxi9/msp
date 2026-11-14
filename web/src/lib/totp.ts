// Pure helpers for the two-factor dashboard UI (TF1). No DOM, no crypto: the
// TOTP secret and the verification live on the server (internal/server/totp.go).
// These only format and validate what the operator types, parse the otpauth URI
// the setup endpoint returns, and lay out the one-time recovery codes. Unit
// tested under `node --test`.

/** The base32 alphabet (RFC 4648), uppercase, used for secrets and recovery codes. */
const BASE32 = /^[A-Z2-7]+$/;

/**
 * Strips everything but digits from a typed TOTP code and caps it at six, so a
 * field can clean "123 456" or a pasted "code: 123456" down to "123456".
 */
export function formatCode(raw: string): string {
  return (raw ?? '').replace(/\D/g, '').slice(0, 6);
}

/** A complete 6-digit TOTP code. */
export function isValidCode(s: string): boolean {
  return /^\d{6}$/.test(s ?? '');
}

/**
 * Normalises a typed recovery code to the stored shape `XXXXX-XXXXX`: uppercases,
 * keeps only base32 characters, and reinserts the hyphen after five characters.
 * A user can paste "abcde fghij" or "ABCDEFGHIJ" and get "ABCDE-FGHIJ".
 */
export function formatRecoveryCode(raw: string): string {
  const s = (raw ?? '').toUpperCase().replace(/[^A-Z2-7]/g, '').slice(0, 10);
  return s.length > 5 ? `${s.slice(0, 5)}-${s.slice(5)}` : s;
}

/** A complete recovery code: two base32 groups of five, hyphen-separated. */
export function isValidRecoveryCode(s: string): boolean {
  return /^[A-Z2-7]{5}-[A-Z2-7]{5}$/.test((s ?? '').toUpperCase());
}

/** Groups a base32 secret into space-separated blocks of four for readable display. */
export function groupSecret(secret: string): string {
  return (secret ?? '').toUpperCase().replace(/\s/g, '').replace(/(.{4})/g, '$1 ').trim();
}

/** The parsed fields of an `otpauth://` URI (the subset the server emits). */
export interface Otpauth {
  type: 'totp' | 'hotp';
  /** The raw decoded label, e.g. "MSP:user@example.com". */
  label: string;
  /** From the label prefix or the `issuer` query parameter; "" if neither. */
  issuer: string;
  /** The label with the issuer prefix stripped, e.g. "user@example.com". */
  account: string;
  /** Base32, uppercased, no padding or spaces. */
  secret: string;
  algorithm: string; // SHA1 default
  digits: number;    // 6 default
  period: number;    // 30 default
}

/**
 * Parses an `otpauth://totp/...` URI. Returns null for anything that is not a
 * well-formed totp/hotp URI with a base32 secret, so a caller can fall back to
 * showing the manual secret.
 */
export function parseOtpauth(uri: string): Otpauth | null {
  if (typeof uri !== 'string' || !/^otpauth:\/\//i.test(uri)) return null;
  let u: URL;
  try { u = new URL(uri); } catch { return null; }
  const type = u.host.toLowerCase();
  if (type !== 'totp' && type !== 'hotp') return null;
  const secret = (u.searchParams.get('secret') ?? '').replace(/[\s=]/g, '').toUpperCase();
  if (!secret || !BASE32.test(secret)) return null;
  let label: string;
  try { label = decodeURIComponent(u.pathname.replace(/^\/+/, '')); } catch { return null; }
  let issuer = u.searchParams.get('issuer') ?? '';
  let account = label;
  const colon = label.indexOf(':');
  if (colon >= 0) {
    const prefix = label.slice(0, colon).trim();
    account = label.slice(colon + 1).trim();
    if (!issuer) issuer = prefix;
  }
  const digits = toPositiveInt(u.searchParams.get('digits'), 6);
  const period = toPositiveInt(u.searchParams.get('period'), 30);
  const algorithm = (u.searchParams.get('algorithm') ?? 'SHA1').toUpperCase();
  return { type, label, issuer, account, secret, algorithm, digits, period };
}

function toPositiveInt(raw: string | null, fallback: number): number {
  const n = Number(raw);
  return Number.isInteger(n) && n > 0 ? n : fallback;
}

/**
 * The plain-text block of recovery codes for the copy/download affordance: a
 * short header plus one code per line, newline-terminated so a saved .txt ends
 * cleanly.
 */
export function recoveryCodesText(codes: string[]): string {
  const header = 'MSP two-factor recovery codes — each works once. Keep them somewhere safe.';
  return `${header}\n\n${(codes ?? []).join('\n')}\n`;
}

/** A sensible download filename for the recovery-code text. */
export const RECOVERY_FILENAME = 'msp-recovery-codes.txt';
