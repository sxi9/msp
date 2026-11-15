package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Service/process watchlists and forwarded logs (WL1). watchlist_items are the
// service/process items a desktop watches, org-scoped (every desktop in the org)
// or device-scoped (one device); WatchlistForDevice unions the applicable items
// into the protocol.WatchItem list the sync response delivers. RecordServices
// stores the device's latest reported state (facts.services) on the device row,
// mirroring RecordSoftware. AppendLogs/ListLogs persist and read the forwarded
// log lines (device_logs), and PruneDeviceLogs is the retention sweep the hourly
// pruner runs. Every list/read is RS2 org-scoped by the caller's visible org set.

// DeviceLogCap bounds device_logs per device: AppendLogs keeps the newest
// DeviceLogCap rows after each insert so a chatty host cannot grow the table
// without bound between the (time-based) retention sweeps.
const DeviceLogCap = 5000

// WatchlistItem is one service/process watchlist entry. OrgName is the owning
// org's name (joined; "" for a global item with org_id NULL); Hostname is the
// device's hostname for a device-scoped item ("" for an org-scoped one). Alert
// reports whether a stopped item opens a service_down alert.
type WatchlistItem struct {
	ID        int64     `json:"id"`
	OrgID     *int64    `json:"org_id"`
	OrgName   string    `json:"org_name"`
	Scope     string    `json:"scope"`
	DeviceID  *int64    `json:"device_id"`
	Hostname  string    `json:"hostname"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Alert     bool      `json:"alert"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy *int64    `json:"created_by"`
}

// CreateWatchlistItemParams is a validated watchlist item to insert. The server
// derives OrgID (the device's org for a device-scoped item) and sets DeviceID for
// scope 'device' only; the store persists what it is given.
type CreateWatchlistItemParams struct {
	OrgID     *int64
	Scope     string
	DeviceID  *int64
	Kind      string
	Name      string
	Alert     bool
	CreatedBy *int64
}

// UpdateWatchlistItemParams is the mutable subset of a watchlist item (PUT):
// its kind, name and alert flag. Scope/org/device are fixed at creation.
type UpdateWatchlistItemParams struct {
	Kind  string
	Name  string
	Alert bool
}

// WatchlistFilter narrows ListWatchlistItems. OrgID restricts to one client org;
// DeviceID restricts to items whose device_id matches (device-scoped items for
// one device). VisibleOrgs is the caller's RS2 visible org set: nil = every org,
// a non-nil slice restricts to org_id IN (…), an empty slice matches nothing.
type WatchlistFilter struct {
	OrgID       *int64
	DeviceID    *int64
	VisibleOrgs []int64
}

// DeviceLog is one forwarded log line (device_logs), newest first in ListLogs.
type DeviceLog struct {
	ID       int64     `json:"id"`
	DeviceID int64     `json:"device_id"`
	OrgID    int64     `json:"org_id"`
	At       time.Time `json:"at"`
	Source   string    `json:"source"`
	Unit     string    `json:"unit"`
	Level    string    `json:"level"`
	Message  string    `json:"message"`
}

// LogFilter narrows ListLogs: Unit is an exact unit match, Level an exact level
// match; "" for either leaves that dimension unfiltered.
type LogFilter struct {
	Unit  string
	Level string
}

const watchlistSelect = `
SELECT w.id, w.org_id, COALESCE(o.name, ''), w.scope, w.device_id, COALESCE(d.hostname, ''),
       w.kind, w.name, w.alert, w.created_at, w.created_by
  FROM watchlist_items w
  LEFT JOIN orgs o ON o.id = w.org_id
  LEFT JOIN devices d ON d.id = w.device_id`

func scanWatchlistItem(row interface{ Scan(...any) error }) (WatchlistItem, error) {
	var (
		w         WatchlistItem
		orgID     sql.NullInt64
		deviceID  sql.NullInt64
		createdBy sql.NullInt64
		alert     int
		created   string
	)
	if err := row.Scan(&w.ID, &orgID, &w.OrgName, &w.Scope, &deviceID, &w.Hostname,
		&w.Kind, &w.Name, &alert, &created, &createdBy); err != nil {
		return WatchlistItem{}, err
	}
	w.OrgID = nullInt64(orgID)
	w.DeviceID = nullInt64(deviceID)
	w.CreatedBy = nullInt64(createdBy)
	w.Alert = alert != 0
	var err error
	if w.CreatedAt, err = parseTime(created); err != nil {
		return WatchlistItem{}, err
	}
	return w, nil
}

// CreateWatchlistItem inserts a watchlist item and returns it.
func (s *Store) CreateWatchlistItem(ctx context.Context, p CreateWatchlistItemParams) (WatchlistItem, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO watchlist_items (org_id, scope, device_id, kind, name, alert, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ptrInt64Arg(p.OrgID), p.Scope, ptrInt64Arg(p.DeviceID), p.Kind, p.Name, boolInt(p.Alert),
		fmtTime(s.Now()), ptrInt64Arg(p.CreatedBy))
	if err != nil {
		return WatchlistItem{}, fmt.Errorf("store: create watchlist item: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return WatchlistItem{}, fmt.Errorf("store: create watchlist item: %w", err)
	}
	return s.GetWatchlistItem(ctx, id)
}

// GetWatchlistItem returns one watchlist item by id (with org_name and hostname
// joined in) for the RS2 single-resource visibility check. ErrNotFound when it
// does not exist.
func (s *Store) GetWatchlistItem(ctx context.Context, id int64) (WatchlistItem, error) {
	w, err := scanWatchlistItem(s.db.QueryRowContext(ctx, watchlistSelect+` WHERE w.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return WatchlistItem{}, ErrNotFound
	}
	if err != nil {
		return WatchlistItem{}, fmt.Errorf("store: get watchlist item: %w", err)
	}
	return w, nil
}

// ListWatchlistItems returns watchlist items newest first, org-scoped (RS2).
func (s *Store) ListWatchlistItems(ctx context.Context, filter WatchlistFilter) ([]WatchlistItem, error) {
	q := watchlistSelect
	var (
		where []string
		args  []any
	)
	if clause, clauseArgs := orgInClause(`w.org_id`, filter.VisibleOrgs); clause != "" {
		where = append(where, clause)
		args = append(args, clauseArgs...)
	}
	if filter.OrgID != nil {
		where = append(where, `w.org_id = ?`)
		args = append(args, *filter.OrgID)
	}
	if filter.DeviceID != nil {
		where = append(where, `w.device_id = ?`)
		args = append(args, *filter.DeviceID)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY w.id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list watchlist items: %w", err)
	}
	defer rows.Close()
	items := []WatchlistItem{}
	for rows.Next() {
		w, err := scanWatchlistItem(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list watchlist items: %w", err)
		}
		items = append(items, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list watchlist items: %w", err)
	}
	return items, nil
}

// UpdateWatchlistItem updates a watchlist item's kind, name and alert flag.
// ErrNotFound when it does not exist.
func (s *Store) UpdateWatchlistItem(ctx context.Context, id int64, p UpdateWatchlistItemParams) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE watchlist_items SET kind = ?, name = ?, alert = ? WHERE id = ?`,
		p.Kind, p.Name, boolInt(p.Alert), id)
	if err != nil {
		return fmt.Errorf("store: update watchlist item: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: update watchlist item: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWatchlistItem removes a watchlist item. ErrNotFound when it does not exist.
func (s *Store) DeleteWatchlistItem(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM watchlist_items WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete watchlist item: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: delete watchlist item: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// WatchlistForDevice returns the device's applicable watchlist as the
// protocol.WatchItem list the sync response delivers: the union of its org's
// org-scoped items and its own device-scoped items, deduplicated on (kind, name)
// and capped at protocol.MaxWatchItems. Android devices are excluded from
// watchlists entirely (nil), matching the agent and the protocol. The result is
// ordered (kind, name) so it is stable across syncs.
func (s *Store) WatchlistForDevice(ctx context.Context, d Device) ([]protocol.WatchItem, error) {
	if d.Platform == protocol.PlatformAndroid {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, name FROM watchlist_items
		  WHERE (scope = 'org' AND org_id = ?) OR (scope = 'device' AND device_id = ?)
		  ORDER BY kind, name`,
		d.OrgID, d.ID)
	if err != nil {
		return nil, fmt.Errorf("store: watchlist for device: %w", err)
	}
	defer rows.Close()
	var (
		out  []protocol.WatchItem
		seen = map[string]bool{}
	)
	for rows.Next() {
		var w protocol.WatchItem
		if err := rows.Scan(&w.Kind, &w.Name); err != nil {
			return nil, fmt.Errorf("store: watchlist for device: %w", err)
		}
		key := w.Kind + "\x00" + w.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, w)
		if len(out) >= protocol.MaxWatchItems {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: watchlist for device: %w", err)
	}
	return out, nil
}

// RecordServices stores a device's latest watched-service state (WL1), reported
// in facts.services. It writes the full [{kind,name,running}] list as JSON into
// services_json and refreshes services_checked_at (now). A nil list is stored as
// an empty array so the column round-trips as "[]". ErrNotFound when the device
// is gone.
func (s *Store) RecordServices(ctx context.Context, deviceID int64, items []protocol.ServiceState, now time.Time) error {
	if items == nil {
		items = []protocol.ServiceState{}
	}
	b, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("store: record services: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET services_json = ?, services_checked_at = ? WHERE id = ?`,
		string(b), fmtTime(now), deviceID)
	if err != nil {
		return fmt.Errorf("store: record services: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: record services: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ServicesFor unmarshals a device's stored services_json into the reported
// state. A blank or unreadable blob yields an empty slice.
func (d Device) ServicesFor() []protocol.ServiceState {
	out := []protocol.ServiceState{}
	if strings.TrimSpace(d.ServicesJSON) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(d.ServicesJSON), &out); err != nil {
		return []protocol.ServiceState{}
	}
	return out
}

// AppendLogs appends forwarded log entries for a device (WL1) in one transaction
// and then trims device_logs for that device to the newest DeviceLogCap rows so a
// single chatty host cannot grow the table without bound between retention
// sweeps. A nil/empty batch is a no-op. The caller has already validated the
// batch (protocol.ValidateLogs) and resolved orgID from the device.
func (s *Store) AppendLogs(ctx context.Context, deviceID, orgID int64, entries []protocol.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: append logs: %w", err)
	}
	defer tx.Rollback()
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO device_logs (device_id, org_id, at, source, unit, level, message)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			deviceID, orgID, fmtTime(e.At), e.Source, e.Unit, e.Level, e.Message); err != nil {
			return fmt.Errorf("store: append logs: %w", err)
		}
	}
	// Trim to the newest DeviceLogCap rows for this device (by at, then id).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM device_logs
		  WHERE device_id = ? AND id NOT IN (
		        SELECT id FROM device_logs WHERE device_id = ? ORDER BY at DESC, id DESC LIMIT ?)`,
		deviceID, deviceID, DeviceLogCap); err != nil {
		return fmt.Errorf("store: append logs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: append logs: %w", err)
	}
	return nil
}

// ListLogs returns a device's forwarded log lines newest first, optionally
// filtered by unit and/or level, capped at limit (defaults to 500 when limit
// <= 0). The caller has already made the RS2 device-visibility check.
func (s *Store) ListLogs(ctx context.Context, deviceID int64, filter LogFilter, limit int) ([]DeviceLog, error) {
	if limit <= 0 {
		limit = 500
	}
	q := `SELECT id, device_id, org_id, at, source, unit, level, message FROM device_logs WHERE device_id = ?`
	args := []any{deviceID}
	if filter.Unit != "" {
		q += ` AND unit = ?`
		args = append(args, filter.Unit)
	}
	if filter.Level != "" {
		q += ` AND level = ?`
		args = append(args, filter.Level)
	}
	q += ` ORDER BY at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list logs: %w", err)
	}
	defer rows.Close()
	logs := []DeviceLog{}
	for rows.Next() {
		var (
			l  DeviceLog
			at string
		)
		if err := rows.Scan(&l.ID, &l.DeviceID, &l.OrgID, &at, &l.Source, &l.Unit, &l.Level, &l.Message); err != nil {
			return nil, fmt.Errorf("store: list logs: %w", err)
		}
		if l.At, err = parseTime(at); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list logs: %w", err)
	}
	return logs, nil
}

// PruneDeviceLogs deletes forwarded log lines older than olderThan in batches of
// pruneBatch rows (using the device_logs_device_at index) and returns the count.
// The hourly pruner runs it with MSP_LOG_RETENTION_DAYS, matching PruneHeartbeats.
func (s *Store) PruneDeviceLogs(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := fmtTime(s.Now().Add(-olderThan))
	var total int64
	for {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM device_logs WHERE id IN (SELECT id FROM device_logs WHERE at < ? LIMIT ?)`, cutoff, pruneBatch)
		if err != nil {
			return total, fmt.Errorf("store: prune device logs: %w", err)
		}
		n, err := rowsAffected(res)
		if err != nil {
			return total, fmt.Errorf("store: prune device logs: %w", err)
		}
		total += n
		if n < pruneBatch {
			return total, nil
		}
	}
}
