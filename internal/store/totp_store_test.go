package store

import (
	"context"
	"testing"
	"time"
)

func TestTOTPStoreLifecycle(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "totp@example.com", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Fresh user: 2FA off, no secret, no recovery codes.
	secret, enabled, remaining, err := s.TOTPStatus(ctx, u.ID)
	if err != nil || secret != "" || enabled || remaining != 0 {
		t.Fatalf("initial status: %q %v %d %v", secret, enabled, remaining, err)
	}
	if got, err := s.GetUser(ctx, u.ID); err != nil || got.TOTPEnabled {
		t.Fatalf("GetUser totp_enabled should be false: %+v %v", got, err)
	}

	// Store a pending secret.
	if err := s.SetTOTPSecret(ctx, u.ID, "SECRETBASE32"); err != nil {
		t.Fatal(err)
	}
	secret, enabled, _, _ = s.TOTPStatus(ctx, u.ID)
	if secret != "SECRETBASE32" || enabled {
		t.Fatalf("after set secret: %q enabled=%v", secret, enabled)
	}

	// Enable and store recovery codes.
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	if err := s.EnableTOTP(ctx, u.ID, now); err != nil {
		t.Fatal(err)
	}
	hashes := []string{HashToken("a"), HashToken("b"), HashToken("c")}
	if err := s.ReplaceRecoveryCodes(ctx, u.ID, hashes); err != nil {
		t.Fatal(err)
	}
	_, enabled, remaining, _ = s.TOTPStatus(ctx, u.ID)
	if !enabled || remaining != 3 {
		t.Fatalf("after enable: enabled=%v remaining=%d", enabled, remaining)
	}
	if got, _ := s.GetUser(ctx, u.ID); !got.TOTPEnabled {
		t.Fatal("GetUser totp_enabled should be true")
	}

	// Consume one recovery code; a second consume of the same hash fails
	// (single-use), and the count drops by one.
	ok, err := s.ConsumeRecoveryCode(ctx, u.ID, HashToken("a"))
	if err != nil || !ok {
		t.Fatalf("consume a: %v %v", ok, err)
	}
	ok, _ = s.ConsumeRecoveryCode(ctx, u.ID, HashToken("a"))
	if ok {
		t.Fatal("recovery code reused")
	}
	ok, _ = s.ConsumeRecoveryCode(ctx, u.ID, HashToken("nope"))
	if ok {
		t.Fatal("unknown recovery code consumed")
	}
	_, _, remaining, _ = s.TOTPStatus(ctx, u.ID)
	if remaining != 2 {
		t.Fatalf("remaining after one consume: %d", remaining)
	}

	// ReplaceRecoveryCodes resets the set.
	if err := s.ReplaceRecoveryCodes(ctx, u.ID, []string{HashToken("x")}); err != nil {
		t.Fatal(err)
	}
	_, _, remaining, _ = s.TOTPStatus(ctx, u.ID)
	if remaining != 1 {
		t.Fatalf("remaining after replace: %d", remaining)
	}

	// Disable clears everything.
	if err := s.DisableTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	secret, enabled, remaining, _ = s.TOTPStatus(ctx, u.ID)
	if secret != "" || enabled || remaining != 0 {
		t.Fatalf("after disable: %q %v %d", secret, enabled, remaining)
	}
	// Disable is idempotent on an already-off user.
	if err := s.DisableTOTP(ctx, u.ID); err != nil {
		t.Fatalf("second disable: %v", err)
	}

	// Unknown user is ErrNotFound for the status read and the mutators.
	if _, _, _, err := s.TOTPStatus(ctx, 999999); err != ErrNotFound {
		t.Fatalf("status missing user: %v", err)
	}
	if err := s.SetTOTPSecret(ctx, 999999, "x"); err != ErrNotFound {
		t.Fatalf("set secret missing user: %v", err)
	}
	if err := s.EnableTOTP(ctx, 999999, now); err != ErrNotFound {
		t.Fatalf("enable missing user: %v", err)
	}
	if err := s.DisableTOTP(ctx, 999999); err != ErrNotFound {
		t.Fatalf("disable missing user: %v", err)
	}
}
