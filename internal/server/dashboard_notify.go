package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// NS1 — the dashboard JSON API for alert notifications: channels, routing
// rules, maintenance windows and the delivery log, all under /api/v1/notify.
//
// This file owns ONLY the HTTP layer. The persistence (the notify_* tables and
// their methods) is a sibling work package that adds the methods below to
// *store.Store; the background dispatcher and the SMTP/HTTP renderers are
// further work packages. So that this package builds and tests on its own,
// before *store.Store carries those methods, the handlers reach the notify
// store through the NotifyStore interface, obtained from notifyStoreProvider.
// The integrator wires it once *store.Store implements NotifyStore:
//
//	notifyStoreProvider = func(s *Server) NotifyStore { return s.st }
//
// (see the drift note in this work package's report). Visibility (RS2) is still
// enforced with the real *store.Store through the existing dashOrgVisible /
// dashVisibleOrgs helpers, so the 404-not-403 rule is the same as everywhere.

// Channel kinds and the enums the API validates (schema CHECK constraints).
const (
	notifyKindEmail   = "email"
	notifyKindWebhook = "webhook"

	webhookFlavorSlack   = "slack"
	webhookFlavorGeneric = "generic"

	notifyMaxName   = 100
	notifyMaxReason = 500
)

// deliveryStatuses and deliveryEvents mirror the notification_deliveries CHECK
// constraints; the API validates the ?status= filter against the former.
var (
	deliveryStatuses = map[string]bool{"pending": true, "sent": true, "failed": true, "suppressed": true}
)

// notifyStoreProvider yields the NotifyStore backing a Server. It is a package
// variable so this work package compiles and tests on its own, before the store
// work package adds the notify methods to *store.Store. The integrator sets it
// in a file it owns; the tests set a fake. It is nil until wired, which the
// handlers report as a 500 rather than panicking.
var notifyStoreProvider func(s *Server) NotifyStore

// NotifyStore is the persistence the notify handlers need. The store work
// package makes *store.Store satisfy it. Every List takes the caller's visible
// org set (RS2): nil means all orgs (admin / all_orgs), a non-nil set keeps only
// rows whose org_id is in it — a global org_id-NULL row is therefore visible
// only to an all-orgs caller, exactly like the audit log.
type NotifyStore interface {
	ListNotifyChannels(ctx context.Context, visible []int64) ([]NotifyChannel, error)
	GetNotifyChannel(ctx context.Context, id int64) (NotifyChannel, error)
	CreateNotifyChannel(ctx context.Context, p CreateNotifyChannelParams) (NotifyChannel, error)
	UpdateNotifyChannel(ctx context.Context, p UpdateNotifyChannelParams) (NotifyChannel, error)
	DeleteNotifyChannel(ctx context.Context, id int64) error
	// EnqueueTestDelivery inserts a pending notification_deliveries row with
	// event "test" for the channel and returns it; the dispatcher sends it.
	EnqueueTestDelivery(ctx context.Context, channelID int64, actorID *int64) (NotificationDelivery, error)

	ListNotifyRules(ctx context.Context, visible []int64) ([]NotifyRule, error)
	GetNotifyRule(ctx context.Context, id int64) (NotifyRule, error)
	CreateNotifyRule(ctx context.Context, p CreateNotifyRuleParams) (NotifyRule, error)
	UpdateNotifyRule(ctx context.Context, p UpdateNotifyRuleParams) (NotifyRule, error)
	DeleteNotifyRule(ctx context.Context, id int64) error

	ListMaintenanceWindows(ctx context.Context, visible []int64) ([]MaintenanceWindow, error)
	GetMaintenanceWindow(ctx context.Context, id int64) (MaintenanceWindow, error)
	CreateMaintenanceWindow(ctx context.Context, p CreateMaintenanceWindowParams) (MaintenanceWindow, error)
	DeleteMaintenanceWindow(ctx context.Context, id int64) error

	ListNotificationDeliveries(ctx context.Context, f DeliveryFilter) ([]NotificationDelivery, error)
}

// --- store row types (what NotifyStore returns) ---------------------------
//
// The secret is never carried out of the store: a channel row reports only
// whether one is set (SecretSet). These types are defined here so this work
// package builds alone; the store work package may define matching types and
// the integrator reconciles the two (see drift note).

// NotifyChannel is a delivery channel. The secret (SMTP password / webhook
// signing secret) is write-only over the API and never leaves the store, so the
// row carries only SecretSet.
type NotifyChannel struct {
	ID         int64
	Kind       string
	Name       string
	OrgID      *int64
	ConfigJSON string
	SecretSet  bool
	Enabled    bool
	CreatedAt  time.Time
	CreatedBy  *int64
}

// NotifyRule routes matching alerts to a channel.
type NotifyRule struct {
	ID          int64
	Name        string
	OrgID       *int64
	MinSeverity string
	AlertTypes  string // comma list, empty = all types
	OnOpen      bool
	OnResolve   bool
	ChannelID   int64
	Enabled     bool
	CreatedAt   time.Time
	CreatedBy   *int64
}

// MaintenanceWindow suppresses deliveries for an org (or all orgs) between two
// absolute times.
type MaintenanceWindow struct {
	ID        int64
	OrgID     *int64
	StartsAt  time.Time
	EndsAt    time.Time
	Reason    string
	CreatedAt time.Time
	CreatedBy *int64
}

// NotificationDelivery is one row of the delivery log.
type NotificationDelivery struct {
	ID         int64
	AlertID    *int64
	RuleID     *int64
	ChannelID  *int64
	OrgID      *int64
	Event      string
	Status     string
	Attempts   int
	LastError  string
	DetailJSON string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// --- store params ---------------------------------------------------------

type CreateNotifyChannelParams struct {
	Kind       string
	Name       string
	OrgID      *int64
	ConfigJSON string
	Secret     string
	Enabled    bool
	CreatedBy  *int64
}

type UpdateNotifyChannelParams struct {
	ID         int64
	Kind       string
	Name       string
	ConfigJSON string
	Secret     *string // nil keeps the stored secret; non-nil replaces it
	Enabled    bool
}

type CreateNotifyRuleParams struct {
	Name        string
	OrgID       *int64
	MinSeverity string
	AlertTypes  string
	OnOpen      bool
	OnResolve   bool
	ChannelID   int64
	Enabled     bool
	CreatedBy   *int64
}

type UpdateNotifyRuleParams struct {
	ID          int64
	Name        string
	MinSeverity string
	AlertTypes  string
	OnOpen      bool
	OnResolve   bool
	ChannelID   int64
	Enabled     bool
}

type CreateMaintenanceWindowParams struct {
	OrgID     *int64
	StartsAt  time.Time
	EndsAt    time.Time
	Reason    string
	CreatedBy *int64
}

// DeliveryFilter narrows the delivery log. Status "" = all; VisibleOrgs is the
// RS2 set (nil = all orgs).
type DeliveryFilter struct {
	Status      string
	OrgID       *int64
	VisibleOrgs []int64
	Limit       int
}

func init() {
	// Follow the repo's route-registration pattern (dashboard.go): append so
	// server.go's Handler picks these up with no edit to a shared file.
	dashboardRoutes = append(dashboardRoutes, registerNotify)
}

// registerNotify registers every /api/v1/notify route. GETs require viewer;
// writes require operator (viewer is read-only). The per-row org-scope and the
// all-orgs rule for global rows are enforced inside each handler.
func registerNotify(s *Server, mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle("GET "+pattern, s.dashRoute(RoleViewer, h))
	}
	with := func(role, pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.dashRoute(role, h))
	}

	// Channels.
	get("/api/v1/notify/channels", s.handleListNotifyChannels)
	with(RoleOperator, "POST /api/v1/notify/channels", s.handleCreateNotifyChannel)
	with(RoleOperator, "PUT /api/v1/notify/channels/{id}", s.handleUpdateNotifyChannel)
	with(RoleOperator, "DELETE /api/v1/notify/channels/{id}", s.handleDeleteNotifyChannel)
	with(RoleOperator, "POST /api/v1/notify/channels/{id}/test", s.handleTestNotifyChannel)

	// Rules.
	get("/api/v1/notify/rules", s.handleListNotifyRules)
	with(RoleOperator, "POST /api/v1/notify/rules", s.handleCreateNotifyRule)
	with(RoleOperator, "PUT /api/v1/notify/rules/{id}", s.handleUpdateNotifyRule)
	with(RoleOperator, "DELETE /api/v1/notify/rules/{id}", s.handleDeleteNotifyRule)

	// Maintenance windows.
	get("/api/v1/notify/windows", s.handleListMaintenanceWindows)
	with(RoleOperator, "POST /api/v1/notify/windows", s.handleCreateMaintenanceWindow)
	with(RoleOperator, "DELETE /api/v1/notify/windows/{id}", s.handleDeleteMaintenanceWindow)

	// Delivery log (read-only).
	get("/api/v1/notify/deliveries", s.handleListDeliveries)
}

// notifyStore returns the wired NotifyStore, or writes a 500 and reports false
// when the integrator has not wired one yet.
func (s *Server) notifyStore(w http.ResponseWriter) (NotifyStore, bool) {
	if notifyStoreProvider == nil {
		s.log.Error("notify store not wired (notifyStoreProvider is nil)")
		writeError(w, http.StatusInternalServerError, "internal", "")
		return nil, false
	}
	return notifyStoreProvider(s), true
}

// --- RS2 helpers for nullable-org notify rows -----------------------------

// notifyRowVisible reports whether the caller may see a stored row with the
// given (nullable) org, writing 404 not_found when not — the same response an
// unrelated resource gives (RS2). A global row (orgID nil) is visible only to an
// all-orgs caller. Built on the existing dashOrgVisible / dashVisibleOrgs.
func (s *Server) notifyRowVisible(w http.ResponseWriter, r *http.Request, orgID *int64) bool {
	if orgID != nil {
		return s.dashOrgVisible(w, r, *orgID)
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return false
	}
	if visible != nil { // scoped user: cannot see a global row
		writeError(w, http.StatusNotFound, "not_found", "")
		return false
	}
	return true
}

// notifyCreateAllowed reports whether the caller may create a row in the given
// (nullable) org. A concrete org the caller cannot see is 404 (hidden like any
// cross-org resource); a global row (orgID nil) requires all-orgs, else 403.
func (s *Server) notifyCreateAllowed(w http.ResponseWriter, r *http.Request, orgID *int64) bool {
	if orgID != nil {
		return s.dashOrgVisible(w, r, *orgID)
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return false
	}
	if visible != nil {
		writeError(w, http.StatusForbidden, "forbidden", "creating a global row requires access to all clients")
		return false
	}
	return true
}

// notifyOrgVisibleQuiet reports visibility without writing a response; used to
// validate a rule's referenced channel without leaking whether it exists.
func (s *Server) notifyOrgVisibleQuiet(ctx context.Context, userID int64, orgID *int64) (bool, error) {
	ids, all, err := s.st.VisibleOrgIDs(ctx, userID)
	if err != nil {
		return false, err
	}
	if all {
		return true, nil
	}
	if orgID == nil {
		return false, nil
	}
	for _, id := range ids {
		if id == *orgID {
			return true, nil
		}
	}
	return false, nil
}

// --- views (API JSON) -----------------------------------------------------

type notifyChannelView struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	OrgID     *int64          `json:"org_id"`
	Config    json.RawMessage `json:"config"`
	SecretSet bool            `json:"secret_set"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
	CreatedBy *int64          `json:"created_by"`
}

func toChannelView(c NotifyChannel) notifyChannelView {
	cfg := json.RawMessage(c.ConfigJSON)
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	return notifyChannelView{
		ID: c.ID, Kind: c.Kind, Name: c.Name, OrgID: c.OrgID, Config: cfg,
		SecretSet: c.SecretSet, Enabled: c.Enabled, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy,
	}
}

type notifyRuleView struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	OrgID       *int64    `json:"org_id"`
	MinSeverity string    `json:"min_severity"`
	AlertTypes  []string  `json:"alert_types"`
	OnOpen      bool      `json:"on_open"`
	OnResolve   bool      `json:"on_resolve"`
	ChannelID   int64     `json:"channel_id"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   *int64    `json:"created_by"`
}

func toRuleView(ru NotifyRule) notifyRuleView {
	return notifyRuleView{
		ID: ru.ID, Name: ru.Name, OrgID: ru.OrgID, MinSeverity: ru.MinSeverity,
		AlertTypes: splitAlertTypes(ru.AlertTypes), OnOpen: ru.OnOpen, OnResolve: ru.OnResolve,
		ChannelID: ru.ChannelID, Enabled: ru.Enabled, CreatedAt: ru.CreatedAt, CreatedBy: ru.CreatedBy,
	}
}

type maintenanceWindowView struct {
	ID        int64     `json:"id"`
	OrgID     *int64    `json:"org_id"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Reason    string    `json:"reason"`
	ActiveNow bool      `json:"active_now"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy *int64    `json:"created_by"`
}

func (s *Server) toWindowView(wnd MaintenanceWindow) maintenanceWindowView {
	now := s.now()
	active := !now.Before(wnd.StartsAt) && now.Before(wnd.EndsAt)
	return maintenanceWindowView{
		ID: wnd.ID, OrgID: wnd.OrgID, StartsAt: wnd.StartsAt, EndsAt: wnd.EndsAt,
		Reason: wnd.Reason, ActiveNow: active, CreatedAt: wnd.CreatedAt, CreatedBy: wnd.CreatedBy,
	}
}

type deliveryView struct {
	ID        int64           `json:"id"`
	AlertID   *int64          `json:"alert_id"`
	RuleID    *int64          `json:"rule_id"`
	ChannelID *int64          `json:"channel_id"`
	OrgID     *int64          `json:"org_id"`
	Event     string          `json:"event"`
	Status    string          `json:"status"`
	Attempts  int             `json:"attempts"`
	LastError string          `json:"last_error"`
	Detail    json.RawMessage `json:"detail"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func toDeliveryView(d NotificationDelivery) deliveryView {
	detail := json.RawMessage(d.DetailJSON)
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	return deliveryView{
		ID: d.ID, AlertID: d.AlertID, RuleID: d.RuleID, ChannelID: d.ChannelID, OrgID: d.OrgID,
		Event: d.Event, Status: d.Status, Attempts: d.Attempts, LastError: d.LastError,
		Detail: detail, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

// --- channels -------------------------------------------------------------

type channelRequest struct {
	Kind    string          `json:"kind"`
	Name    string          `json:"name"`
	OrgID   *int64          `json:"org_id"`
	Config  json.RawMessage `json:"config"`
	Secret  *string         `json:"secret"`
	Enabled *bool           `json:"enabled"`
}

func (s *Server) handleListNotifyChannels(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	visible, vok := s.dashVisibleOrgs(w, r)
	if !vok {
		return
	}
	rows, err := ns.ListNotifyChannels(r.Context(), visible)
	if err != nil {
		s.log.Error("list notify channels", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	out := make([]notifyChannelView, 0, len(rows))
	for _, c := range rows {
		out = append(out, toChannelView(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out})
}

func (s *Server) handleCreateNotifyChannel(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	var req channelRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cfg, detail := validateChannel(req.Kind, strings.TrimSpace(req.Name), req.Config)
	if detail != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	if !s.notifyCreateAllowed(w, r, req.OrgID) {
		return
	}
	secret := ""
	if req.Secret != nil {
		secret = *req.Secret
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	actor, _ := dashActor(r)
	ctx := r.Context()
	c, err := ns.CreateNotifyChannel(ctx, CreateNotifyChannelParams{
		Kind: req.Kind, Name: strings.TrimSpace(req.Name), OrgID: req.OrgID,
		ConfigJSON: cfg, Secret: secret, Enabled: enabled, CreatedBy: actor,
	})
	if err != nil {
		s.dashStoreError(w, err, "create notify channel")
		return
	}
	s.audit(ctx, "user", actor, c.OrgID, "notify.channel_created", "notify_channel", &c.ID, clientIP(r),
		map[string]any{"kind": c.Kind, "name": c.Name})
	writeJSON(w, http.StatusCreated, map[string]any{"channel": toChannelView(c)})
}

func (s *Server) handleUpdateNotifyChannel(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	// Load first so an update across orgs 404s before any change (RS2).
	existing, err := ns.GetNotifyChannel(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get notify channel")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	var req channelRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cfg, detail := validateChannel(req.Kind, strings.TrimSpace(req.Name), req.Config)
	if detail != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	// Secret: omitting leaves it unchanged; "" is rejected; a value replaces it.
	if req.Secret != nil && *req.Secret == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "secret cannot be empty; omit it to keep the stored secret")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	c, err := ns.UpdateNotifyChannel(ctx, UpdateNotifyChannelParams{
		ID: id, Kind: req.Kind, Name: strings.TrimSpace(req.Name),
		ConfigJSON: cfg, Secret: req.Secret, Enabled: enabled,
	})
	if err != nil {
		s.dashStoreError(w, err, "update notify channel")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, c.OrgID, "notify.channel_updated", "notify_channel", &c.ID, clientIP(r),
		map[string]any{"kind": c.Kind, "name": c.Name, "secret_replaced": req.Secret != nil})
	writeJSON(w, http.StatusOK, map[string]any{"channel": toChannelView(c)})
}

func (s *Server) handleDeleteNotifyChannel(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := ns.GetNotifyChannel(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get notify channel")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	if err := ns.DeleteNotifyChannel(ctx, id); err != nil {
		s.dashStoreError(w, err, "delete notify channel")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, existing.OrgID, "notify.channel_deleted", "notify_channel", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestNotifyChannel(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := ns.GetNotifyChannel(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get notify channel")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	actor, _ := dashActor(r)
	d, err := ns.EnqueueTestDelivery(ctx, id, actor)
	if err != nil {
		s.dashStoreError(w, err, "enqueue test delivery")
		return
	}
	s.audit(ctx, "user", actor, existing.OrgID, "notify.channel_tested", "notify_channel", &id, clientIP(r),
		map[string]any{"delivery_id": d.ID})
	writeJSON(w, http.StatusCreated, map[string]any{"delivery": toDeliveryView(d)})
}

// validateChannel validates kind, name and the kind-specific config and returns
// the canonical compact config JSON, or a non-empty detail describing the first
// problem.
func validateChannel(kind, name string, raw json.RawMessage) (string, string) {
	if name == "" {
		return "", "name is required"
	}
	if len(name) > notifyMaxName {
		return "", "name exceeds 100 characters"
	}
	if len(raw) == 0 {
		return "", "config is required"
	}
	switch kind {
	case notifyKindEmail:
		var c struct {
			Host     string   `json:"host"`
			Port     int      `json:"port"`
			From     string   `json:"from"`
			To       []string `json:"to"`
			StartTLS bool     `json:"starttls"`
			Username string   `json:"username"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return "", "config is not a valid object"
		}
		if strings.TrimSpace(c.Host) == "" {
			return "", "config.host is required for an email channel"
		}
		if c.Port < 1 || c.Port > 65535 {
			return "", "config.port must be 1..65535"
		}
		if strings.TrimSpace(c.From) == "" {
			return "", "config.from is required for an email channel"
		}
		if len(c.To) == 0 {
			return "", "config.to must list at least one recipient"
		}
		for _, to := range c.To {
			if strings.TrimSpace(to) == "" {
				return "", "config.to entries cannot be empty"
			}
		}
		b, _ := json.Marshal(c)
		return string(b), ""
	case notifyKindWebhook:
		var c struct {
			URL    string `json:"url"`
			Flavor string `json:"flavor"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return "", "config is not a valid object"
		}
		if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
			return "", "config.url must be an http(s) URL"
		}
		if c.Flavor != webhookFlavorSlack && c.Flavor != webhookFlavorGeneric {
			return "", "config.flavor must be slack or generic"
		}
		b, _ := json.Marshal(c)
		return string(b), ""
	default:
		return "", "kind must be email or webhook"
	}
}

// --- rules ----------------------------------------------------------------

type ruleRequest struct {
	Name        string   `json:"name"`
	OrgID       *int64   `json:"org_id"`
	MinSeverity string   `json:"min_severity"`
	AlertTypes  []string `json:"alert_types"`
	OnOpen      *bool    `json:"on_open"`
	OnResolve   *bool    `json:"on_resolve"`
	ChannelID   int64    `json:"channel_id"`
	Enabled     *bool    `json:"enabled"`
}

func (s *Server) handleListNotifyRules(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	visible, vok := s.dashVisibleOrgs(w, r)
	if !vok {
		return
	}
	rows, err := ns.ListNotifyRules(r.Context(), visible)
	if err != nil {
		s.log.Error("list notify rules", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	out := make([]notifyRuleView, 0, len(rows))
	for _, ru := range rows {
		out = append(out, toRuleView(ru))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (s *Server) handleCreateNotifyRule(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	var req ruleRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	types, detail := validateRule(name, req.MinSeverity, req.AlertTypes, req.ChannelID)
	if detail != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	if !s.notifyCreateAllowed(w, r, req.OrgID) {
		return
	}
	ctx := r.Context()
	if !s.channelReferenceOK(w, r, req.ChannelID) {
		return
	}
	onOpen := true
	if req.OnOpen != nil {
		onOpen = *req.OnOpen
	}
	onResolve := false
	if req.OnResolve != nil {
		onResolve = *req.OnResolve
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	actor, _ := dashActor(r)
	ru, err := ns.CreateNotifyRule(ctx, CreateNotifyRuleParams{
		Name: name, OrgID: req.OrgID, MinSeverity: req.MinSeverity, AlertTypes: types,
		OnOpen: onOpen, OnResolve: onResolve, ChannelID: req.ChannelID, Enabled: enabled, CreatedBy: actor,
	})
	if err != nil {
		s.dashStoreError(w, err, "create notify rule")
		return
	}
	s.audit(ctx, "user", actor, ru.OrgID, "notify.rule_created", "notify_rule", &ru.ID, clientIP(r),
		map[string]any{"name": ru.Name, "channel_id": ru.ChannelID})
	writeJSON(w, http.StatusCreated, map[string]any{"rule": toRuleView(ru)})
}

func (s *Server) handleUpdateNotifyRule(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := ns.GetNotifyRule(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get notify rule")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	var req ruleRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	types, detail := validateRule(name, req.MinSeverity, req.AlertTypes, req.ChannelID)
	if detail != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	if !s.channelReferenceOK(w, r, req.ChannelID) {
		return
	}
	onOpen := true
	if req.OnOpen != nil {
		onOpen = *req.OnOpen
	}
	onResolve := false
	if req.OnResolve != nil {
		onResolve = *req.OnResolve
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	ru, err := ns.UpdateNotifyRule(ctx, UpdateNotifyRuleParams{
		ID: id, Name: name, MinSeverity: req.MinSeverity, AlertTypes: types,
		OnOpen: onOpen, OnResolve: onResolve, ChannelID: req.ChannelID, Enabled: enabled,
	})
	if err != nil {
		s.dashStoreError(w, err, "update notify rule")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, ru.OrgID, "notify.rule_updated", "notify_rule", &ru.ID, clientIP(r),
		map[string]any{"name": ru.Name, "channel_id": ru.ChannelID})
	writeJSON(w, http.StatusOK, map[string]any{"rule": toRuleView(ru)})
}

func (s *Server) handleDeleteNotifyRule(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := ns.GetNotifyRule(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get notify rule")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	if err := ns.DeleteNotifyRule(ctx, id); err != nil {
		s.dashStoreError(w, err, "delete notify rule")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, existing.OrgID, "notify.rule_deleted", "notify_rule", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

// channelReferenceOK reports whether the caller may attach a rule to channelID:
// the channel must exist and be visible to the caller. A missing or invisible
// channel is a 400 (not a 404) that does not reveal whether it exists in
// another org.
func (s *Server) channelReferenceOK(w http.ResponseWriter, r *http.Request, channelID int64) bool {
	ns, ok := s.notifyStore(w)
	if !ok {
		return false
	}
	u := userFrom(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return false
	}
	ch, err := ns.GetNotifyChannel(r.Context(), channelID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "channel_id not found")
		return false
	}
	vis, err := s.notifyOrgVisibleQuiet(r.Context(), u.ID, ch.OrgID)
	if err != nil {
		s.log.Error("channel reference visibility", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return false
	}
	if !vis {
		writeError(w, http.StatusBadRequest, "invalid_request", "channel_id not found")
		return false
	}
	return true
}

// validateRule validates the rule fields and returns the canonical comma list of
// alert types, or a non-empty detail.
func validateRule(name, minSeverity string, types []string, channelID int64) (string, string) {
	if name == "" {
		return "", "name is required"
	}
	if len(name) > notifyMaxName {
		return "", "name exceeds 100 characters"
	}
	switch minSeverity {
	case severityLow, severityMedium, severityHigh:
	default:
		return "", "min_severity must be low, medium or high"
	}
	if channelID <= 0 {
		return "", "channel_id is required"
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(types))
	for _, t := range types {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !knownAlertType(t) {
			return "", "unknown alert type: " + t
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		clean = append(clean, t)
	}
	return strings.Join(clean, ","), ""
}

func knownAlertType(t string) bool {
	for _, a := range alertTypes {
		if a == t {
			return true
		}
	}
	return false
}

func splitAlertTypes(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// --- maintenance windows --------------------------------------------------

type windowRequest struct {
	OrgID    *int64 `json:"org_id"`
	StartsAt string `json:"starts_at"`
	EndsAt   string `json:"ends_at"`
	Reason   string `json:"reason"`
}

func (s *Server) handleListMaintenanceWindows(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	visible, vok := s.dashVisibleOrgs(w, r)
	if !vok {
		return
	}
	rows, err := ns.ListMaintenanceWindows(r.Context(), visible)
	if err != nil {
		s.log.Error("list maintenance windows", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	out := make([]maintenanceWindowView, 0, len(rows))
	for _, wnd := range rows {
		out = append(out, s.toWindowView(wnd))
	}
	writeJSON(w, http.StatusOK, map[string]any{"windows": out})
}

func (s *Server) handleCreateMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	var req windowRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	starts, err := time.Parse(time.RFC3339, req.StartsAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "starts_at must be an RFC3339 timestamp")
		return
	}
	ends, err := time.Parse(time.RFC3339, req.EndsAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "ends_at must be an RFC3339 timestamp")
		return
	}
	if !ends.After(starts) {
		writeError(w, http.StatusBadRequest, "invalid_request", "ends_at must be after starts_at")
		return
	}
	if len(req.Reason) > notifyMaxReason {
		writeError(w, http.StatusBadRequest, "invalid_request", "reason exceeds 500 characters")
		return
	}
	if !s.notifyCreateAllowed(w, r, req.OrgID) {
		return
	}
	actor, _ := dashActor(r)
	ctx := r.Context()
	wnd, err := ns.CreateMaintenanceWindow(ctx, CreateMaintenanceWindowParams{
		OrgID: req.OrgID, StartsAt: starts.UTC(), EndsAt: ends.UTC(),
		Reason: strings.TrimSpace(req.Reason), CreatedBy: actor,
	})
	if err != nil {
		s.dashStoreError(w, err, "create maintenance window")
		return
	}
	s.audit(ctx, "user", actor, wnd.OrgID, "notify.window_created", "maintenance_window", &wnd.ID, clientIP(r),
		map[string]any{"starts_at": wnd.StartsAt.UTC().Format(time.RFC3339), "ends_at": wnd.EndsAt.UTC().Format(time.RFC3339)})
	writeJSON(w, http.StatusCreated, map[string]any{"window": s.toWindowView(wnd)})
}

func (s *Server) handleDeleteMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	existing, err := ns.GetMaintenanceWindow(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get maintenance window")
		return
	}
	if !s.notifyRowVisible(w, r, existing.OrgID) {
		return
	}
	if err := ns.DeleteMaintenanceWindow(ctx, id); err != nil {
		s.dashStoreError(w, err, "delete maintenance window")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, existing.OrgID, "notify.window_deleted", "maintenance_window", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

// --- delivery log ---------------------------------------------------------

func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	ns, ok := s.notifyStore(w)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && !deliveryStatuses[status] {
		writeError(w, http.StatusBadRequest, "invalid_request", "status must be pending, sent, failed or suppressed")
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
	rows, err := ns.ListNotificationDeliveries(r.Context(), DeliveryFilter{
		Status: status, OrgID: orgID, VisibleOrgs: visible, Limit: dashLimit(r, 200, 1000),
	})
	if err != nil {
		s.log.Error("list deliveries", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	out := make([]deliveryView, 0, len(rows))
	for _, d := range rows {
		out = append(out, toDeliveryView(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}
