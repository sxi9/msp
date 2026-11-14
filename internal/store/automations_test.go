package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAutomationCRUD(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, _, _ := fixture(t, s)
	sc := mustScript(t, s, &org.ID, "run")

	next := s.Now().Add(5 * time.Minute)
	a, err := s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &org.ID, Name: "nightly", Enabled: true, Trigger: AutomationTriggerSchedule,
		ScriptID: sc.ID, TargetJSON: `{"status":"online"}`, ScheduleJSON: `{"every_minutes":60}`,
		NextRunAt: &next, CreatedBy: &u.ID,
	})
	if err != nil {
		t.Fatalf("CreateAutomation: %v", err)
	}
	if a.Trigger != AutomationTriggerSchedule || !a.Enabled || a.NextRunAt == nil {
		t.Fatalf("automation = %+v", a)
	}
	if a.AlertJSON != "" {
		t.Fatalf("schedule automation alert_json should be empty, got %q", a.AlertJSON)
	}

	spec, err := a.ScheduleSpec()
	if err != nil || spec.EveryMinutes != 60 {
		t.Fatalf("ScheduleSpec = %+v (%v)", spec, err)
	}
	tgt, err := a.TargetSpec()
	if err != nil || tgt.Status != "online" {
		t.Fatalf("TargetSpec = %+v (%v)", tgt, err)
	}

	upd, err := s.UpdateAutomation(ctx, a.ID, UpdateAutomationParams{
		OrgID: &org.ID, Name: "weekly", Enabled: true, Trigger: AutomationTriggerSchedule,
		ScriptID: sc.ID, TargetJSON: `{"platform":"linux"}`, ScheduleJSON: `{"every_minutes":30}`, NextRunAt: &next,
	})
	if err != nil {
		t.Fatalf("UpdateAutomation: %v", err)
	}
	if upd.Name != "weekly" {
		t.Fatalf("updated = %+v", upd)
	}

	if err := s.SetAutomationEnabled(ctx, a.ID, false); err != nil {
		t.Fatalf("SetAutomationEnabled: %v", err)
	}
	got, _ := s.GetAutomation(ctx, a.ID)
	if got.Enabled {
		t.Fatalf("automation still enabled after disable")
	}

	if err := s.DeleteAutomation(ctx, a.ID); err != nil {
		t.Fatalf("DeleteAutomation: %v", err)
	}
	if _, err := s.GetAutomation(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAutomation after delete = %v, want ErrNotFound", err)
	}
}

func TestCreateAutomationBadScript(t *testing.T) {
	s, _ := openTest(t)
	_, err := s.CreateAutomation(context.Background(), CreateAutomationParams{
		Name: "x", Enabled: true, Trigger: AutomationTriggerSchedule, ScriptID: 9999, TargetJSON: "{}",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateAutomation bad script = %v, want ErrNotFound", err)
	}
}

func TestDueAutomations(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, u, _, _ := fixture(t, s)
	sc := mustScript(t, s, &org.ID, "run")

	due := c.now().Add(10 * time.Minute)
	a, err := s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &org.ID, Name: "sched", Enabled: true, Trigger: AutomationTriggerSchedule,
		ScriptID: sc.ID, TargetJSON: "{}", ScheduleJSON: `{"every_minutes":10}`, NextRunAt: &due, CreatedBy: &u.ID,
	})
	if err != nil {
		t.Fatalf("CreateAutomation: %v", err)
	}
	// A disabled one and an alert one must never appear in DueAutomations.
	disabled, _ := s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &org.ID, Name: "off", Enabled: false, Trigger: AutomationTriggerSchedule,
		ScriptID: sc.ID, TargetJSON: "{}", ScheduleJSON: `{"every_minutes":10}`, NextRunAt: &due,
	})
	_, _ = s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &org.ID, Name: "onalert", Enabled: true, Trigger: AutomationTriggerAlert,
		ScriptID: sc.ID, TargetJSON: "{}", AlertJSON: `{}`,
	})

	// Before the due time: nothing.
	early, err := s.DueAutomations(ctx, due.Add(-time.Minute))
	if err != nil {
		t.Fatalf("DueAutomations early: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("early due = %+v", early)
	}

	// At/after the due time: only the enabled schedule automation.
	now, err := s.DueAutomations(ctx, due)
	if err != nil {
		t.Fatalf("DueAutomations: %v", err)
	}
	if len(now) != 1 || now[0].ID != a.ID {
		t.Fatalf("due = %+v, want only %d", now, a.ID)
	}

	// SetAutomationRan pushes next_run_at out so missed ticks do not stack.
	ranAt := due
	nextRun := due.Add(10 * time.Minute)
	if err := s.SetAutomationRan(ctx, a.ID, ranAt, &nextRun); err != nil {
		t.Fatalf("SetAutomationRan: %v", err)
	}
	after, err := s.DueAutomations(ctx, due)
	if err != nil {
		t.Fatalf("DueAutomations after ran: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("still due after ran = %+v", after)
	}
	reread, _ := s.GetAutomation(ctx, a.ID)
	if reread.LastRunAt == nil || !reread.LastRunAt.Equal(storedTime(ranAt)) {
		t.Fatalf("last_run_at = %v, want %v", reread.LastRunAt, storedTime(ranAt))
	}
	if reread.NextRunAt == nil || !reread.NextRunAt.Equal(storedTime(nextRun)) {
		t.Fatalf("next_run_at = %v, want %v", reread.NextRunAt, storedTime(nextRun))
	}
	_ = disabled
}

func TestMatchAlertAutomations(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, _ := s.CreateOrg(ctx, "A")
	orgB, _ := s.CreateOrg(ctx, "B")
	scA := mustScript(t, s, &orgA.ID, "a")
	scB := mustScript(t, s, &orgB.ID, "b")
	scG := mustScript(t, s, nil, "g")

	// Org A, disk_full, min high.
	hiDisk, _ := s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &orgA.ID, Name: "a-disk-high", Enabled: true, Trigger: AutomationTriggerAlert,
		ScriptID: scA.ID, TargetJSON: "{}", AlertJSON: `{"alert_type":"disk_full","min_severity":"high"}`,
	})
	// Org A, any type, min low.
	anyA, _ := s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &orgA.ID, Name: "a-any", Enabled: true, Trigger: AutomationTriggerAlert,
		ScriptID: scA.ID, TargetJSON: "{}", AlertJSON: `{"min_severity":"low"}`,
	})
	// Org B automation must never match an org A alert.
	_, _ = s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &orgB.ID, Name: "b-any", Enabled: true, Trigger: AutomationTriggerAlert,
		ScriptID: scB.ID, TargetJSON: "{}", AlertJSON: `{}`,
	})
	// Global automation (org nil) matches any org.
	glob, _ := s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: nil, Name: "global", Enabled: true, Trigger: AutomationTriggerAlert,
		ScriptID: scG.ID, TargetJSON: "{}", AlertJSON: `{}`,
	})
	// Disabled must not match.
	_, _ = s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &orgA.ID, Name: "off", Enabled: false, Trigger: AutomationTriggerAlert,
		ScriptID: scA.ID, TargetJSON: "{}", AlertJSON: `{}`,
	})

	// A medium disk_full alert in org A: the high gate excludes hiDisk; anyA and
	// the global one match; org B's does not.
	medDisk := Alert{OrgID: orgA.ID, Type: "disk_full", Severity: "medium"}
	got, err := s.MatchAlertAutomations(ctx, medDisk)
	if err != nil {
		t.Fatalf("MatchAlertAutomations: %v", err)
	}
	ids := idset(got)
	if ids[hiDisk.ID] {
		t.Fatalf("high-severity automation matched a medium alert")
	}
	if !ids[anyA.ID] || !ids[glob.ID] {
		t.Fatalf("expected anyA and global to match: %+v", ids)
	}
	if len(got) != 2 {
		t.Fatalf("match count = %d, want 2 (%+v)", len(got), ids)
	}

	// A high disk_full alert now also matches the high gate.
	hiAlert := Alert{OrgID: orgA.ID, Type: "disk_full", Severity: "high"}
	got2, _ := s.MatchAlertAutomations(ctx, hiAlert)
	if !idset(got2)[hiDisk.ID] {
		t.Fatalf("high alert should match high-gated automation: %+v", idset(got2))
	}

	// A memory_high alert does not match the disk-typed automation.
	memAlert := Alert{OrgID: orgA.ID, Type: "memory_high", Severity: "high"}
	got3, _ := s.MatchAlertAutomations(ctx, memAlert)
	if idset(got3)[hiDisk.ID] {
		t.Fatalf("disk-typed automation matched a memory alert")
	}
}

func TestLastAutomationDeviceRunAt(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)
	sc := mustScript(t, s, &org.ID, "run")
	a, _ := s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &org.ID, Name: "trig", Enabled: true, Trigger: AutomationTriggerAlert,
		ScriptID: sc.ID, TargetJSON: "{}", AlertJSON: `{}`, CreatedBy: &u.ID,
	})

	// Never run yet.
	last, err := s.LastAutomationDeviceRunAt(ctx, a.ID, dev.ID)
	if err != nil {
		t.Fatalf("LastAutomationDeviceRunAt: %v", err)
	}
	if last != nil {
		t.Fatalf("expected nil before any run, got %v", last)
	}

	// A trigger job on the device under this automation sets the last-run time.
	job, _ := s.CreateJob(ctx, CreateJobParams{OrgID: &org.ID, Name: "t", Origin: JobOriginTrigger, ScriptID: &sc.ID, AutomationID: &a.ID, CreatedBy: &u.ID})
	if _, err := s.CreateCommandsForDevices(ctx, job.ID, sc, u.ID, []Device{dev}); err != nil {
		t.Fatalf("fan out: %v", err)
	}
	last, err = s.LastAutomationDeviceRunAt(ctx, a.ID, dev.ID)
	if err != nil {
		t.Fatalf("LastAutomationDeviceRunAt after run: %v", err)
	}
	if last == nil || !last.Equal(storedTime(s.Now())) {
		t.Fatalf("last run = %v, want %v", last, storedTime(s.Now()))
	}

	// A different automation on the same device sees no run.
	other, _ := s.CreateAutomation(ctx, CreateAutomationParams{
		OrgID: &org.ID, Name: "other", Enabled: true, Trigger: AutomationTriggerAlert,
		ScriptID: sc.ID, TargetJSON: "{}", AlertJSON: `{}`,
	})
	if l, _ := s.LastAutomationDeviceRunAt(ctx, other.ID, dev.ID); l != nil {
		t.Fatalf("unrelated automation should have no run, got %v", l)
	}
}

func TestListAutomationsRS2(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, _ := s.CreateOrg(ctx, "A")
	orgB, _ := s.CreateOrg(ctx, "B")
	scA := mustScript(t, s, &orgA.ID, "a")
	scB := mustScript(t, s, &orgB.ID, "b")
	scG := mustScript(t, s, nil, "g")
	mustAuto(t, s, &orgA.ID, scA.ID, "a1")
	mustAuto(t, s, &orgB.ID, scB.ID, "b1")
	mustAuto(t, s, nil, scG.ID, "global")

	all, err := s.ListAutomations(ctx, nil)
	if err != nil {
		t.Fatalf("ListAutomations nil: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all automations = %d, want 3", len(all))
	}
	onlyA, _ := s.ListAutomations(ctx, []int64{orgA.ID})
	if len(onlyA) != 1 || onlyA[0].Name != "a1" {
		t.Fatalf("scoped A = %+v", onlyA)
	}
	none, _ := s.ListAutomations(ctx, []int64{})
	if len(none) != 0 {
		t.Fatalf("empty visible = %d, want 0", len(none))
	}
}

func idset(as []Automation) map[int64]bool {
	m := map[int64]bool{}
	for _, a := range as {
		m[a.ID] = true
	}
	return m
}

func mustAuto(t *testing.T, s *Store, orgID *int64, scriptID int64, name string) Automation {
	t.Helper()
	a, err := s.CreateAutomation(context.Background(), CreateAutomationParams{
		OrgID: orgID, Name: name, Enabled: true, Trigger: AutomationTriggerAlert,
		ScriptID: scriptID, TargetJSON: "{}", AlertJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateAutomation %s: %v", name, err)
	}
	return a
}
