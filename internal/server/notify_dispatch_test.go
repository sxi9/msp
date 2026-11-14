package server

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/store"
)

// --- fakes ------------------------------------------------------------------

// fakeSender records SendEmail/PostWebhook calls and can fail on demand, so the
// dispatcher is tested with no real outbound send.
type fakeSender struct {
	mu       sync.Mutex
	emails   []fakeEmail
	webhooks []fakeWebhook
	failErr  error // when set, every send fails with this error
}

type fakeEmail struct {
	cfg      EmailConfig
	password string
	msg      EmailMessage
}

type fakeWebhook struct {
	url         string
	contentType string
	body        []byte
	headers     map[string]string
}

func (f *fakeSender) SendEmail(ctx context.Context, cfg EmailConfig, password string, msg EmailMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	f.emails = append(f.emails, fakeEmail{cfg: cfg, password: password, msg: msg})
	return nil
}

func (f *fakeSender) PostWebhook(ctx context.Context, rawURL, contentType string, body []byte, headers map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	f.webhooks = append(f.webhooks, fakeWebhook{url: rawURL, contentType: contentType, body: append([]byte(nil), body...), headers: headers})
	return nil
}

// --- shared seed helpers ----------------------------------------------------

func seedDevice(e *testEnv, hostname string) store.Device {
	e.t.Helper()
	dev, _, err := e.st.UpsertDevice(context.Background(), store.UpsertDeviceParams{
		OrgID: e.org.ID, DeviceUID: "uid-" + hostname, Hostname: hostname, Platform: "linux",
		Arch: "amd64", OSVersion: "Ubuntu 24.04", AgentVersion: "0.1.0", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		e.t.Fatalf("UpsertDevice: %v", err)
	}
	return dev
}

// seedAlert opens an alert and returns the stored Alert (with its id and joined
// hostname/org name) via ListAlerts.
func seedAlert(e *testEnv, dev store.Device, typ, severity, msg string) store.Alert {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.st.OpenAlert(ctx, e.org.ID, dev.ID, typ, severity, msg); err != nil {
		e.t.Fatalf("OpenAlert: %v", err)
	}
	alerts, err := e.st.ListAlerts(ctx, true, &e.org.ID, nil, 500)
	if err != nil {
		e.t.Fatalf("ListAlerts: %v", err)
	}
	for _, a := range alerts {
		if a.DeviceID == dev.ID && a.Type == typ {
			return a
		}
	}
	e.t.Fatalf("alert %s not found after open", typ)
	return store.Alert{}
}

func newChannel(e *testEnv, kind, name, configJSON, secret string) store.NotifyChannel {
	e.t.Helper()
	ch, err := e.st.CreateChannel(context.Background(), store.CreateChannelParams{
		Kind: kind, Name: name, OrgID: &e.org.ID, ConfigJSON: configJSON, Secret: secret, Enabled: true, CreatedBy: &e.admin.ID,
	})
	if err != nil {
		e.t.Fatalf("CreateChannel: %v", err)
	}
	return ch
}

func newRule(e *testEnv, ch store.NotifyChannel, minSev string) store.NotifyRule {
	e.t.Helper()
	r, err := e.st.CreateRule(context.Background(), store.CreateRuleParams{
		Name: "rule-" + ch.Name, OrgID: &e.org.ID, MinSeverity: minSev, OnOpen: true, OnResolve: false,
		ChannelID: ch.ID, Enabled: true, CreatedBy: &e.admin.ID,
	})
	if err != nil {
		e.t.Fatalf("CreateRule: %v", err)
	}
	return r
}

// --- dispatcher through the fake sender -------------------------------------

func TestDispatchEmailAndSlack(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	dev := seedDevice(e, "ws-reception-01")

	emailCh := newChannel(e, store.ChannelEmail, "ops-email", `{"host":"smtp.local","port":587,"from":"msp@ex.com","to":["ops@ex.com"]}`, "")
	slackCh := newChannel(e, store.ChannelWebhook, "ops-slack", `{"url":"https://hooks.slack.test/abc","flavor":"slack"}`, "")
	newRule(e, emailCh, "low")
	newRule(e, slackCh, "low")

	alert := seedAlert(e, dev, "offline", "high", "no sync for 6m")
	e.srv.EnqueueAlertNotifications(ctx, alert, store.EventOpened)

	// Two pending deliveries queued (one per rule).
	pending, _ := e.st.ListDeliveries(ctx, store.DeliveryFilter{Status: store.DeliveryPending})
	if len(pending) != 2 {
		t.Fatalf("pending deliveries = %d, want 2", len(pending))
	}

	fs := &fakeSender{}
	d := NewNotifyDispatcher(e.srv, fs)
	d.DispatchOnce(ctx)

	// Both deliveries sent.
	sent, _ := e.st.ListDeliveries(ctx, store.DeliveryFilter{Status: store.DeliverySent})
	if len(sent) != 2 {
		t.Fatalf("sent deliveries = %d, want 2", len(sent))
	}
	if len(fs.emails) != 1 || len(fs.webhooks) != 1 {
		t.Fatalf("sends: %d emails, %d webhooks", len(fs.emails), len(fs.webhooks))
	}
	em := fs.emails[0]
	if !strings.Contains(em.msg.Subject, "high") || !strings.Contains(em.msg.Subject, "offline") ||
		!strings.Contains(em.msg.Subject, "ws-reception-01") {
		t.Fatalf("email subject = %q", em.msg.Subject)
	}
	if !strings.Contains(em.msg.Body, "ws-reception-01") || !strings.Contains(em.msg.Body, "no sync for 6m") {
		t.Fatalf("email body missing device/alert: %q", em.msg.Body)
	}
	var slack map[string]string
	if err := json.Unmarshal(fs.webhooks[0].body, &slack); err != nil {
		t.Fatalf("slack body: %v", err)
	}
	if !strings.Contains(slack["text"], "ws-reception-01") || !strings.Contains(slack["text"], "no sync for 6m") {
		t.Fatalf("slack text = %q", slack["text"])
	}
}

func TestDispatchMinSeverityDoesNotFire(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	dev := seedDevice(e, "ws-02")
	ch := newChannel(e, store.ChannelEmail, "high-only", `{"host":"smtp.local","from":"a","to":["b"]}`, "")
	// Rule requires high; a low alert must not match.
	newRule(e, ch, "high")

	alert := seedAlert(e, dev, "memory_high", "low", "memory 96% used")
	e.srv.EnqueueAlertNotifications(ctx, alert, store.EventOpened)

	all, _ := e.st.ListDeliveries(ctx, store.DeliveryFilter{})
	if len(all) != 0 {
		t.Fatalf("deliveries = %d, want 0 (low alert, high rule)", len(all))
	}
}

func TestDispatchSuppressedDuringMaintenance(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	dev := seedDevice(e, "ws-03")
	ch := newChannel(e, store.ChannelEmail, "ops", `{"host":"smtp.local","from":"a","to":["b"]}`, "")
	newRule(e, ch, "low")

	now := e.clock.now()
	if _, err := e.st.CreateWindow(ctx, store.CreateWindowParams{
		OrgID: &e.org.ID, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), Reason: "patch",
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	alert := seedAlert(e, dev, "offline", "high", "down")
	e.srv.EnqueueAlertNotifications(ctx, alert, store.EventOpened)

	sup, _ := e.st.ListDeliveries(ctx, store.DeliveryFilter{Status: store.DeliverySuppressed})
	if len(sup) != 1 {
		t.Fatalf("suppressed = %d, want 1", len(sup))
	}
	fs := &fakeSender{}
	NewNotifyDispatcher(e.srv, fs).DispatchOnce(ctx)
	if len(fs.emails) != 0 {
		t.Fatalf("suppressed delivery was sent (%d emails)", len(fs.emails))
	}
	sent, _ := e.st.ListDeliveries(ctx, store.DeliveryFilter{Status: store.DeliverySent})
	if len(sent) != 0 {
		t.Fatalf("sent = %d, want 0 while suppressed", len(sent))
	}
}

func TestEnqueueIdempotentAtServer(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	dev := seedDevice(e, "ws-04")
	ch := newChannel(e, store.ChannelEmail, "ops", `{"host":"smtp.local","from":"a","to":["b"]}`, "")
	newRule(e, ch, "low")
	alert := seedAlert(e, dev, "offline", "high", "down")

	// Evaluator runs twice -> still one delivery row.
	e.srv.EnqueueAlertNotifications(ctx, alert, store.EventOpened)
	e.srv.EnqueueAlertNotifications(ctx, alert, store.EventOpened)
	all, _ := e.st.ListDeliveries(ctx, store.DeliveryFilter{})
	if len(all) != 1 {
		t.Fatalf("deliveries after double enqueue = %d, want 1", len(all))
	}
}

func TestDispatchFailingSenderReachesFailed(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	dev := seedDevice(e, "ws-05")
	ch := newChannel(e, store.ChannelWebhook, "ops", `{"url":"https://hooks.test/x","flavor":"generic"}`, "")
	newRule(e, ch, "low")
	alert := seedAlert(e, dev, "offline", "high", "down")
	e.srv.EnqueueAlertNotifications(ctx, alert, store.EventOpened)

	fs := &fakeSender{failErr: fmt.Errorf("connection refused")}
	d := NewNotifyDispatcher(e.srv, fs)

	// Each tick: advance past the backoff so the leased row is claimable again.
	for i := 0; i < notifyMaxAttempts; i++ {
		e.clock.advance(notifyRetryBackoff + time.Second)
		d.DispatchOnce(ctx)
	}

	all, _ := e.st.ListDeliveries(ctx, store.DeliveryFilter{})
	if len(all) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(all))
	}
	got := all[0]
	if got.Status != store.DeliveryFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.Attempts != notifyMaxAttempts {
		t.Fatalf("attempts = %d, want %d", got.Attempts, notifyMaxAttempts)
	}
	if got.LastError == "" {
		t.Fatalf("last_error empty")
	}
	// One more tick does nothing (row is terminal).
	e.clock.advance(notifyRetryBackoff + time.Second)
	d.DispatchOnce(ctx)
	if len(fs.webhooks) != 0 {
		t.Fatalf("terminal delivery should never have succeeded")
	}
}

func TestDispatchTestDelivery(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	ch := newChannel(e, store.ChannelEmail, "ops", `{"host":"smtp.local","from":"a","to":["ops@ex.com"]}`, "")

	id, err := e.srv.EnqueueTestDelivery(ctx, ch)
	if err != nil || id == 0 {
		t.Fatalf("EnqueueTestDelivery id=%d err=%v", id, err)
	}
	fs := &fakeSender{}
	NewNotifyDispatcher(e.srv, fs).DispatchOnce(ctx)

	del, _ := e.st.GetDelivery(ctx, id)
	if del.Status != store.DeliverySent {
		t.Fatalf("test delivery status = %q, want sent", del.Status)
	}
	if len(fs.emails) != 1 || !strings.Contains(fs.emails[0].msg.Subject, "test") {
		t.Fatalf("test email = %+v", fs.emails)
	}
}

// --- real sender: fake SMTP + httptest webhook ------------------------------

func TestStdSenderEmailAgainstFakeSMTP(t *testing.T) {
	srv := startFakeSMTP(t)
	host, port := srv.hostPort()

	s := newStdSender()
	cfg := EmailConfig{Host: host, Port: port, From: "msp@ex.com", To: []string{"ops@ex.com", "oncall@ex.com"}}
	msg := EmailMessage{From: cfg.From, To: cfg.To, Subject: "[MSP] high offline on ws-01 (Acme)", Body: "Device: ws-01\nMessage: no sync for 6m\n"}
	if err := s.SendEmail(context.Background(), cfg, "", msg); err != nil {
		t.Fatalf("SendEmail: %v", err)
	}

	got := srv.wait(t)
	if got.from != "msp@ex.com" {
		t.Fatalf("MAIL FROM = %q", got.from)
	}
	if len(got.to) != 2 || got.to[0] != "ops@ex.com" || got.to[1] != "oncall@ex.com" {
		t.Fatalf("RCPT TO = %v", got.to)
	}
	if !strings.Contains(got.data, "Subject: [MSP] high offline on ws-01 (Acme)") {
		t.Fatalf("missing subject in data:\n%s", got.data)
	}
	if !strings.Contains(got.data, "ws-01") || !strings.Contains(got.data, "no sync for 6m") {
		t.Fatalf("data does not name device/alert:\n%s", got.data)
	}
}

func TestStdSenderWebhookGenericAndSignature(t *testing.T) {
	var (
		gotBody   []byte
		gotSig    string
		gotCT     string
		gotMethod string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotSig = r.Header.Get("X-MSP-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := newStdSender()
	body := []byte(`{"alert_id":9,"event":"opened"}`)
	secret := "sign-me"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if err := s.PostWebhook(context.Background(), ts.URL, "application/json", body, map[string]string{"X-MSP-Signature": want}); err != nil {
		t.Fatalf("PostWebhook: %v", err)
	}
	if gotMethod != http.MethodPost || gotCT != "application/json" {
		t.Fatalf("method %q ct %q", gotMethod, gotCT)
	}
	if string(gotBody) != string(body) {
		t.Fatalf("body = %q", gotBody)
	}
	if gotSig != want {
		t.Fatalf("signature = %q want %q", gotSig, want)
	}
}

func TestStdSenderWebhookNon2xxFails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	s := newStdSender()
	if err := s.PostWebhook(context.Background(), ts.URL, "application/json", []byte("{}"), nil); err == nil {
		t.Fatal("PostWebhook to a 500 should fail")
	}
}

// --- renderers and redaction (pure) -----------------------------------------

func TestRenderers(t *testing.T) {
	nc := notifyContext{
		AlertID: ptrI64(9), OrgName: "Acme", Device: "ws-01", Type: "offline", Severity: "high",
		Message: "no sync for 6m", Event: store.EventOpened, At: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC),
		BaseURL: "https://msp.example.com",
	}
	subject, body := renderEmail(nc)
	if subject != "[MSP] high offline on ws-01 (Acme)" {
		t.Fatalf("subject = %q", subject)
	}
	if !strings.Contains(body, "ws-01") || !strings.Contains(body, "no sync for 6m") ||
		!strings.Contains(body, "https://msp.example.com/") {
		t.Fatalf("body = %q", body)
	}

	var slack map[string]string
	json.Unmarshal(renderSlack(nc), &slack)
	if !strings.HasPrefix(slack["text"], "[MSP] high offline on ws-01 (Acme)") {
		t.Fatalf("slack = %q", slack["text"])
	}

	var gen map[string]any
	json.Unmarshal(renderGeneric(nc), &gen)
	for _, k := range []string{"alert_id", "org", "device", "type", "severity", "message", "event", "at"} {
		if _, ok := gen[k]; !ok {
			t.Fatalf("generic payload missing %q: %v", k, gen)
		}
	}
	if gen["device"] != "ws-01" || gen["severity"] != "high" {
		t.Fatalf("generic payload = %v", gen)
	}

	// Resolved event reads differently; test event is self-describing.
	rnc := nc
	rnc.Event = store.EventResolved
	rs, _ := renderEmail(rnc)
	if !strings.Contains(rs, "resolved") {
		t.Fatalf("resolved subject = %q", rs)
	}
	tnc := notifyContext{OrgName: "Acme", Event: store.EventTest, Message: "Test notification from MSP", At: nc.At}
	ts, tb := renderEmail(tnc)
	if !strings.Contains(ts, "test") || !strings.Contains(tb, "Test notification") {
		t.Fatalf("test render = %q / %q", ts, tb)
	}
}

func TestRedaction(t *testing.T) {
	if got := redactURL("https://hooks.slack.com/services/T000/B000/XXXSECRETTOKEN"); strings.Contains(got, "XXXSECRETTOKEN") {
		t.Fatalf("redactURL kept the token: %q", got)
	}
	if got := redactURL("https://user:pass@host/path?token=abc"); strings.Contains(got, "pass") || strings.Contains(got, "token=abc") {
		t.Fatalf("redactURL kept credentials/query: %q", got)
	}
	err := fmt.Errorf("webhook post: dial https://hooks.test/services/SECRET123?x=1 failed")
	msg := redactError(err, "")
	if strings.Contains(msg, "SECRET123") || strings.Contains(msg, "x=1") {
		t.Fatalf("redactError leaked URL token: %q", msg)
	}
	// Regression (NS1 low finding): redactError strips the channel secret when it
	// is known, honouring the docs/SECURITY.md guarantee even if an underlying
	// library error ever echoed the credential.
	secretErr := fmt.Errorf("smtp auth failed for password hunter2-smtp-pw")
	if got := redactError(secretErr, "hunter2-smtp-pw"); strings.Contains(got, "hunter2-smtp-pw") {
		t.Fatalf("redactError leaked the channel secret: %q", got)
	}
}

// --- fake SMTP server -------------------------------------------------------

type smtpCapture struct {
	from string
	to   []string
	data string
}

type fakeSMTPServer struct {
	ln   net.Listener
	done chan smtpCapture
}

func startFakeSMTP(t *testing.T) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTPServer{ln: ln, done: make(chan smtpCapture, 1)}
	t.Cleanup(func() { ln.Close() })
	go f.accept()
	return f
}

func (f *fakeSMTPServer) hostPort() (string, int) {
	a := f.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (f *fakeSMTPServer) wait(t *testing.T) smtpCapture {
	t.Helper()
	select {
	case c := <-f.done:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("fake SMTP: no message received")
		return smtpCapture{}
	}
}

func (f *fakeSMTPServer) accept() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(s string) { conn.Write([]byte(s)) }

	w("220 mock ESMTP ready\r\n")
	var cap smtpCapture
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
			f.done <- cap
			return
		default:
			w("250 OK\r\n")
		}
	}
}

// addrBetween extracts the address between < and > in "MAIL FROM:<a@b>".
func addrBetween(line string) string {
	i := strings.IndexByte(line, '<')
	j := strings.IndexByte(line, '>')
	if i >= 0 && j > i {
		return line[i+1 : j]
	}
	// Fallback: everything after the colon.
	if k := strings.IndexByte(line, ':'); k >= 0 {
		return strings.TrimSpace(line[k+1:])
	}
	return ""
}

func ptrI64(v int64) *int64 { return &v }

// TestBuildMIMENoHeaderInjection is the regression for the SMTP header/body
// injection finding: a hostname carrying CRLF reaches the Subject via
// renderSubject, and buildMIME must neutralise it so no attacker-controlled
// header (e.g. Bcc) appears as its own line and the header block is not
// terminated early.
func TestBuildMIMENoHeaderInjection(t *testing.T) {
	nc := notifyContext{
		OrgName:  "Acme",
		Device:   "host\r\nBcc: attacker@evil.test\r\n\r\ninjected body",
		Type:     "disk_full",
		Severity: "medium",
		Event:    store.EventOpened,
		At:       time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC),
	}
	subject, body := renderEmail(nc)
	// A recipient/sender with CRLF must also be neutralised.
	msg := EmailMessage{From: "msp@ex.com", To: []string{"ops@ex.com\r\nBcc: x@evil.test"}, Subject: subject, Body: body}
	mime := buildMIME(msg)

	header := mime
	if i := strings.Index(mime, "\r\n\r\n"); i >= 0 {
		header = mime[:i]
	}
	allowed := []string{"From:", "To:", "Subject:", "MIME-Version:", "Content-Type:"}
	for _, line := range strings.Split(header, "\r\n") {
		if line == "" {
			continue
		}
		ok := false
		for _, p := range allowed {
			if strings.HasPrefix(line, p) {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("injected header line in the header block: %q\nfull header:\n%s", line, header)
		}
	}
	if strings.Count(mime, "Subject: ") != 1 {
		t.Fatalf("Subject header does not appear exactly once:\n%s", header)
	}
}

// TestDispatchWebhookSSRFBlocked is the regression for the webhook SSRF finding:
// the production dispatcher refuses to POST to an internal/loopback/metadata
// address even when the channel was stored out of band (bypassing the validating
// API), never calling the sender and landing the delivery failed.
func TestDispatchWebhookSSRFBlocked(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	dev := seedDevice(e, "ws-ssrf")
	ch := newChannel(e, store.ChannelWebhook, "evil", `{"url":"http://169.254.169.254/latest/meta-data/","flavor":"generic"}`, "")
	newRule(e, ch, "low")
	alert := seedAlert(e, dev, "offline", "high", "down")
	e.srv.EnqueueAlertNotifications(ctx, alert, store.EventOpened)

	fs := &fakeSender{}
	d := NewNotifyDispatcher(e.srv, fs) // blockPrivateWebhooks defaults to true
	for i := 0; i < notifyMaxAttempts; i++ {
		e.clock.advance(notifyRetryBackoff + time.Second)
		d.DispatchOnce(ctx)
	}
	if len(fs.webhooks) != 0 {
		t.Fatalf("SSRF webhook was sent %d time(s); the dispatcher must block it", len(fs.webhooks))
	}
	all, _ := e.st.ListDeliveries(ctx, store.DeliveryFilter{})
	if len(all) != 1 || all[0].Status != store.DeliveryFailed {
		t.Fatalf("SSRF delivery = %+v, want exactly 1 failed", all)
	}
	if !strings.Contains(all[0].LastError, "blocked") {
		t.Fatalf("last_error = %q, want a 'blocked' reason", all[0].LastError)
	}
}
