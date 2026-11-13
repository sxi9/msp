package server

import (
	"context"
	"runtime/debug"
	"time"
)

const (
	evaluateEvery = 15 * time.Second
	pruneEvery    = time.Hour
)

// runJobs drives the periodic work until ctx is done: the alert evaluator and
// lost-command sweeper every 15 s, the heartbeat and session pruner hourly.
// Each pass is isolated by recover so one bad pass never stops the loop.
func (s *Server) runJobs(ctx context.Context) {
	evaluate := time.NewTicker(evaluateEvery)
	defer evaluate.Stop()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-evaluate.C:
			s.safely("evaluate", func() { s.evaluateOnce(ctx) })
		case <-prune.C:
			s.safely("prune", func() { s.pruneOnce(ctx) })
		}
	}
}

// pruneOnce deletes heartbeats older than the retention window and expired
// sessions.
func (s *Server) pruneOnce(ctx context.Context) {
	retention := time.Duration(s.cfg.RetentionDays) * 24 * time.Hour
	n, err := s.st.PruneHeartbeats(ctx, retention)
	if err != nil {
		s.log.Error("prune heartbeats", "err", err)
	} else if n > 0 {
		s.log.Info("pruned heartbeats", "rows", n, "retention_days", s.cfg.RetentionDays)
	}
	m, err := s.st.PruneSessions(ctx)
	if err != nil {
		s.log.Error("prune sessions", "err", err)
	} else if m > 0 {
		s.log.Info("pruned sessions", "rows", m)
	}
}

// safely runs fn, logging a panic instead of crashing the process.
func (s *Server) safely(name string, fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("job panic", "job", name, "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	fn()
}
