package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// examplePresetsPath is the template users copy; it must always load.
const examplePresetsPath = "../../deploy/presets.example.json"

func TestPresetsLoaderAcceptsExampleFile(t *testing.T) {
	set, err := LoadPresets(examplePresetsPath)
	if err != nil {
		t.Fatalf("LoadPresets(example): %v", err)
	}
	if len(set.Linux) != 10 || len(set.Windows) != 10 {
		t.Fatalf("want 10 + 10 presets, got %d + %d", len(set.Linux), len(set.Windows))
	}
	// File order is kept, timeouts are filled in, bodies end with a newline.
	wantLinux := []string{"Uptime", "Disk usage", "Top processes", "Updates available", "Install all updates", "Restart a service", "Failed services", "Last 50 log lines", "Logged-in users", "Reboot"}
	for i, p := range set.Linux {
		if p.Label != wantLinux[i] {
			t.Fatalf("linux[%d] label %q, want %q", i, p.Label, wantLinux[i])
		}
	}
	byLabel := func(ps []Preset, label string) Preset {
		for _, p := range ps {
			if p.Label == label {
				return p
			}
		}
		t.Fatalf("preset %q missing", label)
		return Preset{}
	}
	if p := byLabel(set.Linux, "Uptime"); p.Kind != "exec" || strings.Join(p.Argv, " ") != "uptime" || p.TimeoutS != 60 || p.Shell != "" || p.Body != "" {
		t.Fatalf("Uptime %+v", p)
	}
	if p := byLabel(set.Linux, "Install all updates"); p.Kind != "script" || p.Shell != "bash" || p.TimeoutS != 1800 || p.Argv != nil {
		t.Fatalf("Install all updates %+v", p)
	}
	if p := byLabel(set.Windows, "Clear temp files"); p.Shell != "cmd" || p.Body != "del /q /s %TEMP%\\*\r\n" {
		t.Fatalf("Clear temp files %+v", p)
	}
	if p := byLabel(set.Windows, "Group Policy update"); p.Kind != "exec" || p.TimeoutS != 300 {
		t.Fatalf("Group Policy update %+v", p)
	}
	for _, ps := range [][]Preset{set.Linux, set.Windows} {
		for _, p := range ps {
			if p.Kind == "script" && !strings.HasSuffix(p.Body, "\n") {
				t.Fatalf("%q body lacks a trailing newline", p.Label)
			}
			if p.TimeoutS < 1 || p.TimeoutS > protocol.MaxTimeoutS {
				t.Fatalf("%q timeout %d", p.Label, p.TimeoutS)
			}
		}
	}
	// The served JSON form carries only the keys of the kind.
	b, _ := json.Marshal(byLabel(set.Linux, "Uptime"))
	if string(b) != `{"label":"Uptime","kind":"exec","argv":["uptime"],"timeout_s":60}` {
		t.Fatalf("exec JSON %s", b)
	}
	b, _ = json.Marshal(byLabel(set.Linux, "Disk usage"))
	if string(b) != `{"label":"Disk usage","kind":"script","shell":"sh","body":"df -h\n","timeout_s":60}` {
		t.Fatalf("script JSON %s", b)
	}
}

func TestPresetsLoaderRejects(t *testing.T) {
	cases := []struct {
		name, json, want string
	}{
		{"unknown top-level key", `{"linux":[],"android":[]}`, `unknown field "android"`},
		{"unknown preset key", `{"linux":[{"label":"X","kind":"exec","argv":["id"],"comment":"no"}]}`, `unknown field "comment"`},
		{"bad shell for platform", `{"linux":[{"label":"X","kind":"script","shell":"powershell","body":"x\n"}]}`, `linux[0] "X": shell powershell not available on linux`},
		{"bad shell for windows", `{"windows":[{"label":"X","kind":"script","shell":"sh","body":"x\n"}]}`, `windows[0] "X": shell sh not available on windows`},
		{"unknown shell", `{"linux":[{"label":"X","kind":"script","shell":"zsh","body":"x\n"}]}`, `linux[0] "X": shell zsh not available on linux (use sh or bash)`},
		{"unknown shell windows", `{"windows":[{"label":"X","kind":"script","shell":"zsh","body":"x\n"}]}`, `windows[0] "X": shell zsh not available on windows (use powershell or cmd)`},
		{"lock kind", `{"linux":[{"label":"Lock","kind":"lock"}]}`, `linux[0] "Lock": kind must be exec or script`},
		{"message kind", `{"windows":[{"label":"Hi","kind":"message"}]}`, `windows[0] "Hi": kind must be exec or script`},
		{"empty argv", `{"linux":[{"label":"X","kind":"exec","argv":[]}]}`, `linux[0] "X": exec requires argv with at least one element`},
		{"missing argv", `{"linux":[{"label":"X","kind":"exec"}]}`, `exec requires argv`},
		{"argv element with newline", `{"linux":[{"label":"X","kind":"exec","argv":["sh","-c","echo a\necho b"]}]}`, `linux[0] "X": argv[2] must be a single trimmed non-empty line`},
		{"argv element with CR", `{"windows":[{"label":"X","kind":"exec","argv":["cmd","/c","dir\r"]}]}`, `windows[0] "X": argv[2] must be a single trimmed non-empty line`},
		{"empty argv element", `{"linux":[{"label":"X","kind":"exec","argv":["echo",""]}]}`, `linux[0] "X": argv[1] must be a single trimmed non-empty line`},
		{"padded argv element", `{"linux":[{"label":"X","kind":"exec","argv":["echo","  padded  "]}]}`, `linux[0] "X": argv[1] must be a single trimmed non-empty line`},
		{"exec with body", `{"linux":[{"label":"X","kind":"exec","argv":["id"],"body":"x"}]}`, `exec must not set shell or body`},
		{"script with argv", `{"linux":[{"label":"X","kind":"script","shell":"sh","body":"x","argv":["id"]}]}`, `script must not set argv`},
		{"empty body", `{"linux":[{"label":"X","kind":"script","shell":"sh","body":""}]}`, `script body must not be empty`},
		{"duplicate label", `{"linux":[{"label":"Same","kind":"exec","argv":["a"]},{"label":"Same","kind":"exec","argv":["b"]}]}`, `linux[1] "Same": duplicate label (also linux[0])`},
		{"empty label", `{"linux":[{"label":"","kind":"exec","argv":["a"]}]}`, `linux[0] "": label must be 1..60 characters`},
		{"blank label", `{"linux":[{"label":"   ","kind":"exec","argv":["a"]}]}`, `label must be 1..60 characters`},
		{"long label", `{"linux":[{"label":"` + strings.Repeat("x", 61) + `","kind":"exec","argv":["a"]}]}`, `label must be 1..60 characters`},
		{"timeout 0", `{"linux":[{"label":"X","kind":"exec","argv":["a"],"timeout_s":0}]}`, `linux[0] "X": timeout_s must be 1..3600`},
		{"timeout 3601", `{"linux":[{"label":"X","kind":"exec","argv":["a"],"timeout_s":3601}]}`, `timeout_s must be 1..3600`},
		{"timeout negative", `{"linux":[{"label":"X","kind":"exec","argv":["a"],"timeout_s":-5}]}`, `timeout_s must be 1..3600`},
		{"not an object", `[]`, `invalid JSON`},
		{"trailing data", `{"linux":[]} {}`, `trailing data`},
		{"broken json", `{"linux":[`, `invalid JSON`},
	}
	for _, c := range cases {
		_, err := ParsePresets([]byte(c.json))
		if err == nil {
			t.Fatalf("%s: accepted %s", c.name, c.json)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
	// A missing file is named in the error.
	if _, err := LoadPresets("/nonexistent/presets.json"); err == nil || !strings.Contains(err.Error(), "/nonexistent/presets.json") {
		t.Fatalf("missing file: %v", err)
	}
	// A bad file names the file, the platform and the preset.
	bad := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(bad, []byte(`{"windows":[{"label":"Lock","kind":"lock"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPresets(bad); err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), `windows[0] "Lock"`) {
		t.Fatalf("bad file error %v", err)
	}
}

func TestPresetsLoaderNormalises(t *testing.T) {
	set, err := ParsePresets([]byte(`{"linux":[
		{"label":"No newline","kind":"script","shell":"bash","body":"uptime"},
		{"label":"With timeout","kind":"exec","argv":["sleep","1"],"timeout_s":5},
		{"label":"Max","kind":"exec","argv":["x"],"timeout_s":3600},
		{"label":"Min","kind":"exec","argv":["x"],"timeout_s":1}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if set.Linux[0].Body != "uptime\n" {
		t.Fatalf("body %q, want newline appended", set.Linux[0].Body)
	}
	if set.Linux[0].TimeoutS != 60 || set.Linux[1].TimeoutS != 5 || set.Linux[2].TimeoutS != 3600 || set.Linux[3].TimeoutS != 1 {
		t.Fatalf("timeouts %+v", set.Linux)
	}
	// Windows was absent: an empty array, never null.
	if set.Windows == nil || len(set.Windows) != 0 {
		t.Fatalf("windows %#v", set.Windows)
	}
	if b, _ := json.Marshal(set); !strings.Contains(string(b), `"windows":[]`) {
		t.Fatalf("set JSON %s", b)
	}
	// An empty object is a valid (empty) library.
	if set, err := ParsePresets([]byte(`{}`)); err != nil || len(set.Linux) != 0 || len(set.Windows) != 0 {
		t.Fatalf("empty object: %+v %v", set, err)
	}
}

// presetsResponse is the body of GET /api/v1/presets.
type presetsResponse struct {
	Presets PresetSet `json:"presets"`
	Source  string    `json:"source"`
	Path    string    `json:"path"`
}

func getPresets(e *testEnv, c *http.Cookie) presetsResponse {
	e.t.Helper()
	resp, body := e.do(http.MethodGet, "/api/v1/presets", nil, withCookie(c, nil))
	if resp.StatusCode != 200 {
		e.t.Fatalf("GET presets: %d %s", resp.StatusCode, body)
	}
	var out presetsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		e.t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func TestPresetsEndpointBuiltin(t *testing.T) {
	e := newTestEnv(t)

	if resp, body := e.do(http.MethodGet, "/api/v1/presets", nil, nil); resp.StatusCode != 401 || errorCode(t, body) != "unauthorized" {
		t.Fatalf("unauthenticated: %d %s", resp.StatusCode, body)
	}
	// Any role may read the library.
	viewer := dashUser(e, "viewer@example.com", RoleViewer)
	got := getPresets(e, viewer)
	if got.Source != "builtin" || got.Path != "" {
		t.Fatalf("source %q path %q", got.Source, got.Path)
	}
	if len(got.Presets.Linux) != 4 || len(got.Presets.Windows) != 4 {
		t.Fatalf("builtin counts %d/%d", len(got.Presets.Linux), len(got.Presets.Windows))
	}
	if got.Presets.Linux[0].Label != "Uptime" || got.Presets.Linux[3].Label != "Reboot" || got.Presets.Windows[0].Label != "System info" {
		t.Fatalf("builtin labels %+v %+v", got.Presets.Linux, got.Presets.Windows)
	}
	for _, p := range append(got.Presets.Linux, got.Presets.Windows...) {
		if p.TimeoutS != 60 {
			t.Fatalf("%q timeout %d", p.Label, p.TimeoutS)
		}
		if p.Kind != "exec" && p.Kind != "script" {
			t.Fatalf("%q kind %q", p.Label, p.Kind)
		}
	}
	// The builtin set is itself a valid library (same rules as a file).
	b, _ := json.Marshal(builtinPresets())
	if _, err := ParsePresets(b); err != nil {
		t.Fatalf("builtin set fails the loader: %v", err)
	}
}

// bumpMtime moves a file's mtime forward by a second so a rewrite within the
// same instant still counts as a change.
func bumpMtime(t *testing.T, path string, by time.Duration) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := fi.ModTime().Add(by)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

func TestPresetsEndpointFileAndReload(t *testing.T) {
	e := newTestEnv(t)
	admin := dashAdmin(e)
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))

	path := filepath.Join(t.TempDir(), "presets.json")
	example, err := os.ReadFile(examplePresetsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, example, 0o644); err != nil {
		t.Fatal(err)
	}
	e.srv.presets = newPresetLibrary(path, log)
	if err := e.srv.presets.load(); err != nil {
		t.Fatalf("startup load: %v", err)
	}

	got := getPresets(e, admin)
	if got.Source != "file" || got.Path != path {
		t.Fatalf("source %q path %q", got.Source, got.Path)
	}
	if len(got.Presets.Linux) != 10 || got.Presets.Linux[3].Label != "Updates available" || got.Presets.Linux[3].TimeoutS != 300 {
		t.Fatalf("file content %+v", got.Presets.Linux)
	}

	// A rewrite with a newer mtime is picked up on the next GET.
	if err := os.WriteFile(path, []byte(`{"linux":[{"label":"Only one","kind":"exec","argv":["true"],"timeout_s":7}],"windows":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, path, 2*time.Second)
	got = getPresets(e, admin)
	if len(got.Presets.Linux) != 1 || got.Presets.Linux[0].Label != "Only one" || got.Presets.Linux[0].TimeoutS != 7 || len(got.Presets.Windows) != 0 {
		t.Fatalf("after reload %+v", got.Presets)
	}
	if !strings.Contains(logBuf.String(), "presets loaded") {
		t.Fatalf("log %s", logBuf.String())
	}

	// An invalid rewrite keeps the last good set and logs one warning naming the problem.
	logBuf.Reset()
	if err := os.WriteFile(path, []byte(`{"linux":[{"label":"Bad","kind":"lock"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, path, 4*time.Second)
	got = getPresets(e, admin)
	if got.Source != "file" || len(got.Presets.Linux) != 1 || got.Presets.Linux[0].Label != "Only one" {
		t.Fatalf("after bad rewrite %+v (%s)", got.Presets, got.Source)
	}
	warn := logBuf.String()
	if !strings.Contains(warn, "level=WARN") || !strings.Contains(warn, "keeping the last good set") || !strings.Contains(warn, path) || !strings.Contains(warn, `linux[0] \"Bad\": kind must be exec or script`) {
		t.Fatalf("warning log %q", warn)
	}
	// The same bad version is not re-logged on every GET.
	logBuf.Reset()
	getPresets(e, admin)
	if strings.Contains(logBuf.String(), "WARN") {
		t.Fatalf("re-warned without a change: %s", logBuf.String())
	}

	// A deleted file is handled the same way; a fixed file loads again.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got = getPresets(e, admin); got.Source != "file" || got.Presets.Linux[0].Label != "Only one" {
		t.Fatalf("after delete %+v", got.Presets)
	}
	if err := os.WriteFile(path, example, 0o644); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, path, 6*time.Second)
	if got = getPresets(e, admin); len(got.Presets.Linux) != 10 || len(got.Presets.Windows) != 10 {
		t.Fatalf("after restore %d/%d", len(got.Presets.Linux), len(got.Presets.Windows))
	}

	// Run refuses to start on a bad file (what msp-server serve exits on).
	badPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badPath, []byte(`{"linux":[{"label":"X","kind":"exec","argv":[]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Listen = "127.0.0.1:0"
	cfg.PresetsPath = badPath
	srv := New(e.st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = srv.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), badPath) || !strings.Contains(err.Error(), `linux[0] "X": exec requires argv`) {
		t.Fatalf("Run with a bad presets file: %v", err)
	}
	// Without the file the library has never loaded, so the builtin set is served.
	e.srv.presets = newPresetLibrary(badPath, log)
	if got = getPresets(e, admin); got.Source != "builtin" || len(got.Presets.Linux) != 4 {
		t.Fatalf("unloaded file: %s %+v", got.Source, got.Presets)
	}
}
