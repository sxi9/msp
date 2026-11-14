package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/agent"
	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// deployMarker is a harmless shell script pushed to the device by the deploy
// e2e. Run with a path argument it prints a line the test asserts on and creates
// that marker file, proving the deployed file was downloaded, written and
// executed. It installs nothing and touches nothing else.
const deployMarker = `#!/bin/sh
echo "deploy-marker ran with arg=$1"
if [ -n "$1" ]; then
	: > "$1"
fi
`

// fileJSON mirrors the file-library row (store.File) the SD1 endpoints return.
type fileJSON struct {
	ID     int64  `json:"id"`
	OrgID  *int64 `json:"org_id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// uploadFile posts a multipart/form-data upload to POST /api/v1/files through the
// dash's session cookie + CSRF header and returns the created file row. orgID nil
// uploads a global file.
func (d *dash) uploadFile(name, filename string, body []byte, orgID *int64) fileJSON {
	d.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if name != "" {
		_ = mw.WriteField("name", name)
	}
	if orgID != nil {
		_ = mw.WriteField("org_id", strconv.FormatInt(*orgID, 10))
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		d.t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		d.t.Fatalf("write file part: %v", err)
	}
	if err := mw.Close(); err != nil {
		d.t.Fatalf("close multipart: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, d.base+"/api/v1/files", &buf)
	if err != nil {
		d.t.Fatalf("new upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Requested-With", "msp")
	resp, err := d.hc.Do(req)
	if err != nil {
		d.t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		d.t.Fatalf("upload %s: status %d (%s)", name, resp.StatusCode, data)
	}
	var out struct {
		File fileJSON `json:"file"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		d.t.Fatalf("decode upload response %q: %v", data, err)
	}
	return out.File
}

func (d *dash) listFiles() []fileJSON {
	d.t.Helper()
	var out struct {
		Files []fileJSON `json:"files"`
	}
	if code := d.do(http.MethodGet, "/api/v1/files", nil, &out); code != 200 {
		d.t.Fatalf("GET /api/v1/files: status %d", code)
	}
	return out.Files
}

// TestDeployEndToEnd proves SD1 end to end with a REAL download: the in-process
// Linux agent downloads an uploaded library file over the device-token endpoint,
// verifies its hash before writing, writes dest and runs it; package mode drives
// the fake apt from TestMain. It never fetches from the public internet or
// installs a real package.
//
//   - upload a marker script via POST /api/v1/files; it lists with the right
//     size + sha256;
//   - POST /devices/{id}/deploy {source:file, run:true} → the agent downloads,
//     verifies, writes dest 0755 and runs it; the command is done, the marker is
//     in the output and on disk, and deploy.requested is audited;
//   - a library file whose stored hash does not match its bytes fails the deploy
//     without writing dest;
//   - a dest containing ".." is rejected (400 at the API) with no command queued;
//   - source:package htop installs via the fake apt and completes done;
//   - RS2: an operator scoped to another org gets 404 on the deploy endpoint, and
//     a foreign device token gets 404 downloading an org-scoped file;
//   - android → 400 unsupported_on_platform.
func TestDeployEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the deploy e2e proof runs the real Linux agent (download + run + fake apt)")
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

	// The file library lives on disk; the agent-download endpoint and the upload
	// handler both read/write <FilesDir>/<id>, so FilesDir must be set.
	filesDir := t.TempDir()
	srv := server.New(st, server.Config{
		SampleEvery: time.Second,
		LostGrace:   time.Second,
		Version:     agentVersion,
		FilesDir:    filesDir,
		MaxFileMB:   16,
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
	token, _, err := st.CreateEnrollmentToken(ctx, orgA.ID, "deploy-e2e", 5, 24*time.Hour, &admin.ID)
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

	deviceID := cfg.DeviceID
	waitFor(t, "linux device online", 15*time.Second, func() bool {
		return d.device(deviceID).Status == "online"
	})

	deploy := func(cl *dash, id int64, body map[string]any, out any) int {
		return cl.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/deploy", id), body, out)
	}

	// --- upload the marker script (scoped to org A) and verify the listing -----
	marker := []byte(deployMarker)
	file := d.uploadFile("installer.sh", "installer.sh", marker, &orgA.ID)
	if file.Size != int64(len(marker)) || file.SHA256 != sha256hex(marker) {
		t.Fatalf("uploaded file metadata = %+v, want size %d sha256 %s", file, len(marker), sha256hex(marker))
	}
	var listed *fileJSON
	for _, f := range d.listFiles() {
		if f.ID == file.ID {
			fc := f
			listed = &fc
			break
		}
	}
	if listed == nil {
		t.Fatalf("uploaded file %d not in GET /api/v1/files", file.ID)
	}
	if listed.Size != int64(len(marker)) || listed.SHA256 != sha256hex(marker) {
		t.Fatalf("listed file = %+v, want size %d sha256 %s", *listed, len(marker), sha256hex(marker))
	}

	// --- a REAL file deploy: download, verify, write dest 0755, run ------------
	markerOut := filepath.Join(t.TempDir(), "ran.marker")
	// dest lives under a not-yet-existing parent so the agent's 0755 parent
	// creation is exercised too.
	dest := filepath.Join(t.TempDir(), "acme", "installer.sh")
	var queued struct {
		Command commandJSON `json:"command"`
		errorJSON
	}
	if code := deploy(d, deviceID, map[string]any{
		"source": "file", "file_id": file.ID, "dest": dest,
		"run": true, "run_args": []string{markerOut}, "timeout_s": 60,
	}, &queued); code != 201 {
		t.Fatalf("POST deploy file: status %d (%s %s)", code, queued.Error, queued.Detail)
	}
	if queued.Command.Kind != protocol.KindDeploy || queued.Command.Status != "queued" {
		t.Fatalf("queued deploy command: %+v", queued.Command)
	}
	done := d.waitCommand(queued.Command.ID, "done", 20*time.Second)
	if exitCode(done) != 0 || done.Error != "" {
		t.Fatalf("deploy command: exit %d error %q stdout %q", exitCode(done), done.Error, done.Stdout)
	}
	if !strings.Contains(done.Stdout, "deploy-marker ran with arg="+markerOut) {
		t.Fatalf("deploy stdout missing marker output: %q", done.Stdout)
	}
	if _, err := os.Stat(markerOut); err != nil {
		t.Fatalf("marker file not created by the deployed script: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("dest not written: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("dest mode = %o, want 0755 (run)", info.Mode().Perm())
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(marker) {
		t.Fatalf("dest contents differ from the uploaded file")
	}
	if n := countAudit(d.audit(), "deploy.requested", "user"); n < 1 {
		t.Fatalf("deploy.requested audit rows = %d, want >= 1", n)
	}

	// --- a library file whose stored hash does not match its bytes aborts ------
	// The server supplies the sha256 from the stored row, so a tampered/corrupted
	// library file (row hash != bytes on disk) must fail the agent's verification
	// before dest is written. Insert such a row directly and place real bytes.
	realBytes := []byte("the real payload bytes")
	tampered, err := st.CreateFile(ctx, store.CreateFileParams{
		OrgID: &orgA.ID, Name: "tampered.bin", Size: int64(len(realBytes)),
		SHA256: sha256hex([]byte("a completely different content")), CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("create tampered file row: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filesDir, strconv.FormatInt(tampered.ID, 10)), realBytes, 0o644); err != nil {
		t.Fatalf("write tampered bytes: %v", err)
	}
	badDest := filepath.Join(t.TempDir(), "bad", "artifact.bin")
	var badQueued struct {
		Command commandJSON `json:"command"`
	}
	if code := deploy(d, deviceID, map[string]any{
		"source": "file", "file_id": tampered.ID, "dest": badDest, "timeout_s": 60,
	}, &badQueued); code != 201 {
		t.Fatalf("POST deploy tampered file: status %d", code)
	}
	badDone := d.waitCommand(badQueued.Command.ID, "failed", 20*time.Second)
	if !strings.Contains(badDone.Error, "sha256 mismatch") {
		t.Fatalf("tampered deploy error = %q, want a sha256 mismatch", badDone.Error)
	}
	if _, err := os.Stat(badDest); !os.IsNotExist(err) {
		t.Fatalf("dest must NOT be written on a hash mismatch (stat err=%v)", err)
	}

	// --- a dest containing ".." is rejected at the API, no command queued ------
	cmdsBefore, _ := st.ListCommands(ctx, deviceID, 500)
	var dotdotErr errorJSON
	if code := deploy(d, deviceID, map[string]any{
		"source": "file", "file_id": file.ID, "dest": "/opt/../etc/passwd",
	}, &dotdotErr); code != 400 || dotdotErr.Error != "invalid_request" {
		t.Fatalf("deploy with ..: status %d error %q, want 400 invalid_request", code, dotdotErr.Error)
	}
	cmdsAfter, _ := st.ListCommands(ctx, deviceID, 500)
	if len(cmdsAfter) != len(cmdsBefore) {
		t.Fatalf("a rejected .. dest created %d command rows", len(cmdsAfter)-len(cmdsBefore))
	}

	// --- source:package htop via the fake apt completes done -------------------
	var pkgQueued struct {
		Command commandJSON `json:"command"`
		errorJSON
	}
	if code := deploy(d, deviceID, map[string]any{"source": "package", "package": "htop", "timeout_s": 60}, &pkgQueued); code != 201 {
		t.Fatalf("POST deploy package: status %d (%s %s)", code, pkgQueued.Error, pkgQueued.Detail)
	}
	pkgDone := d.waitCommand(pkgQueued.Command.ID, "done", 20*time.Second)
	if exitCode(pkgDone) != 0 || pkgDone.Error != "" {
		t.Fatalf("package deploy: exit %d error %q", exitCode(pkgDone), pkgDone.Error)
	}
	if !strings.Contains(pkgDone.Stdout, "fake apt-get: upgraded") {
		t.Fatalf("package deploy log missing the fake apt output: %q", pkgDone.Stdout)
	}

	// --- RS2: a foreign device token gets 404 downloading an org-A file --------
	orgB, err := st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	devB, devBToken, err := st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("upsert device B: %v", err)
	}
	plain := &http.Client{Timeout: 30 * time.Second}
	var dlErr errorJSON
	if code := doJSON(t, plain, http.MethodGet, fmt.Sprintf("%s/api/agent/v1/files/%d", ts.URL, file.ID), devBToken, nil, &dlErr, false); code != 404 || dlErr.Error != "not_found" {
		t.Fatalf("foreign device token download of org-A file: status %d error %q, want 404 not_found", code, dlErr.Error)
	}
	// The device's own org (A) can still download it, so the 404 is isolation,
	// not a broken file.
	if code := doJSON(t, plain, http.MethodGet, fmt.Sprintf("%s/api/agent/v1/files/%d", ts.URL, file.ID), cfg.DeviceToken, nil, nil, false); code != 200 {
		t.Fatalf("own device token download of its org file: status %d, want 200", code)
	}

	// --- RS2: an operator scoped to org A gets 404 on a deploy to an org-B device
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
	if code := deploy(op, devB.ID, map[string]any{"source": "package", "package": "htop"}, &scopedErr); code != 404 || scopedErr.Error != "not_found" {
		t.Fatalf("scoped operator deploy to org B device: status %d error %q, want 404 not_found", code, scopedErr.Error)
	}
	bCmdsAfter, _ := st.ListCommands(ctx, devB.ID, 500)
	if len(bCmdsAfter) != len(bCmdsBefore) {
		t.Fatalf("scoped 404 created %d command rows in org B", len(bCmdsAfter)-len(bCmdsBefore))
	}

	// --- android → 400 unsupported_on_platform --------------------------------
	var enrolled protocol.EnrollResponse
	if code := doJSON(t, plain, http.MethodPost, ts.URL+"/api/agent/v1/enroll", "", protocol.EnrollRequest{
		EnrollmentToken: token, DeviceUID: "android-deploy", Hostname: "Pixel 8",
		Platform: protocol.PlatformAndroid, Arch: "arm64", OSVersion: "Android 14 (34)",
		AgentVersion: "0.1.0", IntervalS: 60,
	}, &enrolled, false); code != 201 {
		t.Fatalf("android enroll: status %d", code)
	}
	var androidErr errorJSON
	if code := deploy(d, enrolled.DeviceID, map[string]any{"source": "package", "package": "htop"}, &androidErr); code != 400 || androidErr.Error != "unsupported_on_platform" {
		t.Fatalf("deploy on android: status %d error %q, want 400 unsupported_on_platform", code, androidErr.Error)
	}
}
