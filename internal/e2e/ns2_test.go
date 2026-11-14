package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// NS2 end-to-end proof (SMS channel + multi-step escalation policies), driven
// through the real evaluator and the production dispatcher (net/http SMS sender)
// against an httptest SMS provider, on the server fake clock. No real SMS is
// ever sent; escalation timing is driven entirely by advancing the fake clock.
//
// TestNS2Delivery covers the spec Proof bullets 1-4: an SMS channel+rule yields a
// sent SMS carrying the short message with the token never leaked; a 2-step
// policy delivers step 1 on open and step 2 after the clock passes its delay
// while the alert is open+unacked; an acked alert gets no step 2; a maintenance
// window suppresses the steps; and the escalation tick running twice never
// double-sends a step. TestNS2OrgScoping covers bullet 5 (RS2 404 on a
// policy/SMS channel in another org).

// --- httptest SMS provider ---------------------------------------------------

// smsRecord is one received provider request. For a generic channel the body is
// JSON {to,text}; for a twilio channel it is a form (To, Body) with HTTP basic
// auth. auth is the raw Authorization header so a test can prove the token rode
// only there (never in the body, the delivery detail or a log).
type smsRecord struct {
	provider string
	auth     string
	to       string
	text     string
	raw      string
}

type smsSink struct {
	mu      sync.Mutex
	records []smsRecord
	status  int // response status; 0 means 200
}

func (s *smsSink) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := readAll(r)
	rec := smsRecord{auth: r.Header.Get("Authorization"), raw: string(body)}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		rec.provider = "generic"
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		rec.to, rec.text = m["to"], m["text"]
	} else {
		rec.provider = "twilio"
		vals, _ := url.ParseQuery(string(body))
		rec.to, rec.text = vals.Get("To"), vals.Get("Body")
	}
	s.mu.Lock()
	s.records = append(s.records, rec)
	st := s.status
	s.mu.Unlock()
	if st == 0 {
		st = http.StatusOK
	}
	w.WriteHeader(st)
}

// forNumber returns the records delivered to one recipient number.
func (s *smsSink) forNumber(num string) []smsRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []smsRecord
	for _, r := range s.records {
		if r.to == num {
			out = append(out, r)
		}
	}
	return out
}

func (s *smsSink) all() []smsRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]smsRecord(nil), s.records...)
}

// TestNS2Delivery is the NS2 Proof, bullets 1-4, through the real evaluator and
// the production dispatcher against an httptest SMS provider on a fake clock.
func TestNS2Delivery(t *testing.T) {
	ctx := context.Background()
	st, clk := newNotifyStore(t)
	logs := &logBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})
	srv := server.New(st, server.Config{SampleEvery: time.Second, LostGrace: time.Second, Version: agentVersion}, logger)

	admin, err := st.CreateUser(ctx, adminEmail, mustHash(t), "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	sink := &smsSink{}
	smsSrv := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer smsSrv.Close()

	// Channel builders (seeded directly in the store so a twilio channel can keep
	// its httptest url override; the dashboard canonicalises that away, but the
	// dispatcher reads config_json verbatim).
	mkGenericSMS := func(orgID int64, name, to, token string) store.NotifyChannel {
		cfg := fmt.Sprintf(`{"provider":"generic","from":"+15559990000","to":[%q],"url":%q}`, to, smsSrv.URL)
		ch, err := st.CreateChannel(ctx, store.CreateChannelParams{
			Kind: store.ChannelSMS, Name: name, OrgID: &orgID, ConfigJSON: cfg, Secret: token, Enabled: true, CreatedBy: &admin.ID,
		})
		if err != nil {
			t.Fatalf("create sms channel %s: %v", name, err)
		}
		return ch
	}
	mkTwilioSMS := func(orgID int64, name, to, token string) store.NotifyChannel {
		cfg := fmt.Sprintf(`{"provider":"twilio","from":"+15559990000","to":[%q],"account_sid":"ACtest","url":%q}`, to, smsSrv.URL)
		ch, err := st.CreateChannel(ctx, store.CreateChannelParams{
			Kind: store.ChannelSMS, Name: name, OrgID: &orgID, ConfigJSON: cfg, Secret: token, Enabled: true, CreatedBy: &admin.ID,
		})
		if err != nil {
			t.Fatalf("create sms channel %s: %v", name, err)
		}
		return ch
	}
	mkPolicyRule := func(orgID int64, immediate store.NotifyChannel, chA, chB store.NotifyChannel) {
		pol, err := st.CreatePolicy(ctx, store.CreatePolicyParams{
			OrgID: &orgID, Name: "escalation", CreatedBy: &admin.ID,
			Steps: []store.CreatePolicyStep{
				{AfterMinutes: 0, ChannelIDs: []int64{chA.ID}},
				{AfterMinutes: 15, ChannelIDs: []int64{chB.ID}},
			},
		})
		if err != nil {
			t.Fatalf("create policy org %d: %v", orgID, err)
		}
		if _, err := st.CreateRule(ctx, store.CreateRuleParams{
			Name: "esc-rule", OrgID: &orgID, MinSeverity: "low", OnOpen: true, OnResolve: false,
			ChannelID: immediate.ID, EscalationPolicyID: &pol.ID, Enabled: true, CreatedBy: &admin.ID,
		}); err != nil {
			t.Fatalf("create esc rule org %d: %v", orgID, err)
		}
	}

	// Recipient numbers (one per channel, so the sink can be filtered per role).
	const (
		toSMS      = "+15550000001"
		toEscImm   = "+15550000010"
		toEscA     = "+15550000011"
		toEscB     = "+15550000012"
		toAckImm   = "+15550000020"
		toAckA     = "+15550000021"
		toAckB     = "+15550000022"
		toMaintImm = "+15550000030"
		toMaintA   = "+15550000031"
		toMaintB   = "+15550000032"

		tokSMS   = "tok-sms-SECRETaaa"
		tokEsc   = "tok-esc-SECRETbbb"
		tokAck   = "tok-ack-SECRETccc"
		tokMaint = "tok-maint-SECRETddd"
	)

	// org SMS: a plain SMS channel + rule, no policy (bullet 1). Twilio provider so
	// the basic-auth path is exercised too.
	orgSMS, _ := st.CreateOrg(ctx, "Acme SMS")
	devSMS := mkDevice(t, st, orgSMS.ID, "uid-sms", "acme-ws-sms")
	chSMS := mkTwilioSMS(orgSMS.ID, "oncall-sms", toSMS, tokSMS)
	if _, err := st.CreateRule(ctx, store.CreateRuleParams{
		Name: "sms-low", OrgID: &orgSMS.ID, MinSeverity: "low", OnOpen: true, ChannelID: chSMS.ID, Enabled: true, CreatedBy: &admin.ID,
	}); err != nil {
		t.Fatalf("create sms rule: %v", err)
	}

	// org ESC: a 2-step policy, alert left open+unacked (bullets 2 open/step1/step2).
	orgEsc, _ := st.CreateOrg(ctx, "Beta Escalate")
	devEsc := mkDevice(t, st, orgEsc.ID, "uid-esc", "beta-ws-esc")
	mkPolicyRule(orgEsc.ID,
		mkGenericSMS(orgEsc.ID, "esc-imm", toEscImm, tokEsc),
		mkGenericSMS(orgEsc.ID, "esc-a", toEscA, tokEsc),
		mkGenericSMS(orgEsc.ID, "esc-b", toEscB, tokEsc))

	// org ACK: same policy shape; the alert is acknowledged before step 2's delay,
	// so step 2 must never fire (bullet 2 "NOT if acked first").
	orgAck, _ := st.CreateOrg(ctx, "Gamma Ack")
	devAck := mkDevice(t, st, orgAck.ID, "uid-ack", "gamma-ws-ack")
	mkPolicyRule(orgAck.ID,
		mkGenericSMS(orgAck.ID, "ack-imm", toAckImm, tokAck),
		mkGenericSMS(orgAck.ID, "ack-a", toAckA, tokAck),
		mkGenericSMS(orgAck.ID, "ack-b", toAckB, tokAck))

	// org MAINT: same policy shape, but an active maintenance window suppresses
	// every step (bullet 2 "a maintenance window suppresses the steps").
	orgMaint, _ := st.CreateOrg(ctx, "Delta Maint")
	devMaint := mkDevice(t, st, orgMaint.ID, "uid-maint", "delta-ws-maint")
	mkPolicyRule(orgMaint.ID,
		mkGenericSMS(orgMaint.ID, "maint-imm", toMaintImm, tokMaint),
		mkGenericSMS(orgMaint.ID, "maint-a", toMaintA, tokMaint),
		mkGenericSMS(orgMaint.ID, "maint-b", toMaintB, tokMaint))

	// Bring every device online at base, then open a maintenance window for
	// orgMaint that covers the whole run before the devices go offline.
	benign := protocol.Metrics{MemTotalMB: 1000, MemUsedMB: 100, Disks: []protocol.Disk{{Mount: "/", TotalGB: 100, UsedGB: 10}}}
	for _, d := range []store.Device{devSMS, devEsc, devAck, devMaint} {
		syncDevice(t, st, d, benign)
	}
	if _, err := st.CreateWindow(ctx, store.CreateWindowParams{
		OrgID: &orgMaint.ID, StartsAt: clk.now().Add(-time.Minute), EndsAt: clk.now().Add(time.Hour), Reason: "patching",
	}); err != nil {
		t.Fatalf("create window: %v", err)
	}

	// Advance past the offline window (3*30+15 = 105 s) so every device is offline,
	// then open the alerts. opened_at is the clock at this instant (Topen).
	clk.advance(200 * time.Second)
	topen := clk.now()
	server.EvaluateNow(srv) // opens offline alerts + enqueues on_open (step 0)
	server.EvaluateNow(srv) // idempotent second pass

	// --- dispatch #1 at Topen: step 1 (after 0 min) comes due -----------------
	server.DispatchNotificationsNow(srv)
	server.DispatchNotificationsNow(srv) // idempotent: no double-send of step 0/step 1

	// Bullet 1: the plain SMS channel sent exactly one short message; the token
	// never rode in the body, and the body is the one-line summary.
	wantSMS := fmt.Sprintf("[MSP] high offline on %s (%s)", "acme-ws-sms", orgSMS.Name)
	recsSMS := sink.forNumber(toSMS)
	if len(recsSMS) != 1 {
		t.Fatalf("plain SMS: provider got %d messages for %s, want 1", len(recsSMS), toSMS)
	}
	if recsSMS[0].text != wantSMS {
		t.Fatalf("plain SMS text = %q, want %q", recsSMS[0].text, wantSMS)
	}
	if strings.Contains(recsSMS[0].raw, tokSMS) || strings.Contains(recsSMS[0].text, tokSMS) {
		t.Fatalf("plain SMS body leaked the token: %q", recsSMS[0].raw)
	}
	if recsSMS[0].auth == "" {
		t.Fatalf("plain SMS (twilio) had no Authorization header")
	}
	assertSMSSent(t, st, orgSMS.ID, 0)

	// Bullet 2 (open/step1): orgEsc sent the on_open (step 0) and step 1, not step 2.
	if n := len(sink.forNumber(toEscImm)); n != 1 {
		t.Fatalf("orgEsc on_open: got %d, want 1", n)
	}
	if n := len(sink.forNumber(toEscA)); n != 1 {
		t.Fatalf("orgEsc step 1: got %d, want 1", n)
	}
	if n := len(sink.forNumber(toEscB)); n != 0 {
		t.Fatalf("orgEsc step 2 fired early: got %d, want 0", n)
	}
	// The step-1 delivery carries step 1 in the log.
	assertSMSSent(t, st, orgEsc.ID, 1)

	// Bullet 2 (maintenance): orgMaint sent nothing; its deliveries are suppressed.
	for _, num := range []string{toMaintImm, toMaintA} {
		if n := len(sink.forNumber(num)); n != 0 {
			t.Fatalf("orgMaint sent to %s during maintenance: got %d, want 0", num, n)
		}
	}
	for _, d := range orgDeliveries(t, st, orgMaint.ID) {
		if d.Status != store.DeliverySuppressed {
			t.Fatalf("orgMaint delivery %d (step %d) status = %q, want suppressed", d.ID, d.Step, d.Status)
		}
	}

	// orgAck also delivered step 1 (open + unacked at Topen); now acknowledge the
	// alert so step 2 never fires.
	ackAlert, err := st.GetOpenAlert(ctx, devAck.ID, "offline")
	if err != nil {
		t.Fatalf("get orgAck open alert: %v", err)
	}
	if n := len(sink.forNumber(toAckA)); n != 1 {
		t.Fatalf("orgAck step 1: got %d, want 1", n)
	}
	if err := st.AckAlert(ctx, ackAlert.ID, admin.ID); err != nil {
		t.Fatalf("ack orgAck alert: %v", err)
	}

	// --- advance past step 2's 15 min delay; dispatch twice -------------------
	clk.advance(15 * time.Minute)
	if got := clk.now().Sub(topen); got != 15*time.Minute {
		t.Fatalf("clock advanced %v past open, want 15m", got)
	}
	server.DispatchNotificationsNow(srv)
	server.DispatchNotificationsNow(srv) // idempotent: step 2 must not double-send

	// Bullet 2 (step2 while open+unacked): orgEsc now sent step 2 exactly once.
	if n := len(sink.forNumber(toEscB)); n != 1 {
		t.Fatalf("orgEsc step 2: got %d, want exactly 1 (no double-send)", n)
	}
	assertSMSSent(t, st, orgEsc.ID, 2)
	// orgEsc produced exactly three deliveries total (step 0, 1, 2), each once.
	if n := len(orgDeliveries(t, st, orgEsc.ID)); n != 3 {
		t.Fatalf("orgEsc deliveries = %d, want 3 (step 0,1,2; idempotent)", n)
	}

	// Bullet 2 (NOT if acked): orgAck never sent step 2.
	if n := len(sink.forNumber(toAckB)); n != 0 {
		t.Fatalf("orgAck step 2 fired after ack: got %d, want 0", n)
	}
	// orgAck has step 0 and step 1 only (step 2 never enqueued once acked).
	if n := len(orgDeliveries(t, st, orgAck.ID)); n != 2 {
		t.Fatalf("orgAck deliveries = %d, want 2 (step 0,1 only)", n)
	}

	// Bullet 2 (maintenance): orgMaint still sent nothing and now has step 2
	// suppressed too.
	if n := len(sink.forNumber(toMaintB)); n != 0 {
		t.Fatalf("orgMaint step 2 sent during maintenance: got %d, want 0", n)
	}
	if n := len(orgDeliveries(t, st, orgMaint.ID)); n != 3 {
		t.Fatalf("orgMaint deliveries = %d, want 3 (step 0,1,2 all suppressed)", n)
	}

	// Token never leaked anywhere: no delivery detail/last_error carries any token,
	// and neither does the server log.
	for _, orgID := range []int64{orgSMS.ID, orgEsc.ID, orgAck.ID, orgMaint.ID} {
		for _, d := range orgDeliveries(t, st, orgID) {
			for _, tok := range []string{tokSMS, tokEsc, tokAck, tokMaint} {
				if strings.Contains(d.DetailJSON, tok) || strings.Contains(d.LastError, tok) {
					t.Fatalf("delivery %d leaked a token in detail/last_error: %q / %q", d.ID, d.DetailJSON, d.LastError)
				}
			}
		}
	}
	for _, tok := range []string{tokSMS, tokEsc, tokAck, tokMaint} {
		if strings.Contains(logs.String(), tok) {
			t.Fatalf("server log leaked token %q", tok)
		}
	}
	// Every recorded provider body carried the short summary, never a raw token.
	for _, rec := range sink.all() {
		if !strings.HasPrefix(rec.text, "[MSP] ") {
			t.Fatalf("provider got a non-summary SMS: %q", rec.text)
		}
	}
}

// assertSMSSent fails unless org orgID has a sent delivery with the given step.
func assertSMSSent(t *testing.T, st *store.Store, orgID int64, step int) {
	t.Helper()
	for _, d := range orgDeliveries(t, st, orgID) {
		if d.Step == step && d.Status == store.DeliverySent {
			return
		}
	}
	t.Fatalf("org %d has no sent delivery at step %d: %+v", orgID, step, orgDeliveries(t, st, orgID))
}

// TestNS2OrgScoping is the NS2 Proof bullet 5, through the real HTTP API: an
// operator scoped to org A gets a 404 (never a distinguishable 403) on an
// escalation policy or SMS channel in org B, sees only A's policies/channels
// (no secret leaked), and cannot attach a rule to B's policy; a side effect
// never happens, and an admin is unaffected.
func TestNS2OrgScoping(t *testing.T) {
	ctx := context.Background()
	st, _ := newNotifyStore(t)
	logs := &logBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})
	srv := server.New(st, server.Config{SampleEvery: time.Second, LostGrace: time.Second, Version: agentVersion}, logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if _, err := st.CreateUser(ctx, adminEmail, mustHash(t), "admin"); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	orgA, _ := st.CreateOrg(ctx, "Acme Dental")
	orgB, _ := st.CreateOrg(ctx, "Beta Corp")

	// Seed an SMS channel + an escalation policy in each org (secrets set so the
	// list must not leak them).
	mkSMS := func(orgID int64, name, secret string) store.NotifyChannel {
		cfg := `{"provider":"generic","from":"+15559990000","to":["+15550001111"],"url":"https://sms.example.test/send"}`
		ch, err := st.CreateChannel(ctx, store.CreateChannelParams{
			Kind: store.ChannelSMS, Name: name, OrgID: &orgID, ConfigJSON: cfg, Secret: secret, Enabled: true,
		})
		if err != nil {
			t.Fatalf("sms channel %s: %v", name, err)
		}
		return ch
	}
	mkPolicy := func(orgID int64, name string, chID int64) store.EscalationPolicy {
		p, err := st.CreatePolicy(ctx, store.CreatePolicyParams{
			OrgID: &orgID, Name: name, Steps: []store.CreatePolicyStep{{AfterMinutes: 0, ChannelIDs: []int64{chID}}},
		})
		if err != nil {
			t.Fatalf("policy %s: %v", name, err)
		}
		return p
	}
	chA := mkSMS(orgA.ID, "A sms", "a-sms-secret-xyz")
	chB := mkSMS(orgB.ID, "B sms", "b-sms-secret-xyz")
	polA := mkPolicy(orgA.ID, "A policy", chA.ID)
	polB := mkPolicy(orgB.ID, "B policy", chB.ID)

	// Admin creates an operator scoped to org A only.
	adminD := newDash(t, ts.URL)
	if code := adminD.do(http.MethodPost, "/api/v1/login", map[string]string{"email": adminEmail, "password": adminPassword}, nil); code != 200 {
		t.Fatalf("admin login: %d", code)
	}
	var created struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if code := adminD.do(http.MethodPost, "/api/v1/users", map[string]any{
		"email": "ns2-scoped@example.com", "password": "scoped-pass-1", "role": "operator",
		"all_orgs": false, "org_ids": []int64{orgA.ID},
	}, &created); code != 201 {
		t.Fatalf("create scoped operator: %d", code)
	}

	op := newDash(t, ts.URL)
	if code := op.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "ns2-scoped@example.com", "password": "scoped-pass-1"}, nil); code != 200 {
		t.Fatalf("operator login: %d", code)
	}

	// Scoped policy list shows only A.
	var pols struct {
		Policies []struct {
			ID int64 `json:"id"`
		} `json:"policies"`
	}
	if code := op.do(http.MethodGet, "/api/v1/notify/policies", nil, &pols); code != 200 {
		t.Fatalf("scoped policies: %d", code)
	}
	if len(pols.Policies) != 1 || pols.Policies[0].ID != polA.ID {
		t.Fatalf("scoped policies = %+v, want only A (%d)", pols.Policies, polA.ID)
	}

	// Scoped channel list shows only A's SMS channel and leaks no secret.
	code, raw := op.doRaw(http.MethodGet, "/api/v1/notify/channels", nil)
	if code != 200 {
		t.Fatalf("scoped channels: %d", code)
	}
	if strings.Contains(raw, "a-sms-secret-xyz") || strings.Contains(raw, "b-sms-secret-xyz") {
		t.Fatalf("scoped channel list leaked a secret: %s", raw)
	}
	var chans struct {
		Channels []struct {
			ID int64 `json:"id"`
		} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(raw), &chans); err != nil {
		t.Fatalf("decode channels: %v", err)
	}
	if len(chans.Channels) != 1 || chans.Channels[0].ID != chA.ID {
		t.Fatalf("scoped channels = %+v, want only A (%d)", chans.Channels, chA.ID)
	}

	// 404 (no side effect) on every per-row access to org B's policy / SMS channel.
	stepBody := map[string]any{"name": "x", "steps": []map[string]any{{"after_minutes": 0, "channel_ids": []int64{chB.ID}}}}
	type wr struct {
		method, path string
		body         any
	}
	writes := []wr{
		{http.MethodPut, fmt.Sprintf("/api/v1/notify/policies/%d", polB.ID), stepBody},
		{http.MethodDelete, fmt.Sprintf("/api/v1/notify/policies/%d", polB.ID), nil},
		{http.MethodPut, fmt.Sprintf("/api/v1/notify/channels/%d", chB.ID), map[string]any{"kind": "sms", "name": "x", "config": map[string]any{"provider": "generic", "from": "+15559990000", "to": []string{"+15550001111"}, "url": "https://sms.example.test/send"}}},
		{http.MethodDelete, fmt.Sprintf("/api/v1/notify/channels/%d", chB.ID), nil},
		{http.MethodPost, fmt.Sprintf("/api/v1/notify/channels/%d/test", chB.ID), nil},
	}
	for _, p := range writes {
		var e errorJSON
		if code := op.do(p.method, p.path, p.body, &e); code != 404 || e.Error != "not_found" {
			t.Fatalf("scoped %s %s = %d %q, want 404 not_found", p.method, p.path, code, e.Error)
		}
	}
	// No side effect: B's policy and channel survive.
	if _, err := st.GetPolicy(ctx, polB.ID); err != nil {
		t.Fatalf("scoped request affected org B policy: %v", err)
	}
	if _, err := st.GetChannel(ctx, chB.ID); err != nil {
		t.Fatalf("scoped request affected org B channel: %v", err)
	}

	// A scoped operator cannot attach a rule to B's (invisible) policy: a 400 that
	// does not reveal whether it exists.
	var pe errorJSON
	if code := op.do(http.MethodPost, "/api/v1/notify/rules", map[string]any{
		"name": "x", "min_severity": "low", "channel_id": chA.ID, "escalation_policy_id": polB.ID,
	}, &pe); code != 400 || pe.Error != "invalid_request" {
		t.Fatalf("scoped rule->B policy = %d %q, want 400 invalid_request", code, pe.Error)
	}

	// Admin sees both policies.
	var adminPols struct {
		Policies []struct {
			ID int64 `json:"id"`
		} `json:"policies"`
	}
	if code := adminD.do(http.MethodGet, "/api/v1/notify/policies", nil, &adminPols); code != 200 || len(adminPols.Policies) != 2 {
		t.Fatalf("admin policies = %d/%+v, want 2", code, adminPols.Policies)
	}
}
