package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Field length caps for enrollment input; the protocol gives no numbers, so
// these only keep garbage out of the database.
const (
	maxEnrollField = 256
	maxHostname    = 253
)

var validArches = map[string]bool{"amd64": true, "arm64": true, "386": true, "arm": true}

// handlePing is GET /api/agent/v1/ping (no auth).
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.PingResponse{OK: true, Version: s.cfg.Version})
}

// handleHealthz is GET /healthz: 200 when the database answers, else 503.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.st.Ping(r.Context()); err != nil {
		s.log.Error("healthz", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// validateEnroll checks the shape of an enrollment request; the returned
// text is the 400 detail.
func validateEnroll(req protocol.EnrollRequest) error {
	switch {
	case req.EnrollmentToken == "":
		return errors.New("enrollment_token is required")
	case len(req.EnrollmentToken) > maxEnrollField:
		return errors.New("enrollment_token too long")
	case req.DeviceUID == "":
		return errors.New("device_uid is required")
	case len(req.DeviceUID) > maxEnrollField:
		return errors.New("device_uid too long")
	case req.Hostname == "":
		return errors.New("hostname is required")
	case len(req.Hostname) > maxHostname:
		return errors.New("hostname too long")
	case !protocol.ValidPlatform(req.Platform):
		return errors.New("platform must be linux, windows or android")
	case !validArches[req.Arch]:
		return errors.New("arch must be amd64, arm64, 386 or arm")
	case len(req.OSVersion) > maxEnrollField:
		return errors.New("os_version too long")
	case len(req.AgentVersion) > maxEnrollField:
		return errors.New("agent_version too long")
	}
	return nil
}

// handleEnroll is POST /api/agent/v1/enroll (no auth, rate limited).
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.enrollLimit.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "")
		return
	}
	var req protocol.EnrollRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := validateEnroll(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	interval := protocol.ClampInterval(req.IntervalS, req.Platform)

	ctx := r.Context()
	tok, err := s.st.ConsumeEnrollmentToken(ctx, req.EnrollmentToken)
	if err != nil {
		if !errors.Is(err, store.ErrInvalidToken) {
			s.log.Error("consume enrollment token", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid_enrollment_token", "")
		return
	}

	facts, err := json.Marshal(req.Facts)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "facts: "+err.Error())
		return
	}
	dev, plain, err := s.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID:        tok.OrgID,
		DeviceUID:    req.DeviceUID,
		Hostname:     req.Hostname,
		Platform:     req.Platform,
		Arch:         req.Arch,
		OSVersion:    req.OSVersion,
		AgentVersion: req.AgentVersion,
		IntervalS:    interval,
		FactsJSON:    string(facts),
	})
	if err != nil {
		s.log.Error("upsert device", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	s.audit(ctx, "device", &dev.ID, &dev.OrgID, "device.enrolled", "device", &dev.ID, ip, map[string]any{
		"hostname": dev.Hostname,
		"platform": dev.Platform,
		"token_id": tok.ID,
	})
	s.log.Info("device enrolled", "device", dev.ID, "org", dev.OrgID, "hostname", dev.Hostname, "platform", dev.Platform)
	writeJSON(w, http.StatusCreated, protocol.EnrollResponse{
		DeviceID:      dev.ID,
		DeviceToken:   plain,
		PollIntervalS: dev.IntervalS,
	})
}

// handleSync is POST /api/agent/v1/sync: records the heartbeat, claims queued
// commands and, for desktop agents with wait_s > 0 and nothing queued, parks
// the request until a command is queued, wait_s elapses or the client leaves.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request, dev store.Device) {
	var req protocol.SyncRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	interval := protocol.ClampInterval(req.IntervalS, dev.Platform)
	req.IntervalS = interval
	wait := protocol.ClampWait(req.WaitS)
	if dev.Platform == protocol.PlatformAndroid {
		wait = 0
	}

	ctx := r.Context()
	if err := s.st.RecordSync(ctx, dev.ID, clientIP(r), req, s.cfg.SampleEvery); err != nil {
		s.log.Error("record sync", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}

	// Subscribe before the first claim so a command queued between the claim
	// and the select is not missed (the channel is buffered).
	wake, unsubscribe := s.notifier.Subscribe(dev.ID)
	defer unsubscribe()

	cmds, err := s.st.ClaimQueuedCommands(ctx, dev.ID, protocol.MaxCommandsPerSync)
	if err != nil {
		s.log.Error("claim commands", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if len(cmds) == 0 && wait > 0 {
		timer := time.NewTimer(time.Duration(wait) * time.Second)
		defer timer.Stop()
		select {
		case <-wake:
			cmds, err = s.st.ClaimQueuedCommands(ctx, dev.ID, protocol.MaxCommandsPerSync)
			if err != nil {
				s.log.Error("claim commands", "device", dev.ID, "err", err)
				writeError(w, http.StatusInternalServerError, "internal", "")
				return
			}
		case <-timer.C:
		case <-ctx.Done():
			// Client went away; nothing was claimed, nothing to write.
			return
		}
	}
	if cmds == nil {
		cmds = []protocol.Command{}
	}

	poll := interval
	more, err := s.st.HasQueuedCommands(ctx, dev.ID)
	if err != nil {
		s.log.Error("has queued commands", "device", dev.ID, "err", err)
	} else if more {
		poll = 5
	}
	writeJSON(w, http.StatusOK, protocol.SyncResponse{PollIntervalS: poll, Commands: cmds})
}

// handleResult is POST /api/agent/v1/commands/{id}/result.
func (s *Server) handleResult(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	var res protocol.CommandResult
	if err := readJSON(r, &res); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// The agent caps output itself; cap again so a misbehaving agent cannot
	// fill the database.
	if len(res.Stdout) > protocol.MaxOutputBytes {
		res.Stdout = res.Stdout[:protocol.MaxOutputBytes]
		res.Truncated = true
	}
	if len(res.Stderr) > protocol.MaxOutputBytes {
		res.Stderr = res.Stderr[:protocol.MaxOutputBytes]
		res.Truncated = true
	}
	if len(res.Error) > 1024 {
		res.Error = res.Error[:1024]
	}

	ctx := r.Context()
	status, err := s.st.CompleteCommand(ctx, id, dev.ID, res)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "")
		return
	case err != nil:
		s.log.Error("complete command", "command", id, "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	s.audit(ctx, "device", &dev.ID, &dev.OrgID, "command.finished", "command", &id, clientIP(r), map[string]any{
		"exit_code":    res.ExitCode,
		"status":       status,
		"stdout_bytes": len(res.Stdout),
		"stderr_bytes": len(res.Stderr),
		"truncated":    res.Truncated,
	})
	w.WriteHeader(http.StatusNoContent)
}
