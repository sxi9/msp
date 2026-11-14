// Pure helpers for software deployment (SD1): human-readable file sizes, a short
// hash for the library table, package-name and destination-path validation (the
// client-side mirror of protocol.ValidPackageName / ValidDeployDest), and the
// library filter for one device's org. Pure; tested by deploy.test.ts.

const SIZE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB'];

/** Bytes as "512 B", "4.0 KB", "1.2 MB", "3.4 GB"; an em dash for a bad value. */
export function fileSize(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1024) return `${bytes} B`;
  let n = bytes;
  let i = 0;
  while (n >= 1024 && i < SIZE_UNITS.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(1)} ${SIZE_UNITS[i]}`;
}

/** The first n characters of a sha256 hex digest, for the library table. */
export function shortHash(hex: string, n = 12): string {
  return hex ? hex.slice(0, n) : '';
}

/**
 * A single OS package name the deploy "package" source accepts: 1..256 characters
 * of [A-Za-z0-9._+-]. It is passed to the package manager as one argv token, never
 * through a shell, so anything that could split or inject is refused.
 */
export function validPackageName(p: string): boolean {
  return /^[A-Za-z0-9._+-]{1,256}$/.test(p);
}

/**
 * An acceptable deploy destination path: non-empty, absolute (a leading "/", a
 * drive-letter path like C:\ or C:/, or a UNC \\server\share) and with no ".."
 * path component on either separator. Mirrors protocol.ValidDeployDest; the agent
 * additionally refuses to write through a symlink.
 */
export function validDest(dest: string): boolean {
  if (!dest) return false;
  const norm = dest.replace(/\\/g, '/');
  if (norm.split('/').some((seg) => seg === '..')) return false;
  if (dest.startsWith('/')) return true;
  if (dest.startsWith('\\\\')) return true; // UNC \\server\share
  if (/^[A-Za-z]:[/\\]/.test(dest)) return true; // drive letter C:\ or C:/
  return false;
}

/** A human message for an invalid dest (for the dialog preview), or null when valid. */
export function destError(dest: string): string | null {
  const d = dest.trim();
  if (d === '') return 'A destination path is required.';
  if (d.replace(/\\/g, '/').split('/').some((seg) => seg === '..')) return 'The path must not contain "..".';
  if (!validDest(d)) return 'The path must be absolute (/path, C:\\path, or \\\\server\\share).';
  return null;
}

/**
 * The library files usable for a device: those scoped to the device's own org
 * plus every global file (org_id null). The input is not mutated.
 */
export function filesForDevice<T extends { org_id: number | null }>(files: T[], orgId: number): T[] {
  return files.filter((f) => f.org_id === null || f.org_id === orgId);
}
