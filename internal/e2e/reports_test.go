package e2e

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// Report wire shapes (RP1), a subset of the fields the dashboard consumes in
// web/src/types.ts. Decoding the server's JSON into these proves the server
// emits the envelope the real (non-mock) dashboard expects: rows /
// mean_uptime_pct / device_count with a per-row uptime_s on uptime; series +
// device meta on disk; rows with latest on disk/top; overall / orgs / devices
// on patch compliance.
type rptUptimeRow struct {
	DeviceID  int64   `json:"device_id"`
	Hostname  string  `json:"hostname"`
	OrgID     int64   `json:"org_id"`
	OrgName   string  `json:"org_name"`
	Samples   int     `json:"samples"`
	Expected  int     `json:"expected"`
	UptimePct float64 `json:"uptime_pct"`
	UptimeS   *int64  `json:"uptime_s"`
}

type rptUptimeReport struct {
	Days          int            `json:"days"`
	Rows          []rptUptimeRow `json:"rows"`
	MeanUptimePct float64        `json:"mean_uptime_pct"`
	DeviceCount   int            `json:"device_count"`
}

type rptDiskPoint struct {
	At          string   `json:"at"`
	DiskUsedPct *float64 `json:"disk_used_pct"`
}

type rptDiskReport struct {
	DeviceID int64          `json:"device_id"`
	Hostname string         `json:"hostname"`
	OrgID    int64          `json:"org_id"`
	OrgName  string         `json:"org_name"`
	Days     int            `json:"days"`
	Series   []rptDiskPoint `json:"series"`
	Min      *float64       `json:"min"`
	Max      *float64       `json:"max"`
	Latest   *float64       `json:"latest"`
}

type rptDiskTopRow struct {
	DeviceID int64    `json:"device_id"`
	Hostname string   `json:"hostname"`
	OrgID    int64    `json:"org_id"`
	OrgName  string   `json:"org_name"`
	Latest   *float64 `json:"latest"`
}

type rptDiskTopReport struct {
	Days int             `json:"days"`
	Rows []rptDiskTopRow `json:"rows"`
}

type rptPatchCounts struct {
	Total           int `json:"total"`
	UpToDate        int `json:"up_to_date"`
	Pending         int `json:"pending"`
	SecurityPending int `json:"security_pending"`
	RebootRequired  int `json:"reboot_required"`
	NeverChecked    int `json:"never_checked"`
}

type rptPatchOrg struct {
	OrgID   int64          `json:"org_id"`
	OrgName string         `json:"org_name"`
	Counts  rptPatchCounts `json:"counts"`
}

type rptPatchDevice struct {
	DeviceID       int64   `json:"device_id"`
	Hostname       string  `json:"hostname"`
	OrgID          int64   `json:"org_id"`
	OrgName        string  `json:"org_name"`
	Pending        int     `json:"pending"`
	Security       int     `json:"security"`
	RebootRequired bool    `json:"reboot_required"`
	CheckedAt      *string `json:"checked_at"`
}

type rptPatchReport struct {
	Overall rptPatchCounts   `json:"overall"`
	Orgs    []rptPatchOrg    `json:"orgs"`
	Devices []rptPatchDevice `json:"devices"`
}

// getRaw performs a session GET and returns the response and body bytes, for the
// CSV exports (which are not JSON and are read as raw bytes to inspect headers,
// CRLF framing and the injection guard).
func getRaw(t *testing.T, d *dash, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, d.base+path, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", path, err)
	}
	return resp, body
}

// TestReports is the RP1 end-to-end proof driven through the real server
// handler: a store seeded with a known heartbeat history (via RecordSync on a
// driven clock) and patch state, read back through GET /api/v1/reports/*. It
// asserts the uptime aggregate and mid-period proration, the disk series with
// min/max/latest, the patch-compliance buckets, the CSV exports (attachment
// header, CRLF framing, injection guard, matching data) and RS2 scoping (an
// operator scoped to A sees only A and gets 404 on B), plus the retention bound.
func TestReports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := &logBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})

	st, err := store.Open(filepath.Join(t.TempDir(), "msp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// A driven clock so heartbeat timestamps and enrolled_at are deterministic.
	base := time.Now().UTC().Truncate(time.Hour)
	cur := base
	st.Now = func() time.Time { return cur }

	srv := server.New(st, server.Config{
		SampleEvery:   time.Second,
		LostGrace:     time.Second,
		Version:       agentVersion,
		RetentionDays: 7,
	}, logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// --- bootstrap admin + orgs ---------------------------------------------
	hash, err := server.HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	admin, err := st.CreateUser(ctx, adminEmail, hash, "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	_ = admin
	orgA, err := st.CreateOrg(ctx, "Acme Dental")
	if err != nil {
		t.Fatalf("create org A: %v", err)
	}
	orgB, err := st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	// An org whose name begins with '=' is formula-injection bait for the CSV.
	evil, err := st.CreateOrg(ctx, "=Evil Corp")
	if err != nil {
		t.Fatalf("create evil org: %v", err)
	}

	mkDev := func(org store.Org, uid, host string, enrolledAt time.Time) store.Device {
		cur = enrolledAt
		d, _, err := st.UpsertDevice(ctx, store.UpsertDeviceParams{
			OrgID: org.ID, DeviceUID: uid, Hostname: host, Platform: protocol.PlatformLinux,
			Arch: "amd64", IntervalS: 3600, FactsJSON: "{}",
		})
		if err != nil {
			t.Fatalf("upsert device %s: %v", uid, err)
		}
		return d
	}
	// seedHB records one heartbeat at time at with a known disk reading and OS
	// uptime. TotalGB 100 makes disk_used_pct == used, so the series values are
	// exactly what we assert. SampleEvery is 1 s and samples are hours apart, so
	// every call stores a row.
	seedHB := func(devID int64, at time.Time, disk float64, uptimeS int64) {
		cur = at
		if err := st.RecordSync(ctx, devID, "10.0.0.1", protocol.SyncRequest{
			AgentVersion: "0.1.0", IntervalS: 3600, UptimeS: uptimeS,
			Metrics: protocol.Metrics{
				MemTotalMB: 8000, MemUsedMB: 4000,
				Disks: []protocol.Disk{{Mount: "/", TotalGB: 100, UsedGB: disk}},
			},
		}, time.Second); err != nil {
			t.Fatalf("seed heartbeat dev %d: %v", devID, err)
		}
	}

	// Device A (org A): enrolled 48 h ago (full 24 h window), interval 1 h ->
	// expected 24. 12 hourly samples -> 50 %. The disk values give a known
	// min/max/latest (20 / 90 / 55).
	devA := mkDev(orgA, "uid-a", "acme-ws", base.Add(-48*time.Hour))
	diskA := []float64{20, 50, 50, 50, 50, 90, 50, 50, 50, 50, 50, 55}
	for i, d := range diskA { // samples at base-12h .. base-1h (oldest first)
		seedHB(devA.ID, base.Add(-time.Duration(12-i)*time.Hour), d, int64(10000+i*100))
	}
	wantLatestUptimeS := int64(10000 + 11*100)

	// Device B (org A): enrolled 12 h ago (mid-window), interval 1 h -> prorated
	// expected 12. 6 hourly samples -> 50 %, proving proration.
	devB := mkDev(orgA, "uid-b", "acme-laptop", base.Add(-12*time.Hour))
	for i := 0; i < 6; i++ { // samples at base-6h .. base-1h
		seedHB(devB.ID, base.Add(-time.Duration(6-i)*time.Hour), 42, 2000)
	}

	// Device C (org A): checked, one security update -> pending + security.
	devC := mkDev(orgA, "uid-c", "acme-server", base.Add(-48*time.Hour))
	if err := st.RecordPatches(ctx, devC.ID, protocol.PatchReport{
		CheckedAt: base, Manager: protocol.PatchManagerApt,
		Updates: []protocol.PatchUpdate{{Name: "openssl", Security: true}},
	}); err != nil {
		t.Fatalf("record patches C: %v", err)
	}
	// Device A is checked with nothing pending -> up_to_date. Device B is never
	// checked -> never_checked.
	if err := st.RecordPatches(ctx, devA.ID, protocol.PatchReport{
		CheckedAt: base, Manager: protocol.PatchManagerApt,
	}); err != nil {
		t.Fatalf("record patches A: %v", err)
	}

	// Org B device: its own heartbeat + patch state, must never leak to a scoped
	// operator for org A.
	devBeta := mkDev(orgB, "uid-beta", "beta-ws", base.Add(-48*time.Hour))
	seedHB(devBeta.ID, base.Add(-1*time.Hour), 70, 5000)
	if err := st.RecordPatches(ctx, devBeta.ID, protocol.PatchReport{
		CheckedAt: base, Manager: protocol.PatchManagerApt,
		Updates: []protocol.PatchUpdate{{Name: "curl"}},
	}); err != nil {
		t.Fatalf("record patches Beta: %v", err)
	}

	// Injection device in the evil org: a hostname beginning with '='.
	devEvil := mkDev(evil, "uid-evil", "=HOST", base.Add(-48*time.Hour))
	if err := st.RecordPatches(ctx, devEvil.ID, protocol.PatchReport{
		CheckedAt: base, Manager: protocol.PatchManagerApt,
		Updates: []protocol.PatchUpdate{{Name: "openssl", Security: true}},
	}); err != nil {
		t.Fatalf("record patches Evil: %v", err)
	}

	// Reads happen "now" (the end of the window).
	cur = base

	// --- admin + scoped operator logins -------------------------------------
	adminD := newDash(t, ts.URL)
	if code := adminD.do(http.MethodPost, "/api/v1/login", map[string]string{"email": adminEmail, "password": adminPassword}, nil); code != 200 {
		t.Fatalf("admin login: status %d", code)
	}
	var createResp struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if code := adminD.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "scoped@example.com", "password": "scoped-pass-1", "role": "operator",
		"all_orgs": false, "org_ids": []int64{orgA.ID},
	}, &createResp); code != 201 {
		t.Fatalf("create scoped operator: status %d", code)
	}
	op := newDash(t, ts.URL)
	if code := op.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "scoped@example.com", "password": "scoped-pass-1"}, nil); code != 200 {
		t.Fatalf("operator login: status %d", code)
	}

	// --- uptime report (admin, all orgs, days=1) ----------------------------
	var up rptUptimeReport
	if code := adminD.do(http.MethodGet, "/api/v1/reports/uptime?days=1", nil, &up); code != 200 {
		t.Fatalf("admin uptime: status %d", code)
	}
	if up.Days != 1 {
		t.Fatalf("uptime days = %d, want 1", up.Days)
	}
	if up.DeviceCount != 5 || len(up.Rows) != 5 {
		t.Fatalf("uptime device_count = %d / rows = %d, want 5", up.DeviceCount, len(up.Rows))
	}
	upByID := map[int64]rptUptimeRow{}
	for _, r := range up.Rows {
		upByID[r.DeviceID] = r
	}
	if a := upByID[devA.ID]; a.Samples != 12 || a.Expected != 24 || a.UptimePct != 50 {
		t.Fatalf("device A uptime = %+v, want samples 12 expected 24 pct 50", a)
	}
	if a := upByID[devA.ID]; a.UptimeS == nil || *a.UptimeS != wantLatestUptimeS {
		t.Fatalf("device A os uptime_s = %v, want %d", a.UptimeS, wantLatestUptimeS)
	}
	if b := upByID[devB.ID]; b.Samples != 6 || b.Expected != 12 || b.UptimePct != 50 {
		t.Fatalf("device B (prorated) uptime = %+v, want samples 6 expected 12 pct 50", b)
	}

	// --- disk series (admin, device A, days=1) ------------------------------
	var disk rptDiskReport
	if code := adminD.do(http.MethodGet, fmt.Sprintf("/api/v1/reports/disk?device_id=%d&days=1", devA.ID), nil, &disk); code != 200 {
		t.Fatalf("admin disk: status %d", code)
	}
	if disk.DeviceID != devA.ID || disk.Hostname != "acme-ws" || disk.OrgName != "Acme Dental" {
		t.Fatalf("disk meta = %+v, want device A acme-ws / Acme Dental", disk)
	}
	if len(disk.Series) != 12 {
		t.Fatalf("disk series = %d points, want 12", len(disk.Series))
	}
	if disk.Min == nil || disk.Max == nil || disk.Latest == nil || *disk.Min != 20 || *disk.Max != 90 || *disk.Latest != 55 {
		t.Fatalf("disk min/max/latest = %v/%v/%v, want 20/90/55", disk.Min, disk.Max, disk.Latest)
	}

	// --- disk top (admin, all orgs) -----------------------------------------
	var top rptDiskTopReport
	if code := adminD.do(http.MethodGet, "/api/v1/reports/disk/top?days=1", nil, &top); code != 200 {
		t.Fatalf("admin disk top: status %d", code)
	}
	if len(top.Rows) < 2 {
		t.Fatalf("disk top rows = %d, want >= 2 (A, Beta)", len(top.Rows))
	}
	for i := 1; i < len(top.Rows); i++ {
		if top.Rows[i-1].Latest == nil || top.Rows[i].Latest == nil || *top.Rows[i-1].Latest < *top.Rows[i].Latest {
			t.Fatalf("disk top not sorted by latest desc: %+v", top.Rows)
		}
	}
	// Device B has no disk sample within... it does (42). devBeta latest 70 is the
	// fleet high.
	if top.Rows[0].DeviceID != devBeta.ID || top.Rows[0].Latest == nil || *top.Rows[0].Latest != 70 {
		t.Fatalf("disk top[0] = %+v, want beta@70", top.Rows[0])
	}

	// --- patch compliance (scoped operator, exact buckets for org A only) ----
	var pc rptPatchReport
	if code := op.do(http.MethodGet, "/api/v1/reports/patch-compliance", nil, &pc); code != 200 {
		t.Fatalf("scoped patch compliance: status %d", code)
	}
	wantCounts := rptPatchCounts{Total: 3, UpToDate: 1, Pending: 1, SecurityPending: 1, RebootRequired: 0, NeverChecked: 1}
	if pc.Overall != wantCounts {
		t.Fatalf("scoped patch overall = %+v, want %+v", pc.Overall, wantCounts)
	}
	if len(pc.Orgs) != 1 || pc.Orgs[0].OrgID != orgA.ID || pc.Orgs[0].Counts != wantCounts {
		t.Fatalf("scoped patch orgs = %+v, want one org A with %+v", pc.Orgs, wantCounts)
	}
	for _, d := range pc.Devices {
		if d.OrgID == orgB.ID || d.OrgID == evil.ID {
			t.Fatalf("org B/evil device leaked into scoped patch compliance: %+v", d)
		}
	}

	// --- RS2: scoped operator sees only org A, 404 on B ---------------------
	var scopedUp rptUptimeReport
	if code := op.do(http.MethodGet, "/api/v1/reports/uptime", nil, &scopedUp); code != 200 {
		t.Fatalf("scoped uptime: status %d", code)
	}
	for _, r := range scopedUp.Rows {
		if r.OrgID == orgB.ID || r.OrgID == evil.ID {
			t.Fatalf("org B/evil device leaked into scoped uptime: %+v", r)
		}
	}
	if scopedUp.DeviceCount != 3 {
		t.Fatalf("scoped uptime device_count = %d, want 3 (org A only)", scopedUp.DeviceCount)
	}
	{
		var e errorJSON
		if code := op.do(http.MethodGet, fmt.Sprintf("/api/v1/reports/uptime?org_id=%d", orgB.ID), nil, &e); code != 404 || e.Error != "not_found" {
			t.Fatalf("scoped uptime org_id B: status %d error %q, want 404 not_found", code, e.Error)
		}
		e = errorJSON{}
		if code := op.do(http.MethodGet, fmt.Sprintf("/api/v1/reports/disk?device_id=%d", devBeta.ID), nil, &e); code != 404 || e.Error != "not_found" {
			t.Fatalf("scoped disk device B: status %d error %q, want 404 not_found", code, e.Error)
		}
		// The scoped operator can read its own device's disk series.
		if code := op.do(http.MethodGet, fmt.Sprintf("/api/v1/reports/disk?device_id=%d", devA.ID), nil, nil); code != 200 {
			t.Fatalf("scoped disk device A: status %d, want 200", code)
		}
		// 404 before any side effect on the CSV export too.
		if resp, _ := getRaw(t, op, fmt.Sprintf("/api/v1/reports/patch-compliance.csv?org_id=%d", orgB.ID)); resp.StatusCode != 404 {
			t.Fatalf("scoped patch csv org_id B: status %d, want 404", resp.StatusCode)
		}
	}

	// --- days beyond retention rejected (admin) -----------------------------
	for _, p := range []string{
		"/api/v1/reports/uptime?days=8",
		"/api/v1/reports/uptime.csv?days=8",
		fmt.Sprintf("/api/v1/reports/disk?device_id=%d&days=8", devA.ID),
		fmt.Sprintf("/api/v1/reports/disk.csv?device_id=%d&days=8", devA.ID),
		"/api/v1/reports/disk/top?days=8",
	} {
		var e errorJSON
		if code := adminD.do(http.MethodGet, p, nil, &e); code != 400 || e.Error != "invalid_request" {
			t.Fatalf("days beyond retention %s: status %d error %q, want 400 invalid_request", p, code, e.Error)
		}
	}

	// --- CSV exports (admin) ------------------------------------------------
	// Uptime CSV: attachment header, text/csv, CRLF framing, the os_uptime_s
	// column, and a parseable body.
	resp, body := getRaw(t, adminD, "/api/v1/reports/uptime.csv?days=1")
	if resp.StatusCode != 200 {
		t.Fatalf("uptime.csv: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("uptime.csv content-type = %q", ct)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "msp-uptime-") || !strings.Contains(cd, ".csv") {
		t.Fatalf("uptime.csv content-disposition = %q", cd)
	}
	if !bytes.Contains(body, []byte("\r\n")) {
		t.Fatalf("uptime.csv is not CRLF framed: %q", body)
	}
	recs := parseReportCSV(t, body)
	wantUptimeHeader := []string{"device_id", "hostname", "org_id", "org_name", "samples", "expected", "uptime_pct", "os_uptime_s"}
	if strings.Join(recs[0], ",") != strings.Join(wantUptimeHeader, ",") {
		t.Fatalf("uptime.csv header = %v, want %v", recs[0], wantUptimeHeader)
	}
	if len(recs) != 6 { // header + 5 devices
		t.Fatalf("uptime.csv rows = %d, want header + 5", len(recs))
	}

	// Disk CSV: header and one data row per seeded sample.
	resp, body = getRaw(t, adminD, fmt.Sprintf("/api/v1/reports/disk.csv?device_id=%d&days=1", devA.ID))
	if resp.StatusCode != 200 {
		t.Fatalf("disk.csv: status %d", resp.StatusCode)
	}
	recs = parseReportCSV(t, body)
	if strings.Join(recs[0], ",") != "at,disk_used_pct" {
		t.Fatalf("disk.csv header = %v", recs[0])
	}
	if len(recs) != 13 { // header + 12 points
		t.Fatalf("disk.csv rows = %d, want header + 12", len(recs))
	}

	// Patch CSV: the header matches the client preview (no platform column) and
	// the formula-injection guard neutralises the leading '=' in both the
	// hostname and the org name.
	resp, body = getRaw(t, adminD, "/api/v1/reports/patch-compliance.csv")
	if resp.StatusCode != 200 {
		t.Fatalf("patch-compliance.csv: status %d", resp.StatusCode)
	}
	recs = parseReportCSV(t, body)
	wantPatchHeader := []string{"device_id", "hostname", "org_id", "org_name", "pending", "security", "reboot_required", "checked_at"}
	if strings.Join(recs[0], ",") != strings.Join(wantPatchHeader, ",") {
		t.Fatalf("patch csv header = %v, want %v", recs[0], wantPatchHeader)
	}
	var sawHost, sawOrg bool
	for _, row := range recs {
		for _, cell := range row {
			if cell == "=HOST" || cell == "=Evil Corp" {
				t.Fatalf("unguarded injection cell in patch CSV: %q (row %v)", cell, row)
			}
			if cell == "'=HOST" {
				sawHost = true
			}
			if cell == "'=Evil Corp" {
				sawOrg = true
			}
		}
	}
	if !sawHost || !sawOrg {
		t.Fatalf("patch CSV injection guard missing (host=%v org=%v):\n%s", sawHost, sawOrg, body)
	}
	_ = devEvil
}

// parseReportCSV parses a CSV export into records (also proof the quoting is
// well-formed) and fails on a parse error or an empty body.
func parseReportCSV(t *testing.T, body []byte) [][]string {
	t.Helper()
	recs, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v\n%s", err, body)
	}
	if len(recs) < 1 {
		t.Fatalf("csv has no rows:\n%s", body)
	}
	return recs
}
