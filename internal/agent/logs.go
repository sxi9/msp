package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Log forwarding (WL1, desktops only). The desktop agent forwards selected log
// lines to the server on the sync request's logs field: journald on Linux
// (logs_linux.go), the Windows Event Log on Windows (logs_windows.go). Forwarding
// is bounded by design — only the device's watched service units, only at
// level >= warning, at most protocol.MaxLogEntries entries and protocol.MaxLogBytes
// of serialised JSON per sync — and the agent advances its source cursor only after
// the server acks the sync (the 200), so entries dropped to fit the body budget
// (boundSyncBody trims logs before software, newest-first) are re-sent on the next
// sync. Every reader goes through the shared injectable commandExecutor, so no test
// tails a real log source, and the pure parsers (parseJournalJSON,
// parseEventLogJSON) are unit-tested on Linux against captured fixtures. The
// forwarded content is never written to the agent's own log.

// logLine is one parsed forwarded entry paired with the opaque source cursor that
// identifies it (a journald __CURSOR on Linux, an event timestamp on Windows). The
// cursor is kept out of protocol.LogEntry (it never goes on the wire) and used only
// to advance the collector's position after an ack.
type logLine struct {
	entry  protocol.LogEntry
	cursor string
}

// journalReader reads new warning+ log entries for this build's platform. One
// implementation is selected by build tag through newPlatformJournalReader
// (logs_linux.go / logs_windows.go / logs_other.go) over the shared
// commandExecutor.
type journalReader interface {
	// read returns warning+ log entries after cursor for the given watched units,
	// oldest-first, each paired with its opaque source cursor. An empty cursor
	// means "from the reader's start time" (the first read after the agent starts,
	// so a backlog of old warnings is not dumped). It bounds its output to at most
	// maxEntries entries and maxBytes of serialised-JSON LogEntry. A read error
	// yields no entries and the error (the sync still goes out without logs).
	read(ctx context.Context, cursor string, units []string, maxEntries, maxBytes int) ([]logLine, error)
}

// newJournalReader returns this platform's reader over the default os/exec
// executor; newPlatformJournalReader is defined per platform.
func newJournalReader(x commandExecutor) journalReader { return newPlatformJournalReader(x) }

// LogCollector forwards bounded warning+ log entries for the device's watched
// units (WL1). It is the agent's integration surface for the log-forwarding half of
// WL1: the sync loop calls Collect with the device's watched service unit names,
// sets the result on SyncRequest.Logs, and — only after the sync is acked (200) —
// calls Ack with the number of entries that actually reached the server
// (len(req.Logs) after boundSyncBody trimming) so the source cursor advances past
// exactly those entries. Build it once (NewLogCollector) alongside the other
// collectors.
type LogCollector struct {
	reader journalReader

	mu     sync.Mutex
	cursor string   // last acked source cursor; "" before the first ack
	batch  []string // per-entry source cursors for the batch the last Collect returned
}

// NewLogCollector builds the collector over this platform's reader.
func NewLogCollector() *LogCollector {
	return &LogCollector{reader: newJournalReader(execRunner{})}
}

// newLogCollectorWith injects a reader for tests.
func newLogCollectorWith(r journalReader) *LogCollector {
	return &LogCollector{reader: r}
}

// Collect reads new warning+ log entries for the given watched units since the last
// acked cursor, oldest-first, bounded to protocol.MaxLogEntries and
// protocol.MaxLogBytes. It records the per-entry cursors internally; the agent MUST
// call Ack with the number of entries it actually sent (after boundSyncBody and a
// 200) so the cursor advances only past acked entries. Collect returns nil when
// there are no watched units (nothing to forward, so log forwarding is off on a
// device with no service watchlist) or no new entries, so SyncRequest.Logs
// (omitempty) is omitted. units are the names of the device's watched service
// items; a nil/empty units slice disables forwarding for this sync.
func (c *LogCollector) Collect(ctx context.Context, units []string) []protocol.LogEntry {
	if len(units) == 0 {
		c.mu.Lock()
		c.batch = nil
		c.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	cursor := c.cursor
	c.mu.Unlock()

	lines, _ := c.reader.read(ctx, cursor, units, protocol.MaxLogEntries, protocol.MaxLogBytes)
	if len(lines) == 0 {
		c.mu.Lock()
		c.batch = nil
		c.mu.Unlock()
		return nil
	}
	entries := make([]protocol.LogEntry, len(lines))
	cursors := make([]string, len(lines))
	for i, l := range lines {
		entries[i] = l.entry
		cursors[i] = l.cursor
	}
	c.mu.Lock()
	c.batch = cursors
	c.mu.Unlock()
	return entries
}

// Ack advances the stored cursor past the first sent entries of the batch the last
// Collect returned, called by the agent after the sync is acked (200). Passing the
// number of entries actually on the wire (len(req.Logs) after boundSyncBody)
// advances the cursor only to the last entry that reached the server; the dropped
// newest entries are re-read on the next Collect. sent <= 0 (or no batch) advances
// nothing, so a failed sync re-sends the same lines.
func (c *LogCollector) Ack(sent int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sent <= 0 || len(c.batch) == 0 {
		return
	}
	if sent > len(c.batch) {
		sent = len(c.batch)
	}
	c.cursor = c.batch[sent-1]
	c.batch = nil
}

// journalPriorities maps a syslog/journald PRIORITY number (0..7) to its label.
var journalPriorities = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// rawJournal is the subset of a `journalctl -o json` object parseJournalJSON reads.
// Every field is a json.RawMessage because journald encodes a field as a string, a
// number, or (for non-UTF-8 data) an array of byte values; decodeJournalField
// normalises all three.
type rawJournal struct {
	Cursor   json.RawMessage `json:"__CURSOR"`
	Realtime json.RawMessage `json:"__REALTIME_TIMESTAMP"`
	Priority json.RawMessage `json:"PRIORITY"`
	Unit     json.RawMessage `json:"_SYSTEMD_UNIT"`
	Syslog   json.RawMessage `json:"SYSLOG_IDENTIFIER"`
	Message  json.RawMessage `json:"MESSAGE"`
}

// parseJournalJSON parses journalctl -o json output (one JSON object per line,
// oldest-first) into forwarded entries paired with their journal cursors, bounded
// to the oldest-first prefix that fits maxEntries entries and maxBytes of
// serialised-JSON LogEntry. Each entry's timestamp comes from __REALTIME_TIMESTAMP
// (microseconds), its unit from _SYSTEMD_UNIT (falling back to SYSLOG_IDENTIFIER),
// its level from the mapped PRIORITY and its message from MESSAGE. unit and level
// are length-bounded and stripped of control bytes (protocol.LogEntry.Validate
// refuses those); the free-text message is kept, only length-capped. Lines that are
// not a JSON object are skipped. Pure, so it is unit-tested on Linux against a
// captured journalctl fixture.
func parseJournalJSON(data []byte, maxEntries, maxBytes int) []logLine {
	var out []logLine
	total := len("[]")
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var raw rawJournal
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		unit := decodeJournalField(raw.Unit)
		if unit == "" {
			unit = decodeJournalField(raw.Syslog)
		}
		e := protocol.LogEntry{
			At:      journalTime(decodeJournalField(raw.Realtime)),
			Source:  protocol.LogSourceJournald,
			Unit:    cleanField(unit, protocol.MaxLogUnitLen),
			Level:   cleanField(priorityLabel(decodeJournalField(raw.Priority)), protocol.MaxLogLevelLen),
			Message: cutBytes(strings.TrimSpace(decodeJournalField(raw.Message)), protocol.MaxLogMessageLen),
		}
		if !addLogLine(&out, &total, e, decodeJournalField(raw.Cursor), maxEntries, maxBytes) {
			break
		}
	}
	return out
}

// rawEvent is one record of the JSON a Windows Event Log query script emits
// (logs_windows.go); parseEventLogJSON turns a list of them into forwarded entries.
type rawEvent struct {
	Time     string `json:"time"`
	Provider string `json:"provider"`
	Level    string `json:"level"`
	Message  string `json:"message"`
}

// parseEventLogJSON parses the Windows Event Log query JSON (an array of rawEvent,
// or a single object when the query matched one record — ConvertTo-Json drops the
// array) into forwarded entries, oldest-first, bounded to maxEntries entries and
// maxBytes of serialised-JSON LogEntry. The provider is the unit, the Windows level
// name the level, and the event's ISO-8601 timestamp is both the entry time and its
// opaque cursor. Pure, so it is unit-tested on Linux against a captured fixture.
func parseEventLogJSON(data []byte, maxEntries, maxBytes int) []logLine {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' { // a single record is emitted as an object, not an array
		wrapped := make([]byte, 0, len(trimmed)+2)
		wrapped = append(wrapped, '[')
		wrapped = append(wrapped, trimmed...)
		wrapped = append(wrapped, ']')
		trimmed = wrapped
	}
	var raw []rawEvent
	if json.Unmarshal(trimmed, &raw) != nil {
		return nil
	}
	var out []logLine
	total := len("[]")
	for _, ev := range raw {
		e := protocol.LogEntry{
			At:      eventTime(ev.Time),
			Source:  protocol.LogSourceEventlog,
			Unit:    cleanField(ev.Provider, protocol.MaxLogUnitLen),
			Level:   cleanField(ev.Level, protocol.MaxLogLevelLen),
			Message: cutBytes(strings.TrimSpace(ev.Message), protocol.MaxLogMessageLen),
		}
		if !addLogLine(&out, &total, e, ev.Time, maxEntries, maxBytes) {
			break
		}
	}
	return out
}

// addLogLine appends one entry/cursor to out if it keeps the serialised-JSON array
// within maxBytes and out below maxEntries, maintaining the running byte total. It
// returns false when the entry does not fit or the count cap is reached, so the
// caller stops (the batch is the oldest-first prefix that fits).
func addLogLine(out *[]logLine, total *int, e protocol.LogEntry, cursor string, maxEntries, maxBytes int) bool {
	b, err := json.Marshal(e)
	if err != nil {
		return true // skip this entry but keep scanning
	}
	add := len(b)
	if len(*out) > 0 {
		add++ // the comma before this entry
	}
	if *total+add > maxBytes {
		return false
	}
	*total += add
	*out = append(*out, logLine{entry: e, cursor: cursor})
	return len(*out) < maxEntries
}

// decodeJournalField normalises a journald field to a string: a JSON string, a JSON
// number (rendered as-is), or an array of byte values (journald's encoding for a
// non-UTF-8 field, decoded to bytes). An absent or unrecognised field yields "".
func decodeJournalField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	var arr []int
	if json.Unmarshal(raw, &arr) == nil {
		b := make([]byte, 0, len(arr))
		for _, v := range arr {
			if v >= 0 && v <= 255 {
				b = append(b, byte(v))
			}
		}
		return string(b)
	}
	return ""
}

// journalTime parses a journald __REALTIME_TIMESTAMP (microseconds since the Unix
// epoch) into a UTC time, falling back to now on a missing or unparseable value.
func journalTime(micros string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(micros), 10, 64)
	if err != nil || n <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(0, n*1000).UTC()
}

// priorityLabel maps a journald PRIORITY number to its label; a non-numeric or
// out-of-range value is returned unchanged (bounded later).
func priorityLabel(p string) string {
	p = strings.TrimSpace(p)
	if n, err := strconv.Atoi(p); err == nil && n >= 0 && n < len(journalPriorities) {
		return journalPriorities[n]
	}
	return p
}

// eventTime parses an ISO-8601 Event Log timestamp, falling back to now.
func eventTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

// cleanField trims, strips control bytes from, and length-bounds a unit/level
// field. protocol.LogEntry.Validate refuses a control byte in unit or level
// (they surface in the dashboard), so a stray one is removed rather than refused.
func cleanField(s string, max int) string {
	return cutBytes(stripControl(strings.TrimSpace(s)), max)
}

// stripControl removes C0 control bytes and DEL from s, returning s unchanged when
// it has none.
func stripControl(s string) string {
	has := false
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			has = true
			break
		}
	}
	if !has {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x20 && c != 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}
