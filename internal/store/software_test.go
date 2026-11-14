package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// TestRecordSoftwareRoundTrip proves RecordSoftware stores the full list and the
// two denormalised columns, that the DEVICE read exposes software_count and
// software_checked_at while keeping the list out of the DEVICE JSON (SoftwareJSON
// is json:"-"), and that a nil list round-trips as "[]".
func TestRecordSoftwareRoundTrip(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	_, _, dev, _ := fixture(t, s)

	items := []protocol.SoftwareItem{
		{Name: "curl", Version: "8.5.0", Publisher: "Ubuntu", Source: protocol.SoftwareSourceDpkg},
		{Name: "openssl", Version: "3.0.13", Publisher: "Ubuntu", Source: protocol.SoftwareSourceDpkg},
	}
	if err := s.RecordSoftware(ctx, dev.ID, items, c.now()); err != nil {
		t.Fatalf("RecordSoftware: %v", err)
	}

	got, err := s.GetDevice(ctx, dev.ID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if got.SoftwareCount != 2 {
		t.Fatalf("software_count = %d, want 2", got.SoftwareCount)
	}
	if got.SoftwareCheckedAt == nil || !got.SoftwareCheckedAt.Equal(c.now()) {
		t.Fatalf("software_checked_at = %v, want %v", got.SoftwareCheckedAt, c.now())
	}
	var stored []protocol.SoftwareItem
	if err := json.Unmarshal([]byte(got.SoftwareJSON), &stored); err != nil {
		t.Fatalf("software_json not JSON: %v (%q)", err, got.SoftwareJSON)
	}
	if len(stored) != 2 || stored[0].Name != "curl" || stored[1].Version != "3.0.13" {
		t.Fatalf("stored software: %+v", stored)
	}

	// The DEVICE JSON exposes the count and checked-at but not the full list.
	b, _ := json.Marshal(got)
	js := string(b)
	if !strings.Contains(js, `"software_count":2`) || !strings.Contains(js, `"software_checked_at":`) {
		t.Fatalf("DEVICE JSON missing software fields: %s", js)
	}
	if strings.Contains(js, "curl") || strings.Contains(js, "software_json") {
		t.Fatalf("DEVICE JSON must not carry the software list: %s", js)
	}

	// A nil list stores "[]" and a zero count.
	if err := s.RecordSoftware(ctx, dev.ID, nil, c.now()); err != nil {
		t.Fatalf("RecordSoftware nil: %v", err)
	}
	got, _ = s.GetDevice(ctx, dev.ID)
	if got.SoftwareCount != 0 || got.SoftwareJSON != "[]" {
		t.Fatalf("after nil: count %d json %q", got.SoftwareCount, got.SoftwareJSON)
	}

	// A device that never reported software reads as the migration default.
	_, _, dev2 := fixture2(t, s, "Beta Corp", "uid-2", "ws-02")
	got2, _ := s.GetDevice(ctx, dev2.ID)
	if got2.SoftwareCount != 0 || got2.SoftwareJSON != "[]" || got2.SoftwareCheckedAt != nil {
		t.Fatalf("default software: count %d json %q checked %v", got2.SoftwareCount, got2.SoftwareJSON, got2.SoftwareCheckedAt)
	}

	// A missing device is ErrNotFound.
	if err := s.RecordSoftware(ctx, 99999, items, c.now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RecordSoftware missing device: %v", err)
	}
}

// TestSoftwareSearch proves the fleet search matches names case-insensitively as
// a substring, carries device+org+version, orders by org/hostname, and is RS2
// org-scoped: a caller whose visible set excludes an org sees none of its
// software.
func TestSoftwareSearch(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	orgA, _, devA, _ := fixture(t, s) // "Acme Dental" / ws-01
	orgB, _, devB := fixture2(t, s, "Beta Corp", "uid-2", "ws-02")

	if err := s.RecordSoftware(ctx, devA.ID, []protocol.SoftwareItem{
		{Name: "OpenSSL", Version: "3.0.13", Source: protocol.SoftwareSourceDpkg},
		{Name: "curl", Version: "8.5.0", Source: protocol.SoftwareSourceDpkg},
	}, c.now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSoftware(ctx, devB.ID, []protocol.SoftwareItem{
		{Name: "openssl", Version: "1.1.1", Source: protocol.SoftwareSourceRPM},
		{Name: "vim", Version: "9.1", Source: protocol.SoftwareSourceRPM},
	}, c.now()); err != nil {
		t.Fatal(err)
	}

	// All orgs: "ssl" (case-insensitive) matches both devices, ordered by org
	// name (Acme before Beta).
	hits, trunc, err := s.SoftwareSearch(ctx, nil, "ssl", 0)
	if err != nil || trunc {
		t.Fatalf("search: %v trunc %v", err, trunc)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].OrgName != "Acme Dental" || hits[0].Hostname != "ws-01" || hits[0].Name != "OpenSSL" ||
		hits[0].Version != "3.0.13" || hits[0].OrgID != orgA.ID || hits[0].DeviceID != devA.ID {
		t.Fatalf("hit0 = %+v", hits[0])
	}
	if hits[1].OrgName != "Beta Corp" || hits[1].OrgID != orgB.ID || hits[1].Version != "1.1.1" {
		t.Fatalf("hit1 = %+v", hits[1])
	}

	// RS2: a caller who can see only org A gets only org A's row.
	hits, _, err = s.SoftwareSearch(ctx, []int64{orgA.ID}, "ssl", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].OrgID != orgA.ID {
		t.Fatalf("scoped to A: %+v", hits)
	}

	// RS2: a caller who can see only org B gets only org B's row — it must not
	// be able to tell org A's package exists.
	hits, _, err = s.SoftwareSearch(ctx, []int64{orgB.ID}, "openssl", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].OrgID != orgB.ID {
		t.Fatalf("scoped to B: %+v", hits)
	}

	// An empty visible set matches nothing.
	hits, _, err = s.SoftwareSearch(ctx, []int64{}, "ssl", 0)
	if err != nil || len(hits) != 0 {
		t.Fatalf("empty visible set: %+v err %v", hits, err)
	}

	// A blank query returns nothing (the handler also enforces >= 2 chars).
	hits, _, err = s.SoftwareSearch(ctx, nil, "   ", 0)
	if err != nil || len(hits) != 0 {
		t.Fatalf("blank query: %+v err %v", hits, err)
	}

	// A substring that matches no package name returns nothing.
	hits, _, err = s.SoftwareSearch(ctx, nil, "nothinghere", 0)
	if err != nil || len(hits) != 0 {
		t.Fatalf("no-match query: %+v err %v", hits, err)
	}
}

// TestSoftwareSearchTruncates proves the cap is honoured and reported.
func TestSoftwareSearchTruncates(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	_, _, dev, _ := fixture(t, s)

	items := make([]protocol.SoftwareItem, 5)
	for i := range items {
		items[i] = protocol.SoftwareItem{
			Name:    "lib-match",
			Version: "1",
			Source:  protocol.SoftwareSourceDpkg,
		}
	}
	if err := s.RecordSoftware(ctx, dev.ID, items, c.now()); err != nil {
		t.Fatal(err)
	}

	hits, trunc, err := s.SoftwareSearch(ctx, nil, "match", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || !trunc {
		t.Fatalf("want 3 hits truncated, got %d trunc %v", len(hits), trunc)
	}

	// Exactly at the cap with no extras is not truncated.
	hits, trunc, err = s.SoftwareSearch(ctx, nil, "match", 5)
	if err != nil || len(hits) != 5 || trunc {
		t.Fatalf("at cap: %d hits trunc %v err %v", len(hits), trunc, err)
	}
}

// fixture2 creates a second org + linux device with the given names, reusing the
// first fixture's admin user. It returns the org, that admin user and the device.
func fixture2(t *testing.T, s *Store, orgName, uid, hostname string) (Org, User, Device) {
	t.Helper()
	ctx := context.Background()
	org, err := s.CreateOrg(ctx, orgName)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	dev, _, err := s.UpsertDevice(ctx, UpsertDeviceParams{
		OrgID: org.ID, DeviceUID: uid, Hostname: hostname, Platform: "linux", Arch: "amd64",
		OSVersion: "Rocky 9", AgentVersion: "0.1.0", IntervalS: 30, FactsJSON: `{"kernel":"5.14"}`,
	})
	if err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	return org, User{}, dev
}
