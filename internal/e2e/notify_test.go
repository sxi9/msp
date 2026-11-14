package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// NS1 end-to-end proof. Two tests, both against the real store and the real
// server (no fakes in the send path): TestNotificationsDelivery drives the
// evaluator and the production dispatcher with a fake clock, a local fake SMTP
// server and httptest webhooks, and asserts opened-alert fan-out, the severity
// gate, maintenance suppression, and the failing-sender retry/idempotence rules
// (spec Proof bullets 1-4). TestNotificationsOrgScoping drives the real HTTP API
// as a scoped operator and asserts the RS2 404-not-403 isolation on every notify
// resource and the global-row rule (bullet 5).

// --- fake clock -------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// --- local fake SMTP server (multi-message) ---------------------------------

type smtpMsg struct {
	from string
	to   []string
	data string
}

type smtpSink struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []smtpMsg
}

func startSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("smtp listen: %v", err)
	}
	s := &smtpSink{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *smtpSink) addr() (string, int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (s *smtpSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func (s *smtpSink) all() []smtpMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]smtpMsg(nil), s.msgs...)
}

func (s *smtpSink) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.handle(conn)
	}
}

func (s *smtpSink) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(line string) { conn.Write([]byte(line)) }
	w("220 mock ESMTP ready\r\n")
	var cap smtpMsg
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			w("250 mock at your service\r\n")
		case strings.HasPrefix(up, "MAIL FROM"):
			cap.from = addrBetween(line)
			w("250 OK\r\n")
		case strings.HasPrefix(up, "RCPT TO"):
			cap.to = append(cap.to, addrBetween(line))
			w("250 OK\r\n")
		case strings.HasPrefix(up, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>\r\n")
			var b strings.Builder
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				b.WriteString(dl)
			}
			cap.data = b.String()
			w("250 OK queued\r\n")
		case strings.HasPrefix(up, "QUIT"):
			w("221 Bye\r\n")
			s.mu.Lock()
			s.msgs = append(s.msgs, cap)
			s.mu.Unlock()
			return
		default:
			w("250 OK\r\n")
		}
	}
}

func addrBetween(line string) string {
	i := strings.IndexByte(line, '<')
	j := strings.IndexByte(line, '>')
	if i >= 0 && j > i {
		return line[i+1 : j]
	}
	if k := strings.IndexByte(line, ':'); k >= 0 {
		return strings.TrimSpace(line[k+1:])
	}
	return ""
}

// --- httptest webhook recorder ----------------------------------------------

type webhookSink struct {
	mu     sync.Mutex
	bodies [][]byte
	status int // response status; 0 means 200
}

func (h *webhookSink) handler(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, r.ContentLength)
	if r.ContentLength > 0 {
		_, _ = r.Body.Read(body)
	} else {
		body, _ = readAll(r)
	}
	h.mu.Lock()
	h.bodies = append(h.bodies, body)
	st := h.status
	h.mu.Unlock()
	if st == 0 {
		st = http.StatusOK
	}
	w.WriteHeader(st)
}

func (h *webhookSink) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies)
}

func (h *webhookSink) last() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bodies) == 0 {
		return nil
	}
	return h.bodies[len(h.bodies)-1]
}

func readAll(r *http.Request) ([]byte, error) {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return []byte(b.String()), nil
		}
	}
}

// --- shared seeding ---------------------------------------------------------

const notifyMaxAttempts = 5 // spec Delivery: give up at maxAttempts (NS1 notifyMaxAttempts)

func newNotifyStore(t *testing.T) (*store.Store, *fakeClock) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "msp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &fakeClock{t: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	st.Now = clk.now
	return st, clk
}

// syncDevice records one sync for a device so it has a last_seen_at (online at
// the current clock) and the given metrics.
func syncDevice(t *testing.T, st *store.Store, dev store.Device, m protocol.Metrics) {
	t.Helper()
	if m.Disks == nil {
		m.Disks = []protocol.Disk{}
	}
	if m.IPs == nil {
		m.IPs = []string{"10.0.0.2"}
	}
	err := st.RecordSync(context.Background(), dev.ID, "10.0.0.2", protocol.SyncRequest{
		AgentVersion: "0.1.0", IntervalS: 30, UptimeS: 100, Metrics: m,
	}, time.Second)
	if err != nil {
		t.Fatalf("record sync %s: %v", dev.Hostname, err)
	}
}

func mkDevice(t *testing.T, st *store.Store, orgID int64, uid, host string) store.Device {
	t.Helper()
	d, _, err := st.UpsertDevice(context.Background(), store.UpsertDeviceParams{
		OrgID: orgID, DeviceUID: uid, Hostname: host, Platform: "linux",
		Arch: "amd64", OSVersion: "Ubuntu 24.04", AgentVersion: "0.1.0", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("upsert device %s: %v", host, err)
	}
	return d
}

func orgDeliveries(t *testing.T, st *store.Store, orgID int64) []store.NotificationDelivery {
	t.Helper()
	id := orgID
	ds, err := st.ListDeliveries(context.Background(), store.DeliveryFilter{OrgID: &id})
	if err != nil {
		t.Fatalf("list deliveries org %d: %v", orgID, err)
	}
	return ds
}

// TestNotificationsDelivery is the spec Proof, bullets 1-4, driven through the
// real evaluator and the production dispatcher (net/smtp + net/http) against a
// local fake SMTP server and httptest webhooks, on a fake clock.
func TestNotificationsDelivery(t *testing.T) {
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

	// Fake transports.
	smtp1 := startSMTPSink(t)     // scenario 1 (fan-out) email
	smtpSup := startSMTPSink(t)   // scenario 3a (suppressed) email: must receive nothing
	smtpSend := startSMTPSink(t)  // scenario 3b (outside window) email
	slackHook := &webhookSink{}
	slackSrv := httptest.NewServer(http.HandlerFunc(slackHook.handler))
	defer slackSrv.Close()
	failHook := &webhookSink{status: http.StatusInternalServerError}
	failSrv := httptest.NewServer(http.HandlerFunc(failHook.handler))
	defer failSrv.Close()

	// --- orgs, devices, channels, rules, windows -----------------------------
	mkEmailCh := func(orgID int64, name string, sink *smtpSink) store.NotifyChannel {
		host, port := sink.addr()
		cfg := fmt.Sprintf(`{"host":%q,"port":%d,"from":"alerts@msp.test","to":["ops@client.test"],"starttls":false}`, host, port)
		ch, err := st.CreateChannel(ctx, store.CreateChannelParams{
			Kind: store.ChannelEmail, Name: name, OrgID: &orgID, ConfigJSON: cfg, Enabled: true, CreatedBy: &admin.ID,
		})
		if err != nil {
			t.Fatalf("create email channel %s: %v", name, err)
		}
		return ch
	}
	mkWebhookCh := func(orgID int64, name, url, flavor string) store.NotifyChannel {
		cfg := fmt.Sprintf(`{"url":%q,"flavor":%q}`, url, flavor)
		ch, err := st.CreateChannel(ctx, store.CreateChannelParams{
			Kind: store.ChannelWebhook, Name: name, OrgID: &orgID, ConfigJSON: cfg, Enabled: true, CreatedBy: &admin.ID,
		})
		if err != nil {
			t.Fatalf("create webhook channel %s: %v", name, err)
		}
		return ch
	}
	mkRule := func(orgID int64, name, minSev string, ch store.NotifyChannel) {
		if _, err := st.CreateRule(ctx, store.CreateRuleParams{
			Name: name, OrgID: &orgID, MinSeverity: minSev, OnOpen: true, OnResolve: false,
			ChannelID: ch.ID, Enabled: true, CreatedBy: &admin.ID,
		}); err != nil {
			t.Fatalf("create rule %s: %v", name, err)
		}
	}

	// org1: an email rule and a slack rule, both min low (bullet 1).
	org1, _ := st.CreateOrg(ctx, "Acme One")
	dev1 := mkDevice(t, st, org1.ID, "uid-1", "acme-ws-01")
	mkRule(org1.ID, "email-low", "low", mkEmailCh(org1.ID, "ops-email", smtp1))
	mkRule(org1.ID, "slack-low", "low", mkWebhookCh(org1.ID, "ops-slack", slackSrv.URL, "slack"))

	// org2: a min_severity=high rule; the device trips only memory_high (low),
	// which must not fire (bullet 2).
	org2, _ := st.CreateOrg(ctx, "Beta Two")
	dev2 := mkDevice(t, st, org2.ID, "uid-2", "beta-ws-02")
	mkRule(org2.ID, "high-only", "high", mkEmailCh(org2.ID, "beta-email", smtp1))

	// org3: an active maintenance window suppresses the opened alert (bullet 3a).
	org3, _ := st.CreateOrg(ctx, "Gamma Three")
	dev3 := mkDevice(t, st, org3.ID, "uid-3", "gamma-ws-03")
	mkRule(org3.ID, "g-low", "low", mkEmailCh(org3.ID, "gamma-email", smtpSup))

	// org4: no window; the opened alert sends (bullet 3b, "outside it they send").
	org4, _ := st.CreateOrg(ctx, "Delta Four")
	dev4 := mkDevice(t, st, org4.ID, "uid-4", "delta-ws-04")
	mkRule(org4.ID, "d-low", "low", mkEmailCh(org4.ID, "delta-email", smtpSend))

	// org5: a webhook that always 500s; the delivery must reach failed after
	// maxAttempts, and enqueue is idempotent across two ticks (bullet 4).
	org5, _ := st.CreateOrg(ctx, "Epsilon Five")
	dev5 := mkDevice(t, st, org5.ID, "uid-5", "epsilon-ws-05")
	mkRule(org5.ID, "e-low", "low", mkWebhookCh(org5.ID, "eps-hook", failSrv.URL, "generic"))

	// --- bring the offline devices online at base, then go offline -----------
	benign := protocol.Metrics{MemTotalMB: 1000, MemUsedMB: 100, Disks: []protocol.Disk{{Mount: "/", TotalGB: 100, UsedGB: 10}}}
	for _, d := range []store.Device{dev1, dev3, dev4, dev5} {
		syncDevice(t, st, d, benign)
	}
	// Active window for org3 covering the evaluation instant.
	if _, err := st.CreateWindow(ctx, store.CreateWindowParams{
		OrgID: &org3.ID, StartsAt: clk.now().Add(100 * time.Second), EndsAt: clk.now().Add(300 * time.Second), Reason: "patching",
	}); err != nil {
		t.Fatalf("create window: %v", err)
	}

	// Advance past the offline window (3*30+15 = 105 s) so dev1/3/4/5 are offline.
	clk.advance(200 * time.Second)
	// dev2 syncs now (online) with high memory so only memory_high (low) opens.
	syncDevice(t, st, dev2, protocol.Metrics{MemTotalMB: 1000, MemUsedMB: 990, Disks: []protocol.Disk{{Mount: "/", TotalGB: 100, UsedGB: 10}}})

	// --- two evaluator ticks: alerts open and enqueue, idempotently ----------
	server.EvaluateNow(srv)
	server.EvaluateNow(srv)

	// Idempotence: org1's opened alert produced exactly two deliveries (one per
	// rule), not four, across the two ticks.
	if n := len(orgDeliveries(t, st, org1.ID)); n != 2 {
		t.Fatalf("org1 deliveries after two ticks = %d, want 2 (idempotent)", n)
	}
	// Bullet 2: the high rule did not fire on the low memory_high alert.
	if n := len(orgDeliveries(t, st, org2.ID)); n != 0 {
		t.Fatalf("org2 deliveries = %d, want 0 (low alert, high rule)", n)
	}
	// Bullet 3a: org3's delivery is suppressed, not pending.
	d3 := orgDeliveries(t, st, org3.ID)
	if len(d3) != 1 || d3[0].Status != store.DeliverySuppressed {
		t.Fatalf("org3 deliveries = %+v, want 1 suppressed", d3)
	}

	// --- dispatch: send the pending ones -------------------------------------
	server.DispatchNotificationsNow(srv)

	// Bullet 1: org1's two deliveries are sent; the email names the device and
	// alert; the Slack webhook got the Slack JSON naming the device.
	for _, d := range orgDeliveries(t, st, org1.ID) {
		if d.Status != store.DeliverySent {
			t.Fatalf("org1 delivery %d status = %q, want sent (last_error=%q)", d.ID, d.Status, d.LastError)
		}
	}
	if smtp1.count() != 1 {
		t.Fatalf("scenario 1 SMTP received %d messages, want 1", smtp1.count())
	}
	em := smtp1.all()[0]
	if !strings.Contains(em.data, "acme-ws-01") || !strings.Contains(em.data, "no sync for") {
		t.Fatalf("scenario 1 email does not name the device/alert:\n%s", em.data)
	}
	if em.from != "alerts@msp.test" || len(em.to) != 1 || em.to[0] != "ops@client.test" {
		t.Fatalf("scenario 1 email envelope = from %q to %v", em.from, em.to)
	}
	if slackHook.count() != 1 {
		t.Fatalf("Slack webhook received %d posts, want 1", slackHook.count())
	}
	var slack map[string]string
	if err := json.Unmarshal(slackHook.last(), &slack); err != nil {
		t.Fatalf("slack body not JSON: %v (%s)", err, slackHook.last())
	}
	if !strings.Contains(slack["text"], "acme-ws-01") {
		t.Fatalf("slack text does not name the device: %q", slack["text"])
	}

	// Bullet 3a vs 3b: suppressed org sent nothing; the outside-window org sent.
	if smtpSup.count() != 0 {
		t.Fatalf("suppressed org SMTP received %d messages, want 0", smtpSup.count())
	}
	d3 = orgDeliveries(t, st, org3.ID)
	if len(d3) != 1 || d3[0].Status != store.DeliverySuppressed {
		t.Fatalf("org3 delivery after dispatch = %+v, want still suppressed", d3)
	}
	d4 := orgDeliveries(t, st, org4.ID)
	if len(d4) != 1 || d4[0].Status != store.DeliverySent {
		t.Fatalf("org4 delivery = %+v, want 1 sent", d4)
	}
	if smtpSend.count() != 1 {
		t.Fatalf("outside-window org SMTP received %d messages, want 1", smtpSend.count())
	}

	// Bullet 4: the failing webhook delivery retries and lands failed after
	// maxAttempts. Advance past the retry backoff each tick so the leased row is
	// claimable again (the first attempt already ran in the dispatch above).
	for i := 0; i < notifyMaxAttempts+2; i++ {
		clk.advance(2 * time.Minute)
		server.DispatchNotificationsNow(srv)
	}
	d5 := orgDeliveries(t, st, org5.ID)
	if len(d5) != 1 {
		t.Fatalf("org5 deliveries = %d, want 1", len(d5))
	}
	if d5[0].Status != store.DeliveryFailed {
		t.Fatalf("org5 delivery status = %q, want failed", d5[0].Status)
	}
	if d5[0].Attempts != notifyMaxAttempts {
		t.Fatalf("org5 delivery attempts = %d, want %d", d5[0].Attempts, notifyMaxAttempts)
	}
	if d5[0].LastError == "" {
		t.Fatalf("org5 failed delivery has no last_error")
	}
	if failHook.count() < notifyMaxAttempts {
		t.Fatalf("failing webhook was hit %d times, want >= %d", failHook.count(), notifyMaxAttempts)
	}
}

// TestNotificationsOrgScoping is the spec Proof bullet 5, through the real HTTP
// API: an operator scoped to org A gets 404 (never a distinguishable 403) on a
// channel/rule/window in org B with no side effect, sees only A's deliveries,
// and cannot create a global (org_id NULL) row; an admin is unaffected, and a
// grant of B lets the operator manage B.
func TestNotificationsOrgScoping(t *testing.T) {
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

	// Seed notify rows in both orgs directly.
	chA, err := st.CreateChannel(ctx, store.CreateChannelParams{Kind: store.ChannelEmail, Name: "A chan", OrgID: &orgA.ID, ConfigJSON: `{"host":"a","port":25,"from":"a@a","to":["b@b"]}`, Secret: "a-secret-xyz", Enabled: true})
	if err != nil {
		t.Fatalf("channel A: %v", err)
	}
	chB, err := st.CreateChannel(ctx, store.CreateChannelParams{Kind: store.ChannelEmail, Name: "B chan", OrgID: &orgB.ID, ConfigJSON: `{"host":"b","port":25,"from":"a@a","to":["b@b"]}`, Secret: "b-secret-xyz", Enabled: true})
	if err != nil {
		t.Fatalf("channel B: %v", err)
	}
	ruleB, err := st.CreateRule(ctx, store.CreateRuleParams{Name: "B rule", OrgID: &orgB.ID, MinSeverity: "high", OnOpen: true, ChannelID: chB.ID, Enabled: true})
	if err != nil {
		t.Fatalf("rule B: %v", err)
	}
	winB, err := st.CreateWindow(ctx, store.CreateWindowParams{OrgID: &orgB.ID, StartsAt: st.Now(), EndsAt: st.Now().Add(time.Hour), Reason: "x"})
	if err != nil {
		t.Fatalf("window B: %v", err)
	}
	// One delivery per org (log rows).
	aID := orgA.ID
	bID := orgB.ID
	if _, _, err := st.EnqueueDelivery(ctx, store.EnqueueDeliveryParams{ChannelID: &chA.ID, OrgID: &aID, Event: store.EventTest, Status: store.DeliverySent}); err != nil {
		t.Fatalf("delivery A: %v", err)
	}
	if _, _, err := st.EnqueueDelivery(ctx, store.EnqueueDeliveryParams{ChannelID: &chB.ID, OrgID: &bID, Event: store.EventTest, Status: store.DeliverySent}); err != nil {
		t.Fatalf("delivery B: %v", err)
	}

	// --- admin creates an operator scoped to org A only ----------------------
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
		"email": "notify-scoped@example.com", "password": "scoped-pass-1", "role": "operator",
		"all_orgs": false, "org_ids": []int64{orgA.ID},
	}, &created); code != 201 {
		t.Fatalf("create scoped operator: %d", code)
	}
	opID := created.User.ID

	op := newDash(t, ts.URL)
	if code := op.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "notify-scoped@example.com", "password": "scoped-pass-1"}, nil); code != 200 {
		t.Fatalf("operator login: %d", code)
	}

	// --- scoped lists show only org A, no secret leaked ----------------------
	var chans struct {
		Channels []struct {
			ID        int64 `json:"id"`
			SecretSet bool  `json:"secret_set"`
		} `json:"channels"`
	}
	code, raw := op.doRaw(http.MethodGet, "/api/v1/notify/channels", nil)
	if code != 200 {
		t.Fatalf("scoped channels: %d", code)
	}
	if strings.Contains(raw, "a-secret-xyz") || strings.Contains(raw, "b-secret-xyz") {
		t.Fatalf("scoped channel list leaked a secret: %s", raw)
	}
	if err := json.Unmarshal([]byte(raw), &chans); err != nil {
		t.Fatalf("decode channels: %v", err)
	}
	if len(chans.Channels) != 1 || chans.Channels[0].ID != chA.ID {
		t.Fatalf("scoped channels = %+v, want only A (%d)", chans.Channels, chA.ID)
	}

	var dels struct {
		Deliveries []struct {
			OrgID *int64 `json:"org_id"`
		} `json:"deliveries"`
	}
	if code := op.do(http.MethodGet, "/api/v1/notify/deliveries", nil, &dels); code != 200 {
		t.Fatalf("scoped deliveries: %d", code)
	}
	if len(dels.Deliveries) != 1 || dels.Deliveries[0].OrgID == nil || *dels.Deliveries[0].OrgID != orgA.ID {
		t.Fatalf("scoped deliveries = %+v, want only org A", dels.Deliveries)
	}

	// --- 404 (no side effect) on every per-row write in org B ----------------
	type wr struct {
		method, path string
		body         any
	}
	emailCfg := map[string]any{"host": "x", "port": 25, "from": "a@a", "to": []string{"b@b"}}
	writes := []wr{
		{http.MethodPut, fmt.Sprintf("/api/v1/notify/channels/%d", chB.ID), map[string]any{"kind": "email", "name": "x", "config": emailCfg}},
		{http.MethodDelete, fmt.Sprintf("/api/v1/notify/channels/%d", chB.ID), nil},
		{http.MethodPost, fmt.Sprintf("/api/v1/notify/channels/%d/test", chB.ID), nil},
		{http.MethodPut, fmt.Sprintf("/api/v1/notify/rules/%d", ruleB.ID), map[string]any{"name": "x", "min_severity": "high", "channel_id": chB.ID}},
		{http.MethodDelete, fmt.Sprintf("/api/v1/notify/rules/%d", ruleB.ID), nil},
		{http.MethodDelete, fmt.Sprintf("/api/v1/notify/windows/%d", winB.ID), nil},
	}
	for _, p := range writes {
		var e errorJSON
		if code := op.do(p.method, p.path, p.body, &e); code != 404 || e.Error != "not_found" {
			t.Fatalf("scoped %s %s = %d %q, want 404 not_found", p.method, p.path, code, e.Error)
		}
	}
	// No side effect: B's rows survive.
	if _, err := st.GetChannel(ctx, chB.ID); err != nil {
		t.Fatalf("scoped request affected org B channel: %v", err)
	}
	if _, err := st.GetRule(ctx, ruleB.ID); err != nil {
		t.Fatalf("scoped request affected org B rule: %v", err)
	}
	if _, err := st.GetWindow(ctx, winB.ID); err != nil {
		t.Fatalf("scoped request affected org B window: %v", err)
	}

	// Creating a channel in an invisible concrete org is 404.
	if code := op.do(http.MethodPost, "/api/v1/notify/channels", map[string]any{"kind": "email", "name": "x", "org_id": orgB.ID, "config": emailCfg}, nil); code != 404 {
		t.Fatalf("scoped create in org B = %d, want 404", code)
	}
	// Creating a global (org_id NULL) row requires all-orgs: 403 for a scoped op.
	var ge errorJSON
	if code := op.do(http.MethodPost, "/api/v1/notify/channels", map[string]any{"kind": "email", "name": "g", "config": emailCfg}, &ge); code != 403 || ge.Error != "forbidden" {
		t.Fatalf("scoped create global = %d %q, want 403 forbidden", code, ge.Error)
	}

	// --- admin is unaffected -------------------------------------------------
	var adminChans struct {
		Channels []struct {
			ID int64 `json:"id"`
		} `json:"channels"`
	}
	if code := adminD.do(http.MethodGet, "/api/v1/notify/channels", nil, &adminChans); code != 200 || len(adminChans.Channels) != 2 {
		t.Fatalf("admin channels = %d/%+v, want 2", code, adminChans.Channels)
	}

	// --- grant org B; the operator now manages B -----------------------------
	if code := adminD.do(http.MethodPut, fmt.Sprintf("/api/v1/users/%d/orgs", opID),
		map[string]any{"all_orgs": false, "org_ids": []int64{orgA.ID, orgB.ID}}, nil); code != 200 {
		t.Fatalf("grant org B: %d", code)
	}
	if code := op.do(http.MethodDelete, fmt.Sprintf("/api/v1/notify/rules/%d", ruleB.ID), nil, nil); code != 204 {
		t.Fatalf("after grant, scoped op delete B rule = %d, want 204", code)
	}
}

// doRaw sends method path with an optional JSON body and returns the status
// code and the raw response body, for assertions that need the bytes (e.g. that
// a channel secret never appears in the list response).
func (d *dash) doRaw(method, path string, body any) (int, string) {
	d.t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			d.t.Fatalf("%s %s: marshal: %v", method, path, err)
		}
		rdr = strings.NewReader(string(b))
	}
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequest(method, d.base+path, rdr)
	} else {
		req, err = http.NewRequest(method, d.base+path, nil)
	}
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "msp")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		b.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

func mustHash(t *testing.T) string {
	t.Helper()
	h, err := server.HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	return h
}
