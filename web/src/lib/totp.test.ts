import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  formatCode, formatRecoveryCode, groupSecret, isValidCode, isValidRecoveryCode,
  parseOtpauth, recoveryCodesText,
} from './totp.ts';

test('formatCode keeps only digits and caps at six', () => {
  assert.equal(formatCode('123 456'), '123456');
  assert.equal(formatCode('code: 123456 extra'), '123456');
  assert.equal(formatCode('1234567'), '123456');
  assert.equal(formatCode('abc'), '');
  assert.equal(formatCode(''), '');
});

test('isValidCode accepts exactly six digits', () => {
  assert.equal(isValidCode('123456'), true);
  assert.equal(isValidCode('12345'), false);
  assert.equal(isValidCode('1234567'), false);
  assert.equal(isValidCode('12345a'), false);
  assert.equal(isValidCode(''), false);
});

test('formatRecoveryCode uppercases, keeps base32 and reinserts the hyphen', () => {
  assert.equal(formatRecoveryCode('abcde fghij'), 'ABCDE-FGHIJ');
  assert.equal(formatRecoveryCode('abcdefghij'), 'ABCDE-FGHIJ');
  assert.equal(formatRecoveryCode('ab2de-fg7ij'), 'AB2DE-FG7IJ');
  assert.equal(formatRecoveryCode('ab1de'), 'ABDE'); // 1 is not in the base32 alphabet
  assert.equal(formatRecoveryCode('abc'), 'ABC');
});

test('isValidRecoveryCode wants two base32 groups of five', () => {
  assert.equal(isValidRecoveryCode('ABCDE-FGHIJ'), true);
  assert.equal(isValidRecoveryCode('abcde-fghij'), true); // uppercased before the test
  assert.equal(isValidRecoveryCode('AB2DE-FG7IJ'), true);
  assert.equal(isValidRecoveryCode('ABCDE-FGHI'), false);
  assert.equal(isValidRecoveryCode('ABCDEFGHIJ'), false);
  assert.equal(isValidRecoveryCode('ABCD1-FGHIJ'), false); // 1 not base32
  assert.equal(isValidRecoveryCode(''), false);
});

test('groupSecret breaks a secret into blocks of four', () => {
  assert.equal(groupSecret('JBSWY3DPEHPK3PXP'), 'JBSW Y3DP EHPK 3PXP');
  assert.equal(groupSecret('jbswy3dp'), 'JBSW Y3DP');
  assert.equal(groupSecret('ABC'), 'ABC');
});

test('parseOtpauth parses a server-built totp URI', () => {
  const o = parseOtpauth('otpauth://totp/MSP:user@example.com?secret=JBSWY3DPEHPK3PXP&issuer=MSP&digits=6&period=30');
  assert.ok(o);
  assert.equal(o!.type, 'totp');
  assert.equal(o!.label, 'MSP:user@example.com');
  assert.equal(o!.issuer, 'MSP');
  assert.equal(o!.account, 'user@example.com');
  assert.equal(o!.secret, 'JBSWY3DPEHPK3PXP');
  assert.equal(o!.digits, 6);
  assert.equal(o!.period, 30);
  assert.equal(o!.algorithm, 'SHA1');
});

test('parseOtpauth decodes a percent-encoded label and derives issuer from the prefix', () => {
  const o = parseOtpauth('otpauth://totp/MSP%3Aamy%40acme.co?secret=MFRGGZDF');
  assert.ok(o);
  assert.equal(o!.label, 'MSP:amy@acme.co');
  assert.equal(o!.issuer, 'MSP');
  assert.equal(o!.account, 'amy@acme.co');
  assert.equal(o!.digits, 6); // defaults applied
  assert.equal(o!.period, 30);
});

test('parseOtpauth prefers an explicit issuer query over the prefix', () => {
  const o = parseOtpauth('otpauth://totp/Other:amy@acme.co?secret=MFRGGZDF&issuer=RealIssuer');
  assert.equal(o!.issuer, 'RealIssuer');
  assert.equal(o!.account, 'amy@acme.co');
});

test('parseOtpauth rejects malformed input', () => {
  assert.equal(parseOtpauth('https://example.com/?secret=AB'), null);
  assert.equal(parseOtpauth('otpauth://totp/MSP:user'), null); // no secret
  assert.equal(parseOtpauth('otpauth://totp/MSP:user?secret=not base32!!'), null);
  assert.equal(parseOtpauth('otpauth://foo/x?secret=MFRGGZDF'), null); // not totp/hotp
  assert.equal(parseOtpauth(''), null);
});

test('recoveryCodesText is a header plus one code per line, newline-terminated', () => {
  const t = recoveryCodesText(['AAAAA-BBBBB', 'CCCCC-DDDDD']);
  assert.match(t, /recovery codes/i);
  assert.ok(t.endsWith('AAAAA-BBBBB\nCCCCC-DDDDD\n'));
});
