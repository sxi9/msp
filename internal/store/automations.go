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

// Automations (AUT1). A saved rule that produces jobs: either on a schedule
// (trigger 'schedule', driven by the server scheduler via DueAutomations) or when
// a matching alert opens (trigger 'alert', driven from the alert-open hook via
// MatchAlertAutomations). Each run resolves target_json to the automation's
// visible devices (the server does the resolution with ListDevices + the
// automation's org scope) and fans the script out under a job.

// Automation triggers (schema CHECK).
const (
	AutomationTriggerSchedule = "schedule"
	AutomationTriggerAlert    = "alert"
)

// Automation is one saved rule. Trigger decides which of ScheduleJSON/AlertJSON
// applies; TargetJSON is always a device filter. NextRunAt is the scheduler's
// due time (nil for an alert trigger, or until the first schedule is computed).
type Automation struct {
	ID           int64      `json:"id"`
	OrgID        *int64     `json:"org_id"`
	Name         string     `json:"name"`
	Enabled      bool       `json:"enabled"`
	Trigger      string     `json:"trigger"`
	ScriptID     int64      `json:"script_id"`
	TargetJSON   string     `json:"target_json"`
	ScheduleJSON string     `json:"schedule_json"`
	AlertJSON    string     `json:"alert_json"`
	LastRunAt    *time.Time `json:"last_run_at"`
	NextRunAt    *time.Time `json:"next_run_at"`
	CreatedAt    time.Time  `json:"created_at"`
	CreatedBy    *int64     `json:"created_by"`
}

// TargetSpec is the device filter stored in target_json. All fields are optional;
// empty means "no narrowing on this axis". The server resolves it against the
// automation's visible org set, so OrgID (when set) must be inside that set.
// DeviceIDs pins an explicit device set (used by a manual bulk run that checks
// boxes); a scheduled/triggered automation normally uses the filter fields.
type TargetSpec struct {
	OrgID     *int64  `json:"org_id,omitempty"`
	Status    string  `json:"status,omitempty"`
	Platform  string  `json:"platform,omitempty"`
	HostnameQ string  `json:"hostname_q,omitempty"`
	DeviceIDs []int64 `json:"device_ids,omitempty"`
}

// ScheduleSpec is the schedule stored in schedule_json. EveryMinutes drives the
// simple interval schedule (compute next = now + EveryMinutes, so missed ticks do
// not stack); Cron is reserved for a future cron string.
type ScheduleSpec struct {
	EveryMinutes int    `json:"every_minutes,omitempty"`
	Cron         string `json:"cron,omitempty"`
}

// AlertTriggerSpec is the matcher stored in alert_json. An empty AlertType matches
// any type; an empty MinSeverity matches any severity.
type AlertTriggerSpec struct {
	AlertType   string `json:"alert_type,omitempty"`
	MinSeverity string `json:"min_severity,omitempty"`
}

// Matches reports whether an alert satisfies this trigger spec (type and minimum
// severity). Severity ordering reuses SeverityRank (an unknown stored severity
// never passes a min gate by accident).
func (spec AlertTriggerSpec) Matches(alert Alert) bool {
	if spec.AlertType != "" && spec.AlertType != alert.Type {
		return false
	}
	if spec.MinSeverity != "" && SeverityRank(alert.Severity) < SeverityRank(spec.MinSeverity) {
		return false
	}
	return true
}

// CreateAutomationParams is a validated automation to insert. The server has
// checked the shape (a schedule trigger carries ScheduleJSON; an alert trigger
// carries AlertJSON), the RS2 scope (OrgID nil requires all-orgs), and that the
// script is visible. NextRunAt seeds the scheduler (first due time) for a schedule
// trigger; nil for an alert trigger.
type CreateAutomationParams struct {
	OrgID        *int64
	Name         string
	Enabled      bool
	Trigger      string
	ScriptID     int64
	TargetJSON   string
	ScheduleJSON string
	AlertJSON    string
	NextRunAt    *time.Time
	CreatedBy    *int64
}

const automationSelect = `
SELECT id, org_id, name, enabled, "trigger", script_id, target_json, schedule_json, alert_json,
       last_run_at, next_run_at, created_at, created_by
  FROM automations`

func scanAutomation(row interface{ Scan(...any) error }) (Automation, error) {
	var (
		a                       Automation
		orgID, createdBy        sql.NullInt64
		enabled                 int
		scheduleJSON, alertJSON sql.NullString
		lastRun, nextRun        sql.NullString
		created                 string
	)
	if err := row.Scan(&a.ID, &orgID, &a.Name, &enabled, &a.Trigger, &a.ScriptID, &a.TargetJSON,
		&scheduleJSON, &alertJSON, &lastRun, &nextRun, &created, &createdBy); err != nil {
		return Automation{}, err
	}
	a.OrgID = nullInt64(orgID)
	a.CreatedBy = nullInt64(createdBy)
	a.Enabled = enabled != 0
	a.ScheduleJSON = nullString(scheduleJSON)
	a.AlertJSON = nullString(alertJSON)
	var err error
	if a.LastRunAt, err = nullTime(lastRun); err != nil {
		return Automation{}, err
	}
	if a.NextRunAt, err = nullTime(nextRun); err != nil {
		return Automation{}, err
	}
	if a.CreatedAt, err = parseTime(created); err != nil {
		return Automation{}, err
	}
	return a, nil
}

// CreateAutomation inserts an automation and returns it. ErrNotFound when
// ScriptID does not exist (foreign key).
func (s *Store) CreateAutomation(ctx context.Context, p CreateAutomationParams) (Automation, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO automations (org_id, name, enabled, "trigger", script_id, target_json, schedule_json, alert_json,
		                          next_run_at, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ptrInt64Arg(p.OrgID), p.Name, boolInt(p.Enabled), p.Trigger, p.ScriptID, p.TargetJSON,
		nullTextArg(p.ScheduleJSON), nullTextArg(p.AlertJSON), timeArg(p.NextRunAt), fmtTime(s.Now()), ptrInt64Arg(p.CreatedBy))
	if err != nil {
		if isForeignKeyViolation(err) {
			return Automation{}, ErrNotFound
		}
		return Automation{}, fmt.Errorf("store: create automation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Automation{}, fmt.Errorf("store: create automation: %w", err)
	}
	return s.GetAutomation(ctx, id)
}

// UpdateAutomationParams replaces an automation's mutable fields. NextRunAt
// re-seeds the scheduler (a changed interval takes effect at the next tick).
type UpdateAutomationParams struct {
	OrgID        *int64
	Name         string
	Enabled      bool
	Trigger      string
	ScriptID     int64
	TargetJSON   string
	ScheduleJSON string
	AlertJSON    string
	NextRunAt    *time.Time
}

// UpdateAutomation replaces an automation's mutable fields. ErrNotFound when the
// id does not exist or ScriptID does not exist (foreign key).
func (s *Store) UpdateAutomation(ctx context.Context, id int64, p UpdateAutomationParams) (Automation, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE automations SET org_id = ?, name = ?, enabled = ?, "trigger" = ?, script_id = ?,
		        target_json = ?, schedule_json = ?, alert_json = ?, next_run_at = ?
		  WHERE id = ?`,
		ptrInt64Arg(p.OrgID), p.Name, boolInt(p.Enabled), p.Trigger, p.ScriptID, p.TargetJSON,
		nullTextArg(p.ScheduleJSON), nullTextArg(p.AlertJSON), timeArg(p.NextRunAt), id)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Automation{}, ErrNotFound
		}
		return Automation{}, fmt.Errorf("store: update automation: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return Automation{}, fmt.Errorf("store: update automation: %w", err)
	}
	if n == 0 {
		return Automation{}, ErrNotFound
	}
	return s.GetAutomation(ctx, id)
}

// SetAutomationEnabled toggles an automation. ErrNotFound when the id does not
// exist.
func (s *Store) SetAutomationEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE automations SET enabled = ? WHERE id = ?`, boolInt(enabled), id)
	if err != nil {
		return fmt.Errorf("store: set automation enabled: %w", err)
	}
	return s.oneRow(res, "set automation enabled")
}

// DeleteAutomation removes an automation. A job it produced survives
// (automation_id is a plain column, no FK). ErrNotFound when the id does not
// exist.
func (s *Store) DeleteAutomation(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "automations", id, "delete automation")
}

// GetAutomation returns one automation. ErrNotFound when the id does not exist.
func (s *Store) GetAutomation(ctx context.Context, id int64) (Automation, error) {
	a, err := scanAutomation(s.db.QueryRowContext(ctx, automationSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Automation{}, ErrNotFound
	}
	if err != nil {
		return Automation{}, fmt.Errorf("store: get automation: %w", err)
	}
	return a, nil
}

// ListAutomations returns automations (newest first), restricted to a caller's
// visible org set (RS2): nil = every automation (including global org_id NULL
// rows), a non-nil slice restricts to those org ids, a non-nil empty slice
// matches nothing.
func (s *Store) ListAutomations(ctx context.Context, visible []int64) ([]Automation, error) {
	q := automationSelect
	var args []any
	if clause, clauseArgs := orgInClause(`org_id`, visible); clause != "" {
		q += ` WHERE ` + clause
		args = clauseArgs
	}
	q += ` ORDER BY id DESC`
	return s.queryAutomations(ctx, q, args...)
}

// DueAutomations returns the enabled schedule-trigger automations whose
// next_run_at is at or before now, oldest-due first. These are the ones the
// scheduler must run this tick.
func (s *Store) DueAutomations(ctx context.Context, now time.Time) ([]Automation, error) {
	q := automationSelect + ` WHERE enabled = 1 AND "trigger" = ? AND next_run_at IS NOT NULL AND next_run_at <= ?
	         ORDER BY next_run_at, id`
	return s.queryAutomations(ctx, q, AutomationTriggerSchedule, fmtTime(now))
}

// MatchAlertAutomations returns the enabled alert-trigger automations that fire
// for an opening alert: org (org_id NULL or == the alert's org, in SQL) and the
// alert_json matcher (type and min severity, in Go). The caller then applies the
// per-device cooldown (LastAutomationDeviceRunAt) and fans the script out onto the
// alerting device.
func (s *Store) MatchAlertAutomations(ctx context.Context, alert Alert) ([]Automation, error) {
	q := automationSelect + ` WHERE enabled = 1 AND "trigger" = ? AND (org_id IS NULL OR org_id = ?) ORDER BY id`
	all, err := s.queryAutomations(ctx, q, AutomationTriggerAlert, alert.OrgID)
	if err != nil {
		return nil, err
	}
	out := []Automation{}
	for _, a := range all {
		spec, err := a.AlertSpec()
		if err != nil {
			// A row with an unreadable alert_json is skipped rather than failing the
			// whole lookup (the server wrote it, so this should not happen).
			continue
		}
		if spec.Matches(alert) {
			out = append(out, a)
		}
	}
	return out, nil
}

// SetAutomationRan records a run: last_run_at = ran, and next_run_at = next (nil
// for an alert trigger, or the computed next schedule time). ErrNotFound when the
// id does not exist.
func (s *Store) SetAutomationRan(ctx context.Context, id int64, ran time.Time, next *time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE automations SET last_run_at = ?, next_run_at = ? WHERE id = ?`,
		fmtTime(ran), timeArg(next), id)
	if err != nil {
		return fmt.Errorf("store: set automation ran: %w", err)
	}
	return s.oneRow(res, "set automation ran")
}

// LastAutomationDeviceRunAt returns the created_at of the most recent command an
// automation queued on a device (joining the command's job to the automation), or
// nil when the automation has never run on that device. The trigger hook uses it
// for the per-automation-per-device cooldown that stops a flapping alert from
// hammering a device.
func (s *Store) LastAutomationDeviceRunAt(ctx context.Context, automationID, deviceID int64) (*time.Time, error) {
	var created sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT c.created_at FROM commands c JOIN jobs j ON j.id = c.job_id
		  WHERE j.automation_id = ? AND c.device_id = ?
		  ORDER BY c.id DESC LIMIT 1`, automationID, deviceID).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: last automation device run: %w", err)
	}
	return nullTime(created)
}

// AlertSpec parses an alert-trigger automation's alert_json. An empty column
// yields a zero spec (matches any type and severity).
func (a Automation) AlertSpec() (AlertTriggerSpec, error) {
	var spec AlertTriggerSpec
	if strings.TrimSpace(a.AlertJSON) == "" {
		return spec, nil
	}
	if err := json.Unmarshal([]byte(a.AlertJSON), &spec); err != nil {
		return AlertTriggerSpec{}, fmt.Errorf("store: bad alert_json: %w", err)
	}
	return spec, nil
}

// ScheduleSpec parses a schedule-trigger automation's schedule_json. An empty
// column yields a zero spec.
func (a Automation) ScheduleSpec() (ScheduleSpec, error) {
	var spec ScheduleSpec
	if strings.TrimSpace(a.ScheduleJSON) == "" {
		return spec, nil
	}
	if err := json.Unmarshal([]byte(a.ScheduleJSON), &spec); err != nil {
		return ScheduleSpec{}, fmt.Errorf("store: bad schedule_json: %w", err)
	}
	return spec, nil
}

// TargetSpec parses an automation's target_json.
func (a Automation) TargetSpec() (TargetSpec, error) {
	var spec TargetSpec
	if strings.TrimSpace(a.TargetJSON) == "" {
		return spec, nil
	}
	if err := json.Unmarshal([]byte(a.TargetJSON), &spec); err != nil {
		return TargetSpec{}, fmt.Errorf("store: bad target_json: %w", err)
	}
	return spec, nil
}

func (s *Store) queryAutomations(ctx context.Context, q string, args ...any) ([]Automation, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list automations: %w", err)
	}
	defer rows.Close()
	out := []Automation{}
	for rows.Next() {
		a, err := scanAutomation(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list automations: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list automations: %w", err)
	}
	return out, nil
}

// nullTextArg renders an optional TEXT column: "" stores SQL NULL so the column
// round-trips as unset (the schedule_json/alert_json of the other trigger).
func nullTextArg(s string) any {
	if s == "" {
		return nil
	}
	return s
}
