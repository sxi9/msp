package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/store"
)

var csrfHeader = map[string]string{"X-Requested-With": "msp"}

// login posts credentials and returns the response and the session cookie.
func (e *testEnv) login(email, password string) (*http.Response, []byte, *http.Cookie) {
	e.t.Helper()
	resp, body := e.do(http.MethodPost, "/api/v1/login", map[string]string{"email": email, "password": password}, csrfHeader)
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return resp, body, c
		}
	}
	return resp, body, nil
}

func withCookie(c *http.Cookie, extra map[string]string) map[string]string {
	h := map[string]string{"Cookie": c.Name + "=" + c.Value}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func TestLoginMeLogout(t *testing.T) {
	e := newTestEnv(t)

	resp, body, cookie := e.login("Admin@Example.com", testAdminPassword)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	var out userResponse
	if err := json.Unmarshal(body, &out); err != nil || out.User.Email != "admin@example.com" || out.User.Role != RoleAdmin || out.User.ID != e.admin.ID {
		t.Fatalf("login body %s (%v)", body, err)
	}
	if strings.Contains(string(body), "password") {
		t.Fatalf("login body leaks the password hash: %s", body)
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	if !strings.HasPrefix(cookie.Value, "ss_") || !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteStrictMode || cookie.Secure {
		t.Fatalf("cookie flags %+v", cookie)
	}
	if cookie.MaxAge != 12*3600 {
		t.Fatalf("cookie max-age %d, want 12h", cookie.MaxAge)
	}
	if a := e.auditActions("user.login"); len(a) != 1 || a[0].ActorType != "user" || a[0].ActorID == nil || *a[0].ActorID != e.admin.ID || a[0].ActorLabel != "admin@example.com" {
		t.Fatalf("audit user.login %+v", a)
	}

	resp, body = e.do(http.MethodGet, "/api/v1/me", nil, withCookie(cookie, nil))
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"email":"admin@example.com"`) {
		t.Fatalf("me: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != 401 || errorCode(t, body) != "unauthorized" {
		t.Fatalf("me without cookie: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/me", nil, map[string]string{"Cookie": sessionCookie + "=ss_forged"}); resp.StatusCode != 401 {
		t.Fatalf("me with forged cookie: %d", resp.StatusCode)
	}

	// Logout needs the CSRF header, then invalidates the session.
	if resp, _ := e.do(http.MethodPost, "/api/v1/logout", nil, withCookie(cookie, nil)); resp.StatusCode != 403 {
		t.Fatalf("logout without csrf header: %d", resp.StatusCode)
	}
	resp, _ = e.do(http.MethodPost, "/api/v1/logout", nil, withCookie(cookie, csrfHeader))
	if resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("logout did not clear the cookie: %v", resp.Cookies())
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/me", nil, withCookie(cookie, nil)); resp.StatusCode != 401 {
		t.Fatalf("me after logout: %d", resp.StatusCode)
	}
}

func TestLoginFailures(t *testing.T) {
	e := newTestEnv(t)

	if resp, body, c := e.login("admin@example.com", "wrong-password"); resp.StatusCode != 401 || errorCode(t, body) != "invalid_credentials" || c != nil {
		t.Fatalf("bad password: %d %s cookie=%v", resp.StatusCode, body, c)
	}
	if resp, body, _ := e.login("nobody@example.com", "whatever-pass"); resp.StatusCode != 401 || errorCode(t, body) != "invalid_credentials" {
		t.Fatalf("unknown user: %d %s", resp.StatusCode, body)
	}
	if a := e.auditActions("user.login_failed"); len(a) != 2 {
		t.Fatalf("audit user.login_failed count %d", len(a))
	}
	if resp, body := e.do(http.MethodPost, "/api/v1/login", map[string]string{"email": ""}, csrfHeader); resp.StatusCode != 400 || errorCode(t, body) != "invalid_request" {
		t.Fatalf("empty login: %d %s", resp.StatusCode, body)
	}
	// Missing CSRF header on the login POST itself.
	if resp, body := e.do(http.MethodPost, "/api/v1/login", map[string]string{"email": "admin@example.com", "password": testAdminPassword}, nil); resp.StatusCode != 403 || errorCode(t, body) != "csrf" {
		t.Fatalf("login without csrf header: %d %s", resp.StatusCode, body)
	}

	// Disabled user cannot log in and an existing session dies with it.
	hash, _ := e.srv.hashPassword("viewer-pass-123")
	v, err := e.st.CreateUser(context.Background(), "viewer@example.com", hash, RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	_, _, cookie := e.login("viewer@example.com", "viewer-pass-123")
	if cookie == nil {
		t.Fatal("viewer login failed")
	}
	if err := e.st.DisableUser(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if resp, _, _ := e.login("viewer@example.com", "viewer-pass-123"); resp.StatusCode != 401 {
		t.Fatalf("disabled user login: %d", resp.StatusCode)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/me", nil, withCookie(cookie, nil)); resp.StatusCode != 401 {
		t.Fatalf("disabled user's session still valid: %d", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newTestEnv(t)
	// Pin the limiter's clock: ten bcrypt comparisons take long enough under
	// the race detector for the bucket to refill otherwise.
	e.srv.loginLimit.now = e.clock.now
	for i := 1; i <= 10; i++ {
		if resp, _, _ := e.login("nobody@example.com", "x"); resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	resp, body, _ := e.login("admin@example.com", testAdminPassword)
	if resp.StatusCode != 429 || errorCode(t, body) != "rate_limited" {
		t.Fatalf("11th attempt: %d %s", resp.StatusCode, body)
	}
}

func TestSecureCookieFlag(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.SecureCookies = true
	_, _, cookie := e.login("admin@example.com", testAdminPassword)
	if cookie == nil || !cookie.Secure {
		t.Fatalf("cookie %+v, want Secure", cookie)
	}
}

func TestRequireRoleAndCSRF(t *testing.T) {
	e := newTestEnv(t)
	called := false
	h := e.srv.sessionAuth(e.srv.csrf(e.srv.requireRole(RoleOperator, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})))

	call := func(cookie *http.Cookie, method string, csrf bool) int {
		called = false
		r := httptest.NewRequest(method, "/x", nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf {
			r.Header.Set("X-Requested-With", "msp")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if code := call(nil, http.MethodPost, true); code != 401 {
		t.Fatalf("no session: %d", code)
	}
	_, _, admin := e.login("admin@example.com", testAdminPassword)
	if code := call(admin, http.MethodPost, false); code != 403 || called {
		t.Fatalf("POST without csrf header: %d called=%v", code, called)
	}
	if code := call(admin, http.MethodGet, false); code != 204 || !called {
		t.Fatalf("GET needs no csrf header: %d called=%v", code, called)
	}
	if code := call(admin, http.MethodPost, true); code != 204 || !called {
		t.Fatalf("admin POST: %d called=%v", code, called)
	}

	for _, u := range []struct{ email, role string }{{"op@example.com", RoleOperator}, {"view@example.com", RoleViewer}} {
		hash, _ := e.srv.hashPassword("some-pass-123")
		if _, err := e.st.CreateUser(context.Background(), u.email, hash, u.role); err != nil {
			t.Fatal(err)
		}
	}
	_, _, op := e.login("op@example.com", "some-pass-123")
	if code := call(op, http.MethodPost, true); code != 204 {
		t.Fatalf("operator: %d", code)
	}
	_, _, viewer := e.login("view@example.com", "some-pass-123")
	if code := call(viewer, http.MethodPost, true); code != 403 || called {
		t.Fatalf("viewer: %d called=%v", code, called)
	}

	if roleRank(RoleViewer) != 0 || roleRank(RoleOperator) != 1 || roleRank(RoleAdmin) != 2 || roleRank("root") != -1 {
		t.Fatal("roleRank order")
	}
	if !validRole("admin") || validRole("") || validRole("root") {
		t.Fatal("validRole")
	}
	if u := userFrom(context.Background()); u != nil {
		t.Fatal("userFrom on empty context")
	}
	ctx := context.WithValue(context.Background(), ctxKeyUser, &store.User{ID: 7})
	if u := userFrom(ctx); u == nil || u.ID != 7 {
		t.Fatal("userFrom")
	}
}

func TestHashPassword(t *testing.T) {
	e := newTestEnv(t)
	h, err := e.srv.hashPassword("correct horse battery")
	if err != nil || !strings.HasPrefix(h, "$2a$12$") {
		t.Fatalf("hashPassword: %q %v", h, err)
	}
}
