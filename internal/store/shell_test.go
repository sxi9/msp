package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// insertShellCommand inserts a kind-shell command directly (the dashboard's
// shell endpoint is in internal/server). The INSERT also confirms migration
// 0003 widened the commands.kind CHECK to accept 'shell'.
func insertShellCommand(t *testing.T, s *Store, dev Device, org Org, u User) int64 {
	t.Helper()
	res, err := s.db.ExecContext(context.Background(),
		`INSERT INTO commands (device_id, org_id, created_by, kind, shell, timeout_s, status, created_at)
		 VALUES (?, ?, ?, 'shell', 'bash', ?, 'sent', ?)`,
		dev.ID, org.ID, u.ID, protocol.DefaultShellTimeoutS, "2026-11-13T10:00:00Z")
	if err != nil {
		t.Fatalf("insert shell command: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insert shell command id: %v", err)
	}
	return id
}

func TestShellSessions(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, u, dev, _ := fixture(t, s)
	cmdID := insertShellCommand(t, s, dev, org, u)

	// Before the row exists, reads and updates report ErrNotFound.
	if _, err := s.GetShellSession(ctx, cmdID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetShellSession before create = %v", err)
	}
	if err := s.SetShellConnected(ctx, cmdID, c.now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetShellConnected before create = %v", err)
	}
	if err := s.FinishShellSession(ctx, cmdID, FinishShellParams{ClosedAt: c.now()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FinishShellSession before create = %v", err)
	}

	// Create, then the row has all-zero defaults and no timestamps.
	if err := s.CreateShellSession(ctx, cmdID); err != nil {
		t.Fatalf("CreateShellSession: %v", err)
	}
	if err := s.CreateShellSession(ctx, cmdID); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate CreateShellSession = %v", err)
	}
	if err := s.CreateShellSession(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateShellSession for missing command = %v", err)
	}

	sess, err := s.GetShellSession(ctx, cmdID)
	if err != nil {
		t.Fatalf("GetShellSession: %v", err)
	}
	if sess.CommandID != cmdID || sess.ConnectedAt != nil || sess.ClosedAt != nil ||
		sess.BytesIn != 0 || sess.BytesOut != 0 || len(sess.Transcript) != 0 ||
		sess.TranscriptTruncated || sess.CloseReason != "" {
		t.Fatalf("fresh shell session: %+v", sess)
	}

	// Agent attaches.
	connAt := c.now()
	if err := s.SetShellConnected(ctx, cmdID, connAt); err != nil {
		t.Fatalf("SetShellConnected: %v", err)
	}
	sess, _ = s.GetShellSession(ctx, cmdID)
	if sess.ConnectedAt == nil || !sess.ConnectedAt.Equal(storedTime(connAt)) {
		t.Fatalf("connected_at = %v want %v", sess.ConnectedAt, storedTime(connAt))
	}

	// Close: byte counts, close reason and the transcript (binary-safe,
	// including NUL and high bytes) are flushed once.
	c.advance(90 * time.Second)
	closedAt := c.now()
	transcript := []byte("hello\r\nworld\x00\xff done\r\n")
	fin := FinishShellParams{
		ClosedAt: closedAt, BytesIn: 128, BytesOut: 4096,
		Transcript: transcript, TranscriptTruncated: false,
		CloseReason: protocol.CloseTechnicianClosed,
	}
	if err := s.FinishShellSession(ctx, cmdID, fin); err != nil {
		t.Fatalf("FinishShellSession: %v", err)
	}
	sess, _ = s.GetShellSession(ctx, cmdID)
	if sess.BytesIn != 128 || sess.BytesOut != 4096 ||
		sess.CloseReason != protocol.CloseTechnicianClosed || sess.TranscriptTruncated ||
		!bytes.Equal(sess.Transcript, transcript) ||
		sess.ClosedAt == nil || !sess.ClosedAt.Equal(storedTime(closedAt)) ||
		sess.ConnectedAt == nil || !sess.ConnectedAt.Equal(storedTime(connAt)) {
		t.Fatalf("finished shell session: %+v", sess)
	}

	// A transcript over the cap is clamped and the truncated flag forced on.
	cmd2 := insertShellCommand(t, s, dev, org, u)
	if err := s.CreateShellSession(ctx, cmd2); err != nil {
		t.Fatalf("CreateShellSession cmd2: %v", err)
	}
	big := bytes.Repeat([]byte("a"), protocol.MaxTranscriptBytes+500)
	if err := s.FinishShellSession(ctx, cmd2, FinishShellParams{
		ClosedAt: c.now(), Transcript: big, CloseReason: protocol.CloseMaxDuration,
	}); err != nil {
		t.Fatalf("FinishShellSession cmd2: %v", err)
	}
	sess2, _ := s.GetShellSession(ctx, cmd2)
	if len(sess2.Transcript) != protocol.MaxTranscriptBytes || !sess2.TranscriptTruncated {
		t.Fatalf("transcript cap: len=%d truncated=%v", len(sess2.Transcript), sess2.TranscriptTruncated)
	}

	// Agent-never-connected close keeps connected_at NULL.
	cmd3 := insertShellCommand(t, s, dev, org, u)
	if err := s.CreateShellSession(ctx, cmd3); err != nil {
		t.Fatalf("CreateShellSession cmd3: %v", err)
	}
	if err := s.FinishShellSession(ctx, cmd3, FinishShellParams{
		ClosedAt: c.now(), CloseReason: protocol.CloseAgentNeverConnected,
	}); err != nil {
		t.Fatalf("FinishShellSession cmd3: %v", err)
	}
	sess3, _ := s.GetShellSession(ctx, cmd3)
	if sess3.ConnectedAt != nil || sess3.CloseReason != protocol.CloseAgentNeverConnected || len(sess3.Transcript) != 0 {
		t.Fatalf("agent-never-connected session: %+v", sess3)
	}

	// ON DELETE CASCADE: removing the command removes its shell session.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM commands WHERE id = ?`, cmdID); err != nil {
		t.Fatalf("delete command: %v", err)
	}
	if _, err := s.GetShellSession(ctx, cmdID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after command delete, GetShellSession = %v", err)
	}
}
