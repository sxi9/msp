package server

import (
	"context"
	"net/http"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Automation execution (AUT1): the scheduled-automation tick and the
// alert-triggered hook, plus the shared device-target resolution used by both the
// bulk-run endpoint (dashboard_jobs.go) and the automation runs here.
//
// These two entry points are exported so the integrator wires them without this
// file touching the loop/hook owners:
//   - RunScheduledOnce(ctx) belongs in the jobs loop (server.go runJobs), on its
//     own ticker alongside evaluateOnce/pruneOnce, e.g.
//         case <-schedule.C: s.safely("automations", func() { s.RunScheduledOnce(jobsCtx) })
//   - TriggerAutomationsForAlert(ctx, alert) belongs in the alert-open hook
//     (alerts.go s.open), right after EnqueueAlertNotifications on a newly opened
//     alert:
//         s.EnqueueAlertNotifications(ctx, alert, store.EventOpened)
//         s.TriggerAutomationsForAlert(ctx, alert)
// See the drift note in this work package's report for the exact call sites.

// automationTriggerCooldown is the per-automation-per-device minimum spacing
// between alert-triggered runs: a flapping alert that re-opens within this window
// does not re-run the automation on the same device.
const automationTriggerCooldown = 10 * time.Minute

// RunScheduledOnce runs every schedule-trigger automation that is due now: it
// resolves each automation's target to its org-scoped visible devices, fans the
// automation's script out under a job (origin schedule), and advances
// last_run_at/next_run_at (next computed from now, so missed ticks do not stack).
// One automation's failure is logged and never stops the others.
func (s *Server) RunScheduledOnce(ctx context.Context) {
	now := s.now()
	due, err := s.st.DueAutomations(ctx, now)
	if err != nil {
		s.log.Error("automations: due", "err", err)
		return
	}
	for _, a := range due {
		s.runScheduledAutomation(ctx, a, now)
	}
}

func (s *Server) runScheduledAutomation(ctx context.Context, a store.Automation, now time.Time) {
	createdBy, ok := automationCreatedBy(a)
	if !ok {
		s.log.Error("automations: schedule run has no owner", "automation", a.ID)
		return
	}
	sc, err := s.st.GetScript(ctx, a.ScriptID)
	if err != nil {
		s.log.Error("automations: load script", "automation", a.ID, "script", a.ScriptID, "err", err)
		return
	}
	spec, err := a.TargetSpec()
	if err != nil {
		s.log.Error("automations: bad target", "automation", a.ID, "err", err)
		return
	}
	visible, scopeOrg := automationScope(a)
	devices, err := s.resolveTargetDevices(ctx, spec, visible, scopeOrg)
	if err != nil {
		s.log.Error("automations: resolve target", "automation", a.ID, "err", err)
		return
	}
	job, err := s.st.CreateJob(ctx, store.CreateJobParams{
		OrgID: a.OrgID, Name: a.Name, Origin: store.JobOriginSchedule,
		ScriptID: &a.ScriptID, CreatedBy: a.CreatedBy, AutomationID: &a.ID,
	})
	if err != nil {
		s.log.Error("automations: create job", "automation", a.ID, "err", err)
		return
	}
	jcs, err := s.st.CreateCommandsForDevices(ctx, job.ID, sc, createdBy, devices)
	if err != nil {
		s.log.Error("automations: create commands", "automation", a.ID, "job", job.ID, "err", err)
		return
	}
	s.wakeJobDevices(jcs)

	next := s.nextScheduleRun(a, now)
	if err := s.st.SetAutomationRan(ctx, a.ID, now, next); err != nil {
		s.log.Error("automations: set ran", "automation", a.ID, "err", err)
	}
	created := countCreated(jcs)
	s.audit(ctx, "system", nil, a.OrgID, "automation.ran", "automation", &a.ID, "",
		map[string]any{"job_id": job.ID, "total": created, "matched": len(jcs)})
	s.log.Info("automation ran", "automation", a.ID, "job", job.ID, "total", created)
}

// nextScheduleRun computes the next due time for a schedule automation from now
// (missed ticks never stack). It supports the simple every_minutes schedule; an
// automation whose schedule has no positive interval is parked (next nil) rather
// than busy-looping.
func (s *Server) nextScheduleRun(a store.Automation, now time.Time) *time.Time {
	spec, err := a.ScheduleSpec()
	if err != nil || spec.EveryMinutes <= 0 {
		return nil
	}
	next := now.Add(time.Duration(spec.EveryMinutes) * time.Minute).UTC()
	return &next
}

// TriggerAutomationsForAlert runs the alert-trigger automations that match a
// newly opened alert: for each, if the alerting device can run the script and the
// per-automation-per-device cooldown has elapsed, it fans the script out onto that
// one device under a job (origin trigger). One automation's failure is logged and
// never stops the others. The integrator calls this from the alert-open hook.
func (s *Server) TriggerAutomationsForAlert(ctx context.Context, alert store.Alert) {
	autos, err := s.st.MatchAlertAutomations(ctx, alert)
	if err != nil {
		s.log.Error("automations: match alert", "alert", alert.ID, "err", err)
		return
	}
	if len(autos) == 0 {
		return
	}
	dev, err := s.st.GetDevice(ctx, alert.DeviceID)
	if err != nil {
		s.log.Error("automations: load alerting device", "device", alert.DeviceID, "err", err)
		return
	}
	now := s.now()
	for _, a := range autos {
		s.runTriggeredAutomation(ctx, a, alert, dev, now)
	}
}

func (s *Server) runTriggeredAutomation(ctx context.Context, a store.Automation, alert store.Alert, dev store.Device, now time.Time) {
	createdBy, ok := automationCreatedBy(a)
	if !ok {
		s.log.Error("automations: trigger run has no owner", "automation", a.ID)
		return
	}
	// A revoked device cannot run a command; skip it (like the command endpoints).
	if dev.Revoked {
		return
	}
	sc, err := s.st.GetScript(ctx, a.ScriptID)
	if err != nil {
		s.log.Error("automations: load script", "automation", a.ID, "script", a.ScriptID, "err", err)
		return
	}
	// Desktop-only kinds (exec/script) skip android; a platform-specific script
	// skips a device of another platform. Pre-check so no empty job is created.
	if !protocol.KindSupportedOn(sc.Kind, dev.Platform) ||
		(sc.Platform != store.ScriptPlatformAny && sc.Platform != dev.Platform) {
		return
	}
	// Per-automation-per-device cooldown: a flapping alert does not hammer a device.
	last, err := s.st.LastAutomationDeviceRunAt(ctx, a.ID, dev.ID)
	if err != nil {
		s.log.Error("automations: cooldown lookup", "automation", a.ID, "device", dev.ID, "err", err)
		return
	}
	if last != nil && now.Sub(*last) < automationTriggerCooldown {
		return
	}
	job, err := s.st.CreateJob(ctx, store.CreateJobParams{
		OrgID: a.OrgID, Name: a.Name, Origin: store.JobOriginTrigger,
		ScriptID: &a.ScriptID, CreatedBy: a.CreatedBy, AutomationID: &a.ID,
	})
	if err != nil {
		s.log.Error("automations: create job", "automation", a.ID, "err", err)
		return
	}
	jcs, err := s.st.CreateCommandsForDevices(ctx, job.ID, sc, createdBy, []store.Device{dev})
	if err != nil {
		s.log.Error("automations: create commands", "automation", a.ID, "job", job.ID, "err", err)
		return
	}
	s.wakeJobDevices(jcs)
	alertID := alert.ID
	detail := map[string]any{"job_id": job.ID, "alert_id": alertID, "device_id": dev.ID,
		"alert_type": alert.Type, "total": countCreated(jcs)}
	s.audit(ctx, "system", nil, a.OrgID, "automation.triggered", "automation", &a.ID, "", detail)
	s.log.Info("automation triggered", "automation", a.ID, "job", job.ID, "device", dev.ID, "alert", alertID)
}

// --- shared target resolution ----------------------------------------------

// automationScope returns the visible-org set and scope org for an automation's
// run: a client-scoped automation sees only its own org; a global automation
// (org_id NULL) sees every org (visible nil) with no scope narrowing.
func automationScope(a store.Automation) (visible []int64, scopeOrg *int64) {
	if a.OrgID == nil {
		return nil, nil
	}
	return []int64{*a.OrgID}, a.OrgID
}

// automationCreatedBy returns the owner id stamped on the fan-out's commands
// (commands.created_by is NOT NULL). An automation with no owner cannot run.
func automationCreatedBy(a store.Automation) (int64, bool) {
	if a.CreatedBy == nil {
		return 0, false
	}
	return *a.CreatedBy, true
}

// resolveTargetDevices resolves a target spec to the devices it reaches, always
// restricted to the allowed org set (RS2): visible is the caller's/automation's
// visible org set (nil = all orgs), scopeOrg further pins to one client when set
// (a client-scoped script/automation). Device ids outside the allowed scope are
// silently dropped (the HTTP layer rejects them up front with a 404); the filter
// form narrows by org, status, hostname substring and platform.
func (s *Server) resolveTargetDevices(ctx context.Context, spec store.TargetSpec, visible []int64, scopeOrg *int64) ([]store.Device, error) {
	if len(spec.DeviceIDs) > 0 {
		out := make([]store.Device, 0, len(spec.DeviceIDs))
		seen := map[int64]bool{}
		for _, id := range spec.DeviceIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			dev, err := s.st.GetDevice(ctx, id)
			if err != nil {
				continue // missing device: skip (HTTP path already validated existence)
			}
			if inScope(dev.OrgID, visible, scopeOrg) {
				out = append(out, dev)
			}
		}
		return out, nil
	}

	orgFilter := scopeOrg
	if spec.OrgID != nil {
		if scopeOrg != nil && *spec.OrgID != *scopeOrg {
			return []store.Device{}, nil // target org outside the script/automation scope
		}
		orgFilter = spec.OrgID
	}
	devs, err := s.st.ListDevices(ctx, store.DeviceFilter{
		OrgID: orgFilter, VisibleOrgs: visible, Status: spec.Status, Query: spec.HostnameQ,
	})
	if err != nil {
		return nil, err
	}
	if spec.Platform == "" {
		return devs, nil
	}
	out := devs[:0]
	for _, d := range devs {
		if d.Platform == spec.Platform {
			out = append(out, d)
		}
	}
	return out, nil
}

// inScope reports whether an org is inside the allowed set: in visible (nil = all)
// and, when scopeOrg is set, equal to it.
func inScope(org int64, visible []int64, scopeOrg *int64) bool {
	if scopeOrg != nil && org != *scopeOrg {
		return false
	}
	if visible == nil {
		return true
	}
	for _, v := range visible {
		if v == org {
			return true
		}
	}
	return false
}

// validateTargetForCaller enforces RS2 on a target supplied over the API, before
// any side effect: the status/platform enums are valid, a named target org is
// visible to the caller (else 404) and within the script/automation scope (else
// 400), and every named device is visible (else 404) and within scope (else 400).
// It returns false after writing the response on the first problem.
func (s *Server) validateTargetForCaller(w http.ResponseWriter, r *http.Request, spec store.TargetSpec, scopeOrg *int64) bool {
	if !validTargetStatus(spec.Status) {
		writeError(w, http.StatusBadRequest, "invalid_request", "target.status must be online, offline or pending")
		return false
	}
	if !validTargetPlatform(spec.Platform) {
		writeError(w, http.StatusBadRequest, "invalid_request", "target.platform must be linux, windows or android")
		return false
	}
	u := userFrom(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return false
	}
	if spec.OrgID != nil {
		if !s.dashOrgVisible(w, r, *spec.OrgID) {
			return false
		}
		if scopeOrg != nil && *spec.OrgID != *scopeOrg {
			writeError(w, http.StatusBadRequest, "invalid_request", "target org must match the script's client")
			return false
		}
	}
	for _, id := range spec.DeviceIDs {
		dev, err := s.st.GetDevice(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "")
			return false
		}
		can, err := s.st.UserCanSeeOrg(r.Context(), u.ID, dev.OrgID)
		if err != nil {
			s.log.Error("target device visibility", "device", id, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "")
			return false
		}
		if !can {
			writeError(w, http.StatusNotFound, "not_found", "")
			return false
		}
		if scopeOrg != nil && dev.OrgID != *scopeOrg {
			writeError(w, http.StatusBadRequest, "invalid_request", "target device is not in the script's client")
			return false
		}
	}
	return true
}

// countCreated counts the fan-out slots that produced a command (skips excluded).
func countCreated(jcs []store.JobCommand) int {
	n := 0
	for _, jc := range jcs {
		if !jc.Skipped {
			n++
		}
	}
	return n
}
