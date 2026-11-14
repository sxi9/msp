package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/store"
)

// Automations dashboard API (AUT1): CRUD over /api/v1/automations for the saved
// rules that produce jobs — either on a schedule (trigger 'schedule', run by the
// scheduler in automations_run.go) or when a matching alert opens (trigger
// 'alert', run from the alert-open hook). Everything is RS2 org-scoped: a global
// (org_id NULL) automation requires all-orgs; its script and target must stay
// inside the automation's client. Writes require operator; GETs require viewer.
// Registered through dashboardRoutes like the other dashboard_*.go files.

const (
	automationMaxName         = 100
	automationMaxEveryMinutes = 525600 // one year, the furthest a schedule interval may be set
)

func init() {
	dashboardRoutes = append(dashboardRoutes, registerAutomations)
}

func registerAutomations(s *Server, mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle("GET "+pattern, s.dashRoute(RoleViewer, h))
	}
	with := func(role, pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.dashRoute(role, h))
	}
	get("/api/v1/automations", s.handleListAutomations)
	with(RoleOperator, "POST /api/v1/automations", s.handleCreateAutomation)
	get("/api/v1/automations/{id}", s.handleGetAutomation)
	with(RoleOperator, "PUT /api/v1/automations/{id}", s.handleUpdateAutomation)
	with(RoleOperator, "POST /api/v1/automations/{id}/enable", s.handleEnableAutomation)
	with(RoleOperator, "DELETE /api/v1/automations/{id}", s.handleDeleteAutomation)
}

type scheduleReq struct {
	EveryMinutes int    `json:"every_minutes"`
	Cron         string `json:"cron"`
}

type alertReq struct {
	AlertType   string `json:"alert_type"`
	MinSeverity string `json:"min_severity"`
}

type automationRequest struct {
	OrgID    *int64           `json:"org_id"`
	Name     string           `json:"name"`
	Enabled  *bool            `json:"enabled"`
	Trigger  string           `json:"trigger"`
	ScriptID int64            `json:"script_id"`
	Target   store.TargetSpec `json:"target"`
	Schedule *scheduleReq     `json:"schedule"`
	Alert    *alertReq        `json:"alert"`
}

func (s *Server) handleListAutomations(w http.ResponseWriter, r *http.Request) {
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return
	}
	rows, err := s.st.ListAutomations(r.Context(), visible)
	if err != nil {
		s.log.Error("list automations", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"automations": rows})
}

func (s *Server) handleGetAutomation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	a, err := s.st.GetAutomation(r.Context(), id)
	if err != nil {
		s.dashStoreError(w, err, "get automation")
		return
	}
	if !s.notifyRowVisible(w, r, a.OrgID) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"automation": a})
}

func (s *Server) handleCreateAutomation(w http.ResponseWriter, r *http.Request) {
	var req automationRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	targetJSON, scheduleJSON, alertJSON, detail := validateAutomationShape(name, req.Trigger, req.ScriptID, req.Schedule, req.Alert, req.Target)
	if detail != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	// RS2: a global automation requires all-orgs; a concrete org must be visible.
	if !s.notifyCreateAllowed(w, r, req.OrgID) {
		return
	}
	// The script must be visible and belong to the automation's client (or global).
	if _, ok := s.scriptReferenceOK(w, r, req.ScriptID, req.OrgID); !ok {
		return
	}
	// The target may never reach a device outside the automation's client (RS2).
	if !s.validateTargetForCaller(w, r, req.Target, req.OrgID) {
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	var next *time.Time
	if req.Trigger == store.AutomationTriggerSchedule && enabled {
		t := s.now().Add(time.Duration(req.Schedule.EveryMinutes) * time.Minute).UTC()
		next = &t
	}
	actor, _ := dashActor(r)
	ctx := r.Context()
	a, err := s.st.CreateAutomation(ctx, store.CreateAutomationParams{
		OrgID: req.OrgID, Name: name, Enabled: enabled, Trigger: req.Trigger, ScriptID: req.ScriptID,
		TargetJSON: targetJSON, ScheduleJSON: scheduleJSON, AlertJSON: alertJSON, NextRunAt: next, CreatedBy: actor,
	})
	if err != nil {
		s.dashStoreError(w, err, "create automation")
		return
	}
	s.audit(ctx, "user", actor, a.OrgID, "automation.created", "automation", &a.ID, clientIP(r),
		map[string]any{"name": a.Name, "trigger": a.Trigger, "script_id": a.ScriptID})
	writeJSON(w, http.StatusCreated, map[string]any{"automation": a})
}

func (s *Server) handleUpdateAutomation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := s.st.GetAutomation(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get automation")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	var req automationRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	targetJSON, scheduleJSON, alertJSON, detail := validateAutomationShape(name, req.Trigger, req.ScriptID, req.Schedule, req.Alert, req.Target)
	if detail != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	// org_id is immutable; the script and target are validated against the stored org.
	if _, ok := s.scriptReferenceOK(w, r, req.ScriptID, existing.OrgID); !ok {
		return
	}
	if !s.validateTargetForCaller(w, r, req.Target, existing.OrgID) {
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	var next *time.Time
	if req.Trigger == store.AutomationTriggerSchedule && enabled {
		t := s.now().Add(time.Duration(req.Schedule.EveryMinutes) * time.Minute).UTC()
		next = &t
	}
	a, err := s.st.UpdateAutomation(ctx, id, store.UpdateAutomationParams{
		OrgID: existing.OrgID, Name: name, Enabled: enabled, Trigger: req.Trigger, ScriptID: req.ScriptID,
		TargetJSON: targetJSON, ScheduleJSON: scheduleJSON, AlertJSON: alertJSON, NextRunAt: next,
	})
	if err != nil {
		s.dashStoreError(w, err, "update automation")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, a.OrgID, "automation.updated", "automation", &a.ID, clientIP(r),
		map[string]any{"name": a.Name, "trigger": a.Trigger, "script_id": a.ScriptID})
	writeJSON(w, http.StatusOK, map[string]any{"automation": a})
}

type enableRequest struct {
	Enabled *bool `json:"enabled"`
}

func (s *Server) handleEnableAutomation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := s.st.GetAutomation(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get automation")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	var req enableRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "enabled is required")
		return
	}
	// Re-seed next_run_at when enabling a schedule-trigger automation (a freshly
	// enabled one, or one created disabled, otherwise has a stale or nil due time),
	// and clear it when disabling. UpdateAutomation preserves last_run_at, so the
	// run history is kept; it also carries the enabled flag so no separate write is
	// needed. An alert-trigger automation keeps next_run_at nil.
	var next *time.Time
	if existing.Trigger == store.AutomationTriggerSchedule && *req.Enabled {
		if spec, err := existing.ScheduleSpec(); err == nil && spec.EveryMinutes > 0 {
			t := s.now().Add(time.Duration(spec.EveryMinutes) * time.Minute).UTC()
			next = &t
		}
	}
	a, err := s.st.UpdateAutomation(ctx, id, store.UpdateAutomationParams{
		OrgID: existing.OrgID, Name: existing.Name, Enabled: *req.Enabled, Trigger: existing.Trigger,
		ScriptID: existing.ScriptID, TargetJSON: existing.TargetJSON, ScheduleJSON: existing.ScheduleJSON,
		AlertJSON: existing.AlertJSON, NextRunAt: next,
	})
	if err != nil {
		s.dashStoreError(w, err, "set automation enabled")
		return
	}
	actor, _ := dashActor(r)
	action := "automation.disabled"
	if *req.Enabled {
		action = "automation.enabled"
	}
	s.audit(ctx, "user", actor, existing.OrgID, action, "automation", &id, clientIP(r),
		map[string]any{"name": existing.Name})
	writeJSON(w, http.StatusOK, map[string]any{"automation": a})
}

func (s *Server) handleDeleteAutomation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := s.st.GetAutomation(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get automation")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	if err := s.st.DeleteAutomation(ctx, id); err != nil {
		s.dashStoreError(w, err, "delete automation")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, existing.OrgID, "automation.deleted", "automation", &id, clientIP(r),
		map[string]any{"name": existing.Name})
	w.WriteHeader(http.StatusNoContent)
}

// scriptReferenceOK reports whether the caller may attach an automation in org
// ownerOrg to scriptID: the script must exist, be visible to the caller, and
// belong to the same client as the automation (or be global). A missing, invisible
// or cross-org script is a 400 that does not reveal whether it exists in another
// org — same shape as channelReferenceOK in dashboard_notify.go.
func (s *Server) scriptReferenceOK(w http.ResponseWriter, r *http.Request, scriptID int64, ownerOrg *int64) (store.Script, bool) {
	u := userFrom(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return store.Script{}, false
	}
	sc, err := s.st.GetScript(r.Context(), scriptID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "script_id not found")
		return store.Script{}, false
	}
	vis, err := s.notifyOrgVisibleQuiet(r.Context(), u.ID, sc.OrgID)
	if err != nil {
		s.log.Error("script reference visibility", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return store.Script{}, false
	}
	if !vis || !notifyOrgCompatible(sc.OrgID, ownerOrg) {
		writeError(w, http.StatusBadRequest, "invalid_request", "script_id not found")
		return store.Script{}, false
	}
	return sc, true
}

// validateAutomationShape validates the automation fields and returns the
// canonical target/schedule/alert JSON, or a non-empty detail describing the first
// problem. Org scope and the script/target visibility are checked by the caller.
func validateAutomationShape(name, trigger string, scriptID int64, sched *scheduleReq, al *alertReq, target store.TargetSpec) (targetJSON, scheduleJSON, alertJSON, detail string) {
	if name == "" {
		return "", "", "", "name is required"
	}
	if len(name) > automationMaxName {
		return "", "", "", "name exceeds 100 characters"
	}
	if scriptID <= 0 {
		return "", "", "", "script_id is required"
	}
	if !validTargetStatus(target.Status) {
		return "", "", "", "target.status must be online, offline or pending"
	}
	if !validTargetPlatform(target.Platform) {
		return "", "", "", "target.platform must be linux, windows or android"
	}
	tb, err := json.Marshal(target)
	if err != nil {
		return "", "", "", "target is not valid"
	}
	targetJSON = string(tb)

	switch trigger {
	case store.AutomationTriggerSchedule:
		if al != nil && (al.AlertType != "" || al.MinSeverity != "") {
			return "", "", "", "a schedule automation must not carry an alert matcher"
		}
		if sched == nil || sched.EveryMinutes < 1 {
			return "", "", "", "a schedule automation needs every_minutes >= 1"
		}
		if sched.EveryMinutes > automationMaxEveryMinutes {
			return "", "", "", "every_minutes is too large"
		}
		sb, _ := json.Marshal(store.ScheduleSpec{EveryMinutes: sched.EveryMinutes, Cron: strings.TrimSpace(sched.Cron)})
		scheduleJSON = string(sb)
	case store.AutomationTriggerAlert:
		if sched != nil && (sched.EveryMinutes != 0 || strings.TrimSpace(sched.Cron) != "") {
			return "", "", "", "an alert automation must not carry a schedule"
		}
		spec := store.AlertTriggerSpec{}
		if al != nil {
			spec.AlertType = strings.TrimSpace(al.AlertType)
			spec.MinSeverity = strings.TrimSpace(al.MinSeverity)
		}
		if spec.AlertType != "" && !knownAlertType(spec.AlertType) {
			return "", "", "", "unknown alert type: " + spec.AlertType
		}
		if spec.MinSeverity != "" {
			switch spec.MinSeverity {
			case severityLow, severityMedium, severityHigh:
			default:
				return "", "", "", "min_severity must be low, medium or high"
			}
		}
		ab, _ := json.Marshal(spec)
		alertJSON = string(ab)
	default:
		return "", "", "", "trigger must be schedule or alert"
	}
	return targetJSON, scheduleJSON, alertJSON, ""
}
