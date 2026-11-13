package agent

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

func execCmd(id int64, timeout int, argv ...string) protocol.Command {
	return protocol.Command{ID: id, Kind: protocol.KindExec, Argv: argv, TimeoutS: timeout}
}

func scriptCmd(id int64, shell, body string, timeout int) protocol.Command {
	return protocol.Command{ID: id, Kind: protocol.KindScript, Shell: shell, Body: body, TimeoutS: timeout}
}

func TestExecuteEcho(t *testing.T) {
	res := Execute(context.Background(), execCmd(1, 30, "echo", "hello"))
	if res.ExitCode != 0 || res.Stdout != "hello\n" || res.Stderr != "" || res.Error != "" || res.Truncated {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.StartedAt.IsZero() || res.FinishedAt.Before(res.StartedAt) {
		t.Fatalf("bad timestamps: %+v", res)
	}
	if res.StartedAt.Location() != time.UTC {
		t.Fatalf("StartedAt not UTC: %v", res.StartedAt)
	}
}

func TestExecuteExitCode(t *testing.T) {
	// sh is the program the operator chose; argv passes "-c" and the script literally.
	res := Execute(context.Background(), execCmd(2, 30, "sh", "-c", "echo err >&2; exit 3"))
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3 (%+v)", res.ExitCode, res)
	}
	if res.Stderr != "err\n" || res.Error != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteMetacharactersAreLiteral(t *testing.T) {
	res := Execute(context.Background(), execCmd(3, 30, "echo", "a;id"))
	if res.ExitCode != 0 || res.Stdout != "a;id\n" {
		t.Fatalf("metacharacters were interpreted: %+v", res)
	}
	res = Execute(context.Background(), execCmd(4, 30, "echo", "$HOME", "`id`", "a|b"))
	if res.Stdout != "$HOME `id` a|b\n" {
		t.Fatalf("metacharacters were interpreted: %q", res.Stdout)
	}
}

func TestExecuteTimeoutKillsProcessGroup(t *testing.T) {
	// The script prints the pid of a background child and then waits for it;
	// after the timeout the whole group (sh and sleep) must be gone.
	body := "sleep 5 &\necho $!\nwait\n"
	start := time.Now()
	res := Execute(context.Background(), scriptCmd(5, protocol.ShellSh, body, 1))
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %v, want < 3 s", elapsed)
	}
	if res.ExitCode != -1 || !strings.HasPrefix(res.Error, "timeout after 1s") {
		t.Fatalf("unexpected result: %+v", res)
	}
	pidStr := strings.TrimSpace(res.Stdout)
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("stdout %q is not the child pid", res.Stdout)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := os.ReadFile("/proc/" + pidStr + "/status")
		if err != nil {
			return // gone
		}
		if strings.Contains(string(st), "State:\tZ") {
			return // killed, only a zombie waiting for its reaper
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive after group kill:\n%s", pid, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestExecuteTimeoutExec(t *testing.T) {
	start := time.Now()
	res := Execute(context.Background(), execCmd(6, 1, "sleep", "5"))
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout took too long")
	}
	if res.ExitCode != -1 || res.Error != "timeout after 1s" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteTruncation(t *testing.T) {
	res := Execute(context.Background(), scriptCmd(7, protocol.ShellSh, "yes | head -c 200000\n", 30))
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("unexpected result: exit %d err %q", res.ExitCode, res.Error)
	}
	if !res.Truncated {
		t.Fatalf("expected truncated=true")
	}
	if len(res.Stdout) != protocol.MaxOutputBytes {
		t.Fatalf("stdout length = %d, want %d", len(res.Stdout), protocol.MaxOutputBytes)
	}
}

func TestExecuteMissingBinary(t *testing.T) {
	res := Execute(context.Background(), execCmd(8, 30, "/nonexistent/msp-no-such-binary"))
	if res.ExitCode != -1 || !strings.HasPrefix(res.Error, "spawn: ") {
		t.Fatalf("unexpected result: %+v", res)
	}
	res = Execute(context.Background(), protocol.Command{ID: 9, Kind: protocol.KindExec, TimeoutS: 30})
	if res.ExitCode != -1 || !strings.HasPrefix(res.Error, "spawn: ") {
		t.Fatalf("empty argv: %+v", res)
	}
}

func TestExecuteScriptSh(t *testing.T) {
	body := "echo hello\nprintf %s \"$MSP_COMMAND_ID\" >&2\npwd\nls -ld . | cut -c1-10\n"
	res := Execute(context.Background(), scriptCmd(10, protocol.ShellSh, body, 30))
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.HasPrefix(res.Stdout, "hello\n") {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "10" {
		t.Fatalf("MSP_COMMAND_ID not passed: stderr = %q", res.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	dir := lines[1]
	if !strings.Contains(dir, "msp-cmd-10-") {
		t.Fatalf("cwd %q is not the command dir", dir)
	}
	if lines[2] != "drwx------" {
		t.Fatalf("command dir mode %q, want drwx------", lines[2])
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("command dir %s not removed (err=%v)", dir, err)
	}
}

func TestExecuteScriptBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	res := Execute(context.Background(), scriptCmd(11, protocol.ShellBash, "echo ${BASH_VERSION:+bash}\n", 30))
	if res.ExitCode != 0 || res.Stdout != "bash\n" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteScriptFileMode(t *testing.T) {
	body := "f=$0\nls -l \"$f\" | cut -c1-10\n"
	res := Execute(context.Background(), scriptCmd(12, protocol.ShellSh, body, 30))
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "-rw-------" {
		t.Fatalf("script file mode: %+v", res)
	}
}

func TestExecuteWrongShellForPlatform(t *testing.T) {
	for _, shell := range []string{protocol.ShellPowershell, protocol.ShellCmd} {
		res := Execute(context.Background(), scriptCmd(13, shell, "Write-Host hi\n", 30))
		want := "spawn: shell " + shell + " not available on linux"
		if res.ExitCode != -1 || res.Error != want {
			t.Fatalf("shell %s: got %+v, want error %q", shell, res, want)
		}
	}
}

func TestExecutePing(t *testing.T) {
	res := Execute(context.Background(), protocol.Command{ID: 14, Kind: protocol.KindPing, TimeoutS: 30})
	if res.ExitCode != 0 || res.Stdout != "pong" || res.Stderr != "" || res.Error != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteMessage(t *testing.T) {
	res := Execute(context.Background(), protocol.Command{ID: 15, Kind: protocol.KindMessage, Text: "Please call the helpdesk", TimeoutS: 30})
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Stdout != "Please call the helpdesk" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

func TestExecuteUnsupportedKind(t *testing.T) {
	res := Execute(context.Background(), protocol.Command{ID: 16, Kind: "reboot", TimeoutS: 30})
	if res.ExitCode != -1 || res.Error != "unsupported kind: reboot" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteLockFallsBackAndReportsFailure(t *testing.T) {
	// Point PATH at an empty dir so neither loginctl nor xdg-screensaver exist:
	// lock must report failure (exit -1, spawn error) rather than success.
	t.Setenv("PATH", t.TempDir())
	res := Execute(context.Background(), protocol.Command{ID: 17, Kind: protocol.KindLock, TimeoutS: 30})
	if res.ExitCode == 0 || res.Error == "" {
		t.Fatalf("lock without tools must fail: %+v", res)
	}
	// Both attempts are named so the operator can tell why each one failed.
	if !strings.Contains(res.Error, "loginctl: spawn:") || !strings.Contains(res.Error, "xdg-screensaver: spawn:") {
		t.Fatalf("error must list every attempt: %q", res.Error)
	}
}

func TestExecuteLockReportsEveryAttempt(t *testing.T) {
	bin := t.TempDir()
	fail := "#!/bin/sh\necho 'Failed to lock: Access denied' >&2\nexit 1\n"
	if err := os.WriteFile(bin+"/loginctl", []byte(fail), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	res := Execute(context.Background(), protocol.Command{ID: 20, Kind: protocol.KindLock, TimeoutS: 30})
	if res.ExitCode == 0 || !strings.Contains(res.Error, "loginctl: exit 1: Failed to lock: Access denied") || !strings.Contains(res.Error, "xdg-screensaver: spawn:") {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteLockNoSessionsIsNotAFailure(t *testing.T) {
	// A headless host: loginctl exists but reports nothing to lock. Like the
	// Windows path this is exit 0 with "no interactive session".
	bin := t.TempDir()
	none := "#!/bin/sh\necho 'No sessions.' >&2\nexit 1\n"
	if err := os.WriteFile(bin+"/loginctl", []byte(none), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	res := Execute(context.Background(), protocol.Command{ID: 21, Kind: protocol.KindLock, TimeoutS: 30})
	if res.ExitCode != 0 || res.Error != "" || res.Stdout != "no interactive session" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteLockUsesFirstSuccessfulTool(t *testing.T) {
	bin := t.TempDir()
	fail := "#!/bin/sh\necho nope >&2\nexit 1\n"
	ok := "#!/bin/sh\necho locked-by-xdg\n"
	if err := os.WriteFile(bin+"/loginctl", []byte(fail), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+"/xdg-screensaver", []byte(ok), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	res := Execute(context.Background(), protocol.Command{ID: 18, Kind: protocol.KindLock, TimeoutS: 30})
	if res.ExitCode != 0 || res.Error != "" || res.Stdout != "locked-by-xdg\n" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExecuteDefaultTimeout(t *testing.T) {
	// timeout_s 0 means the default; the command still runs normally.
	res := Execute(context.Background(), execCmd(19, 0, "true"))
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestLimitedWriter(t *testing.T) {
	w := &limitedWriter{max: 5}
	n, _ := w.Write([]byte("abc"))
	if n != 3 || w.truncated {
		t.Fatalf("first write: n=%d truncated=%v", n, w.truncated)
	}
	n, _ = w.Write([]byte("defgh"))
	if n != 5 || !w.truncated || w.String() != "abcde" {
		t.Fatalf("second write: n=%d truncated=%v buf=%q", n, w.truncated, w.String())
	}
	n, _ = w.Write([]byte("x"))
	if n != 1 || w.String() != "abcde" {
		t.Fatalf("third write: n=%d buf=%q", n, w.String())
	}
}
