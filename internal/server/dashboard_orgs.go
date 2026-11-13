package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/store"
)

const (
	dashMaxOrgName  = 100
	dashMaxLabel    = 100
	dashDefaultUses = 10
	dashMaxUses     = 1000
	dashDefaultTTLH = 168
	dashMaxTTLH     = 8760
)

// handleListOrgs is GET /api/v1/orgs.
func (s *Server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.st.ListOrgs(r.Context())
	if err != nil {
		s.log.Error("list orgs", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if orgs == nil {
		orgs = []store.Org{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"orgs": orgs})
}

// handleCreateOrg is POST /api/v1/orgs (admin).
func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	if len(name) > dashMaxOrgName {
		writeError(w, http.StatusBadRequest, "invalid_request", "name exceeds 100 characters")
		return
	}
	ctx := r.Context()
	org, err := s.st.CreateOrg(ctx, name)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "an org with that name exists")
		return
	case err != nil:
		s.log.Error("create org", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, &org.ID, "org.created", "org", &org.ID, clientIP(r), map[string]any{"name": org.Name})
	writeJSON(w, http.StatusCreated, map[string]any{"org": org})
}

// handleGetOrg is GET /api/v1/orgs/{id}: the org plus its enrollment tokens.
func (s *Server) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	org, err := s.st.GetOrg(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get org")
		return
	}
	tokens, err := s.st.ListEnrollmentTokens(ctx, id)
	if err != nil {
		s.log.Error("list enrollment tokens", "org", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if tokens == nil {
		tokens = []store.EnrollmentToken{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"org": org, "tokens": tokens})
}

// handleCreateToken is POST /api/v1/orgs/{id}/tokens (operator): mints an
// enrollment token and returns the plaintext once with the install one-liners.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	var req struct {
		Label    string `json:"label"`
		MaxUses  int    `json:"max_uses"`
		TTLHours int    `json:"ttl_hours"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	label := strings.TrimSpace(req.Label)
	if len(label) > dashMaxLabel {
		writeError(w, http.StatusBadRequest, "invalid_request", "label exceeds 100 characters")
		return
	}
	if req.MaxUses == 0 {
		req.MaxUses = dashDefaultUses
	}
	if req.MaxUses < 1 || req.MaxUses > dashMaxUses {
		writeError(w, http.StatusBadRequest, "invalid_request", "max_uses must be 1..1000")
		return
	}
	if req.TTLHours == 0 {
		req.TTLHours = dashDefaultTTLH
	}
	if req.TTLHours < 1 || req.TTLHours > dashMaxTTLH {
		writeError(w, http.StatusBadRequest, "invalid_request", "ttl_hours must be 1..8760")
		return
	}

	ctx := r.Context()
	if _, err := s.st.GetOrg(ctx, id); err != nil {
		s.dashStoreError(w, err, "get org")
		return
	}
	actor, _ := dashActor(r)
	plain, tok, err := s.st.CreateEnrollmentToken(ctx, id, label, req.MaxUses, time.Duration(req.TTLHours)*time.Hour, actor)
	if err != nil {
		s.log.Error("create enrollment token", "org", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	s.audit(ctx, "user", actor, &id, "token.created", "enrollment_token", &tok.ID, clientIP(r), map[string]any{
		"label":      tok.Label,
		"max_uses":   tok.MaxUses,
		"expires_at": tok.ExpiresAt,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":     tok,
		"plaintext": plain,
		"install":   dashInstallLines(dashBaseURL(s, r), plain),
	})
}

// handleRevokeToken is POST /api/v1/orgs/{id}/tokens/{tid}/revoke (operator).
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	tid, ok2 := pathInt(r, "tid")
	if !ok || !ok2 {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	ctx := r.Context()
	if err := s.st.RevokeEnrollmentToken(ctx, id, tid); err != nil {
		s.dashStoreError(w, err, "revoke enrollment token")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, &id, "token.revoked", "enrollment_token", &tid, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

// dashStoreError maps a store error to the response: ErrNotFound -> 404,
// ErrConflict -> 409, anything else -> logged 500.
func (s *Server) dashStoreError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "")
	default:
		s.log.Error(what, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
	}
}
