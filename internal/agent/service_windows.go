//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	svcName        = "msp-agent"
	svcDisplayName = "MSP Agent"
	svcDescription = "Reports this device's health to the MSP server and runs commands queued by its operators."
	svcStopTimeout = 30 * time.Second
)

// Install registers (or re-registers) this executable as the msp-agent
// Windows service running `run --config <cfgPath>` as LocalSystem, sets it
// to restart 10 s after a crash, registers the event log source and starts
// it. The binary stays where it is (install.ps1 puts it under
// %ProgramData%\msp-agent).
func Install(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	args := []string{"run", "--config", cfgPath}
	cfg := mgr.Config{
		DisplayName: svcDisplayName,
		Description: svcDescription,
		StartType:   mgr.StartAutomatic,
	}

	s, err := m.OpenService(svcName)
	if err == nil {
		// Re-install: point the existing service at this binary and config,
		// stop it so the restart below picks both up.
		defer s.Close()
		existing, cerr := s.Config()
		if cerr != nil {
			return fmt.Errorf("read service config: %w", cerr)
		}
		existing.DisplayName = cfg.DisplayName
		existing.Description = cfg.Description
		existing.StartType = cfg.StartType
		existing.BinaryPathName = commandLine(exe, args)
		if err := s.UpdateConfig(existing); err != nil {
			return fmt.Errorf("update service config: %w", err)
		}
		if err := stopService(s); err != nil {
			return err
		}
	} else {
		s, err = m.CreateService(svcName, exe, cfg, args...)
		if err != nil {
			return fmt.Errorf("create service: %w", err)
		}
		defer s.Close()
	}

	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 10 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 86400); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	if err := eventlog.InstallAsEventCreate(svcName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!errors.Is(err, windows.ERROR_ALREADY_EXISTS) && !isAlreadyExists(err) {
		return fmt.Errorf("register event log source: %w", err)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

// Uninstall stops the service (waiting up to 30 s), deletes it and removes
// the event log source. The config file and binary are left in place.
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(svcName)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return fmt.Errorf("service %s is not installed", svcName)
		}
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	var errs []error
	if err := stopService(s); err != nil {
		errs = append(errs, err)
	}
	if err := s.Delete(); err != nil {
		errs = append(errs, fmt.Errorf("delete service: %w", err))
	}
	if err := eventlog.Remove(svcName); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		errs = append(errs, fmt.Errorf("remove event log source: %w", err))
	}
	return errors.Join(errs...)
}

// IsService reports whether the process was started by the service control
// manager (then `run` must hand control to RunAsService).
func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// RunAsService hands the process to the SCM: it reports Running, runs the
// loop in a goroutine, and on Stop or Shutdown cancels the loop's context,
// reports StopPending and returns (svc.Run then reports Stopped).
func RunAsService(run func(ctx context.Context) error) error {
	h := &serviceHandler{run: run}
	if err := svc.Run(svcName, h); err != nil {
		return fmt.Errorf("service control: %w", err)
	}
	return h.err
}

// serviceHandler adapts the agent loop to svc.Handler.
type serviceHandler struct {
	run func(ctx context.Context) error
	err error
}

// Execute implements svc.Handler.
func (h *serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	logEvent(func(l *eventlog.Log) error { return l.Info(1, "msp-agent service started") })

	for {
		select {
		case err := <-done:
			// The loop ended on its own; that only happens on a fatal
			// error, which the SCM recovery actions turn into a restart.
			status <- svc.Status{State: svc.StopPending}
			if err != nil {
				h.err = err
				logEvent(func(l *eventlog.Log) error { return l.Error(2, "msp-agent loop exited: "+err.Error()) })
				return false, 1
			}
			logEvent(func(l *eventlog.Log) error { return l.Info(3, "msp-agent loop exited") })
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(svcStopTimeout / time.Millisecond)}
				cancel()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						h.err = err
					}
				case <-time.After(svcStopTimeout):
				}
				logEvent(func(l *eventlog.Log) error { return l.Info(4, "msp-agent service stopped") })
				return false, 0
			}
		}
	}
}

// stopService asks a service to stop and waits up to svcStopTimeout for it.
// A service that is already stopped is not an error.
func stopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("query service: %w", err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if st, err = s.Control(svc.Stop); err != nil {
			if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
				return nil
			}
			return fmt.Errorf("stop service: %w", err)
		}
	}
	deadline := time.Now().Add(svcStopTimeout)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return fmt.Errorf("service %s did not stop within %s", svcName, svcStopTimeout)
		}
		time.Sleep(300 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return fmt.Errorf("query service: %w", err)
		}
	}
	return nil
}

// commandLine builds the BinaryPathName the SCM stores, quoting the way
// mgr.CreateService does so re-installs and fresh installs match.
func commandLine(exe string, args []string) string {
	s := syscall.EscapeArg(exe)
	for _, a := range args {
		s += " " + syscall.EscapeArg(a)
	}
	return s
}

// isAlreadyExists recognises the eventlog package's own "already exists"
// error ("<key path> registry key already exists"), which it returns as a
// plain error rather than an Errno.
func isAlreadyExists(err error) bool {
	return err != nil && strings.HasSuffix(err.Error(), " registry key already exists")
}

// logEvent writes to the Windows event log under the msp-agent source,
// ignoring failures (the source may not be registered in dev mode).
func logEvent(fn func(l *eventlog.Log) error) {
	l, err := eventlog.Open(svcName)
	if err != nil {
		return
	}
	defer l.Close()
	_ = fn(l)
}
