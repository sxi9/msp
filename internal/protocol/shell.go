package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// KindShell is the interactive remote-shell command kind (RS1, decision D3).
// It is desktop only: KindsFor omits it for android, so the server answers
// 400 unsupported_on_platform for a shell targeting an android device.
const KindShell = "shell"

// Remote-shell terminal sizing and session-duration limits (RS1 section A).
// cols/rows default to 120x32 and are clamped into the Min..Max ranges;
// timeout_s is the maximum session duration (seconds), default 14400 (4 h),
// max 28800 (8 h).
const (
	DefaultShellCols     = 120
	DefaultShellRows     = 32
	MinShellCols         = 20
	MaxShellCols         = 500
	MinShellRows         = 5
	MaxShellRows         = 200
	DefaultShellTimeoutS = 14400
	MaxShellTimeoutS     = 28800
)

// MaxTranscriptBytes caps the recorded agent->browser transcript (RS1 sections
// A and B). Once the buffer reaches this size transcript_truncated is set and
// recording stops, while the relay keeps copying bytes between the two legs.
const MaxTranscriptBytes = 1 << 20 // 1 MiB

// Shell close reasons (RS1 section B). The empty string means the session row
// exists but has not been closed yet.
const (
	CloseTechnicianClosed    = "technician_closed"
	CloseAgentClosed         = "agent_closed"
	CloseIdleTimeout         = "idle_timeout"
	CloseMaxDuration         = "max_duration"
	CloseServerShutdown      = "server_shutdown"
	CloseAgentNeverConnected = "agent_never_connected"
)

// ValidCloseReason reports whether r is one of the defined close reasons, or
// "" for a session that is still open.
func ValidCloseReason(r string) bool {
	switch r {
	case "", CloseTechnicianClosed, CloseAgentClosed, CloseIdleTimeout,
		CloseMaxDuration, CloseServerShutdown, CloseAgentNeverConnected:
		return true
	}
	return false
}

// ShellSession is the per-session accounting the dashboard API attaches to a
// kind-shell COMMAND (RS1 section A). ConnectedAt is null until the agent
// attaches; the whole object is nil on Command (hence null in the JSON) until
// the shell_sessions row exists.
type ShellSession struct {
	BytesIn             int64      `json:"bytes_in"`
	BytesOut            int64      `json:"bytes_out"`
	CloseReason         string     `json:"close_reason"`
	TranscriptTruncated bool       `json:"transcript_truncated"`
	ConnectedAt         *time.Time `json:"connected_at"`
}

// Control-frame type tags: the text (JSON) frames on the shell WebSocket (RS1
// section B). Binary frames carry raw terminal bytes and have no tag.
const (
	CtrlResize = "resize"
	CtrlExit   = "exit"
	CtrlStatus = "status"
)

// Shell status-frame states (server->browser only, RS1 section B).
const (
	ShellStateWaitingForAgent = "waiting_for_agent"
	ShellStateConnected       = "connected"
	ShellStateClosed          = "closed"
)

// ControlFrame is a decoded text control frame. Only the fields that belong to
// its Type carry a value: Cols/Rows for resize, Code for exit, State/Reason for
// status. The constructors and Encode produce exactly the wire JSON of RS1
// section B, and ParseControlFrame reads any of the three back.
type ControlFrame struct {
	Type   string `json:"type"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Code   *int   `json:"code,omitempty"`
	State  string `json:"state,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ResizeFrame builds the browser->agent {"type":"resize","cols":N,"rows":N}.
func ResizeFrame(cols, rows int) ControlFrame {
	return ControlFrame{Type: CtrlResize, Cols: cols, Rows: rows}
}

// ExitFrame builds the agent->browser {"type":"exit","code":N} that the agent
// sends just before it closes. The code may be 0, so it is always emitted.
func ExitFrame(code int) ControlFrame {
	return ControlFrame{Type: CtrlExit, Code: &code}
}

// StatusFrame builds the server->browser {"type":"status","state":...}. reason
// carries the close reason for the "closed" state and is omitted otherwise.
func StatusFrame(state, reason string) ControlFrame {
	return ControlFrame{Type: CtrlStatus, State: state, Reason: reason}
}

// Encode marshals the frame to its wire JSON.
func (f ControlFrame) Encode() []byte {
	b, _ := json.Marshal(f)
	return b
}

// ParseControlFrame decodes a text control frame and rejects an empty or
// unknown type.
func ParseControlFrame(data []byte) (ControlFrame, error) {
	var f ControlFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return ControlFrame{}, fmt.Errorf("shell control frame: %w", err)
	}
	switch f.Type {
	case CtrlResize, CtrlExit, CtrlStatus:
		return f, nil
	default:
		return ControlFrame{}, fmt.Errorf("shell control frame: unknown type %q", f.Type)
	}
}

// ClampShellCols maps 0 to DefaultShellCols and clamps everything else into
// MinShellCols..MaxShellCols.
func ClampShellCols(n int) int {
	if n == 0 {
		return DefaultShellCols
	}
	if n < MinShellCols {
		return MinShellCols
	}
	if n > MaxShellCols {
		return MaxShellCols
	}
	return n
}

// ClampShellRows maps 0 to DefaultShellRows and clamps everything else into
// MinShellRows..MaxShellRows.
func ClampShellRows(n int) int {
	if n == 0 {
		return DefaultShellRows
	}
	if n < MinShellRows {
		return MinShellRows
	}
	if n > MaxShellRows {
		return MaxShellRows
	}
	return n
}

// ClampShellTimeout maps 0 to DefaultShellTimeoutS and clamps everything else
// into 1..MaxShellTimeoutS.
func ClampShellTimeout(n int) int {
	if n == 0 {
		return DefaultShellTimeoutS
	}
	if n < 1 {
		return 1
	}
	if n > MaxShellTimeoutS {
		return MaxShellTimeoutS
	}
	return n
}
