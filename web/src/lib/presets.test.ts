import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EMPTY_PRESETS, loadPresets, mergePresets, presetFill, presetsFor, resetPresets } from './presets.ts';
import type { Preset, PresetLibrary } from '../types.ts';

const uptime: Preset = { label: 'Uptime', kind: 'exec', argv: ['uptime'], timeout_s: 60 };
const disk: Preset = { label: 'Disk usage', kind: 'script', shell: 'sh', body: 'df -h\n', timeout_s: 120 };
const lib: PresetLibrary = { presets: { linux: [uptime, disk], windows: [] }, source: 'file', path: '/data/presets.json' };

test('mergePresets fills missing arrays, drops junk and defaults bad timeouts', () => {
  assert.deepEqual(mergePresets(null), EMPTY_PRESETS);
  assert.deepEqual(mergePresets({}), EMPTY_PRESETS);
  assert.deepEqual(mergePresets({ presets: { linux: [uptime] } } as unknown as PresetLibrary), {
    presets: { linux: [uptime], windows: [] }, source: 'builtin', path: '',
  });
  const merged = mergePresets({
    presets: { linux: [uptime, { label: 'Lock', kind: 'lock' }, null, { ...disk, timeout_s: 0 }], windows: 'nope' },
    source: 'file', path: '/p.json',
  } as unknown as PresetLibrary);
  assert.deepEqual(merged, { presets: { linux: [uptime, { ...disk, timeout_s: 60 }], windows: [] }, source: 'file', path: '/p.json' });
  assert.equal(mergePresets({ ...lib, source: 'weird' as 'file' }).source, 'builtin');
});

test('mergePresets keeps server order', () => {
  const m = mergePresets({ presets: { linux: [disk, uptime], windows: [uptime] }, source: 'file', path: '' });
  assert.deepEqual(m.presets.linux.map((p) => p.label), ['Disk usage', 'Uptime']);
  assert.deepEqual(m.presets.windows.map((p) => p.label), ['Uptime']);
});

test('presetsFor picks the platform list and gives android nothing', () => {
  assert.deepEqual(presetsFor(lib, 'linux'), [uptime, disk]);
  assert.deepEqual(presetsFor(lib, 'windows'), []);
  assert.deepEqual(presetsFor(lib, 'android'), []);
  assert.deepEqual(presetsFor(lib, 'plan9'), []);
  assert.deepEqual(presetsFor(null, 'linux'), []);
});

test('presetFill maps exec to one argument per line and script to body, shell and timeout', () => {
  assert.deepEqual(presetFill({ label: 'R', kind: 'exec', argv: ['shutdown', '/r', '/t', '0'], timeout_s: 30 }), { mode: 'exec', text: 'shutdown\n/r\n/t\n0', shell: '', timeoutText: '30' });
  assert.deepEqual(presetFill(disk), { mode: 'script', text: 'df -h\n', shell: 'sh', timeoutText: '120' });
  assert.deepEqual(presetFill({ label: 'E', kind: 'exec', timeout_s: 60 }), { mode: 'exec', text: '', shell: '', timeoutText: '60' });
});

test('loadPresets fetches once per session and falls back to empty on failure', async () => {
  resetPresets();
  let calls = 0;
  const fetcher = async () => { calls++; return lib; };
  const a = await loadPresets(fetcher);
  const b = await loadPresets(fetcher);
  assert.equal(calls, 1);
  assert.deepEqual(a, lib);
  assert.equal(a, b);

  resetPresets();
  let failures = 0;
  const failing = async (): Promise<PresetLibrary> => { failures++; throw new Error('network_error'); };
  assert.deepEqual(await loadPresets(failing), EMPTY_PRESETS);
  // A failure is not cached: the next load tries the server again.
  assert.deepEqual(await loadPresets(failing), EMPTY_PRESETS);
  assert.equal(failures, 2);
  assert.deepEqual(await loadPresets(fetcher), lib);
  assert.equal(calls, 2);
  resetPresets();
});
