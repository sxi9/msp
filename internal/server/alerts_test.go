package server

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

func (e *testEnv) openAlerts(deviceID int64) map[string]store.Alert {
	e.t.Helper()
	alerts, err := e.st.ListAlerts(context.Background(), true, nil, nil, 100)
	if err != nil {
		e.t.Fatalf("ListAlerts: %v", err)
	}
	out := map[string]store.Alert{}
	for _, a := range alerts {
		if a.DeviceID == deviceID {
			out[a.Type] = a
		}
	}
	return out
}

func TestOfflineAlertOpensAndResolves(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("off", protocol.PlatformLinux)

	// Pending device (never synced): nothing opens.
	EvaluateNow(e.srv)
	if a := e.openAlerts(id); len(a) != 0 {
		t.Fatalf("pending device alerts %+v", a)
	}

	if status, _, _ := e.sync(token, syncRequest(0)); status != 200 {
		t.Fatal("sync")
	}
	EvaluateNow(e.srv)
	if a := e.openAlerts(id); len(a) != 0 {
		t.Fatalf("online device alerts %+v", a)
	}

	// 3*30+15 = 105 s window; 2 minutes later it is offline.
	e.clock.advance(2 * time.Minute)
	if d := e.device(id); d.Status != "offline" {
		t.Fatalf("status %q, want offline", d.Status)
	}
	EvaluateNow(e.srv)
	a := e.openAlerts(id)
	off, ok := a["offline"]
	if !ok || off.Severity != "high" || off.Message != "no sync for 2m" || off.OrgID != e.org.ID {
		t.Fatalf("offline alert %+v", a)
	}
	// Idempotent.
	EvaluateNow(e.srv)
	all, _ := e.st.ListAlerts(context.Background(), false, nil, nil, 100)
	if len(all) != 1 {
		t.Fatalf("second evaluation duplicated the alert: %d rows", len(all))
	}

	// Back online -> resolved.
	e.sync(token, syncRequest(0))
	EvaluateNow(e.srv)
	if a := e.openAlerts(id); len(a) != 0 {
		t.Fatalf("alert not resolved: %+v", a)
	}
	all, _ = e.st.ListAlerts(context.Background(), false, nil, nil, 100)
	if len(all) != 1 || all[0].ResolvedAt == nil {
		t.Fatalf("resolved row %+v", all)
	}

	// Revoked devices never alert, and existing alerts are closed.
	e.clock.advance(2 * time.Minute)
	EvaluateNow(e.srv)
	if a := e.openAlerts(id); len(a) != 1 {
		t.Fatalf("expected offline again: %+v", a)
	}
	if err := e.st.RevokeDevice(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	EvaluateNow(e.srv)
	if a := e.openAlerts(id); len(a) != 0 {
		t.Fatalf("revoked device still alerting: %+v", a)
	}
}

func TestThresholdAlerts(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("thr", protocol.PlatformLinux)

	bad := syncRequest(0)
	// 95.6 % memory, 92.5 % on the fullest disk.
	bad.Metrics.MemUsedMB = 15300
	bad.Metrics.Disks = []protocol.Disk{{Mount: "/", TotalGB: 100, UsedGB: 50}, {Mount: "/data", TotalGB: 200, UsedGB: 185}}
	pct, charging := 10, false
	bad.Metrics.BatteryPct = &pct
	bad.Metrics.BatteryCharging = &charging
	if status, _, _ := e.sync(token, bad); status != 200 {
		t.Fatal("sync")
	}
	EvaluateNow(e.srv)
	a := e.openAlerts(id)
	if len(a) != 3 {
		t.Fatalf("alerts %+v", a)
	}
	if a["disk_full"].Severity != "medium" || a["disk_full"].Message != "disk 92.5% used on /data" {
		t.Errorf("disk_full %+v", a["disk_full"])
	}
	if a["memory_high"].Severity != "low" {
		t.Errorf("memory_high %+v", a["memory_high"])
	}
	if a["battery_low"].Severity != "low" || a["battery_low"].Message != "battery at 10%, not charging" {
		t.Errorf("battery_low %+v", a["battery_low"])
	}

	// Charging at 10 % is not an alert; disk and memory back under threshold.
	charging = true
	bad.Metrics.MemUsedMB = 8000
	bad.Metrics.Disks = []protocol.Disk{{Mount: "/", TotalGB: 100, UsedGB: 50}}
	e.clock.advance(2 * time.Second)
	e.sync(token, bad)
	EvaluateNow(e.srv)
	if a := e.openAlerts(id); len(a) != 0 {
		t.Fatalf("alerts should have resolved: %+v", a)
	}

	// Exactly at the thresholds counts.
	edge := syncRequest(0)
	edge.Metrics.MemUsedMB = 15200 // 95.0 %
	edge.Metrics.Disks = []protocol.Disk{{Mount: "/", TotalGB: 100, UsedGB: 90}}
	pct = 15
	charging = false
	edge.Metrics.BatteryPct = &pct
	edge.Metrics.BatteryCharging = &charging
	e.sync(token, edge)
	EvaluateNow(e.srv)
	if a := e.openAlerts(id); len(a) != 3 {
		t.Fatalf("edge alerts %+v", a)
	}
}

// TestDiskAlertIgnoresBootPartition: a 95 % full 1 GB /boot next to a nearly
// empty root must neither open disk_full nor drive disk_used_pct.
func TestDiskAlertIgnoresBootPartition(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("boot", protocol.PlatformLinux)
	req := syncRequest(0)
	req.Metrics.Disks = []protocol.Disk{
		{Mount: "/", TotalGB: 252, UsedGB: 16.3},
		{Mount: "/boot", TotalGB: 1, UsedGB: 0.95},
		{Mount: "/boot/efi", TotalGB: 0.5, UsedGB: 0.1},
		{Mount: "/opt/tiny", TotalGB: 0.4, UsedGB: 0.3},
	}
	if status, _, _ := e.sync(token, req); status != 200 {
		t.Fatal("sync")
	}
	if d := e.device(id); d.DiskUsedPct == nil || *d.DiskUsedPct != 6.5 {
		t.Fatalf("disk_used_pct = %v, want 6.5 (root only)", d.DiskUsedPct)
	}
	EvaluateNow(e.srv)
	if a := e.openAlerts(id); len(a) != 0 {
		t.Fatalf("alerts %+v, want none", a)
	}
}

// TestEvaluatorWritesOnlyOnChange: a steady state costs no alert writes.
func TestEvaluatorWritesOnlyOnChange(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("steady", protocol.PlatformLinux)
	e.sync(token, syncRequest(0))
	e.clock.advance(2 * time.Minute) // offline -> one open alert
	EvaluateNow(e.srv)
	a := e.openAlerts(id)
	if len(a) != 1 {
		t.Fatalf("alerts %+v", a)
	}
	opened := a["offline"].OpenedAt
	// Several passes later the same row is still the only one and untouched.
	for i := 0; i < 3; i++ {
		e.clock.advance(15 * time.Second)
		EvaluateNow(e.srv)
	}
	all, _ := e.st.ListAlerts(context.Background(), false, nil, nil, 100)
	if len(all) != 1 || all[0].ResolvedAt != nil || !all[0].OpenedAt.Equal(opened) {
		t.Fatalf("steady state changed the alert rows: %+v", all)
	}
	open, err := e.st.OpenAlertTypes(context.Background())
	if err != nil || !open[id]["offline"] || len(open) != 1 || len(open[id]) != 1 {
		t.Fatalf("OpenAlertTypes = %v, %v", open, err)
	}
}

func TestLostCommands(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("lost", protocol.PlatformLinux)
	c := e.queue(id, "sleep", "30")
	if _, out, _ := e.sync(token, syncRequest(0)); len(out.Commands) != 1 {
		t.Fatal("not claimed")
	}

	// Before sent_at + timeout (60 s) + grace (1 min in the test config): still sent.
	e.clock.advance(90 * time.Second)
	EvaluateNow(e.srv)
	if got := e.command(c.ID); got.Status != store.StatusSent {
		t.Fatalf("status %q before the grace elapsed", got.Status)
	}
	e.clock.advance(time.Minute)
	EvaluateNow(e.srv)
	got := e.command(c.ID)
	if got.Status != store.StatusLost || got.FinishedAt == nil {
		t.Fatalf("command %+v, want lost", got)
	}
	a := e.auditActions("command.lost")
	if len(a) != 1 || a[0].ActorType != "system" || a[0].ActorLabel != "system" || a[0].TargetID == nil || *a[0].TargetID != c.ID || a[0].OrgID == nil || *a[0].OrgID != e.org.ID {
		t.Fatalf("audit command.lost %+v", a)
	}
	// A late result is refused.
	e.clock.advance(time.Second)
	res := protocol.CommandResult{ExitCode: 0, StartedAt: e.clock.now(), FinishedAt: e.clock.now()}
	if resp, _ := e.do("POST", "/api/agent/v1/commands/"+strconv.FormatInt(c.ID, 10)+"/result", res, bearer(token)); resp.StatusCode != 409 {
		t.Fatalf("late result: %d", resp.StatusCode)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{45 * time.Second: "45s", 6 * time.Minute: "6m", 3 * time.Hour: "3h", 50 * time.Hour: "2d"}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPruneOnce(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("prune", protocol.PlatformLinux)
	e.sync(token, syncRequest(0))
	if _, err := e.st.CreateSession(context.Background(), e.admin.ID, "127.0.0.1", time.Hour); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(8 * 24 * time.Hour)
	e.srv.pruneOnce(context.Background())
	hb, _ := e.st.ListHeartbeats(context.Background(), id, 10)
	if len(hb) != 0 {
		t.Fatalf("heartbeats not pruned: %d", len(hb))
	}
	n, _ := e.st.PruneSessions(context.Background())
	if n != 0 {
		t.Fatalf("sessions left for a second prune: %d", n)
	}
}
