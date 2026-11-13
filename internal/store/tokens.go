package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

const enrollmentTokenColumns = `id, org_id, token_prefix, label, max_uses, uses, expires_at, created_by, created_at, revoked_at`

func scanEnrollmentToken(row interface{ Scan(...any) error }) (EnrollmentToken, error) {
	var (
		t         EnrollmentToken
		expires   string
		created   string
		createdBy sql.NullInt64
		revokedAt sql.NullString
	)
	if err := row.Scan(&t.ID, &t.OrgID, &t.TokenPrefix, &t.Label, &t.MaxUses, &t.Uses,
		&expires, &createdBy, &created, &revokedAt); err != nil {
		return EnrollmentToken{}, err
	}
	var err error
	if t.ExpiresAt, err = parseTime(expires); err != nil {
		return EnrollmentToken{}, err
	}
	if t.CreatedAt, err = parseTime(created); err != nil {
		return EnrollmentToken{}, err
	}
	t.CreatedBy = nullInt64(createdBy)
	if t.RevokedAt, err = nullTime(revokedAt); err != nil {
		return EnrollmentToken{}, err
	}
	return t, nil
}

// CreateEnrollmentToken mints an "et_" token for an org. The plaintext is
// returned once; the row stores its sha256 and the first 10 characters.
func (s *Store) CreateEnrollmentToken(ctx context.Context, orgID int64, label string, maxUses int, ttl time.Duration, createdBy *int64) (string, EnrollmentToken, error) {
	if maxUses < 1 {
		maxUses = 1
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	plain := NewToken(protocol.EnrollmentTokenPrefix)
	now := s.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO enrollment_tokens (org_id, token_hash, token_prefix, label, max_uses, uses, expires_at, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		orgID, HashToken(plain), plain[:10], label, maxUses, fmtTime(now.Add(ttl)), ptrInt64Arg(createdBy), fmtTime(now))
	if err != nil {
		return "", EnrollmentToken{}, fmt.Errorf("store: create enrollment token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", EnrollmentToken{}, fmt.Errorf("store: create enrollment token: %w", err)
	}
	tok, err := s.getEnrollmentToken(ctx, `id = ?`, id)
	if err != nil {
		return "", EnrollmentToken{}, err
	}
	return plain, tok, nil
}

func (s *Store) getEnrollmentToken(ctx context.Context, where string, args ...any) (EnrollmentToken, error) {
	tok, err := scanEnrollmentToken(s.db.QueryRowContext(ctx,
		`SELECT `+enrollmentTokenColumns+` FROM enrollment_tokens WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return EnrollmentToken{}, ErrNotFound
	}
	if err != nil {
		return EnrollmentToken{}, fmt.Errorf("store: get enrollment token: %w", err)
	}
	return tok, nil
}

// ListEnrollmentTokens returns an org's tokens, newest first.
func (s *Store) ListEnrollmentTokens(ctx context.Context, orgID int64) ([]EnrollmentToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+enrollmentTokenColumns+` FROM enrollment_tokens WHERE org_id = ? ORDER BY id DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list enrollment tokens: %w", err)
	}
	defer rows.Close()
	toks := []EnrollmentToken{}
	for rows.Next() {
		t, err := scanEnrollmentToken(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list enrollment tokens: %w", err)
		}
		toks = append(toks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list enrollment tokens: %w", err)
	}
	return toks, nil
}

// ConsumeEnrollmentToken atomically spends one use of a valid token and
// returns the updated row. It is a single UPDATE guarded by revoked_at,
// expires_at and uses < max_uses, so concurrent enrollments can never push
// uses past max_uses. ErrInvalidToken when no row matched.
func (s *Store) ConsumeEnrollmentToken(ctx context.Context, plain string) (EnrollmentToken, error) {
	hash := HashToken(plain)
	res, err := s.db.ExecContext(ctx,
		`UPDATE enrollment_tokens SET uses = uses + 1
		  WHERE token_hash = ? AND revoked_at IS NULL AND expires_at > ? AND uses < max_uses`,
		hash, fmtTime(s.Now()))
	if err != nil {
		return EnrollmentToken{}, fmt.Errorf("store: consume enrollment token: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return EnrollmentToken{}, fmt.Errorf("store: consume enrollment token: %w", err)
	}
	if n == 0 {
		return EnrollmentToken{}, ErrInvalidToken
	}
	return s.getEnrollmentToken(ctx, `token_hash = ?`, hash)
}

// RevokeEnrollmentToken marks a token revoked (idempotent). ErrNotFound when
// the token does not exist in that org.
func (s *Store) RevokeEnrollmentToken(ctx context.Context, orgID, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE enrollment_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND org_id = ?`,
		fmtTime(s.Now()), id, orgID)
	if err != nil {
		return fmt.Errorf("store: revoke enrollment token: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: revoke enrollment token: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
