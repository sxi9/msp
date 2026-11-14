// QR rendering for the two-factor setup flow (TF1). Wraps the pinned pure-JS
// encoder qrcode-generator@1.4.4 (added to web/package.json) so the otpauth URI
// an authenticator scans can be drawn in the browser with no canvas and no DOM
// dependency in the pure path. The matrix builder is pure (unit tested); the SVG
// builder returns a string the Account view injects. Dark modules are always
// black on an explicit white background so a phone camera reads the code
// regardless of the dashboard's light/dark theme.
import qrcode from 'qrcode-generator';

/** Error-correction level passed to the encoder; 'M' (~15%) is the default. */
export type QRLevel = 'L' | 'M' | 'Q' | 'H';

/** A square module matrix: `true` = a dark module. No quiet zone is included. */
export type QRMatrix = boolean[][];

/**
 * Encodes `text` (an otpauth:// URI here) into a QR module matrix. Byte mode,
 * automatic version (0), EC level `level`. Throws on empty input so a caller
 * never renders a blank code. ASCII input only, which every otpauth URI the
 * server builds is.
 */
export function qrMatrix(text: string, level: QRLevel = 'M'): QRMatrix {
  if (typeof text !== 'string' || text.length === 0) throw new Error('qrMatrix: empty input');
  const qr = qrcode(0, level);
  qr.addData(text); // default Byte mode
  qr.make();
  const n = qr.getModuleCount();
  const rows: QRMatrix = [];
  for (let r = 0; r < n; r++) {
    const row: boolean[] = [];
    for (let c = 0; c < n; c++) row.push(qr.isDark(r, c));
    rows.push(row);
  }
  return rows;
}

/** Options for {@link qrSvg}: the quiet-zone margin (in modules) and the colours. */
export interface QRSvgOptions { margin?: number; dark?: string; light?: string; level?: QRLevel }

/**
 * Renders `text` as a self-contained SVG string (no external refs, no script).
 * The viewBox is in module units, so the element scales to whatever CSS width
 * it is given; dark modules are emitted as one `<path>` of unit squares over a
 * light background rect. A standard 4-module quiet zone is added by default.
 */
export function qrSvg(text: string, opts: QRSvgOptions = {}): string {
  const margin = opts.margin ?? 4;
  const dark = opts.dark ?? '#000000';
  const light = opts.light ?? '#ffffff';
  const m = qrMatrix(text, opts.level ?? 'M');
  const n = m.length;
  const size = n + margin * 2;
  let d = '';
  for (let r = 0; r < n; r++) {
    for (let c = 0; c < n; c++) {
      if (m[r][c]) d += `M${c + margin} ${r + margin}h1v1h-1z`;
    }
  }
  return (
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${size} ${size}" ` +
    `shape-rendering="crispEdges" role="img" aria-label="Two-factor QR code">` +
    `<rect width="${size}" height="${size}" fill="${light}"/>` +
    `<path fill="${dark}" d="${d}"/>` +
    `</svg>`
  );
}
