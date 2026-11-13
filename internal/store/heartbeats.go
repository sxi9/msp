package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ListHeartbeats returns the newest limit samples of a device, oldest first.
func (s *Store) ListHeartbeats(ctx context.Context, deviceID int64, limit int) ([]Heartbeat, error) {
	if limit <= 0 {
		limit = 120
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, device_id, at, cpu_pct, mem_used_pct, disk_used_pct, battery_pct, uptime_s
		   FROM heartbeats WHERE device_id = ? ORDER BY id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list heartbeats: %w", err)
	}
	defer rows.Close()
	newestFirst := []Heartbeat{}
	for rows.Next() {
		var (
			h              Heartbeat
			at             string
			cpu, mem, disk sql.NullFloat64
			battery        sql.NullInt64
			uptime         sql.NullInt64
		)
		if err := rows.Scan(&h.ID, &h.DeviceID, &at, &cpu, &mem, &disk, &battery, &uptime); err != nil {
			return nil, fmt.Errorf("store: list heartbeats: %w", err)
		}
		if h.At, err = parseTime(at); err != nil {
			return nil, err
		}
		h.CPUPct = nullFloat(cpu)
		h.MemUsedPct = nullFloat(mem)
		h.DiskUsedPct = nullFloat(disk)
		h.BatteryPct = nullInt(battery)
		h.UptimeS = nullInt64(uptime)
		newestFirst = append(newestFirst, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list heartbeats: %w", err)
	}
	out := make([]Heartbeat, len(newestFirst))
	for i, h := range newestFirst {
		out[len(newestFirst)-1-i] = h
	}
	return out, nil
}

// PruneHeartbeats deletes samples older than olderThan and returns the count.
func (s *Store) PruneHeartbeats(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := fmtTime(s.Now().Add(-olderThan))
	res, err := s.db.ExecContext(ctx, `DELETE FROM heartbeats WHERE at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune heartbeats: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return 0, fmt.Errorf("store: prune heartbeats: %w", err)
	}
	return n, nil
}
