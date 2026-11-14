package server

import (
	"net/http"
	"strings"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Bulk-run jobs dashboard API (AUT1): POST /api/v1/jobs fans a library script (or
// an inline command) out across the caller's visible devices matching a target
// filter, as one tracked job. GET /api/v1/jobs and GET /api/v1/jobs/{id} report
// a job and its derived progress. Everything is RS2 org-scoped: a target can
// never reach a device outside the caller's visible org set, and a scoped user
// sees only their own jobs. Registered through dashboardRoutes like the other
// dashboard_*.go files.

func init() {
	dashboardRoutes = append(dashboardRoutes, registerJobs)
}

func registerJobs(s *Server, mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle("GET "+pattern, s.dashRoute(RoleViewer, h))
	}
	mux.Handle("POST /api/v1/jobs", s.dashRoute(RoleOperator, s.handleCreateJob))
	get("/api/v1/jobs", s.handleListJobs)
	get("/api/v1/jobs/{id}", s.handleGetJob)
}

// inlineCommand is a one-off command to fan out without saving a library script.
type inlineCommand struct {
	Platform string   `json:"platform"`
	Kind     string   `json:"kind"`
	Argv     []string `json:"argv"`
	Shell    string   `json:"shell"`
	Body     string   `json:"body"`
	TimeoutS int      `json:"timeout_s"`
}

type jobRequest struct {
	Name     string           `json:"name"`
	ScriptID int64            `json:"script_id"`
	Command  *inlineCommand   `json:"command"`
	Target   store.TargetSpec `json:"target"`
}

// jobView bundles a job with its derived progress for the list/detail responses.
type jobView struct {
	store.Job
	Progress store.JobProgress `json:"progress"`
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	u := userFrom(ctx)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return
	}

	// Resolve the script to fan out: a saved library script (visible to the caller,
	// else 404 like an invisible file) or an inline command (scope nil).
	var (
		sc        store.Script
		scriptID  *int64
		hasScript bool
	)
	switch {
	case req.ScriptID > 0:
		loaded, err := s.st.GetScript(ctx, req.ScriptID)
		if err != nil {
			s.dashStoreError(w, err, "get script")
			return
		}
		if !s.notifyRowVisible(w, r, loaded.OrgID) {
			return
		}
		sc = loaded
		id := loaded.ID
		scriptID = &id
		hasScript = true
	case req.Command != nil:
		c := req.Command
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = "Inline command"
		}
		if detail := validateScriptShape(name, "", c.Platform, c.Kind, c.Argv, c.Shell, c.Body, c.TimeoutS); detail != "" {
			writeError(w, http.StatusBadRequest, "invalid_request", detail)
			return
		}
		sc = store.Script{
			OrgID: nil, Name: name, Platform: c.Platform, Kind: c.Kind,
			Argv: c.Argv, Shell: c.Shell, Body: c.Body, TimeoutS: c.TimeoutS,
		}
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "script_id or command is required")
		return
	}

	// RS2: a target may never reach a device outside the caller's visible org set
	// (404, no side effect), and a client-scoped script may only target its own
	// client's devices.
	if !s.validateTargetForCaller(w, r, req.Target, sc.OrgID) {
		return
	}
	devices, err := s.resolveTargetDevices(ctx, req.Target, visible, sc.OrgID)
	if err != nil {
		s.log.Error("resolve job target", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = sc.Name
	}
	actor := u.ID
	job, err := s.st.CreateJob(ctx, store.CreateJobParams{
		OrgID: jobOrg(sc.OrgID, req.Target.OrgID, devices), Name: name, Origin: store.JobOriginManual,
		ScriptID: scriptID, CreatedBy: &actor,
	})
	if err != nil {
		s.log.Error("create job", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	jcs, err := s.st.CreateCommandsForDevices(ctx, job.ID, sc, actor, devices)
	if err != nil {
		s.log.Error("create job commands", "job", job.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	s.wakeJobDevices(jcs)

	job, err = s.st.GetJob(ctx, job.ID) // re-read for the updated total
	if err != nil {
		s.log.Error("reload job", "job", job.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	skipped := len(jcs) - job.Total
	detail := map[string]any{"origin": job.Origin, "name": job.Name, "total": job.Total, "skipped": skipped}
	if hasScript {
		detail["script_id"] = *scriptID
	} else {
		detail["kind"] = sc.Kind
	}
	s.audit(ctx, "user", &actor, job.OrgID, "job.created", "job", &job.ID, clientIP(r), detail)
	s.log.Info("job created", "job", job.ID, "total", job.Total, "skipped", skipped, "user", actor)
	writeJSON(w, http.StatusCreated, map[string]any{"job_id": job.ID, "total": job.Total, "devices": jcs})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	orgID, ok := dashOrgIDParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "org_id must be a positive integer")
		return
	}
	visible, vok := s.dashVisibleOrgs(w, r)
	if !vok {
		return
	}
	if orgID != nil && !s.dashOrgVisible(w, r, *orgID) {
		return
	}
	rows, err := s.st.ListJobs(r.Context(), orgID, visible, dashLimit(r, 200, 1000))
	if err != nil {
		s.log.Error("list jobs", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	out := make([]jobView, 0, len(rows))
	for _, j := range rows {
		p, err := s.st.JobProgress(r.Context(), j.ID)
		if err != nil {
			s.log.Error("job progress", "job", j.ID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "")
			return
		}
		out = append(out, jobView{Job: j, Progress: p})
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	job, err := s.st.GetJob(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get job")
		return
	}
	if !s.notifyRowVisible(w, r, job.OrgID) {
		return
	}
	p, err := s.st.JobProgress(ctx, job.ID)
	if err != nil {
		s.log.Error("job progress", "job", job.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": jobView{Job: job, Progress: p}})
}

// wakeJobDevices wakes the parked sync of every device that got a command so the
// fan-out's commands dispatch without waiting for the next poll.
func (s *Server) wakeJobDevices(jcs []store.JobCommand) {
	for _, jc := range jcs {
		if !jc.Skipped && jc.CommandID != 0 {
			s.notifier.Notify(jc.DeviceID)
		}
	}
}

// jobOrg derives a job's org_id for RS2 list visibility: a client-scoped script
// pins the job to its client; otherwise an explicit target org wins, else the
// single org shared by every resolved device (nil when they span orgs or none
// matched, so only an all-orgs caller sees it).
func jobOrg(scriptOrg, targetOrg *int64, devices []store.Device) *int64 {
	if scriptOrg != nil {
		return scriptOrg
	}
	if targetOrg != nil {
		return targetOrg
	}
	return singleOrg(devices)
}

// singleOrg returns the one org id shared by every device, or nil when the set is
// empty or spans more than one org.
func singleOrg(devices []store.Device) *int64 {
	if len(devices) == 0 {
		return nil
	}
	first := devices[0].OrgID
	for _, d := range devices[1:] {
		if d.OrgID != first {
			return nil
		}
	}
	return &first
}

// validTargetStatus and validTargetPlatform gate the optional target filter
// fields; "" means "no narrowing on that axis".
func validTargetStatus(s string) bool {
	switch s {
	case "", "online", "offline", "pending":
		return true
	}
	return false
}

func validTargetPlatform(p string) bool {
	switch p {
	case "", protocol.PlatformLinux, protocol.PlatformWindows, protocol.PlatformAndroid:
		return true
	}
	return false
}
