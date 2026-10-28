package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// addWT makes a real git worktree at path on a new branch cut from main
// (or detached when branch is empty).
func (h *harness) addWT(path, branch string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	args := []string{"worktree", "add", "-q", "--detach", path, "main"}
	if branch != "" {
		args = []string{"worktree", "add", "-q", "-b", branch, path, "main"}
	}
	h.gitIn(h.repo, args...)
}

// pruneFixture is the repo state the prune tests share: one removable worktree
// (orch/a, with an ignored node_modules) and one of every kind that stays.
func pruneFixture(h *harness) {
	h.t.Helper()
	os.WriteFile(filepath.Join(h.repo, ".gitignore"), []byte("node_modules/\n"), 0o644)
	h.gitIn(h.repo, "add", ".gitignore")
	h.gitIn(h.repo, "commit", "-q", "-m", "ignore")
	h.addWT(h.wt("a"), "orch/a")
	os.MkdirAll(filepath.Join(h.wt("a"), "node_modules", "pkg"), 0o755)
	os.WriteFile(filepath.Join(h.wt("a"), "node_modules", "pkg", "x.js"), []byte("x"), 0o644)
	h.addWT(h.wt("b"), "orch/b")
	os.WriteFile(filepath.Join(h.wt("b"), "wip.go"), []byte("x"), 0o644)
	h.addWT(h.wt("c"), "orch/c")
	os.WriteFile(filepath.Join(h.wt("c"), "done.go"), []byte("x"), 0o644)
	h.gitIn(h.wt("c"), "add", ".")
	h.gitIn(h.wt("c"), "commit", "-q", "-m", "work")
	h.addWT(h.wt("d"), "")
	h.addWT(h.wt("x"), "feat/x")
	h.addWT(h.wt("y"), "orch/int-y")
	h.addWT(filepath.Join(h.tmp, "elsewhere", "k"), "orch/k")
}

func lineWith(out string, parts ...string) string {
	for _, l := range strings.Split(out, "\n") {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return l
		}
	}
	return ""
}

func TestPruneDryRunListsAndKeepsEverything(t *testing.T) {
	h := setup(t)
	pruneFixture(h)
	out := h.must("prune")
	if lineWith(out, "would remove "+h.wt("a"), "orch/a") == "" {
		t.Fatalf("orch/a not listed as removable:\n%s", out)
	}
	for _, c := range []struct{ path, why string }{
		{h.wt("b"), "uncommitted changes"},
		{h.wt("c"), "branch not merged into main"},
		{h.wt("d"), "detached"},
		{h.wt("x"), "not an orch branch"},
		{h.wt("y"), "integration branch"},
		{filepath.Join(h.tmp, "elsewhere", "k"), "outside the orch worktree root"},
	} {
		if lineWith(out, "kept "+c.path, c.why) == "" {
			t.Errorf("want %q kept: %s\n%s", c.path, c.why, out)
		}
	}
	wantContains(t, out, "prune: 1 would be removed, 6 kept", "run orch prune --apply to remove these")
	if strings.Contains(out, "removed "+h.wt("a")) {
		t.Fatal("dry run claims a removal")
	}
	for _, p := range []string{h.wt("a"), filepath.Join(h.wt("a"), "node_modules", "pkg", "x.js")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry run removed %s", p)
		}
	}
	if _, err := git(h.repo, "show-ref", "--verify", "--quiet", "refs/heads/orch/a"); err != nil {
		t.Fatal("dry run deleted the branch")
	}
}

func TestPruneApplyRemovesOnlyTheSafeWorktree(t *testing.T) {
	h := setup(t)
	pruneFixture(h)
	out := h.must("prune", "--apply")
	wantContains(t, out, "removed "+h.wt("a"), "prune: 1 removed, 6 kept")
	if _, err := os.Stat(h.wt("a")); err == nil {
		t.Fatal("orch/a directory (with node_modules) still there")
	}
	if _, err := git(h.repo, "show-ref", "--verify", "--quiet", "refs/heads/orch/a"); err == nil {
		t.Fatal("branch orch/a still there")
	}
	for _, p := range []string{h.wt("b"), h.wt("c"), h.wt("d"), h.wt("x"), h.wt("y"), filepath.Join(h.tmp, "elsewhere", "k")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("apply removed %s", p)
		}
	}
	for _, b := range []string{"orch/b", "orch/c", "feat/x", "orch/int-y", "orch/k"} {
		if _, err := git(h.repo, "show-ref", "--verify", "--quiet", "refs/heads/"+b); err != nil {
			t.Errorf("apply deleted branch %s", b)
		}
	}
}

func TestPruneKeepsWorktreeOfOpenLead(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1") // a real worktree on orch/t1, clean and merged
	out := h.must("prune", "--apply")
	if lineWith(out, "kept "+h.wt("t1"), "open lead") == "" {
		t.Fatalf("open lead's worktree not kept:\n%s", out)
	}
	if _, err := os.Stat(h.wt("t1")); err != nil {
		t.Fatalf("worktree of an open lead removed: %v", err)
	}
}

func TestPruneKeepsWorktreeOpenInHerdr(t *testing.T) {
	h := setup(t)
	h.addWT(h.wt("h"), "orch/h")
	h.editFake(func(st *fakeState) { st.Workspaces = map[string]string{"wsx": h.wt("h")} })
	out := h.must("prune", "--apply")
	if lineWith(out, "kept "+h.wt("h"), "open in Herdr workspace wsx") == "" {
		t.Fatalf("worktree open in Herdr not kept:\n%s", out)
	}
	if _, err := os.Stat(h.wt("h")); err != nil {
		t.Fatal("worktree open in Herdr removed")
	}
}

func TestPruneKeepsEverythingWhenHerdrStateIsUnknown(t *testing.T) {
	cases := map[string]func(h *harness, t *testing.T){
		"herdr down":  func(h *harness, t *testing.T) { h.editFake(func(st *fakeState) { st.Down = true }) },
		"not managed": func(h *harness, t *testing.T) { t.Setenv("HERDR_ENV", "") },
	}
	for name, prep := range cases {
		t.Run(name, func(t *testing.T) {
			h := setup(t)
			pruneFixture(h)
			prep(h, t)
			out := h.must("prune", "--apply")
			if lineWith(out, "kept "+h.wt("a"), "Herdr state unknown") == "" {
				t.Fatalf("orch/a not kept for unknown Herdr state:\n%s", out)
			}
			wantContains(t, out, "prune: 0 removed, 7 kept")
			if _, err := os.Stat(h.wt("a")); err != nil {
				t.Fatal("worktree removed while Herdr state was unknown")
			}
		})
	}
}

func TestPruneRefusesWithoutABase(t *testing.T) {
	h := setup(t)
	h.addWT(h.wt("a"), "orch/a")
	h.gitIn(h.repo, "checkout", "-q", "--detach")
	code, out := h.run("prune", "--apply")
	if code != 1 || !strings.Contains(out, "cannot tell the base branch") {
		t.Fatalf("prune = %d %q", code, out)
	}
	if _, err := os.Stat(h.wt("a")); err != nil {
		t.Fatal("worktree removed without a base")
	}
}

func TestDoctorReportsRemovableWorktreesOnlyWhenThereAreSome(t *testing.T) {
	h := setup(t)
	t.Setenv("HOME", t.TempDir())
	if out := h.must("doctor"); strings.Contains(out, "worktrees") {
		t.Fatalf("doctor reported worktrees with none to remove:\n%s", out)
	}
	h.addWT(h.wt("a"), "orch/a")
	_, out := h.run("doctor")
	wantContains(t, out, "warn,worktrees,", "1 merged, clean orch worktrees can be removed", "run: orch prune")
	h.editFake(func(st *fakeState) { st.Down = true })
	if _, out := h.run("doctor"); strings.Contains(out, "can be removed") {
		t.Fatalf("doctor reported a removal while Herdr state was unknown:\n%s", out)
	}
}

// commitAt commits a file in dir with both git dates set to when.
func commitAt(h *harness, dir, file string, when time.Time) {
	h.t.Helper()
	os.WriteFile(filepath.Join(dir, file), []byte(file), 0o644)
	d := when.Format(time.RFC3339)
	for _, args := range [][]string{{"add", file}, {"commit", "-q", "-m", file}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
		if out, err := cmd.CombinedOutput(); err != nil {
			h.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// staleFixture adds orch/s with one unmerged commit made ageDays ago and, when
// baseMoved, a newer commit on main.
func staleFixture(h *harness, ageDays int, baseMoved bool) {
	h.t.Helper()
	day := 24 * time.Hour
	h.addWT(h.wt("s"), "orch/s")
	commitAt(h, h.wt("s"), "s.go", h.now.Add(-time.Duration(ageDays)*day))
	if baseMoved {
		commitAt(h, h.repo, "main.go", h.now.Add(-day))
	}
}

func branchExists(h *harness, b string) bool {
	_, err := git(h.repo, "show-ref", "--verify", "--quiet", "refs/heads/"+b)
	return err == nil
}

func TestPruneStaleRemovesIdleUnmergedWorktreeButKeepsItsBranch(t *testing.T) {
	h := setup(t)
	staleFixture(h, 60, true)
	if out := h.must("prune", "--apply"); lineWith(out, "kept "+h.wt("s"), "branch not merged into main") == "" || !h.exists(h.wt("s")) {
		t.Fatalf("without --stale the unmerged worktree must stay:\n%s", out)
	}
	out := h.must("prune", "--stale")
	if !h.exists(h.wt("s")) {
		t.Fatal("dry run with --stale removed the worktree")
	}
	wantContains(t, out, "would remove "+h.wt("s")+" (stale, branch kept: orch/s, 1 unmerged commits, last commit 60 days ago)", "run orch prune --apply")
	out = h.must("prune", "--stale", "--apply")
	wantContains(t, out, "removed "+h.wt("s")+" (stale, branch kept: orch/s")
	if h.exists(h.wt("s")) {
		t.Fatal("stale worktree still there")
	}
	if !branchExists(h, "orch/s") {
		t.Fatal("stale removal deleted the branch")
	}
}

func (h *harness) exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPruneStaleKeepsWhatIsNotIdleOrNotLeftBehind(t *testing.T) {
	cases := map[string]struct {
		age   int
		moved bool
		dirty bool
		why   string
	}{
		"too young":          {age: 10, moved: true, why: "branch not merged into main"},
		"base has not moved": {age: 60, moved: false, why: "branch not merged into main"},
		"dirty":              {age: 60, moved: true, dirty: true, why: "uncommitted changes"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := setup(t)
			staleFixture(h, c.age, c.moved)
			if c.dirty {
				os.WriteFile(filepath.Join(h.wt("s"), "wip.go"), []byte("x"), 0o644)
			}
			out := h.must("prune", "--stale", "--apply")
			if lineWith(out, "kept "+h.wt("s"), c.why) == "" || !h.exists(h.wt("s")) {
				t.Fatalf("want kept: %s\n%s", c.why, out)
			}
		})
	}
}

func TestPruneStaleParsesDaysAfterSpaceOrEquals(t *testing.T) {
	for _, args := range [][]string{{"--stale", "45"}, {"--stale=45"}} {
		h := setup(t)
		staleFixture(h, 40, true) // idle under 45 days: kept
		if out := h.must(append([]string{"prune"}, args...)...); lineWith(out, "kept "+h.wt("s")) == "" {
			t.Fatalf("%v: a 40 day old branch went with a 45 day limit:\n%s", args, out)
		}
		h = setup(t)
		staleFixture(h, 50, true) // idle over 45 days: goes
		if out := h.must(append([]string{"prune"}, args...)...); lineWith(out, "would remove "+h.wt("s"), "stale") == "" {
			t.Fatalf("%v: a 50 day old branch stayed with a 45 day limit:\n%s", args, out)
		}
	}
	h := setup(t)
	staleFixture(h, 40, true) // bare --stale means 30 days
	if out := h.must("prune", "--stale"); lineWith(out, "would remove "+h.wt("s"), "stale") == "" {
		t.Fatalf("bare --stale did not default to 30 days:\n%s", out)
	}
}
