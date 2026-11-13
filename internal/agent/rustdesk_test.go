//go:build !windows

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// stubRustdesk writes a fake rustdesk client script into dir that records
// every invocation (one line per call) in dir/calls and answers --get-id
// with id. An empty id makes --get-id print a log line instead.
func stubRustdesk(t *testing.T, dir, id string) string {
	t.Helper()
	bin := filepath.Join(dir, "rustdesk")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> \"" + filepath.Join(dir, "calls") + "\"\n" +
		"case \"$1\" in\n" +
		"  --get-id) " + func() string {
		if id == "" {
			return "echo 'INFO rustdesk: no id yet'"
		}
		return "echo 'INFO starting'; echo '" + id + "'"
	}() + " ;;\n" +
		"  --password) [ -n \"$2\" ] || exit 1 ;;\n" +
		"  --config) [ -n \"$2\" ] || exit 1 ;;\n" +
		"  *) echo \"unknown flag $1\" >&2; exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func stubCalls(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// useRustdeskStubs points the package at a stub binary, a temp config dir
// and a recording restart hook for the duration of the test.
func useRustdeskStubs(t *testing.T, bin string, cfgDirs []string) *[]string {
	t.Helper()
	oldBin, oldDirs, oldRestart := rustdeskBin, rustdeskConfigDirs, rustdeskRestart
	t.Cleanup(func() { rustdeskBin, rustdeskConfigDirs, rustdeskRestart = oldBin, oldDirs, oldRestart })
	rustdeskBin = bin
	rustdeskConfigDirs = func() []string { return cfgDirs }
	restarts := &[]string{}
	rustdeskRestart = func(ctx context.Context) error {
		*restarts = append(*restarts, "restart")
		return nil
	}
	return restarts
}

func TestRustdeskIDFromCLIAndTOML(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	ctx := context.Background()

	// Not installed, no config file: empty (and Facts must not fail).
	useRustdeskStubs(t, filepath.Join(dir, "missing"), []string{cfgDir})
	if got := rustdeskID(ctx); got != "" {
		t.Fatalf("not installed: %q", got)
	}
	if f := newCollector().Facts(); f.RustdeskID != "" {
		t.Fatalf("facts without rustdesk: %q", f.RustdeskID)
	}

	// Installed: the CLI answer wins, even with a log line before the id.
	bin := stubRustdesk(t, dir, "123456789")
	useRustdeskStubs(t, bin, []string{cfgDir})
	if got := rustdeskID(ctx); got != "123456789" {
		t.Fatalf("cli id: %q", got)
	}
	if f := newCollector().Facts(); f.RustdeskID != "123456789" {
		t.Fatalf("facts rustdesk_id: %q", f.RustdeskID)
	}
	if calls := stubCalls(t, dir); len(calls) < 1 || calls[0] != "--get-id" {
		t.Fatalf("calls %v", calls)
	}

	// CLI prints no id: fall back to RustDesk.toml in the config dir.
	bin = stubRustdesk(t, dir, "")
	useRustdeskStubs(t, bin, []string{filepath.Join(dir, "nope"), cfgDir})
	if got := rustdeskID(ctx); got != "" {
		t.Fatalf("no toml yet: %q", got)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := "id = '987654321'\nenc_id = 'abc'\npassword = 'xx'\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "RustDesk.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := rustdeskID(ctx); got != "987654321" {
		t.Fatalf("toml id: %q", got)
	}
	// Garbage in the file is never reported.
	if err := os.WriteFile(filepath.Join(cfgDir, "RustDesk.toml"), []byte("id = \"12ab\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := rustdeskID(ctx); got != "" {
		t.Fatalf("garbage toml id: %q", got)
	}
	// A CLI that prints something that is not an id is ignored too.
	for _, out := range []string{"", "hello", "123\nabc", "12345678901234567890123456789012345"} {
		if got := lastDigitsLine(out); got != "" {
			t.Errorf("lastDigitsLine(%q) = %q", out, got)
		}
	}
	if got := lastDigitsLine("x\n 42 \n\n"); got != "42" {
		t.Fatalf("lastDigitsLine trailing blank = %q", got)
	}
}

func TestMergeRustdeskOptions(t *testing.T) {
	opts := rustdeskOptions("rmm.example.com", "KEY=")
	want := "[options]\ncustom-rendezvous-server = 'rmm.example.com'\nrelay-server = 'rmm.example.com'\nkey = 'KEY='\n"
	if got := mergeRustdeskOptions("", opts); got != want {
		t.Fatalf("empty:\n%s", got)
	}
	existing := "[other]\nfoo = 'bar'\n\n[options]\nkey = 'OLD'\napi-server = 'https://api'\ncustom-rendezvous-server = 'old.example'\n\n[last]\nx = 1\n"
	got := mergeRustdeskOptions(existing, opts)
	for _, must := range []string{"[other]\nfoo = 'bar'", "api-server = 'https://api'", "[last]\nx = 1", "relay-server = 'rmm.example.com'", "key = 'KEY='"} {
		if !strings.Contains(got, must) {
			t.Errorf("merged lacks %q:\n%s", must, got)
		}
	}
	for _, mustNot := range []string{"OLD", "old.example"} {
		if strings.Contains(got, mustNot) {
			t.Errorf("merged keeps %q:\n%s", mustNot, got)
		}
	}
	if strings.Count(got, "[options]") != 1 || strings.Count(got, "key = ") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("merged shape:\n%s", got)
	}
	// Idempotent.
	if again := mergeRustdeskOptions(got, opts); again != got {
		t.Fatalf("not idempotent:\n%s\n---\n%s", got, again)
	}
	// A file without [options] gets the table appended after a blank line.
	if got := mergeRustdeskOptions("[a]\nb = 1\n", opts); got != "[a]\nb = 1\n\n[options]\ncustom-rendezvous-server = 'rmm.example.com'\nrelay-server = 'rmm.example.com'\nkey = 'KEY='\n" {
		t.Fatalf("appended:\n%s", got)
	}
}

func TestRustdeskSetup(t *testing.T) {
	dir := t.TempDir()
	cfgDir1 := filepath.Join(dir, "rd1")
	cfgDir2 := filepath.Join(dir, "rd2")
	agentDir := filepath.Join(dir, "agent")
	cfgPath := filepath.Join(agentDir, "agent.json")
	ctx := context.Background()

	// Agent config without a relay: refused before touching the client.
	if err := Save(cfgPath, Config{ServerURL: "https://rmm.example.com", DeviceToken: "dt_x", DeviceID: 1, IntervalS: 30}); err != nil {
		t.Fatal(err)
	}
	bin := stubRustdesk(t, dir, "123456789")
	restarts := useRustdeskStubs(t, bin, []string{cfgDir1, cfgDir2})
	if _, err := RustdeskSetup(ctx, RustdeskSetupOptions{ConfigPath: cfgPath}); !errors.Is(err, ErrRustdeskNotConfigured) {
		t.Fatalf("no relay: %v", err)
	}
	if calls := stubCalls(t, dir); len(calls) != 0 {
		t.Fatalf("client called without a relay: %v", calls)
	}

	// A relay edited into agent.json with TOML/shell metacharacters (or a
	// port: the relay would then point at the ID-server port) is refused
	// before anything is written or the client is called.
	for _, bad := range []Config{
		{ServerURL: "https://rmm.example.com", DeviceToken: "dt_x", DeviceID: 1, IntervalS: 30, RustdeskHost: "x.example'\nkey = 'other'", RustdeskKey: "KEY="},
		{ServerURL: "https://rmm.example.com", DeviceToken: "dt_x", DeviceID: 1, IntervalS: 30, RustdeskHost: "rmm.example.com:21116", RustdeskKey: "KEY="},
		{ServerURL: "https://rmm.example.com", DeviceToken: "dt_x", DeviceID: 1, IntervalS: 30, RustdeskHost: "rmm.example.com", RustdeskKey: "KEY='\napi-server = 'https://evil'"},
	} {
		if err := Save(cfgPath, bad); err != nil {
			t.Fatal(err)
		}
		if _, err := RustdeskSetup(ctx, RustdeskSetupOptions{ConfigPath: cfgPath}); err == nil || !strings.Contains(err.Error(), "unusable") {
			t.Fatalf("malformed relay %q: %v", bad.RustdeskHost, err)
		}
		if calls := stubCalls(t, dir); len(calls) != 0 {
			t.Fatalf("client called with a malformed relay: %v", calls)
		}
		if _, err := os.Stat(filepath.Join(cfgDir1, "RustDesk2.toml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("RustDesk2.toml written with a malformed relay (%v)", err)
		}
	}

	// Relay present but no client installed.
	cfg := Config{ServerURL: "https://rmm.example.com", DeviceToken: "dt_x", DeviceID: 1, IntervalS: 30, RustdeskHost: "rmm.example.com", RustdeskKey: "KEY="}
	if err := Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	rustdeskBin = filepath.Join(dir, "missing")
	if _, err := RustdeskSetup(ctx, RustdeskSetupOptions{ConfigPath: cfgPath}); !errors.Is(err, ErrRustdeskNotInstalled) {
		t.Fatalf("not installed: %v", err)
	}
	rustdeskBin = bin

	// Happy path with an explicit password.
	res, err := RustdeskSetup(ctx, RustdeskSetupOptions{ConfigPath: cfgPath, Password: "Secret-Pass-1"})
	if err != nil {
		t.Fatalf("setup: %v (warnings %v)", err, res.Warnings)
	}
	if res.ID != "123456789" || res.Password != "Secret-Pass-1" || res.Host != "rmm.example.com" || res.Binary != bin || len(res.Warnings) != 0 {
		t.Fatalf("result %+v", res)
	}
	if calls := stubCalls(t, dir); strings.Join(calls, "|") != "--password Secret-Pass-1|--get-id" {
		t.Fatalf("calls %v", calls)
	}
	if len(*restarts) != 1 {
		t.Fatalf("restarts %v", *restarts)
	}
	if len(res.ConfigFiles) != 2 {
		t.Fatalf("config files %v", res.ConfigFiles)
	}
	for _, p := range res.ConfigFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		want := "[options]\ncustom-rendezvous-server = 'rmm.example.com'\nrelay-server = 'rmm.example.com'\nkey = 'KEY='\n"
		if string(b) != want {
			t.Fatalf("%s:\n%s", p, b)
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", p, st.Mode())
		}
	}
	if res.PasswordFile != filepath.Join(agentDir, "rustdesk-password") {
		t.Fatalf("password file %s", res.PasswordFile)
	}
	b, err := os.ReadFile(res.PasswordFile)
	if err != nil || string(b) != "Secret-Pass-1\n" {
		t.Fatalf("password file content %q (%v)", b, err)
	}
	if st, _ := os.Stat(res.PasswordFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode %v", st.Mode())
	}

	// Generated password: 16 chars from the alphabet, never sent anywhere
	// but the client and the file; an existing RustDesk2.toml keeps its
	// other keys.
	os.Remove(filepath.Join(dir, "calls"))
	if err := os.WriteFile(filepath.Join(cfgDir1, "RustDesk2.toml"), []byte("[options]\napi-server = 'https://api.example'\nkey = 'stale'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = RustdeskSetup(ctx, RustdeskSetupOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("setup generated: %v", err)
	}
	if len(res.Password) != rustdeskPasswordLen {
		t.Fatalf("generated password %q", res.Password)
	}
	for _, c := range res.Password {
		if !strings.ContainsRune(passwordAlphabet, c) {
			t.Fatalf("password char %q outside the alphabet", c)
		}
	}
	if calls := stubCalls(t, dir); len(calls) != 2 || calls[0] != "--password "+res.Password || calls[1] != "--get-id" {
		t.Fatalf("calls %v", calls)
	}
	b, _ = os.ReadFile(filepath.Join(cfgDir1, "RustDesk2.toml"))
	if !strings.Contains(string(b), "api-server = 'https://api.example'") || strings.Contains(string(b), "stale") || strings.Count(string(b), "key = ") != 1 {
		t.Fatalf("merged RustDesk2.toml:\n%s", b)
	}
	b, _ = os.ReadFile(res.PasswordFile)
	if string(b) != res.Password+"\n" {
		t.Fatalf("password file %q", b)
	}
	if p1, p2 := res.Password, func() string { p, _ := newRustdeskPassword(); return p }(); p1 == p2 {
		t.Fatal("two generated passwords must differ")
	}

	// --config-string uses the documented `rustdesk --config` instead of the TOML write.
	os.Remove(filepath.Join(dir, "calls"))
	res, err = RustdeskSetup(ctx, RustdeskSetupOptions{ConfigPath: cfgPath, Password: "pw", ConfigString: "EXPORTED"})
	if err != nil {
		t.Fatalf("setup with config string: %v", err)
	}
	if calls := stubCalls(t, dir); strings.Join(calls, "|") != "--config EXPORTED|--password pw|--get-id" {
		t.Fatalf("calls %v", calls)
	}
	if len(res.ConfigFiles) != 0 {
		t.Fatalf("config files written with a config string: %v", res.ConfigFiles)
	}

	// A failing restart is a warning, a failing --password is an error.
	rustdeskRestart = func(ctx context.Context) error { return errors.New("systemctl: unit not found") }
	res, err = RustdeskSetup(ctx, RustdeskSetupOptions{ConfigPath: cfgPath, Password: "pw"})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "unit not found") {
		t.Fatalf("restart failure: %v %v", err, res.Warnings)
	}
	bad := filepath.Join(dir, "rustdesk-bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho 'boom' >&2; exit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rustdeskBin = bad
	if _, err := RustdeskSetup(ctx, RustdeskSetupOptions{ConfigPath: cfgPath, Password: "pw"}); err == nil || !strings.Contains(err.Error(), "--password") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("password failure: %v", err)
	}
}

func TestConfigSetRustdeskAndStatus(t *testing.T) {
	c := Config{ServerURL: "https://x", DeviceToken: "dt", DeviceID: 1, IntervalS: 30}
	if c.setRustdesk(nil) {
		t.Fatal("nil must not change the config")
	}
	if !c.setRustdesk(&protocol.Rustdesk{Host: "h", Key: "k"}) || c.RustdeskHost != "h" || c.RustdeskKey != "k" {
		t.Fatalf("set: %+v", c)
	}
	if c.setRustdesk(&protocol.Rustdesk{Host: "h", Key: "k"}) {
		t.Fatal("same values must report no change")
	}
	if c.setRustdesk(nil) || c.RustdeskHost != "h" {
		t.Fatal("nil must keep the stored relay")
	}
	if !c.setRustdesk(&protocol.Rustdesk{Host: "h2", Key: "k"}) || c.RustdeskHost != "h2" {
		t.Fatal("changed host must be stored")
	}
	// A malformed block from the server never reaches the config.
	for _, bad := range []protocol.Rustdesk{{Host: "h3:21116", Key: "k"}, {Host: "h3 -k evil", Key: "k"}, {Host: "h3", Key: "k'\nx = 'y'"}, {Host: "", Key: "k"}, {Host: "h3", Key: ""}} {
		if c.setRustdesk(&bad) || c.RustdeskHost != "h2" || c.RustdeskKey != "k" {
			t.Fatalf("malformed relay %+v stored: %+v", bad, c)
		}
	}

	// Round trip through the file; Path is set by Load and not stored.
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"rustdesk_host": "h2"`) || strings.Contains(string(b), `"Path"`) || strings.Contains(string(b), `"path"`) {
		t.Fatalf("saved config:\n%s", b)
	}
	got, err := Load(path)
	if err != nil || got.Path != path || got.RustdeskHost != "h2" || got.RustdeskKey != "k" {
		t.Fatalf("loaded %+v (%v)", got, err)
	}
	s, err := Status(path)
	if err != nil || !strings.Contains(s, "rustdesk:  h2\n") {
		t.Fatalf("status %q (%v)", s, err)
	}
	// Without a relay the key is absent from the file and from status.
	if err := Save(path, Config{ServerURL: "https://x", DeviceToken: "dt", DeviceID: 1, IntervalS: 30}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "rustdesk") {
		t.Fatalf("saved config without relay:\n%s", b)
	}
	if s, _ := Status(path); strings.Contains(s, "rustdesk") {
		t.Fatalf("status without relay %q", s)
	}
}

func TestRunSavesRustdeskRelayFromSync(t *testing.T) {
	f, ts := newFakeServer(t)
	relay := &protocol.Rustdesk{Host: "rmm.example.com", Key: "KEY="}
	f.script = []syncStep{
		{200, protocol.SyncResponse{PollIntervalS: 5, Commands: []protocol.Command{}}},                  // no relay: nothing written
		{200, protocol.SyncResponse{PollIntervalS: 5, Commands: []protocol.Command{}, Rustdesk: relay}}, // relay appears: saved
		{200, protocol.SyncResponse{PollIntervalS: 5, Commands: []protocol.Command{}, Rustdesk: relay}}, // unchanged: not rewritten
		{200, protocol.SyncResponse{PollIntervalS: 5, Commands: []protocol.Command{}}},                  // omitted: kept
		{200, protocol.SyncResponse{PollIntervalS: 5, Commands: []protocol.Command{}, Rustdesk: &protocol.Rustdesk{Host: "rmm2.example.com", Key: "KEY2="}}},
	}
	old := sleepFn
	sleepFn = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	defer func() { sleepFn = old }()

	path := filepath.Join(t.TempDir(), "agent.json")
	cfg := Config{ServerURL: ts.URL, DeviceToken: "dt_secret", DeviceID: 42, IntervalS: 10}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st0, _ := os.Stat(path)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, "t", testLogger(t)) }()
	waitFor(t, "six syncs", func() bool { return f.syncCount() >= 6 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RustdeskHost != "rmm2.example.com" || got.RustdeskKey != "KEY2=" || got.DeviceToken != "dt_secret" || got.IntervalS != 10 {
		t.Fatalf("saved config %+v", got)
	}
	st1, _ := os.Stat(path)
	if st1.Mode().Perm() != 0o600 || st0.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v -> %v", st0.Mode(), st1.Mode())
	}
}

func TestRunWithoutConfigPathDoesNotWrite(t *testing.T) {
	f, ts := newFakeServer(t)
	f.script = []syncStep{{200, protocol.SyncResponse{PollIntervalS: 5, Commands: []protocol.Command{}, Rustdesk: &protocol.Rustdesk{Host: "h", Key: "k"}}}}
	old := sleepFn
	sleepFn = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	defer func() { sleepFn = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	// A config built in memory (tests, e2e) has no Path: the relay is kept
	// in memory only and nothing is written anywhere.
	go func() {
		done <- Run(ctx, Config{ServerURL: ts.URL, DeviceToken: "dt_secret", IntervalS: 10}, "t", testLogger(t))
	}()
	waitFor(t, "two syncs", func() bool { return f.syncCount() >= 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v", err)
	}
}
