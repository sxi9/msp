//go:build windows

package agent

import (
	"context"
	"strings"

	"github.com/sxi9/msp/internal/protocol"
)

// windowsServiceProber reports watched service/process state on Windows by driving
// a PowerShell snapshot script through the injected commandExecutor: Get-Service
// for each watched service name and a single Get-Process snapshot for the watched
// processes. The script emits the JSON winServiceSnapshot, which the shared
// parseWinServiceStates maps back onto the watch items. This path cross-builds and
// its parser is unit-tested on Linux against a captured fixture, but it is NOT
// VERIFIED on a real Windows host (docs/STATUS.md).
type windowsServiceProber struct {
	exec commandExecutor
}

// newPlatformServiceProber builds the Windows (SCM/process) prober.
func newPlatformServiceProber(x commandExecutor) serviceProber {
	return &windowsServiceProber{exec: x}
}

// Probe passes the watched service and process names to the snapshot script via
// the environment (newline-separated), runs it, and maps the JSON result back onto
// the items. A PowerShell failure leaves every item not running.
func (p *windowsServiceProber) Probe(ctx context.Context, items []protocol.WatchItem) []protocol.ServiceState {
	var svc, proc []string
	for _, it := range items {
		switch it.Kind {
		case protocol.WatchKindService:
			svc = append(svc, it.Name)
		case protocol.WatchKindProcess:
			proc = append(proc, it.Name)
		}
	}
	env := []string{
		"MSP_WATCH_SERVICES=" + strings.Join(svc, "\n"),
		"MSP_WATCH_PROCESSES=" + strings.Join(proc, "\n"),
	}
	// psArgs are the shared non-interactive PowerShell flags (patch_windows.go).
	res, err := p.exec.run(ctx, env, "powershell", append(psArgs, winServiceScript)...)
	if err != nil {
		return notRunning(items)
	}
	return parseWinServiceStates([]byte(res.Stdout), items)
}

// winServiceScript queries each watched service's status and snapshots running
// processes, emitting the JSON winServiceSnapshot (lower-cased name -> running). A
// service that does not exist is reported not running; the process snapshot keys
// every running process's base name.
const winServiceScript = `
$ErrorActionPreference = 'SilentlyContinue'
$svcNames = @(); if ($env:MSP_WATCH_SERVICES) { $svcNames = $env:MSP_WATCH_SERVICES.Split("` + "`n" + `") }
$svc = @{}
foreach ($n in $svcNames) {
  if ($n) {
    $s = Get-Service -Name $n -ErrorAction SilentlyContinue
    $svc[$n.ToLower()] = ($s -ne $null -and $s.Status -eq 'Running')
  }
}
$proc = @{}
foreach ($p in Get-Process -ErrorAction SilentlyContinue) { $proc[$p.ProcessName.ToLower()] = $true }
[pscustomobject]@{ services = $svc; processes = $proc; error = '' } | ConvertTo-Json -Depth 4
`
