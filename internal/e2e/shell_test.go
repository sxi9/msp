package e2e

// Remote shell end-to-end proof (RS1, decision D3). This walks the whole
// interactive-terminal path in process: the real server behind httptest, the
// real Linux agent (agent.Run, which spawns a real bash on a real PTY), and a
// real coder/websocket client acting as the browser. It opens a shell over the
// POST, relays `echo RS1OK` through the live WebSocket relay, sends a resize
// control frame, closes the browser leg, then asserts the command completes
// done, the audit carries shell.opened / shell.connected / shell.closed and the
// recorded transcript contains the output.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sxi9/msp/internal/agent"
	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// shellOpenedJSON is the 201 body of POST /api/v1/devices/{id}/shell.
type shellOpenedJSON struct {
	Command commandJSON `json:"command"`
	Ticket  string      `json:"ticket"`
	WSPath  string      `json:"ws_path"`
	errorJSON
}

// cookieHeader serialises the dashboard session cookies the jar holds for base
// into a Cookie header value. The cookie jar does not serve cookies for a ws://
// URL (net/http/cookiejar only answers http/https), so the browser WebSocket
// dial must carry the session cookie explicitly.
func cookieHeader(t *testing.T, d *dash, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base %q: %v", base, err)
	}
	var parts []string
	for _, c := range d.hc.Jar.Cookies(u) {
		parts = append(parts, c.Name+"="+c.Value)
	}
	if len(parts) == 0 {
		t.Fatalf("no session cookie in the jar for %s", base)
	}
	return strings.Join(parts, "; ")
}

// TestRemoteShellEndToEnd is the RS1 in-process proof: server + real Linux agent
// + a real browser WebSocket client through the real relay.
func TestRemoteShellEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the remote-shell proof runs the real Linux agent and a bash PTY")
	}
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logs := &logBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server/agent logs:\n%s", logs.String())
		}
	})

	// --- store + server ------------------------------------------------------
	st, err := store.Open(filepath.Join(t.TempDir(), "msp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	srv := server.New(st, server.Config{
		SampleEvery: time.Second,
		LostGrace:   time.Second,
		Version:     agentVersion,
	}, logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// --- bootstrap admin, org, enrollment token -----------------------------
	hash, err := server.HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	admin, err := st.CreateUser(ctx, adminEmail, hash, "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	org, err := st.CreateOrg(ctx, "Acme Dental")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	token, _, err := st.CreateEnrollmentToken(ctx, org.ID, "shell-e2e", 5, 24*time.Hour, &admin.ID)
	if err != nil {
		t.Fatalf("create enrollment token: %v", err)
	}

	// --- enroll and run the real agent (loopback => http allowed) ------------
	cfg, err := agent.Enroll(ctx, ts.URL, token, "", false, 1, agentVersion)
	if err != nil {
		t.Fatalf("agent enroll: %v", err)
	}
	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(agentCtx, cfg, agentVersion, logger) }()
	defer func() {
		stopAgent()
		select {
		case err := <-agentDone:
			if err != nil {
				t.Errorf("agent.Run returned %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("agent.Run did not return within 30 s of cancel")
		}
	}()

	// --- dashboard login -----------------------------------------------------
	d := newDash(t, ts.URL)
	if code := d.do(http.MethodPost, "/api/v1/login", map[string]string{"email": adminEmail, "password": adminPassword}, nil); code != 200 {
		t.Fatalf("login: status %d", code)
	}

	// --- device comes online -------------------------------------------------
	var dev deviceJSON
	waitFor(t, "linux device online", 10*time.Second, func() bool {
		for _, x := range d.devices() {
			if x.Platform == protocol.PlatformLinux && x.Status == "online" {
				dev = x
				return true
			}
		}
		return false
	})

	// --- open a shell: POST issues the command, the one-time ticket and ws_path
	var opened shellOpenedJSON
	code := d.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/shell", dev.ID), map[string]any{"shell": "bash", "cols": 100, "rows": 40}, &opened)
	if code != 201 {
		t.Fatalf("open shell: status %d (%s %s)", code, opened.Error, opened.Detail)
	}
	if len(opened.Ticket) != 43 || opened.WSPath == "" || opened.Command.ID == 0 {
		t.Fatalf("open shell response: ticket=%q ws_path=%q command=%+v", opened.Ticket, opened.WSPath, opened.Command)
	}
	if opened.Command.Kind != protocol.KindShell {
		t.Fatalf("opened command kind = %q, want shell", opened.Command.Kind)
	}
	cmdID := opened.Command.ID

	// --- browser leg: a real WebSocket client with the session cookie and an
	// Origin matching Host, plus the single-use ticket (RS1 section B).
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	browserHeaders := http.Header{
		"Origin": {ts.URL},
		"Cookie": {cookieHeader(t, d, ts.URL)},
	}
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	browser, _, err := websocket.Dial(dctx, wsBase+opened.WSPath+"?ticket="+url.QueryEscape(opened.Ticket), &websocket.DialOptions{HTTPHeader: browserHeaders})
	dcancel()
	if err != nil {
		t.Fatalf("browser dial: %v", err)
	}
	browser.SetReadLimit(1 << 20)

	// readFrame reads one WebSocket frame with a bounded deadline.
	readFrame := func(timeout time.Duration) (websocket.MessageType, []byte) {
		rctx, rcancel := context.WithTimeout(context.Background(), timeout)
		defer rcancel()
		typ, data, err := browser.Read(rctx)
		if err != nil {
			t.Fatalf("browser read: %v", err)
		}
		return typ, data
	}

	// --- wait until the real agent attaches and the server reports connected.
	// Any PTY output (binary) that arrives alongside the status frame is kept
	// so the RS1OK scan below sees everything the relay delivered.
	var out []byte
	connected := false
	deadline := time.Now().Add(15 * time.Second)
	for !connected && time.Now().Before(deadline) {
		typ, data := readFrame(15 * time.Second)
		switch typ {
		case websocket.MessageText:
			if f, err := protocol.ParseControlFrame(data); err == nil && f.Type == protocol.CtrlStatus {
				switch f.State {
				case protocol.ShellStateConnected:
					connected = true
				case protocol.ShellStateClosed:
					t.Fatalf("shell closed before connecting: reason %q", f.Reason)
				}
			}
		case websocket.MessageBinary:
			out = append(out, data...)
		}
	}
	if !connected {
		t.Fatal("agent never reported connected")
	}

	// --- run a command and assert the bytes come back over the relay ---------
	if err := browser.Write(ctx, websocket.MessageBinary, []byte("echo RS1OK\n")); err != nil {
		t.Fatalf("browser write keystrokes: %v", err)
	}
	scan := time.Now().Add(15 * time.Second)
	for !bytes.Contains(out, []byte("RS1OK")) {
		if time.Now().After(scan) {
			t.Fatalf("did not see RS1OK in relayed output; got %q", out)
		}
		typ, data := readFrame(15 * time.Second)
		if typ == websocket.MessageBinary {
			out = append(out, data...)
		}
	}

	// --- send a resize control frame (browser->agent) ------------------------
	if err := browser.Write(ctx, websocket.MessageText, protocol.ResizeFrame(132, 50).Encode()); err != nil {
		t.Fatalf("browser write resize: %v", err)
	}

	// --- close the browser leg: a live session closed by the technician
	// completes the command done (RS1 section B).
	browser.Close(websocket.StatusNormalClosure, "")

	// --- command completes done ---------------------------------------------
	waitFor(t, "shell command done", 10*time.Second, func() bool {
		return d.command(cmdID).Status == store.StatusDone
	})

	// --- audit carries the three shell events --------------------------------
	entries := d.audit()
	for _, action := range []string{"shell.opened", "shell.connected", "shell.closed"} {
		if !hasAuditAction(entries, action) {
			t.Errorf("missing audit %s (have %d entries)", action, len(entries))
		}
	}

	// --- transcript contains the output. The live session is removed before
	// the row is flushed, so poll until the recorded bytes land.
	transcriptPath := fmt.Sprintf("/api/v1/shell/%d/transcript", cmdID)
	waitFor(t, "transcript to contain RS1OK", 10*time.Second, func() bool {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+transcriptPath, nil)
		resp, err := d.hc.Do(req)
		if err != nil {
			t.Fatalf("GET transcript: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return false
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("transcript Content-Type = %q, want text/plain", ct)
		}
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(resp.Body); err != nil {
			t.Fatalf("read transcript: %v", err)
		}
		return bytes.Contains(buf.Bytes(), []byte("RS1OK"))
	})

	// --- the shell command's history JSON carries its shell_session block -----
	var detail struct {
		Command struct {
			commandJSON
			ShellSession *struct {
				BytesIn     int64  `json:"bytes_in"`
				BytesOut    int64  `json:"bytes_out"`
				CloseReason string `json:"close_reason"`
			} `json:"shell_session"`
		} `json:"command"`
	}
	if code := d.do(http.MethodGet, "/api/v1/commands/"+strconv.FormatInt(cmdID, 10), nil, &detail); code != 200 {
		t.Fatalf("GET command: status %d", code)
	}
	if detail.Command.ShellSession == nil {
		t.Errorf("finished shell command carries no shell_session block")
	} else if detail.Command.ShellSession.CloseReason != protocol.CloseTechnicianClosed {
		t.Errorf("shell_session close_reason = %q, want %q", detail.Command.ShellSession.CloseReason, protocol.CloseTechnicianClosed)
	}
}

// hasAuditAction reports whether any audit entry carries the given action.
func hasAuditAction(entries []auditJSON, action string) bool {
	for _, e := range entries {
		if e.Action == action {
			return true
		}
	}
	return false
}
