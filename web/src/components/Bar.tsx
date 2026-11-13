import { pct } from '../lib/format.ts';

/** Horizontal usage bar; turns amber at 80% and red at 90%. */
export function Bar({ value }: { value: number | null | undefined }) {
  if (value === null || value === undefined) return <span class="muted">{pct(null)}</span>;
  const v = Math.max(0, Math.min(100, value));
  const level = v >= 90 ? ' bar-crit' : v >= 80 ? ' bar-warn' : '';
  return (
    <span class="barwrap" title={pct(v)}>
      <span class={`bar${level}`}><span class="bar-fill" style={`width:${v}%`} /></span>
      <span class="bar-label">{pct(v)}</span>
    </span>
  );
}
