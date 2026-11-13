package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/sxi9/msp/internal/store"
)

const (
	dashMinPassword = 10
	dashMaxEmail    = 254
	dashMaxPassword = 1024
)

// handleListUsers is GET /api/v1/users (admin).
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.st.ListUsers(r.Context())
	if err != nil {
		s.log.Error("list users", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if users == nil {
		users = []store.User{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// handleCreateUser is POST /api/v1/users (admin).
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	switch {
	case email == "" || !strings.Contains(email, "@") || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@"):
		writeError(w, http.StatusBadRequest, "invalid_request", "email must contain @")
		return
	case len(email) > dashMaxEmail:
		writeError(w, http.StatusBadRequest, "invalid_request", "email too long")
		return
	case len(req.Password) < dashMinPassword:
		writeError(w, http.StatusBadRequest, "invalid_request", "password must be at least 10 characters")
		return
	case len(req.Password) > dashMaxPassword:
		writeError(w, http.StatusBadRequest, "invalid_request", "password too long")
		return
	case !validRole(req.Role):
		writeError(w, http.StatusBadRequest, "invalid_request", "role must be admin, operator or viewer")
		return
	}
	hash, err := s.hashPassword(req.Password)
	if err != nil {
		s.log.Error("hash password", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	ctx := r.Context()
	u, err := s.st.CreateUser(ctx, email, hash, req.Role)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "a user with that email exists")
		return
	case err != nil:
		s.log.Error("create user", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, nil, "user.created", "user", &u.ID, clientIP(r), map[string]any{
		"email": u.Email,
		"role":  u.Role,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"user": u})
}

// handleDisableUser is POST /api/v1/users/{id}/disable (admin). An admin
// cannot disable their own account.
func (s *Server) handleDisableUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(r, "id")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "")
		return
	}
	me := userFrom(r.Context())
	if me == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	if me.ID == id {
		writeError(w, http.StatusBadRequest, "invalid_request", "you cannot disable your own account")
		return
	}
	ctx := r.Context()
	target, err := s.st.GetUser(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get user")
		return
	}
	if err := s.st.DisableUser(ctx, id); err != nil {
		s.dashStoreError(w, err, "disable user")
		return
	}
	actor := me.ID
	s.audit(ctx, "user", &actor, nil, "user.disabled", "user", &id, clientIP(r), map[string]any{
		"email": target.Email,
		"role":  target.Role,
	})
	w.WriteHeader(http.StatusNoContent)
}
