//go:build linux

package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/sxi9/msp/internal/protocol"
)

// platformOpenPTY opens a Linux PTY and starts the shell attached to it as a
// new session leader with the slave as its controlling terminal (spec section
// C): open /dev/ptmx, unlock it (TIOCSPTLCK), read the slave number (TIOCGPTN),
// open the slave, set the initial window size (TIOCSWINSZ via IoctlSetWinsize)
// and start the shell with SysProcAttr{Setsid, Setctty, Ctty}. Only
// x/sys/unix (ioctls, Winsize) and the stdlib (syscall.SysProcAttr) are used.
func platformOpenPTY(shell string, cols, rows uint16) (ptySession, error) {
	argv, err := shellPTYArgv(shell)
	if err != nil {
		return nil, err
	}

	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("spawn: open /dev/ptmx: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = ptmx.Close()
		}
	}()

	// Unlock the slave (TIOCSPTLCK with 0) before opening it.
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		return nil, fmt.Errorf("spawn: unlock pty: %w", err)
	}
	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		return nil, fmt.Errorf("spawn: pty number: %w", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("spawn: open pts: %w", err)
	}
	// The child inherits the slave as fds 0/1/2; the parent keeps only the
	// master and closes its own slave handle after the child starts.
	defer slave.Close()

	// Initial window size on the master; the kernel reflects it to the slave.
	_ = unix.IoctlSetWinsize(int(ptmx.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true, // new session so the slave can become its controlling tty
		Setctty: true, // make Ctty the controlling terminal
		Ctty:    0,    // index of the slave in the child's fd table (stdin)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}

	ok = true
	return &linuxPTY{ptmx: ptmx, cmd: cmd}, nil
}

// linuxPTY is a running shell on a Linux PTY. The master (ptmx) carries the
// terminal in both directions; the shell is a process-group/session leader so
// Close can take the whole tree down.
type linuxPTY struct {
	ptmx *os.File
	cmd  *exec.Cmd

	closeOnce sync.Once
	waitOnce  sync.Once
	waitCode  int
	waitErr   error
}

// Read returns terminal output from the master. When the shell exits the kernel
// returns EIO on the master read; that, and a closed master (Close during
// teardown), are reported as io.EOF so the relay stops.
func (p *linuxPTY) Read(b []byte) (int, error) {
	n, err := p.ptmx.Read(b)
	if err != nil && (errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)) {
		if n > 0 {
			return n, nil
		}
		return 0, io.EOF
	}
	return n, err
}

// Write sends keystrokes to the shell's stdin via the master.
func (p *linuxPTY) Write(b []byte) (int, error) {
	return p.ptmx.Write(b)
}

// Resize applies a window-size change (TIOCSWINSZ) to the master.
func (p *linuxPTY) Resize(cols, rows uint16) error {
	return unix.IoctlSetWinsize(int(p.ptmx.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}

// Wait reaps the shell once and caches its exit code.
func (p *linuxPTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		p.waitErr = err
		switch {
		case err == nil:
			p.waitCode = 0
		default:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				p.waitCode = ee.ExitCode()
			} else {
				p.waitCode = -1
			}
		}
	})
	return p.waitCode, p.waitErr
}

// Close kills the shell's process group and closes the master; it is safe to
// call repeatedly and concurrently with Wait.
func (p *linuxPTY) Close() error {
	p.closeOnce.Do(func() {
		if p.cmd.Process != nil && p.cmd.Process.Pid > 0 {
			// Setsid put the child in its own session/group (pgid == pid).
			if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
				_ = p.cmd.Process.Kill()
			}
		}
		_ = p.ptmx.Close()
	})
	return nil
}

// shellPTYArgv resolves the interactive shell to run (spec section C default:
// bash if on PATH else sh). An explicit powershell/cmd is not available here.
func shellPTYArgv(shell string) ([]string, error) {
	switch shell {
	case "", protocol.ShellBash:
		if p, err := exec.LookPath("bash"); err == nil {
			return []string{p, "-i"}, nil
		}
		// Default (and an explicit bash on a host without it) fall back to sh.
		return []string{"/bin/sh", "-i"}, nil
	case protocol.ShellSh:
		return []string{"/bin/sh", "-i"}, nil
	default:
		return nil, fmt.Errorf("shell %s not supported on %s", shell, runtime.GOOS)
	}
}
