package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// openStoreUpTo opens a fresh store and applies only the migrations with version
// <= maxVersion, so a test can assert what a later migration does against real
// pre-migration data instead of reading its SQL. It mirrors Store.Open's DSN and
// Store.migrate, stopping early.
func openStoreUpTo(t *testing.T, maxVersion int) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "msp.db")
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	s := &Store{db: db, Now: func() time.Time { return time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC) }}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	applyMigrationsRange(t, s, 1, maxVersion)
	return s
}

// applyMigrationsRange applies every embedded migration whose version is in
// [lo, hi], in lexical order, recording each in schema_migrations.
func applyMigrationsRange(t *testing.T, s *Store, lo, hi int) {
	t.Helper()
	ctx := context.Background()
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		t.Fatalf("ReadDir migrations: %v", err)
	}
	var names []string
	for _, en := range entries {
		if !en.IsDir() && strings.HasSuffix(en.Name(), ".sql") {
			names = append(names, en.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		v, err := migrationVersion(name)
		if err != nil {
			t.Fatalf("version %s: %v", name, err)
		}
		if v < lo || v > hi {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := s.applyMigration(ctx, v, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}

func seqOf(t *testing.T, s *Store, table string) int64 {
	t.Helper()
	var seq int64
	if err := s.db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&seq); err != nil {
		t.Fatalf("sqlite_sequence %s: %v", table, err)
	}
	return seq
}

// TestMigration0013PreservesNotifyRules is the regression for the NS2 migration
// coverage gap: 0013 recreates notify_channels FK-safely (to widen the kind CHECK
// to accept 'sms'), and notify_rules references notify_channels ON DELETE CASCADE.
// The migration backs up and drops notify_rules before notify_channels so the
// cascade cannot fire, then rebuilds and restores both. This test seeds a channel,
// a rule referencing it and a delivery row under schema 0012, applies 0013, and
// asserts every row (and the rule->channel link) survives, the kind CHECK now
// accepts 'sms', escalation_policy_id/step default correctly, and the AUTOINCREMENT
// sequences are preserved. A future edit that reordered the drops (letting the
// cascade delete rules) or added an FK into notify_channels would fail here.
func TestMigration0013PreservesNotifyRules(t *testing.T) {
	s := openStoreUpTo(t, 12)
	ctx := context.Background()

	org, err := s.CreateOrg(ctx, "Acme Dental")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	user, err := s.CreateUser(ctx, "admin@example.com", "$2a$12$hash", "admin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := fmtTime(s.Now())

	// Seed at the pre-0013 schema (no escalation_policy_id, no delivery step column).
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO notify_channels (kind, name, org_id, config_json, secret, enabled, created_at, created_by)
		 VALUES ('webhook', 'ops-slack', ?, '{"url":"https://hooks.example.com/x","flavor":"slack"}', 'sign-secret', 1, ?, ?)`,
		org.ID, now, user.ID)
	if err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	channelID, _ := res.LastInsertId()

	res, err = s.db.ExecContext(ctx,
		`INSERT INTO notify_rules (name, org_id, min_severity, alert_types, on_open, on_resolve, channel_id, enabled, created_at, created_by)
		 VALUES ('ops-rule', ?, 'low', '', 1, 0, ?, 1, ?, ?)`,
		org.ID, channelID, now, user.ID)
	if err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	ruleID, _ := res.LastInsertId()

	res, err = s.db.ExecContext(ctx,
		`INSERT INTO notification_deliveries (alert_id, rule_id, channel_id, org_id, event, status, attempts, last_error, detail_json, created_at)
		 VALUES (NULL, ?, ?, ?, 'test', 'sent', 0, '', '{"kind":"webhook"}', ?)`,
		ruleID, channelID, org.ID, now)
	if err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
	deliveryID, _ := res.LastInsertId()

	// Apply the migration under test.
	applyMigrationsRange(t, s, 13, 13)

	// The channel survives unchanged.
	var (
		gotKind, gotName, gotSecret string
		gotChOrg                    sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT kind, name, secret, org_id FROM notify_channels WHERE id = ?`, channelID).
		Scan(&gotKind, &gotName, &gotSecret, &gotChOrg); err != nil {
		t.Fatalf("channel gone after 0013: %v", err)
	}
	if gotKind != "webhook" || gotName != "ops-slack" || gotSecret != "sign-secret" || !gotChOrg.Valid || gotChOrg.Int64 != org.ID {
		t.Fatalf("channel changed by 0013: kind=%q name=%q secretSet=%v org=%v", gotKind, gotName, gotSecret != "", gotChOrg)
	}

	// The rule survives (the cascade did NOT fire), keeps its channel_id, and gains
	// escalation_policy_id defaulting to NULL.
	var (
		gotChannelID int64
		gotPolicy    sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT channel_id, escalation_policy_id FROM notify_rules WHERE id = ?`, ruleID).
		Scan(&gotChannelID, &gotPolicy); err != nil {
		t.Fatalf("rule gone after 0013 (cascade fired?): %v", err)
	}
	if gotChannelID != channelID {
		t.Fatalf("rule.channel_id = %d, want %d (FK preserved)", gotChannelID, channelID)
	}
	if gotPolicy.Valid {
		t.Fatalf("rule.escalation_policy_id = %d, want NULL", gotPolicy.Int64)
	}

	// The delivery log row survives with step defaulting to 0 and rule_id intact.
	var (
		gotStep int
		gotRule int64
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT step, rule_id FROM notification_deliveries WHERE id = ?`, deliveryID).
		Scan(&gotStep, &gotRule); err != nil {
		t.Fatalf("delivery gone after 0013: %v", err)
	}
	if gotStep != 0 || gotRule != ruleID {
		t.Fatalf("delivery step=%d rule_id=%d, want 0/%d", gotStep, gotRule, ruleID)
	}

	// The AUTOINCREMENT sequences are preserved by the rebuild.
	if seq := seqOf(t, s, "notify_channels"); seq != channelID {
		t.Fatalf("notify_channels sequence = %d, want %d (preserved)", seq, channelID)
	}
	if seq := seqOf(t, s, "notify_rules"); seq != ruleID {
		t.Fatalf("notify_rules sequence = %d, want %d (preserved)", seq, ruleID)
	}

	// The widened kind CHECK now accepts 'sms', and the new channel gets the next id
	// (so the sequence truly continued rather than resetting).
	res, err = s.db.ExecContext(ctx,
		`INSERT INTO notify_channels (kind, name, org_id, config_json, secret, enabled, created_at, created_by)
		 VALUES ('sms', 'oncall-sms', ?, '{"provider":"generic"}', 'tok', 1, ?, ?)`,
		org.ID, now, user.ID)
	if err != nil {
		t.Fatalf("0013 kind CHECK does not accept 'sms': %v", err)
	}
	smsID, _ := res.LastInsertId()
	if smsID != channelID+1 {
		t.Fatalf("new sms channel id = %d, want %d (AUTOINCREMENT continued)", smsID, channelID+1)
	}

	// The full store methods now read the migrated schema correctly.
	r, err := s.GetRule(ctx, ruleID)
	if err != nil || r.EscalationPolicyID != nil || r.ChannelID != channelID {
		t.Fatalf("GetRule after 0013 = %+v, err %v", r, err)
	}
}
