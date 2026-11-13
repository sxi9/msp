package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// dashScopedUser creates an operator/viewer, logs them in and scopes them to
// orgIDs (all_orgs=0). It returns the cookie and the new user's id.
func dashScopedUser(e *testEnv, email, role string, orgIDs []int64) (*http.Cookie, int64) {
	e.t.Helper()
	c := dashUser(e, email, role)
	u, err := e.st.GetUserByEmail(context.Background(), email)
	if err != nil {
		e.t.Fatalf("GetUserByEmail %s: %v", email, err)
	}
	if err := e.st.SetUserOrgs(context.Background(), u.ID, false, orgIDs); err != nil {
		e.t.Fatalf("SetUserOrgs %s: %v", email, err)
	}
	return c, u.ID
}

// TestDashboardOrgScoping is the RS2 proof: a scoped operator who can see only
// org A sees only A in the list endpoints and gets a 404 — with no side effect
// — on every device-scoped endpoint of a device in org B, until an admin grants
// B. Org A is the test env's own org ("Acme Dental").
func TestDashboardOrgScoping(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	orgA := e.org

	// Org B with its own device, an open alert and a queued shell command.
	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	devA, _ := e.enroll("uid-a", protocol.PlatformLinux) // device.enrolled audit, org A
	devB, _, err := e.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	if _, err := e.st.OpenAlert(ctx, orgA.ID, devA, alertOffline, severityHigh, "a offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.OpenAlert(ctx, orgB.ID, devB.ID, alertOffline, severityHigh, "b offline"); err != nil {
		t.Fatal(err)
	}
	bAlerts, _ := e.st.ListAlerts(ctx, true, &orgB.ID, nil, 0)
	if len(bAlerts) != 1 {
		t.Fatalf("org B open alerts = %d", len(bAlerts))
	}
	alertB := bAlerts[0].ID
	shellB, err := e.st.CreateCommand(ctx, store.CreateCommandParams{
		DeviceID: devB.ID, OrgID: orgB.ID, CreatedBy: e.admin.ID,
		Kind: protocol.KindShell, Shell: "bash", TimeoutS: protocol.DefaultShellTimeoutS,
	})
	if err != nil {
		t.Fatalf("CreateCommand shell: %v", err)
	}
	// Audit rows: one for A, one for B, one system (org_id NULL).
	e.st.Audit(ctx, store.AuditEntry{ActorType: "user", ActorID: &e.admin.ID, OrgID: &orgA.ID, Action: "org.touch_a"})
	e.st.Audit(ctx, store.AuditEntry{ActorType: "user", ActorID: &e.admin.ID, OrgID: &orgB.ID, Action: "org.touch_b"})
	e.st.Audit(ctx, store.AuditEntry{ActorType: "system", Action: "system.touch"})

	op, opID := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{orgA.ID})

	// --- list endpoints show only org A -------------------------------------
	var orgs struct {
		Orgs []store.Org `json:"orgs"`
	}
	resp, body := e.do(http.MethodGet, "/api/v1/orgs", nil, withCookie(op, nil))
	if resp.StatusCode != 200 || json.Unmarshal(body, &orgs) != nil || len(orgs.Orgs) != 1 || orgs.Orgs[0].ID != orgA.ID {
		t.Fatalf("scoped orgs: %d %s", resp.StatusCode, body)
	}
	var devs struct {
		Devices []store.Device `json:"devices"`
	}
	resp, body = e.do(http.MethodGet, "/api/v1/devices", nil, withCookie(op, nil))
	if resp.StatusCode != 200 || json.Unmarshal(body, &devs) != nil || len(devs.Devices) != 1 || devs.Devices[0].ID != devA {
		t.Fatalf("scoped devices: %d %s", resp.StatusCode, body)
	}
	var sum store.Summary
	resp, body = e.do(http.MethodGet, "/api/v1/summary", nil, withCookie(op, nil))
	if resp.StatusCode != 200 || json.Unmarshal(body, &sum) != nil || sum.Orgs != 1 || sum.Devices != 1 || sum.OpenAlerts != 1 {
		t.Fatalf("scoped summary: %d %s", resp.StatusCode, body)
	}
	var alerts struct {
		Alerts []store.Alert `json:"alerts"`
	}
	resp, body = e.do(http.MethodGet, "/api/v1/alerts?state=all", nil, withCookie(op, nil))
	if resp.StatusCode != 200 || json.Unmarshal(body, &alerts) != nil {
		t.Fatalf("scoped alerts: %d %s", resp.StatusCode, body)
	}
	for _, a := range alerts.Alerts {
		if a.OrgID != orgA.ID {
			t.Fatalf("scoped alerts leaked org %d: %s", a.OrgID, body)
		}
	}
	if len(alerts.Alerts) != 1 {
		t.Fatalf("scoped alerts count = %d (want only org A): %s", len(alerts.Alerts), body)
	}
	var audit struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	resp, body = e.do(http.MethodGet, "/api/v1/audit?limit=1000", nil, withCookie(op, nil))
	if resp.StatusCode != 200 || json.Unmarshal(body, &audit) != nil {
		t.Fatalf("scoped audit: %d %s", resp.StatusCode, body)
	}
	sawA := false
	for _, en := range audit.Entries {
		if en.OrgID == nil {
			t.Fatalf("scoped audit leaked a system (null-org) row: %s", en.Action)
		}
		if *en.OrgID == orgB.ID {
			t.Fatalf("scoped audit leaked org B row: %s", en.Action)
		}
		if *en.OrgID == orgA.ID {
			sawA = true
		}
	}
	if !sawA {
		t.Fatalf("scoped audit did not include org A rows: %s", body)
	}

	// --- 404 (no side effect) across orgs on every device-scoped endpoint ----
	cmdsBefore, _ := e.st.ListCommands(ctx, devB.ID, 200)
	devBPath := "/api/v1/devices/" + itoa(devB.ID)
	gets := []string{
		"/api/v1/orgs/" + itoa(orgB.ID),
		devBPath,
		devBPath + "/commands",
		devBPath + "/heartbeats",
		"/api/v1/commands/" + itoa(shellB.ID),
		"/api/v1/shell/" + itoa(shellB.ID) + "/transcript",
	}
	for _, p := range gets {
		if resp, b := e.do(http.MethodGet, p, nil, withCookie(op, nil)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
			t.Fatalf("scoped GET %s: %d %s (want 404 not_found)", p, resp.StatusCode, b)
		}
	}
	posts := []struct {
		path string
		body any
	}{
		{devBPath + "/commands", map[string]any{"kind": "exec", "argv": []string{"id"}}},
		{devBPath + "/commands", map[string]any{"kind": "lock"}},
		{devBPath + "/shell", map[string]any{"shell": "bash"}},
		{devBPath + "/remote-desktop", nil},
		{"/api/v1/alerts/" + itoa(alertB) + "/ack", nil},
	}
	for _, p := range posts {
		if resp, b := e.do(http.MethodPost, p.path, p.body, withCookie(op, csrfHeader)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
			t.Fatalf("scoped POST %s: %d %s (want 404 not_found)", p.path, resp.StatusCode, b)
		}
	}
	// No side effect: no new command in org B, the alert is still unacked, and
	// none of the blocked actions wrote an audit row.
	cmdsAfter, _ := e.st.ListCommands(ctx, devB.ID, 200)
	if len(cmdsAfter) != len(cmdsBefore) {
		t.Fatalf("scoped POSTs created %d command rows in org B", len(cmdsAfter)-len(cmdsBefore))
	}
	if a, _ := e.st.GetAlert(ctx, alertB); a.AcknowledgedBy != nil {
		t.Fatal("scoped ack across orgs acknowledged the alert")
	}
	for _, action := range []string{"command.queued", "device.lock_requested", "shell.opened", "device.remote_desktop_opened", "alert.ack"} {
		if n := len(e.auditActions(action)); n != 0 {
			t.Fatalf("scoped 404 still wrote a %s audit row (%d)", action, n)
		}
	}

	// An all-orgs admin is unaffected: it reads org B's device.
	admin := dashAdmin(e)
	if resp, _ := e.do(http.MethodGet, devBPath, nil, withCookie(admin, nil)); resp.StatusCode != 200 {
		t.Fatalf("admin get org B device: %d", resp.StatusCode)
	}

	// --- admin grants org B; the same operator now sees B -------------------
	resp, body = e.do(http.MethodPut, "/api/v1/users/"+itoa(opID)+"/orgs",
		map[string]any{"all_orgs": false, "org_ids": []int64{orgA.ID, orgB.ID}}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("grant org B: %d %s", resp.StatusCode, body)
	}
	if a := e.auditActions("user.orgs_changed"); len(a) != 1 || a[0].TargetID == nil || *a[0].TargetID != opID {
		t.Fatalf("user.orgs_changed audit %+v", a)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/orgs", nil, withCookie(op, nil))
	if resp.StatusCode != 200 || json.Unmarshal(body, &orgs) != nil || len(orgs.Orgs) != 2 {
		t.Fatalf("after grant scoped orgs: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodGet, devBPath, nil, withCookie(op, nil)); resp.StatusCode != 200 {
		t.Fatalf("after grant scoped get org B device: %d", resp.StatusCode)
	}
}

// TestDashboardUserOrgsAdmin covers the admin management endpoints: GET /users
// exposing all_orgs/org_ids, POST /users scoping an operator, and the PUT
// /users/{id}/orgs rules (reject scoping an admin, missing user, bad org id).
func TestDashboardUserOrgsAdmin(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := dashAdmin(e)
	orgB, _ := e.st.CreateOrg(ctx, "Beta Corp")

	// Create a scoped operator through the API.
	resp, body := e.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "scoped@example.com", "password": "scoped-pass-1", "role": "operator",
		"all_orgs": false, "org_ids": []int64{e.org.ID},
	}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create scoped user: %d %s", resp.StatusCode, body)
	}
	var created struct {
		User struct {
			ID      int64   `json:"id"`
			Role    string  `json:"role"`
			AllOrgs bool    `json:"all_orgs"`
			OrgIDs  []int64 `json:"org_ids"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.User.AllOrgs || len(created.User.OrgIDs) != 1 || created.User.OrgIDs[0] != e.org.ID {
		t.Fatalf("create scoped user body %s (%v)", body, err)
	}
	opID := created.User.ID

	// Creating a scoped user against a non-existent org is a 400.
	if resp, b := e.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "bad@example.com", "password": "scoped-pass-1", "role": "viewer",
		"all_orgs": false, "org_ids": []int64{999999},
	}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "invalid_request" {
		t.Fatalf("create user bad org: %d %s", resp.StatusCode, b)
	}

	// The admin role ignores scoping on create (admins are always all-orgs).
	resp, body = e.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "admin2@example.com", "password": "admin-pass-12", "role": "admin",
		"all_orgs": false, "org_ids": []int64{e.org.ID},
	}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 || !jsonBool(t, body, "user", "all_orgs") {
		t.Fatalf("create admin ignores scoping: %d %s", resp.StatusCode, body)
	}

	// GET /users exposes all_orgs/org_ids on every row.
	resp, body = e.do(http.MethodGet, "/api/v1/users", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("list users: %d %s", resp.StatusCode, body)
	}
	var list struct {
		Users []struct {
			ID      int64   `json:"id"`
			Role    string  `json:"role"`
			AllOrgs bool    `json:"all_orgs"`
			OrgIDs  []int64 `json:"org_ids"`
		} `json:"users"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	for _, u := range list.Users {
		switch {
		case u.ID == opID:
			if u.AllOrgs || len(u.OrgIDs) != 1 || u.OrgIDs[0] != e.org.ID {
				t.Fatalf("scoped operator row %+v", u)
			}
		case u.Role == "admin":
			if !u.AllOrgs || len(u.OrgIDs) != 0 {
				t.Fatalf("admin row must be all-orgs: %+v", u)
			}
		}
	}

	// PUT /users/{id}/orgs: grant both orgs.
	resp, body = e.do(http.MethodPut, "/api/v1/users/"+itoa(opID)+"/orgs",
		map[string]any{"all_orgs": false, "org_ids": []int64{e.org.ID, orgB.ID}}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("put orgs: %d %s", resp.StatusCode, body)
	}
	if all, ids, _ := e.st.GetUserOrgs(ctx, opID); all || len(ids) != 2 {
		t.Fatalf("after put orgs = all %v ids %v", all, ids)
	}

	// Scoping an admin is a 400; a missing user is 404; a bad org id is 400.
	if resp, b := e.do(http.MethodPut, "/api/v1/users/"+itoa(e.admin.ID)+"/orgs",
		map[string]any{"all_orgs": false, "org_ids": []int64{e.org.ID}}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "invalid_request" {
		t.Fatalf("scope admin: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.do(http.MethodPut, "/api/v1/users/999999/orgs",
		map[string]any{"all_orgs": true}, withCookie(admin, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("put orgs missing user: %d", resp.StatusCode)
	}
	if resp, b := e.do(http.MethodPut, "/api/v1/users/"+itoa(opID)+"/orgs",
		map[string]any{"all_orgs": false, "org_ids": []int64{999999}}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "invalid_request" {
		t.Fatalf("put orgs bad org: %d %s", resp.StatusCode, b)
	}

	// An operator cannot reach the admin management endpoints.
	operator := dashUser(e, "op2@example.com", RoleOperator)
	if resp, _ := e.do(http.MethodPut, "/api/v1/users/"+itoa(opID)+"/orgs",
		map[string]any{"all_orgs": true}, withCookie(operator, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("operator put orgs: %d", resp.StatusCode)
	}
}

// jsonBool reads a nested boolean field (obj -> key) from a JSON body.
func jsonBool(t *testing.T, body []byte, obj, key string) bool {
	t.Helper()
	m := dashDecode(t, body)
	o, ok := m[obj].(map[string]any)
	if !ok {
		t.Fatalf("no %q object in %s", obj, body)
	}
	b, _ := o[key].(bool)
	return b
}
