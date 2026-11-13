package server

import (
	"context"
	"time"

	"github.com/coder/websocket"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// shell_relay.go drives one live shell: the two WebSocket legs are attached by
// the handlers, then each leg runs a read loop that copies frames verbatim to
// the other while the server counts bytes, records the agent->browser transcript
// and watches the idle / max-duration / agent-connect / shutdown deadlines.
// Either leg (or any deadline) closing tears the whole session down exactly once
// (closeOnce): the command is completed, the shell_sessions row flushed and the
// shell.closed audit written per RS1 section B.

// getOrCreate returns the live session for a command, creating it (and its
// shell_sessions row and supervisor) on first use. postTime bounds the
// agent-never-connected deadline.
func (m *shellManager) getOrCreate(s *Server, cmd store.Command, hostname string, postTime time.Time) *shellSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ss := m.sessions[cmd.ID]; ss != nil {
		return ss
	}
	ss := newShellSession(s, m, cmd, hostname, postTime)
	m.sessions[cmd.ID] = ss
	return ss
}

// hasAgent reports whether an agent leg is already attached (checked before the
// agent handshake to answer 409, and by the supervisor's connect deadline).
func (ss *shellSession) hasAgent() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.agent != nil
}

// attachAgent binds the agent WebSocket. It returns false if an agent is already
// attached (a race the pre-handshake 409 check normally catches). On success it
// records connected_at, writes the shell.connected audit and tells a waiting
// browser the shell is connected.
func (ss *shellSession) attachAgent(conn *websocket.Conn) bool {
	ss.mu.Lock()
	if ss.agent != nil {
		ss.mu.Unlock()
		return false
	}
	ss.agent = &wsConn{c: conn}
	now := ss.srv.now()
	ss.connectedAt = &now
	browserUp := ss.browser != nil
	ss.mu.Unlock()

	if err := ss.srv.st.SetShellConnected(ss.ctx, ss.cmdID, now); err != nil {
		ss.srv.log.Error("shell: set connected", "command", ss.cmdID, "err", err)
	}
	ss.srv.audit(ss.ctx, "device", &ss.deviceID, &ss.orgID, "shell.connected", "command", &ss.cmdID, "",
		map[string]any{"hostname": ss.hostname})
	if browserUp {
		ss.sendStatus(protocol.ShellStateConnected, "")
	}
	return true
}

// attachBrowser binds the browser WebSocket, records the technician for the
// close audit and sends the initial status frame (connected if the agent is
// already up, else waiting_for_agent).
func (ss *shellSession) attachBrowser(conn *websocket.Conn, t *shellTicket) {
	ss.mu.Lock()
	ss.browser = &wsConn{c: conn}
	uid := t.userID
	ss.userID = &uid
	ss.ip = t.ip
	connected := ss.agent != nil
	ss.mu.Unlock()

	if connected {
		ss.sendStatus(protocol.ShellStateConnected, "")
	} else {
		ss.sendStatus(protocol.ShellStateWaitingForAgent, "")
	}
}

// sendStatus writes a server->browser status control frame, if the browser is
// attached.
func (ss *shellSession) sendStatus(state, reason string) {
	ss.mu.Lock()
	b := ss.browser
	ss.mu.Unlock()
	if b == nil {
		return
	}
	_ = b.write(websocket.MessageText, protocol.StatusFrame(state, reason).Encode())
}

// runAgent is the agent leg read loop: agent->browser output is counted
// (bytes_out), recorded to the transcript and forwarded verbatim; a text frame
// is the exit control frame, whose code is captured for command completion and
// then forwarded on. It closes the session (agent_closed) when the loop ends.
func (ss *shellSession) runAgent(conn *websocket.Conn) {
	conn.SetReadLimit(shellReadLimit)
	defer ss.closeWith(protocol.CloseAgentClosed)
	for {
		typ, data, err := conn.Read(ss.ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			ss.recordOut(data)
			ss.forwardToBrowser(websocket.MessageBinary, data)
		case websocket.MessageText:
			if f, err := protocol.ParseControlFrame(data); err == nil && f.Type == protocol.CtrlExit && f.Code != nil {
				ss.mu.Lock()
				ss.exitArrived = true
				ss.exitCode = *f.Code
				ss.mu.Unlock()
			}
			ss.forwardToBrowser(websocket.MessageText, data)
		}
	}
}

// runBrowser is the browser leg read loop: keystrokes (binary) are forwarded to
// the agent and counted (bytes_in); a text frame is a resize control frame,
// forwarded verbatim. Browser input resets the idle timer. Bytes are dropped
// (not counted) while no agent is attached. It closes the session
// (technician_closed) when the loop ends.
func (ss *shellSession) runBrowser(conn *websocket.Conn) {
	conn.SetReadLimit(shellReadLimit)
	defer ss.closeWith(protocol.CloseTechnicianClosed)
	for {
		typ, data, err := conn.Read(ss.ctx)
		if err != nil {
			return
		}
		ss.touch()
		switch typ {
		case websocket.MessageBinary:
			if ss.forwardToAgent(websocket.MessageBinary, data) {
				ss.mu.Lock()
				ss.bytesIn += int64(len(data))
				ss.mu.Unlock()
			}
		case websocket.MessageText:
			if _, err := protocol.ParseControlFrame(data); err == nil {
				ss.forwardToAgent(websocket.MessageText, data)
			}
		}
	}
}

// touch marks browser activity and wakes the supervisor to reset the idle timer.
func (ss *shellSession) touch() {
	ss.mu.Lock()
	ss.lastInput = time.Now()
	ss.mu.Unlock()
	select {
	case ss.activityCh <- struct{}{}:
	default:
	}
}

// recordOut counts agent->browser bytes and appends them to the transcript up to
// the 1 MiB cap, after which recording stops (truncated) but relaying continues.
func (ss *shellSession) recordOut(data []byte) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.bytesOut += int64(len(data))
	if ss.truncated {
		return
	}
	room := shellTranscriptCap - len(ss.transcript)
	switch {
	case room <= 0:
		ss.truncated = true
	case len(data) > room:
		ss.transcript = append(ss.transcript, data[:room]...)
		ss.truncated = true
	default:
		ss.transcript = append(ss.transcript, data...)
	}
}

func (ss *shellSession) forwardToBrowser(typ websocket.MessageType, data []byte) {
	ss.mu.Lock()
	b := ss.browser
	ss.mu.Unlock()
	if b == nil {
		return
	}
	if err := b.write(typ, data); err != nil {
		ss.closeWith(protocol.CloseTechnicianClosed)
	}
}

// forwardToAgent writes to the agent leg; it returns false (the byte is dropped,
// not counted) when no agent is attached yet.
func (ss *shellSession) forwardToAgent(typ websocket.MessageType, data []byte) bool {
	ss.mu.Lock()
	a := ss.agent
	ss.mu.Unlock()
	if a == nil {
		return false
	}
	if err := a.write(typ, data); err != nil {
		ss.closeWith(protocol.CloseAgentClosed)
		return false
	}
	return true
}

// supervise enforces the four server-side deadlines until the session closes.
// The idle / max-duration / shutdown deadlines are wall-clock durations measured
// from when the relay started; the agent-connect deadline, by contrast, is
// anchored to the POST (RS1 section B: "within 60 s of the POST"), so a browser
// that connects late — the ticket stays valid for up to 60 s after the POST —
// does not win a fresh 60 s. The remaining window is agentConnectTTL minus the
// store-clock time already elapsed since postTime, floored at 0. The ticket's
// 60 s window is likewise measured on the store clock so the tests can drive it
// with clock.advance.
func (ss *shellSession) supervise() {
	agentConnectLeft := ss.mgr.agentConnectTTL - ss.srv.now().Sub(ss.postTime)
	if agentConnectLeft < 0 {
		agentConnectLeft = 0
	}
	agentTimer := time.NewTimer(agentConnectLeft)
	idleTimer := time.NewTimer(ss.mgr.idleTimeout)
	maxTimer := time.NewTimer(ss.maxDur)
	defer agentTimer.Stop()
	defer idleTimer.Stop()
	defer maxTimer.Stop()
	for {
		select {
		case <-ss.ctx.Done():
			return
		case <-ss.srv.shutdown:
			ss.closeWith(protocol.CloseServerShutdown)
			return
		case <-agentTimer.C:
			if !ss.hasAgent() {
				ss.closeWith(protocol.CloseAgentNeverConnected)
				return
			}
		case <-maxTimer.C:
			ss.closeWith(protocol.CloseMaxDuration)
			return
		case <-idleTimer.C:
			ss.mu.Lock()
			last := ss.lastInput
			ss.mu.Unlock()
			if idle := time.Since(last); idle >= ss.mgr.idleTimeout {
				ss.closeWith(protocol.CloseIdleTimeout)
				return
			} else {
				idleTimer.Reset(ss.mgr.idleTimeout - idle)
			}
		case <-ss.activityCh:
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(ss.mgr.idleTimeout)
		}
	}
}

// closeWith runs the teardown exactly once; the first reason wins.
func (ss *shellSession) closeWith(reason string) {
	ss.closeOnce.Do(func() { ss.teardown(reason) })
}

// teardown is the single close path: it tells the browser why, stops the relay
// loops and both sockets, completes the command, flushes the shell_sessions row
// and writes the shell.closed audit (RS1 section B).
func (ss *shellSession) teardown(reason string) {
	ss.mu.Lock()
	bytesIn, bytesOut := ss.bytesIn, ss.bytesOut
	transcript := ss.transcript
	truncated := ss.truncated
	connectedAt := ss.connectedAt
	exitArrived, exitCode := ss.exitArrived, ss.exitCode
	userID := ss.userID
	ip := ss.ip
	agent := ss.agent
	browser := ss.browser
	ss.mu.Unlock()

	if browser != nil {
		_ = browser.write(websocket.MessageText, protocol.StatusFrame(protocol.ShellStateClosed, reason).Encode())
	}
	ss.cancel()
	if agent != nil {
		agent.c.Close(websocket.StatusNormalClosure, "")
	}
	if browser != nil {
		browser.c.Close(websocket.StatusNormalClosure, "")
	}
	ss.mgr.remove(ss.cmdID)

	// The relay runs under the request contexts of the two legs, both now
	// cancelled; complete and flush under a fresh context.
	bg := context.Background()
	now := ss.srv.now()

	// Complete the command (RS1 section B): done when a 0 exit arrived or the
	// technician closed a live session, failed otherwise. exit_code is the exit
	// frame's code or -1 (so a technician-closed live session with no exit frame
	// is done with exit_code -1), stdout empty, error the close reason unless
	// done. CompleteShellCommand sets the status directly rather than deriving it.
	done := (exitArrived && exitCode == 0) || (reason == protocol.CloseTechnicianClosed && connectedAt != nil)
	status := store.StatusFailed
	errText := commandErrorText(reason)
	if done {
		status = store.StatusDone
		errText = ""
	}
	code := -1
	if exitArrived {
		code = exitCode
	}
	if err := ss.srv.st.CompleteShellCommand(bg, ss.cmdID, ss.deviceID, status, code, errText, connectedAt, now); err != nil {
		// ErrConflict means the command was never claimed (still queued) or was
		// already completed; the shell_sessions row and audit are still written.
		ss.srv.log.Error("shell: complete command", "command", ss.cmdID, "err", err)
	}

	if err := ss.srv.st.FinishShellSession(bg, ss.cmdID, store.FinishShellParams{
		ClosedAt:            now,
		BytesIn:             bytesIn,
		BytesOut:            bytesOut,
		Transcript:          transcript,
		TranscriptTruncated: truncated,
		CloseReason:         reason,
	}); err != nil {
		ss.srv.log.Error("shell: finish session", "command", ss.cmdID, "err", err)
	}

	duration := 0
	if connectedAt != nil {
		duration = int(now.Sub(*connectedAt).Seconds())
	}
	detail := map[string]any{"duration_s": duration, "bytes_in": bytesIn, "bytes_out": bytesOut, "reason": reason}
	if reason == protocol.CloseTechnicianClosed && userID != nil {
		ss.srv.audit(bg, "user", userID, &ss.orgID, "shell.closed", "command", &ss.cmdID, ip, detail)
	} else {
		ss.srv.audit(bg, "system", nil, &ss.orgID, "shell.closed", "command", &ss.cmdID, "", detail)
	}
	ss.srv.log.Info("shell closed", "command", ss.cmdID, "device", ss.deviceID, "reason", reason,
		"bytes_in", bytesIn, "bytes_out", bytesOut)
}
