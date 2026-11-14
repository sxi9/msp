import { useState } from 'preact/hooks';
import { api, errorText } from '../api.ts';
import { formatCode, formatRecoveryCode, isValidCode, isValidRecoveryCode } from '../lib/totp.ts';
import type { User } from '../types.ts';

export function Login({ onLogin }: { onLogin: (u: User) => void }) {
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  // Null = step 1 (credentials); set = step 2 (second factor) after totp_required.
  const [stage, setStage] = useState<'password' | 'totp'>('password');

  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError('');
    try {
      const r = await api.login(email.trim(), password);
      if (r.totp_required) {
        setPassword('');
        setStage('totp');
      } else if (r.user) {
        onLogin(r.user);
      } else {
        setError('Unexpected response from the server.');
      }
    } catch (err) {
      setError(errorText(err) === 'invalid_credentials' ? 'Wrong email or password.' : errorText(err));
    } finally {
      setBusy(false);
    }
  };

  if (stage === 'totp') {
    return (
      <SecondFactor
        onVerified={onLogin}
        onBack={() => { setStage('password'); setPassword(''); setError(''); }}
      />
    );
  }

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

function SecondFactor({ onVerified, onBack }: { onVerified: (u: User) => void; onBack: () => void }) {
  const [recovery, setRecovery] = useState(false);
  const [code, setCode] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const ready = recovery ? isValidRecoveryCode(code) : isValidCode(code);

  const onInput = (raw: string) => setCode(recovery ? formatRecoveryCode(raw) : formatCode(raw));
  const switchMode = () => { setRecovery((r) => !r); setCode(''); setError(''); };

  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError('');
    try {
      const r = await api.loginTotp(code.trim());
      onVerified(r.user);
    } catch (err) {
      const t = errorText(err);
      setError(
        t.startsWith('invalid_code') ? (recovery ? 'That recovery code is wrong or already used.' : 'That code is not correct.')
          : t.startsWith('rate_limited') ? 'Too many attempts; wait a minute and try again.'
          : t.startsWith('pending_expired') || t === 'http_401' ? 'Your sign-in timed out. Enter your password again.'
          : t,
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="center">
      <form class="card login" onSubmit={submit}>
        <h1>Two-factor</h1>
        {recovery ? (
          <label>Recovery code
            <input
              inputMode="text" autocomplete="one-time-code" autofocus placeholder="xxxxx-xxxxx"
              value={code} onInput={(e) => onInput(e.currentTarget.value)}
            />
          </label>
        ) : (
          <label>Authenticator code
            <input
              inputMode="numeric" autocomplete="one-time-code" autofocus placeholder="123456" maxLength={6}
              value={code} onInput={(e) => onInput(e.currentTarget.value)}
            />
          </label>
        )}
        {error && <p class="error">{error}</p>}
        <button type="submit" class="btn btn-primary" disabled={busy || !ready}>{busy ? 'Verifying…' : 'Verify'}</button>
        <div class="form-row" style="justify-content:space-between">
          <a href="#/login" onClick={(e) => { e.preventDefault(); switchMode(); }}>
            {recovery ? 'Use an authenticator code' : 'Use a recovery code'}
          </a>
          <a href="#/login" onClick={(e) => { e.preventDefault(); onBack(); }}>Back</a>
        </div>
      </form>
    </div>
  );
}
