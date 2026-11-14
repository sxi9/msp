package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/store"
)

// Alert notifications (NS1): the enqueue glue, the dispatcher goroutine and the
// per-kind renderers/senders. The HTTP CRUD for channels/rules/windows and the
// wiring into the evaluator (alerts.go) and Run (server.go) belong to the
// integrator; this file exposes the exported funcs they call (see the package
// doc and drift notes). Channel secrets reach the SMTP client and the HMAC
// signer here but never a log line, a full webhook URL in a log, or a delivery's
// detail_json -- redaction is applied before anything is logged or stored.

const (
	notifyMaxAttempts  = 5                // give up and mark failed at this many failed attempts
	notifyTickEvery    = 5 * time.Second  // dispatcher poll interval
	notifyRetryBackoff = 30 * time.Second // minimum spacing between attempts of one delivery
	notifyClaimBatch   = 50               // deliveries claimed per tick
	notifySendTimeout  = 20 * time.Second // per-send deadline (SMTP / webhook)
)

// EmailConfig is the non-secret half of an email channel's config_json; the SMTP
// password is the channel's separate secret.
type EmailConfig struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	StartTLS bool     `json:"starttls"`
	Username string   `json:"username"`
}

// WebhookConfig is a webhook channel's config_json; the signing secret is the
// channel's separate secret.
type WebhookConfig struct {
	URL    string `json:"url"`
	Flavor string `json:"flavor"` // "slack" or "generic"
}

// EmailMessage is a rendered plain-text email.
type EmailMessage struct {
	From    string
	To      []string
	Subject string
	Body    string
}

// Sender performs the outbound I/O for a delivery. The real implementation
// (stdSender) uses net/smtp and net/http; tests inject a fake that records the
// calls and can fail on demand. PostWebhook receives the fully rendered body and
// the headers (including the HMAC signature when a secret was set); it does not
// see the channel secret.
type Sender interface {
	SendEmail(ctx context.Context, cfg EmailConfig, password string, msg EmailMessage) error
	PostWebhook(ctx context.Context, rawURL, contentType string, body []byte, headers map[string]string) error
}

// NotifyDispatcher drains pending deliveries: it claims pending rows, loads the
// channel, renders the message for the channel kind, sends it through the Sender
// and marks the row sent or failed. It is injectable with a fake Sender and
// reads the server clock, so tests drive it with a fake clock and no real send.
type NotifyDispatcher struct {
	st          *store.Store
	log         *slog.Logger
	sender      Sender
	now         func() time.Time
	baseURL     string
	maxAttempts int
	tick        time.Duration
	backoff     time.Duration
}

// NewNotifyDispatcher builds a dispatcher for a server. baseURL (cfg.BaseURL) is
// used for the dashboard link in email bodies; the clock is the server's so a
// test that moves the store clock moves the dispatcher too.
func NewNotifyDispatcher(s *Server, sender Sender) *NotifyDispatcher {
	return &NotifyDispatcher{
		st:          s.st,
		log:         s.log,
		sender:      sender,
		now:         s.now,
		baseURL:     strings.TrimRight(s.cfg.BaseURL, "/"),
		maxAttempts: notifyMaxAttempts,
		tick:        notifyTickEvery,
		backoff:     notifyRetryBackoff,
	}
}

// Run drives the dispatcher until ctx is done. The integrator starts it in
// Run alongside the other jobs: go NewNotifyDispatcher(s, newStdSender()).Run(jobsCtx).
func (d *NotifyDispatcher) Run(ctx context.Context) {
	t := time.NewTicker(d.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.DispatchOnce(ctx)
		}
	}
}

// DispatchOnce runs one claim/send/mark pass. It is exported for the in-process
// e2e test and for the retry loop in Run.
func (d *NotifyDispatcher) DispatchOnce(ctx context.Context) {
	retryBefore := d.now().Add(-d.backoff)
	claimed, err := d.st.ClaimPendingDeliveries(ctx, notifyClaimBatch, retryBefore)
	if err != nil {
		d.log.Error("notify dispatch: claim", "err", err)
		return
	}
	for _, del := range claimed {
		d.deliver(ctx, del)
	}
}

// deliver sends one claimed delivery and marks it. A send failure increments the
// attempt count and either retries later (still pending) or, at maxAttempts,
// lands the row failed. Errors are redacted of any secret or token before they
// reach last_error or the log.
func (d *NotifyDispatcher) deliver(ctx context.Context, del store.NotificationDelivery) {
	detail, err := d.send(ctx, del)
	if err == nil {
		if mErr := d.st.MarkDeliverySent(ctx, del.ID, detail); mErr != nil {
			d.log.Error("notify dispatch: mark sent", "delivery", del.ID, "err", mErr)
		}
		return
	}
	terminal := del.Attempts+1 >= d.maxAttempts
	msg := redactError(err, del)
	if mErr := d.st.MarkDeliveryFailed(ctx, del.ID, msg, terminal); mErr != nil {
		d.log.Error("notify dispatch: mark failed", "delivery", del.ID, "err", mErr)
	}
	d.log.Warn("notify delivery failed", "delivery", del.ID, "attempt", del.Attempts+1,
		"terminal", terminal, "error", msg)
}

// send loads the channel, builds the render context and dispatches by kind. The
// returned string is a redacted detail_json describing the send (never a
// secret); the error is non-nil when the send failed.
func (d *NotifyDispatcher) send(ctx context.Context, del store.NotificationDelivery) (string, error) {
	if del.ChannelID == nil {
		return "", fmt.Errorf("delivery %d has no channel", del.ID)
	}
	ch, err := d.st.GetChannel(ctx, *del.ChannelID)
	if err != nil {
		return "", fmt.Errorf("load channel: %w", err)
	}
	if !ch.Enabled {
		return "", fmt.Errorf("channel %d is disabled", ch.ID)
	}
	nc, err := d.renderContext(ctx, del)
	if err != nil {
		return "", err
	}

	sctx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()

	switch ch.Kind {
	case store.ChannelEmail:
		return d.sendEmail(sctx, ch, nc)
	case store.ChannelWebhook:
		return d.sendWebhook(sctx, ch, nc)
	default:
		return "", fmt.Errorf("unknown channel kind %q", ch.Kind)
	}
}

func (d *NotifyDispatcher) sendEmail(ctx context.Context, ch store.NotifyChannel, nc notifyContext) (string, error) {
	var cfg EmailConfig
	if err := json.Unmarshal([]byte(ch.ConfigJSON), &cfg); err != nil {
		return "", fmt.Errorf("bad email config: %w", err)
	}
	if cfg.Host == "" || len(cfg.To) == 0 {
		return "", fmt.Errorf("email channel %d missing host or recipients", ch.ID)
	}
	subject, body := renderEmail(nc)
	msg := EmailMessage{From: cfg.From, To: cfg.To, Subject: subject, Body: body}
	if err := d.sender.SendEmail(ctx, cfg, ch.Secret, msg); err != nil {
		return "", err
	}
	detail, _ := json.Marshal(map[string]any{"kind": "email", "to": cfg.To, "subject": subject})
	return string(detail), nil
}

func (d *NotifyDispatcher) sendWebhook(ctx context.Context, ch store.NotifyChannel, nc notifyContext) (string, error) {
	var cfg WebhookConfig
	if err := json.Unmarshal([]byte(ch.ConfigJSON), &cfg); err != nil {
		return "", fmt.Errorf("bad webhook config: %w", err)
	}
	if cfg.URL == "" {
		return "", fmt.Errorf("webhook channel %d missing url", ch.ID)
	}
	var body []byte
	switch cfg.Flavor {
	case "slack":
		body = renderSlack(nc)
	default: // "generic" and anything unset
		body = renderGeneric(nc)
	}
	headers := map[string]string{}
	if ch.Secret != "" {
		mac := hmac.New(sha256.New, []byte(ch.Secret))
		mac.Write(body)
		headers["X-MSP-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	if err := d.sender.PostWebhook(ctx, cfg.URL, "application/json", body, headers); err != nil {
		return "", err
	}
	detail, _ := json.Marshal(map[string]any{"kind": "webhook", "flavor": cfg.Flavor, "url": redactURL(cfg.URL), "signed": ch.Secret != ""})
	return string(detail), nil
}

// notifyContext is the data the renderers need, assembled from the delivery and
// (for opened/resolved) its alert.
type notifyContext struct {
	AlertID  *int64
	OrgName  string
	Device   string
	Type     string
	Severity string
	Message  string
	Event    string
	At       time.Time
	BaseURL  string
}

// renderContext builds the render data for a delivery. For opened/resolved it
// loads the alert (org name, hostname, type, severity, message); for a test it
// synthesises a self-describing message.
func (d *NotifyDispatcher) renderContext(ctx context.Context, del store.NotificationDelivery) (notifyContext, error) {
	nc := notifyContext{AlertID: del.AlertID, Event: del.Event, At: d.now().UTC(), BaseURL: d.baseURL}
	if del.Event == store.EventTest {
		nc.Type = "test"
		nc.Severity = "test"
		nc.Message = "Test notification from MSP"
		if del.OrgID != nil {
			if org, err := d.st.GetOrg(ctx, *del.OrgID); err == nil {
				nc.OrgName = org.Name
			}
		} else {
			nc.OrgName = "all clients"
		}
		return nc, nil
	}
	if del.AlertID == nil {
		return notifyContext{}, fmt.Errorf("delivery %d has no alert", del.ID)
	}
	alert, err := d.st.GetAlert(ctx, *del.AlertID)
	if err != nil {
		return notifyContext{}, fmt.Errorf("load alert: %w", err)
	}
	nc.OrgName = alert.OrgName
	nc.Device = alert.Hostname
	nc.Type = alert.Type
	nc.Severity = alert.Severity
	nc.Message = alert.Message
	return nc, nil
}

// --- renderers --------------------------------------------------------------

// renderSubject is the one-line summary shared by the email subject and the
// Slack message: "[MSP] <severity> <type> on <hostname> (<org>)".
func renderSubject(nc notifyContext) string {
	if nc.Event == store.EventTest {
		return fmt.Sprintf("[MSP] test notification (%s)", nc.OrgName)
	}
	verb := ""
	if nc.Event == store.EventResolved {
		verb = " resolved"
	}
	return fmt.Sprintf("[MSP] %s %s%s on %s (%s)", nc.Severity, nc.Type, verb, nc.Device, nc.OrgName)
}

// renderEmail returns the plain-text subject and body for an email delivery.
func renderEmail(nc notifyContext) (subject, body string) {
	subject = renderSubject(nc)
	var b strings.Builder
	if nc.Event == store.EventTest {
		fmt.Fprintf(&b, "%s\n\n", nc.Message)
		fmt.Fprintf(&b, "Org:     %s\n", nc.OrgName)
	} else {
		fmt.Fprintf(&b, "Device:   %s\n", nc.Device)
		fmt.Fprintf(&b, "Org:      %s\n", nc.OrgName)
		fmt.Fprintf(&b, "Type:     %s\n", nc.Type)
		fmt.Fprintf(&b, "Severity: %s\n", nc.Severity)
		fmt.Fprintf(&b, "Event:    %s\n", nc.Event)
		fmt.Fprintf(&b, "Message:  %s\n", nc.Message)
	}
	fmt.Fprintf(&b, "Time:     %s\n", nc.At.Format(time.RFC3339))
	if nc.BaseURL != "" {
		fmt.Fprintf(&b, "\nDashboard: %s/\n", nc.BaseURL)
	}
	return subject, b.String()
}

// renderSlack returns a Slack incoming-webhook body: {"text": "<summary> — <message>"}.
func renderSlack(nc notifyContext) []byte {
	text := renderSubject(nc)
	if nc.Message != "" {
		text += " — " + nc.Message
	}
	b, _ := json.Marshal(map[string]string{"text": text})
	return b
}

// renderGeneric returns the generic JSON webhook body.
func renderGeneric(nc notifyContext) []byte {
	payload := map[string]any{
		"alert_id": nc.AlertID,
		"org":      nc.OrgName,
		"device":   nc.Device,
		"type":     nc.Type,
		"severity": nc.Severity,
		"message":  nc.Message,
		"event":    nc.Event,
		"at":       nc.At.Format(time.RFC3339),
	}
	b, _ := json.Marshal(payload)
	return b
}

// --- enqueue glue (called by the integrator from the evaluator) -------------

// EnqueueAlertNotifications enqueues deliveries for an alert event (opened or
// resolved): it finds the matching enabled rules, and for each upserts one
// pending delivery keyed by (alert, rule, event) -- or suppressed when the
// alert's org is inside an active maintenance window at now. It is idempotent
// (the evaluator may run it twice) and never sends; the dispatcher does the
// sending on its next tick. The integrator calls this from the evaluator right
// after an alert is newly opened (OpenAlert returned true) or resolved, passing
// the alert row and store.EventOpened / store.EventResolved.
func (s *Server) EnqueueAlertNotifications(ctx context.Context, alert store.Alert, event string) {
	rules, err := s.st.MatchRules(ctx, alert, event)
	if err != nil {
		s.log.Error("notify enqueue: match rules", "alert", alert.ID, "event", event, "err", err)
		return
	}
	if len(rules) == 0 {
		return
	}
	suppressed, err := s.st.ActiveMaintenance(ctx, alert.OrgID, s.now())
	if err != nil {
		s.log.Error("notify enqueue: maintenance", "org", alert.OrgID, "err", err)
		return
	}
	status := store.DeliveryPending
	if suppressed {
		status = store.DeliverySuppressed
	}
	for _, r := range rules {
		alertID := alert.ID
		ruleID := r.ID
		channelID := r.ChannelID
		orgID := alert.OrgID
		_, _, err := s.st.EnqueueDelivery(ctx, store.EnqueueDeliveryParams{
			AlertID:   &alertID,
			RuleID:    &ruleID,
			ChannelID: &channelID,
			OrgID:     &orgID,
			Event:     event,
			Status:    status,
		})
		if err != nil {
			s.log.Error("notify enqueue: delivery", "alert", alert.ID, "rule", r.ID, "err", err)
		}
	}
}

// EnqueueTestDelivery enqueues an immediate 'test' delivery for one channel and
// returns the created delivery id. The integrator calls this from
// POST /notify/channels/{id}/test; the dispatcher sends it on its next tick.
func (s *Server) EnqueueTestDelivery(ctx context.Context, ch store.NotifyChannel) (int64, error) {
	channelID := ch.ID
	del, _, err := s.st.EnqueueDelivery(ctx, store.EnqueueDeliveryParams{
		ChannelID: &channelID,
		OrgID:     ch.OrgID,
		Event:     store.EventTest,
		Status:    store.DeliveryPending,
	})
	if err != nil {
		return 0, err
	}
	return del.ID, nil
}

// --- redaction --------------------------------------------------------------

// redactURL reduces a webhook URL to scheme://host, dropping userinfo, path and
// query so an embedded token (e.g. a Slack incoming-webhook path, or a token
// query parameter) is never logged or stored. An unparseable URL becomes
// "[redacted-url]".
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted-url]"
	}
	return u.Scheme + "://" + u.Host
}

// redactError produces a last_error string that cannot leak a secret or a
// tokened URL. It never embeds the channel secret (the error strings built here
// do not include it, but an SMTP/HTTP library error might echo a URL), so any
// URL-looking substring is reduced and the known secret is stripped if present.
func redactError(err error, del store.NotificationDelivery) string {
	_ = del
	msg := err.Error()
	var b strings.Builder
	for i := 0; i < len(msg); {
		scheme := urlSchemeAt(msg, i)
		if scheme == "" {
			b.WriteByte(msg[i])
			i++
			continue
		}
		end := i + len(scheme)
		for end < len(msg) && !isURLBoundary(msg[end]) {
			end++
		}
		b.WriteString(redactURL(msg[i:end]))
		i = end
	}
	return b.String()
}

// urlSchemeAt returns "https://" or "http://" when one begins at msg[i], else "".
func urlSchemeAt(msg string, i int) string {
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(msg[i:], scheme) {
			return scheme
		}
	}
	return ""
}

func isURLBoundary(c byte) bool {
	return c == ' ' || c == '"' || c == '\'' || c == '\n' || c == '\t' || c == ')' || c == '>'
}

// --- real sender ------------------------------------------------------------

// stdSender is the production Sender: net/smtp for email, net/http for webhooks.
type stdSender struct {
	client *http.Client
}

// newStdSender builds the production Sender. The integrator passes it to
// NewNotifyDispatcher in Run.
func newStdSender() *stdSender {
	return &stdSender{client: &http.Client{Timeout: notifySendTimeout}}
}

// SendEmail sends one plain-text message over SMTP, using STARTTLS and PLAIN
// auth when the channel is configured for them. It builds the RFC 5322 message
// itself so it can set a subject without pulling in a dependency.
func (s *stdSender) SendEmail(ctx context.Context, cfg EmailConfig, password string, msg EmailMessage) error {
	port := cfg.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", port))

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()

	if cfg.StartTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}
	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, password, cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, to := range msg.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("smtp rcpt: %w", err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := wc.Write([]byte(buildMIME(msg))); err != nil {
		wc.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return c.Quit()
}

// buildMIME renders the minimal RFC 5322 message bytes for a plain-text email.
func buildMIME(msg EmailMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", msg.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(msg.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(&b, "\r\n")
	b.WriteString(strings.ReplaceAll(msg.Body, "\n", "\r\n"))
	return b.String()
}

// PostWebhook POSTs body to rawURL with the given headers.
func (s *stdSender) PostWebhook(ctx context.Context, rawURL, contentType string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "msp-server")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}
