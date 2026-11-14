# TF1 — Two-factor authentication (phase 2)

TOTP (RFC 6238) second factor for staff logins, with one-time recovery codes and an admin reset for
lockouts. The highest-value hardening before any internet-facing deploy. TOTP is implemented natively
with the Go stdlib (crypto/hmac, crypto/sha1, encoding/base32) — no new Go dependency. Fully testable
in-container (TOTP is deterministic given secret + time; tests compute the expected code). No external
calls.

## Model

- `users` gains: `totp_secret TEXT NOT NULL DEFAULT ''` (base32, write-only, never returned),
  `totp_enabled INTEGER NOT NULL DEFAULT 0`, `totp_confirmed_at TEXT NULL`.
- New table `recovery_codes(id INTEGER PK, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE
  CASCADE, code_hash TEXT NOT NULL, used_at TEXT NULL, created_at TEXT NOT NULL)`. Codes are random
  (10 per user, format `xxxxx-xxxxx` base32), stored only as a sha256 hash, single-use.
- Migration `0009_two_factor.sql`: add the three user columns (defaults keep every existing user at
  2FA-off) and the `recovery_codes` table. Do not edit earlier migrations.
- Store: `SetTOTPSecret(userID, secret)`, `EnableTOTP(userID, now)`, `DisableTOTP(userID)` (clears the
  secret and deletes recovery codes), `ReplaceRecoveryCodes(userID, hashes)`,
  `ConsumeRecoveryCode(userID, hash) (bool, error)` (atomic mark-used), `TOTPStatus(userID)`. The user
  row read exposes `totp_enabled` (bool) and never the secret.

## TOTP implementation (`internal/server/totp.go`)

- `totpNow(secret, t) string` and `totpVerify(secret, code, t) bool` per RFC 6238: SHA-1, 6 digits,
  30 s step, accept the current step and ±1 (clock skew). Constant-time compare. `newTOTPSecret()`
  returns 20 random bytes base32-encoded. `otpauthURI(secret, issuer, account)` builds
  `otpauth://totp/MSP:<email>?secret=...&issuer=MSP&digits=6&period=30`. Unit-tested against the
  RFC 6238 test vectors and a round-trip (generate then verify, incl. skew and rejection of a stale
  code). Verifies are rate-limited (reuse `loginLimit` keyed by IP) and audited on failure.

## Enrollment and management (staff, session auth, CSRF)

- `POST /api/v1/me/totp/setup` → generates a secret (not yet enabled), stores it, returns
  `{secret, otpauth_uri}`. Re-running before confirm replaces the pending secret. 409 if already
  enabled (must disable first).
- `POST /api/v1/me/totp/confirm {code}` → verifies the code against the pending secret; on success sets
  `totp_enabled`, generates 10 recovery codes, returns them **once** as plaintext (`{recovery_codes}`);
  audit `user.totp_enabled`. Wrong code → 400 invalid_code.
- `POST /api/v1/me/totp/disable {code}` → a valid current code (or a recovery code) disables 2FA,
  clears the secret and recovery codes; audit `user.totp_disabled`.
- `POST /api/v1/me/totp/recovery/regenerate {code}` → with a valid current code, replaces the recovery
  codes and returns the new set once; audit `user.totp_recovery_regenerated`.
- `GET /api/v1/me` gains `totp_enabled` and `recovery_codes_remaining`.
- Admin reset for lockouts: `POST /api/v1/users/{id}/totp/reset` (admin only) disables the target
  user's 2FA so they can re-enrol; audit `user.totp_reset` with the actor. 404 if the user is not
  visible (RS2) — though admins are all-orgs, keep the check uniform.

## Login flow (two-step; keep the existing single-step for users without 2FA)

- `POST /api/v1/login {email,password}`: unchanged when the user has no 2FA (sets the session cookie,
  returns the user). When `totp_enabled`: do NOT set the full session; instead return `200 {"totp_
  required":true}` and set a short-lived (5 min) **pending** cookie bound to the user id and a nonce
  (its own signed/opaque token stored server-side or as a separate `pending_totp` session kind), never
  a full session. Burn the same bcrypt time as today on bad credentials; a bad password never reveals
  whether 2FA is on.
- `POST /api/v1/login/totp {code}` (reads the pending cookie): verifies a TOTP code or a recovery code;
  on success consumes the pending token, creates the real session, returns the user; audit `user.login`
  (and note second_factor). Wrong code → 401 invalid_code (rate-limited). Missing/expired pending
  cookie → 401. A recovery-code login that drops the remaining count to 0 still succeeds but the `me`
  response flags `recovery_codes_remaining:0`.
- Rate-limit both steps via `loginLimit`. Audit `user.login_totp_failed` on a bad second factor.

## Dashboard

- Account page (`web/src/views/Account.tsx` or the existing account view) gains a **Two-factor
  authentication** section: when off, a "Set up" flow (call setup, show the QR + the manual secret +
  the otpauth URI, enter a code to confirm, then show the 10 recovery codes once with a copy/download-
  as-text affordance and a "I've saved these" confirm); when on, show enabled state, remaining recovery
  codes, "Regenerate recovery codes" and "Turn off" (each asks for a current code).
- QR: render the otpauth URI as a QR in the browser using the small pinned pure-JS lib
  `qrcode-generator@1.4.4` (add to web deps) OR a vendored pure-TS encoder in `web/src/lib/qr.ts`;
  either way also show the secret as selectable text so an authenticator can be set up without the QR.
- Login view: after step 1 returns `totp_required`, show a 6-digit code field (and a "use a recovery
  code" link) that calls `/login/totp`.
- Users page (admin): each user row shows a 2FA on/off badge and, for users with it on, a "Reset 2FA"
  action (confirm in-page) calling the admin reset.
- Pure helpers (code formatting/validation, recovery-code display, otpauth parse) in
  `web/src/lib/totp.ts` with a `node --test`. Keep `?mock=1` working (a user with 2FA on, the login
  second step, and the setup flow usable against the mock).

## Docs

`docs/PROTOCOL.md` (the totp endpoints, the two-step login, the `me` fields), `docs/SECURITY.md` (TOTP
secret is write-only and never logged; recovery codes hashed single-use; constant-time verify;
rate-limited; admin reset is audited; the pending cookie is not a session), `docs/GUIDE.md` (a
"Two-factor authentication" subsection under Users/Account: set it up, recovery codes, what to do if
locked out → ask an admin to reset), `docs/ARCHITECTURE.md`, `docs/FEATURES.md`, `docs/STATUS.md`,
`docs/ROADMAP.md` (tick two-factor authentication; device-token rotation + API keys remain as separate
roadmap items unless done here — do only 2FA in TF1), `docs/CHECKLIST.md`.

## Proof (what the build must show here)

The six proof commands all pass. A new `internal/server` or `internal/e2e` test that:
- sets up TOTP for a user (setup → confirm with a computed code → recovery codes returned once);
- a login for that user returns `totp_required` and no usable session cookie until `/login/totp` with a
  correct computed code; a wrong code is 401 and rate-limited; a stale/replayed code is rejected;
- a recovery code logs in once and cannot be reused;
- `disable` with a valid code turns it off and a subsequent login is single-step again;
- an admin reset clears a locked-out user's 2FA (audited `user.totp_reset`);
- the TOTP primitive matches the RFC 6238 test vectors;
- a user without 2FA logs in exactly as before (every existing auth test still passes).

Verify stage screenshots the Account 2FA setup (QR + recovery codes) and the login second-factor
prompt.
