package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Remote shell dashboard side (RS1 sections A, B, D). This file registers the
// shell routes (appended to dashboardRoutes so server.go's Handler picks them up
// without a change to server.go) and holds the three dashboard-facing handlers:
// the POST that opens a shell and issues the browser ticket, the browser
// WebSocket leg, and the transcript download. The agent WebSocket leg is in
// api_agent_shell.go; the relay itself is in shell.go / shell_relay.go.

func init() {
	dashboardRoutes = append(dashboardRoutes, registerShell)
}

// registerShell adds the remote-shell routes. The dashboard routes run inside
// the standard sessionAuth -> csrf -> requireRole chain; the agent leg uses the
// device bearer-token auth. RegisterShell is the exported entry point for an
// integrator that wires the mux by hand instead of through dashboardRoutes.
func registerShell(s *Server, mux *http.ServeMux) {
	// Dashboard: open a shell (operator, CSRF), the browser WebSocket leg
	// (operator; the Origin+ticket check is in the handler) and the transcript.
	mux.Handle("POST /api/v1/devices/{id}/shell", s.dashRoute(RoleOperator, s.handleOpenShell))
	mux.Handle("GET /api/v1/shell/{id}/ws", s.dashRoute(RoleOperator, s.handleBrowserShell))
	mux.Handle("GET /api/v1/shell/{id}/transcript", s.dashRoute(RoleOperator, s.handleShellTranscript))

	// Agent leg: device bearer auth, no cookies, no CSRF.
	mux.HandleFunc("GET /api/agent/v1/shell/{command_id}", s.deviceAuth(s.handleAgentShell))
}

// RegisterShell exposes the shell route registration for an integrator that
// assembles the mux itself (WP-INTEGRATE). It is a no-op addition on top of the
// automatic dashboardRoutes registration; calling both would double-register and
// panic, so an integrator uses one or the other.
func RegisterShell(s *Server, mux *http.ServeMux) { registerShell(s, mux) }

// shellOpenRequest is the POST /devices/{id}/shell body: every field optional.
type shellOpenRequest struct {
	Shell string `json:"shell"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
}

// handleOpenShell is POST /api/v1/devices/{id}/shell (admin|operator, CSRF).
// Validation order (RS1 section A): device exists (404) -> not revoked
// (unsupported_on_device) -> desktop platform (unsupported_on_platform) -> body
// shape (invalid_request) -> online (device_offline) -> no other live shell
// (shell_busy) -> under the 20-shell cap (too_many_shells). Then the shell
// command is queued, a single-use browser ticket reserved, the open audited and
// the device's parked sync woken.
func (s *Server) handleOpenShell(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	dev, err := s.st.GetDevice(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get device")
		return
	}
	// 404 across orgs before any side effect or 409 (RS2).
	if !s.dashOrgVisible(w, r, dev.OrgID) {
		return
	}
	if dev.Revoked {
		writeError(w, http.StatusBadRequest, "unsupported_on_device", "device token is revoked")
		return
	}
	if dev.Platform == protocol.PlatformAndroid {
		writeError(w, http.StatusBadRequest, "unsupported_on_platform", "remote shell is desktop only")
		return
	}

	var req shellOpenRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Shell != "" && !protocol.ValidShell(req.Shell) {
		writeError(w, http.StatusBadRequest, "invalid_request", "shell must be empty (default) or one of sh, bash, powershell, cmd")
		return
	}
	cols := protocol.ClampShellCols(req.Cols)
	rows := protocol.ClampShellRows(req.Rows)

	if dev.Status != "online" {
		writeError(w, http.StatusConflict, "device_offline", "")
		return
	}
	busy, err := s.deviceHasActiveShell(ctx, dev.ID)
	if err != nil {
		s.log.Error("shell: active shell check", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if busy {
		writeError(w, http.StatusConflict, "shell_busy", "")
		return
	}
	now := s.now()
	if s.shells().full(now) {
		writeError(w, http.StatusTooManyRequests, "too_many_shells", "")
		return
	}

	u := userFrom(ctx)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	created, err := s.st.CreateCommand(ctx, store.CreateCommandParams{
		DeviceID:  dev.ID,
		OrgID:     dev.OrgID,
		CreatedBy: u.ID,
		Kind:      protocol.KindShell,
		Shell:     req.Shell,
		TimeoutS:  protocol.DefaultShellTimeoutS,
	})
	if err != nil {
		s.log.Error("shell: create command", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}

	ip := clientIP(r)
	ticket := s.shells().reserve(created.ID, u.ID, ip, cols, rows, now)
	s.audit(ctx, "user", &u.ID, &dev.OrgID, "shell.opened", "device", &dev.ID, ip, map[string]any{
		"hostname":   dev.Hostname,
		"platform":   dev.Platform,
		"shell":      req.Shell,
		"cols":       cols,
		"rows":       rows,
		"command_id": created.ID,
	})
	s.notifier.Notify(dev.ID)
	s.log.Info("shell opened", "command", created.ID, "device", dev.ID, "user", u.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"command": created,
		"ticket":  ticket,
		"ws_path": "/api/v1/shell/" + strconv.FormatInt(created.ID, 10) + "/ws",
	})
}

// handleBrowserShell is GET /api/v1/shell/{id}/ws?ticket=... (admin|operator).
// Origin host must equal Host (missing Origin -> 403); the ticket must match the
// one issued for the command, be unused and younger than 60 s (single use,
// consumed on handshake). On success the browser leg of the relay runs until the
// socket closes.
func (s *Server) handleBrowserShell(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden", "origin must match host")
		return
	}
	ticketVal := r.URL.Query().Get("ticket")
	if ticketVal == "" {
		writeError(w, http.StatusForbidden, "forbidden", "missing ticket")
		return
	}
	ctx := r.Context()
	cmd, err := s.st.GetCommand(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get command")
		return
	}
	if cmd.Kind != protocol.KindShell {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	if !s.dashOrgVisible(w, r, cmd.OrgID) {
		return
	}
	// Reject a reconnect to a shell that has already closed. Without a live
	// session in memory, getOrCreate would build a brand-new session for the
	// finished command; its teardown would then overwrite the flushed
	// shell_sessions row (empty transcript, zeroed bytes, a new close reason) and
	// write a duplicate shell.closed audit, destroying the retained transcript.
	// A non-null closed_at on the row is the authoritative "already ended" signal;
	// a legitimate first connect has no row yet (it is created on attach).
	if s.shells().session(id) == nil {
		if sess, serr := s.st.GetShellSession(ctx, id); serr == nil && sess.ClosedAt != nil {
			writeError(w, http.StatusConflict, "shell_closed", "the shell session has already ended")
			return
		}
	}
	t, ok := s.shells().consumeTicket(id, ticketVal, s.now())
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden", "invalid or expired ticket")
		return
	}
	hostname := ""
	if dev, err := s.st.GetDevice(ctx, cmd.DeviceID); err == nil {
		hostname = dev.Hostname
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		// Accept already wrote the error response.
		return
	}
	ss := s.shells().getOrCreate(s, cmd, hostname, t.issuedAt)
	ss.attachBrowser(conn, t)
	ss.runBrowser(conn)
}

// handleShellTranscript is GET /api/v1/shell/{id}/transcript (admin|operator):
// 200 text/plain of the recorded agent->browser bytes, served from the live
// session's buffer while it runs and from the flushed shell_sessions row after
// it closes. 404 when the command is not a shell command.
func (s *Server) handleShellTranscript(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	cmd, err := s.st.GetCommand(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get command")
		return
	}
	if cmd.Kind != protocol.KindShell {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	if !s.dashOrgVisible(w, r, cmd.OrgID) {
		return
	}

	var body []byte
	if ss := s.shells().session(id); ss != nil {
		ss.mu.Lock()
		body = append([]byte(nil), ss.transcript...)
		ss.mu.Unlock()
	} else {
		sess, err := s.st.GetShellSession(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			body = nil // session row not created yet: empty transcript
		case err != nil:
			s.log.Error("shell: get transcript", "command", id, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "")
			return
		default:
			body = sess.Transcript
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// deviceHasActiveShell reports whether the device already has a shell command in
// status queued or sent (RS1 section A shell_busy). It scans the device's recent
// commands; an active shell is always among the newest rows.
func (s *Server) deviceHasActiveShell(ctx context.Context, deviceID int64) (bool, error) {
	cmds, err := s.st.ListCommands(ctx, deviceID, 200)
	if err != nil {
		return false, err
	}
	for _, c := range cmds {
		if c.Kind == protocol.KindShell && (c.Status == store.StatusQueued || c.Status == store.StatusSent) {
			return true, nil
		}
	}
	return false, nil
}

// sameOrigin reports whether the request's Origin header host equals its Host
// (case-insensitive). A missing or unparseable Origin is not same-origin.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// decodeOptionalJSON decodes an optional JSON request body: an empty body is
// treated as "all defaults", any other decode error is returned.
func decodeOptionalJSON(r *http.Request, v any) error {
	err := readJSON(r, v)
	if err != nil && err.Error() == "empty body" {
		return nil
	}
	return err
}
