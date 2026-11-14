// Pure helpers for the agent self-update UI (AU1): the release manifest shape,
// a dotted-version comparison, the per-platform lookup and the "outdated" check.
// No DOM, no fetch — unit-tested with node --test (agentver.test.ts).

/** One platform-arch entry of the release manifest: the file served at /dl/<name> and its hash. */
export interface AgentManifestFile { name: string; sha256: string }

/**
 * The agent release manifest (GET /api/v1/agent-release). `version` is the single
 * released version for every file; `files` maps a "<platform>-<arch>" key (e.g.
 * "linux-amd64") to its entry. When self-update is off the server returns
 * `{ version: '', files: {} }`.
 */
export interface AgentManifest { version: string; files: Record<string, AgentManifestFile> }

/** The minimal device shape the version helpers need (a subset of types.Device). */
export interface VersionedDevice { platform: string; arch: string; agent_version: string }

/** Self-update is on when a manifest has a non-empty version and at least one file. */
export function selfUpdateEnabled(m: AgentManifest | null | undefined): boolean {
  return !!m && m.version !== '' && !!m.files && Object.keys(m.files).length > 0;
}

/** The manifest key for a device: "<platform>-<arch>", e.g. "linux-amd64". */
export function platformArch(d: VersionedDevice): string {
  return `${d.platform}-${d.arch}`;
}

/** The manifest entry for a platform-arch key, or null when absent / off. */
export function manifestForPlatform(m: AgentManifest | null | undefined, key: string): AgentManifestFile | null {
  if (!m || !m.files) return null;
  return m.files[key] ?? null;
}

/** The release entry for a device's own platform-arch, or null. */
export function releaseForDevice(m: AgentManifest | null | undefined, d: VersionedDevice): AgentManifestFile | null {
  return manifestForPlatform(m, platformArch(d));
}

/**
 * Compare two dotted versions (e.g. "0.2.0" vs "0.10.1"): -1 if a < b, 1 if a > b,
 * 0 if equal. Segments split on . + _ - ; numeric segments compare numerically
 * (so 0.10 > 0.2), non-numeric segments lexically, and a missing segment counts
 * as the empty string (so "0.2" < "0.2.0").
 */
export function compareVersions(a: string, b: string): number {
  const pa = a.split(/[.+_-]/);
  const pb = b.split(/[.+_-]/);
  const n = Math.max(pa.length, pb.length);
  for (let i = 0; i < n; i++) {
    const sa = pa[i] ?? '';
    const sb = pb[i] ?? '';
    if (sa === sb) continue;
    const numA = /^\d+$/.test(sa);
    const numB = /^\d+$/.test(sb);
    if (numA && numB) {
      const na = Number(sa), nb = Number(sb);
      if (na !== nb) return na < nb ? -1 : 1;
    } else {
      return sa < sb ? -1 : 1;
    }
  }
  return 0;
}

/**
 * Whether a device is running something other than the released version for its
 * platform-arch. Matches the server: the sync advertise and the update endpoint
 * trigger whenever the manifest has an entry for the platform and its version
 * differs from the device's reported agent_version (any difference, not only a
 * downgrade). False when self-update is off or the platform has no release.
 */
export function isOutdated(d: VersionedDevice, m: AgentManifest | null | undefined): boolean {
  if (!selfUpdateEnabled(m)) return false;
  const entry = manifestForPlatform(m, platformArch(d));
  if (!entry) return false;
  return (m as AgentManifest).version !== d.agent_version;
}

/** The version an update would move the device to, or null when none applies. */
export function targetVersion(d: VersionedDevice, m: AgentManifest | null | undefined): string | null {
  return isOutdated(d, m) ? (m as AgentManifest).version : null;
}

/** Count the devices that are outdated against the manifest (for the fleet banner). */
export function outdatedCount(devices: VersionedDevice[], m: AgentManifest | null | undefined): number {
  if (!selfUpdateEnabled(m)) return 0;
  return devices.filter((d) => isOutdated(d, m)).length;
}
