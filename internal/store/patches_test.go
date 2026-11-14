package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

func TestRecordPatches(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, _, dev, _ := fixture(t, s)

	// A fresh device has an empty report object and zeroed denormalised columns.
	if dev.PatchesJSON != "{}" || dev.PendingPatches != 0 || dev.SecurityPatches != 0 ||
		dev.RebootRequired || dev.PatchesCheckedAt != nil {
		t.Fatalf("fresh device patch fields: %+v", dev)
	}

	checked := time.Date(2026, 11, 13, 10, 0, 1, 0, time.UTC)
	report := protocol.PatchReport{
		CheckedAt:      checked,
		Manager:        protocol.PatchManagerApt,
		RebootRequired: true,
		Count:          3,
		SecurityCount:  1,
		Updates: []protocol.PatchUpdate{
			{Name: "openssl", Current: "3.0.1", New: "3.0.2", Security: true, Source: "noble-security"},
			{Name: "curl", Current: "8.5.0", New: "8.5.1", Source: "noble-updates"},
			{Name: "vim", Current: "9.1.0016", New: "9.1.0101", Source: "noble-updates"},
		},
	}
	if err := s.RecordPatches(ctx, dev.ID, report); err != nil {
		t.Fatalf("RecordPatches: %v", err)
	}

	d, err := s.GetDevice(ctx, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.PendingPatches != 3 || d.SecurityPatches != 1 || !d.RebootRequired {
		t.Fatalf("derived counts: pending %d security %d reboot %v", d.PendingPatches, d.SecurityPatches, d.RebootRequired)
	}
	if d.PatchesCheckedAt == nil || !d.PatchesCheckedAt.Equal(checked) {
		t.Fatalf("patches_checked_at = %v, want %v", d.PatchesCheckedAt, checked)
	}
	// The full report round-trips through PatchesJSON, which never reaches DEVICE JSON.
	var got protocol.PatchReport
	if err := json.Unmarshal([]byte(d.PatchesJSON), &got); err != nil {
		t.Fatalf("patches_json is not a PatchReport: %v (%s)", err, d.PatchesJSON)
	}
	if got.Manager != protocol.PatchManagerApt || len(got.Updates) != 3 || got.Updates[0].Name != "openssl" || !got.Updates[0].Security {
		t.Fatalf("stored report: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("stored report must validate: %v", err)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "patches_json") || strings.Contains(string(b), `"patches"`) {
		t.Fatalf("DEVICE JSON must not carry the full report or raw column: %s", b)
	}

	// The device list carries the denormalised counts for the fleet column.
	list, err := s.ListDevices(ctx, DeviceFilter{OrgID: &org.ID})
	if err != nil || len(list) != 1 {
		t.Fatalf("ListDevices = %+v, %v", list, err)
	}
	if list[0].PendingPatches != 3 || list[0].SecurityPatches != 1 || !list[0].RebootRequired {
		t.Fatalf("list patch fields: %+v", list[0])
	}

	// A later report replaces all five columns (counts shrink, reboot clears).
	report2 := protocol.PatchReport{
		CheckedAt: checked.Add(6 * time.Hour),
		Manager:   protocol.PatchManagerApt,
		Count:     1,
		Updates: []protocol.PatchUpdate{
			{Name: "curl", Current: "8.5.0", New: "8.5.1", Source: "noble-updates"},
		},
	}
	if err := s.RecordPatches(ctx, dev.ID, report2); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, dev.ID)
	if d.PendingPatches != 1 || d.SecurityPatches != 0 || d.RebootRequired {
		t.Fatalf("after second report: pending %d security %d reboot %v", d.PendingPatches, d.SecurityPatches, d.RebootRequired)
	}

	// manager "none" with no updates and no timestamp: empty list, NULL check.
	if err := s.RecordPatches(ctx, dev.ID, protocol.PatchReport{Manager: protocol.PatchManagerNone}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, dev.ID)
	if d.PendingPatches != 0 || d.SecurityPatches != 0 || d.PatchesCheckedAt != nil {
		t.Fatalf("none report: %+v", d)
	}
	var none protocol.PatchReport
	if err := json.Unmarshal([]byte(d.PatchesJSON), &none); err != nil || none.Updates == nil || len(none.Updates) != 0 {
		t.Fatalf("none report updates must be a non-nil empty list: %+v err %v", none, err)
	}

	// Missing device.
	if err := s.RecordPatches(ctx, 9999, report); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RecordPatches missing device err = %v", err)
	}
}

func TestRecordPatchesCountFallback(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	_, _, dev, _ := fixture(t, s)

	// The agent capped its list but reported larger totals: the denormalised
	// counts must not understate the host, so the agent totals win.
	report := protocol.PatchReport{
		Manager:       protocol.PatchManagerApt,
		Count:         620,
		SecurityCount: 44,
		Updates: []protocol.PatchUpdate{
			{Name: "openssl", New: "3.0.2", Security: true},
			{Name: "curl", New: "8.5.1"},
		},
	}
	if err := s.RecordPatches(ctx, dev.ID, report); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, dev.ID)
	if d.PendingPatches != 620 || d.SecurityPatches != 44 {
		t.Fatalf("capped-list counts: pending %d security %d", d.PendingPatches, d.SecurityPatches)
	}
}
