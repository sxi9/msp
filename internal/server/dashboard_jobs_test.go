package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// jobCreateResponse mirrors the POST /api/v1/jobs body.
type jobCreateResponse struct {
	JobID   int64              `json:"job_id"`
	Total   int                `json:"total"`
	Devices []store.JobCommand `json:"devices"`
}

// TestDashboardJobsBulkRun covers a manual bulk run: the fan-out across a 2-linux
// + 1-android target queues 2 commands and skips android with a per-device note,
// progress reflects a completion, and job.created is audited.
func TestDashboardJobsBulkRun(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := dashAdmin(e)

	linux1, token1 := e.enroll("uid-l1", protocol.PlatformLinux)
	linux2, _ := e.enroll("uid-l2", protocol.PlatformLinux)
	android, _ := e.enroll("uid-a", protocol.PlatformAndroid)

	sc, err := e.st.CreateScript(ctx, store.CreateScriptParams{
		OrgID: &e.org.ID, Name: "uptime", Platform: "any", Kind: "exec", Argv: []string{"uptime"}, TimeoutS: 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, b := e.do(http.MethodPost, "/api/v1/jobs",
		map[string]any{"script_id": sc.ID, "target": map[string]any{}}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("create job: %d %s", resp.StatusCode, b)
	}
	var jr jobCreateResponse
	if err := json.Unmarshal(b, &jr); err != nil {
		t.Fatal(err)
	}
	if jr.Total != 2 || len(jr.Devices) != 3 {
		t.Fatalf("job fan-out total=%d devices=%d: %s", jr.Total, len(jr.Devices), b)
	}
	var androidSkipped bool
	cmdByDevice := map[int64]int64{}
	for _, d := range jr.Devices {
		if d.DeviceID == android {
			if !d.Skipped || d.Note == "" {
				t.Fatalf("android not skipped with a note: %+v", d)
			}
			androidSkipped = true
			continue
		}
		if d.Skipped || d.CommandID == 0 {
			t.Fatalf("linux device skipped: %+v", d)
		}
		cmdByDevice[d.DeviceID] = d.CommandID
	}
	if !androidSkipped || len(cmdByDevice) != 2 {
		t.Fatalf("fan-out devices wrong: %s", b)
	}
	_ = linux2

	if a := e.auditActions("job.created"); len(a) != 1 || a[0].Detail["total"].(float64) != 2 || a[0].Detail["origin"] != "manual" {
		t.Fatalf("job.created audit %+v", a)
	}

	// Progress: 2 queued initially.
	resp, b = e.do(http.MethodGet, "/api/v1/jobs/"+itoa(jr.JobID), nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("get job: %d %s", resp.StatusCode, b)
	}
	var jv struct {
		Job jobView `json:"job"`
	}
	if err := json.Unmarshal(b, &jv); err != nil {
		t.Fatal(err)
	}
	if jv.Job.Progress.Total != 2 || jv.Job.Progress.Queued != 2 {
		t.Fatalf("initial progress %+v", jv.Job.Progress)
	}

	// Drive linux1's command to done through the agent path; progress updates.
	res := protocol.CommandResult{ExitCode: 0, Stdout: "ok\n", StartedAt: e.clock.now(), FinishedAt: e.clock.now()}
	if _, _, err := claimAndComplete(e, linux1, token1, cmdByDevice[linux1], res); err != nil {
		t.Fatal(err)
	}
	resp, b = e.do(http.MethodGet, "/api/v1/jobs/"+itoa(jr.JobID), nil, withCookie(admin, nil))
	if err := json.Unmarshal(b, &jv); err != nil {
		t.Fatal(err)
	}
	if jv.Job.Progress.Done != 1 || jv.Job.Progress.Queued != 1 || jv.Job.Progress.Total != 2 {
		t.Fatalf("progress after one done %+v: %s", jv.Job.Progress, b)
	}

	// List shows the job for the admin.
	resp, b = e.do(http.MethodGet, "/api/v1/jobs", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("list jobs: %d %s", resp.StatusCode, b)
	}
	var jl struct {
		Jobs []jobView `json:"jobs"`
	}
	if err := json.Unmarshal(b, &jl); err != nil || len(jl.Jobs) != 1 || jl.Jobs[0].ID != jr.JobID {
		t.Fatalf("list jobs %s (%v)", b, err)
	}
}

// TestDashboardJobsTargeting covers explicit device_ids and status/platform filter
// validation.
func TestDashboardJobsTargeting(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := dashAdmin(e)
	l1, _ := e.enroll("uid-l1", protocol.PlatformLinux)
	e.enroll("uid-l2", protocol.PlatformLinux)

	sc, err := e.st.CreateScript(ctx, store.CreateScriptParams{
		OrgID: &e.org.ID, Name: "x", Platform: "linux", Kind: "exec", Argv: []string{"id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// device_ids pins exactly one device.
	resp, b := e.do(http.MethodPost, "/api/v1/jobs",
		map[string]any{"script_id": sc.ID, "target": map[string]any{"device_ids": []int64{l1}}}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("job by device_ids: %d %s", resp.StatusCode, b)
	}
	var jr jobCreateResponse
	json.Unmarshal(b, &jr)
	if jr.Total != 1 || len(jr.Devices) != 1 || jr.Devices[0].DeviceID != l1 {
		t.Fatalf("device_ids fan-out %s", b)
	}

	// Bad filter enums.
	for _, tgt := range []map[string]any{{"status": "weird"}, {"platform": "bsd"}} {
		if resp, rb := e.do(http.MethodPost, "/api/v1/jobs",
			map[string]any{"script_id": sc.ID, "target": tgt}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 {
			t.Fatalf("bad target %v: %d %s", tgt, resp.StatusCode, rb)
		}
	}
	// Neither script_id nor command.
	if resp, _ := e.do(http.MethodPost, "/api/v1/jobs", map[string]any{"target": map[string]any{}}, withCookie(admin, csrfHeader)); resp.StatusCode != 400 {
		t.Fatalf("job without script: %d", resp.StatusCode)
	}
}

// TestDashboardJobsScoping is the RS2 proof for jobs: a scoped operator cannot run
// a job against another client's devices or script, and sees only their own jobs.
func TestDashboardJobsScoping(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	orgA := e.org
	devA, _ := e.enroll("uid-a", protocol.PlatformLinux)

	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatal(err)
	}
	devB, _, err := e.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
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

	op, _ := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{orgA.ID})

	cmdsBefore, _ := e.st.ListCommands(ctx, devB.ID, 200)
	// Target org B, a device in B, or a script in B: all 404, no side effect.
	cases := []map[string]any{
		{"script_id": scA.ID, "target": map[string]any{"org_id": orgB.ID}},
		{"script_id": scA.ID, "target": map[string]any{"device_ids": []int64{devB.ID}}},
		{"script_id": scB.ID, "target": map[string]any{}},
	}
	for _, c := range cases {
		if resp, b := e.do(http.MethodPost, "/api/v1/jobs", c, withCookie(op, csrfHeader)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
			t.Fatalf("scoped job %v: %d %s (want 404)", c, resp.StatusCode, b)
		}
	}
	if after, _ := e.st.ListCommands(ctx, devB.ID, 200); len(after) != len(cmdsBefore) {
		t.Fatal("scoped cross-org job created a command in org B")
	}
	if n := len(e.auditActions("job.created")); n != 0 {
		t.Fatalf("scoped cross-org job wrote %d job.created audits", n)
	}

	// A legitimate job in org A works and is visible to the operator; org B's jobs are not.
	resp, b := e.do(http.MethodPost, "/api/v1/jobs",
		map[string]any{"script_id": scA.ID, "target": map[string]any{"device_ids": []int64{devA}}}, withCookie(op, csrfHeader))
	if resp.StatusCode != 201 {
		t.Fatalf("scoped job in A: %d %s", resp.StatusCode, b)
	}
	jobB, err := e.st.CreateJob(ctx, store.CreateJobParams{OrgID: &orgB.ID, Name: "b-job", Origin: store.JobOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	resp, b = e.do(http.MethodGet, "/api/v1/jobs", nil, withCookie(op, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("scoped list jobs: %d %s", resp.StatusCode, b)
	}
	var jl struct {
		Jobs []jobView `json:"jobs"`
	}
	json.Unmarshal(b, &jl)
	for _, j := range jl.Jobs {
		if j.ID == jobB.ID || j.OrgID == nil || *j.OrgID != orgA.ID {
			t.Fatalf("scoped list leaked a non-A job: %+v", j)
		}
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/jobs/"+itoa(jobB.ID), nil, withCookie(op, nil)); resp.StatusCode != 404 {
		t.Fatalf("scoped get org-B job: %d", resp.StatusCode)
	}
}
