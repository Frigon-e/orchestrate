package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func (h *harness) wt(id string) string { return filepath.Join(h.tmp, "state", "repo", "worktrees", id) }

func (h *harness) herdrCalls(prefix ...string) [][]string { return callsOf(h.fakeState(), prefix...) }

func TestShipSpawnMakesWorktreeAndStartsAgentInIt(t *testing.T) {
	h := setup(t)
	h.task("t1")
	sha, _ := git(h.repo, "rev-parse", "--short=12", "main")
	h.must("spawn", "t1")

	create := h.herdrCalls("worktree", "create")
	if len(create) != 1 {
		t.Fatalf("worktree create calls = %v", create)
	}
	for flag, want := range map[string]string{"--cwd": h.repo, "--branch": "orch/t1", "--base": sha, "--path": h.wt("t1")} {
		if got := flagValue(create[0], flag); got != want {
			t.Errorf("worktree create %s = %q, want %q", flag, got, want)
		}
	}
	if worktreeBranch(h.repo, h.wt("t1")) != "orch/t1" {
		t.Fatal("no worktree on orch/t1")
	}
	split := h.herdrCalls("pane", "split")
	if len(split) != 1 || split[0][2] != "wt1:p0" || flagValue(split[0], "--cwd") != h.wt("t1") || !slices.Contains(split[0], "ORCH_ROLE=lead") {
		t.Fatalf("pane split = %v", split)
	}
	l := h.lead("t1")
	if l.WorkspaceID != "wt1" || l.PaneID != "wt1:p1" {
		t.Fatalf("lead = %+v", l)
	}
	if _, ok := h.fakeState().Panes["wt1:p0"]; ok {
		t.Fatal("the workspace's root pane was left open next to the lead's")
	}
	if len(h.herdrCalls("agent", "start", "lead-t1", "--kind", "claude", "--pane", "wt1:p1")) != 1 {
		t.Fatalf("agent not started in the lead's pane: %v", h.fakeState().Calls)
	}
}

func TestUnmanagedBriefKeepsGitWorktreeAdd(t *testing.T) {
	h := setup(t)
	t.Setenv("HERDR_ENV", "")
	h.must("new", "t1", "--kind", "ship", "--harness", "claude", "--model", "sonnet", "--effort", "high")
	brief, _ := os.ReadFile(filepath.Join(h.dir("t1"), "task.md"))
	wantContains(t, string(brief), "worktree add -b <branch> "+h.wt("t1")+" ")
	if strings.Contains(string(brief), "orch already made it") {
		t.Fatal("unmanaged brief claims orch made the worktree")
	}
}

func TestScoutSpawnKeepsTheGridAndMakesNoWorktree(t *testing.T) {
	h := setup(t)
	h.task("s1", "--kind", "scout")
	h.must("spawn", "s1")
	if c := h.herdrCalls("worktree"); len(c) != 0 {
		t.Fatalf("scout touched worktrees: %v", c)
	}
	l := h.lead("s1")
	if l.WorkspaceID != "" || l.PaneID != "w1:p1" || len(h.herdrCalls("tab", "create")) != 1 {
		t.Fatalf("scout lead = %+v", l)
	}
}

func TestShipSpawnRefusesCollisionsAndFailedCreate(t *testing.T) {
	cases := map[string]struct {
		prep func(h *harness)
		want string
	}{
		"existing branch": {func(h *harness) { h.gitIn(h.repo, "branch", "orch/t1", "main") }, "branch orch/t1 already exists"},
		"existing path": {func(h *harness) {
			if err := os.MkdirAll(h.wt("t1"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "already exists"},
		"herdr create fails": {func(h *harness) { os.WriteFile(h.fake+".createfail", nil, 0o644) }, "cannot create a worktree"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := setup(t)
			h.task("t1")
			c.prep(h)
			code, out := h.run("spawn", "t1")
			if code != 1 || !strings.Contains(out, c.want) {
				t.Fatalf("spawn = %d %q, want a refusal with %q", code, out, c.want)
			}
			if l, _ := loadLead(h.dir("t1")); l != nil {
				t.Fatalf("a refused spawn recorded a lead: %+v", l)
			}
			if len(h.fakeState().Panes) != 0 || len(h.herdrCalls("pane", "split")) != 0 || len(h.herdrCalls("agent", "start")) != 0 {
				t.Fatalf("a refused spawn left panes or an agent: %v", h.fakeState().Calls)
			}
		})
	}
}

func TestShipSpawnLinksEnvOnlyWhenSafe(t *testing.T) {
	cases := []struct {
		name         string
		branch       string
		env, ignored bool
		tracked      bool
		linked       bool
		why          string
	}{
		{name: "feature branch, ignored", branch: "feat", env: true, ignored: true, linked: true},
		{name: "on main", branch: "main", env: true, ignored: true, why: "the checkout is on main"},
		{name: "on master", branch: "master", env: true, ignored: true, why: "the checkout is on master"},
		{name: "no .env", branch: "feat", ignored: true, why: "the checkout has no .env"},
		{name: "not ignored", branch: "feat", env: true, why: ".env is not gitignored"},
		{name: "already in the worktree", branch: "feat", env: true, ignored: true, tracked: true, why: "the worktree already has a .env"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := setup(t)
			if c.ignored {
				os.WriteFile(filepath.Join(h.repo, ".gitignore"), []byte(".env\n"), 0o644)
				h.gitIn(h.repo, "add", ".gitignore")
			}
			if c.env {
				os.WriteFile(filepath.Join(h.repo, ".env"), []byte("TOKEN=x\n"), 0o644)
			}
			if c.tracked {
				h.gitIn(h.repo, "add", "-f", ".env")
			}
			h.gitIn(h.repo, "commit", "-q", "--allow-empty", "-m", "setup")
			h.gitIn(h.repo, "checkout", "-q", "-B", c.branch)
			h.task("t1")
			out := h.must("spawn", "t1")
			dst := filepath.Join(h.wt("t1"), ".env")
			target, err := os.Readlink(dst)
			if c.linked {
				if err != nil || target != filepath.Join(h.repo, ".env") {
					t.Fatalf(".env link = %q, %v", target, err)
				}
				wantContains(t, out, ".env linked into "+h.wt("t1"))
				return
			}
			if err == nil {
				t.Fatalf(".env was linked: %q", target)
			}
			wantContains(t, out, ".env not linked: "+c.why)
			wantContains(t, string(mustRead(t, filepath.Join(h.dir("t1"), "status.log"))), ".env not linked: "+c.why)
		})
	}
}

// shippedLead spawns t1 and marks it done, ready for `orch close`.
func shippedLead(t *testing.T, h *harness) {
	t.Helper()
	h.task("t1")
	h.must("spawn", "t1")
	h.must("report", "--dir", h.dir("t1"), "done", "branch orch/t1")
}

func TestCloseRemovesCleanMergedWorktreeAndBranch(t *testing.T) {
	h := setup(t)
	shippedLead(t, h)
	out := h.must("close", "t1")
	wantContains(t, out, "closed lead-t1", "removed worktree "+h.wt("t1")+" and branch orch/t1")
	if _, err := os.Stat(h.wt("t1")); err == nil {
		t.Fatal("worktree directory still there")
	}
	if _, err := git(h.repo, "show-ref", "--verify", "--quiet", "refs/heads/orch/t1"); err == nil {
		t.Fatal("branch orch/t1 still there")
	}
	for _, c := range h.herdrCalls("worktree", "remove") {
		if slices.Contains(c, "--force") {
			t.Fatalf("close forced the removal: %v", c)
		}
	}
}

func TestCloseKeepsWorktreeThatWouldLoseWork(t *testing.T) {
	cases := map[string]struct {
		dirty func(h *harness)
		want  string
	}{
		"uncommitted file": {func(h *harness) { os.WriteFile(filepath.Join(h.wt("t1"), "wip.go"), []byte("x"), 0o644) }, "it has uncommitted changes"},
		"unmerged commit": {func(h *harness) {
			os.WriteFile(filepath.Join(h.wt("t1"), "done.go"), []byte("x"), 0o644)
			h.gitIn(h.wt("t1"), "add", ".")
			h.gitIn(h.wt("t1"), "commit", "-q", "-m", "work")
		}, "its branch is not merged into main"},
	}
	for name, c := range cases {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s force=%v", name, force), func(t *testing.T) {
				h := setup(t)
				shippedLead(t, h)
				c.dirty(h)
				args := []string{"close", "t1"}
				if force {
					args = append(args, "--force")
				}
				out := h.must(args...)
				wantContains(t, out, "closed lead-t1", "kept worktree "+h.wt("t1")+" (branch orch/t1): "+c.want, "git -C "+h.repo+" worktree remove "+h.wt("t1"))
				if _, err := os.Stat(h.wt("t1")); err != nil {
					t.Fatalf("worktree gone: %v", err)
				}
				if len(h.herdrCalls("worktree", "remove")) != 0 {
					t.Fatal("close asked Herdr to remove a worktree that holds work")
				}
				if h.lead("t1").ClosedAt == 0 {
					t.Fatal("lead not closed")
				}
			})
		}
	}
}

func TestRespawnReusesTheWorktreeAndBranch(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	os.WriteFile(filepath.Join(h.wt("t1"), "wip.go"), []byte("x"), 0o644)
	h.gitIn(h.wt("t1"), "add", ".")
	h.gitIn(h.wt("t1"), "commit", "-q", "-m", "work")
	os.WriteFile(filepath.Join(h.dir("t1"), "handoff.md"), []byte("continue\n"), 0o644)

	h.must("respawn", "t1")
	if n := len(h.herdrCalls("worktree", "create")); n != 1 {
		t.Fatalf("respawn recreated the worktree: %d creates", n)
	}
	if n := len(h.herdrCalls("worktree", "open")); n != 1 {
		t.Fatalf("respawn did not reopen the worktree: %d opens", n)
	}
	if len(h.herdrCalls("worktree", "remove")) != 0 {
		t.Fatal("respawn removed the worktree")
	}
	if _, err := os.Stat(filepath.Join(h.wt("t1"), "wip.go")); err != nil {
		t.Fatalf("committed work lost: %v", err)
	}
	l := h.lead("t1")
	if l.ClosedAt != 0 || l.WorkspaceID == "" || !strings.HasPrefix(l.PaneID, l.WorkspaceID+":") {
		t.Fatalf("new lead = %+v", l)
	}
}

func TestLeadWithoutWorkspaceClosesAndRespawnsAsBefore(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	l := h.lead("t1")
	l.WorkspaceID = "" // a lead from before orch owned worktrees
	if err := saveLead(h.dir("t1"), l); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(h.dir("t1"), "handoff.md"), []byte("continue\n"), 0o644)
	h.must("respawn", "t1")
	if n := len(h.herdrCalls("worktree", "create")); n != 1 {
		t.Fatalf("respawn of an old lead recreated its worktree: %d creates", n)
	}

	l = h.lead("t1")
	l.WorkspaceID = ""
	if err := saveLead(h.dir("t1"), l); err != nil {
		t.Fatal(err)
	}
	h.must("close", "t1", "--force")
	if len(h.herdrCalls("worktree", "remove")) != 0 {
		t.Fatal("close removed the worktree of a lead orch did not make it for")
	}
	if _, err := os.Stat(h.wt("t1")); err != nil {
		t.Fatalf("worktree gone: %v", err)
	}
}
