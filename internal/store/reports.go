package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// Reports (RP1) are read-only aggregates over the data the platform already
// stores: the per-minute heartbeats table and the denormalised device columns.
// Every function is RS2 org-scoped through the caller's visible org set (reusing
// orgInClause) and bounded by the retention window; the caller clamps days to
// 1..MSP_RETENTION_DAYS and this package treats days < 1 as 1 defensively.

// DeviceUptime is one device's check-in reliability over the report window.
// uptime_pct measures "did the agent check in", not OS uptime: samples is the
// number of heartbeat rows seen, expected is how many throttled samples the
// device's interval would produce over the (prorated) period, and uptime_pct is
// 100 * min(samples/expected, 1). A device enrolled mid-period counts only from
// enrollment.
type DeviceUptime struct {
	DeviceID  int64   `json:"device_id"`
	Hostname  string  `json:"hostname"`
	OrgID     int64   `json:"org_id"`
	OrgName   string  `json:"org_name"`
	Samples   int     `json:"samples"`
	Expected  int     `json:"expected"`
	UptimePct float64 `json:"uptime_pct"`
	// UptimeS is the OS uptime the device last reported (the most recent
	// heartbeat's uptime_s); nil when the device has never sent a heartbeat.
	// It is unrelated to uptime_pct (which measures check-in reliability) and is
	// shown alongside it so a reader can see both figures.
	UptimeS *int64 `json:"uptime_s"`
}

// UptimeReportResult is the uptime report: the window, the per-device rows and
// the unweighted mean of their uptime_pct (0 when there are no devices). The
// JSON field names are the report wire contract consumed by the dashboard
// (rows / mean_uptime_pct / device_count); since is extra context.
type UptimeReportResult struct {
	Days        int            `json:"days"`
	Since       time.Time      `json:"since"`
	MeanPct     float64        `json:"mean_uptime_pct"`
	DeviceCount int            `json:"device_count"`
	Devices     []DeviceUptime `json:"rows"`
}

// uptimeSampleFloorS is the minimum spacing between heartbeat rows (the server's
// SampleEvery is at most 1/min by default). Expected is computed against the
// larger of the device interval and this floor, so a device that syncs faster
// than once a minute is not credited with more samples than it can store.
const uptimeSampleFloorS = 60

// UptimeReport computes per-device check-in uptime over the last days days,
// org-scoped to visibleOrgs (nil = every org) and optionally a single orgID.
// Callers validate orgID visibility (RS2) before calling.
func (s *Store) UptimeReport(ctx context.Context, visibleOrgs []int64, orgID *int64, days int) (UptimeReportResult, error) {
	days = clampReportDays(days)
	now := s.Now()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)

	where, args := reportOrgWhere(orgID, visibleOrgs)
	q := `SELECT d.id, d.hostname, d.org_id, o.name, d.interval_s, d.enrolled_at,
	             (SELECT COUNT(*) FROM heartbeats h WHERE h.device_id = d.id AND h.at >= ?) AS samples,
	             (SELECT h2.uptime_s FROM heartbeats h2 WHERE h2.device_id = d.id
	               ORDER BY h2.at DESC, h2.id DESC LIMIT 1) AS os_uptime_s
	        FROM devices d JOIN orgs o ON o.id = d.org_id`
	// The samples subquery takes the first bind arg; the org filter args follow.
	callArgs := append([]any{fmtTime(since)}, args...)
	if where != "" {
		q += ` WHERE ` + where
	}
	q += ` ORDER BY o.name, d.hostname, d.id`

	rows, err := s.db.QueryContext(ctx, q, callArgs...)
	if err != nil {
		return UptimeReportResult{}, fmt.Errorf("store: uptime report: %w", err)
	}
	defer rows.Close()

	out := UptimeReportResult{Days: days, Since: since, Devices: []DeviceUptime{}}
	var sum float64
	for rows.Next() {
		var (
			du       DeviceUptime
			interval int
			enrolled string
			osUptime sql.NullInt64
		)
		if err := rows.Scan(&du.DeviceID, &du.Hostname, &du.OrgID, &du.OrgName, &interval, &enrolled, &du.Samples, &osUptime); err != nil {
			return UptimeReportResult{}, fmt.Errorf("store: uptime report: %w", err)
		}
		enrolledAt, err := parseTime(enrolled)
		if err != nil {
			return UptimeReportResult{}, err
		}
		if osUptime.Valid {
			v := osUptime.Int64
			du.UptimeS = &v
		}
		du.Expected = expectedSamples(since, enrolledAt, now, interval)
		du.UptimePct = uptimePct(du.Samples, du.Expected)
		sum += du.UptimePct
		out.Devices = append(out.Devices, du)
	}
	if err := rows.Err(); err != nil {
		return UptimeReportResult{}, fmt.Errorf("store: uptime report: %w", err)
	}
	out.DeviceCount = len(out.Devices)
	if len(out.Devices) > 0 {
		out.MeanPct = round1(sum / float64(len(out.Devices)))
	}
	return out, nil
}

// expectedSamples is how many throttled heartbeat rows a device should have
// produced over the window, prorated from its enrollment: the period starts at
// the later of the window start and the device's enrollment, and the cadence is
// the larger of the device interval and the 1/min sample floor.
func expectedSamples(since, enrolledAt, now time.Time, intervalS int) int {
	start := since
	if enrolledAt.After(start) {
		start = enrolledAt
	}
	periodS := now.Sub(start).Seconds()
	if periodS <= 0 {
		return 0
	}
	cadence := intervalS
	if cadence < uptimeSampleFloorS {
		cadence = uptimeSampleFloorS
	}
	return int(periodS) / cadence
}

// uptimePct is 100 * min(samples/expected, 1), rounded to one decimal. When no
// samples are expected (a device enrolled too recently to owe one) a device that
// did check in is 100%, otherwise 0%.
func uptimePct(samples, expected int) float64 {
	if expected <= 0 {
		if samples > 0 {
			return 100
		}
		return 0
	}
	r := float64(samples) / float64(expected)
	if r > 1 {
		r = 1
	}
	return round1(r * 100)
}

// DiskPoint is one disk-usage sample in a series.
type DiskPoint struct {
	At          time.Time `json:"at"`
	DiskUsedPct float64   `json:"disk_used_pct"`
}

// DiskSeriesResult is one device's disk_used_pct series over the window with its
// min/max/latest (nil when the series is empty). latest is the most recent
// sample in the window (by time).
type DiskSeriesResult struct {
	DeviceID int64       `json:"device_id"`
	Hostname string      `json:"hostname"`
	OrgID    int64       `json:"org_id"`
	OrgName  string      `json:"org_name"`
	Days     int         `json:"days"`
	Since    time.Time   `json:"since"`
	Points   []DiskPoint `json:"series"`
	Min      *float64    `json:"min"`
	Max      *float64    `json:"max"`
	Latest   *float64    `json:"latest"`
}

// DiskSeries returns a device's disk_used_pct time series over the last days
// days (oldest first) plus min/max/latest. Rows with a NULL disk reading are
// skipped. RS2 visibility of the device is the caller's responsibility (the
// handler checks it exactly like the heartbeats endpoint).
func (s *Store) DiskSeries(ctx context.Context, deviceID int64, days int) (DiskSeriesResult, error) {
	days = clampReportDays(days)
	now := s.Now()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	res := DiskSeriesResult{DeviceID: deviceID, Days: days, Since: since, Points: []DiskPoint{}}

	rows, err := s.db.QueryContext(ctx,
		`SELECT at, disk_used_pct FROM heartbeats
		  WHERE device_id = ? AND at >= ? AND disk_used_pct IS NOT NULL
		  ORDER BY at, id`, deviceID, fmtTime(since))
	if err != nil {
		return DiskSeriesResult{}, fmt.Errorf("store: disk series: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			at   string
			disk float64
		)
		if err := rows.Scan(&at, &disk); err != nil {
			return DiskSeriesResult{}, fmt.Errorf("store: disk series: %w", err)
		}
		t, err := parseTime(at)
		if err != nil {
			return DiskSeriesResult{}, err
		}
		res.Points = append(res.Points, DiskPoint{At: t, DiskUsedPct: disk})
	}
	if err := rows.Err(); err != nil {
		return DiskSeriesResult{}, fmt.Errorf("store: disk series: %w", err)
	}
	if len(res.Points) > 0 {
		mn, mx := res.Points[0].DiskUsedPct, res.Points[0].DiskUsedPct
		for _, p := range res.Points {
			if p.DiskUsedPct < mn {
				mn = p.DiskUsedPct
			}
			if p.DiskUsedPct > mx {
				mx = p.DiskUsedPct
			}
		}
		latest := res.Points[len(res.Points)-1].DiskUsedPct
		res.Min, res.Max, res.Latest = &mn, &mx, &latest
	}
	return res, nil
}

// DiskTopEntry is one device's latest disk usage within the window, for the
// fleet "fullest disks" view. DiskUsedPct/At are the most recent in-window
// sample; only devices with at least one disk sample in the window appear.
type DiskTopEntry struct {
	DeviceID    int64     `json:"device_id"`
	Hostname    string    `json:"hostname"`
	OrgID       int64     `json:"org_id"`
	OrgName     string    `json:"org_name"`
	DiskUsedPct float64   `json:"latest"`
	At          time.Time `json:"at"`
}

// DiskTop returns the visible devices with the highest latest disk usage over
// the window, highest first, capped at limit. org-scoped to visibleOrgs (nil =
// every org) and optionally a single orgID.
func (s *Store) DiskTop(ctx context.Context, visibleOrgs []int64, orgID *int64, days, limit int) ([]DiskTopEntry, error) {
	days = clampReportDays(days)
	if limit <= 0 {
		limit = 10
	}
	now := s.Now()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)

	where, args := reportOrgWhere(orgID, visibleOrgs)
	// The latest in-window disk sample per device via a correlated subquery on id.
	q := `SELECT d.id, d.hostname, d.org_id, o.name, h.disk_used_pct, h.at
	        FROM devices d
	        JOIN orgs o ON o.id = d.org_id
	        JOIN heartbeats h ON h.id = (
	             SELECT h2.id FROM heartbeats h2
	              WHERE h2.device_id = d.id AND h2.at >= ? AND h2.disk_used_pct IS NOT NULL
	              ORDER BY h2.at DESC, h2.id DESC LIMIT 1)`
	callArgs := append([]any{fmtTime(since)}, args...)
	if where != "" {
		q += ` WHERE ` + where
	}
	q += ` ORDER BY h.disk_used_pct DESC, o.name, d.hostname, d.id LIMIT ?`
	callArgs = append(callArgs, limit)

	rows, err := s.db.QueryContext(ctx, q, callArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: disk top: %w", err)
	}
	defer rows.Close()
	out := []DiskTopEntry{}
	for rows.Next() {
		var (
			e  DiskTopEntry
			at string
		)
		if err := rows.Scan(&e.DeviceID, &e.Hostname, &e.OrgID, &e.OrgName, &e.DiskUsedPct, &at); err != nil {
			return nil, fmt.Errorf("store: disk top: %w", err)
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: disk top: %w", err)
	}
	return out, nil
}

// PatchBuckets counts devices by patch state over the visible fleet. up_to_date,
// pending and never_checked partition total; security_pending and
// reboot_required are additional (overlapping) flags. never_checked includes
// devices that have never reported a patch scan, which on an MSP fleet also
// covers platforms where patching is unavailable (e.g. android).
type PatchBuckets struct {
	Total           int `json:"total"`
	UpToDate        int `json:"up_to_date"`
	Pending         int `json:"pending"`
	SecurityPending int `json:"security_pending"`
	RebootRequired  int `json:"reboot_required"`
	NeverChecked    int `json:"never_checked"`
}

// DevicePatch is one device's current patch state for the compliance breakdown.
type DevicePatch struct {
	DeviceID       int64      `json:"device_id"`
	Hostname       string     `json:"hostname"`
	OrgID          int64      `json:"org_id"`
	OrgName        string     `json:"org_name"`
	Platform       string     `json:"platform"`
	Pending        int        `json:"pending"`
	Security       int        `json:"security"`
	RebootRequired bool       `json:"reboot_required"`
	CheckedAt      *time.Time `json:"checked_at"`
}

// PatchOrgCounts is one org's patch-compliance counts, for the per-org breakdown
// the dashboard shows when more than one org is in scope.
type PatchOrgCounts struct {
	OrgID   int64        `json:"org_id"`
	OrgName string       `json:"org_name"`
	Counts  PatchBuckets `json:"counts"`
}

// PatchComplianceResult is the fleet patch summary (overall), the per-org
// breakdown and the per-device rows. The JSON field names (overall / orgs /
// devices) are the report wire contract consumed by the dashboard.
type PatchComplianceResult struct {
	Buckets PatchBuckets     `json:"overall"`
	Orgs    []PatchOrgCounts `json:"orgs"`
	Devices []DevicePatch    `json:"devices"`
}

// PatchCompliance buckets the visible devices by patch state from the
// denormalised device columns (current state, not windowed). org-scoped to
// visibleOrgs (nil = every org) and optionally a single orgID.
func (s *Store) PatchCompliance(ctx context.Context, visibleOrgs []int64, orgID *int64) (PatchComplianceResult, error) {
	where, args := reportOrgWhere(orgID, visibleOrgs)
	q := `SELECT d.id, d.hostname, d.org_id, o.name, d.platform,
	             d.pending_patches, d.security_patches, d.reboot_required, d.patches_checked_at
	        FROM devices d JOIN orgs o ON o.id = d.org_id`
	if where != "" {
		q += ` WHERE ` + where
	}
	q += ` ORDER BY o.name, d.hostname, d.id`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return PatchComplianceResult{}, fmt.Errorf("store: patch compliance: %w", err)
	}
	defer rows.Close()
	res := PatchComplianceResult{Orgs: []PatchOrgCounts{}, Devices: []DevicePatch{}}
	// Per-org buckets, kept in first-seen order (the query is ordered by org name
	// then hostname) via orgIdx so the breakdown is stable.
	orgIdx := map[int64]int{}
	for rows.Next() {
		var (
			dp        DevicePatch
			reboot    int
			checkedAt sql.NullString
		)
		if err := rows.Scan(&dp.DeviceID, &dp.Hostname, &dp.OrgID, &dp.OrgName, &dp.Platform,
			&dp.Pending, &dp.Security, &reboot, &checkedAt); err != nil {
			return PatchComplianceResult{}, fmt.Errorf("store: patch compliance: %w", err)
		}
		dp.RebootRequired = reboot != 0
		if dp.CheckedAt, err = nullTime(checkedAt); err != nil {
			return PatchComplianceResult{}, err
		}
		idx, ok := orgIdx[dp.OrgID]
		if !ok {
			idx = len(res.Orgs)
			orgIdx[dp.OrgID] = idx
			res.Orgs = append(res.Orgs, PatchOrgCounts{OrgID: dp.OrgID, OrgName: dp.OrgName})
		}
		bucketPatch(&res.Buckets, dp)
		bucketPatch(&res.Orgs[idx].Counts, dp)
		res.Devices = append(res.Devices, dp)
	}
	if err := rows.Err(); err != nil {
		return PatchComplianceResult{}, fmt.Errorf("store: patch compliance: %w", err)
	}
	return res, nil
}

// bucketPatch folds one device's patch state into a PatchBuckets counter. The
// up_to_date / pending / never_checked buckets partition total; security_pending
// and reboot_required are additional overlapping flags on the checked set.
func bucketPatch(b *PatchBuckets, dp DevicePatch) {
	b.Total++
	switch {
	case dp.CheckedAt == nil:
		b.NeverChecked++
	case dp.Pending == 0:
		b.UpToDate++
	default:
		b.Pending++
	}
	if dp.CheckedAt != nil && dp.Security > 0 {
		b.SecurityPending++
	}
	if dp.RebootRequired {
		b.RebootRequired++
	}
}

// reportOrgWhere builds the shared org filter for a report: the RS2 visible-org
// restriction (orgInClause) AND an optional explicit org_id, as one WHERE body
// (no leading operator) with its bind args in order.
func reportOrgWhere(orgID *int64, visibleOrgs []int64) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	if orgID != nil {
		clauses = append(clauses, `d.org_id = ?`)
		args = append(args, *orgID)
	}
	if clause, clauseArgs := orgInClause(`d.org_id`, visibleOrgs); clause != "" {
		clauses = append(clauses, clause)
		args = append(args, clauseArgs...)
	}
	return strings.Join(clauses, ` AND `), args
}

// clampReportDays defends against a non-positive days; the caller already
// rejects a days beyond the retention window.
func clampReportDays(days int) int {
	if days < 1 {
		return 1
	}
	return days
}

// round1 rounds to one decimal place.
func round1(f float64) float64 {
	return math.Round(f*10) / 10
}
