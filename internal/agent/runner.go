package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Execute runs one command and returns its result. It never returns an error:
// every failure mode is encoded in the CommandResult exactly as the protocol
// describes (exit_code -1 with "timeout after Ns", "spawn: <reason>" or
// "unsupported kind: X").
//
// Dispatch by kind:
//   - exec:    exec.CommandContext(argv[0], argv[1:]...) with no shell
//   - script:  body written 0600 into a 0700 temp dir, run by the fixed
//     interpreter argv from shellArgv
//   - ping:    exit 0, stdout "pong"
//   - message: showMessage (per platform)
//   - lock:    lockSession (per platform)
func Execute(ctx context.Context, cmd protocol.Command) protocol.CommandResult {
	timeout := protocol.ClampTimeout(cmd.TimeoutS)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	switch cmd.Kind {
	case protocol.KindPing:
		now := time.Now().UTC()
		return protocol.CommandResult{ExitCode: 0, Stdout: "pong", StartedAt: now, FinishedAt: now}
	case protocol.KindMessage:
		return showMessage(ctx, cmd.ID, cmd.Text)
	case protocol.KindLock:
		return lockSession(ctx, cmd.ID)
	case protocol.KindExec:
		if len(cmd.Argv) < 1 {
			return spawnFailure(errors.New("exec requires argv"))
		}
		dir, err := makeCommandDir(cmd.ID)
		if err != nil {
			return spawnFailure(err)
		}
		defer os.RemoveAll(dir)
		return runProcess(ctx, cmd.ID, cmd.Argv, dir, timeout)
	case protocol.KindScript:
		dir, err := makeCommandDir(cmd.ID)
		if err != nil {
			return spawnFailure(err)
		}
		defer os.RemoveAll(dir)
		file := filepath.Join(dir, "script."+scriptExt(cmd.Shell))
		if err := os.WriteFile(file, []byte(cmd.Body), 0o600); err != nil {
			return spawnFailure(err)
		}
		argv, err := shellArgv(cmd.Shell, file)
		if err != nil {
			return spawnFailure(err)
		}
		return runProcess(ctx, cmd.ID, argv, dir, timeout)
	default:
		now := time.Now().UTC()
		return protocol.CommandResult{
			ExitCode:   -1,
			StartedAt:  now,
			FinishedAt: now,
			Error:      "unsupported kind: " + cmd.Kind,
		}
	}
}

// scriptExt is the file extension for a script shell: sh for sh/bash, ps1 for
// powershell, cmd for cmd. Unknown shells get "txt" and fail in shellArgv.
func scriptExt(shell string) string {
	switch shell {
	case protocol.ShellSh, protocol.ShellBash:
		return "sh"
	case protocol.ShellPowershell:
		return "ps1"
	case protocol.ShellCmd:
		return "cmd"
	}
	return "txt"
}

// makeCommandDir creates the per-command 0700 working directory
// <tmp>/msp-cmd-<id>-<random>.
func makeCommandDir(id int64) (string, error) {
	dir, err := os.MkdirTemp("", fmt.Sprintf("msp-cmd-%d-", id))
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// spawnFailure is the result for a command that never started.
func spawnFailure(err error) protocol.CommandResult {
	now := time.Now().UTC()
	msg := err.Error()
	if len(msg) < 7 || msg[:7] != "spawn: " {
		msg = "spawn: " + msg
	}
	return protocol.CommandResult{ExitCode: -1, StartedAt: now, FinishedAt: now, Error: msg}
}

// runProcess starts argv in dir with the agent's environment plus
// MSP_COMMAND_ID, captures up to MaxOutputBytes of each stream, and enforces
// the context deadline by killing the process (group) via killProcess.
func runProcess(ctx context.Context, id int64, argv []string, dir string, timeoutS int) protocol.CommandResult {
	stdout := &limitedWriter{max: protocol.MaxOutputBytes}
	stderr := &limitedWriter{max: protocol.MaxOutputBytes}

	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = dir
	c.Env = append(os.Environ(), "MSP_COMMAND_ID="+strconv.FormatInt(id, 10))
	c.Stdout = stdout
	c.Stderr = stderr
	c.WaitDelay = 2 * time.Second
	c.Cancel = func() error {
		killProcess(c)
		return nil
	}
	setSysProcAttr(c)

	res := protocol.CommandResult{StartedAt: time.Now().UTC()}
	if err := c.Start(); err != nil {
		res.FinishedAt = time.Now().UTC()
		res.ExitCode = -1
		res.Error = "spawn: " + err.Error()
		return res
	}
	waitErr := c.Wait()
	res.FinishedAt = time.Now().UTC()
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	res.Truncated = stdout.truncated || stderr.truncated

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.ExitCode = -1
		res.Error = fmt.Sprintf("timeout after %ds", timeoutS)
	case ctx.Err() != nil:
		res.ExitCode = -1
		res.Error = "cancelled: agent shutting down"
	case waitErr == nil:
		res.ExitCode = 0
	default:
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			res.ExitCode = ee.ExitCode()
			if res.ExitCode == -1 {
				// Killed by a signal outside our control.
				res.Error = ee.String()
			}
		} else {
			res.ExitCode = -1
			res.Error = waitErr.Error()
		}
	}
	return res
}

// limitedWriter keeps the first max bytes and records that more arrived.
type limitedWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	room := w.max - w.buf.Len()
	if room <= 0 {
		if len(p) > 0 {
			w.truncated = true
		}
		return len(p), nil
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *limitedWriter) String() string { return w.buf.String() }

// runArgv is a convenience for the built-in kinds: run argv in a fresh
// command dir and return the result.
func runArgv(ctx context.Context, id int64, argv []string) protocol.CommandResult {
	dir, err := makeCommandDir(id)
	if err != nil {
		return spawnFailure(err)
	}
	defer os.RemoveAll(dir)
	deadline, ok := ctx.Deadline()
	timeoutS := protocol.DefaultTimeoutS
	if ok {
		if s := int(time.Until(deadline).Seconds() + 0.5); s > 0 {
			timeoutS = s
		}
	}
	return runProcess(ctx, id, argv, dir, timeoutS)
}

// platformName is the protocol platform string for this build.
func platformName() string { return runtime.GOOS }
