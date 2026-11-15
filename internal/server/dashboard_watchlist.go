package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Service/process watchlist + forwarded-log dashboard API (WL1). This file owns
// the staff-facing HTTP layer:
//
//	GET    /api/v1/watchlist?org_id=&device_id=   list items (RS2 org-scoped)
//	POST   /api/v1/watchlist                      create an org- or device-scoped item
//	PUT    /api/v1/watchlist/{id}                 update kind/name/alert
//	DELETE /api/v1/watchlist/{id}                 delete
//	GET    /api/v1/devices/{id}/logs?unit=&level=&limit=  a device's forwarded logs
//
// Everything is RS2 org-scoped: a scoped operator/viewer gets 404 not_found (never
// 403) for an item or device in an org it cannot see, before any side effect, so it
// cannot tell the resource exists. Writes require operator; GETs require viewer.
// The persistence is store.watchlist.go (WP-STORE); the service_down alert
// evaluator and the sync-handler integration (ComputeWatchForDevice,
// RecordSyncWatchData) are in watchlist_eval.go. Registered through dashboardRoutes
// like the other dashboard_*.go files.

func init() {
	dashboardRoutes = append(dashboardRoutes, registerWatchlist)
}

func registerWatchlist(s *Server, mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle("GET "+pattern, s.dashRoute(RoleViewer, h))
	}
	with := func(role, pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.dashRoute(role, h))
	}
	get("/api/v1/watchlist", s.handleListWatchlist)
	with(RoleOperator, "POST /api/v1/watchlist", s.handleCreateWatchlistItem)
	with(RoleOperator, "PUT /api/v1/watchlist/{id}", s.handleUpdateWatchlistItem)
	with(RoleOperator, "DELETE /api/v1/watchlist/{id}", s.handleDeleteWatchlistItem)
	get("/api/v1/devices/{id}/logs", s.handleDeviceLogs)
}

// watchlistRequest is the POST/PUT body. On POST, scope selects org_id (scope
// 'org') or device_id (scope 'device', its org is derived from the device); on PUT
// only kind/name/alert are read (scope/org/device are fixed at creation). alert is
// a pointer so an omitted value defaults to true on create and keeps the stored
// flag on update.
type watchlistRequest struct {
	Scope    string `json:"scope"`
	OrgID    *int64 `json:"org_id"`
	DeviceID *int64 `json:"device_id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Alert    *bool  `json:"alert"`
}

// handleListWatchlist is GET /api/v1/watchlist?org_id=&device_id= (viewer). The
// result is RS2-scoped to the caller's visible org set; an explicit ?org_id= or
// ?device_id= that the caller cannot see gets the same 404 as a non-existent one,
// before the list is computed.
func (s *Server) handleListWatchlist(w http.ResponseWriter, r *http.Request) {
	orgID, ok := dashOrgIDParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "org_id must be a positive integer")
		return
	}
	deviceID, ok := dashDeviceIDParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "device_id must be a positive integer")
		return
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return
	}
	if orgID != nil && !s.dashOrgVisible(w, r, *orgID) {
		return
	}
	if deviceID != nil {
		dev, err := s.st.GetDevice(r.Context(), *deviceID)
		if err != nil {
			s.dashStoreError(w, err, "get device")
			return
		}
		if !s.dashOrgVisible(w, r, dev.OrgID) {
			return
		}
	}
	items, err := s.st.ListWatchlistItems(r.Context(), store.WatchlistFilter{
		OrgID: orgID, DeviceID: deviceID, VisibleOrgs: visible,
	})
	if err != nil {
		s.log.Error("list watchlist", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleCreateWatchlistItem is POST /api/v1/watchlist (operator, CSRF). Validation
// order for RS2: shape (invalid_request) -> org/device visibility (404 across orgs,
// before any write) -> desktop platform for a device item (unsupported_on_platform,
// android is excluded from watchlists entirely).
func (s *Server) handleCreateWatchlistItem(w http.ResponseWriter, r *http.Request) {
	var req watchlistRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	// kind + name shape via the shared protocol check (kind in service|process,
	// name 1..MaxWatchNameLen, no control characters).
	if err := (protocol.WatchItem{Kind: req.Kind, Name: name}).Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	alert := true
	if req.Alert != nil {
		alert = *req.Alert
	}

	actor, _ := dashActor(r)
	ctx := r.Context()
	p := store.CreateWatchlistItemParams{Kind: req.Kind, Name: name, Alert: alert, CreatedBy: actor}

	switch req.Scope {
	case "org":
		if req.OrgID == nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "an org-scoped item requires org_id")
			return
		}
		if req.DeviceID != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "an org-scoped item must not set device_id")
			return
		}
		// 404 across orgs before any write (RS2).
		if !s.dashOrgVisible(w, r, *req.OrgID) {
			return
		}
		p.Scope = "org"
		p.OrgID = req.OrgID
	case "device":
		if req.DeviceID == nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "a device-scoped item requires device_id")
			return
		}
		if req.OrgID != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "a device-scoped item must not set org_id (it is derived from the device)")
			return
		}
		dev, err := s.st.GetDevice(ctx, *req.DeviceID)
		if err != nil {
			s.dashStoreError(w, err, "get device")
			return
		}
		if !s.dashOrgVisible(w, r, dev.OrgID) {
			return
		}
		// Watchlists are desktop only; android is excluded entirely (like the
		// agent and WatchlistForDevice), so a device item on android is 400
		// unsupported_on_platform.
		if dev.Platform == protocol.PlatformAndroid {
			writeError(w, http.StatusBadRequest, "unsupported_on_platform", "watchlists are not available on android")
			return
		}
		p.Scope = "device"
		orgID := dev.OrgID
		deviceID := dev.ID
		p.OrgID = &orgID
		p.DeviceID = &deviceID
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "scope must be org or device")
		return
	}

	item, err := s.st.CreateWatchlistItem(ctx, p)
	if err != nil {
		s.dashStoreError(w, err, "create watchlist item")
		return
	}
	s.audit(ctx, "user", actor, item.OrgID, "watchlist.created", "watchlist", &item.ID, clientIP(r),
		map[string]any{"scope": item.Scope, "kind": item.Kind, "name": item.Name, "alert": item.Alert})
	writeJSON(w, http.StatusCreated, map[string]any{"item": item})
}

// handleUpdateWatchlistItem is PUT /api/v1/watchlist/{id} (operator, CSRF). It
// loads the item first so an update across orgs 404s before any change (RS2);
// scope/org/device are fixed at creation, only kind/name/alert change.
func (s *Server) handleUpdateWatchlistItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := s.st.GetWatchlistItem(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get watchlist item")
		return
	}
	// 404 across orgs (nullable org handled) before any change (RS2).
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	var req watchlistRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := (protocol.WatchItem{Kind: req.Kind, Name: name}).Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	alert := existing.Alert
	if req.Alert != nil {
		alert = *req.Alert
	}
	if err := s.st.UpdateWatchlistItem(ctx, id, store.UpdateWatchlistItemParams{
		Kind: req.Kind, Name: name, Alert: alert,
	}); err != nil {
		s.dashStoreError(w, err, "update watchlist item")
		return
	}
	item, err := s.st.GetWatchlistItem(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get watchlist item")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, item.OrgID, "watchlist.updated", "watchlist", &item.ID, clientIP(r),
		map[string]any{"scope": item.Scope, "kind": item.Kind, "name": item.Name, "alert": item.Alert})
	writeJSON(w, http.StatusOK, map[string]any{"item": item})
}

// handleDeleteWatchlistItem is DELETE /api/v1/watchlist/{id} (operator, CSRF). It
// loads the item first so a delete across orgs 404s before the delete (RS2).
func (s *Server) handleDeleteWatchlistItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := s.st.GetWatchlistItem(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get watchlist item")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	if err := s.st.DeleteWatchlistItem(ctx, id); err != nil {
		s.dashStoreError(w, err, "delete watchlist item")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, existing.OrgID, "watchlist.deleted", "watchlist", &id, clientIP(r),
		map[string]any{"scope": existing.Scope, "kind": existing.Kind, "name": existing.Name})
	w.WriteHeader(http.StatusNoContent)
}

// handleDeviceLogs is GET /api/v1/devices/{id}/logs?unit=&level=&limit= (viewer).
// It 404s across orgs for a device the caller cannot see (RS2) before reading any
// log. The default limit is 200, capped at 2000.
func (s *Server) handleDeviceLogs(w http.ResponseWriter, r *http.Request) {
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
	filter := store.LogFilter{
		Unit:  strings.TrimSpace(r.URL.Query().Get("unit")),
		Level: strings.TrimSpace(r.URL.Query().Get("level")),
	}
	logs, err := s.st.ListLogs(ctx, dev.ID, filter, dashLimit(r, 200, 2000))
	if err != nil {
		s.log.Error("list device logs", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

// dashDeviceIDParam reads the optional ?device_id query value. ok is false when the
// value is present but not a positive integer.
func dashDeviceIDParam(r *http.Request) (id *int64, ok bool) {
	raw := r.URL.Query().Get("device_id")
	if raw == "" {
		return nil, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return nil, false
	}
	return &n, true
}
