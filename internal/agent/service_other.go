//go:build !linux && !windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
)

// Service management exists only for systemd (service_linux.go) and the
// Windows SCM (service_windows.go). These stubs keep the android/darwin
// cross-builds compiling; `msp-agent run` still works in the foreground.

// Install is unsupported here.
func Install(cfgPath string) error {
	return fmt.Errorf("service install on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

// Uninstall is unsupported here.
func Uninstall() error {
	return fmt.Errorf("service uninstall on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

// IsService is always false: nothing can start the agent as a service here.
func IsService() bool { return false }

// RunAsService is unsupported here; the CLI runs the loop in the foreground
// because IsService is false.
func RunAsService(run func(ctx context.Context) error) error {
	return fmt.Errorf("run as service on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
