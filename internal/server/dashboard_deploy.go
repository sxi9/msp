package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Software-deployment dashboard endpoint (SD1): POST /api/v1/devices/{id}/deploy
// queues a kind deploy command. A file deploy pushes a library file to the device
// (and optionally runs it); a package deploy installs a named OS package. The
// agent (internal/agent/deploy.go) downloads the file over the device-token
// endpoint (api_agent_files.go), re-verifies the hash and runs it, or installs
// the package through the PM1 executor, reporting the log back on the command
// result like exec/script. Registered through dashboardRoutes like dashboard_patch.go.

func init() {
	dashboardRoutes = append(dashboardRoutes, registerDeploy)
}

func registerDeploy(s *Server, mux *http.ServeMux) {
	mux.Handle("POST /api/v1/devices/{id}/deploy", s.dashRoute(RoleOperator, s.handleDeploy))
}

// RegisterDeploy exposes the deploy route for a hand-wiring integrator. See
// registerShell / registerPatch; use one or the other, not both.
func RegisterDeploy(s *Server, mux *http.ServeMux) { registerDeploy(s, mux) }

// deployRequest is the POST /devices/{id}/deploy body. The caller never supplies
// the sha256: the server looks the library file up and uses its stored hash, so a
// deploy cannot be pointed at a file with a forged digest.
type deployRequest struct {
	Source   string   `json:"source"`   // "file" | "package"
	FileID   int64    `json:"file_id"`  // file source
	Dest     string   `json:"dest"`     // file source: absolute path, cleaned, no ".."
	Run      bool     `json:"run"`      // file source: run dest after writing
	RunArgs  []string `json:"run_args"` // file source: argv, only with run
	Package  string   `json:"package"`  // package source
	TimeoutS int      `json:"timeout_s"`
}

// handleDeploy is POST /api/v1/devices/{id}/deploy (admin|operator, CSRF).
// Validation order (mirrors handlePatch): device exists (404) -> org visible
// (404 across orgs, RS2) -> not revoked (unsupported_on_device) -> desktop
// platform (unsupported_on_platform, i.e. android) -> body shape
// (invalid_request) -> for a file source, the file must be visible for the
// device's org or global (404, else) and supplies the sha256 -> full shape
// validation (invalid_request, e.g. a dest with "..") -> online (device_offline).
// Then the deploy command is queued, deploy.requested audited and the device's
// parked sync woken.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
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
	// deploy is desktop only (KindsFor), so android (and any non-desktop) is 400
	// unsupported_on_platform, like exec/script/shell/patch.
	if !protocol.KindSupportedOn(protocol.KindDeploy, dev.Platform) {
		writeError(w, http.StatusBadRequest, "unsupported_on_platform",
			fmt.Sprintf("software deployment is not available on %s", dev.Platform))
		return
	}

	var req deployRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !protocol.ValidDeploySource(req.Source) {
		writeError(w, http.StatusBadRequest, "invalid_request", "deploy source must be one of file, package")
		return
	}

	cmd := protocol.Command{
		Kind:     protocol.KindDeploy,
		Source:   req.Source,
		Dest:     req.Dest,
		Run:      req.Run,
		RunArgs:  req.RunArgs,
		Package:  req.Package,
		TimeoutS: req.TimeoutS,
	}

	// For a file source, resolve the library file and take its stored hash. The
	// file must be usable for this device: its org is the device's org or global
	// (NULL); anything else is 404, indistinguishable from a missing file (RS2).
	var file store.File
	if req.Source == protocol.DeploySourceFile {
		if req.FileID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "deploy file source requires file_id")
			return
		}
		file, err = s.st.GetFile(ctx, req.FileID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "")
				return
			}
			s.log.Error("deploy: get file", "file", req.FileID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "")
			return
		}
		if file.OrgID != nil && *file.OrgID != dev.OrgID {
			writeError(w, http.StatusNotFound, "not_found", "")
			return
		}
		cmd.FileID = file.ID
		cmd.SHA256 = file.SHA256
	}

	// Full shape validation with the shared validator: source-specific fields, a
	// clean absolute dest (rejects ".."), run_args only with run, bounded timeout.
	if err := cmd.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
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
		Kind:      protocol.KindDeploy,
		Source:    cmd.Source,
		FileID:    cmd.FileID,
		SHA256:    cmd.SHA256,
		Dest:      cmd.Dest,
		Run:       cmd.Run,
		RunArgs:   cmd.RunArgs,
		Package:   cmd.Package,
		TimeoutS:  cmd.TimeoutS,
	})
	if err != nil {
		s.log.Error("create deploy command", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}

	actor := u.ID
	ip := clientIP(r)
	detail := map[string]any{
		"command_id": created.ID,
		"hostname":   dev.Hostname,
		"platform":   dev.Platform,
		"source":     cmd.Source,
		"run":        cmd.Run,
	}
	if cmd.Source == protocol.DeploySourceFile {
		detail["file_id"] = cmd.FileID
		detail["file_name"] = file.Name
		detail["dest"] = cmd.Dest
	} else {
		detail["package"] = cmd.Package
	}
	s.audit(ctx, "user", &actor, &dev.OrgID, "deploy.requested", "command", &created.ID, ip, detail)
	s.notifier.Notify(dev.ID)
	s.log.Info("deploy queued", "command", created.ID, "device", dev.ID, "source", cmd.Source, "user", u.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"command": created})
}
