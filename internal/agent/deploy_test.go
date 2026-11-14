//go:build linux

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// sha256hex is the hex SHA-256 of b, the value a deploy command carries and the
// agent re-checks after download.
func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fileServer models the agent file-download contract (SD1): it serves one
// library file (id 7) at /api/agent/v1/files/{id} with device-token auth, a
// Content-Length and an X-MSP-SHA256 header, and records how many requests it
// saw so a test can prove a rejected deploy never downloaded anything. It is a
// LOCAL httptest server, so no test fetches from the public internet.
type fileServer struct {
	body  []byte
	token string
	hits  int
}

func (fs *fileServer) handler() http.Handler {
	sum := sha256hex(fs.body)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.hits++
		if r.URL.Path != "/api/agent/v1/files/7" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not_found"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fs.token {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid_device_token"}`))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(fs.body)))
		w.Header().Set("X-MSP-SHA256", sum)
		w.WriteHeader(http.StatusOK)
		w.Write(fs.body)
	})
}

// newDeployFixture spins up a local file server and a Client/Deployer pointed at
// it with the device token the server expects.
func newDeployFixture(t *testing.T, body []byte) (*Deployer, *fileServer) {
	t.Helper()
	fs := &fileServer{body: body, token: "dt_test"}
	srv := httptest.NewServer(fs.handler())
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{ServerURL: srv.URL, DeviceToken: "dt_test"}, "test")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return NewDeployer(c), fs
}

// markerScript is the harmless run-path artifact.
func markerScript(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/deploy-marker.sh")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fileCmd(dest string, sum string, run bool, args ...string) protocol.Command {
	return protocol.Command{
		ID:       1,
		Kind:     protocol.KindDeploy,
		Source:   protocol.DeploySourceFile,
		FileID:   7,
		SHA256:   sum,
		Dest:     dest,
		Run:      run,
		RunArgs:  args,
		TimeoutS: 60,
	}
}

func TestDeployFileRunMarker(t *testing.T) {
	body := markerScript(t)
	dep, fs := newDeployFixture(t, body)
	marker := filepath.Join(t.TempDir(), "ran.marker")
	// dest lives under a not-yet-existing parent so the 0755 parent-creation
	// path is exercised too.
	dest := filepath.Join(t.TempDir(), "acme", "installer.sh")

	res := dep.RunDeploy(context.Background(), fileCmd(dest, sha256hex(body), true, marker))
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("run deploy failed: exit=%d err=%q stdout=%q", res.ExitCode, res.Error, res.Stdout)
	}
	if !strings.Contains(res.Stdout, "deploy-marker ran with arg="+marker) {
		t.Fatalf("stdout missing marker output: %q", res.Stdout)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file not created by the deployed script: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("dest not written: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("dest mode = %o, want 0755 (run)", info.Mode().Perm())
	}
	if fs.hits != 1 {
		t.Fatalf("file server hits = %d, want 1", fs.hits)
	}
}

func TestDeployFileNoRunWrites0644(t *testing.T) {
	body := []byte("binary-ish payload\x00\x01\x02")
	dep, _ := newDeployFixture(t, body)
	dest := filepath.Join(t.TempDir(), "lib", "artifact.bin")

	res := dep.RunDeploy(context.Background(), fileCmd(dest, sha256hex(body), false))
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("deploy failed: exit=%d err=%q", res.ExitCode, res.Error)
	}
	if !strings.Contains(res.Stdout, "deployed "+strconv.Itoa(len(body))+" bytes to "+dest) {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("dest not written: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("dest mode = %o, want 0644 (no run)", info.Mode().Perm())
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(body) {
		t.Fatalf("dest contents differ from the served file")
	}
}

func TestDeployFileHashMismatchDoesNotWriteDest(t *testing.T) {
	body := []byte("the real file")
	dep, fs := newDeployFixture(t, body)
	dest := filepath.Join(t.TempDir(), "drop", "installer.sh")
	wrong := strings.Repeat("a", protocol.SHA256HexLen)

	res := dep.RunDeploy(context.Background(), fileCmd(dest, wrong, true))
	if res.ExitCode == 0 {
		t.Fatalf("hash mismatch must fail the command, got exit 0")
	}
	if !strings.Contains(res.Error, "sha256 mismatch") {
		t.Fatalf("error = %q, want a sha256 mismatch", res.Error)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest must NOT be written on a hash mismatch (stat err=%v)", err)
	}
	if fs.hits != 1 {
		t.Fatalf("file server hits = %d, want 1 (download attempted then verified)", fs.hits)
	}
}

func TestDeployFileRejectsUnsafeDest(t *testing.T) {
	body := []byte("x")
	cases := []struct {
		name string
		dest string
	}{
		{"relative", "relative/installer.sh"},
		{"dotdot", "/opt/../etc/passwd"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep, fs := newDeployFixture(t, body)
			res := dep.RunDeploy(context.Background(), fileCmd(tc.dest, sha256hex(body), false))
			if res.ExitCode == 0 {
				t.Fatalf("unsafe dest %q must be rejected", tc.dest)
			}
			if !strings.Contains(res.Error, "dest") {
				t.Fatalf("error = %q, want a dest-path rejection", res.Error)
			}
			if fs.hits != 0 {
				t.Fatalf("an unsafe dest must be rejected BEFORE any download; hits = %d", fs.hits)
			}
		})
	}
}

func TestDeployFileRejectsSymlinkedParent(t *testing.T) {
	body := []byte("payload")
	dep, fs := newDeployFixture(t, body)

	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(link, "installer.sh")

	res := dep.RunDeploy(context.Background(), fileCmd(dest, sha256hex(body), false))
	if res.ExitCode == 0 {
		t.Fatalf("a symlinked parent must be rejected, got exit 0")
	}
	if !strings.Contains(res.Error, "symlinked parent") {
		t.Fatalf("error = %q, want a symlinked-parent rejection", res.Error)
	}
	// Nothing must have been written through the symlink into the real target.
	if _, err := os.Stat(filepath.Join(realDir, "installer.sh")); !os.IsNotExist(err) {
		t.Fatalf("a file was written through the symlinked parent (err=%v)", err)
	}
	if fs.hits != 0 {
		t.Fatalf("a symlinked parent must be rejected BEFORE any download; hits = %d", fs.hits)
	}
}

func TestDeployFileMissingFileIs404(t *testing.T) {
	body := []byte("x")
	dep, _ := newDeployFixture(t, body)
	cmd := fileCmd(filepath.Join(t.TempDir(), "a.sh"), sha256hex(body), false)
	cmd.FileID = 9 // the server only serves id 7
	res := dep.RunDeploy(context.Background(), cmd)
	if res.ExitCode == 0 {
		t.Fatalf("a download 404 must fail the command")
	}
	if !strings.Contains(res.Error, "404") && !strings.Contains(res.Error, "not_found") {
		t.Fatalf("error = %q, want a 404/not_found", res.Error)
	}
}

func TestDownloadFileVerifiesSizeAuthAndHash(t *testing.T) {
	body := []byte("hello deploy world")
	fs := &fileServer{body: body, token: "dt_test"}
	srv := httptest.NewServer(fs.handler())
	t.Cleanup(srv.Close)

	// Correct token: streams, size + hash verified, temp file returned.
	ok, err := NewClient(Config{ServerURL: srv.URL, DeviceToken: "dt_test"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tmp, n, err := ok.downloadFile(context.Background(), 7, sha256hex(body), dir)
	if err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("size = %d, want %d", n, len(body))
	}
	got, _ := os.ReadFile(tmp)
	if string(got) != string(body) {
		t.Fatalf("downloaded bytes differ from the served file")
	}
	if filepath.Dir(tmp) != dir {
		t.Fatalf("temp file %q not created in the requested dir %q", tmp, dir)
	}
	os.Remove(tmp)

	// Wrong token: 401 HTTPError, nothing written.
	bad, err := NewClient(Config{ServerURL: srv.URL, DeviceToken: "dt_wrong"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bad.downloadFile(context.Background(), 7, sha256hex(body), t.TempDir()); err == nil {
		t.Fatalf("a wrong device token must fail the download")
	}
}

// fakeExecutor is defined in patch_test.go (same package, same build tag); the
// deploy package path reuses the same injectable executor seam as PM1.

func TestDeployPackageInstall(t *testing.T) {
	fx := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		return cmdResult{Stdout: "Setting up htop ...\n", ExitCode: 0}, nil
	}}
	dep := newDeployerWith(nil, fx)
	res := dep.RunDeploy(context.Background(), protocol.Command{
		ID: 2, Kind: protocol.KindDeploy, Source: protocol.DeploySourcePackage, Package: "htop", TimeoutS: 600,
	})
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("package install failed: exit=%d err=%q", res.ExitCode, res.Error)
	}
	if !strings.Contains(res.Stdout, "Setting up htop") {
		t.Fatalf("stdout = %q, want the install log", res.Stdout)
	}
	if len(fx.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(fx.calls))
	}
	c := fx.calls[0]
	if c.name != "apt-get" || c.argLine() != "-y install htop" {
		t.Fatalf("install command = %s %s, want apt-get -y install htop", c.name, c.argLine())
	}
	if !c.hasEnv("DEBIAN_FRONTEND=noninteractive") {
		t.Fatalf("install env missing DEBIAN_FRONTEND=noninteractive: %v", c.env)
	}
}

func TestDeployPackageInstallFailure(t *testing.T) {
	fx := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		return cmdResult{Stderr: "E: Unable to locate package nosuch\n", ExitCode: 100}, nil
	}}
	dep := newDeployerWith(nil, fx)
	res := dep.RunDeploy(context.Background(), protocol.Command{
		ID: 3, Kind: protocol.KindDeploy, Source: protocol.DeploySourcePackage, Package: "nosuch",
	})
	if res.ExitCode == 0 {
		t.Fatalf("a failed install must fail the command")
	}
	if !strings.Contains(res.Error, "exited 100") {
		t.Fatalf("error = %q, want the apt exit code", res.Error)
	}
	if !strings.Contains(res.Stdout, "Unable to locate package") {
		t.Fatalf("stdout should carry the apt log: %q", res.Stdout)
	}
}

func TestDeployPackageRejectsBadName(t *testing.T) {
	fx := &fakeExecutor{}
	dep := newDeployerWith(nil, fx)
	res := dep.RunDeploy(context.Background(), protocol.Command{
		ID: 4, Kind: protocol.KindDeploy, Source: protocol.DeploySourcePackage, Package: "bad; rm -rf /",
	})
	if res.ExitCode == 0 {
		t.Fatalf("an invalid package name must be rejected")
	}
	if len(fx.calls) != 0 {
		t.Fatalf("an invalid package name must be rejected BEFORE running apt; calls = %d", len(fx.calls))
	}
}

func TestDeployUnknownSource(t *testing.T) {
	dep := newDeployerWith(nil, &fakeExecutor{})
	res := dep.RunDeploy(context.Background(), protocol.Command{ID: 5, Kind: protocol.KindDeploy, Source: "url"})
	if res.ExitCode == 0 || !strings.Contains(res.Error, "unknown source") {
		t.Fatalf("unknown source must fail: exit=%d err=%q", res.ExitCode, res.Error)
	}
}
