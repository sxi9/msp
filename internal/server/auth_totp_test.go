package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// enableTOTP runs the real setup -> confirm flow for the session behind cookie,
// returning the secret and the recovery codes handed back once. It also asserts
// the audit and the me response reflect the enabled state.
func (e *testEnv) enableTOTP(cookie *http.Cookie) (secret string, recovery []string) {
	e.t.Helper()
	resp, body := e.do(http.MethodPost, "/api/v1/me/totp/setup", struct{}{}, withCookie(cookie, csrfHeader))
	if resp.StatusCode != 200 {
		e.t.Fatalf("setup: %d %s", resp.StatusCode, body)
	}
	var setup struct {
		Secret     string `json:"secret"`
		OTPAuthURI string `json:"otpauth_uri"`
	}
	if err := json.Unmarshal(body, &setup); err != nil || setup.Secret == "" {
		e.t.Fatalf("setup body %s (%v)", body, err)
	}
	if !strings.Contains(setup.OTPAuthURI, "otpauth://totp/MSP:") || !strings.Contains(setup.OTPAuthURI, "secret="+setup.Secret) {
		e.t.Fatalf("otpauth_uri %q", setup.OTPAuthURI)
	}
	code := totpNow(setup.Secret, e.clock.now())
	resp, body = e.do(http.MethodPost, "/api/v1/me/totp/confirm", map[string]string{"code": code}, withCookie(cookie, csrfHeader))
	if resp.StatusCode != 200 {
		e.t.Fatalf("confirm: %d %s", resp.StatusCode, body)
	}
	var conf struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(body, &conf); err != nil || len(conf.RecoveryCodes) != recoveryCodeCount {
		e.t.Fatalf("confirm body %s (%v)", body, err)
	}
	return setup.Secret, conf.RecoveryCodes
}

func TestTOTPSetupConfirmAndMe(t *testing.T) {
	e := newTestEnv(t)
	_, _, cookie := e.login("admin@example.com", testAdminPassword)

	// Before enrolment, me reports 2FA off and zero recovery codes.
	resp, body := e.do(http.MethodGet, "/api/v1/me", nil, withCookie(cookie, nil))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"totp_enabled":false`) || !strings.Contains(string(body), `"recovery_codes_remaining":0`) {
		t.Fatalf("me before: %d %s", resp.StatusCode, body)
	}

	secret, recovery := e.enableTOTP(cookie)
	_ = secret

	// A wrong confirm code before enabling would have been 400; now that it is
	// enabled, setup is 409 and confirm is 409.
	if resp, _ := e.do(http.MethodPost, "/api/v1/me/totp/setup", struct{}{}, withCookie(cookie, csrfHeader)); resp.StatusCode != 409 {
		t.Fatalf("setup when enabled: %d", resp.StatusCode)
	}

	// me now reports enabled and the full set of recovery codes remaining.
	resp, body = e.do(http.MethodGet, "/api/v1/me", nil, withCookie(cookie, nil))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"totp_enabled":true`) || !strings.Contains(string(body), `"recovery_codes_remaining":10`) {
		t.Fatalf("me after: %d %s", resp.StatusCode, body)
	}
	if a := e.auditActions("user.totp_enabled"); len(a) != 1 || a[0].ActorID == nil || *a[0].ActorID != e.admin.ID {
		t.Fatalf("audit user.totp_enabled %+v", a)
	}

	// Recovery codes look like xxxxx-xxxxx and are distinct.
	seen := map[string]bool{}
	for _, c := range recovery {
		if len(c) != 11 || c[5] != '-' {
			t.Fatalf("recovery code shape %q", c)
		}
		if seen[c] {
			t.Fatalf("duplicate recovery code %q", c)
		}
		seen[c] = true
	}
}

func TestTOTPConfirmWrongCode(t *testing.T) {
	e := newTestEnv(t)
	_, _, cookie := e.login("admin@example.com", testAdminPassword)
	if resp, _ := e.do(http.MethodPost, "/api/v1/me/totp/setup", struct{}{}, withCookie(cookie, csrfHeader)); resp.StatusCode != 200 {
		t.Fatalf("setup")
	}
	resp, body := e.do(http.MethodPost, "/api/v1/me/totp/confirm", map[string]string{"code": "000000"}, withCookie(cookie, csrfHeader))
	if resp.StatusCode != 400 || errorCode(t, body) != "invalid_code" {
		t.Fatalf("confirm wrong: %d %s", resp.StatusCode, body)
	}
	// Still not enabled.
	resp, body = e.do(http.MethodGet, "/api/v1/me", nil, withCookie(cookie, nil))
	if !strings.Contains(string(body), `"totp_enabled":false`) {
		t.Fatalf("me after bad confirm: %s", body)
	}
}

// loginExpectTOTP does step one of a login for a 2FA user: it expects
// totp_required=true, no session cookie, and returns the pending cookie.
func (e *testEnv) loginExpectTOTP(email, password string) *http.Cookie {
	e.t.Helper()
	resp, body := e.do(http.MethodPost, "/api/v1/login", map[string]string{"email": email, "password": password}, csrfHeader)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"totp_required":true`) {
		e.t.Fatalf("login step1: %d %s", resp.StatusCode, body)
	}
	var pending *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			e.t.Fatalf("login step1 set a full session cookie")
		}
		if c.Name == pendingTOTPCookie {
			pending = c
		}
	}
	if pending == nil || pending.Value == "" {
		e.t.Fatalf("no pending cookie: %v", resp.Cookies())
	}
	return pending
}

func TestTwoStepLogin(t *testing.T) {
	e := newTestEnv(t)
	_, _, cookie := e.login("admin@example.com", testAdminPassword)
	secret, _ := e.enableTOTP(cookie)

	// Step one: password alone returns totp_required and a pending cookie, no
	// session.
	pending := e.loginExpectTOTP("admin@example.com", testAdminPassword)

	// A wrong second-factor code is 401 invalid_code and audited; no session.
	resp, body := e.do(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": "000000"}, withCookie(pending, csrfHeader))
	if resp.StatusCode != 401 || errorCode(t, body) != "invalid_code" {
		t.Fatalf("login/totp wrong: %d %s", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("wrong code issued a session")
		}
	}
	if a := e.auditActions("user.login_totp_failed"); len(a) != 1 {
		t.Fatalf("audit login_totp_failed %d", len(a))
	}

	// A stale code (computed for a step two windows ago) is rejected.
	stale := totpNow(secret, e.clock.now().Add(-2*totpPeriod))
	if resp, body := e.do(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": stale}, withCookie(pending, csrfHeader)); resp.StatusCode != 401 {
		t.Fatalf("login/totp stale: %d %s", resp.StatusCode, body)
	}

	// The correct code completes the login: a real session is issued.
	code := totpNow(secret, e.clock.now())
	resp, body = e.do(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": code}, withCookie(pending, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("login/totp ok: %d %s", resp.StatusCode, body)
	}
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("no session after login/totp: %v", resp.Cookies())
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/me", nil, withCookie(session, nil)); resp.StatusCode != 200 {
		t.Fatalf("session from login/totp not usable: %d", resp.StatusCode)
	}
	if a := e.auditActions("user.login"); len(a) == 0 {
		t.Fatal("no user.login audit after two-step login")
	} else if a[0].Detail["second_factor"] != "totp" { // newest first (ListAudit DESC)
		t.Fatalf("login audit second_factor %v", a[0].Detail)
	}

	// The pending token is single-use: reusing it (even with a fresh code) fails.
	code2 := totpNow(secret, e.clock.now())
	if resp, _ := e.do(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": code2}, withCookie(pending, csrfHeader)); resp.StatusCode != 401 {
		t.Fatalf("reused pending token: %d", resp.StatusCode)
	}
}

func TestTwoStepLoginPendingExpiry(t *testing.T) {
	e := newTestEnv(t)
	_, _, cookie := e.login("admin@example.com", testAdminPassword)
	secret, _ := e.enableTOTP(cookie)

	pending := e.loginExpectTOTP("admin@example.com", testAdminPassword)
	// Past the 5-minute pending window the token is dead.
	e.clock.advance(pendingTOTPTTL + time.Second)
	code := totpNow(secret, e.clock.now())
	if resp, body := e.do(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": code}, withCookie(pending, csrfHeader)); resp.StatusCode != 401 || errorCode(t, body) != "pending_expired" {
		t.Fatalf("expired pending: %d %s", resp.StatusCode, body)
	}
	// With no pending cookie at all: 401 pending_expired.
	if resp, body := e.do(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": code}, csrfHeader); resp.StatusCode != 401 || errorCode(t, body) != "pending_expired" {
		t.Fatalf("missing pending: %d %s", resp.StatusCode, body)
	}
}

func TestRecoveryCodeLogin(t *testing.T) {
	e := newTestEnv(t)
	_, _, cookie := e.login("admin@example.com", testAdminPassword)
	_, recovery := e.enableTOTP(cookie)

	// Log in with a recovery code once.
	pending := e.loginExpectTOTP("admin@example.com", testAdminPassword)
	resp, body := e.do(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": recovery[0]}, withCookie(pending, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("recovery login: %d %s", resp.StatusCode, body)
	}
	if a := e.auditActions("user.login"); a[0].Detail["second_factor"] != "recovery_code" { // newest first
		t.Fatalf("recovery login audit %v", a[0].Detail)
	}

	// The same recovery code cannot be reused.
	pending2 := e.loginExpectTOTP("admin@example.com", testAdminPassword)
	if resp, body := e.do(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": recovery[0]}, withCookie(pending2, csrfHeader)); resp.StatusCode != 401 {
		t.Fatalf("reused recovery code: %d %s", resp.StatusCode, body)
	}

	// Log in with a second recovery code to get a session, then check me shows
	// two fewer remaining (8 of 10).
	pending3 := e.loginExpectTOTP("admin@example.com", testAdminPassword)
	resp, _ = e.do(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": recovery[1]}, withCookie(pending3, csrfHeader))
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session from second recovery login")
	}
	resp, body = e.do(http.MethodGet, "/api/v1/me", nil, withCookie(session, nil))
	if !strings.Contains(string(body), `"recovery_codes_remaining":8`) {
		t.Fatalf("me recovery remaining after two used: %s", body)
	}
}

func TestTOTPDisableAndRegenerate(t *testing.T) {
	e := newTestEnv(t)
	_, _, cookie := e.login("admin@example.com", testAdminPassword)
	secret, _ := e.enableTOTP(cookie)

	// Regenerate with a valid code returns a new set and the old codes stop
	// working (verified by the remaining count resetting to 10).
	code := totpNow(secret, e.clock.now())
	resp, body := e.do(http.MethodPost, "/api/v1/me/totp/recovery/regenerate", map[string]string{"code": code}, withCookie(cookie, csrfHeader))
	if resp.StatusCode != 200 {
		t.Fatalf("regenerate: %d %s", resp.StatusCode, body)
	}
	var reg struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(body, &reg); err != nil || len(reg.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("regenerate body %s", body)
	}
	if a := e.auditActions("user.totp_recovery_regenerated"); len(a) != 1 {
		t.Fatalf("audit regenerate %d", len(a))
	}

	// Disable with a wrong code fails.
	if resp, body := e.do(http.MethodPost, "/api/v1/me/totp/disable", map[string]string{"code": "000000"}, withCookie(cookie, csrfHeader)); resp.StatusCode != 400 || errorCode(t, body) != "invalid_code" {
		t.Fatalf("disable wrong: %d %s", resp.StatusCode, body)
	}
	// Disable with a valid code turns 2FA off.
	code = totpNow(secret, e.clock.now())
	if resp, body := e.do(http.MethodPost, "/api/v1/me/totp/disable", map[string]string{"code": code}, withCookie(cookie, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("disable ok: %d %s", resp.StatusCode, body)
	}
	if a := e.auditActions("user.totp_disabled"); len(a) != 1 {
		t.Fatalf("audit disabled %d", len(a))
	}

	// me reports off; a subsequent login is single-step again.
	resp, body = e.do(http.MethodGet, "/api/v1/me", nil, withCookie(cookie, nil))
	if !strings.Contains(string(body), `"totp_enabled":false`) || !strings.Contains(string(body), `"recovery_codes_remaining":0`) {
		t.Fatalf("me after disable: %s", body)
	}
	resp, _, session := e.login("admin@example.com", testAdminPassword)
	if resp.StatusCode != 200 || session == nil {
		t.Fatalf("single-step login after disable: %d cookie=%v", resp.StatusCode, session)
	}
}

func TestTOTPAdminReset(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// A second admin whose 2FA we will reset (so the acting admin is not
	// resetting themselves).
	hash, _ := e.srv.hashPassword("other-admin-123")
	other, err := e.st.CreateUser(ctx, "other@example.com", hash, RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, _, otherCookie := e.login("other@example.com", "other-admin-123")
	secret, _ := e.enableTOTP(otherCookie)

	// The acting admin resets the other user's 2FA.
	_, _, admin := e.login("admin@example.com", testAdminPassword)
	resp, body := e.do(http.MethodPost, "/api/v1/users/"+itoa(other.ID)+"/totp/reset", struct{}{}, withCookie(admin, csrfHeader))
	if resp.StatusCode != 204 {
		t.Fatalf("reset: %d %s", resp.StatusCode, body)
	}
	if a := e.auditActions("user.totp_reset"); len(a) != 1 || a[0].ActorID == nil || *a[0].ActorID != e.admin.ID || a[0].TargetID == nil || *a[0].TargetID != other.ID {
		t.Fatalf("audit totp_reset %+v", a)
	}
	// A missing user is 404.
	if resp, _ := e.do(http.MethodPost, "/api/v1/users/999999/totp/reset", struct{}{}, withCookie(admin, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("reset missing: %d", resp.StatusCode)
	}

	// The target can now log in in a single step; the old secret is gone.
	_ = secret
	resp2, _, session := e.login("other@example.com", "other-admin-123")
	if resp2.StatusCode != 200 || session == nil {
		t.Fatalf("login after reset: %d cookie=%v", resp2.StatusCode, session)
	}

	// Reset requires admin: an operator is forbidden.
	hash2, _ := e.srv.hashPassword("op-pass-12345")
	if _, err := e.st.CreateUser(ctx, "op@example.com", hash2, RoleOperator); err != nil {
		t.Fatal(err)
	}
	_, _, op := e.login("op@example.com", "op-pass-12345")
	if resp, _ := e.do(http.MethodPost, "/api/v1/users/"+itoa(other.ID)+"/totp/reset", struct{}{}, withCookie(op, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("operator reset: %d", resp.StatusCode)
	}
}

// TestTOTPSecretNeverReturned confirms the secret is not exposed by me or the
// users list, only by setup (where the owner needs it to enrol).
func TestTOTPSecretNeverReturned(t *testing.T) {
	e := newTestEnv(t)
	_, _, cookie := e.login("admin@example.com", testAdminPassword)
	secret, _ := e.enableTOTP(cookie)

	resp, body := e.do(http.MethodGet, "/api/v1/me", nil, withCookie(cookie, nil))
	if resp.StatusCode != 200 || strings.Contains(string(body), secret) || strings.Contains(string(body), "totp_secret") {
		t.Fatalf("me leaks secret: %s", body)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/users", nil, withCookie(cookie, nil))
	if strings.Contains(string(body), secret) || strings.Contains(string(body), "totp_secret") {
		t.Fatalf("users list leaks secret: %s", body)
	}

	// The store read never carries the secret either.
	u, err := e.st.GetUser(context.Background(), e.admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !u.TOTPEnabled {
		t.Fatal("GetUser totp_enabled false after enable")
	}
}
