//go:build !linux && !windows

package agent

import "context"

// noneJournalReader is the log reader for platforms with no supported log source
// (the android/arm64 and darwin cross-builds). It forwards nothing. The real
// Android agent (android/, Kotlin) does not forward logs at all.
type noneJournalReader struct{}

// newPlatformJournalReader builds the no-op log reader.
func newPlatformJournalReader(commandExecutor) journalReader { return noneJournalReader{} }

// read always returns no entries.
func (noneJournalReader) read(ctx context.Context, cursor string, units []string, maxEntries, maxBytes int) ([]logLine, error) {
	return nil, nil
}
