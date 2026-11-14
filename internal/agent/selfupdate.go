package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Agent self-update (AU1). This is the highest-risk feature in the platform: a
// bad push could brick every managed device. The whole design is therefore
// built around being reversible and self-checking, not just "swap the file":
//
//  1. resolve the running executable and refuse to touch anything that is not a
//     regular file we can replace (e.g. a `go run` temp binary);
//  2. download the advertised URL with the device token into a temp file BESIDE
//     the executable, then verify its size and SHA-256 BEFORE doing anything
//     else — a mismatch aborts with no change and no backup left behind;
//  3. chmod 0755, then back up the current executable to <exe>.bak (a COPY, so
//     the running image is never disturbed);
//  4. HEALTH-CHECK the new binary before committing: run "<tmp> version" and
//     confirm it prints the target version; a failure aborts with the old
//     binary still in place and no dangling backup;
//  5. atomically rename the verified, health-checked binary into place (on
//     Windows the service-safe rename-aside swap, see selfupdate_windows.go);
//  6. the caller posts the command result and then exits so the service manager
//     restarts into the new binary. On the next start BootstrapRollback checks
//     the installed binary and restores <exe>.bak if it cannot run its own
//     version subcommand; after the next good sync FinalizeUpdate removes the
//     backup.
//
// Everything that touches the outside world — the download, the health-check
// executor and the resolve-executable step — is injectable so selfupdate_test.go
// can drive the WHOLE swap against a FAKE "new" binary targeting a temp "exe"
// path, without ever replacing the real test binary or the container's agent.
//
// selfupdate.go is the agent's integration surface for AU1, mirroring deploy.go
// (SD1): the download helper is a method on the package's own *Client (added
// here, not in client.go, so the client's wire plumbing stays owned by the
// integrator), and the Run loop dispatches a KindUpdate command to
// Updater.RunUpdate exactly as it routes KindDeploy to Deployer.RunDeploy.

// healthCheckTimeout bounds the "<tmp> version" probe run before a swap and the
// "<exe> version" probe run by BootstrapRollback. A healthy binary answers in
// well under a second; a hang must not wedge the agent.
const healthCheckTimeout = 30 * time.Second

// updateDownloader fetches an agent-update URL into a temp file. The production
// implementation streams over the agent's authenticated *Client; tests inject a
// fake that writes canned bytes so no test fetches from the public internet. The
// SHA-256 is verified by the Updater after fetch (not by the downloader) so the
// abort-on-mismatch path is exercised regardless of which downloader is used.
type updateDownloader interface {
	// fetch downloads rawURL (relative to the server base) into a new temp file
	// inside dir and returns the temp file path and the number of bytes written.
	// Any failure removes the temp file and returns an error.
	fetch(ctx context.Context, rawURL, dir string) (path string, size int64, err error)
}

// clientDownloader is the production updateDownloader over the agent's *Client.
type clientDownloader struct{ c *Client }

func (d clientDownloader) fetch(ctx context.Context, rawURL, dir string) (string, int64, error) {
	return d.c.downloadUpdate(ctx, rawURL, dir)
}

// Updater applies agent self-update commands (AU1). A dashboard update command
// is forced through RunUpdate; an advertised auto-update is turned into a command
// and applied the same way. The updater serialises updates and remembers the
// version it is applying / has applied so a repeated identical advertisement is
// ignored while one is in flight or a restart is already pending.
type Updater struct {
	client  *Client
	dl      updateDownloader
	exec    commandExecutor
	resolve func() (string, error) // resolves the running executable; injectable for tests

	mu       sync.Mutex
	inflight string // version currently being swapped in ("" when idle)
	applied  string // version swapped in successfully; a restart is pending
}

// NewUpdater builds the production updater: downloads stream over client, the
// health-check runs the new binary through os/exec, and the executable is
// resolved with resolveExe (resolveExecutable: os.Executable + symlink
// resolution). resolveExe is a package variable so the cross-package e2e test can
// point the WHOLE swap at a temp "exe" path (SetResolveExecutableForTest) without
// ever touching the real running binary.
func NewUpdater(client *Client) *Updater {
	return &Updater{
		client:  client,
		dl:      clientDownloader{client},
		exec:    execRunner{},
		resolve: resolveExe,
	}
}

// newUpdaterWith injects the download, health-check and resolve seams for tests.
func newUpdaterWith(client *Client, dl updateDownloader, exec commandExecutor, resolve func() (string, error)) *Updater {
	return &Updater{client: client, dl: dl, exec: exec, resolve: resolve}
}

// ShouldRestart reports whether a self-update has swapped a new binary into
// place and the agent must now exit so the service manager restarts into it. The
// Run loop checks this after posting a command result (or after an auto-update)
// and returns so systemd / the Windows SCM restarts the service.
func (u *Updater) ShouldRestart() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.applied != ""
}

// RunUpdate applies a kind update command and returns its result, like
// RunDeploy / RunPatch. It never returns an error: every failure mode is encoded
// in the CommandResult (ExitCode 1 with Error). On success the new binary is in
// place and ShouldRestart becomes true; RunUpdate does NOT exit the process — the
// caller posts this result first and then exits for the service manager.
func (u *Updater) RunUpdate(ctx context.Context, cmd protocol.Command) protocol.CommandResult {
	started := time.Now().UTC()

	// Re-validate the command on the agent as a last line of defence; the server
	// already filled and validated version/url/sha256 from the manifest.
	if !protocol.ValidAgentVersion(cmd.Version) {
		return updateFailure(started, "update: invalid target version "+strconv.Quote(cmd.Version))
	}
	if !protocol.ValidUpdateURL(cmd.URL) {
		return updateFailure(started, "update: invalid download url")
	}
	if !protocol.ValidSHA256Hex(cmd.SHA256) {
		return updateFailure(started, "update: invalid sha256")
	}

	// Rate-limit / serialise: ignore a repeat of a version already applied (a
	// restart is pending) and refuse a second concurrent swap.
	u.mu.Lock()
	if u.applied == cmd.Version {
		u.mu.Unlock()
		return updateDone(started, u.runningVersion(), cmd.Version, "already applied; a restart is pending")
	}
	if u.inflight != "" {
		busy := u.inflight
		u.mu.Unlock()
		return updateFailure(started, "update: another update ("+busy+") is already in progress")
	}
	u.inflight = cmd.Version
	u.mu.Unlock()

	res, ok := u.apply(ctx, cmd, started)

	u.mu.Lock()
	u.inflight = ""
	if ok {
		u.applied = cmd.Version
	}
	u.mu.Unlock()
	return res
}

// apply performs the resolve → download → verify → chmod → backup → health-check
// → atomic-swap sequence. It returns the command result and whether the swap
// committed. Every early return leaves the filesystem as it found it (no dangling
// temp file, no dangling <exe>.bak).
func (u *Updater) apply(ctx context.Context, cmd protocol.Command, started time.Time) (protocol.CommandResult, bool) {
	// Step 1: resolve the running executable and refuse anything we cannot safely
	// replace (a non-regular file, or a `go run` temp binary).
	exe, err := u.resolve()
	if err != nil {
		return updateFailure(started, "update: "+err.Error()), false
	}
	if err := regularReplaceable(exe); err != nil {
		return updateFailure(started, "update: cannot self-update in this environment: "+err.Error()), false
	}
	dir := filepath.Dir(exe)
	bak := exe + ".bak"

	// Step 2: download beside the executable, then verify size + sha256 BEFORE
	// touching anything. A mismatch aborts here, so no backup is ever created.
	tmp, size, err := u.dl.fetch(ctx, cmd.URL, dir)
	if err != nil {
		return updateFailure(started, "update: download: "+err.Error()), false
	}
	// From here, any failure must remove the temp file.
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmp)
		}
	}()

	if size <= 0 {
		return updateFailure(started, "update: downloaded an empty binary"), false
	}
	got, err := hashFile(tmp)
	if err != nil {
		return updateFailure(started, "update: hash downloaded binary: "+err.Error()), false
	}
	if !strings.EqualFold(got, cmd.SHA256) {
		return updateFailure(started, fmt.Sprintf("update: sha256 mismatch: got %s, want %s", got, strings.ToLower(cmd.SHA256))), false
	}

	// Step 3: make the new binary executable, then back up the current one (a
	// COPY, so the running image is untouched and rollback has an original).
	if err := os.Chmod(tmp, 0o755); err != nil {
		return updateFailure(started, "update: chmod new binary: "+err.Error()), false
	}
	if err := copyFile(exe, bak, 0o755); err != nil {
		return updateFailure(started, "update: back up current binary: "+err.Error()), false
	}
	// From here a failure must also remove the backup so none is left dangling.
	removeBak := func() { os.Remove(bak) }

	// Step 4: health-check the NEW binary before committing. Run "<tmp> version"
	// and confirm it prints the target version; otherwise abort, old binary
	// intact, backup removed.
	if err := u.healthCheck(ctx, tmp, cmd.Version); err != nil {
		removeBak()
		return updateFailure(started, "update: new binary failed health check: "+err.Error()), false
	}

	// Step 5: atomically swap the verified, health-checked binary into place.
	if err := swapExecutable(tmp, exe); err != nil {
		removeBak()
		return updateFailure(started, "update: swap into place: "+err.Error()), false
	}
	committed = true // tmp is consumed by the swap; keep <exe>.bak for the rollback window

	return updateDone(started, u.runningVersion(), cmd.Version, ""), true
}

// healthCheck runs "<binary> version" with a short timeout and confirms it
// prints wantVersion. A non-zero exit, a run error, or any other output means
// the binary is not a working agent of the expected version.
func (u *Updater) healthCheck(ctx context.Context, binary, wantVersion string) error {
	hctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	res, err := u.exec.run(hctx, nil, binary, "version")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("version subcommand exited %d", res.ExitCode)
	}
	got := strings.TrimSpace(res.Stdout)
	if got != wantVersion {
		return fmt.Errorf("version subcommand printed %q, expected %q", got, wantVersion)
	}
	return nil
}

// runningVersion is the version of the agent process that is applying the update
// (the "old" side of old→new). It reads the Client's version so callers do not
// need to thread it through.
func (u *Updater) runningVersion() string {
	if u.client != nil {
		return u.client.version
	}
	return ""
}

// FinalizeUpdate removes a self-update backup once the agent is known good. The
// Run loop calls it once after its first successful sync: reaching the server on
// the new binary is the signal that the update took, so the rollback backup (and,
// on Windows, the renamed-aside old image) can be cleared. It is best-effort and
// safe to call when there is nothing to clean up.
func FinalizeUpdate(log *slog.Logger) {
	exe, err := resolveExe()
	if err != nil {
		return
	}
	finalizeBackups(exe, log)
}

// finalizeBackups removes <exe>.bak and any platform swap artifacts; split out so
// tests can target a temp "exe" path.
func finalizeBackups(exe string, log *slog.Logger) {
	bak := exe + ".bak"
	if _, err := os.Stat(bak); err == nil {
		if err := os.Remove(bak); err != nil {
			if log != nil {
				log.Warn("could not remove self-update backup", "path", bak, "err", err)
			}
		} else if log != nil {
			log.Info("self-update confirmed good; removed backup", "path", bak)
		}
	}
	cleanupSwapArtifacts(exe)
}

// BootstrapRollback is the start-of-day rollback guard. If a self-update backup
// <exe>.bak exists (an update was applied and not yet confirmed good) it
// health-checks the INSTALLED binary by running "<exe> version" and comparing to
// runningVersion; if that fails the backup is restored over the executable and
// restored==true so the caller exits for the service manager to restart into the
// restored binary. A healthy binary leaves the backup in place for FinalizeUpdate
// to clear after the next good sync. With no backup present it does nothing.
func BootstrapRollback(ctx context.Context, runningVersion string, log *slog.Logger) (restored bool) {
	exe, err := resolveExe()
	if err != nil {
		return false
	}
	ok, err := rollbackCheck(ctx, exe, execRunner{}, runningVersion, log)
	if err != nil && log != nil {
		log.Warn("self-update rollback check failed", "err", err)
	}
	return ok
}

// rollbackCheck is the testable core of BootstrapRollback over an injected exe
// path and executor.
func rollbackCheck(ctx context.Context, exe string, x commandExecutor, runningVersion string, log *slog.Logger) (bool, error) {
	bak := exe + ".bak"
	if _, err := os.Stat(bak); err != nil {
		return false, nil // nothing to roll back
	}
	hctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	res, err := x.run(hctx, nil, exe, "version")
	healthy := err == nil && res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == runningVersion
	if healthy {
		return false, nil // installed binary is good; keep the backup for FinalizeUpdate
	}
	if log != nil {
		log.Error("installed agent failed its post-update health check; rolling back", "exe", exe)
	}
	// Restore the backup over the executable. On non-Windows this is a plain
	// overwrite; on Windows the running image is renamed aside first.
	if err := restoreBackup(bak, exe); err != nil {
		return false, fmt.Errorf("restore backup: %w", err)
	}
	os.Remove(bak)
	return true, nil
}

// downloadUpdate streams an agent-update binary from rawURL (relative to the
// server base) into a temp file in dir, authenticated with the device token and
// using the Client's TLS/CA configuration. It bounds the body to the server's
// Content-Length and returns the temp file path and byte count; the caller
// verifies the SHA-256 before using it. Any failure removes the temp file.
//
// Defined on *Client in selfupdate.go (like downloadFile in deploy.go) so the
// AU1 download logic lives with the rest of self-update while reusing the
// Client's base URL, shared transport, device token and User-Agent. The device
// token rides the Authorization header only; it is never logged and never placed
// in the URL.
func (c *Client) downloadUpdate(ctx context.Context, rawURL, dir string) (string, int64, error) {
	full, err := c.resolveUpdateURL(rawURL)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", c.UserAgent())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	// A binary download can outlast the Client's 40 s long-poll timeout, so it
	// uses a client that shares the same transport (hence TLS/CA config) but
	// relies on the command context for its deadline.
	hc := &http.Client{Transport: c.http.Transport}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, &HTTPError{Status: resp.StatusCode}
	}
	if resp.ContentLength < 0 {
		return "", 0, errors.New("server did not send a Content-Length")
	}

	f, err := os.CreateTemp(dir, ".msp-update-*")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	// LimitReader bounds the bytes written to the declared length, so a server
	// that streams more than it promised cannot fill the disk.
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, resp.ContentLength))
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return "", 0, copyErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return "", 0, closeErr
	}
	if n != resp.ContentLength {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("got %d of %d bytes", n, resp.ContentLength)
	}
	return tmp, n, nil
}

// resolveUpdateURL turns the manifest's URL into an absolute URL on the server's
// own base. A bare path ("/dl/...") is joined to the base; an absolute URL is
// accepted only when its host matches the base, so the device token is never
// sent to a different host even if a server advertised one.
func (c *Client) resolveUpdateURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", errors.New("empty update url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse update url: %w", err)
	}
	if u.Scheme == "" && u.Host == "" {
		// Relative path: join to the server base.
		return strings.TrimRight(c.base, "/") + "/" + strings.TrimLeft(rawURL, "/"), nil
	}
	base, err := url.Parse(c.base)
	if err != nil {
		return "", fmt.Errorf("parse server base: %w", err)
	}
	if !strings.EqualFold(u.Host, base.Host) {
		return "", fmt.Errorf("refusing an update url on a different host than the server (%s)", u.Host)
	}
	return u.String(), nil
}

// ShouldApplyAdvertised decides whether an advertised sync-response update should
// be applied automatically: auto-update must be enabled, the block must validate,
// and its version must differ from the running version. The server already gates
// the latter; the agent re-checks. The integrator passes the config's auto-update
// setting as enabled (see the AutoUpdate config hook in the AU1 wiring notes).
func ShouldApplyAdvertised(enabled bool, upd *protocol.AgentUpdate, current string) bool {
	if !enabled || upd == nil {
		return false
	}
	if upd.Validate() != nil {
		return false
	}
	return upd.Version != current
}

// UpdateCommand turns an advertised auto-update into the kind update command the
// Updater applies, so the auto-update path reuses the same swap as a forced
// dashboard command. ID is 0 (there is no server command to report a result to).
func UpdateCommand(upd *protocol.AgentUpdate) protocol.Command {
	return protocol.Command{
		Kind:     protocol.KindUpdate,
		Version:  upd.Version,
		URL:      upd.URL,
		SHA256:   upd.SHA256,
		TimeoutS: protocol.MaxTimeoutS,
	}
}

// resolveExe resolves the running agent executable for the whole self-update
// machinery: the Updater's swap (through NewUpdater), the start-of-day
// BootstrapRollback and the FinalizeUpdate backup cleanup all go through it. It is
// a package variable, defaulting to resolveExecutable, so the cross-package e2e
// test can point every one of them at a temp "exe" path with
// SetResolveExecutableForTest and drive the real swap without replacing the real
// test binary or the container's agent. The agent's own tests inject per-Updater
// seams with newUpdaterWith instead and never touch this.
var resolveExe = resolveExecutable

// SetResolveExecutableForTest overrides how self-update resolves the running
// executable so an end-to-end test can target a temp "exe" path, and returns a
// function that restores the previous resolver. It must only be called from tests;
// production never calls it, so resolveExe stays resolveExecutable.
func SetResolveExecutableForTest(f func() (string, error)) (restore func()) {
	prev := resolveExe
	resolveExe = f
	return func() { resolveExe = prev }
}

// resolveExecutable returns the absolute, symlink-resolved path of the running
// agent binary. It is the production resolve seam for the Updater, and refuses a
// binary that appears to be a `go run` temp build (under the OS temp directory),
// where a self-update makes no sense and would be destructive. Tests inject their
// own resolve seam targeting a temp "exe" path, so this guard never trips them.
func resolveExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if tmp, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		if rel, err := filepath.Rel(tmp, exe); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("the running binary is under the temp directory (go run?)")
		}
	}
	return exe, nil
}

// regularReplaceable reports whether exe is a plain file we may replace, refusing
// a non-regular file (a device, a directory, a symlink target we cannot swap).
func regularReplaceable(exe string) error {
	fi, err := os.Lstat(exe)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("the running binary is not a regular file")
	}
	return nil
}

// hashFile returns the lowercase hex SHA-256 of the file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile copies src to dst with the given mode, writing to a temp file in the
// destination directory and renaming it into place so a half-written backup is
// never left behind.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".msp-bak-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// updateFailure is the result for an update command that could not complete; the
// old binary is always still in place when this is returned.
func updateFailure(started time.Time, msg string) protocol.CommandResult {
	return protocol.CommandResult{ExitCode: 1, StartedAt: started, FinishedAt: time.Now().UTC(), Error: msg}
}

// updateDone is the success result, carrying the old→new version in the log the
// dashboard shows in command history. note adds a short clause (e.g. a no-op
// repeat) when set.
func updateDone(started time.Time, from, to, note string) protocol.CommandResult {
	log := fmt.Sprintf("self-update applied: %s -> %s", from, to)
	if note != "" {
		log = fmt.Sprintf("self-update %s -> %s: %s", from, to, note)
	}
	return protocol.CommandResult{ExitCode: 0, StartedAt: started, FinishedAt: time.Now().UTC(), Stdout: log}
}
