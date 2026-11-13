//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/sxi9/msp/internal/protocol"
)

// user32 procedures used for kind lock in dev mode (not as a service).
var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procLockWorkStation = user32.NewProc("LockWorkStation")
)

// noSessionID is what WTSGetActiveConsoleSessionId returns when nobody is
// attached to the console.
const noSessionID = 0xFFFFFFFF

// setSysProcAttr puts the child in a new process group. Only the direct
// child is killed on timeout (killProcess); a job object that kills the whole
// tree is phase 2.
func setSysProcAttr(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// killProcess terminates the direct child. Tree kill (job objects) is phase
// 2; a PowerShell script that spawned a long-running process can leave it
// running after a timeout, which docs/SECURITY.md states.
func killProcess(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	_ = c.Process.Kill()
}

// shellArgv maps a script shell to its interpreter argv. sh and bash do not
// exist here.
func shellArgv(shell, file string) ([]string, error) {
	switch shell {
	case protocol.ShellPowershell:
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file}, nil
	case protocol.ShellCmd:
		return []string{"cmd.exe", "/C", file}, nil
	}
	return nil, fmt.Errorf("spawn: shell %s not available on windows", shell)
}

// lockSession implements kind lock (A1). As a service the agent lives in
// session 0 where LockWorkStation is a no-op, so it starts
// `rundll32.exe user32.dll,LockWorkStation` inside the active console
// session with that user's token. In dev mode (foreground `run`) it calls
// LockWorkStation directly.
func lockSession(ctx context.Context, id int64) protocol.CommandResult {
	started := time.Now().UTC()
	finish := func(code int, stdout, errText string) protocol.CommandResult {
		return protocol.CommandResult{
			ExitCode:   code,
			Stdout:     stdout,
			StartedAt:  started,
			FinishedAt: time.Now().UTC(),
			Error:      errText,
		}
	}
	if !IsService() {
		r1, _, e := procLockWorkStation.Call()
		if r1 == 0 {
			return finish(1, "", "lock failed: LockWorkStation: "+callError(e).Error())
		}
		return finish(0, "locked current session", "")
	}

	sid := windows.WTSGetActiveConsoleSessionId()
	if sid == noSessionID {
		return finish(0, "no interactive session", "")
	}
	var token windows.Token
	if err := windows.WTSQueryUserToken(sid, &token); err != nil {
		if errors.Is(err, windows.ERROR_NO_TOKEN) {
			return finish(0, "no interactive session", "")
		}
		return finish(1, "", fmt.Sprintf("lock failed: WTSQueryUserToken(%d): %v", sid, err))
	}
	defer token.Close()

	code, err := runInSession(ctx, token, `rundll32.exe user32.dll,LockWorkStation`)
	if err != nil {
		return finish(1, "", "lock failed: "+err.Error())
	}
	if code != 0 {
		return finish(int(code), "", fmt.Sprintf("lock failed: rundll32 exited %d", code))
	}
	return finish(0, "locked session "+strconv.FormatUint(uint64(sid), 10), "")
}

// runInSession starts commandLine on the interactive desktop of the user
// whose token is given, waits for it until ctx is done (then terminates it)
// and returns its exit code.
func runInSession(ctx context.Context, token windows.Token, commandLine string) (uint32, error) {
	cmd, err := windows.UTF16PtrFromString(commandLine)
	if err != nil {
		return 0, err
	}
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return 0, err
	}
	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, token, false); err != nil {
		return 0, fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(env)

	si := windows.StartupInfo{Desktop: desktop}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_NO_WINDOW | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcessAsUser(token, nil, cmd, nil, nil, false, flags, env, nil, &si, &pi); err != nil {
		return 0, fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)

	waitMS := uint32(windows.INFINITE)
	if deadline, ok := ctx.Deadline(); ok {
		d := time.Until(deadline)
		if d <= 0 {
			d = time.Millisecond
		}
		waitMS = uint32(d / time.Millisecond)
	}
	ev, err := windows.WaitForSingleObject(pi.Process, waitMS)
	if err != nil {
		return 0, fmt.Errorf("WaitForSingleObject: %w", err)
	}
	if ev != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(pi.Process, 1)
		return 0, errors.New("timed out waiting for the lock helper")
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		return 0, fmt.Errorf("GetExitCodeProcess: %w", err)
	}
	return code, nil
}

// showMessage implements kind message (A1), best effort: msg.exe broadcasts
// the text to every logged-on session (it works from session 0, so the
// service can use it). Without msg.exe (Windows Home) or when it fails the
// text still goes to the result stdout with exit 0, like the Linux agent.
func showMessage(ctx context.Context, id int64, text string) protocol.CommandResult {
	now := time.Now().UTC()
	if _, err := exec.LookPath("msg.exe"); err != nil {
		return protocol.CommandResult{
			ExitCode:   0,
			Stdout:     text,
			Stderr:     "msg.exe not available on this edition; message recorded only",
			StartedAt:  now,
			FinishedAt: now,
		}
	}
	res := runArgv(ctx, id, []string{"msg.exe", "*", "/TIME:300", text})
	if res.ExitCode == 0 && res.Error == "" {
		if res.Stdout == "" {
			res.Stdout = text
		}
		return res
	}
	return protocol.CommandResult{
		ExitCode:   0,
		Stdout:     text,
		Stderr:     "msg.exe failed: " + firstLine(res.Stderr, res.Stdout, res.Error),
		StartedAt:  res.StartedAt,
		FinishedAt: time.Now().UTC(),
	}
}

// firstLine returns the first non-empty candidate's first line.
func firstLine(candidates ...string) string {
	for _, s := range candidates {
		for i := 0; i < len(s); i++ {
			if s[i] == '\n' || s[i] == '\r' {
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
