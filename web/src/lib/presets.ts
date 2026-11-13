// Command preset library helpers: pure functions over the GET /api/v1/presets
// body plus the once-per-app-session cache the device console reads from.
import type { Preset, PresetLibrary } from '../types.ts';

export const EMPTY_PRESETS: PresetLibrary = { presets: { linux: [], windows: [] }, source: 'builtin', path: '' };

const isPreset = (p: unknown): p is Preset =>
  !!p && typeof p === 'object' && typeof (p as Preset).label === 'string' && ((p as Preset).kind === 'exec' || (p as Preset).kind === 'script');

/**
 * Normalises whatever the server (or a stale cache) returned into a full
 * library: missing or null platform arrays become [], entries that are not
 * presets are dropped, timeout_s outside 1..3600 falls back to 60.
 */
export function mergePresets(lib: Partial<PresetLibrary> | null | undefined): PresetLibrary {
  const list = (v: unknown): Preset[] =>
    (Array.isArray(v) ? v : []).filter(isPreset).map((p) => ({
      ...p,
      timeout_s: Number.isInteger(p.timeout_s) && p.timeout_s >= 1 && p.timeout_s <= 3600 ? p.timeout_s : 60,
    }));
  const src = lib?.presets as Partial<PresetLibrary['presets']> | undefined;
  return {
    presets: { linux: list(src?.linux), windows: list(src?.windows) },
    source: lib?.source === 'file' ? 'file' : 'builtin',
    path: typeof lib?.path === 'string' ? lib.path : '',
  };
}

/** The platform's list in server order; android and unknown platforms have none. */
export function presetsFor(lib: PresetLibrary | null | undefined, platform: string): Preset[] {
  if (!lib) return [];
  if (platform === 'linux') return lib.presets.linux;
  if (platform === 'windows') return lib.presets.windows;
  return [];
}

/** What choosing a preset puts into the console fields. */
export interface PresetFill { mode: 'exec' | 'script'; text: string; shell: string; timeoutText: string }

/** Console field values for a preset: argv one per line for exec, the body (and shell) for script, plus the timeout. */
export function presetFill(p: Preset): PresetFill {
  if (p.kind === 'exec') return { mode: 'exec', text: (p.argv ?? []).join('\n'), shell: '', timeoutText: String(p.timeout_s) };
  return { mode: 'script', text: p.body ?? '', shell: p.shell ?? '', timeoutText: String(p.timeout_s) };
}

let cached: Promise<PresetLibrary> | null = null;

/**
 * Fetches the library once per app session through `fetcher` and shares the
 * result; a failed fetch resolves to the empty library and is not cached, so
 * the next console mount tries again.
 */
export function loadPresets(fetcher: () => Promise<PresetLibrary>): Promise<PresetLibrary> {
  if (!cached) {
    cached = fetcher().then(mergePresets, () => { cached = null; return EMPTY_PRESETS; });
  }
  return cached;
}

/** Drops the cache (tests, and logout if ever needed). */
export function resetPresets(): void {
  cached = null;
}
