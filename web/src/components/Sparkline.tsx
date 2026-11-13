/** Inline SVG polyline of up to 120 samples; nulls break the line. */
export function Sparkline({ label, values, max = 100, last }: { label: string; values: (number | null)[]; max?: number; last: string }) {
  const w = 240, h = 40, n = values.length;
  const x = (i: number) => (n <= 1 ? 0 : (i / (n - 1)) * w);
  const y = (v: number) => h - 2 - (Math.max(0, Math.min(max, v)) / max) * (h - 4);
  let d = '';
  let pen = false;
  values.forEach((v, i) => {
    if (v === null || v === undefined) { pen = false; return; }
    d += `${pen ? 'L' : 'M'}${x(i).toFixed(1)},${y(v).toFixed(1)} `;
    pen = true;
  });
  return (
    <div class="spark">
      <div class="spark-head"><span>{label}</span><strong>{last}</strong></div>
      <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" role="img" aria-label={`${label} history`}>
        <line x1="0" y1={y(max * 0.5)} x2={w} y2={y(max * 0.5)} class="spark-grid" />
        {d ? <path d={d.trim()} class="spark-line" /> : <text x={w / 2} y={h / 2 + 4} class="spark-empty">no samples yet</text>}
      </svg>
    </div>
  );
}
