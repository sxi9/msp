package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Patch status and install (PM1). The agent reports pending OS updates per
// device and installs them on request. Linux uses apt, Windows uses Windows
// Update; both go through a patchManager (one implementation per platform,
// patch_apt.go / patch_windows.go / patch_other.go) whose OS calls run through
// an injectable commandExecutor. Tests feed canned apt output and a captured
// Windows Update JSON fixture, or a fake executor; no test ever mutates the
// real host's packages.

// patchInterval is how often the agent re-scans for pending updates on its own,
// beside the first sync after start and right after a patch command.
const patchInterval = 6 * time.Hour

// cmdResult is the captured output of one external command.
type cmdResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// commandExecutor runs an external command and returns its captured output. The
// production implementation (execRunner) is os/exec; tests inject a fake that
// returns canned output without starting a process, so patch code never touches
// the real package manager.
type commandExecutor interface {
	run(ctx context.Context, env []string, name string, args ...string) (cmdResult, error)
}

// execRunner is the production commandExecutor. A non-zero process exit is
// reported in cmdResult.ExitCode with a nil error; only a failure to start the
// process (or a context cancellation) returns an error. env entries are added to
// the agent's own environment.
type execRunner struct{}

func (execRunner) run(ctx context.Context, env []string, name string, args ...string) (cmdResult, error) {
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		c.Env = append(os.Environ(), env...)
	}
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	res := cmdResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	res.ExitCode = -1
	return res, err
}

// patchManager scans for pending OS updates and installs them. One
// implementation is selected by platform over an injectable commandExecutor.
type patchManager interface {
	// Scan collects the current pending-updates report. A failed scan (apt lock
	// held, apt missing, PowerShell error) is encoded in the report's Error
	// field, never as a returned error, so the caller can keep the last good
	// list and still carry the new error on the wire.
	Scan(ctx context.Context) (protocol.PatchReport, error)
	// Install applies updates for mode (all|security|ids), the named package ids
	// (mode ids only) and the reboot policy (never|if_required). It returns the
	// install log, whether it rebooted, and an error on failure.
	Install(ctx context.Context, mode string, ids []string, reboot string) (output string, rebooted bool, err error)
}

// newPatchManager returns the patch manager for this build's platform over the
// default os/exec executor; newPlatformPatchManager is defined per platform.
func newPatchManager() patchManager { return newPlatformPatchManager(execRunner{}) }

// PatchCollector produces the periodic PatchReport the agent attaches to its
// sync request and runs kind patch commands. It scans on the first Report call,
// every patchInterval after that, and (via a cached report) right after a patch
// command. Between scans Report returns nil so the sync omits the patches field;
// a failed scan keeps the previous good update list and carries the new error.
//
// PatchCollector is the agent's integration surface for PM1: the sync loop calls
// Report each iteration and sets the result on SyncRequest.PatchReport, and the
// command dispatch routes a KindPatch command to RunPatch.
type PatchCollector struct {
	mgr patchManager

	mu       sync.Mutex
	last     *protocol.PatchReport
	lastScan time.Time
	pending  bool // a command re-scanned; send the cached report on the next sync
}

// NewPatchCollector builds the collector over this platform's patch manager.
func NewPatchCollector() *PatchCollector { return &PatchCollector{mgr: newPatchManager()} }

// newPatchCollectorWith injects a manager for tests.
func newPatchCollectorWith(mgr patchManager) *PatchCollector { return &PatchCollector{mgr: mgr} }

// Report returns the patch report to attach to a sync request, or nil when none
// is due. It sends a report cached by a recent command, else scans when the
// patchInterval has elapsed (or on the first call), else returns nil.
func (p *PatchCollector) Report(ctx context.Context, now time.Time) *protocol.PatchReport {
	p.mu.Lock()
	if p.pending && p.last != nil {
		r := *p.last
		p.pending = false
		p.mu.Unlock()
		return &r
	}
	due := p.last == nil || now.Sub(p.lastScan) >= patchInterval
	p.mu.Unlock()
	if !due {
		return nil
	}
	r := p.collect(ctx, now)
	return &r
}

// collect scans and caches the result, keeping the previous good update list and
// counts when the scan errored (but carrying the new error, checked_at and
// reboot flag).
func (p *PatchCollector) collect(ctx context.Context, now time.Time) protocol.PatchReport {
	report, _ := p.mgr.Scan(ctx)
	if report.CheckedAt.IsZero() {
		report.CheckedAt = now
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if report.Error != "" && p.last != nil {
		merged := *p.last
		merged.Error = report.Error
		merged.CheckedAt = report.CheckedAt
		merged.RebootRequired = report.RebootRequired
		report = merged
	}
	cp := report
	p.last = &cp
	p.lastScan = now
	return report
}

// rescan re-collects after a patch command and arranges for the fresh report to
// ride the next sync.
func (p *PatchCollector) rescan(ctx context.Context, now time.Time) protocol.PatchReport {
	r := p.collect(ctx, now)
	p.mu.Lock()
	p.pending = true
	p.mu.Unlock()
	return r
}

// RunPatch executes a kind patch command (scan or install) and returns its
// CommandResult. A scan re-collects the report (the result is a short counts
// summary); an install applies the updates and returns the install log, then
// re-collects so the next sync carries fresh counts. The full package list is
// never logged — only this CommandResult carries it, exactly like exec/script.
func (p *PatchCollector) RunPatch(ctx context.Context, cmd protocol.Command) protocol.CommandResult {
	started := time.Now().UTC()

	// Bound the whole command by its timeout_s (default DefaultPatchTimeoutS,
	// max MaxPatchTimeoutS): a patch install can be slow, so it has its own
	// ceiling above MaxTimeoutS.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(clampPatchTimeout(cmd.TimeoutS))*time.Second)
	defer cancel()

	action := cmd.Action
	if action == "" {
		action = protocol.PatchActionInstall
	}

	if action == protocol.PatchActionScan {
		report := p.rescan(ctx, started)
		res := protocol.CommandResult{StartedAt: started, FinishedAt: time.Now().UTC(), Stdout: summarizePatchReport(report)}
		if report.Error != "" {
			res.ExitCode = 1
			res.Error = report.Error
		}
		return res
	}

	mode := cmd.Mode
	if mode == "" {
		mode = protocol.PatchModeSecurity
	}
	reboot := cmd.Reboot
	if reboot == "" {
		reboot = protocol.PatchRebootNever
	}

	// ids must be a subset of the pending list; the shape check is the server's
	// (Command.Validate), the subset check is the agent's.
	if mode == protocol.PatchModeIDs {
		if len(cmd.IDs) == 0 {
			return patchFailure(started, "install mode ids requires at least one package id")
		}
		pending := p.collect(ctx, started)
		known := map[string]bool{}
		for _, u := range pending.Updates {
			known[u.Name] = true
		}
		var unknown []string
		for _, id := range cmd.IDs {
			if !known[id] {
				unknown = append(unknown, id)
			}
		}
		if len(unknown) > 0 {
			return patchFailure(started, "package ids not in the pending list: "+strings.Join(unknown, ", "))
		}
	}

	out, rebooted, err := p.mgr.Install(ctx, mode, cmd.IDs, reboot)
	finished := time.Now().UTC()
	// Refresh the report regardless of success so the next sync is accurate.
	p.rescan(ctx, finished)

	stdout, truncated := capLog(out)
	res := protocol.CommandResult{StartedAt: started, FinishedAt: finished, Stdout: stdout, Truncated: truncated}
	if err != nil {
		res.ExitCode = 1
		res.Error = err.Error()
		return res
	}
	if rebooted {
		if res.Stdout != "" {
			res.Stdout += "\n"
		}
		res.Stdout += "reboot initiated (reboot_required cleared)"
	}
	return res
}

// patchFailure is the result for a patch command that could not run as asked.
func patchFailure(started time.Time, msg string) protocol.CommandResult {
	return protocol.CommandResult{ExitCode: 1, StartedAt: started, FinishedAt: time.Now().UTC(), Error: msg}
}

// clampPatchTimeout maps 0 to protocol.DefaultPatchTimeoutS and clamps every
// other value into 1..protocol.MaxPatchTimeoutS.
func clampPatchTimeout(n int) int {
	if n == 0 {
		return protocol.DefaultPatchTimeoutS
	}
	if n < 1 {
		return 1
	}
	if n > protocol.MaxPatchTimeoutS {
		return protocol.MaxPatchTimeoutS
	}
	return n
}

// capLog truncates an install log to protocol.MaxOutputBytes and reports whether
// it was cut.
func capLog(s string) (string, bool) {
	if len(s) <= protocol.MaxOutputBytes {
		return s, false
	}
	return s[:protocol.MaxOutputBytes], true
}

// summarizePatchReport is the short command result for a scan.
func summarizePatchReport(r protocol.PatchReport) string {
	if r.Error != "" {
		return "patch scan failed: " + r.Error
	}
	s := fmt.Sprintf("%d update(s) pending, %d security (manager %s)", r.Count, r.SecurityCount, r.Manager)
	if r.RebootRequired {
		s += "; reboot required"
	}
	return s
}

// finalizePatchReport normalises a freshly scanned report: it bounds each field,
// fills Count/SecurityCount from the full list, sorts the updates (security
// first then name), caps the list at protocol.MaxPatchUpdates and ensures a
// non-nil slice and timestamp. Counts are taken before the cap so a host with
// more than the cap is never understated (the store keeps the larger of the two).
func finalizePatchReport(r protocol.PatchReport, now time.Time) protocol.PatchReport {
	if r.CheckedAt.IsZero() {
		r.CheckedAt = now
	}
	for i := range r.Updates {
		r.Updates[i].Name = cut(r.Updates[i].Name, protocol.MaxPatchNameLen)
		r.Updates[i].Current = cut(r.Updates[i].Current, protocol.MaxPatchVersionLen)
		r.Updates[i].New = cut(r.Updates[i].New, protocol.MaxPatchVersionLen)
		r.Updates[i].Source = cut(r.Updates[i].Source, protocol.MaxPatchSourceLen)
	}
	sort.SliceStable(r.Updates, func(i, j int) bool {
		a, b := r.Updates[i], r.Updates[j]
		if a.Security != b.Security {
			return a.Security // security first
		}
		return a.Name < b.Name
	})
	r.Count = len(r.Updates)
	sec := 0
	for _, u := range r.Updates {
		if u.Security {
			sec++
		}
	}
	r.SecurityCount = sec
	if len(r.Updates) > protocol.MaxPatchUpdates {
		r.Updates = r.Updates[:protocol.MaxPatchUpdates]
	}
	if r.Updates == nil {
		r.Updates = []protocol.PatchUpdate{}
	}
	return r
}

// cut bounds a string to max bytes (patch fields are ASCII: package names,
// versions and sources).
func cut(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// joinOutput combines a command's stdout and stderr into one log, each trimmed
// of trailing newlines.
func joinOutput(res cmdResult) string {
	out := strings.TrimRight(res.Stdout, "\n")
	if s := strings.TrimRight(res.Stderr, "\n"); s != "" {
		if out != "" {
			out += "\n"
		}
		out += s
	}
	return out
}

// securityFromSuite reports whether an apt origin/suite names a security pocket
// (Ubuntu ".../noble-security", Debian "Debian-Security:...").
func securityFromSuite(s string) bool {
	return strings.Contains(strings.ToLower(s), "security")
}

// parseAptSimulate parses `apt-get -s dist-upgrade` output into pending updates.
// It reads the `Inst <name> [<current>] (<new> <origin...> [<arch>])` lines (the
// optional `[<current>]` is absent for a brand-new dependency); security is
// detected from the origin naming a security suite. It is a pure function so it
// is unit-tested against captured apt output.
func parseAptSimulate(out string) []protocol.PatchUpdate {
	var ups []protocol.PatchUpdate
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "Inst "))
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			continue
		}
		name := rest[:sp]
		rest = strings.TrimSpace(rest[sp+1:])

		current := ""
		if strings.HasPrefix(rest, "[") {
			if end := strings.IndexByte(rest, ']'); end > 0 {
				current = rest[1:end]
				rest = strings.TrimSpace(rest[end+1:])
			}
		}

		newVer, source := "", ""
		if strings.HasPrefix(rest, "(") {
			inner := rest[1:]
			if end := strings.IndexByte(inner, ')'); end >= 0 {
				inner = inner[:end]
			}
			fields := strings.Fields(inner)
			if len(fields) > 0 {
				newVer = fields[0]
			}
			var origin []string
			for _, f := range fields[1:] {
				if strings.HasPrefix(f, "[") { // the [arch] token ends the origin
					break
				}
				origin = append(origin, f)
			}
			source = strings.Join(origin, " ")
		}

		ups = append(ups, protocol.PatchUpdate{
			Name:     name,
			Current:  current,
			New:      newVer,
			Security: securityFromSuite(source),
			Source:   source,
		})
	}
	return ups
}

// wuReport is the JSON a Windows Update scan script emits; parseWindowsUpdates
// turns it into a PatchReport. Kept in the shared file (not patch_windows.go) so
// the pure parser is compiled and unit-tested on Linux against a captured
// fixture.
type wuReport struct {
	RebootRequired bool       `json:"reboot_required"`
	Error          string     `json:"error"`
	Updates        []wuUpdate `json:"updates"`
}

type wuUpdate struct {
	Name         string   `json:"name"`
	KB           string   `json:"kb"`
	Categories   []string `json:"categories"`
	MsrcSeverity string   `json:"msrc_severity"`
	IsSecurity   bool     `json:"is_security"`
}

// parseWindowsUpdates parses the Windows Update scan JSON into a PatchReport.
// Windows updates carry no simple from/to version, so Current/New stay empty and
// the KB, categories and MSRC severity go into Source. Security is the script's
// flag or any category naming "Security".
func parseWindowsUpdates(data []byte, now time.Time) (protocol.PatchReport, error) {
	report := protocol.PatchReport{Manager: protocol.PatchManagerWU, CheckedAt: now, Updates: []protocol.PatchUpdate{}}
	var raw wuReport
	if err := json.Unmarshal(data, &raw); err != nil {
		return report, err
	}
	report.RebootRequired = raw.RebootRequired
	report.Error = raw.Error
	for _, u := range raw.Updates {
		report.Updates = append(report.Updates, protocol.PatchUpdate{
			Name:     u.Name,
			Security: u.IsSecurity || anyCategorySecurity(u.Categories),
			Source:   wuSource(u),
		})
	}
	return finalizePatchReport(report, now), nil
}

// wuSource renders a Windows update's provenance: KB, categories and severity.
func wuSource(u wuUpdate) string {
	var parts []string
	if u.KB != "" {
		parts = append(parts, u.KB)
	}
	if len(u.Categories) > 0 {
		parts = append(parts, strings.Join(u.Categories, ", "))
	}
	s := strings.Join(parts, " ")
	if u.MsrcSeverity != "" {
		if s != "" {
			s += " "
		}
		s += "(" + u.MsrcSeverity + ")"
	}
	return s
}

// parseWindowsInstallResult reads the trailing {"rebooted":bool,"error":string}
// JSON line the install script prints. A line without that object (the script
// died before printing it) yields rebooted false and an empty error, so the
// caller falls back to the process exit code.
func parseWindowsInstallResult(out string) (rebooted bool, errMsg string) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") || !strings.Contains(line, "rebooted") {
			continue
		}
		var r struct {
			Rebooted bool   `json:"rebooted"`
			Error    string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &r) == nil {
			return r.Rebooted, r.Error
		}
	}
	return false, ""
}

// anyCategorySecurity reports whether any category names a security update.
func anyCategorySecurity(cats []string) bool {
	for _, c := range cats {
		if strings.Contains(strings.ToLower(c), "security") {
			return true
		}
	}
	return false
}
