package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sxi9/msp/internal/protocol"
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

// evalPass carries the open-alert snapshot of one evaluator pass so that a
// device whose state did not change costs no write statement at all (the
// evaluator runs every 15 s for every device; with SQLite's single writer
// those idle writes would otherwise compete with every agent sync).
type evalPass struct {
	open map[int64]map[string]bool
}

func (p *evalPass) isOpen(deviceID int64, typ string) bool {
	return p.open[deviceID][typ]
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
	open, err := s.st.OpenAlertTypes(ctx)
	if err != nil {
		s.log.Error("evaluate: open alerts", "err", err)
		return
	}
	pass := &evalPass{open: open}
	for _, d := range devices {
		if d.Revoked {
			for _, typ := range alertTypes {
				s.resolve(ctx, pass, d, typ)
			}
			continue
		}
		s.evaluateDevice(ctx, pass, d, now)
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

func (s *Server) evaluateDevice(ctx context.Context, pass *evalPass, d store.Device, now time.Time) {
	switch d.Status {
	case "offline":
		since := "unknown"
		if d.LastSeenAt != nil {
			since = humanDuration(now.Sub(*d.LastSeenAt))
		}
		s.open(ctx, pass, d, alertOffline, severityHigh, "no sync for "+since)
	case "online":
		s.resolve(ctx, pass, d, alertOffline)
	}
	// A pending device (never synced) is neither online nor offline.

	if d.DiskUsedPct != nil && *d.DiskUsedPct >= s.cfg.DiskPct {
		s.open(ctx, pass, d, alertDiskFull, severityMedium, diskMessage(d))
	} else {
		s.resolve(ctx, pass, d, alertDiskFull)
	}
	if d.MemUsedPct != nil && *d.MemUsedPct >= s.cfg.MemPct {
		s.open(ctx, pass, d, alertMemoryHigh, severityLow, fmt.Sprintf("memory %.1f%% used", *d.MemUsedPct))
	} else {
		s.resolve(ctx, pass, d, alertMemoryHigh)
	}
	charging := d.BatteryCharging != nil && *d.BatteryCharging
	if d.BatteryPct != nil && *d.BatteryPct <= s.cfg.BatteryPct && !charging {
		s.open(ctx, pass, d, alertBatteryLow, severityLow, fmt.Sprintf("battery at %d%%, not charging", *d.BatteryPct))
	} else {
		s.resolve(ctx, pass, d, alertBatteryLow)
	}
}

// diskMessage names the filesystem that tripped the threshold ("disk 92.5%
// used on /data") by re-deriving it from the device's last metrics sample;
// without a sample the message carries the percentage alone.
func diskMessage(d store.Device) string {
	pct := *d.DiskUsedPct
	var m protocol.Metrics
	if d.LastMetricsJSON != "" && json.Unmarshal([]byte(d.LastMetricsJSON), &m) == nil {
		if v, mount, ok := protocol.DiskUsedPct(m.Disks); ok && mount != "" {
			return fmt.Sprintf("disk %.1f%% used on %s", v, mount)
		}
	}
	return fmt.Sprintf("disk %.1f%% used", pct)
}

func (s *Server) open(ctx context.Context, pass *evalPass, d store.Device, typ, severity, message string) {
	if pass.isOpen(d.ID, typ) {
		return
	}
	created, err := s.st.OpenAlert(ctx, d.OrgID, d.ID, typ, severity, message)
	if err != nil {
		s.log.Error("open alert", "device", d.ID, "type", typ, "err", err)
		return
	}
	if created {
		s.log.Info("alert opened", "device", d.ID, "hostname", d.Hostname, "type", typ, "message", message)
		// NS1: enqueue notifications for the newly opened alert. Load the row so
		// the enqueue has its id, severity and message (OpenAlert returns only a
		// created flag).
		if alert, err := s.st.GetOpenAlert(ctx, d.ID, typ); err == nil {
			s.EnqueueAlertNotifications(ctx, alert, store.EventOpened)
		} else {
			s.log.Error("notify enqueue: load opened alert", "device", d.ID, "type", typ, "err", err)
		}
	}
}

func (s *Server) resolve(ctx context.Context, pass *evalPass, d store.Device, typ string) {
	if !pass.isOpen(d.ID, typ) {
		return
	}
	// NS1: capture the alert row before it is closed so a resolved-event
	// notification can be enqueued with its id, severity and message.
	alert, aerr := s.st.GetOpenAlert(ctx, d.ID, typ)
	if err := s.st.ResolveAlert(ctx, d.ID, typ); err != nil {
		s.log.Error("resolve alert", "device", d.ID, "type", typ, "err", err)
		return
	}
	if aerr != nil {
		s.log.Error("notify enqueue: load resolved alert", "device", d.ID, "type", typ, "err", aerr)
		return
	}
	s.EnqueueAlertNotifications(ctx, alert, store.EventResolved)
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
