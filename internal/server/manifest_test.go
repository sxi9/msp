package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// sampleManifest is a well-formed agents.json with one released version and the
// three published desktop files (AU1).
func sampleManifest() string {
	hex := strings.Repeat("a", protocol.SHA256HexLen)
	return `{"version":"0.2.0","files":{
	   "linux-amd64":{"name":"msp-agent-linux-amd64","sha256":"` + hex + `"},
	   "linux-arm64":{"name":"msp-agent-linux-arm64","sha256":"` + hex + `"},
	   "windows-amd64":{"name":"msp-agent-windows-amd64.exe","sha256":"` + hex + `"}}}`
}

func TestParseManifestAcceptsSample(t *testing.T) {
	m, err := ParseManifest([]byte(sampleManifest()))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Version != "0.2.0" || len(m.Files) != 3 {
		t.Fatalf("manifest: %+v", m)
	}
	f, ok := m.Files["windows-amd64"]
	if !ok || f.Name != "msp-agent-windows-amd64.exe" || !protocol.ValidSHA256Hex(f.SHA256) {
		t.Fatalf("windows entry: %+v ok=%v", f, ok)
	}
}

func TestParseManifestRejects(t *testing.T) {
	hex := strings.Repeat("a", protocol.SHA256HexLen)
	bad := []struct {
		data string
		want string
	}{
		{`{"version":"","files":{}}`, "version must be"},
		{`{"version":"0.2 .0","files":{}}`, "version must be"},
		{`{"version":"0.2.0","files":{"linux-amd64":{"name":"","sha256":"` + hex + `"}}}`, "must be a bare filename"},
		{`{"version":"0.2.0","files":{"linux-amd64":{"name":"../evil","sha256":"` + hex + `"}}}`, "must be a bare filename"},
		{`{"version":"0.2.0","files":{"linux-amd64":{"name":"sub/dir","sha256":"` + hex + `"}}}`, "must be a bare filename"},
		{`{"version":"0.2.0","files":{"linux-amd64":{"name":"ok","sha256":"nothex"}}}`, "sha256 must be"},
		{`{"version":"0.2.0","files":{"":{"name":"ok","sha256":"` + hex + `"}}}`, "empty platform-arch key"},
		{`{"version":"0.2.0","files":{},"extra":1}`, "invalid JSON"},
		{`{"version":"0.2.0","files":{}} trailing`, "trailing data"},
		{`not json`, "invalid JSON"},
	}
	for i, b := range bad {
		_, err := ParseManifest([]byte(b.data))
		if err == nil {
			t.Errorf("bad[%d]: expected error containing %q", i, b.want)
			continue
		}
		if !strings.Contains(err.Error(), b.want) {
			t.Errorf("bad[%d]: error %q does not contain %q", i, err, b.want)
		}
	}
}

func TestManifestLibraryLoadAndLookup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(sampleManifest()), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := newManifestLibrary(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := lib.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := lib.ReleasedVersion(); got != "0.2.0" {
		t.Fatalf("ReleasedVersion = %q", got)
	}
	entry, ok := lib.ManifestForPlatform("linux-amd64")
	if !ok || entry.Version != "0.2.0" || entry.Name != "msp-agent-linux-amd64" || !protocol.ValidSHA256Hex(entry.SHA256) {
		t.Fatalf("ManifestForPlatform(linux-amd64) = %+v ok=%v", entry, ok)
	}
	// An unknown platform-arch has no entry.
	if _, ok := lib.ManifestForPlatform("darwin-arm64"); ok {
		t.Fatal("darwin-arm64 must have no manifest entry")
	}
}

func TestManifestLibraryAbsentIsOff(t *testing.T) {
	// An empty dir (no agents.json): self-update is off, not an error.
	lib := newManifestLibrary(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := lib.load(); err != nil {
		t.Fatalf("load of absent manifest must not error: %v", err)
	}
	if lib.ReleasedVersion() != "" {
		t.Fatalf("absent manifest must report no version, got %q", lib.ReleasedVersion())
	}
	if _, ok := lib.ManifestForPlatform("linux-amd64"); ok {
		t.Fatal("absent manifest must advertise nothing")
	}
	// A disabled agent dir ("") is likewise off.
	off := newManifestLibrary("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := off.load(); err != nil {
		t.Fatalf("load with no agent dir: %v", err)
	}
	if off.ReleasedVersion() != "" {
		t.Fatal("no agent dir must report no version")
	}
}

func TestManifestLibraryMalformedKeepsOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFileName)
	if err := os.WriteFile(path, []byte(`{"version":"","files":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := newManifestLibrary(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := lib.load(); err == nil {
		t.Fatal("a malformed manifest must return an error from load")
	}
	// Self-update stays off rather than crashing.
	if lib.ReleasedVersion() != "" {
		t.Fatalf("malformed manifest must report no version, got %q", lib.ReleasedVersion())
	}
}

func TestManifestLibraryReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFileName)
	if err := os.WriteFile(path, []byte(sampleManifest()), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := newManifestLibrary(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := lib.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if lib.ReleasedVersion() != "0.2.0" {
		t.Fatalf("initial version %q", lib.ReleasedVersion())
	}

	// Rewrite with a new version and fewer files so both mtime and size change,
	// then confirm current() picks it up without a reload call.
	hex := strings.Repeat("b", protocol.SHA256HexLen)
	v2 := `{"version":"0.3.0","files":{"linux-amd64":{"name":"msp-agent-linux-amd64","sha256":"` + hex + `"}}}`
	if err := os.WriteFile(path, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lib.ReleasedVersion(); got != "0.3.0" {
		t.Fatalf("reloaded version = %q, want 0.3.0", got)
	}
	if _, ok := lib.ManifestForPlatform("windows-amd64"); ok {
		t.Fatal("windows entry must be gone after the reload")
	}

	// Removing the file turns self-update off on the next access.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if lib.ReleasedVersion() != "" {
		t.Fatalf("removed manifest must report no version, got %q", lib.ReleasedVersion())
	}
}
