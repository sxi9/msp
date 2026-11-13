package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/sxi9/msp/internal/store"
)

const (
	dashMinPassword = 10
	dashMaxEmail    = 254
	dashMaxPassword = 1024
)

// userWithOrgs is a user row plus its per-technician org assignment (RS2):
// all_orgs, and org_ids (empty when all_orgs). The embedded store.User's
// fields are promoted, so the JSON is a user object with two extra fields.
type userWithOrgs struct {
	store.User
	AllOrgs bool    `json:"all_orgs"`
	OrgIDs  []int64 `json:"org_ids"`
}

// withOrgs loads a user's org assignment and wraps it for the response.
func (s *Server) withOrgs(ctx context.Context, u store.User) (userWithOrgs, error) {
	all, ids, err := s.st.GetUserOrgs(ctx, u.ID)
	if err != nil {
		return userWithOrgs{}, err
	}
	return userWithOrgs{User: u, AllOrgs: all, OrgIDs: ids}, nil
}

// handleListUsers is GET /api/v1/users (admin): each row carries all_orgs and
// org_ids (RS2).
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := s.st.ListUsers(ctx)
	if err != nil {
		s.log.Error("list users", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	out := make([]userWithOrgs, 0, len(users))
	for _, u := range users {
		wo, err := s.withOrgs(ctx, u)
		if err != nil {
			s.log.Error("list users: org assignment", "user", u.ID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "")
			return
		}
		out = append(out, wo)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// handleCreateUser is POST /api/v1/users (admin). For an operator or viewer the
// body may carry "all_orgs":false with "org_ids":[...] to scope the new user
// (RS2); an admin is always all-orgs, so the scoping fields are ignored for the
// admin role. A missing all_orgs keeps today's full visibility.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string  `json:"email"`
		Password string  `json:"password"`
		Role     string  `json:"role"`
		AllOrgs  *bool   `json:"all_orgs"`
		OrgIDs   []int64 `json:"org_ids"`
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
	ctx := r.Context()
	// A scoped create is only for operator/viewer; validate the org ids up
	// front so the account is never created against a non-existent org.
	scope := req.Role != RoleAdmin && req.AllOrgs != nil && !*req.AllOrgs
	if scope {
		if detail, ok := s.dashValidateOrgIDs(ctx, req.OrgIDs); !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", detail)
			return
		}
	}
	hash, err := s.hashPassword(req.Password)
	if err != nil {
		s.log.Error("hash password", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	// Create the account and apply its scoping in one store transaction: a
	// scoped create either commits with all_orgs=0 and the org rows, or commits
	// nothing, so a failure never leaves a fully-visible (all_orgs=1) account
	// behind (the column default). allOrgs true (the unscoped path, and always
	// for an admin) keeps today's full visibility.
	u, err := s.st.CreateUserScoped(ctx, email, hash, req.Role, !scope, req.OrgIDs)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "a user with that email exists")
		return
	case errors.Is(err, store.ErrNotFound):
		// A validated org was deleted in the window before the insert; nothing
		// was committed, so no account exists. Report it like the up-front check.
		writeError(w, http.StatusBadRequest, "invalid_request", "org_ids contains an org that does not exist")
		return
	case err != nil:
		s.log.Error("create user", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	wo, err := s.withOrgs(ctx, u)
	if err != nil {
		s.log.Error("create user: org assignment", "user", u.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	actor, _ := dashActor(r)
	s.audit(ctx, "user", actor, nil, "user.created", "user", &u.ID, clientIP(r), map[string]any{
		"email":    u.Email,
		"role":     u.Role,
		"all_orgs": wo.AllOrgs,
		"org_ids":  wo.OrgIDs,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"user": wo})
}

// dashValidateOrgIDs reports whether every id names an existing org, returning
// the 400 detail otherwise.
func (s *Server) dashValidateOrgIDs(ctx context.Context, ids []int64) (detail string, ok bool) {
	for _, id := range ids {
		if id <= 0 {
			return "org_ids must be positive integers", false
		}
		exists, err := s.st.OrgExists(ctx, id)
		if err != nil {
			s.log.Error("validate org ids", "org", id, "err", err)
			return "could not validate org_ids", false
		}
		if !exists {
			return "org_ids contains an org that does not exist", false
		}
	}
	return "", true
}

// handleSetUserOrgs is PUT /api/v1/users/{id}/orgs (admin): replaces a user's
// org assignment with {"all_orgs":bool,"org_ids":[...]} and audits
// user.orgs_changed with the new set (RS2). 404 when the user does not exist,
// 400 when the target is an admin and all_orgs is false (admins cannot be
// scoped) or an org id does not exist.
func (s *Server) handleSetUserOrgs(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		AllOrgs bool    `json:"all_orgs"`
		OrgIDs  []int64 `json:"org_ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	target, err := s.st.GetUser(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get user")
		return
	}
	if target.Role == RoleAdmin && !req.AllOrgs {
		writeError(w, http.StatusBadRequest, "invalid_request", "an admin is always all-orgs and cannot be scoped")
		return
	}
	if !req.AllOrgs {
		if detail, ok := s.dashValidateOrgIDs(ctx, req.OrgIDs); !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", detail)
			return
		}
	}
	if err := s.st.SetUserOrgs(ctx, id, req.AllOrgs, req.OrgIDs); err != nil {
		s.dashStoreError(w, err, "set user orgs")
		return
	}
	wo, err := s.withOrgs(ctx, target)
	if err != nil {
		s.log.Error("set user orgs: org assignment", "user", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	actor := me.ID
	s.audit(ctx, "user", &actor, nil, "user.orgs_changed", "user", &id, clientIP(r), map[string]any{
		"email":    target.Email,
		"role":     target.Role,
		"all_orgs": wo.AllOrgs,
		"org_ids":  wo.OrgIDs,
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": wo})
}

// validPassword returns the 400 detail for an unacceptable new password, or "".
func validPassword(pw string) string {
	switch {
	case len(pw) < dashMinPassword:
		return "password must be at least 10 characters"
	case len(pw) > dashMaxPassword:
		return "password too long"
	}
	return ""
}

// handleChangePassword is POST /api/v1/me/password {"current_password",
// "new_password"} for the session user of any role: verifies the current
// password (rate limited per IP like login, so a stolen session cannot
// brute-force it), stores the new hash, drops the user's other sessions and
// audits user.password_changed. The current session stays valid.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	me := userFrom(r.Context())
	if me == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	ip := clientIP(r)
	if !s.loginLimit.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "")
		return
	}
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Current == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "current_password is required")
		return
	}
	if detail := validPassword(req.New); detail != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", detail)
		return
	}
	ctx := r.Context()
	if bcrypt.CompareHashAndPassword([]byte(me.PasswordHash), []byte(req.Current)) != nil {
		s.audit(ctx, "user", &me.ID, nil, "user.password_change_failed", "user", &me.ID, ip, map[string]any{"email": me.Email, "reason": "bad_password"})
		writeError(w, http.StatusBadRequest, "invalid_request", "current password is incorrect")
		return
	}
	if req.New == req.Current {
		writeError(w, http.StatusBadRequest, "invalid_request", "new password must differ from the current one")
		return
	}
	hash, err := s.hashPassword(req.New)
	if err != nil {
		s.log.Error("hash password", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	keep := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		keep = c.Value
	}
	if err := s.st.SetPassword(ctx, me.ID, hash, keep); err != nil {
		s.dashStoreError(w, err, "set password")
		return
	}
	s.audit(ctx, "user", &me.ID, nil, "user.password_changed", "user", &me.ID, ip, map[string]any{"email": me.Email})
	w.WriteHeader(http.StatusNoContent)
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
