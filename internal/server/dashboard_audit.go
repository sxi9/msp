package server

import (
	"net/http"
	"strconv"

	"github.com/sxi9/msp/internal/store"
)

// handleListAudit is GET /api/v1/audit?limit=200&org_id=&before=<id>,
// newest first (limit default 200, max 1000).
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	orgID, ok := dashOrgIDParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "org_id must be a positive integer")
		return
	}
	var before int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "before must be a positive integer")
			return
		}
		before = n
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return
	}
	if orgID != nil && !s.dashOrgVisible(w, r, *orgID) {
		return
	}
	entries, err := s.st.ListAudit(r.Context(), dashLimit(r, 200, 1000), orgID, visible, before)
	if err != nil {
		s.log.Error("list audit", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
