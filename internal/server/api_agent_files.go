package server

import (
	"errors"
	"net/http"
	"os"
	"strconv"

	"github.com/sxi9/msp/internal/store"
)

// The agent file-download endpoint (SD1): GET /api/agent/v1/files/{id} with
// device-token auth (never a session). It streams a library file's bytes with
// Content-Length and an X-MSP-SHA256 header the agent re-verifies, but only when
// the file's org is the device's own org or NULL (a global artifact). Any other
// file — in another client's org — is 404 not_found, indistinguishable from a
// missing one, so a device token cannot probe another client's library.
//
// Registered through dashboardRoutes (the generic "register on the mux" list, the
// same one the dashboard routes use) so server.go's Handler picks it up without a
// change there. The device-token middleware is applied here, exactly as the other
// agent routes apply it in Handler.

func init() {
	dashboardRoutes = append(dashboardRoutes, registerAgentFiles)
}

func registerAgentFiles(s *Server, mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/v1/files/{id}", s.deviceAuth(s.handleAgentFile))
}

// RegisterAgentFiles exposes the agent-download route for an integrator that
// assembles the mux itself. See registerShell / registerPatch; use one or the
// other, not both.
func RegisterAgentFiles(s *Server, mux *http.ServeMux) { registerAgentFiles(s, mux) }

// handleAgentFile streams GET /api/agent/v1/files/{id} to an authenticated
// device. The org gate (global file or the device's own org, else 404) runs
// before the bytes are opened.
func (s *Server) handleAgentFile(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	f, err := s.st.GetFile(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "")
			return
		}
		s.log.Error("agent file: get", "id", id, "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	// Allowed only for a global file (org_id NULL) or the device's own org; any
	// other org is 404, not 403, so the device cannot tell the file exists (RS2).
	if f.OrgID != nil && *f.OrgID != dev.OrgID {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	if s.cfg.FilesDir == "" {
		s.log.Error("agent file: MSP_FILES_DIR is not configured")
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	file, err := os.Open(s.filePath(f.ID))
	if err != nil {
		// Metadata without bytes on disk: treat as not found for the agent.
		if os.IsNotExist(err) {
			s.log.Error("agent file: bytes missing on disk", "id", f.ID)
			writeError(w, http.StatusNotFound, "not_found", "")
			return
		}
		s.log.Error("agent file: open", "id", f.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		s.log.Error("agent file: stat", "id", f.ID, "err", err)
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	// X-MSP-SHA256 carries the stored hash the agent re-verifies after download;
	// http.ServeContent sets Content-Length (and the 200) for the full-body GET.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-MSP-SHA256", f.SHA256)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, strconv.FormatInt(f.ID, 10), info.ModTime(), file)
}
