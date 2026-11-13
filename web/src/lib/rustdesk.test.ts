import { test } from 'node:test';
import assert from 'node:assert/strict';
import { isRustdeskId, notReportedHint, relaySummary, remoteDesktopError, rustdeskUrl, technicianHint } from './rustdesk.ts';

test('rustdeskUrl is the connect scheme from the protocol', () => {
  assert.equal(rustdeskUrl('123456789'), 'rustdesk://connect/123456789');
});

test('isRustdeskId accepts 1..32 digits only', () => {
  for (const ok of ['1', '123456789', '9'.repeat(32)]) assert.equal(isRustdeskId(ok), true, ok);
  for (const bad of ['', '12a', '1 2', '9'.repeat(33), null, undefined, 123456789, '１２３']) assert.equal(isRustdeskId(bad), false, String(bad));
});

test('remoteDesktopError maps the endpoint codes and passes others through', () => {
  assert.match(remoteDesktopError('rustdesk_disabled'), /MSP_RUSTDESK_HOST/);
  assert.match(remoteDesktopError('no_rustdesk_id'), /--with-rustdesk/);
  assert.match(remoteDesktopError('not_found'), /revoked/);
  assert.match(remoteDesktopError('forbidden'), /operators/);
  assert.equal(remoteDesktopError('network_error', 'server unreachable'), 'network_error: server unreachable');
  assert.equal(remoteDesktopError('http_500'), 'http_500');
});

test('relaySummary and technicianHint name the host', () => {
  assert.equal(relaySummary('rmm.example.com'), 'ID server rmm.example.com, relay rmm.example.com');
  assert.equal(relaySummary(''), 'no relay configured');
  assert.match(technicianHint('rmm.example.com'), /rmm\.example\.com/);
  assert.match(technicianHint('rmm.example.com'), /rustdesk:\/\/connect/);
});

test('notReportedHint never tells a phone to use the desktop installer flag', () => {
  assert.match(notReportedHint('linux'), /--with-rustdesk/);
  assert.match(notReportedHint('windows'), /msp-agent rustdesk setup/);
  assert.doesNotMatch(notReportedHint('android'), /--with-rustdesk|msp-agent rustdesk setup/);
  assert.match(notReportedHint('android'), /by hand/);
});
