package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Per-technician org scoping (RS2). A user's visible org set is: all orgs when
// role == admin or all_orgs == 1; otherwise exactly the user_orgs rows. The
// helpers here are the single definition of that rule; every org-scoped read
// and write in the server resolves the caller's set through them.

// GetUserOrgs returns a user's org assignment: allOrgs is true when the user
// sees every org (admin, or all_orgs == 1), in which case orgIDs is empty;
// otherwise orgIDs is exactly the user_orgs rows (ascending, possibly empty).
// ErrNotFound when the user does not exist.
func (s *Store) GetUserOrgs(ctx context.Context, userID int64) (allOrgs bool, orgIDs []int64, err error) {
	var (
		role string
		ao   int
	)
	err = s.db.QueryRowContext(ctx, `SELECT role, all_orgs FROM users WHERE id = ?`, userID).Scan(&role, &ao)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, ErrNotFound
	}
	if err != nil {
		return false, nil, fmt.Errorf("store: get user orgs: %w", err)
	}
	orgIDs = []int64{}
	if role == "admin" || ao != 0 {
		return true, orgIDs, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT org_id FROM user_orgs WHERE user_id = ? ORDER BY org_id`, userID)
	if err != nil {
		return false, nil, fmt.Errorf("store: get user orgs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return false, nil, fmt.Errorf("store: get user orgs: %w", err)
		}
		orgIDs = append(orgIDs, id)
	}
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("store: get user orgs: %w", err)
	}
	return false, orgIDs, nil
}

// VisibleOrgIDs resolves a user's visible org set. all == true means
// unrestricted (every org) and ids is nil; otherwise ids is the exact set the
// user may see (a non-nil, possibly empty slice: empty means the user sees no
// org yet). Pass ids straight into the list-query filters (nil = all).
func (s *Store) VisibleOrgIDs(ctx context.Context, userID int64) (ids []int64, all bool, err error) {
	allOrgs, orgIDs, err := s.GetUserOrgs(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	if allOrgs {
		return nil, true, nil
	}
	return orgIDs, false, nil
}

// UserCanSeeOrg reports whether a user may see one org. ErrNotFound when the
// user does not exist.
func (s *Store) UserCanSeeOrg(ctx context.Context, userID, orgID int64) (bool, error) {
	ids, all, err := s.VisibleOrgIDs(ctx, userID)
	if err != nil {
		return false, err
	}
	if all {
		return true, nil
	}
	for _, id := range ids {
		if id == orgID {
			return true, nil
		}
	}
	return false, nil
}

// SetUserOrgs replaces a user's org assignment in one transaction. allOrgs
// true clears the user_orgs rows (full visibility); allOrgs false stores
// exactly orgIDs (deduplicated). An admin is always all-orgs: for an admin
// target allOrgs is forced to true and orgIDs ignored. Every org id must
// exist (ErrNotFound otherwise); ErrNotFound also when the user does not
// exist. A promotion to admin or a later all_orgs toggle is reflected at once
// by VisibleOrgIDs, so a demoted admin keeps all_orgs=1 until scoped here.
func (s *Store) SetUserOrgs(ctx context.Context, userID int64, allOrgs bool, orgIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set user orgs: %w", err)
	}
	defer tx.Rollback()

	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: set user orgs: %w", err)
	}
	if role == "admin" {
		allOrgs = true
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET all_orgs = ? WHERE id = ?`, boolInt(allOrgs), userID); err != nil {
		return fmt.Errorf("store: set user orgs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_orgs WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: set user orgs: %w", err)
	}
	if !allOrgs {
		seen := map[int64]bool{}
		for _, oid := range orgIDs {
			if seen[oid] {
				continue
			}
			seen[oid] = true
			// INSERT ... SELECT guards against a non-existent org: a bad id
			// inserts no row, which we report as ErrNotFound (the org is gone).
			res, err := tx.ExecContext(ctx,
				`INSERT INTO user_orgs (user_id, org_id) SELECT ?, id FROM orgs WHERE id = ?`, userID, oid)
			if err != nil {
				return fmt.Errorf("store: set user orgs: %w", err)
			}
			n, err := rowsAffected(res)
			if err != nil {
				return fmt.Errorf("store: set user orgs: %w", err)
			}
			if n == 0 {
				return ErrNotFound
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set user orgs: %w", err)
	}
	return nil
}

// orgInClause builds a SQL fragment restricting col to the visible org set and
// its bind args. visible == nil means no restriction (empty fragment). A
// non-nil but empty set restricts to nothing ("0 = 1"), so a scoped user with
// no orgs sees no rows. The fragment carries no leading/trailing boolean
// operators; the caller joins it with AND/WHERE.
func orgInClause(col string, visible []int64) (string, []any) {
	if visible == nil {
		return "", nil
	}
	if len(visible) == 0 {
		return "0 = 1", nil
	}
	ph := make([]string, len(visible))
	args := make([]any, len(visible))
	for i, id := range visible {
		ph[i] = "?"
		args[i] = id
	}
	return col + " IN (" + strings.Join(ph, ", ") + ")", args
}
