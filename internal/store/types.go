package store

import "time"

// JSON tags on these types are exactly the dashboard API field names from
// docs/PROTOCOL.md; the server marshals them directly.

// Org is a client organisation with fleet counts computed at read time.
type Org struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	CreatedAt   time.Time `json:"created_at"`
	DeviceCount int       `json:"device_count"`
	OnlineCount int       `json:"online_count"`
	OpenAlerts  int       `json:"open_alerts"`
}

// User is a staff account. PasswordHash never leaves the server.
type User struct {
	ID           int64      `json:"id"`
	Email        string     `json:"email"`
	Role         string     `json:"role"`
	CreatedAt    time.Time  `json:"created_at"`
	DisabledAt   *time.Time `json:"disabled_at"`
	PasswordHash string     `json:"-"`
}

// Session is a dashboard login session; only its hash is stored.
type Session struct {
	TokenHash string    `json:"-"`
	UserID    int64     `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	IP        string    `json:"ip"`
}

// EnrollmentToken is a per-org enrollment credential. The plaintext is
// returned once at creation; TokenPrefix is its first 10 characters.
type EnrollmentToken struct {
	ID          int64      `json:"id"`
	OrgID       int64      `json:"org_id"`
	TokenPrefix string     `json:"token_prefix"`
	Label       string     `json:"label"`
	MaxUses     int        `json:"max_uses"`
	Uses        int        `json:"uses"`
	ExpiresAt   time.Time  `json:"expires_at"`
	CreatedBy   *int64     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
}

// Device is an enrolled endpoint. Status is computed by StatusOf at read
// time; Revoked means token_hash IS NULL; CanRunCommands gates the free-form
// exec/script console (desktop platform and not revoked); RustdeskID is the
// id the agent last reported in its facts (nil when unknown, RD1).
type Device struct {
	ID              int64      `json:"id"`
	OrgID           int64      `json:"org_id"`
	OrgName         string     `json:"org_name"`
	DeviceUID       string     `json:"device_uid"`
	Hostname        string     `json:"hostname"`
	Platform        string     `json:"platform"`
	Arch            string     `json:"arch"`
	OSVersion       string     `json:"os_version"`
	AgentVersion    string     `json:"agent_version"`
	IntervalS       int        `json:"interval_s"`
	Status          string     `json:"status"`
	EnrolledAt      time.Time  `json:"enrolled_at"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	LastIP          string     `json:"last_ip"`
	Revoked         bool       `json:"revoked"`
	CPUPct          *float64   `json:"cpu_pct"`
	MemUsedPct      *float64   `json:"mem_used_pct"`
	DiskUsedPct     *float64   `json:"disk_used_pct"`
	BatteryPct      *int       `json:"battery_pct"`
	BatteryCharging *bool      `json:"battery_charging"`
	UptimeS         *int64     `json:"uptime_s"`
	OpenAlerts      int        `json:"open_alerts"`
	CanRunCommands  bool       `json:"can_run_commands"`
	RustdeskID      *string    `json:"rustdesk_id"`
	FactsJSON       string     `json:"-"`
	LastMetricsJSON string     `json:"-"`
}

// Heartbeat is one throttled metrics sample (at most one per device per
// sampleEvery).
type Heartbeat struct {
	ID          int64     `json:"-"`
	DeviceID    int64     `json:"-"`
	At          time.Time `json:"at"`
	CPUPct      *float64  `json:"cpu_pct"`
	MemUsedPct  *float64  `json:"mem_used_pct"`
	DiskUsedPct *float64  `json:"disk_used_pct"`
	BatteryPct  *int      `json:"battery_pct"`
	UptimeS     *int64    `json:"uptime_s"`
}

// Command is a queued or finished unit of work for one device. Argv is never
// nil (an empty array for non-exec kinds); Text is set for kind message only.
type Command struct {
	ID             int64      `json:"id"`
	DeviceID       int64      `json:"device_id"`
	OrgID          int64      `json:"org_id"`
	CreatedBy      int64      `json:"created_by"`
	CreatedByEmail string     `json:"created_by_email"`
	Kind           string     `json:"kind"`
	Argv           []string   `json:"argv"`
	Shell          string     `json:"shell"`
	Body           string     `json:"body"`
	Text           string     `json:"text"`
	TimeoutS       int        `json:"timeout_s"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	SentAt         *time.Time `json:"sent_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	ExitCode       *int       `json:"exit_code"`
	Stdout         string     `json:"stdout"`
	Stderr         string     `json:"stderr"`
	Truncated      bool       `json:"truncated"`
	Error          string     `json:"error"`
}

// Alert is one open or resolved condition on a device.
type Alert struct {
	ID             int64      `json:"id"`
	OrgID          int64      `json:"org_id"`
	OrgName        string     `json:"org_name"`
	DeviceID       int64      `json:"device_id"`
	Hostname       string     `json:"hostname"`
	Type           string     `json:"type"`
	Severity       string     `json:"severity"`
	Message        string     `json:"message"`
	OpenedAt       time.Time  `json:"opened_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	AcknowledgedBy *int64     `json:"acknowledged_by"`
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
}

// AuditEntry is one append-only audit row. ActorLabel is derived at read
// time: the user's email, "device:<id>" or "system".
type AuditEntry struct {
	ID         int64          `json:"id"`
	At         time.Time      `json:"at"`
	ActorType  string         `json:"actor_type"`
	ActorID    *int64         `json:"actor_id"`
	ActorLabel string         `json:"actor_label"`
	OrgID      *int64         `json:"org_id"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   *int64         `json:"target_id"`
	IP         string         `json:"ip"`
	Detail     map[string]any `json:"detail"`
}

// DeviceFilter narrows ListDevices. Status is one of online|offline|pending
// or "" for all; Query is a hostname substring.
type DeviceFilter struct {
	OrgID  *int64
	Status string
	Query  string
}

// Summary is the dashboard landing-page counter set.
type Summary struct {
	Orgs       int `json:"orgs"`
	Devices    int `json:"devices"`
	Online     int `json:"online"`
	Offline    int `json:"offline"`
	Pending    int `json:"pending"`
	OpenAlerts int `json:"open_alerts"`
}

// UpsertDeviceParams is the enrollment payload as the server has validated it.
// FactsJSON is the marshalled protocol.Facts ("{}" when empty); RustdeskID is
// facts.rustdesk_id, "" when the agent did not report one (a re-enrollment
// without it keeps the stored id).
type UpsertDeviceParams struct {
	OrgID        int64
	DeviceUID    string
	Hostname     string
	Platform     string
	Arch         string
	OSVersion    string
	AgentVersion string
	IntervalS    int
	FactsJSON    string
	RustdeskID   string
}

// CreateCommandParams is a validated command to queue. Argv for exec, Shell
// and Body for script, Text for message; TimeoutS 0 means the default (60).
type CreateCommandParams struct {
	DeviceID  int64
	OrgID     int64
	CreatedBy int64
	Kind      string
	Argv      []string
	Shell     string
	Body      string
	Text      string
	TimeoutS  int
}
