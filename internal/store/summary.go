package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Summary computes the dashboard counters in one device scan plus two counts.
func (s *Store) Summary(ctx context.Context) (Summary, error) {
	var sum Summary
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orgs`).Scan(&sum.Orgs); err != nil {
		return Summary{}, fmt.Errorf("store: summary: %w", err)
	}
	now := s.Now()
	rows, err := s.db.QueryContext(ctx, `SELECT last_seen_at, interval_s FROM devices`)
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
	if sum.OpenAlerts, err = s.CountOpenAlerts(ctx); err != nil {
		return Summary{}, err
	}
	return sum, nil
}
