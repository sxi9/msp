package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Audit appends one entry. At defaults to Now(); Detail is stored as JSON
// ("{}" when nil). The audit log is append-only: there is no update or delete.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	at := e.At
	if at.IsZero() {
		at = s.Now()
	}
	detail := "{}"
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("store: audit: %w", err)
		}
		detail = string(b)
	}
	actorType := e.ActorType
	if actorType == "" {
		actorType = "system"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (at, actor_type, actor_id, org_id, action, target_type, target_id, ip, detail_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fmtTime(at), actorType, ptrInt64Arg(e.ActorID), ptrInt64Arg(e.OrgID), e.Action, e.TargetType,
		ptrInt64Arg(e.TargetID), e.IP, detail)
	if err != nil {
		return fmt.Errorf("store: audit: %w", err)
	}
	return nil
}

// ListAudit returns entries newest first, optionally scoped to an org and to
// ids below beforeID (0 = no bound). ActorLabel is the user's email for
// actor_type user, "device:<id>" for device, "system" otherwise.
func (s *Store) ListAudit(ctx context.Context, limit int, orgID *int64, beforeID int64) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT e.id, e.at, e.actor_type, e.actor_id, u.email, e.org_id, e.action, e.target_type, e.target_id,
	             e.ip, e.detail_json
	        FROM audit_log e LEFT JOIN users u ON e.actor_type = 'user' AND u.id = e.actor_id`
	var (
		where []string
		args  []any
	)
	if orgID != nil {
		where = append(where, `e.org_id = ?`)
		args = append(args, *orgID)
	}
	if beforeID > 0 {
		where = append(where, `e.id < ?`)
		args = append(args, beforeID)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY e.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	entries := []AuditEntry{}
	for rows.Next() {
		var (
			e                        AuditEntry
			at, detail               string
			actorID, orgID, targetID sql.NullInt64
			email                    sql.NullString
		)
		if err := rows.Scan(&e.ID, &at, &e.ActorType, &actorID, &email, &orgID, &e.Action, &e.TargetType, &targetID,
			&e.IP, &detail); err != nil {
			return nil, fmt.Errorf("store: list audit: %w", err)
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		e.ActorID = nullInt64(actorID)
		e.OrgID = nullInt64(orgID)
		e.TargetID = nullInt64(targetID)
		e.Detail = map[string]any{}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &e.Detail); err != nil || e.Detail == nil {
				e.Detail = map[string]any{}
			}
		}
		switch e.ActorType {
		case "user":
			if email.Valid && email.String != "" {
				e.ActorLabel = email.String
			} else if e.ActorID != nil {
				e.ActorLabel = "user:" + strconv.FormatInt(*e.ActorID, 10)
			} else {
				e.ActorLabel = "user"
			}
		case "device":
			if e.ActorID != nil {
				e.ActorLabel = "device:" + strconv.FormatInt(*e.ActorID, 10)
			} else {
				e.ActorLabel = "device"
			}
		default:
			e.ActorLabel = "system"
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	return entries, nil
}
