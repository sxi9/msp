package server

import (
	"context"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// scheduleJobs returns the schedule-origin jobs for the env (newest first).
func scheduleJobs(e *testEnv, origin string) []store.Job {
	e.t.Helper()
	all, err := e.st.ListJobs(context.Background(), nil, nil, 1000)
	if err != nil {
		e.t.Fatalf("ListJobs: %v", err)
	}
	var out []store.Job
	for _, j := range all {
		if j.Origin == origin {
			out = append(out, j)
		}
	}
	return out
}

// TestRunScheduledOnce drives the scheduler with the fake clock: a due automation
// fans its script out under a schedule job, advances next_run_at, and missed ticks
// never stack.
func TestRunScheduledOnce(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.enroll("uid-l1", protocol.PlatformLinux)
	e.enroll("uid-l2", protocol.PlatformLinux)

	sc, err := e.st.CreateScript(ctx, store.CreateScriptParams{OrgID: &e.org.ID, Name: "uptime", Platform: "any", Kind: "exec", Argv: []string{"uptime"}})
	if err != nil {
		t.Fatal(err)
	}
	now := e.clock.now()
	a, err := e.st.CreateAutomation(ctx, store.CreateAutomationParams{
		OrgID: &e.org.ID, Name: "every5", Enabled: true, Trigger: "schedule", ScriptID: sc.ID,
		TargetJSON: "{}", ScheduleJSON: `{"every_minutes":5}`, NextRunAt: &now, CreatedBy: &e.admin.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Due now: one run.
	e.srv.RunScheduledOnce(ctx)
	jobs := scheduleJobs(e, store.JobOriginSchedule)
	if len(jobs) != 1 || jobs[0].Total != 2 || jobs[0].AutomationID == nil || *jobs[0].AutomationID != a.ID {
		t.Fatalf("after first run jobs=%+v", jobs)
	}
	if ar := e.auditActions("automation.ran"); len(ar) != 1 || ar[0].Detail["total"].(float64) != 2 {
		t.Fatalf("automation.ran audit %+v", ar)
	}
	ga, _ := e.st.GetAutomation(ctx, a.ID)
	if ga.NextRunAt == nil || !ga.NextRunAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("next_run_at after run %v", ga.NextRunAt)
	}

	// Not due again until next; a run right now is a no-op.
	e.srv.RunScheduledOnce(ctx)
	if n := len(scheduleJobs(e, store.JobOriginSchedule)); n != 1 {
		t.Fatalf("run before due created %d jobs", n)
	}

	// Missed ticks do not stack: advance far past several intervals; one run only.
	e.clock.advance(100 * time.Minute)
	e.srv.RunScheduledOnce(ctx)
	jobs = scheduleJobs(e, store.JobOriginSchedule)
	if len(jobs) != 2 {
		t.Fatalf("missed ticks stacked: %d schedule jobs", len(jobs))
	}
	ga, _ = e.st.GetAutomation(ctx, a.ID)
	if ga.NextRunAt == nil || !ga.NextRunAt.Equal(e.clock.now().Add(5*time.Minute)) {
		t.Fatalf("next computed from now: %v (now %v)", ga.NextRunAt, e.clock.now())
	}

	// A disabled automation never runs.
	if err := e.st.SetAutomationEnabled(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(100 * time.Minute)
	e.srv.RunScheduledOnce(ctx)
	if n := len(scheduleJobs(e, store.JobOriginSchedule)); n != 2 {
		t.Fatalf("disabled automation ran: %d jobs", n)
	}
}

// TestTriggerAutomationsForAlert drives the alert-trigger hook: a matching alert
// runs the automation's script on the alerting device once, the cooldown blocks an
// immediate re-run, and an android device is skipped for an exec script.
func TestTriggerAutomationsForAlert(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	linuxID, _ := e.enroll("uid-l", protocol.PlatformLinux)

	sc, err := e.st.CreateScript(ctx, store.CreateScriptParams{OrgID: &e.org.ID, Name: "remediate", Platform: "any", Kind: "exec", Argv: []string{"systemctl", "restart", "foo"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateAutomation(ctx, store.CreateAutomationParams{
		OrgID: &e.org.ID, Name: "on-alert", Enabled: true, Trigger: "alert", ScriptID: sc.ID,
		TargetJSON: "{}", AlertJSON: `{"min_severity":"low"}`, CreatedBy: &e.admin.ID,
	}); err != nil {
		t.Fatal(err)
	}

	openAlert := func(devID int64) store.Alert {
		if _, err := e.st.OpenAlert(ctx, e.org.ID, devID, alertOffline, severityHigh, "down"); err != nil {
			t.Fatal(err)
		}
		al, err := e.st.GetOpenAlert(ctx, devID, alertOffline)
		if err != nil {
			t.Fatal(err)
		}
		return al
	}

	// First open: one triggered job + command on the device.
	e.srv.TriggerAutomationsForAlert(ctx, openAlert(linuxID))
	if jobs := scheduleJobs(e, store.JobOriginTrigger); len(jobs) != 1 || jobs[0].Total != 1 {
		t.Fatalf("after first trigger jobs=%+v", jobs)
	}
	if tr := e.auditActions("automation.triggered"); len(tr) != 1 || tr[0].Detail["device_id"].(float64) != float64(linuxID) {
		t.Fatalf("automation.triggered audit %+v", tr)
	}
	cmds, _ := e.st.ListCommands(ctx, linuxID, 50)
	if len(cmds) != 1 || cmds[0].Kind != "exec" || cmds[0].JobID == 0 {
		t.Fatalf("triggered command %+v", cmds)
	}

	// Immediate re-trigger: cooldown blocks it.
	e.srv.TriggerAutomationsForAlert(ctx, openAlert(linuxID))
	if n := len(scheduleJobs(e, store.JobOriginTrigger)); n != 1 {
		t.Fatalf("cooldown did not block re-run: %d trigger jobs", n)
	}

	// Past the cooldown: it runs again.
	e.clock.advance(automationTriggerCooldown + time.Minute)
	e.srv.TriggerAutomationsForAlert(ctx, openAlert(linuxID))
	if n := len(scheduleJobs(e, store.JobOriginTrigger)); n != 2 {
		t.Fatalf("post-cooldown re-run: %d trigger jobs", n)
	}

	// Android is skipped for an exec script: no job, no command, no audit.
	androidID, _ := e.enroll("uid-a", protocol.PlatformAndroid)
	before := len(e.auditActions("automation.triggered"))
	e.srv.TriggerAutomationsForAlert(ctx, openAlert(androidID))
	if n := len(scheduleJobs(e, store.JobOriginTrigger)); n != 2 {
		t.Fatalf("android created a trigger job: %d", n)
	}
	if ac, _ := e.st.ListCommands(ctx, androidID, 50); len(ac) != 0 {
		t.Fatalf("android got %d commands", len(ac))
	}
	if after := len(e.auditActions("automation.triggered")); after != before {
		t.Fatalf("android wrote a triggered audit: %d -> %d", before, after)
	}
}
