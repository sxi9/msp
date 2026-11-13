package server

import (
	"net/http"

	"github.com/sxi9/msp/internal/protocol"
)

// Remote desktop through the MSP's own RustDesk relay (decision D4, phase A,
// feature RD1). The server knows the relay (cfg.RustdeskHost + RustdeskKey)
// and each device's RustDesk id (reported by the agent in its facts). The
// dashboard asks GET /api/v1/rustdesk whether the feature is on, and the
// Connect button POSTs /devices/{id}/remote-desktop, which audits the request
// and answers the rustdesk://connect/<id> URL the technician's own client
// opens. The screen session itself runs between the two RustDesk clients
// through hbbs/hbbr; the MSP server is never in that path.

// handleRustdeskInfo is GET /api/v1/rustdesk (any role).
func (s *Server) handleRustdeskInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.rustdeskEnabled(),
		"host":    s.cfg.RustdeskHost,
		"key":     s.cfg.RustdeskKey,
	})
}

// handleRemoteDesktop is POST /api/v1/devices/{id}/remote-desktop (operator).
// Like every device-scoped endpoint it resolves the device, then returns 404
// not_found if the device is unknown, in an org the caller cannot see, or
// revoked — before any side effect (RS2 per-user visibility and the
// 404-not-403 rule, docs/SECURITY.md sections 4 and 8). 409 rustdesk_disabled
// when the server has no relay, 409 no_rustdesk_id when the agent never
// reported one. The id is whatever the agent asserted in its facts (digits
// only): the server cannot verify that it belongs to this machine.
func (s *Server) handleRemoteDesktop(w http.ResponseWriter, r *http.Request) {
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
	if dev.Revoked {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	if !s.rustdeskEnabled() {
		writeError(w, http.StatusConflict, "rustdesk_disabled", "set MSP_RUSTDESK_HOST and MSP_RUSTDESK_KEY on the server (docs/OPS.md, Remote desktop)")
		return
	}
	if dev.RustdeskID == nil || !protocol.ValidRustdeskID(*dev.RustdeskID) {
		writeError(w, http.StatusConflict, "no_rustdesk_id", "the agent has not reported a RustDesk id; install the RustDesk client with --with-rustdesk")
		return
	}
	u := userFrom(ctx)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	rdID := *dev.RustdeskID
	actor := u.ID
	s.audit(ctx, "user", &actor, &dev.OrgID, "device.remote_desktop_opened", "device", &dev.ID, clientIP(r), map[string]any{
		"hostname":    dev.Hostname,
		"platform":    dev.Platform,
		"rustdesk_id": rdID,
		"host":        s.cfg.RustdeskHost,
	})
	s.log.Info("remote desktop opened", "device", dev.ID, "user", u.ID, "rustdesk_id", rdID)
	writeJSON(w, http.StatusOK, map[string]any{
		"url":         protocol.RustdeskConnectURL(rdID),
		"rustdesk_id": rdID,
		"host":        s.cfg.RustdeskHost,
		"key":         s.cfg.RustdeskKey,
	})
}
