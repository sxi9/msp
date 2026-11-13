package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Summary computes the dashboard counters in one device scan plus two counts.
// visible restricts every counter to a caller's visible org set (RS2): nil =
// every org, a non-nil but empty slice = none.
func (s *Store) Summary(ctx context.Context, visible []int64) (Summary, error) {
	var sum Summary

	orgWhere, orgArgs := orgInClause(`id`, visible)
	orgQ := `SELECT COUNT(*) FROM orgs`
	if orgWhere != "" {
		orgQ += ` WHERE ` + orgWhere
	}
	if err := s.db.QueryRowContext(ctx, orgQ, orgArgs...).Scan(&sum.Orgs); err != nil {
		return Summary{}, fmt.Errorf("store: summary: %w", err)
	}

	now := s.Now()
	devWhere, devArgs := orgInClause(`org_id`, visible)
	devQ := `SELECT last_seen_at, interval_s FROM devices`
	if devWhere != "" {
		devQ += ` WHERE ` + devWhere
	}
	rows, err := s.db.QueryContext(ctx, devQ, devArgs...)
	if err != nil {
		return Summary{}, fmt.Errorf("store: summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			lastSeen sql.NullString
			interval int
		)
		if err := rows.Scan(&lastSeen, &interval); err != nil {
			return Summary{}, fmt.Errorf("store: summary: %w", err)
		}
		seen, err := nullTime(lastSeen)
		if err != nil {
			return Summary{}, err
		}
		sum.Devices++
		switch StatusOf(seen, interval, now) {
		case "online":
			sum.Online++
		case "offline":
			sum.Offline++
		default:
			sum.Pending++
		}
	}
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("store: summary: %w", err)
	}

	alertWhere, alertArgs := orgInClause(`org_id`, visible)
	alertQ := `SELECT COUNT(*) FROM alerts WHERE resolved_at IS NULL`
	if alertWhere != "" {
		alertQ += ` AND ` + alertWhere
	}
	if err := s.db.QueryRowContext(ctx, alertQ, alertArgs...).Scan(&sum.OpenAlerts); err != nil {
		return Summary{}, fmt.Errorf("store: summary: %w", err)
	}
	return sum, nil
}
