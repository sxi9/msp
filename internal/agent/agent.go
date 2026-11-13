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
)

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

	waitS := min(protocol.MaxWaitS, cfg.IntervalS-1)
	if waitS < 0 {
		waitS = 0
	}
	backoff := backoffMin
	syncs := 0 // successful syncs so far; facts go with sync 0, 120, 240, ...
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	defer wg.Wait()

	log.Info("agent started", "server", cfg.ServerURL, "device_id", cfg.DeviceID, "interval_s", cfg.IntervalS, "version", version)
	for ctx.Err() == nil {
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
			req.Facts = &f
		}

		t0 := time.Now()
		resp, err := client.Sync(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrUnauthorized) {
				log.Error("token rejected, re-enroll with msp-agent install", "err", err)
				if !sleepFn(ctx, unauthorizedSleep) {
					return nil
				}
				continue
			}
			log.Warn("sync failed", "err", err, "retry_in", backoff)
			if !sleepFn(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}
		backoff = backoffMin
		syncs++

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
				res := Execute(ctx, cmd)
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
