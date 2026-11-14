package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// patchPath is POST /api/v1/devices/{id}/patch.
func patchPath(id int64) string { return "/api/v1/devices/" + itoa(id) + "/patch" }

// online syncs a device once so its last_seen_at is the clock's now, making it
// online for the status computed at read time.
func (e *testEnv) online(token string) {
	e.t.Helper()
	if status, _, raw := e.sync(token, syncRequest(0)); status != 200 {
		e.t.Fatalf("sync to go online: %d %s", status, raw)
	}
}

// patchCommands returns the device's queued patch commands (none expected on the
// rejection paths, which must not create a command before they fail).
func (e *testEnv) patchCommands(deviceID int64) []store.Command {
	e.t.Helper()
	cmds, err := e.st.ListCommands(context.Background(), deviceID, 200)
	if err != nil {
		e.t.Fatalf("ListCommands(%d): %v", deviceID, err)
	}
	var out []store.Command
	for _, c := range cmds {
		if c.Kind == protocol.KindPatch {
			out = append(out, c)
		}
	}
	return out
}

// TestPatchEndpointRejections covers the patch endpoint's guards (PM1): role,
// CSRF and session; RS2 404 across orgs with no command or audit; android 400
// unsupported_on_platform; revoked 400 unsupported_on_device; offline 409; and
// the server-side ids-subset check. The successful queue path exercises the
// store's KindPatch insert (WP-STORE) and is proved end-to-end in internal/e2e.
func TestPatchEndpointRejections(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	// A linux device, brought online so the online guard is not what trips.
	devID, token := e.enroll("uid-lin", protocol.PlatformLinux)
	e.online(token)

	install := map[string]any{"action": "install", "mode": "security", "reboot": "never"}

	// --- session, CSRF and role ---------------------------------------------
	if resp, _ := e.do(http.MethodPost, patchPath(devID), install, nil); resp.StatusCode != 401 {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	if resp, body := e.do(http.MethodPost, patchPath(devID), install, withCookie(admin, nil)); resp.StatusCode != 403 || errorCode(t, body) != "csrf" {
		t.Fatalf("no csrf header: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(http.MethodPost, patchPath(devID), install, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 || errorCode(t, body) != "forbidden" {
		t.Fatalf("viewer: %d %s", resp.StatusCode, body)
	}

	// --- RS2: a scoped operator gets 404 on a device in an org it cannot see,
	// with no command created in B and no audit row written --------------------
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
	if resp, body := e.do(http.MethodPost, patchPath(devB.ID), install, withCookie(op, csrfHeader)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("scoped operator on org B device: %d %s", resp.StatusCode, body)
	}
	// An unknown id is the same 404 (indistinguishable from an invisible org).
	if resp, body := e.do(http.MethodPost, patchPath(999999), install, withCookie(op, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("unknown device: %d %s", resp.StatusCode, body)
	}
	if n := len(e.patchCommands(devB.ID)); n != 0 {
		t.Fatalf("org B must have no patch command, got %d", n)
	}
	if n := len(e.auditActions("patch.install_requested")) + len(e.auditActions("patch.scan_requested")); n != 0 {
		t.Fatalf("no patch audit row must exist after rejections, got %d", n)
	}

	// --- android: unsupported_on_platform ------------------------------------
	andID, andToken := e.enroll("uid-and", protocol.PlatformAndroid)
	e.online(andToken)
	if resp, body := e.do(http.MethodPost, patchPath(andID), install, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, body) != "unsupported_on_platform" {
		t.Fatalf("android: %d %s", resp.StatusCode, body)
	}

	// --- offline: device_offline ---------------------------------------------
	offID, offToken := e.enroll("uid-off", protocol.PlatformLinux)
	e.online(offToken)
	e.clock.advance(3*30*time.Second + 20*time.Second) // past 3*interval+15s
	if resp, body := e.do(http.MethodPost, patchPath(offID), install, withCookie(admin, csrfHeader)); resp.StatusCode != 409 || errorCode(t, body) != "device_offline" {
		t.Fatalf("offline: %d %s", resp.StatusCode, body)
	}

	// --- invalid shape: a bad mode is 400 invalid_request --------------------
	if resp, body := e.do(http.MethodPost, patchPath(devID), map[string]any{"mode": "bananas"}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("bad mode: %d %s", resp.StatusCode, body)
	}

	// --- revoked: unsupported_on_device --------------------------------------
	revID, revToken := e.enroll("uid-rev", protocol.PlatformLinux)
	e.online(revToken)
	if err := e.st.RevokeDevice(ctx, revID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if resp, body := e.do(http.MethodPost, patchPath(revID), install, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, body) != "unsupported_on_device" {
		t.Fatalf("revoked: %d %s", resp.StatusCode, body)
	}

	// --- ids not a subset of the pending list: 400 naming the unknown id ------
	report := protocol.PatchReport{
		CheckedAt: e.clock.now(), Manager: protocol.PatchManagerApt, Count: 2, SecurityCount: 1,
		Updates: []protocol.PatchUpdate{
			{Name: "openssl", Current: "3.0.1", New: "3.0.2", Security: true},
			{Name: "curl", Current: "8.0", New: "8.1"},
		},
	}
	if err := e.st.RecordPatches(ctx, devID, report); err != nil {
		t.Fatalf("RecordPatches: %v", err)
	}
	badIDs := map[string]any{"action": "install", "mode": "ids", "ids": []string{"openssl", "ghost"}}
	resp, body := e.do(http.MethodPost, patchPath(devID), badIDs, withCookie(admin, csrfHeader))
	if resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" || !strings.Contains(string(body), "ghost") {
		t.Fatalf("ids not a subset: %d %s", resp.StatusCode, body)
	}
	if n := len(e.patchCommands(devID)); n != 0 {
		t.Fatalf("a rejected ids install must create no command, got %d", n)
	}
}

// TestUnknownPatchIDs unit-tests the server-side subset check in isolation: it
// only flags unknown ids when the stored report has a usable pending list, and
// defers to the agent (returns nil) when there is no report to check against.
func TestUnknownPatchIDs(t *testing.T) {
	report := `{"manager":"apt","updates":[{"name":"openssl"},{"name":"curl"}]}`
	cases := []struct {
		name string
		json string
		ids  []string
		want []string
	}{
		{"all known", report, []string{"openssl", "curl"}, nil},
		{"one unknown", report, []string{"openssl", "ghost"}, []string{"ghost"}},
		{"unknowns sorted", report, []string{"zed", "abc"}, []string{"abc", "zed"}},
		{"no ids", report, nil, nil},
		{"empty report defers", "{}", []string{"openssl"}, nil},
		{"empty updates defers", `{"manager":"apt","updates":[]}`, []string{"openssl"}, nil},
		{"unreadable defers", "not json", []string{"openssl"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := unknownPatchIDs(c.json, c.ids)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("unknownPatchIDs(%q, %v) = %v, want %v", c.json, c.ids, got, c.want)
			}
		})
	}
}
