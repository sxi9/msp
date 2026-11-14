package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// TestFiles covers the SD1 file library: CreateFile/GetFile/ListFiles/DeleteFile,
// the org-scoped list (RS2: a global org_id NULL file is visible only to an
// all-orgs caller; a scoped caller sees only its own orgs' files), the
// created_by_email join and the JSON shape.
func TestFiles(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, u, _, _ := fixture(t, s)
	orgB, err := s.CreateOrg(ctx, "Beta Clinic")
	if err != nil {
		t.Fatal(err)
	}

	hex := strings.Repeat("a", protocol.SHA256HexLen)
	global, err := s.CreateFile(ctx, CreateFileParams{Name: "installer.sh", Size: 1234, SHA256: hex, ContentType: "application/x-sh", CreatedBy: &u.ID})
	if err != nil {
		t.Fatalf("CreateFile global: %v", err)
	}
	if global.OrgID != nil || global.Name != "installer.sh" || global.Size != 1234 || global.SHA256 != hex ||
		global.ContentType != "application/x-sh" || global.CreatedBy == nil || *global.CreatedBy != u.ID ||
		global.CreatedByEmail != "admin@example.com" || global.CreatedAt.IsZero() {
		t.Fatalf("global file = %+v", global)
	}
	fileA, err := s.CreateFile(ctx, CreateFileParams{OrgID: &orgA.ID, Name: "a.bin", Size: 10, SHA256: hex})
	if err != nil {
		t.Fatal(err)
	}
	fileB, err := s.CreateFile(ctx, CreateFileParams{OrgID: &orgB.ID, Name: "b.bin", Size: 20, SHA256: hex})
	if err != nil {
		t.Fatal(err)
	}
	if fileA.OrgID == nil || *fileA.OrgID != orgA.ID || fileA.CreatedBy != nil || fileA.CreatedByEmail != "" {
		t.Fatalf("file A = %+v (no uploader expected)", fileA)
	}

	// GetFile round-trips; a missing id is ErrNotFound.
	got, err := s.GetFile(ctx, fileB.ID)
	if err != nil || got.ID != fileB.ID || got.OrgID == nil || *got.OrgID != orgB.ID {
		t.Fatalf("GetFile = %+v, %v", got, err)
	}
	if _, err := s.GetFile(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFile missing err = %v", err)
	}

	// All-orgs caller (VisibleOrgs nil) sees every file, global included, newest
	// first.
	all, err := s.ListFiles(ctx, FileFilter{})
	if err != nil || len(all) != 3 || all[0].ID != fileB.ID || all[2].ID != global.ID {
		t.Fatalf("ListFiles all = %+v, %v", all, err)
	}
	// A caller scoped to org A sees only org A's file: not global, not org B's.
	scoped, err := s.ListFiles(ctx, FileFilter{VisibleOrgs: []int64{orgA.ID}})
	if err != nil || len(scoped) != 1 || scoped[0].ID != fileA.ID {
		t.Fatalf("ListFiles scoped A = %+v, %v", scoped, err)
	}
	// A scoped caller with no visible orgs sees nothing.
	if none, _ := s.ListFiles(ctx, FileFilter{VisibleOrgs: []int64{}}); len(none) != 0 {
		t.Fatalf("ListFiles empty visible set = %+v", none)
	}
	// An explicit org filter narrows further (here combined with all-orgs).
	byOrg, _ := s.ListFiles(ctx, FileFilter{OrgID: &orgB.ID})
	if len(byOrg) != 1 || byOrg[0].ID != fileB.ID {
		t.Fatalf("ListFiles org B filter = %+v", byOrg)
	}

	// JSON shape of a File.
	b, _ := json.Marshal(global)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "org_id", "name", "size", "sha256", "content_type", "created_at", "created_by", "created_by_email"} {
		if _, ok := m[k]; !ok {
			t.Errorf("FILE JSON lacks %q: %s", k, b)
		}
	}
	if m["org_id"] != nil {
		t.Fatalf("global file org_id must be null: %s", b)
	}

	// DeleteFile removes the row; a second delete is ErrNotFound. The store never
	// touches the filesystem (the server owns disk I/O).
	if err := s.DeleteFile(ctx, fileA.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFile(ctx, fileA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	if left, _ := s.ListFiles(ctx, FileFilter{}); len(left) != 2 {
		t.Fatalf("after delete = %+v", left)
	}
}

// TestCreateDeployCommandRoundTrip proves the WP-SCAFFOLD command side (SD1): a
// deploy command persists source/file_id/sha256/dest/run/run_args/package,
// exposes them on the dashboard COMMAND (store.Command) and emits them in the
// agent command JSON (protocol.Command), while a non-deploy command keeps its
// shape.
func TestCreateDeployCommandRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)
	hex := strings.Repeat("b", protocol.SHA256HexLen)

	// A file deploy with run + run_args.
	cmd, err := s.CreateCommand(ctx, CreateCommandParams{
		DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: protocol.KindDeploy,
		Source: protocol.DeploySourceFile, FileID: 7, SHA256: hex, Dest: "/opt/acme/installer.sh",
		Run: true, RunArgs: []string{"--silent", "--log=/tmp/x"}, TimeoutS: 3600,
	})
	if err != nil {
		t.Fatalf("CreateCommand deploy: %v", err)
	}
	if cmd.Kind != protocol.KindDeploy || cmd.Source != protocol.DeploySourceFile || cmd.FileID != 7 ||
		cmd.SHA256 != hex || cmd.Dest != "/opt/acme/installer.sh" || !cmd.Run ||
		len(cmd.RunArgs) != 2 || cmd.RunArgs[0] != "--silent" || cmd.Package != "" {
		t.Fatalf("stored deploy command: %+v", cmd)
	}

	got, err := s.GetCommand(ctx, cmd.ID)
	if err != nil || got.Source != protocol.DeploySourceFile || got.FileID != 7 || got.Dest != "/opt/acme/installer.sh" ||
		!got.Run || len(got.RunArgs) != 2 {
		t.Fatalf("GetCommand deploy: %+v err %v", got, err)
	}

	// A package deploy stores only the package name.
	pkgCmd, err := s.CreateCommand(ctx, CreateCommandParams{
		DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: protocol.KindDeploy,
		Source: protocol.DeploySourcePackage, Package: "htop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pkgCmd.Source != protocol.DeploySourcePackage || pkgCmd.Package != "htop" ||
		pkgCmd.FileID != 0 || pkgCmd.Dest != "" || pkgCmd.Run || pkgCmd.RunArgs != nil {
		t.Fatalf("package deploy command: %+v", pkgCmd)
	}
	pb, _ := json.Marshal(pkgCmd)
	if !strings.Contains(string(pb), `"package":"htop"`) || strings.Contains(string(pb), "file_id") ||
		strings.Contains(string(pb), "dest") || strings.Contains(string(pb), "run_args") {
		t.Fatalf("package deploy COMMAND JSON: %s", pb)
	}

	// The agent command JSON carries the deploy fields, oldest first.
	claimed, err := s.ClaimQueuedCommands(ctx, dev.ID, 10)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("ClaimQueuedCommands = %+v, %v", claimed, err)
	}
	c := claimed[0]
	if c.Kind != protocol.KindDeploy || c.Source != protocol.DeploySourceFile || c.FileID != 7 ||
		c.SHA256 != hex || c.Dest != "/opt/acme/installer.sh" || !c.Run || len(c.RunArgs) != 2 {
		t.Fatalf("claimed deploy command: %+v", c)
	}
	wire, _ := json.Marshal(c)
	for _, k := range []string{`"source":"file"`, `"file_id":7`, `"dest":"/opt/acme/installer.sh"`, `"run":true`, `"run_args":["--silent","--log=/tmp/x"]`} {
		if !strings.Contains(string(wire), k) {
			t.Fatalf("agent deploy JSON missing %s: %s", k, wire)
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("claimed deploy command must validate: %v", err)
	}
	pkgWire, _ := json.Marshal(claimed[1])
	if !strings.Contains(string(pkgWire), `"source":"package"`) || !strings.Contains(string(pkgWire), `"package":"htop"`) ||
		strings.Contains(string(pkgWire), "file_id") || strings.Contains(string(pkgWire), "dest") {
		t.Fatalf("agent package deploy JSON: %s", pkgWire)
	}

	// A non-deploy command omits every deploy field so its JSON shape is unchanged.
	ex, err := s.CreateCommand(ctx, CreateCommandParams{
		DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: protocol.KindExec,
		Argv: []string{"uname", "-a"}, TimeoutS: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Source != "" || ex.FileID != 0 || ex.Dest != "" || ex.Run || ex.RunArgs != nil || ex.Package != "" {
		t.Fatalf("exec command must not carry deploy fields: %+v", ex)
	}
	eb, _ := json.Marshal(ex)
	for _, k := range []string{"source", "file_id", "sha256", "dest", `"run"`, "run_args", "package"} {
		if strings.Contains(string(eb), k) {
			t.Fatalf("exec COMMAND JSON must omit %q: %s", k, eb)
		}
	}
}

// TestMigration0008PreservesShellSessions applies migrations 0001..0007, seeds a
// shell command with a shell_sessions row, then applies 0008 and checks the
// rebuild kept the shell_sessions FK and rows, created the files table and now
// accepts a 'deploy' command (mirrors TestMigration0007PreservesShellSessions).
func TestMigration0008PreservesShellSessions(t *testing.T) {
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
		"0007_patch_commands.sql",
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
	// A patch command from 0007 must survive the 0008 rebuild with its fields.
	mustExec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, action, mode, reboot, patch_ids_json, timeout_s, status, created_at) VALUES (8, 1, 1, 1, 'patch', 'install', 'ids', 'never', '["openssl"]', 3600, 'queued', '2026-11-13T10:00:00Z')`)

	apply("0008_files_and_deploy.sql")

	// shell_sessions survived with content and FK intact.
	var bytesIn, bytesOut int
	var transcript, reason string
	if err := db.QueryRow(`SELECT bytes_in, bytes_out, transcript, close_reason FROM shell_sessions WHERE command_id = 7`).
		Scan(&bytesIn, &bytesOut, &transcript, &reason); err != nil {
		t.Fatalf("shell_sessions row lost by 0008: %v", err)
	}
	if bytesIn != 11 || bytesOut != 22 || transcript != "hello" || reason != "technician_closed" {
		t.Fatalf("shell_sessions content changed: in=%d out=%d transcript=%q reason=%q", bytesIn, bytesOut, transcript, reason)
	}
	// The patch command's fields survived the rebuild.
	var action, mode, ids string
	if err := db.QueryRow(`SELECT action, mode, patch_ids_json FROM commands WHERE id = 8`).Scan(&action, &mode, &ids); err != nil ||
		action != "install" || mode != "ids" || ids != `["openssl"]` {
		t.Fatalf("patch command fields lost: action=%q mode=%q ids=%q err=%v", action, mode, ids, err)
	}

	// The files table exists and cascades on its org.
	mustExec(`INSERT INTO files (id, org_id, name, size, sha256, content_type, created_at, created_by) VALUES (1, 1, 'a.sh', 10, 'deadbeef', '', '2026-11-13T10:00:00Z', 1)`)
	mustExec(`INSERT INTO files (id, org_id, name, size, sha256, created_at) VALUES (2, NULL, 'global.sh', 20, 'cafebabe', '2026-11-13T10:00:00Z')`)
	if _, err := db.Exec(`INSERT INTO files (id, org_id, name, size, sha256, created_at) VALUES (3, 999, 'x', 1, 'x', '2026-11-13T10:00:00Z')`); err == nil {
		t.Fatal("files.org_id FK to orgs must be enforced")
	}

	// 'deploy' is now an accepted kind; a bogus kind is still rejected.
	mustExec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, source, file_id, sha256, dest, run, run_args_json, timeout_s, status, created_at) VALUES (9, 1, 1, 1, 'deploy', 'file', 1, 'abc', '/opt/x', 1, '["--silent"]', 3600, 'queued', '2026-11-13T10:00:00Z')`)
	mustExec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, source, package, timeout_s, status, created_at) VALUES (10, 1, 1, 1, 'deploy', 'package', 'htop', 3600, 'queued', '2026-11-13T10:00:00Z')`)
	if _, err := db.Exec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, timeout_s, status, created_at) VALUES (11, 1, 1, 1, 'bogus', 60, 'queued', '2026-11-13T10:00:00Z')`); err == nil {
		t.Fatal("the kind CHECK must still reject an unknown kind after 0008")
	}
	// The shell_sessions FK still bites.
	if _, err := db.Exec(`INSERT INTO shell_sessions (command_id) VALUES (999)`); err == nil {
		t.Fatal("shell_sessions FK to commands must still be enforced after 0008")
	}
}
