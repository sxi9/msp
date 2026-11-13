package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Remote shell relay (RS1, decision D3). The server keeps every live shell in
// memory keyed by command id and talks to the database only at the three
// moments the shell_sessions row changes (CreateShellSession, SetShellConnected,
// FinishShellSession); the recorded transcript is buffered in RAM and flushed
// once on close. All of the in-memory state for one Server lives in a
// shellManager; shell.go owns the manager, the single-use browser tickets and
// the per-session bookkeeping, shell_relay.go owns the two-leg relay loop, and
// dashboard_shell.go / api_agent_shell.go own the HTTP + WebSocket handlers.

// Relay limits and timers (RS1 section B). These are the production defaults;
// newShellManager copies them into per-Server fields the in-package tests tune.
const (
	shellMaxConcurrent   = 20                       // at most 20 concurrent shells per server
	shellTicketTTL       = 60 * time.Second         // browser ticket is single-use and younger than 60 s
	shellAgentConnectTTL = 60 * time.Second         // agent must attach within 60 s of the POST
	shellIdleTimeout     = 30 * time.Minute         // 30 min without browser input closes the shell
	shellTranscriptCap   = protocol.MaxTranscriptBytes
	shellWriteTimeout    = 15 * time.Second // per-frame write deadline
	shellReadLimit       = 1 << 20          // max single WebSocket message (1 MiB)
)

// shellManager holds the live shells and the outstanding single-use browser
// tickets for one Server, both keyed by command id and guarded by one mutex.
// The timer and cap fields default to the constants above and are overridden by
// the in-package tests so idle/ticket/agent-connect behaviour can be exercised
// without real waiting.
type shellManager struct {
	mu       sync.Mutex
	sessions map[int64]*shellSession
	tickets  map[int64]*shellTicket

	maxConcurrent       int
	ticketTTL           time.Duration
	agentConnectTTL     time.Duration
	idleTimeout         time.Duration
	maxDurationOverride time.Duration // 0 = use the command's timeout_s
}

func newShellManager() *shellManager {
	return &shellManager{
		sessions:        map[int64]*shellSession{},
		tickets:         map[int64]*shellTicket{},
		maxConcurrent:   shellMaxConcurrent,
		ticketTTL:       shellTicketTTL,
		agentConnectTTL: shellAgentConnectTTL,
		idleTimeout:     shellIdleTimeout,
	}
}

// Per-Server shell state is kept in a package-level registry so that it attaches
// to a Server without a field on the Server struct (server.go is owned by
// another work package). Managers are created lazily and live for the Server's
// lifetime.
var (
	shellMgrMu sync.Mutex
	shellMgrs  = map[*Server]*shellManager{}
)

// shells returns this Server's shell manager, creating it on first use.
func (s *Server) shells() *shellManager {
	shellMgrMu.Lock()
	defer shellMgrMu.Unlock()
	m := shellMgrs[s]
	if m == nil {
		m = newShellManager()
		shellMgrs[s] = m
	}
	return m
}

// shellTicket is a reserved, single-use browser credential for one shell
// command. issuedAt is the POST time: it bounds both the 60 s ticket window and
// the 60 s agent-never-connected deadline. userID/ip identify the technician for
// the shell.closed audit on a technician_closed session.
type shellTicket struct {
	value    string
	issuedAt time.Time
	userID   int64
	ip       string
	cols     int
	rows     int
}

// randomTicket returns a 43-character base64url (32 random bytes) ticket, the
// same shape as a device token's secret.
func randomTicket() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read never returns an error on the platforms we build
		// for; panic rather than hand out a predictable ticket.
		panic("shell: rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// full reports whether the server already relays (or has reserved) the maximum
// number of concurrent shells; expired tickets are pruned first.
func (m *shellManager) full(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneTicketsLocked(now)
	return len(m.sessions)+len(m.tickets) >= m.maxConcurrent
}

// reserve records a single-use ticket for a command and returns its value. The
// caller has already checked full(); the reservation keeps the slot until the
// browser consumes the ticket or it expires.
func (m *shellManager) reserve(cmdID, userID int64, ip string, cols, rows int, now time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneTicketsLocked(now)
	val := randomTicket()
	m.tickets[cmdID] = &shellTicket{value: val, issuedAt: now, userID: userID, ip: ip, cols: cols, rows: rows}
	return val
}

// consumeTicket validates and consumes the browser ticket for a command. It
// returns the ticket (for the userID/ip it carries) only on an exact,
// unexpired, constant-time match; a match deletes the ticket (single use), an
// expiry deletes it, a mismatch leaves it so a wrong guess cannot consume it.
func (m *shellManager) consumeTicket(cmdID int64, value string, now time.Time) (*shellTicket, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tickets[cmdID]
	if t == nil {
		return nil, false
	}
	if now.Sub(t.issuedAt) >= m.ticketTTL {
		delete(m.tickets, cmdID)
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(t.value), []byte(value)) != 1 {
		return nil, false
	}
	delete(m.tickets, cmdID)
	return t, true
}

// pruneTicketsLocked drops expired tickets; the caller holds m.mu.
func (m *shellManager) pruneTicketsLocked(now time.Time) {
	for id, t := range m.tickets {
		if now.Sub(t.issuedAt) >= m.ticketTTL {
			delete(m.tickets, id)
		}
	}
}

// session returns the live session for a command, if any.
func (m *shellManager) session(cmdID int64) *shellSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[cmdID]
}

// remove drops a session from the live map (called once, on close).
func (m *shellManager) remove(cmdID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, cmdID)
}

// shellSession is one live shell: the two WebSocket legs, the byte counters, the
// in-RAM transcript and the close bookkeeping. Everything that mutates after
// creation is guarded by mu; the relay loops (shell_relay.go) drive it.
type shellSession struct {
	srv      *Server
	mgr      *shellManager
	cmdID    int64
	deviceID int64
	orgID    int64
	hostname string
	maxDur   time.Duration

	ctx        context.Context
	cancel     context.CancelFunc
	activityCh chan struct{} // signalled on browser input to reset the idle timer
	closeOnce  sync.Once
	postTime   time.Time // POST time (ticket.issuedAt); base for the agent-connect deadline

	mu          sync.Mutex
	agent       *wsConn
	browser     *wsConn
	connectedAt *time.Time // store-clock time the agent attached; nil until then
	userID      *int64     // operator who opened the shell (from the browser ticket)
	ip          string     // technician ip for the shell.closed audit
	bytesIn     int64      // browser -> agent terminal bytes
	bytesOut    int64      // agent -> browser terminal bytes
	transcript  []byte     // recorded agent -> browser bytes (cap shellTranscriptCap)
	truncated   bool
	exitArrived bool
	exitCode    int
	lastInput   time.Time // wall clock of the last browser input (idle tracking)
}

// wsConn serialises writes to one WebSocket; coder/websocket allows a single
// concurrent writer, and the browser leg is written by both the agent-copy loop
// and the status/close path.
type wsConn struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (w *wsConn) write(typ websocket.MessageType, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), shellWriteTimeout)
	defer cancel()
	return w.c.Write(ctx, typ, data)
}

// newShellSession builds a session, writes its shell_sessions row and starts the
// supervisor goroutine. postTime bounds the agent-never-connected deadline.
func newShellSession(s *Server, m *shellManager, cmd store.Command, hostname string, postTime time.Time) *shellSession {
	ctx, cancel := context.WithCancel(context.Background())
	maxDur := m.maxDurationOverride
	if maxDur <= 0 {
		secs := cmd.TimeoutS
		if secs <= 0 {
			secs = protocol.DefaultShellTimeoutS
		}
		maxDur = time.Duration(secs) * time.Second
	}
	ss := &shellSession{
		srv:        s,
		mgr:        m,
		cmdID:      cmd.ID,
		deviceID:   cmd.DeviceID,
		orgID:      cmd.OrgID,
		hostname:   hostname,
		maxDur:     maxDur,
		ctx:        ctx,
		cancel:     cancel,
		activityCh: make(chan struct{}, 1),
		postTime:   postTime,
		lastInput:  time.Now(),
	}
	// Create the shell_sessions row once, when the relay for this command
	// starts. A conflict means a row already exists (should not happen: one
	// session per command); log and carry on so the relay still works.
	if err := s.st.CreateShellSession(ctx, cmd.ID); err != nil {
		s.log.Error("shell: create session row", "command", cmd.ID, "err", err)
	}
	go ss.supervise()
	return ss
}

// commandErrorText maps a close reason to the command's error field. Only the
// agent-never-connected reason has a human phrasing in the contract; every other
// non-done reason stores the close-reason token (RS1 section B).
func commandErrorText(reason string) string {
	if reason == protocol.CloseAgentNeverConnected {
		return "agent never connected"
	}
	return reason
}
