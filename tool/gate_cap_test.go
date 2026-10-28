package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capRound records a completed gate round of spec.md. With cur it is bound to
// the current spec text and checkout; otherwise it is an older version.
func capRound(t *testing.T, h *harness, name, state string, cur bool, extra string) string {
	t.Helper()
	root := h.a.siteRoot(t)
	if !cur {
		dir := filepath.Join(root, "gate", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, dir, map[string]string{"gate.json": `{"state":"` + state + `","spec_rel":"spec.md"` + extra + `}`})
		return dir
	}
	rec, _, dir := saveSubjectApproval(t, h, h.repo)
	rec.State, rec.SpecRel = state, "spec.md"
	rec.Reduction = &reviewResult{ActiveBlockers: []string{"finding-1", "structural-" + subjectDigest([]byte("owns overlap"))}}
	rec.Findings = []gateFinding{{ID: "finding-1", Severity: "blocker", Where: "package alpha", Claim: "alpha has\nno rollback", Verified: true}}
	rec.Deterministic = []string{"owns overlap"}
	rec.Questions = []gateQuestion{{Package: "alpha", Question: "Which queue wins?", Decisive: true}, {Package: "alpha", Question: "Cosmetic?", Decisive: false}}
	if err := writeGate(dir, rec); err != nil {
		t.Fatal(err)
	}
	return dir
}

func capRoundRaw(t *testing.T, h *harness, name, body string) {
	t.Helper()
	dir := filepath.Join(h.a.siteRoot(t), "gate", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"gate.json": body})
}

func capHarness(t *testing.T, cfg string) *harness {
	h := setup(t)
	if cfg != "" {
		writeFiles(t, h.tmp, map[string]string{"config.toml": cfg})
	}
	h.commitSpec(planHeader + planPkg("alpha", "kind: ship\nowns: app.py", "") + planTail)
	return h
}

func TestPlanGateCapRefusesBelowCap(t *testing.T) {
	h := capHarness(t, "")
	code, out := h.run("plan", "spec.md")
	if code != 1 {
		t.Fatalf("0 rounds: %d\n%s", code, out)
	}
	wantContains(t, out, "0 completed round(s) done", "gate_max_rounds = 2")
	capRound(t, h, "cur", "fail", true, "")
	code, out = h.run("plan", "spec.md")
	if code != 1 {
		t.Fatalf("1 round: %d\n%s", code, out)
	}
	wantContains(t, out, "1 completed round(s) done", "nothing was created")
}

func TestPlanGateCapProceedsWithKnownRisks(t *testing.T) {
	h := capHarness(t, "")
	capRound(t, h, "old1", "incomplete", false, "")
	dir := capRound(t, h, "cur", "fail", true, "")
	out := h.must("plan", "spec.md")
	wantContains(t, out, "gate_max_rounds = 2", "2 completed round(s)", filepath.Join(dir, "report.md"))
	task := string(mustRead(t, filepath.Join(h.a.siteRoot(t), "alpha", "task.md")))
	wantContains(t, task, "## Known risks\n\nGate round 2 did not pass; these were not resolved. Full report: "+filepath.Join(dir, "report.md"),
		"\n- package alpha: alpha has no rollback", "\n- owns overlap", "\n- Question: Which queue wins?")
	if strings.Contains(task, "Cosmetic?") {
		t.Fatal("non-blocking question listed")
	}
}

func TestPlanGateCapIgnoresStaleErrorAndExperimentRounds(t *testing.T) {
	h := capHarness(t, "")
	capRound(t, h, "stale", "stale", false, "")
	capRound(t, h, "err", "error", false, "")
	capRound(t, h, "cur", "fail", true, "")
	exp := filepath.Join(h.a.siteRoot(t), "gate", "cur", "experiments", "x")
	if err := os.MkdirAll(exp, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, exp, map[string]string{"gate.json": `{"state":"fail","spec_rel":"spec.md","experiment":"x"}`})
	// A record of another spec path and a top-level experiment record do not count.
	capRoundRaw(t, h, "other", `{"state":"fail","spec_rel":"other.md"}`)
	capRoundRaw(t, h, "expTop", `{"state":"fail","spec_rel":"spec.md","experiment":"y"}`)
	code, out := h.run("plan", "spec.md")
	if code != 1 {
		t.Fatalf("uncounted rounds reached the cap: %d\n%s", code, out)
	}
	wantContains(t, out, "1 completed round(s) done")
}

func TestPlanGateCapNeedsAReviewOfTheCurrentVersion(t *testing.T) {
	h := capHarness(t, "")
	capRound(t, h, "old1", "fail", false, "")
	capRound(t, h, "old2", "incomplete", false, "")
	code, out := h.run("plan", "spec.md")
	if code != 1 {
		t.Fatalf("older versions alone reached the cap: %d\n%s", code, out)
	}
	wantContains(t, out, "2 completed round(s) done")
}

func TestPlanGateCapZeroKeepsRefusing(t *testing.T) {
	h := capHarness(t, "gate_max_rounds = 0\n")
	capRound(t, h, "old1", "fail", false, "")
	capRound(t, h, "old2", "fail", false, "")
	capRound(t, h, "cur", "fail", true, "")
	code, out := h.run("plan", "spec.md")
	if code != 1 {
		t.Fatalf("no cap but plan proceeded: %d\n%s", code, out)
	}
	wantContains(t, out, "3 completed round(s) done")
}

func TestPlanSkipGateAddsNoKnownRisks(t *testing.T) {
	h := capHarness(t, "")
	capRound(t, h, "old1", "fail", false, "")
	capRound(t, h, "cur", "fail", true, "")
	h.must("plan", "spec.md", "--skip-gate")
	task := string(mustRead(t, filepath.Join(h.a.siteRoot(t), "alpha", "task.md")))
	if strings.Contains(task, "Known risks") {
		t.Fatalf("--skip-gate added risks:\n%s", task)
	}
	if _, err := os.Stat(filepath.Join(h.a.siteRoot(t), "alpha", "task.md")); err != nil {
		t.Fatal(err)
	}
}
