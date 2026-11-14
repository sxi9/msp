package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sxi9/msp/internal/store"
)

// TestDashboardScripts covers the script-library CRUD, its validation and the RS2
// org scoping (a scoped user never sees another client's or a global script, and
// creating a global script requires all-orgs).
func TestDashboardScripts(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	// Viewer is read-only.
	if resp, _ := e.do(http.MethodPost, "/api/v1/scripts",
		map[string]any{"name": "x", "platform": "linux", "kind": "exec", "argv": []string{"id"}},
		withCookie(viewer, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("viewer create script: %d", resp.StatusCode)
	}

	// Create an exec script (operator) scoped to org A.
	body := map[string]any{
		"name": "List users", "description": "who", "platform": "linux", "kind": "exec",
		"argv": []string{"getent", "passwd"}, "timeout_s": 30, "org_id": e.org.ID,
	}
	resp, b := e.do(http.MethodPost, "/api/v1/scripts", body, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create script: %d %s", resp.StatusCode, b)
	}
	var created struct {
		Script store.Script `json:"script"`
	}
	if err := json.Unmarshal(b, &created); err != nil || created.Script.ID == 0 ||
		created.Script.Kind != "exec" || len(created.Script.Argv) != 2 || created.Script.OrgID == nil {
		t.Fatalf("created script %s (%v)", b, err)
	}
	if a := e.auditActions("script.created"); len(a) != 1 || *a[0].TargetID != created.Script.ID {
		t.Fatalf("script.created audit %+v", a)
	}

	// Validation.
	bad := []map[string]any{
		{"name": "", "platform": "linux", "kind": "exec", "argv": []string{"id"}},
		{"name": "x", "platform": "bogus", "kind": "exec", "argv": []string{"id"}},
		{"name": "x", "platform": "linux", "kind": "exec", "argv": []string{}},
		{"name": "x", "platform": "linux", "kind": "script", "shell": "sh", "body": ""},
		{"name": "x", "platform": "linux", "kind": "script", "shell": "powershell", "body": "echo"},
		{"name": "x", "platform": "linux", "kind": "nope"},
	}
	for _, bd := range bad {
		if resp, rb := e.do(http.MethodPost, "/api/v1/scripts", bd, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, rb) != "invalid_request" {
			t.Fatalf("bad script %v: %d %s", bd, resp.StatusCode, rb)
		}
	}

	// Update: change to a script-kind body; get reflects it.
	upd := map[string]any{"name": "List users", "platform": "any", "kind": "script", "shell": "sh", "body": "id\n", "timeout_s": 45}
	resp, b = e.do(http.MethodPut, "/api/v1/scripts/"+itoa(created.Script.ID), upd, withCookie(admin, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("update script: %d %s", resp.StatusCode, b)
	}
	resp, b = e.do(http.MethodGet, "/api/v1/scripts/"+itoa(created.Script.ID), nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("get script: %d %s", resp.StatusCode, b)
	}
	var got struct {
		Script store.Script `json:"script"`
	}
	if err := json.Unmarshal(b, &got); err != nil || got.Script.Kind != "script" || got.Script.Body != "id\n" || got.Script.Platform != "any" {
		t.Fatalf("updated script %s (%v)", b, err)
	}

	// --- RS2 ---------------------------------------------------------------
	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatal(err)
	}
	scB, err := e.st.CreateScript(ctx, store.CreateScriptParams{
		OrgID: &orgB.ID, Name: "b-only", Platform: "linux", Kind: "exec", Argv: []string{"id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	global, err := e.st.CreateScript(ctx, store.CreateScriptParams{
		OrgID: nil, Name: "everywhere", Platform: "any", Kind: "exec", Argv: []string{"uptime"},
	})
	if err != nil {
		t.Fatal(err)
	}

	op, _ := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{e.org.ID})
	resp, b = e.do(http.MethodGet, "/api/v1/scripts", nil, withCookie(op, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("scoped list scripts: %d %s", resp.StatusCode, b)
	}
	var list struct {
		Scripts []store.Script `json:"scripts"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	for _, sc := range list.Scripts {
		if sc.ID == scB.ID || sc.ID == global.ID {
			t.Fatalf("scoped list leaked script %d (%s)", sc.ID, b)
		}
		if sc.OrgID == nil || *sc.OrgID != e.org.ID {
			t.Fatalf("scoped list has non-A script: %+v", sc)
		}
	}

	// Cross-org get / update / delete all 404 (no side effect, RS2).
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		h := withCookie(op, nil)
		var bd any
		if m != http.MethodGet {
			h = withCookie(op, csrfHeader)
		}
		if m == http.MethodPut {
			bd = map[string]any{"name": "hax", "platform": "linux", "kind": "exec", "argv": []string{"id"}}
		}
		if resp, rb := e.do(m, "/api/v1/scripts/"+itoa(scB.ID), bd, h); resp.StatusCode != 404 || errorCode(t, rb) != "not_found" {
			t.Fatalf("scoped %s cross-org script: %d %s", m, resp.StatusCode, rb)
		}
	}
	// The global script is also invisible to a scoped user.
	if resp, rb := e.do(http.MethodGet, "/api/v1/scripts/"+itoa(global.ID), nil, withCookie(op, nil)); resp.StatusCode != 404 {
		t.Fatalf("scoped get global script: %d %s", resp.StatusCode, rb)
	}

	// Creating a global script requires all-orgs: a scoped operator is 403.
	if resp, rb := e.do(http.MethodPost, "/api/v1/scripts",
		map[string]any{"name": "g", "platform": "any", "kind": "exec", "argv": []string{"id"}},
		withCookie(op, csrfHeader)); resp.StatusCode != 403 || errorCode(t, rb) != "forbidden" {
		t.Fatalf("scoped create global: %d %s", resp.StatusCode, rb)
	}
	// An admin can.
	if resp, rb := e.do(http.MethodPost, "/api/v1/scripts",
		map[string]any{"name": "g2", "platform": "any", "kind": "exec", "argv": []string{"id"}},
		withCookie(admin, csrfHeader)); resp.StatusCode != 201 {
		t.Fatalf("admin create global: %d %s", resp.StatusCode, rb)
	}

	// Delete (admin), then gone.
	if resp, _ := e.do(http.MethodDelete, "/api/v1/scripts/"+itoa(created.Script.ID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("delete script: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/scripts/"+itoa(created.Script.ID), nil, withCookie(admin, nil)); resp.StatusCode != 404 {
		t.Fatalf("deleted script still readable: %d", resp.StatusCode)
	}
}
