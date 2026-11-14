package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Loop timing constants from the protocol.
const (
	backoffMin        = 5 * time.Second
	backoffMax        = 300 * time.Second
	unauthorizedSleep = 5 * time.Minute
	resultRetries     = 3
	resultRetryDelay  = 5 * time.Second
	factsEvery        = 120
	maxConcurrent     = 2
	// Enrollment is rate limited per client IP (60/min, refilled
	// continuously); a room of machines behind one NAT imaged at once can
	// hit it, so a 429 is retried rather than failing the install.
	enrollRetries    = 6
	enrollRetryDelay = 10 * time.Second
	// Self-update probation (AU1). After an update swaps a new binary in, that
	// binary has this long AND this many consecutive sync failures to complete
	// ONE successful sync before it is judged a bad release and rolled back to
	// <exe>.bak. BootstrapRollback already reverts a binary that cannot run its
	// own `version` subcommand; probation closes the gap the reviewer found — a
	// release that answers `version` (so it passes the pre-swap health check and
	// the bootstrap check) but can never sync (a broken request shape the server
	// 400s every time, a TLS/CA regression, a panic in the sync path) would
	// otherwise back off forever on the new binary and never roll back, bricking
	// the device. The window bounds the common "backs off forever" case; the
	// failure count lets a test with a no-wait sleep reach the rollback quickly.
	// Rolling back is the safe direction: the restored binary is the known-good
	// previous release, and if the new one was in fact fine the server simply
	// re-advertises it.
	updateProbationWindow      = 10 * time.Minute
	updateProbationMaxFailures = 8
)

// PatchReporter is the Run loop's patch surface (PM1): the periodic pending-
// updates report attached to a sync request, and the handler for a kind patch
// command. *PatchCollector implements it; NewPatchCollector builds the
// production one over this platform's package manager.
type PatchReporter interface {
	// Report returns the patch report to attach to the next sync, or nil when
	// none is due (between scans).
	Report(ctx context.Context, now time.Time) *protocol.PatchReport
	// RunPatch executes a kind patch command (scan or install) and returns its
	// result, like Execute for the other kinds.
	RunPatch(ctx context.Context, cmd protocol.Command) protocol.CommandResult
}

// newPatchReporter builds the PatchReporter the Run loop uses. It is a variable
// so the agent's own tests can substitute a reporter that never touches the real
// package manager; production builds NewPatchCollector. The cross-package e2e
// test instead drives the real collector against a fake apt-get on PATH.
var newPatchReporter = func() PatchReporter { return NewPatchCollector() }

// sleepFn pauses for d or until ctx is done; it returns false when ctx ended
// first. Tests replace it to run the loop without real waiting.
var sleepFn = func(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Enroll registers this host with the server and returns the config to save:
// it collects facts, the machine id, hostname, platform, arch and OS version,
// POSTs /enroll and fills in the device token and id.
func Enroll(ctx context.Context, serverURL, token, caFile string, insecureHTTP bool, intervalS int, version string) (Config, error) {
	cfg := Config{
		ServerURL:    serverURL,
		IntervalS:    protocol.ClampInterval(intervalS, runtime.GOOS),
		CAFile:       caFile,
		InsecureHTTP: insecureHTTP,
	}
	if token == "" {
		return cfg, errors.New("enrollment token is empty")
	}
	client, err := NewClient(cfg, version)
	if err != nil {
		return cfg, err
	}
	uid, err := machineID()
	if err != nil {
		return cfg, err
	}
	col := newCollector()
	req := protocol.EnrollRequest{
		EnrollmentToken: token,
		DeviceUID:       uid,
		Hostname:        hostname(),
		Platform:        platformName(),
		Arch:            arch(),
		OSVersion:       osVersion(),
		AgentVersion:    version,
		IntervalS:       cfg.IntervalS,
		Facts:           col.Facts(),
	}
	// Attach the installed-software inventory (SI1) so the device page is
	// populated from first contact; the collector is read-only (dpkg/rpm on
	// Linux, the Windows uninstall registry) and returns nil where there is
	// none, so Facts.Software is simply omitted on such a host.
	req.Facts.Software = NewSoftwareCollector().Collect(ctx)
	var resp protocol.EnrollResponse
	for attempt := 1; ; attempt++ {
		resp, err = client.Enroll(ctx, req)
		var he *HTTPError
		if err == nil || !errors.As(err, &he) || he.Status != http.StatusTooManyRequests || attempt > enrollRetries {
			break
		}
		slog.Warn("enrollment rate limited by the server, retrying", "attempt", attempt, "retry_in", enrollRetryDelay)
		if !sleepFn(ctx, enrollRetryDelay) {
			return cfg, fmt.Errorf("enroll: %w", ctx.Err())
		}
	}
	if err != nil {
		return cfg, fmt.Errorf("enroll: %w", err)
	}
	if resp.DeviceToken == "" || resp.DeviceID == 0 {
		return cfg, errors.New("enroll: server answered without a device token")
	}
	cfg.DeviceToken = resp.DeviceToken
	cfg.DeviceID = resp.DeviceID
	cfg.ServerURL = client.base
	cfg.setRustdesk(resp.Rustdesk)
	return cfg, nil
}

// Run is the agent's main loop until ctx is done (then it returns nil):
// sync with metrics (facts on the first sync and every 120th) -> run every
// returned command in a goroutine (at most two at once) and post its result
// -> re-sync at once when commands arrived, else sleep the remainder of
// poll_interval_s. Network/5xx errors back off 5 s doubling to 300 s; a 401
// logs "token rejected" and retries every 5 minutes; the agent never exits on
// its own.
func Run(ctx context.Context, cfg Config, version string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	client, err := NewClient(cfg, version)
	if err != nil {
		return err
	}
	col := newCollector()
	patchCol := newPatchReporter()
	// Software deployment (SD1): a kind deploy command downloads a library file
	// over this client (file source) or installs a package through the shared
	// executor (package source). The deployer is built once over the same Client
	// the sync loop uses, like patchCol above.
	deployer := NewDeployer(client)
	// Agent self-update (AU1): a kind update command (forced from the dashboard) or
	// an advertised auto-update is applied through the updater, which downloads,
	// verifies the hash, health-checks and atomically swaps the binary, keeping a
	// backup for rollback. Built once over the same Client, like deployer above.
	updater := NewUpdater(client)
	// Installed-software inventory (SI1): the desktop agent reports the host's
	// installed packages/programs on the same cadence as facts. The collector
	// runs dpkg/rpm (Linux) or reads the uninstall registry (Windows); every
	// call is read-only, and Collect returns nil on a host with no inventory so
	// Facts.Software (omitempty) is omitted there.
	swCol := NewSoftwareCollector()

	waitS := min(protocol.MaxWaitS, cfg.IntervalS-1)
	if waitS < 0 {
		waitS = 0
	}
	backoff := backoffMin
	syncs := 0 // successful syncs so far; facts go with sync 0, 120, 240, ...
	finalizedUpdate := false
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	defer wg.Wait()

	log.Info("agent started", "server", cfg.ServerURL, "device_id", cfg.DeviceID, "interval_s", cfg.IntervalS, "version", version)
	// Start-of-day self-update rollback (AU1): if a prior update left a backup and
	// the installed binary cannot run its own version subcommand, restore the backup
	// and exit so the service manager restarts into the known-good binary.
	if BootstrapRollback(ctx, version, log) {
		log.Warn("rolled back a failed self-update; exiting so the service restarts into the restored binary")
		return nil
	}
	// Self-update probation (AU1): if a prior update left a backup that
	// BootstrapRollback did NOT roll back, this binary is freshly swapped in and
	// not yet confirmed good — it answers `version` but has not yet proven it can
	// sync. It must complete one successful sync within updateProbationWindow /
	// updateProbationMaxFailures or be rolled back to <exe>.bak, so a release that
	// starts but cannot reach the server does not brick the device.
	onProbation := UpdateBackupPending()
	probationDeadline := time.Now().Add(updateProbationWindow)
	probationFailures := 0
	if onProbation {
		log.Info("self-update on probation; the new binary must sync to be confirmed",
			"within", updateProbationWindow, "max_failures", updateProbationMaxFailures)
	}
	// probationFail records one failed sync during probation and reports whether
	// the agent should now roll back and exit. Outside probation it is a no-op.
	probationFail := func(reason string) (rollback bool) {
		if !onProbation {
			return false
		}
		probationFailures++
		if probationFailures < updateProbationMaxFailures && !time.Now().After(probationDeadline) {
			return false
		}
		if RollbackNow(log) {
			log.Warn("self-update could not sync within its probation window; rolled back, exiting so the service restarts into the restored binary",
				"reason", reason, "failures", probationFailures)
			return true
		}
		// Nothing to roll back (or the restore failed): stop probation so the
		// agent keeps retrying rather than re-checking a rollback it cannot do.
		onProbation = false
		return false
	}
	for ctx.Err() == nil {
		// A self-update that swapped a new binary into place asks the agent to exit
		// so the service manager restarts into it; the deferred wg.Wait above has
		// already let the update's command result reach the server.
		if updater.ShouldRestart() {
			log.Info("restarting into the updated agent")
			return nil
		}
		metrics, merr := col.Metrics(ctx)
		if merr != nil {
			log.Warn("metrics unavailable", "err", merr)
		}
		req := protocol.SyncRequest{
			AgentVersion: version,
			IntervalS:    cfg.IntervalS,
			WaitS:        waitS,
			UptimeS:      col.Uptime(),
			Metrics:      metrics,
		}
		if syncs%factsEvery == 0 {
			f := col.Facts()
			f.Software = swCol.Collect(ctx)
			req.Facts = &f
		}
		// The patch report rides the sync when one is due (the first sync after
		// start, every patchInterval, and right after a patch command); the
		// collector returns nil otherwise so the field is omitted (PM1).
		if pr := patchCol.Report(ctx, time.Now().UTC()); pr != nil {
			req.PatchReport = pr
		}
		// Keep the whole sync body within the server's agent body limit: on the
		// facts cadence the facts envelope, a fresh patch report and a near-maximum
		// software inventory ride the same sync and together can exceed it, which
		// would 400 the sync before the heartbeat is recorded. Trim the software
		// inventory (never the heartbeat, metrics or patch report) to fit (SI1).
		if dropped := boundSyncBody(&req); dropped > 0 {
			log.Warn("installed-software inventory trimmed to fit the sync body budget",
				"dropped", dropped, "kept", len(req.Facts.Software), "budget_bytes", protocol.AgentBodyBudget)
		}

		t0 := time.Now()
		resp, err := client.Sync(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrUnauthorized) {
				log.Error("token rejected, re-enroll with msp-agent install", "err", err)
				if probationFail("token rejected") {
					return nil
				}
				if !sleepFn(ctx, unauthorizedSleep) {
					return nil
				}
				continue
			}
			log.Warn("sync failed", "err", err, "retry_in", backoff)
			if probationFail("sync failed") {
				return nil
			}
			if !sleepFn(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}
		backoff = backoffMin
		syncs++
		// A successful sync confirms a freshly swapped-in binary: probation ends
		// and FinalizeUpdate (below) clears the backup.
		onProbation = false

		// The relay the server reports is kept in the config file so
		// `msp-agent rustdesk setup` works without a re-enrollment; written
		// only when it changed.
		if r := resp.Rustdesk; r != nil && r.Validate() != nil {
			log.Warn("ignoring a malformed rustdesk relay from the server", "err", r.Validate())
		} else if cfg.setRustdesk(r) {
			log.Info("rustdesk relay updated by the server", "host", cfg.RustdeskHost)
			if cfg.Path != "" {
				if err := Save(cfg.Path, cfg); err != nil {
					log.Warn("saving the rustdesk relay to the config failed", "path", cfg.Path, "err", err)
				}
			}
		}

		// Self-update bookkeeping (AU1). After the first successful sync on this
		// binary, clear any leftover <exe>.bak: reaching the server is the signal
		// that an update (if one happened) took, so the rollback backup can go.
		if !finalizedUpdate {
			finalizedUpdate = true
			FinalizeUpdate(log)
		}
		// Auto-apply an advertised update when auto-update is enabled for this agent.
		// A forced dashboard command arrives as a normal command below; this is the
		// background path. RunUpdate posts no result (no command id), exactly like an
		// auto deploy, and ShouldRestart (top of the loop) picks up the swap.
		if ShouldApplyAdvertised(cfg.AutoUpdateEnabled(), resp.AgentUpdate, version) {
			res := updater.RunUpdate(ctx, UpdateCommand(resp.AgentUpdate))
			log.Info("auto-update applied", "log", res.Stdout, "err", res.Error)
		}

		for _, cmd := range resp.Commands {
			// A kind shell command (RS1, decision D3) is an interactive relay
			// that can live for hours; it is dispatched outside the two-slot
			// sem so it never starves the one-shot commands, and RunShell's own
			// atomic guard still enforces a single live shell per agent. The
			// server completes a shell command when the relay closes, so a
			// result is posted only for the two refuse cases (shell busy; PTY
			// could not open) that RunShell signals with refused == true.
			if cmd.Kind == protocol.KindShell {
				wg.Add(1)
				go func(cmd protocol.Command) {
					defer wg.Done()
					log.Info("command received", "id", cmd.ID, "kind", cmd.Kind)
					if res, refused := client.RunShell(ctx, cmd, log); refused {
						log.Info("command finished", "id", cmd.ID, "exit_code", res.ExitCode, "error", res.Error)
						postResult(ctx, client, cmd.ID, res, log)
					}
				}(cmd)
				continue
			}
			wg.Add(1)
			go func(cmd protocol.Command) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				log.Info("command received", "id", cmd.ID, "kind", cmd.Kind)
				// A kind patch command (PM1) scans or installs through the patch
				// collector (so the fresh report rides the next sync); a kind deploy
				// command (SD1) downloads+runs a file or installs a package through
				// the deployer; every other kind runs through Execute.
				var res protocol.CommandResult
				switch cmd.Kind {
				case protocol.KindPatch:
					res = patchCol.RunPatch(ctx, cmd)
				case protocol.KindDeploy:
					res = deployer.RunDeploy(ctx, cmd)
				case protocol.KindUpdate:
					// Agent self-update (AU1): download, verify, health-check and swap
					// the binary. RunUpdate never exits the process; postResult reports
					// old->new, and ShouldRestart (checked at the top of the loop) makes
					// the agent exit for the service manager to restart into it.
					res = updater.RunUpdate(ctx, cmd)
				default:
					res = Execute(ctx, cmd)
				}
				log.Info("command finished", "id", cmd.ID, "exit_code", res.ExitCode, "error", res.Error)
				postResult(ctx, client, cmd.ID, res, log)
			}(cmd)
		}
		if len(resp.Commands) > 0 {
			continue
		}

		poll := resp.PollIntervalS
		if poll < 1 {
			poll = 1
		}
		if poll > cfg.IntervalS {
			poll = cfg.IntervalS
		}
		remaining := time.Duration(poll)*time.Second - time.Since(t0)
		if remaining < 0 {
			remaining = 0
		}
		if !sleepFn(ctx, remaining) {
			return nil
		}
	}
	return nil
}

// postResult reports a result, retrying up to three times 5 s apart on
// transport errors; 404 (not sent to this device) and 409 (already finished)
// are final and stop the retries.
func postResult(ctx context.Context, client *Client, id int64, res protocol.CommandResult, log *slog.Logger) {
	for attempt := 1; attempt <= resultRetries; attempt++ {
		err := client.PostResult(ctx, id, res)
		if err == nil {
			return
		}
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 409) {
			log.Warn("result rejected", "id", id, "err", err)
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("posting result failed", "id", id, "attempt", attempt, "err", err)
		if attempt < resultRetries && !sleepFn(ctx, resultRetryDelay) {
			return
		}
	}
}

// Status renders the config for `msp-agent status`.
func Status(cfgPath string) (string, error) {
	cfg, err := Load(cfgPath)
	if err != nil {
		return "", err
	}
	s := fmt.Sprintf("config:    %s\nserver:    %s\ndevice id: %d\ninterval:  %ds\n",
		cfgPath, cfg.ServerURL, cfg.DeviceID, cfg.IntervalS)
	if cfg.CAFile != "" {
		s += fmt.Sprintf("ca file:   %s\n", cfg.CAFile)
	}
	if cfg.InsecureHTTP {
		s += "insecure http: yes\n"
	}
	if cfg.RustdeskHost != "" {
		s += fmt.Sprintf("rustdesk:  %s\n", cfg.RustdeskHost)
	}
	if cfg.DeviceToken == "" {
		s += "enrolled:  no\n"
	} else {
		s += "enrolled:  yes\n"
	}
	return s, nil
}
