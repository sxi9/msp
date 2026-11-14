package store

import (
	"context"
	"errors"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// mkDevice enrolls a device with the given platform under an org.
func mkDevice(t *testing.T, s *Store, orgID int64, uid, hostname, platform string) Device {
	t.Helper()
	d, _, err := s.UpsertDevice(context.Background(), UpsertDeviceParams{
		OrgID: orgID, DeviceUID: uid, Hostname: hostname, Platform: platform, Arch: "amd64",
		OSVersion: "x", AgentVersion: "0.1.0", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("UpsertDevice %s: %v", hostname, err)
	}
	return d
}

func TestCreateJobAndFanOut(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, dev1, _ := fixture(t, s)
	dev2 := mkDevice(t, s, org.ID, "uid-2", "ws-02", "linux")
	android := mkDevice(t, s, org.ID, "uid-a", "phone-01", "android")

	sc := mustScript(t, s, &org.ID, "patch-run")

	job, err := s.CreateJob(ctx, CreateJobParams{
		OrgID: &org.ID, Name: "bulk uptime", Origin: JobOriginManual, ScriptID: &sc.ID, CreatedBy: &u.ID,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.Total != 0 || job.Origin != JobOriginManual {
		t.Fatalf("new job = %+v", job)
	}

	results, err := s.CreateCommandsForDevices(ctx, job.ID, sc, u.ID, []Device{dev1, dev2, android})
	if err != nil {
		t.Fatalf("CreateCommandsForDevices: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	// First two create commands; android is skipped with a note.
	if results[0].Skipped || results[0].CommandID == 0 {
		t.Fatalf("dev1 result = %+v", results[0])
	}
	if results[1].Skipped || results[1].CommandID == 0 {
		t.Fatalf("dev2 result = %+v", results[1])
	}
	if !results[2].Skipped || results[2].CommandID != 0 || results[2].Note == "" {
		t.Fatalf("android result = %+v (want skipped with note)", results[2])
	}

	// jobs.total is the number of commands actually created.
	reread, err := s.GetJob(ctx, job.ID)
	if err != nil || reread.Total != 2 {
		t.Fatalf("GetJob total = %d (%v), want 2", reread.Total, err)
	}

	// Each created command carries the job_id.
	cmd, err := s.GetCommand(ctx, results[0].CommandID)
	if err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if cmd.JobID != job.ID {
		t.Fatalf("command job_id = %d, want %d", cmd.JobID, job.ID)
	}
	if cmd.Kind != "exec" || len(cmd.Argv) == 0 {
		t.Fatalf("command shape = %+v", cmd)
	}
}

func TestJobProgress(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, dev1, _ := fixture(t, s)
	dev2 := mkDevice(t, s, org.ID, "uid-2", "ws-02", "linux")
	sc := mustScript(t, s, &org.ID, "run")

	job, err := s.CreateJob(ctx, CreateJobParams{OrgID: &org.ID, Name: "j", Origin: JobOriginManual, ScriptID: &sc.ID, CreatedBy: &u.ID})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := s.CreateCommandsForDevices(ctx, job.ID, sc, u.ID, []Device{dev1, dev2}); err != nil {
		t.Fatalf("fan out: %v", err)
	}

	p, err := s.JobProgress(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobProgress: %v", err)
	}
	if p.Total != 2 || p.Queued != 2 || p.Done != 0 || p.Failed != 0 {
		t.Fatalf("progress after create = %+v", p)
	}

	// Drive dev1's command to done through the normal claim/complete path.
	claimed, err := s.ClaimQueuedCommands(ctx, dev1.ID, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimQueuedCommands = %d (%v)", len(claimed), err)
	}
	p, err = s.JobProgress(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobProgress sent: %v", err)
	}
	if p.Sent != 1 || p.Queued != 1 {
		t.Fatalf("progress after claim = %+v", p)
	}
	if _, err := s.CompleteCommand(ctx, claimed[0].ID, dev1.ID, protocol.CommandResult{ExitCode: 0, Stdout: "ok"}); err != nil {
		t.Fatalf("CompleteCommand: %v", err)
	}

	// Drive dev2's command to failed.
	claimed2, err := s.ClaimQueuedCommands(ctx, dev2.ID, 10)
	if err != nil || len(claimed2) != 1 {
		t.Fatalf("ClaimQueuedCommands dev2 = %d (%v)", len(claimed2), err)
	}
	if _, err := s.CompleteCommand(ctx, claimed2[0].ID, dev2.ID, protocol.CommandResult{ExitCode: 1, Error: "boom"}); err != nil {
		t.Fatalf("CompleteCommand dev2: %v", err)
	}

	p, err = s.JobProgress(ctx, job.ID)
	if err != nil {
		t.Fatalf("JobProgress done: %v", err)
	}
	if p.Total != 2 || p.Done != 1 || p.Failed != 1 || p.Queued != 0 || p.Sent != 0 {
		t.Fatalf("final progress = %+v", p)
	}
}

func TestListJobsRS2(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, _ := s.CreateOrg(ctx, "A")
	orgB, _ := s.CreateOrg(ctx, "B")

	mustJob(t, s, &orgA.ID, "a-job")
	mustJob(t, s, &orgB.ID, "b-job")
	mustJob(t, s, nil, "global-job")

	all, err := s.ListJobs(ctx, nil, nil, 0)
	if err != nil {
		t.Fatalf("ListJobs nil: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all jobs = %d, want 3", len(all))
	}

	onlyA, err := s.ListJobs(ctx, nil, []int64{orgA.ID}, 0)
	if err != nil {
		t.Fatalf("ListJobs A: %v", err)
	}
	if len(onlyA) != 1 || onlyA[0].Name != "a-job" {
		t.Fatalf("scoped A jobs = %+v", onlyA)
	}

	// org filter narrows to one client within the visible set.
	byOrg, err := s.ListJobs(ctx, &orgB.ID, nil, 0)
	if err != nil {
		t.Fatalf("ListJobs orgB: %v", err)
	}
	if len(byOrg) != 1 || byOrg[0].Name != "b-job" {
		t.Fatalf("org-filtered jobs = %+v", byOrg)
	}

	none, err := s.ListJobs(ctx, nil, []int64{}, 0)
	if err != nil {
		t.Fatalf("ListJobs empty: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("empty visible jobs = %d, want 0", len(none))
	}
}

func TestGetJobNotFound(t *testing.T) {
	s, _ := openTest(t)
	if _, err := s.GetJob(context.Background(), 123); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetJob missing = %v, want ErrNotFound", err)
	}
}

func mustJob(t *testing.T, s *Store, orgID *int64, name string) Job {
	t.Helper()
	j, err := s.CreateJob(context.Background(), CreateJobParams{OrgID: orgID, Name: name, Origin: JobOriginManual})
	if err != nil {
		t.Fatalf("CreateJob %s: %v", name, err)
	}
	return j
}
