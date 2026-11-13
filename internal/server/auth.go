package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/sxi9/msp/internal/store"
)

const (
	sessionCookie = "msp_session"
	sessionTTL    = 12 * time.Hour
	bcryptCost    = 12
	// dummyHash is a real bcrypt cost-12 hash compared against when the email
	// is unknown, so a failed login costs the same time either way.
	dummyHash = "$2a$12$Dw7nVG2qRFSDwa2UUWgYY.UA9CAEHEv6KuN2Jpo2R1VW1p3nOb7uu"
)

// hashPassword returns the bcrypt (cost 12) hash of pw.
func (s *Server) hashPassword(pw string) (string, error) {
	return HashPassword(pw)
}

// validRole reports whether r is admin, operator or viewer.
func validRole(r string) bool {
	return roleRank(r) >= 0
}

// audit appends one audit row, logging (never failing the request) when the
// write itself fails.
func (s *Server) audit(ctx context.Context, actorType string, actorID *int64, orgID *int64, action, targetType string, targetID *int64, ip string, detail map[string]any) {
	err := s.st.Audit(ctx, store.AuditEntry{
		ActorType:  actorType,
		ActorID:    actorID,
		OrgID:      orgID,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		IP:         ip,
		Detail:     detail,
	})
	if err != nil {
		s.log.Error("audit write", "action", action, "err", err)
	}
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userResponse struct {
	User store.User `json:"user"`
}

// handleLogin is POST /api/v1/login.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.loginLimit.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "")
		return
	}
	var req loginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "email and password are required")
		return
	}

	ctx := r.Context()
	u, err := s.st.GetUserByEmail(ctx, email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Burn the same bcrypt time as a real comparison so unknown and
		// known emails are indistinguishable by latency.
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(req.Password))
		s.audit(ctx, "user", nil, nil, "user.login_failed", "user", nil, ip, map[string]any{"email": email, "reason": "unknown_user"})
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "")
		return
	case err != nil:
		s.log.Error("login lookup", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		s.audit(ctx, "user", &u.ID, nil, "user.login_failed", "user", &u.ID, ip, map[string]any{"email": email, "reason": "bad_password"})
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "")
		return
	}
	if u.DisabledAt != nil {
		s.audit(ctx, "user", &u.ID, nil, "user.login_failed", "user", &u.ID, ip, map[string]any{"email": email, "reason": "disabled"})
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "")
		return
	}

	plain, err := s.st.CreateSession(ctx, u.ID, ip, sessionTTL)
	if err != nil {
		s.log.Error("create session", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	http.SetCookie(w, s.sessionCookie(r, plain, sessionTTL))
	s.audit(ctx, "user", &u.ID, nil, "user.login", "user", &u.ID, ip, map[string]any{"email": email})
	writeJSON(w, http.StatusOK, userResponse{User: u})
}

// sessionCookie builds the msp_session cookie; a non-positive ttl clears it.
func (s *Server) sessionCookie(r *http.Request, value string, ttl time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.cfg.SecureCookies || r.TLS != nil,
	}
	if ttl > 0 {
		c.MaxAge = int(ttl / time.Second)
	} else {
		c.MaxAge = -1
	}
	return c
}

// handleLogout is POST /api/v1/logout: deletes the session if the cookie
// names one and clears the cookie; always 204.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.st.DeleteSession(r.Context(), c.Value); err != nil {
			s.log.Error("delete session", "err", err)
		}
	}
	http.SetCookie(w, s.sessionCookie(r, "", 0))
	w.WriteHeader(http.StatusNoContent)
}

// handleMe is GET /api/v1/me (inside sessionAuth).
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	writeJSON(w, http.StatusOK, userResponse{User: *u})
}

// HashPassword is hashPassword for callers outside the package (the CLI's
// `user add`).
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}
