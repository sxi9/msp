//go:build windows

package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/sxi9/msp/internal/protocol"
)

// platformOpenPTY starts the shell attached to a Windows pseudoconsole
// (ConPTY, spec section C): a pipe pair feeds the console, CreatePseudoConsole
// makes the HPCON, the shell is launched with the pseudoconsole proc-thread
// attribute, and the parent keeps the input-write and output-read pipe ends to
// drive the relay. Only golang.org/x/sys/windows is used.
//
// This path is cross-compiled in CI but has not been verified on a real Windows
// host (docs/STATUS.md marks Windows ConPTY NOT VERIFIED).
func platformOpenPTY(shell string, cols, rows uint16) (ptySession, error) {
	cmdline, err := shellPTYCmdline(shell)
	if err != nil {
		return nil, err
	}

	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("spawn: input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, fmt.Errorf("spawn: output pipe: %w", err)
	}

	var hpc windows.Handle
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &hpc); err != nil {
		for _, h := range []windows.Handle{inRead, inWrite, outRead, outWrite} {
			windows.CloseHandle(h)
		}
		return nil, fmt.Errorf("spawn: create pseudoconsole: %w", err)
	}
	// The pseudoconsole duplicated the ends it was given; the parent no longer
	// needs them and keeps only inWrite (keystrokes) and outRead (output).
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)

	attr, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("spawn: attribute list: %w", err)
	}
	if err := attr.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(hpc), unsafe.Sizeof(hpc)); err != nil {
		attr.Delete()
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("spawn: set pseudoconsole attribute: %w", err)
	}

	cmdPtr, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		attr.Delete()
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("spawn: command line: %w", err)
	}

	var si windows.StartupInfoEx
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attr.List()

	var pi windows.ProcessInformation
	err = windows.CreateProcess(
		nil,
		cmdPtr,
		nil,
		nil,
		false, // the pseudoconsole is passed via the attribute, not inherited
		windows.EXTENDED_STARTUPINFO_PRESENT,
		nil,
		nil,
		(*windows.StartupInfo)(unsafe.Pointer(&si)),
		&pi,
	)
	if err != nil {
		attr.Delete()
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		return nil, fmt.Errorf("spawn: %w", err)
	}
	windows.CloseHandle(pi.Thread)

	return &windowsPTY{
		hpc:  hpc,
		in:   os.NewFile(uintptr(inWrite), "conpty-in"),
		out:  os.NewFile(uintptr(outRead), "conpty-out"),
		proc: pi.Process,
		attr: attr,
	}, nil
}

// windowsPTY is a running shell on a Windows pseudoconsole.
type windowsPTY struct {
	hpc  windows.Handle
	in   *os.File
	out  *os.File
	proc windows.Handle
	attr *windows.ProcThreadAttributeListContainer

	closeOnce sync.Once
	waitOnce  sync.Once
	waitCode  int
}

// Read returns console output; a closed pipe (shell exit or Close) is io.EOF.
func (p *windowsPTY) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	if err != nil && errors.Is(err, os.ErrClosed) {
		if n > 0 {
			return n, nil
		}
		return 0, io.EOF
	}
	return n, err
}

// Write sends keystrokes to the console input pipe.
func (p *windowsPTY) Write(b []byte) (int, error) {
	return p.in.Write(b)
}

// Resize applies a new window size to the pseudoconsole.
func (p *windowsPTY) Resize(cols, rows uint16) error {
	return windows.ResizePseudoConsole(p.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Wait blocks until the shell exits and caches its exit code.
func (p *windowsPTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		_, err := windows.WaitForSingleObject(p.proc, windows.INFINITE)
		if err != nil {
			p.waitCode = -1
			return
		}
		var code uint32
		if err := windows.GetExitCodeProcess(p.proc, &code); err != nil {
			p.waitCode = -1
			return
		}
		p.waitCode = int(int32(code))
	})
	return p.waitCode, nil
}

// Close tears down the pseudoconsole, the pipes and the shell process; safe to
// call repeatedly and concurrently with Wait.
func (p *windowsPTY) Close() error {
	p.closeOnce.Do(func() {
		// ClosePseudoConsole signals the shell to exit; terminate as a backstop.
		windows.ClosePseudoConsole(p.hpc)
		_ = p.in.Close()
		_ = p.out.Close()
		if p.proc != 0 {
			_ = windows.TerminateProcess(p.proc, 1)
			windows.CloseHandle(p.proc)
		}
		if p.attr != nil {
			p.attr.Delete()
		}
	})
	return nil
}

// shellPTYCmdline resolves the shell command line (spec section C default:
// powershell on Windows). sh/bash are not available here.
func shellPTYCmdline(shell string) (string, error) {
	switch shell {
	case "", protocol.ShellPowershell:
		return "powershell.exe", nil
	case protocol.ShellCmd:
		return "cmd.exe", nil
	default:
		return "", fmt.Errorf("shell %s not supported on windows", shell)
	}
}
