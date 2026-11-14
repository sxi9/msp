package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Jobs (AUT1). A job groups the per-device commands created in one fan-out of a
// script across a device set — a manual bulk run, a scheduled automation, or an
// alert-triggered automation. The commands themselves are ordinary queued
// commands (the agent runs them unchanged); jobs only records what the fan-out
// was (origin, script, automation, total) and progress is derived at read time
// by counting those commands by status (JobProgress).

// Job origins (schema CHECK).
const (
	JobOriginManual   = "manual"
	JobOriginSchedule = "schedule"
	JobOriginTrigger  = "trigger"
)

// Job is one fan-out. OrgID is nil for a job that spans several visible orgs (a
// global script run across them); ScriptID/AutomationID/CreatedBy are nil when
// not applicable (an inline bulk run carries no script; a manual run carries no
// automation; a system run carries no user). Total is the number of commands
// actually created (android/platform skips are not counted).
type Job struct {
	ID           int64     `json:"id"`
	OrgID        *int64    `json:"org_id"`
	Name         string    `json:"name"`
	Origin       string    `json:"origin"`
	ScriptID     *int64    `json:"script_id"`
	CreatedBy    *int64    `json:"created_by"`
	AutomationID *int64    `json:"automation_id"`
	CreatedAt    time.Time `json:"created_at"`
	Total        int       `json:"total"`
}

// JobProgress is the status breakdown of a job's commands, derived by counting.
// Failed folds in every terminal non-success status (failed, timeout, lost,
// cancelled) so the dashboard's four columns (queued/sent/done/failed) sum to
// Total.
type JobProgress struct {
	JobID  int64 `json:"job_id"`
	Total  int   `json:"total"`
	Queued int   `json:"queued"`
	Sent   int   `json:"sent"`
	Done   int   `json:"done"`
	Failed int   `json:"failed"`
}

// CreateJobParams is a validated job to insert. Total starts at 0;
// CreateCommandsForDevices updates it to the number of commands it created.
type CreateJobParams struct {
	OrgID        *int64
	Name         string
	Origin       string
	ScriptID     *int64
	CreatedBy    *int64
	AutomationID *int64
}

const jobSelect = `
SELECT id, org_id, name, origin, script_id, created_by, automation_id, created_at, total
  FROM jobs`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var (
		j                                  Job
		orgID, scriptID, createdBy, autoID sql.NullInt64
		created                            string
	)
	if err := row.Scan(&j.ID, &orgID, &j.Name, &j.Origin, &scriptID, &createdBy, &autoID, &created, &j.Total); err != nil {
		return Job{}, err
	}
	j.OrgID = nullInt64(orgID)
	j.ScriptID = nullInt64(scriptID)
	j.CreatedBy = nullInt64(createdBy)
	j.AutomationID = nullInt64(autoID)
	var err error
	if j.CreatedAt, err = parseTime(created); err != nil {
		return Job{}, err
	}
	return j, nil
}

// CreateJob inserts a job row (total 0) and returns it.
func (s *Store) CreateJob(ctx context.Context, p CreateJobParams) (Job, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (org_id, name, origin, script_id, created_by, automation_id, created_at, total)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		ptrInt64Arg(p.OrgID), p.Name, p.Origin, ptrInt64Arg(p.ScriptID), ptrInt64Arg(p.CreatedBy),
		ptrInt64Arg(p.AutomationID), fmtTime(s.Now()))
	if err != nil {
		return Job{}, fmt.Errorf("store: create job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Job{}, fmt.Errorf("store: create job: %w", err)
	}
	return s.GetJob(ctx, id)
}

// GetJob returns one job. ErrNotFound when the id does not exist.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: get job: %w", err)
	}
	return j, nil
}

// ListJobs returns jobs (newest first), restricted to a caller's visible org set
// (RS2): nil = every job (including global org_id NULL rows), a non-nil slice
// restricts to those org ids, a non-nil empty slice matches nothing. orgID, when
// set, further narrows to one client.
func (s *Store) ListJobs(ctx context.Context, orgID *int64, visible []int64, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 200
	}
	q := jobSelect
	var (
		where []string
		args  []any
	)
	if orgID != nil {
		where = append(where, `org_id = ?`)
		args = append(args, *orgID)
	}
	if clause, clauseArgs := orgInClause(`org_id`, visible); clause != "" {
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
		return nil, fmt.Errorf("store: list jobs: %w", err)
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list jobs: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list jobs: %w", err)
	}
	return out, nil
}

// JobProgress counts a job's commands by status. A job with no commands returns
// an all-zero progress (not ErrNotFound; the caller checks the job exists).
func (s *Store) JobProgress(ctx context.Context, jobID int64) (JobProgress, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM commands WHERE job_id = ? GROUP BY status`, jobID)
	if err != nil {
		return JobProgress{}, fmt.Errorf("store: job progress: %w", err)
	}
	defer rows.Close()
	p := JobProgress{JobID: jobID}
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return JobProgress{}, fmt.Errorf("store: job progress: %w", err)
		}
		p.Total += n
		switch status {
		case StatusQueued:
			p.Queued += n
		case StatusSent:
			p.Sent += n
		case StatusDone:
			p.Done += n
		default: // failed, timeout, lost, cancelled
			p.Failed += n
		}
	}
	if err := rows.Err(); err != nil {
		return JobProgress{}, fmt.Errorf("store: job progress: %w", err)
	}
	return p, nil
}

// JobCommand is one device's slot in a fan-out: either a created command (ID set,
// Skipped false) or a skip (Skipped true, Note why). The device and its org are
// echoed for the progress view and the RS2 label.
type JobCommand struct {
	DeviceID  int64  `json:"device_id"`
	Hostname  string `json:"hostname"`
	OrgID     int64  `json:"org_id"`
	CommandID int64  `json:"command_id,omitempty"`
	Skipped   bool   `json:"skipped"`
	Note      string `json:"note,omitempty"`
}

// JobDeviceRow is one device's slot in a job's detail view: the device, its org
// label, and the command this job queued on it with its current status. Unlike
// JobCommand (the POST /jobs fan-out breakdown, which also reports platform/android
// skips), these are reconstructed from the commands table at read time, so only
// devices that actually got a command appear (a skip queues no command and is not
// persisted). CommandID/Status are therefore always set here; the TS JobDeviceRow
// keeps them nullable to share one type with the POST breakdown.
type JobDeviceRow struct {
	DeviceID  int64  `json:"device_id"`
	Hostname  string `json:"hostname"`
	OrgID     int64  `json:"org_id"`
	OrgName   string `json:"org_name"`
	CommandID *int64 `json:"command_id"`
	Status    string `json:"status"`
	Skipped   bool   `json:"skipped"`
	Note      string `json:"note"`
}

// JobDevices returns a job's per-device command rows (one per command it queued),
// joined to the device and its org for the RS2 label, ordered by hostname then
// command id. A job with no commands returns an empty slice (the caller has already
// checked the job exists and is visible).
func (s *Store) JobDevices(ctx context.Context, jobID int64) ([]JobDeviceRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.hostname, d.org_id, o.name, c.id, c.status
		   FROM commands c
		   JOIN devices d ON d.id = c.device_id
		   JOIN orgs o ON o.id = d.org_id
		  WHERE c.job_id = ?
		  ORDER BY d.hostname, c.id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: job devices: %w", err)
	}
	defer rows.Close()
	out := []JobDeviceRow{}
	for rows.Next() {
		var (
			r   JobDeviceRow
			cid int64
		)
		if err := rows.Scan(&r.DeviceID, &r.Hostname, &r.OrgID, &r.OrgName, &cid, &r.Status); err != nil {
			return nil, fmt.Errorf("store: job devices: %w", err)
		}
		r.CommandID = &cid
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: job devices: %w", err)
	}
	return out, nil
}

// CreateCommandsForDevices fans a script out across devices under one job,
// reusing CreateCommand with the job_id so JobProgress can count the result. A
// device whose platform does not support the script's kind (android for
// exec/script) or does not match the script's platform (a 'linux' script on a
// windows device) is skipped with a per-device note and no command. createdBy is
// stamped on every command (commands.created_by is NOT NULL): the acting operator
// for a manual run, or the automation's owner for a scheduled/triggered run. It
// updates jobs.total to the number of commands actually created and returns the
// per-device outcome (skips included), in the given device order.
func (s *Store) CreateCommandsForDevices(ctx context.Context, jobID int64, sc Script, createdBy int64, devices []Device) ([]JobCommand, error) {
	out := make([]JobCommand, 0, len(devices))
	created := 0
	for _, d := range devices {
		jc := JobCommand{DeviceID: d.ID, Hostname: d.Hostname, OrgID: d.OrgID}
		if note := scriptSkipNote(sc, d); note != "" {
			jc.Skipped = true
			jc.Note = note
			out = append(out, jc)
			continue
		}
		cmd, err := s.CreateCommand(ctx, CreateCommandParams{
			DeviceID:  d.ID,
			OrgID:     d.OrgID,
			CreatedBy: createdBy,
			Kind:      sc.Kind,
			Argv:      sc.Argv,
			Shell:     sc.Shell,
			Body:      sc.Body,
			TimeoutS:  sc.TimeoutS,
			JobID:     jobID,
		})
		if err != nil {
			return nil, fmt.Errorf("store: create job commands: %w", err)
		}
		jc.CommandID = cmd.ID
		created++
		out = append(out, jc)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET total = ? WHERE id = ?`, created, jobID); err != nil {
		return nil, fmt.Errorf("store: create job commands: %w", err)
	}
	return out, nil
}

// scriptSkipNote reports why a script cannot run on a device, or "" when it can.
// Both gates (kind unsupported on the platform, and a platform-specific script on
// a different platform) are enforced here so a fan-out never queues a command a
// device cannot run.
func scriptSkipNote(sc Script, d Device) string {
	if !protocol.KindSupportedOn(sc.Kind, d.Platform) {
		return fmt.Sprintf("%s commands are not supported on %s", sc.Kind, d.Platform)
	}
	if sc.Platform != ScriptPlatformAny && sc.Platform != d.Platform {
		return fmt.Sprintf("script targets %s, device is %s", sc.Platform, d.Platform)
	}
	return ""
}
