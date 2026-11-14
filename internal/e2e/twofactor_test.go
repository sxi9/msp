package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
)

// Two-factor authentication (TF1) end to end, driven through the real server
// handler over httptest with a real SQLite store. Everything is deterministic:
// TOTP codes are computed here (tfTOTP, an independent RFC 6238 implementation
// cross-checked against the published vectors) and fed to the server, so no
// authenticator app or external clock is needed. These tests prove the full TF1
// Proof list: enrolment, the two-step login, recovery codes, disable, the admin
// reset, rate limiting and that a non-2FA user logs in exactly as before.

const (
	tfSessionCookie = "msp_session"
	tfPendingCookie = "msp_pending_totp"
)

// tfB32 matches the server's base32 (RFC 4648, no padding). newTOTPSecret and
// decodeTOTPSecret use the same encoding, so a secret the server hands back here
// decodes with this.
var tfB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// tfTOTP is an independent RFC 6238 (HMAC-SHA1, 6 digits, 30 s step) code
// generator. TestTwoFactorRFC6238Vectors pins it to the RFC's published vectors,
// so every code it feeds the server in these tests is a genuine RFC-6238 code —
// the server accepting them is the end-to-end proof its primitive matches.
func tfTOTP(secret string, t time.Time) string {
	key, err := tfB32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		panic("tfTOTP: decode secret: " + err.Error())
	}
	counter := uint64(t.UTC().Unix()) / 30
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off]&0x7f) << 24) |
		(uint32(sum[off+1]) << 16) |
		(uint32(sum[off+2]) << 8) |
		uint32(sum[off+3])
	return fmt.Sprintf("%06d", bin%1000000)
}

// tfEnv is a 2FA fixture: a real store, a real server behind httptest and a
// bootstrap admin. Each test builds its own so the per-IP login rate limiter
// (10/min) starts with a full bucket.
type tfEnv struct {
	t     *testing.T
	st    *store.Store
	srv   *server.Server
	ts    *httptest.Server
	hc    *http.Client
	admin store.User
}

func newTFEnv(t *testing.T) *tfEnv {
	t.Helper()
	logs := &logBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})

	st, err := store.Open(filepath.Join(t.TempDir(), "msp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv := server.New(st, server.Config{
		SampleEvery: time.Second,
		LostGrace:   time.Second,
		Version:     agentVersion,
	}, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	hash, err := server.HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	admin, err := st.CreateUser(context.Background(), adminEmail, hash, "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// No cookie jar: every request carries exactly the cookies the test passes,
	// so the pending-cookie replay and "pending is not a session" cases are
	// unambiguous.
	return &tfEnv{t: t, st: st, srv: srv, ts: ts, hc: &http.Client{Timeout: 30 * time.Second}, admin: admin}
}

// tfResp is a single HTTP exchange: status, raw body and the Set-Cookie list.
type tfResp struct {
	status  int
	body    []byte
	cookies []*http.Cookie
}

// call sends method path with an optional JSON body and the given cookies,
// setting the CSRF header on writes (the dashboard/login requirement). It never
// follows redirects into a cookie jar: cookies are explicit.
func (e *tfEnv) call(method, path string, body any, cookies ...*http.Cookie) tfResp {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "msp")
	}
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return tfResp{status: resp.StatusCode, body: data, cookies: resp.Cookies()}
}

// cookie returns the named Set-Cookie with a non-empty value, or nil (so a
// cleared cookie, MaxAge<0 with an empty value, reads as absent).
func cookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name && c.Value != "" {
			return c
		}
	}
	return nil
}

// login runs step one of a login and returns the exchange.
func (e *tfEnv) login(email, password string) tfResp {
	e.t.Helper()
	return e.call(http.MethodPost, "/api/v1/login", map[string]string{"email": email, "password": password})
}

// enroll runs the real setup -> confirm flow for the session and returns the
// secret and the one-time recovery codes.
func (e *tfEnv) enroll(session *http.Cookie) (secret string, recovery []string) {
	e.t.Helper()
	r := e.call(http.MethodPost, "/api/v1/me/totp/setup", struct{}{}, session)
	if r.status != 200 {
		e.t.Fatalf("setup: %d %s", r.status, r.body)
	}
	var setup struct {
		Secret     string `json:"secret"`
		OTPAuthURI string `json:"otpauth_uri"`
	}
	if err := json.Unmarshal(r.body, &setup); err != nil || setup.Secret == "" {
		e.t.Fatalf("setup body %s (%v)", r.body, err)
	}
	if !strings.HasPrefix(setup.OTPAuthURI, "otpauth://totp/MSP:") || !strings.Contains(setup.OTPAuthURI, "secret="+setup.Secret) {
		e.t.Fatalf("otpauth_uri %q", setup.OTPAuthURI)
	}
	r = e.call(http.MethodPost, "/api/v1/me/totp/confirm", map[string]string{"code": tfTOTP(setup.Secret, time.Now())}, session)
	if r.status != 200 {
		e.t.Fatalf("confirm: %d %s", r.status, r.body)
	}
	var conf struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(r.body, &conf); err != nil {
		e.t.Fatalf("confirm body %s (%v)", r.body, err)
	}
	return setup.Secret, conf.RecoveryCodes
}

// lastAudit returns the most recent audit row for action (ListAudit is DESC), or
// nil if none.
func (e *tfEnv) lastAudit(action string) *store.AuditEntry {
	e.t.Helper()
	entries, err := e.st.ListAudit(context.Background(), 200, nil, nil, 0)
	if err != nil {
		e.t.Fatalf("list audit: %v", err)
	}
	for i := range entries {
		if entries[i].Action == action {
			return &entries[i]
		}
	}
	return nil
}

// TestTwoFactorNonTwoFactorLoginUnchanged proves a user without 2FA logs in in a
// single step exactly as before TF1: a real session cookie, no pending cookie,
// no totp_required, and the session is immediately usable.
func TestTwoFactorNonTwoFactorLoginUnchanged(t *testing.T) {
	e := newTFEnv(t)
	r := e.login(adminEmail, adminPassword)
	if r.status != 200 {
		t.Fatalf("login: %d %s", r.status, r.body)
	}
	if bytes.Contains(r.body, []byte("totp_required")) {
		t.Fatalf("totp_required for a non-2FA user: %s", r.body)
	}
	if p := cookie(r.cookies, tfPendingCookie); p != nil {
		t.Fatalf("pending cookie set for a non-2FA user")
	}
	session := cookie(r.cookies, tfSessionCookie)
	if session == nil {
		t.Fatalf("no session cookie: %v", r.cookies)
	}
	me := e.call(http.MethodGet, "/api/v1/me", nil, session)
	if me.status != 200 || !bytes.Contains(me.body, []byte(`"totp_enabled":false`)) || !bytes.Contains(me.body, []byte(`"recovery_codes_remaining":0`)) {
		t.Fatalf("me: %d %s", me.status, me.body)
	}
}

// TestTwoFactorSetupConfirmAndTwoStepLogin proves: setup -> confirm (computed
// code) -> ten recovery codes once; me reflects enabled/remaining and never the
// secret; a fresh login returns totp_required with a pending cookie and no
// usable session until /login/totp; wrong and stale codes are 401; a correct
// code completes the login and the pending token is single-use.
func TestTwoFactorSetupConfirmAndTwoStepLogin(t *testing.T) {
	e := newTFEnv(t)
	r := e.login(adminEmail, adminPassword)
	session := cookie(r.cookies, tfSessionCookie)
	if session == nil {
		t.Fatalf("bootstrap login: %d %s", r.status, r.body)
	}

	secret, recovery := e.enroll(session)
	if len(recovery) != 10 {
		t.Fatalf("recovery codes = %d, want 10", len(recovery))
	}
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
	if e.lastAudit("user.totp_enabled") == nil {
		t.Fatal("no user.totp_enabled audit")
	}

	// Re-running setup now that 2FA is on is a conflict.
	if rr := e.call(http.MethodPost, "/api/v1/me/totp/setup", struct{}{}, session); rr.status != 409 {
		t.Fatalf("setup when enabled: %d", rr.status)
	}

	// me reports enabled, the full recovery set, and never the secret.
	me := e.call(http.MethodGet, "/api/v1/me", nil, session)
	if !bytes.Contains(me.body, []byte(`"totp_enabled":true`)) || !bytes.Contains(me.body, []byte(`"recovery_codes_remaining":10`)) {
		t.Fatalf("me after enrol: %s", me.body)
	}
	if bytes.Contains(me.body, []byte(secret)) || bytes.Contains(me.body, []byte("totp_secret")) {
		t.Fatalf("me leaks the secret: %s", me.body)
	}

	// Step one of a fresh login: totp_required, a pending cookie, no session.
	r = e.login(adminEmail, adminPassword)
	if r.status != 200 || !bytes.Contains(r.body, []byte(`"totp_required":true`)) {
		t.Fatalf("login step1: %d %s", r.status, r.body)
	}
	if s := cookie(r.cookies, tfSessionCookie); s != nil {
		t.Fatal("login step1 issued a full session cookie")
	}
	pending := cookie(r.cookies, tfPendingCookie)
	if pending == nil {
		t.Fatalf("no pending cookie: %v", r.cookies)
	}
	// The pending cookie is not a session.
	if m := e.call(http.MethodGet, "/api/v1/me", nil, pending); m.status != 401 {
		t.Fatalf("pending cookie usable as a session: %d", m.status)
	}

	// Wrong second factor: 401 invalid_code, no session.
	wrong := e.call(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": "000000"}, pending)
	if wrong.status != 401 || !bytes.Contains(wrong.body, []byte("invalid_code")) {
		t.Fatalf("wrong code: %d %s", wrong.status, wrong.body)
	}
	if cookie(wrong.cookies, tfSessionCookie) != nil {
		t.Fatal("wrong code issued a session")
	}
	if e.lastAudit("user.login_totp_failed") == nil {
		t.Fatal("no user.login_totp_failed audit after a wrong code")
	}

	// A stale code (three steps old) is outside the ±1 window.
	stale := e.call(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": tfTOTP(secret, time.Now().Add(-90*time.Second))}, pending)
	if stale.status != 401 {
		t.Fatalf("stale code: %d %s", stale.status, stale.body)
	}

	// The correct computed code completes the login.
	good := e.call(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": tfTOTP(secret, time.Now())}, pending)
	if good.status != 200 {
		t.Fatalf("correct code: %d %s", good.status, good.body)
	}
	newSession := cookie(good.cookies, tfSessionCookie)
	if newSession == nil {
		t.Fatalf("no session after /login/totp: %v", good.cookies)
	}
	if m := e.call(http.MethodGet, "/api/v1/me", nil, newSession); m.status != 200 {
		t.Fatalf("session from /login/totp not usable: %d", m.status)
	}
	if a := e.lastAudit("user.login"); a == nil || a.Detail["second_factor"] != "totp" {
		t.Fatalf("login audit second_factor: %+v", a)
	}

	// The pending token is single-use: replaying the same cookie fails even with
	// a freshly computed code.
	replay := e.call(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": tfTOTP(secret, time.Now())}, pending)
	if replay.status != 401 {
		t.Fatalf("replayed pending token: %d %s", replay.status, replay.body)
	}
}

// TestTwoFactorRecoveryCodeLogin proves a recovery code logs in once, cannot be
// reused, and the remaining count decrements.
func TestTwoFactorRecoveryCodeLogin(t *testing.T) {
	e := newTFEnv(t)
	r := e.login(adminEmail, adminPassword)
	session := cookie(r.cookies, tfSessionCookie)
	_, recovery := e.enroll(session)

	// Log in once with the first recovery code.
	r = e.login(adminEmail, adminPassword)
	pending := cookie(r.cookies, tfPendingCookie)
	ok := e.call(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": recovery[0]}, pending)
	if ok.status != 200 {
		t.Fatalf("recovery login: %d %s", ok.status, ok.body)
	}
	if a := e.lastAudit("user.login"); a == nil || a.Detail["second_factor"] != "recovery_code" {
		t.Fatalf("recovery login audit: %+v", a)
	}

	// The same code cannot be reused (fresh pending token).
	r = e.login(adminEmail, adminPassword)
	pending2 := cookie(r.cookies, tfPendingCookie)
	reuse := e.call(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": recovery[0]}, pending2)
	if reuse.status != 401 {
		t.Fatalf("reused recovery code: %d %s", reuse.status, reuse.body)
	}

	// A different recovery code still works; remaining drops to 8 (2 of 10 used).
	r = e.login(adminEmail, adminPassword)
	pending3 := cookie(r.cookies, tfPendingCookie)
	second := e.call(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": recovery[1]}, pending3)
	if second.status != 200 {
		t.Fatalf("second recovery login: %d %s", second.status, second.body)
	}
	newSession := cookie(second.cookies, tfSessionCookie)
	me := e.call(http.MethodGet, "/api/v1/me", nil, newSession)
	if !bytes.Contains(me.body, []byte(`"recovery_codes_remaining":8`)) {
		t.Fatalf("remaining after two used: %s", me.body)
	}
}

// TestTwoFactorDisable proves a valid code turns 2FA off (clearing it) and a
// subsequent login is single-step again; a wrong code cannot disable.
func TestTwoFactorDisable(t *testing.T) {
	e := newTFEnv(t)
	r := e.login(adminEmail, adminPassword)
	session := cookie(r.cookies, tfSessionCookie)
	secret, _ := e.enroll(session)

	if bad := e.call(http.MethodPost, "/api/v1/me/totp/disable", map[string]string{"code": "000000"}, session); bad.status != 400 || !bytes.Contains(bad.body, []byte("invalid_code")) {
		t.Fatalf("disable wrong: %d %s", bad.status, bad.body)
	}
	if ok := e.call(http.MethodPost, "/api/v1/me/totp/disable", map[string]string{"code": tfTOTP(secret, time.Now())}, session); ok.status != 204 {
		t.Fatalf("disable: %d %s", ok.status, ok.body)
	}
	if e.lastAudit("user.totp_disabled") == nil {
		t.Fatal("no user.totp_disabled audit")
	}

	// Login is single-step again.
	r = e.login(adminEmail, adminPassword)
	if r.status != 200 || bytes.Contains(r.body, []byte("totp_required")) {
		t.Fatalf("login after disable: %d %s", r.status, r.body)
	}
	if cookie(r.cookies, tfSessionCookie) == nil {
		t.Fatalf("no session after single-step login: %v", r.cookies)
	}
}

// TestTwoFactorAdminReset proves an admin can clear a locked-out user's 2FA
// (audited user.totp_reset with the actor and target), a missing user is 404,
// and the reset user then logs in single-step.
func TestTwoFactorAdminReset(t *testing.T) {
	e := newTFEnv(t)
	ctx := context.Background()

	// A second admin to lock out and reset, so the acting admin is not resetting
	// themselves.
	hash, err := server.HashPassword("other-admin-123")
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.st.CreateUser(ctx, "other@example.com", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	r := e.login("other@example.com", "other-admin-123")
	otherSession := cookie(r.cookies, tfSessionCookie)
	if otherSession == nil {
		t.Fatalf("other login: %d %s", r.status, r.body)
	}
	e.enroll(otherSession)

	// The acting admin resets the other user's 2FA.
	r = e.login(adminEmail, adminPassword)
	adminSession := cookie(r.cookies, tfSessionCookie)
	reset := e.call(http.MethodPost, fmt.Sprintf("/api/v1/users/%d/totp/reset", other.ID), struct{}{}, adminSession)
	if reset.status != 204 {
		t.Fatalf("reset: %d %s", reset.status, reset.body)
	}
	if a := e.lastAudit("user.totp_reset"); a == nil || a.ActorID == nil || *a.ActorID != e.admin.ID || a.TargetID == nil || *a.TargetID != other.ID {
		t.Fatalf("user.totp_reset audit: %+v", a)
	}
	// A missing user is 404.
	if miss := e.call(http.MethodPost, "/api/v1/users/999999/totp/reset", struct{}{}, adminSession); miss.status != 404 {
		t.Fatalf("reset missing user: %d", miss.status)
	}

	// The reset user now logs in in a single step; the old secret is gone.
	r = e.login("other@example.com", "other-admin-123")
	if r.status != 200 || bytes.Contains(r.body, []byte("totp_required")) {
		t.Fatalf("login after reset: %d %s", r.status, r.body)
	}
	if cookie(r.cookies, tfSessionCookie) == nil {
		t.Fatalf("no session after reset login: %v", r.cookies)
	}
}

// TestTwoFactorRateLimited proves the second login step is rate-limited: wrong
// codes return 401 invalid_code until the per-IP login budget (10/min) is spent,
// then 429 rate_limited. The pending token survives failures, so every attempt
// reaches the verifier.
func TestTwoFactorRateLimited(t *testing.T) {
	e := newTFEnv(t)
	r := e.login(adminEmail, adminPassword)
	session := cookie(r.cookies, tfSessionCookie)
	e.enroll(session)

	r = e.login(adminEmail, adminPassword)
	pending := cookie(r.cookies, tfPendingCookie)
	if pending == nil {
		t.Fatalf("no pending cookie: %v", r.cookies)
	}

	var saw401, saw429 bool
	for i := 0; i < 20; i++ {
		resp := e.call(http.MethodPost, "/api/v1/login/totp", map[string]string{"code": "000000"}, pending)
		switch resp.status {
		case 401:
			saw401 = true
			if !bytes.Contains(resp.body, []byte("invalid_code")) {
				t.Fatalf("401 but not invalid_code: %s", resp.body)
			}
		case 429:
			saw429 = true
			if !bytes.Contains(resp.body, []byte("rate_limited")) {
				t.Fatalf("429 but not rate_limited: %s", resp.body)
			}
		default:
			t.Fatalf("attempt %d: unexpected status %d: %s", i, resp.status, resp.body)
		}
	}
	if !saw401 {
		t.Fatal("never saw a 401 invalid_code before rate limiting")
	}
	if !saw429 {
		t.Fatal("wrong-code attempts were never rate limited")
	}
}

// TestTwoFactorRFC6238Vectors pins tfTOTP (the generator driving every code in
// these tests) to the RFC 6238 Appendix B SHA-1 vectors, taking the low six
// digits of each published eight-digit value. The server accepting tfTOTP codes
// elsewhere is therefore proof its own primitive matches RFC 6238.
func TestTwoFactorRFC6238Vectors(t *testing.T) {
	const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // base32 of "12345678901234567890"
	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, c := range cases {
		if got := tfTOTP(rfcSecret, time.Unix(c.unix, 0).UTC()); got != c.want {
			t.Errorf("tfTOTP(@%d) = %q, want %q", c.unix, got, c.want)
		}
	}
}
