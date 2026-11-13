// Package protocol holds every wire structure, constant and validation rule
// shared by the MSP server, the desktop agent, the tests and the JSON fixtures
// that the Android agent is tested against. It imports nothing outside the
// standard library so that the agent binary stays small and auditable.
//
// docs/PROTOCOL.md is the normative description; this file is its Go form.
package protocol

import (
	"fmt"
	"time"
)

// Platforms a device can report at enrollment.
const (
	PlatformLinux   = "linux"
	PlatformWindows = "windows"
	PlatformAndroid = "android"
)

// Command kinds (AMENDMENT A1). exec and script run only on desktop agents;
// lock, ping and message are built-in and allowed on every platform.
const (
	KindExec    = "exec"
	KindScript  = "script"
	KindLock    = "lock"
	KindPing    = "ping"
	KindMessage = "message"
)

// Shells accepted for kind script.
const (
	ShellSh         = "sh"
	ShellBash       = "bash"
	ShellPowershell = "powershell"
	ShellCmd        = "cmd"
)

// Token prefixes. Plaintext tokens are prefix + 43 chars base64url (32 random bytes).
const (
	EnrollmentTokenPrefix = "et_"
	DeviceTokenPrefix     = "dt_"
)

// Timing and size limits.
const (
	DefaultIntervalDesktop = 30
	DefaultIntervalAndroid = 60
	MinIntervalS           = 1
	MaxIntervalS           = 3600
	MaxWaitS               = 25
	MaxOutputBytes         = 65536
	DefaultTimeoutS        = 60
	MaxTimeoutS            = 3600
	MaxCommandsPerSync     = 10
	MaxScriptBytes         = 262144
	MaxArgBytes            = 4096
	MaxMessageBytes        = 4096
)

// Disk is one mounted filesystem in a metrics sample.
type Disk struct {
	Mount   string  `json:"mount"`
	TotalGB float64 `json:"total_gb"`
	UsedGB  float64 `json:"used_gb"`
}

// Metrics is the per-sync sample. CPUPct is null on Android and on the
// agent's first sample; the battery fields are absent when there is no battery.
type Metrics struct {
	CPUPct          *float64 `json:"cpu_pct"`
	MemTotalMB      int64    `json:"mem_total_mb"`
	MemUsedMB       int64    `json:"mem_used_mb"`
	Disks           []Disk   `json:"disks"`
	BatteryPct      *int     `json:"battery_pct,omitempty"`
	BatteryCharging *bool    `json:"battery_charging,omitempty"`
	IPs             []string `json:"ips"`
}

// Facts is the slow-changing inventory sent at enrollment, on the first sync
// after start and every 120th sync.
type Facts struct {
	Kernel       string   `json:"kernel"`
	CPUModel     string   `json:"cpu_model"`
	CPUCount     int      `json:"cpu_count"`
	MemTotalMB   int64    `json:"mem_total_mb"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	Serial       string   `json:"serial"`
	IPs          []string `json:"ips"`
}

// EnrollRequest is the body of POST /api/agent/v1/enroll.
type EnrollRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	DeviceUID       string `json:"device_uid"`
	Hostname        string `json:"hostname"`
	Platform        string `json:"platform"`
	Arch            string `json:"arch"`
	OSVersion       string `json:"os_version"`
	AgentVersion    string `json:"agent_version"`
	IntervalS       int    `json:"interval_s"`
	Facts           Facts  `json:"facts"`
}

// EnrollResponse is the 201 body of POST /api/agent/v1/enroll.
type EnrollResponse struct {
	DeviceID      int64  `json:"device_id"`
	DeviceToken   string `json:"device_token"`
	PollIntervalS int    `json:"poll_interval_s"`
}

// SyncRequest is the body of POST /api/agent/v1/sync: heartbeat, metrics and
// the long-poll wait. Facts is present only when the agent re-sends inventory.
type SyncRequest struct {
	AgentVersion string  `json:"agent_version"`
	IntervalS    int     `json:"interval_s"`
	WaitS        int     `json:"wait_s"`
	UptimeS      int64   `json:"uptime_s"`
	Metrics      Metrics `json:"metrics"`
	Facts        *Facts  `json:"facts,omitempty"`
}

// Command is one unit of work delivered to an agent inside a SyncResponse.
// Argv is set for exec, Shell+Body for script, Text for message; lock and ping
// carry only id, kind and timeout_s.
type Command struct {
	ID       int64    `json:"id"`
	Kind     string   `json:"kind"`
	Argv     []string `json:"argv,omitempty"`
	Shell    string   `json:"shell,omitempty"`
	Body     string   `json:"body,omitempty"`
	Text     string   `json:"text,omitempty"`
	TimeoutS int      `json:"timeout_s"`
}

// SyncResponse is the 200 body of POST /api/agent/v1/sync. Commands is always
// an array, never null.
type SyncResponse struct {
	PollIntervalS int       `json:"poll_interval_s"`
	Commands      []Command `json:"commands"`
}

// CommandResult is the body of POST /api/agent/v1/commands/{id}/result.
// Error is "" on normal completion, "timeout after Ns" on deadline (ExitCode -1)
// or "spawn: <reason>" when the process could not start (ExitCode -1).
type CommandResult struct {
	ExitCode   int       `json:"exit_code"`
	Stdout     string    `json:"stdout"`
	Stderr     string    `json:"stderr"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Truncated  bool      `json:"truncated"`
	Error      string    `json:"error"`
}

// ErrorResponse is the error body used by every endpoint.
type ErrorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

// PingResponse is the body of GET /api/agent/v1/ping.
type PingResponse struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

// ValidPlatform reports whether p is linux, windows or android.
func ValidPlatform(p string) bool {
	switch p {
	case PlatformLinux, PlatformWindows, PlatformAndroid:
		return true
	}
	return false
}

// ClampInterval maps 0 to the platform default (60 for android, 30 otherwise)
// and clamps everything else into MinIntervalS..MaxIntervalS.
func ClampInterval(n int, platform string) int {
	if n == 0 {
		if platform == PlatformAndroid {
			return DefaultIntervalAndroid
		}
		return DefaultIntervalDesktop
	}
	if n < MinIntervalS {
		return MinIntervalS
	}
	if n > MaxIntervalS {
		return MaxIntervalS
	}
	return n
}

// ClampWait clamps the long-poll wait requested by an agent into 0..MaxWaitS.
func ClampWait(n int) int {
	if n < 0 {
		return 0
	}
	if n > MaxWaitS {
		return MaxWaitS
	}
	return n
}

// ClampTimeout maps 0 to DefaultTimeoutS and clamps everything else into
// 1..MaxTimeoutS. Validate rejects out-of-range values; this is for callers
// that have already validated or want to be lenient (the agent).
func ClampTimeout(n int) int {
	if n == 0 {
		return DefaultTimeoutS
	}
	if n < 1 {
		return 1
	}
	if n > MaxTimeoutS {
		return MaxTimeoutS
	}
	return n
}

// ValidKind reports whether k is one of the five command kinds.
func ValidKind(k string) bool {
	switch k {
	case KindExec, KindScript, KindLock, KindPing, KindMessage:
		return true
	}
	return false
}

// ValidShell reports whether s is one of sh, bash, powershell, cmd.
func ValidShell(s string) bool {
	switch s {
	case ShellSh, ShellBash, ShellPowershell, ShellCmd:
		return true
	}
	return false
}

// ShellsFor returns the script shells available on a platform: sh and bash on
// linux, powershell and cmd on windows, none on android.
func ShellsFor(platform string) []string {
	switch platform {
	case PlatformLinux:
		return []string{ShellSh, ShellBash}
	case PlatformWindows:
		return []string{ShellPowershell, ShellCmd}
	}
	return nil
}

// KindsFor returns the command kinds a platform accepts: all five on desktop
// platforms, only lock, ping and message on android, none for an unknown platform.
func KindsFor(platform string) []string {
	switch platform {
	case PlatformLinux, PlatformWindows:
		return []string{KindExec, KindScript, KindLock, KindPing, KindMessage}
	case PlatformAndroid:
		return []string{KindLock, KindPing, KindMessage}
	}
	return nil
}

// KindSupportedOn reports whether a command kind may be queued for a platform.
// The server answers 400 unsupported_on_platform when this is false.
func KindSupportedOn(kind, platform string) bool {
	for _, k := range KindsFor(platform) {
		if k == kind {
			return true
		}
	}
	return false
}

// Validate checks the shape of a command independently of the target
// platform (use KindSupportedOn for that). The returned error text is what the
// server sends as the 400 invalid_request detail.
func (c Command) Validate() error {
	if c.TimeoutS < 0 || c.TimeoutS > MaxTimeoutS {
		return fmt.Errorf("timeout_s must be 0 (default %d) or 1..%d", DefaultTimeoutS, MaxTimeoutS)
	}
	switch c.Kind {
	case KindExec:
		if len(c.Argv) < 1 {
			return fmt.Errorf("exec requires argv with at least one element")
		}
		for i, a := range c.Argv {
			if len(a) > MaxArgBytes {
				return fmt.Errorf("argv[%d] exceeds %d bytes", i, MaxArgBytes)
			}
		}
		if c.Shell != "" || c.Body != "" {
			return fmt.Errorf("exec must not set shell or body")
		}
		if c.Text != "" {
			return fmt.Errorf("exec must not set text")
		}
	case KindScript:
		if !ValidShell(c.Shell) {
			return fmt.Errorf("script shell must be one of sh, bash, powershell, cmd")
		}
		if c.Body == "" {
			return fmt.Errorf("script body must not be empty")
		}
		if len(c.Body) > MaxScriptBytes {
			return fmt.Errorf("script body exceeds %d bytes", MaxScriptBytes)
		}
		if len(c.Argv) != 0 {
			return fmt.Errorf("script must not set argv")
		}
		if c.Text != "" {
			return fmt.Errorf("script must not set text")
		}
	case KindLock, KindPing:
		if len(c.Argv) != 0 || c.Shell != "" || c.Body != "" || c.Text != "" {
			return fmt.Errorf("%s takes no argv, shell, body or text", c.Kind)
		}
	case KindMessage:
		if c.Text == "" {
			return fmt.Errorf("message text must not be empty")
		}
		if len(c.Text) > MaxMessageBytes {
			return fmt.Errorf("message text exceeds %d bytes", MaxMessageBytes)
		}
		if len(c.Argv) != 0 || c.Shell != "" || c.Body != "" {
			return fmt.Errorf("message must not set argv, shell or body")
		}
	default:
		return fmt.Errorf("unknown kind %q (expected exec, script, lock, ping or message)", c.Kind)
	}
	return nil
}
