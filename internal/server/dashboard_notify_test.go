package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/store"
)

// fakeNotifyStore is an in-memory NotifyStore for the handler tests. Visibility
// (RS2) is enforced by the handlers through the real *store.Store (users +
// user_orgs); this fake only persists the notify rows and applies the visible
// filter it is handed, so the tests exercise handler logic, not SQL.
type fakeNotifyStore struct {
	mu         sync.Mutex
	next       int64
	channels   map[int64]NotifyChannel
	secrets    map[int64]string // the stored secret, never exposed through the API
	rules      map[int64]NotifyRule
	windows    map[int64]MaintenanceWindow
	deliveries []NotificationDelivery
}

func newFakeNotifyStore() *fakeNotifyStore {
	return &fakeNotifyStore{
		next:     1,
		channels: map[int64]NotifyChannel{},
		secrets:  map[int64]string{},
		rules:    map[int64]NotifyRule{},
		windows:  map[int64]MaintenanceWindow{},
	}
}

func orgPtr(id int64) *int64 { return &id }

// orgVisibleToSet mirrors the store's org_id IN (visible) rule, including the
// "global rows are visible only to all-orgs callers" nuance.
func orgVisibleToSet(orgID *int64, visible []int64) bool {
	if visible == nil {
		return true
	}
	if orgID == nil {
		return false
	}
	for _, id := range visible {
		if id == *orgID {
			return true
		}
	}
	return false
}

func (f *fakeNotifyStore) seedChannel(c NotifyChannel, secret string) NotifyChannel {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == 0 {
		c.ID = f.next
		f.next++
	} else if c.ID >= f.next {
		f.next = c.ID + 1
	}
	c.SecretSet = secret != ""
	f.channels[c.ID] = c
	f.secrets[c.ID] = secret
	return c
}

func (f *fakeNotifyStore) secretOf(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secrets[id]
}

func (f *fakeNotifyStore) ListNotifyChannels(_ context.Context, visible []int64) ([]NotifyChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []NotifyChannel{}
	for _, c := range f.channels {
		if orgVisibleToSet(c.OrgID, visible) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeNotifyStore) GetNotifyChannel(_ context.Context, id int64) (NotifyChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.channels[id]
	if !ok {
		return NotifyChannel{}, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeNotifyStore) CreateNotifyChannel(_ context.Context, p CreateNotifyChannelParams) (NotifyChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := NotifyChannel{
		ID: f.next, Kind: p.Kind, Name: p.Name, OrgID: p.OrgID, ConfigJSON: p.ConfigJSON,
		SecretSet: p.Secret != "", Enabled: p.Enabled, CreatedAt: time.Now().UTC(), CreatedBy: p.CreatedBy,
	}
	f.next++
	f.channels[c.ID] = c
	f.secrets[c.ID] = p.Secret
	return c, nil
}

func (f *fakeNotifyStore) UpdateNotifyChannel(_ context.Context, p UpdateNotifyChannelParams) (NotifyChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.channels[p.ID]
	if !ok {
		return NotifyChannel{}, store.ErrNotFound
	}
	c.Kind = p.Kind
	c.Name = p.Name
	c.ConfigJSON = p.ConfigJSON
	c.Enabled = p.Enabled
	if p.Secret != nil { // nil keeps the stored secret
		f.secrets[p.ID] = *p.Secret
		c.SecretSet = *p.Secret != ""
	}
	f.channels[p.ID] = c
	return c, nil
}

func (f *fakeNotifyStore) DeleteNotifyChannel(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.channels[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.channels, id)
	delete(f.secrets, id)
	return nil
}

func (f *fakeNotifyStore) EnqueueTestDelivery(_ context.Context, channelID int64, _ *int64) (NotificationDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.channels[channelID]
	if !ok {
		return NotificationDelivery{}, store.ErrNotFound
	}
	d := NotificationDelivery{
		ID: f.next, ChannelID: &channelID, OrgID: c.OrgID, Event: "test", Status: "pending",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	f.next++
	f.deliveries = append(f.deliveries, d)
	return d, nil
}

func (f *fakeNotifyStore) seedRule(ru NotifyRule) NotifyRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ru.ID == 0 {
		ru.ID = f.next
		f.next++
	} else if ru.ID >= f.next {
		f.next = ru.ID + 1
	}
	f.rules[ru.ID] = ru
	return ru
}

func (f *fakeNotifyStore) ListNotifyRules(_ context.Context, visible []int64) ([]NotifyRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []NotifyRule{}
	for _, ru := range f.rules {
		if orgVisibleToSet(ru.OrgID, visible) {
			out = append(out, ru)
		}
	}
	return out, nil
}

func (f *fakeNotifyStore) GetNotifyRule(_ context.Context, id int64) (NotifyRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ru, ok := f.rules[id]
	if !ok {
		return NotifyRule{}, store.ErrNotFound
	}
	return ru, nil
}

func (f *fakeNotifyStore) CreateNotifyRule(_ context.Context, p CreateNotifyRuleParams) (NotifyRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ru := NotifyRule{
		ID: f.next, Name: p.Name, OrgID: p.OrgID, MinSeverity: p.MinSeverity, AlertTypes: p.AlertTypes,
		OnOpen: p.OnOpen, OnResolve: p.OnResolve, ChannelID: p.ChannelID, Enabled: p.Enabled,
		CreatedAt: time.Now().UTC(), CreatedBy: p.CreatedBy,
	}
	f.next++
	f.rules[ru.ID] = ru
	return ru, nil
}

func (f *fakeNotifyStore) UpdateNotifyRule(_ context.Context, p UpdateNotifyRuleParams) (NotifyRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ru, ok := f.rules[p.ID]
	if !ok {
		return NotifyRule{}, store.ErrNotFound
	}
	ru.Name = p.Name
	ru.MinSeverity = p.MinSeverity
	ru.AlertTypes = p.AlertTypes
	ru.OnOpen = p.OnOpen
	ru.OnResolve = p.OnResolve
	ru.ChannelID = p.ChannelID
	ru.Enabled = p.Enabled
	f.rules[p.ID] = ru
	return ru, nil
}

func (f *fakeNotifyStore) DeleteNotifyRule(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.rules, id)
	return nil
}

func (f *fakeNotifyStore) seedWindow(wnd MaintenanceWindow) MaintenanceWindow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if wnd.ID == 0 {
		wnd.ID = f.next
		f.next++
	} else if wnd.ID >= f.next {
		f.next = wnd.ID + 1
	}
	f.windows[wnd.ID] = wnd
	return wnd
}

func (f *fakeNotifyStore) ListMaintenanceWindows(_ context.Context, visible []int64) ([]MaintenanceWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []MaintenanceWindow{}
	for _, wnd := range f.windows {
		if orgVisibleToSet(wnd.OrgID, visible) {
			out = append(out, wnd)
		}
	}
	return out, nil
}

func (f *fakeNotifyStore) GetMaintenanceWindow(_ context.Context, id int64) (MaintenanceWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	wnd, ok := f.windows[id]
	if !ok {
		return MaintenanceWindow{}, store.ErrNotFound
	}
	return wnd, nil
}

func (f *fakeNotifyStore) CreateMaintenanceWindow(_ context.Context, p CreateMaintenanceWindowParams) (MaintenanceWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	wnd := MaintenanceWindow{
		ID: f.next, OrgID: p.OrgID, StartsAt: p.StartsAt, EndsAt: p.EndsAt, Reason: p.Reason,
		CreatedAt: time.Now().UTC(), CreatedBy: p.CreatedBy,
	}
	f.next++
	f.windows[wnd.ID] = wnd
	return wnd, nil
}

func (f *fakeNotifyStore) DeleteMaintenanceWindow(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.windows[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.windows, id)
	return nil
}

func (f *fakeNotifyStore) seedDelivery(d NotificationDelivery) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d.ID == 0 {
		d.ID = f.next
		f.next++
	}
	f.deliveries = append(f.deliveries, d)
}

func (f *fakeNotifyStore) ListNotificationDeliveries(_ context.Context, fl DeliveryFilter) ([]NotificationDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []NotificationDelivery{}
	for _, d := range f.deliveries {
		if !orgVisibleToSet(d.OrgID, fl.VisibleOrgs) {
			continue
		}
		if fl.Status != "" && d.Status != fl.Status {
			continue
		}
		if fl.OrgID != nil && (d.OrgID == nil || *d.OrgID != *fl.OrgID) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// installFakeNotify sets the package provider to return fake for the rest of the
// test and restores it afterwards.
func installFakeNotify(t *testing.T, fake *fakeNotifyStore) {
	t.Helper()
	prev := notifyStoreProvider
	notifyStoreProvider = func(*Server) NotifyStore { return fake }
	t.Cleanup(func() { notifyStoreProvider = prev })
}

func emailConfig() map[string]any {
	return map[string]any{
		"host": "smtp.example.com", "port": 587, "from": "alerts@example.com",
		"to": []string{"ops@example.com"}, "starttls": true, "username": "alerts",
	}
}

// TestNotifyChannelSecretWriteOnly covers the full channel CRUD plus the
// write-only secret rule: the secret is never returned, omitting it on PUT keeps
// it, "" is rejected, a value replaces it.
func TestNotifyChannelSecretWriteOnly(t *testing.T) {
	e := newTestEnv(t)
	fake := newFakeNotifyStore()
	installFakeNotify(t, fake)
	admin := dashAdmin(e)

	const secret = "smtp-s3cret-xyz-9999"
	body := map[string]any{
		"kind": "email", "name": "Ops email", "org_id": e.org.ID,
		"config": emailConfig(), "secret": secret,
	}
	resp, raw := e.do(http.MethodPost, "/api/v1/notify/channels", body, withCookie(admin, csrfHeader))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create channel: %d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("create response leaked the secret: %s", raw)
	}
	var created struct {
		Channel notifyChannelView `json:"channel"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := created.Channel.ID
	if !created.Channel.SecretSet {
		t.Fatalf("secret_set should be true: %s", raw)
	}
	if fake.secretOf(id) != secret {
		t.Fatalf("store did not receive the secret")
	}

	// GET list never returns the secret value, only secret_set.
	resp, raw = e.do(http.MethodGet, "/api/v1/notify/channels", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 || strings.Contains(string(raw), secret) {
		t.Fatalf("list leaked secret or bad status: %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"secret_set":true`) {
		t.Fatalf("list missing secret_set: %s", raw)
	}

	path := "/api/v1/notify/channels/" + itoa(id)

	// PUT without secret keeps the stored one.
	put := map[string]any{"kind": "email", "name": "Ops email 2", "config": emailConfig()}
	if resp, b := e.do(http.MethodPut, path, put, withCookie(admin, csrfHeader)); resp.StatusCode != 200 {
		t.Fatalf("update without secret: %d %s", resp.StatusCode, b)
	}
	if fake.secretOf(id) != secret {
		t.Fatalf("omitting secret on PUT changed it to %q", fake.secretOf(id))
	}

	// PUT with an empty secret is rejected.
	putEmpty := map[string]any{"kind": "email", "name": "Ops email 2", "config": emailConfig(), "secret": ""}
	if resp, b := e.do(http.MethodPut, path, putEmpty, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "invalid_request" {
		t.Fatalf("empty secret should be 400: %d %s", resp.StatusCode, b)
	}
	if fake.secretOf(id) != secret {
		t.Fatalf("rejected empty-secret PUT still changed the secret")
	}

	// PUT with a value replaces it.
	putNew := map[string]any{"kind": "email", "name": "Ops email 2", "config": emailConfig(), "secret": "new-secret-1234"}
	if resp, b := e.do(http.MethodPut, path, putNew, withCookie(admin, csrfHeader)); resp.StatusCode != 200 {
		t.Fatalf("update with new secret: %d %s", resp.StatusCode, b)
	}
	if fake.secretOf(id) != "new-secret-1234" {
		t.Fatalf("secret not replaced: %q", fake.secretOf(id))
	}

	// Invalid config is rejected.
	bad := map[string]any{"kind": "webhook", "name": "bad", "org_id": e.org.ID, "config": map[string]any{"url": "ftp://x", "flavor": "slack"}}
	if resp, b := e.do(http.MethodPost, "/api/v1/notify/channels", bad, withCookie(admin, csrfHeader)); resp.StatusCode != 400 {
		t.Fatalf("bad webhook url should be 400: %d %s", resp.StatusCode, b)
	}

	// DELETE.
	if resp, b := e.do(http.MethodDelete, path, nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("delete channel: %d %s", resp.StatusCode, b)
	}
}

// TestNotifyTestSendEnqueues checks POST /notify/channels/{id}/test enqueues one
// test delivery and returns its id.
func TestNotifyTestSendEnqueues(t *testing.T) {
	e := newTestEnv(t)
	fake := newFakeNotifyStore()
	installFakeNotify(t, fake)
	admin := dashAdmin(e)

	ch := fake.seedChannel(NotifyChannel{Kind: "webhook", Name: "Slack", OrgID: orgPtr(e.org.ID), Enabled: true}, "signsecret")

	resp, raw := e.do(http.MethodPost, "/api/v1/notify/channels/"+itoa(ch.ID)+"/test", nil, withCookie(admin, csrfHeader))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("test send: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		Delivery deliveryView `json:"delivery"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Delivery.ID == 0 || out.Delivery.Event != "test" {
		t.Fatalf("delivery not returned: %s", raw)
	}
	if n := len(fake.deliveries); n != 1 {
		t.Fatalf("want 1 enqueued delivery, got %d", n)
	}
	if fake.deliveries[0].Event != "test" || fake.deliveries[0].ChannelID == nil || *fake.deliveries[0].ChannelID != ch.ID {
		t.Fatalf("enqueued delivery wrong: %+v", fake.deliveries[0])
	}
	// The channel-tested action is audited.
	if n := len(e.auditActions("notify.channel_tested")); n != 1 {
		t.Fatalf("want 1 notify.channel_tested audit, got %d", n)
	}
}

// TestNotifyRoles checks viewer is read-only and operator may manage rows in a
// visible org.
func TestNotifyRoles(t *testing.T) {
	e := newTestEnv(t)
	fake := newFakeNotifyStore()
	installFakeNotify(t, fake)

	viewer := dashUser(e, "viewer@example.com", RoleViewer)
	operator := dashUser(e, "operator@example.com", RoleOperator)

	ch := fake.seedChannel(NotifyChannel{Kind: "email", Name: "C", OrgID: orgPtr(e.org.ID), Enabled: true}, "")

	// Viewer can read every list.
	for _, p := range []string{"/api/v1/notify/channels", "/api/v1/notify/rules", "/api/v1/notify/windows", "/api/v1/notify/deliveries"} {
		if resp, b := e.do(http.MethodGet, p, nil, withCookie(viewer, nil)); resp.StatusCode != 200 {
			t.Fatalf("viewer GET %s: %d %s", p, resp.StatusCode, b)
		}
	}
	// Viewer cannot write.
	chanBody := map[string]any{"kind": "email", "name": "V", "org_id": e.org.ID, "config": emailConfig()}
	if resp, b := e.do(http.MethodPost, "/api/v1/notify/channels", chanBody, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 || errorCode(t, b) != "forbidden" {
		t.Fatalf("viewer create channel: %d %s", resp.StatusCode, b)
	}
	ruleBody := map[string]any{"name": "R", "org_id": e.org.ID, "min_severity": "high", "channel_id": ch.ID, "on_open": true}
	if resp, _ := e.do(http.MethodPost, "/api/v1/notify/rules", ruleBody, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("viewer create rule: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodDelete, "/api/v1/notify/channels/"+itoa(ch.ID), nil, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("viewer delete channel: %d", resp.StatusCode)
	}
	// CSRF required even for operator.
	if resp, b := e.do(http.MethodPost, "/api/v1/notify/channels", chanBody, withCookie(operator, nil)); resp.StatusCode != 403 || errorCode(t, b) != "csrf" {
		t.Fatalf("operator without CSRF: %d %s", resp.StatusCode, b)
	}

	// Operator may create a rule in a visible org referencing a visible channel.
	resp, b := e.do(http.MethodPost, "/api/v1/notify/rules", ruleBody, withCookie(operator, csrfHeader))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("operator create rule: %d %s", resp.StatusCode, b)
	}
	var cr struct {
		Rule notifyRuleView `json:"rule"`
	}
	if json.Unmarshal(b, &cr) != nil || cr.Rule.MinSeverity != "high" || cr.Rule.OrgID == nil || *cr.Rule.OrgID != e.org.ID {
		t.Fatalf("created rule wrong: %s", b)
	}

	// A rule referencing an unknown channel is rejected.
	badRule := map[string]any{"name": "R2", "org_id": e.org.ID, "min_severity": "low", "channel_id": 999999}
	if resp, b := e.do(http.MethodPost, "/api/v1/notify/rules", badRule, withCookie(operator, csrfHeader)); resp.StatusCode != 400 {
		t.Fatalf("rule with unknown channel: %d %s", resp.StatusCode, b)
	}
	// An invalid severity is rejected.
	badSev := map[string]any{"name": "R3", "org_id": e.org.ID, "min_severity": "nope", "channel_id": ch.ID}
	if resp, _ := e.do(http.MethodPost, "/api/v1/notify/rules", badSev, withCookie(operator, csrfHeader)); resp.StatusCode != 400 {
		t.Fatalf("rule with bad severity should be 400: %d", resp.StatusCode)
	}
}

// TestNotifyOrgScoping is the RS2 proof for the notify endpoints: a scoped
// operator who sees only org A lists only A's rows (no org B, no global), gets a
// 404 on every per-row write in org B, cannot create a global row, while an
// all-orgs admin can. No secret is leaked in a scoped list.
func TestNotifyOrgScoping(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fake := newFakeNotifyStore()
	installFakeNotify(t, fake)
	orgA := e.org

	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	chA := fake.seedChannel(NotifyChannel{Kind: "email", Name: "A chan", OrgID: orgPtr(orgA.ID), Enabled: true}, "a-secret")
	chB := fake.seedChannel(NotifyChannel{Kind: "email", Name: "B chan", OrgID: orgPtr(orgB.ID), Enabled: true}, "b-secret-xyz")
	chG := fake.seedChannel(NotifyChannel{Kind: "webhook", Name: "Global", OrgID: nil, Enabled: true}, "global-secret")
	ruleB := fake.seedRule(NotifyRule{Name: "B rule", OrgID: orgPtr(orgB.ID), MinSeverity: "high", ChannelID: chB.ID, OnOpen: true, Enabled: true})
	winB := fake.seedWindow(MaintenanceWindow{OrgID: orgPtr(orgB.ID), StartsAt: e.clock.now(), EndsAt: e.clock.now().Add(time.Hour)})
	fake.seedDelivery(NotificationDelivery{ChannelID: &chA.ID, OrgID: orgPtr(orgA.ID), Event: "opened", Status: "sent"})
	fake.seedDelivery(NotificationDelivery{ChannelID: &chB.ID, OrgID: orgPtr(orgB.ID), Event: "opened", Status: "sent"})

	op, opID := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{orgA.ID})

	// --- scoped list shows only org A (no B, no global), no secret leaked -----
	resp, raw := e.do(http.MethodGet, "/api/v1/notify/channels", nil, withCookie(op, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("scoped channels: %d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "b-secret-xyz") || strings.Contains(string(raw), "global-secret") || strings.Contains(string(raw), "a-secret") {
		t.Fatalf("scoped list leaked a secret: %s", raw)
	}
	var chans struct {
		Channels []notifyChannelView `json:"channels"`
	}
	if json.Unmarshal(raw, &chans) != nil || len(chans.Channels) != 1 || chans.Channels[0].ID != chA.ID {
		t.Fatalf("scoped channels should be only A: %s", raw)
	}

	// Scoped deliveries show only org A.
	resp, raw = e.do(http.MethodGet, "/api/v1/notify/deliveries", nil, withCookie(op, nil))
	var dels struct {
		Deliveries []deliveryView `json:"deliveries"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &dels) != nil || len(dels.Deliveries) != 1 || dels.Deliveries[0].OrgID == nil || *dels.Deliveries[0].OrgID != orgA.ID {
		t.Fatalf("scoped deliveries should be only A: %d %s", resp.StatusCode, raw)
	}

	// --- 404 (no side effect) on every per-row write in org B ----------------
	type wr struct {
		method, path string
		body         any
	}
	writes := []wr{
		{http.MethodPut, "/api/v1/notify/channels/" + itoa(chB.ID), map[string]any{"kind": "email", "name": "x", "config": emailConfig()}},
		{http.MethodDelete, "/api/v1/notify/channels/" + itoa(chB.ID), nil},
		{http.MethodPost, "/api/v1/notify/channels/" + itoa(chB.ID) + "/test", nil},
		{http.MethodPut, "/api/v1/notify/rules/" + itoa(ruleB.ID), map[string]any{"name": "x", "min_severity": "high", "channel_id": chB.ID}},
		{http.MethodDelete, "/api/v1/notify/rules/" + itoa(ruleB.ID), nil},
		{http.MethodDelete, "/api/v1/notify/windows/" + itoa(winB.ID), nil},
	}
	for _, p := range writes {
		if resp, b := e.do(p.method, p.path, p.body, withCookie(op, csrfHeader)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
			t.Fatalf("scoped %s %s: %d %s (want 404 not_found)", p.method, p.path, resp.StatusCode, b)
		}
	}
	// No side effect: B's channel, rule, window and deliveries untouched.
	if _, ok := fake.channels[chB.ID]; !ok {
		t.Fatal("scoped request deleted org B channel")
	}
	if _, ok := fake.rules[ruleB.ID]; !ok {
		t.Fatal("scoped request deleted org B rule")
	}
	if _, ok := fake.windows[winB.ID]; !ok {
		t.Fatal("scoped request deleted org B window")
	}
	if len(fake.deliveries) != 2 {
		t.Fatalf("scoped test-send enqueued a delivery in org B: %d", len(fake.deliveries))
	}

	// Creating a channel in an invisible concrete org is 404.
	if resp, b := e.do(http.MethodPost, "/api/v1/notify/channels",
		map[string]any{"kind": "email", "name": "x", "org_id": orgB.ID, "config": emailConfig()},
		withCookie(op, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("scoped create in org B: %d %s", resp.StatusCode, b)
	}
	// Creating a global row (org_id null) requires all-orgs: 403 for a scoped op.
	if resp, b := e.do(http.MethodPost, "/api/v1/notify/channels",
		map[string]any{"kind": "email", "name": "g", "config": emailConfig()},
		withCookie(op, csrfHeader)); resp.StatusCode != 403 || errorCode(t, b) != "forbidden" {
		t.Fatalf("scoped create global: %d %s (want 403 forbidden)", resp.StatusCode, b)
	}

	// --- all-orgs admin is unaffected ---------------------------------------
	admin := dashAdmin(e)
	resp, raw = e.do(http.MethodGet, "/api/v1/notify/channels", nil, withCookie(admin, nil))
	if json.Unmarshal(raw, &chans) != nil || len(chans.Channels) != 3 {
		t.Fatalf("admin should see all 3 channels: %s", raw)
	}
	// Admin may create a global channel.
	if resp, b := e.do(http.MethodPost, "/api/v1/notify/channels",
		map[string]any{"kind": "webhook", "name": "g2", "config": map[string]any{"url": "https://hooks.example.com/x", "flavor": "generic"}},
		withCookie(admin, csrfHeader)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin create global: %d %s", resp.StatusCode, b)
	}
	// Admin may delete org B's channel.
	if resp, _ := e.do(http.MethodDelete, "/api/v1/notify/channels/"+itoa(chB.ID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("admin delete org B channel: %d", resp.StatusCode)
	}

	// --- grant org B; the operator now manages B ----------------------------
	if resp, b := e.do(http.MethodPut, "/api/v1/users/"+itoa(opID)+"/orgs",
		map[string]any{"all_orgs": false, "org_ids": []int64{orgA.ID, orgB.ID}}, withCookie(admin, csrfHeader)); resp.StatusCode != 200 {
		t.Fatalf("grant org B: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.do(http.MethodDelete, "/api/v1/notify/rules/"+itoa(ruleB.ID), nil, withCookie(op, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("after grant, scoped op delete B rule: %d", resp.StatusCode)
	}
	_ = chG
}

// TestNotifyWebhookSSRFRejected is the regression for the webhook SSRF finding:
// a webhook channel aimed at an internal/loopback/link-local/metadata address is
// rejected (400) at both create and update, while a public host is accepted.
func TestNotifyWebhookSSRFRejected(t *testing.T) {
	e := newTestEnv(t)
	fake := newFakeNotifyStore()
	installFakeNotify(t, fake)
	admin := dashAdmin(e)

	blocked := []string{
		"http://127.0.0.1:9000/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/hook",
		"http://192.168.1.10/hook",
		"http://[::1]:8080/hook",
		"https://localhost/hook",
		"http://metadata.google.internal/",
	}
	for _, u := range blocked {
		body := map[string]any{"kind": "webhook", "name": "x", "org_id": e.org.ID, "config": map[string]any{"url": u, "flavor": "generic"}}
		if resp, b := e.do(http.MethodPost, "/api/v1/notify/channels", body, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "invalid_request" {
			t.Fatalf("SSRF url %q should be 400 invalid_request: %d %s", u, resp.StatusCode, b)
		}
	}

	// A public webhook host is accepted.
	okBody := map[string]any{"kind": "webhook", "name": "ok", "org_id": e.org.ID, "config": map[string]any{"url": "https://hooks.example.com/services/x", "flavor": "slack"}}
	resp, b := e.do(http.MethodPost, "/api/v1/notify/channels", okBody, withCookie(admin, csrfHeader))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("public webhook should be created: %d %s", resp.StatusCode, b)
	}
	var created struct {
		Channel notifyChannelView `json:"channel"`
	}
	if err := json.Unmarshal(b, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// PUT to an internal URL is rejected on the same validation path.
	put := map[string]any{"kind": "webhook", "name": "ok", "config": map[string]any{"url": "http://127.0.0.1/hook", "flavor": "slack"}}
	if resp, pb := e.do(http.MethodPut, "/api/v1/notify/channels/"+itoa(created.Channel.ID), put, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, pb) != "invalid_request" {
		t.Fatalf("PUT to an internal url should be 400 invalid_request: %d %s", resp.StatusCode, pb)
	}
}

// TestNotifyChannelKindImmutable is the regression for the channel-kind finding:
// a PUT that changes the channel kind is rejected (400) and leaves the stored
// kind and config untouched, rather than silently dropping the kind change while
// replacing config_json with the other kind's shape.
func TestNotifyChannelKindImmutable(t *testing.T) {
	e := newTestEnv(t)
	fake := newFakeNotifyStore()
	installFakeNotify(t, fake)
	admin := dashAdmin(e)

	const cfg = `{"host":"smtp.x","port":587,"from":"a@x","to":["b@x"]}`
	ch := fake.seedChannel(NotifyChannel{Kind: "email", Name: "Ops", OrgID: orgPtr(e.org.ID), ConfigJSON: cfg, Enabled: true}, "")
	path := "/api/v1/notify/channels/" + itoa(ch.ID)

	// email -> webhook is rejected; the stored kind/config stay put.
	put := map[string]any{"kind": "webhook", "name": "Ops", "config": map[string]any{"url": "https://hooks.example.com/x", "flavor": "slack"}}
	if resp, b := e.do(http.MethodPut, path, put, withCookie(admin, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "invalid_request" {
		t.Fatalf("kind change should be 400 invalid_request: %d %s", resp.StatusCode, b)
	}
	got := fake.channels[ch.ID]
	if got.Kind != "email" {
		t.Fatalf("stored kind mutated to %q", got.Kind)
	}
	if got.ConfigJSON != cfg {
		t.Fatalf("stored config replaced with the other kind's shape: %s", got.ConfigJSON)
	}

	// A same-kind update still works.
	okPut := map[string]any{"kind": "email", "name": "Ops 2", "config": emailConfig()}
	if resp, b := e.do(http.MethodPut, path, okPut, withCookie(admin, csrfHeader)); resp.StatusCode != 200 {
		t.Fatalf("same-kind update should be 200: %d %s", resp.StatusCode, b)
	}
}
