package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// scopedOrgJSON / scopedAuditJSON carry the org_id that the generic
// e2e_test.go shapes omit, so the isolation assertions can prove a scoped
// operator never sees org B's rows (RS2).
type scopedOrgJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type scopedAuditJSON struct {
	Action string `json:"action"`
	OrgID  *int64 `json:"org_id"`
}

// TestOrgScoping is the RS2 isolation proof driven end to end through the real
// server handler: two orgs A and B each with a device, an open alert and audit
// history, and an operator scoped to A only. The operator must see only A in
// every list endpoint, must get a 404 (never a distinguishable 403) with no
// side effect on every device- and org-scoped endpoint of org B, and must gain
// B the moment an admin grants it through PUT /users/{id}/orgs. Admins are
// unaffected throughout.
func TestOrgScoping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := &logBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})

	st, err := store.Open(filepath.Join(t.TempDir(), "msp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	srv := server.New(st, server.Config{
		SampleEvery: time.Second,
		LostGrace:   time.Second,
		Version:     agentVersion,
	}, logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// --- bootstrap: admin, two orgs, a device + open alert in each -----------
	hash, err := server.HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	admin, err := st.CreateUser(ctx, adminEmail, hash, "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	orgA, err := st.CreateOrg(ctx, "Acme Dental")
	if err != nil {
		t.Fatalf("create org A: %v", err)
	}
	orgB, err := st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}

	mkDevice := func(org store.Org, uid, host string) store.Device {
		d, _, err := st.UpsertDevice(ctx, store.UpsertDeviceParams{
			OrgID: org.ID, DeviceUID: uid, Hostname: host, Platform: protocol.PlatformLinux,
			Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
		})
		if err != nil {
			t.Fatalf("upsert device %s: %v", uid, err)
		}
		return d
	}
	devA := mkDevice(orgA, "uid-a", "acme-ws")
	devB := mkDevice(orgB, "uid-b", "beta-ws")

	if _, err := st.OpenAlert(ctx, orgA.ID, devA.ID, "offline", "high", "a offline"); err != nil {
		t.Fatalf("open alert A: %v", err)
	}
	if _, err := st.OpenAlert(ctx, orgB.ID, devB.ID, "offline", "high", "b offline"); err != nil {
		t.Fatalf("open alert B: %v", err)
	}
	bAlerts, err := st.ListAlerts(ctx, true, &orgB.ID, nil, 0)
	if err != nil || len(bAlerts) != 1 {
		t.Fatalf("org B open alerts = %d (%v)", len(bAlerts), err)
	}
	alertB := bAlerts[0].ID

	// A queued shell command in org B gives a concrete command id to probe for
	// the GET /commands/{id} and GET /shell/{id}/transcript 404s.
	shellB, err := st.CreateCommand(ctx, store.CreateCommandParams{
		DeviceID: devB.ID, OrgID: orgB.ID, CreatedBy: admin.ID,
		Kind: protocol.KindShell, Shell: "bash", TimeoutS: protocol.DefaultShellTimeoutS,
	})
	if err != nil {
		t.Fatalf("create shell command in B: %v", err)
	}

	// Audit history: one row tied to A, one to B, one system row (org_id NULL).
	// The scoped operator must see only the A row.
	st.Audit(ctx, store.AuditEntry{ActorType: "user", ActorID: &admin.ID, OrgID: &orgA.ID, Action: "org.touch_a"})
	st.Audit(ctx, store.AuditEntry{ActorType: "user", ActorID: &admin.ID, OrgID: &orgB.ID, Action: "org.touch_b"})
	st.Audit(ctx, store.AuditEntry{ActorType: "system", Action: "system.touch"})

	// --- admin logs in and creates an operator scoped to org A only ----------
	adminD := newDash(t, ts.URL)
	if code := adminD.do(http.MethodPost, "/api/v1/login", map[string]string{"email": adminEmail, "password": adminPassword}, nil); code != 200 {
		t.Fatalf("admin login: status %d", code)
	}
	var createResp struct {
		User struct {
			ID      int64   `json:"id"`
			Role    string  `json:"role"`
			AllOrgs bool    `json:"all_orgs"`
			OrgIDs  []int64 `json:"org_ids"`
		} `json:"user"`
	}
	if code := adminD.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "scoped@example.com", "password": "scoped-pass-1", "role": "operator",
		"all_orgs": false, "org_ids": []int64{orgA.ID},
	}, &createResp); code != 201 {
		t.Fatalf("create scoped operator: status %d", code)
	}
	opID := createResp.User.ID
	if createResp.User.AllOrgs || len(createResp.User.OrgIDs) != 1 || createResp.User.OrgIDs[0] != orgA.ID {
		t.Fatalf("created operator not scoped to A: %+v", createResp.User)
	}

	op := newDash(t, ts.URL)
	if code := op.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "scoped@example.com", "password": "scoped-pass-1"}, nil); code != 200 {
		t.Fatalf("operator login: status %d", code)
	}

	// --- list endpoints show only org A --------------------------------------
	var orgs struct {
		Orgs []scopedOrgJSON `json:"orgs"`
	}
	if code := op.do(http.MethodGet, "/api/v1/orgs", nil, &orgs); code != 200 {
		t.Fatalf("scoped GET /orgs: status %d", code)
	}
	if len(orgs.Orgs) != 1 || orgs.Orgs[0].ID != orgA.ID {
		t.Fatalf("scoped GET /orgs = %+v, want only org A (%d)", orgs.Orgs, orgA.ID)
	}

	devs := op.devices()
	if len(devs) != 1 || devs[0].ID != devA.ID {
		t.Fatalf("scoped GET /devices = %+v, want only device A (%d)", devs, devA.ID)
	}

	var summary struct {
		Orgs       int `json:"orgs"`
		Devices    int `json:"devices"`
		OpenAlerts int `json:"open_alerts"`
	}
	if code := op.do(http.MethodGet, "/api/v1/summary", nil, &summary); code != 200 {
		t.Fatalf("scoped GET /summary: status %d", code)
	}
	if summary.Orgs != 1 || summary.Devices != 1 || summary.OpenAlerts != 1 {
		t.Fatalf("scoped summary = %+v, want 1 org / 1 device / 1 open alert", summary)
	}

	var alerts struct {
		Alerts []struct {
			ID    int64 `json:"id"`
			OrgID int64 `json:"org_id"`
		} `json:"alerts"`
	}
	if code := op.do(http.MethodGet, "/api/v1/alerts?state=all", nil, &alerts); code != 200 {
		t.Fatalf("scoped GET /alerts: status %d", code)
	}
	if len(alerts.Alerts) != 1 || alerts.Alerts[0].OrgID != orgA.ID {
		t.Fatalf("scoped GET /alerts = %+v, want only org A", alerts.Alerts)
	}

	var audit struct {
		Entries []scopedAuditJSON `json:"entries"`
	}
	if code := op.do(http.MethodGet, "/api/v1/audit?limit=1000", nil, &audit); code != 200 {
		t.Fatalf("scoped GET /audit: status %d", code)
	}
	sawA := false
	for _, en := range audit.Entries {
		if en.OrgID == nil {
			t.Fatalf("scoped audit leaked a system (null-org) row: %s", en.Action)
		}
		if *en.OrgID == orgB.ID {
			t.Fatalf("scoped audit leaked an org B row: %s", en.Action)
		}
		if *en.OrgID == orgA.ID {
			sawA = true
		}
	}
	if !sawA {
		t.Fatalf("scoped audit did not include any org A row: %+v", audit.Entries)
	}

	// --- 404 (no side effect) across orgs on every scoped endpoint -----------
	cmdsBefore, _ := st.ListCommands(ctx, devB.ID, 200)
	devBPath := fmt.Sprintf("/api/v1/devices/%d", devB.ID)
	gets := []string{
		fmt.Sprintf("/api/v1/orgs/%d", orgB.ID),
		devBPath,
		fmt.Sprintf("/api/v1/shell/%d/transcript", shellB.ID),
	}
	for _, p := range gets {
		var e errorJSON
		if code := op.do(http.MethodGet, p, nil, &e); code != 404 || e.Error != "not_found" {
			t.Fatalf("scoped GET %s: status %d error %q, want 404 not_found", p, code, e.Error)
		}
	}
	posts := []struct {
		path string
		body any
	}{
		{devBPath + "/commands", protocol.Command{Kind: protocol.KindExec, Argv: []string{"id"}}},
		{devBPath + "/commands", protocol.Command{Kind: protocol.KindLock}},
		{devBPath + "/shell", map[string]any{"shell": "bash"}},
		{devBPath + "/remote-desktop", nil},
		{fmt.Sprintf("/api/v1/alerts/%d/ack", alertB), nil},
	}
	for _, p := range posts {
		var e errorJSON
		if code := op.do(http.MethodPost, p.path, p.body, &e); code != 404 || e.Error != "not_found" {
			t.Fatalf("scoped POST %s: status %d error %q, want 404 not_found", p.path, code, e.Error)
		}
	}

	// No side effect: no new command row in org B, the alert still unacked, and
	// none of the blocked writes left an audit row (checked via the admin view,
	// which sees every org).
	cmdsAfter, _ := st.ListCommands(ctx, devB.ID, 200)
	if len(cmdsAfter) != len(cmdsBefore) {
		t.Fatalf("scoped 404 POSTs created %d command rows in org B", len(cmdsAfter)-len(cmdsBefore))
	}
	if a, _ := st.GetAlert(ctx, alertB); a.AcknowledgedBy != nil {
		t.Fatal("scoped ack across orgs acknowledged org B's alert")
	}
	var adminAudit struct {
		Entries []auditJSON `json:"entries"`
	}
	if code := adminD.do(http.MethodGet, "/api/v1/audit?limit=1000", nil, &adminAudit); code != 200 {
		t.Fatalf("admin GET /audit: status %d", code)
	}
	for _, action := range []string{"command.queued", "device.lock_requested", "shell.opened", "device.remote_desktop_opened", "alert.ack"} {
		if n := countAudit(adminAudit.Entries, action, "user"); n != 0 {
			t.Fatalf("scoped 404 still wrote a %s audit row (%d)", action, n)
		}
	}

	// An all-orgs admin is unaffected: it reads org B's device directly.
	if got := adminD.device(devB.ID); got.ID != devB.ID {
		t.Fatalf("admin GET org B device = %+v, want device %d", got, devB.ID)
	}

	// --- admin grants org B; the same operator now sees B --------------------
	if code := adminD.do(http.MethodPut, fmt.Sprintf("/api/v1/users/%d/orgs", opID),
		map[string]any{"all_orgs": false, "org_ids": []int64{orgA.ID, orgB.ID}}, nil); code != 200 {
		t.Fatalf("admin PUT /users/%d/orgs grant B: status %d", opID, code)
	}
	if code := op.do(http.MethodGet, "/api/v1/orgs", nil, &orgs); code != 200 || len(orgs.Orgs) != 2 {
		t.Fatalf("after grant scoped GET /orgs: status %d orgs %+v, want 2", code, orgs.Orgs)
	}
	if got := op.device(devB.ID); got.ID != devB.ID {
		t.Fatalf("after grant scoped GET org B device = %+v, want device %d", got, devB.ID)
	}
	// The grant wrote exactly one user.orgs_changed audit row for this operator.
	if code := adminD.do(http.MethodGet, "/api/v1/audit?limit=1000", nil, &adminAudit); code != 200 {
		t.Fatalf("admin GET /audit after grant: status %d", code)
	}
	if n := countAudit(adminAudit.Entries, "user.orgs_changed", "user"); n != 1 {
		t.Fatalf("user.orgs_changed audit rows = %d, want 1", n)
	}
}
