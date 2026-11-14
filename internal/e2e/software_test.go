package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/agent"
	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// fakeDpkgOutput is a captured `dpkg-query -W -f '${Package}\t${Version}\t${Maintainer}\n'`
// dump (tab-separated), mirroring internal/agent/testdata/dpkg-query.txt: six
// real packages plus one malformed line the parser must skip, so the reported
// inventory count is exactly six. A fake dpkg-query on PATH feeds this to the
// real agent software collector, so no test depends on which packages the
// container happens to have installed.
const fakeDpkgOutput = "accountsservice\t23.13.9-2ubuntu6\tUbuntu Developers <ubuntu-devel-discuss@lists.ubuntu.com>\n" +
	"base-files\t13ubuntu10.2\tSanthosh Manohar <santhosh@ubuntu.com>\n" +
	"curl\t8.5.0-2ubuntu10.6\tUbuntu Developers <ubuntu-devel-discuss@lists.ubuntu.com>\n" +
	"firefox\t131.0.3+build1-0ubuntu0.24.04.1\tMozilla <mozilla@example.com>\n" +
	"openssl\t3.0.13-0ubuntu3.4\tUbuntu Developers <ubuntu-devel-discuss@lists.ubuntu.com>\n" +
	"zlib1g:amd64\t1:1.3.dfsg-3.1ubuntu2.1\tUbuntu Developers\n" +
	"broken-no-version-line\n"

// fakeDpkgCount is the number of well-formed entries in fakeDpkgOutput (the
// malformed trailing line is dropped by parseDpkg).
const fakeDpkgCount = 6

// softwareHitJSON mirrors a store.SoftwareHit in the /api/v1/software response.
type softwareHitJSON struct {
	DeviceID int64  `json:"device_id"`
	Hostname string `json:"hostname"`
	OrgID    int64  `json:"org_id"`
	OrgName  string `json:"org_name"`
	Name     string `json:"name"`
	Version  string `json:"version"`
}

// swDeviceJSON carries the denormalised software columns the DEVICE object gains
// in SI1 (the generic deviceJSON omits them).
type swDeviceJSON struct {
	ID                int64   `json:"id"`
	Platform          string  `json:"platform"`
	Status            string  `json:"status"`
	SoftwareCount     int     `json:"software_count"`
	SoftwareCheckedAt *string `json:"software_checked_at"`
}

// installFakeDpkgQuery prepends a temp dir holding a fake dpkg-query (which
// prints fakeDpkgOutput and ignores its arguments) to PATH, so the real agent
// software collector reads a known inventory instead of the container's. PATH is
// restored on cleanup, after the agent has been stopped (cleanups run LIFO, and
// this one is registered before the agent-stop cleanup).
func installFakeDpkgQuery(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "dpkg-output.txt")
	if err := os.WriteFile(outPath, []byte(fakeDpkgOutput), 0o644); err != nil {
		t.Fatalf("write fake dpkg output: %v", err)
	}
	script := "#!/bin/sh\nexec cat " + outPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, "dpkg-query"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake dpkg-query: %v", err)
	}
	old := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", old) })
	os.Setenv("PATH", dir+string(os.PathListSeparator)+old)
}

// TestSoftwareEndToEnd proves SI1 against a fake dpkg-query on PATH, exercising
// the real agent software path in process (collector -> dpkgLister -> fake
// dpkg-query -> parseDpkg):
//   - the agent reports its installed software, visible on GET /api/v1/devices/{id}
//     with a plausible count (the six well-formed fixture entries), a set
//     software_checked_at, and the full list as the sibling "software" array;
//   - GET /api/v1/software?q=openssl returns that device with the package/version;
//   - q shorter than two characters is 400 invalid_request;
//   - RS2: an operator scoped to another org sees neither the device nor its
//     software in the search (no cross-org rows).
func TestSoftwareEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the software e2e proof runs the real Linux agent against a fake dpkg-query")
	}
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	installFakeDpkgQuery(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := &logBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server/agent logs:\n%s", logs.String())
		}
	})

	st, err := store.Open(filepath.Join(t.TempDir(), "msp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	srv := server.New(st, server.Config{
		SampleEvery: time.Second,
		LostGrace:   time.Second,
		Version:     agentVersion,
	}, logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// --- bootstrap admin, org A, enrollment token ----------------------------
	hash, err := server.HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	admin, err := st.CreateUser(ctx, adminEmail, hash, "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	orgA, err := st.CreateOrg(ctx, "Acme Dental")
	if err != nil {
		t.Fatalf("create org A: %v", err)
	}
	token, _, err := st.CreateEnrollmentToken(ctx, orgA.ID, "software-e2e", 5, 24*time.Hour, &admin.ID)
	if err != nil {
		t.Fatalf("create enrollment token: %v", err)
	}

	// --- enroll and run the real agent (loopback => http allowed) -------------
	cfg, err := agent.Enroll(ctx, ts.URL, token, "", false, 1, agentVersion)
	if err != nil {
		t.Fatalf("agent enroll: %v", err)
	}
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(agentCtx, cfg, agentVersion, logger) }()
	t.Cleanup(func() {
		stopAgent()
		select {
		case <-agentDone:
		case <-time.After(30 * time.Second):
			t.Fatalf("agent.Run did not return within 30 s of cancel")
		}
	})

	d := newDash(t, ts.URL)
	if code := d.do(http.MethodPost, "/api/v1/login", map[string]string{"email": adminEmail, "password": adminPassword}, nil); code != 200 {
		t.Fatalf("login: status %d", code)
	}

	// --- the agent reports its software; it appears on GET /devices/{id} -------
	deviceID := cfg.DeviceID
	getDevice := func() swDeviceJSON {
		var resp struct {
			Device swDeviceJSON `json:"device"`
		}
		if code := d.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", deviceID), nil, &resp); code != 200 {
			t.Fatalf("GET device: status %d", code)
		}
		return resp.Device
	}
	waitFor(t, "device online with its software inventory", 15*time.Second, func() bool {
		dev := getDevice()
		return dev.Status == "online" && dev.SoftwareCount == fakeDpkgCount
	})
	dev := getDevice()
	if dev.SoftwareCount != fakeDpkgCount {
		t.Fatalf("software_count = %d, want %d (%+v)", dev.SoftwareCount, fakeDpkgCount, dev)
	}
	if dev.SoftwareCheckedAt == nil || *dev.SoftwareCheckedAt == "" {
		t.Fatalf("software_checked_at not set: %+v", dev)
	}

	// The full list is a sibling "software" array on the device detail, holding
	// the parsed dpkg entries (name/version/publisher/source), proving the real
	// agent parse path matched the fixture.
	var detail struct {
		Software []protocol.SoftwareItem `json:"software"`
	}
	if code := d.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", deviceID), nil, &detail); code != 200 {
		t.Fatalf("GET device detail: status %d", code)
	}
	if len(detail.Software) != fakeDpkgCount {
		t.Fatalf("device detail software has %d entries, want %d: %+v", len(detail.Software), fakeDpkgCount, detail.Software)
	}
	var openssl *protocol.SoftwareItem
	for i := range detail.Software {
		if detail.Software[i].Name == "openssl" {
			openssl = &detail.Software[i]
		}
	}
	if openssl == nil {
		t.Fatalf("no openssl entry in the device software list: %+v", detail.Software)
	}
	if openssl.Version != "3.0.13-0ubuntu3.4" || openssl.Source != protocol.SoftwareSourceDpkg {
		t.Fatalf("openssl entry = %+v, want version 3.0.13-0ubuntu3.4 source dpkg", *openssl)
	}
	if openssl.Publisher != "Ubuntu Developers" {
		t.Fatalf("openssl publisher = %q, want %q (maintainer email stripped)", openssl.Publisher, "Ubuntu Developers")
	}

	// --- GET /api/v1/software?q=openssl returns that device -------------------
	softwareSearch := func(cl *dash, q string, out any) int {
		return cl.do(http.MethodGet, "/api/v1/software?q="+q, nil, out)
	}
	var found struct {
		Query     string            `json:"query"`
		Results   []softwareHitJSON `json:"results"`
		Truncated bool              `json:"truncated"`
	}
	if code := softwareSearch(d, "openssl", &found); code != 200 {
		t.Fatalf("GET /api/v1/software?q=openssl: status %d", code)
	}
	sawDevice := false
	for _, h := range found.Results {
		if h.DeviceID == deviceID && h.Name == "openssl" {
			sawDevice = true
			if h.Version != "3.0.13-0ubuntu3.4" || h.OrgID != orgA.ID {
				t.Fatalf("openssl hit = %+v, want version 3.0.13-0ubuntu3.4 org %d", h, orgA.ID)
			}
		}
	}
	if !sawDevice {
		t.Fatalf("software search for openssl did not return device %d: %+v", deviceID, found.Results)
	}

	// --- q shorter than two characters is rejected ---------------------------
	var shortErr errorJSON
	if code := softwareSearch(d, "o", &shortErr); code != 400 || shortErr.Error != "invalid_request" {
		t.Fatalf("software search q=o: status %d error %q, want 400 invalid_request", code, shortErr.Error)
	}

	// --- RS2: an operator scoped to another org sees no cross-org rows --------
	orgB, err := st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	var createResp struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if code := d.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "scoped@example.com", "password": "scoped-pass-1", "role": "operator",
		"all_orgs": false, "org_ids": []int64{orgB.ID},
	}, &createResp); code != 201 {
		t.Fatalf("create scoped operator: status %d", code)
	}
	op := newDash(t, ts.URL)
	if code := op.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "scoped@example.com", "password": "scoped-pass-1"}, nil); code != 200 {
		t.Fatalf("operator login: status %d", code)
	}
	var scoped struct {
		Results []softwareHitJSON `json:"results"`
	}
	if code := softwareSearch(op, "openssl", &scoped); code != 200 {
		t.Fatalf("scoped operator software search: status %d", code)
	}
	if len(scoped.Results) != 0 {
		t.Fatalf("operator scoped to org B saw %d cross-org software rows: %+v", len(scoped.Results), scoped.Results)
	}

	// An explicit ?org_id= for the org the device is in (and which the operator
	// cannot see) is a 404, not a disclosure (RS2), before the search runs.
	var scopedErr errorJSON
	if code := op.do(http.MethodGet, fmt.Sprintf("/api/v1/software?q=openssl&org_id=%d", orgA.ID), nil, &scopedErr); code != 404 || scopedErr.Error != "not_found" {
		t.Fatalf("scoped operator ?org_id=A: status %d error %q, want 404 not_found", code, scopedErr.Error)
	}
}
