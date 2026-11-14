package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// openAlertID opens an alert and returns its row id, for delivery tests that
// need a real alert_id (the alert_id FK is enforced).
func openAlertID(t *testing.T, s *Store, orgID, deviceID int64, typ, severity string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := s.OpenAlert(ctx, orgID, deviceID, typ, severity, "msg"); err != nil {
		t.Fatalf("OpenAlert: %v", err)
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM alerts WHERE device_id = ? AND type = ? AND resolved_at IS NULL`,
		deviceID, typ).Scan(&id); err != nil {
		t.Fatalf("alert id: %v", err)
	}
	return id
}

func i64(v int64) *int64 { return &v }

func TestChannelCRUD(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, admin, _, _ := fixture(t, s)

	ch, err := s.CreateChannel(ctx, CreateChannelParams{
		Kind: ChannelEmail, Name: "ops", OrgID: &org.ID,
		ConfigJSON: `{"host":"smtp.example.com"}`, Secret: "hunter2", Enabled: true, CreatedBy: &admin.ID,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if !ch.SecretSet || ch.Secret != "hunter2" || ch.Kind != ChannelEmail || ch.OrgID == nil || *ch.OrgID != org.ID {
		t.Fatalf("created channel = %+v", ch)
	}

	// Secret unchanged when not supplied.
	up, err := s.UpdateChannel(ctx, ch.ID, UpdateChannelParams{
		Name: "ops2", OrgID: &org.ID, ConfigJSON: `{"host":"smtp2"}`, Secret: nil, Enabled: false,
	})
	if err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	if up.Name != "ops2" || up.Secret != "hunter2" || up.Enabled {
		t.Fatalf("update left secret/fields wrong: %+v", up)
	}
	// Secret replaced when supplied.
	ns := "newpass"
	up, err = s.UpdateChannel(ctx, ch.ID, UpdateChannelParams{
		Name: "ops2", OrgID: &org.ID, ConfigJSON: `{"host":"smtp2"}`, Secret: &ns, Enabled: true,
	})
	if err != nil || up.Secret != "newpass" {
		t.Fatalf("update secret: %+v err %v", up, err)
	}

	got, err := s.GetChannel(ctx, ch.ID)
	if err != nil || got.Secret != "newpass" || !got.SecretSet {
		t.Fatalf("GetChannel: %+v err %v", got, err)
	}

	if err := s.DeleteChannel(ctx, ch.ID); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if _, err := s.GetChannel(ctx, ch.ID); err != ErrNotFound {
		t.Fatalf("after delete GetChannel err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteChannel(ctx, ch.ID); err != ErrNotFound {
		t.Fatalf("double delete err = %v, want ErrNotFound", err)
	}
}

func TestChannelAndRuleVisibility(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, admin, _, _ := fixture(t, s)
	orgB, err := s.CreateOrg(ctx, "Beta")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	chA, _ := s.CreateChannel(ctx, CreateChannelParams{Kind: ChannelEmail, Name: "a", OrgID: &orgA.ID, ConfigJSON: "{}", CreatedBy: &admin.ID})
	chB, _ := s.CreateChannel(ctx, CreateChannelParams{Kind: ChannelEmail, Name: "b", OrgID: &orgB.ID, ConfigJSON: "{}", CreatedBy: &admin.ID})
	chGlobal, _ := s.CreateChannel(ctx, CreateChannelParams{Kind: ChannelWebhook, Name: "g", OrgID: nil, ConfigJSON: "{}", CreatedBy: &admin.ID})

	// All-orgs caller (visible nil) sees everything, including the global row.
	all, err := s.ListChannels(ctx, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("ListChannels(nil) = %d, err %v", len(all), err)
	}
	// Scoped to A: sees only A (not B, not the global row).
	scoped, err := s.ListChannels(ctx, []int64{orgA.ID})
	if err != nil || len(scoped) != 1 || scoped[0].ID != chA.ID {
		t.Fatalf("ListChannels([A]) = %+v, err %v", scoped, err)
	}
	// Empty visible set sees nothing.
	none, _ := s.ListChannels(ctx, []int64{})
	if len(none) != 0 {
		t.Fatalf("ListChannels([]) = %d, want 0", len(none))
	}
	_ = chB
	_ = chGlobal

	// Rules inherit the same scoping.
	rA, err := s.CreateRule(ctx, CreateRuleParams{Name: "ra", OrgID: &orgA.ID, MinSeverity: "low", ChannelID: chA.ID, OnOpen: true, Enabled: true})
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if _, err := s.CreateRule(ctx, CreateRuleParams{Name: "rg", OrgID: nil, MinSeverity: "low", ChannelID: chGlobal.ID, OnOpen: true, Enabled: true}); err != nil {
		t.Fatalf("CreateRule global: %v", err)
	}
	scopedRules, _ := s.ListRules(ctx, []int64{orgA.ID})
	if len(scopedRules) != 1 || scopedRules[0].ID != rA.ID {
		t.Fatalf("ListRules([A]) = %+v", scopedRules)
	}
	allRules, _ := s.ListRules(ctx, nil)
	if len(allRules) != 2 {
		t.Fatalf("ListRules(nil) = %d, want 2", len(allRules))
	}

	// A bad channel id is ErrNotFound (foreign key).
	if _, err := s.CreateRule(ctx, CreateRuleParams{Name: "bad", MinSeverity: "low", ChannelID: 9999, OnOpen: true}); err != ErrNotFound {
		t.Fatalf("CreateRule bad channel err = %v, want ErrNotFound", err)
	}
}

func TestMatchRules(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, _, _, _ := fixture(t, s)
	orgB, _ := s.CreateOrg(ctx, "Beta")
	ch, _ := s.CreateChannel(ctx, CreateChannelParams{Kind: ChannelEmail, Name: "c", OrgID: &orgA.ID, ConfigJSON: "{}"})

	// High-severity, offline-only rule for org A, on open only.
	high, _ := s.CreateRule(ctx, CreateRuleParams{Name: "high", OrgID: &orgA.ID, MinSeverity: "high",
		AlertTypes: "offline", OnOpen: true, OnResolve: false, ChannelID: ch.ID, Enabled: true})
	// Global low-severity rule, all types, on open + resolve.
	global, _ := s.CreateRule(ctx, CreateRuleParams{Name: "glob", OrgID: nil, MinSeverity: "low",
		AlertTypes: "", OnOpen: true, OnResolve: true, ChannelID: ch.ID, Enabled: true})
	// Disabled rule never matches.
	s.CreateRule(ctx, CreateRuleParams{Name: "off", OrgID: &orgA.ID, MinSeverity: "low", OnOpen: true, ChannelID: ch.ID, Enabled: false})

	offlineHighA := Alert{ID: 1, OrgID: orgA.ID, Type: "offline", Severity: "high"}
	ids := ruleIDs(mustMatch(t, s, offlineHighA, EventOpened))
	if !has(ids, high.ID) || !has(ids, global.ID) || len(ids) != 2 {
		t.Fatalf("offline/high/A opened matched %v, want high+global", ids)
	}

	// Low alert does not meet the high rule's min_severity.
	lowA := Alert{ID: 2, OrgID: orgA.ID, Type: "offline", Severity: "low"}
	ids = ruleIDs(mustMatch(t, s, lowA, EventOpened))
	if has(ids, high.ID) || !has(ids, global.ID) || len(ids) != 1 {
		t.Fatalf("low/A opened matched %v, want global only", ids)
	}

	// disk_full does not match the offline-only high rule.
	diskA := Alert{ID: 3, OrgID: orgA.ID, Type: "disk_full", Severity: "high"}
	ids = ruleIDs(mustMatch(t, s, diskA, EventOpened))
	if has(ids, high.ID) || !has(ids, global.ID) {
		t.Fatalf("disk_full/high/A opened matched %v, want global only", ids)
	}

	// Org B sees only the global rule (A's rule is org-scoped to A).
	bAlert := Alert{ID: 4, OrgID: orgB.ID, Type: "offline", Severity: "high"}
	ids = ruleIDs(mustMatch(t, s, bAlert, EventOpened))
	if has(ids, high.ID) || !has(ids, global.ID) || len(ids) != 1 {
		t.Fatalf("B opened matched %v, want global only", ids)
	}

	// On resolve, only rules with on_resolve fire (global), not the high rule.
	ids = ruleIDs(mustMatch(t, s, offlineHighA, EventResolved))
	if has(ids, high.ID) || !has(ids, global.ID) || len(ids) != 1 {
		t.Fatalf("offline/high/A resolved matched %v, want global only", ids)
	}

	// A test event matches no routing rule.
	if got := mustMatch(t, s, offlineHighA, EventTest); len(got) != 0 {
		t.Fatalf("test event matched %d rules, want 0", len(got))
	}
}

func TestSeverityRank(t *testing.T) {
	if !(SeverityRank("low") < SeverityRank("medium") && SeverityRank("medium") < SeverityRank("high")) {
		t.Fatal("severity order")
	}
	if SeverityRank("bogus") >= SeverityRank("low") {
		t.Fatal("unknown severity should rank below low")
	}
}

func TestMaintenanceWindows(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	orgA, admin, _, _ := fixture(t, s)
	orgB, _ := s.CreateOrg(ctx, "Beta")
	now := c.now()

	// Active window for org A: [now-1h, now+1h].
	wA, err := s.CreateWindow(ctx, CreateWindowParams{OrgID: &orgA.ID, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Reason: "patch", CreatedBy: &admin.ID})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	active, err := s.ActiveMaintenance(ctx, orgA.ID, now)
	if err != nil || !active {
		t.Fatalf("A active = %v, err %v, want true", active, err)
	}
	// Org B not covered by an A-scoped window.
	if active, _ := s.ActiveMaintenance(ctx, orgB.ID, now); active {
		t.Fatal("B should not be in A's window")
	}
	// Before and after the window: not active.
	if active, _ := s.ActiveMaintenance(ctx, orgA.ID, now.Add(-2*time.Hour)); active {
		t.Fatal("A should not be active 2h before")
	}
	if active, _ := s.ActiveMaintenance(ctx, orgA.ID, now.Add(2*time.Hour)); active {
		t.Fatal("A should not be active 2h after")
	}

	// Global window covers every org.
	if _, err := s.CreateWindow(ctx, CreateWindowParams{OrgID: nil, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("CreateWindow global: %v", err)
	}
	if active, _ := s.ActiveMaintenance(ctx, orgB.ID, now); !active {
		t.Fatal("B should be covered by the global window")
	}

	// List + visibility + delete.
	scoped, _ := s.ListWindows(ctx, []int64{orgA.ID})
	if len(scoped) != 1 || scoped[0].ID != wA.ID {
		t.Fatalf("ListWindows([A]) = %+v", scoped)
	}
	if err := s.DeleteWindow(ctx, wA.ID); err != nil {
		t.Fatalf("DeleteWindow: %v", err)
	}
	if err := s.DeleteWindow(ctx, wA.ID); err != ErrNotFound {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
}

func TestEnqueueDeliveryIdempotent(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, admin, dev, _ := fixture(t, s)
	ch, _ := s.CreateChannel(ctx, CreateChannelParams{Kind: ChannelEmail, Name: "c", OrgID: &org.ID, ConfigJSON: "{}", CreatedBy: &admin.ID})
	rule, _ := s.CreateRule(ctx, CreateRuleParams{Name: "r", OrgID: &org.ID, MinSeverity: "low", OnOpen: true, ChannelID: ch.ID, Enabled: true})
	alertID := openAlertID(t, s, org.ID, dev.ID, "offline", "high")

	p := EnqueueDeliveryParams{AlertID: &alertID, RuleID: i64(rule.ID), ChannelID: i64(ch.ID), OrgID: &org.ID, Event: EventOpened}
	d1, created1, err := s.EnqueueDelivery(ctx, p)
	if err != nil || !created1 {
		t.Fatalf("first enqueue created=%v err=%v", created1, err)
	}
	// Same (alert, rule, event): no new row, same id returned.
	d2, created2, err := s.EnqueueDelivery(ctx, p)
	if err != nil || created2 || d2.ID != d1.ID {
		t.Fatalf("second enqueue created=%v id=%d (want id %d) err=%v", created2, d2.ID, d1.ID, err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM notification_deliveries`).Scan(&n)
	if n != 1 {
		t.Fatalf("delivery rows = %d, want 1 (idempotent)", n)
	}

	// Each test delivery (alert_id NULL) is its own row.
	tp := EnqueueDeliveryParams{ChannelID: i64(ch.ID), OrgID: &org.ID, Event: EventTest}
	if _, c1, _ := s.EnqueueDelivery(ctx, tp); !c1 {
		t.Fatal("first test enqueue not created")
	}
	if _, c2, _ := s.EnqueueDelivery(ctx, tp); !c2 {
		t.Fatal("second test enqueue should also create a row")
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM notification_deliveries WHERE event = 'test'`).Scan(&n)
	if n != 2 {
		t.Fatalf("test delivery rows = %d, want 2", n)
	}
}

func TestClaimAndMarkDeliveries(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, _, dev, _ := fixture(t, s)
	ch, _ := s.CreateChannel(ctx, CreateChannelParams{Kind: ChannelEmail, Name: "c", OrgID: &org.ID, ConfigJSON: "{}"})
	rule, _ := s.CreateRule(ctx, CreateRuleParams{Name: "r", OrgID: &org.ID, MinSeverity: "low", OnOpen: true, ChannelID: ch.ID, Enabled: true})
	alertID := openAlertID(t, s, org.ID, dev.ID, "offline", "high")
	del, _, err := s.EnqueueDelivery(ctx, EnqueueDeliveryParams{AlertID: &alertID, RuleID: i64(rule.ID), ChannelID: i64(ch.ID), OrgID: &org.ID, Event: EventOpened})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	backoff := 30 * time.Second
	retryBefore := c.now().Add(-backoff)

	// First claim gets the new (never-leased) row.
	claimed, err := s.ClaimPendingDeliveries(ctx, 10, retryBefore)
	if err != nil || len(claimed) != 1 || claimed[0].ID != del.ID {
		t.Fatalf("first claim = %+v err %v", claimed, err)
	}
	// An immediate second claim does not re-grab it (lease: updated_at = now).
	again, _ := s.ClaimPendingDeliveries(ctx, 10, c.now().Add(-backoff))
	if len(again) != 0 {
		t.Fatalf("immediate re-claim = %d, want 0 (leased)", len(again))
	}
	// After the backoff elapses it is claimable again.
	c.advance(backoff + time.Second)
	again, _ = s.ClaimPendingDeliveries(ctx, 10, c.now().Add(-backoff))
	if len(again) != 1 {
		t.Fatalf("post-backoff re-claim = %d, want 1", len(again))
	}

	// Mark failed twice (non-terminal) then terminal.
	if err := s.MarkDeliveryFailed(ctx, del.ID, "boom", false); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, _ := s.GetDelivery(ctx, del.ID)
	if got.Attempts != 1 || got.Status != DeliveryPending || got.LastError != "boom" {
		t.Fatalf("after one failure = %+v", got)
	}
	if err := s.MarkDeliveryFailed(ctx, del.ID, "boom2", true); err != nil {
		t.Fatalf("mark failed terminal: %v", err)
	}
	got, _ = s.GetDelivery(ctx, del.ID)
	if got.Attempts != 2 || got.Status != DeliveryFailed || got.LastError != "boom2" {
		t.Fatalf("after terminal failure = %+v", got)
	}
	// A failed row is terminal: marking it again touches nothing.
	if err := s.MarkDeliveryFailed(ctx, del.ID, "x", false); err != ErrNotFound {
		t.Fatalf("mark failed on terminal row = %v, want ErrNotFound", err)
	}

	// A fresh delivery can be marked sent (only while pending).
	alert2 := openAlertID(t, s, org.ID, dev.ID, "disk_full", "medium")
	d2, _, _ := s.EnqueueDelivery(ctx, EnqueueDeliveryParams{AlertID: &alert2, RuleID: i64(rule.ID), ChannelID: i64(ch.ID), OrgID: &org.ID, Event: EventOpened})
	if err := s.MarkDeliverySent(ctx, d2.ID, `{"kind":"email"}`); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	got, _ = s.GetDelivery(ctx, d2.ID)
	if got.Status != DeliverySent || got.DetailJSON != `{"kind":"email"}` {
		t.Fatalf("after sent = %+v", got)
	}
}

// TestClaimConcurrent asserts two concurrent claims partition the pending rows:
// no row is handed to both (the immediate write transaction serialises them and
// the lease on updated_at excludes an already-claimed row).
func TestClaimConcurrent(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, _, dev, _ := fixture(t, s)
	ch, _ := s.CreateChannel(ctx, CreateChannelParams{Kind: ChannelEmail, Name: "c", OrgID: &org.ID, ConfigJSON: "{}"})
	rule, _ := s.CreateRule(ctx, CreateRuleParams{Name: "r", OrgID: &org.ID, MinSeverity: "low", OnOpen: true, ChannelID: ch.ID, Enabled: true})

	const n = 20
	for i := 0; i < n; i++ {
		typ := "offline"
		if i%2 == 1 {
			typ = "disk_full"
		}
		// Distinct alerts so the idempotency key differs per row.
		aid := openAlertID(t, s, org.ID, dev.ID, typ, "high")
		// Resolve immediately so the next loop can re-open the same type as a new alert id.
		if _, _, err := s.EnqueueDelivery(ctx, EnqueueDeliveryParams{AlertID: &aid, RuleID: i64(rule.ID), ChannelID: i64(ch.ID), OrgID: &org.ID, Event: EventOpened}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := s.ResolveAlert(ctx, dev.ID, typ); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	var pending int
	s.db.QueryRow(`SELECT COUNT(*) FROM notification_deliveries WHERE status='pending'`).Scan(&pending)
	if pending != n {
		t.Fatalf("pending = %d, want %d", pending, n)
	}

	retryBefore := c.now().Add(-30 * time.Second)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		seen    = map[int64]int{}
		total   int
		results [2][]NotificationDelivery
	)
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			cl, err := s.ClaimPendingDeliveries(ctx, n, retryBefore)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			results[g] = cl
			for _, d := range cl {
				seen[d.ID]++
				total++
			}
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	if total != n {
		t.Fatalf("claimed total = %d, want %d", total, n)
	}
	for id, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("delivery %d claimed %d times (want exactly once)", id, cnt)
		}
	}
}

func TestListDeliveries(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, _, devA, _ := fixture(t, s)
	orgB, _ := s.CreateOrg(ctx, "Beta")
	devB, _, err := s.UpsertDevice(ctx, UpsertDeviceParams{OrgID: orgB.ID, DeviceUID: "ub", Hostname: "wb", Platform: "linux", IntervalS: 30, FactsJSON: "{}"})
	if err != nil {
		t.Fatalf("UpsertDevice B: %v", err)
	}
	ch, _ := s.CreateChannel(ctx, CreateChannelParams{Kind: ChannelEmail, Name: "c", ConfigJSON: "{}"})
	rule, _ := s.CreateRule(ctx, CreateRuleParams{Name: "r", MinSeverity: "low", OnOpen: true, ChannelID: ch.ID, Enabled: true})

	aA := openAlertID(t, s, orgA.ID, devA.ID, "offline", "high")
	aB := openAlertID(t, s, orgB.ID, devB.ID, "offline", "high")
	s.EnqueueDelivery(ctx, EnqueueDeliveryParams{AlertID: &aA, RuleID: i64(rule.ID), ChannelID: i64(ch.ID), OrgID: &orgA.ID, Event: EventOpened})
	dB, _, _ := s.EnqueueDelivery(ctx, EnqueueDeliveryParams{AlertID: &aB, RuleID: i64(rule.ID), ChannelID: i64(ch.ID), OrgID: &orgB.ID, Event: EventOpened})
	s.MarkDeliverySent(ctx, dB.ID, "{}")

	// Scoped to A: only A's delivery.
	got, err := s.ListDeliveries(ctx, DeliveryFilter{VisibleOrgs: []int64{orgA.ID}})
	if err != nil || len(got) != 1 || got[0].OrgID == nil || *got[0].OrgID != orgA.ID {
		t.Fatalf("ListDeliveries([A]) = %+v err %v", got, err)
	}
	// All-orgs, filtered by status sent: only B's.
	got, _ = s.ListDeliveries(ctx, DeliveryFilter{Status: DeliverySent})
	if len(got) != 1 || got[0].ID != dB.ID {
		t.Fatalf("ListDeliveries(sent) = %+v", got)
	}
	// All-orgs, no filter: both.
	got, _ = s.ListDeliveries(ctx, DeliveryFilter{})
	if len(got) != 2 {
		t.Fatalf("ListDeliveries(all) = %d, want 2", len(got))
	}
}

// --- test helpers ---

func mustMatch(t *testing.T, s *Store, a Alert, event string) []NotifyRule {
	t.Helper()
	rules, err := s.MatchRules(context.Background(), a, event)
	if err != nil {
		t.Fatalf("MatchRules: %v", err)
	}
	return rules
}

func ruleIDs(rules []NotifyRule) []int64 {
	ids := make([]int64, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
	}
	return ids
}

func has(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
