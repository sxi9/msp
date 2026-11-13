//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// unitFile is the systemd unit installed by `msp-agent install`. It is
// byte-identical to deploy/msp-agent.service (service_test.go checks).
const unitFile = `[Unit]
Description=MSP Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/msp-agent run --config /etc/msp-agent/agent.json
Restart=always
RestartSec=10
KillMode=process

[Install]
WantedBy=multi-user.target
`

// Install targets; variables so the test can redirect them into a temp dir.
var (
	installBin = "/usr/local/bin/msp-agent"
	unitPath   = "/etc/systemd/system/msp-agent.service"
	systemctl  = func(args ...string) error {
		c := exec.Command("systemctl", args...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
		return nil
	}
)

// Install copies this executable to /usr/local/bin/msp-agent (unless it
// already runs from there), writes the unit, then daemon-reload, enable --now
// and restart (so a re-install picks up the new binary and config).
func Install(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if exe != installBin {
		if err := copyExecutable(exe, installBin); err != nil {
			return fmt.Errorf("install binary: %w", err)
		}
	}
	unit := unitFile
	if cfgPath != "" && cfgPath != "/etc/msp-agent/agent.json" {
		unit = strings.Replace(unit, "--config /etc/msp-agent/agent.json", "--config "+cfgPath, 1)
	}
	unit = strings.Replace(unit, "ExecStart=/usr/local/bin/msp-agent ", "ExecStart="+installBin+" ", 1)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--now", "msp-agent"); err != nil {
		return err
	}
	return systemctl("restart", "msp-agent")
}

// Uninstall stops and disables the unit and removes it; the config file and
// binary are left in place.
func Uninstall() error {
	var errs []error
	if err := systemctl("disable", "--now", "msp-agent"); err != nil {
		errs = append(errs, err)
	}
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := systemctl("daemon-reload"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// IsService is false on Linux: systemd runs the same foreground `run`.
func IsService() bool { return false }

// RunAsService runs the loop until SIGINT or SIGTERM.
func RunAsService(run func(ctx context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}

// copyExecutable copies src to dst (0755) via a temp file + rename so a
// running binary at dst is replaced atomically (no "text file busy").
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".msp-agent-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
