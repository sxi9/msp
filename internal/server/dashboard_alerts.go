package server

import (
	"net/http"

	"github.com/sxi9/msp/internal/store"
)

// handleListAlerts is GET /api/v1/alerts?state=open|all&org_id= (limit 500,
// newest first; default state open).
func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	openOnly := true
	switch r.URL.Query().Get("state") {
	case "", "open":
	case "all":
		openOnly = false
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "state must be open or all")
		return
	}
	orgID, ok := dashOrgIDParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "org_id must be a positive integer")
		return
	}
	visible, vok := s.dashVisibleOrgs(w, r)
	if !vok {
		return
	}
	if orgID != nil && !s.dashOrgVisible(w, r, *orgID) {
		return
	}
	alerts, err := s.st.ListAlerts(r.Context(), openOnly, orgID, visible, 500)
	if err != nil {
		s.log.Error("list alerts", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if alerts == nil {
		alerts = []store.Alert{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts})
}

// handleAckAlert is POST /api/v1/alerts/{id}/ack (operator): flags the open
// alert as acknowledged by the session user. 404 when the alert is missing
// or already resolved.
func (s *Server) handleAckAlert(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	u := userFrom(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	ctx := r.Context()

	// Resolve the alert first so an ack across orgs 404s before the side
	// effect (RS2), and so the audit entry carries its org and device.
	alert, err := s.st.GetAlert(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get alert")
		return
	}
	if !s.dashOrgVisible(w, r, alert.OrgID) {
		return
	}
	orgID := alert.OrgID
	detail := map[string]any{
		"device_id": alert.DeviceID,
		"hostname":  alert.Hostname,
		"type":      alert.Type,
	}

	if err := s.st.AckAlert(ctx, id, u.ID); err != nil {
		s.dashStoreError(w, err, "ack alert")
		return
	}
	actor := u.ID
	s.audit(ctx, "user", &actor, &orgID, "alert.ack", "alert", &id, clientIP(r), detail)
	w.WriteHeader(http.StatusNoContent)
}
