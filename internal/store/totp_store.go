package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetTOTPSecret stores a (not-yet-enabled) base32 TOTP secret for the user,
// replacing any pending one. The secret is write-only: it is never returned by
// a user read and never logged. ErrNotFound when the user does not exist.
func (s *Store) SetTOTPSecret(ctx context.Context, userID int64, secret string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET totp_secret = ? WHERE id = ?`, secret, userID)
	if err != nil {
		return fmt.Errorf("store: set totp secret: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: set totp secret: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// EnableTOTP marks the user's 2FA enabled and records the confirmation time.
// ErrNotFound when the user does not exist.
func (s *Store) EnableTOTP(ctx context.Context, userID int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET totp_enabled = 1, totp_confirmed_at = ? WHERE id = ?`, fmtTime(now), userID)
	if err != nil {
		return fmt.Errorf("store: enable totp: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: enable totp: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DisableTOTP turns the user's 2FA off: it clears the secret, unsets the
// enabled flag and the confirmation time, and deletes every recovery code, all
// in one transaction. It is the store side of both the self-service disable and
// the admin reset, so it is idempotent for a user who already has 2FA off.
// ErrNotFound when the user does not exist.
func (s *Store) DisableTOTP(ctx context.Context, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: disable totp: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET totp_secret = '', totp_enabled = 0, totp_confirmed_at = NULL, totp_last_step = 0 WHERE id = ?`, userID)
	if err != nil {
		return fmt.Errorf("store: disable totp: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: disable totp: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: disable totp: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: disable totp: %w", err)
	}
	return nil
}

// ReplaceRecoveryCodes deletes the user's existing recovery codes and inserts
// the given sha256 hashes (10 at a time, in use), in one transaction. The
// caller has already verified the user exists (a bad user_id fails the FK).
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID int64, hashes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace recovery codes: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: replace recovery codes: %w", err)
	}
	now := fmtTime(s.Now())
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO recovery_codes (user_id, code_hash, created_at) VALUES (?, ?, ?)`,
			userID, h, now); err != nil {
			return fmt.Errorf("store: replace recovery codes: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace recovery codes: %w", err)
	}
	return nil
}

// ConsumeRecoveryCode atomically marks one unused recovery code matching hash
// as used and reports whether one was consumed. The single guarded UPDATE makes
// it single-use even under concurrent attempts (SQLite serialises writers).
func (s *Store) ConsumeRecoveryCode(ctx context.Context, userID int64, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
		fmtTime(s.Now()), userID, hash)
	if err != nil {
		return false, fmt.Errorf("store: consume recovery code: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return false, fmt.Errorf("store: consume recovery code: %w", err)
	}
	return n > 0, nil
}

// ConsumeTOTPStep atomically records step as the user's highest consumed TOTP
// time step and reports whether it was accepted. It succeeds only when step is
// strictly greater than the stored totp_last_step, so a code from a step already
// used at login — including the same code replayed within its +/-1 skew window —
// is rejected, making an accepted TOTP code one-time per RFC 6238 Section 5.2.
// The single guarded UPDATE is atomic even under concurrent logins (SQLite
// serialises writers). A false with a nil error means the step was already
// consumed; ErrNotFound means the user does not exist.
func (s *Store) ConsumeTOTPStep(ctx context.Context, userID int64, step uint64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`,
		int64(step), userID, int64(step))
	if err != nil {
		return false, fmt.Errorf("store: consume totp step: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return false, fmt.Errorf("store: consume totp step: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	// No row changed: either the step was already consumed (a replay) or the user
	// is gone. Distinguish the two so a replay reads as a clean false, not a
	// not-found error, and a vanished user is reported as such.
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("store: consume totp step: %w", err)
	}
	return false, nil
}

// TOTPStatus returns the user's TOTP secret (for verification), whether 2FA is
// enabled, and the number of unused recovery codes remaining. The secret stays
// inside the server: handlers use it to verify codes and never echo it.
// ErrNotFound when the user does not exist.
func (s *Store) TOTPStatus(ctx context.Context, userID int64) (secret string, enabled bool, remaining int, err error) {
	var (
		sec string
		en  int
		rem int
	)
	row := s.db.QueryRowContext(ctx,
		`SELECT totp_secret, totp_enabled,
		        (SELECT COUNT(*) FROM recovery_codes WHERE user_id = users.id AND used_at IS NULL)
		   FROM users WHERE id = ?`, userID)
	if err := row.Scan(&sec, &en, &rem); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, 0, ErrNotFound
		}
		return "", false, 0, fmt.Errorf("store: totp status: %w", err)
	}
	return sec, en != 0, rem, nil
}
