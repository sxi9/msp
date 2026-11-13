package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// dashBodyHead returns the first 200 characters of a script body for the
// audit row.
func dashBodyHead(body string) string {
	const n = 200
	if len(body) <= n {
		return body
	}
	return body[:n]
}

// handleCreateCommand is POST /api/v1/devices/{id}/commands (operator).
// Validation order: device exists (404) -> not revoked (unsupported_on_device)
// -> shape (invalid_request) -> kind allowed on the platform
// (unsupported_on_platform) -> script shell available on the platform
// (invalid_request). Then the command is queued, audited and the device's
// parked sync is woken.
func (s *Server) handleCreateCommand(w http.ResponseWriter, r *http.Request) {
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
	// 404 across orgs before any side effect, role result or 409 (RS2).
	if !s.dashOrgVisible(w, r, dev.OrgID) {
		return
	}
	var cmd protocol.Command
	if err := readJSON(r, &cmd); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if dev.Revoked {
		writeError(w, http.StatusBadRequest, "unsupported_on_device", "device token is revoked")
		return
	}
	if err := cmd.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !protocol.KindSupportedOn(cmd.Kind, dev.Platform) {
		writeError(w, http.StatusBadRequest, "unsupported_on_platform",
			fmt.Sprintf("kind %s is not available on %s", cmd.Kind, dev.Platform))
		return
	}
	if cmd.Kind == protocol.KindScript {
		allowed := false
		for _, sh := range protocol.ShellsFor(dev.Platform) {
			if sh == cmd.Shell {
				allowed = true
				break
			}
		}
		if !allowed {
			writeError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("shell %s not available on %s", cmd.Shell, dev.Platform))
			return
		}
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
		Kind:      cmd.Kind,
		Argv:      cmd.Argv,
		Shell:     cmd.Shell,
		Body:      cmd.Body,
		Text:      cmd.Text,
		TimeoutS:  cmd.TimeoutS,
	})
	if err != nil {
		s.log.Error("create command", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}

	detail := map[string]any{
		"kind":      created.Kind,
		"timeout_s": created.TimeoutS,
		"hostname":  dev.Hostname,
	}
	switch created.Kind {
	case protocol.KindExec:
		detail["argv"] = created.Argv
	case protocol.KindScript:
		sum := sha256.Sum256([]byte(created.Body))
		detail["shell"] = created.Shell
		detail["body_sha256"] = hex.EncodeToString(sum[:])
		detail["body_head"] = dashBodyHead(created.Body)
	case protocol.KindMessage:
		detail["text"] = created.Text
	}
	ip := clientIP(r)
	actor := u.ID
	s.audit(ctx, "user", &actor, &dev.OrgID, "command.queued", "command", &created.ID, ip, detail)
	if created.Kind == protocol.KindLock {
		s.audit(ctx, "user", &actor, &dev.OrgID, "device.lock_requested", "device", &dev.ID, ip, map[string]any{
			"command_id": created.ID,
			"hostname":   dev.Hostname,
			"platform":   dev.Platform,
		})
	}
	s.notifier.Notify(dev.ID)
	s.log.Info("command queued", "command", created.ID, "device", dev.ID, "kind", created.Kind, "user", u.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"command": created})
}

// commandWithShell is a command JSON enriched with the shell_session block a
// kind shell command carries (RS1 section A). The embedded store.Command's
// fields are promoted, so the object is a plain command with one added
// shell_session field; it is null until the shell_sessions row exists.
type commandWithShell struct {
	store.Command
	ShellSession *protocol.ShellSession `json:"shell_session"`
}

// enrichCommand returns the JSON value for one command: a kind shell command
// carries its shell_session block (null until the relay creates the session
// row), every other kind is returned unchanged. The live byte counters are
// flushed to the row on close, so a finished shell shows accurate totals; while
// a shell is live the browser reads the relay directly rather than this JSON.
func (s *Server) enrichCommand(ctx context.Context, cmd store.Command) any {
	if cmd.Argv == nil {
		cmd.Argv = []string{}
	}
	if cmd.Kind != protocol.KindShell {
		return cmd
	}
	out := commandWithShell{Command: cmd}
	switch sess, err := s.st.GetShellSession(ctx, cmd.ID); {
	case err == nil:
		out.ShellSession = &protocol.ShellSession{
			BytesIn:             sess.BytesIn,
			BytesOut:            sess.BytesOut,
			CloseReason:         sess.CloseReason,
			TranscriptTruncated: sess.TranscriptTruncated,
			ConnectedAt:         sess.ConnectedAt,
		}
	case errors.Is(err, store.ErrNotFound):
		// No session row yet: shell_session stays null (RS1 section A).
	default:
		s.log.Error("shell: get session for command json", "command", cmd.ID, "err", err)
	}
	return out
}

// handleListCommands is GET /api/v1/devices/{id}/commands?limit=50, newest first.
func (s *Server) handleListCommands(w http.ResponseWriter, r *http.Request) {
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
	if !s.dashOrgVisible(w, r, dev.OrgID) {
		return
	}
	cmds, err := s.st.ListCommands(ctx, id, dashLimit(r, 50, 200))
	if err != nil {
		s.log.Error("list commands", "device", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	out := make([]any, len(cmds))
	for i, c := range cmds {
		out[i] = s.enrichCommand(ctx, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": out})
}

// handleGetCommand is GET /api/v1/commands/{id}.
func (s *Server) handleGetCommand(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	cmd, err := s.st.GetCommand(r.Context(), id)
	if err != nil {
		s.dashStoreError(w, err, "get command")
		return
	}
	if !s.dashOrgVisible(w, r, cmd.OrgID) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": s.enrichCommand(r.Context(), cmd)})
}
