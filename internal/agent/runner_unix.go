//go:build !windows

package agent

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// setSysProcAttr puts the child in its own process group so a timeout can
// kill the whole tree the script spawned.
func setSysProcAttr(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess kills the child's process group (kill -9 -pgid); it falls back
// to killing just the child if the group kill fails.
func killProcess(c *exec.Cmd) {
	if c.Process == nil || c.Process.Pid <= 0 {
		return
	}
	if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil {
		_ = c.Process.Kill()
	}
}

// shellArgv maps a script shell to its interpreter argv: sh -> /bin/sh,
// bash -> bash from PATH. powershell and cmd do not exist here.
func shellArgv(shell, file string) ([]string, error) {
	switch shell {
	case protocol.ShellSh:
		return []string{"/bin/sh", file}, nil
	case protocol.ShellBash:
		p, err := exec.LookPath("bash")
		if err != nil {
			return nil, fmt.Errorf("spawn: shell bash not found on PATH")
		}
		return []string{p, file}, nil
	}
	return nil, fmt.Errorf("spawn: shell %s not available on %s", shell, runtime.GOOS)
}

// lockSession implements kind lock (A1): loginctl lock-sessions, then
// xdg-screensaver lock; the first that exits 0 is reported, else the last
// failure.
func lockSession(ctx context.Context, id int64) protocol.CommandResult {
	attempts := [][]string{
		{"loginctl", "lock-sessions"},
		{"xdg-screensaver", "lock"},
	}
	var last protocol.CommandResult
	for _, argv := range attempts {
		if ctx.Err() != nil {
			break
		}
		last = runArgv(ctx, id, argv)
		if last.ExitCode == 0 && last.Error == "" {
			return last
		}
	}
	if last.Error == "" && last.ExitCode != 0 {
		last.Error = "lock failed: loginctl and xdg-screensaver both exited non-zero"
	}
	return last
}

// showMessage implements kind message (A1): notify-send when it is on PATH
// and succeeds, otherwise the text goes to the result stdout with exit 0 so
// the operator still sees that it was delivered to the record.
func showMessage(ctx context.Context, id int64, text string) protocol.CommandResult {
	if _, err := exec.LookPath("notify-send"); err == nil {
		res := runArgv(ctx, id, []string{"notify-send", "--app-name=MSP Agent", "MSP", text})
		if res.ExitCode == 0 && res.Error == "" {
			if res.Stdout == "" {
				res.Stdout = text
			}
			return res
		}
		now := time.Now().UTC()
		return protocol.CommandResult{
			ExitCode:   0,
			Stdout:     text,
			Stderr:     "notify-send failed: " + firstLine(res.Stderr, res.Error),
			StartedAt:  res.StartedAt,
			FinishedAt: now,
		}
	}
	now := time.Now().UTC()
	return protocol.CommandResult{ExitCode: 0, Stdout: text, StartedAt: now, FinishedAt: now}
}

// firstLine returns the first non-empty candidate's first line.
func firstLine(candidates ...string) string {
	for _, s := range candidates {
		for i := 0; i < len(s); i++ {
			if s[i] == '\n' {
				s = s[:i]
				break
			}
		}
		if s != "" {
			return s
		}
	}
	return "unknown error"
}
