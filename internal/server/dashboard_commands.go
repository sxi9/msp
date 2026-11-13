package server

import (
	"crypto/sha256"
	"encoding/hex"
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

// handleListCommands is GET /api/v1/devices/{id}/commands?limit=50, newest first.
func (s *Server) handleListCommands(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	if _, err := s.st.GetDevice(ctx, id); err != nil {
		s.dashStoreError(w, err, "get device")
		return
	}
	cmds, err := s.st.ListCommands(ctx, id, dashLimit(r, 50, 200))
	if err != nil {
		s.log.Error("list commands", "device", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if cmds == nil {
		cmds = []store.Command{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": cmds})
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
	if cmd.Argv == nil {
		cmd.Argv = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": cmd})
}
