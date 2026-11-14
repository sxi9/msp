package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Software deployment (SD1). The agent runs a kind deploy command two ways:
//
//   - source "file":    download a server-side library file over the
//     authenticated agent channel, verify its size and SHA-256 BEFORE writing,
//     place it safely at an absolute dest (temp file + rename), and optionally
//     run it with argv (no shell), capturing the output.
//   - source "package": install one named OS package through the shared
//     injectable command executor (the same seam PM1 uses), so tests drive a
//     fake apt and no test ever installs a real package or fetches anything
//     from the public internet.
//
// deploy.go is the agent's integration surface for SD1. The download helper is
// added here (on the package's own *Client) rather than in client.go so the
// client's wire plumbing stays owned by the integrator; see the package Client
// internals (base, http, token, UserAgent) it reuses below. The Run loop
// dispatches a KindDeploy command to Deployer.RunDeploy, exactly as it routes a
// KindPatch command to PatchCollector.RunPatch.

// Deployer runs kind deploy commands. File deploys stream bytes through the
// agent's authenticated Client; package deploys go through an injectable
// commandExecutor (execRunner in production, a fake in tests) so the real
// package manager is never touched by a test.
type Deployer struct {
	client *Client
	exec   commandExecutor
}

// NewDeployer builds the production deployer: file downloads use client, package
// installs use os/exec (which resolves apt-get from PATH, so the e2e test's fake
// apt-get on PATH drives the real path without mutating the container).
func NewDeployer(client *Client) *Deployer {
	return &Deployer{client: client, exec: execRunner{}}
}

// newDeployerWith injects an executor for tests (a fake apt).
func newDeployerWith(client *Client, exec commandExecutor) *Deployer {
	return &Deployer{client: client, exec: exec}
}

// RunDeploy executes a kind deploy command and returns its CommandResult. Every
// failure mode is encoded in the result (ExitCode non-zero with Error), never a
// returned error, exactly like Execute and RunPatch. The whole command is
// bounded by its timeout_s (default DefaultTimeoutS, 1..MaxTimeoutS); a file
// download that outlives it is cancelled through the context.
func (d *Deployer) RunDeploy(ctx context.Context, cmd protocol.Command) protocol.CommandResult {
	started := time.Now().UTC()
	timeout := protocol.ClampTimeout(cmd.TimeoutS)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	switch cmd.Source {
	case protocol.DeploySourcePackage:
		return d.runPackage(ctx, cmd, started)
	case protocol.DeploySourceFile:
		return d.runFile(ctx, cmd, started, timeout)
	default:
		return deployFailure(started, "deploy: unknown source "+strconv.Quote(cmd.Source))
	}
}

// runFile downloads the library file, verifies its hash, installs it safely at
// dest (temp file + rename, 0755 when it will be run else 0644) and, when Run is
// set, executes dest with run_args via argv (no shell). A hash mismatch fails
// the command without ever writing dest.
func (d *Deployer) runFile(ctx context.Context, cmd protocol.Command, started time.Time, timeoutS int) protocol.CommandResult {
	// Path safety, layer 1 (platform-independent shape): absolute, cleaned, no
	// "..". The server's Command.Validate already enforced this; the agent
	// re-checks so a bad command never reaches the filesystem.
	if !protocol.ValidDeployDest(cmd.Dest) {
		return deployFailure(started, "deploy: unsafe dest path "+strconv.Quote(cmd.Dest))
	}
	dest := filepath.Clean(cmd.Dest)
	// Path safety, layer 2 (platform-native absolute): a leading-slash path is
	// absolute on Linux but not on Windows (which needs a drive letter), so the
	// native check rejects a dest that is not absolute on THIS platform.
	if !filepath.IsAbs(dest) {
		return deployFailure(started, "deploy: dest is not absolute on this platform: "+strconv.Quote(cmd.Dest))
	}

	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return deployFailure(started, "deploy: create parent directory: "+err.Error())
	}
	// Path safety, layer 3: refuse to write through a symlinked parent that
	// would redirect the file outside the intended directory.
	if err := checkParentNoSymlinkEscape(parent); err != nil {
		return deployFailure(started, "deploy: "+err.Error())
	}

	// Download into a temp file IN the dest's parent (so the final rename is an
	// atomic, same-filesystem move) and verify size + sha256 before any rename.
	tmp, size, err := d.client.downloadFile(ctx, cmd.FileID, cmd.SHA256, parent)
	if err != nil {
		return deployFailure(started, "deploy: "+err.Error())
	}

	mode := os.FileMode(0o644)
	if cmd.Run {
		mode = 0o755
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return deployFailure(started, "deploy: chmod downloaded file: "+err.Error())
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return deployFailure(started, "deploy: install to dest: "+err.Error())
	}

	if !cmd.Run {
		now := time.Now().UTC()
		return protocol.CommandResult{
			StartedAt:  started,
			FinishedAt: now,
			Stdout:     fmt.Sprintf("deployed %d bytes to %s", size, dest),
		}
	}

	// Run dest with run_args via argv, no shell, bounded by the command ctx.
	dir, err := makeCommandDir(cmd.ID)
	if err != nil {
		return deployFailure(started, "deploy: "+err.Error())
	}
	defer os.RemoveAll(dir)
	argv := append([]string{dest}, cmd.RunArgs...)
	res := runProcess(ctx, cmd.ID, argv, dir, timeoutS)
	// Report the command's start as when the deploy began (download included),
	// not just when the process started.
	res.StartedAt = started
	return res
}

// runPackage installs one named OS package through the injectable executor. The
// package name is a single argv token (never a shell), re-validated here as a
// last line of defence.
func (d *Deployer) runPackage(ctx context.Context, cmd protocol.Command, started time.Time) protocol.CommandResult {
	if !protocol.ValidPackageName(cmd.Package) {
		return deployFailure(started, "deploy: invalid package name "+strconv.Quote(cmd.Package))
	}
	out, err := installPackage(ctx, d.exec, cmd.Package)
	finished := time.Now().UTC()
	stdout, truncated := capLog(out)
	res := protocol.CommandResult{StartedAt: started, FinishedAt: finished, Stdout: stdout, Truncated: truncated}
	if err != nil {
		res.ExitCode = 1
		res.Error = err.Error()
	}
	return res
}

// installPackage installs one package via the shared command executor. On Linux
// it runs `DEBIAN_FRONTEND=noninteractive apt-get -y install <pkg>` (the same
// executor seam PM1 uses for apt). The Windows installer path is NOT VERIFIED
// and is not wired here yet; a package deploy on Windows fails cleanly.
func installPackage(ctx context.Context, x commandExecutor, pkg string) (string, error) {
	if runtime.GOOS == protocol.PlatformWindows {
		return "", errors.New("package deploy is not supported on windows yet (NOT VERIFIED)")
	}
	env := []string{"DEBIAN_FRONTEND=noninteractive"}
	res, err := x.run(ctx, env, "apt-get", "-y", "install", pkg)
	log := joinOutput(res)
	if err != nil {
		return log, fmt.Errorf("apt-get -y install %s: %w", pkg, err)
	}
	if res.ExitCode != 0 {
		return log, fmt.Errorf("apt-get -y install %s exited %d", pkg, res.ExitCode)
	}
	return log, nil
}

// checkParentNoSymlinkEscape refuses a dest whose immediate parent directory is
// reached by traversing a symlink in its final component: such a parent could
// redirect the write outside the intended directory. Symlinks higher up the
// tree (for example a distro's /var/run -> /run) are resolved into the
// comparison and allowed, so only a symlinked parent itself is rejected.
func checkParentNoSymlinkEscape(parent string) error {
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("resolve parent directory %s: %w", parent, err)
	}
	grandparent := filepath.Dir(parent)
	realGrand, err := filepath.EvalSymlinks(grandparent)
	if err != nil {
		return fmt.Errorf("resolve parent directory %s: %w", grandparent, err)
	}
	want := filepath.Join(realGrand, filepath.Base(parent))
	if realParent != want {
		return fmt.Errorf("refusing to write through a symlinked parent %s -> %s", parent, realParent)
	}
	return nil
}

// deployFailure is the result for a deploy command that could not run as asked
// (bad path, download/verify failure, or a failed package install).
func deployFailure(started time.Time, msg string) protocol.CommandResult {
	return protocol.CommandResult{ExitCode: 1, StartedAt: started, FinishedAt: time.Now().UTC(), Error: msg}
}

// downloadFile streams GET /api/agent/v1/files/{id} into a temp file in dir,
// authenticated with the device token and using the Client's TLS/CA
// configuration. It verifies the response Content-Length against the bytes
// written and the SHA-256 against wantHex (and the server's advertised
// X-MSP-SHA256 when present) BEFORE returning the temp file path, so the caller
// never installs or runs unverified bytes. Any failure removes the temp file
// and returns an error; a non-2xx answer becomes an *HTTPError, matching the
// Client's other calls.
//
// It is defined on *Client, in deploy.go, so the SD1 download logic lives with
// the rest of the deploy code while still reusing the Client's base URL, shared
// transport (hence TLS/CA config), device token and User-Agent.
func (c *Client) downloadFile(ctx context.Context, fileID int64, wantHex, dir string) (string, int64, error) {
	url := fmt.Sprintf("%s/api/agent/v1/files/%d", c.base, fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", c.UserAgent())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	// A library file can be large and take longer than the Client's 40 s
	// long-poll timeout, so the download uses a client that shares the same
	// transport (and therefore the same TLS/CA configuration) but relies on the
	// command's context for its deadline instead of a fixed overall timeout.
	hc := &http.Client{Transport: c.http.Transport}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		he := &HTTPError{Status: resp.StatusCode}
		var er protocol.ErrorResponse
		if json.Unmarshal(data, &er) == nil {
			he.Code = er.Error
			he.Detail = er.Detail
		}
		return "", 0, he
	}
	if resp.ContentLength < 0 {
		return "", 0, errors.New("download: server did not send a Content-Length")
	}

	f, err := os.CreateTemp(dir, ".msp-deploy-*")
	if err != nil {
		return "", 0, fmt.Errorf("download: %w", err)
	}
	tmp := f.Name()

	h := sha256.New()
	// LimitReader bounds the bytes written to the declared Content-Length, so a
	// server that streams more than it promised cannot fill the disk; a short or
	// over-long body is caught by the size check below.
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, resp.ContentLength))
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("download: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("download: %w", closeErr)
	}
	if n != resp.ContentLength {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("download: got %d of %d bytes", n, resp.ContentLength)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantHex) {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("download: sha256 mismatch: got %s, want %s", got, strings.ToLower(wantHex))
	}
	if adv := resp.Header.Get("X-MSP-SHA256"); adv != "" && !strings.EqualFold(adv, got) {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("download: server X-MSP-SHA256 %s does not match the downloaded bytes %s", adv, got)
	}
	return tmp, n, nil
}
