package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Script library (AUT1). A script is a saved, editable command template (distinct
// from the static presets.json console presets): either an exec (argv) or a
// script (shell + body) that a bulk run or an automation fans out across a device
// set. OrgID nil = a global script usable for any client (RS2 visibility, exactly
// like a global file/channel); a client-scoped script carries that org's id.
//
// Platform gates which devices a script may run on: 'linux', 'windows' or 'any'
// (linux and windows). exec/script are desktop kinds (KindSupportedOn excludes
// android), so a fan-out skips android devices with a per-device note.

// Script platform values (schema CHECK).
const (
	ScriptPlatformLinux   = "linux"
	ScriptPlatformWindows = "windows"
	ScriptPlatformAny     = "any"
)

// Script is one library entry. Argv is never nil (an empty slice for a script-kind
// row), mirroring Command.Argv; Shell/Body are set for kind script only.
type Script struct {
	ID          int64     `json:"id"`
	OrgID       *int64    `json:"org_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Platform    string    `json:"platform"`
	Kind        string    `json:"kind"`
	Argv        []string  `json:"argv"`
	Shell       string    `json:"shell"`
	Body        string    `json:"body"`
	TimeoutS    int       `json:"timeout_s"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   *int64    `json:"created_by"`
}

// CreateScriptParams is a validated library entry to insert. The server has
// already checked the shape (exec carries Argv; script carries Shell+Body) and
// the RS2 scope (OrgID nil requires all-orgs). TimeoutS 0 stores the column
// default (300).
type CreateScriptParams struct {
	OrgID       *int64
	Name        string
	Description string
	Platform    string
	Kind        string
	Argv        []string
	Shell       string
	Body        string
	TimeoutS    int
	CreatedBy   *int64
}

const scriptSelect = `
SELECT id, org_id, name, description, platform, kind, argv_json, shell, body, timeout_s, created_at, created_by
  FROM scripts`

func scanScript(row interface{ Scan(...any) error }) (Script, error) {
	var (
		sc                    Script
		orgID, createdBy      sql.NullInt64
		argvJSON, shell, body sql.NullString
		created               string
	)
	if err := row.Scan(&sc.ID, &orgID, &sc.Name, &sc.Description, &sc.Platform, &sc.Kind,
		&argvJSON, &shell, &body, &sc.TimeoutS, &created, &createdBy); err != nil {
		return Script{}, err
	}
	sc.OrgID = nullInt64(orgID)
	sc.CreatedBy = nullInt64(createdBy)
	var err error
	if sc.Argv, err = decodeArgv(argvJSON); err != nil {
		return Script{}, err
	}
	sc.Shell = nullString(shell)
	sc.Body = nullString(body)
	if sc.CreatedAt, err = parseTime(created); err != nil {
		return Script{}, err
	}
	return sc, nil
}

// scriptArgs renders the kind-dependent columns for an INSERT/UPDATE: a JSON argv
// for exec, shell+body for script, the other branch's columns left NULL.
func scriptArgs(kind string, argv []string, shell, body string) (argvJSON, shellArg, bodyArg any, err error) {
	switch kind {
	case "exec":
		a := argv
		if a == nil {
			a = []string{}
		}
		b, mErr := json.Marshal(a)
		if mErr != nil {
			return nil, nil, nil, fmt.Errorf("store: script argv: %w", mErr)
		}
		return string(b), nil, nil, nil
	case "script":
		return nil, shell, body, nil
	}
	return nil, nil, nil, fmt.Errorf("store: script: unknown kind %q", kind)
}

// CreateScript inserts a library script and returns it.
func (s *Store) CreateScript(ctx context.Context, p CreateScriptParams) (Script, error) {
	argvJSON, shellArg, bodyArg, err := scriptArgs(p.Kind, p.Argv, p.Shell, p.Body)
	if err != nil {
		return Script{}, err
	}
	timeout := p.TimeoutS
	if timeout <= 0 {
		timeout = 300
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO scripts (org_id, name, description, platform, kind, argv_json, shell, body, timeout_s, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ptrInt64Arg(p.OrgID), p.Name, p.Description, p.Platform, p.Kind, argvJSON, shellArg, bodyArg,
		timeout, fmtTime(s.Now()), ptrInt64Arg(p.CreatedBy))
	if err != nil {
		return Script{}, fmt.Errorf("store: create script: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Script{}, fmt.Errorf("store: create script: %w", err)
	}
	return s.GetScript(ctx, id)
}

// UpdateScriptParams replaces a script's mutable fields. Kind/Platform may change
// (the server re-validates argv/shell/body accordingly).
type UpdateScriptParams struct {
	OrgID       *int64
	Name        string
	Description string
	Platform    string
	Kind        string
	Argv        []string
	Shell       string
	Body        string
	TimeoutS    int
}

// UpdateScript replaces a script's mutable fields. ErrNotFound when the id does
// not exist.
func (s *Store) UpdateScript(ctx context.Context, id int64, p UpdateScriptParams) (Script, error) {
	argvJSON, shellArg, bodyArg, err := scriptArgs(p.Kind, p.Argv, p.Shell, p.Body)
	if err != nil {
		return Script{}, err
	}
	timeout := p.TimeoutS
	if timeout <= 0 {
		timeout = 300
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE scripts SET org_id = ?, name = ?, description = ?, platform = ?, kind = ?,
		        argv_json = ?, shell = ?, body = ?, timeout_s = ?
		  WHERE id = ?`,
		ptrInt64Arg(p.OrgID), p.Name, p.Description, p.Platform, p.Kind, argvJSON, shellArg, bodyArg, timeout, id)
	if err != nil {
		return Script{}, fmt.Errorf("store: update script: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return Script{}, fmt.Errorf("store: update script: %w", err)
	}
	if n == 0 {
		return Script{}, ErrNotFound
	}
	return s.GetScript(ctx, id)
}

// DeleteScript removes a library script. A job that used it keeps its rows
// (script_id ON DELETE SET NULL); an automation that referenced it is deleted
// (script_id ON DELETE CASCADE). ErrNotFound when the id does not exist.
func (s *Store) DeleteScript(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "scripts", id, "delete script")
}

// GetScript returns one script. ErrNotFound when the id does not exist.
func (s *Store) GetScript(ctx context.Context, id int64) (Script, error) {
	sc, err := scanScript(s.db.QueryRowContext(ctx, scriptSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Script{}, ErrNotFound
	}
	if err != nil {
		return Script{}, fmt.Errorf("store: get script: %w", err)
	}
	return sc, nil
}

// ListScripts returns scripts (newest first), restricted to a caller's visible
// org set (RS2): nil = every script (including global org_id NULL rows), a
// non-nil slice restricts to those org ids (which excludes global scripts, so a
// scoped user does not see them), and a non-nil empty slice matches nothing.
func (s *Store) ListScripts(ctx context.Context, visible []int64) ([]Script, error) {
	q := scriptSelect
	var args []any
	if clause, clauseArgs := orgInClause(`org_id`, visible); clause != "" {
		q += ` WHERE ` + clause
		args = clauseArgs
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list scripts: %w", err)
	}
	defer rows.Close()
	out := []Script{}
	for rows.Next() {
		sc, err := scanScript(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list scripts: %w", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list scripts: %w", err)
	}
	return out, nil
}
