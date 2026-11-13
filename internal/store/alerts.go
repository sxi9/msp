package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const alertSelect = `
SELECT a.id, a.org_id, o.name, a.device_id, d.hostname, a.type, a.severity, a.message,
       a.opened_at, a.resolved_at, a.acknowledged_by, a.acknowledged_at
  FROM alerts a JOIN orgs o ON o.id = a.org_id JOIN devices d ON d.id = a.device_id`

func scanAlert(row interface{ Scan(...any) error }) (Alert, error) {
	var (
		a               Alert
		opened          string
		resolved, acked sql.NullString
		ackBy           sql.NullInt64
	)
	err := row.Scan(&a.ID, &a.OrgID, &a.OrgName, &a.DeviceID, &a.Hostname, &a.Type, &a.Severity, &a.Message,
		&opened, &resolved, &ackBy, &acked)
	if err != nil {
		return Alert{}, err
	}
	if a.OpenedAt, err = parseTime(opened); err != nil {
		return Alert{}, err
	}
	if a.ResolvedAt, err = nullTime(resolved); err != nil {
		return Alert{}, err
	}
	a.AcknowledgedBy = nullInt64(ackBy)
	if a.AcknowledgedAt, err = nullTime(acked); err != nil {
		return Alert{}, err
	}
	return a, nil
}

// OpenAlert opens an alert of the given type for a device unless one is
// already open (INSERT OR IGNORE against the alerts_open partial unique
// index). created reports whether a new row was inserted.
func (s *Store) OpenAlert(ctx context.Context, orgID, deviceID int64, typ, severity, message string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO alerts (org_id, device_id, type, severity, message, opened_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		orgID, deviceID, typ, severity, message, fmtTime(s.Now()))
	if err != nil {
		return false, fmt.Errorf("store: open alert: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return false, fmt.Errorf("store: open alert: %w", err)
	}
	return n == 1, nil
}

// ResolveAlert closes the open alert of a type on a device, if any.
func (s *Store) ResolveAlert(ctx context.Context, deviceID int64, typ string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET resolved_at = ? WHERE device_id = ? AND type = ? AND resolved_at IS NULL`,
		fmtTime(s.Now()), deviceID, typ)
	if err != nil {
		return fmt.Errorf("store: resolve alert: %w", err)
	}
	return nil
}

// ListAlerts returns alerts newest first, optionally only open ones and
// only for one org, with org_name and hostname joined in.
func (s *Store) ListAlerts(ctx context.Context, openOnly bool, orgID *int64, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 500
	}
	q := alertSelect
	var (
		where []string
		args  []any
	)
	if openOnly {
		where = append(where, `a.resolved_at IS NULL`)
	}
	if orgID != nil {
		where = append(where, `a.org_id = ?`)
		args = append(args, *orgID)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY a.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list alerts: %w", err)
	}
	defer rows.Close()
	alerts := []Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list alerts: %w", err)
		}
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list alerts: %w", err)
	}
	return alerts, nil
}

// AckAlert marks an open alert acknowledged by a user. ErrNotFound when the
// alert does not exist or is already resolved.
func (s *Store) AckAlert(ctx context.Context, id, userID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET acknowledged_by = ?, acknowledged_at = ? WHERE id = ? AND resolved_at IS NULL`,
		userID, fmtTime(s.Now()), id)
	if err != nil {
		return fmt.Errorf("store: ack alert: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: ack alert: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountOpenAlerts returns the number of unresolved alerts.
func (s *Store) CountOpenAlerts(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts WHERE resolved_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count open alerts: %w", err)
	}
	return n, nil
}
