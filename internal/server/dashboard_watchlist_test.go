package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

func watchlistPath() string             { return "/api/v1/watchlist" }
func watchlistItemPath(id int64) string { return "/api/v1/watchlist/" + itoa(id) }
func deviceLogsPath(id int64) string    { return "/api/v1/devices/" + itoa(id) + "/logs" }

// decodeWatchItem / decodeWatchItems / decodeLogs unmarshal the typed response
// envelopes the handlers write ({"item":…}, {"items":…}, {"logs":…}).
func decodeWatchItem(t *testing.T, body []byte) store.WatchlistItem {
	t.Helper()
	var env struct {
		Item store.WatchlistItem `json:"item"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode item %s: %v", body, err)
	}
	return env.Item
}

func decodeWatchItems(t *testing.T, body []byte) []store.WatchlistItem {
	t.Helper()
	var env struct {
		Items []store.WatchlistItem `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode items %s: %v", body, err)
	}
	return env.Items
}

func decodeLogs(t *testing.T, body []byte) []store.DeviceLog {
	t.Helper()
	var env struct {
		Logs []store.DeviceLog `json:"logs"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode logs %s: %v", body, err)
	}
	return env.Logs
}

// TestWatchlistCRUD covers create/list/update/delete through the API: the org- and
// device-scoped create paths, the derived org on a device item, the shape guards
// (bad kind, missing/surplus scope fields, android device) and the role/CSRF chain.
func TestWatchlistCRUD(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	devID, _ := e.enroll("uid-lin", protocol.PlatformLinux)
	andID, _ := e.enroll("uid-and", protocol.PlatformAndroid)

	orgItem := map[string]any{"scope": "org", "org_id": e.org.ID, "kind": "service", "name": "ssh.service"}

	// --- session, CSRF and role on the write path -----------------------------
	if resp, _ := e.do(http.MethodPost, watchlistPath(), orgItem, nil); resp.StatusCode != 401 {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	if resp, body := e.do(http.MethodPost, watchlistPath(), orgItem, withCookie(admin, nil)); resp.StatusCode != 403 || errorCode(t, body) != "csrf" {
		t.Fatalf("no csrf: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(http.MethodPost, watchlistPath(), orgItem, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 || errorCode(t, body) != "forbidden" {
		t.Fatalf("viewer write: %d %s", resp.StatusCode, body)
	}

	// --- create org-scoped (alert defaults to true) ---------------------------
	resp, body := e.do(http.MethodPost, watchlistPath(), orgItem, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create org item: %d %s", resp.StatusCode, body)
	}
	org := decodeWatchItem(t, body)
	if org.Scope != "org" || org.OrgID == nil || *org.OrgID != e.org.ID || org.DeviceID != nil ||
		org.Kind != "service" || org.Name != "ssh.service" || !org.Alert {
		t.Fatalf("org item = %+v", org)
	}

	// --- create device-scoped (org derived from the device, alert=false) ------
	devBody := map[string]any{"scope": "device", "device_id": devID, "kind": "process", "name": "backup-agent", "alert": false}
	resp, body = e.do(http.MethodPost, watchlistPath(), devBody, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create device item: %d %s", resp.StatusCode, body)
	}
	dev := decodeWatchItem(t, body)
	if dev.Scope != "device" || dev.DeviceID == nil || *dev.DeviceID != devID ||
		dev.OrgID == nil || *dev.OrgID != e.org.ID || dev.Kind != "process" || dev.Alert {
		t.Fatalf("device item = %+v", dev)
	}

	// --- list: both items ------------------------------------------------------
	resp, body = e.do(http.MethodGet, watchlistPath(), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	if items := decodeWatchItems(t, body); len(items) != 2 {
		t.Fatalf("list count = %d", len(items))
	}
	// ?device_id= narrows to the device item.
	resp, body = e.do(http.MethodGet, watchlistPath()+"?device_id="+itoa(devID), nil, withCookie(admin, nil))
	if items := decodeWatchItems(t, body); resp.StatusCode != 200 || len(items) != 1 || items[0].ID != dev.ID {
		t.Fatalf("list by device: %d %s", resp.StatusCode, body)
	}

	// --- update kind/name/alert ------------------------------------------------
	upd := map[string]any{"kind": "service", "name": "sshd.service", "alert": false}
	resp, body = e.do(http.MethodPut, watchlistItemPath(org.ID), upd, withCookie(admin, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("update: %d %s", resp.StatusCode, body)
	}
	if got := decodeWatchItem(t, body); got.Name != "sshd.service" || got.Alert {
		t.Fatalf("after update: %+v", got)
	}
	// Omitting alert on PUT keeps the stored flag (now false).
	resp, body = e.do(http.MethodPut, watchlistItemPath(org.ID), map[string]any{"kind": "service", "name": "sshd.service"}, withCookie(admin, csrfHeader))
	if got := decodeWatchItem(t, body); resp.StatusCode != 200 || got.Alert {
		t.Fatalf("update keep alert: %d %+v", resp.StatusCode, got)
	}

	// --- delete ---------------------------------------------------------------
	if resp, body := e.do(http.MethodDelete, watchlistItemPath(org.ID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("delete: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(http.MethodDelete, watchlistItemPath(org.ID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("delete again: %d %s", resp.StatusCode, body)
	}

	// --- shape guards ----------------------------------------------------------
	bad := []struct {
		name string
		body map[string]any
		code string
	}{
		{"bad kind", map[string]any{"scope": "org", "org_id": e.org.ID, "kind": "widget", "name": "x"}, "invalid_request"},
		{"empty name", map[string]any{"scope": "org", "org_id": e.org.ID, "kind": "service", "name": ""}, "invalid_request"},
		{"org without org_id", map[string]any{"scope": "org", "kind": "service", "name": "x"}, "invalid_request"},
		{"org with device_id", map[string]any{"scope": "org", "org_id": e.org.ID, "device_id": devID, "kind": "service", "name": "x"}, "invalid_request"},
		{"device without device_id", map[string]any{"scope": "device", "kind": "service", "name": "x"}, "invalid_request"},
		{"device with org_id", map[string]any{"scope": "device", "device_id": devID, "org_id": e.org.ID, "kind": "service", "name": "x"}, "invalid_request"},
		{"bad scope", map[string]any{"scope": "fleet", "kind": "service", "name": "x"}, "invalid_request"},
		{"android device", map[string]any{"scope": "device", "device_id": andID, "kind": "service", "name": "x"}, "unsupported_on_platform"},
	}
	for _, tc := range bad {
		resp, body := e.do(http.MethodPost, watchlistPath(), tc.body, withCookie(admin, csrfHeader))
		if resp.StatusCode == 201 || errorCode(t, body) != tc.code {
			t.Fatalf("%s: %d %s (want %s)", tc.name, resp.StatusCode, body, tc.code)
		}
	}
}

// TestWatchlistRS2 is the WL1 scoping proof: an operator scoped to org A cannot
// see, create, update, delete or otherwise probe another org's watchlist, and
// cannot read another device's logs — every cross-org access is 404 not_found with
// no side effect.
func TestWatchlistRS2(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	orgA := e.org
	devA, _ := e.enroll("uid-a", protocol.PlatformLinux)

	// Org B with its own device and a watchlist item + a forwarded log line.
	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	devB, _, err := e.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("UpsertDevice B: %v", err)
	}
	itemB, err := e.st.CreateWatchlistItem(ctx, store.CreateWatchlistItemParams{
		OrgID: &orgB.ID, Scope: "org", Kind: "service", Name: "nginx.service", Alert: true,
	})
	if err != nil {
		t.Fatalf("CreateWatchlistItem B: %v", err)
	}
	if err := e.st.AppendLogs(ctx, devB.ID, orgB.ID, []protocol.LogEntry{
		{At: e.clock.now(), Source: protocol.LogSourceJournald, Unit: "nginx.service", Level: "err", Message: "secret"},
	}); err != nil {
		t.Fatalf("AppendLogs B: %v", err)
	}
	// An item in A so the scoped list is non-empty (and proves it sees only A).
	itemA, err := e.st.CreateWatchlistItem(ctx, store.CreateWatchlistItemParams{
		OrgID: &orgA.ID, Scope: "org", Kind: "service", Name: "ssh.service", Alert: true,
	})
	if err != nil {
		t.Fatalf("CreateWatchlistItem A: %v", err)
	}

	op, _ := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{orgA.ID})

	// List: only A's item, never B's.
	resp, body := e.do(http.MethodGet, watchlistPath(), nil, withCookie(op, nil))
	items := decodeWatchItems(t, body)
	if resp.StatusCode != 200 || len(items) != 1 || items[0].ID != itemA.ID {
		t.Fatalf("scoped list = %d %s", resp.StatusCode, body)
	}
	// ?org_id=B and ?device_id=B's device are 404 (indistinguishable from absent).
	if resp, _ := e.do(http.MethodGet, watchlistPath()+"?org_id="+itoa(orgB.ID), nil, withCookie(op, nil)); resp.StatusCode != 404 {
		t.Fatalf("scoped list org B: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodGet, watchlistPath()+"?device_id="+itoa(devB.ID), nil, withCookie(op, nil)); resp.StatusCode != 404 {
		t.Fatalf("scoped list device B: %d", resp.StatusCode)
	}

	// Create in B (org-scoped and device-scoped) is 404, with no row written.
	before, _ := e.st.ListWatchlistItems(ctx, store.WatchlistFilter{OrgID: &orgB.ID})
	for _, bad := range []map[string]any{
		{"scope": "org", "org_id": orgB.ID, "kind": "service", "name": "x"},
		{"scope": "device", "device_id": devB.ID, "kind": "service", "name": "x"},
	} {
		if resp, body := e.do(http.MethodPost, watchlistPath(), bad, withCookie(op, csrfHeader)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
			t.Fatalf("scoped create in B %v: %d %s", bad, resp.StatusCode, body)
		}
	}
	after, _ := e.st.ListWatchlistItems(ctx, store.WatchlistFilter{OrgID: &orgB.ID})
	if len(after) != len(before) {
		t.Fatalf("scoped create wrote to B: %d -> %d", len(before), len(after))
	}

	// Update/delete B's item is 404 (no change), as is an unknown id.
	if resp, _ := e.do(http.MethodPut, watchlistItemPath(itemB.ID), map[string]any{"kind": "service", "name": "z"}, withCookie(op, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("scoped update B item: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodDelete, watchlistItemPath(itemB.ID), nil, withCookie(op, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("scoped delete B item: %d", resp.StatusCode)
	}
	if got, _ := e.st.GetWatchlistItem(ctx, itemB.ID); got.Name != "nginx.service" {
		t.Fatalf("B item mutated: %+v", got)
	}

	// Logs: A readable, B's device 404.
	if resp, _ := e.do(http.MethodGet, deviceLogsPath(devA), nil, withCookie(op, nil)); resp.StatusCode != 200 {
		t.Fatalf("scoped logs A: %d", resp.StatusCode)
	}
	if resp, body := e.do(http.MethodGet, deviceLogsPath(devB.ID), nil, withCookie(op, nil)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("scoped logs B: %d %s", resp.StatusCode, body)
	}
}

// TestDeviceLogsEndpoint proves the forwarded-log read returns a device's lines
// newest first with unit/level filtering and a limit (RS2 device visibility is in
// TestWatchlistRS2).
func TestDeviceLogsEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := dashAdmin(e)
	devID, _ := e.enroll("uid-lin", protocol.PlatformLinux)

	base := e.clock.now()
	if err := e.st.AppendLogs(ctx, devID, e.org.ID, []protocol.LogEntry{
		{At: base, Source: protocol.LogSourceJournald, Unit: "nginx.service", Level: "warning", Message: "worker exited"},
		{At: base.Add(time.Second), Source: protocol.LogSourceJournald, Unit: "sshd.service", Level: "err", Message: "bad id"},
		{At: base.Add(2 * time.Second), Source: protocol.LogSourceJournald, Unit: "nginx.service", Level: "err", Message: "upstream down"},
	}); err != nil {
		t.Fatalf("AppendLogs: %v", err)
	}

	// All, newest first.
	resp, body := e.do(http.MethodGet, deviceLogsPath(devID), nil, withCookie(admin, nil))
	logs := decodeLogs(t, body)
	if resp.StatusCode != 200 || len(logs) != 3 || logs[0].Message != "upstream down" {
		t.Fatalf("all logs: %d %s", resp.StatusCode, body)
	}
	// Unit filter.
	if logs := decodeLogs(t, mustGet(e, admin, deviceLogsPath(devID)+"?unit=nginx.service")); len(logs) != 2 {
		t.Fatalf("unit filter: %d", len(logs))
	}
	// Level filter.
	if logs := decodeLogs(t, mustGet(e, admin, deviceLogsPath(devID)+"?level=err")); len(logs) != 2 {
		t.Fatalf("level filter: %d", len(logs))
	}
	// Combined + limit.
	logs = decodeLogs(t, mustGet(e, admin, deviceLogsPath(devID)+"?unit=nginx.service&level=err&limit=1"))
	if len(logs) != 1 || logs[0].Message != "upstream down" {
		t.Fatalf("combined: %+v", logs)
	}
	// Unknown device is 404.
	if resp, _ := e.do(http.MethodGet, deviceLogsPath(999999), nil, withCookie(admin, nil)); resp.StatusCode != 404 {
		t.Fatalf("unknown device logs: %d", resp.StatusCode)
	}
}

func mustGet(e *testEnv, cookie *http.Cookie, path string) []byte {
	e.t.Helper()
	resp, body := e.do(http.MethodGet, path, nil, withCookie(cookie, nil))
	if resp.StatusCode != 200 {
		e.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	return body
}
