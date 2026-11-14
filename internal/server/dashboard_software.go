package server

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/sxi9/msp/internal/store"
)

// Installed-software inventory search (SI1). This file holds the one
// dashboard-facing read endpoint, GET /api/v1/software, which searches the
// `software_json` the agents report (recorded by store.RecordSoftware from the
// sync/enroll handler) across the caller's visible devices. It mirrors the
// patch/deploy dashboard files: it registers its route through dashboardRoutes
// at init so server.go's Handler picks it up, and exposes a RegisterSoftware for
// an integrator that wires the mux by hand.
//
// INTEGRATION NOTE (WP-INTEGRATE): the agents send the inventory in
// facts.software; the sync/enroll handler (internal/server/api_agent.go) must
// persist it exactly like it does the patch report, with one line beside each
// RecordPatches call:
//
//	// enroll (handleEnroll), after RecordPatches — a failure must not fail enroll:
//	if len(req.Facts.Software) > 0 {
//		if err := s.st.RecordSoftware(ctx, dev.ID, req.Facts.Software, s.st.Now()); err != nil {
//			s.log.Error("record software at enroll", "device", dev.ID, "err", err)
//		}
//	}
//
//	// sync (handleSync), after RecordPatches (req.Facts may be nil on a sync):
//	if req.Facts != nil && len(req.Facts.Software) > 0 {
//		if err := s.st.RecordSoftware(ctx, dev.ID, req.Facts.Software, s.st.Now()); err != nil {
//			s.log.Error("record software", "device", dev.ID, "err", err)
//		}
//	}
//
// (s.st.Now is the store clock; match store.RecordSoftware's own signature.)
//
// The device-detail handler (handleGetDevice in dashboard_devices.go) must also
// emit the full list as a sibling of "patches", since store.Device keeps it in
// SoftwareJSON (json:"-", like PatchesJSON). Add one entry to its writeJSON map:
//
//	"software": json.RawMessage(dashJSONArrayOr(dev.SoftwareJSON, "[]")),
//
// where the raw stored array ("[]" when unset) is passed through. The device list
// already carries dev.SoftwareCount and dev.SoftwareCheckedAt (marshalled on the
// Device struct), which drive the fleet "Software" column and the panel heading.

// minSoftwareQuery is the shortest accepted search substring: one- or
// zero-character queries would match almost everything, so they are rejected
// with 400 invalid_request. The result cap lives in the store
// (store.SoftwareSearchLimit); the store returns whether it truncated.
const minSoftwareQuery = 2

func init() {
	dashboardRoutes = append(dashboardRoutes, registerSoftware)
}

// registerSoftware adds the software-search route inside the standard
// sessionAuth -> csrf -> requireRole chain. It is a read, so the viewer role is
// enough (GET goes through dashRoute like the device list).
func registerSoftware(s *Server, mux *http.ServeMux) {
	mux.Handle("GET /api/v1/software", s.dashRoute(RoleViewer, s.handleSoftwareSearch))
}

// RegisterSoftware exposes the software-search route registration for an
// integrator that assembles the mux itself (WP-INTEGRATE). Using both this and
// the automatic dashboardRoutes registration would double-register and panic, so
// an integrator uses one or the other — see RegisterPatch for the same pattern.
func RegisterSoftware(s *Server, mux *http.ServeMux) { registerSoftware(s, mux) }

// handleSoftwareSearch is GET /api/v1/software?q=<substring>&org_id= (viewer+,
// session auth): the installed-software packages whose name matches the
// case-insensitive substring q, across the caller's visible devices (SI1). q is
// required and must be at least minSoftwareQuery characters. An explicit
// ?org_id= that the caller cannot see gets the same 404 as a non-existent org
// (RS2), before the search runs; otherwise the search is scoped to that one org.
// Without ?org_id= it spans every visible org.
func (s *Server) handleSoftwareSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) < minSoftwareQuery {
		writeError(w, http.StatusBadRequest, "invalid_request", "q is required and must be at least 2 characters")
		return
	}
	orgID, ok := dashOrgIDParam(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "org_id must be a positive integer")
		return
	}
	visible, ok := s.dashVisibleOrgs(w, r)
	if !ok {
		return
	}
	// An explicit ?org_id= that is not visible is a 404 (RS2), before the
	// search; a visible one narrows the search to that single org.
	if orgID != nil {
		if !s.dashOrgVisible(w, r, *orgID) {
			return
		}
		visible = []int64{*orgID}
	}
	rows, truncated, err := s.st.SoftwareSearch(r.Context(), visible, q, store.SoftwareSearchLimit)
	if err != nil {
		s.log.Error("software search", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if rows == nil {
		rows = []store.SoftwareHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":     q,
		"results":   rows,
		"truncated": truncated,
	})
}
