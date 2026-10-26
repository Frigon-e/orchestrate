package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const intBranch = "orch/int-spec"

// intSpec is two chained ship packages and an unrelated third one.
func intSpec() string {
	return planHeader +
		planPkg("alpha", "kind: ship\nowns: alpha/", "") +
		planPkg("beta", "kind: ship\ndepends: alpha\nowns: beta/", "") +
		planPkg("gamma", "kind: ship\nowns: gamma/", "") +
		planTail
}

func (h *harness) setCheck(command string) {
	h.t.Helper()
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	writeFiles(h.t, filepath.Join(h.repo, ".agents"), map[string]string{"orchestrate.toml": "check = \"" + command + "\"\n"})
}

func (h *harness) branchTip(branch string) string {
	h.t.Helper()
	out, err := exec.Command("git", "-C", h.repo, "rev-parse", branch).CombinedOutput()
	if err != nil {
		h.t.Fatalf("rev-parse %s: %v\n%s", branch, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (h *harness) hasBranch(branch string) bool {
	return exec.Command("git", "-C", h.repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

func (h *harness) branchHas(branch, path string) bool {
	return exec.Command("git", "-C", h.repo, "cat-file", "-e", branch+":"+path).Run() == nil
}

// planInt plans intSpec with a passing check and returns the harness.
func planInt(t *testing.T) *harness {
	t.Helper()
	h := setup(t)
	h.setCheck("true")
	h.commitSpec(intSpec())
	h.must("plan", "spec.md", "--skip-gate")
	return h
}

// leadWork gives task id the worktree a lead would cut from its pinned base
// and commits files in it.
func (h *harness) leadWork(id string, files map[string]string) string {
	h.t.Helper()
	fm, _, err := readTask(filepath.Join(h.dir(id), "task.md"))
	if err != nil {
		h.t.Fatal(err)
	}
	_, sha, _ := strings.Cut(fm["base"], "@")
	wt := worktreePath(h.dir(id), id)
	if _, err := os.Lstat(wt); err != nil { // spawn already made it inside Herdr
		h.git("worktree", "add", "-q", "-b", "orch/"+id, wt, sha)
	}
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(wt, name)), 0o755)
		writeFiles(h.t, wt, map[string]string{name: body})
	}
	h.gitIn(wt, "add", ".")
	h.gitIn(wt, "commit", "-q", "-m", id)
	return wt
}

// sideCommit commits straight onto the integration branch, as a stand-in for
// another package having moved it.
func (h *harness) sideCommit(branch string, files map[string]string) {
	h.t.Helper()
	side, temp := filepath.Join(h.tmp, "state", "repo", "integration", "spec", "wt"), false
	if _, err := os.Stat(side); err != nil { // no merge worktree yet: borrow one
		side, temp = filepath.Join(h.tmp, "side"), true
		h.git("worktree", "add", "-q", side, branch)
	}
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(side, name)), 0o755)
		writeFiles(h.t, side, map[string]string{name: body})
	}
	h.gitIn(side, "add", ".")
	h.gitIn(side, "commit", "-q", "-m", "side")
	if temp {
		h.git("worktree", "remove", "--force", side)
	}
}

func (h *harness) done(id string) { h.must("report", "--dir", h.dir(id), "done", "ok") }

func TestIntegrationPlanCutsBranchAndPinsTasks(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	main := h.branchTip("main")
	out := h.must("plan", "spec.md", "--skip-gate")
	wantContains(t, out, "integration: "+intBranch+"\n", "merge_worktree: "+filepath.Join(h.tmp, "state", "repo", "integration", "spec", "wt"))
	if h.branchTip(intBranch) != main {
		t.Fatal("integration branch is not cut from the base")
	}
	for _, id := range []string{"store", "api"} {
		fm, _, _ := readTask(filepath.Join(h.dir(id), "task.md"))
		if !strings.HasPrefix(fm["base"], intBranch+"@") || fm["integration"] != intBranch || len(fm["spec_hash"]) != 12 {
			t.Errorf("%s front matter: base %q integration %q spec_hash %q", id, fm["base"], fm["integration"], fm["spec_hash"])
		}
	}
	if fm, _, _ := readTask(filepath.Join(h.dir("docs"), "task.md")); fm["integration"] != "" {
		t.Errorf("scout got integration %q", fm["integration"])
	}
	// a rerun reuses the branch
	h.sideCommit(intBranch, map[string]string{"x": "x"})
	tip := h.branchTip(intBranch)
	h.must("plan", "spec.md", "--skip-gate")
	if h.branchTip(intBranch) != tip {
		t.Error("rerun moved the integration branch")
	}
}

func TestIntegrationPlanDryRunAndNoDependencies(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	out := h.must("plan", "spec.md", "--dry-run", "--skip-gate")
	wantContains(t, out, "integration: "+intBranch+" (would create)")
	if h.hasBranch(intBranch) {
		t.Fatal("dry run created the branch")
	}

	h = setup(t)
	h.commitSpec(planHeader + planPkg("solo", "kind: ship\nowns: solo/", "") + planTail)
	out = h.must("plan", "spec.md", "--skip-gate")
	if strings.Contains(out, "integration:") || h.hasBranch(intBranch) {
		t.Errorf("a spec without dependencies got an integration branch:\n%s", out)
	}
	fm, _, _ := readTask(filepath.Join(h.dir("solo"), "task.md"))
	if fm["integration"] != "" || !strings.HasPrefix(fm["base"], "main@") {
		t.Errorf("solo: integration %q base %q", fm["integration"], fm["base"])
	}
}

func TestIntegrationVerifyMergesAndIsIdempotent(t *testing.T) {
	h := planInt(t)
	main := h.branchTip("main")
	h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	out := h.must("verify", "alpha")
	wantContains(t, out, "verify: pass", "integration: merged into "+intBranch)
	if !h.branchHas(intBranch, "alpha/f") || h.branchTip("main") != main {
		t.Fatal("alpha is not on the integration branch only")
	}
	head := h.branchTip("orch/alpha")
	wantMarker := "candidate " + head + "\nmerge " + h.branchTip(intBranch) + "\n"
	if got := string(mustRead(t, filepath.Join(h.dir("alpha"), "integrated"))); got != wantMarker {
		t.Errorf("integrated = %q, want %q", got, wantMarker)
	}
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("alpha"), "status.log"))), "merged into "+intBranch+" at ")
	tip := h.branchTip(intBranch)
	wantContains(t, h.must("verify", "alpha"), "integration: already merged", "no post-merge check ran")
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("alpha"), "status.log"))), "no post-merge check ran")
	if got := string(mustRead(t, filepath.Join(h.dir("alpha"), "integrated"))); got != wantMarker {
		t.Errorf("already-merged marker = %q, want %q", got, wantMarker)
	}
	if h.branchTip(intBranch) != tip {
		t.Error("second verify moved the branch")
	}
}

func TestIntegrationNoMergeFlag(t *testing.T) {
	h := planInt(t)
	wt := h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	writeFiles(t, wt, map[string]string{"alpha/untracked": "work in progress\n"})
	writeFiles(t, wt, map[string]string{"alpha/f": "uncommitted work\n"})
	out := h.must("verify", "alpha", "--no-merge")
	wantContains(t, out, "verify: pass (not integrated)", "2 changed")
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("alpha"), "status.log"))), "verify pass (not integrated)")
	if strings.Contains(out, "integration:") || h.branchHas(intBranch, "alpha/f") {
		t.Errorf("--no-merge merged:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.dir("alpha"), "integrated")); err == nil {
		t.Error("--no-merge wrote the integrated marker")
	}
}

func TestIntegrationVerifyRejectsUntrackedImplementation(t *testing.T) {
	h := planInt(t)
	wt := h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	writeFiles(t, wt, map[string]string{"alpha/implementation": "required implementation\n"})
	tip := h.branchTip(intBranch)
	code, out := h.run("verify", "alpha")
	if code != 1 {
		t.Fatalf("untracked implementation silently omitted: exit %d\n%s", code, out)
	}
	wantContains(t, out, "candidate is not clean", "--no-merge")
	if h.branchTip(intBranch) != tip || exists(filepath.Join(h.dir("alpha"), "integrated")) {
		t.Fatal("dirty candidate was integrated")
	}
}

func TestIntegrationVerifyRejectsDirtyTrackedCandidate(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "unstaged"
		if staged {
			name = "staged"
		}
		t.Run(name, func(t *testing.T) {
			h := planInt(t)
			wt := h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
			writeFiles(t, wt, map[string]string{"alpha/f": "changed\n"})
			if staged {
				h.gitIn(wt, "add", "alpha/f")
			}
			tip := h.branchTip(intBranch)
			if code, out := h.run("verify", "alpha"); code != 1 || !strings.Contains(out, "candidate is not clean") {
				t.Fatalf("dirty candidate: exit %d\n%s", code, out)
			}
			if h.branchTip(intBranch) != tip || exists(filepath.Join(h.dir("alpha"), "integrated")) {
				t.Fatal("dirty candidate integrated")
			}
		})
	}
}

func TestIntegrationVerifyRejectsCheckMutation(t *testing.T) {
	for _, tc := range []struct{ name, check, message string }{
		{"tracked source", "echo changed > alpha/f", "non-ignored working-tree changes"},
		{"untracked source", "echo implementation > alpha/new", "non-ignored working-tree changes"},
		{"advanced HEAD", "git -c core.hooksPath=/dev/null commit --allow-empty -qm advanced", "HEAD moved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := planInt(t)
			h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
			h.setCheck(tc.check)
			tip := h.branchTip(intBranch)
			code, out := h.run("verify", "alpha")
			if code != 1 {
				t.Fatalf("mutating check: exit %d\n%s", code, out)
			}
			wantContains(t, out, "verify: fail", "candidate: changed after checks", tc.message, "integration refused")
			if h.branchTip(intBranch) != tip || exists(filepath.Join(h.dir("alpha"), "integrated")) {
				t.Fatal("mutating check authorized integration")
			}
		})
	}
}

func TestIntegrationVerifyAllowsIgnoredCheckOutputs(t *testing.T) {
	h := planInt(t)
	wt := h.leadWork("alpha", map[string]string{"alpha/f": "a\n", "alpha/.gitignore": "*.log\n"})
	writeFiles(t, wt, map[string]string{"alpha/before.log": "ignored\n"})
	h.setCheck("echo output > alpha/check.log")
	wantContains(t, h.must("verify", "alpha"), "verify: pass", "integration: merged into "+intBranch)
	h.must("integration", "remove", "spec")
}

func TestIntegrationPostMergeCheckMutationRecovers(t *testing.T) {
	for _, tc := range []struct{ name, check string }{
		{"passing dirty check", "echo changed > alpha/f; echo output > check-output"},
		{"failing dirty check", "echo changed > alpha/f; echo output > check-output; exit 1"},
		{"advanced HEAD", "git -c core.hooksPath=/dev/null commit --allow-empty -qm advanced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := planInt(t)
			h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
			h.setCheck(`case \"$PWD\" in */integration/*) ` + tc.check + `;; esac`)
			tip := h.branchTip(intBranch)
			code, out := h.run("verify", "alpha")
			if code != 1 {
				t.Fatalf("post-merge check: exit %d\n%s", code, out)
			}
			wantContains(t, out, "integration: check failed", "post-merge check changed the merge result", "reset merge and cleaned non-ignored check outputs")
			if h.branchTip(intBranch) != tip || exists(filepath.Join(h.dir("alpha"), "integrated")) {
				t.Fatal("mutating post-merge check authorized integration")
			}
			iwt := filepath.Join(h.tmp, "state", "repo", "integration", "spec", "wt")
			if status, err := git(iwt, "status", "--porcelain"); err != nil || status != "" {
				t.Fatalf("rollback left dirty worktree: %v\n%s", err, status)
			}
			h.must("integration", "remove", "spec")
		})
	}
}

func TestIntegrationDependentAcceptsLegacyMarker(t *testing.T) {
	h := planInt(t)
	h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	h.done("alpha")
	h.must("verify", "alpha")
	writeFiles(t, h.dir("alpha"), map[string]string{"integrated": h.branchTip("orch/alpha") + "\n"})
	wantContains(t, h.must("spawn", "beta"), "spawned")
}

func TestIntegrationDependentRejectsInvalidMarker(t *testing.T) {
	for _, marker := range []string{"candidate %s\nmerge invalid\n", "candidate %s\nmerge %s\nextra\n"} {
		t.Run(marker, func(t *testing.T) {
			h := planInt(t)
			h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
			h.done("alpha")
			h.must("verify", "alpha")
			sha := h.branchTip("orch/alpha")
			writeFiles(t, h.dir("alpha"), map[string]string{"integrated": strings.ReplaceAll(marker, "%s", sha)})
			if code, out := h.run("spawn", "beta"); code != 1 || !strings.Contains(out, "not merged into") {
				t.Fatalf("malformed marker authorized dependency: exit %d\n%s", code, out)
			}
		})
	}
}

func TestIntegrationTwoPackagesMergeInSequence(t *testing.T) {
	h := planInt(t)
	h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	h.leadWork("gamma", map[string]string{"gamma/f": "g\n"})
	h.must("verify", "alpha")
	h.must("verify", "gamma")
	if !h.branchHas(intBranch, "alpha/f") || !h.branchHas(intBranch, "gamma/f") {
		t.Fatal("both packages should be on the integration branch")
	}
	log, _ := exec.Command("git", "-C", h.repo, "log", "--merges", "--format=%s", intBranch).CombinedOutput()
	wantContains(t, string(log), "orch: merge alpha into "+intBranch, "orch: merge gamma into "+intBranch)
}

func TestIntegrationConflictLeavesBranchUnchanged(t *testing.T) {
	h := planInt(t)
	h.sideCommit(intBranch, map[string]string{"alpha/f": "theirs\n"})
	h.leadWork("alpha", map[string]string{"alpha/f": "mine\n"})
	tip := h.branchTip(intBranch)
	code, out := h.run("verify", "alpha")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantContains(t, out, "integration: conflict", "alpha/f")
	if h.branchTip(intBranch) != tip {
		t.Error("a conflicting merge moved the branch")
	}
	iwt := filepath.Join(h.tmp, "state", "repo", "integration", "spec", "wt")
	if st, _ := git(iwt, "status", "--porcelain"); st != "" {
		t.Errorf("merge worktree left dirty:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(h.dir("alpha"), "integrated")); err == nil {
		t.Error("a conflicting package was marked integrated")
	}
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("alpha"), "status.log"))), "conflicted")
}

func TestIntegrationFailingCheckResetsBranch(t *testing.T) {
	h := planInt(t)
	// green in the lead's worktree, red in the merge worktree
	h.setCheck(`case \"$PWD\" in */integration/*) echo merged tree is red; exit 1;; esac`)
	h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	tip := h.branchTip(intBranch)
	code, out := h.run("verify", "alpha")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantContains(t, out, "verify: pass", "integration: check failed", "merged tree is red")
	if h.branchTip(intBranch) != tip || h.branchHas(intBranch, "alpha/f") {
		t.Error("the branch kept a merge that failed the check")
	}
	if _, err := os.Stat(filepath.Join(h.dir("alpha"), "integrated")); err == nil {
		t.Error("a failing merge was marked integrated")
	}
}

func TestIntegrationDependentSpawn(t *testing.T) {
	h := planInt(t)
	h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	h.done("alpha")

	code, out := h.run("spawn", "beta")
	if code != 1 || !strings.Contains(out, "dependency alpha is done but not merged into "+intBranch+": run orch verify alpha") {
		t.Fatalf("spawn before merge: exit %d\n%s", code, out)
	}
	h.must("verify", "alpha")
	h.sideCommit(intBranch, map[string]string{"other": "o"}) // the tip moves after the merge
	tip := h.branchTip(intBranch)[:12]
	h.must("spawn", "beta")

	fm, _, _ := readTask(filepath.Join(h.dir("beta"), "task.md"))
	if fm["base"] != intBranch+"@"+tip {
		t.Errorf("base = %q, want %s@%s", fm["base"], intBranch, tip)
	}
	// spawn cuts the lead's worktree branch from the pinned tip, not the moved tip
	if got := h.branchTip(shipBranch("beta"))[:12]; got != tip {
		t.Errorf("worktree branch cut from %s, want the pinned %s", got, tip)
	}

	// the lead verifies on top of alpha
	h.leadWork("beta", map[string]string{"beta/f": "b\n"})
	h.must("verify", "beta")
	if !h.branchHas(intBranch, "alpha/f") || !h.branchHas(intBranch, "beta/f") || !h.branchHas(intBranch, "other") {
		t.Error("integration branch lacks work")
	}
	if h.branchHas("main", "alpha/f") {
		t.Error("work reached the base branch")
	}
}

func TestIntegrationRespawnKeepsExistingBase(t *testing.T) {
	h := planInt(t)
	h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	h.done("alpha")
	h.must("verify", "alpha")
	h.must("spawn", "beta")
	h.leadWork("beta", map[string]string{"beta/f": "b\n"})
	before := string(mustRead(t, filepath.Join(h.dir("beta"), "task.md")))
	h.sideCommit(intBranch, map[string]string{"other": "o"})
	h.must("close", "beta", "--force")
	h.must("spawn", "beta")
	if after := string(mustRead(t, filepath.Join(h.dir("beta"), "task.md"))); after != before {
		t.Errorf("a respawn with a worktree re-pinned the brief:\n%s", after)
	}
}

func TestIntegrationRejectsOptionLikeBranch(t *testing.T) {
	h := planInt(t)
	h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	for _, bad := range []string{"--upload-pack=x", "main", "orch/int-a b", "-x"} {
		if err := setFrontMatter(filepath.Join(h.dir("alpha"), "task.md"), "integration", bad); err != nil {
			t.Fatal(err)
		}
		code, out := h.run("verify", "alpha")
		if code != 1 || !strings.Contains(out, "is not an orch integration branch") {
			t.Errorf("verify with integration %q: exit %d\n%s", bad, code, out)
		}
	}
	h.done("alpha")
	if err := setFrontMatter(filepath.Join(h.dir("beta"), "task.md"), "integration", "--bad"); err != nil {
		t.Fatal(err)
	}
	if code, out := h.run("spawn", "beta"); code != 1 || !strings.Contains(out, "is not an orch integration branch") {
		t.Errorf("spawn with a bad integration: exit %d\n%s", code, out)
	}
}

func TestPlanReportsStaleBriefs(t *testing.T) {
	h := setup(t)
	h.commitSpec(intSpec())
	h.must("plan", "spec.md", "--skip-gate")
	if out := h.must("plan", "spec.md", "--skip-gate"); strings.Contains(out, "stale") {
		t.Fatalf("an unchanged spec is stale:\n%s", out)
	}
	h.commitSpec(strings.Replace(intSpec(), "Build beta.", "Build   beta\nand more.", 1))
	before := string(mustRead(t, filepath.Join(h.dir("beta"), "task.md")))
	code, out := h.run("plan", "spec.md", "--skip-gate")
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	wantContains(t, out, "  beta,ship,sonnet,alpha,stale", "stale[1]:", "beta: the spec changed", "remove "+h.dir("beta")+" by hand and rerun orch plan", "  alpha,ship,sonnet,,exists")
	if string(mustRead(t, filepath.Join(h.dir("beta"), "task.md"))) != before {
		t.Error("plan rewrote a stale brief")
	}

	// whitespace-only edits do not count, and old tasks without a hash stay exists
	h.commitSpec(intSpec())
	h.must("plan", "spec.md", "--skip-gate")
	path := filepath.Join(h.dir("beta"), "task.md")
	b := string(mustRead(t, path))
	var kept []string
	for _, l := range strings.Split(b, "\n") {
		if !strings.HasPrefix(l, "spec_hash:") {
			kept = append(kept, l)
		}
	}
	os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644)
	h.commitSpec(strings.Replace(intSpec(), "Build beta.", "Build beta, differently.", 1))
	wantContains(t, h.must("plan", "spec.md", "--skip-gate"), "  beta,ship,sonnet,alpha,exists")
}

func TestIntegrationRemove(t *testing.T) {
	h := planInt(t)
	wantContains(t, h.must("integration", "remove", "spec"), "no merge worktree for spec")
	h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	h.must("verify", "alpha")
	iwt := filepath.Join(h.tmp, "state", "repo", "integration", "spec", "wt")
	os.WriteFile(filepath.Join(iwt, "dirty"), []byte("x"), 0o644)
	if code, out := h.run("integration", "remove", "spec"); code != 1 || !strings.Contains(out, "uncommitted changes") {
		t.Fatalf("dirty worktree removed: %d\n%s", code, out)
	}
	os.Remove(filepath.Join(iwt, "dirty"))
	if !exists(filepath.Join(filepath.Dir(iwt), "lock")) {
		t.Fatal("the merge left no lock file to remove")
	}
	h.must("integration", "remove", "spec")
	if _, err := os.Stat(iwt); err == nil {
		t.Error("worktree still there")
	}
	if exists(filepath.Join(filepath.Dir(iwt), "lock")) {
		t.Error("remove left the lock file behind")
	}
	if !h.hasBranch(intBranch) {
		t.Error("remove deleted the branch")
	}
	if code, _ := h.run("integration", "remove", "../x"); code != 1 {
		t.Errorf("bad slug: exit %d", code)
	}
}

func TestRepinBrief(t *testing.T) {
	text := "---\nid: b\nbase: orch/int-s@aaaaaaaaaaaa\nharness: claude\n---\n\n# Spec\n\nmention aaaaaaaaaaaa here\n\n# Report-back\n\ngit worktree add -b <branch> /w/b aaaaaaaaaaaa\n"
	got, err := repinBrief(text, "orch/int-s", "bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nid: b\nbase: orch/int-s@bbbbbbbbbbbb\nharness: claude\n---\n\n# Spec\n\nmention aaaaaaaaaaaa here\n\n# Report-back\n\ngit worktree add -b <branch> /w/b bbbbbbbbbbbb\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, bad := range []string{"--x", "zzzzzzzz", ""} {
		if _, err := repinBrief(text, "orch/int-s", bad); err == nil {
			t.Errorf("sha %q accepted", bad)
		}
	}
	if _, err := repinBrief(strings.Replace(text, "@aaaaaaaaaaaa", "@--evil", 1), "orch/int-s", "bbbbbbbbbbbb"); err == nil {
		t.Error("an old base that is not a sha was accepted")
	}
}

func TestIntegrationBranchBelongsToOneSpecPath(t *testing.T) {
	h := setup(t)
	h.commitSpec(intSpec())
	h.must("plan", "spec.md", "--skip-gate")
	h.must("plan", "spec.md", "--skip-gate", "--dry-run") // the same spec may plan again

	// Another spec with the same base name would share orch/int-spec.
	os.MkdirAll(filepath.Join(h.repo, "other"), 0o755)
	writeFiles(t, h.repo, map[string]string{"other/spec.md": strings.ReplaceAll(strings.ReplaceAll(intSpec(), "alpha", "xalpha"), "beta", "xbeta")})
	h.git("add", "other/spec.md")
	h.git("commit", "-q", "-m", "other spec")
	code, out := h.run("plan", "other/spec.md", "--skip-gate", "--dry-run")
	if code != 1 || !strings.Contains(out, "already belongs to the spec spec.md") {
		t.Fatalf("a second spec took the first one's branch: %d\n%s", code, out)
	}
	if code, _ := h.run("plan", "other/spec.md", "--skip-gate"); code != 1 {
		t.Fatalf("plan of the second spec exit %d", code)
	}
	if entries := allTasks(runRoot(h.repo)); len(entries) != 3 {
		t.Fatalf("the refused plan created tasks: %v", entries)
	}
}
