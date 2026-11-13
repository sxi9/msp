import type { DeviceStatus } from '../types.ts';

/** Coloured dot: green online, red offline, grey pending, amber when alerting. */
export function StatusDot({ status, alerting = false }: { status: DeviceStatus; alerting?: boolean }) {
  const cls = alerting ? 'alert' : status;
  return <span class={`dot dot-${cls}`} title={alerting ? `${status}, alerting` : status} />;
}
