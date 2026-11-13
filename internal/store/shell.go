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

// Remote shell (RS1, decision D3). The server keeps the live session in memory
// keyed by command id and talks to the database only at the three moments the
// shell_sessions row changes:
//
//   - CreateShellSession when the relay for a command starts (the COMMAND's
//     shell_session is null until this row exists).
//   - SetShellConnected when the agent attaches (connected_at).
//   - FinishShellSession once, at close, with the final byte counts, close
//     reason and the recorded agent->browser transcript.
//
// This is the "buffer in memory, flush once on close" model from RS1 section E:
// the relay accumulates the transcript (cap protocol.MaxTranscriptBytes) in RAM
// and writes it here a single time, so there is no per-chunk AppendTranscript
// round trip. GetShellSession reads the row back for the dashboard COMMAND JSON
// and for the GET .../transcript endpoint.

// ShellSession is a shell_sessions row. Transcript is the recorded
// agent->browser output (capped at protocol.MaxTranscriptBytes); ConnectedAt is
// nil until the agent attaches and ClosedAt is nil until the session closes.
type ShellSession struct {
	CommandID           int64
	ConnectedAt         *time.Time
	ClosedAt            *time.Time
	BytesIn             int64
	BytesOut            int64
	Transcript          []byte
	TranscriptTruncated bool
	CloseReason         string
}

// FinishShellParams is the terminal state written once when a session closes.
type FinishShellParams struct {
	ClosedAt            time.Time
	BytesIn             int64
	BytesOut            int64
	Transcript          []byte
	TranscriptTruncated bool
	CloseReason         string
}

// CreateShellSession inserts the shell_sessions row for a shell command with
// all counters zero and no timestamps. ErrConflict if a row already exists for
// the command; ErrNotFound if the command does not exist (foreign key).
func (s *Store) CreateShellSession(ctx context.Context, commandID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO shell_sessions (command_id) VALUES (?)`, commandID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if isForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: create shell session: %w", err)
	}
	return nil
}

// SetShellConnected records the moment the agent attached. ErrNotFound when no
// session row exists for the command.
func (s *Store) SetShellConnected(ctx context.Context, commandID int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shell_sessions SET connected_at = ? WHERE command_id = ?`, fmtTime(at), commandID)
	if err != nil {
		return fmt.Errorf("store: set shell connected: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: set shell connected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// FinishShellSession writes a session's terminal state once at close: closed_at,
// the byte counts, the transcript and its truncated flag, and the close reason.
// A transcript longer than protocol.MaxTranscriptBytes is clamped and the
// truncated flag forced on. connected_at is left as SetShellConnected set it (or
// NULL when the agent never attached). ErrNotFound when no session row exists.
func (s *Store) FinishShellSession(ctx context.Context, commandID int64, p FinishShellParams) error {
	transcript := p.Transcript
	truncated := p.TranscriptTruncated
	if len(transcript) > protocol.MaxTranscriptBytes {
		transcript = transcript[:protocol.MaxTranscriptBytes]
		truncated = true
	}
	if transcript == nil {
		transcript = []byte{}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE shell_sessions SET closed_at = ?, bytes_in = ?, bytes_out = ?, transcript = ?,
		        transcript_truncated = ?, close_reason = ? WHERE command_id = ?`,
		fmtTime(p.ClosedAt), p.BytesIn, p.BytesOut, transcript, boolInt(truncated), p.CloseReason, commandID)
	if err != nil {
		return fmt.Errorf("store: finish shell session: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: finish shell session: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CompleteShellCommand records the terminal state of a shell command on relay
// close. Unlike CompleteCommand it does not derive the status from the exit
// code: the relay has already decided it (RS1 section B), so a session the
// technician closed while it was live is `done` with exit_code -1 (no exit
// frame) — a combination ResultStatus cannot express. stdout is always empty;
// error is "" when done, the close reason otherwise. ErrNotFound when the
// command does not exist or belongs to another device; ErrConflict unless it is
// in status sent.
func (s *Store) CompleteShellCommand(ctx context.Context, id, deviceID int64, status string, exitCode int, errText string, started *time.Time, finished time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: complete shell command: %w", err)
	}
	defer tx.Rollback()

	var (
		cur   string
		owner int64
	)
	err = tx.QueryRowContext(ctx, `SELECT status, device_id FROM commands WHERE id = ?`, id).Scan(&cur, &owner)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != deviceID) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: complete shell command: %w", err)
	}
	if cur != StatusSent {
		return ErrConflict
	}

	var startedVal any
	if started != nil && !started.IsZero() {
		startedVal = fmtTimeMs(*started)
	}
	fin := finished
	if fin.IsZero() {
		fin = s.Now()
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE commands SET status = ?, exit_code = ?, stdout = '', stderr = '', truncated = 0, error = ?,
		        started_at = ?, finished_at = ?
		  WHERE id = ? AND status = 'sent'`,
		status, exitCode, errText, startedVal, fmtTimeMs(fin), id)
	if err != nil {
		return fmt.Errorf("store: complete shell command: %w", err)
	}
	return tx.Commit()
}

// GetShellSession reads a session row, transcript included. ErrNotFound when no
// row exists for the command.
func (s *Store) GetShellSession(ctx context.Context, commandID int64) (ShellSession, error) {
	var (
		sess              = ShellSession{CommandID: commandID}
		connected, closed sql.NullString
		transcript        []byte
		truncated         int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT connected_at, closed_at, bytes_in, bytes_out, transcript, transcript_truncated, close_reason
		   FROM shell_sessions WHERE command_id = ?`, commandID).
		Scan(&connected, &closed, &sess.BytesIn, &sess.BytesOut, &transcript, &truncated, &sess.CloseReason)
	if errors.Is(err, sql.ErrNoRows) {
		return ShellSession{}, ErrNotFound
	}
	if err != nil {
		return ShellSession{}, fmt.Errorf("store: get shell session: %w", err)
	}
	if sess.ConnectedAt, err = nullTime(connected); err != nil {
		return ShellSession{}, err
	}
	if sess.ClosedAt, err = nullTime(closed); err != nil {
		return ShellSession{}, err
	}
	if transcript == nil {
		transcript = []byte{}
	}
	sess.Transcript = transcript
	sess.TranscriptTruncated = truncated != 0
	return sess, nil
}

// isForeignKeyViolation reports whether err is a FOREIGN KEY constraint failure
// from SQLite (e.g. a shell_sessions row for a command id that does not exist).
func isForeignKeyViolation(err error) bool {
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
