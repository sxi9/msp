package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/sxi9/msp/internal/protocol"
)

// The command preset library: the entries behind the "Presets…" dropdown of
// the device console. The built-in set lives here (builtinPresets) so the
// server is the single source of truth; MSP_PRESETS points at a JSON file that
// replaces it entirely and is re-read whenever its mtime or size changes, so
// the MSP edits presets without rebuilding or restarting anything.
//
// File format: {"linux":[PRESET...],"windows":[PRESET...]} where PRESET is
// {"label","kind":"exec"|"script","argv":[...] (exec),"shell","body" (script),
// "timeout_s" (optional, 1..3600, default 60)}. Unknown keys are refused. Only
// exec and script are presets: lock, ping and message are actions.

// Preset is one entry of the library as served by GET /api/v1/presets. Argv is
// present for exec, Shell and Body for script; TimeoutS is always filled in.
type Preset struct {
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Argv     []string `json:"argv,omitempty"`
	Shell    string   `json:"shell,omitempty"`
	Body     string   `json:"body,omitempty"`
	TimeoutS int      `json:"timeout_s"`
}

// PresetSet is the per-platform library. Both slices are non-nil so the JSON
// form always carries arrays.
type PresetSet struct {
	Linux   []Preset `json:"linux"`
	Windows []Preset `json:"windows"`
}

// MaxPresetLabel is the longest accepted label (in characters).
const MaxPresetLabel = 60

// presetFile is the on-disk form. TimeoutS is a pointer so that an omitted
// timeout (default 60) and an explicit 0 (an error) can be told apart.
type presetFile struct {
	Linux   []presetEntry `json:"linux"`
	Windows []presetEntry `json:"windows"`
}

type presetEntry struct {
	Label    string   `json:"label"`
	Kind     string   `json:"kind"`
	Argv     []string `json:"argv"`
	Shell    string   `json:"shell"`
	Body     string   `json:"body"`
	TimeoutS *int     `json:"timeout_s"`
}

// builtinPresets is the set served when MSP_PRESETS is unset. It is the list
// the dashboard shipped with before the library became file-based.
func builtinPresets() PresetSet {
	exec := func(label string, argv ...string) Preset {
		return Preset{Label: label, Kind: protocol.KindExec, Argv: argv, TimeoutS: protocol.DefaultTimeoutS}
	}
	script := func(label, shell, body string) Preset {
		return Preset{Label: label, Kind: protocol.KindScript, Shell: shell, Body: body, TimeoutS: protocol.DefaultTimeoutS}
	}
	return PresetSet{
		Linux: []Preset{
			exec("Uptime", "uptime"),
			script("Disk usage", protocol.ShellSh, "df -h\n"),
			script("Top processes", protocol.ShellSh, "ps aux --sort=-%cpu | head -20\n"),
			exec("Reboot", "systemctl", "reboot"),
		},
		Windows: []Preset{
			script("System info", protocol.ShellPowershell, "Get-ComputerInfo | Select-Object OsName,OsUptime\n"),
			script("Drives", protocol.ShellPowershell, "Get-PSDrive -PSProvider FileSystem\n"),
			script("Top processes", protocol.ShellPowershell, "Get-Process | Sort-Object CPU -Descending | Select-Object -First 20\n"),
			exec("Reboot", "shutdown", "/r", "/t", "0"),
		},
	}
}

// LoadPresets reads and validates a presets file. The error names the file,
// the platform, the preset index and label, and the problem, so it can be
// printed as-is at startup or logged on a failed reload.
func LoadPresets(path string) (PresetSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PresetSet{}, fmt.Errorf("presets file %s: %w", path, err)
	}
	set, err := ParsePresets(data)
	if err != nil {
		return PresetSet{}, fmt.Errorf("presets file %s: %w", path, err)
	}
	return set, nil
}

// ParsePresets decodes and validates the JSON form of a presets file.
func ParsePresets(data []byte) (PresetSet, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f presetFile
	if err := dec.Decode(&f); err != nil {
		return PresetSet{}, fmt.Errorf("invalid JSON: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return PresetSet{}, errors.New("invalid JSON: trailing data after the object")
	}
	linux, err := validatePresets(protocol.PlatformLinux, f.Linux)
	if err != nil {
		return PresetSet{}, err
	}
	windows, err := validatePresets(protocol.PlatformWindows, f.Windows)
	if err != nil {
		return PresetSet{}, err
	}
	return PresetSet{Linux: linux, Windows: windows}, nil
}

// validatePresets checks one platform's list and returns its served form:
// labels unique and 1..MaxPresetLabel characters, kind exec or script, the
// shape checked by protocol.Command.Validate, every exec argv element a
// single trimmed non-empty line (the console shows argv one element per line
// and re-parses it that way, so anything else would be sent differently from
// what the file says), the shell one of protocol.ShellsFor(platform), script
// bodies newline-terminated and timeout_s 1..MaxTimeoutS (default
// DefaultTimeoutS when omitted).
func validatePresets(platform string, entries []presetEntry) ([]Preset, error) {
	out := make([]Preset, 0, len(entries))
	seen := make(map[string]int, len(entries))
	for i, e := range entries {
		fail := func(format string, args ...any) error {
			return fmt.Errorf("%s[%d] %q: %s", platform, i, e.Label, fmt.Sprintf(format, args...))
		}
		if n := utf8.RuneCountInString(e.Label); n < 1 || n > MaxPresetLabel || strings.TrimSpace(e.Label) == "" {
			return nil, fail("label must be 1..%d characters", MaxPresetLabel)
		}
		if prev, dup := seen[e.Label]; dup {
			return nil, fail("duplicate label (also %s[%d])", platform, prev)
		}
		seen[e.Label] = i
		switch e.Kind {
		case protocol.KindExec, protocol.KindScript:
		default:
			return nil, fail("kind must be exec or script (lock, ping and message are actions, not presets)")
		}
		timeout := protocol.ClampTimeout(0)
		if e.TimeoutS != nil {
			if *e.TimeoutS < 1 || *e.TimeoutS > protocol.MaxTimeoutS {
				return nil, fail("timeout_s must be 1..%d", protocol.MaxTimeoutS)
			}
			timeout = protocol.ClampTimeout(*e.TimeoutS)
		}
		body := e.Body
		if e.Kind == protocol.KindScript && body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		// The platform check comes first so that any shell that is wrong for
		// this platform gets the platform's own list, not the cross-platform one.
		if e.Kind == protocol.KindScript && !shellAvailable(e.Shell, platform) {
			return nil, fail("shell %s not available on %s (use %s)", e.Shell, platform, strings.Join(protocol.ShellsFor(platform), " or "))
		}
		cmd := protocol.Command{Kind: e.Kind, Argv: e.Argv, Shell: e.Shell, Body: body, TimeoutS: timeout}
		if err := cmd.Validate(); err != nil {
			return nil, fail("%v", err)
		}
		if e.Kind == protocol.KindExec {
			for j, a := range e.Argv {
				if a == "" || strings.ContainsAny(a, "\r\n") || a != strings.TrimSpace(a) {
					return nil, fail("argv[%d] must be a single trimmed non-empty line (the console sends one argument per line)", j)
				}
			}
		}
		p := Preset{Label: e.Label, Kind: e.Kind, TimeoutS: timeout}
		if e.Kind == protocol.KindExec {
			p.Argv = append([]string(nil), e.Argv...)
		} else {
			p.Shell = e.Shell
			p.Body = body
		}
		out = append(out, p)
	}
	return out, nil
}

// shellAvailable reports whether shell is one of protocol.ShellsFor(platform).
func shellAvailable(shell, platform string) bool {
	for _, sh := range protocol.ShellsFor(platform) {
		if sh == shell {
			return true
		}
	}
	return false
}

// presetLibrary serves the current set. With no path it is the builtin set;
// with a path the file is (re)loaded lazily on access whenever its mtime or
// size changed since the last attempt. A failed reload keeps the last good set
// and logs one Warn per changed file version.
type presetLibrary struct {
	path string
	log  *slog.Logger

	mu     sync.Mutex
	set    PresetSet
	loaded bool   // set holds a successfully loaded file
	stamp  string // mtime+size of the last attempt ("" = never tried / stat failed)
	tried  bool   // a load was attempted at least once
}

func newPresetLibrary(path string, log *slog.Logger) *presetLibrary {
	return &presetLibrary{path: path, log: log}
}

// fileStamp is the change marker of the file: mtime (ns) and size.
func (l *presetLibrary) fileStamp() string {
	fi, err := os.Stat(l.path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
}

// load reads the file now (startup). It returns the loader's error so that
// msp-server serve can refuse to start on a bad file.
func (l *presetLibrary) load() error {
	if l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refreshLocked(true)
}

// refreshLocked reloads when the file changed since the last attempt. With
// force the file is read regardless. The error of a reload after a good load
// is logged as a warning and the last good set is kept.
func (l *presetLibrary) refreshLocked(force bool) error {
	stamp := l.fileStamp()
	if !force && l.tried && stamp == l.stamp {
		return nil
	}
	l.tried = true
	l.stamp = stamp
	set, err := LoadPresets(l.path)
	if err != nil {
		if l.loaded {
			l.log.Warn("presets reload failed, keeping the last good set", "err", err)
		}
		return err
	}
	l.set = set
	l.loaded = true
	l.log.Info("presets loaded", "path", l.path, "linux", len(set.Linux), "windows", len(set.Windows))
	return nil
}

// current returns the set to serve, its source ("builtin" or "file") and the
// file path ("" for builtin). With a file that has never loaded successfully
// (only possible when the startup check was skipped) the builtin set is served.
func (l *presetLibrary) current() (set PresetSet, source, path string) {
	if l.path == "" {
		return builtinPresets(), "builtin", ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.refreshLocked(false)
	if !l.loaded {
		if err != nil {
			l.log.Warn("presets file unusable, serving the builtin set", "err", err)
		}
		return builtinPresets(), "builtin", ""
	}
	return l.set, "file", l.path
}

// handleListPresets is GET /api/v1/presets (any role).
func (s *Server) handleListPresets(w http.ResponseWriter, r *http.Request) {
	set, source, path := s.presets.current()
	writeJSON(w, http.StatusOK, map[string]any{"presets": set, "source": source, "path": path})
}
