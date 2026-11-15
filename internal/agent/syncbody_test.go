package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// TestBoundSyncBodyWorstCase is the regression test for the SI1 review finding:
// a facts-cadence sync carrying a near-maximum installed-software inventory (SI1)
// together with a full patch report (PM1) and the metrics/facts envelope could
// exceed the server's /api/agent/ body limit (protocol.MaxAgentBodyBytes),
// deterministically 400ing the sync before the heartbeat is recorded and
// bricking the device. boundSyncBody must trim the software inventory so the
// whole body fits, while preserving the heartbeat, metrics and the patch report.
func TestBoundSyncBodyWorstCase(t *testing.T) {
	// A software inventory a real desktop would produce, capped by the production
	// finalizeSoftware to protocol.MaxSoftwareBytes (~192 KiB of software JSON).
	var rawSw []protocol.SoftwareItem
	for i := 0; i < 4000; i++ {
		rawSw = append(rawSw, protocol.SoftwareItem{
			Name:      fmt.Sprintf("libpackage-example-%04d-dev", i),
			Version:   "1:2.3.4-5ubuntu6.7",
			Publisher: "Ubuntu Developers",
			Source:    protocol.SoftwareSourceDpkg,
		})
	}
	sw := finalizeSoftware(rawSw)
	if n, _ := json.Marshal(sw); len(n) <= protocol.MaxSoftwareBytes-2048 {
		t.Fatalf("test setup: software list should fill the software byte budget, got %d bytes", len(n))
	}

	// A full pending-updates report (PM1) with realistic apt fields, capped by the
	// production finalizePatchReport to protocol.MaxPatchUpdates entries.
	var ups []protocol.PatchUpdate
	for i := 0; i < protocol.MaxPatchUpdates+50; i++ {
		ups = append(ups, protocol.PatchUpdate{
			Name:     fmt.Sprintf("linux-image-6.8.0-%03d-generic", i),
			Current:  "6.8.0-45.45",
			New:      "6.8.0-48.48",
			Security: i%2 == 0,
			Source:   "Ubuntu:24.04/noble-security",
		})
	}
	pr := finalizePatchReport(protocol.PatchReport{Manager: protocol.PatchManagerApt, Updates: ups}, time.Now().UTC())

	req := protocol.SyncRequest{
		AgentVersion: "1.2.3",
		IntervalS:    30,
		WaitS:        25,
		UptimeS:      123456,
		Metrics: protocol.Metrics{
			MemTotalMB: 16000, MemUsedMB: 8120,
			Disks: []protocol.Disk{{Mount: "/", TotalGB: 476, UsedGB: 201.3}},
			IPs:   []string{"10.0.0.5"},
		},
		Facts: &protocol.Facts{
			Kernel: "6.8.0-45-generic", CPUModel: "Intel(R) Core(TM) i5-1345U", CPUCount: 8,
			MemTotalMB: 16000, Manufacturer: "Dell Inc.", Model: "Latitude 5540", Serial: "ABC123",
			IPs: []string{"10.0.0.5"}, Software: sw,
		},
		PatchReport: &pr,
	}

	// Precondition: the untrimmed body really does blow the agent body limit, so
	// the trimming path is exercised (this is the bug that bricked the sync).
	before, err := json.Marshal(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) <= protocol.MaxAgentBodyBytes {
		t.Fatalf("test setup: worst-case body should exceed the limit, got %d <= %d", len(before), protocol.MaxAgentBodyBytes)
	}

	// Snapshot the patch report so we can prove boundSyncBody leaves it untouched
	// (Count stays the pre-cap total, Updates stays capped to MaxPatchUpdates).
	wantPatch := *req.PatchReport

	logsDropped, dropped := boundSyncBody(&req)
	if logsDropped != 0 {
		t.Fatalf("no logs to trim in this case, logsDropped=%d", logsDropped)
	}
	if dropped <= 0 {
		t.Fatalf("expected software to be trimmed, dropped=%d", dropped)
	}

	after, err := json.Marshal(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > protocol.MaxAgentBodyBytes {
		t.Fatalf("bounded body still over the server limit: %d > %d", len(after), protocol.MaxAgentBodyBytes)
	}

	// The heartbeat, metrics and the patch report must survive untouched; only the
	// software inventory is trimmed, and some of it still rides the sync.
	if req.AgentVersion != "1.2.3" || req.UptimeS != 123456 || len(req.Metrics.Disks) != 1 {
		t.Fatalf("heartbeat/metrics must be preserved: %+v", req)
	}
	if req.PatchReport == nil || len(req.PatchReport.Updates) != protocol.MaxPatchUpdates {
		t.Fatalf("patch report updates must stay capped at %d: %d", protocol.MaxPatchUpdates, len(req.PatchReport.Updates))
	}
	if req.PatchReport.Count != wantPatch.Count || req.PatchReport.SecurityCount != wantPatch.SecurityCount {
		t.Fatalf("patch report counts must be preserved untouched: got count=%d sec=%d want count=%d sec=%d",
			req.PatchReport.Count, req.PatchReport.SecurityCount, wantPatch.Count, wantPatch.SecurityCount)
	}
	if req.Facts == nil || len(req.Facts.Software) == 0 {
		t.Fatal("some software should remain after trimming")
	}
	if len(req.Facts.Software) >= len(sw) {
		t.Fatalf("software should have been trimmed: kept %d of %d", len(req.Facts.Software), len(sw))
	}
}

// TestBoundSyncBodyLeavesSmallSyncUntouched checks the common case: a sync whose
// body is already within budget is not modified and reports nothing dropped.
func TestBoundSyncBodyLeavesSmallSyncUntouched(t *testing.T) {
	// A facts sync with a small software list (well under the budget).
	small := protocol.SyncRequest{
		AgentVersion: "1.2.3",
		Facts: &protocol.Facts{
			Kernel: "6.8.0-45-generic",
			Software: []protocol.SoftwareItem{
				{Name: "curl", Version: "8.5.0", Publisher: "Ubuntu", Source: protocol.SoftwareSourceDpkg},
				{Name: "openssl", Version: "3.0.13", Publisher: "Ubuntu", Source: protocol.SoftwareSourceDpkg},
			},
		},
	}
	if logs, sw := boundSyncBody(&small); logs != 0 || sw != 0 {
		t.Fatalf("small facts sync must be left untouched, logsDropped=%d swDropped=%d", logs, sw)
	}
	if len(small.Facts.Software) != 2 {
		t.Fatalf("software must be preserved: %d entries", len(small.Facts.Software))
	}

	// A metrics-only sync (no facts, no software) is a no-op.
	heartbeat := protocol.SyncRequest{AgentVersion: "1.2.3", Metrics: protocol.Metrics{MemTotalMB: 16000}}
	if logs, sw := boundSyncBody(&heartbeat); logs != 0 || sw != 0 {
		t.Fatalf("metrics-only sync must be a no-op, logsDropped=%d swDropped=%d", logs, sw)
	}

	// A worst-case software list over the budget but with no patch report fits on
	// its own (software alone is capped under the budget), so nothing is trimmed.
	var rawSw []protocol.SoftwareItem
	for i := 0; i < 4000; i++ {
		rawSw = append(rawSw, protocol.SoftwareItem{
			Name: fmt.Sprintf("pkg-%04d-", i) + strings.Repeat("x", 30), Version: "1.0", Source: protocol.SoftwareSourceDpkg,
		})
	}
	softwareOnly := protocol.SyncRequest{Facts: &protocol.Facts{Kernel: "x", Software: finalizeSoftware(rawSw)}}
	kept := len(softwareOnly.Facts.Software)
	if logs, sw := boundSyncBody(&softwareOnly); logs != 0 || sw != 0 {
		t.Fatalf("software-only sync within budget must be untouched, logsDropped=%d swDropped=%d", logs, sw)
	}
	if len(softwareOnly.Facts.Software) != kept {
		t.Fatalf("software-only list changed: %d -> %d", kept, len(softwareOnly.Facts.Software))
	}
}
