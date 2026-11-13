import { useCallback, useEffect, useState } from 'preact/hooks';
import { errorText } from './api.ts';

export interface Loaded<T> { data: T | null; error: string | null; reload: () => void }

/**
 * Loads `load()` once per change of `deps`, and again every `everyMs` while the
 * tab is visible (0 = no polling). `reload()` forces a refresh.
 */
export function usePoll<T>(load: () => Promise<T>, deps: unknown[], everyMs = 0): Loaded<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);
  const reload = useCallback(() => setTick((t) => t + 1), []);
  useEffect(() => {
    let alive = true;
    const run = (first: boolean) => {
      if (!first && document.visibilityState !== 'visible') return;
      load().then(
        (d) => { if (alive) { setData(d); setError(null); } },
        (e) => { if (alive) setError(errorText(e)); },
      );
    };
    run(true);
    const id = everyMs > 0 ? setInterval(() => run(false), everyMs) : 0;
    return () => { alive = false; if (id) clearInterval(id); };
  }, [...deps, tick, everyMs]);
  return { data, error, reload };
}
