package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// updatePath is POST /api/v1/devices/{id}/update.
func updatePath(id int64) string { return "/api/v1/devices/" + itoa(id) + "/update" }

// useAgentManifest points the server's manifest library at a fresh MSP_AGENT_DIR
// holding a sampleManifest() agents.json (released version 0.2.0 for all three
// desktop files). The test env builds the server with an empty AgentDir, so the
// library is rebuilt here over the new directory. It returns the directory so a
// test can rewrite or remove agents.json to exercise a reload.
func (e *testEnv) useAgentManifest() string {
	e.t.Helper()
	dir := e.t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(sampleManifest()), 0o644); err != nil {
		e.t.Fatalf("write agents.json: %v", err)
	}
	e.srv.cfg.AgentDir = dir
	e.srv.manifests = newManifestLibrary(dir, e.srv.log)
	if err := e.srv.manifests.load(); err != nil {
		e.t.Fatalf("load manifest: %v", err)
	}
	return dir
}

// syncAtVersion syncs a device once, reporting a specific agent_version, so the
// stored agent_version (and the sync advertise comparison) reflect it.
func (e *testEnv) syncAtVersion(token, version string) protocol.SyncResponse {
	e.t.Helper()
	req := syncRequest(0)
	req.AgentVersion = version
	status, out, raw := e.sync(token, req)
	if status != 200 {
		e.t.Fatalf("sync at version %s: %d %s", version, status, raw)
	}
	return out
}

// updateCommands returns a device's queued update commands (none expected on the
// rejection paths, which must not create a command before they fail).
func (e *testEnv) updateCommands(deviceID int64) []store.Command {
	e.t.Helper()
	cmds, err := e.st.ListCommands(context.Background(), deviceID, 200)
	if err != nil {
		e.t.Fatalf("ListCommands(%d): %v", deviceID, err)
	}
	var out []store.Command
	for _, c := range cmds {
		if c.Kind == protocol.KindUpdate {
			out = append(out, c)
		}
	}
	return out
}

// TestUpdateEndpoint covers the self-update endpoint's guards and the happy path
// (AU1): role/CSRF/session; RS2 404 across orgs with no command or audit; android
// 400; revoked 400; no_update_available 409 (up-to-date device and self-update
// off); offline 409; and a successful update queue filled from the manifest with
// the agent.update_requested audit.
func TestUpdateEndpoint(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.useAgentManifest()
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	// A linux device (enrolled at 0.1.0, older than the manifest's 0.2.0), online.
	devID, token := e.enroll("uid-lin", protocol.PlatformLinux)
	e.online(token)

	// --- session / CSRF / role ------------------------------------------------
	if resp, _ := e.do(http.MethodPost, updatePath(devID), nil, nil); resp.StatusCode != 401 {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	if resp, b := e.do(http.MethodPost, updatePath(devID), nil, withCookie(admin, nil)); resp.StatusCode != 403 || errorCode(t, b) != "csrf" {
		t.Fatalf("no csrf: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do(http.MethodPost, updatePath(devID), nil, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 || errorCode(t, b) != "forbidden" {
		t.Fatalf("viewer: %d %s", resp.StatusCode, b)
	}

	// --- RS2: scoped operator on an org B device → 404, no command, no audit ---
	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	devB, _, err := e.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", AgentVersion: "0.1.0", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	op, _ := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{e.org.ID})
	if resp, b := e.do(http.MethodPost, updatePath(devB.ID), nil, withCookie(op, csrfHeader)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
		t.Fatalf("scoped operator on org B device: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.do(http.MethodPost, updatePath(999999), nil, withCookie(op, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("unknown device: %d", resp.StatusCode)
	}
	if n := len(e.updateCommands(devB.ID)); n != 0 {
		t.Fatalf("org B must have no update command, got %d", n)
	}
	if n := len(e.auditActions("agent.update_requested")); n != 0 {
		t.Fatalf("no update audit row must exist after rejections, got %d", n)
	}

	// --- android: unsupported_on_platform -------------------------------------
	andID, andToken := e.enroll("uid-and", protocol.PlatformAndroid)
	e.online(andToken)
	if resp, b := e.do(http.MethodPost, updatePath(andID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "unsupported_on_platform" {
		t.Fatalf("android: %d %s", resp.StatusCode, b)
	}

	// --- revoked: unsupported_on_device ---------------------------------------
	revID, revToken := e.enroll("uid-rev", protocol.PlatformLinux)
	e.online(revToken)
	if err := e.st.RevokeDevice(ctx, revID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if resp, b := e.do(http.MethodPost, updatePath(revID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "unsupported_on_device" {
		t.Fatalf("revoked: %d %s", resp.StatusCode, b)
	}

	// --- no_update_available: a device already on the released version --------
	upID, upToken := e.enroll("uid-up", protocol.PlatformLinux)
	e.syncAtVersion(upToken, "0.2.0") // report the manifest version; also brings it online
	if resp, b := e.do(http.MethodPost, updatePath(upID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 409 || errorCode(t, b) != "no_update_available" {
		t.Fatalf("up-to-date device: %d %s", resp.StatusCode, b)
	}

	// --- no_update_available: self-update off (no manifest at all) ------------
	e.srv.manifests = newManifestLibrary("", e.srv.log) // disable
	if resp, b := e.do(http.MethodPost, updatePath(devID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 409 || errorCode(t, b) != "no_update_available" {
		t.Fatalf("self-update off: %d %s", resp.StatusCode, b)
	}
	e.useAgentManifest() // re-enable for the rest

	// --- offline: device_offline (outdated device, so only the online check trips) ---
	offID, offToken := e.enroll("uid-off", protocol.PlatformLinux)
	e.online(offToken)
	e.clock.advance(3*30*time.Second + 20*time.Second)
	if resp, b := e.do(http.MethodPost, updatePath(offID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 409 || errorCode(t, b) != "device_offline" {
		t.Fatalf("offline: %d %s", resp.StatusCode, b)
	}

	// Nothing above should have queued an update command on devID yet.
	if n := len(e.updateCommands(devID)); n != 0 {
		t.Fatalf("rejections created %d update commands on devID, want 0", n)
	}

	// --- happy path: an update queues from the manifest -----------------------
	// The offline test advanced the shared clock, so re-online devID first.
	e.online(token)
	resp, body := e.do(http.MethodPost, updatePath(devID), nil, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("update: %d %s", resp.StatusCode, body)
	}
	c := decodeCommand(t, body)
	wantSHA := sampleManifestSHA()
	if c.Kind != protocol.KindUpdate || c.Version != "0.2.0" || c.URL != "/dl/msp-agent-linux-amd64" ||
		c.SHA256 != wantSHA || c.TimeoutS != updateTimeoutS {
		t.Fatalf("queued update = %+v", c)
	}

	// One update command and one agent.update_requested audit with from/to version.
	if n := len(e.updateCommands(devID)); n != 1 {
		t.Fatalf("devID update commands = %d, want 1", n)
	}
	audits := e.auditActions("agent.update_requested")
	if len(audits) != 1 {
		t.Fatalf("agent.update_requested audit rows = %d, want 1", len(audits))
	}
	a := audits[0]
	if a.ActorType != "user" || a.ActorID == nil || *a.ActorID != e.admin.ID || a.TargetID == nil || *a.TargetID != c.ID ||
		a.OrgID == nil || *a.OrgID != e.org.ID || a.Detail["from_version"] != "0.1.0" || a.Detail["to_version"] != "0.2.0" {
		t.Fatalf("agent.update_requested audit %+v", a)
	}
}

// sampleManifestSHA is the (single) sha256 every sampleManifest() file carries.
func sampleManifestSHA() string {
	m, err := ParseManifest([]byte(sampleManifest()))
	if err != nil {
		panic(err)
	}
	return m.Files["linux-amd64"].SHA256
}

// TestAgentReleaseEndpoint: GET /api/v1/agent-release returns the manifest for a
// viewer, and an empty manifest when self-update is off.
func TestAgentReleaseEndpoint(t *testing.T) {
	e := newTestEnv(t)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	// Off (no agents.json): empty manifest, version "".
	resp, body := e.do(http.MethodGet, "/api/v1/agent-release", nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("agent-release off: %d %s", resp.StatusCode, body)
	}
	var off Manifest
	if err := json.Unmarshal(body, &off); err != nil || off.Version != "" {
		t.Fatalf("agent-release off body %s (%v)", body, err)
	}

	// Needs a session.
	if resp, _ := e.do(http.MethodGet, "/api/v1/agent-release", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("agent-release no session: %d", resp.StatusCode)
	}

	// With a manifest: version and the three files.
	e.useAgentManifest()
	resp, body = e.do(http.MethodGet, "/api/v1/agent-release", nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("agent-release on: %d %s", resp.StatusCode, body)
	}
	var on Manifest
	if err := json.Unmarshal(body, &on); err != nil || on.Version != "0.2.0" || len(on.Files) != 3 {
		t.Fatalf("agent-release on body %s (%v)", body, err)
	}
	if f, ok := on.Files["windows-amd64"]; !ok || f.Name != "msp-agent-windows-amd64.exe" {
		t.Fatalf("windows entry %+v ok=%v", f, ok)
	}
}

// TestSyncAdvertisesAgentUpdate is the AU1 sync-advertise proof: a desktop agent
// older than the released version gets an agent_update block on its sync; once it
// reports the released version it no longer does; android never does; and a
// revoked device is not advertised to.
func TestSyncAdvertisesAgentUpdate(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.useAgentManifest()

	// Linux at 0.1.0 (older than 0.2.0): the sync carries agent_update.
	linID, linToken := e.enroll("uid-lin", protocol.PlatformLinux)
	out := e.syncAtVersion(linToken, "0.1.0")
	if out.AgentUpdate == nil || out.AgentUpdate.Version != "0.2.0" ||
		out.AgentUpdate.URL != "/dl/msp-agent-linux-amd64" || out.AgentUpdate.SHA256 != sampleManifestSHA() {
		t.Fatalf("expected agent_update for an outdated linux agent, got %+v", out.AgentUpdate)
	}
	if err := out.AgentUpdate.Validate(); err != nil {
		t.Fatalf("advertised agent_update must validate: %v", err)
	}

	// Once the agent reports 0.2.0, nothing is advertised.
	if out := e.syncAtVersion(linToken, "0.2.0"); out.AgentUpdate != nil {
		t.Fatalf("up-to-date agent must get no agent_update, got %+v", out.AgentUpdate)
	}

	// Android never gets it (KindUpdate is desktop only).
	_, andToken := e.enroll("uid-and", protocol.PlatformAndroid)
	if out := e.syncAtVersion(andToken, "0.1.0"); out.AgentUpdate != nil {
		t.Fatalf("android must get no agent_update, got %+v", out.AgentUpdate)
	}

	// A revoked device is left alone even if outdated.
	if err := e.st.RevokeDevice(ctx, linID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	// A revoked token is rejected at auth, so check the helper directly on the row.
	dev := e.device(linID)
	if up := e.srv.agentUpdateFor(dev, "0.1.0"); up != nil {
		t.Fatalf("revoked device must not be advertised to, got %+v", up)
	}

	// Self-update off: no advertise at all.
	e.srv.manifests = newManifestLibrary("", e.srv.log)
	dev2 := e.device(linID)
	dev2.Revoked = false
	if up := e.srv.agentUpdateFor(dev2, "0.1.0"); up != nil {
		t.Fatalf("self-update off must not advertise, got %+v", up)
	}
}
