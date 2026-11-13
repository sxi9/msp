// Package protocol holds every wire structure, constant and validation rule
// shared by the MSP server, the desktop agent, the tests and the JSON fixtures
// that the Android agent is tested against. It imports nothing outside the
// standard library so that the agent binary stays small and auditable.
//
// docs/PROTOCOL.md is the normative description; this file is its Go form.
package protocol

import (
	"fmt"
	"net"
	"strings"
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
	// MaxRustdeskIDLen caps facts.rustdesk_id (RustDesk ids are 9 digits today;
	// the column and the validator leave room for longer numeric ids).
	MaxRustdeskIDLen = 32
	// Installed-apps inventory (facts.apps, Android only in phase 1). The entry
	// cap keeps a sync body with 1000 typical entries (~100 bytes each) well
	// under the 256 KiB agent body limit; the Android agent additionally stops
	// adding entries at 192 KiB of serialised apps JSON so that even
	// maximum-length fields cannot push the body over the limit.
	MaxApps          = 1000
	MaxAppPackageLen = 256
	MaxAppLabelLen   = 256
	MaxAppVersionLen = 64
)

// RustdeskURIPrefix is the URL scheme the official RustDesk client registers
// (get_uri_prefix() in src/common.rs: "<app name lowercased>://"), and
// RustdeskConnectAuthority the authority for a remote-control session
// (src/core_main.rs builds "rustdesk://connect/<id>").
const (
	RustdeskURIPrefix        = "rustdesk://"
	RustdeskConnectAuthority = "connect"
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

// App is one installed application in the facts inventory: the package id,
// the user-visible label (the package id when the app has none), the version
// name ("" when the package declares none) and whether it is a system app.
type App struct {
	Package string `json:"package"`
	Label   string `json:"label"`
	Version string `json:"version"`
	System  bool   `json:"system"`
}

// Facts is the slow-changing inventory sent at enrollment, on the first sync
// after start and every 120th sync. Two keys are optional: RustdeskID, the
// id of the locally installed RustDesk client (digits, <= MaxRustdeskIDLen),
// omitted when RustDesk is not installed (RD1); and Apps, the installed-apps
// inventory (at most MaxApps entries, non-system apps first, then by label),
// sent by the Android agent and omitted by the desktop agents in phase 1.
type Facts struct {
	Kernel       string   `json:"kernel"`
	CPUModel     string   `json:"cpu_model"`
	CPUCount     int      `json:"cpu_count"`
	MemTotalMB   int64    `json:"mem_total_mb"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	Serial       string   `json:"serial"`
	IPs          []string `json:"ips"`
	RustdeskID   string   `json:"rustdesk_id,omitempty"`
	Apps         []App    `json:"apps,omitempty"`
}

// Rustdesk tells an agent which self-hosted RustDesk relay to use (RD1):
// Host is the bare public hostname or IP of the hbbs/hbbr host (no port: the
// ID server is reached on 21116 and the relay on 21117, the defaults of both
// the server and the client) and Key the content of the server's
// id_ed25519.pub (or the -k value given to hbbs/hbbr). It is present in
// EnrollResponse and SyncResponse only while the server has a relay
// configured. Both values are validated with ValidRustdeskHost and
// ValidRustdeskKey on the server before they are sent and on the agent
// before they are written into RustDesk2.toml.
type Rustdesk struct {
	Host string `json:"host"`
	Key  string `json:"key"`
}

// Validate reports why a relay block is unusable ("" and nil when it is
// fine); the text is suitable for a startup error or an agent log line.
func (r Rustdesk) Validate() error {
	if !ValidRustdeskHost(r.Host) {
		return fmt.Errorf("rustdesk host %q must be a bare DNS hostname or IP address without a port (hbbs is reached on 21116, hbbr on 21117)", r.Host)
	}
	if !ValidRustdeskKey(r.Key) {
		return fmt.Errorf("rustdesk key must be one line of 1..%d base64 characters (the content of id_ed25519.pub)", MaxRustdeskKeyLen)
	}
	return nil
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
	DeviceID      int64     `json:"device_id"`
	DeviceToken   string    `json:"device_token"`
	PollIntervalS int       `json:"poll_interval_s"`
	Rustdesk      *Rustdesk `json:"rustdesk,omitempty"`
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
// an array, never null. Rustdesk is omitted when the server has no relay.
type SyncResponse struct {
	PollIntervalS int       `json:"poll_interval_s"`
	Commands      []Command `json:"commands"`
	Rustdesk      *Rustdesk `json:"rustdesk,omitempty"`
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

// MaxRustdeskKeyLen bounds the relay public key (an Ed25519 public key is 44
// base64 characters; a custom -k value may be longer).
const MaxRustdeskKeyLen = 256

// ValidRustdeskHost reports whether host is a bare DNS hostname (RFC 1123
// labels of letters, digits and hyphens, at most 253 characters) or an IPv4
// or IPv6 literal. A port, scheme, path, whitespace or any character that
// would need quoting in RustDesk2.toml is rejected: the value is written
// verbatim into a TOML literal string and onto the hbbs command line.
func ValidRustdeskHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// ValidRustdeskKey reports whether key is one line of base64 (standard or
// URL-safe alphabet, with or without padding; also the characters hbbs
// accepts for a custom -k key): 1..MaxRustdeskKeyLen characters, no
// whitespace, quotes or control characters.
func ValidRustdeskKey(key string) bool {
	if key == "" || len(key) > MaxRustdeskKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ValidRustdeskID reports whether id is a RustDesk client id as the protocol
// accepts it in facts: 1..MaxRustdeskIDLen ASCII digits.
func ValidRustdeskID(id string) bool {
	if id == "" || len(id) > MaxRustdeskIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

// Validate checks the optional facts fields; the returned text is the
// 400 invalid_request detail and names the offending apps index.
func (f Facts) Validate() error {
	if f.RustdeskID != "" && !ValidRustdeskID(f.RustdeskID) {
		return fmt.Errorf("facts.rustdesk_id must be 1..%d digits", MaxRustdeskIDLen)
	}
	if len(f.Apps) > MaxApps {
		return fmt.Errorf("facts.apps has %d entries, at most %d are accepted", len(f.Apps), MaxApps)
	}
	for i, a := range f.Apps {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("facts.apps[%d].%w", i, err)
		}
	}
	return nil
}

// Validate checks one inventory entry; the error names the field without
// the "facts.apps[i]." prefix, which Facts.Validate adds.
func (a App) Validate() error {
	switch {
	case a.Package == "" || len(a.Package) > MaxAppPackageLen:
		return fmt.Errorf("package must be 1..%d bytes", MaxAppPackageLen)
	case len(a.Label) > MaxAppLabelLen:
		return fmt.Errorf("label exceeds %d bytes", MaxAppLabelLen)
	case len(a.Version) > MaxAppVersionLen:
		return fmt.Errorf("version exceeds %d bytes", MaxAppVersionLen)
	}
	return nil
}

// RustdeskConnectURL is the URL that opens a remote-control session to a
// RustDesk id in the installed client: rustdesk://connect/<id>. The id must
// have passed ValidRustdeskID.
func RustdeskConnectURL(id string) string {
	return RustdeskURIPrefix + RustdeskConnectAuthority + "/" + id
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
