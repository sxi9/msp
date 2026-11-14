package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// clock is an overridable test clock; every store under test reads it.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openTest(t *testing.T) (*Store, *clock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "msp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	s.Now = c.now
	return s, c
}

// fixture creates an org, an admin user and a linux device, returning the
// device and its plaintext token.
func fixture(t *testing.T, s *Store) (Org, User, Device, string) {
	t.Helper()
	ctx := context.Background()
	org, err := s.CreateOrg(ctx, "Acme Dental")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	u, err := s.CreateUser(ctx, "Admin@Example.com", "$2a$12$hash", "admin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	dev, tok, err := s.UpsertDevice(ctx, UpsertDeviceParams{
		OrgID: org.ID, DeviceUID: "uid-1", Hostname: "ws-01", Platform: "linux", Arch: "amd64",
		OSVersion: "Ubuntu 24.04", AgentVersion: "0.1.0", IntervalS: 30, FactsJSON: `{"kernel":"6.8"}`,
	})
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	return org, u, dev, tok
}

// migrationCount is the number of files in migrations/ (0001_init,
// 0002_heartbeats_at, 0003_shell_sessions, 0004_user_orgs, 0005_notifications,
// 0006_device_patches).
const migrationCount = 6

func TestOpenMigratesAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "msp.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "one"); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != migrationCount {
		t.Fatalf("schema_migrations rows = %d, err %v", n, err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if err := s2.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	orgs, err := s2.ListOrgs(ctx, nil)
	if err != nil || len(orgs) != 1 || orgs[0].Name != "one" {
		t.Fatalf("after reopen orgs = %+v, err %v", orgs, err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != migrationCount {
		t.Fatalf("schema_migrations rows after reopen = %d, err %v", n, err)
	}
	var plan string
	if err := s2.db.QueryRow(`EXPLAIN QUERY PLAN DELETE FROM heartbeats WHERE id IN (SELECT id FROM heartbeats WHERE at < '2026-01-01T00:00:00Z' LIMIT 5000)`).Scan(new(int), new(int), new(int), &plan); err == nil && strings.Contains(plan, "SCAN heartbeats") && !strings.Contains(plan, "INDEX") {
		t.Fatalf("heartbeat prune does not use an index: %s", plan)
	}
	var fk int
	if err := s2.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, err %v", fk, err)
	}
	var mode string
	if err := s2.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, err %v", mode, err)
	}
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") should fail")
	}
}

func TestTokensAndHashing(t *testing.T) {
	tok := NewToken("et_")
	if len(tok) != 3+43 || !strings.HasPrefix(tok, "et_") {
		t.Fatalf("NewToken = %q (len %d)", tok, len(tok))
	}
	if tok == NewToken("et_") {
		t.Fatal("two tokens are equal")
	}
	if len(HashToken(tok)) != 64 || HashToken(tok) != HashToken(tok) {
		t.Fatal("HashToken is not a stable sha256 hex")
	}
}

func TestStatusOf(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	cases := []struct {
		name     string
		lastSeen *time.Time
		interval int
		want     string
	}{
		{"never seen", nil, 30, "pending"},
		{"just now", at(0), 30, "online"},
		{"30s at boundary 105s", at(105 * time.Second), 30, "online"},
		{"30s one past boundary", at(106 * time.Second), 30, "offline"},
		{"60s at boundary 195s", at(195 * time.Second), 60, "online"},
		{"60s past boundary", at(196 * time.Second), 60, "offline"},
		{"2s at boundary 21s", at(21 * time.Second), 2, "online"},
		{"2s past boundary", at(22 * time.Second), 2, "offline"},
	}
	for _, c := range cases {
		if got := StatusOf(c.lastSeen, c.interval, now); got != c.want {
			t.Errorf("%s: StatusOf = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOrgsAndUsers(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)

	if _, err := s.CreateOrg(ctx, "Acme Dental"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate org err = %v, want ErrConflict", err)
	}
	if _, err := s.CreateUser(ctx, "admin@example.com", "x", "viewer"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate user err = %v, want ErrConflict", err)
	}
	if u.Email != "admin@example.com" {
		t.Fatalf("email not lowercased: %q", u.Email)
	}
	got, err := s.GetUserByEmail(ctx, "ADMIN@example.com")
	if err != nil || got.ID != u.ID || got.PasswordHash != "$2a$12$hash" {
		t.Fatalf("GetUserByEmail = %+v, %v", got, err)
	}
	if _, err := s.GetUserByEmail(ctx, "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user err = %v", err)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatalf("CountUsers = %d", n)
	}
	if _, err := s.GetOrg(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOrg missing err = %v", err)
	}

	// Counts: the device is pending (never synced), one open alert.
	if _, err := s.OpenAlert(ctx, org.ID, dev.ID, "offline", "high", "no sync"); err != nil {
		t.Fatal(err)
	}
	orgs, err := s.ListOrgs(ctx, nil)
	if err != nil || len(orgs) != 1 {
		t.Fatalf("ListOrgs = %+v, %v", orgs, err)
	}
	if o := orgs[0]; o.DeviceCount != 1 || o.OnlineCount != 0 || o.OpenAlerts != 1 {
		t.Fatalf("org counts = %+v", o)
	}
	if err := s.RecordSync(ctx, dev.ID, "10.0.0.1", protocol.SyncRequest{IntervalS: 30}, time.Minute); err != nil {
		t.Fatal(err)
	}
	o, err := s.GetOrg(ctx, org.ID)
	if err != nil || o.OnlineCount != 1 {
		t.Fatalf("GetOrg after sync = %+v, %v", o, err)
	}

	// Disable: GetSession must stop working and the user stays listed.
	plain, err := s.CreateSession(ctx, u.ID, "127.0.0.1", 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, plain); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if err := s.DisableUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableUser(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DisableUser missing = %v", err)
	}
	if _, err := s.GetSession(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session of disabled user err = %v", err)
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].DisabledAt == nil {
		t.Fatalf("ListUsers = %+v, %v", users, err)
	}
	b, _ := json.Marshal(users[0])
	if strings.Contains(string(b), "password") {
		t.Fatalf("user JSON leaks the hash: %s", b)
	}
}

func TestSessions(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	_, u, _, _ := fixture(t, s)

	plain, err := s.CreateSession(ctx, u.ID, "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "ss_") {
		t.Fatalf("session plaintext %q", plain)
	}
	got, err := s.GetSession(ctx, plain)
	if err != nil || got.ID != u.ID {
		t.Fatalf("GetSession = %+v, %v", got, err)
	}
	if _, err := s.GetSession(ctx, "ss_bogus"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bogus session err = %v", err)
	}
	c.advance(time.Hour + time.Second)
	if _, err := s.GetSession(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session err = %v", err)
	}
	n, err := s.PruneSessions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("PruneSessions = %d, %v", n, err)
	}
	plain2, _ := s.CreateSession(ctx, u.ID, "", time.Hour)
	if err := s.DeleteSession(ctx, plain2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, plain2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session err = %v", err)
	}
}

func TestEnrollmentTokens(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, u, _, _ := fixture(t, s)

	plain, tok, err := s.CreateEnrollmentToken(ctx, org.ID, "front desk", 2, time.Hour, &u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "et_") || tok.TokenPrefix != plain[:10] || tok.MaxUses != 2 || tok.Uses != 0 ||
		tok.CreatedBy == nil || *tok.CreatedBy != u.ID || tok.Label != "front desk" {
		t.Fatalf("token = %+v plain %q", tok, plain)
	}
	if _, err := s.ConsumeEnrollmentToken(ctx, "et_nope"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown token err = %v", err)
	}
	got, err := s.ConsumeEnrollmentToken(ctx, plain)
	if err != nil || got.Uses != 1 {
		t.Fatalf("first consume = %+v, %v", got, err)
	}
	got, err = s.ConsumeEnrollmentToken(ctx, plain)
	if err != nil || got.Uses != 2 {
		t.Fatalf("second consume = %+v, %v", got, err)
	}
	if _, err := s.ConsumeEnrollmentToken(ctx, plain); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("exhausted token err = %v", err)
	}

	// Expiry.
	plainExp, _, err := s.CreateEnrollmentToken(ctx, org.ID, "", 10, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour + time.Second)
	if _, err := s.ConsumeEnrollmentToken(ctx, plainExp); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token err = %v", err)
	}

	// Revoke.
	plainRev, tokRev, err := s.CreateEnrollmentToken(ctx, org.ID, "", 10, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeEnrollmentToken(ctx, org.ID+1, tokRev.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke in wrong org err = %v", err)
	}
	if err := s.RevokeEnrollmentToken(ctx, org.ID, tokRev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeEnrollmentToken(ctx, plainRev); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token err = %v", err)
	}
	list, err := s.ListEnrollmentTokens(ctx, org.ID)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListEnrollmentTokens = %d, %v", len(list), err)
	}
	if list[0].ID != tokRev.ID || list[0].RevokedAt == nil {
		t.Fatalf("newest first / revoked_at: %+v", list[0])
	}
}

func TestConsumeEnrollmentTokenConcurrent(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, _, _, _ := fixture(t, s)
	const maxUses = 7
	plain, tok, err := s.CreateEnrollmentToken(ctx, org.ID, "", maxUses, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ok, bad int
	)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ConsumeEnrollmentToken(ctx, plain)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrInvalidToken):
				bad++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != maxUses || bad != 20-maxUses {
		t.Fatalf("ok=%d bad=%d", ok, bad)
	}
	list, _ := s.ListEnrollmentTokens(ctx, org.ID)
	if len(list) != 1 || list[0].ID != tok.ID || list[0].Uses != maxUses {
		t.Fatalf("uses = %+v", list)
	}
}

func TestUpsertDeviceRotatesToken(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, _, dev, tok := fixture(t, s)

	if !strings.HasPrefix(tok, "dt_") {
		t.Fatalf("device token %q", tok)
	}
	if dev.Status != "pending" || dev.Revoked || !dev.CanRunCommands || dev.OrgName != "Acme Dental" {
		t.Fatalf("device = %+v", dev)
	}
	byTok, err := s.GetDeviceByToken(ctx, tok)
	if err != nil || byTok.ID != dev.ID {
		t.Fatalf("GetDeviceByToken = %+v, %v", byTok, err)
	}
	// Add a heartbeat and a command so we can prove history survives.
	if err := s.RecordSync(ctx, dev.ID, "1.2.3.4", protocol.SyncRequest{IntervalS: 30}, time.Minute); err != nil {
		t.Fatal(err)
	}

	dev2, tok2, err := s.UpsertDevice(ctx, UpsertDeviceParams{
		OrgID: org.ID, DeviceUID: "uid-1", Hostname: "ws-01-renamed", Platform: "linux", Arch: "amd64",
		OSVersion: "Ubuntu 24.10", AgentVersion: "0.2.0", IntervalS: 45,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dev2.ID != dev.ID {
		t.Fatalf("re-enroll changed id %d -> %d", dev.ID, dev2.ID)
	}
	if tok2 == tok {
		t.Fatal("token not rotated")
	}
	if dev2.Hostname != "ws-01-renamed" || dev2.OSVersion != "Ubuntu 24.10" || dev2.IntervalS != 45 || dev2.FactsJSON != "{}" {
		t.Fatalf("inventory not refreshed: %+v", dev2)
	}
	if dev2.LastSeenAt == nil {
		t.Fatal("history (last_seen_at) lost on re-enroll")
	}
	if _, err := s.GetDeviceByToken(ctx, tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("old token err = %v", err)
	}
	if d, err := s.GetDeviceByToken(ctx, tok2); err != nil || d.ID != dev.ID {
		t.Fatalf("new token: %v", err)
	}
	hb, _ := s.ListHeartbeats(ctx, dev.ID, 10)
	if len(hb) != 1 {
		t.Fatalf("heartbeats after re-enroll = %d", len(hb))
	}

	// Same uid in another org is a different device.
	org2, _ := s.CreateOrg(ctx, "Other")
	dev3, _, err := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org2.ID, DeviceUID: "uid-1", Hostname: "x", Platform: "windows"})
	if err != nil || dev3.ID == dev.ID {
		t.Fatalf("cross-org upsert = %+v, %v", dev3, err)
	}

	// Revoke.
	if err := s.RevokeDevice(ctx, dev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDeviceByToken(ctx, tok2); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token err = %v", err)
	}
	d, _ := s.GetDevice(ctx, dev.ID)
	if !d.Revoked || d.CanRunCommands {
		t.Fatalf("revoked device = %+v", d)
	}
	if err := s.RevokeDevice(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing = %v", err)
	}
	// Re-enrolling a revoked device gives it a token again.
	d4, tok4, err := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org.ID, DeviceUID: "uid-1", Hostname: "ws-01", Platform: "linux"})
	if err != nil || d4.ID != dev.ID || d4.Revoked || tok4 == "" {
		t.Fatalf("re-enroll after revoke = %+v, %v", d4, err)
	}
}

func TestRecordSyncAndHeartbeats(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	_, _, dev, _ := fixture(t, s)

	cpu := 12.5
	batt := 87
	charging := false
	req := protocol.SyncRequest{
		AgentVersion: "0.1.1", IntervalS: 30, WaitS: 25, UptimeS: 86400,
		Metrics: protocol.Metrics{
			CPUPct: &cpu, MemTotalMB: 16000, MemUsedMB: 8120,
			Disks:      []protocol.Disk{{Mount: "/", TotalGB: 476, UsedGB: 201.3}, {Mount: "/data", TotalGB: 100, UsedGB: 90}},
			BatteryPct: &batt, BatteryCharging: &charging, IPs: []string{"10.0.0.12"},
		},
		Facts: &protocol.Facts{Kernel: "6.8.0", CPUCount: 8},
	}
	if err := s.RecordSync(ctx, dev.ID, "10.0.0.12", req, time.Minute); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDevice(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "online" || d.LastIP != "10.0.0.12" || d.AgentVersion != "0.1.1" || d.LastSeenAt == nil {
		t.Fatalf("after sync = %+v", d)
	}
	if d.CPUPct == nil || *d.CPUPct != 12.5 || d.MemUsedPct == nil || *d.MemUsedPct != 50.8 ||
		d.DiskUsedPct == nil || *d.DiskUsedPct != 90 || d.BatteryPct == nil || *d.BatteryPct != 87 ||
		d.BatteryCharging == nil || *d.BatteryCharging || d.UptimeS == nil || *d.UptimeS != 86400 {
		t.Fatalf("metric columns = cpu %v mem %v disk %v batt %v chg %v up %v",
			d.CPUPct, d.MemUsedPct, d.DiskUsedPct, d.BatteryPct, d.BatteryCharging, d.UptimeS)
	}
	if !strings.Contains(d.FactsJSON, `"kernel":"6.8.0"`) || !strings.Contains(d.LastMetricsJSON, `"mem_used_mb":8120`) {
		t.Fatalf("json columns: facts %s metrics %s", d.FactsJSON, d.LastMetricsJSON)
	}

	// Second sync 1 s later: no facts (kept), no new heartbeat row.
	c.advance(time.Second)
	req2 := req
	req2.Facts = nil
	req2.Metrics.CPUPct = nil
	req2.Metrics.MemTotalMB = 0
	req2.Metrics.Disks = nil
	req2.Metrics.BatteryPct = nil
	req2.Metrics.BatteryCharging = nil
	if err := s.RecordSync(ctx, dev.ID, "10.0.0.13", req2, time.Minute); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, dev.ID)
	if d.CPUPct != nil || d.MemUsedPct != nil || d.DiskUsedPct != nil || d.BatteryPct != nil || d.BatteryCharging != nil {
		t.Fatalf("null metrics not stored as NULL: %+v", d)
	}
	if !strings.Contains(d.FactsJSON, `"kernel":"6.8.0"`) {
		t.Fatalf("facts replaced although absent: %s", d.FactsJSON)
	}
	hb, err := s.ListHeartbeats(ctx, dev.ID, 120)
	if err != nil || len(hb) != 1 {
		t.Fatalf("heartbeats after two syncs 1 s apart = %d, %v", len(hb), err)
	}

	// 60 s later a new sample is taken; oldest first.
	c.advance(60 * time.Second)
	if err := s.RecordSync(ctx, dev.ID, "", req, time.Minute); err != nil {
		t.Fatal(err)
	}
	hb, _ = s.ListHeartbeats(ctx, dev.ID, 120)
	if len(hb) != 2 || !hb[0].At.Before(hb[1].At) || hb[1].CPUPct == nil || *hb[1].CPUPct != 12.5 {
		t.Fatalf("heartbeats = %+v", hb)
	}
	if got, _ := s.ListHeartbeats(ctx, dev.ID, 1); len(got) != 1 || !got[0].At.Equal(hb[1].At) {
		t.Fatalf("limit 1 should return the newest: %+v", got)
	}

	// Prune.
	c.advance(7 * 24 * time.Hour)
	n, err := s.PruneHeartbeats(ctx, 7*24*time.Hour-time.Minute)
	if err != nil || n != 2 {
		t.Fatalf("PruneHeartbeats = %d, %v", n, err)
	}
	if err := s.RecordSync(ctx, 999, "", req, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sync for missing device err = %v", err)
	}

	// Android interval clamp: 0 -> 60.
	org, _ := s.GetOrg(ctx, dev.OrgID)
	adev, _, _ := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org.ID, DeviceUID: "and", Hostname: "Pixel", Platform: "android"})
	if adev.IntervalS != 60 || adev.CanRunCommands {
		t.Fatalf("android device = %+v", adev)
	}
	if err := s.RecordSync(ctx, adev.ID, "", protocol.SyncRequest{IntervalS: 0}, time.Minute); err != nil {
		t.Fatal(err)
	}
	adev, _ = s.GetDevice(ctx, adev.ID)
	if adev.IntervalS != 60 {
		t.Fatalf("android interval after sync = %d", adev.IntervalS)
	}
}

func TestListDevicesAndDelete(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, _, dev, _ := fixture(t, s)
	org2, _ := s.CreateOrg(ctx, "Beta Clinic")
	dev2, _, _ := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org2.ID, DeviceUID: "u2", Hostname: "srv-100%", Platform: "windows"})
	dev3, _, _ := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org.ID, DeviceUID: "u3", Hostname: "ws-02", Platform: "linux", IntervalS: 30})

	if err := s.RecordSync(ctx, dev3.ID, "", protocol.SyncRequest{IntervalS: 30}, time.Minute); err != nil {
		t.Fatal(err)
	}
	c.advance(200 * time.Second)
	if err := s.RecordSync(ctx, dev.ID, "", protocol.SyncRequest{IntervalS: 30}, time.Minute); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListDevices(ctx, DeviceFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("ListDevices all = %d, %v", len(all), err)
	}
	// Ordered by org name then hostname: Acme/ws-01, Acme/ws-02, Beta/srv-100%.
	if all[0].ID != dev.ID || all[1].ID != dev3.ID || all[2].ID != dev2.ID {
		t.Fatalf("order = %d %d %d", all[0].ID, all[1].ID, all[2].ID)
	}
	if all[0].Status != "online" || all[1].Status != "offline" || all[2].Status != "pending" {
		t.Fatalf("statuses = %s %s %s", all[0].Status, all[1].Status, all[2].Status)
	}
	for _, c := range []struct {
		f    DeviceFilter
		want []int64
	}{
		{DeviceFilter{OrgID: &org.ID}, []int64{dev.ID, dev3.ID}},
		{DeviceFilter{Status: "online"}, []int64{dev.ID}},
		{DeviceFilter{Status: "offline"}, []int64{dev3.ID}},
		{DeviceFilter{Status: "pending"}, []int64{dev2.ID}},
		{DeviceFilter{Query: "WS-0"}, []int64{dev.ID, dev3.ID}},
		{DeviceFilter{Query: "100%"}, []int64{dev2.ID}},
		{DeviceFilter{Query: "_"}, nil},
		{DeviceFilter{OrgID: &org.ID, Status: "offline", Query: "ws"}, []int64{dev3.ID}},
	} {
		got, err := s.ListDevices(ctx, c.f)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, d := range got {
			ids = append(ids, d.ID)
		}
		if len(ids) != len(c.want) {
			t.Fatalf("filter %+v: got %v want %v", c.f, ids, c.want)
		}
		for i := range ids {
			if ids[i] != c.want[i] {
				t.Fatalf("filter %+v: got %v want %v", c.f, ids, c.want)
			}
		}
	}

	// Delete cascades to heartbeats, commands and alerts.
	_, u, _, _ := org, User{}, dev, ""
	u, _ = s.GetUserByEmail(ctx, "admin@example.com")
	if _, err := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: "ping"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAlert(ctx, org.ID, dev.ID, "disk_full", "medium", "92%"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(ctx, dev.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(ctx, dev.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	for _, tbl := range []string{"heartbeats", "commands", "alerts"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+tbl+` WHERE device_id = ?`, dev.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s rows after delete = %d, %v", tbl, n, err)
		}
	}
	if _, err := s.GetDevice(ctx, dev.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDevice after delete = %v", err)
	}
}

func TestCommandsLifecycle(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)

	exec, err := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID,
		Kind: "exec", Argv: []string{"echo", "a;id"}, TimeoutS: 0})
	if err != nil {
		t.Fatal(err)
	}
	if exec.Status != "queued" || exec.TimeoutS != 60 || exec.CreatedByEmail != "admin@example.com" ||
		len(exec.Argv) != 2 || exec.Argv[1] != "a;id" || exec.SentAt != nil || exec.ExitCode != nil {
		t.Fatalf("exec = %+v", exec)
	}
	script, err := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID,
		Kind: "script", Shell: "sh", Body: "df -h\n", TimeoutS: 120})
	if err != nil {
		t.Fatal(err)
	}
	if script.Argv == nil || len(script.Argv) != 0 || script.Shell != "sh" || script.Body != "df -h\n" {
		t.Fatalf("script = %+v", script)
	}
	var sha string
	if err := s.db.QueryRow(`SELECT body_sha256 FROM commands WHERE id = ?`, script.ID).Scan(&sha); err != nil || len(sha) != 64 {
		t.Fatalf("body_sha256 = %q, %v", sha, err)
	}
	msg, err := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID,
		Kind: "message", Text: "Please call the helpdesk", TimeoutS: 30})
	if err != nil || msg.Text != "Please call the helpdesk" {
		t.Fatalf("message = %+v, %v", msg, err)
	}
	lock, err := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: "lock", TimeoutS: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: "reboot"}); err == nil {
		t.Fatal("unknown kind accepted")
	}

	more, _ := s.HasQueuedCommands(ctx, dev.ID)
	if !more {
		t.Fatal("HasQueuedCommands = false")
	}

	// Claim 2 of 4, oldest first, in wire form.
	claimed, err := s.ClaimQueuedCommands(ctx, dev.ID, 2)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if claimed[0].ID != exec.ID || claimed[0].Kind != "exec" || claimed[0].Argv[0] != "echo" || claimed[0].TimeoutS != 60 ||
		claimed[1].ID != script.ID || claimed[1].Shell != "sh" || claimed[1].Body != "df -h\n" || claimed[1].Argv != nil {
		t.Fatalf("claimed = %+v", claimed)
	}
	wire, _ := json.Marshal(claimed[1])
	if strings.Contains(string(wire), `"argv"`) || strings.Contains(string(wire), `"text"`) {
		t.Fatalf("script wire form carries foreign keys: %s", wire)
	}
	more, _ = s.HasQueuedCommands(ctx, dev.ID)
	if !more {
		t.Fatal("HasQueuedCommands after partial claim = false")
	}
	rest, err := s.ClaimQueuedCommands(ctx, dev.ID, 10)
	if err != nil || len(rest) != 2 || rest[0].ID != msg.ID || rest[0].Text != "Please call the helpdesk" || rest[1].ID != lock.ID {
		t.Fatalf("rest = %+v, %v", rest, err)
	}
	wire, _ = json.Marshal(rest[1])
	if string(wire) != `{"id":`+itoa(lock.ID)+`,"kind":"lock","timeout_s":30}` {
		t.Fatalf("lock wire = %s", wire)
	}
	more, _ = s.HasQueuedCommands(ctx, dev.ID)
	if more {
		t.Fatal("HasQueuedCommands after full claim = true")
	}
	if again, _ := s.ClaimQueuedCommands(ctx, dev.ID, 10); len(again) != 0 || again == nil {
		t.Fatalf("claim on empty queue = %v", again)
	}
	got, _ := s.GetCommand(ctx, exec.ID)
	if got.Status != "sent" || got.SentAt == nil {
		t.Fatalf("after claim = %+v", got)
	}

	// Result state machine.
	start := c.now()
	res := protocol.CommandResult{ExitCode: 0, Stdout: "a;id\n", StartedAt: start, FinishedAt: start.Add(1340 * time.Millisecond)}
	if _, err := s.CompleteCommand(ctx, exec.ID, dev.ID+1, res); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong device err = %v", err)
	}
	if _, err := s.CompleteCommand(ctx, 9999, dev.ID, res); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing command err = %v", err)
	}
	st, err := s.CompleteCommand(ctx, exec.ID, dev.ID, res)
	if err != nil || st != "done" {
		t.Fatalf("complete = %q, %v", st, err)
	}
	if _, err := s.CompleteCommand(ctx, exec.ID, dev.ID, res); !errors.Is(err, ErrConflict) {
		t.Fatalf("second completion err = %v", err)
	}
	got, _ = s.GetCommand(ctx, exec.ID)
	if got.Status != "done" || got.ExitCode == nil || *got.ExitCode != 0 || got.Stdout != "a;id\n" ||
		got.StartedAt == nil || got.FinishedAt == nil || !got.FinishedAt.Equal(start.Add(1340*time.Millisecond)) {
		t.Fatalf("done command = %+v (timestamps must keep milliseconds)", got)
	}
	st, _ = s.CompleteCommand(ctx, script.ID, dev.ID, protocol.CommandResult{ExitCode: -1, Error: "timeout after 120s"})
	if st != "timeout" {
		t.Fatalf("timeout derivation = %q", st)
	}
	st, _ = s.CompleteCommand(ctx, msg.ID, dev.ID, protocol.CommandResult{ExitCode: 2, Stderr: "boom"})
	if st != "failed" {
		t.Fatalf("failed derivation (exit) = %q", st)
	}
	st, _ = s.CompleteCommand(ctx, lock.ID, dev.ID, protocol.CommandResult{ExitCode: 0, Error: "spawn: no such file"})
	if st != "failed" {
		t.Fatalf("failed derivation (error) = %q", st)
	}
	// A queued (never sent) command cannot be completed.
	q, _ := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: "ping"})
	if _, err := s.CompleteCommand(ctx, q.ID, dev.ID, res); !errors.Is(err, ErrConflict) {
		t.Fatalf("complete queued err = %v", err)
	}

	list, err := s.ListCommands(ctx, dev.ID, 3)
	if err != nil || len(list) != 3 || list[0].ID != q.ID || list[1].ID != lock.ID {
		t.Fatalf("ListCommands = %+v, %v", list, err)
	}
	if _, err := s.GetCommand(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetCommand missing = %v", err)
	}

	// JSON shape of the dashboard COMMAND object.
	b, _ := json.Marshal(got)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "device_id", "org_id", "created_by", "created_by_email", "kind", "argv", "shell", "body", "text",
		"timeout_s", "status", "created_at", "sent_at", "started_at", "finished_at", "exit_code", "stdout", "stderr", "truncated", "error"} {
		if _, ok := m[k]; !ok {
			t.Errorf("COMMAND JSON lacks %q: %s", k, b)
		}
	}
	if len(m) != 21 {
		t.Errorf("COMMAND JSON has %d keys: %s", len(m), b)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestClaimQueuedCommandsConcurrent(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)
	const total = 40
	for i := 0; i < total; i++ {
		if _, err := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: "ping"}); err != nil {
			t.Fatal(err)
		}
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[int64]int{}
	)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				cmds, err := s.ClaimQueuedCommands(ctx, dev.ID, 3)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(cmds) == 0 {
					return
				}
				mu.Lock()
				for _, c := range cmds {
					seen[c.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("claimed %d distinct commands, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("command %d handed out %d times", id, n)
		}
	}
	var sent int
	s.db.QueryRow(`SELECT COUNT(*) FROM commands WHERE status = 'sent'`).Scan(&sent)
	if sent != total {
		t.Fatalf("sent rows = %d", sent)
	}
}

func TestMarkLostCommands(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)

	short, _ := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: "ping", TimeoutS: 30})
	long, _ := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: "ping", TimeoutS: 600})
	queued, _ := s.CreateCommand(ctx, CreateCommandParams{DeviceID: dev.ID, OrgID: org.ID, CreatedBy: u.ID, Kind: "ping", TimeoutS: 1})
	if _, err := s.ClaimQueuedCommands(ctx, dev.ID, 2); err != nil {
		t.Fatal(err)
	}
	grace := 5 * time.Minute

	lost, err := s.MarkLostCommands(ctx, grace)
	if err != nil || len(lost) != 0 {
		t.Fatalf("nothing should be lost yet: %+v, %v", lost, err)
	}
	// 30 s + 5 min exactly is not yet past; one second more is.
	c.advance(30*time.Second + grace)
	if lost, _ = s.MarkLostCommands(ctx, grace); len(lost) != 0 {
		t.Fatalf("lost at the exact cutoff: %+v", lost)
	}
	c.advance(time.Second)
	lost, err = s.MarkLostCommands(ctx, grace)
	if err != nil || len(lost) != 1 || lost[0].ID != short.ID || lost[0].Status != "lost" ||
		lost[0].Error != "lost: no result from agent" || lost[0].FinishedAt == nil {
		t.Fatalf("lost = %+v, %v", lost, err)
	}
	if got, _ := s.GetCommand(ctx, long.ID); got.Status != "sent" {
		t.Fatalf("long command touched: %+v", got)
	}
	if got, _ := s.GetCommand(ctx, queued.ID); got.Status != "queued" {
		t.Fatalf("queued command touched: %+v", got)
	}
	// A late result for a lost command is a conflict.
	if _, err := s.CompleteCommand(ctx, short.ID, dev.ID, protocol.CommandResult{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("late result err = %v", err)
	}
	c.advance(time.Hour)
	if lost, _ = s.MarkLostCommands(ctx, grace); len(lost) != 1 || lost[0].ID != long.ID {
		t.Fatalf("second sweep = %+v", lost)
	}
}

func TestAlerts(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)

	created, err := s.OpenAlert(ctx, org.ID, dev.ID, "offline", "high", "no sync for 6m")
	if err != nil || !created {
		t.Fatalf("OpenAlert = %v, %v", created, err)
	}
	created, err = s.OpenAlert(ctx, org.ID, dev.ID, "offline", "high", "no sync for 7m")
	if err != nil || created {
		t.Fatalf("second OpenAlert = %v, %v (want idempotent)", created, err)
	}
	if created, _ = s.OpenAlert(ctx, org.ID, dev.ID, "disk_full", "medium", "92%"); !created {
		t.Fatal("different type should open")
	}
	if n, _ := s.CountOpenAlerts(ctx); n != 2 {
		t.Fatalf("CountOpenAlerts = %d", n)
	}
	open, err := s.ListAlerts(ctx, true, nil, nil, 0)
	if err != nil || len(open) != 2 || open[0].Type != "disk_full" || open[1].Type != "offline" {
		t.Fatalf("ListAlerts open = %+v, %v", open, err)
	}
	if a := open[1]; a.OrgName != "Acme Dental" || a.Hostname != "ws-01" || a.Severity != "high" || a.Message != "no sync for 6m" ||
		a.ResolvedAt != nil || a.AcknowledgedBy != nil || a.AcknowledgedAt != nil {
		t.Fatalf("alert = %+v", a)
	}
	if err := s.AckAlert(ctx, open[1].ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AckAlert(ctx, 999, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ack missing = %v", err)
	}
	c.advance(time.Minute)
	if err := s.ResolveAlert(ctx, dev.ID, "offline"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveAlert(ctx, dev.ID, "offline"); err != nil {
		t.Fatalf("resolving twice: %v", err)
	}
	if err := s.AckAlert(ctx, open[1].ID, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ack resolved = %v", err)
	}
	if created, _ = s.OpenAlert(ctx, org.ID, dev.ID, "offline", "high", "again"); !created {
		t.Fatal("OpenAlert after resolve should create a new row")
	}
	all, _ := s.ListAlerts(ctx, false, nil, nil, 0)
	if len(all) != 3 || all[0].Message != "again" || all[2].ResolvedAt == nil || all[2].AcknowledgedBy == nil || *all[2].AcknowledgedBy != u.ID {
		t.Fatalf("ListAlerts all = %+v", all)
	}
	other := org.ID + 1
	if got, _ := s.ListAlerts(ctx, false, &other, nil, 0); len(got) != 0 {
		t.Fatalf("org filter = %+v", got)
	}
	if got, _ := s.ListAlerts(ctx, true, &org.ID, nil, 1); len(got) != 1 {
		t.Fatalf("limit = %+v", got)
	}
	d, _ := s.GetDevice(ctx, dev.ID)
	if d.OpenAlerts != 2 {
		t.Fatalf("device open_alerts = %d", d.OpenAlerts)
	}
	b, _ := json.Marshal(all[0])
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"id", "org_id", "org_name", "device_id", "hostname", "type", "severity", "message", "opened_at", "resolved_at", "acknowledged_by", "acknowledged_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("ALERT JSON lacks %q: %s", k, b)
		}
	}
}

func TestAuditAndSummary(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)

	cmdID := int64(7)
	if err := s.Audit(ctx, AuditEntry{ActorType: "user", ActorID: &u.ID, OrgID: &org.ID, Action: "command.queued",
		TargetType: "command", TargetID: &cmdID, IP: "127.0.0.1", Detail: map[string]any{"kind": "exec", "argv": []string{"uname"}}}); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second)
	if err := s.Audit(ctx, AuditEntry{ActorType: "device", ActorID: &dev.ID, OrgID: &org.ID, Action: "command.finished"}); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Audit(ctx, AuditEntry{At: fixed, ActorType: "system", Action: "command.lost"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, AuditEntry{ActorType: "nobody", Action: "x"}); err == nil {
		t.Fatal("bad actor_type accepted")
	}

	entries, err := s.ListAudit(ctx, 200, nil, nil, 0)
	if err != nil || len(entries) != 3 {
		t.Fatalf("ListAudit = %+v, %v", entries, err)
	}
	sys, devE, usr := entries[0], entries[1], entries[2]
	if sys.ActorLabel != "system" || !sys.At.Equal(fixed) || sys.Detail == nil || len(sys.Detail) != 0 {
		t.Fatalf("system entry = %+v", sys)
	}
	if devE.ActorLabel != "device:"+itoa(dev.ID) || devE.ActorType != "device" {
		t.Fatalf("device entry = %+v", devE)
	}
	if usr.ActorLabel != "admin@example.com" || usr.Detail["kind"] != "exec" || usr.TargetID == nil || *usr.TargetID != 7 || usr.IP != "127.0.0.1" {
		t.Fatalf("user entry = %+v", usr)
	}
	if got, _ := s.ListAudit(ctx, 200, &org.ID, nil, 0); len(got) != 2 {
		t.Fatalf("org filter = %+v", got)
	}
	if got, _ := s.ListAudit(ctx, 200, nil, nil, devE.ID); len(got) != 1 || got[0].ID != usr.ID {
		t.Fatalf("before filter = %+v", got)
	}
	if got, _ := s.ListAudit(ctx, 1, nil, nil, 0); len(got) != 1 || got[0].ID != sys.ID {
		t.Fatalf("limit = %+v", got)
	}
	b, _ := json.Marshal(usr)
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"id", "at", "actor_type", "actor_id", "actor_label", "org_id", "action", "target_type", "target_id", "ip", "detail"} {
		if _, ok := m[k]; !ok {
			t.Errorf("AUDIT JSON lacks %q: %s", k, b)
		}
	}

	// Summary.
	org2, _ := s.CreateOrg(ctx, "Beta")
	d2, _, _ := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org2.ID, DeviceUID: "b", Hostname: "b", Platform: "windows", IntervalS: 30})
	s.RecordSync(ctx, d2.ID, "", protocol.SyncRequest{IntervalS: 30}, time.Minute)
	c.advance(10 * time.Minute)
	s.RecordSync(ctx, dev.ID, "", protocol.SyncRequest{IntervalS: 30}, time.Minute)
	s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org2.ID, DeviceUID: "p", Hostname: "p", Platform: "android"})
	s.OpenAlert(ctx, org2.ID, d2.ID, "offline", "high", "x")
	sum, err := s.Summary(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum != (Summary{Orgs: 2, Devices: 3, Online: 1, Offline: 1, Pending: 1, OpenAlerts: 1}) {
		t.Fatalf("Summary = %+v", sum)
	}
}

func TestDeviceJSONShape(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	_, _, dev, _ := fixture(t, s)
	if err := s.RecordSync(ctx, dev.ID, "10.0.0.12", protocol.SyncRequest{IntervalS: 30, UptimeS: 5}, time.Minute); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, dev.ID)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "org_id", "org_name", "device_uid", "hostname", "platform", "arch", "os_version", "agent_version",
		"interval_s", "status", "enrolled_at", "last_seen_at", "last_ip", "revoked", "cpu_pct", "mem_used_pct", "disk_used_pct",
		"battery_pct", "battery_charging", "uptime_s", "open_alerts", "can_run_commands", "rustdesk_id",
		"pending_patches", "security_patches", "reboot_required", "patches_checked_at"}
	var got []string
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("DEVICE keys = %v\nwant %v", got, want)
	}
	if m["cpu_pct"] != nil || m["battery_pct"] != nil || m["status"] != "online" || m["uptime_s"] != float64(5) || m["rustdesk_id"] != nil {
		t.Fatalf("DEVICE values: %s", b)
	}
	// A device that has never reported patches: zero counts, no reboot, no check.
	if m["pending_patches"] != float64(0) || m["security_patches"] != float64(0) || m["reboot_required"] != false || m["patches_checked_at"] != nil {
		t.Fatalf("DEVICE patch defaults: %s", b)
	}
	if ts, _ := m["last_seen_at"].(string); ts != "2026-11-13T10:00:00Z" {
		t.Fatalf("last_seen_at = %q (want RFC3339 UTC second precision)", ts)
	}
	if strings.Contains(string(b), "facts_json") || strings.Contains(string(b), "last_metrics_json") || strings.Contains(string(b), "patches_json") {
		t.Fatalf("raw JSON columns leaked: %s", b)
	}
}

func TestRustdeskIDStoredFromFacts(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, _, dev, _ := fixture(t, s)
	if dev.RustdeskID != nil {
		t.Fatalf("fresh device must have no rustdesk id: %v", *dev.RustdeskID)
	}
	// A sync without facts, and one with facts but no id, leave it unknown.
	if err := s.RecordSync(ctx, dev.ID, "10.0.0.12", protocol.SyncRequest{IntervalS: 30}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSync(ctx, dev.ID, "10.0.0.12", protocol.SyncRequest{IntervalS: 30, Facts: &protocol.Facts{Kernel: "6.8"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, dev.ID); d.RustdeskID != nil {
		t.Fatalf("rustdesk id after syncs without one: %v", *d.RustdeskID)
	}
	// A sync whose facts carry the id stores it; a later sync without facts keeps it.
	if err := s.RecordSync(ctx, dev.ID, "10.0.0.12", protocol.SyncRequest{IntervalS: 30, Facts: &protocol.Facts{Kernel: "6.8", RustdeskID: "123456789"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSync(ctx, dev.ID, "10.0.0.12", protocol.SyncRequest{IntervalS: 30}, time.Minute); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, dev.ID)
	if d.RustdeskID == nil || *d.RustdeskID != "123456789" {
		t.Fatalf("rustdesk id = %v", d.RustdeskID)
	}
	// Re-enrollment without an id keeps the stored one; with an id replaces it.
	if _, _, err := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org.ID, DeviceUID: dev.DeviceUID, Hostname: dev.Hostname, Platform: dev.Platform}); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.GetDevice(ctx, dev.ID); d.RustdeskID == nil || *d.RustdeskID != "123456789" {
		t.Fatalf("rustdesk id after re-enroll without id = %v", d.RustdeskID)
	}
	if _, _, err := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org.ID, DeviceUID: dev.DeviceUID, Hostname: dev.Hostname, Platform: dev.Platform, RustdeskID: "987654321"}); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.GetDevice(ctx, dev.ID); d.RustdeskID == nil || *d.RustdeskID != "987654321" {
		t.Fatalf("rustdesk id after re-enroll with id = %v", d.RustdeskID)
	}
	// Enrolling a new device with an id stores it at once.
	d2, _, err := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: org.ID, DeviceUID: "uid-rd", Hostname: "ws-rd", Platform: protocol.PlatformWindows, RustdeskID: "111222333"})
	if err != nil {
		t.Fatal(err)
	}
	if d2.RustdeskID == nil || *d2.RustdeskID != "111222333" {
		t.Fatalf("new device rustdesk id = %v", d2.RustdeskID)
	}
}
