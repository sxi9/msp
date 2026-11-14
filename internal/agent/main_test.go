package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// noopPatchReporter is the patch reporter the Run loop uses under test: it never
// scans or installs, so no agent test touches the real package manager (PM1).
// The PatchCollector itself is tested directly with an injected fake executor in
// patch_test.go, and the full agent patch path is proved against a fake apt-get
// in internal/e2e.
type noopPatchReporter struct{}

func (noopPatchReporter) Report(context.Context, time.Time) *protocol.PatchReport { return nil }

func (noopPatchReporter) RunPatch(_ context.Context, _ protocol.Command) protocol.CommandResult {
	now := time.Now().UTC()
	return protocol.CommandResult{StartedAt: now, FinishedAt: now}
}

// TestMain substitutes the no-op patch reporter for every test in this package
// so the Run loop never runs apt.
func TestMain(m *testing.M) {
	newPatchReporter = func() PatchReporter { return noopPatchReporter{} }
	os.Exit(m.Run())
}
