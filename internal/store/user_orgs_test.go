package store

import (
	"context"
	"errors"
	"testing"
)

// visibleEqual reports whether VisibleOrgIDs returned the expected set.
func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestUserOrgsScoping(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, admin, _, _ := fixture(t, s) // admin + orgA "Acme Dental"
	orgB, err := s.CreateOrg(ctx, "Beta Clinic")
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.CreateUser(ctx, "op@example.com", "$2a$12$hash", "operator")
	if err != nil {
		t.Fatal(err)
	}

	// Fresh users (admin and operator) default to all-orgs.
	for _, u := range []User{admin, op} {
		all, ids, err := s.GetUserOrgs(ctx, u.ID)
		if err != nil || !all || len(ids) != 0 {
			t.Fatalf("fresh %s GetUserOrgs = all %v ids %v err %v", u.Email, all, ids, err)
		}
		vids, vall, err := s.VisibleOrgIDs(ctx, u.ID)
		if err != nil || !vall || vids != nil {
			t.Fatalf("fresh %s VisibleOrgIDs = %v %v %v", u.Email, vids, vall, err)
		}
		for _, o := range []int64{orgA.ID, orgB.ID, 99999} {
			if can, err := s.UserCanSeeOrg(ctx, u.ID, o); err != nil || !can {
				t.Fatalf("fresh %s UserCanSeeOrg(%d) = %v %v", u.Email, o, can, err)
			}
		}
	}

	// Scope the operator to org A only.
	if err := s.SetUserOrgs(ctx, op.ID, false, []int64{orgA.ID}); err != nil {
		t.Fatal(err)
	}
	all, ids, err := s.GetUserOrgs(ctx, op.ID)
	if err != nil || all || !sameIDs(ids, []int64{orgA.ID}) {
		t.Fatalf("scoped op GetUserOrgs = all %v ids %v err %v", all, ids, err)
	}
	vids, vall, err := s.VisibleOrgIDs(ctx, op.ID)
	if err != nil || vall || !sameIDs(vids, []int64{orgA.ID}) {
		t.Fatalf("scoped op VisibleOrgIDs = %v %v %v", vids, vall, err)
	}
	if can, _ := s.UserCanSeeOrg(ctx, op.ID, orgA.ID); !can {
		t.Fatal("scoped op cannot see org A")
	}
	if can, _ := s.UserCanSeeOrg(ctx, op.ID, orgB.ID); can {
		t.Fatal("scoped op can see org B")
	}

	// A non-existent org id is rejected and leaves the assignment unchanged.
	if err := s.SetUserOrgs(ctx, op.ID, false, []int64{orgB.ID, 99999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetUserOrgs bad org = %v, want ErrNotFound", err)
	}
	if _, ids, _ := s.GetUserOrgs(ctx, op.ID); !sameIDs(ids, []int64{orgA.ID}) {
		t.Fatalf("assignment changed after a failed SetUserOrgs: %v", ids)
	}

	// Duplicate ids are deduplicated; granting both orgs works.
	if err := s.SetUserOrgs(ctx, op.ID, false, []int64{orgB.ID, orgA.ID, orgB.ID}); err != nil {
		t.Fatal(err)
	}
	if _, ids, _ := s.GetUserOrgs(ctx, op.ID); !sameIDs(ids, []int64{orgA.ID, orgB.ID}) {
		t.Fatalf("both orgs = %v", ids)
	}

	// Empty scoped set: the user sees nothing.
	if err := s.SetUserOrgs(ctx, op.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	vids, vall, err = s.VisibleOrgIDs(ctx, op.ID)
	if err != nil || vall || vids == nil || len(vids) != 0 {
		t.Fatalf("empty scoped VisibleOrgIDs = %v %v %v (want non-nil empty, all=false)", vids, vall, err)
	}
	if can, _ := s.UserCanSeeOrg(ctx, op.ID, orgA.ID); can {
		t.Fatal("empty scoped op can still see org A")
	}

	// Back to all-orgs clears the rows.
	if err := s.SetUserOrgs(ctx, op.ID, true, []int64{orgA.ID}); err != nil {
		t.Fatal(err)
	}
	if all, ids, _ := s.GetUserOrgs(ctx, op.ID); !all || len(ids) != 0 {
		t.Fatalf("back to all-orgs = all %v ids %v", all, ids)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_orgs WHERE user_id = ?`, op.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("user_orgs rows after all-orgs = %d, %v", rows, err)
	}

	// An admin is always all-orgs: scoping one is forced back to all.
	if err := s.SetUserOrgs(ctx, admin.ID, false, []int64{orgA.ID}); err != nil {
		t.Fatal(err)
	}
	if all, ids, _ := s.GetUserOrgs(ctx, admin.ID); !all || len(ids) != 0 {
		t.Fatalf("scoped admin = all %v ids %v (admins cannot be scoped)", all, ids)
	}

	// A scoped user later promoted to admin becomes all-orgs at once.
	if err := s.SetUserOrgs(ctx, op.ID, false, []int64{orgA.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET role = 'admin' WHERE id = ?`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, all, _ := s.VisibleOrgIDs(ctx, op.ID); !all {
		t.Fatal("promoted admin is not all-orgs")
	}

	// Missing user.
	if err := s.SetUserOrgs(ctx, 99999, false, []int64{orgA.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetUserOrgs missing user = %v", err)
	}
	if _, _, err := s.GetUserOrgs(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUserOrgs missing user = %v", err)
	}
	if _, err := s.UserCanSeeOrg(ctx, 99999, orgA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserCanSeeOrg missing user = %v", err)
	}
}

func TestCreateUserScoped(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, _, _, _ := fixture(t, s) // admin + orgA "Acme Dental"
	orgB, err := s.CreateOrg(ctx, "Beta Clinic")
	if err != nil {
		t.Fatal(err)
	}

	// A scoped create commits the account and exactly its org rows in one tx.
	op, err := s.CreateUserScoped(ctx, "Op@Example.com", "$2a$12$hash", "operator", false, []int64{orgB.ID, orgA.ID, orgB.ID})
	if err != nil {
		t.Fatalf("CreateUserScoped scoped: %v", err)
	}
	if op.Email != "op@example.com" {
		t.Fatalf("email not normalised: %q", op.Email)
	}
	all, ids, err := s.GetUserOrgs(ctx, op.ID)
	if err != nil || all || !sameIDs(ids, []int64{orgA.ID, orgB.ID}) {
		t.Fatalf("scoped create GetUserOrgs = all %v ids %v err %v", all, ids, err)
	}

	// A non-existent org id rolls the whole insert back: no account is left
	// behind (the regression — the old non-atomic path committed the user with
	// the all_orgs=1 column default, i.e. broader visibility than requested).
	if _, err := s.CreateUserScoped(ctx, "bad@example.com", "$2a$12$hash", "viewer", false, []int64{orgA.ID, 99999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateUserScoped bad org = %v, want ErrNotFound", err)
	}
	if _, err := s.GetUserByEmail(ctx, "bad@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad-org create left an account behind: %v", err)
	}

	// An empty scoped set creates a user who sees nothing yet.
	none, err := s.CreateUserScoped(ctx, "none@example.com", "$2a$12$hash", "viewer", false, nil)
	if err != nil {
		t.Fatalf("CreateUserScoped empty: %v", err)
	}
	vids, vall, err := s.VisibleOrgIDs(ctx, none.ID)
	if err != nil || vall || vids == nil || len(vids) != 0 {
		t.Fatalf("empty scoped VisibleOrgIDs = %v %v %v", vids, vall, err)
	}

	// The unscoped path (allOrgs true) keeps full visibility and ignores orgIDs.
	full, err := s.CreateUserScoped(ctx, "full@example.com", "$2a$12$hash", "operator", true, []int64{orgA.ID})
	if err != nil {
		t.Fatalf("CreateUserScoped all-orgs: %v", err)
	}
	if a, i, _ := s.GetUserOrgs(ctx, full.ID); !a || len(i) != 0 {
		t.Fatalf("all-orgs create = all %v ids %v", a, i)
	}

	// An admin is always all-orgs: the scoping fields are ignored even with
	// allOrgs false and an org list.
	adm, err := s.CreateUserScoped(ctx, "admin2@example.com", "$2a$12$hash", "admin", false, []int64{orgA.ID})
	if err != nil {
		t.Fatalf("CreateUserScoped admin: %v", err)
	}
	if a, i, _ := s.GetUserOrgs(ctx, adm.ID); !a || len(i) != 0 {
		t.Fatalf("scoped admin create = all %v ids %v (admins cannot be scoped)", a, i)
	}

	// A duplicate email is ErrConflict and commits nothing extra.
	if _, err := s.CreateUserScoped(ctx, "op@example.com", "$2a$12$hash", "operator", false, []int64{orgA.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateUserScoped duplicate = %v, want ErrConflict", err)
	}
}

func TestUserOrgsCascade(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, _, _, _ := fixture(t, s)
	orgB, _ := s.CreateOrg(ctx, "Beta Clinic")
	op, err := s.CreateUser(ctx, "op@example.com", "$2a$12$hash", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserOrgs(ctx, op.ID, false, []int64{orgA.ID, orgB.ID}); err != nil {
		t.Fatal(err)
	}

	// Deleting an org drops its user_orgs rows (ON DELETE CASCADE), leaving the
	// user scoped to the orgs that remain.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM orgs WHERE id = ?`, orgB.ID); err != nil {
		t.Fatal(err)
	}
	if _, ids, _ := s.GetUserOrgs(ctx, op.ID); !sameIDs(ids, []int64{orgA.ID}) {
		t.Fatalf("after org delete ids = %v", ids)
	}

	// Disabling (soft) keeps the rows; a hard delete of the user cascades.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, op.ID); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_orgs WHERE user_id = ?`, op.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("user_orgs rows after user delete = %d, %v", rows, err)
	}
}
