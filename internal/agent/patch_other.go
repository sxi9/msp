//go:build !linux && !windows

package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// noneManager is the patch manager for platforms without desktop package
// management (the android/arm64 and darwin cross-builds). It reports manager
// none with an empty update list and refuses installs. The real Android agent
// (android/, Kotlin) never sends a patch report at all.
type noneManager struct{}

// newPlatformPatchManager builds the no-op patch manager.
func newPlatformPatchManager(commandExecutor) patchManager { return noneManager{} }

// Scan reports manager none with an empty list and the current timestamp.
func (noneManager) Scan(ctx context.Context) (protocol.PatchReport, error) {
	now := time.Now().UTC()
	return finalizePatchReport(protocol.PatchReport{Manager: protocol.PatchManagerNone, CheckedAt: now}, now), nil
}

// Install always fails: there is no package manager to drive.
func (noneManager) Install(ctx context.Context, mode string, ids []string, reboot string) (string, bool, error) {
	return "", false, fmt.Errorf("no supported package manager on this platform")
}
