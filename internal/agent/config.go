// Package agent is the desktop agent: configuration, the HTTP client that
// speaks internal/protocol, the sync loop, the command runner, per-platform
// metrics collection and the service (systemd / Windows SCM) glue.
//
// It imports only internal/protocol, the standard library and
// golang.org/x/sys; it never imports internal/server or internal/store.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config is the on-disk agent configuration (agent.json). Secrets live here,
// so Save writes it 0600 inside a 0700 directory.
type Config struct {
	ServerURL    string `json:"server_url"`
	DeviceToken  string `json:"device_token"`
	DeviceID     int64  `json:"device_id"`
	IntervalS    int    `json:"interval_s"`
	CAFile       string `json:"ca_file,omitempty"`
	InsecureHTTP bool   `json:"insecure_http,omitempty"`
}

// DefaultPath is where the service reads its configuration:
// /etc/msp-agent/agent.json on Linux, %ProgramData%\msp-agent\agent.json on
// Windows.
func DefaultPath() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "msp-agent", "agent.json")
	}
	return "/etc/msp-agent/agent.json"
}

// Load reads and decodes a config file.
func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// Validate reports whether the config holds what Run needs.
func (c Config) Validate() error {
	if c.ServerURL == "" {
		return errors.New("config: server_url is empty")
	}
	if c.DeviceToken == "" {
		return errors.New("config: device_token is empty (not enrolled)")
	}
	if c.IntervalS < 1 {
		return errors.New("config: interval_s must be >= 1")
	}
	return nil
}

// Save writes the config atomically: MkdirAll 0700, temp file in the same
// directory, chmod 0600, rename over the target.
func Save(path string, c Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".agent-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	// Rename keeps the temp file's mode; chmod again in case the umask or a
	// pre-existing target changed it.
	return os.Chmod(path, 0o600)
}
