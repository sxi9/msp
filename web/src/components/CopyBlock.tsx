import { useState } from 'preact/hooks';

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { ok = false; }
    ta.remove();
    return ok;
  }
}

export function CopyButton({ text }: { text: string }) {
  const [state, setState] = useState('Copy');
  const onClick = async () => {
    setState((await copyText(text)) ? 'Copied' : 'Copy failed');
    setTimeout(() => setState('Copy'), 1500);
  };
  return <button type="button" class="btn btn-sm" onClick={onClick}>{state}</button>;
}

/** Monospace block with a Copy button; `highlight` marks a secret shown once. */
export function CopyBlock({ text, label, highlight = false }: { text: string; label?: string; highlight?: boolean }) {
  return (
    <div class={`copyblock${highlight ? ' highlight' : ''}`}>
      {label && <div class="copyblock-label">{label}</div>}
      <pre class="out">{text}</pre>
      <CopyButton text={text} />
    </div>
  );
}
