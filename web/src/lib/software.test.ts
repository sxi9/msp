import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  filterSoftware, groupHitsByDevice, softwareOf, softwareSummary, sortSoftware, sourceLabel, summariseHits,
  type SoftwareHit,
} from './software.ts';

const sw = (name: string, version = '1.0', publisher = 'Ubuntu', source = 'dpkg') => ({ name, version, publisher, source });

const inventory = [
  sw('openssl', '3.0.13', 'Ubuntu Developers'),
  sw('curl', '8.5.0', 'Ubuntu Developers'),
  sw('Nginx-core', '1.24.0', 'Ubuntu Developers'),
  sw('7zip', '', '', 'registry'),
  sw('Acme VPN', '2.1.0', 'Acme Corp', 'registry'),
];

test('softwareOf: null without an array, typed entries otherwise, bad entries skipped', () => {
  assert.equal(softwareOf({ kernel: '6.8' }), null);
  assert.equal(softwareOf('nope'), null);
  assert.equal(softwareOf(undefined), null);
  assert.deepEqual(softwareOf([]), []);
  assert.deepEqual(softwareOf([
    { name: 'openssl', version: '3.0', publisher: 'Ubuntu', source: 'dpkg' },
    { name: 'curl' },
    { name: '', version: 'ghost' },
    'junk', null, 7,
    { version: 'no name' },
  ]), [
    { name: 'openssl', version: '3.0', publisher: 'Ubuntu', source: 'dpkg' },
    { name: 'curl', version: '', publisher: '', source: '' },
  ]);
});

test('sortSoftware: name case-insensitive, then version; input not mutated', () => {
  const copy = [...inventory];
  assert.deepEqual(sortSoftware(inventory).map((a) => a.name),
    ['7zip', 'Acme VPN', 'curl', 'Nginx-core', 'openssl']);
  assert.deepEqual(inventory, copy);
  // Same name, different version: lower version first.
  assert.deepEqual(sortSoftware([sw('vim', '9.1.1'), sw('vim', '9.1.0')]).map((a) => a.version), ['9.1.0', '9.1.1']);
});

test('filterSoftware: case-insensitive name/publisher search, sorted', () => {
  assert.deepEqual(filterSoftware(inventory, '').map((a) => a.name),
    ['7zip', 'Acme VPN', 'curl', 'Nginx-core', 'openssl']);
  assert.deepEqual(filterSoftware(inventory, 'NGINX').map((a) => a.name), ['Nginx-core']);
  assert.deepEqual(filterSoftware(inventory, '  acme  ').map((a) => a.name), ['Acme VPN']);
  // Matches the publisher, not just the name.
  assert.deepEqual(filterSoftware(inventory, 'ubuntu').map((a) => a.name), ['curl', 'Nginx-core', 'openssl']);
  assert.deepEqual(filterSoftware(inventory, 'nothing'), []);
});

test('softwareSummary counts', () => {
  assert.equal(softwareSummary([]), '0 packages');
  assert.equal(softwareSummary([sw('a')]), '1 package');
  assert.equal(softwareSummary(inventory), '5 packages');
});

test('sourceLabel maps known sources, passes through others', () => {
  assert.equal(sourceLabel('dpkg'), 'dpkg (Debian/Ubuntu)');
  assert.equal(sourceLabel('rpm'), 'rpm (RHEL/Fedora)');
  assert.equal(sourceLabel('registry'), 'Windows registry');
  assert.equal(sourceLabel('snap'), 'snap');
  assert.equal(sourceLabel(''), 'unknown');
});

const hits: SoftwareHit[] = [
  { device_id: 2, hostname: 'ws-xray', org_id: 1, org_name: 'Acme Dental', name: 'openssl', version: '3.0.1' },
  { device_id: 1, hostname: 'ws-reception', org_id: 1, org_name: 'Acme Dental', name: 'openssl', version: '3.0.13' },
  { device_id: 1, hostname: 'ws-reception', org_id: 1, org_name: 'Acme Dental', name: 'openssl-dev', version: '3.0.13' },
  { device_id: 5, hostname: 'srv-files', org_id: 2, org_name: 'Zed Legal', name: 'openssl', version: '3.0.2' },
];

test('groupHitsByDevice: one group per device, ordered by org then hostname, packages sorted', () => {
  const groups = groupHitsByDevice(hits);
  assert.deepEqual(groups.map((g) => [g.org_name, g.hostname, g.device_id]), [
    ['Acme Dental', 'ws-reception', 1],
    ['Acme Dental', 'ws-xray', 2],
    ['Zed Legal', 'srv-files', 5],
  ]);
  // ws-reception's two matches, sorted by name.
  assert.deepEqual(groups[0].packages.map((p) => p.name), ['openssl', 'openssl-dev']);
  assert.equal(groups[1].packages.length, 1);
});

test('groupHitsByDevice: empty input, input not mutated', () => {
  assert.deepEqual(groupHitsByDevice([]), []);
  const copy = JSON.parse(JSON.stringify(hits));
  groupHitsByDevice(hits);
  assert.deepEqual(hits, copy);
});

test('summariseHits counts packages and distinct devices', () => {
  assert.equal(summariseHits([]), '0 packages on 0 devices');
  assert.equal(summariseHits(hits), '4 packages on 3 devices');
  assert.equal(summariseHits([hits[0]]), '1 package on 1 device');
});
