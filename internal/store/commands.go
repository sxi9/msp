package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Command statuses.
const (
	StatusQueued    = "queued"
	StatusSent      = "sent"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusLost      = "lost"
	StatusCancelled = "cancelled"
)

const commandSelect = `
SELECT c.id, c.device_id, c.org_id, c.created_by, COALESCE(u.email, ''), c.kind, c.argv_json, c.shell, c.body,
       c.text, c.timeout_s, c.status, c.created_at, c.sent_at, c.started_at, c.finished_at,
       c.exit_code, c.stdout, c.stderr, c.truncated, c.error
  FROM commands c LEFT JOIN users u ON u.id = c.created_by`

func scanCommand(row interface{ Scan(...any) error }) (Command, error) {
	var (
		c                           Command
		argvJSON, shell, body, text sql.NullString
		created                     string
		sentAt, startedAt, finished sql.NullString
		exitCode                    sql.NullInt64
		truncated                   int
	)
	err := row.Scan(&c.ID, &c.DeviceID, &c.OrgID, &c.CreatedBy, &c.CreatedByEmail, &c.Kind, &argvJSON, &shell, &body,
		&text, &c.TimeoutS, &c.Status, &created, &sentAt, &startedAt, &finished,
		&exitCode, &c.Stdout, &c.Stderr, &truncated, &c.Error)
	if err != nil {
		return Command{}, err
	}
	c.Argv, err = decodeArgv(argvJSON)
	if err != nil {
		return Command{}, err
	}
	c.Shell = nullString(shell)
	c.Body = nullString(body)
	c.Text = nullString(text)
	if c.CreatedAt, err = parseTime(created); err != nil {
		return Command{}, err
	}
	if c.SentAt, err = nullTime(sentAt); err != nil {
		return Command{}, err
	}
	if c.StartedAt, err = nullTime(startedAt); err != nil {
		return Command{}, err
	}
	if c.FinishedAt, err = nullTime(finished); err != nil {
		return Command{}, err
	}
	c.ExitCode = nullInt(exitCode)
	c.Truncated = truncated != 0
	return c, nil
}

// decodeArgv turns argv_json into a never-nil slice.
func decodeArgv(ns sql.NullString) ([]string, error) {
	if !ns.Valid || ns.String == "" {
		return []string{}, nil
	}
	var argv []string
	if err := json.Unmarshal([]byte(ns.String), &argv); err != nil {
		return nil, fmt.Errorf("store: bad argv_json: %w", err)
	}
	if argv == nil {
		argv = []string{}
	}
	return argv, nil
}

func getCommand(ctx context.Context, q querier, id int64) (Command, error) {
	c, err := scanCommand(q.QueryRowContext(ctx, commandSelect+` WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Command{}, ErrNotFound
	}
	if err != nil {
		return Command{}, fmt.Errorf("store: get command: %w", err)
	}
	return c, nil
}

// CreateCommand queues a command for a device. The server has already
// validated the shape (protocol.Command.Validate) and the platform; the store
// records argv as JSON for exec, shell/body/body_sha256 for script and text
// for message, status queued.
func (s *Store) CreateCommand(ctx context.Context, p CreateCommandParams) (Command, error) {
	var argvJSON, shell, body, bodySHA, text any
	switch p.Kind {
	case protocol.KindExec:
		argv := p.Argv
		if argv == nil {
			argv = []string{}
		}
		b, err := json.Marshal(argv)
		if err != nil {
			return Command{}, fmt.Errorf("store: create command: %w", err)
		}
		argvJSON = string(b)
	case protocol.KindScript:
		sum := sha256.Sum256([]byte(p.Body))
		shell, body, bodySHA = p.Shell, p.Body, hex.EncodeToString(sum[:])
	case protocol.KindMessage:
		text = p.Text
	case protocol.KindLock, protocol.KindPing:
	default:
		return Command{}, fmt.Errorf("store: create command: unknown kind %q", p.Kind)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO commands (device_id, org_id, created_by, kind, argv_json, shell, body, body_sha256, text,
		                       timeout_s, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?)`,
		p.DeviceID, p.OrgID, p.CreatedBy, p.Kind, argvJSON, shell, body, bodySHA, text,
		protocol.ClampTimeout(p.TimeoutS), fmtTime(s.Now()))
	if err != nil {
		return Command{}, fmt.Errorf("store: create command: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Command{}, fmt.Errorf("store: create command: %w", err)
	}
	return getCommand(ctx, s.db, id)
}

// ClaimQueuedCommands moves up to max queued commands of a device to status
// sent (sent_at = now) and returns them in agent wire form, oldest first. It
// runs in one write transaction, so two concurrent syncs for the same device
// never receive the same command.
func (s *Store) ClaimQueuedCommands(ctx context.Context, deviceID int64, max int) ([]protocol.Command, error) {
	if max <= 0 {
		max = protocol.MaxCommandsPerSync
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: claim commands: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, kind, argv_json, shell, body, text, timeout_s FROM commands
		  WHERE device_id = ? AND status = 'queued' ORDER BY id LIMIT ?`, deviceID, max)
	if err != nil {
		return nil, fmt.Errorf("store: claim commands: %w", err)
	}
	cmds := []protocol.Command{}
	for rows.Next() {
		var (
			c                           protocol.Command
			argvJSON, shell, body, text sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.Kind, &argvJSON, &shell, &body, &text, &c.TimeoutS); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: claim commands: %w", err)
		}
		if c.Kind == protocol.KindExec {
			if c.Argv, err = decodeArgv(argvJSON); err != nil {
				rows.Close()
				return nil, err
			}
		}
		c.Shell = nullString(shell)
		c.Body = nullString(body)
		c.Text = nullString(text)
		cmds = append(cmds, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: claim commands: %w", err)
	}
	if len(cmds) == 0 {
		return cmds, tx.Commit()
	}

	ids := make([]any, 0, len(cmds)+1)
	ids = append(ids, fmtTime(s.Now()))
	for _, c := range cmds {
		ids = append(ids, c.ID)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE commands SET status = 'sent', sent_at = ? WHERE id IN (`+placeholders(len(cmds))+`) AND status = 'queued'`,
		ids...)
	if err != nil {
		return nil, fmt.Errorf("store: claim commands: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return nil, fmt.Errorf("store: claim commands: %w", err)
	}
	if n != int64(len(cmds)) {
		// Cannot happen inside an immediate transaction; refuse to hand out
		// commands we did not mark as sent.
		return nil, fmt.Errorf("store: claim commands: claimed %d of %d rows", n, len(cmds))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: claim commands: %w", err)
	}
	return cmds, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// HasQueuedCommands reports whether more queued commands wait for a device.
func (s *Store) HasQueuedCommands(ctx context.Context, deviceID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM commands WHERE device_id = ? AND status = 'queued'`, deviceID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: has queued commands: %w", err)
	}
	return n > 0, nil
}

// GetCommand returns one command with its output.
func (s *Store) GetCommand(ctx context.Context, id int64) (Command, error) {
	return getCommand(ctx, s.db, id)
}

// ListCommands returns a device's commands, newest first.
func (s *Store) ListCommands(ctx context.Context, deviceID int64, limit int) ([]Command, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, commandSelect+` WHERE c.device_id = ? ORDER BY c.id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list commands: %w", err)
	}
	defer rows.Close()
	cmds := []Command{}
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list commands: %w", err)
		}
		cmds = append(cmds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list commands: %w", err)
	}
	return cmds, nil
}

// CompleteCommand records an agent's result. ErrNotFound when the command
// does not exist or belongs to another device; ErrConflict unless it is in
// status sent. The derived status (timeout | failed | done) is returned.
func (s *Store) CompleteCommand(ctx context.Context, id, deviceID int64, res protocol.CommandResult) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("store: complete command: %w", err)
	}
	defer tx.Rollback()

	var (
		status string
		owner  int64
	)
	err = tx.QueryRowContext(ctx, `SELECT status, device_id FROM commands WHERE id = ?`, id).Scan(&status, &owner)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != deviceID) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: complete command: %w", err)
	}
	if status != StatusSent {
		return "", ErrConflict
	}

	derived := ResultStatus(res)
	var started, finished any
	if !res.StartedAt.IsZero() {
		started = fmtTime(res.StartedAt)
	}
	if !res.FinishedAt.IsZero() {
		finished = fmtTime(res.FinishedAt)
	} else {
		finished = fmtTime(s.Now())
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE commands SET status = ?, exit_code = ?, stdout = ?, stderr = ?, truncated = ?, error = ?,
		        started_at = ?, finished_at = ?
		  WHERE id = ? AND status = 'sent'`,
		derived, res.ExitCode, res.Stdout, res.Stderr, boolInt(res.Truncated), res.Error,
		started, finished, id)
	if err != nil {
		return "", fmt.Errorf("store: complete command: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: complete command: %w", err)
	}
	return derived, nil
}

// ResultStatus derives the terminal status of a command result: timeout when
// the error starts with "timeout", failed when the error is non-empty or the
// exit code is non-zero, otherwise done.
func ResultStatus(res protocol.CommandResult) string {
	switch {
	case strings.HasPrefix(res.Error, "timeout"):
		return StatusTimeout
	case res.Error != "" || res.ExitCode != 0:
		return StatusFailed
	}
	return StatusDone
}

// MarkLostCommands flips every command still in status sent whose
// sent_at + timeout_s + grace is in the past to status lost and returns them.
func (s *Store) MarkLostCommands(ctx context.Context, grace time.Duration) ([]Command, error) {
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: mark lost: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id, sent_at, timeout_s FROM commands WHERE status = 'sent'`)
	if err != nil {
		return nil, fmt.Errorf("store: mark lost: %w", err)
	}
	var lost []int64
	for rows.Next() {
		var (
			id      int64
			sentAt  sql.NullString
			timeout int
		)
		if err := rows.Scan(&id, &sentAt, &timeout); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: mark lost: %w", err)
		}
		sent, err := nullTime(sentAt)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if sent == nil {
			continue
		}
		cutoff := sent.Add(time.Duration(timeout)*time.Second + grace)
		if cutoff.Before(now) {
			lost = append(lost, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: mark lost: %w", err)
	}
	sort.Slice(lost, func(i, j int) bool { return lost[i] < lost[j] })

	for _, id := range lost {
		if _, err := tx.ExecContext(ctx,
			`UPDATE commands SET status = 'lost', finished_at = ?, error = 'lost: no result from agent'
			  WHERE id = ? AND status = 'sent'`, fmtTime(now), id); err != nil {
			return nil, fmt.Errorf("store: mark lost: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: mark lost: %w", err)
	}

	out := []Command{}
	for _, id := range lost {
		c, err := getCommand(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
