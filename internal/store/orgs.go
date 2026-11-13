package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// CreateOrg inserts an organisation. A duplicate name yields ErrConflict.
func (s *Store) CreateOrg(ctx context.Context, name string) (Org, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Org{}, errors.New("store: org name is empty")
	}
	now := s.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO orgs (name, created_at) VALUES (?, ?)`, name, fmtTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return Org{}, ErrConflict
		}
		return Org{}, fmt.Errorf("store: create org: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Org{}, fmt.Errorf("store: create org: %w", err)
	}
	return Org{ID: id, Name: name, CreatedAt: storedTime(now)}, nil
}

// GetOrg returns one organisation with its fleet counts.
func (s *Store) GetOrg(ctx context.Context, id int64) (Org, error) {
	var (
		o  Org
		at string
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, name, created_at FROM orgs WHERE id = ?`, id).Scan(&o.ID, &o.Name, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Org{}, ErrNotFound
	}
	if err != nil {
		return Org{}, fmt.Errorf("store: get org: %w", err)
	}
	if o.CreatedAt, err = parseTime(at); err != nil {
		return Org{}, err
	}
	counts, err := s.orgCounts(ctx, &id)
	if err != nil {
		return Org{}, err
	}
	if c, ok := counts[id]; ok {
		o.DeviceCount, o.OnlineCount, o.OpenAlerts = c.devices, c.online, c.openAlerts
	}
	return o, nil
}

// ListOrgs returns every organisation ordered by name, with device_count,
// online_count (StatusOf over a device scan) and open_alerts.
func (s *Store) ListOrgs(ctx context.Context) ([]Org, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM orgs ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list orgs: %w", err)
	}
	defer rows.Close()
	orgs := []Org{}
	for rows.Next() {
		var (
			o  Org
			at string
		)
		if err := rows.Scan(&o.ID, &o.Name, &at); err != nil {
			return nil, fmt.Errorf("store: list orgs: %w", err)
		}
		if o.CreatedAt, err = parseTime(at); err != nil {
			return nil, err
		}
		orgs = append(orgs, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list orgs: %w", err)
	}
	counts, err := s.orgCounts(ctx, nil)
	if err != nil {
		return nil, err
	}
	for i := range orgs {
		if c, ok := counts[orgs[i].ID]; ok {
			orgs[i].DeviceCount, orgs[i].OnlineCount, orgs[i].OpenAlerts = c.devices, c.online, c.openAlerts
		}
	}
	return orgs, nil
}

type orgCount struct {
	devices, online, openAlerts int
}

// orgCounts scans devices (status via StatusOf) and groups open alerts, for
// one org or for all when orgID is nil.
func (s *Store) orgCounts(ctx context.Context, orgID *int64) (map[int64]*orgCount, error) {
	counts := map[int64]*orgCount{}
	get := func(id int64) *orgCount {
		c, ok := counts[id]
		if !ok {
			c = &orgCount{}
			counts[id] = c
		}
		return c
	}
	now := s.Now()

	q := `SELECT org_id, last_seen_at, interval_s FROM devices`
	var args []any
	if orgID != nil {
		q += ` WHERE org_id = ?`
		args = append(args, *orgID)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: org counts: %w", err)
	}
	for rows.Next() {
		var (
			id       int64
			lastSeen sql.NullString
			interval int
		)
		if err := rows.Scan(&id, &lastSeen, &interval); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: org counts: %w", err)
		}
		seen, err := nullTime(lastSeen)
		if err != nil {
			rows.Close()
			return nil, err
		}
		c := get(id)
		c.devices++
		if StatusOf(seen, interval, now) == "online" {
			c.online++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: org counts: %w", err)
	}

	q = `SELECT org_id, COUNT(*) FROM alerts WHERE resolved_at IS NULL`
	if orgID != nil {
		q += ` AND org_id = ?`
	}
	q += ` GROUP BY org_id`
	rows, err = s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: org counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id int64
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: org counts: %w", err)
		}
		get(id).openAlerts = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: org counts: %w", err)
	}
	return counts, nil
}
