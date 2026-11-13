package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// loadAppsFixture reads internal/protocol/fixtures/sync_android_apps.json,
// the canonical sync with an installed-apps inventory in its facts.
func loadAppsFixture(t *testing.T) protocol.SyncRequest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "protocol", "fixtures", "sync_android_apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	var req protocol.SyncRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req.Facts == nil || len(req.Facts.Apps) == 0 {
		t.Fatalf("fixture carries no apps: %+v", req.Facts)
	}
	return req
}

// appsOfDetail returns facts.apps of a GET /api/v1/devices/{id} body.
func appsOfDetail(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	detail := dashDecode(t, body)
	facts, _ := detail["facts"].(map[string]any)
	raw, ok := facts["apps"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, a := range raw {
		out = append(out, a.(map[string]any))
	}
	return out
}

// TestAppsInventoryStoredAndServed: the apps array of an Android agent's
// facts survives enrollment and sync verbatim, in the agent's order, and is
// served inside facts by the device endpoint.
func TestAppsInventoryStoredAndServed(t *testing.T) {
	e := newTestEnv(t)
	e.srv.enrollLimit = newRateLimiter(1000)
	fixture := loadAppsFixture(t)

	// Enrollment with apps.
	req := enrollRequest("phone-1", protocol.PlatformAndroid)
	req.EnrollmentToken = e.token
	req.Facts = *fixture.Facts
	resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("enroll: %d %s", resp.StatusCode, body)
	}
	var enrolled protocol.EnrollResponse
	if err := json.Unmarshal(body, &enrolled); err != nil {
		t.Fatal(err)
	}
	if d := e.device(enrolled.DeviceID); !strings.Contains(d.FactsJSON, `"apps":[{"package":"com.example.nolabel"`) {
		t.Fatalf("apps not stored at enrollment: %s", d.FactsJSON)
	}

	admin := dashAdmin(e)
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(enrolled.DeviceID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("get device: %d %s", resp.StatusCode, body)
	}
	apps := appsOfDetail(t, body)
	if len(apps) != len(fixture.Facts.Apps) {
		t.Fatalf("detail facts.apps has %d entries, want %d: %s", len(apps), len(fixture.Facts.Apps), body)
	}
	for i, want := range fixture.Facts.Apps {
		got := apps[i]
		if got["package"] != want.Package || got["label"] != want.Label || got["version"] != want.Version || got["system"] != want.System {
			t.Fatalf("apps[%d] = %v, want %+v", i, got, want)
		}
	}
	if keys := dashKeys(apps[0]); strings.Join(keys, ",") != "label,package,system,version" {
		t.Fatalf("app keys %v", keys)
	}

	// A sync without facts leaves the stored inventory alone.
	sreq := syncRequest(0)
	if code, _, body := e.sync(enrolled.DeviceToken, sreq); code != 200 {
		t.Fatalf("sync without facts: %d %s", code, body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(enrolled.DeviceID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || len(appsOfDetail(t, body)) != len(fixture.Facts.Apps) {
		t.Fatalf("apps lost after a sync without facts: %d %s", resp.StatusCode, body)
	}

	// A sync with facts (the fixture itself, with one app fewer) replaces it.
	sreq = fixture
	facts := *fixture.Facts
	facts.Apps = facts.Apps[:len(facts.Apps)-1]
	sreq.Facts = &facts
	if code, _, body := e.sync(enrolled.DeviceToken, sreq); code != 200 {
		t.Fatalf("sync with apps: %d %s", code, body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(enrolled.DeviceID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || len(appsOfDetail(t, body)) != len(fixture.Facts.Apps)-1 {
		t.Fatalf("apps not replaced by the sync facts: %d %s", resp.StatusCode, body)
	}

	// An empty array (the Android agent with nothing to report, or a failed
	// PackageManager read) is accepted and stored as no key at all.
	rawFixture, err := os.ReadFile(filepath.Join("..", "protocol", "fixtures", "sync_android_apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rawSync map[string]any
	if err := json.Unmarshal(rawFixture, &rawSync); err != nil {
		t.Fatal(err)
	}
	rawSync["facts"].(map[string]any)["apps"] = []any{}
	emptyBody, err := json.Marshal(rawSync)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(emptyBody), `"apps":[]`) {
		t.Fatalf("test body must carry an empty apps array: %s", emptyBody)
	}
	resp, body = e.do(http.MethodPost, "/api/agent/v1/sync", string(emptyBody), bearer(enrolled.DeviceToken))
	if resp.StatusCode != 200 {
		t.Fatalf("sync with empty apps: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(enrolled.DeviceID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || strings.Contains(string(body), `"apps"`) {
		t.Fatalf("an empty apps array must be stored as no apps key: %d %s", resp.StatusCode, body)
	}

	// Facts without apps (desktop agents, phase 1) carry no apps key at all.
	sreq = syncRequest(0)
	plain := enrollRequest("x", protocol.PlatformLinux).Facts
	sreq.Facts = &plain
	if code, _, body := e.sync(enrolled.DeviceToken, sreq); code != 200 {
		t.Fatalf("sync with plain facts: %d %s", code, body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices/"+itoa(enrolled.DeviceID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || strings.Contains(string(body), `"apps"`) {
		t.Fatalf("plain facts must not produce an apps key: %d %s", resp.StatusCode, body)
	}
}

// TestAppsInventoryValidation: an invalid inventory is refused on both
// agent endpoints with 400 invalid_request and a detail naming the index.
func TestAppsInventoryValidation(t *testing.T) {
	e := newTestEnv(t)
	e.srv.enrollLimit = newRateLimiter(1000)
	fixture := loadAppsFixture(t)
	_, token := e.enroll("phone-2", protocol.PlatformAndroid)

	tooMany := make([]protocol.App, protocol.MaxApps+1)
	for i := range tooMany {
		tooMany[i] = protocol.App{Package: "com.example.app", Label: "App", Version: "1"}
	}
	cases := []struct {
		name string
		apps []protocol.App
		want string
	}{
		{"empty package", []protocol.App{{Package: "com.ok", Label: "ok"}, {Package: "", Label: "no package"}}, "facts.apps[1].package must be 1..256 bytes"},
		{"long label", []protocol.App{{Package: "com.ok", Label: strings.Repeat("l", protocol.MaxAppLabelLen+1)}}, "facts.apps[0].label exceeds 256 bytes"},
		{"long version", []protocol.App{{Package: "a"}, {Package: "b"}, {Package: "c", Version: strings.Repeat("v", protocol.MaxAppVersionLen+1)}}, "facts.apps[2].version exceeds 64 bytes"},
		{"too many", tooMany, "facts.apps has 1001 entries, at most 1000 are accepted"},
	}
	for _, c := range cases {
		facts := *fixture.Facts
		facts.Apps = c.apps

		sreq := fixture
		sreq.Facts = &facts
		code, _, body := e.sync(token, sreq)
		if code != http.StatusBadRequest || errorCode(t, body) != "invalid_request" || !strings.Contains(string(body), c.want) {
			t.Errorf("sync %s: %d %s (want detail %q)", c.name, code, body, c.want)
		}

		ereq := enrollRequest("phone-3", protocol.PlatformAndroid)
		ereq.EnrollmentToken = e.token
		ereq.Facts = facts
		resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", ereq, nil)
		if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "invalid_request" || !strings.Contains(string(body), c.want) {
			t.Errorf("enroll %s: %d %s (want detail %q)", c.name, resp.StatusCode, body, c.want)
		}
	}

	// The maximum valid inventory with typical field sizes fits the 256 KiB
	// agent body limit with room to spare.
	full := make([]protocol.App, protocol.MaxApps)
	for i := range full {
		full[i] = protocol.App{Package: "com.vendor.product.component.module" + itoa(int64(i)), Label: "Vendor Product Component " + itoa(int64(i)), Version: "123.456.789.012345", System: i%3 == 0}
	}
	facts := *fixture.Facts
	facts.Apps = full
	sreq := fixture
	sreq.Facts = &facts
	raw, _ := json.Marshal(sreq)
	if len(raw) >= 256<<10 {
		t.Fatalf("a full inventory serialises to %d bytes, over the agent body limit", len(raw))
	}
	if code, _, body := e.sync(token, sreq); code != 200 {
		t.Fatalf("sync with %d apps (%d bytes): %d %s", protocol.MaxApps, len(raw), code, body)
	}
}
