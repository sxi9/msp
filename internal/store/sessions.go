package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SessionTokenPrefix starts every dashboard session cookie value.
const SessionTokenPrefix = "ss_"

// CreateSession creates a login session and returns the plaintext cookie
// value ("ss_" + random); only its sha256 is stored.
func (s *Store) CreateSession(ctx context.Context, userID int64, ip string, ttl time.Duration) (string, error) {
	plain := NewToken(SessionTokenPrefix)
	now := s.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, ip) VALUES (?, ?, ?, ?, ?)`,
		HashToken(plain), userID, fmtTime(now), fmtTime(now.Add(ttl)), ip)
	if err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	return plain, nil
}

// GetSession resolves a plaintext session value to its user. ErrNotFound when
// the session is unknown or expired, or the user is disabled.
func (s *Store) GetSession(ctx context.Context, plain string) (User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.email, u.password_hash, u.role, u.created_at, u.disabled_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = ? AND s.expires_at > ?`,
		HashToken(plain), fmtTime(s.Now()))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: get session: %w", err)
	}
	if u.DisabledAt != nil {
		return User{}, ErrNotFound
	}
	return u, nil
}

// DeleteSession removes a session; deleting an unknown session is not an error.
func (s *Store) DeleteSession(ctx context.Context, plain string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(plain)); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// PruneSessions deletes expired sessions and returns how many were removed.
func (s *Store) PruneSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, fmtTime(s.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: prune sessions: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return 0, fmt.Errorf("store: prune sessions: %w", err)
	}
	return n, nil
}
