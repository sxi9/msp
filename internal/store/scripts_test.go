package store

import (
	"context"
	"errors"
	"testing"
)

func TestCreateScriptExecAndScript(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, _, _ := fixture(t, s)

	exec, err := s.CreateScript(ctx, CreateScriptParams{
		OrgID: &org.ID, Name: "uptime", Description: "show uptime", Platform: ScriptPlatformLinux,
		Kind: "exec", Argv: []string{"uptime", "-p"}, TimeoutS: 0, CreatedBy: &u.ID,
	})
	if err != nil {
		t.Fatalf("CreateScript exec: %v", err)
	}
	if exec.Kind != "exec" || len(exec.Argv) != 2 || exec.Argv[0] != "uptime" {
		t.Fatalf("exec script = %+v", exec)
	}
	if exec.Body != "" || exec.Shell != "" {
		t.Fatalf("exec script should have no shell/body: %+v", exec)
	}
	if exec.TimeoutS != 300 {
		t.Fatalf("default timeout = %d, want 300", exec.TimeoutS)
	}
	if exec.OrgID == nil || *exec.OrgID != org.ID {
		t.Fatalf("org = %v", exec.OrgID)
	}

	sc, err := s.CreateScript(ctx, CreateScriptParams{
		Name: "cleanup", Platform: ScriptPlatformAny, Kind: "script",
		Shell: "/bin/sh", Body: "rm -rf /tmp/cache", TimeoutS: 120,
	})
	if err != nil {
		t.Fatalf("CreateScript script: %v", err)
	}
	if sc.Kind != "script" || sc.Shell != "/bin/sh" || sc.Body != "rm -rf /tmp/cache" {
		t.Fatalf("script = %+v", sc)
	}
	// Argv is never nil for a script-kind row.
	if sc.Argv == nil || len(sc.Argv) != 0 {
		t.Fatalf("script argv should be empty non-nil: %#v", sc.Argv)
	}
	if sc.OrgID != nil {
		t.Fatalf("global script org should be nil: %v", sc.OrgID)
	}
}

func TestCreateScriptBadKind(t *testing.T) {
	s, _ := openTest(t)
	_, err := s.CreateScript(context.Background(), CreateScriptParams{
		Name: "x", Platform: "any", Kind: "lock",
	})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestGetUpdateDeleteScript(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	org, u, _, _ := fixture(t, s)

	sc, err := s.CreateScript(ctx, CreateScriptParams{
		OrgID: &org.ID, Name: "one", Platform: "linux", Kind: "exec", Argv: []string{"ls"}, CreatedBy: &u.ID,
	})
	if err != nil {
		t.Fatalf("CreateScript: %v", err)
	}

	got, err := s.GetScript(ctx, sc.ID)
	if err != nil || got.Name != "one" {
		t.Fatalf("GetScript = %+v, %v", got, err)
	}

	// Change kind exec -> script.
	upd, err := s.UpdateScript(ctx, sc.ID, UpdateScriptParams{
		OrgID: &org.ID, Name: "two", Platform: "any", Kind: "script", Shell: "", Body: "echo hi", TimeoutS: 45,
	})
	if err != nil {
		t.Fatalf("UpdateScript: %v", err)
	}
	if upd.Name != "two" || upd.Kind != "script" || upd.Body != "echo hi" || upd.TimeoutS != 45 {
		t.Fatalf("updated = %+v", upd)
	}
	if len(upd.Argv) != 0 {
		t.Fatalf("argv should clear when switching to script: %#v", upd.Argv)
	}

	if _, err := s.UpdateScript(ctx, 9999, UpdateScriptParams{Name: "n", Platform: "any", Kind: "exec"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateScript missing = %v, want ErrNotFound", err)
	}

	if err := s.DeleteScript(ctx, sc.ID); err != nil {
		t.Fatalf("DeleteScript: %v", err)
	}
	if _, err := s.GetScript(ctx, sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetScript after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteScript(ctx, sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteScript again = %v, want ErrNotFound", err)
	}
}

func TestListScriptsRS2(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	orgA, err := s.CreateOrg(ctx, "A")
	if err != nil {
		t.Fatalf("CreateOrg A: %v", err)
	}
	orgB, err := s.CreateOrg(ctx, "B")
	if err != nil {
		t.Fatalf("CreateOrg B: %v", err)
	}
	mustScript(t, s, &orgA.ID, "a1")
	mustScript(t, s, &orgB.ID, "b1")
	mustScript(t, s, nil, "global")

	// All-orgs caller (visible nil) sees every script, global included.
	all, err := s.ListScripts(ctx, nil)
	if err != nil {
		t.Fatalf("ListScripts nil: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all scripts = %d, want 3", len(all))
	}

	// Scoped to A: only A's script, not B's and not the global one.
	onlyA, err := s.ListScripts(ctx, []int64{orgA.ID})
	if err != nil {
		t.Fatalf("ListScripts A: %v", err)
	}
	if len(onlyA) != 1 || onlyA[0].Name != "a1" {
		t.Fatalf("scoped A scripts = %+v", onlyA)
	}

	// Empty visible slice matches nothing.
	none, err := s.ListScripts(ctx, []int64{})
	if err != nil {
		t.Fatalf("ListScripts empty: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("empty visible scripts = %d, want 0", len(none))
	}
}

func mustScript(t *testing.T, s *Store, orgID *int64, name string) Script {
	t.Helper()
	sc, err := s.CreateScript(context.Background(), CreateScriptParams{
		OrgID: orgID, Name: name, Platform: "any", Kind: "exec", Argv: []string{"echo", name},
	})
	if err != nil {
		t.Fatalf("CreateScript %s: %v", name, err)
	}
	return sc
}
