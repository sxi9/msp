package server

import (
	"context"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// TestServiceDownAlert proves the WL1 evaluator: it opens a service_down alert
// (severity medium) for a device with an alert=1 watchlist item whose latest state
// is stopped, resolves it when the service is running again, ignores alert=0 items,
// names every stopped service, and is idempotent. The e2e test exercises the same
// path with the real agent + a fake systemctl.
func TestServiceDownAlert(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	devID, _ := e.enroll("uid-lin", protocol.PlatformLinux)

	item, err := e.st.CreateWatchlistItem(ctx, store.CreateWatchlistItemParams{
		OrgID: &e.org.ID, Scope: "org", Kind: "service", Name: "ssh.service", Alert: true,
	})
	if err != nil {
		t.Fatalf("CreateWatchlistItem: %v", err)
	}
	// A non-alerting item that is also stopped must never open an alert.
	if _, err := e.st.CreateWatchlistItem(ctx, store.CreateWatchlistItemParams{
		OrgID: &e.org.ID, Scope: "org", Kind: "process", Name: "noisy", Alert: false,
	}); err != nil {
		t.Fatalf("CreateWatchlistItem noisy: %v", err)
	}

	// No reported state yet: unknown, not down.
	EvaluateWatchlistsNow(e.srv)
	if a := e.openAlerts(devID); len(a) != 0 {
		t.Fatalf("no-state alerts %+v", a)
	}

	// ssh stopped, noisy stopped: only ssh (alert=1) opens service_down.
	mustRecord(t, e, devID, []protocol.ServiceState{
		{Kind: "service", Name: "ssh.service", Running: false},
		{Kind: "process", Name: "noisy", Running: false},
	})
	EvaluateWatchlistsNow(e.srv)
	a := e.openAlerts(devID)
	sd, ok := a["service_down"]
	if !ok || sd.Severity != severityMedium || sd.Message != "ssh.service is not running" || sd.OrgID != e.org.ID {
		t.Fatalf("service_down alert %+v", a)
	}
	if len(a) != 1 {
		t.Fatalf("expected only service_down, got %+v", a)
	}
	// Idempotent: a second pass opens nothing new.
	EvaluateWatchlistsNow(e.srv)
	all, _ := e.st.ListAlerts(ctx, false, nil, nil, 100)
	if n := countType(all, "service_down"); n != 1 {
		t.Fatalf("service_down rows = %d, want 1", n)
	}

	// ssh running again: resolved.
	mustRecord(t, e, devID, []protocol.ServiceState{{Kind: "service", Name: "ssh.service", Running: true}})
	EvaluateWatchlistsNow(e.srv)
	if a := e.openAlerts(devID); len(a) != 0 {
		t.Fatalf("after resolve %+v", a)
	}

	// Two stopped alerting services: the message names both, sorted.
	if _, err := e.st.CreateWatchlistItem(ctx, store.CreateWatchlistItemParams{
		OrgID: &e.org.ID, Scope: "org", Kind: "service", Name: "nginx.service", Alert: true,
	}); err != nil {
		t.Fatalf("CreateWatchlistItem nginx: %v", err)
	}
	mustRecord(t, e, devID, []protocol.ServiceState{
		{Kind: "service", Name: "ssh.service", Running: false},
		{Kind: "service", Name: "nginx.service", Running: false},
	})
	EvaluateWatchlistsNow(e.srv)
	if sd := e.openAlerts(devID)["service_down"]; sd.Message != "2 watched services not running: nginx.service, ssh.service" {
		t.Fatalf("multi message = %q", sd.Message)
	}

	// Revoking the device resolves the open service_down.
	if err := e.st.RevokeDevice(ctx, devID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	EvaluateWatchlistsNow(e.srv)
	if a := e.openAlerts(devID); len(a) != 0 {
		t.Fatalf("after revoke %+v", a)
	}
	_ = item
}

// TestServiceDownAndroidExcluded proves an android device never opens a
// service_down alert even when an org-scoped alerting item for its org is reported
// stopped (android is excluded from watchlists entirely).
func TestServiceDownAndroidExcluded(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	andID, _ := e.enroll("uid-and", protocol.PlatformAndroid)

	if _, err := e.st.CreateWatchlistItem(ctx, store.CreateWatchlistItemParams{
		OrgID: &e.org.ID, Scope: "org", Kind: "service", Name: "ssh.service", Alert: true,
	}); err != nil {
		t.Fatalf("CreateWatchlistItem: %v", err)
	}
	// Even a (hypothetical) stopped state on the android row must not alert.
	mustRecord(t, e, andID, []protocol.ServiceState{{Kind: "service", Name: "ssh.service", Running: false}})
	EvaluateWatchlistsNow(e.srv)
	if a := e.openAlerts(andID); len(a) != 0 {
		t.Fatalf("android service_down %+v", a)
	}
}

// TestComputeWatchAndRecordSyncWatchData covers the sync-handler glue:
// ComputeWatchForDevice unions + orders the device's items (nil for no watchlist /
// android), and RecordSyncWatchData persists facts.services + the forwarded logs
// while a nil/empty call is a no-op.
func TestComputeWatchAndRecordSyncWatchData(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	devID, _ := e.enroll("uid-lin", protocol.PlatformLinux)

	// No watchlist yet: nil.
	if w := e.srv.ComputeWatchForDevice(ctx, e.device(devID)); w != nil {
		t.Fatalf("empty watch = %+v", w)
	}

	if _, err := e.st.CreateWatchlistItem(ctx, store.CreateWatchlistItemParams{
		OrgID: &e.org.ID, Scope: "org", Kind: "service", Name: "ssh.service", Alert: true,
	}); err != nil {
		t.Fatalf("CreateWatchlistItem org: %v", err)
	}
	id := devID
	if _, err := e.st.CreateWatchlistItem(ctx, store.CreateWatchlistItemParams{
		OrgID: &e.org.ID, Scope: "device", DeviceID: &id, Kind: "process", Name: "backup-agent", Alert: true,
	}); err != nil {
		t.Fatalf("CreateWatchlistItem device: %v", err)
	}
	watch := e.srv.ComputeWatchForDevice(ctx, e.device(devID))
	if len(watch) != 2 ||
		watch[0] != (protocol.WatchItem{Kind: "process", Name: "backup-agent"}) ||
		watch[1] != (protocol.WatchItem{Kind: "service", Name: "ssh.service"}) {
		t.Fatalf("watch = %+v", watch)
	}

	// RecordSyncWatchData stores services + logs.
	facts := &protocol.Facts{Services: []protocol.ServiceState{{Kind: "service", Name: "ssh.service", Running: false}}}
	logs := []protocol.LogEntry{{At: e.clock.now(), Source: protocol.LogSourceJournald, Unit: "ssh.service", Level: "err", Message: "down"}}
	e.srv.RecordSyncWatchData(ctx, e.device(devID), facts, logs)
	if got := e.device(devID).ServicesFor(); len(got) != 1 || got[0].Running {
		t.Fatalf("services after record = %+v", got)
	}
	stored, _ := e.st.ListLogs(ctx, devID, store.LogFilter{}, 0)
	if len(stored) != 1 || stored[0].Message != "down" {
		t.Fatalf("logs after record = %+v", stored)
	}

	// A nil facts + empty logs call leaves both untouched.
	e.srv.RecordSyncWatchData(ctx, e.device(devID), nil, nil)
	if got := e.device(devID).ServicesFor(); len(got) != 1 {
		t.Fatalf("services after no-op = %+v", got)
	}
	stored, _ = e.st.ListLogs(ctx, devID, store.LogFilter{}, 0)
	if len(stored) != 1 {
		t.Fatalf("logs after no-op = %d", len(stored))
	}
}

func mustRecord(t *testing.T, e *testEnv, deviceID int64, items []protocol.ServiceState) {
	t.Helper()
	if err := e.st.RecordServices(context.Background(), deviceID, items, e.clock.now()); err != nil {
		t.Fatalf("RecordServices: %v", err)
	}
}

func countType(alerts []store.Alert, typ string) int {
	n := 0
	for _, a := range alerts {
		if a.Type == typ {
			n++
		}
	}
	return n
}
