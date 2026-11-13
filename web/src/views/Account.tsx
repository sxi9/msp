import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { when } from '../lib/format.ts';
import type { User } from '../types.ts';

/** The session user's own settings: today only the password change. */
export function Account({ user }: { user: User }) {
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
    <section>
      <div class="toolbar"><h1>Account</h1></div>
      <dl class="kv header">
        <dt>Email</dt><dd>{user.email}</dd>
        <dt>Role</dt><dd>{user.role}</dd>
        <dt>Created</dt><dd>{when(user.created_at)}</dd>
      </dl>
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
    </section>
  );
}
