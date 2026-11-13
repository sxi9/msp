package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// deviceSelect is the one SELECT every device read goes through; scanDevice
// matches its column order.
const deviceSelect = `
SELECT d.id, d.org_id, o.name, d.device_uid, d.hostname, d.platform, d.arch, d.os_version,
       d.agent_version, d.interval_s, d.enrolled_at, d.last_seen_at, d.last_ip,
       d.token_hash IS NULL, d.facts_json, d.last_metrics_json,
       d.cpu_pct, d.mem_used_pct, d.disk_used_pct, d.battery_pct, d.battery_charging, d.uptime_s,
       (SELECT COUNT(*) FROM alerts a WHERE a.device_id = d.id AND a.resolved_at IS NULL)
  FROM devices d JOIN orgs o ON o.id = d.org_id`

func (s *Store) scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var (
		d                                      Device
		enrolled                               string
		lastSeen                               sql.NullString
		revoked                                int
		cpu, mem, disk                         sql.NullFloat64
		battery, batteryCharging, uptime, open sql.NullInt64
	)
	err := row.Scan(&d.ID, &d.OrgID, &d.OrgName, &d.DeviceUID, &d.Hostname, &d.Platform, &d.Arch, &d.OSVersion,
		&d.AgentVersion, &d.IntervalS, &enrolled, &lastSeen, &d.LastIP,
		&revoked, &d.FactsJSON, &d.LastMetricsJSON,
		&cpu, &mem, &disk, &battery, &batteryCharging, &uptime, &open)
	if err != nil {
		return Device{}, err
	}
	if d.EnrolledAt, err = parseTime(enrolled); err != nil {
		return Device{}, err
	}
	if d.LastSeenAt, err = nullTime(lastSeen); err != nil {
		return Device{}, err
	}
	d.Revoked = revoked != 0
	d.CPUPct = nullFloat(cpu)
	d.MemUsedPct = nullFloat(mem)
	d.DiskUsedPct = nullFloat(disk)
	d.BatteryPct = nullInt(battery)
	d.BatteryCharging = nullBool(batteryCharging)
	d.UptimeS = nullInt64(uptime)
	d.OpenAlerts = int(open.Int64)
	d.Status = StatusOf(d.LastSeenAt, d.IntervalS, s.Now())
	d.CanRunCommands = d.Platform != protocol.PlatformAndroid && !d.Revoked
	return d, nil
}

func (s *Store) getDevice(ctx context.Context, q querier, where string, args ...any) (Device, error) {
	d, err := s.scanDevice(q.QueryRowContext(ctx, deviceSelect+` WHERE `+where, args...))
	if err != nil {
		return Device{}, err
	}
	return d, nil
}

// PreviousEnrollment describes the device row an enrollment is about to
// replace: the same (org_id, device_uid) already exists. The server uses it
// to flag a re-enrollment from a different host (cloned machine-id) in the
// audit log.
type PreviousEnrollment struct {
	DeviceID int64
	Hostname string
	Status   string // pending | online | offline, as StatusOf computes it
	Revoked  bool
}

// FindEnrollment returns the existing row for (orgID, deviceUID), if any.
func (s *Store) FindEnrollment(ctx context.Context, orgID int64, deviceUID string) (PreviousEnrollment, bool, error) {
	var (
		p        PreviousEnrollment
		lastSeen sql.NullString
		interval int
		revoked  int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, hostname, last_seen_at, interval_s, token_hash IS NULL FROM devices WHERE org_id = ? AND device_uid = ?`,
		orgID, deviceUID).Scan(&p.DeviceID, &p.Hostname, &lastSeen, &interval, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return PreviousEnrollment{}, false, nil
	}
	if err != nil {
		return PreviousEnrollment{}, false, fmt.Errorf("store: find enrollment: %w", err)
	}
	seen, err := nullTime(lastSeen)
	if err != nil {
		return PreviousEnrollment{}, false, err
	}
	p.Status = StatusOf(seen, interval, s.Now())
	p.Revoked = revoked != 0
	return p, true, nil
}

// UpsertDevice enrolls a device. A re-enrollment of the same (org_id,
// device_uid) keeps the row (and its heartbeats, commands and alerts),
// refreshes the inventory columns and rotates the device token. The
// plaintext "dt_" token is returned once.
func (s *Store) UpsertDevice(ctx context.Context, p UpsertDeviceParams) (Device, string, error) {
	facts := strings.TrimSpace(p.FactsJSON)
	if facts == "" {
		facts = "{}"
	}
	plain := NewToken(protocol.DeviceTokenPrefix)
	hash := HashToken(plain)
	interval := protocol.ClampInterval(p.IntervalS, p.Platform)
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO devices (org_id, device_uid, hostname, platform, arch, os_version, agent_version,
		                      token_hash, interval_s, enrolled_at, facts_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (org_id, device_uid) DO UPDATE SET
		     hostname = excluded.hostname, arch = excluded.arch, os_version = excluded.os_version,
		     agent_version = excluded.agent_version, interval_s = excluded.interval_s,
		     facts_json = excluded.facts_json, token_hash = excluded.token_hash
		 RETURNING id`,
		p.OrgID, p.DeviceUID, p.Hostname, p.Platform, p.Arch, p.OSVersion, p.AgentVersion,
		hash, interval, fmtTime(s.Now()), facts).Scan(&id)
	if err != nil {
		return Device{}, "", fmt.Errorf("store: upsert device: %w", err)
	}
	d, err := s.getDevice(ctx, s.db, `d.id = ?`, id)
	if err != nil {
		return Device{}, "", fmt.Errorf("store: upsert device: %w", err)
	}
	return d, plain, nil
}

// GetDeviceByToken authenticates an agent by its plaintext device token.
// ErrInvalidToken when unknown or revoked.
func (s *Store) GetDeviceByToken(ctx context.Context, plain string) (Device, error) {
	d, err := s.getDevice(ctx, s.db, `d.token_hash = ?`, HashToken(plain))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrInvalidToken
	}
	if err != nil {
		return Device{}, fmt.Errorf("store: get device by token: %w", err)
	}
	return d, nil
}

// GetDevice returns one device by id.
func (s *Store) GetDevice(ctx context.Context, id int64) (Device, error) {
	d, err := s.getDevice(ctx, s.db, `d.id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("store: get device: %w", err)
	}
	return d, nil
}

// ListDevices returns devices ordered by org name then hostname. OrgID and the
// hostname substring are applied in SQL, the status filter in Go via StatusOf.
func (s *Store) ListDevices(ctx context.Context, f DeviceFilter) ([]Device, error) {
	q := deviceSelect
	var (
		where []string
		args  []any
	)
	if f.OrgID != nil {
		where = append(where, `d.org_id = ?`)
		args = append(args, *f.OrgID)
	}
	if f.Query != "" {
		where = append(where, `d.hostname LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeEscape(f.Query)+"%")
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY o.name, d.hostname, d.id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list devices: %w", err)
	}
	defer rows.Close()
	devices := []Device{}
	for rows.Next() {
		d, err := s.scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list devices: %w", err)
		}
		if f.Status != "" && d.Status != f.Status {
			continue
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list devices: %w", err)
	}
	return devices, nil
}

// likeEscape makes a user string safe inside a LIKE pattern with ESCAPE '\'.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// RecordSync stores a heartbeat in one transaction: it refreshes the
// device's last_seen/last_ip/agent_version/interval and the denormalised
// metric columns (and facts when present), then inserts a heartbeats row if
// the previous sample is older than sampleEvery.
func (s *Store) RecordSync(ctx context.Context, deviceID int64, ip string, req protocol.SyncRequest, sampleEvery time.Duration) error {
	now := s.Now()
	m := req.Metrics
	if m.Disks == nil {
		m.Disks = []protocol.Disk{}
	}
	if m.IPs == nil {
		m.IPs = []string{}
	}
	metricsJSON, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: record sync: %w", err)
	}
	var memPct *float64
	if m.MemTotalMB > 0 {
		v := pct1(float64(m.MemUsedMB), float64(m.MemTotalMB))
		memPct = &v
	}
	// Fullest filesystem that counts (small /boot and EFI mounts are ignored,
	// see protocol.DiskCounts); the mount is recovered from last_metrics_json
	// by the alert evaluator.
	var diskPct *float64
	if v, _, ok := protocol.DiskUsedPct(m.Disks); ok {
		diskPct = &v
	}
	var factsJSON *string
	if req.Facts != nil {
		b, err := json.Marshal(req.Facts)
		if err != nil {
			return fmt.Errorf("store: record sync: %w", err)
		}
		str := string(b)
		factsJSON = &str
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: record sync: %w", err)
	}
	defer tx.Rollback()

	var (
		platform   string
		lastSample sql.NullString
	)
	err = tx.QueryRowContext(ctx, `SELECT platform, last_sample_at FROM devices WHERE id = ?`, deviceID).Scan(&platform, &lastSample)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: record sync: %w", err)
	}
	interval := protocol.ClampInterval(req.IntervalS, platform)

	_, err = tx.ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ?, last_ip = ?, agent_version = ?, interval_s = ?,
		        last_metrics_json = ?, cpu_pct = ?, mem_used_pct = ?, disk_used_pct = ?,
		        battery_pct = ?, battery_charging = ?, uptime_s = ?,
		        facts_json = COALESCE(?, facts_json)
		  WHERE id = ?`,
		fmtTime(now), ip, req.AgentVersion, interval,
		string(metricsJSON), ptrFloatArg(m.CPUPct), ptrFloatArg(memPct), ptrFloatArg(diskPct),
		ptrIntArg(m.BatteryPct), ptrBoolArg(m.BatteryCharging), req.UptimeS,
		factsJSON, deviceID)
	if err != nil {
		return fmt.Errorf("store: record sync: %w", err)
	}

	sample := true
	if lastSample.Valid && lastSample.String != "" {
		t, err := parseTime(lastSample.String)
		if err != nil {
			return err
		}
		if now.Sub(t) < sampleEvery {
			sample = false
		}
	}
	if sample {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO heartbeats (device_id, at, cpu_pct, mem_used_pct, disk_used_pct, battery_pct, uptime_s)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			deviceID, fmtTime(now), ptrFloatArg(m.CPUPct), ptrFloatArg(memPct), ptrFloatArg(diskPct),
			ptrIntArg(m.BatteryPct), req.UptimeS); err != nil {
			return fmt.Errorf("store: record sync: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE devices SET last_sample_at = ? WHERE id = ?`, fmtTime(now), deviceID); err != nil {
			return fmt.Errorf("store: record sync: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: record sync: %w", err)
	}
	return nil
}

// pct1 is used/total*100 rounded to one decimal. Multiplying before dividing
// keeps exact ratios such as 8120/16000 from landing on 507.4999.
func pct1(used, total float64) float64 {
	return math.Round(used*1000/total) / 10
}

// RevokeDevice clears the device token so the agent's next request is 401.
func (s *Store) RevokeDevice(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET token_hash = NULL WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: revoke device: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: revoke device: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDevice removes a device; heartbeats, commands and alerts cascade.
func (s *Store) DeleteDevice(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete device: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: delete device: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
