import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { usePoll } from '../hooks.ts';
import { when } from '../lib/format.ts';
import { formatCode, groupSecret, isValidCode, parseOtpauth, RECOVERY_FILENAME, recoveryCodesText } from '../lib/totp.ts';
import { qrSvg } from '../lib/qr.ts';
import { CopyButton } from '../components/CopyBlock.tsx';
import type { TotpSetup, User } from '../types.ts';

/** The session user's own settings: password change and two-factor (TF1). */
export function Account({ user }: { user: User }) {
  return (
    <section>
      <div class="toolbar"><h1>Account</h1></div>
      <dl class="kv header">
        <dt>Email</dt><dd>{user.email}</dd>
        <dt>Role</dt><dd>{user.role}</dd>
        <dt>Created</dt><dd>{when(user.created_at)}</dd>
      </dl>
      <ChangePassword user={user} />
      <TwoFactor />
    </section>
  );
}

function ChangePassword({ user }: { user: User }) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [again, setAgain] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [done, setDone] = useState(false);
  const mismatch = again !== '' && next !== again;
  const submit = async (e: Event) => {
    e.preventDefault();
    setError(''); setDone(false);
    if (next !== again) { setError('The new password was not repeated correctly.'); return; }
    setBusy(true);
    try {
      await api.changePassword(current, next);
      setCurrent(''); setNext(''); setAgain('');
      setDone(true);
    } catch (err) {
      const t = errorText(err);
      setError(t.startsWith('rate_limited') ? 'Too many attempts; wait a minute and try again.' : t.replace(/^invalid_request: /, ''));
    } finally { setBusy(false); }
  };
  return (
    <>
      <h2>Change password</h2>
      <form class="card login" onSubmit={submit}>
        <label>Current password<input type="password" required autocomplete="current-password" value={current} onInput={(e) => setCurrent(e.currentTarget.value)} /></label>
        <label>New password (10+ characters)<input type="password" required minLength={10} autocomplete="new-password" value={next} onInput={(e) => setNext(e.currentTarget.value)} /></label>
        <label>Repeat new password<input type="password" required minLength={10} autocomplete="new-password" value={again} onInput={(e) => setAgain(e.currentTarget.value)} /></label>
        {mismatch && <span class="warn">The passwords do not match.</span>}
        <button type="submit" class="btn btn-primary" disabled={busy || !current || next.length < 10 || mismatch}>Change password</button>
        {error && <span class="error">{error}</span>}
        {done && <span class="ok">Password changed. Your other sessions were signed out.</span>}
      </form>
      <p class="muted">Forgot it? An administrator resets it from the server with <code>msp-server user set-password --email {user.email} --password-stdin</code>.</p>
    </>
  );
}

// The two-factor section drives a small state machine:
//   idle      — show on/off state and the management actions
//   setup     — a pending secret exists; show QR + secret, confirm with a code
//   codes     — recovery codes returned once; show them, "I've saved these"
type Mode = 'idle' | 'setup' | 'codes';

function TwoFactor() {
  // /me carries totp_enabled + recovery_codes_remaining (TF1).
  const me = usePoll(() => api.me().then((r) => r.user), [], 0);
  const [mode, setMode] = useState<Mode>('idle');
  const [pending, setPending] = useState<TotpSetup | null>(null);
  const [codes, setCodes] = useState<string[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  if (me.error) return (<><h2>Two-factor authentication</h2><p class="error">{me.error}</p></>);
  if (!me.data) return (<><h2>Two-factor authentication</h2><p class="muted">Loading…</p></>);
  const enabled = !!me.data.totp_enabled;
  const remaining = me.data.recovery_codes_remaining ?? 0;

  const beginSetup = async () => {
    setBusy(true); setError('');
    try {
      setPending(await api.totpSetup());
      setMode('setup');
    } catch (err) {
      setError(errorText(err).startsWith('conflict') ? 'Two-factor is already on.' : errorText(err));
    } finally { setBusy(false); }
  };
  const confirm = async (code: string) => {
    const r = await api.totpConfirm(code);
    setCodes(r.recovery_codes);
    setPending(null);
    setMode('codes');
    me.reload();
  };
  const regenerate = async (code: string) => {
    const r = await api.totpRegenerate(code);
    setCodes(r.recovery_codes);
    setMode('codes');
    me.reload();
  };
  const disable = async (code: string) => {
    await api.totpDisable(code);
    setMode('idle');
    me.reload();
  };
  const doneWithCodes = () => { setCodes([]); setMode('idle'); };

  return (
    <div class="twofa">
      <h2>Two-factor authentication</h2>

      {mode === 'codes' && (
        <RecoveryCodesPanel codes={codes} onDone={doneWithCodes} />
      )}

      {mode === 'setup' && pending && (
        <SetupPanel setup={pending} onConfirm={confirm} onCancel={() => { setPending(null); setMode('idle'); setError(''); }} />
      )}

      {mode === 'idle' && !enabled && (
        <div class="card">
          <p>Add a time-based one-time code from an authenticator app (Google Authenticator, 1Password, Authy…) as a second step at sign-in. You will also get ten single-use recovery codes.</p>
          <button type="button" class="btn btn-primary" disabled={busy} onClick={beginSetup}>Set up two-factor</button>
          {error && <span class="error">{error}</span>}
        </div>
      )}

      {mode === 'idle' && enabled && (
        <div class="card twofa-on">
          <p><span class="badge st-done">On</span> Two-factor is on for this account.</p>
          <p class={remaining <= 2 ? 'warn' : 'muted'}>
            {remaining} of your recovery codes {remaining === 1 ? 'is' : 'are'} unused.
            {remaining === 0 && ' Regenerate a fresh set so you are not locked out.'}
          </p>
          <div class="form-row">
            <CodePrompt label="Regenerate recovery codes" verb="Regenerate" allowRecovery={false} onSubmit={regenerate} />
            <CodePrompt label="Turn off" verb="Turn off" danger allowRecovery onSubmit={disable} />
          </div>
          <p class="muted">Lost your authenticator and your recovery codes? Ask an administrator to reset two-factor for your account, then set it up again.</p>
        </div>
      )}
    </div>
  );
}

function SetupPanel({ setup, onConfirm, onCancel }: {
  setup: TotpSetup; onConfirm: (code: string) => Promise<void>; onCancel: () => void;
}) {
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const parsed = parseOtpauth(setup.otpauth_uri);
  const secret = parsed?.secret ?? setup.secret;
  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true); setError('');
    try {
      await onConfirm(code);
    } catch (err) {
      const t = errorText(err);
      setError(t.startsWith('invalid_code') ? 'That code is not correct — check your app and try again.'
        : t.startsWith('rate_limited') ? 'Too many attempts; wait a minute and try again.' : t);
    } finally { setBusy(false); }
  };
  return (
    <div class="card twofa-setup">
      <ol class="twofa-steps">
        <li>Scan this QR code with your authenticator app, or enter the secret by hand.</li>
      </ol>
      <div class="twofa-qrrow">
        <div class="twofa-qr" dangerouslySetInnerHTML={{ __html: qrSvg(setup.otpauth_uri) }} />
        <div class="twofa-secret">
          <div class="copyblock-label">Secret (if you cannot scan)</div>
          <div class="form-row">
            <code class="twofa-secret-text">{groupSecret(secret)}</code>
            <CopyButton text={secret} />
          </div>
          {parsed && <p class="muted">Account {parsed.account} · issuer {parsed.issuer || 'MSP'} · 6 digits · 30s</p>}
        </div>
      </div>
      <form class="form-row" onSubmit={submit}>
        <label style="flex-direction:column;align-items:stretch">Enter the 6-digit code to confirm
          <input inputMode="numeric" autocomplete="one-time-code" placeholder="123456" maxLength={6}
            value={code} onInput={(e) => setCode(formatCode(e.currentTarget.value))} />
        </label>
        <button type="submit" class="btn btn-primary" disabled={busy || !isValidCode(code)}>Confirm</button>
        <button type="button" class="btn" disabled={busy} onClick={onCancel}>Cancel</button>
        {error && <span class="error">{error}</span>}
      </form>
    </div>
  );
}

function RecoveryCodesPanel({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const [saved, setSaved] = useState(false);
  const text = recoveryCodesText(codes);
  const download = () => {
    try {
      const blob = new Blob([text], { type: 'text/plain' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url; a.download = RECOVERY_FILENAME;
      document.body.appendChild(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 0);
    } catch { /* clipboard/copy remains available */ }
    setSaved(true);
  };
  return (
    <div class="card twofa-codes highlight">
      <p><strong>Save your recovery codes.</strong> Each works once if you lose your authenticator. They are shown only now and will not be shown again.</p>
      <ul class="twofa-codelist">
        {codes.map((c) => <li key={c}><code>{c}</code></li>)}
      </ul>
      <div class="form-row">
        <CopyButton text={text} />
        <button type="button" class="btn" onClick={download}>Download .txt</button>
        <button type="button" class="btn btn-primary" disabled={!saved} onClick={onDone}>I've saved these</button>
        {!saved && <span class="muted">Copy or download them first.</span>}
      </div>
    </div>
  );
}

/**
 * An inline "ask for a current code" action: the button swaps for a code field
 * and a confirm. `allowRecovery` widens the accepted input to a recovery code too
 * (so a user without their authenticator can still turn 2FA off).
 */
function CodePrompt({ label, verb, onSubmit, danger = false, allowRecovery = false }: {
  label: string; verb: string; onSubmit: (code: string) => Promise<void>; danger?: boolean; allowRecovery?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  // Accept a 6-digit code always; when allowRecovery, also accept the raw typed
  // recovery code (validated server-side) so we keep the field permissive.
  const ready = isValidCode(code) || (allowRecovery && code.trim().length >= 6);
  const close = () => { setOpen(false); setCode(''); setError(''); };
  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true); setError('');
    try {
      await onSubmit(code.trim());
      close();
    } catch (err) {
      const t = errorText(err);
      setError(t.startsWith('invalid_code') ? 'That code is not correct.'
        : t.startsWith('rate_limited') ? 'Too many attempts; wait a minute.' : t);
    } finally { setBusy(false); }
  };
  if (!open) {
    return <button type="button" class={`btn btn-sm${danger ? ' btn-danger' : ''}`} onClick={() => setOpen(true)}>{label}</button>;
  }
  return (
    <form class="twofa-prompt" onSubmit={submit}>
      <input inputMode={allowRecovery ? 'text' : 'numeric'} autocomplete="one-time-code" autofocus
        placeholder={allowRecovery ? 'code or recovery code' : '123456'} maxLength={allowRecovery ? 11 : 6}
        value={code} onInput={(e) => setCode(allowRecovery ? e.currentTarget.value : formatCode(e.currentTarget.value))} />
      <button type="submit" class={`btn btn-sm${danger ? ' btn-danger' : ' btn-primary'}`} disabled={busy || !ready}>{verb}</button>
      <button type="button" class="btn btn-sm" disabled={busy} onClick={close}>Cancel</button>
      {error && <span class="error">{error}</span>}
    </form>
  );
}
