package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// seedSeries brings a device online and records sampleCount heartbeats one
// minute apart (the test env's SampleEvery is 1s, so each sync stores a row).
func (e *testEnv) seedSeries(token string, sampleCount int) {
	e.t.Helper()
	for i := 0; i < sampleCount; i++ {
		if status, _, raw := e.sync(token, syncRequest(0)); status != 200 {
			e.t.Fatalf("seed sync: %d %s", status, raw)
		}
		e.clock.advance(time.Minute)
	}
}

// TestReportAuthAndRoles proves the report routes sit behind the session chain
// and that a viewer (read-only) may read them.
func TestReportAuthAndRoles(t *testing.T) {
	e := newTestEnv(t)
	_, token := e.enroll("uid-1", protocol.PlatformLinux)
	e.seedSeries(token, 3)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	paths := []string{
		"/api/v1/reports/uptime",
		"/api/v1/reports/disk/top",
		"/api/v1/reports/patch-compliance",
	}
	for _, p := range paths {
		if resp, body := e.do(http.MethodGet, p, nil, nil); resp.StatusCode != 401 {
			t.Fatalf("%s no session: %d %s", p, resp.StatusCode, body)
		}
		if resp, body := e.do(http.MethodGet, p, nil, withCookie(viewer, nil)); resp.StatusCode != 200 {
			t.Fatalf("%s viewer: %d %s", p, resp.StatusCode, body)
		}
	}
}

// TestReportUptimeShapeAndCSV checks the uptime JSON shape, that a seeded device
// appears, and that the CSV export is well-formed with the attachment header.
func TestReportUptimeShapeAndCSV(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	e.seedSeries(token, 5)

	resp, body := e.do(http.MethodGet, "/api/v1/reports/uptime?days=1", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("uptime: %d %s", resp.StatusCode, body)
	}
	var rep store.UptimeReportResult
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("decode uptime: %v", err)
	}
	if rep.Days != 1 {
		t.Fatalf("days = %d", rep.Days)
	}
	var found bool
	for _, d := range rep.Devices {
		if d.DeviceID == devID {
			found = true
			if d.Samples < 1 {
				t.Fatalf("seeded device has no samples: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("seeded device missing from uptime report: %+v", rep.Devices)
	}

	// CSV: attachment header, text/csv, parses cleanly, header row matches.
	resp, body = e.do(http.MethodGet, "/api/v1/reports/uptime.csv?days=1", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("uptime.csv: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %q", ct)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "msp-uptime-") || !strings.Contains(cd, ".csv") {
		t.Fatalf("content-disposition = %q", cd)
	}
	recs := parseCSV(t, body)
	if len(recs) < 2 {
		t.Fatalf("csv rows = %d, want header + data", len(recs))
	}
	wantHeader := []string{"device_id", "hostname", "org_id", "org_name", "samples", "expected", "uptime_pct", "os_uptime_s"}
	if strings.Join(recs[0], ",") != strings.Join(wantHeader, ",") {
		t.Fatalf("csv header = %v", recs[0])
	}
}

// TestReportDiskShapeAndCSV checks the single-device disk series JSON + CSV and
// the device_id validation.
func TestReportDiskShapeAndCSV(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	e.seedSeries(token, 4)

	// Missing device_id -> 400.
	if resp, body := e.do(http.MethodGet, "/api/v1/reports/disk", nil, withCookie(admin, nil)); resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("disk no device_id: %d %s", resp.StatusCode, body)
	}
	// Unknown device -> 404.
	if resp, _ := e.do(http.MethodGet, "/api/v1/reports/disk?device_id=999999", nil, withCookie(admin, nil)); resp.StatusCode != 404 {
		t.Fatalf("disk unknown device: %d", resp.StatusCode)
	}

	resp, body := e.do(http.MethodGet, "/api/v1/reports/disk?device_id="+itoa(devID)+"&days=1", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("disk: %d %s", resp.StatusCode, body)
	}
	var rep store.DiskSeriesResult
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("decode disk: %v", err)
	}
	if rep.DeviceID != devID || len(rep.Points) < 1 || rep.Latest == nil {
		t.Fatalf("disk series = %+v", rep)
	}

	resp, body = e.do(http.MethodGet, "/api/v1/reports/disk.csv?device_id="+itoa(devID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("disk.csv: %d %s", resp.StatusCode, body)
	}
	recs := parseCSV(t, body)
	if len(recs) < 2 || strings.Join(recs[0], ",") != "at,disk_used_pct" {
		t.Fatalf("disk csv = %v", recs)
	}
}

// TestReportPatchComplianceAndInjection proves the patch-compliance buckets over
// the API and the CSV injection guard: an org named "=evil" must appear with a
// leading-quote guard in the CSV so a spreadsheet will not evaluate it.
func TestReportPatchComplianceAndInjection(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := dashAdmin(e)

	// A device in an org whose name begins with '=' (formula-injection bait).
	evil, err := e.st.CreateOrg(ctx, "=cmd|calc")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	dev, _, err := e.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: evil.ID, DeviceUID: "uid-evil", Hostname: "=HOST", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	if err := e.st.RecordPatches(ctx, dev.ID, protocol.PatchReport{
		CheckedAt: e.clock.now(), Manager: protocol.PatchManagerApt,
		Updates: []protocol.PatchUpdate{{Name: "openssl", Security: true}},
	}); err != nil {
		t.Fatalf("RecordPatches: %v", err)
	}

	resp, body := e.do(http.MethodGet, "/api/v1/reports/patch-compliance", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("patch-compliance: %d %s", resp.StatusCode, body)
	}
	var rep store.PatchComplianceResult
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.Buckets.SecurityPending < 1 || rep.Buckets.Pending < 1 {
		t.Fatalf("buckets = %+v, want security+pending >= 1", rep.Buckets)
	}

	resp, body = e.do(http.MethodGet, "/api/v1/reports/patch-compliance.csv", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("patch csv: %d %s", resp.StatusCode, body)
	}
	recs := parseCSV(t, body)
	// Find the row for our evil org and assert the guard prefix on both text cells.
	var guarded bool
	for _, row := range recs {
		for _, cell := range row {
			if cell == "'=cmd|calc" {
				guarded = true
			}
			// The raw dangerous value must never appear unguarded.
			if cell == "=cmd|calc" || cell == "=HOST" {
				t.Fatalf("unguarded injection cell in CSV: %q (row %v)", cell, row)
			}
		}
	}
	if !guarded {
		t.Fatalf("org name guard ('=cmd|calc) not found in CSV: %s", body)
	}
	if !strings.Contains(string(body), "'=HOST") {
		t.Fatalf("hostname guard ('=HOST) not found in CSV: %s", body)
	}
}

// TestReportDaysRejected proves a days beyond the retention window is a 400 on
// every windowed report + CSV, and a non-numeric days is a 400.
func TestReportDaysRejected(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	e.seedSeries(token, 2)
	// DefaultConfig retention is 7 days.
	tooLong := "days=8"
	paths := []string{
		"/api/v1/reports/uptime?" + tooLong,
		"/api/v1/reports/uptime.csv?" + tooLong,
		"/api/v1/reports/disk?device_id=" + itoa(devID) + "&" + tooLong,
		"/api/v1/reports/disk.csv?device_id=" + itoa(devID) + "&" + tooLong,
		"/api/v1/reports/disk/top?" + tooLong,
	}
	for _, p := range paths {
		if resp, body := e.do(http.MethodGet, p, nil, withCookie(admin, nil)); resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
			t.Fatalf("%s days too long: %d %s", p, resp.StatusCode, body)
		}
	}
	// Non-numeric days.
	if resp, body := e.do(http.MethodGet, "/api/v1/reports/uptime?days=abc", nil, withCookie(admin, nil)); resp.StatusCode != 400 {
		t.Fatalf("bad days: %d %s", resp.StatusCode, body)
	}
}

// TestReportRS2Scoping proves a scoped operator sees only its org's rows and gets
// 404 (not 403) on an explicit org_id/device_id in another org.
func TestReportRS2Scoping(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	orgA := e.org

	// Org A device with a series.
	devA, tokenA := e.enroll("uid-a", protocol.PlatformLinux)
	e.seedSeries(tokenA, 3)

	// Org B with its own device + patch state + heartbeat.
	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	devB, _, err := e.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	if err := e.st.RecordPatches(ctx, devB.ID, protocol.PatchReport{
		CheckedAt: e.clock.now(), Manager: protocol.PatchManagerApt,
		Updates: []protocol.PatchUpdate{{Name: "curl"}},
	}); err != nil {
		t.Fatalf("RecordPatches B: %v", err)
	}

	op, _ := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{orgA.ID})

	// Uptime: scoped list has no org B device.
	resp, body := e.do(http.MethodGet, "/api/v1/reports/uptime", nil, withCookie(op, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("scoped uptime: %d %s", resp.StatusCode, body)
	}
	var up store.UptimeReportResult
	if err := json.Unmarshal(body, &up); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, d := range up.Devices {
		if d.OrgID == orgB.ID || d.DeviceID == devB.ID {
			t.Fatalf("org B device leaked into scoped uptime: %+v", d)
		}
	}

	// Patch compliance: no org B rows.
	resp, body = e.do(http.MethodGet, "/api/v1/reports/patch-compliance", nil, withCookie(op, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("scoped patch: %d %s", resp.StatusCode, body)
	}
	var pc store.PatchComplianceResult
	if err := json.Unmarshal(body, &pc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, d := range pc.Devices {
		if d.OrgID == orgB.ID {
			t.Fatalf("org B device leaked into scoped patch compliance: %+v", d)
		}
	}

	// Explicit org_id in B -> 404 not_found (indistinguishable from missing).
	if resp, body := e.do(http.MethodGet, "/api/v1/reports/uptime?org_id="+itoa(orgB.ID), nil, withCookie(op, nil)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("scoped org_id B uptime: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(http.MethodGet, "/api/v1/reports/patch-compliance.csv?org_id="+itoa(orgB.ID), nil, withCookie(op, nil)); resp.StatusCode != 404 {
		t.Fatalf("scoped org_id B patch csv: %d %s", resp.StatusCode, body)
	}
	// device_id in B -> 404 on the disk report.
	if resp, body := e.do(http.MethodGet, "/api/v1/reports/disk?device_id="+itoa(devB.ID), nil, withCookie(op, nil)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("scoped device B disk: %d %s", resp.StatusCode, body)
	}
	// The scoped operator can read its own device's disk series.
	if resp, body := e.do(http.MethodGet, "/api/v1/reports/disk?device_id="+itoa(devA), nil, withCookie(op, nil)); resp.StatusCode != 200 {
		t.Fatalf("scoped device A disk: %d %s", resp.StatusCode, body)
	}
}

// parseCSV reads a CSV body into records, failing the test on a parse error (a
// malformed export) — the records are also proof the quoting is well-formed.
func parseCSV(t *testing.T, body []byte) [][]string {
	t.Helper()
	recs, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v\n%s", err, body)
	}
	return recs
}
