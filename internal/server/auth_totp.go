package server

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sxi9/msp/internal/store"
)

const (
	// pendingTOTPCookie carries the short-lived token issued by step one of a
	// two-step (2FA) login. It is NOT a session: it only names which user is
	// mid-login and expires after pendingTOTPTTL.
	pendingTOTPCookie = "msp_pending_totp"
	pendingTOTPTTL    = 5 * time.Minute
	recoveryCodeCount = 10
)

func init() {
	dashboardRoutes = append(dashboardRoutes, registerTOTP)
}

// registerTOTP wires the TF1 endpoints: the second login step (csrf only, like
// login), the self-service enrolment/management endpoints (session + csrf, any
// role) and the admin reset (admin role).
func registerTOTP(s *Server, mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/login/totp", s.csrf(s.handleLoginTOTP))
	mux.Handle("POST /api/v1/me/totp/setup", s.sessionAuth(s.csrf(s.handleTOTPSetup)))
	mux.Handle("POST /api/v1/me/totp/confirm", s.sessionAuth(s.csrf(s.handleTOTPConfirm)))
	mux.Handle("POST /api/v1/me/totp/disable", s.sessionAuth(s.csrf(s.handleTOTPDisable)))
	mux.Handle("POST /api/v1/me/totp/recovery/regenerate", s.sessionAuth(s.csrf(s.handleTOTPRegenerate)))
	mux.Handle("POST /api/v1/users/{id}/totp/reset", s.dashRoute(RoleAdmin, s.handleTOTPReset))
}

// --- pending-login token store ----------------------------------------------

// pendingTOTPStore holds the in-memory tokens issued between the two login
// steps. They live at most pendingTOTPTTL and are keyed by the sha256 of the
// opaque cookie value, so the plaintext is never retained. One server process
// backs the single SQLite database, so in-memory state is sufficient and avoids
// a schema table for a 5-minute credential.
type pendingTOTPStore struct {
	mu sync.Mutex
	m  map[string]pendingTOTPEntry
}

type pendingTOTPEntry struct {
	userID    int64
	expiresAt time.Time
}

func newPendingTOTPStore() *pendingTOTPStore {
	return &pendingTOTPStore{m: make(map[string]pendingTOTPEntry)}
}

// create issues a pending token for userID, sweeps expired entries and returns
// the plaintext cookie value.
func (p *pendingTOTPStore) create(userID int64, now time.Time) string {
	plain := store.NewToken("pt_")
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.m {
		if !e.expiresAt.After(now) {
			delete(p.m, k)
		}
	}
	p.m[store.HashToken(plain)] = pendingTOTPEntry{userID: userID, expiresAt: now.Add(pendingTOTPTTL)}
	return plain
}

// lookup resolves a plaintext token to its user id without consuming it; it
// drops the entry and returns false when it is unknown or expired.
func (p *pendingTOTPStore) lookup(plain string, now time.Time) (int64, bool) {
	h := store.HashToken(plain)
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[h]
	if !ok {
		return 0, false
	}
	if !e.expiresAt.After(now) {
		delete(p.m, h)
		return 0, false
	}
	return e.userID, true
}

// remove consumes a pending token (on a successful second step).
func (p *pendingTOTPStore) remove(plain string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, store.HashToken(plain))
}

// pendingCookie builds (or, with a non-positive ttl, clears) the pending-login
// cookie, matching the session cookie's protective flags.
func (s *Server) pendingCookie(r *http.Request, value string, ttl time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     pendingTOTPCookie,
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

// --- recovery codes ---------------------------------------------------------

// recoveryAlphabet is the lower-case base32 alphabet recovery codes are drawn
// from (no 0/1/8/9, so they are unambiguous to read and type).
const recoveryAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// newRecoveryCode returns one random recovery code formatted "xxxxx-xxxxx".
func newRecoveryCode() string {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("server: crypto/rand: " + err.Error())
	}
	var b strings.Builder
	for i, v := range raw {
		if i == 5 {
			b.WriteByte('-')
		}
		b.WriteByte(recoveryAlphabet[int(v)%len(recoveryAlphabet)])
	}
	return b.String()
}

// canonicalRecoveryCode normalises a recovery code for hashing/compare: lower
// case, only [a-z2-7] kept, re-hyphenated as xxxxx-xxxxx when ten chars long. So
// a user may type it with or without the dash, in any case.
func canonicalRecoveryCode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '2' && r <= '7') {
			b.WriteRune(r)
		}
	}
	cleaned := b.String()
	if len(cleaned) == 10 {
		return cleaned[:5] + "-" + cleaned[5:]
	}
	return cleaned
}

// generateRecoveryCodes returns recoveryCodeCount fresh codes (plaintext, shown
// once) and their sha256 hashes (stored).
func generateRecoveryCodes() (codes []string, hashes []string) {
	codes = make([]string, recoveryCodeCount)
	hashes = make([]string, recoveryCodeCount)
	for i := range codes {
		c := newRecoveryCode()
		codes[i] = c
		hashes[i] = store.HashToken(canonicalRecoveryCode(c))
	}
	return codes, hashes
}

// verifySecondFactor checks code against the user's TOTP secret and, failing
// that, against the user's unused recovery codes (consuming one). It reports
// success and whether a recovery code was used.
func (s *Server) verifySecondFactor(ctx context.Context, userID int64, secret, code string) (ok bool, usedRecovery bool, err error) {
	if step, valid := totpVerifyStep(secret, code, s.now()); valid {
		// RFC 6238 Section 5.2: a validated TOTP code is accepted at most once.
		// Burn the matched step atomically; a replay of the same (or any older
		// in-window) code fails here even when presented with a fresh pending
		// token, so a captured still-valid code cannot mint a second session.
		consumed, cerr := s.st.ConsumeTOTPStep(ctx, userID, step)
		if cerr != nil {
			return false, false, cerr
		}
		return consumed, false, nil
	}
	consumed, err := s.st.ConsumeRecoveryCode(ctx, userID, store.HashToken(canonicalRecoveryCode(code)))
	if err != nil {
		return false, false, err
	}
	return consumed, consumed, nil
}

// --- handlers ---------------------------------------------------------------

// handleLoginTOTP is POST /api/v1/login/totp: the second step of a two-step
// login. It reads the pending cookie, verifies a TOTP or recovery code,
// consumes the pending token and creates the real session.
func (s *Server) handleLoginTOTP(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.loginLimit.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "")
		return
	}
	c, err := r.Cookie(pendingTOTPCookie)
	if err != nil || c.Value == "" {
		writeError(w, http.StatusUnauthorized, "pending_expired", "")
		return
	}
	userID, ok := s.pendingTOTP.lookup(c.Value, s.now())
	if !ok {
		http.SetCookie(w, s.pendingCookie(r, "", 0))
		writeError(w, http.StatusUnauthorized, "pending_expired", "")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	ctx := r.Context()
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		// The user vanished (deleted) between the two steps.
		http.SetCookie(w, s.pendingCookie(r, "", 0))
		s.pendingTOTP.remove(c.Value)
		writeError(w, http.StatusUnauthorized, "pending_expired", "")
		return
	}
	if u.DisabledAt != nil {
		http.SetCookie(w, s.pendingCookie(r, "", 0))
		s.pendingTOTP.remove(c.Value)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "")
		return
	}
	secret, enabled, _, err := s.st.TOTPStatus(ctx, userID)
	if err != nil {
		s.log.Error("login totp status", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if !enabled {
		// 2FA was turned off in the window; the pending token is now meaningless.
		http.SetCookie(w, s.pendingCookie(r, "", 0))
		s.pendingTOTP.remove(c.Value)
		writeError(w, http.StatusUnauthorized, "pending_expired", "")
		return
	}
	okCode, usedRecovery, err := s.verifySecondFactor(ctx, userID, secret, req.Code)
	if err != nil {
		s.log.Error("login totp verify", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if !okCode {
		s.audit(ctx, "user", &u.ID, nil, "user.login_totp_failed", "user", &u.ID, ip, map[string]any{"email": u.Email})
		writeError(w, http.StatusUnauthorized, "invalid_code", "")
		return
	}

	s.pendingTOTP.remove(c.Value)
	http.SetCookie(w, s.pendingCookie(r, "", 0))
	plain, err := s.st.CreateSession(ctx, u.ID, ip, sessionTTL)
	if err != nil {
		s.log.Error("create session", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	http.SetCookie(w, s.sessionCookie(r, plain, sessionTTL))
	method := "totp"
	if usedRecovery {
		method = "recovery_code"
	}
	s.audit(ctx, "user", &u.ID, nil, "user.login", "user", &u.ID, ip, map[string]any{"email": u.Email, "second_factor": method})
	writeJSON(w, http.StatusOK, userResponse{User: u})
}

// handleTOTPSetup is POST /api/v1/me/totp/setup: generates and stores a pending
// secret (not yet enabled) and returns it with its otpauth URI. 409 if 2FA is
// already enabled.
func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	me := userFrom(r.Context())
	if me == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	ctx := r.Context()
	_, enabled, _, err := s.st.TOTPStatus(ctx, me.ID)
	if err != nil {
		s.log.Error("totp setup status", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if enabled {
		writeError(w, http.StatusConflict, "conflict", "two-factor authentication is already enabled; disable it first")
		return
	}
	secret := newTOTPSecret()
	if err := s.st.SetTOTPSecret(ctx, me.ID, secret); err != nil {
		s.dashStoreError(w, err, "set totp secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret":      secret,
		"otpauth_uri": otpauthURI(secret, totpIssuer, me.Email),
	})
}

// handleTOTPConfirm is POST /api/v1/me/totp/confirm {code}: verifies the code
// against the pending secret, enables 2FA, generates the recovery codes and
// returns them once.
func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
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
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	secret, enabled, _, err := s.st.TOTPStatus(ctx, me.ID)
	if err != nil {
		s.log.Error("totp confirm status", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if enabled {
		writeError(w, http.StatusConflict, "conflict", "two-factor authentication is already enabled")
		return
	}
	if secret == "" || !totpVerify(secret, req.Code, s.now()) {
		writeError(w, http.StatusBadRequest, "invalid_code", "")
		return
	}
	if err := s.st.EnableTOTP(ctx, me.ID, s.now()); err != nil {
		s.dashStoreError(w, err, "enable totp")
		return
	}
	codes, hashes := generateRecoveryCodes()
	if err := s.st.ReplaceRecoveryCodes(ctx, me.ID, hashes); err != nil {
		s.dashStoreError(w, err, "replace recovery codes")
		return
	}
	s.audit(ctx, "user", &me.ID, nil, "user.totp_enabled", "user", &me.ID, ip, map[string]any{"email": me.Email})
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// handleTOTPDisable is POST /api/v1/me/totp/disable {code}: a valid current
// TOTP code or a recovery code turns 2FA off and clears the secret and codes.
func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
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
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	secret, enabled, _, err := s.st.TOTPStatus(ctx, me.ID)
	if err != nil {
		s.log.Error("totp disable status", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if !enabled {
		writeError(w, http.StatusBadRequest, "invalid_request", "two-factor authentication is not enabled")
		return
	}
	okCode, _, err := s.verifySecondFactor(ctx, me.ID, secret, req.Code)
	if err != nil {
		s.log.Error("totp disable verify", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if !okCode {
		writeError(w, http.StatusBadRequest, "invalid_code", "")
		return
	}
	if err := s.st.DisableTOTP(ctx, me.ID); err != nil {
		s.dashStoreError(w, err, "disable totp")
		return
	}
	s.audit(ctx, "user", &me.ID, nil, "user.totp_disabled", "user", &me.ID, ip, map[string]any{"email": me.Email})
	w.WriteHeader(http.StatusNoContent)
}

// handleTOTPRegenerate is POST /api/v1/me/totp/recovery/regenerate {code}: with
// a valid current TOTP code, replaces the recovery codes and returns the new
// set once.
func (s *Server) handleTOTPRegenerate(w http.ResponseWriter, r *http.Request) {
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
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	secret, enabled, _, err := s.st.TOTPStatus(ctx, me.ID)
	if err != nil {
		s.log.Error("totp regenerate status", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if !enabled {
		writeError(w, http.StatusBadRequest, "invalid_request", "two-factor authentication is not enabled")
		return
	}
	if !totpVerify(secret, req.Code, s.now()) {
		writeError(w, http.StatusBadRequest, "invalid_code", "")
		return
	}
	codes, hashes := generateRecoveryCodes()
	if err := s.st.ReplaceRecoveryCodes(ctx, me.ID, hashes); err != nil {
		s.dashStoreError(w, err, "replace recovery codes")
		return
	}
	s.audit(ctx, "user", &me.ID, nil, "user.totp_recovery_regenerated", "user", &me.ID, ip, map[string]any{"email": me.Email})
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// handleTOTPReset is POST /api/v1/users/{id}/totp/reset (admin): disables a
// locked-out user's 2FA so they can re-enrol. 404 when the user does not exist.
func (s *Server) handleTOTPReset(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	// RS2 (TF1 spec line 47): the target must be visible before any side effect.
	// This route is gated to RoleAdmin (dashRoute(RoleAdmin, ...)) and an admin is
	// always all-orgs, so every user is visible to the caller and the uniform
	// visible-org check is a no-op here — the admin-all-orgs invariant is what
	// keeps it safe, exactly as the sibling admin user endpoints (handleDisableUser,
	// handleSetUserOrgs) rely on. A non-existent id still returns 404 via
	// GetUser/dashStoreError below. If this route were ever opened to a scoped
	// role, add an explicit visible-user check here before DisableTOTP.
	target, err := s.st.GetUser(ctx, id)
	if err != nil {
		s.dashStoreError(w, err, "get user")
		return
	}
	if err := s.st.DisableTOTP(ctx, id); err != nil {
		s.dashStoreError(w, err, "reset totp")
		return
	}
	actor := me.ID
	s.audit(ctx, "user", &actor, nil, "user.totp_reset", "user", &id, clientIP(r), map[string]any{
		"email": target.Email,
		"role":  target.Role,
	})
	w.WriteHeader(http.StatusNoContent)
}
