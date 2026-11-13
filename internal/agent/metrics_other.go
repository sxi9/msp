//go:build !linux && !windows

package agent

import (
	"context"
	"runtime"

	"github.com/sxi9/msp/internal/protocol"
)

// This file keeps the agent compiling for GOOS values it does not really
// support (android/arm64 and darwin cross-builds are CI proofs only). It
// reports hostname-level facts and no host metrics; the real collectors are
// metrics_linux.go and metrics_windows.go.

// Metrics returns a sample with no CPU, memory or disk data and the local
// IPs. It never fails.
func (c *collector) Metrics(ctx context.Context) (protocol.Metrics, error) {
	return protocol.Metrics{Disks: []protocol.Disk{}, IPs: localIPs()}, nil
}

// Facts reports what the Go runtime knows: the OS name as kernel, the CPU
// count and the local IPs.
func (c *collector) Facts() protocol.Facts {
	return protocol.Facts{
		Kernel:   runtime.GOOS,
		CPUCount: runtime.NumCPU(),
		IPs:      localIPs(),
	}
}

// Uptime is unknown here.
func (c *collector) Uptime() int64 { return 0 }

// osVersion is the GOOS name.
func osVersion() string { return runtime.GOOS }
