package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// softwareHit mirrors one row of the GET /api/v1/software response
// (store.SoftwareSearchRow) for decoding in the test.
type softwareHit struct {
	DeviceID int64  `json:"device_id"`
	Hostname string `json:"hostname"`
	OrgID    int64  `json:"org_id"`
	OrgName  string `json:"org_name"`
	Name     string `json:"name"`
	Version  string `json:"version"`
}

type softwareSearchResponse struct {
	Query     string        `json:"query"`
	Results   []softwareHit `json:"results"`
	Truncated bool          `json:"truncated"`
}

// softwareSearch GETs /api/v1/software with the given cookie and query and
// decodes the response.
func (e *testEnv) softwareSearch(c *http.Cookie, query, orgID string) (*http.Response, softwareSearchResponse, []byte) {
	e.t.Helper()
	path := "/api/v1/software?q=" + query
	if orgID != "" {
		path += "&org_id=" + orgID
	}
	resp, body := e.do(http.MethodGet, path, nil, withCookie(c, nil))
	var out softwareSearchResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			e.t.Fatalf("decode software search: %v (%s)", err, body)
		}
	}
	return resp, out, body
}

// hasPackage reports whether the results carry (deviceID, name).
func (r softwareSearchResponse) hasPackage(deviceID int64, name string) bool {
	for _, h := range r.Results {
		if h.DeviceID == deviceID && h.Name == name {
			return true
		}
	}
	return false
}

// hasDevice reports whether any result row is for deviceID.
func (r softwareSearchResponse) hasDevice(deviceID int64) bool {
	for _, h := range r.Results {
		if h.DeviceID == deviceID {
			return true
		}
	}
	return false
}

// TestSoftwareSearch covers the SI1 fleet software-search endpoint: a match is
// returned with the device and package; q shorter than two characters is 400; a
// missing q is 400; the session and role guards hold; and RS2 — a scoped
// operator sees neither a device in an org it cannot see nor its software, and an
// explicit cross-org ?org_id= is the same 404 as a non-existent org, with no
// cross-org rows leaking into an unscoped search.
func TestSoftwareSearch(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := dashAdmin(e)

	// Org A (the env's own org): a linux device with real installed software.
	devA, _ := e.enroll("uid-a", protocol.PlatformLinux)
	aSoftware := []protocol.SoftwareItem{
		{Name: "openssl", Version: "3.0.13-0ubuntu3.4", Publisher: "Ubuntu Developers", Source: "dpkg"},
		{Name: "curl", Version: "8.5.0-2ubuntu10.6", Publisher: "Ubuntu Developers", Source: "dpkg"},
		{Name: "nginx-core", Version: "1.24.0-2ubuntu7", Publisher: "Ubuntu Developers", Source: "dpkg"},
	}
	if err := e.st.RecordSoftware(ctx, devA, aSoftware, e.clock.now()); err != nil {
		t.Fatalf("RecordSoftware A: %v", err)
	}

	// Org B with its own device and a package only it has.
	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	devBRow, _, err := e.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("UpsertDevice B: %v", err)
	}
	bSoftware := []protocol.SoftwareItem{
		{Name: "openssl", Version: "3.0.13-0ubuntu3.4", Publisher: "Ubuntu Developers", Source: "dpkg"},
		{Name: "beta-crm", Version: "2.1.0", Publisher: "Beta Corp", Source: "dpkg"},
	}
	if err := e.st.RecordSoftware(ctx, devBRow.ID, bSoftware, e.clock.now()); err != nil {
		t.Fatalf("RecordSoftware B: %v", err)
	}

	// --- session, role ------------------------------------------------------
	if resp, _ := e.do(http.MethodGet, "/api/v1/software?q=openssl", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("no session: %d", resp.StatusCode)
	}

	// --- q too short / missing ----------------------------------------------
	if resp, _, body := e.softwareSearch(admin, "a", ""); resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("q too short: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(http.MethodGet, "/api/v1/software", nil, withCookie(admin, nil)); resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("missing q: %d %s", resp.StatusCode, body)
	}

	// --- admin: a match returns the device and package ----------------------
	resp, out, body := e.softwareSearch(admin, "openssl", "")
	if resp.StatusCode != 200 {
		t.Fatalf("admin openssl: %d %s", resp.StatusCode, body)
	}
	if !out.hasPackage(devA, "openssl") {
		t.Fatalf("admin search missing org A openssl: %s", body)
	}
	if !out.hasPackage(devBRow.ID, "openssl") {
		t.Fatalf("admin (all orgs) search missing org B openssl: %s", body)
	}
	// A substring that is a prefix of a longer name also matches.
	if resp, out, body := e.softwareSearch(admin, "nginx", ""); resp.StatusCode != 200 || !out.hasPackage(devA, "nginx-core") {
		t.Fatalf("admin nginx substring: %d %s", resp.StatusCode, body)
	}

	// --- admin scoped to one org via ?org_id= -------------------------------
	resp, out, body = e.softwareSearch(admin, "openssl", itoa(e.org.ID))
	if resp.StatusCode != 200 {
		t.Fatalf("admin org A openssl: %d %s", resp.StatusCode, body)
	}
	if !out.hasDevice(devA) || out.hasDevice(devBRow.ID) {
		t.Fatalf("org_id scoping leaked across orgs: %s", body)
	}

	// --- RS2: a scoped operator sees only its own org -----------------------
	op, _ := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{e.org.ID})
	// An unscoped search returns org A's rows and never org B's.
	resp, out, body = e.softwareSearch(op, "openssl", "")
	if resp.StatusCode != 200 {
		t.Fatalf("scoped operator openssl: %d %s", resp.StatusCode, body)
	}
	if !out.hasDevice(devA) {
		t.Fatalf("scoped operator missing its own org A device: %s", body)
	}
	if out.hasDevice(devBRow.ID) {
		t.Fatalf("scoped operator saw a cross-org device: %s", body)
	}
	// A package only org B has returns nothing for the scoped operator.
	if resp, out, body := e.softwareSearch(op, "beta-crm", ""); resp.StatusCode != 200 || len(out.Results) != 0 {
		t.Fatalf("scoped operator must not see org B's beta-crm: %d %s", resp.StatusCode, body)
	}
	// An explicit ?org_id= of an org the operator cannot see is 404 (RS2),
	// indistinguishable from a non-existent org.
	if resp, _, body := e.softwareSearch(op, "openssl", itoa(orgB.ID)); resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Fatalf("scoped operator cross-org org_id: %d %s", resp.StatusCode, body)
	}
	if resp, _, body := e.softwareSearch(op, "openssl", "999999"); resp.StatusCode != 404 {
		t.Fatalf("scoped operator unknown org_id: %d %s", resp.StatusCode, body)
	}
}
