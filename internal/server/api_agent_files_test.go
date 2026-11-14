package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
	"github.com/sxi9/msp/internal/store"
)

// agentFilePath is GET /api/agent/v1/files/{id}.
func agentFilePath(id int64) string { return "/api/agent/v1/files/" + itoa(id) }

// TestAgentFileDownload proves the device-token download endpoint (SD1): a
// device streams a global file and a file in its own org (200 with the stored
// hash in X-MSP-SHA256), but a file in another org is 404 — indistinguishable
// from a missing one — for both a staff-uploaded org file and across device
// tokens. Auth is the device token, never a session.
func TestAgentFileDownload(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.useFilesDir()
	admin := dashAdmin(e)

	// A device in the env's own org A, plus an org B with its own device (its
	// plaintext token comes straight from UpsertDevice).
	devA, tokenA := e.enroll("uid-a", protocol.PlatformLinux)
	_ = devA
	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	_, tokenB, err := e.st.UpsertDevice(ctx, store.UpsertDeviceParams{
		OrgID: orgB.ID, DeviceUID: "uid-b", Hostname: "beta-ws", Platform: protocol.PlatformLinux,
		Arch: "amd64", IntervalS: 30, FactsJSON: "{}",
	})
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}

	gContent := []byte("global artifact\n")
	gSum := sha256.Sum256(gContent)
	gHex := hex.EncodeToString(gSum[:])
	_, body := e.uploadFile(withCookie(admin, csrfHeader), "global.sh", nil, "global.sh", gContent)
	gf := fileFrom(t, body)

	aContent := []byte("org A artifact\n")
	_, body = e.uploadFile(withCookie(admin, csrfHeader), "a.sh", &e.org.ID, "a.sh", aContent)
	af := fileFrom(t, body)

	bContent := []byte("org B artifact\n")
	_, body = e.uploadFile(withCookie(admin, csrfHeader), "b.sh", &orgB.ID, "b.sh", bContent)
	bf := fileFrom(t, body)

	// --- device A: global and its own org → 200 with bytes + X-MSP-SHA256 ------
	resp, out := e.do(http.MethodGet, agentFilePath(gf.ID), nil, bearer(tokenA))
	if resp.StatusCode != 200 {
		t.Fatalf("device A global: %d", resp.StatusCode)
	}
	if string(out) != string(gContent) {
		t.Fatalf("device A global bytes = %q, want %q", out, gContent)
	}
	if h := resp.Header.Get("X-MSP-SHA256"); h != gHex {
		t.Fatalf("X-MSP-SHA256 = %q, want %q", h, gHex)
	}
	if cl := resp.Header.Get("Content-Length"); cl != itoa(int64(len(gContent))) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(gContent))
	}
	if resp, out := e.do(http.MethodGet, agentFilePath(af.ID), nil, bearer(tokenA)); resp.StatusCode != 200 || string(out) != string(aContent) {
		t.Fatalf("device A own-org file: %d %q", resp.StatusCode, out)
	}

	// --- device A: a file in org B → 404 (cannot probe another client) --------
	if resp, out := e.do(http.MethodGet, agentFilePath(bf.ID), nil, bearer(tokenA)); resp.StatusCode != 404 || errorCode(t, out) != "not_found" {
		t.Fatalf("device A on org B file: %d %s", resp.StatusCode, out)
	}
	// Unknown id is the same 404.
	if resp, _ := e.do(http.MethodGet, agentFilePath(999999), nil, bearer(tokenA)); resp.StatusCode != 404 {
		t.Fatalf("device A unknown file: %d", resp.StatusCode)
	}

	// --- device B: its own org file and global → 200; org A's file → 404 ------
	if resp, out := e.do(http.MethodGet, agentFilePath(bf.ID), nil, bearer(tokenB)); resp.StatusCode != 200 || string(out) != string(bContent) {
		t.Fatalf("device B own-org file: %d %q", resp.StatusCode, out)
	}
	if resp, _ := e.do(http.MethodGet, agentFilePath(gf.ID), nil, bearer(tokenB)); resp.StatusCode != 200 {
		t.Fatalf("device B global: %d", resp.StatusCode)
	}
	if resp, out := e.do(http.MethodGet, agentFilePath(af.ID), nil, bearer(tokenB)); resp.StatusCode != 404 || errorCode(t, out) != "not_found" {
		t.Fatalf("device B on org A file: %d %s", resp.StatusCode, out)
	}

	// --- auth: no bearer / bad token → 401 invalid_device_token, never a session
	if resp, out := e.do(http.MethodGet, agentFilePath(gf.ID), nil, nil); resp.StatusCode != 401 || errorCode(t, out) != "invalid_device_token" {
		t.Fatalf("no bearer: %d %s", resp.StatusCode, out)
	}
	if resp, _ := e.do(http.MethodGet, agentFilePath(gf.ID), nil, bearer("dt_bogus")); resp.StatusCode != 401 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
}
