package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// TestCreatePatchCommandRoundTrip proves the WP-STORE command side (PM1): a
// patch command persists action/mode/reboot and the ids list, exposes them on
// the dashboard COMMAND (store.Command), and ClaimQueuedCommands emits them in
// the agent command JSON (protocol.Command) while a non-patch command keeps its
// shape. It also checks the patch timeout clamps on the patch scale.
func TestCreatePatchCommandRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)

	cmd, err := s.CreateCommand(ctx, CreateCommandParams{
		DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: protocol.KindPatch,
		Action: protocol.PatchActionInstall, Mode: protocol.PatchModeIDs,
		IDs: []string{"openssl", "curl"}, Reboot: protocol.PatchRebootIfRequired,
		TimeoutS: 0,
	})
	if err != nil {
		t.Fatalf("CreateCommand patch: %v", err)
	}
	if cmd.Kind != protocol.KindPatch || cmd.Action != protocol.PatchActionInstall ||
		cmd.Mode != protocol.PatchModeIDs || cmd.Reboot != protocol.PatchRebootIfRequired ||
		len(cmd.IDs) != 2 || cmd.IDs[0] != "openssl" {
		t.Fatalf("stored patch command: %+v", cmd)
	}
	if cmd.TimeoutS != protocol.DefaultPatchTimeoutS {
		t.Fatalf("patch timeout = %d, want default %d", cmd.TimeoutS, protocol.DefaultPatchTimeoutS)
	}

	// The dashboard COMMAND JSON exposes the patch fields; a re-read matches.
	got, err := s.GetCommand(ctx, cmd.ID)
	if err != nil || got.Action != protocol.PatchActionInstall || got.Mode != protocol.PatchModeIDs ||
		got.Reboot != protocol.PatchRebootIfRequired || len(got.IDs) != 2 {
		t.Fatalf("GetCommand patch: %+v err %v", got, err)
	}

	// The agent command JSON carries action/mode/reboot/ids.
	claimed, err := s.ClaimQueuedCommands(ctx, dev.ID, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimQueuedCommands = %+v, %v", claimed, err)
	}
	c := claimed[0]
	if c.Kind != protocol.KindPatch || c.Action != protocol.PatchActionInstall ||
		c.Mode != protocol.PatchModeIDs || c.Reboot != protocol.PatchRebootIfRequired ||
		len(c.IDs) != 2 || c.IDs[1] != "curl" {
		t.Fatalf("claimed patch command: %+v", c)
	}
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"action":"install"`) || !strings.Contains(string(b), `"ids":["openssl","curl"]`) {
		t.Fatalf("agent patch JSON: %s", b)
	}

	// A patch timeout over the ceiling clamps to MaxPatchTimeoutS (not the 3600
	// one-shot cap).
	big, err := s.CreateCommand(ctx, CreateCommandParams{
		DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: protocol.KindPatch,
		Action: protocol.PatchActionScan, TimeoutS: protocol.MaxPatchTimeoutS + 1000,
	})
	if err != nil || big.TimeoutS != protocol.MaxPatchTimeoutS {
		t.Fatalf("patch timeout clamp: %d err %v", big.TimeoutS, err)
	}

	// A non-patch command omits the patch fields (empty strings, nil ids) so its
	// JSON shape is unchanged.
	ex, err := s.CreateCommand(ctx, CreateCommandParams{
		DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: protocol.KindExec,
		Argv: []string{"uname", "-a"}, TimeoutS: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Action != "" || ex.Mode != "" || ex.Reboot != "" || ex.IDs != nil {
		t.Fatalf("exec command must not carry patch fields: %+v", ex)
	}
	eb, _ := json.Marshal(ex)
	if strings.Contains(string(eb), "action") || strings.Contains(string(eb), `"ids"`) ||
		strings.Contains(string(eb), "reboot") || strings.Contains(string(eb), `"mode"`) {
		t.Fatalf("exec COMMAND JSON must omit patch fields: %s", eb)
	}
}

// TestMigration0007PreservesShellSessions applies migrations 0001..0006, seeds a
// shell command with a shell_sessions row (the first and only child of
// commands), then applies 0007 and checks the rebuild kept the shell_sessions
// FK and rows and now accepts a 'patch' command. The migration runner wraps each
// file in one foreign_keys=ON transaction, so a naive DROP TABLE commands would
// cascade-delete shell_sessions; this guards that it does not.
func TestMigration0007PreservesShellSessions(t *testing.T) {
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
	} {
		apply(n)
	}

	// Seed org/user/device/shell command and a shell_sessions row with content.
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

	apply("0007_patch_commands.sql")

	// The shell_sessions row survived with its content and FK intact.
	var bytesIn, bytesOut int
	var transcript, reason string
	if err := db.QueryRow(`SELECT bytes_in, bytes_out, transcript, close_reason FROM shell_sessions WHERE command_id = 7`).
		Scan(&bytesIn, &bytesOut, &transcript, &reason); err != nil {
		t.Fatalf("shell_sessions row lost by 0007: %v", err)
	}
	if bytesIn != 11 || bytesOut != 22 || transcript != "hello" || reason != "technician_closed" {
		t.Fatalf("shell_sessions content changed: in=%d out=%d transcript=%q reason=%q", bytesIn, bytesOut, transcript, reason)
	}
	// The shell command row is still there and still rejects an unknown kind.
	var kind string
	if err := db.QueryRow(`SELECT kind FROM commands WHERE id = 7`).Scan(&kind); err != nil || kind != "shell" {
		t.Fatalf("shell command lost: kind=%q err=%v", kind, err)
	}
	// 'patch' is now an accepted kind; a bogus kind is still rejected by the CHECK.
	mustExec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, action, mode, reboot, patch_ids_json, timeout_s, status, created_at) VALUES (8, 1, 1, 1, 'patch', 'install', 'ids', 'never', '["openssl"]', 3600, 'queued', '2026-11-13T10:00:00Z')`)
	if _, err := db.Exec(`INSERT INTO commands (id, device_id, org_id, created_by, kind, timeout_s, status, created_at) VALUES (9, 1, 1, 1, 'bogus', 60, 'queued', '2026-11-13T10:00:00Z')`); err == nil {
		t.Fatal("the kind CHECK must still reject an unknown kind after 0007")
	}
	// The FK still bites: a shell_sessions row for a missing command is refused.
	if _, err := db.Exec(`INSERT INTO shell_sessions (command_id) VALUES (999)`); err == nil {
		t.Fatal("shell_sessions FK to commands must still be enforced after 0007")
	}
}

func TestRecordPatches(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, _, dev, _ := fixture(t, s)

	// A fresh device has an empty report object and zeroed denormalised columns.
	if dev.PatchesJSON != "{}" || dev.PendingPatches != 0 || dev.SecurityPatches != 0 ||
		dev.RebootRequired || dev.PatchesCheckedAt != nil {
		t.Fatalf("fresh device patch fields: %+v", dev)
	}

	checked := time.Date(2026, 11, 13, 10, 0, 1, 0, time.UTC)
	report := protocol.PatchReport{
		CheckedAt:      checked,
		Manager:        protocol.PatchManagerApt,
		RebootRequired: true,
		Count:          3,
		SecurityCount:  1,
		Updates: []protocol.PatchUpdate{
			{Name: "openssl", Current: "3.0.1", New: "3.0.2", Security: true, Source: "noble-security"},
			{Name: "curl", Current: "8.5.0", New: "8.5.1", Source: "noble-updates"},
			{Name: "vim", Current: "9.1.0016", New: "9.1.0101", Source: "noble-updates"},
		},
	}
	if err := s.RecordPatches(ctx, dev.ID, report); err != nil {
		t.Fatalf("RecordPatches: %v", err)
	}

	d, err := s.GetDevice(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.PendingPatches != 3 || d.SecurityPatches != 1 || !d.RebootRequired {
		t.Fatalf("derived counts: pending %d security %d reboot %v", d.PendingPatches, d.SecurityPatches, d.RebootRequired)
	}
	if d.PatchesCheckedAt == nil || !d.PatchesCheckedAt.Equal(checked) {
		t.Fatalf("patches_checked_at = %v, want %v", d.PatchesCheckedAt, checked)
	}
	// The full report round-trips through PatchesJSON, which never reaches DEVICE JSON.
	var got protocol.PatchReport
	if err := json.Unmarshal([]byte(d.PatchesJSON), &got); err != nil {
		t.Fatalf("patches_json is not a PatchReport: %v (%s)", err, d.PatchesJSON)
	}
	if got.Manager != protocol.PatchManagerApt || len(got.Updates) != 3 || got.Updates[0].Name != "openssl" || !got.Updates[0].Security {
		t.Fatalf("stored report: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("stored report must validate: %v", err)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "patches_json") || strings.Contains(string(b), `"patches"`) {
		t.Fatalf("DEVICE JSON must not carry the full report or raw column: %s", b)
	}

	// The device list carries the denormalised counts for the fleet column.
	list, err := s.ListDevices(ctx, DeviceFilter{OrgID: &org.ID})
	if err != nil || len(list) != 1 {
		t.Fatalf("ListDevices = %+v, %v", list, err)
	}
	if list[0].PendingPatches != 3 || list[0].SecurityPatches != 1 || !list[0].RebootRequired {
		t.Fatalf("list patch fields: %+v", list[0])
	}

	// A later report replaces all five columns (counts shrink, reboot clears).
	report2 := protocol.PatchReport{
		CheckedAt: checked.Add(6 * time.Hour),
		Manager:   protocol.PatchManagerApt,
		Count:     1,
		Updates: []protocol.PatchUpdate{
			{Name: "curl", Current: "8.5.0", New: "8.5.1", Source: "noble-updates"},
		},
	}
	if err := s.RecordPatches(ctx, dev.ID, report2); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, dev.ID)
	if d.PendingPatches != 1 || d.SecurityPatches != 0 || d.RebootRequired {
		t.Fatalf("after second report: pending %d security %d reboot %v", d.PendingPatches, d.SecurityPatches, d.RebootRequired)
	}

	// manager "none" with no updates and no timestamp: empty list, NULL check.
	if err := s.RecordPatches(ctx, dev.ID, protocol.PatchReport{Manager: protocol.PatchManagerNone}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, dev.ID)
	if d.PendingPatches != 0 || d.SecurityPatches != 0 || d.PatchesCheckedAt != nil {
		t.Fatalf("none report: %+v", d)
	}
	var none protocol.PatchReport
	if err := json.Unmarshal([]byte(d.PatchesJSON), &none); err != nil || none.Updates == nil || len(none.Updates) != 0 {
		t.Fatalf("none report updates must be a non-nil empty list: %+v err %v", none, err)
	}

	// Missing device.
	if err := s.RecordPatches(ctx, 9999, report); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RecordPatches missing device err = %v", err)
	}
}

func TestRecordPatchesCountFallback(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	_, _, dev, _ := fixture(t, s)

	// The agent capped its list but reported larger totals: the denormalised
	// counts must not understate the host, so the agent totals win.
	report := protocol.PatchReport{
		Manager:       protocol.PatchManagerApt,
		Count:         620,
		SecurityCount: 44,
		Updates: []protocol.PatchUpdate{
			{Name: "openssl", New: "3.0.2", Security: true},
			{Name: "curl", New: "8.5.1"},
		},
	}
	if err := s.RecordPatches(ctx, dev.ID, report); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, dev.ID)
	if d.PendingPatches != 620 || d.SecurityPatches != 44 {
		t.Fatalf("capped-list counts: pending %d security %d", d.PendingPatches, d.SecurityPatches)
	}
}
