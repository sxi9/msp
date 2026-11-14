package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sxi9/msp/internal/store"
)

// TestDashboardAutomations covers automation CRUD, validation, the enable toggle
// and RS2 org scoping.
func TestDashboardAutomations(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	scA, err := e.st.CreateScript(ctx, store.CreateScriptParams{OrgID: &e.org.ID, Name: "a", Platform: "linux", Kind: "exec", Argv: []string{"id"}})
	if err != nil {
		t.Fatal(err)
	}

	// Viewer cannot create.
	if resp, _ := e.do(http.MethodPost, "/api/v1/automations",
		map[string]any{"name": "x", "trigger": "schedule", "script_id": scA.ID, "schedule": map[string]any{"every_minutes": 5}, "target": map[string]any{}},
		withCookie(viewer, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("viewer create automation: %d", resp.StatusCode)
	}

	// Create a schedule automation; next_run_at is seeded.
	resp, b := e.do(http.MethodPost, "/api/v1/automations", map[string]any{
		"name": "nightly", "org_id": e.org.ID, "trigger": "schedule", "script_id": scA.ID,
		"schedule": map[string]any{"every_minutes": 5}, "target": map[string]any{"platform": "linux"},
	}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create schedule automation: %d %s", resp.StatusCode, b)
	}
	var created struct {
		Automation store.Automation `json:"automation"`
	}
	if err := json.Unmarshal(b, &created); err != nil || created.Automation.ID == 0 ||
		created.Automation.Trigger != "schedule" || created.Automation.NextRunAt == nil {
		t.Fatalf("created automation %s (%v)", b, err)
	}
	wantNext := e.clock.now().Add(5 * 60 * 1e9) // +5m
	if !created.Automation.NextRunAt.Equal(wantNext) {
		t.Fatalf("next_run_at %v, want %v", created.Automation.NextRunAt, wantNext)
	}
	if a := e.auditActions("automation.created"); len(a) != 1 || *a[0].TargetID != created.Automation.ID {
		t.Fatalf("automation.created audit %+v", a)
	}

	// Create an alert automation.
	resp, b = e.do(http.MethodPost, "/api/v1/automations", map[string]any{
		"name": "on-offline", "org_id": e.org.ID, "trigger": "alert", "script_id": scA.ID,
		"alert": map[string]any{"alert_type": "offline", "min_severity": "high"}, "target": map[string]any{},
	}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create alert automation: %d %s", resp.StatusCode, b)
	}

	// Validation.
	bad := []map[string]any{
		{"name": "", "trigger": "schedule", "script_id": scA.ID, "schedule": map[string]any{"every_minutes": 5}, "target": map[string]any{}},
		{"name": "x", "trigger": "schedule", "script_id": scA.ID, "target": map[string]any{}},                                                                                                   // no every_minutes
		{"name": "x", "trigger": "schedule", "script_id": scA.ID, "schedule": map[string]any{"every_minutes": 5}, "alert": map[string]any{"alert_type": "offline"}, "target": map[string]any{}}, // schedule + alert
		{"name": "x", "trigger": "alert", "script_id": scA.ID, "alert": map[string]any{"alert_type": "bogus"}, "target": map[string]any{}},
		{"name": "x", "trigger": "alert", "script_id": scA.ID, "alert": map[string]any{"min_severity": "urgent"}, "target": map[string]any{}},
		{"name": "x", "trigger": "bogus", "script_id": scA.ID, "target": map[string]any{}},
		{"name": "x", "trigger": "schedule", "schedule": map[string]any{"every_minutes": 5}, "target": map[string]any{}}, // no script
	}
	for _, bd := range bad {
		if resp, rb := e.do(http.MethodPost, "/api/v1/automations", bd, withCookie(admin, csrfHeader)); resp.StatusCode != 400 {
			t.Fatalf("bad automation %v: %d %s", bd, resp.StatusCode, rb)
		}
	}

	// Enable toggle: disable clears next_run_at, enable re-seeds it.
	path := "/api/v1/automations/" + itoa(created.Automation.ID) + "/enable"
	resp, b = e.do(http.MethodPost, path, map[string]any{"enabled": false}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("disable: %d %s", resp.StatusCode, b)
	}
	var tog struct {
		Automation store.Automation `json:"automation"`
	}
	json.Unmarshal(b, &tog)
	if tog.Automation.Enabled || tog.Automation.NextRunAt != nil {
		t.Fatalf("disabled automation %+v", tog.Automation)
	}
	resp, b = e.do(http.MethodPost, path, map[string]any{"enabled": true}, withCookie(admin, csrfHeader))
	json.Unmarshal(b, &tog)
	if !tog.Automation.Enabled || tog.Automation.NextRunAt == nil {
		t.Fatalf("re-enabled automation %+v", tog.Automation)
	}
	if len(e.auditActions("automation.disabled")) != 1 || len(e.auditActions("automation.enabled")) != 1 {
		t.Fatal("enable/disable audit rows")
	}

	// Update.
	resp, b = e.do(http.MethodPut, "/api/v1/automations/"+itoa(created.Automation.ID), map[string]any{
		"name": "nightly-2", "trigger": "schedule", "script_id": scA.ID,
		"schedule": map[string]any{"every_minutes": 10}, "target": map[string]any{},
	}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("update automation: %d %s", resp.StatusCode, b)
	}

	// Delete.
	if resp, _ := e.do(http.MethodDelete, "/api/v1/automations/"+itoa(created.Automation.ID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("delete automation: %d", resp.StatusCode)
	}
}

// TestDashboardAutomationsScoping is the RS2 proof for automations.
func TestDashboardAutomationsScoping(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	orgA := e.org

	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatal(err)
	}
	scA, err := e.st.CreateScript(ctx, store.CreateScriptParams{OrgID: &orgA.ID, Name: "a", Platform: "linux", Kind: "exec", Argv: []string{"id"}})
	if err != nil {
		t.Fatal(err)
	}
	scB, err := e.st.CreateScript(ctx, store.CreateScriptParams{OrgID: &orgB.ID, Name: "b", Platform: "linux", Kind: "exec", Argv: []string{"id"}})
	if err != nil {
		t.Fatal(err)
	}
	autoB, err := e.st.CreateAutomation(ctx, store.CreateAutomationParams{
		OrgID: &orgB.ID, Name: "b-auto", Enabled: true, Trigger: "alert", ScriptID: scB.ID,
		TargetJSON: "{}", AlertJSON: `{"alert_type":"offline"}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	op, _ := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{orgA.ID})

	// Scoped create of a global automation is 403.
	if resp, b := e.do(http.MethodPost, "/api/v1/automations", map[string]any{
		"name": "g", "trigger": "schedule", "script_id": scA.ID, "schedule": map[string]any{"every_minutes": 5}, "target": map[string]any{},
	}, withCookie(op, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("scoped create global automation: %d %s", resp.StatusCode, b)
	}
	// Referencing org B's script from an org-A automation is a 400 that hides it.
	if resp, b := e.do(http.MethodPost, "/api/v1/automations", map[string]any{
		"name": "x", "org_id": orgA.ID, "trigger": "schedule", "script_id": scB.ID, "schedule": map[string]any{"every_minutes": 5}, "target": map[string]any{},
	}, withCookie(op, csrfHeader)); resp.StatusCode != 400 {
		t.Fatalf("scoped ref org-B script: %d %s", resp.StatusCode, b)
	}
	// Targeting org B is a 404.
	if resp, b := e.do(http.MethodPost, "/api/v1/automations", map[string]any{
		"name": "x", "org_id": orgA.ID, "trigger": "schedule", "script_id": scA.ID, "schedule": map[string]any{"every_minutes": 5},
		"target": map[string]any{"org_id": orgB.ID},
	}, withCookie(op, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("scoped target org B: %d %s", resp.StatusCode, b)
	}
	// Scoped get/list hides org B's automation.
	if resp, _ := e.do(http.MethodGet, "/api/v1/automations/"+itoa(autoB.ID), nil, withCookie(op, nil)); resp.StatusCode != 404 {
		t.Fatalf("scoped get org-B automation: %d", resp.StatusCode)
	}
	resp, b := e.do(http.MethodGet, "/api/v1/automations", nil, withCookie(op, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("scoped list: %d %s", resp.StatusCode, b)
	}
	var list struct {
		Automations []store.Automation `json:"automations"`
	}
	json.Unmarshal(b, &list)
	for _, a := range list.Automations {
		if a.ID == autoB.ID || a.OrgID == nil || *a.OrgID != orgA.ID {
			t.Fatalf("scoped list leaked automation %+v", a)
		}
	}
}
