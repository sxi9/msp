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
// lock, ping and message are built-in and allowed on every platform. patch
// (PM1) is desktop only, like exec, script and shell (KindShell, shell.go).
const (
	KindExec    = "exec"
	KindScript  = "script"
	KindLock    = "lock"
	KindPing    = "ping"
	KindMessage = "message"
	KindPatch   = "patch"
)

// Patch command fields (PM1). action defaults to install, mode to security and
// reboot to never; a scan re-collects the report and ignores mode/ids/reboot.
const (
	PatchActionScan    = "scan"
	PatchActionInstall = "install"

	PatchModeAll      = "all"
	PatchModeSecurity = "security"
	PatchModeIDs      = "ids"

	PatchRebootNever      = "never"
	PatchRebootIfRequired = "if_required"
)

// Patch report managers (PM1): apt on Linux, Windows Update on Windows, none
// when no supported package manager is found.
const (
	PatchManagerApt  = "apt"
	PatchManagerWU   = "wu"
	PatchManagerNone = "none"
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
	// Patch report (facts.patches, PM1). The updates list is capped at 500
	// entries (security first then name) and each field is length-bounded;
	// Validate rejects anything larger with a 400 invalid_request naming the
	// index. MaxPatchTimeoutS is the ceiling for the patch command's timeout_s
	// (default 3600), higher than MaxTimeoutS because an install can be slow.
	MaxPatchUpdates      = 500
	MaxPatchNameLen      = 256
	MaxPatchVersionLen   = 128
	MaxPatchSourceLen    = 256
	DefaultPatchTimeoutS = 3600
	MaxPatchTimeoutS     = 14400
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

// PatchUpdate is one pending OS update in a PatchReport (PM1): the package
// name, its installed (Current) and candidate (New) versions, whether it is a
// security update and the apt/WU source it comes from. Every field is
// length-bounded (name <= MaxPatchNameLen, versions <= MaxPatchVersionLen,
// source <= MaxPatchSourceLen); the agent cuts its fields to the same limits
// before sending.
type PatchUpdate struct {
	Name     string `json:"name"`
	Current  string `json:"current"`
	New      string `json:"new"`
	Security bool   `json:"security"`
	Source   string `json:"source"`
}

// PatchReport is the pending-updates report an agent collects periodically and
// sends as the optional top-level "patches" field on the sync and enroll
// request, beside facts (PM1). Manager is apt, wu or none (none = no supported
// package manager, the report still carries CheckedAt and an empty list).
// Error non-empty means the scan failed (e.g. apt lock held) and the agent
// keeps re-sending the last good list. Count and SecurityCount are the agent's
// own totals; Updates is capped at MaxPatchUpdates entries, security first then
// name.
type PatchReport struct {
	CheckedAt      time.Time     `json:"checked_at"`
	Manager        string        `json:"manager"`
	RebootRequired bool          `json:"reboot_required"`
	Count          int           `json:"count"`
	SecurityCount  int           `json:"security_count"`
	Error          string        `json:"error"`
	Updates        []PatchUpdate `json:"updates"`
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
	// PatchReport (PM1) is the optional pending-updates report, sent beside
	// facts; omitted by Android, which has no desktop package management.
	PatchReport *PatchReport `json:"patches,omitempty"`
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
	// PatchReport (PM1) is the optional pending-updates report; the agent sends
	// it on the first sync after start, every 6 h and right after an install or
	// scan, independently of Facts (patches scan slower). Android never sends it.
	PatchReport *PatchReport `json:"patches,omitempty"`
}

// Command is one unit of work delivered to an agent inside a SyncResponse.
// Argv is set for exec, Shell+Body for script, Text for message; lock and ping
// carry only id, kind and timeout_s.
type Command struct {
	ID    int64    `json:"id"`
	Kind  string   `json:"kind"`
	Argv  []string `json:"argv,omitempty"`
	Shell string   `json:"shell,omitempty"`
	Body  string   `json:"body,omitempty"`
	Text  string   `json:"text,omitempty"`
	// Action, Mode, IDs and Reboot belong to kind patch (PM1): action is scan
	// or install (default install), mode is all, security or ids (default
	// security), IDs names the packages for mode ids, and reboot is never or
	// if_required (default never). They are omitted for every other kind.
	Action   string   `json:"action,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	IDs      []string `json:"ids,omitempty"`
	Reboot   string   `json:"reboot,omitempty"`
	TimeoutS int      `json:"timeout_s"`
	// Cols, Rows and ShellSession belong to kind shell (RS1, decision D3).
	// Cols/Rows are the initial terminal size; they are omitted (hence 0,
	// meaning "default") for every other kind and whenever a shell command
	// leaves them at the default. ShellSession is the per-session accounting
	// the dashboard API attaches to a shell COMMAND; it is nil until the
	// shell_sessions row exists.
	Cols         int           `json:"cols,omitempty"`
	Rows         int           `json:"rows,omitempty"`
	ShellSession *ShellSession `json:"shell_session,omitempty"`
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

// ValidPatchManager reports whether m is apt, wu or none.
func ValidPatchManager(m string) bool {
	switch m {
	case PatchManagerApt, PatchManagerWU, PatchManagerNone:
		return true
	}
	return false
}

// ValidPatchAction reports whether a is scan or install.
func ValidPatchAction(a string) bool {
	return a == PatchActionScan || a == PatchActionInstall
}

// ValidPatchMode reports whether m is all, security or ids.
func ValidPatchMode(m string) bool {
	switch m {
	case PatchModeAll, PatchModeSecurity, PatchModeIDs:
		return true
	}
	return false
}

// ValidPatchReboot reports whether r is never or if_required.
func ValidPatchReboot(r string) bool {
	return r == PatchRebootNever || r == PatchRebootIfRequired
}

// Validate checks a patch report: the manager must be apt, wu or none, the
// updates list is capped at MaxPatchUpdates entries and each entry's fields are
// length-bounded. The returned text is the 400 invalid_request detail and names
// the offending updates index.
func (r PatchReport) Validate() error {
	if !ValidPatchManager(r.Manager) {
		return fmt.Errorf("patches.manager must be one of apt, wu, none")
	}
	if len(r.Updates) > MaxPatchUpdates {
		return fmt.Errorf("patches.updates has %d entries, at most %d are accepted", len(r.Updates), MaxPatchUpdates)
	}
	for i, u := range r.Updates {
		if err := u.Validate(); err != nil {
			return fmt.Errorf("patches.updates[%d].%w", i, err)
		}
	}
	return nil
}

// Validate checks one update entry; the error names the field without the
// "patches.updates[i]." prefix, which PatchReport.Validate adds.
func (u PatchUpdate) Validate() error {
	switch {
	case u.Name == "" || len(u.Name) > MaxPatchNameLen:
		return fmt.Errorf("name must be 1..%d bytes", MaxPatchNameLen)
	case len(u.Current) > MaxPatchVersionLen:
		return fmt.Errorf("current exceeds %d bytes", MaxPatchVersionLen)
	case len(u.New) > MaxPatchVersionLen:
		return fmt.Errorf("new exceeds %d bytes", MaxPatchVersionLen)
	case len(u.Source) > MaxPatchSourceLen:
		return fmt.Errorf("source exceeds %d bytes", MaxPatchSourceLen)
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

// ClampPatchTimeout maps 0 to DefaultPatchTimeoutS and clamps everything else
// into 1..MaxPatchTimeoutS. A patch install can be slow, so kind patch has its
// own ceiling (MaxPatchTimeoutS) above the one-shot MaxTimeoutS, like kind
// shell's ClampShellTimeout.
func ClampPatchTimeout(n int) int {
	if n == 0 {
		return DefaultPatchTimeoutS
	}
	if n < 1 {
		return 1
	}
	if n > MaxPatchTimeoutS {
		return MaxPatchTimeoutS
	}
	return n
}

// ValidKind reports whether k is one of the command kinds (RS1 added shell).
func ValidKind(k string) bool {
	switch k {
	case KindExec, KindScript, KindLock, KindPing, KindMessage, KindShell, KindPatch:
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

// KindsFor returns the command kinds a platform accepts: exec, script, lock,
// ping, message, shell and patch on desktop platforms, only lock, ping and
// message on android, none for an unknown platform. shell (RS1) and patch (PM1)
// are desktop only, so the server answers 400 unsupported_on_platform for
// either on android.
func KindsFor(platform string) []string {
	switch platform {
	case PlatformLinux, PlatformWindows:
		return []string{KindExec, KindScript, KindLock, KindPing, KindMessage, KindShell, KindPatch}
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
	// kind shell allows a far longer session (MaxShellTimeoutS) and kind patch
	// a longer install (MaxPatchTimeoutS); every other kind is capped at
	// MaxTimeoutS.
	maxTimeout := MaxTimeoutS
	switch c.Kind {
	case KindShell:
		maxTimeout = MaxShellTimeoutS
	case KindPatch:
		maxTimeout = MaxPatchTimeoutS
	}
	if c.TimeoutS < 0 || c.TimeoutS > maxTimeout {
		return fmt.Errorf("timeout_s must be 0 (default) or 1..%d", maxTimeout)
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
	case KindShell:
		// shell is optional; "" means the agent's platform default. cols/rows
		// are not validated here: the server clamps them into range with
		// ClampShellCols/ClampShellRows, it never rejects them.
		if c.Shell != "" && !ValidShell(c.Shell) {
			return fmt.Errorf("shell must be empty (default) or one of sh, bash, powershell, cmd")
		}
		if len(c.Argv) != 0 || c.Body != "" || c.Text != "" {
			return fmt.Errorf("shell must not set argv, body or text")
		}
	case KindPatch:
		// action/mode/reboot are optional; "" means the defaults install,
		// security and never. A scan ignores mode/ids/reboot, so the shape
		// check validates the fields independently and leaves the subset check
		// (ids must be a subset of the pending list) to the agent.
		if c.Action != "" && !ValidPatchAction(c.Action) {
			return fmt.Errorf("patch action must be empty (default install) or one of scan, install")
		}
		if c.Mode != "" && !ValidPatchMode(c.Mode) {
			return fmt.Errorf("patch mode must be empty (default security) or one of all, security, ids")
		}
		if c.Reboot != "" && !ValidPatchReboot(c.Reboot) {
			return fmt.Errorf("patch reboot must be empty (default never) or one of never, if_required")
		}
		if len(c.IDs) > MaxPatchUpdates {
			return fmt.Errorf("patch ids has %d entries, at most %d are accepted", len(c.IDs), MaxPatchUpdates)
		}
		for i, id := range c.IDs {
			if id == "" || len(id) > MaxPatchNameLen {
				return fmt.Errorf("patch ids[%d] must be 1..%d bytes", i, MaxPatchNameLen)
			}
		}
		if len(c.Argv) != 0 || c.Shell != "" || c.Body != "" || c.Text != "" {
			return fmt.Errorf("patch must not set argv, shell, body or text")
		}
	default:
		return fmt.Errorf("unknown kind %q (expected exec, script, lock, ping, message, shell or patch)", c.Kind)
	}
	return nil
}
