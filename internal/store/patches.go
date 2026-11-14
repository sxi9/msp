package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sxi9/msp/internal/protocol"
)

// RecordPatches stores a device's patch report (PM1). It writes the full report
// as JSON into patches_json and refreshes the four denormalised columns the
// fleet and device pages read: patches_checked_at, pending_patches,
// security_patches and reboot_required. The counts are derived from the stored
// update list (pending = len(updates), security = the entries flagged
// security), which is the list the dashboard shows; if the agent's own totals
// are larger (its list was capped at protocol.MaxPatchUpdates) they win, so the
// fleet count never understates a host with more than the cap. patches_checked_at
// is NULL when the report carries no timestamp. ErrNotFound when the device is
// gone.
func (s *Store) RecordPatches(ctx context.Context, deviceID int64, report protocol.PatchReport) error {
	if report.Updates == nil {
		report.Updates = []protocol.PatchUpdate{}
	}
	b, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("store: record patches: %w", err)
	}
	pending := len(report.Updates)
	security := 0
	for _, u := range report.Updates {
		if u.Security {
			security++
		}
	}
	if report.Count > pending {
		pending = report.Count
	}
	if report.SecurityCount > security {
		security = report.SecurityCount
	}
	var checkedAt any
	if !report.CheckedAt.IsZero() {
		checkedAt = fmtTime(report.CheckedAt)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET patches_json = ?, patches_checked_at = ?, pending_patches = ?,
		        security_patches = ?, reboot_required = ?
		  WHERE id = ?`,
		string(b), checkedAt, pending, security, boolInt(report.RebootRequired), deviceID)
	if err != nil {
		return fmt.Errorf("store: record patches: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: record patches: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
