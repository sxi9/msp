package server

import (
	"net/http"

	"github.com/coder/websocket"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// handleAgentShell is the agent WebSocket leg: GET /api/agent/v1/shell/{command_id}
// with Authorization: Bearer dt_... (RS1 section B). The server checks that the
// device token is valid (deviceAuth), the command belongs to that device, is
// kind shell and is in status sent, and that no agent socket is already
// attached (else 409). On attach the command stays sent, connected_at is
// recorded, shell.connected is audited and a waiting browser is told the shell
// is connected; the agent leg of the relay then runs until the socket closes.
func (s *Server) handleAgentShell(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id, ok := pathInt(r, "command_id")
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
	if cmd.DeviceID != dev.ID || cmd.Kind != protocol.KindShell {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	if cmd.Status != store.StatusSent {
		writeError(w, http.StatusConflict, "conflict", "command is not awaiting an agent shell")
		return
	}
	if ss := s.shells().session(id); ss != nil && ss.hasAgent() {
		writeError(w, http.StatusConflict, "conflict", "a shell agent is already attached")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		// Accept already wrote the error response.
		return
	}
	ss := s.shells().getOrCreate(s, cmd, dev.Hostname, s.now())
	if !ss.attachAgent(conn) {
		// A second agent raced in between the pre-check and here; refuse it.
		conn.Close(websocket.StatusPolicyViolation, "agent already attached")
		return
	}
	ss.runAgent(conn)
}
