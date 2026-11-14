package server

import (
	"net/http"
	"strings"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Script library dashboard API (AUT1): CRUD over /api/v1/scripts for the saved,
// editable command templates (distinct from the static presets.json console
// presets). A script is an exec (argv) or a script (shell + body) that a bulk run
// (dashboard_jobs.go) or an automation (dashboard_automations.go) fans out across
// a device set. Everything is RS2 org-scoped: org_id NULL = a global script
// visible to an all-orgs caller only; a client-scoped script carries that org's
// id. Writes require operator; GETs require viewer. Registered through
// dashboardRoutes like the other dashboard_*.go files.

const (
	scriptMaxName = 100
	scriptMaxDesc = 1000
	scriptMaxBody = 1 << 16 // 64 KiB script body cap
)

func init() {
	dashboardRoutes = append(dashboardRoutes, registerScripts)
}

func registerScripts(s *Server, mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle("GET "+pattern, s.dashRoute(RoleViewer, h))
	}
	with := func(role, pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.dashRoute(role, h))
	}
	get("/api/v1/scripts", s.handleListScripts)
	with(RoleOperator, "POST /api/v1/scripts", s.handleCreateScript)
	get("/api/v1/scripts/{id}", s.handleGetScript)
	with(RoleOperator, "PUT /api/v1/scripts/{id}", s.handleUpdateScript)
	with(RoleOperator, "DELETE /api/v1/scripts/{id}", s.handleDeleteScript)
}

type scriptRequest struct {
	OrgID       *int64   `json:"org_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Platform    string   `json:"platform"`
	Kind        string   `json:"kind"`
	Argv        []string `json:"argv"`
	Shell       string   `json:"shell"`
	Body        string   `json:"body"`
	TimeoutS    int      `json:"timeout_s"`
}

func (s *Server) handleListScripts(w http.ResponseWriter, r *http.Request) {
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return
	}
	rows, err := s.st.ListScripts(r.Context(), visible)
	if err != nil {
		s.log.Error("list scripts", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scripts": rows})
}

func (s *Server) handleGetScript(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	sc, err := s.st.GetScript(r.Context(), id)
	if err != nil {
		s.dashStoreError(w, err, "get script")
		return
	}
	// 404 (not 403) across orgs so a scoped user cannot tell it exists (RS2).
	if !s.notifyRowVisible(w, r, sc.OrgID) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"script": sc})
}

func (s *Server) handleCreateScript(w http.ResponseWriter, r *http.Request) {
	var req scriptRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if detail := validateScriptShape(name, req.Description, req.Platform, req.Kind, req.Argv, req.Shell, req.Body, req.TimeoutS); detail != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	// A global (org_id NULL) script requires all-orgs; a concrete org must be
	// visible, else 404 (RS2) before any write.
	if !s.notifyCreateAllowed(w, r, req.OrgID) {
		return
	}
	actor, _ := dashActor(r)
	ctx := r.Context()
	sc, err := s.st.CreateScript(ctx, store.CreateScriptParams{
		OrgID: req.OrgID, Name: name, Description: strings.TrimSpace(req.Description),
		Platform: req.Platform, Kind: req.Kind, Argv: req.Argv, Shell: req.Shell, Body: req.Body,
		TimeoutS: req.TimeoutS, CreatedBy: actor,
	})
	if err != nil {
		s.dashStoreError(w, err, "create script")
		return
	}
	s.audit(ctx, "user", actor, sc.OrgID, "script.created", "script", &sc.ID, clientIP(r),
		map[string]any{"name": sc.Name, "platform": sc.Platform, "kind": sc.Kind})
	writeJSON(w, http.StatusCreated, map[string]any{"script": sc})
}

func (s *Server) handleUpdateScript(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	// Load first so an update across orgs 404s before any change (RS2).
	existing, err := s.st.GetScript(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get script")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	var req scriptRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if detail := validateScriptShape(name, req.Description, req.Platform, req.Kind, req.Argv, req.Shell, req.Body, req.TimeoutS); detail != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	// org_id is immutable (like a notify channel's): the store UpdateScript takes a
	// new org_id, but moving a script across clients would silently re-scope every
	// job/automation that uses it. Keep it on the stored org.
	sc, err := s.st.UpdateScript(ctx, id, store.UpdateScriptParams{
		OrgID: existing.OrgID, Name: name, Description: strings.TrimSpace(req.Description),
		Platform: req.Platform, Kind: req.Kind, Argv: req.Argv, Shell: req.Shell, Body: req.Body,
		TimeoutS: req.TimeoutS,
	})
	if err != nil {
		s.dashStoreError(w, err, "update script")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, sc.OrgID, "script.updated", "script", &sc.ID, clientIP(r),
		map[string]any{"name": sc.Name, "platform": sc.Platform, "kind": sc.Kind})
	writeJSON(w, http.StatusOK, map[string]any{"script": sc})
}

func (s *Server) handleDeleteScript(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := s.st.GetScript(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get script")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	if err := s.st.DeleteScript(ctx, id); err != nil {
		s.dashStoreError(w, err, "delete script")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, existing.OrgID, "script.deleted", "script", &id, clientIP(r),
		map[string]any{"name": existing.Name})
	w.WriteHeader(http.StatusNoContent)
}

// validateScriptShape validates the common script shape (used by scripts CRUD and
// the inline bulk-run body) and returns a non-empty detail describing the first
// problem, or "" when the shape is valid. It does not touch org scope (the
// caller's RS2 helpers do that).
func validateScriptShape(name, description, platform, kind string, argv []string, shell, body string, timeoutS int) string {
	if name == "" {
		return "name is required"
	}
	if len(name) > scriptMaxName {
		return "name exceeds 100 characters"
	}
	if len(description) > scriptMaxDesc {
		return "description exceeds 1000 characters"
	}
	switch platform {
	case store.ScriptPlatformLinux, store.ScriptPlatformWindows, store.ScriptPlatformAny:
	default:
		return "platform must be linux, windows or any"
	}
	if timeoutS < 0 || timeoutS > protocol.MaxTimeoutS {
		return "timeout_s out of range"
	}
	switch kind {
	case protocol.KindExec:
		if len(argv) == 0 {
			return "exec script requires a non-empty argv"
		}
		for _, a := range argv {
			if strings.TrimSpace(a) == "" {
				return "argv entries cannot be empty"
			}
		}
	case protocol.KindScript:
		if strings.TrimSpace(body) == "" {
			return "script requires a body"
		}
		if len(body) > scriptMaxBody {
			return "body exceeds 64 KiB"
		}
		if shell != "" {
			if !protocol.ValidShell(shell) {
				return "shell must be one of sh, bash, powershell, cmd"
			}
			// On a concrete platform the shell must be one that platform runs; on
			// 'any' a well-formed shell is accepted (each platform uses its own).
			if platform != store.ScriptPlatformAny && !shellOnPlatform(shell, platform) {
				return "shell " + shell + " is not available on " + platform
			}
		}
	default:
		return "kind must be exec or script"
	}
	return ""
}

// shellOnPlatform reports whether shell is one of the shells platform runs.
func shellOnPlatform(shell, platform string) bool {
	for _, sh := range protocol.ShellsFor(platform) {
		if sh == shell {
			return true
		}
	}
	return false
}
