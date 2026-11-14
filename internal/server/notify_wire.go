package server

import (
	"context"
	"time"

	"github.com/sxi9/msp/internal/store"
)

// NS1 integration wiring (the integrator's file). The notify HTTP handlers in
// dashboard_notify.go reach persistence through the NotifyStore interface and
// the handler-package row types, so that work package builds and tests on its
// own with a fake. This file adapts the real *store.Store to NotifyStore,
// converting the store rows to the handler row types (and never carrying a
// secret across the boundary -- a channel row exposes only SecretSet), installs
// the provider, and exposes the synchronous dispatch entry point the in-process
// e2e test drives (mirroring EvaluateNow).

func init() {
	notifyStoreProvider = func(s *Server) NotifyStore { return storeNotify{s: s} }
}

// storeNotify adapts *store.Store to the NotifyStore interface the notify
// handlers use.
type storeNotify struct{ s *Server }

// --- row conversions (store -> handler types) -------------------------------

func channelFromStore(c store.NotifyChannel) NotifyChannel {
	return NotifyChannel{
		ID: c.ID, Kind: c.Kind, Name: c.Name, OrgID: c.OrgID, ConfigJSON: c.ConfigJSON,
		SecretSet: c.SecretSet, Enabled: c.Enabled, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy,
	}
}

func ruleFromStore(r store.NotifyRule) NotifyRule {
	return NotifyRule{
		ID: r.ID, Name: r.Name, OrgID: r.OrgID, MinSeverity: r.MinSeverity, AlertTypes: r.AlertTypes,
		OnOpen: r.OnOpen, OnResolve: r.OnResolve, ChannelID: r.ChannelID, Enabled: r.Enabled,
		CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy,
	}
}

func windowFromStore(w store.MaintenanceWindow) MaintenanceWindow {
	return MaintenanceWindow{
		ID: w.ID, OrgID: w.OrgID, StartsAt: w.StartsAt, EndsAt: w.EndsAt, Reason: w.Reason,
		CreatedAt: w.CreatedAt, CreatedBy: w.CreatedBy,
	}
}

func deliveryFromStore(d store.NotificationDelivery) NotificationDelivery {
	var updated time.Time
	if d.UpdatedAt != nil {
		updated = *d.UpdatedAt
	}
	return NotificationDelivery{
		ID: d.ID, AlertID: d.AlertID, RuleID: d.RuleID, ChannelID: d.ChannelID, OrgID: d.OrgID,
		Event: d.Event, Status: d.Status, Attempts: d.Attempts, LastError: d.LastError,
		DetailJSON: d.DetailJSON, CreatedAt: d.CreatedAt, UpdatedAt: updated,
	}
}

// --- channels ---------------------------------------------------------------

func (n storeNotify) ListNotifyChannels(ctx context.Context, visible []int64) ([]NotifyChannel, error) {
	rows, err := n.s.st.ListChannels(ctx, visible)
	if err != nil {
		return nil, err
	}
	out := make([]NotifyChannel, 0, len(rows))
	for _, c := range rows {
		out = append(out, channelFromStore(c))
	}
	return out, nil
}

func (n storeNotify) GetNotifyChannel(ctx context.Context, id int64) (NotifyChannel, error) {
	c, err := n.s.st.GetChannel(ctx, id)
	if err != nil {
		return NotifyChannel{}, err
	}
	return channelFromStore(c), nil
}

func (n storeNotify) CreateNotifyChannel(ctx context.Context, p CreateNotifyChannelParams) (NotifyChannel, error) {
	c, err := n.s.st.CreateChannel(ctx, store.CreateChannelParams{
		Kind: p.Kind, Name: p.Name, OrgID: p.OrgID, ConfigJSON: p.ConfigJSON,
		Secret: p.Secret, Enabled: p.Enabled, CreatedBy: p.CreatedBy,
	})
	if err != nil {
		return NotifyChannel{}, err
	}
	return channelFromStore(c), nil
}

func (n storeNotify) UpdateNotifyChannel(ctx context.Context, p UpdateNotifyChannelParams) (NotifyChannel, error) {
	// org_id is immutable on update; keep the stored one (the store's UpdateChannel
	// always writes org_id, so it must be supplied).
	existing, err := n.s.st.GetChannel(ctx, p.ID)
	if err != nil {
		return NotifyChannel{}, err
	}
	c, err := n.s.st.UpdateChannel(ctx, p.ID, store.UpdateChannelParams{
		Name: p.Name, OrgID: existing.OrgID, ConfigJSON: p.ConfigJSON, Secret: p.Secret, Enabled: p.Enabled,
	})
	if err != nil {
		return NotifyChannel{}, err
	}
	return channelFromStore(c), nil
}

func (n storeNotify) DeleteNotifyChannel(ctx context.Context, id int64) error {
	return n.s.st.DeleteChannel(ctx, id)
}

func (n storeNotify) EnqueueTestDelivery(ctx context.Context, channelID int64, _ *int64) (NotificationDelivery, error) {
	ch, err := n.s.st.GetChannel(ctx, channelID)
	if err != nil {
		return NotificationDelivery{}, err
	}
	id, err := n.s.EnqueueTestDelivery(ctx, ch)
	if err != nil {
		return NotificationDelivery{}, err
	}
	d, err := n.s.st.GetDelivery(ctx, id)
	if err != nil {
		return NotificationDelivery{}, err
	}
	return deliveryFromStore(d), nil
}

// --- rules ------------------------------------------------------------------

func (n storeNotify) ListNotifyRules(ctx context.Context, visible []int64) ([]NotifyRule, error) {
	rows, err := n.s.st.ListRules(ctx, visible)
	if err != nil {
		return nil, err
	}
	out := make([]NotifyRule, 0, len(rows))
	for _, r := range rows {
		out = append(out, ruleFromStore(r))
	}
	return out, nil
}

func (n storeNotify) GetNotifyRule(ctx context.Context, id int64) (NotifyRule, error) {
	r, err := n.s.st.GetRule(ctx, id)
	if err != nil {
		return NotifyRule{}, err
	}
	return ruleFromStore(r), nil
}

func (n storeNotify) CreateNotifyRule(ctx context.Context, p CreateNotifyRuleParams) (NotifyRule, error) {
	r, err := n.s.st.CreateRule(ctx, store.CreateRuleParams{
		Name: p.Name, OrgID: p.OrgID, MinSeverity: p.MinSeverity, AlertTypes: p.AlertTypes,
		OnOpen: p.OnOpen, OnResolve: p.OnResolve, ChannelID: p.ChannelID, Enabled: p.Enabled, CreatedBy: p.CreatedBy,
	})
	if err != nil {
		return NotifyRule{}, err
	}
	return ruleFromStore(r), nil
}

func (n storeNotify) UpdateNotifyRule(ctx context.Context, p UpdateNotifyRuleParams) (NotifyRule, error) {
	// org_id is immutable on update; keep the stored one.
	existing, err := n.s.st.GetRule(ctx, p.ID)
	if err != nil {
		return NotifyRule{}, err
	}
	r, err := n.s.st.UpdateRule(ctx, p.ID, store.CreateRuleParams{
		Name: p.Name, OrgID: existing.OrgID, MinSeverity: p.MinSeverity, AlertTypes: p.AlertTypes,
		OnOpen: p.OnOpen, OnResolve: p.OnResolve, ChannelID: p.ChannelID, Enabled: p.Enabled,
	})
	if err != nil {
		return NotifyRule{}, err
	}
	return ruleFromStore(r), nil
}

func (n storeNotify) DeleteNotifyRule(ctx context.Context, id int64) error {
	return n.s.st.DeleteRule(ctx, id)
}

// --- maintenance windows ----------------------------------------------------

func (n storeNotify) ListMaintenanceWindows(ctx context.Context, visible []int64) ([]MaintenanceWindow, error) {
	rows, err := n.s.st.ListWindows(ctx, visible)
	if err != nil {
		return nil, err
	}
	out := make([]MaintenanceWindow, 0, len(rows))
	for _, w := range rows {
		out = append(out, windowFromStore(w))
	}
	return out, nil
}

func (n storeNotify) GetMaintenanceWindow(ctx context.Context, id int64) (MaintenanceWindow, error) {
	w, err := n.s.st.GetWindow(ctx, id)
	if err != nil {
		return MaintenanceWindow{}, err
	}
	return windowFromStore(w), nil
}

func (n storeNotify) CreateMaintenanceWindow(ctx context.Context, p CreateMaintenanceWindowParams) (MaintenanceWindow, error) {
	w, err := n.s.st.CreateWindow(ctx, store.CreateWindowParams{
		OrgID: p.OrgID, StartsAt: p.StartsAt, EndsAt: p.EndsAt, Reason: p.Reason, CreatedBy: p.CreatedBy,
	})
	if err != nil {
		return MaintenanceWindow{}, err
	}
	return windowFromStore(w), nil
}

func (n storeNotify) DeleteMaintenanceWindow(ctx context.Context, id int64) error {
	return n.s.st.DeleteWindow(ctx, id)
}

// --- delivery log -----------------------------------------------------------

func (n storeNotify) ListNotificationDeliveries(ctx context.Context, f DeliveryFilter) ([]NotificationDelivery, error) {
	rows, err := n.s.st.ListDeliveries(ctx, store.DeliveryFilter{
		Status: f.Status, OrgID: f.OrgID, VisibleOrgs: f.VisibleOrgs, Limit: f.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]NotificationDelivery, 0, len(rows))
	for _, d := range rows {
		out = append(out, deliveryFromStore(d))
	}
	return out, nil
}

// DispatchNotificationsNow runs one notification dispatch pass synchronously
// with the production sender (net/smtp + net/http). It mirrors EvaluateNow for
// the in-process e2e test; Run starts the dispatcher on its own ticker.
func DispatchNotificationsNow(s *Server) {
	NewNotifyDispatcher(s, newStdSender()).DispatchOnce(context.Background())
}
