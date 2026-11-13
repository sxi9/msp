package server

import (
	"context"
	"fmt"
	"time"

	"github.com/sxi9/msp/internal/store"
)

// Alert types and severities (schema CHECK constraints).
const (
	alertOffline    = "offline"
	alertDiskFull   = "disk_full"
	alertMemoryHigh = "memory_high"
	alertBatteryLow = "battery_low"

	severityLow    = "low"
	severityMedium = "medium"
	severityHigh   = "high"
)

var alertTypes = []string{alertOffline, alertDiskFull, alertMemoryHigh, alertBatteryLow}

// EvaluateNow runs one evaluator pass synchronously. It exists for the
// in-process e2e test; the jobs loop calls evaluateOnce on its own ticker.
func EvaluateNow(s *Server) {
	s.evaluateOnce(context.Background())
}

// evaluateOnce opens and resolves the four alert types for every device from
// its current state, then flips overdue sent commands to lost.
func (s *Server) evaluateOnce(ctx context.Context) {
	now := s.now()
	devices, err := s.st.ListDevices(ctx, store.DeviceFilter{})
	if err != nil {
		s.log.Error("evaluate: list devices", "err", err)
		return
	}
	for _, d := range devices {
		if d.Revoked {
			for _, typ := range alertTypes {
				s.resolve(ctx, d, typ)
			}
			continue
		}
		s.evaluateDevice(ctx, d, now)
	}

	lost, err := s.st.MarkLostCommands(ctx, s.cfg.LostGrace)
	if err != nil {
		s.log.Error("evaluate: mark lost commands", "err", err)
		return
	}
	for _, c := range lost {
		id := c.ID
		detail := map[string]any{"device_id": c.DeviceID, "kind": c.Kind, "timeout_s": c.TimeoutS}
		if c.SentAt != nil {
			detail["sent_at"] = c.SentAt.UTC().Format(time.RFC3339)
		}
		s.audit(ctx, "system", nil, &c.OrgID, "command.lost", "command", &id, "", detail)
		s.log.Warn("command lost", "command", c.ID, "device", c.DeviceID)
	}
}

func (s *Server) evaluateDevice(ctx context.Context, d store.Device, now time.Time) {
	switch d.Status {
	case "offline":
		since := "unknown"
		if d.LastSeenAt != nil {
			since = humanDuration(now.Sub(*d.LastSeenAt))
		}
		s.open(ctx, d, alertOffline, severityHigh, "no sync for "+since)
	case "online":
		s.resolve(ctx, d, alertOffline)
	}
	// A pending device (never synced) is neither online nor offline.

	if d.DiskUsedPct != nil && *d.DiskUsedPct >= s.cfg.DiskPct {
		s.open(ctx, d, alertDiskFull, severityMedium, fmt.Sprintf("disk %.1f%% used", *d.DiskUsedPct))
	} else {
		s.resolve(ctx, d, alertDiskFull)
	}
	if d.MemUsedPct != nil && *d.MemUsedPct >= s.cfg.MemPct {
		s.open(ctx, d, alertMemoryHigh, severityLow, fmt.Sprintf("memory %.1f%% used", *d.MemUsedPct))
	} else {
		s.resolve(ctx, d, alertMemoryHigh)
	}
	charging := d.BatteryCharging != nil && *d.BatteryCharging
	if d.BatteryPct != nil && *d.BatteryPct <= s.cfg.BatteryPct && !charging {
		s.open(ctx, d, alertBatteryLow, severityLow, fmt.Sprintf("battery at %d%%, not charging", *d.BatteryPct))
	} else {
		s.resolve(ctx, d, alertBatteryLow)
	}
}

func (s *Server) open(ctx context.Context, d store.Device, typ, severity, message string) {
	created, err := s.st.OpenAlert(ctx, d.OrgID, d.ID, typ, severity, message)
	if err != nil {
		s.log.Error("open alert", "device", d.ID, "type", typ, "err", err)
		return
	}
	if created {
		s.log.Info("alert opened", "device", d.ID, "hostname", d.Hostname, "type", typ, "message", message)
	}
}

func (s *Server) resolve(ctx context.Context, d store.Device, typ string) {
	if err := s.st.ResolveAlert(ctx, d.ID, typ); err != nil {
		s.log.Error("resolve alert", "device", d.ID, "type", typ, "err", err)
	}
}

// humanDuration renders "45s", "6m", "3h" or "2d" for alert messages.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
