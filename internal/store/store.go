// Package store is the only package that talks to the database. Every SQL
// statement of the MSP server lives here, behind typed methods, so that a
// Postgres implementation in phase 2 is a second store, not a rewrite.
//
// The backing store is one SQLite file (modernc.org/sqlite, pure Go) opened in
// WAL mode with a 5 s busy timeout. Timestamps are stored as RFC3339 UTC text,
// secrets as sha256 hex, booleans as INTEGER 0/1.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrNotFound is returned when a row does not exist (or, for scoped
	// lookups, does not belong to the given owner).
	ErrNotFound = errors.New("store: not found")
	// ErrInvalidToken is returned for an unknown, expired, revoked or
	// exhausted enrollment token and for an unknown or revoked device token.
	ErrInvalidToken = errors.New("store: invalid token")
	// ErrConflict is returned on a uniqueness violation (duplicate org name or
	// user email) and when a command result arrives for a command that is not
	// in status sent.
	ErrConflict = errors.New("store: conflict")
)

// Store wraps the database. Now is the clock used for every timestamp the
// store writes and for status computation; tests override it.
type Store struct {
	db  *sql.DB
	Now func() time.Time
}

// querier is satisfied by *sql.DB and *sql.Tx so that row helpers work inside
// and outside transactions.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Open opens (creating if needed) the SQLite database at path, applies the
// connection pragmas, runs pending migrations and returns the Store.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)" +
		// BEGIN IMMEDIATE for every transaction: a deferred transaction that
		// reads first and writes later fails with SQLITE_BUSY_SNAPSHOT under
		// concurrent writers (the busy handler does not cover that case);
		// taking the write lock up front lets busy_timeout do its job.
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db, Now: func() time.Time { return time.Now().UTC() }}
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping runs SELECT 1; the /healthz endpoint uses it.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// migrate applies every embedded migrations/NNNN_name.sql file whose version
// is not yet recorded in schema_migrations, in lexical order, each inside its
// own transaction.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		var applied int
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&applied)
		if err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
		if applied > 0 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
		if err := s.applyMigration(ctx, version, string(body)); err != nil {
			return fmt.Errorf("store: migrate %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, version int, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		version, fmtTime(s.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

// migrationVersion parses the leading integer of "NNNN_name.sql".
func migrationVersion(name string) (int, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, fmt.Errorf("store: migration %q: name must be NNNN_name.sql", name)
	}
	v, err := strconv.Atoi(name[:i])
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("store: migration %q: bad version prefix", name)
	}
	return v, nil
}

// HashToken returns the sha256 hex digest of a plaintext token; only digests
// are stored.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// NewToken returns prefix + 43 chars of base64url (32 random bytes).
func NewToken(prefix string) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the OS entropy source is broken; there is
		// no safe fallback for a credential.
		panic("store: crypto/rand: " + err.Error())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// StatusOf computes a device's status: "pending" when it has never synced,
// "online" when now - lastSeen <= 3*intervalS + 15 s, otherwise "offline".
func StatusOf(lastSeen *time.Time, intervalS int, now time.Time) string {
	if lastSeen == nil {
		return "pending"
	}
	window := time.Duration(3*intervalS)*time.Second + 15*time.Second
	if now.Sub(*lastSeen) <= window {
		return "online"
	}
	return "offline"
}

// fmtTime renders a timestamp the way every column stores it.
func fmtTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// storedTime is the value a timestamp has after a round trip through the
// database (UTC, second precision), so freshly created rows match re-read ones.
func storedTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Second)
}

// parseTime reads a stored timestamp.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: bad timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// nullTime converts a nullable TEXT column into *time.Time.
func nullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// timeArg renders an optional timestamp for an INSERT/UPDATE argument.
func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func nullFloat(nf sql.NullFloat64) *float64 {
	if !nf.Valid {
		return nil
	}
	v := nf.Float64
	return &v
}

func nullInt(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}

func nullInt64(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	v := ni.Int64
	return &v
}

func nullBool(ni sql.NullInt64) *bool {
	if !ni.Valid {
		return nil
	}
	v := ni.Int64 != 0
	return &v
}

func nullString(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ptrFloatArg(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrIntArg(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrInt64Arg(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrBoolArg(p *bool) any {
	if p == nil {
		return nil
	}
	return boolInt(*p)
}

// isUniqueViolation reports whether err is a UNIQUE / PRIMARY KEY constraint
// failure from SQLite.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case 2067, 1555: // SQLITE_CONSTRAINT_UNIQUE, SQLITE_CONSTRAINT_PRIMARYKEY
			return true
		}
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// rowsAffected is a small helper for UPDATE/DELETE statements that must
// report whether they hit a row.
func rowsAffected(res sql.Result) (int64, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}
