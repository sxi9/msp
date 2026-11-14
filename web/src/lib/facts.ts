// The device-detail "Facts" panel (web/src/views/Device.tsx): which facts keys
// become rows. Two facts keys are inventory arrays with their own dedicated
// panels and must never render as a Facts row — `apps` (the Android installed-
// apps inventory -> the "Installed apps" panel) and `software` (the desktop
// installed-software inventory, SI1 -> the "Installed software" panel). The
// server stores the whole facts object, these arrays included, in
// devices.facts_json and returns it as the device detail's `facts`, so a real
// desktop device carries a `software` array here; without excluding it the panel
// renders an unreadable "[object Object], [object Object], …" row (one per
// package). Pure; tested by facts.test.ts.

/** Facts keys that have their own panel and are never shown as a Facts row. */
export const INVENTORY_FACT_KEYS: readonly string[] = ['apps', 'software'];

/** The facts entries to render as rows: every key except the inventory arrays. */
export function factRows(facts: Record<string, unknown>): [string, unknown][] {
  return Object.entries(facts).filter(([k]) => !INVENTORY_FACT_KEYS.includes(k));
}
