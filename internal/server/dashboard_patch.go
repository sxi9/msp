package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Patch install/scan dashboard endpoint (PM1). This file registers the patch
// route (appended to dashboardRoutes so server.go's Handler picks it up without
// a change there, exactly like dashboard_shell.go) and holds the single
// dashboard-facing handler: POST /api/v1/devices/{id}/patch, which queues a
// kind patch command through the existing command-create path. The agent side
// (internal/agent/patch*.go) applies it and reports the install log back on the
// command result like exec/script; the report that this panel reads is recorded
// by the sync/enroll handler via store.RecordPatches.

func init() {
	dashboardRoutes = append(dashboardRoutes, registerPatch)
}

// registerPatch adds the patch route inside the standard sessionAuth -> csrf ->
// requireRole chain (operator, so admin|operator). RegisterPatch is the exported
// entry point for an integrator that wires the mux by hand; using both would
// double-register and panic, so an integrator uses one or the other.
func registerPatch(s *Server, mux *http.ServeMux) {
	mux.Handle("POST /api/v1/devices/{id}/patch", s.dashRoute(RoleOperator, s.handlePatch))
}

// RegisterPatch exposes the patch route registration for an integrator that
// assembles the mux itself (WP-INTEGRATE). See registerShell for the same
// pattern.
func RegisterPatch(s *Server, mux *http.ServeMux) { registerPatch(s, mux) }

// patchRequest is the POST /devices/{id}/patch body. Every field is optional:
// action defaults to install, mode to security, reboot to never, timeout_s to
// the patch default. A scan ignores mode/ids/reboot (it just re-collects the
// report), so those are cleared before the command is queued.
type patchRequest struct {
	Action   string   `json:"action"`
	Mode     string   `json:"mode"`
	IDs      []string `json:"ids"`
	Reboot   string   `json:"reboot"`
	TimeoutS int      `json:"timeout_s"`
}

// handlePatch is POST /api/v1/devices/{id}/patch (admin|operator, CSRF).
// Validation order (mirrors handleOpenShell): device exists (404) -> org visible
// (404 across orgs, RS2) -> not revoked (unsupported_on_device) -> desktop
// platform (unsupported_on_platform, i.e. android) -> body shape
// (invalid_request, via protocol.Command.Validate) -> ids a subset of the
// pending list where the server has one (invalid_request) -> online
// (device_offline). Then the patch command is queued, the scan/install audited
// with mode/ids/reboot, and the device's parked sync woken.
func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
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
	// patch is desktop only (KindsFor), so android (and any non-desktop) is
	// 400 unsupported_on_platform, like exec/script/shell.
	if !protocol.KindSupportedOn(protocol.KindPatch, dev.Platform) {
		writeError(w, http.StatusBadRequest, "unsupported_on_platform",
			fmt.Sprintf("patching is not available on %s", dev.Platform))
		return
	}

	var req patchRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// Apply the defaults, then normalise a scan (mode/ids/reboot ignored).
	action := req.Action
	if action == "" {
		action = protocol.PatchActionInstall
	}
	mode := req.Mode
	if mode == "" {
		mode = protocol.PatchModeSecurity
	}
	reboot := req.Reboot
	if reboot == "" {
		reboot = protocol.PatchRebootNever
	}
	ids := req.IDs
	if action == protocol.PatchActionScan {
		mode, ids, reboot = "", nil, ""
	}

	// Validate the shape with the shared command validator (it covers the patch
	// fields: valid action/mode/reboot, bounded ids and timeout_s).
	cmd := protocol.Command{
		Kind:     protocol.KindPatch,
		Action:   action,
		Mode:     mode,
		IDs:      ids,
		Reboot:   reboot,
		TimeoutS: req.TimeoutS,
	}
	if err := cmd.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Server-side ids subset check "where applicable": when the device has a
	// pending-updates report, every requested id must be one of its package
	// names. Without a report the server cannot check, so it defers to the agent
	// (which fails the command naming the unknown ids). Only install+ids uses ids.
	if action == protocol.PatchActionInstall && mode == protocol.PatchModeIDs {
		if unknown := unknownPatchIDs(dev.PatchesJSON, ids); unknown != nil {
			writeError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("unknown package(s) not in the pending list: %s", strings.Join(unknown, ", ")))
			return
		}
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
		Kind:      protocol.KindPatch,
		Action:    action,
		Mode:      mode,
		IDs:       ids,
		Reboot:    reboot,
		TimeoutS:  req.TimeoutS,
	})
	if err != nil {
		s.log.Error("create patch command", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}

	actor := u.ID
	ip := clientIP(r)
	act := "patch.install_requested"
	detail := map[string]any{
		"command_id": created.ID,
		"hostname":   dev.Hostname,
		"platform":   dev.Platform,
		"action":     action,
	}
	if action == protocol.PatchActionScan {
		act = "patch.scan_requested"
	} else {
		detail["mode"] = mode
		detail["reboot"] = reboot
		if mode == protocol.PatchModeIDs {
			detail["ids"] = ids
		}
	}
	s.audit(ctx, "user", &actor, &dev.OrgID, act, "command", &created.ID, ip, detail)
	s.notifier.Notify(dev.ID)
	s.log.Info("patch queued", "command", created.ID, "device", dev.ID, "action", action, "mode", mode, "user", u.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"command": created})
}

// unknownPatchIDs returns the requested ids that are not package names in the
// device's stored patch report, or nil when every id is known (or the report
// has no usable update list, in which case the subset check does not apply and
// the agent validates instead). The returned slice is sorted for a stable
// detail message.
func unknownPatchIDs(patchesJSON string, ids []string) []string {
	if patchesJSON == "" || len(ids) == 0 {
		return nil
	}
	var report protocol.PatchReport
	if err := json.Unmarshal([]byte(patchesJSON), &report); err != nil {
		return nil // unreadable report: defer to the agent
	}
	if len(report.Updates) == 0 {
		return nil // no pending list to check against: defer to the agent
	}
	pending := make(map[string]struct{}, len(report.Updates))
	for _, u := range report.Updates {
		pending[u.Name] = struct{}{}
	}
	var unknown []string
	for _, id := range ids {
		if _, ok := pending[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	return unknown
}
