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

// SMSConfig is the non-secret half of an SMS channel's config_json (NS2); the
// provider auth token is the channel's separate secret. For "twilio" the Messages
// API is addressed by AccountSID (URL is an optional test/self-hosted override);
// for "generic" URL is the POST target.
type SMSConfig struct {
	Provider   string   `json:"provider"` // "twilio" or "generic"
	From       string   `json:"from"`
	To         []string `json:"to"`
	AccountSID string   `json:"account_sid"`
	URL        string   `json:"url"`
}

// SMSMessage is a rendered short message bound for one or more recipients.
type SMSMessage struct {
	From string
	To   []string
	Text string
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
	// SendSMS delivers msg through the configured provider (NS2). token is the
	// channel secret (the provider auth token); it never reaches a log or a
	// delivery's detail_json.
	SendSMS(ctx context.Context, cfg SMSConfig, token string, msg SMSMessage) error
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
	// blockPrivateWebhooks re-checks each webhook URL against the SSRF guard
	// (validateWebhookURL) before sending, so a channel whose URL was stored out
	// of band (not through the validating API) still cannot reach an internal
	// address. True in production; the in-process e2e test disables it because it
	// drives httptest webhook servers on loopback.
	blockPrivateWebhooks bool
}

// NewNotifyDispatcher builds a dispatcher for a server. baseURL (cfg.BaseURL) is
// used for the dashboard link in email bodies; the clock is the server's so a
// test that moves the store clock moves the dispatcher too.
func NewNotifyDispatcher(s *Server, sender Sender) *NotifyDispatcher {
	return &NotifyDispatcher{
		st:                   s.st,
		log:                  s.log,
		sender:               sender,
		now:                  s.now,
		baseURL:              strings.TrimRight(s.cfg.BaseURL, "/"),
		maxAttempts:          notifyMaxAttempts,
		tick:                 notifyTickEvery,
		backoff:              notifyRetryBackoff,
		blockPrivateWebhooks: true,
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

// DispatchOnce runs one escalation + claim/send/mark pass. It first enqueues any
// escalation steps that have come due (NS2), then claims and sends pending
// deliveries, so a due step is enqueued and sent within the same tick. It is
// exported for the in-process e2e test and for the retry loop in Run.
func (d *NotifyDispatcher) DispatchOnce(ctx context.Context) {
	d.EscalateOnce(ctx)
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

// EscalateOnce enqueues the escalation steps (NS2) that have come due for every
// open, unacknowledged alert whose matched rule carries a policy. Each due step
// enqueues one pending delivery per channel, keyed by
// (alert, rule, channel, event opened, step), so a repeated tick is idempotent
// and never double-sends; a step is enqueued suppressed when the alert's org is
// in an active maintenance window, exactly like the NS1 open/resolve path.
// Escalation stops the moment an alert is acknowledged or resolved because
// DueEscalations excludes those alerts. DispatchOnce runs this before its claim
// pass; the integrator may also call it from the evaluator tick.
func (d *NotifyDispatcher) EscalateOnce(ctx context.Context) {
	now := d.now()
	due, err := d.st.DueEscalations(ctx, now)
	if err != nil {
		d.log.Error("notify escalate: due", "err", err)
		return
	}
	for _, de := range due {
		suppressed, err := d.st.ActiveMaintenance(ctx, de.Alert.OrgID, now)
		if err != nil {
			d.log.Error("notify escalate: maintenance", "org", de.Alert.OrgID, "err", err)
			continue
		}
		status := store.DeliveryPending
		if suppressed {
			status = store.DeliverySuppressed
		}
		for _, chID := range de.Step.ChannelIDs {
			alertID := de.Alert.ID
			ruleID := de.Rule.ID
			channelID := chID
			orgID := de.Alert.OrgID
			if _, _, err := d.st.EnqueueDelivery(ctx, store.EnqueueDeliveryParams{
				AlertID:   &alertID,
				RuleID:    &ruleID,
				ChannelID: &channelID,
				OrgID:     &orgID,
				Event:     store.EventOpened,
				Step:      de.Step.StepOrder,
				Status:    status,
			}); err != nil {
				d.log.Error("notify escalate: enqueue", "alert", alertID, "rule", ruleID,
					"step", de.Step.StepOrder, "err", err)
			}
		}
	}
}

// deliver sends one claimed delivery and marks it. A send failure increments the
// attempt count and either retries later (still pending) or, at maxAttempts,
// lands the row failed. Errors are redacted of any secret or token before they
// reach last_error or the log.
func (d *NotifyDispatcher) deliver(ctx context.Context, del store.NotificationDelivery) {
	// NS2: escalation must "stop entirely" once the alert is acknowledged or
	// resolved. DueEscalations stops ENQUEUEING new steps on ack/resolve, but a
	// step already enqueued -- e.g. one left pending by a transient provider
	// failure -- is still claimed here, so re-check the alert at send time and
	// suppress the step rather than page after the operator has taken the alert.
	if superseded, serr := d.escalationSuperseded(ctx, del); serr != nil {
		// A transient read error should not permanently wedge the delivery; log and
		// fall through to the normal send path (which will itself fail safely if the
		// alert is truly gone).
		d.log.Warn("notify dispatch: escalation state", "delivery", del.ID, "err", serr)
	} else if superseded {
		if mErr := d.st.MarkDeliverySuppressed(ctx, del.ID); mErr != nil {
			d.log.Error("notify dispatch: mark suppressed", "delivery", del.ID, "err", mErr)
		}
		return
	}
	detail, err := d.send(ctx, del)
	if err == nil {
		if mErr := d.st.MarkDeliverySent(ctx, del.ID, detail); mErr != nil {
			d.log.Error("notify dispatch: mark sent", "delivery", del.ID, "err", mErr)
		}
		return
	}
	terminal := del.Attempts+1 >= d.maxAttempts
	// Load the channel secret (best effort) so redactError can strip it from the
	// message as documented, even if an underlying SMTP/HTTP error ever echoed it.
	secret := ""
	if del.ChannelID != nil {
		if ch, cerr := d.st.GetChannel(ctx, *del.ChannelID); cerr == nil {
			secret = ch.Secret
		}
	}
	msg := redactError(err, secret)
	if mErr := d.st.MarkDeliveryFailed(ctx, del.ID, msg, terminal); mErr != nil {
		d.log.Error("notify dispatch: mark failed", "delivery", del.ID, "err", mErr)
	}
	d.log.Warn("notify delivery failed", "delivery", del.ID, "attempt", del.Attempts+1,
		"terminal", terminal, "error", msg)
}

// escalationSuperseded reports whether del is an escalation-step delivery
// (step >= 1, event opened) whose alert has since been acknowledged or resolved,
// in which case the step must not be sent (NS2 "stop entirely"). A non-escalation
// delivery (step 0), a test delivery or a delivery with no alert is never
// superseded, so the NS1 open/resolve/test semantics are unchanged.
func (d *NotifyDispatcher) escalationSuperseded(ctx context.Context, del store.NotificationDelivery) (bool, error) {
	if del.Step < 1 || del.Event != store.EventOpened || del.AlertID == nil {
		return false, nil
	}
	alert, err := d.st.GetAlert(ctx, *del.AlertID)
	if err != nil {
		return false, err
	}
	return alert.AcknowledgedAt != nil || alert.ResolvedAt != nil, nil
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
	case store.ChannelSMS:
		return d.sendSMS(sctx, ch, nc)
	default:
		return "", fmt.Errorf("unknown channel kind %q", ch.Kind)
	}
}

// sendSMS renders the short message and hands it to the provider through the
// Sender. The provider token (ch.Secret) is passed to the sender but never
// returned in the detail; the detail records only the provider and recipients
// (with any URL reduced to scheme://host).
func (d *NotifyDispatcher) sendSMS(ctx context.Context, ch store.NotifyChannel, nc notifyContext) (string, error) {
	var cfg SMSConfig
	if err := json.Unmarshal([]byte(ch.ConfigJSON), &cfg); err != nil {
		return "", fmt.Errorf("bad sms config: %w", err)
	}
	if reason := validateSMSConfigForSend(cfg); reason != "" {
		return "", fmt.Errorf("sms channel %d invalid: %s", ch.ID, reason)
	}
	// Send-time SSRF re-check, mirroring sendWebhook: a channel stored out of band
	// (not through the validating dashboard API) must still not reach an internal
	// address. The generic provider POSTs to cfg.URL; a twilio channel may carry a
	// url override that stdSender.SendSMS honours. A twilio channel with no override
	// (cfg.URL == "") addresses the fixed public api.twilio.com endpoint and needs
	// no check.
	if d.blockPrivateWebhooks && cfg.URL != "" {
		if detail := validateWebhookURL(cfg.URL); detail != "" {
			return "", fmt.Errorf("sms channel %d blocked: %s", ch.ID, detail)
		}
	}
	msg := SMSMessage{From: cfg.From, To: cfg.To, Text: renderSMS(nc)}
	if err := d.sender.SendSMS(ctx, cfg, ch.Secret, msg); err != nil {
		return "", err
	}
	detail := map[string]any{"kind": "sms", "provider": cfg.Provider, "to": cfg.To, "count": len(cfg.To)}
	if cfg.URL != "" {
		detail["url"] = redactURL(cfg.URL)
	}
	b, _ := json.Marshal(detail)
	return string(b), nil
}

// validateSMSConfigForSend is the dispatcher's defence-in-depth check before a
// send: the dashboard validates on write, but a channel stored out of band must
// still not produce a malformed send. It returns "" when the config is usable.
func validateSMSConfigForSend(cfg SMSConfig) string {
	switch cfg.Provider {
	case smsProviderTwilio:
		if cfg.AccountSID == "" && cfg.URL == "" {
			return "twilio provider needs account_sid or url"
		}
	case smsProviderGeneric:
		if cfg.URL == "" {
			return "generic provider needs url"
		}
	default:
		return "unknown provider"
	}
	if len(cfg.To) == 0 {
		return "no recipients"
	}
	return ""
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
	if d.blockPrivateWebhooks {
		if detail := validateWebhookURL(cfg.URL); detail != "" {
			return "", fmt.Errorf("webhook channel %d blocked: %s", ch.ID, detail)
		}
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

// renderSMS returns the short SMS body (NS2): the one-line summary, trimmed to
// ~300 chars, with any URL reduced to scheme://host so a tokened link can never
// ride out in a text message. The body is built only from alert fields (no
// secret), so the redaction is belt-and-braces.
func renderSMS(nc notifyContext) string {
	return truncate(redactURLsIn(renderSubject(nc)), 300)
}

// truncate shortens s to at most n bytes, appending an ellipsis marker when it
// cuts (keeping the whole result within n).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
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
// tokened URL: it strips the channel secret (when known) out of the message, then
// reduces any http(s) URL-looking substring to scheme://host. The error strings
// built in this package never embed the secret, but an underlying SMTP/HTTP
// library error could, so the explicit strip keeps the documented guarantee
// (docs/SECURITY.md section 12) true regardless. Pass secret "" when none is set.
func redactError(err error, secret string) string {
	msg := err.Error()
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "[redacted-secret]")
	}
	return redactURLsIn(msg)
}

// redactURLsIn reduces every http(s) URL-looking substring in msg to
// scheme://host, so an embedded token (a Slack webhook path, a query token) is
// never carried out. Non-URL text is left untouched.
func redactURLsIn(msg string) string {
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

// sanitizeHeader neutralises a value interpolated into an email header so a
// control character it carries cannot inject an extra header or terminate the
// header block early (header/body injection). CR and LF are collapsed to a
// single space; other C0 controls and DEL are dropped. The alert hostname flows
// into the Subject (renderSubject) and the From/To come from channel config, so
// every header value passes through here before buildMIME writes it raw.
func sanitizeHeader(v string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, v)
}

// buildMIME renders the minimal RFC 5322 message bytes for a plain-text email.
// Every header value is sanitized of CR/LF first so a hostname or address with an
// embedded newline cannot forge headers (e.g. an extra Bcc:) or inject a body.
func buildMIME(msg EmailMessage) string {
	to := make([]string, len(msg.To))
	for i, t := range msg.To {
		to[i] = sanitizeHeader(t)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", sanitizeHeader(msg.From))
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitizeHeader(msg.Subject))
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

// twilioMessagesURL is the default Twilio Messages API endpoint for an account;
// a channel may override it (config.url) for a self-hosted gateway or a test.
func twilioMessagesURL(accountSID string) string {
	return "https://api.twilio.com/2010-04-01/Accounts/" + url.PathEscape(accountSID) + "/Messages.json"
}

// SendSMS delivers one short message to each recipient through the configured
// provider (NS2). twilio POSTs a form (From, To, Body) with HTTP basic auth
// (account_sid : token) to the Messages API; generic POSTs a JSON {to, text} with
// the token as a bearer header. One request per recipient keeps the two shapes
// uniform and matches Twilio's per-recipient model. The token is sent only in the
// Authorization header, never in a URL or a returned error.
func (s *stdSender) SendSMS(ctx context.Context, cfg SMSConfig, token string, msg SMSMessage) error {
	switch cfg.Provider {
	case smsProviderTwilio:
		endpoint := cfg.URL
		if endpoint == "" {
			endpoint = twilioMessagesURL(cfg.AccountSID)
		}
		for _, to := range msg.To {
			form := url.Values{}
			form.Set("From", msg.From)
			form.Set("To", to)
			form.Set("Body", msg.Text)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
			if err != nil {
				return fmt.Errorf("sms request: %w", err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("User-Agent", "msp-server")
			req.SetBasicAuth(cfg.AccountSID, token)
			if err := s.doSMS(req); err != nil {
				return err
			}
		}
		return nil
	case smsProviderGeneric:
		for _, to := range msg.To {
			body, _ := json.Marshal(map[string]string{"to": to, "text": msg.Text})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, strings.NewReader(string(body)))
			if err != nil {
				return fmt.Errorf("sms request: %w", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "msp-server")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			if err := s.doSMS(req); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown sms provider %q", cfg.Provider)
	}
}

// doSMS performs one SMS HTTP request and treats any non-2xx as a failure. The
// response body is discarded so a provider error page cannot echo a credential
// back into a log.
func (s *stdSender) doSMS(req *http.Request) error {
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("sms post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sms status %d", resp.StatusCode)
	}
	return nil
}
