package server

import (
	"net/http"
	"strconv"
	"strings"
)

// The dashboard JSON API lives in the dashboard_*.go files. registerDashboard
// is appended to dashboardRoutes at init so server.go's Handler picks it up
// without the two halves of the package referring to each other's files.
func init() {
	dashboardRoutes = append(dashboardRoutes, registerDashboard)
}

// registerDashboard registers every /api/v1 dashboard route plus the static
// routes (/dl/ and the SPA catch-all). Every API route runs inside
// sessionAuth -> csrf -> requireRole; GETs require the viewer role.
func registerDashboard(s *Server, mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle("GET "+pattern, s.dashRoute(RoleViewer, h))
	}
	with := func(role, pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.dashRoute(role, h))
	}

	get("/api/v1/summary", s.handleSummary)

	// Orgs and enrollment tokens.
	get("/api/v1/orgs", s.handleListOrgs)
	with(RoleAdmin, "POST /api/v1/orgs", s.handleCreateOrg)
	get("/api/v1/orgs/{id}", s.handleGetOrg)
	with(RoleOperator, "POST /api/v1/orgs/{id}/tokens", s.handleCreateToken)
	with(RoleOperator, "POST /api/v1/orgs/{id}/tokens/{tid}/revoke", s.handleRevokeToken)

	// Devices.
	get("/api/v1/devices", s.handleListDevices)
	get("/api/v1/devices/{id}", s.handleGetDevice)
	get("/api/v1/devices/{id}/heartbeats", s.handleListHeartbeats)
	with(RoleAdmin, "POST /api/v1/devices/{id}/revoke", s.handleRevokeDevice)
	with(RoleAdmin, "DELETE /api/v1/devices/{id}", s.handleDeleteDevice)

	// Commands.
	with(RoleOperator, "POST /api/v1/devices/{id}/commands", s.handleCreateCommand)
	get("/api/v1/devices/{id}/commands", s.handleListCommands)
	get("/api/v1/commands/{id}", s.handleGetCommand)
	get("/api/v1/presets", s.handleListPresets)

	// Alerts, audit, users.
	get("/api/v1/alerts", s.handleListAlerts)
	with(RoleOperator, "POST /api/v1/alerts/{id}/ack", s.handleAckAlert)
	get("/api/v1/audit", s.handleListAudit)
	with(RoleAdmin, "GET /api/v1/users", s.handleListUsers)
	with(RoleAdmin, "POST /api/v1/users", s.handleCreateUser)
	with(RoleAdmin, "POST /api/v1/users/{id}/disable", s.handleDisableUser)

	// Static: agent downloads and the SPA (index.html fallback).
	mux.HandleFunc("GET /dl/{file...}", s.handleDownload)
	mux.HandleFunc("/", s.handleSPA)
}

// dashRoute wraps a dashboard handler in the standard chain.
func (s *Server) dashRoute(role string, h http.HandlerFunc) http.Handler {
	return s.sessionAuth(s.csrf(s.requireRole(role, h)))
}

// handleSummary is GET /api/v1/summary.
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.st.Summary(r.Context())
	if err != nil {
		s.log.Error("summary", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// dashBaseURL is the public base URL used in install one-liners: cfg.BaseURL
// when set, else the request's scheme and host. The scheme is https when the
// connection is TLS or, behind a trusted proxy, when X-Forwarded-Proto says so.
func dashBaseURL(s *Server, r *http.Request) string {
	if b := strings.TrimRight(s.cfg.BaseURL, "/"); b != "" {
		return b
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// dashInstallLines renders the per-OS install one-liners for an enrollment
// token exactly as docs/PROTOCOL.md specifies them.
func dashInstallLines(base, plaintext string) map[string]string {
	base = strings.TrimRight(base, "/")
	return map[string]string{
		"linux":   "curl -fsSLo /tmp/msp-install.sh " + base + "/dl/install.sh && sudo bash /tmp/msp-install.sh --server " + base + " --token " + plaintext,
		"windows": "irm " + base + "/dl/install.ps1 -OutFile $env:TEMP\\msp-install.ps1; & $env:TEMP\\msp-install.ps1 -Server " + base + " -Token " + plaintext,
		"android": "Install " + base + "/dl/msp-agent.apk, open MSP Agent, enter " + base + " and the token " + plaintext,
	}
}

// dashLimit reads the ?limit query value: def when absent or not a positive
// integer, capped at max.
func dashLimit(r *http.Request, def, max int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// dashOrgIDParam reads the optional ?org_id query value. ok is false when the
// value is present but not a positive integer.
func dashOrgIDParam(r *http.Request) (id *int64, ok bool) {
	raw := r.URL.Query().Get("org_id")
	if raw == "" {
		return nil, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return nil, false
	}
	return &n, true
}

// dashActor returns the session user's id pointer for audit rows.
func dashActor(r *http.Request) (actorID *int64, ok bool) {
	u := userFrom(r.Context())
	if u == nil {
		return nil, false
	}
	id := u.ID
	return &id, true
}
