//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// rebootRequiredFile exists on Debian/Ubuntu when a prior upgrade needs a reboot
// (update-notifier-common / unattended-upgrades write it). A variable so tests
// can point it at a temp path.
var rebootRequiredFile = "/var/run/reboot-required"

// aptManager scans and installs updates on Debian/Ubuntu via apt. Every OS call
// goes through the injected commandExecutor, so tests feed canned apt output and
// a fake apt-get and no test ever mutates the container's packages.
type aptManager struct {
	exec commandExecutor
	now  func() time.Time
}

// newPlatformPatchManager builds the Linux (apt) patch manager.
func newPlatformPatchManager(x commandExecutor) patchManager {
	return &aptManager{exec: x, now: func() time.Time { return time.Now().UTC() }}
}

// Scan lists pending updates from `apt-get -s dist-upgrade` (a simulation, so it
// changes nothing) and reads /var/run/reboot-required for the reboot flag. A
// failed scan (apt lock held, apt missing) is recorded in the report's Error.
func (m *aptManager) Scan(ctx context.Context) (protocol.PatchReport, error) {
	now := m.now()
	report := protocol.PatchReport{
		Manager:        protocol.PatchManagerApt,
		CheckedAt:      now,
		RebootRequired: fileExists(rebootRequiredFile),
		Updates:        []protocol.PatchUpdate{},
	}
	res, err := m.exec.run(ctx, nil, "apt-get", "-s", "dist-upgrade")
	if err != nil {
		report.Error = aptError("apt-get -s dist-upgrade", res, err)
		return finalizePatchReport(report, now), nil
	}
	if res.ExitCode != 0 {
		report.Error = aptError("apt-get -s dist-upgrade", res, nil)
		return finalizePatchReport(report, now), nil
	}
	report.Updates = parseAptSimulate(res.Stdout)
	return finalizePatchReport(report, now), nil
}

// Install applies updates with apt-get under DEBIAN_FRONTEND=noninteractive:
// `apt-get -y upgrade` for all; the security subset (from a scan) or the named
// ids via `apt-get -y --only-upgrade install <names>`. With reboot=if_required
// it runs `systemctl reboot` only when /var/run/reboot-required exists.
func (m *aptManager) Install(ctx context.Context, mode string, ids []string, reboot string) (string, bool, error) {
	env := []string{"DEBIAN_FRONTEND=noninteractive"}
	var argv []string
	switch mode {
	case protocol.PatchModeAll:
		argv = []string{"apt-get", "-y", "upgrade"}
	case protocol.PatchModeSecurity:
		names, err := m.securityNames(ctx)
		if err != nil {
			return "", false, err
		}
		if len(names) == 0 {
			return "no security updates pending", false, nil
		}
		argv = append([]string{"apt-get", "-y", "--only-upgrade", "install"}, names...)
	case protocol.PatchModeIDs:
		if len(ids) == 0 {
			return "", false, fmt.Errorf("install mode ids requires at least one package id")
		}
		argv = append([]string{"apt-get", "-y", "--only-upgrade", "install"}, ids...)
	default:
		return "", false, fmt.Errorf("unknown patch mode %q", mode)
	}

	res, err := m.exec.run(ctx, env, argv[0], argv[1:]...)
	log := joinOutput(res)
	if err != nil {
		return log, false, fmt.Errorf("%s: %w", argv[0], err)
	}
	if res.ExitCode != 0 {
		return log, false, fmt.Errorf("%s exited %d", strings.Join(argv, " "), res.ExitCode)
	}

	if reboot == protocol.PatchRebootIfRequired && fileExists(rebootRequiredFile) {
		rres, rerr := m.exec.run(ctx, nil, "systemctl", "reboot")
		if out := joinOutput(rres); out != "" {
			log += "\n" + out
		}
		if rerr != nil {
			return log, false, fmt.Errorf("systemctl reboot: %w", rerr)
		}
		if rres.ExitCode != 0 {
			return log, false, fmt.Errorf("systemctl reboot exited %d", rres.ExitCode)
		}
		return log, true, nil
	}
	return log, false, nil
}

// securityNames returns the names of the pending security updates from a scan.
func (m *aptManager) securityNames(ctx context.Context) ([]string, error) {
	report, _ := m.Scan(ctx)
	if report.Error != "" {
		return nil, fmt.Errorf("scan for security updates: %s", report.Error)
	}
	var names []string
	for _, u := range report.Updates {
		if u.Security {
			names = append(names, u.Name)
		}
	}
	return names, nil
}

// aptError builds a scan error from a command's output: the first stderr (then
// stdout) line, falling back to the exit code; the lock message surfaces as-is.
func aptError(cmd string, res cmdResult, err error) string {
	if err != nil {
		return fmt.Sprintf("%s: %v", cmd, err)
	}
	msg := firstLineOf(strings.TrimSpace(res.Stderr))
	if msg == "" {
		msg = firstLineOf(strings.TrimSpace(res.Stdout))
	}
	if msg == "" {
		msg = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return fmt.Sprintf("%s: %s", cmd, msg)
}

// fileExists reports whether path is present.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
