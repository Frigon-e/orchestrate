package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// moveToBackendPath relocates id's worktree to a path orch would never compute
// and records it in lead.json, as a backend that picks its own location does.
func moveToBackendPath(h *harness, id string) string {
	h.t.Helper()
	to := filepath.Join(h.tmp, "backend-chose", id)
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		h.t.Fatal(err)
	}
	from := worktreePath(h.dir(id), id)
	h.git("worktree", "move", from, to)
	l := h.lead(id)
	l.WorktreePath = to
	if err := saveLead(h.dir(id), l); err != nil {
		h.t.Fatal(err)
	}
	if _, err := os.Stat(from); err == nil {
		h.t.Fatalf("computed path %s still exists", from)
	}
	return to
}

func TestLeadWorktreeKeepsComputedPathWithoutRecord(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	if got, want := leadWorktree(h.dir("t1"), "t1"), h.wt("t1"); got != want {
		t.Fatalf("leadWorktree = %q, want computed %q", got, want)
	}
}

func TestVerifyFindsWorktreeAtBackendChosenPath(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	moveToBackendPath(h, "t1")
	_, out := h.run("verify", "t1")
	if strings.Contains(out, "no worktree at") {
		t.Fatalf("verify looked at the computed path:\n%s", out)
	}
}

func TestDoctorFlagsDirtyWorktreeAtBackendChosenPath(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	wt := moveToBackendPath(h, "t1")
	h.must("report", "--dir", h.dir("t1"), "done", "ok")
	if err := os.WriteFile(filepath.Join(wt, "wip.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out := h.run("doctor")
	wantContains(t, out, "done, but its worktree has uncommitted changes")
}

func TestStallFingerprintAndWakeOutcomeReadBackendChosenPath(t *testing.T) {
	h := setup(t)
	h.wakeClockFollows()
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return wakeAnswers(), shadowUsage{}, nil
	})
	h.task("t1")
	h.must("spawn", "t1")
	wt := moveToBackendPath(h, "t1")
	h.must("watch", "--once")
	s, err := h.a.site()
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Duration(s.cfg.StallMinutes+1) * time.Minute)
	wantContains(t, h.must("watch", "--once"), "orch: t1 stalled")
	recs := h.wakeRecords()
	if len(recs) == 0 || recs[0].Meta["fingerprint"] == "" {
		t.Fatalf("stalled alert carried no worktree fingerprint: %+v", recs)
	}
	if err := os.WriteFile(filepath.Join(wt, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Duration(s.cfg.StallMinutes+1) * time.Minute)
	resolveOutcomes(s)
	outs := h.wakeOutcomes()
	if len(outs) != 1 || outs[0].Outcome["progressed_alone"] != true {
		t.Fatalf("outcomes = %+v", outs)
	}
}

func TestSubStartUsesBackendChosenWorktree(t *testing.T) {
	h := setup(t)
	ownedLead(t, h)
	wt := moveToBackendPath(h, "t1")
	h.must("sub", "start", "first", "--harness", "claude", "--model", "sonnet", "look")
	splits := h.herdrCalls("pane", "split")
	if got := flagValue(splits[len(splits)-1], "--cwd"); got != wt {
		t.Fatalf("helper cwd = %q, want %q", got, wt)
	}
}

func TestRespawnKeepsBaseWhenWorktreeIsAtBackendChosenPath(t *testing.T) {
	h := planInt(t)
	h.leadWork("alpha", map[string]string{"alpha/f": "a\n"})
	h.done("alpha")
	h.must("verify", "alpha")
	h.must("spawn", "beta")
	h.leadWork("beta", map[string]string{"beta/f": "b\n"})
	moveToBackendPath(h, "beta")
	before := string(mustRead(t, filepath.Join(h.dir("beta"), "task.md")))
	h.sideCommit(intBranch, map[string]string{"other": "o"})
	if err := requireMerged(mustSite(t, h), h.dir("beta"), "beta", mustFM(t, h, "beta")); err != nil {
		t.Fatal(err)
	}
	if after := string(mustRead(t, filepath.Join(h.dir("beta"), "task.md"))); after != before {
		t.Errorf("re-pinned a brief whose worktree exists at its stored path:\n%s", after)
	}
}

func mustSite(t *testing.T, h *harness) site {
	t.Helper()
	s, err := h.a.site()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustFM(t *testing.T, h *harness, id string) map[string]string {
	t.Helper()
	fm, _, err := readTask(filepath.Join(h.dir(id), "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	return fm
}
