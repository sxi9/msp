package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sxi9/msp/internal/store"
)

// handleListDevices is GET /api/v1/devices?org_id=&status=&q=.
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	orgID, ok := dashOrgIDParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "org_id must be a positive integer")
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", "online", "offline", "pending":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "status must be online, offline or pending")
		return
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return
	}
	// An explicit ?org_id= that is not visible gets the same 404 as a
	// non-existent org (RS2), before the list is computed.
	if orgID != nil && !s.dashOrgVisible(w, r, *orgID) {
		return
	}
	devices, err := s.st.ListDevices(r.Context(), store.DeviceFilter{
		OrgID:       orgID,
		Status:      status,
		Query:       strings.TrimSpace(r.URL.Query().Get("q")),
		VisibleOrgs: visible,
	})
	if err != nil {
		s.log.Error("list devices", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if devices == nil {
		devices = []store.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

// dashJSONObject decodes a stored JSON object column; {} on any failure.
func dashJSONObject(raw string) map[string]any {
	m := map[string]any{}
	if raw == "" {
		return m
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// handleGetDevice is GET /api/v1/devices/{id}: the device, its facts and
// last metrics objects, its open alerts and its last 20 commands without
// output.
func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
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

	all, err := s.st.ListAlerts(ctx, true, &dev.OrgID, nil, 0)
	if err != nil {
		s.log.Error("list alerts", "device", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	alerts := []store.Alert{}
	for _, a := range all {
		if a.DeviceID == dev.ID {
			alerts = append(alerts, a)
		}
	}

	cmds, err := s.st.ListCommands(ctx, id, 20)
	if err != nil {
		s.log.Error("list commands", "device", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if cmds == nil {
		cmds = []store.Command{}
	}
	for i := range cmds {
		cmds[i].Stdout = ""
		cmds[i].Stderr = ""
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device":   dev,
		"facts":    dashJSONObject(dev.FactsJSON),
		"metrics":  dashJSONObject(dev.LastMetricsJSON),
		"patches":  dashJSONObject(dev.PatchesJSON),
		"alerts":   alerts,
		"commands": cmds,
	})
}

// handleListHeartbeats is GET /api/v1/devices/{id}/heartbeats?limit=120.
func (s *Server) handleListHeartbeats(w http.ResponseWriter, r *http.Request) {
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
	hbs, err := s.st.ListHeartbeats(ctx, id, dashLimit(r, 120, 1000))
	if err != nil {
		s.log.Error("list heartbeats", "device", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if hbs == nil {
		hbs = []store.Heartbeat{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"heartbeats": hbs})
}

// handleRevokeDevice is POST /api/v1/devices/{id}/revoke (admin): clears the
// device token so the agent's next request is 401.
func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
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
	if err := s.st.RevokeDevice(ctx, id); err != nil {
		s.dashStoreError(w, err, "revoke device")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, &dev.OrgID, "device.revoked", "device", &id, clientIP(r), map[string]any{
		"hostname": dev.Hostname,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteDevice is DELETE /api/v1/devices/{id} (admin): removes the
// device and, by cascade, its heartbeats, commands and alerts.
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
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
	if err := s.st.DeleteDevice(ctx, id); err != nil {
		s.dashStoreError(w, err, "delete device")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, &dev.OrgID, "device.deleted", "device", &id, clientIP(r), map[string]any{
		"hostname": dev.Hostname,
		"platform": dev.Platform,
	})
	w.WriteHeader(http.StatusNoContent)
}
