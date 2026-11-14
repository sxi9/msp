import { test } from 'node:test';
import assert from 'node:assert/strict';
import { qrMatrix, qrSvg } from './qr.ts';

const URI = 'otpauth://totp/MSP:user@example.com?secret=JBSWY3DPEHPK3PXP&issuer=MSP&digits=6&period=30';

test('qrMatrix returns a non-empty square matrix for a known input', () => {
  const m = qrMatrix(URI);
  assert.ok(m.length >= 21, `module count ${m.length}`);
  for (const row of m) assert.equal(row.length, m.length, 'every row is the matrix width');
  // The top-left finder pattern makes the corner module dark in every valid QR.
  assert.equal(m[0][0], true);
  // A real code has both dark and light modules.
  const flat = m.flat();
  assert.ok(flat.some((x) => x), 'has a dark module');
  assert.ok(flat.some((x) => !x), 'has a light module');
});

test('qrMatrix is deterministic for the same input', () => {
  assert.deepEqual(qrMatrix(URI), qrMatrix(URI));
});

test('qrMatrix rejects empty input rather than drawing a blank code', () => {
  assert.throws(() => qrMatrix(''), /empty input/);
  // @ts-expect-error — guarding the non-string case at runtime too.
  assert.throws(() => qrMatrix(null), /empty input/);
});

test('qrSvg wraps the matrix in a scalable, self-contained SVG', () => {
  const svg = qrSvg(URI);
  assert.match(svg, /^<svg /);
  assert.match(svg, /viewBox="0 0 \d+ \d+"/);
  assert.match(svg, /<path fill="#000000" d="M/);
  assert.match(svg, /<rect width="\d+" height="\d+" fill="#ffffff"\/>/);
  assert.doesNotMatch(svg, /<script/i);
});

test('qrSvg viewBox is the module count plus a quiet zone on both sides', () => {
  const n = qrMatrix(URI).length;
  const svg = qrSvg(URI, { margin: 4 });
  const expected = n + 8;
  assert.match(svg, new RegExp(`viewBox="0 0 ${expected} ${expected}"`));
});

test('qrSvg honours custom colours', () => {
  const svg = qrSvg(URI, { dark: '#111111', light: '#eeeeee' });
  assert.match(svg, /fill="#111111"/);
  assert.match(svg, /fill="#eeeeee"/);
});
