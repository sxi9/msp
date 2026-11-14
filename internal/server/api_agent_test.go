package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// testClock is the store/server clock under test control.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testEnv is one server over a temp SQLite file with an org, an enrollment
// token and an admin user ready to use.
type testEnv struct {
	t     *testing.T
	st    *store.Store
	srv   *Server
	ts    *httptest.Server
	clock *testClock
	org   store.Org
	admin store.User
	token string // enrollment token plaintext
}

const testAdminPassword = "admin-pass-123"

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "msp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	clock := &testClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	st.Now = clock.now

	cfg := DefaultConfig()
	cfg.Version = "test"
	cfg.SampleEvery = time.Second
	cfg.LostGrace = time.Minute
	srv := New(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	org, err := st.CreateOrg(ctx, "Acme Dental")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	hash, err := srv.hashPassword(testAdminPassword)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	admin, err := st.CreateUser(ctx, "admin@example.com", hash, RoleAdmin)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	tok, _, err := st.CreateEnrollmentToken(ctx, org.ID, "test", 10, 24*time.Hour, &admin.ID)
	if err != nil {
		t.Fatalf("CreateEnrollmentToken: %v", err)
	}
	return &testEnv{t: t, st: st, srv: srv, ts: ts, clock: clock, org: org, admin: admin, token: tok}
}

// do sends a JSON request and returns the response (body already read).
func (e *testEnv) do(method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rdr = strings.NewReader(b)
		case []byte:
			rdr = bytes.NewReader(b)
		default:
			buf, err := json.Marshal(body)
			if err != nil {
				e.t.Fatalf("marshal body: %v", err)
			}
			rdr = bytes.NewReader(buf)
		}
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func enrollRequest(uid, platform string) protocol.EnrollRequest {
	arch := "amd64"
	if platform == protocol.PlatformAndroid {
		arch = "arm64"
	}
	return protocol.EnrollRequest{
		DeviceUID:    uid,
		Hostname:     "ws-" + uid,
		Platform:     platform,
		Arch:         arch,
		OSVersion:    "Test OS 1.0",
		AgentVersion: "0.1.0",
		IntervalS:    30,
		Facts:        protocol.Facts{Kernel: "6.8.0", CPUCount: 4, MemTotalMB: 16000, IPs: []string{"10.0.0.12"}},
	}
}

// enroll enrolls a device through the API and returns its id and token.
func (e *testEnv) enroll(uid, platform string) (int64, string) {
	e.t.Helper()
	req := enrollRequest(uid, platform)
	req.EnrollmentToken = e.token
	resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("enroll %s: status %d body %s", uid, resp.StatusCode, body)
	}
	var out protocol.EnrollResponse
	if err := json.Unmarshal(body, &out); err != nil {
		e.t.Fatalf("enroll response: %v", err)
	}
	if !strings.HasPrefix(out.DeviceToken, "dt_") || out.DeviceID == 0 {
		e.t.Fatalf("enroll response %+v", out)
	}
	return out.DeviceID, out.DeviceToken
}

func syncRequest(waitS int) protocol.SyncRequest {
	cpu := 12.5
	return protocol.SyncRequest{
		AgentVersion: "0.1.0",
		IntervalS:    30,
		WaitS:        waitS,
		UptimeS:      86400,
		Metrics: protocol.Metrics{
			CPUPct:     &cpu,
			MemTotalMB: 16000,
			MemUsedMB:  8000,
			Disks:      []protocol.Disk{{Mount: "/", TotalGB: 100, UsedGB: 40}},
			IPs:        []string{"10.0.0.12"},
		},
	}
}

// sync posts a sync and decodes the response.
func (e *testEnv) sync(token string, req protocol.SyncRequest) (int, protocol.SyncResponse, []byte) {
	e.t.Helper()
	resp, body := e.do(http.MethodPost, "/api/agent/v1/sync", req, bearer(token))
	var out protocol.SyncResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			e.t.Fatalf("sync response %s: %v", body, err)
		}
	}
	return resp.StatusCode, out, body
}

// queue creates a queued command for a device directly in the store.
func (e *testEnv) queue(deviceID int64, argv ...string) store.Command {
	e.t.Helper()
	c, err := e.st.CreateCommand(context.Background(), store.CreateCommandParams{
		DeviceID: deviceID, OrgID: e.org.ID, CreatedBy: e.admin.ID,
		Kind: protocol.KindExec, Argv: argv, TimeoutS: 60,
	})
	if err != nil {
		e.t.Fatalf("CreateCommand: %v", err)
	}
	return c
}

func (e *testEnv) device(id int64) store.Device {
	e.t.Helper()
	d, err := e.st.GetDevice(context.Background(), id)
	if err != nil {
		e.t.Fatalf("GetDevice(%d): %v", id, err)
	}
	return d
}

func (e *testEnv) command(id int64) store.Command {
	e.t.Helper()
	c, err := e.st.GetCommand(context.Background(), id)
	if err != nil {
		e.t.Fatalf("GetCommand(%d): %v", id, err)
	}
	return c
}

func (e *testEnv) auditActions(action string) []store.AuditEntry {
	e.t.Helper()
	entries, err := e.st.ListAudit(context.Background(), 500, nil, nil, 0)
	if err != nil {
		e.t.Fatalf("ListAudit: %v", err)
	}
	var out []store.AuditEntry
	for _, en := range entries {
		if en.Action == action {
			out = append(out, en)
		}
	}
	return out
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var er protocol.ErrorResponse
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatalf("error body %q: %v", body, err)
	}
	return er.Error
}

func TestPingAndHealthz(t *testing.T) {
	e := newTestEnv(t)
	resp, body := e.do(http.MethodGet, "/api/agent/v1/ping", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("ping status %d", resp.StatusCode)
	}
	var ping protocol.PingResponse
	if err := json.Unmarshal(body, &ping); err != nil || !ping.OK || ping.Version != "test" {
		t.Fatalf("ping body %s (%v)", body, err)
	}
	for _, h := range []struct{ name, want string }{
		{"Content-Security-Policy", cspValue},
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		{"Referrer-Policy", "no-referrer"},
	} {
		if got := resp.Header.Get(h.name); got != h.want {
			t.Errorf("header %s = %q, want %q", h.name, got, h.want)
		}
	}
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Errorf("HSTS must not be set without TLS")
	}

	resp, body = e.do(http.MethodGet, "/healthz", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("healthz %d %s", resp.StatusCode, body)
	}
}

func TestEnroll(t *testing.T) {
	e := newTestEnv(t)
	e.srv.enrollLimit = newRateLimiter(1000) // this test enrolls more than 10 times

	id, token := e.enroll("uid-1", protocol.PlatformLinux)
	d := e.device(id)
	if d.Hostname != "ws-uid-1" || d.Platform != "linux" || d.OrgID != e.org.ID || d.Status != "pending" || d.IntervalS != 30 {
		t.Fatalf("device row %+v", d)
	}
	if !strings.Contains(d.FactsJSON, `"kernel":"6.8.0"`) {
		t.Errorf("facts not stored: %s", d.FactsJSON)
	}
	if _, err := e.st.GetDeviceByToken(context.Background(), token); err != nil {
		t.Fatalf("device token not usable: %v", err)
	}
	if got := e.auditActions("device.enrolled"); len(got) != 1 || got[0].ActorType != "device" || got[0].ActorID == nil || *got[0].ActorID != id {
		t.Fatalf("audit device.enrolled = %+v", got)
	}

	// Re-enrolling the same uid keeps the row and rotates the token.
	id2, token2 := e.enroll("uid-1", protocol.PlatformLinux)
	if id2 != id || token2 == token {
		t.Fatalf("re-enroll: id %d->%d token rotated=%v", id, id2, token2 != token)
	}
	if _, err := e.st.GetDeviceByToken(context.Background(), token); err == nil {
		t.Fatalf("old token still valid after re-enroll")
	}

	// Bad token.
	req := enrollRequest("uid-2", protocol.PlatformLinux)
	req.EnrollmentToken = "et_nope"
	resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil)
	if resp.StatusCode != 401 || errorCode(t, body) != "invalid_enrollment_token" {
		t.Fatalf("bad token: %d %s", resp.StatusCode, body)
	}

	// Invalid request shapes.
	for _, bad := range []protocol.EnrollRequest{
		{EnrollmentToken: e.token, DeviceUID: "", Hostname: "h", Platform: "linux", Arch: "amd64"},
		{EnrollmentToken: e.token, DeviceUID: "u", Hostname: "", Platform: "linux", Arch: "amd64"},
		{EnrollmentToken: e.token, DeviceUID: "u", Hostname: "h", Platform: "macos", Arch: "amd64"},
		{EnrollmentToken: e.token, DeviceUID: "u", Hostname: "h", Platform: "linux", Arch: "mips"},
	} {
		resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", bad, nil)
		if resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
			t.Errorf("invalid enroll %+v: %d %s", bad, resp.StatusCode, body)
		}
	}
	resp, body = e.do(http.MethodPost, "/api/agent/v1/enroll", "{not json", nil)
	if resp.StatusCode != 400 {
		t.Errorf("malformed JSON: %d %s", resp.StatusCode, body)
	}

	// Exhausted token (max_uses 1).
	one, _, err := e.st.CreateEnrollmentToken(context.Background(), e.org.ID, "one", 1, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	req = enrollRequest("uid-3", protocol.PlatformLinux)
	req.EnrollmentToken = one
	if resp, _ := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil); resp.StatusCode != 201 {
		t.Fatalf("first use of single-use token: %d", resp.StatusCode)
	}
	req.DeviceUID = "uid-4"
	if resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil); resp.StatusCode != 401 || errorCode(t, body) != "invalid_enrollment_token" {
		t.Fatalf("exhausted token: %d %s", resp.StatusCode, body)
	}

	// Expired token.
	e.clock.advance(25 * time.Hour)
	req = enrollRequest("uid-5", protocol.PlatformLinux)
	req.EnrollmentToken = e.token
	if resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil); resp.StatusCode != 401 {
		t.Fatalf("expired token: %d %s", resp.StatusCode, body)
	}
}

func TestEnrollRateLimit(t *testing.T) {
	e := newTestEnv(t)
	req := enrollRequest("rl", protocol.PlatformLinux)
	req.EnrollmentToken = "et_bogus"
	for i := 1; i <= enrollPerMinute; i++ {
		if resp, _ := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil); resp.StatusCode != 401 {
			t.Fatalf("call %d: status %d, want 401", i, resp.StatusCode)
		}
	}
	resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil)
	if resp.StatusCode != 429 || errorCode(t, body) != "rate_limited" {
		t.Fatalf("call %d: %d %s", enrollPerMinute+1, resp.StatusCode, body)
	}
}

// TestReEnrollFromAnotherHostIsAudited: the same (org, device_uid) enrolling
// under a different hostname (cloned machine-id) rotates the token as
// documented, but the audit row and the server log say so.
func TestReEnrollFromAnotherHostIsAudited(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("clone", protocol.PlatformLinux)
	if status, _, _ := e.sync(token, syncRequest(0)); status != 200 {
		t.Fatal("sync")
	}
	req := enrollRequest("clone", protocol.PlatformLinux)
	req.Hostname = "ws-other"
	req.EnrollmentToken = e.token
	resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("re-enroll: %d %s", resp.StatusCode, body)
	}
	var out protocol.EnrollResponse
	if err := json.Unmarshal(body, &out); err != nil || out.DeviceID != id {
		t.Fatalf("re-enroll response %s (%v), want device %d", body, err, id)
	}
	a := e.auditActions("device.enrolled")
	if len(a) != 2 {
		t.Fatalf("device.enrolled rows = %d", len(a))
	}
	first, second := a[1].Detail, a[0].Detail
	if a[0].ID < a[1].ID {
		first, second = a[0].Detail, a[1].Detail
	}
	if _, ok := first["re_enrolled"]; ok {
		t.Fatalf("first enrollment flagged as re-enrollment: %v", first)
	}
	if second["re_enrolled"] != true || second["previous_hostname"] != "ws-clone" || second["previous_status"] != "online" || second["warning"] == nil {
		t.Fatalf("second enrollment detail %v", second)
	}
	// The first agent's token is now invalid, as the protocol documents.
	if status, _, _ := e.sync(token, syncRequest(0)); status != 401 {
		t.Fatalf("old token after re-enroll: %d, want 401", status)
	}
	// Same hostname again: re_enrolled but no warning.
	req.Hostname = "ws-other"
	if resp, _ := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil); resp.StatusCode != 201 {
		t.Fatalf("third enroll: %d", resp.StatusCode)
	}
	a = e.auditActions("device.enrolled")
	var newest store.AuditEntry
	for _, row := range a {
		if row.ID > newest.ID {
			newest = row
		}
	}
	if newest.Detail["re_enrolled"] != true || newest.Detail["warning"] != nil {
		t.Fatalf("same-host re-enroll detail %v", newest.Detail)
	}
}

// TestSyncLongPollReleasedOnShutdown: a parked sync answers at once with an
// empty command list when the server starts shutting down, so Run's 10 s
// Shutdown budget is never consumed by long-polls.
func TestSyncLongPollReleasedOnShutdown(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("shut", protocol.PlatformLinux)

	type result struct {
		status  int
		out     protocol.SyncResponse
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		status, out, _ := e.sync(token, syncRequest(25))
		done <- result{status, out, time.Since(start)}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.srv.notifier.waiting(id) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sync never parked on the notifier")
		}
		time.Sleep(5 * time.Millisecond)
	}
	began := time.Now()
	e.srv.beginShutdown()
	e.srv.beginShutdown() // idempotent
	r := <-done
	if r.status != 200 || len(r.out.Commands) != 0 || r.out.PollIntervalS != 30 {
		t.Fatalf("released poll: %d %+v", r.status, r.out)
	}
	if since := time.Since(began); since > time.Second {
		t.Fatalf("released %v after beginShutdown, want well under the 25 s wait", since)
	}
	// A sync after shutdown began does not park either.
	start = time.Now()
	if status, out, _ := e.sync(token, syncRequest(25)); status != 200 || len(out.Commands) != 0 || time.Since(start) > time.Second {
		t.Fatalf("post-shutdown sync: %d %+v after %v", status, out, time.Since(start))
	}
}

func TestSyncMarksOnlineAndStoresSample(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("sync", protocol.PlatformLinux)

	status, out, body := e.sync(token, syncRequest(0))
	if status != 200 {
		t.Fatalf("sync: %d %s", status, body)
	}
	if out.PollIntervalS != 30 || out.Commands == nil || len(out.Commands) != 0 {
		t.Fatalf("sync response %+v", out)
	}
	if !strings.Contains(string(body), `"commands":[]`) {
		t.Fatalf("commands must serialise as []: %s", body)
	}
	d := e.device(id)
	if d.Status != "online" || d.LastSeenAt == nil || d.LastIP == "" {
		t.Fatalf("device after sync %+v", d)
	}
	if d.CPUPct == nil || *d.CPUPct != 12.5 || d.MemUsedPct == nil || *d.MemUsedPct != 50 || d.DiskUsedPct == nil || *d.DiskUsedPct != 40 {
		t.Fatalf("metrics columns %+v", d)
	}
	hb, err := e.st.ListHeartbeats(context.Background(), id, 10)
	if err != nil || len(hb) != 1 {
		t.Fatalf("heartbeats = %v, %v", hb, err)
	}

	// Interval is clamped and stored; 0 means the platform default.
	req := syncRequest(0)
	req.IntervalS = 0
	if _, out, _ := e.sync(token, req); out.PollIntervalS != 30 {
		t.Errorf("interval 0 -> poll %d, want 30", out.PollIntervalS)
	}
	req.IntervalS = 99999
	if _, out, _ := e.sync(token, req); out.PollIntervalS != protocol.MaxIntervalS {
		t.Errorf("interval 99999 -> poll %d, want %d", out.PollIntervalS, protocol.MaxIntervalS)
	}

	// No bearer / bad bearer.
	resp, body := e.do(http.MethodPost, "/api/agent/v1/sync", syncRequest(0), nil)
	if resp.StatusCode != 401 || errorCode(t, body) != "invalid_device_token" {
		t.Fatalf("no bearer: %d %s", resp.StatusCode, body)
	}
	if status, _, _ := e.sync("dt_wrong", syncRequest(0)); status != 401 {
		t.Fatalf("bad bearer: %d", status)
	}
}

func TestSyncDeliversQueuedAndSignalsMore(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("queued", protocol.PlatformLinux)
	for i := 0; i < 11; i++ {
		e.queue(id, "echo", fmt.Sprint(i))
	}
	status, out, body := e.sync(token, syncRequest(25))
	if status != 200 {
		t.Fatalf("sync: %d %s", status, body)
	}
	if len(out.Commands) != protocol.MaxCommandsPerSync || out.PollIntervalS != 5 {
		t.Fatalf("got %d commands poll %d; want 10 and 5", len(out.Commands), out.PollIntervalS)
	}
	if out.Commands[0].Kind != "exec" || len(out.Commands[0].Argv) != 2 || out.Commands[0].TimeoutS != 60 {
		t.Fatalf("first command %+v", out.Commands[0])
	}
	if c := e.command(out.Commands[0].ID); c.Status != store.StatusSent || c.SentAt == nil {
		t.Fatalf("claimed command %+v", c)
	}
	status, out, _ = e.sync(token, syncRequest(25))
	if status != 200 || len(out.Commands) != 1 || out.PollIntervalS != 30 {
		t.Fatalf("second sync: %d %+v", status, out)
	}
}

func TestSyncLongPollWakesOnNotify(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("poll", protocol.PlatformLinux)

	type result struct {
		status  int
		out     protocol.SyncResponse
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		status, out, _ := e.sync(token, syncRequest(10))
		done <- result{status, out, time.Since(start)}
	}()

	// Wait until the request is parked, then queue a command and notify.
	deadline := time.Now().Add(5 * time.Second)
	for e.srv.notifier.waiting(id) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sync never parked on the notifier")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c := e.queue(id, "uname", "-a")
	notified := time.Now()
	e.srv.notifier.Notify(id)

	r := <-done
	if r.status != 200 || len(r.out.Commands) != 1 || r.out.Commands[0].ID != c.ID {
		t.Fatalf("long-poll result %+v", r)
	}
	if since := time.Since(notified); since > 200*time.Millisecond {
		t.Fatalf("woke %v after Notify, want < 200ms", since)
	}
	if e.srv.notifier.waiting(id) != 0 {
		t.Fatalf("waiter not unsubscribed")
	}
}

func TestSyncLongPollTimesOutEmpty(t *testing.T) {
	e := newTestEnv(t)
	_, token := e.enroll("poll2", protocol.PlatformLinux)
	start := time.Now()
	status, out, _ := e.sync(token, syncRequest(1))
	elapsed := time.Since(start)
	if status != 200 || len(out.Commands) != 0 || out.PollIntervalS != 30 {
		t.Fatalf("timed-out poll: %d %+v", status, out)
	}
	if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("poll took %v, want about 1 s", elapsed)
	}
}

func TestSyncClaimsExactlyOnce(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("race", protocol.PlatformLinux)
	c := e.queue(id, "id")

	const n = 4
	results := make(chan protocol.SyncResponse, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, out, body := e.sync(token, syncRequest(0))
			if status != 200 {
				t.Errorf("sync: %d %s", status, body)
			}
			results <- out
		}()
	}
	wg.Wait()
	close(results)
	delivered := 0
	for out := range results {
		for _, cmd := range out.Commands {
			if cmd.ID == c.ID {
				delivered++
			}
		}
	}
	if delivered != 1 {
		t.Fatalf("command delivered %d times, want exactly once", delivered)
	}
}

func TestSyncAndroidNoWaitButGetsBuiltinCommands(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("pixel", protocol.PlatformAndroid)
	if d := e.device(id); d.CanRunCommands || d.IntervalS != 30 {
		t.Fatalf("android device %+v", d)
	}
	lock, err := e.st.CreateCommand(context.Background(), store.CreateCommandParams{
		DeviceID: id, OrgID: e.org.ID, CreatedBy: e.admin.ID, Kind: protocol.KindLock, TimeoutS: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := e.st.CreateCommand(context.Background(), store.CreateCommandParams{
		DeviceID: id, OrgID: e.org.ID, CreatedBy: e.admin.ID, Kind: protocol.KindMessage, Text: "call the helpdesk", TimeoutS: 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := syncRequest(25) // wait must be ignored for android
	req.IntervalS = 60
	req.Metrics.CPUPct = nil
	start := time.Now()
	status, out, body := e.sync(token, req)
	if status != 200 || time.Since(start) > time.Second {
		t.Fatalf("android sync: %d after %v (%s)", status, time.Since(start), body)
	}
	if len(out.Commands) != 2 || out.Commands[0].ID != lock.ID || out.Commands[0].Kind != "lock" || out.Commands[1].ID != msg.ID || out.Commands[1].Text != "call the helpdesk" {
		t.Fatalf("android commands %+v", out.Commands)
	}
	if strings.Contains(string(body), `"argv"`) || !strings.Contains(string(body), `"text":"call the helpdesk"`) {
		t.Fatalf("wire shape: %s", body)
	}
	if out.PollIntervalS != 60 {
		t.Fatalf("android poll %d, want 60", out.PollIntervalS)
	}

	// Nothing queued: still returns immediately.
	start = time.Now()
	if status, out, _ := e.sync(token, req); status != 200 || len(out.Commands) != 0 || time.Since(start) > time.Second {
		t.Fatalf("empty android sync: %d %+v after %v", status, out, time.Since(start))
	}
}

func TestResult(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("res", protocol.PlatformLinux)
	_, otherToken := e.enroll("other", protocol.PlatformLinux)
	now := e.clock.now()
	okResult := protocol.CommandResult{ExitCode: 0, Stdout: "Linux\n", StartedAt: now, FinishedAt: now.Add(time.Second)}

	// Exists for this device but not in status sent (still queued) -> 409
	// (store.ErrConflict: the row is the device's but is not awaiting a result).
	c := e.queue(id, "uname", "-a")
	path := fmt.Sprintf("/api/agent/v1/commands/%d/result", c.ID)
	if resp, body := e.do(http.MethodPost, path, okResult, bearer(token)); resp.StatusCode != 409 || errorCode(t, body) != "conflict" {
		t.Fatalf("result for queued command: %d %s", resp.StatusCode, body)
	}
	if _, out, _ := e.sync(token, syncRequest(0)); len(out.Commands) != 1 {
		t.Fatal("command not claimed")
	}

	// Wrong device -> 404.
	if resp, body := e.do(http.MethodPost, path, okResult, bearer(otherToken)); resp.StatusCode != 404 {
		t.Fatalf("result from other device: %d %s", resp.StatusCode, body)
	}
	// Unknown id and bad id.
	if resp, _ := e.do(http.MethodPost, "/api/agent/v1/commands/999999/result", okResult, bearer(token)); resp.StatusCode != 404 {
		t.Fatalf("unknown id: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodPost, "/api/agent/v1/commands/abc/result", okResult, bearer(token)); resp.StatusCode != 404 {
		t.Fatalf("bad id: %d", resp.StatusCode)
	}
	// Happy path.
	if resp, body := e.do(http.MethodPost, path, okResult, bearer(token)); resp.StatusCode != 204 {
		t.Fatalf("result: %d %s", resp.StatusCode, body)
	}
	got := e.command(c.ID)
	if got.Status != store.StatusDone || got.Stdout != "Linux\n" || got.ExitCode == nil || *got.ExitCode != 0 || got.FinishedAt == nil {
		t.Fatalf("completed command %+v", got)
	}
	if a := e.auditActions("command.finished"); len(a) != 1 || a[0].ActorType != "device" || a[0].Detail["status"] != "done" {
		t.Fatalf("audit command.finished %+v", a)
	}
	// Duplicate -> 409.
	if resp, body := e.do(http.MethodPost, path, okResult, bearer(token)); resp.StatusCode != 409 || errorCode(t, body) != "conflict" {
		t.Fatalf("duplicate result: %d %s", resp.StatusCode, body)
	}

	// Status derivation: failed and timeout; server-side output cap.
	cases := []struct {
		res  protocol.CommandResult
		want string
	}{
		{protocol.CommandResult{ExitCode: 1, Stderr: "boom", StartedAt: now, FinishedAt: now}, store.StatusFailed},
		{protocol.CommandResult{ExitCode: -1, Error: "spawn: not found", StartedAt: now, FinishedAt: now}, store.StatusFailed},
		{protocol.CommandResult{ExitCode: -1, Error: "timeout after 60s", StartedAt: now, FinishedAt: now}, store.StatusTimeout},
		{protocol.CommandResult{ExitCode: 0, Stdout: strings.Repeat("x", protocol.MaxOutputBytes+10), StartedAt: now, FinishedAt: now}, store.StatusDone},
	}
	for _, tc := range cases {
		c := e.queue(id, "true")
		e.sync(token, syncRequest(0))
		resp, body := e.do(http.MethodPost, fmt.Sprintf("/api/agent/v1/commands/%d/result", c.ID), tc.res, bearer(token))
		if resp.StatusCode != 204 {
			t.Fatalf("result %+v: %d %s", tc.res.Error, resp.StatusCode, body)
		}
		got := e.command(c.ID)
		if got.Status != tc.want {
			t.Errorf("status for error %q exit %d = %q, want %q", tc.res.Error, tc.res.ExitCode, got.Status, tc.want)
		}
		if len(tc.res.Stdout) > protocol.MaxOutputBytes && (len(got.Stdout) != protocol.MaxOutputBytes || !got.Truncated) {
			t.Errorf("stdout not capped: len %d truncated %v", len(got.Stdout), got.Truncated)
		}
	}
}

func TestRevokedDeviceIsRejected(t *testing.T) {
	e := newTestEnv(t)
	id, token := e.enroll("rev", protocol.PlatformLinux)
	if status, _, _ := e.sync(token, syncRequest(0)); status != 200 {
		t.Fatalf("sync before revoke: %d", status)
	}
	if err := e.st.RevokeDevice(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	status, _, body := e.sync(token, syncRequest(0))
	if status != 401 || errorCode(t, body) != "invalid_device_token" {
		t.Fatalf("sync after revoke: %d %s", status, body)
	}
	resp, _ := e.do(http.MethodPost, "/api/agent/v1/commands/1/result", protocol.CommandResult{}, bearer(token))
	if resp.StatusCode != 401 {
		t.Fatalf("result after revoke: %d", resp.StatusCode)
	}
}

func TestBodyLimitAndTrailingData(t *testing.T) {
	e := newTestEnv(t)
	big := bytes.Repeat([]byte("a"), 300<<10)
	resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", big, nil)
	if resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("oversized agent body: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(http.MethodPost, "/api/agent/v1/enroll", `{"enrollment_token":"x"} {"again":1}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "trailing") {
		t.Fatalf("trailing data: %d %s", resp.StatusCode, body)
	}
}

func TestClientIPTrustProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:4444"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(r); got != "10.1.2.3" {
		t.Fatalf("clientIP outside middleware = %q", got)
	}
	s := &Server{cfg: Config{TrustProxy: false}}
	if got := s.resolveIP(r); got != "10.1.2.3" {
		t.Fatalf("untrusted proxy: %q", got)
	}
	s.cfg.TrustProxy = true
	if got := s.resolveIP(r); got != "203.0.113.9" {
		t.Fatalf("trusted proxy: %q", got)
	}
}

// TestEnrollRejectsControlCharsInHostname is the regression for the SMTP
// header/body injection finding: a hostname carrying a CR/LF (or any control
// char) is refused at enrollment, so it can never reach the notification email's
// Subject header. A normal hostname still enrolls.
func TestEnrollRejectsControlCharsInHostname(t *testing.T) {
	e := newTestEnv(t)
	e.srv.enrollLimit = newRateLimiter(1000)
	for _, host := range []string{
		"host\r\nBcc: attacker@evil.test",
		"host\r\n\r\ninjected body",
		"host\nx",
		"host\tx",
		"bad\x00host",
	} {
		req := enrollRequest("uid-ctl", protocol.PlatformLinux)
		req.EnrollmentToken = e.token
		req.Hostname = host
		resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil)
		if resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
			t.Fatalf("hostname %q should be 400 invalid_request: %d %s", host, resp.StatusCode, body)
		}
	}
	req := enrollRequest("uid-ok", protocol.PlatformLinux)
	req.EnrollmentToken = e.token
	req.Hostname = "ws-reception-01"
	if resp, body := e.do(http.MethodPost, "/api/agent/v1/enroll", req, nil); resp.StatusCode != 201 {
		t.Fatalf("normal hostname should enroll: %d %s", resp.StatusCode, body)
	}
}
