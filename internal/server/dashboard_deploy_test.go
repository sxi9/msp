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

// decodeCommand decodes a {"command":Command} response body.
func decodeCommand(t *testing.T, body []byte) store.Command {
	t.Helper()
	var out struct {
		Command store.Command `json:"command"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode command body %s: %v", body, err)
	}
	return out.Command
}

// deployPath is POST /api/v1/devices/{id}/deploy.
func deployPath(id int64) string { return "/api/v1/devices/" + itoa(id) + "/deploy" }

// deployCommands returns a device's queued deploy commands (none expected on the
// rejection paths, which must not create a command before they fail).
func (e *testEnv) deployCommands(deviceID int64) []store.Command {
	e.t.Helper()
	cmds, err := e.st.ListCommands(context.Background(), deviceID, 200)
	if err != nil {
		e.t.Fatalf("ListCommands(%d): %v", deviceID, err)
	}
	var out []store.Command
	for _, c := range cmds {
		if c.Kind == protocol.KindDeploy {
			out = append(out, c)
		}
	}
	return out
}

// TestDeployEndpoint covers the deploy endpoint's guards and the two happy paths
// (SD1): role/CSRF/session; RS2 404 across orgs with no command or audit; android
// 400; revoked 400; offline 409; bad source/dest/package 400; file-not-found and
// cross-org-file 404; and a successful file deploy and package deploy queue.
func TestDeployEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.useFilesDir()
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	// A linux device in the env's org, brought online.
	devID, token := e.enroll("uid-lin", protocol.PlatformLinux)
	e.online(token)

	// A global library file to deploy.
	_, body := e.uploadFile(withCookie(admin, csrfHeader), "installer.sh", nil, "installer.sh", []byte("#!/bin/sh\necho hi\n"))
	gf := fileFrom(t, body)
	fileDeploy := map[string]any{"source": "file", "file_id": gf.ID, "dest": "/opt/acme/installer.sh", "run": true, "run_args": []string{"--silent"}}

	// --- session / CSRF / role ------------------------------------------------
	if resp, _ := e.do(http.MethodPost, deployPath(devID), fileDeploy, nil); resp.StatusCode != 401 {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	if resp, b := e.do(http.MethodPost, deployPath(devID), fileDeploy, withCookie(admin, nil)); resp.StatusCode != 403 || errorCode(t, b) != "csrf" {
		t.Fatalf("no csrf: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do(http.MethodPost, deployPath(devID), fileDeploy, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 || errorCode(t, b) != "forbidden" {
		t.Fatalf("viewer: %d %s", resp.StatusCode, b)
	}

	// --- RS2: scoped operator on an org B device → 404, no command, no audit ---
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
	op, _ := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{e.org.ID})
	pkgDeployB := map[string]any{"source": "package", "package": "htop"}
	if resp, b := e.do(http.MethodPost, deployPath(devB.ID), pkgDeployB, withCookie(op, csrfHeader)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
		t.Fatalf("scoped operator on org B device: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.do(http.MethodPost, deployPath(999999), pkgDeployB, withCookie(op, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("unknown device: %d", resp.StatusCode)
	}
	if n := len(e.deployCommands(devB.ID)); n != 0 {
		t.Fatalf("org B must have no deploy command, got %d", n)
	}
	if n := len(e.auditActions("deploy.requested")); n != 0 {
		t.Fatalf("no deploy audit row must exist after rejections, got %d", n)
	}

	// --- android: unsupported_on_platform -------------------------------------
	andID, andToken := e.enroll("uid-and", protocol.PlatformAndroid)
	e.online(andToken)
	if resp, b := e.do(http.MethodPost, deployPath(andID), map[string]any{"source": "package", "package": "htop"}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "unsupported_on_platform" {
		t.Fatalf("android: %d %s", resp.StatusCode, b)
	}

	// --- revoked: unsupported_on_device ---------------------------------------
	revID, revToken := e.enroll("uid-rev", protocol.PlatformLinux)
	e.online(revToken)
	if err := e.st.RevokeDevice(ctx, revID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if resp, b := e.do(http.MethodPost, deployPath(revID), fileDeploy, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "unsupported_on_device" {
		t.Fatalf("revoked: %d %s", resp.StatusCode, b)
	}

	// --- offline: device_offline (valid file, so only the online check trips) --
	offID, offToken := e.enroll("uid-off", protocol.PlatformLinux)
	e.online(offToken)
	e.clock.advance(3*30*time.Second + 20*time.Second)
	if resp, b := e.do(http.MethodPost, deployPath(offID), fileDeploy, withCookie(admin, csrfHeader)); resp.StatusCode != 409 || errorCode(t, b) != "device_offline" {
		t.Fatalf("offline: %d %s", resp.StatusCode, b)
	}

	// --- shape: bad source, bad dest, bad package name → 400 ------------------
	if resp, b := e.do(http.MethodPost, deployPath(devID), map[string]any{"source": "bananas"}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "invalid_request" {
		t.Fatalf("bad source: %d %s", resp.StatusCode, b)
	}
	badDest := map[string]any{"source": "file", "file_id": gf.ID, "dest": "/opt/../etc/passwd"}
	if resp, b := e.do(http.MethodPost, deployPath(devID), badDest, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "invalid_request" {
		t.Fatalf("dest with ..: %d %s", resp.StatusCode, b)
	}
	badPkg := map[string]any{"source": "package", "package": "evil;rm -rf"}
	if resp, b := e.do(http.MethodPost, deployPath(devID), badPkg, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "invalid_request" {
		t.Fatalf("bad package name: %d %s", resp.StatusCode, b)
	}

	// --- file not found and cross-org file → 404 ------------------------------
	if resp, b := e.do(http.MethodPost, deployPath(devID), map[string]any{"source": "file", "file_id": 999999, "dest": "/opt/x"}, withCookie(admin, csrfHeader)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
		t.Fatalf("file not found: %d %s", resp.StatusCode, b)
	}
	_, body = e.uploadFile(withCookie(admin, csrfHeader), "beta.sh", &orgB.ID, "beta.sh", []byte("b"))
	bf := fileFrom(t, body)
	crossOrg := map[string]any{"source": "file", "file_id": bf.ID, "dest": "/opt/x"}
	if resp, b := e.do(http.MethodPost, deployPath(devID), crossOrg, withCookie(admin, csrfHeader)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
		t.Fatalf("cross-org file: %d %s", resp.StatusCode, b)
	}

	// Nothing above should have queued a deploy command on devID yet.
	if n := len(e.deployCommands(devID)); n != 0 {
		t.Fatalf("rejections created %d deploy commands on devID, want 0", n)
	}

	// --- happy path: a file deploy queues with the stored hash ----------------
	// The offline test advanced the shared clock, so re-online devID first.
	e.online(token)
	resp, body := e.do(http.MethodPost, deployPath(devID), fileDeploy, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("file deploy: %d %s", resp.StatusCode, body)
	}
	c := decodeCommand(t, body)
	if c.Kind != protocol.KindDeploy || c.Source != "file" || c.FileID != gf.ID || c.SHA256 != gf.SHA256 ||
		c.Dest != "/opt/acme/installer.sh" || !c.Run || len(c.RunArgs) != 1 || c.RunArgs[0] != "--silent" {
		t.Fatalf("queued file deploy = %+v", c)
	}

	// --- happy path: a package deploy -----------------------------------------
	resp, body = e.do(http.MethodPost, deployPath(devID), map[string]any{"source": "package", "package": "htop"}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("package deploy: %d %s", resp.StatusCode, body)
	}
	pc := decodeCommand(t, body)
	if pc.Source != "package" || pc.Package != "htop" || pc.FileID != 0 {
		t.Fatalf("queued package deploy = %+v", pc)
	}

	// Two deploy commands and two deploy.requested audit rows.
	if n := len(e.deployCommands(devID)); n != 2 {
		t.Fatalf("devID deploy commands = %d, want 2", n)
	}
	if n := len(e.auditActions("deploy.requested")); n != 2 {
		t.Fatalf("deploy.requested audit rows = %d, want 2", n)
	}
}
