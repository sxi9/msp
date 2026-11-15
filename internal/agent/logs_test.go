//go:build linux

package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// fakeReader is an injectable journalReader for LogCollector tests. fn returns the
// lines for a given cursor; read records every cursor it was called with.
type fakeReader struct {
	cursors []string
	units   [][]string
	fn      func(cursor string, units []string) []logLine
}

func (f *fakeReader) read(ctx context.Context, cursor string, units []string, maxEntries, maxBytes int) ([]logLine, error) {
	f.cursors = append(f.cursors, cursor)
	f.units = append(f.units, units)
	if f.fn != nil {
		return f.fn(cursor, units), nil
	}
	return nil, nil
}

func TestParseJournalJSON(t *testing.T) {
	data, err := os.ReadFile("testdata/journalctl.json")
	if err != nil {
		t.Fatal(err)
	}
	got := parseJournalJSON(data, protocol.MaxLogEntries, protocol.MaxLogBytes)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	wantUnit := []string{"nginx.service", "nginx.service", "kernel"} // 3rd falls back to SYSLOG_IDENTIFIER
	wantLevel := []string{"warning", "err", "crit"}                  // PRIORITY 4,3,2
	wantMsg := []string{"upstream timed out while connecting", "worker process 1234 exited with code 1", "Hi there"}
	wantCursor := []string{"s=aa;i=1;b=bb", "s=aa;i=2;b=bb", "s=aa;i=3;b=bb"}
	for i, l := range got {
		if l.entry.Source != protocol.LogSourceJournald {
			t.Errorf("entry %d source = %q", i, l.entry.Source)
		}
		if l.entry.Unit != wantUnit[i] {
			t.Errorf("entry %d unit = %q, want %q", i, l.entry.Unit, wantUnit[i])
		}
		if l.entry.Level != wantLevel[i] {
			t.Errorf("entry %d level = %q, want %q", i, l.entry.Level, wantLevel[i])
		}
		if l.entry.Message != wantMsg[i] {
			t.Errorf("entry %d message = %q, want %q", i, l.entry.Message, wantMsg[i])
		}
		if l.cursor != wantCursor[i] {
			t.Errorf("entry %d cursor = %q, want %q", i, l.cursor, wantCursor[i])
		}
		if l.entry.At.IsZero() {
			t.Errorf("entry %d has zero time", i)
		}
		if err := l.entry.Validate(); err != nil {
			t.Errorf("entry %d fails LogEntry.Validate: %v", i, err)
		}
	}
	// The timestamps come from __REALTIME_TIMESTAMP (microseconds), one minute apart.
	if d := got[1].entry.At.Sub(got[0].entry.At); d.Minutes() != 1 {
		t.Errorf("timestamp delta = %v, want 1m", d)
	}
}

func TestParseJournalJSONCaps(t *testing.T) {
	data, err := os.ReadFile("testdata/journalctl.json")
	if err != nil {
		t.Fatal(err)
	}
	// Entry cap: at most maxEntries, oldest-first.
	if got := parseJournalJSON(data, 2, protocol.MaxLogBytes); len(got) != 2 || got[0].cursor != "s=aa;i=1;b=bb" {
		t.Fatalf("entry cap: got %d entries (first %v)", len(got), firstCursor(got))
	}
	// Byte cap: a tiny budget keeps only the oldest entry that fits (or none).
	small := parseJournalJSON(data, protocol.MaxLogEntries, 200)
	if len(small) >= 3 {
		t.Fatalf("byte cap: expected fewer than 3, got %d", len(small))
	}
	if len(small) > 0 && small[0].cursor != "s=aa;i=1;b=bb" {
		t.Fatalf("byte cap keeps oldest-first, got first %q", small[0].cursor)
	}
	// Garbage lines are skipped, not fatal.
	mixed := []byte("not json\n" + string(data) + "\n{bad")
	if got := parseJournalJSON(mixed, protocol.MaxLogEntries, protocol.MaxLogBytes); len(got) != 3 {
		t.Fatalf("mixed input: got %d, want 3", len(got))
	}
}

func TestDecodeJournalFieldAndPriority(t *testing.T) {
	// A non-UTF-8 field is encoded as an array of byte values.
	if s := decodeJournalField([]byte("[72,105]")); s != "Hi" {
		t.Errorf("array decode = %q, want Hi", s)
	}
	if s := decodeJournalField([]byte(`"plain"`)); s != "plain" {
		t.Errorf("string decode = %q", s)
	}
	if s := decodeJournalField([]byte("7")); s != "7" {
		t.Errorf("number decode = %q", s)
	}
	if priorityLabel("4") != "warning" || priorityLabel("0") != "emerg" || priorityLabel("7") != "debug" {
		t.Errorf("priority mapping wrong")
	}
	if priorityLabel("notice") != "notice" { // non-numeric passes through
		t.Errorf("non-numeric priority should pass through")
	}
}

func TestJournalctlReaderArgs(t *testing.T) {
	data, err := os.ReadFile("testdata/journalctl.json")
	if err != nil {
		t.Fatal(err)
	}
	var seen recordedCall
	fx := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		seen = c
		return cmdResult{Stdout: string(data)}, nil
	}}
	r := newPlatformJournalReader(fx)
	units := []string{"nginx.service", "ssh.service"}

	// First read (no cursor): uses --since, not --after-cursor, and -u per unit.
	lines, _ := r.read(context.Background(), "", units, protocol.MaxLogEntries, protocol.MaxLogBytes)
	if len(lines) != 3 {
		t.Fatalf("got %d lines", len(lines))
	}
	if seen.name != "journalctl" {
		t.Fatalf("ran %q", seen.name)
	}
	a := seen.argLine()
	if !strings.Contains(a, "--since") || strings.Contains(a, "--after-cursor") {
		t.Errorf("first read should use --since, args: %s", a)
	}
	if !strings.Contains(a, "-o json") || !strings.Contains(a, "-p warning") {
		t.Errorf("missing json/priority flags: %s", a)
	}
	if !hasFlagValue(seen.args, "-u", "nginx.service") || !hasFlagValue(seen.args, "-u", "ssh.service") {
		t.Errorf("missing -u unit filters: %s", a)
	}

	// Second read with a cursor: uses --after-cursor <cursor>, not --since.
	r.read(context.Background(), "s=aa;i=9;b=bb", units, protocol.MaxLogEntries, protocol.MaxLogBytes)
	a = seen.argLine()
	if !hasFlagValue(seen.args, "--after-cursor", "s=aa;i=9;b=bb") || strings.Contains(a, "--since") {
		t.Errorf("second read should use --after-cursor, args: %s", a)
	}
}

func TestLogCollectorCursorAfterAck(t *testing.T) {
	all := []logLine{
		{entry: logEntry("one"), cursor: "c1"},
		{entry: logEntry("two"), cursor: "c2"},
		{entry: logEntry("three"), cursor: "c3"},
	}
	after := func(cursor string, _ []string) []logLine {
		start := 0
		for i, l := range all {
			if l.cursor == cursor {
				start = i + 1
			}
		}
		return append([]logLine(nil), all[start:]...)
	}
	units := []string{"nginx.service"}

	// Full ack: cursor advances past the whole batch; nothing re-sent.
	fr := &fakeReader{fn: after}
	c := newLogCollectorWith(fr)
	got := c.Collect(context.Background(), units)
	if len(got) != 3 {
		t.Fatalf("first collect got %d, want 3", len(got))
	}
	if fr.cursors[0] != "" {
		t.Fatalf("first read cursor = %q, want empty", fr.cursors[0])
	}
	c.Ack(len(got))
	if got := c.Collect(context.Background(), units); got != nil {
		t.Fatalf("after full ack expected nil, got %v", got)
	}
	if fr.cursors[1] != "c3" {
		t.Fatalf("second read cursor = %q, want c3", fr.cursors[1])
	}

	// Partial ack (boundSyncBody trimmed the newest entry): cursor advances only to
	// the last entry that reached the server, so the dropped entry is re-read.
	fr = &fakeReader{fn: after}
	c = newLogCollectorWith(fr)
	got = c.Collect(context.Background(), units)
	c.Ack(2) // only 2 of 3 fit the body budget
	re := c.Collect(context.Background(), units)
	if len(re) != 1 || re[0].Message != "three" {
		t.Fatalf("partial ack should re-send entry three, got %+v", re)
	}
	if fr.cursors[1] != "c2" {
		t.Fatalf("read after partial ack cursor = %q, want c2", fr.cursors[1])
	}

	// No ack (failed sync): the same batch is re-read from the start.
	fr = &fakeReader{fn: after}
	c = newLogCollectorWith(fr)
	c.Collect(context.Background(), units)
	re = c.Collect(context.Background(), units)
	if len(re) != 3 {
		t.Fatalf("no ack should re-send all 3, got %d", len(re))
	}
	if fr.cursors[1] != "" {
		t.Fatalf("read after no ack cursor = %q, want empty", fr.cursors[1])
	}
}

func TestLogCollectorNoUnits(t *testing.T) {
	fr := &fakeReader{fn: func(string, []string) []logLine { return []logLine{{entry: logEntry("x"), cursor: "c"}} }}
	c := newLogCollectorWith(fr)
	if got := c.Collect(context.Background(), nil); got != nil {
		t.Fatalf("no units should forward nothing, got %v", got)
	}
	if len(fr.cursors) != 0 {
		t.Fatalf("reader should not be called with no units")
	}
	// Ack after an empty collect is a no-op.
	c.Ack(5)
}

func TestParseEventLogJSON(t *testing.T) {
	data, err := os.ReadFile("testdata/eventlog.json")
	if err != nil {
		t.Fatal(err)
	}
	got := parseEventLogJSON(data, protocol.MaxLogEntries, protocol.MaxLogBytes)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].entry.Source != protocol.LogSourceEventlog || got[0].entry.Unit != "Application Error" || got[0].entry.Level != "Error" {
		t.Fatalf("entry 0 = %+v", got[0].entry)
	}
	if got[1].entry.Level != "Warning" || got[1].cursor != "2026-11-15T09:05:30.0000000Z" {
		t.Fatalf("entry 1 = %+v (cursor %q)", got[1].entry, got[1].cursor)
	}
	for i, l := range got {
		if err := l.entry.Validate(); err != nil {
			t.Errorf("event %d fails LogEntry.Validate: %v", i, err)
		}
	}
	// ConvertTo-Json emits a single object (not an array) for one record.
	single := []byte(`{"time":"2026-11-15T10:00:00.0000000Z","provider":"P","level":"Warning","message":"m"}`)
	if got := parseEventLogJSON(single, protocol.MaxLogEntries, protocol.MaxLogBytes); len(got) != 1 || got[0].entry.Unit != "P" {
		t.Fatalf("single-object parse got %+v", got)
	}
	// Empty / garbage input is tolerated.
	if got := parseEventLogJSON(nil, protocol.MaxLogEntries, protocol.MaxLogBytes); got != nil {
		t.Fatalf("nil input got %v", got)
	}
}

func TestCleanField(t *testing.T) {
	// Control bytes are stripped (LogEntry.Validate refuses them in unit/level).
	if got := cleanField("ng\tinx\n.service", protocol.MaxLogUnitLen); got != "nginx.service" {
		t.Fatalf("cleanField = %q", got)
	}
	long := strings.Repeat("u", protocol.MaxLogUnitLen+10)
	if got := cleanField(long, protocol.MaxLogUnitLen); len(got) != protocol.MaxLogUnitLen {
		t.Fatalf("cleanField length = %d, want %d", len(got), protocol.MaxLogUnitLen)
	}
}

// logEntry builds a minimal valid journald LogEntry for collector tests.
func logEntry(msg string) protocol.LogEntry {
	return protocol.LogEntry{Source: protocol.LogSourceJournald, Unit: "nginx.service", Level: "warning", Message: msg}
}

// hasFlagValue reports whether args contains flag immediately followed by value.
func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func firstCursor(ls []logLine) string {
	if len(ls) == 0 {
		return ""
	}
	return ls[0].cursor
}
