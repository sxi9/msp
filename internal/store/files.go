package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The server-side file library (SD1). Rows hold the metadata; the bytes live on
// disk under MSP_FILES_DIR/<id>. org_id NULL is a global artifact usable for any
// client, a non-null org_id scopes it to one client (RS2 visibility applies).
// The caller (the server handlers) owns disk I/O, the org-visibility decision on
// a single file and the agent-download org check; the store only persists and
// reads the rows, carrying org_id so those checks can be made.

const fileSelect = `
SELECT f.id, f.org_id, f.name, f.size, f.sha256, f.content_type, f.created_at, f.created_by,
       COALESCE(u.email, '')
  FROM files f LEFT JOIN users u ON u.id = f.created_by`

func scanFile(row interface{ Scan(...any) error }) (File, error) {
	var (
		f         File
		orgID     sql.NullInt64
		createdBy sql.NullInt64
		created   string
	)
	if err := row.Scan(&f.ID, &orgID, &f.Name, &f.Size, &f.SHA256, &f.ContentType, &created, &createdBy, &f.CreatedByEmail); err != nil {
		return File{}, err
	}
	f.OrgID = nullInt64(orgID)
	f.CreatedBy = nullInt64(createdBy)
	var err error
	if f.CreatedAt, err = parseTime(created); err != nil {
		return File{}, err
	}
	return f, nil
}

// CreateFile inserts a file-library row and returns it. The caller has already
// placed the bytes on disk and computed Size and SHA256.
func (s *Store) CreateFile(ctx context.Context, p CreateFileParams) (File, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO files (org_id, name, size, sha256, content_type, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ptrInt64Arg(p.OrgID), p.Name, p.Size, p.SHA256, p.ContentType, fmtTime(s.Now()), ptrInt64Arg(p.CreatedBy))
	if err != nil {
		return File{}, fmt.Errorf("store: create file: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return File{}, fmt.Errorf("store: create file: %w", err)
	}
	return s.GetFile(ctx, id)
}

// GetFile returns one file-library row. ErrNotFound when it does not exist.
func (s *Store) GetFile(ctx context.Context, id int64) (File, error) {
	f, err := scanFile(s.db.QueryRowContext(ctx, fileSelect+` WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("store: get file: %w", err)
	}
	return f, nil
}

// ListFiles returns file-library rows newest first, org-scoped (RS2). A non-nil
// VisibleOrgs restricts to files whose org_id is in the set via org_id IN (…),
// which excludes global (NULL) files, so a scoped user sees only its own orgs'
// files; a nil VisibleOrgs (all-orgs caller) returns every file, global ones
// included. OrgID, when set, further restricts to one client org.
func (s *Store) ListFiles(ctx context.Context, filter FileFilter) ([]File, error) {
	q := fileSelect
	var (
		where []string
		args  []any
	)
	if clause, clauseArgs := orgInClause(`f.org_id`, filter.VisibleOrgs); clause != "" {
		where = append(where, clause)
		args = append(args, clauseArgs...)
	}
	if filter.OrgID != nil {
		where = append(where, `f.org_id = ?`)
		args = append(args, *filter.OrgID)
	}
	for i, c := range where {
		if i == 0 {
			q += ` WHERE ` + c
		} else {
			q += ` AND ` + c
		}
	}
	q += ` ORDER BY f.id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list files: %w", err)
	}
	defer rows.Close()
	files := []File{}
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list files: %w", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list files: %w", err)
	}
	return files, nil
}

// DeleteFile removes a file-library row. ErrNotFound when it does not exist.
// The caller removes the bytes on disk (the store never touches the filesystem).
func (s *Store) DeleteFile(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete file: %w", err)
	}
	n, err := rowsAffected(res)
	if err != nil {
		return fmt.Errorf("store: delete file: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
