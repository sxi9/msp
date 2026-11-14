package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sxi9/msp/internal/protocol"
)

// The agent release manifest (AU1): the server advertises a newer agent to
// desktops and fills an update command's version/url/sha256 from it. The MSP
// publishes the agent binaries to MSP_AGENT_DIR (served at /dl/) together with a
// manifest file agents.json; this file loads and validates it, and re-reads it
// when its mtime or size changes, exactly like the preset library (presets.go).
//
// File format:
//
//	{"version":"0.2.0","files":{
//	   "linux-amd64":{"name":"msp-agent-linux-amd64","sha256":"<hex>"},
//	   "linux-arm64":{"name":"msp-agent-linux-arm64","sha256":"<hex>"},
//	   "windows-amd64":{"name":"msp-agent-windows-amd64.exe","sha256":"<hex>"}}}
//
// version is the single released version for every file. Each entry's name is the
// file served at /dl/<name> (a bare filename: no "/", "\", ".." or leading "."),
// and sha256 is the binary's SHA-256 hex digest (the agent re-verifies it after
// download). If agents.json is absent, self-update is simply off: no update is
// ever advertised and ReleasedVersion returns "".

// ManifestFileName is the manifest's fixed basename inside MSP_AGENT_DIR.
const ManifestFileName = "agents.json"

// ManifestFile is one platform-arch entry of the manifest as it is stored on
// disk and served to callers.
type ManifestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Manifest is the parsed, validated agents.json. Version is the single released
// version; Files maps a "<platform>-<arch>" key (e.g. "linux-amd64") to its file.
type Manifest struct {
	Version string                  `json:"version"`
	Files   map[string]ManifestFile `json:"files"`
}

// ManifestEntry is what ManifestForPlatform returns for one platform-arch: the
// released version together with the file's name and hash.
type ManifestEntry struct {
	Version string
	Name    string
	SHA256  string
}

// manifestEnvelope is the on-disk form, decoded with DisallowUnknownFields so a
// typo in agents.json is rejected rather than silently ignored.
type manifestEnvelope struct {
	Version string                  `json:"version"`
	Files   map[string]ManifestFile `json:"files"`
}

// ParseManifest decodes and validates the JSON form of agents.json. The error
// names the offending platform key and the problem, so it can be printed as-is
// at startup or logged on a failed reload.
func ParseManifest(data []byte) (Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var e manifestEnvelope
	if err := dec.Decode(&e); err != nil {
		return Manifest{}, fmt.Errorf("invalid JSON: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Manifest{}, errors.New("invalid JSON: trailing data after the object")
	}
	if !protocol.ValidAgentVersion(e.Version) {
		return Manifest{}, fmt.Errorf("version must be 1..%d bytes of [A-Za-z0-9.+_-]", protocol.MaxAgentVersionLen)
	}
	for key, f := range e.Files {
		if key == "" {
			return Manifest{}, errors.New("files has an empty platform-arch key")
		}
		if !validManifestFileName(f.Name) {
			return Manifest{}, fmt.Errorf("files[%q].name %q must be a bare filename (no /, \\, .. or leading .)", key, f.Name)
		}
		if !protocol.ValidSHA256Hex(f.SHA256) {
			return Manifest{}, fmt.Errorf("files[%q].sha256 must be %d hex characters", key, protocol.SHA256HexLen)
		}
	}
	return Manifest{Version: e.Version, Files: e.Files}, nil
}

// validManifestFileName mirrors handleDownload's name policy: a non-empty bare
// filename with no path separators, no "..", and not starting with ".".
func validManifestFileName(name string) bool {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") ||
		strings.Contains(name, "..") || strings.HasPrefix(name, ".") {
		return false
	}
	return true
}

// LoadManifest reads and validates agents.json at path. The error names the file
// and the problem.
func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest file %s: %w", path, err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest file %s: %w", path, err)
	}
	return m, nil
}

// manifestLibrary serves the current manifest. dir is MSP_AGENT_DIR ("" = agent
// serving disabled, so self-update is off); the file agents.json inside it is
// (re)loaded lazily on access whenever its mtime or size changed since the last
// attempt. A missing file is not an error (self-update off); a present but
// malformed file keeps the last good manifest (if any) and logs one Warn per
// changed file version. This mirrors presetLibrary in presets.go.
type manifestLibrary struct {
	dir string
	log *slog.Logger

	mu       sync.Mutex
	manifest Manifest
	loaded   bool   // manifest holds a successfully loaded file
	stamp    string // mtime+size of the last attempt ("" = absent / never tried)
	tried    bool   // a load was attempted at least once
}

func newManifestLibrary(dir string, log *slog.Logger) *manifestLibrary {
	return &manifestLibrary{dir: dir, log: log}
}

// path is the manifest file's full path, or "" when agent serving is disabled.
func (l *manifestLibrary) path() string {
	if l.dir == "" {
		return ""
	}
	return filepath.Join(l.dir, ManifestFileName)
}

// fileStamp is the change marker of the file: mtime (ns) and size; "" when the
// file is absent or cannot be stat'd (self-update off).
func (l *manifestLibrary) fileStamp() string {
	p := l.path()
	if p == "" {
		return ""
	}
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
}

// load reads the file now (startup). A missing file is not an error (self-update
// is simply off); a present but malformed file returns its error so msp-server
// can log it. Unlike presets, a bad manifest need not stop the server: the safe
// default is self-update off.
func (l *manifestLibrary) load() error {
	if l.dir == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refreshLocked(true)
}

// refreshLocked reloads when the file changed since the last attempt. With force
// the file is read regardless. A missing file clears nothing and is no error; a
// reload error after a good load is logged and the last good manifest is kept.
func (l *manifestLibrary) refreshLocked(force bool) error {
	p := l.path()
	if p == "" {
		return nil
	}
	stamp := l.fileStamp()
	if !force && l.tried && stamp == l.stamp {
		return nil
	}
	l.tried = true
	l.stamp = stamp
	if stamp == "" {
		// The file is absent (or unreadable): self-update is off. Drop any
		// previously loaded manifest so a removed file stops advertising.
		if l.loaded {
			l.log.Info("agent manifest removed, self-update disabled", "path", p)
		}
		l.manifest = Manifest{}
		l.loaded = false
		return nil
	}
	m, err := LoadManifest(p)
	if err != nil {
		if l.loaded {
			l.log.Warn("agent manifest reload failed, keeping the last good manifest", "err", err)
		} else {
			l.log.Warn("agent manifest unusable, self-update disabled", "err", err)
		}
		return err
	}
	l.manifest = m
	l.loaded = true
	l.log.Info("agent manifest loaded", "path", p, "version", m.Version, "files", len(m.Files))
	return nil
}

// current returns the loaded manifest and whether self-update is available (a
// manifest has loaded successfully). It refreshes from disk when the file
// changed.
func (l *manifestLibrary) current() (Manifest, bool) {
	if l.dir == "" {
		return Manifest{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshLocked(false)
	if !l.loaded {
		return Manifest{}, false
	}
	return l.manifest, true
}

// ManifestForPlatform returns the release entry for a "<platform>-<arch>" key
// (e.g. "linux-amd64"), and false when self-update is off or the manifest has no
// entry for that platform-arch.
func (l *manifestLibrary) ManifestForPlatform(platformArch string) (ManifestEntry, bool) {
	m, ok := l.current()
	if !ok {
		return ManifestEntry{}, false
	}
	f, ok := m.Files[platformArch]
	if !ok {
		return ManifestEntry{}, false
	}
	return ManifestEntry{Version: m.Version, Name: f.Name, SHA256: f.SHA256}, true
}

// ReleasedVersion returns the current released agent version, or "" when
// self-update is off (no usable manifest).
func (l *manifestLibrary) ReleasedVersion() string {
	m, ok := l.current()
	if !ok {
		return ""
	}
	return m.Version
}
