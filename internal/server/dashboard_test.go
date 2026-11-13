package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// dashLogin logs a user in and returns the session cookie.
func dashLogin(e *testEnv, email, password string) *http.Cookie {
	e.t.Helper()
	resp, body, c := e.login(email, password)
	if resp.StatusCode != http.StatusOK || c == nil {
		e.t.Fatalf("login %s: %d %s", email, resp.StatusCode, body)
	}
	return c
}

// dashAdmin logs the test admin in.
func dashAdmin(e *testEnv) *http.Cookie {
	return dashLogin(e, "admin@example.com", testAdminPassword)
}

// dashUser creates a user with the given role and logs them in.
func dashUser(e *testEnv, email, role string) *http.Cookie {
	e.t.Helper()
	const pw = "role-pass-12345"
	hash, err := e.srv.hashPassword(pw)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.CreateUser(context.Background(), email, hash, role); err != nil {
		e.t.Fatalf("CreateUser %s: %v", email, err)
	}
	return dashLogin(e, email, pw)
}

// dashDecode unmarshals a JSON body into a generic map.
func dashDecode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return m
}

func dashKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// dashDeviceKeys is the exact DEVICE key set from docs/PROTOCOL.md.
var dashDeviceKeys = []string{
	"agent_version", "arch", "battery_charging", "battery_pct", "can_run_commands", "cpu_pct",
	"device_uid", "disk_used_pct", "enrolled_at", "hostname", "id", "interval_s", "last_ip",
	"last_seen_at", "mem_used_pct", "open_alerts", "org_id", "org_name", "os_version", "platform",
	"revoked", "status", "uptime_s",
}

func TestDashboardAuthAndRoles(t *testing.T) {
	e := newTestEnv(t)
	devID, _ := e.enroll("uid-1", protocol.PlatformLinux)

	// No session at all.
	if resp, body := e.do(http.MethodGet, "/api/v1/devices", nil, nil); resp.StatusCode != 401 || errorCode(t, body) != "unauthorized" {
		t.Fatalf("unauthenticated devices: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/summary", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated summary: %d", resp.StatusCode)
	}

	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)
	operator := dashUser(e, "operator@example.com", RoleOperator)
	cmdPath := "/api/v1/devices/" + itoa(devID) + "/commands"
	exec := map[string]any{"kind": "exec", "argv": []string{"uname", "-a"}}

	// Viewer can read but not mutate.
	if resp, _ := e.do(http.MethodGet, "/api/v1/devices", nil, withCookie(viewer, nil)); resp.StatusCode != 200 {
		t.Fatalf("viewer list devices: %d", resp.StatusCode)
	}
	if resp, body := e.do(http.MethodPost, cmdPath, exec, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 || errorCode(t, body) != "forbidden" {
		t.Fatalf("viewer POST command: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/orgs", map[string]string{"name": "X"}, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("viewer POST org: %d", resp.StatusCode)
	}

	// CSRF header is mandatory on mutations, even for admins.
	if resp, body := e.do(http.MethodPost, cmdPath, exec, withCookie(admin, nil)); resp.StatusCode != 403 || errorCode(t, body) != "csrf" {
		t.Fatalf("POST without X-Requested-With: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodDelete, "/api/v1/devices/"+itoa(devID), nil, withCookie(admin, nil)); resp.StatusCode != 403 {
		t.Fatalf("DELETE without X-Requested-With: %d", resp.StatusCode)
	}

	// Operator may queue commands but not administer.
	if resp, body := e.do(http.MethodPost, cmdPath, exec, withCookie(operator, csrfHeader)); resp.StatusCode != 201 {
		t.Fatalf("operator POST command: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/devices/"+itoa(devID)+"/revoke", nil, withCookie(operator, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("operator revoke device: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/users", nil, withCookie(operator, nil)); resp.StatusCode != 403 {
		t.Fatalf("operator list users: %d", resp.StatusCode)
	}

	// Summary works for any role.
	resp, body := e.do(http.MethodGet, "/api/v1/summary", nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("summary: %d %s", resp.StatusCode, body)
	}
	var sum store.Summary
	if err := json.Unmarshal(body, &sum); err != nil || sum.Orgs != 1 || sum.Devices != 1 || sum.Pending != 1 {
		t.Fatalf("summary body %s (%v)", body, err)
	}

	// Unknown API routes are 404 JSON, not the SPA.
	if resp, body := e.do(http.MethodGet, "/api/v1/nope", nil, withCookie(admin, nil)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("unknown api route: %d %s", resp.StatusCode, body)
	}
}

func TestDashboardDevicesShape(t *testing.T) {
	e := newTestEnv(t)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	if status, _, body := e.sync(token, syncRequest(0)); status != 200 {
		t.Fatalf("sync: %d %s", status, body)
	}
	androidID, _ := e.enroll("uid-android", protocol.PlatformAndroid)
	admin := dashAdmin(e)

	resp, body := e.do(http.MethodGet, "/api/v1/devices", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("list devices: %d %s", resp.StatusCode, body)
	}
	list := dashDecode(t, body)
	devices, _ := list["devices"].([]any)
	if len(devices) != 2 {
		t.Fatalf("want 2 devices, got %s", body)
	}
	for _, d := range devices {
		got := dashKeys(d.(map[string]any))
		if strings.Join(got, ",") != strings.Join(dashDeviceKeys, ",") {
			t.Fatalf("device keys\n got %v\nwant %v", got, dashDeviceKeys)
		}
	}
	for _, d := range devices {
		m := d.(map[string]any)
		switch int64(m["id"].(float64)) {
		case devID:
			if m["status"] != "online" || m["can_run_commands"] != true || m["cpu_pct"].(float64) != 12.5 || m["org_name"] != "Acme Dental" {
				t.Fatalf("linux device %v", m)
			}
		case androidID:
			if m["status"] != "pending" || m["can_run_commands"] != false || m["platform"] != "android" {
				t.Fatalf("android device %v", m)
			}
		}
	}

	// Filters.
	resp, body = e.do(http.MethodGet, "/api/v1/devices?status=online", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || len(dashDecode(t, body)["devices"].([]any)) != 1 {
		t.Fatalf("status=online: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices?q=android", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || len(dashDecode(t, body)["devices"].([]any)) != 1 {
		t.Fatalf("q=android: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices?org_id="+itoa(e.org.ID+1), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || len(dashDecode(t, body)["devices"].([]any)) != 0 {
		t.Fatalf("org_id other: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(http.MethodGet, "/api/v1/devices?status=bogus", nil, withCookie(admin, nil)); resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("status=bogus: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/devices?org_id=abc", nil, withCookie(admin, nil)); resp.StatusCode != 400 {
		t.Fatalf("org_id=abc: %d", resp.StatusCode)
	}

	// Device detail shape.
	queued := e.queue(devID, "echo", "secret-output")
	res := protocol.CommandResult{ExitCode: 0, Stdout: "secret-output\n", StartedAt: e.clock.now(), FinishedAt: e.clock.now()}
	if _, _, err := claimAndComplete(e, devID, token, queued.ID, res); err != nil {
		t.Fatal(err)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(devID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("get device: %d %s", resp.StatusCode, body)
	}
	detail := dashDecode(t, body)
	if got := dashKeys(detail); strings.Join(got, ",") != "alerts,commands,device,facts,metrics" {
		t.Fatalf("detail keys %v", got)
	}
	dev := detail["device"].(map[string]any)
	if dev["hostname"] != "ws-uid-1" || dev["status"] != "online" {
		t.Fatalf("detail device %v", dev)
	}
	facts := detail["facts"].(map[string]any)
	if facts["kernel"] != "6.8.0" || facts["cpu_count"].(float64) != 4 {
		t.Fatalf("detail facts %v", facts)
	}
	metrics := detail["metrics"].(map[string]any)
	if metrics["mem_total_mb"].(float64) != 16000 {
		t.Fatalf("detail metrics %v", metrics)
	}
	if _, ok := detail["alerts"].([]any); !ok {
		t.Fatalf("detail alerts %v", detail["alerts"])
	}
	cmds := detail["commands"].([]any)
	if len(cmds) != 1 {
		t.Fatalf("detail commands %v", cmds)
	}
	c0 := cmds[0].(map[string]any)
	if c0["status"] != "done" || c0["stdout"] != "" || c0["kind"] != "exec" {
		t.Fatalf("detail command must have blank output: %v", c0)
	}
	// The full output is reachable through GET /api/v1/commands/{id}.
	resp, body = e.do(http.MethodGet, "/api/v1/commands/"+itoa(queued.ID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"stdout":"secret-output\n"`) {
		t.Fatalf("get command: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(http.MethodGet, "/api/v1/commands/999999", nil, withCookie(admin, nil)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("missing command: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/devices/999999", nil, withCookie(admin, nil)); resp.StatusCode != 404 {
		t.Fatalf("missing device: %d", resp.StatusCode)
	}

	// Heartbeats: one sample so far, oldest first, limit respected.
	e.clock.advance(2 * time.Second)
	if status, _, _ := e.sync(token, syncRequest(0)); status != 200 {
		t.Fatalf("second sync: %d", status)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(devID)+"/heartbeats?limit=1", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeats: %d %s", resp.StatusCode, body)
	}
	hbs := dashDecode(t, body)["heartbeats"].([]any)
	if len(hbs) != 1 {
		t.Fatalf("heartbeats limit=1 gave %d rows", len(hbs))
	}
	if got := dashKeys(hbs[0].(map[string]any)); strings.Join(got, ",") != "at,battery_pct,cpu_pct,disk_used_pct,mem_used_pct,uptime_s" {
		t.Fatalf("heartbeat keys %v", got)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(devID)+"/heartbeats", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || len(dashDecode(t, body)["heartbeats"].([]any)) != 2 {
		t.Fatalf("heartbeats default: %d %s", resp.StatusCode, body)
	}
}

// claimAndComplete lets the agent claim its queued commands and post one
// result, the way the real agent would.
func claimAndComplete(e *testEnv, devID int64, token string, cmdID int64, res protocol.CommandResult) (int, protocol.SyncResponse, error) {
	status, out, _ := e.sync(token, syncRequest(0))
	if status != 200 {
		e.t.Fatalf("claim sync: %d", status)
	}
	resp, body := e.do(http.MethodPost, "/api/agent/v1/commands/"+itoa(cmdID)+"/result", res, bearer(token))
	if resp.StatusCode != 204 {
		e.t.Fatalf("post result: %d %s", resp.StatusCode, body)
	}
	return status, out, nil
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func TestDashboardCommandValidation(t *testing.T) {
	e := newTestEnv(t)
	linuxID, _ := e.enroll("uid-linux", protocol.PlatformLinux)
	androidID, _ := e.enroll("uid-android", protocol.PlatformAndroid)
	admin := dashAdmin(e)
	post := func(devID int64, body any) (int, []byte) {
		resp, b := e.do(http.MethodPost, "/api/v1/devices/"+itoa(devID)+"/commands", body, withCookie(admin, csrfHeader))
		return resp.StatusCode, b
	}

	// Empty argv: 400 invalid_request with the Validate() detail.
	status, body := post(linuxID, map[string]any{"kind": "exec", "argv": []string{}})
	if status != 400 || errorCode(t, body) != "invalid_request" || !strings.Contains(string(body), "argv") {
		t.Fatalf("empty argv: %d %s", status, body)
	}
	// Unknown kind.
	if status, body := post(linuxID, map[string]any{"kind": "reboot"}); status != 400 || errorCode(t, body) != "invalid_request" || !strings.Contains(string(body), "unknown kind") {
		t.Fatalf("unknown kind: %d %s", status, body)
	}
	// Malformed JSON.
	if status, body := post(linuxID, "{not json"); status != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("bad json: %d %s", status, body)
	}
	// Script with powershell on a linux device.
	status, body = post(linuxID, map[string]any{"kind": "script", "shell": "powershell", "body": "Get-Date"})
	if status != 400 || errorCode(t, body) != "invalid_request" || !strings.Contains(string(body), "shell powershell not available on linux") {
		t.Fatalf("powershell on linux: %d %s", status, body)
	}
	// exec on android: unsupported_on_platform (A1).
	status, body = post(androidID, map[string]any{"kind": "exec", "argv": []string{"id"}})
	if status != 400 || errorCode(t, body) != "unsupported_on_platform" {
		t.Fatalf("exec on android: %d %s", status, body)
	}
	if status, body := post(androidID, map[string]any{"kind": "script", "shell": "sh", "body": "id"}); status != 400 || errorCode(t, body) != "unsupported_on_platform" {
		t.Fatalf("script on android: %d %s", status, body)
	}
	// Missing device.
	if status, body := post(999999, map[string]any{"kind": "ping"}); status != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("missing device: %d %s", status, body)
	}
	// Timeout out of range.
	if status, body := post(linuxID, map[string]any{"kind": "ping", "timeout_s": 99999}); status != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("timeout range: %d %s", status, body)
	}

	// Happy path: parked sync wakes, 201 queued, audit row with argv.
	wake, unsub := e.srv.notifier.Subscribe(linuxID)
	defer unsub()
	status, body = post(linuxID, map[string]any{"kind": "exec", "argv": []string{"echo", "a;id"}, "timeout_s": 30})
	if status != 201 {
		t.Fatalf("happy path: %d %s", status, body)
	}
	var out struct {
		Command store.Command `json:"command"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	c := out.Command
	if c.Status != "queued" || c.Kind != "exec" || len(c.Argv) != 2 || c.Argv[1] != "a;id" || c.TimeoutS != 30 || c.DeviceID != linuxID || c.CreatedBy != e.admin.ID || c.CreatedByEmail != "admin@example.com" {
		t.Fatalf("created command %+v", c)
	}
	if got := dashKeys(dashDecode(t, body)["command"].(map[string]any)); strings.Join(got, ",") !=
		"argv,body,created_at,created_by,created_by_email,device_id,error,exit_code,finished_at,id,kind,org_id,sent_at,shell,started_at,status,stderr,stdout,text,timeout_s,truncated" {
		t.Fatalf("command keys %v", got)
	}
	select {
	case <-wake:
	case <-time.After(2 * time.Second):
		t.Fatal("parked subscriber was not woken by the queued command")
	}
	audits := e.auditActions("command.queued")
	if len(audits) != 1 {
		t.Fatalf("command.queued audit rows: %d", len(audits))
	}
	a := audits[0]
	if a.ActorType != "user" || a.ActorID == nil || *a.ActorID != e.admin.ID || a.TargetID == nil || *a.TargetID != c.ID || a.OrgID == nil || *a.OrgID != e.org.ID {
		t.Fatalf("command.queued audit %+v", a)
	}
	argv, _ := a.Detail["argv"].([]any)
	if len(argv) != 2 || argv[1] != "a;id" || a.Detail["kind"] != "exec" {
		t.Fatalf("command.queued detail %v", a.Detail)
	}

	// Script: audit carries sha256 + head, never the body verbatim as a field.
	status, body = post(linuxID, map[string]any{"kind": "script", "shell": "sh", "body": "df -h\nuptime\n", "timeout_s": 120})
	if status != 201 {
		t.Fatalf("script: %d %s", status, body)
	}
	audits = e.auditActions("command.queued")
	sa := audits[0] // newest first
	if sa.Detail["shell"] != "sh" || sa.Detail["body_head"] != "df -h\nuptime\n" || len(sa.Detail["body_sha256"].(string)) != 64 {
		t.Fatalf("script audit detail %v", sa.Detail)
	}

	// Built-in kinds on android: lock writes device.lock_requested as well.
	status, body = post(androidID, map[string]any{"kind": "lock"})
	if status != 201 {
		t.Fatalf("lock on android: %d %s", status, body)
	}
	locks := e.auditActions("device.lock_requested")
	if len(locks) != 1 || locks[0].ActorType != "user" || *locks[0].ActorID != e.admin.ID || *locks[0].TargetID != androidID {
		t.Fatalf("device.lock_requested audit %+v", locks)
	}
	status, body = post(androidID, map[string]any{"kind": "message", "text": "Please call the helpdesk"})
	if status != 201 || !strings.Contains(string(body), `"text":"Please call the helpdesk"`) {
		t.Fatalf("message on android: %d %s", status, body)
	}
	if status, body := post(androidID, map[string]any{"kind": "message"}); status != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("empty message: %d %s", status, body)
	}
	if status, body := post(linuxID, map[string]any{"kind": "ping"}); status != 201 {
		t.Fatalf("ping on linux: %d %s", status, body)
	}

	// Revoked device: any kind is unsupported_on_device.
	if resp, _ := e.do(http.MethodPost, "/api/v1/devices/"+itoa(linuxID)+"/revoke", nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if status, body := post(linuxID, map[string]any{"kind": "ping"}); status != 400 || errorCode(t, body) != "unsupported_on_device" {
		t.Fatalf("ping on revoked: %d %s", status, body)
	}
	if rev := e.auditActions("device.revoked"); len(rev) != 1 || rev[0].Detail["hostname"] != "ws-uid-linux" {
		t.Fatalf("device.revoked audit %+v", rev)
	}

	// Command list for the device, newest first, limit honoured.
	resp, body := e.do(http.MethodGet, "/api/v1/devices/"+itoa(linuxID)+"/commands?limit=1", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("list commands: %d %s", resp.StatusCode, body)
	}
	cmds := dashDecode(t, body)["commands"].([]any)
	if len(cmds) != 1 || cmds[0].(map[string]any)["kind"] != "ping" {
		t.Fatalf("list commands limit=1: %s", body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(linuxID)+"/commands", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || len(dashDecode(t, body)["commands"].([]any)) != 3 {
		t.Fatalf("list commands: %d %s", resp.StatusCode, body)
	}
}

func TestDashboardOrgsAndTokens(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	operator := dashUser(e, "operator@example.com", RoleOperator)

	// Create org, duplicate conflicts, validation.
	resp, body := e.do(http.MethodPost, "/api/v1/orgs", map[string]string{"name": "  Beta Clinic  "}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create org: %d %s", resp.StatusCode, body)
	}
	var created struct {
		Org store.Org `json:"org"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.Org.Name != "Beta Clinic" || created.Org.ID == 0 {
		t.Fatalf("create org body %s (%v)", body, err)
	}
	if resp, body := e.do(http.MethodPost, "/api/v1/orgs", map[string]string{"name": "Beta Clinic"}, withCookie(admin, csrfHeader)); resp.StatusCode != 409 || errorCode(t, body) != "conflict" {
		t.Fatalf("duplicate org: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/orgs", map[string]string{"name": "   "}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 {
		t.Fatalf("empty org name: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/orgs", map[string]string{"name": strings.Repeat("x", 101)}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 {
		t.Fatalf("long org name: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/orgs", map[string]string{"name": "Op Org"}, withCookie(operator, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("operator create org: %d", resp.StatusCode)
	}
	if a := e.auditActions("org.created"); len(a) != 1 || a[0].OrgID == nil || *a[0].OrgID != created.Org.ID {
		t.Fatalf("org.created audit %+v", a)
	}

	// List includes counts.
	e.enroll("uid-1", protocol.PlatformLinux)
	resp, body = e.do(http.MethodGet, "/api/v1/orgs", nil, withCookie(operator, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("list orgs: %d %s", resp.StatusCode, body)
	}
	var list struct {
		Orgs []store.Org `json:"orgs"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Orgs) != 2 || list.Orgs[0].Name != "Acme Dental" || list.Orgs[0].DeviceCount != 1 {
		t.Fatalf("list orgs body %s (%v)", body, err)
	}

	// Token creation returns the plaintext once with install lines.
	tokPath := "/api/v1/orgs/" + itoa(e.org.ID) + "/tokens"
	resp, body = e.do(http.MethodPost, tokPath, map[string]any{"label": "front desk", "max_uses": 3, "ttl_hours": 24}, withCookie(operator, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create token: %d %s", resp.StatusCode, body)
	}
	var tok struct {
		Token     store.EnrollmentToken `json:"token"`
		Plaintext string                `json:"plaintext"`
		Install   map[string]string     `json:"install"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Plaintext, "et_") || len(tok.Plaintext) != 46 {
		t.Fatalf("plaintext %q", tok.Plaintext)
	}
	if tok.Token.Label != "front desk" || tok.Token.MaxUses != 3 || tok.Token.TokenPrefix != tok.Plaintext[:10] || tok.Token.CreatedBy == nil {
		t.Fatalf("token row %+v", tok.Token)
	}
	if tok.Token.ExpiresAt.Sub(e.clock.now()) != 24*time.Hour {
		t.Fatalf("expires_at %v", tok.Token.ExpiresAt)
	}
	base := e.ts.URL
	if tok.Install["linux"] != "curl -fsSLo /tmp/msp-install.sh "+base+"/dl/install.sh && sudo bash /tmp/msp-install.sh --server "+base+" --token "+tok.Plaintext {
		t.Fatalf("install.linux %q", tok.Install["linux"])
	}
	if !strings.Contains(tok.Install["windows"], tok.Plaintext) || !strings.Contains(tok.Install["windows"], "irm "+base+"/dl/install.ps1") {
		t.Fatalf("install.windows %q", tok.Install["windows"])
	}
	if !strings.Contains(tok.Install["android"], tok.Plaintext) || !strings.Contains(tok.Install["android"], base+"/dl/msp-agent.apk") {
		t.Fatalf("install.android %q", tok.Install["android"])
	}
	if a := e.auditActions("token.created"); len(a) != 1 || a[0].Detail["label"] != "front desk" || a[0].Detail["max_uses"].(float64) != 3 || a[0].Detail["expires_at"] == nil {
		t.Fatalf("token.created audit %+v", a)
	}

	// The plaintext enrolls a device; the org page shows the row but never the plaintext.
	req := enrollRequest("uid-tok", protocol.PlatformLinux)
	req.EnrollmentToken = tok.Plaintext
	if resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil); resp.StatusCode != 201 {
		t.Fatalf("enroll with new token: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/orgs/"+itoa(e.org.ID), nil, withCookie(operator, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("get org: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), tok.Plaintext) {
		t.Fatal("org page leaks the token plaintext")
	}
	var org struct {
		Org    store.Org               `json:"org"`
		Tokens []store.EnrollmentToken `json:"tokens"`
	}
	if err := json.Unmarshal(body, &org); err != nil || org.Org.ID != e.org.ID || org.Org.DeviceCount != 2 || len(org.Tokens) != 2 {
		t.Fatalf("org body %s (%v)", body, err)
	}
	var row *store.EnrollmentToken
	for i := range org.Tokens {
		if org.Tokens[i].ID == tok.Token.ID {
			row = &org.Tokens[i]
		}
	}
	if row == nil || row.Uses != 1 || row.RevokedAt != nil {
		t.Fatalf("token row after enroll %+v", row)
	}

	// Defaults and range checks.
	resp, body = e.do(http.MethodPost, tokPath, map[string]any{}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create token defaults: %d %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.Token.MaxUses != 10 || tok.Token.ExpiresAt.Sub(e.clock.now()) != 168*time.Hour {
		t.Fatalf("token defaults %+v (%v)", tok.Token, err)
	}
	if resp, _ := e.do(http.MethodPost, tokPath, map[string]any{"max_uses": 1001}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 {
		t.Fatalf("max_uses 1001: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodPost, tokPath, map[string]any{"ttl_hours": 9000}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 {
		t.Fatalf("ttl_hours 9000: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/orgs/999999/tokens", map[string]any{}, withCookie(admin, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("token for missing org: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/orgs/999999", nil, withCookie(admin, nil)); resp.StatusCode != 404 {
		t.Fatalf("missing org: %d", resp.StatusCode)
	}

	// Revoke: the token stops enrolling; wrong org is 404.
	revokePath := tokPath + "/" + itoa(row.ID) + "/revoke"
	if resp, _ := e.do(http.MethodPost, "/api/v1/orgs/"+itoa(created.Org.ID)+"/tokens/"+itoa(row.ID)+"/revoke", nil, withCookie(operator, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("revoke via wrong org: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodPost, revokePath, nil, withCookie(operator, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	req.DeviceUID = "uid-tok-2"
	if resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil); resp.StatusCode != 401 || errorCode(t, body) != "invalid_enrollment_token" {
		t.Fatalf("enroll with revoked token: %d %s", resp.StatusCode, body)
	}
	if a := e.auditActions("token.revoked"); len(a) != 1 || a[0].TargetID == nil || *a[0].TargetID != row.ID {
		t.Fatalf("token.revoked audit %+v", a)
	}
}

func TestDashboardBaseURL(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	tokPath := "/api/v1/orgs/" + itoa(e.org.ID) + "/tokens"

	// Configured base URL wins and is used without a trailing slash.
	e.srv.cfg.BaseURL = "https://rmm.example.com/"
	resp, body := e.do(http.MethodPost, tokPath, map[string]any{}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 || !strings.Contains(string(body), "irm https://rmm.example.com/dl/install.ps1") || strings.Contains(string(body), "example.com//") {
		t.Fatalf("base url from config: %d %s", resp.StatusCode, body)
	}

	// Without it, X-Forwarded-Proto is honoured only behind a trusted proxy.
	e.srv.cfg.BaseURL = ""
	h := withCookie(admin, csrfHeader)
	h["X-Forwarded-Proto"] = "https"
	resp, body = e.do(http.MethodPost, tokPath, map[string]any{}, h)
	if resp.StatusCode != 201 || !strings.Contains(string(body), "--server "+e.ts.URL+" ") {
		t.Fatalf("untrusted proxy header: %d %s", resp.StatusCode, body)
	}
	e.srv.cfg.TrustProxy = true
	resp, body = e.do(http.MethodPost, tokPath, map[string]any{}, h)
	want := "https://" + strings.TrimPrefix(e.ts.URL, "http://")
	if resp.StatusCode != 201 || !strings.Contains(string(body), "--server "+want+" ") {
		t.Fatalf("trusted proxy header: %d %s", resp.StatusCode, body)
	}

	lines := dashInstallLines("http://h:1/", "et_x")
	if lines["linux"] != "curl -fsSLo /tmp/msp-install.sh http://h:1/dl/install.sh && sudo bash /tmp/msp-install.sh --server http://h:1 --token et_x" {
		t.Fatalf("linux line %q", lines["linux"])
	}
	if lines["windows"] != `irm http://h:1/dl/install.ps1 -OutFile $env:TEMP\msp-install.ps1; & $env:TEMP\msp-install.ps1 -Server http://h:1 -Token et_x` {
		t.Fatalf("windows line %q", lines["windows"])
	}
	if lines["android"] != "Install http://h:1/dl/msp-agent.apk, open MSP Agent, enter http://h:1 and the token et_x" {
		t.Fatalf("android line %q", lines["android"])
	}
}

func TestDashboardAlertsAndAudit(t *testing.T) {
	e := newTestEnv(t)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	if status, _, _ := e.sync(token, syncRequest(0)); status != 200 {
		t.Fatalf("sync: %d", status)
	}
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	// Nothing open yet.
	resp, body := e.do(http.MethodGet, "/api/v1/alerts", nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 || string(body) != `{"alerts":[]}` {
		t.Fatalf("alerts empty: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/alerts?state=weird", nil, withCookie(viewer, nil)); resp.StatusCode != 400 {
		t.Fatalf("alerts bad state: %d", resp.StatusCode)
	}

	// Go offline (30 s interval -> 105 s window) and evaluate.
	e.clock.advance(2 * time.Minute)
	EvaluateNow(e.srv)
	resp, body = e.do(http.MethodGet, "/api/v1/alerts?state=open&org_id="+itoa(e.org.ID), nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("alerts open: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Alerts []store.Alert `json:"alerts"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Alerts) != 1 || out.Alerts[0].Type != "offline" || out.Alerts[0].DeviceID != devID || out.Alerts[0].Hostname != "ws-uid-1" || out.Alerts[0].OrgName != "Acme Dental" {
		t.Fatalf("open alerts %s (%v)", body, err)
	}
	if got := dashKeys(dashDecode(t, body)["alerts"].([]any)[0].(map[string]any)); strings.Join(got, ",") !=
		"acknowledged_at,acknowledged_by,device_id,hostname,id,message,opened_at,org_id,org_name,resolved_at,severity,type" {
		t.Fatalf("alert keys %v", got)
	}
	alertID := out.Alerts[0].ID

	// The device page lists the open alert.
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(devID), nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 || len(dashDecode(t, body)["alerts"].([]any)) != 1 {
		t.Fatalf("device page alerts: %d %s", resp.StatusCode, body)
	}

	// Ack: viewer forbidden, operator ok, audit row, second ack still 204 (still open), missing 404.
	ackPath := "/api/v1/alerts/" + itoa(alertID) + "/ack"
	if resp, _ := e.do(http.MethodPost, ackPath, nil, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("viewer ack: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodPost, ackPath, nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("ack: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/alerts/999999/ack", nil, withCookie(admin, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("ack missing: %d", resp.StatusCode)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/alerts", nil, withCookie(viewer, nil))
	if err := json.Unmarshal(body, &out); err != nil || len(out.Alerts) != 1 || out.Alerts[0].AcknowledgedBy == nil || *out.Alerts[0].AcknowledgedBy != e.admin.ID || out.Alerts[0].AcknowledgedAt == nil {
		t.Fatalf("acked alert %s (%v)", body, err)
	}
	if a := e.auditActions("alert.ack"); len(a) != 1 || a[0].OrgID == nil || *a[0].OrgID != e.org.ID || a[0].Detail["type"] != "offline" {
		t.Fatalf("alert.ack audit %+v", a)
	}

	// Back online: resolved; state=all still shows it, open does not.
	e.clock.advance(time.Second)
	if status, _, _ := e.sync(token, syncRequest(0)); status != 200 {
		t.Fatalf("sync: %d", status)
	}
	EvaluateNow(e.srv)
	resp, body = e.do(http.MethodGet, "/api/v1/alerts", nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 || string(body) != `{"alerts":[]}` {
		t.Fatalf("alerts after resolve: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/alerts?state=all", nil, withCookie(viewer, nil))
	if err := json.Unmarshal(body, &out); err != nil || len(out.Alerts) != 1 || out.Alerts[0].ResolvedAt == nil {
		t.Fatalf("alerts all: %s (%v)", body, err)
	}

	// Audit listing: newest first, org filter, before pagination, limit.
	resp, body = e.do(http.MethodGet, "/api/v1/audit?limit=2", nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("audit: %d %s", resp.StatusCode, body)
	}
	var audit struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(body, &audit); err != nil || len(audit.Entries) != 2 || audit.Entries[0].ID < audit.Entries[1].ID {
		t.Fatalf("audit limit=2 %s (%v)", body, err)
	}
	if got := dashKeys(dashDecode(t, body)["entries"].([]any)[0].(map[string]any)); strings.Join(got, ",") !=
		"action,actor_id,actor_label,actor_type,at,detail,id,ip,org_id,target_id,target_type" {
		t.Fatalf("audit keys %v", got)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/audit?before="+itoa(audit.Entries[0].ID), nil, withCookie(viewer, nil))
	var page struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.Entries) == 0 || page.Entries[0].ID != audit.Entries[1].ID {
		t.Fatalf("audit before: %s (%v)", body, err)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/audit?org_id="+itoa(e.org.ID), nil, withCookie(viewer, nil))
	if err := json.Unmarshal(body, &page); err != nil || len(page.Entries) == 0 {
		t.Fatalf("audit org filter: %s (%v)", body, err)
	}
	for _, en := range page.Entries {
		if en.OrgID == nil || *en.OrgID != e.org.ID {
			t.Fatalf("audit org filter leaked %+v", en)
		}
	}
	seen := map[string]bool{}
	for _, en := range page.Entries {
		seen[en.ActorType+":"+en.Action] = true
	}
	if !seen["device:device.enrolled"] || !seen["user:alert.ack"] {
		t.Fatalf("audit actors %v", seen)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/audit?before=x", nil, withCookie(viewer, nil)); resp.StatusCode != 400 {
		t.Fatalf("audit bad before: %d", resp.StatusCode)
	}
}

func TestDashboardUsers(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	operator := dashUser(e, "operator@example.com", RoleOperator)

	// Admin only.
	if resp, body := e.do(http.MethodGet, "/api/v1/users", nil, withCookie(operator, nil)); resp.StatusCode != 403 || errorCode(t, body) != "forbidden" {
		t.Fatalf("operator list users: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/users", map[string]string{"email": "x@example.com", "password": "long-enough-pw", "role": "viewer"}, withCookie(operator, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("operator create user: %d", resp.StatusCode)
	}

	// Create: validation, lowercasing, conflict, audit.
	bad := []map[string]string{
		{"email": "nope", "password": "long-enough-pw", "role": "viewer"},
		{"email": "x@example.com", "password": "short", "role": "viewer"},
		{"email": "x@example.com", "password": "long-enough-pw", "role": "root"},
	}
	for _, b := range bad {
		if resp, body := e.do(http.MethodPost, "/api/v1/users", b, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
			t.Fatalf("create user %v: %d %s", b, resp.StatusCode, body)
		}
	}
	resp, body := e.do(http.MethodPost, "/api/v1/users", map[string]string{"email": "Tech@Example.com", "password": "tech-pass-1234", "role": "operator"}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create user: %d %s", resp.StatusCode, body)
	}
	var created struct {
		User store.User `json:"user"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.User.Email != "tech@example.com" || created.User.Role != "operator" || created.User.DisabledAt != nil {
		t.Fatalf("created user %s (%v)", body, err)
	}
	if strings.Contains(string(body), "password") {
		t.Fatalf("user body leaks the hash: %s", body)
	}
	if resp, body := e.do(http.MethodPost, "/api/v1/users", map[string]string{"email": "tech@example.com", "password": "tech-pass-1234", "role": "viewer"}, withCookie(admin, csrfHeader)); resp.StatusCode != 409 || errorCode(t, body) != "conflict" {
		t.Fatalf("duplicate user: %d %s", resp.StatusCode, body)
	}
	if a := e.auditActions("user.created"); len(a) != 1 || a[0].Detail["email"] != "tech@example.com" || *a[0].ActorID != e.admin.ID {
		t.Fatalf("user.created audit %+v", a)
	}
	// The new user can log in.
	tech := dashLogin(e, "tech@example.com", "tech-pass-1234")

	// List.
	resp, body = e.do(http.MethodGet, "/api/v1/users", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("list users: %d %s", resp.StatusCode, body)
	}
	var list struct {
		Users []store.User `json:"users"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Users) != 3 {
		t.Fatalf("list users %s (%v)", body, err)
	}
	if got := dashKeys(dashDecode(t, body)["users"].([]any)[0].(map[string]any)); strings.Join(got, ",") != "created_at,disabled_at,email,id,role" {
		t.Fatalf("user keys %v", got)
	}

	// Disable: self 400, missing 404, ok 204 and the session dies.
	if resp, body := e.do(http.MethodPost, "/api/v1/users/"+itoa(e.admin.ID)+"/disable", nil, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("disable self: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/users/999999/disable", nil, withCookie(admin, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("disable missing: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/users/"+itoa(created.User.ID)+"/disable", nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("disable: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/me", nil, withCookie(tech, nil)); resp.StatusCode != 401 {
		t.Fatalf("disabled user session still valid: %d", resp.StatusCode)
	}
	if a := e.auditActions("user.disabled"); len(a) != 1 || *a[0].TargetID != created.User.ID {
		t.Fatalf("user.disabled audit %+v", a)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/users", nil, withCookie(admin, nil))
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	for _, u := range list.Users {
		if u.ID == created.User.ID && u.DisabledAt == nil {
			t.Fatalf("disabled_at not set: %+v", u)
		}
	}
}

func TestDashboardDeviceDelete(t *testing.T) {
	e := newTestEnv(t)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	admin := dashAdmin(e)
	e.queue(devID, "echo", "x")

	if resp, _ := e.do(http.MethodDelete, "/api/v1/devices/999999", nil, withCookie(admin, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("delete missing: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodDelete, "/api/v1/devices/"+itoa(devID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/devices/"+itoa(devID), nil, withCookie(admin, nil)); resp.StatusCode != 404 {
		t.Fatalf("deleted device still readable: %d", resp.StatusCode)
	}
	if status, _, _ := e.sync(token, syncRequest(0)); status != 401 {
		t.Fatalf("deleted device token still accepted: %d", status)
	}
	if a := e.auditActions("device.deleted"); len(a) != 1 || a[0].Detail["hostname"] != "ws-uid-1" || *a[0].OrgID != e.org.ID {
		t.Fatalf("device.deleted audit %+v", a)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/devices/"+itoa(devID)+"/revoke", nil, withCookie(admin, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("revoke missing: %d", resp.StatusCode)
	}
}

func TestDashboardStatic(t *testing.T) {
	e := newTestEnv(t)

	// SPA shell at / and at any deep link, with no-cache and the CSP.
	for _, p := range []string{"/", "/devices/42", "/orgs", "/index.html"} {
		resp, body := e.do(http.MethodGet, p, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d", p, resp.StatusCode)
		}
		if !strings.Contains(string(body), "app.js") || !strings.Contains(string(body), "<!doctype html>") {
			t.Fatalf("GET %s body %q", p, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Fatalf("GET %s content-type %q", p, ct)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("GET %s cache-control %q", p, resp.Header.Get("Cache-Control"))
		}
		if resp.Header.Get("Content-Security-Policy") != cspValue {
			t.Fatalf("GET %s missing CSP", p)
		}
	}
	// Bundle files are served with their own types when present.
	if _, err := os.Stat(filepath.Join("..", "..", "web", "dist", "app.js")); err == nil {
		resp, _ := e.do(http.MethodGet, "/app.js", nil, nil)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
			t.Fatalf("app.js: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	if dashContentType("app.css") != "text/css; charset=utf-8" || dashContentType("x.html") != "text/html; charset=utf-8" || dashContentType("SHA256SUMS") != "text/plain; charset=utf-8" {
		t.Fatal("content types")
	}
	// A HEAD request carries no body.
	resp, body := e.do(http.MethodHead, "/", nil, nil)
	if resp.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("HEAD /: %d %d bytes", resp.StatusCode, len(body))
	}
	// Unknown API/dl paths never fall back to the SPA.
	for _, p := range []string{"/api", "/api/", "/api/v2/x", "/dl", "/dl/"} {
		resp, body := e.do(http.MethodGet, p, nil, nil)
		if resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
			t.Fatalf("GET %s: %d %s", p, resp.StatusCode, body)
		}
	}
	if resp, body := e.do(http.MethodPost, "/", nil, nil); resp.StatusCode != 405 || errorCode(t, body) != "method_not_allowed" {
		t.Fatalf("POST /: %d %s", resp.StatusCode, body)
	}

	// /dl/: 404 without AgentDir.
	if resp, body := e.do(http.MethodGet, "/dl/install.sh", nil, nil); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("dl without AgentDir: %d %s", resp.StatusCode, body)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "inner.txt"), []byte("inner"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.AgentDir = dir

	resp, body = e.do(http.MethodGet, "/dl/install.sh", nil, nil)
	if resp.StatusCode != 200 || string(body) != "#!/bin/sh\necho hi\n" {
		t.Fatalf("dl install.sh: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("dl content-type %q", ct)
	}
	if resp, _ := e.do(http.MethodGet, "/dl/missing.bin", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("dl missing: %d", resp.StatusCode)
	}
	// Traversal, hidden files, subdirectories and directories never serve a
	// file. A sentinel outside AgentDir must not leak; the mux may answer a
	// dot-segment path with its own redirect, which is fine as long as the
	// redirect does not land back under /dl/.
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{
		"/dl/../secret.txt", "/dl/..%2Fsecret.txt", "/dl/%2e%2e/secret.txt", "/dl/%2e%2e%2fsecret.txt",
		"/dl/sub/inner.txt", "/dl/sub", "/dl/.secret", "/dl/..", "/dl/%2e%2e", "/dl/sub%2Finner.txt",
	} {
		req, err := http.NewRequest(http.MethodGet, e.ts.URL+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		switch resp.StatusCode {
		case 404:
		case 301, 308:
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/dl/") {
				t.Fatalf("GET %s redirected to %q", p, loc)
			}
		default:
			t.Fatalf("GET %s: %d %q, want 404 or a redirect away from /dl/", p, resp.StatusCode, b)
		}
		if strings.Contains(string(b), "TOPSECRET") || strings.Contains(string(b), "inner") {
			t.Fatalf("GET %s leaked file content: %q", p, b)
		}
	}
}
