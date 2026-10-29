package main

import (
	"fmt"
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

// riskRec is a latest gate run with n verified blocker findings finding-1..n.
func riskRec(n int) *gateRecord {
	rec := &gateRecord{Hash: "hcur", Reduction: &reviewResult{}}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("finding-%d", i)
		rec.Findings = append(rec.Findings, gateFinding{ID: id, Severity: "blocker", Verified: true, Where: fmt.Sprintf("w%d", i), Claim: fmt.Sprintf("c%d", i)})
		rec.Reduction.ActiveBlockers = append(rec.Reduction.ActiveBlockers, id)
	}
	return rec
}

func riskItem(n int, state, hash string) ledgerItem {
	return ledgerItem{ID: fmt.Sprintf("L%d", n), State: state, Reason: "why " + state, Seen: []ledgerSighting{{Hash: hash, Finding: fmt.Sprintf("finding-%d", n)}}}
}

func TestPlanKnownRisksNameTheLedgerItem(t *testing.T) {
	rec := riskRec(4)
	rec.Questions = []gateQuestion{{Package: "a", Question: "Which?", Decisive: true}}
	dup := riskItem(3, "duplicate", "hcur")
	dup.Of = "L1"
	led := &gateLedger{Schema: 1, Items: []ledgerItem{
		riskItem(1, "open", "hcur"), riskItem(2, "rejected", "hcur"), dup,
		riskItem(4, "accepted", "hold"), // seen only in an older run
	}}
	got := gateKnownRisks(rec, 2, "/r/report.md", led, "spec.md")
	want := "Gate round 2 did not pass; these were not resolved. Full report: /r/report.md\n" +
		"Handle or escalate each risk that touches your package.\n" +
		"Ledger: orch gate resolve spec.md lists every item.\n" +
		"- L1 (open): w1: c1\n- L2 (rejected: why rejected): w2: c2\n- L3 (duplicate of L1: why duplicate): w3: c3\n- w4: c4\n- Question: Which?"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// A hint-level match changes nothing.
	led.Items[0].Match = &ledgerMatch{Choice: "L2", P: 0.95, HintAt: 5}
	if again := gateKnownRisks(rec, 2, "/r/report.md", led, "spec.md"); again != got {
		t.Fatalf("match changed the output:\n%s", again)
	}
}

func TestPlanKnownRisksListAcceptedFirstOutsideTheCap(t *testing.T) {
	rec := riskRec(33)
	led := &gateLedger{Schema: 1}
	for i := 1; i <= 33; i++ {
		state := "open"
		if i > 30 {
			state = "accepted"
		}
		led.Items = append(led.Items, riskItem(i, state, "hcur"))
	}
	lines := strings.Split(gateKnownRisks(rec, 2, "/r", led, "spec.md"), "\n")
	bullets := lines[3:] // header, handle line, ledger line
	if len(bullets) != 3+25+1 {
		t.Fatalf("%d bullets:\n%s", len(bullets), strings.Join(bullets, "\n"))
	}
	for i, id := range []string{"L31", "L32", "L33"} {
		if !strings.HasPrefix(bullets[i], "- "+id+" (accepted: ") {
			t.Errorf("bullet %d: %s", i, bullets[i])
		}
	}
	if bullets[3] != "- L1 (open): w1: c1" || bullets[27] != "- L25 (open): w25: c25" || bullets[28] != "- 5 more in the report" {
		t.Fatalf("others: %q %q %q", bullets[3], bullets[27], bullets[28])
	}
}

func TestPlanKnownRisksSkipACorruptLedgerWithAWarning(t *testing.T) {
	h := capHarness(t, "")
	s, err := h.a.site()
	if err != nil {
		t.Fatal(err)
	}
	path := ledgerPath(s.root, "spec.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if led := planLedger(h.a, s.root, "spec.md"); led != nil || h.errw.Len() != 0 {
		t.Fatalf("missing ledger: %v %q", led, h.errw.String())
	}
	if err := os.WriteFile(path, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	led := planLedger(h.a, s.root, "spec.md")
	if led != nil || !strings.HasPrefix(h.errw.String(), "orch: gate ledger "+path+" unreadable, known risks listed without ledger notes: ") {
		t.Fatalf("corrupt ledger: %v %q", led, h.errw.String())
	}
	rec := riskRec(2)
	if got, want := gateKnownRisks(rec, 1, "/r", led, "spec.md"), "Gate round 1 did not pass; these were not resolved. Full report: /r\nHandle or escalate each risk that touches your package.\n- w1: c1\n- w2: c2"; got != want {
		t.Fatalf("nil-ledger output:\n%s", got)
	}
}

func TestPlanProceedsWithLedgerNotesAfterTheCap(t *testing.T) {
	h := capHarness(t, "")
	capRound(t, h, "old1", "incomplete", false, "")
	dir := capRound(t, h, "cur", "fail", true, "")
	s, _ := h.a.site()
	led := &gateLedger{Schema: 1, SpecRel: "spec.md", Next: 2, Items: []ledgerItem{riskItem(1, "accepted", filepath.Base(dir))}}
	led.Items[0].Seen[0].Finding = "finding-1"
	if err := os.MkdirAll(filepath.Dir(ledgerPath(s.root, "spec.md")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveLedger(ledgerPath(s.root, "spec.md"), led); err != nil {
		t.Fatal(err)
	}
	h.must("plan", "spec.md")
	task := string(mustRead(t, filepath.Join(s.root, "alpha", "task.md")))
	wantContains(t, task, "Ledger: orch gate resolve spec.md lists every item.", "\n- L1 (accepted: why accepted): package alpha: alpha has no rollback", "\n- owns overlap")
}
