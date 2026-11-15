package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// TestWatchlistCRUD proves create/get/list/update/delete of watchlist items, that
// GetWatchlistItem joins org_name/hostname and returns ErrNotFound for a missing
// id, and that ListWatchlistItems is RS2 org-scoped: a caller whose visible set
// excludes an org sees none of its items.
func TestWatchlistCRUD(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	orgA, admin, devA, _ := fixture(t, s)
	orgB, _, devB := fixture2(t, s, "Beta Corp", "uid-2", "ws-02")

	// An org-scoped item on A, a device-scoped item on A's device, and an item on B.
	orgItem, err := s.CreateWatchlistItem(ctx, CreateWatchlistItemParams{
		OrgID: &orgA.ID, Scope: "org", Kind: protocol.WatchKindService, Name: "ssh.service",
		Alert: true, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("CreateWatchlistItem org: %v", err)
	}
	if orgItem.OrgName != "Acme Dental" || orgItem.Scope != "org" || !orgItem.Alert ||
		orgItem.DeviceID != nil || orgItem.Hostname != "" || !orgItem.CreatedAt.Equal(c.now()) {
		t.Fatalf("org item = %+v", orgItem)
	}
	devItem, err := s.CreateWatchlistItem(ctx, CreateWatchlistItemParams{
		OrgID: &orgA.ID, Scope: "device", DeviceID: &devA.ID, Kind: protocol.WatchKindProcess,
		Name: "backup-agent", Alert: false, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("CreateWatchlistItem device: %v", err)
	}
	if devItem.DeviceID == nil || *devItem.DeviceID != devA.ID || devItem.Hostname != "ws-01" || devItem.Alert {
		t.Fatalf("device item = %+v", devItem)
	}
	if _, err := s.CreateWatchlistItem(ctx, CreateWatchlistItemParams{
		OrgID: &orgB.ID, Scope: "device", DeviceID: &devB.ID, Kind: protocol.WatchKindService, Name: "nginx.service", Alert: true,
	}); err != nil {
		t.Fatalf("CreateWatchlistItem B: %v", err)
	}

	// GetWatchlistItem round-trips; a missing id is ErrNotFound.
	got, err := s.GetWatchlistItem(ctx, orgItem.ID)
	if err != nil || got.Name != "ssh.service" {
		t.Fatalf("GetWatchlistItem: %+v err %v", got, err)
	}
	if _, err := s.GetWatchlistItem(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetWatchlistItem missing: %v", err)
	}

	// All orgs: three items.
	all, err := s.ListWatchlistItems(ctx, WatchlistFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("list all: %d err %v", len(all), err)
	}

	// RS2: scoped to A gives only A's two items; scoped to B only B's one; an
	// empty visible set gives none.
	onlyA, err := s.ListWatchlistItems(ctx, WatchlistFilter{VisibleOrgs: []int64{orgA.ID}})
	if err != nil || len(onlyA) != 2 {
		t.Fatalf("scoped A: %d err %v", len(onlyA), err)
	}
	onlyB, err := s.ListWatchlistItems(ctx, WatchlistFilter{VisibleOrgs: []int64{orgB.ID}})
	if err != nil || len(onlyB) != 1 || onlyB[0].OrgID == nil || *onlyB[0].OrgID != orgB.ID {
		t.Fatalf("scoped B: %+v err %v", onlyB, err)
	}
	none, err := s.ListWatchlistItems(ctx, WatchlistFilter{VisibleOrgs: []int64{}})
	if err != nil || len(none) != 0 {
		t.Fatalf("empty visible set: %d err %v", len(none), err)
	}

	// The device filter narrows to one device's device-scoped items.
	devItems, err := s.ListWatchlistItems(ctx, WatchlistFilter{DeviceID: &devA.ID})
	if err != nil || len(devItems) != 1 || devItems[0].ID != devItem.ID {
		t.Fatalf("device filter: %+v err %v", devItems, err)
	}

	// Update the org item's name/kind/alert.
	if err := s.UpdateWatchlistItem(ctx, orgItem.ID, UpdateWatchlistItemParams{
		Kind: protocol.WatchKindService, Name: "sshd.service", Alert: false,
	}); err != nil {
		t.Fatalf("UpdateWatchlistItem: %v", err)
	}
	got, _ = s.GetWatchlistItem(ctx, orgItem.ID)
	if got.Name != "sshd.service" || got.Alert {
		t.Fatalf("after update: %+v", got)
	}
	if err := s.UpdateWatchlistItem(ctx, 99999, UpdateWatchlistItemParams{Kind: "service", Name: "x", Alert: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateWatchlistItem missing: %v", err)
	}

	// Delete.
	if err := s.DeleteWatchlistItem(ctx, orgItem.ID); err != nil {
		t.Fatalf("DeleteWatchlistItem: %v", err)
	}
	if err := s.DeleteWatchlistItem(ctx, orgItem.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteWatchlistItem again: %v", err)
	}
}

// TestWatchlistForDevice proves the sync-response watch list unions a device's
// org-scoped and device-scoped items, deduplicates on (kind, name), and excludes
// Android devices entirely.
func TestWatchlistForDevice(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, _, devA, _ := fixture(t, s)

	// org-scoped ssh.service + a duplicate device-scoped ssh.service (dedup to one)
	// + a device-scoped process; plus an org item in another org that must not leak.
	mustCreate := func(p CreateWatchlistItemParams) {
		if _, err := s.CreateWatchlistItem(ctx, p); err != nil {
			t.Fatalf("CreateWatchlistItem: %v", err)
		}
	}
	mustCreate(CreateWatchlistItemParams{OrgID: &orgA.ID, Scope: "org", Kind: "service", Name: "ssh.service", Alert: true})
	mustCreate(CreateWatchlistItemParams{OrgID: &orgA.ID, Scope: "device", DeviceID: &devA.ID, Kind: "service", Name: "ssh.service", Alert: true})
	mustCreate(CreateWatchlistItemParams{OrgID: &orgA.ID, Scope: "device", DeviceID: &devA.ID, Kind: "process", Name: "backup-agent", Alert: true})
	orgB, _, devB := fixture2(t, s, "Beta Corp", "uid-2", "ws-02")
	mustCreate(CreateWatchlistItemParams{OrgID: &orgB.ID, Scope: "org", Kind: "service", Name: "nginx.service", Alert: true})

	watch, err := s.WatchlistForDevice(ctx, devA)
	if err != nil {
		t.Fatalf("WatchlistForDevice: %v", err)
	}
	// Ordered (kind, name): process/backup-agent, service/ssh.service (deduped).
	if len(watch) != 2 {
		t.Fatalf("watch = %+v", watch)
	}
	if watch[0] != (protocol.WatchItem{Kind: "process", Name: "backup-agent"}) ||
		watch[1] != (protocol.WatchItem{Kind: "service", Name: "ssh.service"}) {
		t.Fatalf("watch order/dedup = %+v", watch)
	}

	// Device B sees only its org's item.
	watchB, err := s.WatchlistForDevice(ctx, devB)
	if err != nil || len(watchB) != 1 || watchB[0].Name != "nginx.service" {
		t.Fatalf("watch B = %+v err %v", watchB, err)
	}

	// Android devices are excluded entirely, even with a matching org item.
	andDev, _, err2 := s.UpsertDevice(ctx, UpsertDeviceParams{
		OrgID: orgA.ID, DeviceUID: "uid-and", Hostname: "phone-1", Platform: "android",
		AgentVersion: "0.1.0", IntervalS: 60,
	})
	if err2 != nil {
		t.Fatalf("UpsertDevice android: %v", err2)
	}
	watchAnd, err := s.WatchlistForDevice(ctx, andDev)
	if err != nil || watchAnd != nil {
		t.Fatalf("android watch = %+v err %v", watchAnd, err)
	}
}

// TestRecordServicesRoundTrip proves RecordServices stores the state list and
// services_checked_at, that the DEVICE read exposes services_checked_at while
// keeping the list out of the DEVICE JSON (ServicesJSON is json:"-"), that a nil
// list round-trips as "[]", and that a missing device is ErrNotFound.
func TestRecordServicesRoundTrip(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	_, _, dev, _ := fixture(t, s)

	items := []protocol.ServiceState{
		{Kind: "service", Name: "ssh.service", Running: true},
		{Kind: "service", Name: "nginx.service", Running: false},
		{Kind: "process", Name: "backup-agent", Running: true},
	}
	if err := s.RecordServices(ctx, dev.ID, items, c.now()); err != nil {
		t.Fatalf("RecordServices: %v", err)
	}
	got, err := s.GetDevice(ctx, dev.ID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if got.ServicesCheckedAt == nil || !got.ServicesCheckedAt.Equal(c.now()) {
		t.Fatalf("services_checked_at = %v, want %v", got.ServicesCheckedAt, c.now())
	}
	stored := got.ServicesFor()
	if len(stored) != 3 || stored[1].Name != "nginx.service" || stored[1].Running {
		t.Fatalf("stored services: %+v", stored)
	}

	// The DEVICE JSON exposes services_checked_at but not the full list.
	b, _ := json.Marshal(got)
	js := string(b)
	if !strings.Contains(js, `"services_checked_at":`) {
		t.Fatalf("DEVICE JSON missing services_checked_at: %s", js)
	}
	if strings.Contains(js, "backup-agent") || strings.Contains(js, "services_json") {
		t.Fatalf("DEVICE JSON must not carry the service list: %s", js)
	}

	// A nil list stores "[]".
	if err := s.RecordServices(ctx, dev.ID, nil, c.now()); err != nil {
		t.Fatalf("RecordServices nil: %v", err)
	}
	got, _ = s.GetDevice(ctx, dev.ID)
	if got.ServicesJSON != "[]" || len(got.ServicesFor()) != 0 {
		t.Fatalf("after nil: json %q", got.ServicesJSON)
	}

	// A device that never reported services reads as the migration default.
	_, _, dev2 := fixture2(t, s, "Beta Corp", "uid-2", "ws-02")
	got2, _ := s.GetDevice(ctx, dev2.ID)
	if got2.ServicesJSON != "[]" || got2.ServicesCheckedAt != nil {
		t.Fatalf("default services: json %q checked %v", got2.ServicesJSON, got2.ServicesCheckedAt)
	}

	// A missing device is ErrNotFound.
	if err := s.RecordServices(ctx, 99999, items, c.now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RecordServices missing device: %v", err)
	}
}

// TestAppendAndListLogs proves AppendLogs persists forwarded lines with the
// device's org, ListLogs returns them newest first with unit/level filtering and
// a limit, and an empty batch is a no-op.
func TestAppendAndListLogs(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	orgA, _, devA, _ := fixture(t, s)

	base := c.now()
	entries := []protocol.LogEntry{
		{At: base, Source: protocol.LogSourceJournald, Unit: "nginx.service", Level: "warning", Message: "worker exited"},
		{At: base.Add(time.Second), Source: protocol.LogSourceJournald, Unit: "sshd.service", Level: "err", Message: "bad protocol id"},
		{At: base.Add(2 * time.Second), Source: protocol.LogSourceJournald, Unit: "nginx.service", Level: "err", Message: "upstream down"},
	}
	if err := s.AppendLogs(ctx, devA.ID, orgA.ID, entries); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}
	// An empty batch is a no-op.
	if err := s.AppendLogs(ctx, devA.ID, orgA.ID, nil); err != nil {
		t.Fatalf("AppendLogs empty: %v", err)
	}

	// Newest first, org carried through.
	all, err := s.ListLogs(ctx, devA.ID, LogFilter{}, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("list all: %d err %v", len(all), err)
	}
	if all[0].Message != "upstream down" || all[2].Message != "worker exited" {
		t.Fatalf("order: %+v", all)
	}
	if all[0].OrgID != orgA.ID {
		t.Fatalf("org_id = %d, want %d", all[0].OrgID, orgA.ID)
	}

	// Unit filter.
	nginx, err := s.ListLogs(ctx, devA.ID, LogFilter{Unit: "nginx.service"}, 0)
	if err != nil || len(nginx) != 2 {
		t.Fatalf("unit filter: %d err %v", len(nginx), err)
	}
	// Level filter.
	errs, err := s.ListLogs(ctx, devA.ID, LogFilter{Level: "err"}, 0)
	if err != nil || len(errs) != 2 {
		t.Fatalf("level filter: %d err %v", len(errs), err)
	}
	// Combined filter + limit.
	one, err := s.ListLogs(ctx, devA.ID, LogFilter{Unit: "nginx.service", Level: "err"}, 1)
	if err != nil || len(one) != 1 || one[0].Message != "upstream down" {
		t.Fatalf("combined: %+v err %v", one, err)
	}
}

// TestAppendLogsCap proves AppendLogs trims device_logs to the newest
// DeviceLogCap rows per device, dropping the oldest.
func TestAppendLogsCap(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	orgA, _, devA, _ := fixture(t, s)

	base := c.now()
	n := DeviceLogCap + 25
	entries := make([]protocol.LogEntry, n)
	for i := range entries {
		entries[i] = protocol.LogEntry{
			At:      base.Add(time.Duration(i) * time.Second),
			Source:  protocol.LogSourceJournald,
			Unit:    "app.service",
			Level:   "warning",
			Message: "line",
		}
	}
	if err := s.AppendLogs(ctx, devA.ID, orgA.ID, entries); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_logs WHERE device_id = ?`, devA.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != DeviceLogCap {
		t.Fatalf("after cap: count = %d, want %d", count, DeviceLogCap)
	}
	// The newest entry survives; the oldest (base) was trimmed.
	newest, err := s.ListLogs(ctx, devA.ID, LogFilter{}, 1)
	if err != nil || len(newest) != 1 || !newest[0].At.Equal(base.Add(time.Duration(n-1)*time.Second)) {
		t.Fatalf("newest survivor: %+v err %v", newest, err)
	}
}

// TestPruneDeviceLogs proves the retention sweep deletes entries older than the
// window and keeps newer ones.
func TestPruneDeviceLogs(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	orgA, _, devA, _ := fixture(t, s)

	old := c.now()
	if err := s.AppendLogs(ctx, devA.ID, orgA.ID, []protocol.LogEntry{
		{At: old, Source: protocol.LogSourceJournald, Unit: "a.service", Level: "warning", Message: "old"},
	}); err != nil {
		t.Fatalf("AppendLogs old: %v", err)
	}
	// Advance the clock 4 days and append a fresh line.
	c.advance(4 * 24 * time.Hour)
	fresh := c.now()
	if err := s.AppendLogs(ctx, devA.ID, orgA.ID, []protocol.LogEntry{
		{At: fresh, Source: protocol.LogSourceJournald, Unit: "a.service", Level: "warning", Message: "fresh"},
	}); err != nil {
		t.Fatalf("AppendLogs fresh: %v", err)
	}

	// Prune older than 3 days: the old line goes, the fresh one stays.
	n, err := s.PruneDeviceLogs(ctx, 3*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneDeviceLogs: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	remaining, err := s.ListLogs(ctx, devA.ID, LogFilter{}, 0)
	if err != nil || len(remaining) != 1 || remaining[0].Message != "fresh" {
		t.Fatalf("remaining: %+v err %v", remaining, err)
	}
}
