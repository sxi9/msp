package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/agent"
	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// jobCommandJSON mirrors store.JobCommand for the POST /jobs response.
type jobCommandJSON struct {
	DeviceID  int64  `json:"device_id"`
	Hostname  string `json:"hostname"`
	OrgID     int64  `json:"org_id"`
	CommandID int64  `json:"command_id"`
	Skipped   bool   `json:"skipped"`
	Note      string `json:"note"`
}

type jobProgressJSON struct {
	JobID  int64 `json:"job_id"`
	Total  int   `json:"total"`
	Queued int   `json:"queued"`
	Sent   int   `json:"sent"`
	Done   int   `json:"done"`
	Failed int   `json:"failed"`
}

// TestAutomations is the AUT1 proof driven end to end through the real server
// handler and the real Linux agent, with the store clock (store.Now) moved by hand
// for the scheduled/triggered transitions (no sleeping, no external anything):
//
//   - a saved library script bulk-run on a multi-device target becomes one job
//     (total 2); the real Linux agent runs its command to done and the job progress
//     reflects it; an android device in the same target is skipped with a note;
//     job.created is audited;
//   - RS2: an operator scoped to org A sees only A's scripts/jobs/automations and
//     cannot target or reference org B's devices or scripts (404);
//   - a scheduled automation fires when the clock advances, missed ticks do not
//     stack, and automation.ran is audited;
//   - a triggered automation runs its script on the alerting device when a matching
//     alert opens (through the real alert-open hook), the per-device cooldown blocks
//     an immediate re-run, and automation.triggered is audited.
func TestAutomations(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the AUT1 e2e proof runs the real Linux agent")
	}
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := &logBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server/agent logs:\n%s", logs.String())
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

	// --- bootstrap: admin, two orgs, an enrollment token in A ----------------
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
	token, _, err := st.CreateEnrollmentToken(ctx, orgA.ID, "e2e", 5, 24*time.Hour, &admin.ID)
	if err != nil {
		t.Fatalf("create enrollment token: %v", err)
	}

	// --- enroll and run the real Linux agent (device A in org A) --------------
	cfg, err := agent.Enroll(ctx, ts.URL, token, "", false, 1, agentVersion)
	if err != nil {
		t.Fatalf("agent enroll: %v", err)
	}
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(agentCtx, cfg, agentVersion, logger) }()
	agentStopped := false
	waitAgent := func() {
		t.Helper()
		stopAgent()
		if agentStopped {
			return
		}
		agentStopped = true
		select {
		case err := <-agentDone:
			if err != nil {
				t.Fatalf("agent.Run returned %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("agent.Run did not return within 30 s of cancel")
		}
	}
	defer waitAgent()
	devAID := cfg.DeviceID

	// A second (never-running) linux device and an android device, both in org A.
	mkDevice := func(org store.Org, uid, host, platform string) store.Device {
		t.Helper()
		d, _, err := st.UpsertDevice(ctx, store.UpsertDeviceParams{
			OrgID: org.ID, DeviceUID: uid, Hostname: host, Platform: platform,
			Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
		})
		if err != nil {
			t.Fatalf("upsert device %s: %v", uid, err)
		}
		return d
	}
	devB := mkDevice(orgA, "uid-bulk-b", "acme-ws2", protocol.PlatformLinux)
	androidDev := mkDevice(orgA, "uid-android", "acme-phone", protocol.PlatformAndroid)
	devInB := mkDevice(orgB, "uid-bcorp", "bcorp-ws", protocol.PlatformLinux)

	// --- library scripts: one in A (used for the bulk run + automations), one in B.
	scriptA, err := st.CreateScript(ctx, store.CreateScriptParams{
		OrgID: &orgA.ID, Name: "echo-ok", Platform: store.ScriptPlatformAny,
		Kind: protocol.KindExec, Argv: []string{"echo", "aut1-ok"}, TimeoutS: 30, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("create script A: %v", err)
	}
	scriptB, err := st.CreateScript(ctx, store.CreateScriptParams{
		OrgID: &orgB.ID, Name: "b-script", Platform: store.ScriptPlatformAny,
		Kind: protocol.KindExec, Argv: []string{"echo", "b"}, TimeoutS: 30, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("create script B: %v", err)
	}

	// --- automations created directly so their due times are under test control.
	// base is safely in the future so nothing fires until the clock is moved there.
	base := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	far := base.Add(1000 * time.Hour)
	targetDevB, _ := json.Marshal(store.TargetSpec{DeviceIDs: []int64{devB.ID}})
	schedA, err := st.CreateAutomation(ctx, store.CreateAutomationParams{
		OrgID: &orgA.ID, Name: "nightly-echo", Enabled: true, Trigger: store.AutomationTriggerSchedule,
		ScriptID: scriptA.ID, TargetJSON: string(targetDevB), ScheduleJSON: `{"every_minutes":5}`,
		NextRunAt: &base, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("create scheduled automation: %v", err)
	}
	alertA, err := st.CreateAutomation(ctx, store.CreateAutomationParams{
		OrgID: &orgA.ID, Name: "on-offline", Enabled: true, Trigger: store.AutomationTriggerAlert,
		ScriptID: scriptA.ID, TargetJSON: "{}", AlertJSON: `{"alert_type":"offline"}`, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("create alert automation: %v", err)
	}
	// Org B automation (never due in our window) and a job, so RS2 has B rows to hide.
	bAuto, err := st.CreateAutomation(ctx, store.CreateAutomationParams{
		OrgID: &orgB.ID, Name: "b-auto", Enabled: true, Trigger: store.AutomationTriggerSchedule,
		ScriptID: scriptB.ID, TargetJSON: "{}", ScheduleJSON: `{"every_minutes":10}`,
		NextRunAt: &far, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("create org B automation: %v", err)
	}
	bScriptID := scriptB.ID
	bJob, err := st.CreateJob(ctx, store.CreateJobParams{
		OrgID: &orgB.ID, Name: "b-job", Origin: store.JobOriginManual, ScriptID: &bScriptID, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("create org B job: %v", err)
	}

	// auditCount counts audit rows with an action (store view = every org).
	auditCount := func(action string) int {
		t.Helper()
		rows, err := st.ListAudit(ctx, 2000, nil, nil, 0)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		n := 0
		for _, e := range rows {
			if e.Action == action {
				n++
			}
		}
		return n
	}
	jobsByOrigin := func(origin string) []store.Job {
		t.Helper()
		all, err := st.ListJobs(ctx, nil, nil, 2000)
		if err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		var out []store.Job
		for _, j := range all {
			if j.Origin == origin {
				out = append(out, j)
			}
		}
		return out
	}

	// --- admin login + a scoped operator (org A only) ------------------------
	adminD := newDash(t, ts.URL)
	if code := adminD.do(http.MethodPost, "/api/v1/login", map[string]string{"email": adminEmail, "password": adminPassword}, nil); code != 200 {
		t.Fatalf("admin login: status %d", code)
	}
	var opResp struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if code := adminD.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "op-a@example.com", "password": "op-a-pass-123", "role": "operator",
		"all_orgs": false, "org_ids": []int64{orgA.ID},
	}, &opResp); code != 201 {
		t.Fatalf("create scoped operator: status %d", code)
	}
	op := newDash(t, ts.URL)
	if code := op.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "op-a@example.com", "password": "op-a-pass-123"}, nil); code != 200 {
		t.Fatalf("operator login: status %d", code)
	}

	// --- bulk run: scriptA over [devA, devB, android]; total 2, android skipped.
	var bulk struct {
		JobID   int64            `json:"job_id"`
		Total   int              `json:"total"`
		Devices []jobCommandJSON `json:"devices"`
	}
	if code := adminD.do(http.MethodPost, "/api/v1/jobs", map[string]any{
		"script_id": scriptA.ID,
		"target":    map[string]any{"device_ids": []int64{devAID, devB.ID, androidDev.ID}},
	}, &bulk); code != 201 {
		t.Fatalf("bulk run: status %d", code)
	}
	if bulk.Total != 2 {
		t.Fatalf("bulk job total = %d, want 2 (android skipped)", bulk.Total)
	}
	var devACmdID int64
	androidSkipped := false
	for _, jc := range bulk.Devices {
		switch jc.DeviceID {
		case devAID:
			devACmdID = jc.CommandID
		case androidDev.ID:
			if !jc.Skipped || jc.Note == "" {
				t.Errorf("android row = %+v, want skipped with a note", jc)
			}
			androidSkipped = true
		}
	}
	if !androidSkipped {
		t.Fatalf("android device missing from fan-out breakdown: %+v", bulk.Devices)
	}
	if devACmdID == 0 {
		t.Fatalf("no command created for the running linux device: %+v", bulk.Devices)
	}
	// The real agent runs device A's command to done.
	done := adminD.waitCommand(devACmdID, "done", 15*time.Second)
	if done.Stdout != "aut1-ok\n" || exitCode(done) != 0 {
		t.Errorf("bulk command result: stdout %q exit %d", done.Stdout, exitCode(done))
	}
	// Progress reflects it: done >= 1, device B still queued, total 2.
	var jobDetail struct {
		Job struct {
			ID       int64           `json:"id"`
			Total    int             `json:"total"`
			Progress jobProgressJSON `json:"progress"`
		} `json:"job"`
	}
	if code := adminD.do(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", bulk.JobID), nil, &jobDetail); code != 200 {
		t.Fatalf("get job: status %d", code)
	}
	if jobDetail.Job.Progress.Total != 2 || jobDetail.Job.Progress.Done < 1 || jobDetail.Job.Progress.Queued < 1 {
		t.Errorf("job progress = %+v, want total 2 / done >= 1 / queued >= 1", jobDetail.Job.Progress)
	}
	if n := auditCount("job.created"); n != 1 {
		t.Errorf("job.created audit = %d, want 1", n)
	}

	// --- RS2: the scoped operator sees only org A, never org B ---------------
	var scripts struct {
		Scripts []store.Script `json:"scripts"`
	}
	if code := op.do(http.MethodGet, "/api/v1/scripts", nil, &scripts); code != 200 {
		t.Fatalf("operator GET /scripts: status %d", code)
	}
	if !containsScript(scripts.Scripts, scriptA.ID) || containsScript(scripts.Scripts, scriptB.ID) {
		t.Fatalf("operator scripts = %+v, want A (%d) and not B (%d)", scripts.Scripts, scriptA.ID, scriptB.ID)
	}
	var autos struct {
		Automations []store.Automation `json:"automations"`
	}
	if code := op.do(http.MethodGet, "/api/v1/automations", nil, &autos); code != 200 {
		t.Fatalf("operator GET /automations: status %d", code)
	}
	if !containsAutomation(autos.Automations, schedA.ID) || !containsAutomation(autos.Automations, alertA.ID) || containsAutomation(autos.Automations, bAuto.ID) {
		t.Fatalf("operator automations leaked org B or missed A: %+v", autos.Automations)
	}
	var jobs struct {
		Jobs []struct {
			ID    int64  `json:"id"`
			OrgID *int64 `json:"org_id"`
		} `json:"jobs"`
	}
	if code := op.do(http.MethodGet, "/api/v1/jobs", nil, &jobs); code != 200 {
		t.Fatalf("operator GET /jobs: status %d", code)
	}
	sawBulk := false
	for _, j := range jobs.Jobs {
		if j.ID == bJob.ID {
			t.Fatalf("operator jobs leaked org B job %d", bJob.ID)
		}
		if j.ID == bulk.JobID {
			sawBulk = true
		}
	}
	if !sawBulk {
		t.Fatalf("operator did not see its own org A bulk job %d", bulk.JobID)
	}
	// Direct access to an org B resource is 404 (never a distinguishable 403).
	for _, p := range []string{
		fmt.Sprintf("/api/v1/scripts/%d", scriptB.ID),
		fmt.Sprintf("/api/v1/automations/%d", bAuto.ID),
		fmt.Sprintf("/api/v1/jobs/%d", bJob.ID),
	} {
		var e errorJSON
		if code := op.do(http.MethodGet, p, nil, &e); code != 404 || e.Error != "not_found" {
			t.Errorf("operator GET %s: status %d error %q, want 404 not_found", p, code, e.Error)
		}
	}
	// A bulk run referencing org B's script, or targeting org B's device, is 404.
	{
		var e errorJSON
		if code := op.do(http.MethodPost, "/api/v1/jobs", map[string]any{
			"script_id": scriptB.ID, "target": map[string]any{"device_ids": []int64{devAID}},
		}, &e); code != 404 || e.Error != "not_found" {
			t.Errorf("operator bulk with org B script: status %d error %q, want 404 not_found", code, e.Error)
		}
		e = errorJSON{}
		if code := op.do(http.MethodPost, "/api/v1/jobs", map[string]any{
			"script_id": scriptA.ID, "target": map[string]any{"device_ids": []int64{devInB.ID}},
		}, &e); code != 404 || e.Error != "not_found" {
			t.Errorf("operator bulk targeting org B device: status %d error %q, want 404 not_found", code, e.Error)
		}
		// An automation targeting org B's device is likewise refused (404).
		e = errorJSON{}
		if code := op.do(http.MethodPost, "/api/v1/automations", map[string]any{
			"org_id": orgA.ID, "name": "x", "trigger": "schedule", "script_id": scriptA.ID,
			"target": map[string]any{"device_ids": []int64{devInB.ID}}, "schedule": map[string]any{"every_minutes": 5},
		}, &e); code != 404 || e.Error != "not_found" {
			t.Errorf("operator automation targeting org B device: status %d error %q, want 404 not_found", code, e.Error)
		}
	}
	// The three rejected POSTs created nothing: the only manual jobs are the org A
	// bulk job and the pre-seeded org B job.
	if n := len(jobsByOrigin(store.JobOriginManual)); n != 2 {
		t.Fatalf("RS2 rejected POSTs changed the manual-job set: %d (want 2: bulk + the org B seed)", n)
	}

	// --- scheduled automation: fires when the clock reaches its due time -----
	st.Now = func() time.Time { return base }
	srv.RunScheduledOnce(ctx)
	sched := jobsByOrigin(store.JobOriginSchedule)
	if len(sched) != 1 || sched[0].Total != 1 || sched[0].AutomationID == nil || *sched[0].AutomationID != schedA.ID {
		t.Fatalf("after first schedule tick jobs = %+v, want 1 job total 1 for automation %d", sched, schedA.ID)
	}
	if n := auditCount("automation.ran"); n != 1 {
		t.Errorf("automation.ran audit = %d, want 1", n)
	}
	if ga, _ := st.GetAutomation(ctx, schedA.ID); ga.NextRunAt == nil || !ga.NextRunAt.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("next_run_at after first tick = %v, want %v", mustTime(ga.NextRunAt), base.Add(5*time.Minute))
	}
	// Same clock: not due again.
	srv.RunScheduledOnce(ctx)
	if n := len(jobsByOrigin(store.JobOriginSchedule)); n != 1 {
		t.Fatalf("a tick before the due time created a job: %d schedule jobs", n)
	}
	// Missed ticks do not stack: jump far past several intervals -> exactly one run.
	st.Now = func() time.Time { return base.Add(200 * time.Minute) }
	srv.RunScheduledOnce(ctx)
	if n := len(jobsByOrigin(store.JobOriginSchedule)); n != 2 {
		t.Fatalf("missed ticks stacked: %d schedule jobs, want 2", n)
	}
	if n := auditCount("automation.ran"); n != 2 {
		t.Errorf("automation.ran audit after second tick = %d, want 2", n)
	}
	if ga, _ := st.GetAutomation(ctx, schedA.ID); ga.NextRunAt == nil || !ga.NextRunAt.Equal(base.Add(205*time.Minute)) {
		t.Fatalf("next computed from now: %v, want %v", mustTime(ga.NextRunAt), base.Add(205*time.Minute))
	}

	// --- triggered automation: runs on the alerting device via the open hook -
	waitAgent() // stop the real agent so device A can go (and stay) offline
	st.Now = func() time.Time { return base.Add(300 * time.Minute) }
	server.EvaluateNow(srv) // opens device A's offline alert -> fires the hook
	trig := jobsByOrigin(store.JobOriginTrigger)
	if len(trig) != 1 || trig[0].Total != 1 || trig[0].AutomationID == nil || *trig[0].AutomationID != alertA.ID {
		t.Fatalf("after alert open trigger jobs = %+v, want 1 job total 1 for automation %d", trig, alertA.ID)
	}
	if n := auditCount("automation.triggered"); n != 1 {
		t.Errorf("automation.triggered audit = %d, want 1", n)
	}
	cmds, err := st.ListCommands(ctx, devAID, 50)
	if err != nil {
		t.Fatalf("list device A commands: %v", err)
	}
	trigCmd := false
	for _, c := range cmds {
		if c.JobID == trig[0].ID {
			trigCmd = true
			if c.Kind != protocol.KindExec {
				t.Errorf("triggered command kind %q, want exec", c.Kind)
			}
		}
	}
	if !trigCmd {
		t.Fatalf("no command on the alerting device for trigger job %d", trig[0].ID)
	}

	// Cooldown: an immediate re-trigger on the same device is blocked.
	alert, err := st.GetOpenAlert(ctx, devAID, "offline")
	if err != nil {
		t.Fatalf("get open offline alert: %v", err)
	}
	srv.TriggerAutomationsForAlert(ctx, alert)
	if n := len(jobsByOrigin(store.JobOriginTrigger)); n != 1 {
		t.Fatalf("cooldown did not block the immediate re-run: %d trigger jobs", n)
	}
	// Past the cooldown (10 min): it runs again.
	st.Now = func() time.Time { return base.Add(311 * time.Minute) }
	srv.TriggerAutomationsForAlert(ctx, alert)
	if n := len(jobsByOrigin(store.JobOriginTrigger)); n != 2 {
		t.Fatalf("post-cooldown re-run: %d trigger jobs, want 2", n)
	}
	if n := auditCount("automation.triggered"); n != 2 {
		t.Errorf("automation.triggered audit after cooldown = %d, want 2", n)
	}
}

func containsScript(scripts []store.Script, id int64) bool {
	for _, s := range scripts {
		if s.ID == id {
			return true
		}
	}
	return false
}

func containsAutomation(autos []store.Automation, id int64) bool {
	for _, a := range autos {
		if a.ID == id {
			return true
		}
	}
	return false
}

func mustTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
