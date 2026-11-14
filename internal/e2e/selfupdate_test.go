//go:build linux

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
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/agent"
	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// fakeAgentBinary is a tiny shell script that prints version on any invocation,
// so "<binary> version" (the agent's self-update health check) answers with
// version. It stands in for a cross-compiled agent release: the e2e never
// replaces the real test binary — every swap targets a temp "exe" path the test
// controls through agent.SetResolveExecutableForTest.
func fakeAgentBinary(version string) []byte {
	return []byte("#!/bin/sh\necho " + version + "\n")
}

// publishRelease writes the served binary and agents.json for one release into
// agentDir (served at /dl/). shaOverride, when non-empty, is written as the
// manifest sha256 instead of the real hash of bin, to force a download mismatch.
func publishRelease(t *testing.T, agentDir, archKey, version, fileName string, bin []byte, shaOverride string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(agentDir, fileName), bin, 0o755); err != nil {
		t.Fatalf("write served binary: %v", err)
	}
	sha := shaOverride
	if sha == "" {
		sha = sha256hex(bin)
	}
	manifest := fmt.Sprintf(`{"version":%q,"files":{%q:{"name":%q,"sha256":%q}}}`, version, archKey, fileName, sha)
	if err := os.WriteFile(filepath.Join(agentDir, server.ManifestFileName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write agents.json: %v", err)
	}
}

// agentReleaseVersion reads GET /api/v1/agent-release and returns the manifest's
// current version ("" when self-update is off). The server reloads agents.json on
// access, so the test uses this as the sync point after publishRelease.
func (d *dash) agentReleaseVersion() string {
	d.t.Helper()
	var m struct {
		Version string `json:"version"`
	}
	if code := d.do(http.MethodGet, "/api/v1/agent-release", nil, &m); code != 200 {
		d.t.Fatalf("GET /api/v1/agent-release: status %d", code)
	}
	return m.Version
}

// TestSelfUpdateEndToEnd proves AU1 end to end with the REAL Linux agent and a
// REAL download from the server's /dl/, against a fake "new" binary that targets a
// temp "exe" path (never the real test binary):
//
//   - a manifest advertising a newer version makes the sync response carry
//     agent_update;
//   - POST /devices/{id}/update queues an update command; the agent downloads the
//     served binary, verifies its hash, health-checks it, swaps the temp exe and
//     reports done old->new, keeping a .bak; agent.update_requested is audited;
//   - a sha256 mismatch fails with no swap and no dangling backup;
//   - a health-check failure (served binary prints the wrong version) aborts the
//     swap with the old binary intact;
//   - the start-of-day bootstrap rollback restores the backup over a broken binary;
//   - an operator scoped to another org gets 404 (RS2); android → 400
//     unsupported_on_platform; a device already on the released version → 409
//     no_update_available.
func TestSelfUpdateEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the self-update e2e proof runs the real Linux agent (download + health-check + swap)")
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

	// AgentDir holds agents.json and the served binaries; the server advertises and
	// fills update commands from it, and /dl/ serves the binaries to the agent.
	agentDir := t.TempDir()
	srv := server.New(st, server.Config{
		SampleEvery: time.Second,
		LostGrace:   time.Second,
		Version:     agentVersion,
		AgentDir:    agentDir,
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
	token, _, err := st.CreateEnrollmentToken(ctx, orgA.ID, "selfupdate-e2e", 10, 24*time.Hour, &admin.ID)
	if err != nil {
		t.Fatalf("create enrollment token: %v", err)
	}

	d := newDash(t, ts.URL)
	if code := d.do(http.MethodPost, "/api/v1/login", map[string]string{"email": adminEmail, "password": adminPassword}, nil); code != 200 {
		t.Fatalf("login: status %d", code)
	}
	plain := &http.Client{Timeout: 30 * time.Second}
	archKey := protocol.PlatformLinux + "-" + runtime.GOARCH

	// --- 1. start-of-day bootstrap rollback (exported API, temp exe) ----------
	// A broken installed binary with a good <exe>.bak is rolled back: BootstrapRollback
	// restores the backup and clears it. Scoped to its own temp exe so it does not
	// disturb the running agent set up below.
	{
		rbExe := filepath.Join(t.TempDir(), "msp-agent")
		goodOld := fakeAgentBinary("0.1.0")
		if err := os.WriteFile(rbExe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rbExe+".bak", goodOld, 0o755); err != nil {
			t.Fatal(err)
		}
		restore := agent.SetResolveExecutableForTest(func() (string, error) { return rbExe, nil })
		restored := agent.BootstrapRollback(ctx, "0.1.0", logger)
		restore()
		if !restored {
			t.Fatal("bootstrap rollback: expected the broken binary to be rolled back")
		}
		if got, _ := os.ReadFile(rbExe); string(got) != string(goodOld) {
			t.Fatal("bootstrap rollback: exe was not restored to the backup content")
		}
		if _, err := os.Stat(rbExe + ".bak"); !os.IsNotExist(err) {
			t.Fatal("bootstrap rollback: backup should be removed after a rollback")
		}
	}

	// --- point the whole self-update machinery at a temp "exe" for this agent ---
	agentExe := filepath.Join(t.TempDir(), "msp-agent")
	seed := fakeAgentBinary(agentVersion) // the "old" running agent image
	if err := os.WriteFile(agentExe, seed, 0o755); err != nil {
		t.Fatal(err)
	}
	restoreResolve := agent.SetResolveExecutableForTest(func() (string, error) { return agentExe, nil })
	defer restoreResolve()

	// --- enroll and run the real agent with auto-update OFF (forced path only) ---
	cfg, err := agent.Enroll(ctx, ts.URL, token, "", false, 1, agentVersion)
	if err != nil {
		t.Fatalf("agent enroll: %v", err)
	}
	off := false
	cfg.AutoUpdate = &off
	if cfg.AutoUpdateEnabled() {
		t.Fatal("auto-update should be disabled for the forced-path agent")
	}
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(agentCtx, cfg, agentVersion, logger) }()
	agentStopped := false
	waitAgent := func() {
		t.Helper()
		stopAgent()
		if agentStopped {
			return
		}
		agentStopped = true
		select {
		case err := <-agentDone:
			if err != nil {
				t.Fatalf("agent.Run returned %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("agent.Run did not return within 30 s of cancel")
		}
	}
	defer waitAgent()

	deviceID := cfg.DeviceID
	waitFor(t, "linux device online", 15*time.Second, func() bool {
		return d.device(deviceID).Status == "online"
	})

	update := func(cl *dash, id int64, body map[string]any, out any) int {
		return cl.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/update", id), body, out)
	}

	// --- 2. publish release 0.2.0; endpoint + advertise gates -----------------
	publishRelease(t, agentDir, archKey, "0.2.0", "msp-agent-"+archKey, fakeAgentBinary("0.2.0"), "")
	waitFor(t, "manifest 0.2.0 loaded", 5*time.Second, func() bool { return d.agentReleaseVersion() == "0.2.0" })

	// A probe desktop device reporting the old version gets agent_update on sync.
	var probe protocol.EnrollResponse
	if code := doJSON(t, plain, http.MethodPost, ts.URL+"/api/agent/v1/enroll", "", protocol.EnrollRequest{
		EnrollmentToken: token, DeviceUID: "probe-old", Hostname: "probe-ws",
		Platform: protocol.PlatformLinux, Arch: runtime.GOARCH, OSVersion: "Ubuntu 24.04",
		AgentVersion: agentVersion, IntervalS: 30,
	}, &probe, false); code != 201 {
		t.Fatalf("probe enroll: status %d", code)
	}
	var sync protocol.SyncResponse
	if code := doJSON(t, plain, http.MethodPost, ts.URL+"/api/agent/v1/sync", probe.DeviceToken, protocol.SyncRequest{
		AgentVersion: agentVersion, IntervalS: 30, WaitS: 0, UptimeS: 100,
		Metrics: protocol.Metrics{MemTotalMB: 8000, MemUsedMB: 4000, IPs: []string{"10.0.0.9"}},
	}, &sync, false); code != 200 {
		t.Fatalf("probe sync: status %d", code)
	}
	if sync.AgentUpdate == nil || sync.AgentUpdate.Version != "0.2.0" {
		t.Fatalf("sync response agent_update = %+v, want version 0.2.0", sync.AgentUpdate)
	}
	if sync.AgentUpdate.URL != "/dl/msp-agent-"+archKey || sync.AgentUpdate.SHA256 != sha256hex(fakeAgentBinary("0.2.0")) {
		t.Fatalf("sync response agent_update url/sha = %+v", sync.AgentUpdate)
	}

	// no_update_available: a device already on the released version.
	var current protocol.EnrollResponse
	if code := doJSON(t, plain, http.MethodPost, ts.URL+"/api/agent/v1/enroll", "", protocol.EnrollRequest{
		EnrollmentToken: token, DeviceUID: "already-current", Hostname: "current-ws",
		Platform: protocol.PlatformLinux, Arch: runtime.GOARCH, OSVersion: "Ubuntu 24.04",
		AgentVersion: "0.2.0", IntervalS: 30,
	}, &current, false); code != 201 {
		t.Fatalf("current-version enroll: status %d", code)
	}
	var noUpd errorJSON
	if code := update(d, current.DeviceID, nil, &noUpd); code != 409 || noUpd.Error != "no_update_available" {
		t.Fatalf("update on an up-to-date device: status %d error %q, want 409 no_update_available", code, noUpd.Error)
	}

	// RS2: an operator scoped to org A gets 404 updating an org-B device, before any
	// side effect (no command queued).
	orgB, err := st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	devB, _, err := st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: runtime.GOARCH, AgentVersion: agentVersion, IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("upsert device B: %v", err)
	}
	if code := d.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "scoped@example.com", "password": "scoped-pass-1", "role": "operator",
		"all_orgs": false, "org_ids": []int64{orgA.ID},
	}, nil); code != 201 {
		t.Fatalf("create scoped operator: status %d", code)
	}
	op := newDash(t, ts.URL)
	if code := op.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "scoped@example.com", "password": "scoped-pass-1"}, nil); code != 200 {
		t.Fatalf("operator login: status %d", code)
	}
	bCmdsBefore, _ := st.ListCommands(ctx, devB.ID, 500)
	var scopedErr errorJSON
	if code := update(op, devB.ID, nil, &scopedErr); code != 404 || scopedErr.Error != "not_found" {
		t.Fatalf("scoped operator update of org-B device: status %d error %q, want 404 not_found", code, scopedErr.Error)
	}
	bCmdsAfter, _ := st.ListCommands(ctx, devB.ID, 500)
	if len(bCmdsAfter) != len(bCmdsBefore) {
		t.Fatalf("scoped 404 created %d command rows in org B", len(bCmdsAfter)-len(bCmdsBefore))
	}

	// android → 400 unsupported_on_platform.
	var androidDev protocol.EnrollResponse
	if code := doJSON(t, plain, http.MethodPost, ts.URL+"/api/agent/v1/enroll", "", protocol.EnrollRequest{
		EnrollmentToken: token, DeviceUID: "android-selfupdate", Hostname: "Pixel 8",
		Platform: protocol.PlatformAndroid, Arch: "arm64", OSVersion: "Android 14 (34)",
		AgentVersion: "0.1.0", IntervalS: 60,
	}, &androidDev, false); code != 201 {
		t.Fatalf("android enroll: status %d", code)
	}
	var androidErr errorJSON
	if code := update(d, androidDev.DeviceID, nil, &androidErr); code != 400 || androidErr.Error != "unsupported_on_platform" {
		t.Fatalf("update on android: status %d error %q, want 400 unsupported_on_platform", code, androidErr.Error)
	}

	// --- 3. a sha256 mismatch fails with no swap, no dangling backup ----------
	publishRelease(t, agentDir, archKey, "0.3.0", "msp-agent-mismatch", fakeAgentBinary("0.3.0"), sha256hex([]byte("not these bytes")))
	waitFor(t, "manifest 0.3.0 loaded", 5*time.Second, func() bool { return d.agentReleaseVersion() == "0.3.0" })
	var mmQueued struct {
		Command commandJSON `json:"command"`
		errorJSON
	}
	if code := update(d, deviceID, map[string]any{"timeout_s": 120}, &mmQueued); code != 201 {
		t.Fatalf("POST update (mismatch): status %d (%s %s)", code, mmQueued.Error, mmQueued.Detail)
	}
	if mmQueued.Command.Kind != protocol.KindUpdate || mmQueued.Command.Status != "queued" {
		t.Fatalf("queued update command (mismatch): %+v", mmQueued.Command)
	}
	mm := d.waitCommand(mmQueued.Command.ID, "failed", 20*time.Second)
	if !strings.Contains(mm.Error, "sha256 mismatch") {
		t.Fatalf("mismatch update error = %q, want a sha256 mismatch", mm.Error)
	}
	if got, _ := os.ReadFile(agentExe); string(got) != string(seed) {
		t.Fatal("the agent binary must be untouched on a hash mismatch")
	}
	if _, err := os.Stat(agentExe + ".bak"); !os.IsNotExist(err) {
		t.Fatal("no backup may be left behind on a hash mismatch")
	}

	// --- 4. a health-check failure aborts the swap, old binary intact ---------
	publishRelease(t, agentDir, archKey, "0.4.0", "msp-agent-healthfail", fakeAgentBinary("9.9.9"), "")
	waitFor(t, "manifest 0.4.0 loaded", 5*time.Second, func() bool { return d.agentReleaseVersion() == "0.4.0" })
	var hcQueued struct {
		Command commandJSON `json:"command"`
		errorJSON
	}
	if code := update(d, deviceID, map[string]any{"timeout_s": 120}, &hcQueued); code != 201 {
		t.Fatalf("POST update (health-check): status %d (%s %s)", code, hcQueued.Error, hcQueued.Detail)
	}
	hc := d.waitCommand(hcQueued.Command.ID, "failed", 20*time.Second)
	if !strings.Contains(hc.Error, "health check") {
		t.Fatalf("health-check update error = %q, want a health-check failure", hc.Error)
	}
	if got, _ := os.ReadFile(agentExe); string(got) != string(seed) {
		t.Fatal("the agent binary must be intact when the health check fails")
	}
	if _, err := os.Stat(agentExe + ".bak"); !os.IsNotExist(err) {
		t.Fatal("no dangling backup after a failed health check")
	}

	// --- 5. a successful self-update: download, verify, health-check, swap -----
	newBin := fakeAgentBinary("0.5.0")
	publishRelease(t, agentDir, archKey, "0.5.0", "msp-agent-good", newBin, "")
	waitFor(t, "manifest 0.5.0 loaded", 5*time.Second, func() bool { return d.agentReleaseVersion() == "0.5.0" })
	var okQueued struct {
		Command commandJSON `json:"command"`
		errorJSON
	}
	if code := update(d, deviceID, map[string]any{"timeout_s": 120}, &okQueued); code != 201 {
		t.Fatalf("POST update (success): status %d (%s %s)", code, okQueued.Error, okQueued.Detail)
	}
	done := d.waitCommand(okQueued.Command.ID, "done", 20*time.Second)
	if exitCode(done) != 0 || done.Error != "" {
		t.Fatalf("successful update: exit %d error %q", exitCode(done), done.Error)
	}
	if !strings.Contains(done.Stdout, agentVersion+" -> 0.5.0") {
		t.Fatalf("update result should report old->new, got %q", done.Stdout)
	}
	// The temp exe is now the new binary, and the old one is kept as <exe>.bak.
	if got, _ := os.ReadFile(agentExe); string(got) != string(newBin) {
		t.Fatal("the agent binary was not replaced with the new release")
	}
	if got, err := os.ReadFile(agentExe + ".bak"); err != nil || string(got) != string(seed) {
		t.Fatalf("expected <exe>.bak to hold the old binary, err=%v", err)
	}
	if n := countAudit(d.audit(), "agent.update_requested", "user"); n < 1 {
		t.Fatalf("agent.update_requested audit rows = %d, want >= 1", n)
	}

	// The successful swap asks the agent to exit so the service restarts into the new
	// binary; agent.Run returns nil on its own.
	select {
	case err := <-agentDone:
		agentStopped = true
		if err != nil {
			t.Fatalf("agent.Run after a successful self-update returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("agent.Run did not exit after a successful self-update")
	}
}
