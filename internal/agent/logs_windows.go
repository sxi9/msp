//go:build windows

package agent

import (
	"context"
	"time"
)

// eventLogReader forwards warning+ Windows Event Log records via a PowerShell
// Get-WinEvent query through the injected commandExecutor. It queries the
// Application and System logs at level Critical/Error/Warning since a stored time
// cursor (the last forwarded record's timestamp), seeded on the first read to the
// agent's start time so a backlog is not dumped. This path cross-builds and its
// parser is unit-tested on Linux against a captured fixture, but it is NOT VERIFIED
// on a real Windows host (docs/STATUS.md); the time cursor can re-send records that
// share the last timestamp — acceptable until the real-hardware pass.
type eventLogReader struct {
	exec  commandExecutor
	since time.Time
}

// newPlatformJournalReader builds the Windows (Event Log) reader.
func newPlatformJournalReader(x commandExecutor) journalReader {
	return &eventLogReader{exec: x, since: time.Now().UTC()}
}

// read runs the Event Log query script with the start time passed through the
// environment and parses a bounded oldest-first batch. units are not used as a
// filter here (Windows provider names are not service names); forwarding stays
// bounded by level, the entry/byte caps and the advancing time cursor.
func (r *eventLogReader) read(ctx context.Context, cursor string, units []string, maxEntries, maxBytes int) ([]logLine, error) {
	start := r.since
	if cursor != "" {
		if t, err := time.Parse(time.RFC3339Nano, cursor); err == nil {
			start = t
		}
	}
	env := []string{"MSP_LOG_SINCE=" + start.UTC().Format(time.RFC3339Nano)}
	// psArgs are the shared non-interactive PowerShell flags (patch_windows.go).
	res, err := r.exec.run(ctx, env, "powershell", append(psArgs, eventLogScript)...)
	if err != nil {
		return nil, err
	}
	return parseEventLogJSON([]byte(res.Stdout), maxEntries, maxBytes), nil
}

// eventLogScript queries the Application and System logs at level Critical(1),
// Error(2) and Warning(3) since MSP_LOG_SINCE, oldest-first, and emits the JSON
// parseEventLogJSON expects ({time,provider,level,message} per record). A query
// that matches nothing prints nothing.
const eventLogScript = `
$ErrorActionPreference = 'SilentlyContinue'
$since = Get-Date
if ($env:MSP_LOG_SINCE) { $since = [datetime]::Parse($env:MSP_LOG_SINCE).ToUniversalTime() }
$events = Get-WinEvent -FilterHashtable @{ LogName = @('Application','System'); Level = @(1,2,3); StartTime = $since } -ErrorAction SilentlyContinue
if ($events) {
  $events | Sort-Object TimeCreated |
    Select-Object @{ n='time'; e={ $_.TimeCreated.ToUniversalTime().ToString('o') } }, @{ n='provider'; e={ $_.ProviderName } }, @{ n='level'; e={ $_.LevelDisplayName } }, @{ n='message'; e={ $_.Message } } |
    ConvertTo-Json -Depth 3
}
`
