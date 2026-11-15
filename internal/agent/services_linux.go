//go:build linux

package agent

import (
	"context"

	"github.com/sxi9/msp/internal/protocol"
)

// linuxServiceProber reports watched service/process state on Linux: system
// services via one `systemctl is-active` call (parsed by parseIsActive) and
// processes each via `pgrep -x <name>` (exit 0 means a process whose name matches
// exactly is running). Every call goes through the injected commandExecutor, so
// tests fake both and no real service is queried or touched.
type linuxServiceProber struct {
	exec commandExecutor
}

// newPlatformServiceProber builds the Linux (systemctl/pgrep) prober.
func newPlatformServiceProber(x commandExecutor) serviceProber {
	return &linuxServiceProber{exec: x}
}

// Probe batches all service items into a single `systemctl is-active u1 u2 ...`
// (the per-unit state is in stdout regardless of the non-zero exit code; a failure
// to run systemctl leaves every service not running) and checks each process item
// with pgrep.
func (p *linuxServiceProber) Probe(ctx context.Context, items []protocol.WatchItem) []protocol.ServiceState {
	states := make([]protocol.ServiceState, len(items))
	var svcIdx []int
	var svcNames []string
	for i, it := range items {
		states[i] = protocol.ServiceState{Kind: it.Kind, Name: it.Name}
		switch it.Kind {
		case protocol.WatchKindService:
			svcIdx = append(svcIdx, i)
			svcNames = append(svcNames, it.Name)
		case protocol.WatchKindProcess:
			states[i].Running = p.processRunning(ctx, it.Name)
		}
	}
	if len(svcNames) > 0 {
		// systemctl is-active exits non-zero when any unit is inactive; the
		// per-unit words are still on stdout, so the exit code and a start error
		// are ignored and parseIsActive reads the words (a total failure leaves
		// every service not running, the safe default for a service_down alert).
		res, _ := p.exec.run(ctx, nil, "systemctl", append([]string{"is-active"}, svcNames...)...)
		running := parseIsActive(res.Stdout, len(svcNames))
		for k, idx := range svcIdx {
			states[idx].Running = running[k]
		}
	}
	return states
}

// processRunning reports whether a process whose name matches name exactly is
// running, via `pgrep -x <name>` (exit 0 => found, 1 => none). A pgrep that cannot
// run (missing binary, or a pattern error) reports not running.
func (p *linuxServiceProber) processRunning(ctx context.Context, name string) bool {
	res, err := p.exec.run(ctx, nil, "pgrep", "-x", name)
	if err != nil {
		return false
	}
	return res.ExitCode == 0
}
