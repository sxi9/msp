import { useState } from 'preact/hooks';
import type { ComponentChildren } from 'preact';

/**
 * Button with an in-page confirm step (no window.confirm): the first click
 * swaps the button for "<question> [Yes] [Cancel]".
 */
export function ConfirmButton({ label, question, onConfirm, danger = false, disabled = false, children }: {
  label: string; question: string; onConfirm: () => void | Promise<void>; danger?: boolean; disabled?: boolean; children?: ComponentChildren;
}) {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  if (!asking) {
    return <button type="button" class={`btn btn-sm${danger ? ' btn-danger' : ''}`} disabled={disabled} onClick={() => setAsking(true)}>{label}</button>;
  }
  const yes = async () => {
    setBusy(true);
    try { await onConfirm(); } finally { setBusy(false); setAsking(false); }
  };
  return (
    <span class="confirm">
      <span>{question}</span>
      {children}
      <button type="button" class={`btn btn-sm${danger ? ' btn-danger' : ' btn-primary'}`} disabled={busy} onClick={yes}>Yes</button>
      <button type="button" class="btn btn-sm" disabled={busy} onClick={() => setAsking(false)}>Cancel</button>
    </span>
  );
}
