package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/sxi9/msp/internal/protocol"
)

// Service/process watchlists (WL1, desktops only). The server delivers the
// device's applicable watchlist in the sync response's watch list; the agent
// probes each item's running state and reports it in the next sync's
// facts.services, from which the server opens/resolves a service_down alert.
// Linux uses `systemctl is-active` for services and `pgrep` for processes;
// Windows queries the service control manager and a process snapshot. Every
// probe goes through the shared injectable commandExecutor (the PM1 patch seam,
// patch.go), so no test touches real services, and the pure parsers
// (parseIsActive and parseWinServiceStates) are unit-tested on Linux against
// captured fixtures. Android has no watchlist, so the collector reports nothing
// there (Collect returns nil for an empty watch list).

// serviceProber reports the running state of watched services/processes for this
// build's platform. One implementation is selected by build tag through
// newPlatformServiceProber (services_linux.go / services_windows.go /
// services_other.go) over the shared commandExecutor.
type serviceProber interface {
	// Probe returns the running state of each watch item, one ServiceState per
	// item in the same order. A probe that cannot be performed (tool missing,
	// query error) reports the item as not running rather than failing the whole
	// collection, so a transient probe failure surfaces as a service_down the
	// server resolves once the probe works again.
	Probe(ctx context.Context, items []protocol.WatchItem) []protocol.ServiceState
}

// newServiceProber returns this platform's prober over the default os/exec
// executor; newPlatformServiceProber is defined per platform.
func newServiceProber(x commandExecutor) serviceProber { return newPlatformServiceProber(x) }

// ServiceCollector probes the device's applicable watchlist and produces the
// facts.services the desktop agent attaches on the facts cadence (WL1). It is the
// agent's integration surface for the watchlist half of WL1: the sync loop stores
// the server-delivered watch list with SetWatch each time a sync response arrives,
// and calls Collect when it (re)sends facts, setting the result on Facts.Services
// — exactly as the SI1 SoftwareCollector sets Facts.Software. Build it once
// (NewServiceCollector) alongside the other collectors.
type ServiceCollector struct {
	prober serviceProber

	mu    sync.Mutex
	watch []protocol.WatchItem
}

// NewServiceCollector builds the collector over this platform's prober.
func NewServiceCollector() *ServiceCollector {
	return &ServiceCollector{prober: newServiceProber(execRunner{})}
}

// newServiceCollectorWith injects a prober for tests.
func newServiceCollectorWith(p serviceProber) *ServiceCollector {
	return &ServiceCollector{prober: p}
}

// SetWatch stores the device's applicable watchlist as delivered on a sync
// response (SyncResponse.Watch); the next Collect probes it. Each item is trimmed
// and revalidated (the server already bounds the list with ValidateWatch, this is
// defence in depth) and the list is capped at protocol.MaxWatchItems. The slice is
// copied so the caller may reuse its own. An empty list clears the watch, so the
// next Collect returns nil and Facts.Services is omitted.
func (c *ServiceCollector) SetWatch(items []protocol.WatchItem) {
	cleaned := make([]protocol.WatchItem, 0, len(items))
	for _, w := range items {
		item := protocol.WatchItem{Kind: w.Kind, Name: strings.TrimSpace(w.Name)}
		if item.Validate() != nil {
			continue
		}
		cleaned = append(cleaned, item)
		if len(cleaned) >= protocol.MaxWatchItems {
			break
		}
	}
	c.mu.Lock()
	c.watch = cleaned
	c.mu.Unlock()
}

// Collect probes the stored watchlist and returns the running state of each item
// (WL1), bounded to protocol.MaxServices entries. It returns nil when the device
// has no watchlist, so Facts.Services (omitempty) is omitted. The probed names and
// states are never written to the agent's own log.
func (c *ServiceCollector) Collect(ctx context.Context) []protocol.ServiceState {
	c.mu.Lock()
	watch := c.watch
	c.mu.Unlock()
	if len(watch) == 0 {
		return nil
	}
	states := c.prober.Probe(ctx, watch)
	if len(states) == 0 {
		return nil
	}
	if len(states) > protocol.MaxServices {
		states = states[:protocol.MaxServices]
	}
	return states
}

// ServiceUnits returns the names of the stored watchlist's service-kind items, for
// the agent to pass to the LogCollector: log forwarding is bounded to the device's
// watched service units (WL1). Returns nil when the device watches no services.
func (c *ServiceCollector) ServiceUnits() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var units []string
	for _, w := range c.watch {
		if w.Kind == protocol.WatchKindService {
			units = append(units, w.Name)
		}
	}
	return units
}

// parseIsActive maps the lines of `systemctl is-active <u1> <u2> ...` to a running
// bool per unit, in order. systemctl prints one status word per unit — "active"
// when running, else "inactive" / "failed" / "activating" / "deactivating" /
// "unknown" — one line each, oldest argument first, even though its own exit code
// is non-zero when any unit is not active. Only "active" counts as running
// (transitional states are treated as down). It is pure, so it is unit-tested
// against captured systemctl output. A short output (systemctl errored before
// printing every line) leaves the missing units not running.
func parseIsActive(out string, n int) []bool {
	res := make([]bool, n)
	i := 0
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if i >= n {
			break
		}
		res[i] = s == "active"
		i++
	}
	return res
}

// winServiceSnapshot is the JSON a Windows service/process snapshot script emits:
// a map of lower-cased service name -> running and lower-cased process base name
// -> running. Kept in the shared file (not services_windows.go) so parseWinServiceStates
// is compiled and unit-tested on Linux against a captured fixture.
type winServiceSnapshot struct {
	Services  map[string]bool `json:"services"`
	Processes map[string]bool `json:"processes"`
	Error     string          `json:"error"`
}

// parseWinServiceStates maps a Windows snapshot back onto the watch items, in
// order, producing one ServiceState per item. Service and process names are
// matched case-insensitively (Windows names are); a process watch name is matched
// against the base name both as given and with a trailing ".exe" trimmed
// (Get-Process reports "notepad", a watch may say "notepad.exe"). A nil/garbled
// snapshot leaves every item not running. Pure, unit-tested on Linux.
func parseWinServiceStates(data []byte, items []protocol.WatchItem) []protocol.ServiceState {
	var snap winServiceSnapshot
	if len(data) > 0 {
		_ = json.Unmarshal(data, &snap)
	}
	states := make([]protocol.ServiceState, len(items))
	for i, it := range items {
		s := protocol.ServiceState{Kind: it.Kind, Name: it.Name}
		name := strings.ToLower(it.Name)
		switch it.Kind {
		case protocol.WatchKindService:
			s.Running = snap.Services[name]
		case protocol.WatchKindProcess:
			s.Running = snap.Processes[name] || snap.Processes[strings.TrimSuffix(name, ".exe")]
		}
		states[i] = s
	}
	return states
}

// notRunning returns a ServiceState slice reporting every item not running, the
// safe default when a whole probe could not run.
func notRunning(items []protocol.WatchItem) []protocol.ServiceState {
	states := make([]protocol.ServiceState, len(items))
	for i, it := range items {
		states[i] = protocol.ServiceState{Kind: it.Kind, Name: it.Name}
	}
	return states
}
