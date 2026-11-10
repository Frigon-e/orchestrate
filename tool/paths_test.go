package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A repo's run root is the same for a process inside Orca and one in a plain
// shell, even when Herdr is configured with a state dir the plain shell would
// otherwise pick.
func TestRunRootSameInsideAndOutsideOrca(t *testing.T) {
	noBackendEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ORCH_HOME", "")
	herdrCfg := filepath.Join(home, "herdr.toml")
	mustWrite(t, herdrCfg, "[worktrees]\ndirectory = \""+filepath.Join(home, "herdr-wt")+"\"\n", 0o644)
	t.Setenv("ORCH_HERDR_CONFIG", herdrCfg)
	repo := filepath.Join(home, "src", "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	// Inside Orca: no state.dir, so the shared default.
	t.Setenv("ORCH_BACKEND", "orca")
	inside := runRoot(repo)
	want := filepath.Join(home, ".local", "state", "orchestrate", "myrepo")
	if inside != want {
		t.Fatalf("inside orca got %s, want %s", inside, want)
	}
	if err := claimRoot(inside, repo); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ORCH_BACKEND", "")
	if outside := runRoot(repo); outside != inside {
		t.Fatalf("plain shell got %s, orca used %s", outside, inside)
	}
}

// A plain shell follows whichever backend claimed the repo last, and Herdr's
// own root is unchanged while Herdr is active.
func TestRunRootOutsideFollowsLastClaim(t *testing.T) {
	noBackendEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ORCH_HOME", "")
	herdrCfg := filepath.Join(home, "herdr.toml")
	wt := filepath.Join(home, "herdr-wt")
	mustWrite(t, herdrCfg, "[worktrees]\ndirectory = \""+wt+"\"\n", 0o644)
	t.Setenv("ORCH_HERDR_CONFIG", herdrCfg)
	repo := filepath.Join(home, "src", "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ORCH_BACKEND", "herdr")
	herdrRoot := runRoot(repo)
	if want := filepath.Join(wt, "myrepo", ".orchestrate"); herdrRoot != want {
		t.Fatalf("herdr root changed: got %s, want %s", herdrRoot, want)
	}
	if err := claimRoot(herdrRoot, repo); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_BACKEND", "")
	if got := runRoot(repo); got != herdrRoot {
		t.Fatalf("plain shell after herdr got %s, want %s", got, herdrRoot)
	}

	// Orca claims later: the plain shell moves with it.
	t.Setenv("ORCH_BACKEND", "orca")
	orcaRoot := runRoot(repo)
	if err := claimRoot(orcaRoot, repo); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_BACKEND", "")
	if got := runRoot(repo); got != orcaRoot {
		t.Fatalf("plain shell after orca got %s, want %s", got, orcaRoot)
	}

	// A marker pointing at a root another repo owns is ignored.
	other := filepath.Join(home, "src2", "myrepo")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := runRoot(other); got == orcaRoot || got == herdrRoot {
		t.Fatalf("other repo got %s, a root it does not own", got)
	}
}
