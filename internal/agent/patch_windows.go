//go:build windows

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// wuManager scans and installs updates through the Windows Update COM API
// (Microsoft.Update.Session) driven from PowerShell, which emits JSON parsed by
// the shared parseWindowsUpdates. Every call goes through the injected
// commandExecutor. This path cross-builds and its parser is unit-tested against
// a captured fixture, but it is NOT VERIFIED on a real Windows host (docs/STATUS.md).
type wuManager struct {
	exec commandExecutor
	now  func() time.Time
}

// newPlatformPatchManager builds the Windows (Windows Update) patch manager.
func newPlatformPatchManager(x commandExecutor) patchManager {
	return &wuManager{exec: x, now: func() time.Time { return time.Now().UTC() }}
}

// psArgs are the fixed PowerShell flags for a non-interactive COM-driven script.
var psArgs = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}

// Scan searches Windows Update for pending updates and returns the parsed report.
// A PowerShell/COM failure is recorded in the report's Error.
func (m *wuManager) Scan(ctx context.Context) (protocol.PatchReport, error) {
	now := m.now()
	res, err := m.exec.run(ctx, nil, "powershell", append(psArgs, wuScanScript)...)
	if err != nil {
		return finalizePatchReport(protocol.PatchReport{Manager: protocol.PatchManagerWU, CheckedAt: now, Error: "windows update scan: " + err.Error()}, now), nil
	}
	if res.ExitCode != 0 {
		msg := firstLineOf(strings.TrimSpace(res.Stderr))
		if msg == "" {
			msg = fmt.Sprintf("exit %d", res.ExitCode)
		}
		return finalizePatchReport(protocol.PatchReport{Manager: protocol.PatchManagerWU, CheckedAt: now, Error: "windows update scan: " + msg}, now), nil
	}
	report, perr := parseWindowsUpdates([]byte(res.Stdout), now)
	if perr != nil {
		report.Error = "parsing windows update output: " + perr.Error()
		report.Manager = protocol.PatchManagerWU
		report.CheckedAt = now
		if report.Updates == nil {
			report.Updates = []protocol.PatchUpdate{}
		}
	}
	return report, nil
}

// Install downloads and installs updates for the given mode/ids and, with
// reboot=if_required, reboots when Windows Update reports a reboot is pending.
// The mode, ids and reboot policy are passed to the install script through the
// environment. The install log is the script's output; the trailing JSON line
// carries the rebooted flag and any error.
func (m *wuManager) Install(ctx context.Context, mode string, ids []string, reboot string) (string, bool, error) {
	env := []string{
		"MSP_PATCH_MODE=" + mode,
		"MSP_PATCH_IDS=" + strings.Join(ids, ","),
		"MSP_PATCH_REBOOT=" + reboot,
	}
	res, err := m.exec.run(ctx, env, "powershell", append(psArgs, wuInstallScript)...)
	log := joinOutput(res)
	if err != nil {
		return log, false, fmt.Errorf("windows update install: %w", err)
	}
	rebooted, errMsg := parseWindowsInstallResult(res.Stdout)
	if errMsg != "" {
		return log, rebooted, fmt.Errorf("windows update install: %s", errMsg)
	}
	if res.ExitCode != 0 {
		return log, rebooted, fmt.Errorf("windows update install exited %d", res.ExitCode)
	}
	return log, rebooted, nil
}

// wuScanScript enumerates pending updates via the Update COM API and prints the
// JSON parseWindowsUpdates expects. IsInstalled=0 and IsHidden=0 selects what is
// pending; MsrcSeverity and the category names feed security detection.
const wuScanScript = `
$ErrorActionPreference = 'Stop'
try {
  $session = New-Object -ComObject Microsoft.Update.Session
  $searcher = $session.CreateUpdateSearcher()
  $result = $searcher.Search("IsInstalled=0 and IsHidden=0")
  $updates = @()
  foreach ($u in $result.Updates) {
    $cats = @(); foreach ($c in $u.Categories) { $cats += $c.Name }
    $kb = ''; if ($u.KBArticleIDs.Count -gt 0) { $kb = 'KB' + $u.KBArticleIDs.Item(0) }
    $isSec = $false; foreach ($c in $cats) { if ($c -match 'Security') { $isSec = $true } }
    $updates += [pscustomobject]@{
      name = $u.Title; kb = $kb; categories = $cats;
      msrc_severity = [string]$u.MsrcSeverity; is_security = $isSec
    }
  }
  $reboot = $false
  try { $reboot = (New-Object -ComObject Microsoft.Update.SystemInfo).RebootRequired } catch {}
  [pscustomobject]@{ reboot_required = $reboot; error = ''; updates = $updates } | ConvertTo-Json -Depth 4
} catch {
  [pscustomobject]@{ reboot_required = $false; error = $_.Exception.Message; updates = @() } | ConvertTo-Json -Depth 4
}
`

// wuInstallScript downloads and installs the updates selected by MSP_PATCH_MODE
// (all | security | ids, with MSP_PATCH_IDS a comma list of KB numbers for ids)
// and reboots when MSP_PATCH_REBOOT=if_required and a reboot is pending. It
// prints a human log and a final JSON line {"rebooted":bool,"error":string}.
const wuInstallScript = `
$ErrorActionPreference = 'Stop'
$mode = $env:MSP_PATCH_MODE; if (-not $mode) { $mode = 'security' }
$ids = @(); if ($env:MSP_PATCH_IDS) { $ids = $env:MSP_PATCH_IDS.Split(',') }
$rebootPolicy = $env:MSP_PATCH_REBOOT; if (-not $rebootPolicy) { $rebootPolicy = 'never' }
$rebooted = $false
try {
  $session = New-Object -ComObject Microsoft.Update.Session
  $searcher = $session.CreateUpdateSearcher()
  $result = $searcher.Search("IsInstalled=0 and IsHidden=0")
  $toInstall = New-Object -ComObject Microsoft.Update.UpdateColl
  foreach ($u in $result.Updates) {
    $isSec = $false; foreach ($c in $u.Categories) { if ($c.Name -match 'Security') { $isSec = $true } }
    $kb = ''; if ($u.KBArticleIDs.Count -gt 0) { $kb = 'KB' + $u.KBArticleIDs.Item(0) }
    $pick = $false
    switch ($mode) {
      'all'      { $pick = $true }
      'security' { $pick = $isSec }
      'ids'      { $pick = ($ids -contains $kb) -or ($ids -contains $u.Title) }
    }
    if ($pick) { Write-Output ("install: " + $u.Title); $toInstall.Add($u) | Out-Null }
  }
  if ($toInstall.Count -eq 0) {
    Write-Output 'no matching updates'
  } else {
    $dl = $session.CreateUpdateDownloader(); $dl.Updates = $toInstall; $dl.Download() | Out-Null
    $inst = $session.CreateUpdateInstaller(); $inst.Updates = $toInstall
    $r = $inst.Install()
    Write-Output ("result code: " + $r.ResultCode)
    if ($rebootPolicy -eq 'if_required' -and $r.RebootRequired) {
      $rebooted = $true
      Write-Output 'rebooting'
    }
  }
  [pscustomobject]@{ rebooted = $rebooted; error = '' } | ConvertTo-Json -Compress
  if ($rebooted) { Restart-Computer -Force }
} catch {
  [pscustomobject]@{ rebooted = $false; error = $_.Exception.Message } | ConvertTo-Json -Compress
}
`
