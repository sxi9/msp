package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Installed-software inventory (SI1). The desktop agent reports its installed
// software in facts.software; RecordSoftware stores the full list as JSON and
// the two denormalised columns the fleet and device pages read. SoftwareSearch
// is the fleet-wide package hunt, org-scoped for RS2.

// SoftwareSearchLimit is the default cap on SoftwareSearch rows when the caller
// passes a non-positive limit; the search stops collecting at the limit and
// reports truncation so the UI can say "showing the first N".
const SoftwareSearchLimit = 500

// SoftwareHit is one match from SoftwareSearch: the device it is installed on
// (with its org, for the link and the RS2 label) and the matching package's
// name and version.
type SoftwareHit struct {
	DeviceID int64  `json:"device_id"`
	Hostname string `json:"hostname"`
	OrgID    int64  `json:"org_id"`
	OrgName  string `json:"org_name"`
	Name     string `json:"name"`
	Version  string `json:"version"`
}

// RecordSoftware stores a device's installed-software inventory (SI1). It writes
// the full list as JSON into software_json and refreshes the two denormalised
// columns the dashboard reads: software_count (len(items)) and
// software_checked_at (now). A nil list is stored as an empty array so the
// column round-trips as "[]". ErrNotFound when the device is gone.
func (s *Store) RecordSoftware(ctx context.Context, deviceID int64, items []protocol.SoftwareItem, now time.Time) error {
	if items == nil {
		items = []protocol.SoftwareItem{}
	}
	b, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("store: record software: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET software_json = ?, software_count = ?, software_checked_at = ?
		  WHERE id = ?`,
		string(b), len(items), fmtTime(now), deviceID)
	if err != nil {
		return fmt.Errorf("store: record software: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: record software: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SoftwareSearch finds installed packages whose name contains query
// (case-insensitive substring) across the caller's visible devices (RS2:
// visibleOrgs nil = every org, a non-nil slice restricts to those org ids, an
// empty slice matches nothing). It returns the matching rows ordered by org
// name, hostname then package name, capped at limit (SoftwareSearchLimit when
// limit <= 0); truncated is true when the cap cut the result (more matches
// exist). The scan unmarshals each visible device's software_json in Go, which
// is adequate at phase-1 fleet scale and avoids a JSON LIKE whose matches would
// not be anchored to a package boundary.
func (s *Store) SoftwareSearch(ctx context.Context, visibleOrgs []int64, query string, limit int) ([]SoftwareHit, bool, error) {
	if limit <= 0 {
		limit = SoftwareSearchLimit
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	hits := []SoftwareHit{}
	if needle == "" {
		return hits, false, nil
	}

	q := `SELECT d.id, d.hostname, d.org_id, o.name, d.software_json
	        FROM devices d JOIN orgs o ON o.id = d.org_id
	       WHERE d.software_count > 0`
	var args []any
	if clause, clauseArgs := orgInClause(`d.org_id`, visibleOrgs); clause != "" {
		q += ` AND ` + clause
		args = append(args, clauseArgs...)
	}
	q += ` ORDER BY o.name, d.hostname, d.id`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: software search: %w", err)
	}
	defer rows.Close()

	truncated := false
	for rows.Next() {
		var (
			h    SoftwareHit
			blob string
		)
		if err := rows.Scan(&h.DeviceID, &h.Hostname, &h.OrgID, &h.OrgName, &blob); err != nil {
			return nil, false, fmt.Errorf("store: software search: %w", err)
		}
		var items []protocol.SoftwareItem
		if blob != "" {
			if err := json.Unmarshal([]byte(blob), &items); err != nil {
				// A device with an unreadable blob is skipped rather than failing
				// the whole search; the list was written by RecordSoftware so this
				// should not happen in practice.
				continue
			}
		}
		for _, it := range items {
			if !strings.Contains(strings.ToLower(it.Name), needle) {
				continue
			}
			if len(hits) >= limit {
				truncated = true
				break
			}
			hits = append(hits, SoftwareHit{
				DeviceID: h.DeviceID,
				Hostname: h.Hostname,
				OrgID:    h.OrgID,
				OrgName:  h.OrgName,
				Name:     it.Name,
				Version:  it.Version,
			})
		}
		if truncated {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: software search: %w", err)
	}
	return hits, truncated, nil
}
