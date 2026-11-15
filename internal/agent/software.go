package agent

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sxi9/msp/internal/protocol"
)

// Installed-software inventory (SI1). The desktop agent reports the OS's
// installed software per device and the server shows it and lets a technician
// search the fleet. Linux reads dpkg (apt) or, failing that, rpm; Windows reads
// the uninstall registry keys. Collection goes through a softwareLister, one
// implementation per platform (software_dpkg.go / software_windows.go /
// software_other.go). The dpkg/rpm calls run through the shared injectable
// commandExecutor (the same seam as the PM1 patch manager), so tests feed canned
// output and never touch the real package database. The registry read is pure in
// its parser (parseRegistry), which is unit-tested on Linux against a captured
// dump. Android keeps reporting apps (protocol.Facts.Apps); a device reports one
// or the other, never both.

// softwareLister enumerates the host's installed software for this platform. One
// implementation is selected by build tag through newPlatformSoftwareLister.
type softwareLister interface {
	// List returns the raw (unbounded, unsorted) inventory. A missing package
	// manager or an unreadable registry yields an empty list and a nil error, so
	// the agent still syncs; a nil error with a nil slice means "nothing found".
	List(ctx context.Context) ([]protocol.SoftwareItem, error)
}

// newSoftwareLister returns this platform's lister over the default os/exec
// executor; newPlatformSoftwareLister is defined per platform.
func newSoftwareLister(x commandExecutor) softwareLister { return newPlatformSoftwareLister(x) }

// SoftwareCollector produces the installed-software inventory the desktop agent
// attaches to its facts (SI1). It is the agent's integration surface for SI1:
// the sync loop calls Collect when it (re)sends facts and sets the result on
// Facts.Software, exactly as it sets Facts.Apps for the Android agent. Build the
// collector once (NewSoftwareCollector) alongside the metrics collector.
type SoftwareCollector struct {
	lister softwareLister
}

// NewSoftwareCollector builds the collector over this platform's lister.
func NewSoftwareCollector() *SoftwareCollector {
	return &SoftwareCollector{lister: newSoftwareLister(execRunner{})}
}

// newSoftwareCollectorWith injects a lister for tests.
func newSoftwareCollectorWith(l softwareLister) *SoftwareCollector {
	return &SoftwareCollector{lister: l}
}

// Collect returns the host's installed-software inventory, bounded to the
// protocol limits: each field is cut to its length bound, the list is sorted by
// name (case-insensitive, stable) and capped to protocol.MaxSoftware entries and
// protocol.MaxSoftwareBytes of serialised JSON so it cannot push the sync body
// over the agent body limit. A lister error yields an empty list (the sync still
// goes out). The result is nil when the host reports no software, so a facts
// payload omits the software key (omitempty) on a platform with no inventory.
func (c *SoftwareCollector) Collect(ctx context.Context) []protocol.SoftwareItem {
	items, _ := c.lister.List(ctx)
	return finalizeSoftware(items)
}

// finalizeSoftware normalises a raw inventory: it trims and length-bounds each
// field (dropping entries with no name), sorts by lower-cased name then version,
// caps the list at protocol.MaxSoftware entries and then trims it to the
// protocol.MaxSoftwareBytes serialised-JSON budget. It returns nil for an empty
// result so Facts.Software (omitempty) is omitted.
func finalizeSoftware(items []protocol.SoftwareItem) []protocol.SoftwareItem {
	out := make([]protocol.SoftwareItem, 0, len(items))
	for _, it := range items {
		name := cutBytes(strings.TrimSpace(it.Name), protocol.MaxSoftwareNameLen)
		if name == "" {
			continue
		}
		out = append(out, protocol.SoftwareItem{
			Name:      name,
			Version:   cutBytes(strings.TrimSpace(it.Version), protocol.MaxSoftwareVersionLen),
			Publisher: cutBytes(strings.TrimSpace(it.Publisher), protocol.MaxSoftwarePublisherLen),
			Source:    it.Source,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].Version < out[j].Version
	})
	if len(out) > protocol.MaxSoftware {
		out = out[:protocol.MaxSoftware]
	}
	out = capSoftwareBytes(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// capSoftwareBytes returns the longest prefix of items whose serialised JSON
// array stays within protocol.MaxSoftwareBytes.
func capSoftwareBytes(items []protocol.SoftwareItem) []protocol.SoftwareItem {
	return capSoftwareBytesTo(items, protocol.MaxSoftwareBytes)
}

// capSoftwareBytesTo returns the longest prefix of items whose serialised JSON
// array stays within limit bytes. It accounts for the enclosing brackets and the
// comma separators so the bound matches the wire size.
func capSoftwareBytesTo(items []protocol.SoftwareItem, limit int) []protocol.SoftwareItem {
	total := len("[]")
	for i, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			return items[:i]
		}
		add := len(b)
		if i > 0 {
			add++ // the comma before this entry
		}
		if total+add > limit {
			return items[:i]
		}
		total += add
	}
	return items
}

// boundSyncBody keeps a sync request's serialised body within the agent body
// budget by trimming, in order, the forwarded logs (WL1) and then the
// installed-software inventory (SI1) — the two largest and least time-sensitive
// parts of a facts-cadence sync. Each field is already bounded on its own
// (capSoftwareBytes, finalizePatchReport, the logs collector's own cap), but the
// server caps the whole /api/agent/ request at protocol.MaxAgentBodyBytes, and on
// the facts cadence the facts envelope, a fresh patch report, a near-maximum
// software list and a batch of forwarded logs can ride the same sync and together
// exceed it — which would 400 the sync before the heartbeat is recorded, bricking
// the device's sync.
//
// Logs are trimmed first (they are re-sent next sync, since the agent advances its
// journal/event-log cursor only after the server acks this sync), then software;
// the heartbeat, metrics and the patch report are never touched. Both slices keep
// their longest prefix that fits, so the agent must order its log batch
// oldest-first for the dropped (newest) entries to be re-sent. It returns the
// number of log and software entries dropped (0, 0 when nothing needed trimming).
func boundSyncBody(req *protocol.SyncRequest) (logsDropped, swDropped int) {
	if body, err := json.Marshal(req); err != nil || len(body) <= protocol.AgentBodyBudget {
		return 0, 0
	}

	// (1) Trim the forwarded logs. Logs is SyncRequest's last field, so with it
	// cleared (omitempty drops the key) the remaining headroom minus the "logs"
	// key overhead is what the array may occupy.
	if len(req.Logs) > 0 {
		fullLogs := req.Logs
		req.Logs = nil
		base, err := json.Marshal(req)
		if err != nil {
			logsDropped = len(fullLogs) // leave logs dropped; the heartbeat still gets through
		} else {
			const logsKeyOverhead = len(`,"logs":`)
			budget := protocol.AgentBodyBudget - len(base) - logsKeyOverhead
			if budget < len("[]") {
				logsDropped = len(fullLogs) // no room for any logs
			} else {
				kept := capLogBytesTo(fullLogs, budget)
				req.Logs = kept
				logsDropped = len(fullLogs) - len(kept)
			}
		}
		if body, err := json.Marshal(req); err == nil && len(body) <= protocol.AgentBodyBudget {
			return logsDropped, 0
		}
	}

	// (2) Still over budget: trim the installed-software inventory. The array is
	// Facts' last field, so it serialises as `,"software":[…]` inside the facts
	// object.
	if req.Facts == nil || len(req.Facts.Software) == 0 {
		return logsDropped, 0
	}
	full := req.Facts.Software
	req.Facts.Software = nil
	base, err := json.Marshal(req)
	if err != nil {
		return logsDropped, len(full) // leave software dropped; the heartbeat still gets through
	}
	const softwareKeyOverhead = len(`,"software":`)
	budget := protocol.AgentBodyBudget - len(base) - softwareKeyOverhead
	if budget < len("[]") {
		return logsDropped, len(full) // no room for any software
	}
	kept := capSoftwareBytesTo(full, budget)
	req.Facts.Software = kept
	return logsDropped, len(full) - len(kept)
}

// capLogBytesTo returns the longest prefix of entries whose serialised JSON array
// stays within limit bytes, accounting for the enclosing brackets and the comma
// separators so the bound matches the wire size (the capSoftwareBytesTo pattern).
func capLogBytesTo(entries []protocol.LogEntry, limit int) []protocol.LogEntry {
	total := len("[]")
	for i, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			return entries[:i]
		}
		add := len(b)
		if i > 0 {
			add++ // the comma before this entry
		}
		if total+add > limit {
			return entries[:i]
		}
		total += add
	}
	return entries
}

// cutBytes truncates s to at most max bytes without splitting a UTF-8 rune
// (Windows DisplayName/Publisher strings can be non-ASCII). Facts.Validate
// bounds the same fields in bytes.
func cutBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// parseDpkg parses `dpkg-query -W -f '${Package}\t${Version}\t${Maintainer}\n'`
// output into inventory entries (source dpkg). It is pure so it is unit-tested
// against captured dpkg output. The maintainer is the publisher, with a trailing
// "<email>" stripped. Lines without at least a name and version are skipped.
func parseDpkg(out string) []protocol.SoftwareItem {
	var items []protocol.SoftwareItem
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		name := strings.TrimSpace(f[0])
		if name == "" {
			continue
		}
		publisher := ""
		if len(f) >= 3 {
			publisher = cleanMaintainer(f[2])
		}
		items = append(items, protocol.SoftwareItem{
			Name:      name,
			Version:   strings.TrimSpace(f[1]),
			Publisher: publisher,
			Source:    protocol.SoftwareSourceDpkg,
		})
	}
	return items
}

// parseRpm parses `rpm -qa --qf '%{NAME}\t%{VERSION}-%{RELEASE}\t%{VENDOR}\n'`
// output into inventory entries (source rpm). "(none)" vendors (rpm's literal
// for an unset field) become an empty publisher. Pure, unit-tested against
// captured output.
func parseRpm(out string) []protocol.SoftwareItem {
	var items []protocol.SoftwareItem
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		name := strings.TrimSpace(f[0])
		if name == "" {
			continue
		}
		publisher := ""
		if len(f) >= 3 {
			publisher = strings.TrimSpace(f[2])
			if publisher == "(none)" {
				publisher = ""
			}
		}
		items = append(items, protocol.SoftwareItem{
			Name:      name,
			Version:   strings.TrimSpace(f[1]),
			Publisher: publisher,
			Source:    protocol.SoftwareSourceRPM,
		})
	}
	return items
}

// cleanMaintainer trims a dpkg Maintainer field to the name, dropping a trailing
// "<email>".
func cleanMaintainer(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// rawUninstallEntry is one Windows "uninstall" registry subkey's relevant
// values, decoded on Windows from the registry (software_windows.go) and in
// tests from a captured JSON dump. It feeds parseRegistry.
type rawUninstallEntry struct {
	DisplayName      string `json:"display_name"`
	DisplayVersion   string `json:"display_version"`
	Publisher        string `json:"publisher"`
	SystemComponent  int    `json:"system_component"`
	WindowsInstaller int    `json:"windows_installer"`
	ReleaseType      string `json:"release_type"`
	ParentKeyName    string `json:"parent_key_name"`
}

// parseRegistry turns decoded uninstall-key entries into inventory entries
// (source registry), applying the same selection "Programs and Features" uses:
// an entry must have a DisplayName, must not be a SystemComponent (hidden
// plumbing), must not be a child of another entry (ParentKeyName set), and must
// not be an OS/driver update (ReleaseType names an update/hotfix). Pure, so it is
// unit-tested on Linux against a captured dump.
func parseRegistry(entries []rawUninstallEntry) []protocol.SoftwareItem {
	var items []protocol.SoftwareItem
	for _, e := range entries {
		name := strings.TrimSpace(e.DisplayName)
		if name == "" {
			continue
		}
		if e.SystemComponent != 0 {
			continue
		}
		if strings.TrimSpace(e.ParentKeyName) != "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(e.ReleaseType)) {
		case "security update", "update", "hotfix":
			continue
		}
		items = append(items, protocol.SoftwareItem{
			Name:      name,
			Version:   strings.TrimSpace(e.DisplayVersion),
			Publisher: strings.TrimSpace(e.Publisher),
			Source:    protocol.SoftwareSourceRegistry,
		})
	}
	return items
}
