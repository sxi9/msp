package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sxi9/msp/internal/store"
)

// The server-side file library (SD1). Staff upload artifacts here; the metadata
// lives in the files table (internal/store/files.go) and the bytes on disk under
// MSP_FILES_DIR/<id>. Agents fetch them over the device-token endpoint
// (api_agent_files.go) and a deploy command (dashboard_deploy.go) pushes one to
// a device. This file holds the four staff endpoints:
//
//	POST   /api/v1/files       multipart upload (admin|operator), RS2 org-scoped
//	GET    /api/v1/files       list (viewer), org-scoped, never streams bytes
//	GET    /api/v1/files/{id}  metadata (viewer)
//	DELETE /api/v1/files/{id}  remove the row and the disk file (admin|operator)
//
// Routes register through dashboardRoutes so server.go's Handler picks them up
// without a change there, exactly like dashboard_patch.go / dashboard_shell.go.

func init() {
	dashboardRoutes = append(dashboardRoutes, registerFiles)
}

// registerFiles adds the file-library routes inside the standard
// sessionAuth -> csrf -> requireRole chain. GETs need viewer; the upload and
// delete need operator (so admin|operator). RegisterFiles is the exported entry
// point for an integrator that wires the mux by hand (WP-INTEGRATE); using both
// would double-register and panic, so an integrator uses one or the other.
func registerFiles(s *Server, mux *http.ServeMux) {
	mux.Handle("POST /api/v1/files", s.dashRoute(RoleOperator, s.handleUploadFile))
	mux.Handle("GET /api/v1/files", s.dashRoute(RoleViewer, s.handleListFiles))
	mux.Handle("GET /api/v1/files/{id}", s.dashRoute(RoleViewer, s.handleGetFile))
	mux.Handle("DELETE /api/v1/files/{id}", s.dashRoute(RoleOperator, s.handleDeleteFile))
}

// RegisterFiles exposes the file-library route registration for an integrator
// that assembles the mux itself. See registerShell / registerPatch.
func RegisterFiles(s *Server, mux *http.ServeMux) { registerFiles(s, mux) }

// maxFileNameLen bounds the stored display name; the name is metadata only (the
// bytes live at MSP_FILES_DIR/<id>, never at a path derived from the name).
const maxFileNameLen = 255

// filePath is where a library file's bytes live on disk: MSP_FILES_DIR/<id>.
func (s *Server) filePath(id int64) string {
	return filepath.Join(s.cfg.FilesDir, strconv.FormatInt(id, 10))
}

// handleUploadFile is POST /api/v1/files (admin|operator, CSRF). It is a
// multipart/form-data upload: a `file` part plus `name` and optional `org_id`
// form fields. The file part is streamed to a temp file in MSP_FILES_DIR (the
// same filesystem as the final path, so the move is atomic) while its sha256 and
// size are computed; a file larger than MSP_MAX_FILE_MB is 413. The row is then
// inserted and the temp file renamed to MSP_FILES_DIR/<id>. Org rules (RS2):
// admin or an operator who can see the named org; a global file (org_id omitted)
// requires all-orgs. Audit file.uploaded (name, size, sha256).
//
// This endpoint is exempt from the generic dashboard body limit (middleware.go
// bodyLimit) because the artifact is far larger than a JSON request; the per-file
// cap below is the real limit.
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.cfg.FilesDir == "" {
		s.log.Error("file upload: MSP_FILES_DIR is not configured")
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	maxBytes := int64(s.cfg.MaxFileMB) << 20
	// Cap the whole request body a little above the per-file cap to cover the
	// multipart envelope and the small name/org_id fields; the file part itself
	// is counted and rejected at exactly maxBytes with 413 below.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "expected a multipart/form-data upload")
		return
	}

	var (
		tmpPath     string
		haveFile    bool
		uploadName  string
		contentType string
		size        int64
		sum         string
		name        string
		orgID       *int64
	)
	// cleanup removes a half-written temp file on any early exit.
	cleanup := func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
			tmpPath = ""
		}
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			if isTooLarge(err) {
				s.writeFileTooLarge(w)
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request", "malformed multipart body")
			return
		}
		switch part.FormName() {
		case "file":
			if haveFile { // ignore a second file part
				_ = part.Close()
				continue
			}
			haveFile = true
			uploadName = part.FileName()
			contentType = part.Header.Get("Content-Type")
			if err := os.MkdirAll(s.cfg.FilesDir, 0o755); err != nil {
				_ = part.Close()
				s.log.Error("file upload: mkdir files dir", "dir", s.cfg.FilesDir, "err", err)
				writeError(w, http.StatusInternalServerError, "internal", "")
				return
			}
			tmp, err := os.CreateTemp(s.cfg.FilesDir, ".upload-*")
			if err != nil {
				_ = part.Close()
				s.log.Error("file upload: create temp", "err", err)
				writeError(w, http.StatusInternalServerError, "internal", "")
				return
			}
			tmpPath = tmp.Name()
			h := sha256.New()
			// LimitReader at maxBytes+1 so a file exactly at the cap is accepted
			// and one byte over trips the 413 below.
			n, copyErr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(part, maxBytes+1))
			closeErr := tmp.Close()
			_ = part.Close()
			if copyErr != nil {
				cleanup()
				if isTooLarge(copyErr) {
					s.writeFileTooLarge(w)
					return
				}
				s.log.Error("file upload: stream to temp", "err", copyErr)
				writeError(w, http.StatusInternalServerError, "internal", "")
				return
			}
			if closeErr != nil {
				cleanup()
				s.log.Error("file upload: close temp", "err", closeErr)
				writeError(w, http.StatusInternalServerError, "internal", "")
				return
			}
			if n > maxBytes {
				cleanup()
				s.writeFileTooLarge(w)
				return
			}
			size = n
			sum = hex.EncodeToString(h.Sum(nil))
		case "name":
			v, err := readSmallPart(part)
			_ = part.Close()
			if err != nil {
				cleanup()
				writeError(w, http.StatusBadRequest, "invalid_request", "name field is too large")
				return
			}
			name = strings.TrimSpace(v)
		case "org_id":
			v, err := readSmallPart(part)
			_ = part.Close()
			if err != nil {
				cleanup()
				writeError(w, http.StatusBadRequest, "invalid_request", "org_id field is too large")
				return
			}
			if v = strings.TrimSpace(v); v != "" {
				n, perr := strconv.ParseInt(v, 10, 64)
				if perr != nil || n <= 0 {
					cleanup()
					writeError(w, http.StatusBadRequest, "invalid_request", "org_id must be a positive integer")
					return
				}
				orgID = &n
			}
		default:
			_ = part.Close()
		}
	}

	if !haveFile {
		cleanup()
		writeError(w, http.StatusBadRequest, "invalid_request", "a file part is required")
		return
	}
	// Fall back to the uploaded filename when no explicit name was given.
	if name == "" {
		name = strings.TrimSpace(uploadName)
	}
	switch {
	case name == "":
		cleanup()
		writeError(w, http.StatusBadRequest, "invalid_request", "a file name is required")
		return
	case len(name) > maxFileNameLen:
		cleanup()
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("name exceeds %d bytes", maxFileNameLen))
		return
	case hasControlChars(name):
		cleanup()
		writeError(w, http.StatusBadRequest, "invalid_request", "name contains control characters")
		return
	}

	// Org permission (RS2): a named org must be visible to the caller (else 404,
	// hidden like any cross-org resource); a global file (no org_id) requires
	// all-orgs (else 403). Checked after streaming so the temp file is cleaned up.
	if !s.fileCreateAllowed(w, r, orgID) {
		cleanup()
		return
	}

	u := userFrom(ctx)
	var createdBy *int64
	if u != nil {
		createdBy = &u.ID
	}
	f, err := s.st.CreateFile(ctx, store.CreateFileParams{
		OrgID:       orgID,
		Name:        name,
		Size:        size,
		SHA256:      sum,
		ContentType: contentType,
		CreatedBy:   createdBy,
	})
	if err != nil {
		cleanup()
		s.log.Error("create file", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	// Move the bytes into place. On failure roll the row back so there is never
	// metadata without bytes.
	if err := os.Rename(tmpPath, s.filePath(f.ID)); err != nil {
		if delErr := s.st.DeleteFile(ctx, f.ID); delErr != nil {
			s.log.Error("file upload: rollback row", "id", f.ID, "err", delErr)
		}
		cleanup()
		s.log.Error("file upload: move into place", "id", f.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	tmpPath = "" // ownership transferred to the final path

	s.audit(ctx, "user", createdBy, orgID, "file.uploaded", "file", &f.ID, clientIP(r), map[string]any{
		"name":   f.Name,
		"size":   f.Size,
		"sha256": f.SHA256,
	})
	s.log.Info("file uploaded", "id", f.ID, "name", f.Name, "size", f.Size, "global", orgID == nil)
	writeJSON(w, http.StatusCreated, map[string]any{"file": f})
}

// handleListFiles is GET /api/v1/files?org_id= (viewer): the library, org-scoped
// (RS2), newest first, never the bytes.
func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	orgID, ok := dashOrgIDParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "org_id must be a positive integer")
		return
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return
	}
	if orgID != nil && !s.dashOrgVisible(w, r, *orgID) {
		return
	}
	files, err := s.st.ListFiles(r.Context(), store.FileFilter{OrgID: orgID, VisibleOrgs: visible})
	if err != nil {
		s.log.Error("list files", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if files == nil {
		files = []store.File{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// handleGetFile is GET /api/v1/files/{id} (viewer): one file's metadata, 404
// when it does not exist or is not visible to the caller (RS2).
func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	f, err := s.st.GetFile(r.Context(), id)
	if err != nil {
		s.dashStoreError(w, err, "get file")
		return
	}
	if !s.fileRowVisible(w, r, f.OrgID) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": f})
}

// handleDeleteFile is DELETE /api/v1/files/{id} (admin|operator): removes the
// row and the disk file. 404 across orgs before the delete (RS2). Audit
// file.deleted.
func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	f, err := s.st.GetFile(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get file")
		return
	}
	if !s.fileRowVisible(w, r, f.OrgID) {
		return
	}
	if err := s.st.DeleteFile(ctx, id); err != nil {
		s.dashStoreError(w, err, "delete file")
		return
	}
	// Remove the bytes (best effort: the row is already gone, so a missing or
	// unremovable file must not fail the request).
	if s.cfg.FilesDir != "" {
		if err := os.Remove(s.filePath(id)); err != nil && !os.IsNotExist(err) {
			s.log.Error("file delete: remove bytes", "id", id, "err", err)
		}
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, f.OrgID, "file.deleted", "file", &id, clientIP(r), map[string]any{
		"name":   f.Name,
		"size":   f.Size,
		"sha256": f.SHA256,
	})
	w.WriteHeader(http.StatusNoContent)
}

// fileRowVisible reports whether the caller may see a stored file with the given
// (nullable) org, writing 404 not_found when not — the same response an
// unrelated resource gives (RS2). A global file (orgID nil) is visible only to an
// all-orgs caller.
func (s *Server) fileRowVisible(w http.ResponseWriter, r *http.Request, orgID *int64) bool {
	if orgID != nil {
		return s.dashOrgVisible(w, r, *orgID)
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return false
	}
	if visible != nil { // scoped user: cannot see a global file
		writeError(w, http.StatusNotFound, "not_found", "")
		return false
	}
	return true
}

// fileCreateAllowed reports whether the caller may create a file in the given
// (nullable) org. A concrete org the caller cannot see is 404 (hidden like any
// cross-org resource); a global file (orgID nil) requires all-orgs, else 403.
func (s *Server) fileCreateAllowed(w http.ResponseWriter, r *http.Request, orgID *int64) bool {
	if orgID != nil {
		return s.dashOrgVisible(w, r, *orgID)
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return false
	}
	if visible != nil {
		writeError(w, http.StatusForbidden, "forbidden", "a global file requires access to all clients")
		return false
	}
	return true
}

// writeFileTooLarge answers 413 with the configured cap in the detail.
func (s *Server) writeFileTooLarge(w http.ResponseWriter) {
	writeError(w, http.StatusRequestEntityTooLarge, "file_too_large",
		fmt.Sprintf("the upload exceeds the %d MiB limit", s.cfg.MaxFileMB))
}

// readSmallPart reads a small multipart form field (name, org_id). It caps the
// read so a field cannot blow the request budget; the error is non-nil only when
// the part itself errors.
func readSmallPart(part io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, 4096))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// isTooLarge reports whether err is (or wraps) the body-limit error, which the
// handler maps to 413.
func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}
