package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// decodeJSON unmarshals a response body into a typed value.
func decodeJSON(body []byte, v any) error { return json.Unmarshal(body, v) }

const (
	testRustdeskHost = "rmm.example.com"
	testRustdeskKey  = "OeVuKk5nlHiXp+7qsR6CbcHBGxhU4PJkNbvoX3P5X0k="
)

// enableRustdesk configures the relay on a test server; handlers read cfg
// per request, so this takes effect at once.
func enableRustdesk(e *testEnv) {
	e.srv.cfg.RustdeskHost = testRustdeskHost
	e.srv.cfg.RustdeskKey = testRustdeskKey
}

func TestConfigValidateRustdesk(t *testing.T) {
	if err := (Config{}).ValidateRustdesk(); err != nil {
		t.Fatalf("both empty: %v", err)
	}
	if err := (Config{RustdeskHost: "h", RustdeskKey: "k"}).ValidateRustdesk(); err != nil {
		t.Fatalf("both set: %v", err)
	}
	for _, c := range []Config{{RustdeskHost: "h"}, {RustdeskKey: "k"}} {
		if err := c.ValidateRustdesk(); err == nil || !strings.Contains(err.Error(), "set together") {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	// A port, a shell-splittable value or a TOML-breaking character is a
	// startup error, not something forwarded to every agent.
	for _, c := range []Config{
		{RustdeskHost: "rmm.example.com:21116", RustdeskKey: "k"},
		{RustdeskHost: "x.example -k evil", RustdeskKey: "k"},
		{RustdeskHost: "x.example'\nkey = 'other'", RustdeskKey: "k"},
		{RustdeskHost: "rmm.example.com", RustdeskKey: "k'\napi-server = 'https://evil'"},
	} {
		if err := c.ValidateRustdesk(); err == nil || !strings.Contains(err.Error(), "MSP_RUSTDESK_HOST / MSP_RUSTDESK_KEY") {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	if err := (Config{RustdeskHost: "10.0.0.1", RustdeskKey: "OeVuKk5nlHiXp+7qsR6CbcHBGxhU4PJkNbvoX3P5X0k="}).ValidateRustdesk(); err != nil {
		t.Fatalf("ip host: %v", err)
	}
}

func TestRustdeskInfoAndAgentResponses(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	// Disabled: the dashboard sees enabled=false, agents get no rustdesk block.
	resp, body := e.do(http.MethodGet, "/api/v1/rustdesk", nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 || string(body) != `{"enabled":false,"host":"","key":""}` {
		t.Fatalf("rustdesk disabled: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/rustdesk", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("rustdesk without session: %d", resp.StatusCode)
	}
	req := enrollRequest("uid-1", protocol.PlatformLinux)
	req.EnrollmentToken = e.token
	resp, body = e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil)
	if resp.StatusCode != 201 || strings.Contains(string(body), "rustdesk") {
		t.Fatalf("enroll while disabled: %d %s", resp.StatusCode, body)
	}
	var enrolled protocol.EnrollResponse
	if err := decodeJSON(body, &enrolled); err != nil || enrolled.Rustdesk != nil {
		t.Fatalf("enroll response %s: %v", body, err)
	}
	token := enrolled.DeviceToken
	status, sync, raw := e.sync(token, syncRequest(0))
	if status != 200 || sync.Rustdesk != nil || strings.Contains(string(raw), "rustdesk") {
		t.Fatalf("sync while disabled: %d %s", status, raw)
	}

	// Enabled: both answers carry host and key.
	enableRustdesk(e)
	resp, body = e.do(http.MethodGet, "/api/v1/rustdesk", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || string(body) != `{"enabled":true,"host":"`+testRustdeskHost+`","key":"`+testRustdeskKey+`"}` {
		t.Fatalf("rustdesk enabled: %d %s", resp.StatusCode, body)
	}
	status, sync, raw = e.sync(token, syncRequest(0))
	if status != 200 || sync.Rustdesk == nil || sync.Rustdesk.Host != testRustdeskHost || sync.Rustdesk.Key != testRustdeskKey {
		t.Fatalf("sync while enabled: %d %s", status, raw)
	}
	req2 := enrollRequest("uid-2", protocol.PlatformWindows)
	req2.EnrollmentToken = e.token
	resp, body = e.do(http.MethodPost, "/api/agent/v1/enroll", req2, nil)
	if err := decodeJSON(body, &enrolled); resp.StatusCode != 201 || err != nil || enrolled.Rustdesk == nil || enrolled.Rustdesk.Host != testRustdeskHost {
		t.Fatalf("enroll while enabled: %d %s", resp.StatusCode, body)
	}
}

func TestRustdeskIDFromFacts(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)

	// Enrollment with an id stores it; the DEVICE JSON exposes it.
	req := enrollRequest("uid-1", protocol.PlatformLinux)
	req.EnrollmentToken = e.token
	req.Facts.RustdeskID = "123456789"
	resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("enroll: %d %s", resp.StatusCode, body)
	}
	var enrolled protocol.EnrollResponse
	if err := decodeJSON(body, &enrolled); err != nil {
		t.Fatal(err)
	}
	dev := e.device(enrolled.DeviceID)
	if dev.RustdeskID == nil || *dev.RustdeskID != "123456789" {
		t.Fatalf("rustdesk id after enroll: %v", dev.RustdeskID)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(dev.ID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || dashDecode(t, body)["device"].(map[string]any)["rustdesk_id"] != "123456789" {
		t.Fatalf("device json: %d %s", resp.StatusCode, body)
	}

	// Enrollment without one leaves null; a sync whose facts carry it fills it.
	req2 := enrollRequest("uid-2", protocol.PlatformWindows)
	req2.EnrollmentToken = e.token
	resp, body = e.do(http.MethodPost, "/api/agent/v1/enroll", req2, nil)
	if err := decodeJSON(body, &enrolled); resp.StatusCode != 201 || err != nil {
		t.Fatalf("enroll 2: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(enrolled.DeviceID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || dashDecode(t, body)["device"].(map[string]any)["rustdesk_id"] != nil {
		t.Fatalf("device 2 json: %d %s", resp.StatusCode, body)
	}
	sreq := syncRequest(0)
	sreq.Facts = &protocol.Facts{Kernel: "10.0.22631", RustdeskID: "987654321", IPs: []string{"10.0.0.13"}}
	if status, _, raw := e.sync(enrolled.DeviceToken, sreq); status != 200 {
		t.Fatalf("sync with facts: %d %s", status, raw)
	}
	if status, _, raw := e.sync(enrolled.DeviceToken, syncRequest(0)); status != 200 {
		t.Fatalf("sync without facts: %d %s", status, raw)
	}
	if d := e.device(enrolled.DeviceID); d.RustdeskID == nil || *d.RustdeskID != "987654321" {
		t.Fatalf("rustdesk id after sync: %v", d.RustdeskID)
	}

	// Garbage is refused at both endpoints with the Facts.Validate text.
	bad := enrollRequest("uid-3", protocol.PlatformLinux)
	bad.EnrollmentToken = e.token
	bad.Facts.RustdeskID = "12ab"
	if resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", bad, nil); resp.StatusCode != 400 || !strings.Contains(string(body), "rustdesk_id") {
		t.Fatalf("enroll with bad id: %d %s", resp.StatusCode, body)
	}
	sreq.Facts.RustdeskID = strings.Repeat("1", protocol.MaxRustdeskIDLen+1)
	if status, _, raw := e.sync(enrolled.DeviceToken, sreq); status != 400 || errorCode(t, raw) != "invalid_request" {
		t.Fatalf("sync with bad id: %d %s", status, raw)
	}
	if d := e.device(enrolled.DeviceID); d.RustdeskID == nil || *d.RustdeskID != "987654321" {
		t.Fatalf("rustdesk id must survive a refused sync: %v", d.RustdeskID)
	}
}

func TestRemoteDesktop(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)
	operator := dashUser(e, "operator@example.com", RoleOperator)

	withID := enrollRequest("uid-rd", protocol.PlatformLinux)
	withID.EnrollmentToken = e.token
	withID.Facts.RustdeskID = "123456789"
	resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", withID, nil)
	var enrolled protocol.EnrollResponse
	if err := decodeJSON(body, &enrolled); resp.StatusCode != 201 || err != nil {
		t.Fatalf("enroll: %d %s", resp.StatusCode, body)
	}
	devID := enrolled.DeviceID
	noID, _ := e.enroll("uid-plain", protocol.PlatformWindows)
	path := func(id int64) string { return "/api/v1/devices/" + itoa(id) + "/remote-desktop" }

	// Server without a relay: 409 rustdesk_disabled, nothing audited.
	if resp, body := e.do(http.MethodPost, path(devID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 409 || errorCode(t, body) != "rustdesk_disabled" {
		t.Fatalf("disabled: %d %s", resp.StatusCode, body)
	}
	if n := len(e.auditActions("device.remote_desktop_opened")); n != 0 {
		t.Fatalf("audit rows while disabled: %d", n)
	}
	enableRustdesk(e)

	// Role, CSRF and session checks.
	if resp, body := e.do(http.MethodPost, path(devID), nil, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 || errorCode(t, body) != "forbidden" {
		t.Fatalf("viewer: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(http.MethodPost, path(devID), nil, withCookie(admin, nil)); resp.StatusCode != 403 || errorCode(t, body) != "csrf" {
		t.Fatalf("no csrf header: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodPost, path(devID), nil, csrfHeader); resp.StatusCode != 401 {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	// Tenant scope: an id outside the visible set is 404 like any other access.
	if resp, body := e.do(http.MethodPost, path(999999), nil, withCookie(operator, csrfHeader)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("unknown device: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodPost, "/api/v1/devices/abc/remote-desktop", nil, withCookie(operator, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("bad id: %d", resp.StatusCode)
	}
	// No id reported yet: 409 no_rustdesk_id.
	if resp, body := e.do(http.MethodPost, path(noID), nil, withCookie(operator, csrfHeader)); resp.StatusCode != 409 || errorCode(t, body) != "no_rustdesk_id" {
		t.Fatalf("no id: %d %s", resp.StatusCode, body)
	}
	if n := len(e.auditActions("device.remote_desktop_opened")); n != 0 {
		t.Fatalf("audit rows before a success: %d", n)
	}

	// Success: URL from the protocol, id, host and key; one audit row with user, device, IP.
	resp, body = e.do(http.MethodPost, path(devID), nil, withCookie(operator, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("connect: %d %s", resp.StatusCode, body)
	}
	got := dashDecode(t, body)
	if strings.Join(dashKeys(got), ",") != "host,key,rustdesk_id,url" {
		t.Fatalf("keys %v", dashKeys(got))
	}
	if got["url"] != "rustdesk://connect/123456789" || got["rustdesk_id"] != "123456789" || got["host"] != testRustdeskHost || got["key"] != testRustdeskKey {
		t.Fatalf("body %s", body)
	}
	rows := e.auditActions("device.remote_desktop_opened")
	if len(rows) != 1 {
		t.Fatalf("audit rows: %d", len(rows))
	}
	a := rows[0]
	if a.ActorType != "user" || a.ActorLabel != "operator@example.com" || a.TargetType != "device" || a.TargetID == nil || *a.TargetID != devID ||
		a.OrgID == nil || *a.OrgID != e.org.ID || a.IP != "127.0.0.1" || a.Detail["rustdesk_id"] != "123456789" || a.Detail["hostname"] != "ws-uid-rd" {
		t.Fatalf("audit row %+v", a)
	}

	// A revoked device is 404, exactly like a device outside the org set.
	if resp, _ := e.do(http.MethodPost, "/api/v1/devices/"+itoa(devID)+"/revoke", nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if resp, body := e.do(http.MethodPost, path(devID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("revoked: %d %s", resp.StatusCode, body)
	}
	if n := len(e.auditActions("device.remote_desktop_opened")); n != 1 {
		t.Fatalf("audit rows after failures: %d", n)
	}
}
