package server

import (
	"testing"
	"time"
)

// rfcSecret is the RFC 6238 Appendix B test secret ("12345678901234567890"),
// base32-encoded as authenticator apps hold it.
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// TestTOTPRFC6238Vectors checks totpNow against the RFC 6238 Appendix B SHA-1
// vectors, taking the low 6 digits of each 8-digit published value.
func TestTOTPRFC6238Vectors(t *testing.T) {
	cases := []struct {
		unix int64
		want string // low 6 digits of the RFC's 8-digit SHA-1 TOTP
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, c := range cases {
		at := time.Unix(c.unix, 0).UTC()
		got := totpNow(rfcSecret, at)
		if got != c.want {
			t.Errorf("totpNow(@%d) = %q, want %q", c.unix, got, c.want)
		}
		if !totpVerify(rfcSecret, got, at) {
			t.Errorf("totpVerify(@%d) rejected its own code %q", c.unix, got)
		}
	}
}

// TestTOTPRoundTripAndSkew verifies a generated code, the ±1 step skew window,
// and rejection of a stale code (two steps away) and a wrong code.
func TestTOTPRoundTripAndSkew(t *testing.T) {
	secret := newTOTPSecret()
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	code := totpNow(secret, base)

	// Current step verifies.
	if !totpVerify(secret, code, base) {
		t.Fatal("current-step code rejected")
	}
	// One step earlier and later still verify (clock skew tolerance).
	if !totpVerify(secret, code, base.Add(-totpPeriod)) {
		t.Fatal("code rejected one step late (verifier clock -30s)")
	}
	if !totpVerify(secret, code, base.Add(totpPeriod)) {
		t.Fatal("code rejected one step early (verifier clock +30s)")
	}
	// Two steps away is stale and must be rejected.
	if totpVerify(secret, code, base.Add(2*totpPeriod)) {
		t.Fatal("stale code (2 steps later) accepted")
	}
	if totpVerify(secret, code, base.Add(-2*totpPeriod)) {
		t.Fatal("stale code (2 steps earlier) accepted")
	}
	// A wrong code of the right length is rejected.
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if totpVerify(secret, wrong, base) {
		t.Fatal("wrong code accepted")
	}
	// A malformed code (wrong length / non-base32 secret) is rejected, not a panic.
	if totpVerify(secret, "12345", base) || totpVerify(secret, "1234567", base) {
		t.Fatal("short/long code accepted")
	}
	if totpVerify("not base 32 !!!", code, base) {
		t.Fatal("undecodable secret accepted a code")
	}
}

// TestTOTPVerifyStep checks totpVerifyStep returns the exact 30 s step a code
// matched (the value the one-time login counter is keyed on), across the ±1 skew
// window, and reports !ok for a wrong code without pinning a step.
func TestTOTPVerifyStep(t *testing.T) {
	secret := newTOTPSecret()
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	code := totpNow(secret, base)
	want := totpCounter(base)

	// At the matching instant the code maps to the current step (offset 0).
	if step, ok := totpVerifyStep(secret, code, base); !ok || step != want {
		t.Fatalf("current step: ok=%v step=%d want %d", ok, step, want)
	}
	// With the verifier's clock one step ahead, the same code matches via the -1
	// skew offset and still resolves to the step it was generated for.
	if step, ok := totpVerifyStep(secret, code, base.Add(totpPeriod)); !ok || step != want {
		t.Fatalf("skew +1 step: ok=%v step=%d want %d", ok, step, want)
	}
	// With the verifier's clock one step behind, it matches via the +1 offset.
	if step, ok := totpVerifyStep(secret, code, base.Add(-totpPeriod)); !ok || step != want {
		t.Fatalf("skew -1 step: ok=%v step=%d want %d", ok, step, want)
	}
	// A wrong code is not ok (and pins no step).
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if _, ok := totpVerifyStep(secret, wrong, base); ok {
		t.Fatal("wrong code reported a matched step")
	}
}

// TestNewTOTPSecret checks the secret decodes to 20 bytes and differs each call.
func TestNewTOTPSecret(t *testing.T) {
	a := newTOTPSecret()
	b := newTOTPSecret()
	if a == b {
		t.Fatal("two secrets collided")
	}
	key, err := decodeTOTPSecret(a)
	if err != nil || len(key) != totpSecretBytes {
		t.Fatalf("secret %q decodes to %d bytes, err %v", a, len(key), err)
	}
}

// TestOTPAuthURI checks the otpauth URI shape documented by the spec.
func TestOTPAuthURI(t *testing.T) {
	uri := otpauthURI("ABC234", "MSP", "admin@example.com")
	want := "otpauth://totp/MSP:admin@example.com?secret=ABC234&issuer=MSP&digits=6&period=30"
	if uri != want {
		t.Fatalf("otpauthURI = %q, want %q", uri, want)
	}
}
