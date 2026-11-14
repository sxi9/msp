// Package server is the HTTP side of msp-server: the agent API (enroll, sync
// long-poll, command results), the staff session API (login, logout, me), the
// dashboard JSON API (registered by the dashboard_*.go files through
// dashboardRoutes) and the background jobs (alert evaluator, lost-command
// sweeper, pruner). Every database access goes through internal/store.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Config is the server's runtime configuration. cmd/msp-server fills it from
// environment variables and flags; tests fill it directly.
type Config struct {
	Listen        string // address for http.Server (default ":8080")
	BaseURL       string // public base URL used in install one-liners; "" = derive from the request
	TLSCert       string // PEM certificate path; TLS is served when TLSCert and TLSKey are both set
	TLSKey        string
	AgentDir      string        // directory served at /dl/ ("" = disabled)
	FilesDir      string        // directory holding the uploaded file library (SD1); files live at <FilesDir>/<id>
	MaxFileMB     int           // upload size cap in MiB for the file library (SD1; default 200)
	PresetsPath   string        // JSON command preset library replacing the builtin set ("" = builtin); re-read when its mtime changes
	RustdeskHost  string        // self-hosted RustDesk ID/relay server, bare hostname or IP (RD1); enabled only together with RustdeskKey
	RustdeskKey   string        // the relay's public key (content of id_ed25519.pub)
	SecureCookies bool          // force the Secure flag on the session cookie (behind a TLS proxy)
	TrustProxy    bool          // honour the first X-Forwarded-For address
	Version       string        // reported by /api/agent/v1/ping
	RetentionDays int           // heartbeat retention for the hourly pruner
	SampleEvery   time.Duration // minimum spacing of heartbeat rows per device
	LostGrace     time.Duration // sent_at + timeout_s + LostGrace < now => command lost
	OfflineGrace  time.Duration // reserved for a per-deployment offline margin (0 = protocol default)
	DiskPct       float64       // disk_full threshold (>=)
	MemPct        float64       // memory_high threshold (>=)
	BatteryPct    int           // battery_low threshold (<=, not charging)
}

// DefaultConfig returns the phase-1 defaults from docs/PLAN.md.
func DefaultConfig() Config {
	return Config{
		Listen:        ":8080",
		RetentionDays: 7,
		MaxFileMB:     200,
		SampleEvery:   60 * time.Second,
		LostGrace:     5 * time.Minute,
		DiskPct:       90,
		MemPct:        95,
		BatteryPct:    15,
	}
}

// Server holds the store, the configuration and the in-memory state shared by
// every handler: the long-poll notifier and the two per-IP rate limiters.
type Server struct {
	st          *store.Store
	cfg         Config
	log         *slog.Logger
	notifier    *notifier
	loginLimit  *rateLimiter
	enrollLimit *rateLimiter
	presets     *presetLibrary

	// shutdown is closed when Run starts shutting the listener down so that
	// every parked sync long-poll answers at once (http.Server.Shutdown does
	// not cancel in-flight request contexts; without this each parked sync
	// would run out the 10 s budget and Shutdown would fail).
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

// dashboardRoutes is appended to by dashboard.go's init(); Handler calls every
// registered function so the two halves of the package stay in separate files.
var dashboardRoutes []func(s *Server, mux *http.ServeMux)

// New wires a Server. A nil logger means slog.Default(). Zero durations and
// thresholds in cfg fall back to DefaultConfig so a partially filled Config
// (as the e2e test builds) still behaves.
func New(st *store.Store, cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	def := DefaultConfig()
	if cfg.Listen == "" {
		cfg.Listen = def.Listen
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = def.RetentionDays
	}
	if cfg.MaxFileMB <= 0 {
		cfg.MaxFileMB = def.MaxFileMB
	}
	if cfg.SampleEvery <= 0 {
		cfg.SampleEvery = def.SampleEvery
	}
	if cfg.LostGrace <= 0 {
		cfg.LostGrace = def.LostGrace
	}
	if cfg.DiskPct <= 0 {
		cfg.DiskPct = def.DiskPct
	}
	if cfg.MemPct <= 0 {
		cfg.MemPct = def.MemPct
	}
	if cfg.BatteryPct <= 0 {
		cfg.BatteryPct = def.BatteryPct
	}
	return &Server{
		st:          st,
		cfg:         cfg,
		log:         log,
		notifier:    newNotifier(),
		loginLimit:  newRateLimiter(10),
		enrollLimit: newRateLimiter(enrollPerMinute),
		presets:     newPresetLibrary(cfg.PresetsPath, log),
		shutdown:    make(chan struct{}),
	}
}

// enrollPerMinute is the per-IP enrollment budget. A client site usually sits
// behind one NAT address, so it must cover a room of machines being imaged
// in the same minute; 60/min still stops token guessing (a token has 256
// bits of entropy, guessing is hopeless at any rate).
const enrollPerMinute = 60

// beginShutdown releases every parked long-poll; it is idempotent.
func (s *Server) beginShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}

// Handler assembles the mux and wraps it with the middleware chain:
// recover -> request log -> security headers -> body limit -> mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Agent API (device bearer auth, no cookies, no CSRF).
	mux.HandleFunc("GET /api/agent/v1/ping", s.handlePing)
	mux.HandleFunc("POST /api/agent/v1/enroll", s.handleEnroll)
	mux.HandleFunc("POST /api/agent/v1/sync", s.deviceAuth(s.handleSync))
	mux.HandleFunc("POST /api/agent/v1/commands/{id}/result", s.deviceAuth(s.handleResult))
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Staff session API.
	mux.HandleFunc("POST /api/v1/login", s.csrf(s.handleLogin))
	mux.HandleFunc("POST /api/v1/logout", s.csrf(s.handleLogout))
	mux.Handle("GET /api/v1/me", s.sessionAuth(http.HandlerFunc(s.handleMe)))
	mux.Handle("POST /api/v1/me/password", s.sessionAuth(s.csrf(s.handleChangePassword)))

	for _, register := range dashboardRoutes {
		register(s, mux)
	}

	var h http.Handler = mux
	h = s.bodyLimit(h)
	h = s.securityHeaders(h)
	h = s.requestLog(h)
	h = s.recoverPanic(h)
	return h
}

// ValidateRustdesk reports an invalid RustDesk configuration: a host without
// a key or a key without a host, a host that is not a bare hostname or IP
// (a port, scheme or anything that would need quoting in the agents'
// RustDesk2.toml), or a key that is not one base64 line. Both empty means
// remote desktop is disabled; both set and well-formed enables it (RD1).
func (c Config) ValidateRustdesk() error {
	if (c.RustdeskHost == "") != (c.RustdeskKey == "") {
		return errors.New("MSP_RUSTDESK_HOST and MSP_RUSTDESK_KEY must be set together (both to enable remote desktop, neither to disable it)")
	}
	if c.RustdeskHost == "" {
		return nil
	}
	if err := (protocol.Rustdesk{Host: c.RustdeskHost, Key: c.RustdeskKey}).Validate(); err != nil {
		return fmt.Errorf("MSP_RUSTDESK_HOST / MSP_RUSTDESK_KEY: %w", err)
	}
	return nil
}

// rustdeskEnabled reports whether a relay is configured.
func (s *Server) rustdeskEnabled() bool {
	return s.cfg.RustdeskHost != "" && s.cfg.RustdeskKey != ""
}

// rustdeskInfo is the relay block for agent responses: nil when disabled.
func (s *Server) rustdeskInfo() *protocol.Rustdesk {
	if !s.rustdeskEnabled() {
		return nil
	}
	return &protocol.Rustdesk{Host: s.cfg.RustdeskHost, Key: s.cfg.RustdeskKey}
}

// Run serves until ctx is done, starting the background jobs alongside, then
// shuts the listener down gracefully (10 s). A configured presets file that
// does not load, or a half-configured RustDesk relay, is a startup error:
// nothing is served.
func (s *Server) Run(ctx context.Context) error {
	if err := s.cfg.ValidateRustdesk(); err != nil {
		return err
	}
	if err := s.presets.load(); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	jobsCtx, cancelJobs := context.WithCancel(ctx)
	defer cancelJobs()
	jobsDone := make(chan struct{})
	go func() {
		defer close(jobsDone)
		var wg sync.WaitGroup
		wg.Add(2)
		// Alert evaluator, lost-command sweeper and pruner.
		go func() {
			defer wg.Done()
			s.runJobs(jobsCtx)
		}()
		// NS1 notification dispatcher: drains pending deliveries on its own
		// ticker with the production sender, stopping when jobsCtx is cancelled
		// on shutdown.
		go func() {
			defer wg.Done()
			NewNotifyDispatcher(s, newStdSender()).Run(jobsCtx)
		}()
		wg.Wait()
	}()

	errc := make(chan error, 1)
	go func() {
		tls := s.cfg.TLSCert != "" && s.cfg.TLSKey != ""
		s.log.Info("listening", "addr", s.cfg.Listen, "tls", tls, "version", s.cfg.Version)
		var err error
		if tls {
			err = srv.ListenAndServeTLS(s.cfg.TLSCert, s.cfg.TLSKey)
		} else {
			err = srv.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		cancelJobs()
		<-jobsDone
		return err
	case <-ctx.Done():
	}

	s.beginShutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	cancelJobs()
	<-jobsDone
	if serveErr := <-errc; serveErr != nil && err == nil {
		err = serveErr
	}
	return err
}

// now is the clock every handler uses; it is the store's so tests can move
// both at once.
func (s *Server) now() time.Time {
	return s.st.Now()
}
