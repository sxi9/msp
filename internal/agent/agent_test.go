package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// fakeServer is a minimal in-memory /api/agent/v1 implementation.
type fakeServer struct {
	t *testing.T

	mu       sync.Mutex
	requests int // every sync request, authorised or not
	enrolls  []protocol.EnrollRequest
	syncs    []protocol.SyncRequest
	results  map[int64]protocol.CommandResult
	resultN  map[int64]int
	ua       string
	auth     []string
	script   []syncStep      // consumed one per sync
	resultRC map[int64][]int // per-command status codes to answer before 204
}

type syncStep struct {
	status int
	resp   protocol.SyncResponse
}

func newFakeServer(t *testing.T) (*fakeServer, *httptest.Server) {
	f := &fakeServer{t: t, results: map[int64]protocol.CommandResult{}, resultN: map[int64]int{}, resultRC: map[int64][]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/v1/enroll", f.enroll)
	mux.HandleFunc("POST /api/agent/v1/sync", f.sync)
	mux.HandleFunc("POST /api/agent/v1/commands/{id}/result", f.result)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return f, ts
}

func writeErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(protocol.ErrorResponse{Error: code})
}

func (f *fakeServer) enroll(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid_request")
		return
	}
	f.mu.Lock()
	f.enrolls = append(f.enrolls, req)
	f.ua = r.Header.Get("User-Agent")
	f.mu.Unlock()
	if req.EnrollmentToken != "et_good" {
		writeErr(w, 401, "invalid_enrollment_token")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(protocol.EnrollResponse{DeviceID: 42, DeviceToken: "dt_secret", PollIntervalS: req.IntervalS})
}

func (f *fakeServer) sync(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer dt_secret" {
		writeErr(w, 401, "invalid_device_token")
		return
	}
	var req protocol.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid_request")
		return
	}
	f.mu.Lock()
	f.syncs = append(f.syncs, req)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	var step syncStep
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	} else {
		step = syncStep{status: 200, resp: protocol.SyncResponse{PollIntervalS: req.IntervalS, Commands: []protocol.Command{}}}
	}
	f.mu.Unlock()
	if step.status != 200 {
		writeErr(w, step.status, "boom")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(step.resp)
}

func (f *fakeServer) result(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer dt_secret" {
		writeErr(w, 401, "invalid_device_token")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var res protocol.CommandResult
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		writeErr(w, 400, "invalid_request")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resultN[id]++
	if codes := f.resultRC[id]; len(codes) > 0 {
		code := codes[0]
		f.resultRC[id] = codes[1:]
		writeErr(w, code, "scripted")
		return
	}
	f.results[id] = res
	w.WriteHeader(204)
}

func (f *fakeServer) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncs)
}

func (f *fakeServer) resultFor(id int64) (protocol.CommandResult, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[id]
	return r, ok
}

// sleepRecorder replaces sleepFn and records every requested duration.
type sleepRecorder struct {
	mu    sync.Mutex
	sleep []time.Duration
	// hook may stop the loop: return false to make the agent return.
	hook func(d time.Duration) bool
}

func (s *sleepRecorder) fn(ctx context.Context, d time.Duration) bool {
	s.mu.Lock()
	s.sleep = append(s.sleep, d)
	s.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	if s.hook != nil {
		return s.hook(d)
	}
	return true
}

func (s *sleepRecorder) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.sleep...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEnrollWritesConfig(t *testing.T) {
	f, ts := newFakeServer(t)
	cfg, err := Enroll(context.Background(), ts.URL+"/", "et_good", "", false, 7, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceToken != "dt_secret" || cfg.DeviceID != 42 || cfg.IntervalS != 7 || cfg.ServerURL != ts.URL {
		t.Fatalf("config = %+v", cfg)
	}
	f.mu.Lock()
	req := f.enrolls[0]
	ua := f.ua
	f.mu.Unlock()
	if req.Platform != "linux" || req.Arch == "" || req.Hostname == "" || req.DeviceUID == "" || req.OSVersion == "" {
		t.Fatalf("enroll request = %+v", req)
	}
	if req.AgentVersion != "1.2.3" || req.IntervalS != 7 || req.Facts.CPUCount < 1 || req.Facts.IPs == nil {
		t.Fatalf("enroll request = %+v", req)
	}
	if !strings.HasPrefix(ua, "msp-agent/1.2.3 (linux/") {
		t.Fatalf("user agent = %q", ua)
	}

	path := filepath.Join(t.TempDir(), "sub", "agent.json")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", st.Mode().Perm())
	}
	dst, _ := os.Stat(filepath.Dir(path))
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("config dir mode = %o, want 700", dst.Mode().Perm())
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if back != cfg {
		t.Fatalf("round trip: %+v != %+v", back, cfg)
	}
	raw, _ := os.ReadFile(path)
	for _, key := range []string{`"server_url"`, `"device_token"`, `"device_id"`, `"interval_s"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("config json lacks %s: %s", key, raw)
		}
	}
	if strings.Contains(string(raw), "ca_file") || strings.Contains(string(raw), "insecure_http") {
		t.Fatalf("empty optional keys must be omitted: %s", raw)
	}

	status, err := Status(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, path) || !strings.Contains(status, "42") || !strings.Contains(status, ts.URL) {
		t.Fatalf("status = %q", status)
	}
}

func TestEnrollBadToken(t *testing.T) {
	_, ts := newFakeServer(t)
	_, err := Enroll(context.Background(), ts.URL, "et_bad", "", false, 0, "test")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 401 || he.Code != "invalid_enrollment_token" {
		t.Fatalf("err = %#v", err)
	}
}

func TestEnrollDefaultInterval(t *testing.T) {
	_, ts := newFakeServer(t)
	cfg, err := Enroll(context.Background(), ts.URL, "et_good", "", false, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IntervalS != protocol.DefaultIntervalDesktop {
		t.Fatalf("interval = %d, want %d", cfg.IntervalS, protocol.DefaultIntervalDesktop)
	}
}

func TestClientURLPolicy(t *testing.T) {
	cases := []struct {
		url      string
		insecure bool
		ok       bool
	}{
		{"https://msp.example.com", false, true},
		{"https://msp.example.com:8443/", false, true},
		{"http://127.0.0.1:18080", false, true},
		{"http://localhost:8080", false, true},
		{"http://[::1]:8080", false, true},
		{"http://10.0.0.5:8080", false, false},
		{"http://msp.example.com", false, false},
		{"http://msp.example.com", true, true},
		{"ftp://msp.example.com", false, false},
		{"msp.example.com", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		_, err := NewClient(Config{ServerURL: c.url, InsecureHTTP: c.insecure}, "test")
		if (err == nil) != c.ok {
			t.Errorf("NewClient(%q, insecure=%v): err=%v, want ok=%v", c.url, c.insecure, err, c.ok)
		}
		if err != nil && !c.ok && strings.HasPrefix(c.url, "http://") && !c.insecure {
			if !strings.Contains(err.Error(), "--insecure-http") {
				t.Errorf("error should mention --insecure-http: %v", err)
			}
		}
	}
	if _, err := NewClient(Config{ServerURL: "https://x", CAFile: "/nonexistent.pem"}, "t"); err == nil {
		t.Errorf("missing CA file must fail")
	}
	bad := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(bad, []byte("not pem"), 0o600)
	if _, err := NewClient(Config{ServerURL: "https://x", CAFile: bad}, "t"); err == nil {
		t.Errorf("CA file without certificates must fail")
	}
}

func TestRunSyncsExecutesAndPostsResult(t *testing.T) {
	f, ts := newFakeServer(t)
	f.script = []syncStep{
		{200, protocol.SyncResponse{PollIntervalS: 5, Commands: []protocol.Command{
			{ID: 7, Kind: protocol.KindExec, Argv: []string{"echo", "hello"}, TimeoutS: 30},
			{ID: 8, Kind: protocol.KindPing, TimeoutS: 30},
		}}},
	}
	rec := &sleepRecorder{}
	old := sleepFn
	sleepFn = rec.fn
	defer func() { sleepFn = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{ServerURL: ts.URL, DeviceToken: "dt_secret", DeviceID: 42, IntervalS: 10}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, "9.9.9", testLogger(t)) }()

	waitFor(t, "both results", func() bool {
		_, a := f.resultFor(7)
		_, b := f.resultFor(8)
		return a && b
	})
	waitFor(t, "second sync", func() bool { return f.syncCount() >= 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}

	f.mu.Lock()
	first, second := f.syncs[0], f.syncs[1]
	auth := f.auth[0]
	f.mu.Unlock()
	if auth != "Bearer dt_secret" {
		t.Fatalf("auth header = %q", auth)
	}
	if first.AgentVersion != "9.9.9" || first.IntervalS != 10 || first.WaitS != 9 {
		t.Fatalf("first sync = %+v", first)
	}
	if first.Facts == nil || first.Facts.CPUCount < 1 {
		t.Fatalf("first sync must carry facts: %+v", first)
	}
	if second.Facts != nil {
		t.Fatalf("second sync must not carry facts")
	}
	if first.Metrics.MemTotalMB <= 0 || first.Metrics.IPs == nil || first.Metrics.Disks == nil {
		t.Fatalf("metrics = %+v", first.Metrics)
	}
	if first.Metrics.CPUPct != nil {
		t.Fatalf("first sample must have cpu_pct null, got %v", *first.Metrics.CPUPct)
	}
	res7, _ := f.resultFor(7)
	if res7.ExitCode != 0 || res7.Stdout != "hello\n" || res7.Error != "" {
		t.Fatalf("result 7 = %+v", res7)
	}
	res8, _ := f.resultFor(8)
	if res8.ExitCode != 0 || res8.Stdout != "pong" {
		t.Fatalf("result 8 = %+v", res8)
	}
	// After a response with commands the agent re-syncs immediately; the
	// first sleep comes after the empty second sync.
	ds := rec.durations()
	if len(ds) == 0 {
		t.Fatalf("expected the loop to sleep after the empty second sync")
	}
	if ds[0] <= 0 || ds[0] > 10*time.Second {
		t.Fatalf("first sleep = %v, want (0, 10 s] (poll 10 minus elapsed)", ds[0])
	}
}

func TestRunImmediateResyncAfterCommands(t *testing.T) {
	f, ts := newFakeServer(t)
	f.script = []syncStep{
		{200, protocol.SyncResponse{PollIntervalS: 2, Commands: []protocol.Command{{ID: 1, Kind: protocol.KindPing, TimeoutS: 5}}}},
		{200, protocol.SyncResponse{PollIntervalS: 2, Commands: []protocol.Command{{ID: 2, Kind: protocol.KindPing, TimeoutS: 5}}}},
	}
	rec := &sleepRecorder{}
	old := sleepFn
	sleepFn = rec.fn
	defer func() { sleepFn = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopAt := make(chan struct{})
	rec.hook = func(time.Duration) bool { close(stopAt); return false }
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{ServerURL: ts.URL, DeviceToken: "dt_secret", IntervalS: 2}, "t", testLogger(t))
	}()
	<-stopAt
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Three syncs (two with commands, one empty) and exactly one sleep: the
	// two command-carrying responses were followed by immediate re-syncs.
	if n := f.syncCount(); n != 3 {
		t.Fatalf("syncs = %d, want 3", n)
	}
	if ds := rec.durations(); len(ds) != 1 {
		t.Fatalf("sleeps = %v, want exactly one", ds)
	}
	waitFor(t, "results", func() bool { _, a := f.resultFor(1); _, b := f.resultFor(2); return a && b })
}

func TestRunBackoffOn500(t *testing.T) {
	f, ts := newFakeServer(t)
	f.script = []syncStep{{status: 500}, {status: 502}, {status: 503}}
	rec := &sleepRecorder{}
	old := sleepFn
	sleepFn = rec.fn
	defer func() { sleepFn = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{ServerURL: ts.URL, DeviceToken: "dt_secret", IntervalS: 30}, "t", testLogger(t))
	}()
	waitFor(t, "four syncs", func() bool { return f.syncCount() >= 4 })
	waitFor(t, "sleep after success", func() bool { return len(rec.durations()) >= 4 })
	cancel()
	<-done
	ds := rec.durations()
	if ds[0] != 5*time.Second || ds[1] != 10*time.Second || ds[2] != 20*time.Second {
		t.Fatalf("backoff = %v, want 5s 10s 20s", ds[:3])
	}
	// The successful fourth sync resets the backoff and sleeps the poll
	// interval (25 s wait leaves 30 - elapsed).
	if ds[3] <= 20*time.Second || ds[3] > 30*time.Second {
		t.Fatalf("post-success sleep = %v, want ~30 s", ds[3])
	}
}

func TestRunBackoffCapsAt300s(t *testing.T) {
	f, ts := newFakeServer(t)
	for i := 0; i < 9; i++ {
		f.script = append(f.script, syncStep{status: 500})
	}
	rec := &sleepRecorder{}
	old := sleepFn
	sleepFn = rec.fn
	defer func() { sleepFn = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{ServerURL: ts.URL, DeviceToken: "dt_secret", IntervalS: 30}, "t", testLogger(t))
	}()
	waitFor(t, "nine failures", func() bool { return len(rec.durations()) >= 9 })
	cancel()
	<-done
	ds := rec.durations()
	want := []time.Duration{5, 10, 20, 40, 80, 160, 300, 300, 300}
	for i, w := range want {
		if ds[i] != w*time.Second {
			t.Fatalf("backoff[%d] = %v, want %v (all: %v)", i, ds[i], w*time.Second, ds[:9])
		}
	}
}

func TestRunSleepsFiveMinutesOn401(t *testing.T) {
	f, ts := newFakeServer(t)
	rec := &sleepRecorder{}
	old := sleepFn
	sleepFn = rec.fn
	defer func() { sleepFn = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	// Wrong token: every sync answers 401.
	go func() {
		done <- Run(ctx, Config{ServerURL: ts.URL, DeviceToken: "dt_revoked", IntervalS: 30}, "t", testLogger(t))
	}()
	waitFor(t, "two 401 sleeps", func() bool { return len(rec.durations()) >= 2 })
	cancel()
	<-done
	ds := rec.durations()
	if ds[0] != 5*time.Minute || ds[1] != 5*time.Minute {
		t.Fatalf("401 sleeps = %v, want 5m each", ds[:2])
	}
	f.mu.Lock()
	n := f.requests
	f.mu.Unlock()
	if n < 2 {
		t.Fatalf("agent must keep retrying after 401, saw %d requests", n)
	}
}

func TestRunResultRetryAndFinalCodes(t *testing.T) {
	f, ts := newFakeServer(t)
	f.resultRC[1] = []int{500}      // one transient failure, then 204
	f.resultRC[2] = []int{409, 500} // final: must stop after the 409
	f.script = []syncStep{
		{200, protocol.SyncResponse{PollIntervalS: 30, Commands: []protocol.Command{
			{ID: 1, Kind: protocol.KindPing, TimeoutS: 5},
			{ID: 2, Kind: protocol.KindPing, TimeoutS: 5},
		}}},
	}
	rec := &sleepRecorder{}
	old := sleepFn
	sleepFn = rec.fn
	defer func() { sleepFn = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{ServerURL: ts.URL, DeviceToken: "dt_secret", IntervalS: 30}, "t", testLogger(t))
	}()
	waitFor(t, "result 1 stored", func() bool { _, ok := f.resultFor(1); return ok })
	waitFor(t, "result 2 attempted", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.resultN[2] >= 1
	})
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	f.mu.Lock()
	n1, n2 := f.resultN[1], f.resultN[2]
	f.mu.Unlock()
	if n1 != 2 {
		t.Fatalf("result 1 posted %d times, want 2 (500 then 204)", n1)
	}
	if n2 != 1 {
		t.Fatalf("result 2 posted %d times, want 1 (409 is final)", n2)
	}
	found := false
	for _, d := range rec.durations() {
		if d == 5*time.Second {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a 5 s retry sleep, got %v", rec.durations())
	}
}

func TestRunConcurrencyLimit(t *testing.T) {
	f, ts := newFakeServer(t)
	var cmds []protocol.Command
	for i := int64(1); i <= 4; i++ {
		cmds = append(cmds, protocol.Command{ID: i, Kind: protocol.KindExec, Argv: []string{"sleep", "0.4"}, TimeoutS: 10})
	}
	f.script = []syncStep{{200, protocol.SyncResponse{PollIntervalS: 30, Commands: cmds}}}
	rec := &sleepRecorder{}
	old := sleepFn
	sleepFn = rec.fn
	defer func() { sleepFn = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- Run(ctx, Config{ServerURL: ts.URL, DeviceToken: "dt_secret", IntervalS: 30}, "t", testLogger(t))
	}()
	waitFor(t, "four results", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.results) == 4
	})
	elapsed := time.Since(start)
	cancel()
	<-done
	// Four 0.4 s commands with a limit of two run in two waves: >= 0.8 s.
	if elapsed < 750*time.Millisecond {
		t.Fatalf("four commands finished in %v; the concurrency limit of 2 was not applied", elapsed)
	}
	// Timestamps: sweep start/finish events and check that no instant had
	// more than two commands running.
	f.mu.Lock()
	type event struct {
		at    time.Time
		delta int
	}
	var events []event
	for _, r := range f.results {
		events = append(events, event{r.StartedAt, +1}, event{r.FinishedAt, -1})
	}
	f.mu.Unlock()
	sort.Slice(events, func(i, j int) bool {
		if events[i].at.Equal(events[j].at) {
			return events[i].delta < events[j].delta // finish before start
		}
		return events[i].at.Before(events[j].at)
	})
	running, peak := 0, 0
	for _, e := range events {
		running += e.delta
		if running > peak {
			peak = running
		}
	}
	if peak > 2 {
		t.Fatalf("%d commands ran at once, limit is 2", peak)
	}
	if peak < 2 {
		t.Fatalf("peak concurrency %d; expected the two permitted slots to be used", peak)
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	err := Run(context.Background(), Config{ServerURL: "https://x"}, "t", nil)
	if err == nil {
		t.Fatal("expected error for config without device token")
	}
	err = Run(context.Background(), Config{ServerURL: "http://10.1.1.1", DeviceToken: "dt_x", IntervalS: 30}, "t", nil)
	if err == nil || !strings.Contains(err.Error(), "insecure-http") {
		t.Fatalf("expected URL policy error, got %v", err)
	}
}

func TestRunReturnsWhenContextCancelled(t *testing.T) {
	_, ts := newFakeServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{ServerURL: ts.URL, DeviceToken: "dt_secret", IntervalS: 30}, "t", testLogger(t))
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestSyncNeverReturnsNilCommands(t *testing.T) {
	f, ts := newFakeServer(t)
	f.script = []syncStep{{200, protocol.SyncResponse{PollIntervalS: 30}}}
	c, err := NewClient(Config{ServerURL: ts.URL, DeviceToken: "dt_secret"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Sync(context.Background(), protocol.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Commands == nil || len(resp.Commands) != 0 {
		t.Fatalf("commands = %#v", resp.Commands)
	}
}
