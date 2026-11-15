package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// Server-side WL1 glue: the service_down alert evaluator, the sync-response watch
// list, and the sync/enroll persistence of reported service states + forwarded
// logs.
//
// Integration (api_agent.go and alerts.go are owned by WP-INTEGRATE — see the
// drift note in this work package's report):
//   - alerts.go: add alertServiceDown to the alertTypes slice (so the revoked
//     resolve loop, and knownAlertType in the notify/automation filters, accept
//     it), and call s.evaluateWatchlists(ctx, pass, devices, now) from
//     evaluateOnce, after the per-device loop (it shares the same evalPass
//     snapshot).
//   - api_agent.go handleSync: resp.Watch = s.ComputeWatchForDevice(ctx, dev);
//     s.RecordSyncWatchData(ctx, dev, req.Facts, req.Logs); and, beside
//     Facts.Validate/PatchReport.Validate, if err := protocol.ValidateLogs(req.Logs)
//     to 400 a bad log batch.
//   - api_agent.go handleEnroll: s.RecordSyncWatchData(ctx, dev, &req.Facts, nil)
//     (EnrollRequest carries no logs).

// alertServiceDown is the WL1 alert type: a device has at least one alert=1
// watchlist item whose latest reported state is stopped. It is opened/resolved
// through the same s.open/s.resolve machinery as the other alert types, so it feeds
// the NS1 notifications and the AUT1 triggered automations unchanged. The alerts
// table CHECK already lists it (migration 0015).
const alertServiceDown = "service_down"

// evaluateWatchlists opens and resolves the service_down alert for every device in
// one evaluator pass. It mirrors evaluateDevice: it is called by evaluateOnce
// (alerts.go) with the shared evalPass snapshot and device list, so a device whose
// state does not change costs no write. A revoked device (and any android device,
// which has no watchlist) always resolves.
//
// A device is "service down" when at least one of its applicable alert=1 watchlist
// items (its org's org-scoped items plus its own device-scoped items) has a latest
// reported state of stopped. An item with no reported state (never reported, or not
// in the last facts.services) is treated as unknown, not down, so an alert fires
// only on an observed stopped service.
func (s *Server) evaluateWatchlists(ctx context.Context, pass *evalPass, devices []store.Device, now time.Time) {
	// One query per pass: all alerting items, bucketed by scope. The evaluator runs
	// every 15 s for every device, so this keeps it to a single read.
	items, err := s.st.ListWatchlistItems(ctx, store.WatchlistFilter{})
	if err != nil {
		s.log.Error("evaluate watchlists: list items", "err", err)
		return
	}
	orgItems := map[int64][]store.WatchlistItem{}
	devItems := map[int64][]store.WatchlistItem{}
	for _, it := range items {
		if !it.Alert {
			continue
		}
		switch it.Scope {
		case "org":
			if it.OrgID != nil {
				orgItems[*it.OrgID] = append(orgItems[*it.OrgID], it)
			}
		case "device":
			if it.DeviceID != nil {
				devItems[*it.DeviceID] = append(devItems[*it.DeviceID], it)
			}
		}
	}
	for _, d := range devices {
		if d.Revoked || d.Platform == protocol.PlatformAndroid {
			s.resolve(ctx, pass, d, alertServiceDown)
			continue
		}
		applicable := make([]store.WatchlistItem, 0, len(orgItems[d.OrgID])+len(devItems[d.ID]))
		applicable = append(applicable, orgItems[d.OrgID]...)
		applicable = append(applicable, devItems[d.ID]...)
		down := downWatchItems(applicable, d.ServicesFor())
		if len(down) > 0 {
			s.open(ctx, pass, d, alertServiceDown, severityMedium, serviceDownMessage(down))
		} else {
			s.resolve(ctx, pass, d, alertServiceDown)
		}
	}
}

// downWatchItems returns the names (deduplicated, sorted) of the alert=1 items that
// the device last reported as stopped. An item is down only when a matching
// (kind,name) reported state exists and is not running; an unknown item is skipped.
func downWatchItems(items []store.WatchlistItem, states []protocol.ServiceState) []string {
	running := make(map[string]bool, len(states))
	present := make(map[string]bool, len(states))
	for _, st := range states {
		k := st.Kind + "\x00" + st.Name
		present[k] = true
		running[k] = st.Running
	}
	seen := map[string]bool{}
	var down []string
	for _, it := range items {
		k := it.Kind + "\x00" + it.Name
		if !present[k] || running[k] {
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		down = append(down, it.Name)
	}
	sort.Strings(down)
	return down
}

// serviceDownMessage is the human-readable alert message naming the stopped
// service(s). down is non-empty and already sorted.
func serviceDownMessage(down []string) string {
	if len(down) == 1 {
		return down[0] + " is not running"
	}
	return fmt.Sprintf("%d watched services not running: %s", len(down), strings.Join(down, ", "))
}

// EvaluateWatchlistsNow runs one standalone service_down evaluation pass (loading
// the device list and open-alert snapshot itself). It exists for the in-process
// tests and the e2e harness, mirroring EvaluateNow; the jobs loop reaches the same
// code through evaluateOnce -> evaluateWatchlists on its own ticker.
func EvaluateWatchlistsNow(s *Server) {
	ctx := context.Background()
	devices, err := s.st.ListDevices(ctx, store.DeviceFilter{})
	if err != nil {
		s.log.Error("evaluate watchlists: list devices", "err", err)
		return
	}
	open, err := s.st.OpenAlertTypes(ctx)
	if err != nil {
		s.log.Error("evaluate watchlists: open alerts", "err", err)
		return
	}
	pass := &evalPass{open: open}
	s.evaluateWatchlists(ctx, pass, devices, s.now())
}

// ComputeWatchForDevice returns the device's applicable watchlist for the sync
// response's Watch field (WL1): the union of its org's org-scoped items and its own
// device-scoped items (store.WatchlistForDevice does the dedup + cap; android
// yields nil). The result is self-checked with protocol.ValidateWatch and dropped
// (nil) on a store error or an invalid list, so a bad item never rides the sync
// response. The sync handler assigns resp.Watch = s.ComputeWatchForDevice(...).
func (s *Server) ComputeWatchForDevice(ctx context.Context, d store.Device) []protocol.WatchItem {
	watch, err := s.st.WatchlistForDevice(ctx, d)
	if err != nil {
		s.log.Error("compute watch for device", "device", d.ID, "err", err)
		return nil
	}
	if err := protocol.ValidateWatch(watch); err != nil {
		s.log.Error("compute watch for device: invalid watch list", "device", d.ID, "err", err)
		return nil
	}
	return watch
}

// RecordSyncWatchData persists the WL1 parts of a sync/enroll request: the reported
// service states (facts.services -> RecordServices) and the forwarded log batch
// (req.Logs -> AppendLogs). Like RecordSoftware/RecordPatches, a failure here is
// logged but never fails the heartbeat, so a watchlist/log problem does not stop the
// agent syncing. A sync without facts, or whose facts carry no services, leaves the
// last stored state untouched; an empty log batch is a no-op. The caller has already
// run protocol.ValidateLogs on the batch for the 400 path.
func (s *Server) RecordSyncWatchData(ctx context.Context, dev store.Device, facts *protocol.Facts, logs []protocol.LogEntry) {
	if facts != nil && len(facts.Services) > 0 {
		if err := s.st.RecordServices(ctx, dev.ID, facts.Services, s.st.Now()); err != nil {
			s.log.Error("record services", "device", dev.ID, "err", err)
		}
	}
	if len(logs) > 0 {
		if err := s.st.AppendLogs(ctx, dev.ID, dev.OrgID, logs); err != nil {
			s.log.Error("append logs", "device", dev.ID, "err", err)
		}
	}
}
