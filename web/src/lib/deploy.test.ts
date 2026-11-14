import { test } from 'node:test';
import assert from 'node:assert/strict';
import { destError, fileSize, filesForDevice, shortHash, validDest, validPackageName } from './deploy.ts';

test('fileSize: units and rounding', () => {
  assert.equal(fileSize(0), '0 B');
  assert.equal(fileSize(512), '512 B');
  assert.equal(fileSize(1023), '1023 B');
  assert.equal(fileSize(1024), '1.0 KB');
  assert.equal(fileSize(1536), '1.5 KB');
  assert.equal(fileSize(1048576), '1.0 MB');
  assert.equal(fileSize(5 * 1024 * 1024 * 1024), '5.0 GB');
  assert.equal(fileSize(-1), '—');
  assert.equal(fileSize(null), '—');
  assert.equal(fileSize(NaN), '—');
});

test('shortHash', () => {
  assert.equal(shortHash('abcdef0123456789', 8), 'abcdef01');
  assert.equal(shortHash('abcdef0123456789'), 'abcdef012345');
  assert.equal(shortHash(''), '');
});

test('validPackageName: charset and bounds', () => {
  for (const ok of ['htop', 'lib_ssl', 'g++', 'a.b-c+d', '0ad']) assert.equal(validPackageName(ok), true, ok);
  for (const bad of ['', 'rm -rf', 'a;b', 'pkg/evil', 'na$me', 'a'.repeat(257)]) assert.equal(validPackageName(bad), false, bad);
});

test('validDest: absolute, no "..", unix/windows/UNC', () => {
  for (const ok of ['/opt/acme/installer.sh', '/x', 'C:\\Program Files\\x.exe', 'c:/tools/x', '\\\\server\\share\\x']) {
    assert.equal(validDest(ok), true, ok);
  }
  for (const bad of ['', 'relative/path', '../escape', '/opt/../etc/passwd', 'C:\\a\\..\\b', 'x.sh']) {
    assert.equal(validDest(bad), false, bad);
  }
});

test('destError: specific messages, null when valid', () => {
  assert.equal(destError('/opt/x'), null);
  assert.equal(destError('   '), 'A destination path is required.');
  assert.match(destError('/a/../b')!, /\.\./);
  assert.match(destError('relative')!, /absolute/);
});

test('filesForDevice: own org plus globals, input not mutated', () => {
  const files = [
    { id: 1, org_id: null },
    { id: 2, org_id: 1 },
    { id: 3, org_id: 2 },
    { id: 4, org_id: null },
  ];
  const copy = [...files];
  assert.deepEqual(filesForDevice(files, 1).map((f) => f.id), [1, 2, 4]);
  assert.deepEqual(filesForDevice(files, 2).map((f) => f.id), [1, 3, 4]);
  assert.deepEqual(filesForDevice(files, 9).map((f) => f.id), [1, 4]);
  assert.deepEqual(files, copy);
});
