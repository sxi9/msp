import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import type { User } from '../types.ts';

export function Login({ onLogin }: { onLogin: (u: User) => void }) {
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError('');
    try {
      onLogin((await api.login(email.trim(), password)).user);
    } catch (err) {
      setError(errorText(err) === 'invalid_credentials' ? 'Wrong email or password.' : errorText(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div class="center">
      <form class="card login" onSubmit={submit}>
        <h1>MSP console</h1>
        <label>Email<input type="email" autocomplete="username" required value={email} onInput={(e) => setEmail(e.currentTarget.value)} /></label>
        <label>Password<input type="password" autocomplete="current-password" required value={password} onInput={(e) => setPassword(e.currentTarget.value)} /></label>
        {error && <p class="error">{error}</p>}
        <button type="submit" class="btn btn-primary" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
      </form>
    </div>
  );
}
