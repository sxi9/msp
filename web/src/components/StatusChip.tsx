import type { DeliveryStatus } from '../types.ts';

/**
 * The coloured delivery-status chip shared by the delivery log and the channel
 * "Send test" result: sent is green, failed red, suppressed amber, pending grey.
 */
export function DeliveryStatusChip({ status }: { status: DeliveryStatus }) {
  return <span class={`badge nst-${status}`}>{status}</span>;
}
