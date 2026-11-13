//go:build !windows

package agent

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
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
// xdg-screensaver lock; the first that exits 0 is reported. A host without
// any interactive session (headless server, container) answers exit 0 with
// stdout "no interactive session" like the Windows path. Otherwise the
// failure reports what every attempt said, not just the last one.
func lockSession(ctx context.Context, id int64) protocol.CommandResult {
	attempts := [][]string{
		{"loginctl", "lock-sessions"},
		{"xdg-screensaver", "lock"},
	}
	var (
		last    protocol.CommandResult
		reasons []string
	)
	for _, argv := range attempts {
		if ctx.Err() != nil {
			break
		}
		last = runArgv(ctx, id, argv)
		if last.ExitCode == 0 && last.Error == "" {
			return last
		}
		if argv[0] == "loginctl" && noSessions(last) {
			last.ExitCode = 0
			last.Error = ""
			last.Stdout = "no interactive session"
			return last
		}
		reasons = append(reasons, argv[0]+": "+attemptReason(last))
	}
	if last.ExitCode != 0 || last.Error != "" {
		last.Error = "lock failed: " + strings.Join(reasons, "; ")
	}
	return last
}

// noSessions recognises loginctl answering that there is nothing to lock
// (no logind sessions, or no logind at all in a container).
func noSessions(r protocol.CommandResult) bool {
	out := strings.ToLower(r.Stdout + "\n" + r.Stderr)
	return strings.Contains(out, "no sessions") || strings.Contains(out, "no such session") ||
		strings.Contains(out, "failed to connect to bus") || strings.Contains(out, "not been booted with systemd")
}

// attemptReason summarises one failed lock attempt: the spawn error, else
// the exit code with the first stderr line.
func attemptReason(r protocol.CommandResult) string {
	if r.Error != "" {
		return r.Error
	}
	if line := firstLine(r.Stderr, r.Stdout); line != "unknown error" {
		return fmt.Sprintf("exit %d: %s", r.ExitCode, line)
	}
	return fmt.Sprintf("exit %d", r.ExitCode)
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
