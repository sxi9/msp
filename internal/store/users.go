package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const userColumns = `id, email, password_hash, role, created_at, disabled_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var (
		u          User
		created    string
		disabledAt sql.NullString
	)
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &created, &disabledAt); err != nil {
		return User{}, err
	}
	var err error
	if u.CreatedAt, err = parseTime(created); err != nil {
		return User{}, err
	}
	if u.DisabledAt, err = nullTime(disabledAt); err != nil {
		return User{}, err
	}
	return u, nil
}

// CreateUser inserts a staff account. The email is lowercased and trimmed;
// a duplicate yields ErrConflict. passwordHash is the caller's bcrypt hash.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash, role string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	now := s.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, role, created_at) VALUES (?, ?, ?, ?)`,
		email, passwordHash, role, fmtTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("store: create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("store: create user: %w", err)
	}
	return User{ID: id, Email: email, Role: role, CreatedAt: storedTime(now), PasswordHash: passwordHash}, nil
}

// GetUserByEmail looks a user up by (lowercased) email.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, email))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: get user: %w", err)
	}
	return u, nil
}

// GetUser looks a user up by id.
func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: get user: %w", err)
	}
	return u, nil
}

// ListUsers returns every account ordered by email.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY email, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list users: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	return users, nil
}

// DisableUser sets disabled_at (idempotent) and deletes the user's sessions.
func (s *Store) DisableUser(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: disable user: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET disabled_at = COALESCE(disabled_at, ?) WHERE id = ?`, fmtTime(s.Now()), id)
	if err != nil {
		return fmt.Errorf("store: disable user: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: disable user: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("store: disable user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: disable user: %w", err)
	}
	return nil
}

// CountUsers returns the number of accounts (the CLI uses it for bootstrap).
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}
