// Package e2e is the in-process end-to-end proof: a real store on a temp
// directory, the real server handler behind httptest, and the real Linux agent
// loop (agent.Run with the real runner) enrolled against it. It walks the same
// sequence as scripts/e2e.sh without separate binaries, so it runs under
// plain go test and the race detector, and it drives the clock (store.Now)
// instead of sleeping for the offline and lost-command transitions.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/agent"
	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

const (
	adminEmail    = "admin@example.com"
	adminPassword = "admin-pass-123"
	agentVersion  = "test"
)

// JSON shapes of the dashboard API, limited to the fields asserted here.
type deviceJSON struct {
	ID             int64  `json:"id"`
	Hostname       string `json:"hostname"`
	Platform       string `json:"platform"`
	Status         string `json:"status"`
	Revoked        bool   `json:"revoked"`
	CanRunCommands bool   `json:"can_run_commands"`
}

type commandJSON struct {
	ID        int64  `json:"id"`
	DeviceID  int64  `json:"device_id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	ExitCode  *int   `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Error     string `json:"error"`
	Truncated bool   `json:"truncated"`
}

type alertJSON struct {
	ID         int64   `json:"id"`
	DeviceID   int64   `json:"device_id"`
	Type       string  `json:"type"`
	Severity   string  `json:"severity"`
	ResolvedAt *string `json:"resolved_at"`
}

type auditJSON struct {
	Action    string `json:"action"`
	ActorType string `json:"actor_type"`
}

type errorJSON struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

// logBuf collects server and agent logs; they are printed only on failure.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// dash is a minimal dashboard client: cookie jar session, CSRF header on
// every non-GET request, JSON in and out.
type dash struct {
	t    *testing.T
	base string
	hc   *http.Client
}

func newDash(t *testing.T, base string) *dash {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &dash{t: t, base: base, hc: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

// do sends method path with body (JSON-marshalled when non-nil), decodes the
// response into out when non-nil and returns the status code.
func (d *dash) do(method, path string, body, out any) int {
	d.t.Helper()
	return doJSON(d.t, d.hc, method, d.base+path, "", body, out, true)
}

// doJSON is the shared HTTP helper for dashboard (csrf=true, session cookie
// from the client's jar) and agent (bearer token) calls.
func doJSON(t *testing.T, hc *http.Client, method, url, bearer string, body, out any, csrf bool) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("%s %s: marshal body: %v", method, url, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf && method != http.MethodGet {
		req.Header.Set("X-Requested-With", "msp")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, url, err)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, url, data, err)
		}
	}
	return resp.StatusCode
}

func (d *dash) devices() []deviceJSON {
	d.t.Helper()
	var resp struct {
		Devices []deviceJSON `json:"devices"`
	}
	if code := d.do(http.MethodGet, "/api/v1/devices", nil, &resp); code != 200 {
		d.t.Fatalf("GET /api/v1/devices: status %d", code)
	}
	return resp.Devices
}

func (d *dash) device(id int64) deviceJSON {
	d.t.Helper()
	var resp struct {
		Device deviceJSON `json:"device"`
	}
	if code := d.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", id), nil, &resp); code != 200 {
		d.t.Fatalf("GET /api/v1/devices/%d: status %d", id, code)
	}
	return resp.Device
}

// queue posts a command for a device and expects 201.
func (d *dash) queue(deviceID int64, cmd protocol.Command) commandJSON {
	d.t.Helper()
	var resp struct {
		Command commandJSON `json:"command"`
		errorJSON
	}
	code := d.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/commands", deviceID), cmd, &resp)
	if code != 201 {
		d.t.Fatalf("queue %s on device %d: status %d (%s %s)", cmd.Kind, deviceID, code, resp.Error, resp.Detail)
	}
	if resp.Command.Status != "queued" {
		d.t.Fatalf("queued command %d has status %q", resp.Command.ID, resp.Command.Status)
	}
	return resp.Command
}

func (d *dash) command(id int64) commandJSON {
	d.t.Helper()
	var resp struct {
		Command commandJSON `json:"command"`
	}
	if code := d.do(http.MethodGet, fmt.Sprintf("/api/v1/commands/%d", id), nil, &resp); code != 200 {
		d.t.Fatalf("GET /api/v1/commands/%d: status %d", id, code)
	}
	return resp.Command
}

func terminal(status string) bool {
	switch status {
	case "done", "failed", "timeout", "lost", "cancelled":
		return true
	}
	return false
}

// waitCommand polls a command until it has status want; any other terminal
// status or the deadline fails the test.
func (d *dash) waitCommand(id int64, want string, timeout time.Duration) commandJSON {
	d.t.Helper()
	var last commandJSON
	waitFor(d.t, fmt.Sprintf("command %d to be %s", id, want), timeout, func() bool {
		last = d.command(id)
		if last.Status == want {
			return true
		}
		if terminal(last.Status) {
			d.t.Fatalf("command %d ended as %s (exit %v, error %q), wanted %s", id, last.Status, last.ExitCode, last.Error, want)
		}
		return false
	})
	return last
}

// run queues a command and waits for its final status.
func (d *dash) run(deviceID int64, cmd protocol.Command, want string, timeout time.Duration) commandJSON {
	d.t.Helper()
	c := d.queue(deviceID, cmd)
	return d.waitCommand(c.ID, want, timeout)
}

func (d *dash) openAlerts() []alertJSON {
	d.t.Helper()
	var resp struct {
		Alerts []alertJSON `json:"alerts"`
	}
	if code := d.do(http.MethodGet, "/api/v1/alerts?state=open", nil, &resp); code != 200 {
		d.t.Fatalf("GET /api/v1/alerts: status %d", code)
	}
	return resp.Alerts
}

func (d *dash) audit() []auditJSON {
	d.t.Helper()
	var resp struct {
		Entries []auditJSON `json:"entries"`
	}
	if code := d.do(http.MethodGet, "/api/v1/audit?limit=200", nil, &resp); code != 200 {
		d.t.Fatalf("GET /api/v1/audit: status %d", code)
	}
	return resp.Entries
}

func countAudit(entries []auditJSON, action, actorType string) int {
	n := 0
	for _, e := range entries {
		if e.Action == action && e.ActorType == actorType {
			n++
		}
	}
	return n
}

// waitFor polls cond every 200 ms until it is true or timeout elapses.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func exitCode(c commandJSON) int {
	if c.ExitCode == nil {
		return -999
	}
	return *c.ExitCode
}

func TestEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the e2e proof runs the real Linux agent")
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
	token, _, err := st.CreateEnrollmentToken(ctx, org.ID, "e2e", 5, 24*time.Hour, &admin.ID)
	if err != nil {
		t.Fatalf("create enrollment token: %v", err)
	}
	if !strings.HasPrefix(token, protocol.EnrollmentTokenPrefix) {
		t.Fatalf("enrollment token %q lacks prefix %q", token, protocol.EnrollmentTokenPrefix)
	}

	// --- enroll and run the real agent (loopback => http allowed) ------------
	cfg, err := agent.Enroll(ctx, ts.URL, token, "", false, 1, agentVersion)
	if err != nil {
		t.Fatalf("agent enroll: %v", err)
	}
	if cfg.DeviceID == 0 || !strings.HasPrefix(cfg.DeviceToken, protocol.DeviceTokenPrefix) {
		t.Fatalf("enroll returned device_id %d token %q", cfg.DeviceID, cfg.DeviceToken)
	}
	if cfg.IntervalS != 1 {
		t.Fatalf("enroll interval_s = %d, want 1", cfg.IntervalS)
	}

	agentCtx, stopAgent := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(agentCtx, cfg, agentVersion, logger) }()
	agentStopped := false
	waitAgent := func() { // idempotent: called in the lost-command step and again by defer
		t.Helper()
		stopAgent()
		if agentStopped {
			return
		}
		agentStopped = true
		select {
		case err := <-agentDone:
			if err != nil {
				t.Fatalf("agent.Run returned %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("agent.Run did not return within 30 s of cancel")
		}
	}
	defer waitAgent()

	// --- dashboard login -----------------------------------------------------
	d := newDash(t, ts.URL)
	var me struct {
		User struct {
			ID    int64  `json:"id"`
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	if code := d.do(http.MethodPost, "/api/v1/login", map[string]string{"email": adminEmail, "password": adminPassword}, &me); code != 200 {
		t.Fatalf("login: status %d", code)
	}
	if me.User.Role != "admin" || me.User.Email != adminEmail {
		t.Fatalf("login returned user %+v", me.User)
	}
	if code := d.do(http.MethodGet, "/api/v1/me", nil, &me); code != 200 || me.User.Role != "admin" {
		t.Fatalf("GET /api/v1/me: status %d user %+v", code, me.User)
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
	if dev.ID != cfg.DeviceID {
		t.Fatalf("online device id %d, enrolled id %d", dev.ID, cfg.DeviceID)
	}
	host, _ := os.Hostname()
	if dev.Hostname != host {
		t.Errorf("device hostname %q, want %q", dev.Hostname, host)
	}
	if !dev.CanRunCommands || dev.Revoked {
		t.Errorf("device can_run_commands=%v revoked=%v, want true/false", dev.CanRunCommands, dev.Revoked)
	}
	var detail struct {
		Device  deviceJSON     `json:"device"`
		Facts   map[string]any `json:"facts"`
		Metrics struct {
			MemTotalMB int64 `json:"mem_total_mb"`
		} `json:"metrics"`
		Commands []commandJSON `json:"commands"`
	}
	if code := d.do(http.MethodGet, fmt.Sprintf("/api/v1/devices/%d", dev.ID), nil, &detail); code != 200 {
		t.Fatalf("device detail: status %d", code)
	}
	if detail.Metrics.MemTotalMB <= 0 {
		t.Errorf("device detail metrics.mem_total_mb = %d, want > 0", detail.Metrics.MemTotalMB)
	}
	if detail.Facts["kernel"] == nil || detail.Facts["kernel"] == "" {
		t.Errorf("device detail facts.kernel missing: %v", detail.Facts)
	}

	// --- exec round trip -----------------------------------------------------
	start := time.Now()
	c := d.run(dev.ID, protocol.Command{Kind: protocol.KindExec, Argv: []string{"echo", "hello"}, TimeoutS: 30}, "done", 10*time.Second)
	t.Logf("exec round trip: %s", time.Since(start).Round(time.Millisecond))
	if c.Stdout != "hello\n" || exitCode(c) != 0 || c.Error != "" {
		t.Errorf("echo hello: stdout %q exit %d error %q", c.Stdout, exitCode(c), c.Error)
	}

	// --- shell metacharacters stay literal -----------------------------------
	c = d.run(dev.ID, protocol.Command{Kind: protocol.KindExec, Argv: []string{"echo", "a;id"}, TimeoutS: 30}, "done", 10*time.Second)
	if c.Stdout != "a;id\n" {
		t.Errorf("injection check: stdout %q, want \"a;id\\n\"", c.Stdout)
	}

	// --- script kind ---------------------------------------------------------
	c = d.run(dev.ID, protocol.Command{Kind: protocol.KindScript, Shell: protocol.ShellSh, Body: "printf x\n", TimeoutS: 30}, "done", 10*time.Second)
	if c.Stdout != "x" || exitCode(c) != 0 {
		t.Errorf("script: stdout %q exit %d", c.Stdout, exitCode(c))
	}

	// --- built-in ping (A1) --------------------------------------------------
	c = d.run(dev.ID, protocol.Command{Kind: protocol.KindPing, TimeoutS: 30}, "done", 10*time.Second)
	if c.Stdout != "pong" || exitCode(c) != 0 {
		t.Errorf("ping: stdout %q exit %d", c.Stdout, exitCode(c))
	}

	// --- timeout -------------------------------------------------------------
	start = time.Now()
	c = d.run(dev.ID, protocol.Command{Kind: protocol.KindExec, Argv: []string{"sleep", "5"}, TimeoutS: 1}, "timeout", 8*time.Second)
	t.Logf("timeout observed after %s", time.Since(start).Round(time.Millisecond))
	if exitCode(c) != -1 || !strings.HasPrefix(c.Error, "timeout") {
		t.Errorf("timeout: exit %d error %q", exitCode(c), c.Error)
	}

	// --- validation: wrong shell for the platform, unknown kind, no argv -------
	for _, bad := range []protocol.Command{
		{Kind: protocol.KindScript, Shell: protocol.ShellPowershell, Body: "dir"},
		{Kind: "reboot"},
		{Kind: protocol.KindExec},
	} {
		var e errorJSON
		if code := d.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/commands", dev.ID), bad, &e); code != 400 || e.Error != "invalid_request" {
			t.Errorf("queue %+v: status %d error %q, want 400 invalid_request", bad, code, e.Error)
		}
	}

	// --- a second result for a finished command is 409 -----------------------
	dup := protocol.CommandResult{ExitCode: 0, Stdout: "again", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
	plain := &http.Client{Timeout: 30 * time.Second}
	var e errorJSON
	code := doJSON(t, plain, http.MethodPost, fmt.Sprintf("%s/api/agent/v1/commands/%d/result", ts.URL, c.ID), cfg.DeviceToken, dup, &e, false)
	if code != 409 || e.Error != "conflict" {
		t.Errorf("duplicate result: status %d error %q, want 409 conflict", code, e.Error)
	}
	// ... and a result with a bad token is 401.
	code = doJSON(t, plain, http.MethodPost, fmt.Sprintf("%s/api/agent/v1/commands/%d/result", ts.URL, c.ID), "dt_bogus", dup, &e, false)
	if code != 401 || e.Error != "invalid_device_token" {
		t.Errorf("bad device token: status %d error %q, want 401 invalid_device_token", code, e.Error)
	}

	// --- android: exec refused, lock/ping/message delivered on sync ----------
	var enrolled protocol.EnrollResponse
	code = doJSON(t, plain, http.MethodPost, ts.URL+"/api/agent/v1/enroll", "", protocol.EnrollRequest{
		EnrollmentToken: token,
		DeviceUID:       "android-e2e",
		Hostname:        "Pixel 8",
		Platform:        protocol.PlatformAndroid,
		Arch:            "arm64",
		OSVersion:       "Android 14 (34)",
		AgentVersion:    "0.1.0",
		IntervalS:       60,
	}, &enrolled, false)
	if code != 201 || enrolled.DeviceID == 0 || enrolled.DeviceToken == "" {
		t.Fatalf("android enroll: status %d resp %+v", code, enrolled)
	}
	e = errorJSON{}
	code = d.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/commands", enrolled.DeviceID),
		protocol.Command{Kind: protocol.KindExec, Argv: []string{"id"}}, &e)
	if code != 400 || e.Error != "unsupported_on_platform" {
		t.Errorf("exec on android: status %d error %q, want 400 unsupported_on_platform", code, e.Error)
	}
	android := d.device(enrolled.DeviceID)
	if android.CanRunCommands || android.Status != "pending" {
		t.Errorf("android device can_run_commands=%v status=%q, want false/pending", android.CanRunCommands, android.Status)
	}
	lock := d.queue(enrolled.DeviceID, protocol.Command{Kind: protocol.KindLock, TimeoutS: 30})
	msg := d.queue(enrolled.DeviceID, protocol.Command{Kind: protocol.KindMessage, Text: "Please call the helpdesk", TimeoutS: 30})
	var sync protocol.SyncResponse
	code = doJSON(t, plain, http.MethodPost, ts.URL+"/api/agent/v1/sync", enrolled.DeviceToken, protocol.SyncRequest{
		AgentVersion: "0.1.0",
		IntervalS:    60,
		WaitS:        0,
		UptimeS:      100,
		Metrics: protocol.Metrics{
			MemTotalMB: 8000, MemUsedMB: 4000,
			Disks: []protocol.Disk{{Mount: "/data", TotalGB: 64, UsedGB: 20}},
			IPs:   []string{"10.0.0.5"},
		},
	}, &sync, false)
	if code != 200 {
		t.Fatalf("android sync: status %d", code)
	}
	if len(sync.Commands) != 2 || sync.Commands[0].ID != lock.ID || sync.Commands[0].Kind != protocol.KindLock ||
		sync.Commands[1].ID != msg.ID || sync.Commands[1].Kind != protocol.KindMessage || sync.Commands[1].Text != "Please call the helpdesk" {
		t.Errorf("android sync commands = %+v, want lock %d then message %d with text", sync.Commands, lock.ID, msg.ID)
	}
	if sync.PollIntervalS != 60 {
		t.Errorf("android poll_interval_s = %d, want 60", sync.PollIntervalS)
	}
	for _, id := range []int64{lock.ID, msg.ID} {
		res := protocol.CommandResult{ExitCode: 0, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
		if code := doJSON(t, plain, http.MethodPost, fmt.Sprintf("%s/api/agent/v1/commands/%d/result", ts.URL, id), enrolled.DeviceToken, res, nil, false); code != 204 {
			t.Errorf("android result for %d: status %d, want 204", id, code)
		}
		if got := d.command(id); got.Status != "done" {
			t.Errorf("android command %d status %q, want done", id, got.Status)
		}
	}
	waitFor(t, "android device online", 5*time.Second, func() bool { return d.device(enrolled.DeviceID).Status == "online" })

	// --- csrf and role checks ------------------------------------------------
	{
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/devices/%d/commands", ts.URL, dev.ID), strings.NewReader(`{"kind":"exec","argv":["id"]}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := d.hc.Do(req)
		if err != nil {
			t.Fatalf("csrf request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("POST without X-Requested-With: status %d, want 403", resp.StatusCode)
		}
	}
	var created struct {
		User struct {
			ID   int64  `json:"id"`
			Role string `json:"role"`
		} `json:"user"`
	}
	if code := d.do(http.MethodPost, "/api/v1/users", map[string]string{"email": "viewer@example.com", "password": "viewer-pass-123", "role": "viewer"}, &created); code != 201 || created.User.Role != "viewer" {
		t.Fatalf("create viewer: status %d user %+v", code, created.User)
	}
	viewer := newDash(t, ts.URL)
	if code := viewer.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "viewer@example.com", "password": "viewer-pass-123"}, nil); code != 200 {
		t.Fatalf("viewer login: status %d", code)
	}
	e = errorJSON{}
	if code := viewer.do(http.MethodPost, fmt.Sprintf("/api/v1/devices/%d/commands", dev.ID), protocol.Command{Kind: protocol.KindPing}, &e); code != 403 || e.Error != "forbidden" {
		t.Errorf("viewer queues command: status %d error %q, want 403 forbidden", code, e.Error)
	}
	if code := viewer.do(http.MethodGet, "/api/v1/devices", nil, nil); code != 200 {
		t.Errorf("viewer lists devices: status %d, want 200", code)
	}
	if code := viewer.do(http.MethodGet, "/api/v1/users", nil, nil); code != 403 {
		t.Errorf("viewer lists users: status %d, want 403", code)
	}
	anon := newDash(t, ts.URL)
	if code := anon.do(http.MethodGet, "/api/v1/devices", nil, nil); code != 401 {
		t.Errorf("anonymous lists devices: status %d, want 401", code)
	}

	// --- audit trail distinguishes user and device actors --------------------
	entries := d.audit()
	if n := countAudit(entries, "command.queued", "user"); n < 6 {
		t.Errorf("audit command.queued by user: %d, want >= 6", n)
	}
	if n := countAudit(entries, "command.finished", "device"); n < 6 {
		t.Errorf("audit command.finished by device: %d, want >= 6", n)
	}
	if n := countAudit(entries, "device.enrolled", "device"); n < 2 {
		t.Errorf("audit device.enrolled: %d, want >= 2", n)
	}
	if n := countAudit(entries, "device.lock_requested", "user"); n < 1 {
		t.Errorf("audit device.lock_requested by user: %d, want >= 1", n)
	}
	if n := countAudit(entries, "user.login", "user"); n < 2 {
		t.Errorf("audit user.login: %d, want >= 2", n)
	}

	// --- dashboard shell and health ------------------------------------------
	resp, err := plain.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	shell, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(shell), "app.js") {
		t.Errorf("GET /: status %d, body contains app.js = %v", resp.StatusCode, strings.Contains(string(shell), "app.js"))
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("GET /: Content-Security-Policy %q", csp)
	}
	var health struct {
		OK bool `json:"ok"`
	}
	if code := doJSON(t, plain, http.MethodGet, ts.URL+"/healthz", "", nil, &health, false); code != 200 || !health.OK {
		t.Errorf("GET /healthz: status %d ok=%v", code, health.OK)
	}

	// --- lost command: claimed by the agent, never reported ------------------
	// sleep 30 with a 60 s timeout; the agent claims it (status sent), then the
	// agent is stopped before it can finish, so no result ever arrives.
	lostCmd := d.queue(dev.ID, protocol.Command{Kind: protocol.KindExec, Argv: []string{"sleep", "30"}, TimeoutS: 60})
	waitFor(t, "lost-check command to be claimed", 10*time.Second, func() bool {
		return d.command(lostCmd.ID).Status == "sent"
	})
	waitAgent()
	if got := d.command(lostCmd.ID); got.Status != "sent" {
		t.Fatalf("command %d status after agent stop = %q, want sent", lostCmd.ID, got.Status)
	}

	// --- offline flip: advance the clock past 3*1+15 s and evaluate ----------
	base := time.Now().UTC().Truncate(time.Second)
	st.Now = func() time.Time { return base.Add(60 * time.Second) }
	server.EvaluateNow(srv)
	if got := d.device(dev.ID); got.Status != "offline" {
		t.Errorf("device status after +60 s = %q, want offline", got.Status)
	}
	offline := 0
	for _, a := range d.openAlerts() {
		if a.DeviceID == dev.ID && a.Type == "offline" {
			offline++
			if a.Severity != "high" {
				t.Errorf("offline alert severity %q, want high", a.Severity)
			}
		}
	}
	if offline != 1 {
		t.Errorf("open offline alerts for device %d = %d, want 1", dev.ID, offline)
	}
	if got := d.command(lostCmd.ID); got.Status != "sent" {
		t.Errorf("command %d at +60 s = %q, want still sent (60 s timeout + grace not reached)", lostCmd.ID, got.Status)
	}

	// --- lost sweep: +2 min is past sent_at + 60 s timeout + 1 s grace --------
	st.Now = func() time.Time { return base.Add(2 * time.Minute) }
	server.EvaluateNow(srv)
	if got := d.command(lostCmd.ID); got.Status != "lost" {
		t.Errorf("command %d at +2 min = %q, want lost", lostCmd.ID, got.Status)
	}
	entries = d.audit()
	if n := countAudit(entries, "command.lost", "system"); n != 1 {
		t.Errorf("audit command.lost by system: %d, want 1", n)
	}

	var summary struct {
		Orgs       int `json:"orgs"`
		Devices    int `json:"devices"`
		Online     int `json:"online"`
		Offline    int `json:"offline"`
		Pending    int `json:"pending"`
		OpenAlerts int `json:"open_alerts"`
	}
	if code := d.do(http.MethodGet, "/api/v1/summary", nil, &summary); code != 200 {
		t.Fatalf("summary: status %d", code)
	}
	// The android device (interval 60 s, window 195 s) is still online at +2 min.
	if summary.Orgs != 1 || summary.Devices != 2 || summary.Offline != 1 || summary.Online != 1 || summary.OpenAlerts < 1 {
		t.Errorf("summary = %+v, want 1 org, 2 devices, 1 offline, 1 online, >= 1 open alert", summary)
	}
}
