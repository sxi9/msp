import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  clampCols, clampRows, clampDims, closeReasonText, encodeKeystroke, parseControl,
  resizeFrame, shellWsUrl, statusText, COLS_MAX, COLS_MIN, ROWS_MAX, ROWS_MIN,
} from './shell.ts';

test('clamp cols/rows to the RS1 limits and floor fractions', () => {
  assert.equal(clampCols(120), 120);
  assert.equal(clampCols(0), COLS_MIN);
  assert.equal(clampCols(5), COLS_MIN);
  assert.equal(clampCols(9999), COLS_MAX);
  assert.equal(clampCols(80.9), 80);
  assert.equal(clampCols(NaN), COLS_MIN);
  assert.equal(clampRows(32), 32);
  assert.equal(clampRows(1), ROWS_MIN);
  assert.equal(clampRows(9999), ROWS_MAX);
  assert.deepEqual(clampDims({ cols: 10, rows: 1000 }), { cols: COLS_MIN, rows: ROWS_MAX });
});

test('resizeFrame emits a clamped resize control frame', () => {
  assert.equal(resizeFrame(120, 32), '{"type":"resize","cols":120,"rows":32}');
  assert.deepEqual(JSON.parse(resizeFrame(1, 1)), { type: 'resize', cols: COLS_MIN, rows: ROWS_MIN });
  assert.deepEqual(JSON.parse(resizeFrame(10000, 10000)), { type: 'resize', cols: COLS_MAX, rows: ROWS_MAX });
});

test('encodeKeystroke round-trips through UTF-8 bytes', () => {
  assert.deepEqual([...encodeKeystroke('ab')], [97, 98]);
  assert.deepEqual([...encodeKeystroke('\r')], [13]);
  assert.equal(new TextDecoder().decode(encodeKeystroke('héllo')), 'héllo');
});

test('parseControl reads status frames with a default reason', () => {
  assert.deepEqual(parseControl('{"type":"status","state":"connected"}'), { type: 'status', state: 'connected', reason: '' });
  assert.deepEqual(parseControl('{"type":"status","state":"waiting_for_agent","reason":""}'), { type: 'status', state: 'waiting_for_agent', reason: '' });
  assert.deepEqual(parseControl('{"type":"status","state":"closed","reason":"idle_timeout"}'), { type: 'status', state: 'closed', reason: 'idle_timeout' });
});

test('parseControl reads exit frames', () => {
  assert.deepEqual(parseControl('{"type":"exit","code":0}'), { type: 'exit', code: 0 });
  assert.deepEqual(parseControl('{"type":"exit","code":-1}'), { type: 'exit', code: -1 });
});

test('parseControl rejects junk, unknown types and bad states', () => {
  assert.equal(parseControl('not json'), null);
  assert.equal(parseControl('42'), null);
  assert.equal(parseControl('null'), null);
  assert.equal(parseControl('{"type":"resize","cols":1,"rows":1}'), null);
  assert.equal(parseControl('{"type":"status","state":"bogus"}'), null);
  assert.equal(parseControl('{"type":"exit"}'), null);
  assert.equal(parseControl('{"type":"exit","code":"0"}'), null);
});

test('statusText and closeReasonText describe each state', () => {
  assert.match(statusText('waiting_for_agent'), /Waiting/);
  assert.equal(statusText('connected'), 'Connected');
  assert.match(statusText('closed', 'technician_closed'), /closed by you/);
  assert.match(statusText('closed', 'agent_never_connected'), /never connected/);
  assert.match(statusText('closed', ''), /Closed/);
  assert.equal(closeReasonText(''), 'closed');
  assert.equal(closeReasonText('max_duration'), 'closed at the maximum session duration');
  assert.equal(closeReasonText('something_new'), 'something_new');
});

test('shellWsUrl builds a same-origin ws/wss URL with the ticket', () => {
  assert.equal(
    shellWsUrl({ protocol: 'http:', host: 'localhost:8080' }, '/api/v1/shell/7/ws', 'tkt123'),
    'ws://localhost:8080/api/v1/shell/7/ws?ticket=tkt123',
  );
  assert.equal(
    shellWsUrl({ protocol: 'https:', host: 'rmm.example.com' }, '/api/v1/shell/42/ws', 'a b+c'),
    'wss://rmm.example.com/api/v1/shell/42/ws?ticket=a%20b%2Bc',
  );
});
