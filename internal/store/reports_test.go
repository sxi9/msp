package store

import (
	"context"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// seedHB inserts one heartbeat row at a given time with an optional disk reading
// (nil leaves disk_used_pct NULL). Reports read this table directly.
func seedHB(t *testing.T, s *Store, deviceID int64, at time.Time, disk *float64) {
	t.Helper()
	var d any
	if disk != nil {
		d = *disk
	}
	if _, err := s.db.Exec(
		`INSERT INTO heartbeats (device_id, at, cpu_pct, mem_used_pct, disk_used_pct, battery_pct, uptime_s)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		deviceID, fmtTime(at), 10.0, 20.0, d, nil, 3600); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
}

// setEnrolled backdates a device's enrolled_at so proration can be exercised.
func setEnrolled(t *testing.T, s *Store, deviceID int64, at time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE devices SET enrolled_at = ?, interval_s = ? WHERE id = ?`,
		fmtTime(at), 3600, deviceID); err != nil {
		t.Fatalf("set enrolled: %v", err)
	}
}

// rptDevice enrolls a linux device in org (mkDevice lives in jobs_test.go).
func rptDevice(t *testing.T, s *Store, orgID int64, uid, hostname string) Device {
	t.Helper()
	return mkDevice(t, s, orgID, uid, hostname, protocol.PlatformLinux)
}

func fptr(f float64) *float64 { return &f }

// TestUptimeReportAggregates proves samples/expected/uptime_pct, the org mean,
// and mid-period proration: device B, enrolled halfway through the window, owes
// only half the samples device A does, so its 6 samples are 50% (not 25%).
func TestUptimeReportAggregates(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, _, _, _ := fixture(t, s)
	now := c.now()
	since := now.Add(-24 * time.Hour)

	// Device A: enrolled before the window (full 24h period), interval 1h ->
	// expected 24. Seed 12 hourly samples -> 50%.
	devA := rptDevice(t, s, org.ID, "uid-a", "ws-a")
	setEnrolled(t, s, devA.ID, now.Add(-48*time.Hour))
	for i := 1; i <= 12; i++ {
		seedHB(t, s, devA.ID, now.Add(-time.Duration(i)*time.Hour), fptr(40))
	}

	// Device B: enrolled 12h ago (mid-window), interval 1h -> prorated expected
	// 12. Seed 6 hourly samples (all after enrollment) -> 50%, proving proration.
	devB := rptDevice(t, s, org.ID, "uid-b", "ws-b")
	setEnrolled(t, s, devB.ID, now.Add(-12*time.Hour))
	for i := 1; i <= 6; i++ {
		seedHB(t, s, devB.ID, now.Add(-time.Duration(i)*time.Hour), fptr(55))
	}

	rep, err := s.UptimeReport(ctx, nil, nil, 1)
	if err != nil {
		t.Fatalf("UptimeReport: %v", err)
	}
	if rep.Days != 1 || !rep.Since.Equal(since) {
		t.Fatalf("window: days=%d since=%v want since=%v", rep.Days, rep.Since, since)
	}
	byID := map[int64]DeviceUptime{}
	for _, d := range rep.Devices {
		byID[d.DeviceID] = d
	}
	a := byID[devA.ID]
	if a.Expected != 24 || a.Samples != 12 || a.UptimePct != 50 {
		t.Fatalf("device A = %+v, want expected 24 samples 12 pct 50", a)
	}
	b := byID[devB.ID]
	if b.Expected != 12 || b.Samples != 6 || b.UptimePct != 50 {
		t.Fatalf("device B (prorated) = %+v, want expected 12 samples 6 pct 50", b)
	}
	// fixture's device has no samples -> 0%. Mean = (50+50+0)/3 = 33.3.
	if len(rep.Devices) != 3 {
		t.Fatalf("devices = %d, want 3", len(rep.Devices))
	}
	if rep.MeanPct != 33.3 {
		t.Fatalf("mean = %v, want 33.3", rep.MeanPct)
	}
}

// TestUptimeReportCapsAndScope checks samples beyond expected cap at 100% and
// the RS2 org filter.
func TestUptimeReportCapsAndScope(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	orgA, _, _, _ := fixture(t, s)
	orgB, err := s.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	now := c.now()

	devA := rptDevice(t, s, orgA.ID, "uid-a", "ws-a")
	setEnrolled(t, s, devA.ID, now.Add(-48*time.Hour))
	// More than 24 samples in a 24h/1h window -> capped at 100%.
	for i := 1; i <= 40; i++ {
		seedHB(t, s, devA.ID, now.Add(-time.Duration(i)*30*time.Minute), fptr(40))
	}
	devB := rptDevice(t, s, orgB.ID, "uid-b", "ws-b")
	setEnrolled(t, s, devB.ID, now.Add(-48*time.Hour))

	rep, err := s.UptimeReport(ctx, nil, nil, 1)
	if err != nil {
		t.Fatalf("UptimeReport: %v", err)
	}
	for _, d := range rep.Devices {
		if d.DeviceID == devA.ID && d.UptimePct != 100 {
			t.Fatalf("device A capped pct = %v, want 100", d.UptimePct)
		}
	}

	// Scope to org A only: device B (org B) must not appear.
	scoped, err := s.UptimeReport(ctx, []int64{orgA.ID}, nil, 1)
	if err != nil {
		t.Fatalf("UptimeReport scoped: %v", err)
	}
	for _, d := range scoped.Devices {
		if d.OrgID == orgB.ID {
			t.Fatalf("org B device leaked into scoped uptime: %+v", d)
		}
	}
	// An empty visible set yields no rows.
	none, err := s.UptimeReport(ctx, []int64{}, nil, 1)
	if err != nil {
		t.Fatalf("UptimeReport empty scope: %v", err)
	}
	if len(none.Devices) != 0 || none.MeanPct != 0 {
		t.Fatalf("empty scope = %+v, want no devices", none)
	}
}

// TestDiskSeries proves the ordered series, min/max/latest, the window cutoff and
// that NULL-disk rows are skipped.
func TestDiskSeries(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, _, _, _ := fixture(t, s)
	now := c.now()
	dev := rptDevice(t, s, org.ID, "uid-a", "ws-a")

	// In-window samples, inserted out of time order to prove ORDER BY at.
	seedHB(t, s, dev.ID, now.Add(-3*time.Hour), fptr(40))
	seedHB(t, s, dev.ID, now.Add(-1*time.Hour), fptr(30)) // newest -> latest
	seedHB(t, s, dev.ID, now.Add(-2*time.Hour), fptr(60)) // max
	// A NULL-disk row (skipped) and a sample older than the window (excluded).
	seedHB(t, s, dev.ID, now.Add(-30*time.Minute), nil)
	seedHB(t, s, dev.ID, now.Add(-48*time.Hour), fptr(99))

	rep, err := s.DiskSeries(ctx, dev.ID, 1)
	if err != nil {
		t.Fatalf("DiskSeries: %v", err)
	}
	if len(rep.Points) != 3 {
		t.Fatalf("points = %d (%+v), want 3", len(rep.Points), rep.Points)
	}
	if rep.Points[0].DiskUsedPct != 40 || rep.Points[1].DiskUsedPct != 60 || rep.Points[2].DiskUsedPct != 30 {
		t.Fatalf("series order = %+v", rep.Points)
	}
	if rep.Min == nil || rep.Max == nil || rep.Latest == nil {
		t.Fatalf("min/max/latest nil: %+v", rep)
	}
	if *rep.Min != 30 || *rep.Max != 60 || *rep.Latest != 30 {
		t.Fatalf("min=%v max=%v latest=%v, want 30/60/30", *rep.Min, *rep.Max, *rep.Latest)
	}

	// An empty series has nil min/max/latest.
	empty, err := s.DiskSeries(ctx, rptDevice(t, s, org.ID, "uid-empty", "ws-empty").ID, 1)
	if err != nil {
		t.Fatalf("DiskSeries empty: %v", err)
	}
	if len(empty.Points) != 0 || empty.Min != nil || empty.Latest != nil {
		t.Fatalf("empty series = %+v", empty)
	}
}

// TestDiskTop proves the ordering by latest disk, the limit, and that devices
// without an in-window disk sample are excluded.
func TestDiskTop(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	orgA, _, _, _ := fixture(t, s)
	orgB, err := s.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	now := c.now()

	x := rptDevice(t, s, orgA.ID, "uid-x", "ws-x")
	seedHB(t, s, x.ID, now.Add(-2*time.Hour), fptr(70))
	seedHB(t, s, x.ID, now.Add(-1*time.Hour), fptr(90)) // latest 90
	y := rptDevice(t, s, orgA.ID, "uid-y", "ws-y")
	seedHB(t, s, y.ID, now.Add(-1*time.Hour), fptr(50))
	z := rptDevice(t, s, orgA.ID, "uid-z", "ws-z") // no disk samples at all
	_ = z
	// Device in org B, out-of-window sample only -> excluded by window.
	b := rptDevice(t, s, orgB.ID, "uid-b", "ws-b")
	seedHB(t, s, b.ID, now.Add(-48*time.Hour), fptr(99))

	top, err := s.DiskTop(ctx, nil, nil, 1, 10)
	if err != nil {
		t.Fatalf("DiskTop: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("top = %+v, want 2 (x,y)", top)
	}
	if top[0].DeviceID != x.ID || top[0].DiskUsedPct != 90 {
		t.Fatalf("top[0] = %+v, want x@90", top[0])
	}
	if top[1].DeviceID != y.ID || top[1].DiskUsedPct != 50 {
		t.Fatalf("top[1] = %+v, want y@50", top[1])
	}

	// limit caps the result.
	one, err := s.DiskTop(ctx, nil, nil, 1, 1)
	if err != nil || len(one) != 1 || one[0].DeviceID != x.ID {
		t.Fatalf("DiskTop limit 1 = %+v err %v", one, err)
	}
	// RS2 scope to org A excludes org B.
	scoped, err := s.DiskTop(ctx, []int64{orgA.ID}, nil, 1, 10)
	if err != nil {
		t.Fatalf("DiskTop scoped: %v", err)
	}
	for _, e := range scoped {
		if e.OrgID == orgB.ID {
			t.Fatalf("org B leaked into scoped disk top: %+v", e)
		}
	}
}

// TestPatchCompliance proves the buckets (up_to_date / pending / security_pending
// / reboot_required / never_checked) and the per-device rows, plus the RS2 scope.
func TestPatchCompliance(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	org, _, _, _ := fixture(t, s)
	now := c.now()

	// D1: checked, no pending -> up_to_date.
	d1 := rptDevice(t, s, org.ID, "uid-p1", "ws-1")
	if err := s.RecordPatches(ctx, d1.ID, protocol.PatchReport{CheckedAt: now, Manager: protocol.PatchManagerApt}); err != nil {
		t.Fatalf("RecordPatches d1: %v", err)
	}
	// D2: checked, one security update -> pending + security_pending.
	d2 := rptDevice(t, s, org.ID, "uid-p2", "ws-2")
	if err := s.RecordPatches(ctx, d2.ID, protocol.PatchReport{
		CheckedAt: now, Manager: protocol.PatchManagerApt,
		Updates: []protocol.PatchUpdate{{Name: "openssl", Security: true}},
	}); err != nil {
		t.Fatalf("RecordPatches d2: %v", err)
	}
	// D3: never checked.
	d3 := rptDevice(t, s, org.ID, "uid-p3", "ws-3")
	// D4: checked, one non-security update, reboot required -> pending + reboot_required.
	d4 := rptDevice(t, s, org.ID, "uid-p4", "ws-4")
	if err := s.RecordPatches(ctx, d4.ID, protocol.PatchReport{
		CheckedAt: now, Manager: protocol.PatchManagerApt, RebootRequired: true,
		Updates: []protocol.PatchUpdate{{Name: "curl"}},
	}); err != nil {
		t.Fatalf("RecordPatches d4: %v", err)
	}
	// D5 (regression, RP1 low finding): never checked but with reboot_required set
	// (a zero CheckedAt leaves patches_checked_at NULL while reboot_required=1).
	// reboot_required is a flag on the checked set, so bucketPatch must NOT count
	// this device — mirroring the web bucketDevices helper, which skips a
	// never-checked device before the reboot check. Without the guard the two
	// "mirrored" helpers would disagree (server 2, web 1).
	d5 := rptDevice(t, s, org.ID, "uid-p5", "ws-5")
	if err := s.RecordPatches(ctx, d5.ID, protocol.PatchReport{
		Manager: protocol.PatchManagerApt, RebootRequired: true,
	}); err != nil {
		t.Fatalf("RecordPatches d5: %v", err)
	}

	rep, err := s.PatchCompliance(ctx, nil, nil)
	if err != nil {
		t.Fatalf("PatchCompliance: %v", err)
	}
	// fixture's own device (ws-01) is never checked too; with d3 and d5 that is
	// never 3, total 6. reboot_required stays 1 (only d4, which is checked): the
	// never-checked d5 does not contribute, matching the web helper.
	want := PatchBuckets{Total: 6, UpToDate: 1, Pending: 2, SecurityPending: 1, RebootRequired: 1, NeverChecked: 3}
	if rep.Buckets != want {
		t.Fatalf("buckets = %+v, want %+v", rep.Buckets, want)
	}
	byID := map[int64]DevicePatch{}
	for _, d := range rep.Devices {
		byID[d.DeviceID] = d
	}
	// d5 is never-checked yet carries the reboot_required flag; it is excluded from
	// the reboot_required bucket above but the per-device row still reports the flag.
	if dp := byID[d5.ID]; dp.CheckedAt != nil || !dp.RebootRequired {
		t.Fatalf("d5 = %+v, want never-checked with reboot_required set", dp)
	}
	if dp := byID[d3.ID]; dp.CheckedAt != nil {
		t.Fatalf("d3 checked_at = %v, want nil", dp.CheckedAt)
	}
	if dp := byID[d2.ID]; dp.Security != 1 || dp.Pending != 1 || dp.CheckedAt == nil {
		t.Fatalf("d2 = %+v, want pending 1 security 1 checked", dp)
	}
	if dp := byID[d4.ID]; !dp.RebootRequired || dp.Pending != 1 {
		t.Fatalf("d4 = %+v, want reboot + pending 1", dp)
	}
	_ = d1

	// RS2: empty visible set -> no rows, zero buckets.
	none, err := s.PatchCompliance(ctx, []int64{}, nil)
	if err != nil {
		t.Fatalf("PatchCompliance empty scope: %v", err)
	}
	if none.Buckets.Total != 0 || len(none.Devices) != 0 {
		t.Fatalf("empty scope = %+v", none.Buckets)
	}
}
