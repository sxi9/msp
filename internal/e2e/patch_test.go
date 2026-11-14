package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/agent"
	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// patchDeviceJSON carries the denormalised patch columns the DEVICE object gains
// in PM1 (the generic deviceJSON in e2e_test.go omits them).
type patchDeviceJSON struct {
	ID               int64   `json:"id"`
	Platform         string  `json:"platform"`
	Status           string  `json:"status"`
	PendingPatches   int     `json:"pending_patches"`
	SecurityPatches  int     `json:"security_patches"`
	RebootRequired   bool    `json:"reboot_required"`
	PatchesCheckedAt *string `json:"patches_checked_at"`
}

// patchUpdateJSON / patchReportJSON mirror the device-detail sibling "patches"
// object (protocol.PatchReport).
type patchUpdateJSON struct {
	Name     string `json:"name"`
	Current  string `json:"current"`
	New      string `json:"new"`
	Security bool   `json:"security"`
	Source   string `json:"source"`
}

type patchReportJSON struct {
	Manager       string            `json:"manager"`
	Count         int               `json:"count"`
	SecurityCount int               `json:"security_count"`
	Updates       []patchUpdateJSON `json:"updates"`
}

// TestPatchEndToEnd proves PM1 against the fake apt-get from TestMain, exercising
// the real agent patch path in process (collector -> aptManager -> fake apt):
//   - the agent reports pending updates incl. a security one, visible in
//     GET /api/v1/devices/{id} with the right counts and the full report;
//   - POST .../patch {install,security} queues a command, the agent installs via
//     the fake apt, the command completes done with the log, and
//     patch.install_requested is audited;
//   - {install,ids} with an id not in the pending list fails with that detail;
//   - RS2: an operator scoped to another org gets 404 with no command created;
//   - an android device is 400 unsupported_on_platform.
func TestPatchEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the patch e2e proof runs the real Linux agent against a fake apt-get")
	}
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
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
	token, _, err := st.CreateEnrollmentToken(ctx, orgA.ID, "patch-e2e", 5, 24*time.Hour, &admin.ID)
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
	defer func() {
		stopAgent()
		select {
		case <-agentDone:
		case <-time.After(30 * time.Second):
			t.Fatalf("agent.Run did not return within 30 s of cancel")
		}
	}()

	d := newDash(t, ts.URL)
	if code := d.do(http.MethodPost, "/api/v1/login", map[string]string{"email": adminEmail, "password": adminPassword}, nil); code != 200 {
		t.Fatalf("login: status %d", code)
	}

	// --- the agent reports pending updates; they appear on GET /devices/{id} --
	deviceID := cfg.DeviceID
	getDevice := func() patchDeviceJSON {
		var resp struct {
			Device patchDeviceJSON `json:"device"`
		}
		if code := d.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", deviceID), nil, &resp); code != 200 {
			t.Fatalf("GET device: status %d", code)
		}
		return resp.Device
	}
	waitFor(t, "device online with the patch report", 15*time.Second, func() bool {
		dev := getDevice()
		return dev.Status == "online" && dev.PendingPatches == 3
	})
	dev := getDevice()
	if dev.SecurityPatches != 2 {
		t.Fatalf("security_patches = %d, want 2 (%+v)", dev.SecurityPatches, dev)
	}
	if dev.PatchesCheckedAt == nil || *dev.PatchesCheckedAt == "" {
		t.Fatalf("patches_checked_at not set: %+v", dev)
	}

	// The full report is a sibling "patches" object on the device detail, with a
	// security update present.
	var detail struct {
		Patches patchReportJSON `json:"patches"`
	}
	if code := d.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", deviceID), nil, &detail); code != 200 {
		t.Fatalf("GET device detail: status %d", code)
	}
	if detail.Patches.Manager != protocol.PatchManagerApt || len(detail.Patches.Updates) != 3 {
		t.Fatalf("device detail patches = %+v", detail.Patches)
	}
	sawSecurity := false
	for _, u := range detail.Patches.Updates {
		if u.Name == "openssl" && u.Security && u.New == "3.0.2-0ubuntu1.19" {
			sawSecurity = true
		}
	}
	if !sawSecurity {
		t.Fatalf("expected a security openssl update in the report: %+v", detail.Patches.Updates)
	}

	// --- POST .../patch {install,security}: queue, install via fake apt, done --
	patch := func(cl *dash, id int64, body map[string]any, out any) int {
		return cl.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/patch", id), body, out)
	}
	var queued struct {
		Command commandJSON `json:"command"`
		errorJSON
	}
	if code := patch(d, deviceID, map[string]any{"action": "install", "mode": "security"}, &queued); code != 201 {
		t.Fatalf("POST patch install security: status %d (%s %s)", code, queued.Error, queued.Detail)
	}
	if queued.Command.Kind != protocol.KindPatch || queued.Command.Status != "queued" {
		t.Fatalf("queued patch command: %+v", queued.Command)
	}
	done := d.waitCommand(queued.Command.ID, "done", 15*time.Second)
	if exitCode(done) != 0 || done.Error != "" {
		t.Fatalf("patch install command: exit %d error %q", exitCode(done), done.Error)
	}
	if !strings.Contains(done.Stdout, "fake apt-get: upgraded") {
		t.Fatalf("patch install log missing the fake apt output: %q", done.Stdout)
	}
	if n := countAudit(d.audit(), "patch.install_requested", "user"); n < 1 {
		t.Fatalf("patch.install_requested audit rows = %d, want >= 1", n)
	}

	// --- {install,ids} with an unknown id fails with that detail -------------
	cmdsBefore, _ := st.ListCommands(ctx, deviceID, 500)
	var idsErr errorJSON
	code := patch(d, deviceID, map[string]any{"action": "install", "mode": "ids", "ids": []string{"openssl", "ghost"}}, &idsErr)
	if code != 400 || idsErr.Error != "invalid_request" || !strings.Contains(idsErr.Detail, "ghost") {
		t.Fatalf("install unknown id: status %d error %q detail %q, want 400 naming ghost", code, idsErr.Error, idsErr.Detail)
	}
	cmdsAfter, _ := st.ListCommands(ctx, deviceID, 500)
	if len(cmdsAfter) != len(cmdsBefore) {
		t.Fatalf("a rejected ids install created %d command rows", len(cmdsAfter)-len(cmdsBefore))
	}

	// --- RS2: an operator scoped elsewhere gets 404, with no command created --
	orgB, err := st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	devB, _, err := st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("upsert device B: %v", err)
	}
	var createResp struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if code := d.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "scoped@example.com", "password": "scoped-pass-1", "role": "operator",
		"all_orgs": false, "org_ids": []int64{orgA.ID},
	}, &createResp); code != 201 {
		t.Fatalf("create scoped operator: status %d", code)
	}
	op := newDash(t, ts.URL)
	if code := op.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "scoped@example.com", "password": "scoped-pass-1"}, nil); code != 200 {
		t.Fatalf("operator login: status %d", code)
	}
	bCmdsBefore, _ := st.ListCommands(ctx, devB.ID, 500)
	var scopedErr errorJSON
	if code := patch(op, devB.ID, map[string]any{"action": "install", "mode": "security"}, &scopedErr); code != 404 || scopedErr.Error != "not_found" {
		t.Fatalf("scoped operator on org B device: status %d error %q, want 404 not_found", code, scopedErr.Error)
	}
	bCmdsAfter, _ := st.ListCommands(ctx, devB.ID, 500)
	if len(bCmdsAfter) != len(bCmdsBefore) {
		t.Fatalf("scoped 404 created %d command rows in org B", len(bCmdsAfter)-len(bCmdsBefore))
	}

	// --- android: 400 unsupported_on_platform --------------------------------
	plain := &http.Client{Timeout: 30 * time.Second}
	var enrolled protocol.EnrollResponse
	if code := doJSON(t, plain, http.MethodPost, ts.URL+"/api/agent/v1/enroll", "", protocol.EnrollRequest{
		EnrollmentToken: token, DeviceUID: "android-patch", Hostname: "Pixel 8",
		Platform: protocol.PlatformAndroid, Arch: "arm64", OSVersion: "Android 14 (34)",
		AgentVersion: "0.1.0", IntervalS: 60,
	}, &enrolled, false); code != 201 {
		t.Fatalf("android enroll: status %d", code)
	}
	var androidErr errorJSON
	if code := patch(d, enrolled.DeviceID, map[string]any{"action": "install", "mode": "security"}, &androidErr); code != 400 || androidErr.Error != "unsupported_on_platform" {
		t.Fatalf("patch on android: status %d error %q, want 400 unsupported_on_platform", code, androidErr.Error)
	}
}
