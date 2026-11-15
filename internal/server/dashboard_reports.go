package server

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sxi9/msp/internal/store"
)

// Reports (RP1) are read-only, RS2 org-scoped views over the data the platform
// already stores: device check-in uptime, disk-usage trends and patch
// compliance, each as JSON and as a CSV export. This file registers the routes
// (appended to dashboardRoutes, like dashboard_patch.go) and holds the handlers;
// the aggregation SQL lives in internal/store/reports.go. Every route runs the
// standard sessionAuth -> csrf -> requireRole(viewer) chain: reports are
// read-only, so a viewer may see them.

func init() {
	dashboardRoutes = append(dashboardRoutes, registerReports)
}

// registerReports adds the report + CSV routes. The .csv paths are distinct
// exact patterns from their JSON siblings, and /reports/disk/top is a distinct
// path from /reports/disk, so the mux routes them without conflict.
func registerReports(s *Server, mux *http.ServeMux) {
	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle("GET "+pattern, s.dashRoute(RoleViewer, h))
	}
	get("/api/v1/reports/uptime", s.handleReportUptime)
	get("/api/v1/reports/uptime.csv", s.handleReportUptimeCSV)
	get("/api/v1/reports/disk", s.handleReportDisk)
	get("/api/v1/reports/disk.csv", s.handleReportDiskCSV)
	get("/api/v1/reports/disk/top", s.handleReportDiskTop)
	get("/api/v1/reports/patch-compliance", s.handleReportPatch)
	get("/api/v1/reports/patch-compliance.csv", s.handleReportPatchCSV)
}

// RegisterReports exposes the report route registration for an integrator that
// assembles the mux itself. See registerPatch for the same pattern.
func RegisterReports(s *Server, mux *http.ServeMux) { registerReports(s, mux) }

// reportDays reads ?days=N: default 7, a positive integer, rejected (400) when
// it is not a positive integer or exceeds the retention window (the period a
// report can cover is bounded by MSP_RETENTION_DAYS). ok is false and the
// response is written on rejection.
func (s *Server) reportDays(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("days")
	days := 7
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "days must be a positive integer")
			return 0, false
		}
		days = n
	}
	if days > s.cfg.RetentionDays {
		writeError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("days must be between 1 and %d (the retention window)", s.cfg.RetentionDays))
		return 0, false
	}
	return days, true
}

// reportScope resolves the common org scope for a fleet report: the optional
// ?org_id (400 when malformed), the caller's RS2 visible org set, and a 404 when
// an explicit org_id is outside that set (indistinguishable from a missing org).
// ok is false and the response written on any failure.
func (s *Server) reportScope(w http.ResponseWriter, r *http.Request) (orgID *int64, visible []int64, ok bool) {
	orgID, good := dashOrgIDParam(r)
	if !good {
		writeError(w, http.StatusBadRequest, "invalid_request", "org_id must be a positive integer")
		return nil, nil, false
	}
	visible, good = s.dashVisibleOrgs(w, r)
	if !good {
		return nil, nil, false
	}
	if orgID != nil && !s.dashOrgVisible(w, r, *orgID) {
		return nil, nil, false
	}
	return orgID, visible, true
}

// handleReportUptime is GET /api/v1/reports/uptime?org_id=&days=N.
func (s *Server) handleReportUptime(w http.ResponseWriter, r *http.Request) {
	days, ok := s.reportDays(w, r)
	if !ok {
		return
	}
	orgID, visible, ok := s.reportScope(w, r)
	if !ok {
		return
	}
	rep, err := s.st.UptimeReport(r.Context(), visible, orgID, days)
	if err != nil {
		s.log.Error("uptime report", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleReportUptimeCSV is GET /api/v1/reports/uptime.csv?org_id=&days=N.
func (s *Server) handleReportUptimeCSV(w http.ResponseWriter, r *http.Request) {
	days, ok := s.reportDays(w, r)
	if !ok {
		return
	}
	orgID, visible, ok := s.reportScope(w, r)
	if !ok {
		return
	}
	rep, err := s.st.UptimeReport(r.Context(), visible, orgID, days)
	if err != nil {
		s.log.Error("uptime report csv", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	cw := beginCSV(w, "uptime", s.st.Now())
	_ = cw.Write([]string{"device_id", "hostname", "org_id", "org_name", "samples", "expected", "uptime_pct", "os_uptime_s"})
	for _, d := range rep.Devices {
		osUptime := ""
		if d.UptimeS != nil {
			osUptime = strconv.FormatInt(*d.UptimeS, 10)
		}
		_ = cw.Write([]string{
			strconv.FormatInt(d.DeviceID, 10),
			csvSafe(d.Hostname),
			strconv.FormatInt(d.OrgID, 10),
			csvSafe(d.OrgName),
			strconv.Itoa(d.Samples),
			strconv.Itoa(d.Expected),
			strconv.FormatFloat(d.UptimePct, 'f', -1, 64),
			osUptime,
		})
	}
	cw.Flush()
}

// handleReportDisk is GET /api/v1/reports/disk?device_id=&days=N (one device).
func (s *Server) handleReportDisk(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.reportDevice(w, r)
	if !ok {
		return
	}
	days, ok := s.reportDays(w, r)
	if !ok {
		return
	}
	rep, err := s.st.DiskSeries(r.Context(), dev.ID, days)
	if err != nil {
		s.log.Error("disk series", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	rep.Hostname, rep.OrgID, rep.OrgName = dev.Hostname, dev.OrgID, dev.OrgName
	writeJSON(w, http.StatusOK, rep)
}

// handleReportDiskCSV is GET /api/v1/reports/disk.csv?device_id=&days=N.
func (s *Server) handleReportDiskCSV(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.reportDevice(w, r)
	if !ok {
		return
	}
	days, ok := s.reportDays(w, r)
	if !ok {
		return
	}
	rep, err := s.st.DiskSeries(r.Context(), dev.ID, days)
	if err != nil {
		s.log.Error("disk series csv", "device", dev.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	cw := beginCSV(w, "disk", s.st.Now())
	_ = cw.Write([]string{"at", "disk_used_pct"})
	for _, p := range rep.Points {
		_ = cw.Write([]string{
			p.At.UTC().Format(time.RFC3339),
			strconv.FormatFloat(p.DiskUsedPct, 'f', -1, 64),
		})
	}
	cw.Flush()
}

// handleReportDiskTop is GET /api/v1/reports/disk/top?org_id=&days=N&limit=.
func (s *Server) handleReportDiskTop(w http.ResponseWriter, r *http.Request) {
	days, ok := s.reportDays(w, r)
	if !ok {
		return
	}
	orgID, visible, ok := s.reportScope(w, r)
	if !ok {
		return
	}
	limit := dashLimit(r, 10, 100)
	entries, err := s.st.DiskTop(r.Context(), visible, orgID, days, limit)
	if err != nil {
		s.log.Error("disk top", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "rows": entries})
}

// handleReportPatch is GET /api/v1/reports/patch-compliance?org_id=.
func (s *Server) handleReportPatch(w http.ResponseWriter, r *http.Request) {
	orgID, visible, ok := s.reportScope(w, r)
	if !ok {
		return
	}
	rep, err := s.st.PatchCompliance(r.Context(), visible, orgID)
	if err != nil {
		s.log.Error("patch compliance", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleReportPatchCSV is GET /api/v1/reports/patch-compliance.csv?org_id=.
func (s *Server) handleReportPatchCSV(w http.ResponseWriter, r *http.Request) {
	orgID, visible, ok := s.reportScope(w, r)
	if !ok {
		return
	}
	rep, err := s.st.PatchCompliance(r.Context(), visible, orgID)
	if err != nil {
		s.log.Error("patch compliance csv", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	cw := beginCSV(w, "patch-compliance", s.st.Now())
	_ = cw.Write([]string{"device_id", "hostname", "org_id", "org_name", "pending", "security", "reboot_required", "checked_at"})
	for _, d := range rep.Devices {
		checked := ""
		if d.CheckedAt != nil {
			checked = d.CheckedAt.UTC().Format(time.RFC3339)
		}
		_ = cw.Write([]string{
			strconv.FormatInt(d.DeviceID, 10),
			csvSafe(d.Hostname),
			strconv.FormatInt(d.OrgID, 10),
			csvSafe(d.OrgName),
			strconv.Itoa(d.Pending),
			strconv.Itoa(d.Security),
			strconv.FormatBool(d.RebootRequired),
			checked,
		})
	}
	cw.Flush()
}

// reportDevice resolves ?device_id, loads the device and enforces RS2 visibility
// (the same order as the heartbeats endpoint): a missing/invalid id is 400, an
// unknown device or one in an invisible org is 404. ok is false and the response
// written on any failure.
func (s *Server) reportDevice(w http.ResponseWriter, r *http.Request) (store.Device, bool) {
	raw := r.URL.Query().Get("device_id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "device_id must be a positive integer")
		return store.Device{}, false
	}
	dev, err := s.st.GetDevice(r.Context(), id)
	if err != nil {
		s.dashStoreError(w, err, "get device")
		return store.Device{}, false
	}
	if !s.dashOrgVisible(w, r, dev.OrgID) {
		return store.Device{}, false
	}
	return dev, true
}

// beginCSV writes the text/csv headers (including the attachment filename
// msp-<report>-<date>.csv) and returns a csv.Writer over the response. The
// caller writes rows through csvSafe for any text cell and Flushes at the end.
func beginCSV(w http.ResponseWriter, report string, now time.Time) *csv.Writer {
	filename := fmt.Sprintf("msp-%s-%s.csv", report, now.UTC().Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	cw.UseCRLF = true // mirror the client-side CSV builder (CRLF lines, trailing CRLF)
	return cw
}

// csvSafe neutralises CSV/formula injection in a text cell: a leading =, +, -, @
// (or a leading tab/CR that some sheets also treat as a formula lead-in) is
// prefixed with a single quote so a spreadsheet renders it as text, never
// executes it. Quoting of commas/quotes/newlines is handled by encoding/csv.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}
