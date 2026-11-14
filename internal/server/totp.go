package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238) second factor, implemented with the Go standard library only
// (HMAC-SHA1, base32): SHA-1, 6 digits, a 30 s step, accepting the current step
// and ±1 for clock skew, compared in constant time. The TOTP secret is 20
// random bytes, base32-encoded.

const (
	totpDigits      = 6
	totpPeriod      = 30 * time.Second
	totpSecretBytes = 20
	totpIssuer      = "MSP"
)

// b32 is the RFC 4648 base32 alphabet without padding, which authenticator apps
// expect for the otpauth secret.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret returns a fresh secret: 20 random bytes, base32-encoded.
func newTOTPSecret() string {
	var b [totpSecretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the OS entropy source is broken; there is no
		// safe fallback for a credential.
		panic("server: crypto/rand: " + err.Error())
	}
	return b32.EncodeToString(b[:])
}

// decodeTOTPSecret decodes a base32 secret, tolerating lower case and spaces so
// a hand-typed secret still works.
func decodeTOTPSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.TrimSpace(secret))
	s = strings.ReplaceAll(s, " ", "")
	return b32.DecodeString(s)
}

// totpCounter is the RFC 6238 time step T for an instant.
func totpCounter(t time.Time) uint64 {
	return uint64(t.UTC().Unix()) / uint64(totpPeriod/time.Second)
}

// hotp computes the RFC 4226 HOTP value for key and counter as a zero-padded
// decimal string of totpDigits digits.
func hotp(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	var mod uint32 = 1
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, bin%mod)
}

// totpNow returns the TOTP code for secret at instant t; "" if the secret does
// not decode.
func totpNow(secret string, t time.Time) string {
	key, err := decodeTOTPSecret(secret)
	if err != nil || len(key) == 0 {
		return ""
	}
	return hotp(key, totpCounter(t))
}

// totpVerifyStep reports whether code is valid for secret at instant t, accepting
// the current 30 s step and the one before and after it (clock skew), and when
// valid returns the time step it matched. The matched step lets a caller enforce
// one-time use (RFC 6238 Section 5.2) by refusing a step that was already
// consumed. The comparison is constant time and the loop does not short-circuit,
// so a valid code is not distinguishable by timing from an invalid one of the
// same length; the matched step is selected with a constant-time mask.
func totpVerifyStep(secret, code string, t time.Time) (step uint64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	key, err := decodeTOTPSecret(secret)
	if err != nil || len(key) == 0 {
		return 0, false
	}
	counter := totpCounter(t)
	matched := 0
	var matchedStep uint64
	for _, off := range []int64{-1, 0, 1} {
		if off < 0 && counter == 0 {
			continue
		}
		cand := uint64(int64(counter) + off)
		expected := hotp(key, cand)
		eq := subtle.ConstantTimeCompare([]byte(expected), []byte(code))
		matched |= eq
		// Constant-time select: fold cand in when eq==1, keep matchedStep when
		// eq==0, without branching on the comparison result.
		mask := -uint64(eq) // 0 or all-ones
		matchedStep = (matchedStep &^ mask) | (cand & mask)
	}
	return matchedStep, matched == 1
}

// totpVerify reports whether code is valid for secret at instant t, accepting
// the current 30 s step and the one before and after it (clock skew). It is the
// stateless check used where one-time enforcement is not needed (enrolment
// confirm, disable, recovery regeneration); the login second factor uses
// totpVerifyStep plus the store's per-user step counter instead.
func totpVerify(secret, code string, t time.Time) bool {
	_, ok := totpVerifyStep(secret, code, t)
	return ok
}

// otpauthURI builds the otpauth:// URI an authenticator app enrols from. The
// query is written in the order the spec documents (secret, issuer, digits,
// period); the issuer is prefixed to the label so apps group entries under MSP.
func otpauthURI(secret, issuer, account string) string {
	label := issuer + ":" + account
	return "otpauth://totp/" + url.PathEscape(label) +
		"?secret=" + url.QueryEscape(secret) +
		"&issuer=" + url.QueryEscape(issuer) +
		"&digits=6&period=30"
}
