package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Alert notifications (NS1). Channels, rules, maintenance windows and the
// delivery log live here. Every type carries an *int64 OrgID (nil = a global
// row usable by / matching every client) so the server can scope staff
// visibility exactly like RS2 through orgInClause. Channel secrets are stored in
// the clear (they are credentials used to authenticate or sign outbound sends,
// not password hashes); they are loaded into the structs for the dispatcher but
// tagged json:"-" so they never leave over the API -- callers expose SecretSet.

// Channel kinds and delivery events/statuses (schema CHECK constraints).
const (
	ChannelEmail   = "email"
	ChannelWebhook = "webhook"
	ChannelSMS     = "sms"

	EventOpened   = "opened"
	EventResolved = "resolved"
	EventTest     = "test"

	DeliveryPending    = "pending"
	DeliverySent       = "sent"
	DeliveryFailed     = "failed"
	DeliverySuppressed = "suppressed"
)

// NotifyChannel is an email or webhook destination. Secret is loaded for the
// dispatcher but never serialised; SecretSet reports whether one is set so the
// API can show "secret_set":bool without returning the value.
type NotifyChannel struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	OrgID      *int64    `json:"org_id"`
	ConfigJSON string    `json:"config_json"`
	Secret     string    `json:"-"`
	SecretSet  bool      `json:"secret_set"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  *int64    `json:"created_by"`
}

// NotifyRule routes matching alerts to one channel. AlertTypes is a comma list
// (empty = all types); MinSeverity gates by severity; OnOpen/OnResolve gate by
// event.
type NotifyRule struct {
	ID                 int64     `json:"id"`
	Name               string    `json:"name"`
	OrgID              *int64    `json:"org_id"`
	MinSeverity        string    `json:"min_severity"`
	AlertTypes         string    `json:"alert_types"`
	OnOpen             bool      `json:"on_open"`
	OnResolve          bool      `json:"on_resolve"`
	ChannelID          int64     `json:"channel_id"`
	EscalationPolicyID *int64    `json:"escalation_policy_id"` // NS2: nil = no policy
	Enabled            bool      `json:"enabled"`
	CreatedAt          time.Time `json:"created_at"`
	CreatedBy          *int64    `json:"created_by"`
}

// MaintenanceWindow suppresses an org's (or, OrgID nil, every org's) deliveries
// between absolute StartsAt and EndsAt.
type MaintenanceWindow struct {
	ID        int64     `json:"id"`
	OrgID     *int64    `json:"org_id"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy *int64    `json:"created_by"`
}

// NotificationDelivery is one row of the delivery log, keyed by
// (alert, rule, channel, event, step). Step is 0 for a non-escalation NS1
// delivery and the escalation step number (>=1) for a policy step. UpdatedAt is
// nil until the dispatcher first claims the row; it then doubles as the retry
// lease (ClaimPendingDeliveries).
type NotificationDelivery struct {
	ID         int64      `json:"id"`
	AlertID    *int64     `json:"alert_id"`
	RuleID     *int64     `json:"rule_id"`
	ChannelID  *int64     `json:"channel_id"`
	OrgID      *int64     `json:"org_id"`
	Event      string     `json:"event"`
	Step       int        `json:"step"`
	Status     string     `json:"status"`
	Attempts   int        `json:"attempts"`
	LastError  string     `json:"last_error"`
	DetailJSON string     `json:"detail_json"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

// --- Channels ---------------------------------------------------------------

// CreateChannelParams is a validated channel to insert. Secret "" means no
// secret.
type CreateChannelParams struct {
	Kind       string
	Name       string
	OrgID      *int64
	ConfigJSON string
	Secret     string
	Enabled    bool
	CreatedBy  *int64
}

// CreateChannel inserts a channel and returns it.
func (s *Store) CreateChannel(ctx context.Context, p CreateChannelParams) (NotifyChannel, error) {
	now := s.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO notify_channels (kind, name, org_id, config_json, secret, enabled, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Kind, p.Name, ptrInt64Arg(p.OrgID), p.ConfigJSON, p.Secret, boolInt(p.Enabled), fmtTime(now), ptrInt64Arg(p.CreatedBy))
	if err != nil {
		return NotifyChannel{}, fmt.Errorf("store: create channel: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return NotifyChannel{}, fmt.Errorf("store: create channel: %w", err)
	}
	return s.GetChannel(ctx, id)
}

// UpdateChannelParams updates a channel. Secret nil leaves the stored secret
// unchanged; a non-nil pointer replaces it (an empty string is a caller error,
// rejected at the API, not here).
type UpdateChannelParams struct {
	Name       string
	OrgID      *int64
	ConfigJSON string
	Secret     *string
	Enabled    bool
}

// UpdateChannel replaces a channel's mutable fields. ErrNotFound when the id
// does not exist.
func (s *Store) UpdateChannel(ctx context.Context, id int64, p UpdateChannelParams) (NotifyChannel, error) {
	set := []string{"name = ?", "org_id = ?", "config_json = ?", "enabled = ?"}
	args := []any{p.Name, ptrInt64Arg(p.OrgID), p.ConfigJSON, boolInt(p.Enabled)}
	if p.Secret != nil {
		set = append(set, "secret = ?")
		args = append(args, *p.Secret)
	}
	args = append(args, id)
	res, err := s.db.ExecContext(ctx, `UPDATE notify_channels SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return NotifyChannel{}, fmt.Errorf("store: update channel: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return NotifyChannel{}, fmt.Errorf("store: update channel: %w", err)
	}
	if n == 0 {
		return NotifyChannel{}, ErrNotFound
	}
	return s.GetChannel(ctx, id)
}

// DeleteChannel removes a channel (its rules cascade; the delivery log keeps the
// dangling channel_id). ErrNotFound when the id does not exist.
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "notify_channels", id, "delete channel")
}

const channelSelect = `
SELECT id, kind, name, org_id, config_json, secret, enabled, created_at, created_by
  FROM notify_channels`

func scanChannel(row interface{ Scan(...any) error }) (NotifyChannel, error) {
	var (
		c         NotifyChannel
		orgID     sql.NullInt64
		createdBy sql.NullInt64
		enabled   int
		created   string
	)
	if err := row.Scan(&c.ID, &c.Kind, &c.Name, &orgID, &c.ConfigJSON, &c.Secret, &enabled, &created, &createdBy); err != nil {
		return NotifyChannel{}, err
	}
	c.OrgID = nullInt64(orgID)
	c.CreatedBy = nullInt64(createdBy)
	c.Enabled = enabled != 0
	c.SecretSet = c.Secret != ""
	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return NotifyChannel{}, err
	}
	return c, nil
}

// GetChannel returns one channel (secret included for the dispatcher).
// ErrNotFound when the id does not exist.
func (s *Store) GetChannel(ctx context.Context, id int64) (NotifyChannel, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx, channelSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return NotifyChannel{}, ErrNotFound
	}
	if err != nil {
		return NotifyChannel{}, fmt.Errorf("store: get channel: %w", err)
	}
	return c, nil
}

// ListChannels returns channels (newest first), restricted to a caller's
// visible org set (RS2): nil = every channel, a non-nil but empty slice = none.
// A global channel (org_id NULL) is visible only to all-orgs callers (visible
// nil), exactly like an RS2 system row.
func (s *Store) ListChannels(ctx context.Context, visible []int64) ([]NotifyChannel, error) {
	q := channelSelect
	if clause, args := orgInClause(`org_id`, visible); clause != "" {
		q += ` WHERE ` + clause
		return s.queryChannels(ctx, q+` ORDER BY id DESC`, args...)
	}
	return s.queryChannels(ctx, q+` ORDER BY id DESC`)
}

func (s *Store) queryChannels(ctx context.Context, q string, args ...any) ([]NotifyChannel, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	defer rows.Close()
	out := []NotifyChannel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list channels: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	return out, nil
}

// --- Rules ------------------------------------------------------------------

// CreateRuleParams is a validated rule to insert. EscalationPolicyID is optional
// (NS2); nil leaves the rule without a policy.
type CreateRuleParams struct {
	Name               string
	OrgID              *int64
	MinSeverity        string
	AlertTypes         string
	OnOpen             bool
	OnResolve          bool
	ChannelID          int64
	EscalationPolicyID *int64
	Enabled            bool
	CreatedBy          *int64
}

// CreateRule inserts a rule. ErrNotFound when ChannelID does not exist (foreign
// key).
func (s *Store) CreateRule(ctx context.Context, p CreateRuleParams) (NotifyRule, error) {
	now := s.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO notify_rules (name, org_id, min_severity, alert_types, on_open, on_resolve, channel_id, escalation_policy_id, enabled, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, ptrInt64Arg(p.OrgID), p.MinSeverity, p.AlertTypes, boolInt(p.OnOpen), boolInt(p.OnResolve),
		p.ChannelID, ptrInt64Arg(p.EscalationPolicyID), boolInt(p.Enabled), fmtTime(now), ptrInt64Arg(p.CreatedBy))
	if err != nil {
		if isForeignKeyViolation(err) {
			return NotifyRule{}, ErrNotFound
		}
		return NotifyRule{}, fmt.Errorf("store: create rule: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return NotifyRule{}, fmt.Errorf("store: create rule: %w", err)
	}
	return s.GetRule(ctx, id)
}

// UpdateRule replaces a rule's mutable fields. ErrNotFound when the id does not
// exist; ErrNotFound when ChannelID does not exist (foreign key).
func (s *Store) UpdateRule(ctx context.Context, id int64, p CreateRuleParams) (NotifyRule, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE notify_rules SET name = ?, org_id = ?, min_severity = ?, alert_types = ?, on_open = ?,
		        on_resolve = ?, channel_id = ?, escalation_policy_id = ?, enabled = ? WHERE id = ?`,
		p.Name, ptrInt64Arg(p.OrgID), p.MinSeverity, p.AlertTypes, boolInt(p.OnOpen), boolInt(p.OnResolve),
		p.ChannelID, ptrInt64Arg(p.EscalationPolicyID), boolInt(p.Enabled), id)
	if err != nil {
		if isForeignKeyViolation(err) {
			return NotifyRule{}, ErrNotFound
		}
		return NotifyRule{}, fmt.Errorf("store: update rule: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return NotifyRule{}, fmt.Errorf("store: update rule: %w", err)
	}
	if n == 0 {
		return NotifyRule{}, ErrNotFound
	}
	return s.GetRule(ctx, id)
}

// SetRulePolicy attaches (policyID non-nil) or detaches (nil) a rule's escalation
// policy without touching its other fields. ErrNotFound when the rule does not
// exist.
func (s *Store) SetRulePolicy(ctx context.Context, ruleID int64, policyID *int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE notify_rules SET escalation_policy_id = ? WHERE id = ?`, ptrInt64Arg(policyID), ruleID)
	if err != nil {
		return fmt.Errorf("store: set rule policy: %w", err)
	}
	return s.oneRow(res, "set rule policy")
}

// DeleteRule removes a rule. ErrNotFound when the id does not exist.
func (s *Store) DeleteRule(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "notify_rules", id, "delete rule")
}

const ruleSelect = `
SELECT id, name, org_id, min_severity, alert_types, on_open, on_resolve, channel_id, escalation_policy_id,
       enabled, created_at, created_by
  FROM notify_rules`

func scanRule(row interface{ Scan(...any) error }) (NotifyRule, error) {
	var (
		r                 NotifyRule
		orgID, createdBy  sql.NullInt64
		policyID          sql.NullInt64
		onOpen, onResolve int
		enabled           int
		created           string
	)
	if err := row.Scan(&r.ID, &r.Name, &orgID, &r.MinSeverity, &r.AlertTypes, &onOpen, &onResolve,
		&r.ChannelID, &policyID, &enabled, &created, &createdBy); err != nil {
		return NotifyRule{}, err
	}
	r.OrgID = nullInt64(orgID)
	r.CreatedBy = nullInt64(createdBy)
	r.EscalationPolicyID = nullInt64(policyID)
	r.OnOpen = onOpen != 0
	r.OnResolve = onResolve != 0
	r.Enabled = enabled != 0
	var err error
	if r.CreatedAt, err = parseTime(created); err != nil {
		return NotifyRule{}, err
	}
	return r, nil
}

// GetRule returns one rule. ErrNotFound when the id does not exist.
func (s *Store) GetRule(ctx context.Context, id int64) (NotifyRule, error) {
	r, err := scanRule(s.db.QueryRowContext(ctx, ruleSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return NotifyRule{}, ErrNotFound
	}
	if err != nil {
		return NotifyRule{}, fmt.Errorf("store: get rule: %w", err)
	}
	return r, nil
}

// ListRules returns rules (newest first), restricted to a caller's visible org
// set (RS2): nil = every rule, a non-nil but empty slice = none.
func (s *Store) ListRules(ctx context.Context, visible []int64) ([]NotifyRule, error) {
	q := ruleSelect
	var args []any
	if clause, clauseArgs := orgInClause(`org_id`, visible); clause != "" {
		q += ` WHERE ` + clause
		args = clauseArgs
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list rules: %w", err)
	}
	defer rows.Close()
	out := []NotifyRule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list rules: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list rules: %w", err)
	}
	return out, nil
}

// MatchRules returns the enabled rules that fire for an alert event: org
// (rule.org_id NULL or == the alert's org), severity (alert severity >=
// rule.min_severity), alert type (rule.alert_types empty or contains the type)
// and event (opened -> on_open, resolved -> on_resolve). A 'test' event matches
// no routing rule (tests target one channel directly). Severity and the comma
// list are filtered in Go; the org and event gates are in SQL.
func (s *Store) MatchRules(ctx context.Context, alert Alert, event string) ([]NotifyRule, error) {
	if event != EventOpened && event != EventResolved {
		return []NotifyRule{}, nil
	}
	eventCol := "on_open"
	if event == EventResolved {
		eventCol = "on_resolve"
	}
	q := ruleSelect + ` WHERE enabled = 1 AND ` + eventCol + ` = 1 AND (org_id IS NULL OR org_id = ?) ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, alert.OrgID)
	if err != nil {
		return nil, fmt.Errorf("store: match rules: %w", err)
	}
	defer rows.Close()
	out := []NotifyRule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: match rules: %w", err)
		}
		if SeverityRank(alert.Severity) < SeverityRank(r.MinSeverity) {
			continue
		}
		if !alertTypeMatches(r.AlertTypes, alert.Type) {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: match rules: %w", err)
	}
	return out, nil
}

// SeverityRank orders alert severities low < medium < high; an unknown value
// ranks below low so it never passes a min-severity gate by accident.
func SeverityRank(sev string) int {
	switch sev {
	case severityLowName:
		return 0
	case severityMediumName:
		return 1
	case severityHighName:
		return 2
	}
	return -1
}

const (
	severityLowName    = "low"
	severityMediumName = "medium"
	severityHighName   = "high"
)

// alertTypeMatches reports whether a rule's comma-separated alert_types list
// (empty = all) contains typ. Surrounding spaces and empty entries are ignored.
func alertTypeMatches(list, typ string) bool {
	list = strings.TrimSpace(list)
	if list == "" {
		return true
	}
	for _, part := range strings.Split(list, ",") {
		if strings.TrimSpace(part) == typ {
			return true
		}
	}
	return false
}

// --- Maintenance windows ----------------------------------------------------

// CreateWindowParams is a validated maintenance window to insert.
type CreateWindowParams struct {
	OrgID     *int64
	StartsAt  time.Time
	EndsAt    time.Time
	Reason    string
	CreatedBy *int64
}

// CreateWindow inserts a maintenance window.
func (s *Store) CreateWindow(ctx context.Context, p CreateWindowParams) (MaintenanceWindow, error) {
	now := s.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO maintenance_windows (org_id, starts_at, ends_at, reason, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ptrInt64Arg(p.OrgID), fmtTime(p.StartsAt), fmtTime(p.EndsAt), p.Reason, fmtTime(now), ptrInt64Arg(p.CreatedBy))
	if err != nil {
		return MaintenanceWindow{}, fmt.Errorf("store: create window: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return MaintenanceWindow{}, fmt.Errorf("store: create window: %w", err)
	}
	return s.GetWindow(ctx, id)
}

// DeleteWindow removes a maintenance window. ErrNotFound when the id does not
// exist.
func (s *Store) DeleteWindow(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "maintenance_windows", id, "delete window")
}

const windowSelect = `
SELECT id, org_id, starts_at, ends_at, reason, created_at, created_by FROM maintenance_windows`

func scanWindow(row interface{ Scan(...any) error }) (MaintenanceWindow, error) {
	var (
		wdw              MaintenanceWindow
		orgID, createdBy sql.NullInt64
		starts, ends     string
		created          string
	)
	if err := row.Scan(&wdw.ID, &orgID, &starts, &ends, &wdw.Reason, &created, &createdBy); err != nil {
		return MaintenanceWindow{}, err
	}
	wdw.OrgID = nullInt64(orgID)
	wdw.CreatedBy = nullInt64(createdBy)
	var err error
	if wdw.StartsAt, err = parseTime(starts); err != nil {
		return MaintenanceWindow{}, err
	}
	if wdw.EndsAt, err = parseTime(ends); err != nil {
		return MaintenanceWindow{}, err
	}
	if wdw.CreatedAt, err = parseTime(created); err != nil {
		return MaintenanceWindow{}, err
	}
	return wdw, nil
}

// GetWindow returns one window. ErrNotFound when the id does not exist.
func (s *Store) GetWindow(ctx context.Context, id int64) (MaintenanceWindow, error) {
	w, err := scanWindow(s.db.QueryRowContext(ctx, windowSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MaintenanceWindow{}, ErrNotFound
	}
	if err != nil {
		return MaintenanceWindow{}, fmt.Errorf("store: get window: %w", err)
	}
	return w, nil
}

// ListWindows returns windows (newest first), restricted to a caller's visible
// org set (RS2): nil = every window, a non-nil but empty slice = none.
func (s *Store) ListWindows(ctx context.Context, visible []int64) ([]MaintenanceWindow, error) {
	q := windowSelect
	var args []any
	if clause, clauseArgs := orgInClause(`org_id`, visible); clause != "" {
		q += ` WHERE ` + clause
		args = clauseArgs
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list windows: %w", err)
	}
	defer rows.Close()
	out := []MaintenanceWindow{}
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list windows: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list windows: %w", err)
	}
	return out, nil
}

// ActiveMaintenance reports whether org orgID is inside an active maintenance
// window at now: a window whose org_id is NULL (all clients) or == orgID and
// whose [starts_at, ends_at] contains now (inclusive). Maintenance suppression
// is an operational mute, not a security control (docs/SECURITY.md).
func (s *Store) ActiveMaintenance(ctx context.Context, orgID int64, now time.Time) (bool, error) {
	nowStr := fmtTime(now)
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM maintenance_windows
		  WHERE (org_id IS NULL OR org_id = ?) AND starts_at <= ? AND ends_at >= ?`,
		orgID, nowStr, nowStr).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: active maintenance: %w", err)
	}
	return n > 0, nil
}

// --- Deliveries -------------------------------------------------------------

// EnqueueDeliveryParams is one row to enqueue. Status is DeliveryPending
// normally or DeliverySuppressed when the alert's org is inside an active
// maintenance window. For a 'test' event AlertID and RuleID are nil. Step is 0
// for a non-escalation NS1 delivery and the escalation step number (>=1) for a
// policy step.
type EnqueueDeliveryParams struct {
	AlertID   *int64
	RuleID    *int64
	ChannelID *int64
	OrgID     *int64
	Event     string
	Step      int
	Status    string
}

// EnqueueDelivery upserts a delivery keyed by (alert_id, rule_id, channel_id,
// event, step) so the evaluator or the escalation tick running twice never
// double-sends. created reports whether a new row was inserted (false when an
// equivalent row already existed and was left untouched). An NS1 delivery has
// Step 0 and one channel per rule, so the key reduces to the NS1
// (alert, rule, event) guarantee. A 'test' event has alert_id NULL, which never
// conflicts (NULLs are distinct in the unique index), so each test enqueue is a
// fresh row.
func (s *Store) EnqueueDelivery(ctx context.Context, p EnqueueDeliveryParams) (NotificationDelivery, bool, error) {
	status := p.Status
	if status == "" {
		status = DeliveryPending
	}
	now := s.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO notification_deliveries (alert_id, rule_id, channel_id, org_id, event, step, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(alert_id, rule_id, channel_id, event, step) DO NOTHING`,
		ptrInt64Arg(p.AlertID), ptrInt64Arg(p.RuleID), ptrInt64Arg(p.ChannelID), ptrInt64Arg(p.OrgID),
		p.Event, p.Step, status, fmtTime(now))
	if err != nil {
		return NotificationDelivery{}, false, fmt.Errorf("store: enqueue delivery: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return NotificationDelivery{}, false, fmt.Errorf("store: enqueue delivery: %w", err)
	}
	if n == 1 {
		id, err := res.LastInsertId()
		if err != nil {
			return NotificationDelivery{}, false, fmt.Errorf("store: enqueue delivery: %w", err)
		}
		d, err := s.GetDelivery(ctx, id)
		return d, true, err
	}
	// Conflict: the (alert, rule, channel, event, step) row already exists; return
	// it unchanged.
	d, err := s.getDeliveryByKey(ctx, p.AlertID, p.RuleID, p.ChannelID, p.Event, p.Step)
	return d, false, err
}

const deliverySelect = `
SELECT id, alert_id, rule_id, channel_id, org_id, event, step, status, attempts, last_error, detail_json,
       created_at, updated_at FROM notification_deliveries`

func scanDelivery(row interface{ Scan(...any) error }) (NotificationDelivery, error) {
	var (
		d                                 NotificationDelivery
		alertID, ruleID, channelID, orgID sql.NullInt64
		created                           string
		updated                           sql.NullString
	)
	if err := row.Scan(&d.ID, &alertID, &ruleID, &channelID, &orgID, &d.Event, &d.Step, &d.Status, &d.Attempts,
		&d.LastError, &d.DetailJSON, &created, &updated); err != nil {
		return NotificationDelivery{}, err
	}
	d.AlertID = nullInt64(alertID)
	d.RuleID = nullInt64(ruleID)
	d.ChannelID = nullInt64(channelID)
	d.OrgID = nullInt64(orgID)
	var err error
	if d.CreatedAt, err = parseTime(created); err != nil {
		return NotificationDelivery{}, err
	}
	if d.UpdatedAt, err = nullTime(updated); err != nil {
		return NotificationDelivery{}, err
	}
	return d, nil
}

// GetDelivery returns one delivery. ErrNotFound when the id does not exist.
func (s *Store) GetDelivery(ctx context.Context, id int64) (NotificationDelivery, error) {
	d, err := scanDelivery(s.db.QueryRowContext(ctx, deliverySelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationDelivery{}, ErrNotFound
	}
	if err != nil {
		return NotificationDelivery{}, fmt.Errorf("store: get delivery: %w", err)
	}
	return d, nil
}

func (s *Store) getDeliveryByKey(ctx context.Context, alertID, ruleID, channelID *int64, event string, step int) (NotificationDelivery, error) {
	// alert_id/rule_id/channel_id are nullable; "col IS ?" matches NULL to NULL and
	// a value to a value, which is what the (alert, rule, channel, event, step) key
	// needs.
	d, err := scanDelivery(s.db.QueryRowContext(ctx,
		deliverySelect+` WHERE alert_id IS ? AND rule_id IS ? AND channel_id IS ? AND event = ? AND step = ?`,
		ptrInt64Arg(alertID), ptrInt64Arg(ruleID), ptrInt64Arg(channelID), event, step))
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationDelivery{}, ErrNotFound
	}
	if err != nil {
		return NotificationDelivery{}, fmt.Errorf("store: get delivery by key: %w", err)
	}
	return d, nil
}

// DeliveryFilter narrows ListDeliveries.
type DeliveryFilter struct {
	Status      string
	OrgID       *int64
	VisibleOrgs []int64
	Limit       int
}

// ListDeliveries returns the delivery log newest first, restricted to a
// caller's visible org set (RS2) and optionally one status / org.
func (s *Store) ListDeliveries(ctx context.Context, f DeliveryFilter) ([]NotificationDelivery, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := deliverySelect
	var (
		where []string
		args  []any
	)
	if f.Status != "" {
		where = append(where, `status = ?`)
		args = append(args, f.Status)
	}
	if f.OrgID != nil {
		where = append(where, `org_id = ?`)
		args = append(args, *f.OrgID)
	}
	if clause, clauseArgs := orgInClause(`org_id`, f.VisibleOrgs); clause != "" {
		where = append(where, clause)
		args = append(args, clauseArgs...)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list deliveries: %w", err)
	}
	defer rows.Close()
	out := []NotificationDelivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list deliveries: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list deliveries: %w", err)
	}
	return out, nil
}

// ClaimPendingDeliveries atomically leases up to limit pending deliveries for
// the dispatcher and returns them (oldest first). A row is claimable when it is
// pending and either was never claimed (updated_at IS NULL) or its lease has
// expired (updated_at <= retryBefore); the claim stamps updated_at = now, which
// both leases the row (a concurrent claim, serialised by the immediate write
// transaction, then sees the fresh updated_at and skips it) and schedules the
// next retry no sooner than one backoff after this attempt. Pass retryBefore =
// now - backoff.
func (s *Store) ClaimPendingDeliveries(ctx context.Context, limit int, retryBefore time.Time) ([]NotificationDelivery, error) {
	if limit <= 0 {
		limit = 100
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM notification_deliveries
		  WHERE status = 'pending' AND (updated_at IS NULL OR updated_at <= ?)
		  ORDER BY id LIMIT ?`, fmtTime(retryBefore), limit)
	if err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: claim deliveries: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	if len(ids) == 0 {
		return []NotificationDelivery{}, tx.Commit()
	}

	args := make([]any, 0, len(ids)+1)
	args = append(args, fmtTime(now))
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE notification_deliveries SET updated_at = ? WHERE id IN (`+placeholders(len(ids))+`) AND status = 'pending'`,
		args...); err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}

	claimed := make([]NotificationDelivery, 0, len(ids))
	for _, id := range ids {
		d, err := scanDelivery(tx.QueryRowContext(ctx, deliverySelect+` WHERE id = ?`, id))
		if err != nil {
			return nil, fmt.Errorf("store: claim deliveries: %w", err)
		}
		claimed = append(claimed, d)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: claim deliveries: %w", err)
	}
	return claimed, nil
}

// MarkDeliverySent records a successful send: status sent, last_error cleared,
// an optional redacted detail (never a secret). Only a row still pending is
// updated. ErrNotFound when no pending row with that id exists.
func (s *Store) MarkDeliverySent(ctx context.Context, id int64, detailJSON string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE notification_deliveries SET status = 'sent', last_error = '', detail_json = ?, updated_at = ?
		  WHERE id = ? AND status = 'pending'`,
		detailJSON, fmtTime(s.Now()), id)
	if err != nil {
		return fmt.Errorf("store: mark delivery sent: %w", err)
	}
	return s.oneRow(res, "mark delivery sent")
}

// MarkDeliveryFailed records a failed send attempt: attempts += 1 and last_error
// set. terminal true leaves the row failed (the dispatcher gave up at
// maxAttempts); terminal false leaves it pending so a later tick retries it
// after the backoff. Only a row still pending is updated. ErrNotFound when no
// pending row with that id exists.
func (s *Store) MarkDeliveryFailed(ctx context.Context, id int64, lastError string, terminal bool) error {
	status := DeliveryPending
	if terminal {
		status = DeliveryFailed
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE notification_deliveries SET status = ?, attempts = attempts + 1, last_error = ?, updated_at = ?
		  WHERE id = ? AND status = 'pending'`,
		status, lastError, fmtTime(s.Now()), id)
	if err != nil {
		return fmt.Errorf("store: mark delivery failed: %w", err)
	}
	return s.oneRow(res, "mark delivery failed")
}

// --- escalation policies (NS2) ----------------------------------------------

// EscalationPolicy is an ordered list of steps. A rule may point at a policy
// (notify_rules.escalation_policy_id); when it does, the policy's steps drive
// timed deliveries while a matching alert stays open and unacknowledged, in
// addition to the rule's own on_open channel. OrgID nil = a global policy
// (visible only to all-orgs staff, RS2).
type EscalationPolicy struct {
	ID        int64            `json:"id"`
	OrgID     *int64           `json:"org_id"`
	Name      string           `json:"name"`
	CreatedAt time.Time        `json:"created_at"`
	CreatedBy *int64           `json:"created_by"`
	Steps     []EscalationStep `json:"steps"`
}

// EscalationStep fires its ChannelIDs AfterMinutes after an alert opens.
// StepOrder is 1-based and is the step number recorded on each delivery
// (NotificationDelivery.Step); the NS1 non-escalation deliveries use step 0.
type EscalationStep struct {
	ID           int64   `json:"id"`
	PolicyID     int64   `json:"policy_id"`
	StepOrder    int     `json:"step_order"`
	AfterMinutes int     `json:"after_minutes"`
	ChannelIDs   []int64 `json:"channel_ids"`
}

// CreatePolicyParams is a validated policy to insert. Steps are stored in the
// given order, numbered 1..n.
type CreatePolicyParams struct {
	OrgID     *int64
	Name      string
	CreatedBy *int64
	Steps     []CreatePolicyStep
}

// CreatePolicyStep is one step of a new policy.
type CreatePolicyStep struct {
	AfterMinutes int
	ChannelIDs   []int64
}

// CreatePolicy inserts a policy and its steps atomically and returns the loaded
// policy (with steps).
func (s *Store) CreatePolicy(ctx context.Context, p CreatePolicyParams) (EscalationPolicy, error) {
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: create policy: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO escalation_policies (org_id, name, created_at, created_by) VALUES (?, ?, ?, ?)`,
		ptrInt64Arg(p.OrgID), p.Name, fmtTime(now), ptrInt64Arg(p.CreatedBy))
	if err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: create policy: %w", err)
	}
	policyID, err := res.LastInsertId()
	if err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: create policy: %w", err)
	}
	for i, step := range p.Steps {
		ids := step.ChannelIDs
		if ids == nil {
			ids = []int64{}
		}
		idsJSON, err := json.Marshal(ids)
		if err != nil {
			return EscalationPolicy{}, fmt.Errorf("store: create policy: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO escalation_steps (policy_id, step_order, after_minutes, channel_ids_json) VALUES (?, ?, ?, ?)`,
			policyID, i+1, step.AfterMinutes, string(idsJSON)); err != nil {
			return EscalationPolicy{}, fmt.Errorf("store: create policy: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: create policy: %w", err)
	}
	return s.GetPolicy(ctx, policyID)
}

// UpdatePolicyParams replaces a policy's name and its full ordered step list
// (NS2). OrgID is immutable (like a channel's), so it is not a field here.
type UpdatePolicyParams struct {
	Name  string
	Steps []CreatePolicyStep
}

// UpdatePolicy replaces a policy's name and steps atomically (the old steps are
// deleted and the new ones inserted, renumbered 1..n). ErrNotFound when the id
// does not exist.
func (s *Store) UpdatePolicy(ctx context.Context, id int64, p UpdatePolicyParams) (EscalationPolicy, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: update policy: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE escalation_policies SET name = ? WHERE id = ?`, p.Name, id)
	if err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: update policy: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: update policy: %w", err)
	}
	if n == 0 {
		return EscalationPolicy{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM escalation_steps WHERE policy_id = ?`, id); err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: update policy: %w", err)
	}
	for i, step := range p.Steps {
		ids := step.ChannelIDs
		if ids == nil {
			ids = []int64{}
		}
		idsJSON, err := json.Marshal(ids)
		if err != nil {
			return EscalationPolicy{}, fmt.Errorf("store: update policy: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO escalation_steps (policy_id, step_order, after_minutes, channel_ids_json) VALUES (?, ?, ?, ?)`,
			id, i+1, step.AfterMinutes, string(idsJSON)); err != nil {
			return EscalationPolicy{}, fmt.Errorf("store: update policy: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: update policy: %w", err)
	}
	return s.GetPolicy(ctx, id)
}

// DeletePolicy removes a policy (its steps cascade) and detaches any rules that
// point at it (escalation_policy_id -> NULL) so no rule is left referencing a
// deleted policy. ErrNotFound when the id does not exist.
func (s *Store) DeletePolicy(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete policy: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE notify_rules SET escalation_policy_id = NULL WHERE escalation_policy_id = ?`, id); err != nil {
		return fmt.Errorf("store: delete policy: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM escalation_policies WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete policy: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: delete policy: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

const policySelect = `SELECT id, org_id, name, created_at, created_by FROM escalation_policies`

func scanPolicy(row interface{ Scan(...any) error }) (EscalationPolicy, error) {
	var (
		p                EscalationPolicy
		orgID, createdBy sql.NullInt64
		created          string
	)
	if err := row.Scan(&p.ID, &orgID, &p.Name, &created, &createdBy); err != nil {
		return EscalationPolicy{}, err
	}
	p.OrgID = nullInt64(orgID)
	p.CreatedBy = nullInt64(createdBy)
	var err error
	if p.CreatedAt, err = parseTime(created); err != nil {
		return EscalationPolicy{}, err
	}
	return p, nil
}

// loadSteps returns a policy's steps ordered by step_order.
func (s *Store) loadSteps(ctx context.Context, policyID int64) ([]EscalationStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, policy_id, step_order, after_minutes, channel_ids_json
		   FROM escalation_steps WHERE policy_id = ? ORDER BY step_order`, policyID)
	if err != nil {
		return nil, fmt.Errorf("store: load steps: %w", err)
	}
	defer rows.Close()
	out := []EscalationStep{}
	for rows.Next() {
		var (
			st      EscalationStep
			idsJSON string
		)
		if err := rows.Scan(&st.ID, &st.PolicyID, &st.StepOrder, &st.AfterMinutes, &idsJSON); err != nil {
			return nil, fmt.Errorf("store: load steps: %w", err)
		}
		if err := json.Unmarshal([]byte(idsJSON), &st.ChannelIDs); err != nil {
			return nil, fmt.Errorf("store: load steps: bad channel_ids_json: %w", err)
		}
		if st.ChannelIDs == nil {
			st.ChannelIDs = []int64{}
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load steps: %w", err)
	}
	return out, nil
}

// GetPolicy returns one policy with its steps. ErrNotFound when the id does not
// exist.
func (s *Store) GetPolicy(ctx context.Context, id int64) (EscalationPolicy, error) {
	p, err := scanPolicy(s.db.QueryRowContext(ctx, policySelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return EscalationPolicy{}, ErrNotFound
	}
	if err != nil {
		return EscalationPolicy{}, fmt.Errorf("store: get policy: %w", err)
	}
	if p.Steps, err = s.loadSteps(ctx, p.ID); err != nil {
		return EscalationPolicy{}, err
	}
	return p, nil
}

// ListPolicies returns policies (newest first) with their steps, restricted to a
// caller's visible org set (RS2): nil = every policy, a non-nil but empty slice =
// none. A global policy (org_id NULL) is visible only to all-orgs callers.
func (s *Store) ListPolicies(ctx context.Context, visible []int64) ([]EscalationPolicy, error) {
	q := policySelect
	var args []any
	if clause, clauseArgs := orgInClause(`org_id`, visible); clause != "" {
		q += ` WHERE ` + clause
		args = clauseArgs
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list policies: %w", err)
	}
	defer rows.Close()
	var ids []int64
	out := []EscalationPolicy{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list policies: %w", err)
		}
		out = append(out, p)
		ids = append(ids, p.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list policies: %w", err)
	}
	for i := range out {
		if out[i].Steps, err = s.loadSteps(ctx, ids[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PoliciesForRule returns the escalation policy a rule points at (with its
// steps), as a slice of zero or one: empty when the rule has no policy (or the
// rule does not exist). The open-time enqueue path uses it to know whether a
// matched rule carries a policy. ErrNotFound is never returned; a missing rule
// yields an empty slice.
func (s *Store) PoliciesForRule(ctx context.Context, ruleID int64) ([]EscalationPolicy, error) {
	var policyID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT escalation_policy_id FROM notify_rules WHERE id = ?`, ruleID).Scan(&policyID)
	if errors.Is(err, sql.ErrNoRows) {
		return []EscalationPolicy{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: policies for rule: %w", err)
	}
	if !policyID.Valid {
		return []EscalationPolicy{}, nil
	}
	p, err := s.GetPolicy(ctx, policyID.Int64)
	if errors.Is(err, ErrNotFound) {
		return []EscalationPolicy{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []EscalationPolicy{p}, nil
}

// DueEscalation is one escalation step that has come due for one open,
// unacknowledged alert through one matching policy-bearing rule. The dispatcher
// expands Step.ChannelIDs into one delivery each, keyed by
// (alert, rule, channel, event=opened, step=Step.StepOrder), so a repeated tick
// is idempotent.
type DueEscalation struct {
	Alert Alert
	Rule  NotifyRule
	Step  EscalationStep
}

// DueEscalations returns, for every open and unacknowledged alert, the escalation
// steps that have come due at now: a step of the policy attached to an enabled,
// on_open rule that matches the alert's org, severity and type, whose
// opened_at + after_minutes <= now. Acknowledged or resolved alerts are excluded
// in SQL, so escalation stops the moment an alert is acked or resolved. Severity
// and the comma alert-type list are filtered in Go exactly as MatchRules does.
func (s *Store) DueEscalations(ctx context.Context, now time.Time) ([]DueEscalation, error) {
	// Open, unacknowledged alerts.
	aRows, err := s.db.QueryContext(ctx,
		alertSelect+` WHERE a.resolved_at IS NULL AND a.acknowledged_at IS NULL ORDER BY a.id`)
	if err != nil {
		return nil, fmt.Errorf("store: due escalations: %w", err)
	}
	defer aRows.Close()
	var alerts []Alert
	for aRows.Next() {
		a, err := scanAlert(aRows)
		if err != nil {
			return nil, fmt.Errorf("store: due escalations: %w", err)
		}
		alerts = append(alerts, a)
	}
	if err := aRows.Err(); err != nil {
		return nil, fmt.Errorf("store: due escalations: %w", err)
	}
	if len(alerts) == 0 {
		return []DueEscalation{}, nil
	}

	// Enabled on_open rules that carry a policy.
	rRows, err := s.db.QueryContext(ctx,
		ruleSelect+` WHERE enabled = 1 AND on_open = 1 AND escalation_policy_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: due escalations: %w", err)
	}
	defer rRows.Close()
	var rules []NotifyRule
	for rRows.Next() {
		r, err := scanRule(rRows)
		if err != nil {
			return nil, fmt.Errorf("store: due escalations: %w", err)
		}
		rules = append(rules, r)
	}
	if err := rRows.Err(); err != nil {
		return nil, fmt.Errorf("store: due escalations: %w", err)
	}
	if len(rules) == 0 {
		return []DueEscalation{}, nil
	}

	stepsCache := map[int64][]EscalationStep{}
	out := []DueEscalation{}
	for _, a := range alerts {
		for _, r := range rules {
			if r.EscalationPolicyID == nil {
				continue
			}
			if r.OrgID != nil && *r.OrgID != a.OrgID {
				continue
			}
			if SeverityRank(a.Severity) < SeverityRank(r.MinSeverity) {
				continue
			}
			if !alertTypeMatches(r.AlertTypes, a.Type) {
				continue
			}
			policyID := *r.EscalationPolicyID
			steps, ok := stepsCache[policyID]
			if !ok {
				if steps, err = s.loadSteps(ctx, policyID); err != nil {
					return nil, err
				}
				stepsCache[policyID] = steps
			}
			for _, st := range steps {
				due := a.OpenedAt.Add(time.Duration(st.AfterMinutes) * time.Minute)
				if !due.After(now) {
					out = append(out, DueEscalation{Alert: a, Rule: r, Step: st})
				}
			}
		}
	}
	return out, nil
}

// --- small shared helpers ---------------------------------------------------

func (s *Store) deleteByID(ctx context.Context, table string, id int64, what string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	return s.oneRow(res, what)
}

func (s *Store) oneRow(res sql.Result, what string) error {
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
