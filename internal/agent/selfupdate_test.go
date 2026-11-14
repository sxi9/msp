//go:build linux

package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// A fake "new" agent binary: a tiny shell script that prints version on any
// invocation, so "<binary> version" (the health check) answers with version.
// This never replaces the real test binary — every swap in these tests targets a
// temp "exe" path created under t.TempDir().
func fakeBinary(version string) []byte {
	return []byte("#!/bin/sh\necho " + version + "\n")
}

// fakeBrokenBinary exits non-zero on any invocation, modelling a swapped-in
// binary that cannot run its own version subcommand (the rollback trigger).
func fakeBrokenBinary() []byte {
	return []byte("#!/bin/sh\nexit 1\n")
}

// fakeDownloader is an injected updateDownloader that writes canned bytes to a
// temp file beside the "exe" instead of fetching from the network.
type fakeDownloader struct {
	body []byte
	err  error
}

func (d fakeDownloader) fetch(ctx context.Context, rawURL, dir string) (string, int64, error) {
	if d.err != nil {
		return "", 0, d.err
	}
	f, err := os.CreateTemp(dir, ".msp-update-*")
	if err != nil {
		return "", 0, err
	}
	if _, err := f.Write(d.body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", 0, err
	}
	return f.Name(), int64(len(d.body)), nil
}

// leftoverTempFiles reports download/backup temp files still lying in dir, which
// a clean abort must not leave behind.
func leftoverTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var left []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".msp-update-") || strings.HasPrefix(n, ".msp-bak-") {
			left = append(left, n)
		}
	}
	return left
}

// TestSelfUpdateSuccess drives the WHOLE swap against a fake new binary: the
// temp "exe" is replaced with the new binary, the old one is kept as <exe>.bak,
// and the result reports old -> new.
func TestSelfUpdateSuccess(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "msp-agent")
	oldBody := fakeBinary("0.1.0")
	if err := os.WriteFile(exe, oldBody, 0o755); err != nil {
		t.Fatal(err)
	}
	newBody := fakeBinary("0.2.0")

	u := newUpdaterWith(&Client{version: "0.1.0"}, fakeDownloader{body: newBody}, execRunner{}, func() (string, error) { return exe, nil })
	cmd := protocol.Command{Kind: protocol.KindUpdate, Version: "0.2.0", URL: "/dl/msp-agent", SHA256: sha256hex(newBody), TimeoutS: 600}

	res := u.RunUpdate(context.Background(), cmd)
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("expected success, got exit=%d err=%q", res.ExitCode, res.Error)
	}
	if !strings.Contains(res.Stdout, "0.1.0 -> 0.2.0") {
		t.Fatalf("result should report old->new, got %q", res.Stdout)
	}
	if !u.ShouldRestart() {
		t.Fatal("ShouldRestart should be true after a committed swap")
	}
	// The exe is now the new binary.
	if got, _ := os.ReadFile(exe); string(got) != string(newBody) {
		t.Fatalf("exe was not replaced with the new binary")
	}
	// The old binary is kept for rollback.
	if got, err := os.ReadFile(exe + ".bak"); err != nil || string(got) != string(oldBody) {
		t.Fatalf("expected <exe>.bak to hold the old binary, err=%v", err)
	}
	if left := leftoverTempFiles(t, dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
	// The swapped-in binary really answers its version subcommand.
	out, err := execRunner{}.run(context.Background(), nil, exe, "version")
	if err != nil || strings.TrimSpace(out.Stdout) != "0.2.0" {
		t.Fatalf("installed binary did not report the new version: out=%q err=%v", out.Stdout, err)
	}
}

// TestSelfUpdateSHA256Mismatch: a hash mismatch aborts before any change — no
// swap, no backup, no dangling temp file.
func TestSelfUpdateSHA256Mismatch(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "msp-agent")
	oldBody := fakeBinary("0.1.0")
	if err := os.WriteFile(exe, oldBody, 0o755); err != nil {
		t.Fatal(err)
	}
	newBody := fakeBinary("0.2.0")

	u := newUpdaterWith(&Client{version: "0.1.0"}, fakeDownloader{body: newBody}, execRunner{}, func() (string, error) { return exe, nil })
	cmd := protocol.Command{Kind: protocol.KindUpdate, Version: "0.2.0", URL: "/dl/x", SHA256: sha256hex([]byte("different bytes")), TimeoutS: 600}

	res := u.RunUpdate(context.Background(), cmd)
	if res.ExitCode == 0 || !strings.Contains(res.Error, "sha256 mismatch") {
		t.Fatalf("expected a sha256 mismatch failure, got exit=%d err=%q", res.ExitCode, res.Error)
	}
	if u.ShouldRestart() {
		t.Fatal("a failed update must not request a restart")
	}
	if got, _ := os.ReadFile(exe); string(got) != string(oldBody) {
		t.Fatal("exe must be untouched on a hash mismatch")
	}
	if _, err := os.Stat(exe + ".bak"); !os.IsNotExist(err) {
		t.Fatal("no backup may be created on a hash mismatch")
	}
	if left := leftoverTempFiles(t, dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// TestSelfUpdateHealthCheckFails: the new binary prints the wrong version, so the
// health check aborts the swap with the old binary intact and no dangling backup.
func TestSelfUpdateHealthCheckFails(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "msp-agent")
	oldBody := fakeBinary("0.1.0")
	if err := os.WriteFile(exe, oldBody, 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid hash, but the binary reports a version other than the target.
	newBody := fakeBinary("9.9.9")

	u := newUpdaterWith(&Client{version: "0.1.0"}, fakeDownloader{body: newBody}, execRunner{}, func() (string, error) { return exe, nil })
	cmd := protocol.Command{Kind: protocol.KindUpdate, Version: "0.2.0", URL: "/dl/x", SHA256: sha256hex(newBody), TimeoutS: 600}

	res := u.RunUpdate(context.Background(), cmd)
	if res.ExitCode == 0 || !strings.Contains(res.Error, "health check") {
		t.Fatalf("expected a health-check failure, got exit=%d err=%q", res.ExitCode, res.Error)
	}
	if u.ShouldRestart() {
		t.Fatal("a failed update must not request a restart")
	}
	if got, _ := os.ReadFile(exe); string(got) != string(oldBody) {
		t.Fatal("exe must be intact when the health check fails")
	}
	if _, err := os.Stat(exe + ".bak"); !os.IsNotExist(err) {
		t.Fatal("no dangling backup after a failed health check")
	}
	if left := leftoverTempFiles(t, dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// TestRollbackRestoresBackup: a binary that fails its version check at boot is
// rolled back to <exe>.bak, and the backup is cleared.
func TestRollbackRestoresBackup(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "msp-agent")
	bak := exe + ".bak"
	if err := os.WriteFile(exe, fakeBrokenBinary(), 0o755); err != nil {
		t.Fatal(err)
	}
	goodOld := fakeBinary("0.1.0")
	if err := os.WriteFile(bak, goodOld, 0o755); err != nil {
		t.Fatal(err)
	}

	restored, err := rollbackCheck(context.Background(), exe, execRunner{}, "0.2.0", nil)
	if err != nil {
		t.Fatalf("rollbackCheck error: %v", err)
	}
	if !restored {
		t.Fatal("expected rollback to restore the backup")
	}
	if got, _ := os.ReadFile(exe); string(got) != string(goodOld) {
		t.Fatal("exe was not restored to the backup content")
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Fatal("backup should be removed after a rollback")
	}
}

// TestRollbackHealthyKeepsBinary: a healthy installed binary is left in place and
// the backup is kept for FinalizeUpdate to clear after the next good sync.
func TestRollbackHealthyKeepsBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "msp-agent")
	bak := exe + ".bak"
	newGood := fakeBinary("0.2.0")
	if err := os.WriteFile(exe, newGood, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, fakeBinary("0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}

	restored, err := rollbackCheck(context.Background(), exe, execRunner{}, "0.2.0", nil)
	if err != nil {
		t.Fatalf("rollbackCheck error: %v", err)
	}
	if restored {
		t.Fatal("a healthy binary must not be rolled back")
	}
	if got, _ := os.ReadFile(exe); string(got) != string(newGood) {
		t.Fatal("a healthy exe must be left untouched")
	}
	if _, err := os.Stat(bak); err != nil {
		t.Fatal("the backup must be kept until FinalizeUpdate removes it")
	}
}

// TestRollbackNoBackupNoop: with no backup present the check does nothing.
func TestRollbackNoBackupNoop(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "msp-agent")
	if err := os.WriteFile(exe, fakeBrokenBinary(), 0o755); err != nil {
		t.Fatal(err)
	}
	restored, err := rollbackCheck(context.Background(), exe, execRunner{}, "0.2.0", nil)
	if err != nil || restored {
		t.Fatalf("no backup should be a no-op, got restored=%v err=%v", restored, err)
	}
}

// TestFinalizeBackupsRemoves clears the backup once the agent is known good.
func TestFinalizeBackupsRemoves(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "msp-agent")
	bak := exe + ".bak"
	if err := os.WriteFile(exe, fakeBinary("0.2.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, fakeBinary("0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	finalizeBackups(exe, nil)
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Fatal("FinalizeUpdate should remove <exe>.bak")
	}
}

// TestRunUpdateRateLimit: a repeat of an already-applied version is a no-op
// success (restart pending), and a bad command is rejected before any work.
func TestRunUpdateRateLimit(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "msp-agent")
	if err := os.WriteFile(exe, fakeBinary("0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBody := fakeBinary("0.2.0")
	u := newUpdaterWith(&Client{version: "0.1.0"}, fakeDownloader{body: newBody}, execRunner{}, func() (string, error) { return exe, nil })
	cmd := protocol.Command{Kind: protocol.KindUpdate, Version: "0.2.0", URL: "/dl/x", SHA256: sha256hex(newBody), TimeoutS: 600}

	if res := u.RunUpdate(context.Background(), cmd); res.ExitCode != 0 {
		t.Fatalf("first apply should succeed: %q", res.Error)
	}
	// A repeat of the applied version should not re-swap; it is a no-op success.
	res := u.RunUpdate(context.Background(), cmd)
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "already applied") {
		t.Fatalf("repeat of applied version should be a no-op success, got exit=%d out=%q", res.ExitCode, res.Stdout)
	}

	// An invalid command is rejected outright.
	bad := protocol.Command{Kind: protocol.KindUpdate, Version: "", URL: "/dl/x", SHA256: sha256hex(newBody)}
	if res := u.RunUpdate(context.Background(), bad); res.ExitCode == 0 {
		t.Fatal("an update with no version must fail")
	}
}

// TestShouldApplyAdvertised covers the auto-update decision.
func TestShouldApplyAdvertised(t *testing.T) {
	good := &protocol.AgentUpdate{Version: "0.2.0", URL: "/dl/x", SHA256: strings.Repeat("a", 64)}
	cases := []struct {
		name    string
		enabled bool
		upd     *protocol.AgentUpdate
		current string
		want    bool
	}{
		{"applies when newer and enabled", true, good, "0.1.0", true},
		{"disabled never applies", false, good, "0.1.0", false},
		{"nil update never applies", true, nil, "0.1.0", false},
		{"same version never applies", true, good, "0.2.0", false},
		{"invalid block never applies", true, &protocol.AgentUpdate{Version: "0.2.0", URL: "x", SHA256: "nope"}, "0.1.0", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldApplyAdvertised(c.enabled, c.upd, c.current); got != c.want {
				t.Fatalf("ShouldApplyAdvertised = %v, want %v", got, c.want)
			}
		})
	}
	// UpdateCommand mirrors the advertised block into a kind update command.
	cmd := UpdateCommand(good)
	if cmd.Kind != protocol.KindUpdate || cmd.Version != good.Version || cmd.URL != good.URL || cmd.SHA256 != good.SHA256 {
		t.Fatalf("UpdateCommand did not mirror the advertised update: %+v", cmd)
	}
}

// TestResolveUpdateURL: a bare path joins the base; an absolute URL is accepted
// only on the same host, so the device token never goes to another host.
func TestResolveUpdateURL(t *testing.T) {
	c := &Client{base: "https://srv.example"}
	if got, err := c.resolveUpdateURL("/dl/msp-agent-linux-amd64"); err != nil || got != "https://srv.example/dl/msp-agent-linux-amd64" {
		t.Fatalf("relative path: got %q err %v", got, err)
	}
	if got, err := c.resolveUpdateURL("https://srv.example/dl/x"); err != nil || got != "https://srv.example/dl/x" {
		t.Fatalf("same-host absolute: got %q err %v", got, err)
	}
	if _, err := c.resolveUpdateURL("https://evil.example/dl/x"); err == nil {
		t.Fatal("an update url on another host must be refused")
	}
	if _, err := c.resolveUpdateURL(""); err == nil {
		t.Fatal("an empty url must be refused")
	}
}

// TestDownloadUpdate exercises the real streaming download against a LOCAL
// httptest server (no public internet): it writes the served bytes to a temp file
// bounded by Content-Length and carries the device token in the Authorization
// header.
func TestDownloadUpdate(t *testing.T) {
	body := fakeBinary("0.2.0")
	const token = "dt_selfupdatetest"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer srv.Close()

	c, err := NewClient(Config{ServerURL: srv.URL, DeviceToken: token, IntervalS: 30, InsecureHTTP: true}, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tmp, n, err := c.downloadUpdate(context.Background(), "/dl/msp-agent", dir)
	if err != nil {
		t.Fatalf("downloadUpdate: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("size: got %d want %d", n, len(body))
	}
	if got, _ := os.ReadFile(tmp); string(got) != string(body) {
		t.Fatal("downloaded bytes do not match served bytes")
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("device token not sent: %q", gotAuth)
	}
	if h, _ := hashFile(tmp); h != sha256hex(body) {
		t.Fatal("hashFile disagrees with the served bytes")
	}
}
