package server

import (
	"fmt"
	"net/http"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Agent self-update dashboard surface (AU1). This file registers two routes
// (appended to dashboardRoutes so server.go's Handler picks them up without a
// change there, like dashboard_patch.go / dashboard_deploy.go):
//
//   - POST /api/v1/devices/{id}/update (admin|operator): queue a kind update
//     command. The technician never types a version/url/sha256 — the server fills
//     them from the release manifest for the device's platform-arch at queue time.
//   - GET /api/v1/agent-release (viewer): the current release manifest, so the
//     dashboard can show the released version banner and decide which devices are
//     outdated. When self-update is off (no agents.json) it returns an empty
//     manifest ({"version":"","files":{}}).
//
// It also holds agentUpdateFor, the sync-response advertise helper the agent sync
// handler calls to attach an optional agent_update block (see handleSync).

func init() {
	dashboardRoutes = append(dashboardRoutes, registerUpdate)
}

func registerUpdate(s *Server, mux *http.ServeMux) {
	mux.Handle("POST /api/v1/devices/{id}/update", s.dashRoute(RoleOperator, s.handleUpdate))
	mux.Handle("GET /api/v1/agent-release", s.dashRoute(RoleViewer, s.handleAgentRelease))
}

// RegisterUpdate exposes the update routes for an integrator that assembles the
// mux itself (WP-INTEGRATE). See registerPatch / registerDeploy for the same
// pattern; use registerUpdate or RegisterUpdate, not both (double-registering a
// pattern panics).
func RegisterUpdate(s *Server, mux *http.ServeMux) { registerUpdate(s, mux) }

// updateRequest is the optional POST /devices/{id}/update body. Every field is
// optional; the version/url/sha256 are never accepted from the caller (the server
// fills them from the manifest). timeout_s lets an operator lengthen the window
// for a slow link; it defaults to updateTimeoutS and is clamped by CreateCommand.
type updateRequest struct {
	TimeoutS int `json:"timeout_s"`
}

// updateTimeoutS is the default budget for a self-update command: a download, a
// hash verify, a health check and the swap. Longer than the 60 s default but
// within MaxTimeoutS; an operator may override it in the request.
const updateTimeoutS = 600

// handleUpdate is POST /api/v1/devices/{id}/update (admin|operator, CSRF).
// Validation order (mirrors handlePatch / handleDeploy): device exists (404) ->
// org visible (404 across orgs, RS2, before any side effect) -> not revoked
// (unsupported_on_device) -> desktop platform (unsupported_on_platform, i.e.
// android) -> a newer release exists for the device's platform-arch (else 409
// no_update_available) -> online (device_offline). Then the update command is
// queued with the manifest's version/url/sha256, agent.update_requested audited
// (from_version, to_version) and the device's parked sync woken.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
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
	if dev.Revoked {
		writeError(w, http.StatusBadRequest, "unsupported_on_device", "device token is revoked")
		return
	}
	// update is desktop only (KindsFor), so android (and any non-desktop) is 400
	// unsupported_on_platform, like exec/script/shell/patch/deploy.
	if !protocol.KindSupportedOn(protocol.KindUpdate, dev.Platform) {
		writeError(w, http.StatusBadRequest, "unsupported_on_platform",
			fmt.Sprintf("agent self-update is not available on %s", dev.Platform))
		return
	}

	var req updateRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Resolve the release for this device's platform-arch. No manifest entry, or a
	// released version equal to what the device already runs, means there is
	// nothing newer to push: 409 no_update_available (the dashboard greys the
	// button, but a stale page could still POST, so the server re-checks).
	entry, ok := s.manifests.ManifestForPlatform(dev.Platform + "-" + dev.Arch)
	if !ok || entry.Version == dev.AgentVersion {
		writeError(w, http.StatusConflict, "no_update_available", "")
		return
	}

	cmd := protocol.Command{
		Kind:     protocol.KindUpdate,
		Version:  entry.Version,
		URL:      agentDownloadPath(entry.Name),
		SHA256:   entry.SHA256,
		TimeoutS: req.TimeoutS,
	}
	if cmd.TimeoutS <= 0 {
		cmd.TimeoutS = updateTimeoutS
	}
	// Shape check with the shared validator (valid version/url/sha256, no stray
	// deploy/patch/exec fields, bounded timeout). A malformed manifest that slipped
	// past ParseManifest would be caught here before anything is queued.
	if err := cmd.Validate(); err != nil {
		s.log.Error("update: manifest produced an invalid command", "device", dev.ID, "err", err)
		writeError(w, http.StatusConflict, "no_update_available", "")
		return
	}

	if dev.Status != "online" {
		writeError(w, http.StatusConflict, "device_offline", "")
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
		Kind:      protocol.KindUpdate,
		Version:   cmd.Version,
		URL:       cmd.URL,
		SHA256:    cmd.SHA256,
		TimeoutS:  cmd.TimeoutS,
	})
	if err != nil {
		s.log.Error("create update command", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}

	actor := u.ID
	ip := clientIP(r)
	s.audit(ctx, "user", &actor, &dev.OrgID, "agent.update_requested", "command", &created.ID, ip, map[string]any{
		"command_id":   created.ID,
		"hostname":     dev.Hostname,
		"platform":     dev.Platform,
		"from_version": dev.AgentVersion,
		"to_version":   cmd.Version,
	})
	s.notifier.Notify(dev.ID)
	s.log.Info("agent update queued", "command", created.ID, "device", dev.ID,
		"from", dev.AgentVersion, "to", cmd.Version, "user", u.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"command": created})
}

// handleAgentRelease is GET /api/v1/agent-release (viewer): the current agent
// release manifest. When self-update is off (no usable agents.json) it returns an
// empty manifest so the dashboard can treat version == "" as "disabled". The hash
// is published (the agent re-verifies it after download) and is not a secret.
func (s *Server) handleAgentRelease(w http.ResponseWriter, r *http.Request) {
	m, ok := s.manifests.current()
	if !ok {
		writeJSON(w, http.StatusOK, Manifest{Version: "", Files: map[string]ManifestFile{}})
		return
	}
	if m.Files == nil {
		m.Files = map[string]ManifestFile{}
	}
	writeJSON(w, http.StatusOK, m)
}

// agentDownloadPath is the server-relative download URL for a manifest file name:
// /dl/<name>, matching handleDownload's route and the agent_update fixture.
func agentDownloadPath(name string) string {
	return "/dl/" + name
}

// agentUpdateFor computes the agent_update block to attach to a device's sync
// response (AU1), or nil when none should be sent. It sends one only when the
// release manifest has an entry for the device's platform-arch whose version
// differs from the version the agent just reported (reportedVersion). Android
// never gets it (KindsFor omits update), and a revoked device is left alone. The
// returned block is validated so a malformed manifest never reaches an agent.
//
// The agent sync handler (handleSync in api_agent.go) calls this and sets the
// result on the SyncResponse. Integrator insertion point: in handleSync, just
// before the final writeJSON of the SyncResponse, set
//
//	resp := protocol.SyncResponse{PollIntervalS: poll, Commands: cmds, Rustdesk: s.rustdeskInfo()}
//	resp.AgentUpdate = s.agentUpdateFor(dev, req.AgentVersion)
//	writeJSON(w, http.StatusOK, resp)
func (s *Server) agentUpdateFor(dev store.Device, reportedVersion string) *protocol.AgentUpdate {
	if dev.Revoked {
		return nil
	}
	if !protocol.KindSupportedOn(protocol.KindUpdate, dev.Platform) {
		return nil
	}
	entry, ok := s.manifests.ManifestForPlatform(dev.Platform + "-" + dev.Arch)
	if !ok || entry.Version == reportedVersion {
		return nil
	}
	up := &protocol.AgentUpdate{Version: entry.Version, URL: agentDownloadPath(entry.Name), SHA256: entry.SHA256}
	if err := up.Validate(); err != nil {
		s.log.Warn("not advertising a malformed agent_update", "device", dev.ID, "err", err)
		return nil
	}
	return up
}
