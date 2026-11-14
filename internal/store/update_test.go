package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// TestCreateUpdateCommandRoundTrip queues a kind update command (AU1) and checks
// it stores and claims with version/url/sha256, that a non-update command omits
// those keys, and that the claimed agent JSON carries them.
func TestCreateUpdateCommandRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)
	hex := strings.Repeat("c", protocol.SHA256HexLen)

	cmd, err := s.CreateCommand(ctx, CreateCommandParams{
		DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: protocol.KindUpdate,
		Version: "0.2.0", URL: "/dl/msp-agent-linux-amd64", SHA256: hex, TimeoutS: 600,
	})
	if err != nil {
		t.Fatalf("CreateCommand update: %v", err)
	}
	if cmd.Kind != protocol.KindUpdate || cmd.Version != "0.2.0" || cmd.URL != "/dl/msp-agent-linux-amd64" ||
		cmd.SHA256 != hex || cmd.TimeoutS != 600 {
		t.Fatalf("stored update command: %+v", cmd)
	}
	// The stored COMMAND JSON carries version/url/sha256 and no deploy-only keys.
	cb, _ := json.Marshal(cmd)
	for _, k := range []string{`"kind":"update"`, `"version":"0.2.0"`, `"url":"/dl/msp-agent-linux-amd64"`, `"sha256":`} {
		if !strings.Contains(string(cb), k) {
			t.Fatalf("update COMMAND JSON missing %s: %s", k, cb)
		}
	}
	for _, k := range []string{"source", "file_id", "dest", "run_args", "package"} {
		if strings.Contains(string(cb), k) {
			t.Fatalf("update COMMAND JSON must omit %q: %s", k, cb)
		}
	}

	got, err := s.GetCommand(ctx, cmd.ID)
	if err != nil || got.Version != "0.2.0" || got.URL != "/dl/msp-agent-linux-amd64" || got.SHA256 != hex {
		t.Fatalf("GetCommand update: %+v err %v", got, err)
	}

	// The agent command JSON carries the update fields and validates.
	claimed, err := s.ClaimQueuedCommands(ctx, dev.ID, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimQueuedCommands = %+v, %v", claimed, err)
	}
	c := claimed[0]
	if c.Kind != protocol.KindUpdate || c.Version != "0.2.0" || c.URL != "/dl/msp-agent-linux-amd64" || c.SHA256 != hex {
		t.Fatalf("claimed update command: %+v", c)
	}
	wire, _ := json.Marshal(c)
	for _, k := range []string{`"kind":"update"`, `"version":"0.2.0"`, `"url":"/dl/msp-agent-linux-amd64"`, `"sha256":"` + hex + `"`} {
		if !strings.Contains(string(wire), k) {
			t.Fatalf("agent update JSON missing %s: %s", k, wire)
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("claimed update command must validate: %v", err)
	}

	// A non-update command omits version/url so its JSON shape is unchanged.
	ex, err := s.CreateCommand(ctx, CreateCommandParams{
		DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: protocol.KindExec,
		Argv: []string{"uname", "-a"}, TimeoutS: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Version != "" || ex.URL != "" {
		t.Fatalf("exec command must not carry update fields: %+v", ex)
	}
	eb, _ := json.Marshal(ex)
	for _, k := range []string{"version", `"url"`} {
		if strings.Contains(string(eb), k) {
			t.Fatalf("exec COMMAND JSON must omit %q: %s", k, eb)
		}
	}
}

// TestMigration0012PreservesShellSessions applies migrations 0001..0011, seeds a
// shell command with a shell_sessions row (and a deploy command), then applies
// 0012 and checks the rebuild kept the shell_sessions FK and rows, preserved the
// deploy fields, and now accepts an 'update' command with version/url
// (mirrors TestMigration0008PreservesShellSessions).
func TestMigration0012PreservesShellSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	dsn := "file:" + path + "?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	apply := func(name string) {
		t.Helper()
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			t.Fatalf("apply %s: %v", name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
	}
	for _, n := range []string{
		"0001_init.sql", "0002_heartbeats_at.sql", "0003_shell_sessions.sql",
		"0004_user_orgs.sql", "0005_notifications.sql", "0006_device_patches.sql",
		"0007_patch_commands.sql", "0008_files_and_deploy.sql", "0009_two_factor.sql",
		"0010_totp_last_step.sql", "0011_device_software.sql",
	} {
		apply(n)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO orgs (id, name, created_at) VALUES (1, 'Acme', '2026-11-13T10:00:00Z')`)
	mustExec(`INSERT INTO users (id, email, password_hash, role, created_at) VALUES (1, 'a@x', 'h', 'admin', '2026-11-13T10:00:00Z')`)
	mustExec(`INSERT INTO devices (id, org_id, device_uid, hostname, platform, enrolled_at) VALUES (1, 1, 'uid', 'ws', 'linux', '2026-11-13T10:00:00Z')`)
	mustExec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, shell, timeout_s, status, created_at) VALUES (7, 1, 1, 1, 'shell', 'bash', 14400, 'done', '2026-11-13T10:00:00Z')`)
	mustExec(`INSERT INTO shell_sessions (command_id, bytes_in, bytes_out, transcript, close_reason) VALUES (7, 11, 22, 'hello', 'technician_closed')`)
	// A deploy command from 0008 must survive the 0012 rebuild with its fields.
	mustExec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, source, file_id, sha256, dest, run, run_args_json, timeout_s, status, created_at) VALUES (9, 1, 1, 1, 'deploy', 'file', 1, 'abc', '/opt/x', 1, '["--silent"]', 3600, 'queued', '2026-11-13T10:00:00Z')`)

	apply("0012_update_command.sql")

	// shell_sessions survived with content and FK intact.
	var bytesIn, bytesOut int
	var transcript, reason string
	if err := db.QueryRow(`SELECT bytes_in, bytes_out, transcript, close_reason FROM shell_sessions WHERE command_id = 7`).
		Scan(&bytesIn, &bytesOut, &transcript, &reason); err != nil {
		t.Fatalf("shell_sessions row lost by 0012: %v", err)
	}
	if bytesIn != 11 || bytesOut != 22 || transcript != "hello" || reason != "technician_closed" {
		t.Fatalf("shell_sessions content changed: in=%d out=%d transcript=%q reason=%q", bytesIn, bytesOut, transcript, reason)
	}
	// The deploy command's fields survived the rebuild.
	var source, dest, runArgs string
	if err := db.QueryRow(`SELECT source, dest, run_args_json FROM commands WHERE id = 9`).Scan(&source, &dest, &runArgs); err != nil ||
		source != "file" || dest != "/opt/x" || runArgs != `["--silent"]` {
		t.Fatalf("deploy command fields lost: source=%q dest=%q run_args=%q err=%v", source, dest, runArgs, err)
	}

	// 'update' is now an accepted kind with its version/url columns; a bogus kind
	// is still rejected.
	mustExec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, version, url, sha256, timeout_s, status, created_at) VALUES (14, 1, 1, 1, 'update', '0.2.0', '/dl/msp-agent-linux-amd64', 'deadbeef', 600, 'queued', '2026-11-13T10:00:00Z')`)
	var version, url string
	if err := db.QueryRow(`SELECT version, url FROM commands WHERE id = 14`).Scan(&version, &url); err != nil ||
		version != "0.2.0" || url != "/dl/msp-agent-linux-amd64" {
		t.Fatalf("update command fields: version=%q url=%q err=%v", version, url, err)
	}
	if _, err := db.Exec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, timeout_s, status, created_at) VALUES (15, 1, 1, 1, 'bogus', 60, 'queued', '2026-11-13T10:00:00Z')`); err == nil {
		t.Fatal("the kind CHECK must still reject an unknown kind after 0012")
	}
	// The shell_sessions FK still bites.
	if _, err := db.Exec(`INSERT INTO shell_sessions (command_id) VALUES (999)`); err == nil {
		t.Fatal("shell_sessions FK to commands must still be enforced after 0012")
	}
}
