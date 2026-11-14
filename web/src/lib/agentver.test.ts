import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  compareVersions, isOutdated, manifestForPlatform, outdatedCount, platformArch,
  releaseForDevice, selfUpdateEnabled, targetVersion, type AgentManifest,
} from './agentver.ts';

const sha = 'a'.repeat(64);
const manifest: AgentManifest = {
  version: '0.2.0',
  files: {
    'linux-amd64': { name: 'msp-agent-linux-amd64', sha256: sha },
    'linux-arm64': { name: 'msp-agent-linux-arm64', sha256: sha },
    'windows-amd64': { name: 'msp-agent-windows-amd64.exe', sha256: sha },
  },
};
const off: AgentManifest = { version: '', files: {} };
const dev = (platform: string, arch: string, agent_version: string) => ({ platform, arch, agent_version });

test('selfUpdateEnabled: needs a version and at least one file', () => {
  assert.equal(selfUpdateEnabled(manifest), true);
  assert.equal(selfUpdateEnabled(off), false);
  assert.equal(selfUpdateEnabled(null), false);
  assert.equal(selfUpdateEnabled(undefined), false);
  assert.equal(selfUpdateEnabled({ version: '0.2.0', files: {} }), false);
});

test('platformArch and manifestForPlatform', () => {
  assert.equal(platformArch(dev('linux', 'amd64', '0.1.0')), 'linux-amd64');
  assert.equal(manifestForPlatform(manifest, 'linux-amd64')?.name, 'msp-agent-linux-amd64');
  assert.equal(manifestForPlatform(manifest, 'windows-amd64')?.name, 'msp-agent-windows-amd64.exe');
  assert.equal(manifestForPlatform(manifest, 'linux-386'), null);
  assert.equal(manifestForPlatform(null, 'linux-amd64'), null);
  assert.equal(manifestForPlatform(off, 'linux-amd64'), null);
});

test('releaseForDevice resolves the device platform-arch', () => {
  assert.equal(releaseForDevice(manifest, dev('windows', 'amd64', '0.1.0'))?.name, 'msp-agent-windows-amd64.exe');
  assert.equal(releaseForDevice(manifest, dev('linux', '386', '0.1.0')), null);
});

test('compareVersions: numeric segments compare numerically', () => {
  assert.equal(compareVersions('0.2.0', '0.2.0'), 0);
  assert.equal(compareVersions('0.1.0', '0.2.0'), -1);
  assert.equal(compareVersions('0.2.0', '0.1.0'), 1);
  // 0.10 is newer than 0.2 (numeric, not lexical).
  assert.equal(compareVersions('0.2.0', '0.10.0'), -1);
  assert.equal(compareVersions('0.10.0', '0.9.0'), 1);
  // A missing trailing segment is older than an explicit one.
  assert.equal(compareVersions('0.2', '0.2.0'), -1);
  // Non-numeric suffixes fall back to lexical comparison.
  assert.equal(compareVersions('1.0.0-rc1', '1.0.0-rc2'), -1);
  assert.equal(compareVersions('1.0.0', '1.0.0'), 0);
});

test('isOutdated: true only when the platform has a differing release', () => {
  // Outdated: older than the release.
  assert.equal(isOutdated(dev('linux', 'amd64', '0.1.0'), manifest), true);
  assert.equal(isOutdated(dev('windows', 'amd64', '0.1.9'), manifest), true);
  // Up to date.
  assert.equal(isOutdated(dev('linux', 'amd64', '0.2.0'), manifest), false);
  // No release entry for this platform-arch.
  assert.equal(isOutdated(dev('linux', '386', '0.1.0'), manifest), false);
  assert.equal(isOutdated(dev('android', 'arm64', '0.1.0'), manifest), false);
  // Self-update off.
  assert.equal(isOutdated(dev('linux', 'amd64', '0.1.0'), off), false);
  assert.equal(isOutdated(dev('linux', 'amd64', '0.1.0'), null), false);
});

test('targetVersion is the release when outdated, else null', () => {
  assert.equal(targetVersion(dev('linux', 'amd64', '0.1.0'), manifest), '0.2.0');
  assert.equal(targetVersion(dev('linux', 'amd64', '0.2.0'), manifest), null);
  assert.equal(targetVersion(dev('linux', '386', '0.1.0'), manifest), null);
});

test('outdatedCount counts only resolvable, differing platforms', () => {
  const devices = [
    dev('linux', 'amd64', '0.1.0'), // outdated
    dev('linux', 'amd64', '0.2.0'), // current
    dev('windows', 'amd64', '0.1.0'), // outdated
    dev('android', 'arm64', '0.1.0'), // no release
    dev('linux', '386', '0.1.0'), // no release
  ];
  assert.equal(outdatedCount(devices, manifest), 2);
  assert.equal(outdatedCount(devices, off), 0);
  assert.equal(outdatedCount(devices, null), 0);
});
