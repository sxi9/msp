//go:build !linux && !windows

package agent

import (
	"context"

	"github.com/sxi9/msp/internal/protocol"
)

// noneServiceProber is the prober for platforms without desktop service/process
// management (the android/arm64 and darwin cross-builds). Those platforms never
// have a watchlist, so Collect returns nil before Probe is reached; this reports
// every item not running and exists only to satisfy the shared seam.
type noneServiceProber struct{}

// newPlatformServiceProber builds the no-op prober.
func newPlatformServiceProber(commandExecutor) serviceProber { return noneServiceProber{} }

// Probe reports every item not running.
func (noneServiceProber) Probe(ctx context.Context, items []protocol.WatchItem) []protocol.ServiceState {
	return notRunning(items)
}
