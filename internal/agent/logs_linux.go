//go:build linux

package agent

import (
	"context"
	"time"
)

// journalctlReader forwards warning+ journald entries via `journalctl -o json`
// through the injected commandExecutor, so tests feed a captured journalctl fixture
// and no real journal is tailed. On the first read (no cursor) it bounds the query
// to entries since the agent started, so a backlog of old warnings is not dumped;
// after that it reads strictly after the last acked journal cursor.
type journalctlReader struct {
	exec  commandExecutor
	since time.Time
}

// newPlatformJournalReader builds the Linux (journalctl) reader.
func newPlatformJournalReader(x commandExecutor) journalReader {
	return &journalctlReader{exec: x, since: time.Now().UTC()}
}

// read runs journalctl for the given units at priority warning and above, in JSON,
// oldest-first, after the stored cursor (or since the reader's start time on the
// first read), and parses a bounded oldest-first batch. It deliberately does not
// pass journalctl's -n/--lines (which would return the newest N and skip older
// unsent entries, letting the cursor jump a gap): the whole after-cursor stream for
// the watched units at warning+ is small, and parseJournalJSON keeps the oldest
// maxEntries that fit so the cursor drains the backlog oldest-first without loss.
func (r *journalctlReader) read(ctx context.Context, cursor string, units []string, maxEntries, maxBytes int) ([]logLine, error) {
	args := []string{"-o", "json", "-p", "warning", "--no-pager", "-q"}
	if cursor != "" {
		args = append(args, "--after-cursor", cursor)
	} else {
		args = append(args, "--since", r.since.Format("2006-01-02 15:04:05"))
	}
	for _, u := range units {
		args = append(args, "-u", u)
	}
	res, err := r.exec.run(ctx, nil, "journalctl", args...)
	if err != nil {
		return nil, err
	}
	return parseJournalJSON([]byte(res.Stdout), maxEntries, maxBytes), nil
}
