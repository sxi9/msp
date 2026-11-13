package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Remote shell relay tests (RS1). Because store.CreateCommand does not yet
// accept kind shell (WP-STORE gap; see drift notes), two kinds of test coexist
// here: the relay itself is exercised directly over a real command row (an exec
// command, which satisfies the shell_sessions foreign key) through two in-process
// WebSocket clients, and the full POST->WS->close path is covered by
// TestShellEndToEnd, which skips until the store accepts shell commands.

// --- WebSocket test helpers -------------------------------------------------

func wsURL(base, path string) string { return "ws" + strings.TrimPrefix(base, "http") + path }

// dialWS dials a WebSocket, failing the test on error. headers may be nil.
func dialWS(t *testing.T, url string, headers http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return conn
}

// readBinary returns the next binary frame, skipping text (status) frames.
func readBinary(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		typ, data, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read binary: %v", err)
		}
		if typ == websocket.MessageBinary {
			return data
		}
	}
}

// readControl returns the next text control frame, decoded.
func readControl(t *testing.T, conn *websocket.Conn) protocol.ControlFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read control: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("want text control frame, got type %v", typ)
	}
	f, err := protocol.ParseControlFrame(data)
	if err != nil {
		t.Fatalf("parse control %s: %v", data, err)
	}
	return f
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: condition not met within %s", what, timeout)
}

// sentShellStandin queues an exec command and claims it so it sits in status
// 'sent', ready to stand in for a shell command while the store cannot create
// one. It returns the command row.
func sentShellStandin(e *testEnv, devID int64, token string) store.Command {
	e.t.Helper()
	c := e.queue(devID, "sleep", "1")
	if st, _, body := e.sync(token, syncRequest(0)); st != 200 {
		e.t.Fatalf("claim command: %d %s", st, body)
	}
	got := e.command(c.ID)
	if got.Status != store.StatusSent {
		e.t.Fatalf("stand-in command status = %s, want sent", got.Status)
	}
	return got
}

// relayLegs is an httptest server exposing the two relay legs directly, bound to
// a command that already exists in the store. It drives the real attach + relay
// loops without the kind-shell command creation the store cannot yet do.
func relayLegs(e *testEnv, cmd store.Command, ticket *shellTicket) *httptest.Server {
	e.t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/{id}", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ss := e.srv.shells().getOrCreate(e.srv, cmd, "testhost", e.srv.now())
		if !ss.attachAgent(conn) {
			conn.Close(websocket.StatusPolicyViolation, "already attached")
			return
		}
		ss.runAgent(conn)
	})
	mux.HandleFunc("GET /browser/{id}", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ss := e.srv.shells().getOrCreate(e.srv, cmd, "testhost", e.srv.now())
		ss.attachBrowser(conn, ticket)
		ss.runBrowser(conn)
	})
	ts := httptest.NewServer(mux)
	e.t.Cleanup(ts.Close)
	return ts
}

// --- ticket / cap unit tests ------------------------------------------------

func TestShellTicketLifecycle(t *testing.T) {
	m := newShellManager()
	now := time.Now()
	val := m.reserve(1, 42, "1.2.3.4", 120, 32, now)
	if len(val) != 43 {
		t.Fatalf("ticket length = %d, want 43", len(val))
	}

	// Wrong value does not consume the ticket.
	if _, ok := m.consumeTicket(1, "nope", now); ok {
		t.Fatal("wrong ticket value accepted")
	}
	// Right value consumes it once, carrying the technician identity.
	tk, ok := m.consumeTicket(1, val, now)
	if !ok || tk.userID != 42 || tk.ip != "1.2.3.4" {
		t.Fatalf("consume: ok=%v ticket=%+v", ok, tk)
	}
	// Single use: a second consume fails.
	if _, ok := m.consumeTicket(1, val, now); ok {
		t.Fatal("ticket consumed twice")
	}

	// Expiry: a ticket older than the TTL is rejected.
	val2 := m.reserve(2, 1, "ip", 0, 0, now)
	if _, ok := m.consumeTicket(2, val2, now.Add(m.ticketTTL)); ok {
		t.Fatal("expired ticket accepted")
	}
	if _, ok := m.consumeTicket(2, val2, now.Add(m.ticketTTL/2)); ok {
		t.Fatal("ticket should already be gone after the expiry attempt pruned it")
	}
}

func TestShellCapCounting(t *testing.T) {
	m := newShellManager()
	m.maxConcurrent = 3
	now := time.Now()
	if m.full(now) {
		t.Fatal("empty manager reported full")
	}
	m.reserve(1, 1, "", 0, 0, now)
	m.reserve(2, 1, "", 0, 0, now)
	if m.full(now) {
		t.Fatal("full at 2/3")
	}
	m.reserve(3, 1, "", 0, 0, now)
	if !m.full(now) {
		t.Fatal("not full at 3/3")
	}
	// Expired tickets free the slots.
	if m.full(now.Add(m.ticketTTL)) {
		t.Fatal("expired tickets still counted as full")
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://app.example.com", "app.example.com", true},
		{"https://App.Example.com", "app.example.com", true},
		{"http://evil.example.com", "app.example.com", false},
		{"", "app.example.com", false},
		{"http://app.example.com:8443", "app.example.com:8443", true},
		{"http://app.example.com", "app.example.com:8443", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://"+c.host+"/api/v1/shell/1/ws", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := sameOrigin(r); got != c.want {
			t.Errorf("sameOrigin(origin=%q host=%q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

func TestCommandErrorText(t *testing.T) {
	if got := commandErrorText(protocol.CloseAgentNeverConnected); got != "agent never connected" {
		t.Errorf("agent_never_connected text = %q", got)
	}
	if got := commandErrorText(protocol.CloseIdleTimeout); got != "idle_timeout" {
		t.Errorf("idle text = %q", got)
	}
}

// --- POST /devices/{id}/shell validation (reached without the store gap) -----

func TestShellOpenValidation(t *testing.T) {
	e := newTestEnv(t)
	linuxID, linuxTok := e.enroll("uid-linux", protocol.PlatformLinux)
	androidID, _ := e.enroll("uid-android", protocol.PlatformAndroid)
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)
	operator := dashUser(e, "operator@example.com", RoleOperator)

	openPath := func(id int64) string { return "/api/v1/devices/" + strconv.FormatInt(id, 10) + "/shell" }
	body := map[string]any{"shell": "bash", "cols": 100, "rows": 40}

	// Viewer is forbidden; CSRF is mandatory.
	if resp, b := e.do(http.MethodPost, openPath(linuxID), body, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 || errorCode(t, b) != "forbidden" {
		t.Fatalf("viewer open: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do(http.MethodPost, openPath(linuxID), body, withCookie(operator, nil)); resp.StatusCode != 403 || errorCode(t, b) != "csrf" {
		t.Fatalf("missing csrf: %d %s", resp.StatusCode, b)
	}

	// Offline (never synced -> pending) device.
	if resp, b := e.do(http.MethodPost, openPath(linuxID), body, withCookie(operator, csrfHeader)); resp.StatusCode != 409 || errorCode(t, b) != "device_offline" {
		t.Fatalf("offline open: %d %s", resp.StatusCode, b)
	}

	// Android is unsupported regardless of online state.
	if resp, b := e.do(http.MethodPost, openPath(androidID), body, withCookie(operator, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "unsupported_on_platform" {
		t.Fatalf("android open: %d %s", resp.StatusCode, b)
	}

	// Unknown device -> 404.
	if resp, _ := e.do(http.MethodPost, openPath(99999), body, withCookie(operator, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("unknown device open: %d", resp.StatusCode)
	}

	// Bring the device online.
	if st, _, b := e.sync(linuxTok, syncRequest(0)); st != 200 {
		t.Fatalf("sync: %d %s", st, b)
	}

	// Too many shells: fill the slot budget with reserved tickets.
	for i := 0; i < shellMaxConcurrent; i++ {
		e.srv.shells().reserve(int64(100000+i), e.admin.ID, "ip", 120, 32, e.srv.now())
	}
	if resp, b := e.do(http.MethodPost, openPath(linuxID), body, withCookie(operator, csrfHeader)); resp.StatusCode != 429 || errorCode(t, b) != "too_many_shells" {
		t.Fatalf("too many shells: %d %s", resp.StatusCode, b)
	}

	// Revoked device -> unsupported_on_device.
	if resp, _ := e.do(http.MethodPost, "/api/v1/devices/"+strconv.FormatInt(linuxID, 10)+"/revoke", nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 && resp.StatusCode != 200 {
		t.Fatalf("revoke failed")
	}
	if resp, b := e.do(http.MethodPost, openPath(linuxID), body, withCookie(operator, csrfHeader)); resp.StatusCode != 400 || errorCode(t, b) != "unsupported_on_device" {
		t.Fatalf("revoked open: %d %s", resp.StatusCode, b)
	}
}

// TestShellOpenCrossOrg confirms staff are not org-scoped (docs/SECURITY.md
// section 4, as for remote desktop): opening a shell on a device in another org
// reaches the online check (409 device_offline), not a 403/404 org gate.
func TestShellOpenCrossOrg(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	org2, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	dev, _, err := e.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: org2.ID, DeviceUID: "beta-1", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	operator := dashUser(e, "operator@example.com", RoleOperator)
	path := "/api/v1/devices/" + strconv.FormatInt(dev.ID, 10) + "/shell"
	if resp, b := e.do(http.MethodPost, path, map[string]any{}, withCookie(operator, csrfHeader)); resp.StatusCode != 409 || errorCode(t, b) != "device_offline" {
		t.Fatalf("cross-org open: %d %s", resp.StatusCode, b)
	}
}

// TestShellBusy covers the shell_busy guard; it skips until the store can create
// a shell command (WP-STORE gap).
func TestShellBusy(t *testing.T) {
	e := newTestEnv(t)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	operator := dashUser(e, "operator@example.com", RoleOperator)
	if st, _, b := e.sync(token, syncRequest(0)); st != 200 {
		t.Fatalf("sync: %d %s", st, b)
	}
	if _, err := e.st.CreateCommand(context.Background(), store.CreateCommandParams{
		DeviceID: devID, OrgID: e.org.ID, CreatedBy: e.admin.ID,
		Kind: protocol.KindShell, Shell: "bash", TimeoutS: protocol.DefaultShellTimeoutS,
	}); err != nil {
		t.Skipf("store.CreateCommand does not accept kind shell yet (WP-STORE gap): %v", err)
	}
	path := "/api/v1/devices/" + strconv.FormatInt(devID, 10) + "/shell"
	if resp, b := e.do(http.MethodPost, path, map[string]any{}, withCookie(operator, csrfHeader)); resp.StatusCode != 409 || errorCode(t, b) != "shell_busy" {
		t.Fatalf("busy open: %d %s", resp.StatusCode, b)
	}
}

func TestShellBrowserLegOriginAndTranscript404(t *testing.T) {
	e := newTestEnv(t)
	devID, _ := e.enroll("uid-1", protocol.PlatformLinux)
	operator := dashUser(e, "operator@example.com", RoleOperator)

	// Browser WS leg: missing Origin is rejected before anything else.
	if resp, b := e.do(http.MethodGet, "/api/v1/shell/1/ws?ticket=abc", nil, withCookie(operator, nil)); resp.StatusCode != 403 || errorCode(t, b) != "forbidden" {
		t.Fatalf("missing origin: %d %s", resp.StatusCode, b)
	}

	// Transcript of a non-shell command -> 404.
	c := e.queue(devID, "uname", "-a")
	tp := "/api/v1/shell/" + strconv.FormatInt(c.ID, 10) + "/transcript"
	if resp, b := e.do(http.MethodGet, tp, nil, withCookie(operator, nil)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
		t.Fatalf("transcript of exec command: %d %s", resp.StatusCode, b)
	}
	// Transcript of an unknown command -> 404.
	if resp, _ := e.do(http.MethodGet, "/api/v1/shell/88888/transcript", nil, withCookie(operator, nil)); resp.StatusCode != 404 {
		t.Fatalf("transcript of unknown command: %d", resp.StatusCode)
	}
}

// TestWebSocketUpgradeThroughMiddleware proves a WebSocket can be hijacked
// through the exact production middleware chain (recover -> request log ->
// security headers -> body limit), i.e. that the statusWriter wrapper's Unwrap
// reaches the hijacker. The real shell WS handlers run behind this chain.
func TestWebSocketUpgradeThroughMiddleware(t *testing.T) {
	e := newTestEnv(t)
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("up"))
		cancel()
		conn.Close(websocket.StatusNormalClosure, "")
	})
	var h http.Handler = base
	h = e.srv.bodyLimit(h)
	h = e.srv.securityHeaders(h)
	h = e.srv.requestLog(h)
	h = e.srv.recoverPanic(h)
	ts := httptest.NewServer(h)
	defer ts.Close()

	conn := dialWS(t, wsURL(ts.URL, "/ws"), nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	if got := readBinary(t, conn); string(got) != "up" {
		t.Fatalf("upgrade through middleware: got %q", got)
	}
}

// --- the relay itself, over two in-process WebSocket clients -----------------

func TestShellRelayPipesBytesAndCloses(t *testing.T) {
	e := newTestEnv(t)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	cmd := sentShellStandin(e, devID, token)
	ticket := &shellTicket{value: "t", issuedAt: e.srv.now(), userID: e.admin.ID, ip: "9.9.9.9", cols: 120, rows: 32}
	ts := relayLegs(e, cmd, ticket)
	idStr := strconv.FormatInt(cmd.ID, 10)

	// Agent connects first, then the browser (so the browser's first status is
	// "connected"). The fake agent echoes keystrokes and records resizes.
	agent := dialWS(t, wsURL(ts.URL, "/agent/"+idStr), nil)
	resizes := make(chan protocol.ControlFrame, 4)
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			typ, data, err := agent.Read(ctx)
			cancel()
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				wc, wcancel := context.WithTimeout(context.Background(), time.Second)
				_ = agent.Write(wc, websocket.MessageBinary, data)
				wcancel()
			case websocket.MessageText:
				if f, err := protocol.ParseControlFrame(data); err == nil {
					resizes <- f
				}
			}
		}
	}()

	browser := dialWS(t, wsURL(ts.URL, "/browser/"+idStr), nil)
	if f := readControl(t, browser); f.Type != protocol.CtrlStatus || f.State != protocol.ShellStateConnected {
		t.Fatalf("first browser frame = %+v, want status connected", f)
	}

	// Browser -> agent -> browser round trip.
	payload := []byte("echo transcript 123\n")
	ctxW, cancelW := context.WithTimeout(context.Background(), time.Second)
	if err := browser.Write(ctxW, websocket.MessageBinary, payload); err != nil {
		t.Fatalf("browser write: %v", err)
	}
	cancelW()
	if got := readBinary(t, browser); string(got) != string(payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}

	// Browser -> agent resize control frame.
	ctxR, cancelR := context.WithTimeout(context.Background(), time.Second)
	_ = browser.Write(ctxR, websocket.MessageText, protocol.ResizeFrame(90, 24).Encode())
	cancelR()
	select {
	case f := <-resizes:
		if f.Type != protocol.CtrlResize || f.Cols != 90 || f.Rows != 24 {
			t.Fatalf("agent resize = %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent never received the resize frame")
	}

	// Technician closes the live session -> technician_closed, command done.
	browser.Close(websocket.StatusNormalClosure, "")

	waitFor(t, 3*time.Second, "command done", func() bool {
		return e.command(cmd.ID).Status == store.StatusDone
	})
	sess, err := e.st.GetShellSession(context.Background(), cmd.ID)
	if err != nil {
		t.Fatalf("GetShellSession: %v", err)
	}
	if sess.CloseReason != protocol.CloseTechnicianClosed {
		t.Fatalf("close reason = %q, want technician_closed", sess.CloseReason)
	}
	if sess.ConnectedAt == nil {
		t.Fatal("connected_at not recorded")
	}
	if string(sess.Transcript) != string(payload) {
		t.Fatalf("transcript = %q, want %q", sess.Transcript, payload)
	}
	if sess.BytesIn != int64(len(payload)) || sess.BytesOut != int64(len(payload)) {
		t.Fatalf("bytes in/out = %d/%d, want %d/%d", sess.BytesIn, sess.BytesOut, len(payload), len(payload))
	}
	if sess.TranscriptTruncated {
		t.Fatal("transcript unexpectedly truncated")
	}
	// A technician-closed live session is done, but no exit frame arrived, so
	// exit_code is -1 (RS1 section B: "exit_code from the exit message or -1");
	// store.CompleteShellCommand sets the status directly so done can carry -1.
	if got := e.command(cmd.ID); got.ExitCode == nil || *got.ExitCode != -1 || got.Error != "" {
		t.Fatalf("done command exit=%v error=%q", got.ExitCode, got.Error)
	}

	// Audit has shell.connected (actor device) and shell.closed (actor user,
	// the technician).
	if got := e.auditActions("shell.connected"); len(got) != 1 || got[0].ActorType != "device" {
		t.Fatalf("shell.connected audit = %+v", got)
	}
	closed := e.auditActions("shell.closed")
	if len(closed) != 1 || closed[0].ActorType != "user" || closed[0].Detail["reason"] != "technician_closed" {
		t.Fatalf("shell.closed audit = %+v", closed)
	}
}

func TestShellRelayAgentExitIsDone(t *testing.T) {
	e := newTestEnv(t)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	cmd := sentShellStandin(e, devID, token)
	ticket := &shellTicket{value: "t", issuedAt: e.srv.now(), userID: e.admin.ID}
	ts := relayLegs(e, cmd, ticket)
	idStr := strconv.FormatInt(cmd.ID, 10)

	browser := dialWS(t, wsURL(ts.URL, "/browser/"+idStr), nil)
	// browser connects first -> waiting_for_agent.
	if f := readControl(t, browser); f.State != protocol.ShellStateWaitingForAgent {
		t.Fatalf("first frame = %+v, want waiting_for_agent", f)
	}
	agent := dialWS(t, wsURL(ts.URL, "/agent/"+idStr), nil)
	// browser should now see "connected".
	if f := readControl(t, browser); f.State != protocol.ShellStateConnected {
		t.Fatalf("second frame = %+v, want connected", f)
	}

	// Agent sends output, then an exit 0 frame, then closes -> command done.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = agent.Write(ctx, websocket.MessageBinary, []byte("done\n"))
	_ = agent.Write(ctx, websocket.MessageText, protocol.ExitFrame(0).Encode())
	cancel()
	agent.Close(websocket.StatusNormalClosure, "")

	waitFor(t, 3*time.Second, "command done", func() bool {
		return e.command(cmd.ID).Status == store.StatusDone
	})
	sess, _ := e.st.GetShellSession(context.Background(), cmd.ID)
	if sess.CloseReason != protocol.CloseAgentClosed {
		t.Fatalf("close reason = %q, want agent_closed", sess.CloseReason)
	}
	if got := e.command(cmd.ID); got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("exit code = %v, want 0", got.ExitCode)
	}
	_ = browser
}

func TestShellRelayAgentNeverConnected(t *testing.T) {
	e := newTestEnv(t)
	e.srv.shells().agentConnectTTL = 120 * time.Millisecond
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	cmd := sentShellStandin(e, devID, token)
	ticket := &shellTicket{value: "t", issuedAt: e.srv.now(), userID: e.admin.ID}
	ts := relayLegs(e, cmd, ticket)
	idStr := strconv.FormatInt(cmd.ID, 10)

	browser := dialWS(t, wsURL(ts.URL, "/browser/"+idStr), nil)
	if f := readControl(t, browser); f.State != protocol.ShellStateWaitingForAgent {
		t.Fatalf("first frame = %+v, want waiting_for_agent", f)
	}
	// No agent ever attaches; within the TTL the session closes.
	if f := readControl(t, browser); f.State != protocol.ShellStateClosed || f.Reason != protocol.CloseAgentNeverConnected {
		t.Fatalf("close frame = %+v, want closed/agent_never_connected", f)
	}
	waitFor(t, 3*time.Second, "command failed", func() bool {
		return e.command(cmd.ID).Status == store.StatusFailed
	})
	if got := e.command(cmd.ID); got.Error != "agent never connected" {
		t.Fatalf("error = %q, want 'agent never connected'", got.Error)
	}
	sess, _ := e.st.GetShellSession(context.Background(), cmd.ID)
	if sess.CloseReason != protocol.CloseAgentNeverConnected || sess.ConnectedAt != nil {
		t.Fatalf("session = %+v", sess)
	}
}

func TestShellRelayIdleTimeout(t *testing.T) {
	e := newTestEnv(t)
	e.srv.shells().idleTimeout = 120 * time.Millisecond
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	cmd := sentShellStandin(e, devID, token)
	ticket := &shellTicket{value: "t", issuedAt: e.srv.now(), userID: e.admin.ID}
	ts := relayLegs(e, cmd, ticket)
	idStr := strconv.FormatInt(cmd.ID, 10)

	dialWS(t, wsURL(ts.URL, "/agent/"+idStr), nil)
	browser := dialWS(t, wsURL(ts.URL, "/browser/"+idStr), nil)
	// Consume the connected status, then stay idle.
	_ = readControl(t, browser)
	if f := readControl(t, browser); f.State != protocol.ShellStateClosed || f.Reason != protocol.CloseIdleTimeout {
		t.Fatalf("close frame = %+v, want closed/idle_timeout", f)
	}
	waitFor(t, 3*time.Second, "session closed", func() bool {
		s, err := e.st.GetShellSession(context.Background(), cmd.ID)
		return err == nil && s.CloseReason == protocol.CloseIdleTimeout
	})
}

func TestShellRelayMaxDuration(t *testing.T) {
	e := newTestEnv(t)
	e.srv.shells().maxDurationOverride = 120 * time.Millisecond
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	cmd := sentShellStandin(e, devID, token)
	ticket := &shellTicket{value: "t", issuedAt: e.srv.now(), userID: e.admin.ID}
	ts := relayLegs(e, cmd, ticket)
	idStr := strconv.FormatInt(cmd.ID, 10)

	dialWS(t, wsURL(ts.URL, "/agent/"+idStr), nil)
	browser := dialWS(t, wsURL(ts.URL, "/browser/"+idStr), nil)
	_ = readControl(t, browser)
	if f := readControl(t, browser); f.State != protocol.ShellStateClosed || f.Reason != protocol.CloseMaxDuration {
		t.Fatalf("close frame = %+v, want closed/max_duration", f)
	}
}

func TestShellRelayRejectsSecondAgent(t *testing.T) {
	e := newTestEnv(t)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	cmd := sentShellStandin(e, devID, token)
	ticket := &shellTicket{value: "t", issuedAt: e.srv.now(), userID: e.admin.ID}
	ts := relayLegs(e, cmd, ticket)
	idStr := strconv.FormatInt(cmd.ID, 10)

	first := dialWS(t, wsURL(ts.URL, "/agent/"+idStr), nil)
	defer first.Close(websocket.StatusNormalClosure, "")
	waitFor(t, 2*time.Second, "agent attached", func() bool {
		ss := e.srv.shells().session(cmd.ID)
		return ss != nil && ss.hasAgent()
	})

	// A second agent is refused: attachAgent returns false and the socket is
	// closed, while the session keeps the first agent.
	second := dialWS(t, wsURL(ts.URL, "/agent/"+idStr), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := second.Read(ctx); err == nil {
		t.Fatal("second agent connection was not closed")
	}
	if ss := e.srv.shells().session(cmd.ID); ss == nil || !ss.hasAgent() {
		t.Fatal("first agent lost after the second was rejected")
	}
}

// TestShellEndToEnd drives the whole path through the real HTTP + WebSocket
// handlers. It skips until store.CreateCommand accepts kind shell (WP-STORE
// gap): the POST then returns 500.
func TestShellEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	devID, token := e.enroll("uid-1", protocol.PlatformLinux)
	operator := dashUser(e, "operator@example.com", RoleOperator)
	if st, _, b := e.sync(token, syncRequest(0)); st != 200 {
		t.Fatalf("sync: %d %s", st, b)
	}

	openPath := "/api/v1/devices/" + strconv.FormatInt(devID, 10) + "/shell"
	resp, body := e.do(http.MethodPost, openPath, map[string]any{"shell": "bash", "cols": 100, "rows": 40}, withCookie(operator, csrfHeader))
	if resp.StatusCode == http.StatusInternalServerError {
		t.Skipf("POST shell returned 500: store.CreateCommand does not yet accept kind shell (WP-STORE gap): %s", body)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open shell: %d %s", resp.StatusCode, body)
	}
	var opened struct {
		Command store.Command `json:"command"`
		Ticket  string        `json:"ticket"`
		WSPath  string        `json:"ws_path"`
	}
	if err := decodeJSON(body, &opened); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	if len(opened.Ticket) != 43 || opened.WSPath == "" {
		t.Fatalf("open response %s", body)
	}
	cmdID := opened.Command.ID

	// The agent claims the shell command (queued -> sent), then attaches.
	if st, _, b := e.sync(token, syncRequest(0)); st != 200 {
		t.Fatalf("claim sync: %d %s", st, b)
	}
	agent := dialWS(t, wsURL(e.ts.URL, "/api/agent/v1/shell/"+strconv.FormatInt(cmdID, 10)), http.Header{"Authorization": {"Bearer " + token}})
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			typ, data, err := agent.Read(ctx)
			cancel()
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				wc, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = agent.Write(wc, websocket.MessageBinary, data)
				cancel()
			}
		}
	}()

	browserHeaders := http.Header{"Origin": {e.ts.URL}, "Cookie": {operator.Name + "=" + operator.Value}}
	browser := dialWS(t, wsURL(e.ts.URL, opened.WSPath+"?ticket="+opened.Ticket), browserHeaders)
	if f := readControl(t, browser); f.State != protocol.ShellStateConnected {
		t.Fatalf("first frame = %+v", f)
	}

	// A second agent for the same command is refused with 409 before upgrade.
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, sresp, serr := websocket.Dial(sctx, wsURL(e.ts.URL, "/api/agent/v1/shell/"+strconv.FormatInt(cmdID, 10)),
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	scancel()
	if serr == nil || sresp == nil || sresp.StatusCode != http.StatusConflict {
		t.Fatalf("second agent: err=%v resp=%v", serr, sresp)
	}

	payload := []byte("hello-e2e\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = browser.Write(ctx, websocket.MessageBinary, payload)
	cancel()
	if got := readBinary(t, browser); string(got) != string(payload) {
		t.Fatalf("echo = %q", got)
	}
	browser.Close(websocket.StatusNormalClosure, "")

	waitFor(t, 3*time.Second, "command done", func() bool {
		return e.command(cmdID).Status == store.StatusDone
	})
	for _, action := range []string{"shell.opened", "shell.connected", "shell.closed"} {
		if got := e.auditActions(action); len(got) == 0 {
			t.Fatalf("missing audit %s", action)
		}
	}
	// Transcript endpoint returns the recorded output.
	tp := "/api/v1/shell/" + strconv.FormatInt(cmdID, 10) + "/transcript"
	resp, tbody := e.do(http.MethodGet, tp, nil, withCookie(operator, nil))
	if resp.StatusCode != 200 || !strings.Contains(string(tbody), "hello-e2e") {
		t.Fatalf("transcript: %d %s", resp.StatusCode, tbody)
	}
}
