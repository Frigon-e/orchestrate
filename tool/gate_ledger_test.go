package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type ledgerEnv struct {
	h      *harness
	s      site
	dir    string // run dir with a report.md
	path   string
	nowSec int64
}

func newLedgerEnv(t *testing.T) *ledgerEnv {
	t.Helper()
	h := setup(t)
	h.commitSpec(planHeader + planPkg("alpha", "kind: ship\nowns: app.py", "") + planTail)
	s, err := h.a.site()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"report.md": "# Gate report\n\nbody\n\n## Question cards\nsummary\n"})
	return &ledgerEnv{h: h, s: s, dir: dir, path: ledgerPath(s.root, "spec.md")}
}

func ledgerRec(hash, state string, ids ...string) *gateRecord {
	rec := &gateRecord{Spec: "spec.md", SpecRel: "spec.md", Hash: hash, State: state}
	for _, id := range ids {
		rec.Findings = append(rec.Findings, gateFinding{ID: id, Severity: "blocker", Verified: true, Source: "adversary-a", Where: "where " + id, Claim: "claim " + id, Evidence: "e.go:1"})
	}
	return rec
}

func (e *ledgerEnv) record(rec *gateRecord) {
	e.h.a.recordGateLedger(e.s, rec, e.dir, e.h.a.now())
}

func (e *ledgerEnv) load(t *testing.T) *gateLedger {
	t.Helper()
	led, exists, err := loadLedger(e.path, "spec.md")
	if err != nil || !exists {
		t.Fatalf("ledger: exists %v, %v", exists, err)
	}
	return led
}

func (e *ledgerEnv) report(t *testing.T) string {
	return string(mustRead(t, filepath.Join(e.dir, "report.md")))
}

func TestLedgerIDsAreStableAcrossRuns(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a", "finding-b"))
	e.record(ledgerRec("h2", "fail", "finding-b", "finding-c"))
	led := e.load(t)
	if len(led.Items) != 3 || led.Items[0].ID != "L1" || led.Items[1].ID != "L2" || led.Items[2].ID != "L3" || led.Next != 4 {
		t.Fatalf("items %+v next %d", led.Items, led.Next)
	}
	if got := len(led.Items[1].Seen); got != 2 || led.Items[1].Seen[1].Hash != "h2" || led.Items[1].Seen[1].Finding != "finding-b" {
		t.Fatalf("L2 sightings: %+v", led.Items[1].Seen)
	}
	if led.Items[0].Check != "" || led.Items[0].Claim != "claim finding-a" {
		t.Fatalf("L1 fields: %+v", led.Items[0])
	}
	// Close L1 as fixed, see it again: it reopens with a history entry, and the
	// next new finding does not reuse an old number.
	e.h.must("gate", "resolve", "spec.md", "L1", "fixed", "--reason", "patched")
	e.record(ledgerRec("h3", "fail", "finding-a", "finding-d"))
	led = e.load(t)
	l1 := led.Items[0]
	if l1.State != "open" || l1.Reason != "" || l1.ClosedAt != 0 || len(l1.History) != 2 || l1.History[1].State != "open" || len(l1.Seen) != 2 {
		t.Fatalf("reopened L1: %+v", l1)
	}
	if led.Items[3].ID != "L4" {
		t.Fatalf("new item: %+v", led.Items[3])
	}
}

func TestLedgerClosedItemsOnlyGainSightings(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a"))
	e.h.must("gate", "resolve", "spec.md", "L1", "rejected", "--reason", "not real")
	e.record(ledgerRec("h2", "fail", "finding-a"))
	it := e.load(t).Items[0]
	if it.State != "rejected" || len(it.Seen) != 2 || len(it.History) != 1 {
		t.Fatalf("%+v", it)
	}
}

func TestLedgerSkipsRunsThatAreNotGateResults(t *testing.T) {
	e := newLedgerEnv(t)
	exp := ledgerRec("h1", "fail", "finding-a")
	exp.Experiment = "x"
	noRel := ledgerRec("h1", "fail", "finding-a")
	noRel.SpecRel = ""
	for _, rec := range []*gateRecord{exp, ledgerRec("h1", "error", "finding-a"), ledgerRec("h1", "stale", "finding-a"), noRel} {
		e.record(rec)
	}
	if entries, _ := os.ReadDir(filepath.Dir(e.path)); len(entries) != 0 {
		t.Fatalf("ledger dir has %v", entries)
	}
	if strings.Contains(e.report(t), "Findings ledger") {
		t.Fatal("report section written")
	}
	// A pass with no findings also leaves no file.
	e.record(ledgerRec("h1", "pass"))
	if _, err := os.Stat(e.path); err == nil {
		t.Fatal("ledger created for a run with no findings")
	}
}

func TestLedgerOnlyVerifiedBlockersWithAnIDEnter(t *testing.T) {
	e := newLedgerEnv(t)
	rec := ledgerRec("h1", "fail", "finding-a", "finding-b", "finding-c")
	rec.Findings[1].Severity = "concern"
	rec.Findings[2].Verified = false
	rec.Findings = append(rec.Findings, gateFinding{Severity: "blocker", Verified: true, Claim: "no id"})
	e.record(rec)
	led := e.load(t)
	if len(led.Items) != 1 || led.Items[0].Seen[0].Finding != "finding-a" {
		t.Fatalf("%+v", led.Items)
	}
}

func TestLedgerCheckKeyComesFromTheReviewFinding(t *testing.T) {
	e := newLedgerEnv(t)
	rec := ledgerRec("h1", "fail", "finding-a")
	rec.Review = &gateReviewRecords{Findings: []reviewFinding{{ID: "finding-a", ItemID: "@adversary-a", CheckID: "adversary-a"}}}
	e.record(rec)
	if got := e.load(t).Items[0].Check; got != "@adversary-a/adversary-a" {
		t.Fatalf("check %q", got)
	}
}

func TestLedgerResolveMadeDuringTheMatcherSurvivesTheMerge(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a"))
	gateLedgerMatch = func(ctx context.Context, s site, rel string, cands []ledgerItem, fresh []ledgerItem) map[string]ledgerMatch {
		// The author closes L1 after the snapshot and before the locked merge.
		e.h.must("gate", "resolve", "spec.md", "L1", "accepted", "--reason", "known")
		return nil
	}
	t.Cleanup(func() { gateLedgerMatch = nil })
	e.record(ledgerRec("h2", "fail", "finding-b"))
	led := e.load(t)
	if led.Items[0].State != "accepted" || led.Items[0].Reason != "known" || len(led.Items) != 2 {
		t.Fatalf("%+v", led.Items)
	}
}

func TestLedgerMatchSeamCandidatesAndAnswers(t *testing.T) {
	e := newLedgerEnv(t)
	var calls int
	var cands, fresh []string
	gateLedgerMatch = func(ctx context.Context, s site, rel string, c []ledgerItem, f []ledgerItem) map[string]ledgerMatch {
		calls++
		cands, fresh = nil, nil
		for _, it := range c {
			cands = append(cands, it.ID+":"+it.State)
		}
		for _, it := range f {
			fresh = append(fresh, it.Seen[0].Finding)
		}
		return map[string]ledgerMatch{"finding-c": {Decision: "d1", Choice: "L1", P: 0.9, Candidates: []string{"L1"}}}
	}
	t.Cleanup(func() { gateLedgerMatch = nil })
	e.record(ledgerRec("h1", "fail", "finding-a"))
	if calls != 0 {
		t.Fatalf("matcher called on an empty snapshot")
	}
	e.record(ledgerRec("h2", "fail", "finding-a", "finding-b"))
	e.h.must("gate", "resolve", "spec.md", "L2", "duplicate", "--of", "L1", "--reason", "same")
	e.record(ledgerRec("h3", "fail", "finding-c"))
	if calls != 2 || strings.Join(cands, ",") != "L1:open" || strings.Join(fresh, ",") != "finding-c" {
		t.Fatalf("calls %d candidates %v fresh %v", calls, cands, fresh)
	}
	led := e.load(t)
	if m := led.Items[2].Match; m == nil || m.Choice != "L1" || m.P != 0.9 || led.Items[2].State != "open" {
		t.Fatalf("L3: %+v", led.Items[2])
	}
	if led.Items[0].Match != nil || led.Items[1].Match != nil {
		t.Fatal("an unmatched item got a match")
	}
}

func TestLedgerMatcherAnswerChangesNothingInTheRecordOrReport(t *testing.T) {
	run := func(withSeam bool) (string, string, string) {
		e := newLedgerEnv(t)
		e.record(ledgerRec("h1", "fail", "finding-a"))
		if withSeam {
			gateLedgerMatch = func(ctx context.Context, s site, rel string, c []ledgerItem, f []ledgerItem) map[string]ledgerMatch {
				out := map[string]ledgerMatch{}
				for _, it := range f {
					out[it.Seen[0].Finding] = ledgerMatch{Choice: "L1", P: 0.99, Restates: true}
				}
				return out
			}
			defer func() { gateLedgerMatch = nil }()
		}
		rec := ledgerRec("h2", "fail", "finding-b")
		before, _ := json.Marshal(rec)
		e.record(rec)
		after, _ := json.Marshal(rec)
		if string(before) != string(after) {
			t.Fatalf("record changed:\n%s\n%s", before, after)
		}
		led := e.load(t)
		if len(led.Items) != 2 || led.Items[1].ID != "L2" || led.Items[1].State != "open" {
			t.Fatalf("seam=%v: %+v", withSeam, led.Items)
		}
		return string(after), e.report(t), led.Items[1].ID
	}
	a1, r1, _ := run(false)
	a2, r2, _ := run(true)
	if a1 != a2 || r1 != r2 {
		t.Fatalf("seam changed the output:\n%s\n---\n%s", r1, r2)
	}
}

func TestLedgerResolveRefusals(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a", "finding-b", "finding-c"))
	e.h.must("gate", "resolve", "spec.md", "L3", "duplicate", "--of", "L1", "--reason", "same")
	cases := []struct {
		want int
		args []string
	}{
		{2, []string{"L1", "fixed", "--reason", "  "}},
		{2, []string{"L1", "fixed"}},
		{2, []string{"L1", "duplicate", "--reason", "x"}},
		{2, []string{"L1", "fixed", "--reason", "x", "--of", "L2"}},
		{2, []string{"L1", "wontfix", "--reason", "x"}},
		{2, []string{"L1", "fixed", "extra", "--reason", "x"}},
		{1, []string{"L9", "fixed", "--reason", "x"}},
		{1, []string{"L2", "duplicate", "--of", "L9", "--reason", "x"}},
		{1, []string{"L2", "duplicate", "--of", "L2", "--reason", "x"}},
		{1, []string{"L2", "duplicate", "--of", "L3", "--reason", "x"}}, // newer
	}
	for _, c := range cases {
		code, out := e.h.run(append([]string{"gate", "resolve", "spec.md"}, c.args...)...)
		if code != c.want {
			t.Errorf("%v: exit %d, want %d\n%s", c.args, code, c.want, out)
		}
	}
	e.record(ledgerRec("h2", "fail", "finding-d"))
	if code, out := e.h.run("gate", "resolve", "spec.md", "L2", "duplicate", "--of", "L4", "--reason", "x"); code != 1 || !strings.Contains(out, "newer") {
		t.Errorf("newer: %d %s", code, out)
	}
	if code, out := e.h.run("gate", "resolve", "spec.md", "L4", "duplicate", "--of", "L3", "--reason", "x"); code != 1 || !strings.Contains(out, "itself a duplicate") {
		t.Errorf("duplicate of a duplicate: %d %s", code, out)
	}
	if led := e.load(t); led.Items[1].State != "open" || led.Items[3].State != "open" {
		t.Fatalf("a refused resolve changed state: %+v", led.Items)
	}
}

func TestLedgerResolveWithoutALedgerFails(t *testing.T) {
	e := newLedgerEnv(t)
	for _, args := range [][]string{{"gate", "resolve", "spec.md"}, {"gate", "resolve", "spec.md", "L1", "fixed", "--reason", "x"}} {
		code, out := e.h.run(args...)
		if code != 1 || !strings.Contains(out, "no ledger for spec.md; run orch gate spec.md first") {
			t.Errorf("%v: %d %s", args, code, out)
		}
	}
	if code, _ := e.h.run("gate", "resolve"); code != 2 {
		t.Errorf("no args: %d", code)
	}
}

func TestLedgerResolvePrintsTransitionAndRecordsIt(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a", "finding-b"))
	if out := e.h.must("gate", "resolve", "spec.md", "L2", "duplicate", "--of", "L1", "--reason", " same thing "); strings.TrimSpace(out) != "L2: open -> duplicate" {
		t.Fatalf("%q", out)
	}
	it := e.load(t).Items[1]
	if it.State != "duplicate" || it.Of != "L1" || it.Reason != "same thing" || it.ClosedAt != e.h.a.now().Unix() || len(it.History) != 1 {
		t.Fatalf("%+v", it)
	}
	e.h.must("gate", "resolve", "spec.md", "L2", "open", "--reason", "wrong")
	it = e.load(t).Items[1]
	if it.State != "open" || it.Of != "" || it.ClosedAt != 0 || len(it.History) != 2 {
		t.Fatalf("%+v", it)
	}
}

func (e *ledgerEnv) edit(t *testing.T, fn func(*gateLedger)) {
	t.Helper()
	led := e.load(t)
	fn(led)
	if err := saveLedger(e.path, led); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerListOrderHidesTheMatchAndCutsLongLines(t *testing.T) {
	e := newLedgerEnv(t)
	rec := ledgerRec("h1", "fail", "finding-a", "finding-b", "finding-c", "finding-d", "finding-e", "finding-f")
	rec.Findings[0].Claim = strings.Repeat("long ", 100)
	e.record(rec)
	e.h.must("gate", "resolve", "spec.md", "L2", "fixed", "--reason", "f")
	e.h.must("gate", "resolve", "spec.md", "L3", "accepted", "--reason", "a")
	e.h.must("gate", "resolve", "spec.md", "L4", "rejected", "--reason", "r")
	e.h.must("gate", "resolve", "spec.md", "L5", "duplicate", "--of", "L1", "--reason", "d")
	e.edit(t, func(l *gateLedger) { l.Items[5].Match = &ledgerMatch{Choice: "none", P: 0.9, Decision: "secret-id"} })
	out := strings.Split(strings.TrimSpace(e.h.must("gate", "resolve", "spec.md")), "\n")
	if out[0] != "Ledger for spec.md: open 2, fixed 1, accepted 1, rejected 1, duplicate 1" {
		t.Fatalf("header %q", out[0])
	}
	var ids []string
	for _, l := range out[1:] {
		ids = append(ids, strings.Fields(l)[0])
	}
	if strings.Join(ids, " ") != "L1 L6 L2 L3 L4 L5" {
		t.Fatalf("order %v", ids)
	}
	if n := len([]rune(out[1])); n != 240 {
		t.Fatalf("long line is %d runes: %q", n, out[1])
	}
	if !strings.HasPrefix(out[2], "L6 open [blocker, adversary-a] runs 1, last h1: where finding-f: claim finding-f") {
		t.Fatalf("L6 %q", out[2])
	}
	if out[3] != "L2 fixed [blocker, adversary-a] runs 1, last h1: where finding-b: claim finding-b | f" || !strings.HasSuffix(out[6], " | of L1 | d") {
		t.Fatalf("closed lines %q / %q", out[3], out[6])
	}
	if strings.Contains(strings.Join(out, "\n"), "secret-id") || strings.Contains(strings.ToLower(strings.Join(out, "\n")), "match") {
		t.Fatal("list printed the match")
	}
}

func TestLedgerHintRule(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "f1", "f2", "f3", "f4", "f5", "f6", "f7"))
	e.h.must("gate", "resolve", "spec.md", "L2", "duplicate", "--of", "L1", "--reason", "d")
	e.h.must("gate", "resolve", "spec.md", "L6", "fixed", "--reason", "f")
	m := func(choice string, p float64) *ledgerMatch { return &ledgerMatch{Choice: choice, P: p} }
	e.edit(t, func(l *gateLedger) {
		l.Items[2].Match = m("L2", 0.86) // root of a duplicate
		l.Items[3].Match = m("L1", 0.69) // below the threshold
		l.Items[4].Match = m("none", 0.95)
		l.Items[5].Match = m("L1", 0.95) // closed
		l.Items[6].Match = m("L1", 0.7)  // on the threshold
	})
	e.h.a.now = func() time.Time { return time.Unix(1000, 0) }
	out := e.h.must("gate", "resolve", "spec.md")
	for line, want := range map[string]string{"L3 open": " | likely same as L1, 86%", "L7 open": " | likely same as L1, 70%"} {
		found := false
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, line) {
				found = true
				if !strings.HasSuffix(l, want) {
					t.Errorf("%s: %q", line, l)
				}
			}
		}
		if !found {
			t.Errorf("no %s line", line)
		}
	}
	if strings.Count(out, "likely same as") != 2 {
		t.Fatalf("hints:\n%s", out)
	}
	led := e.load(t)
	for i, want := range map[int]int64{2: 1000, 3: 0, 4: 0, 5: 0, 6: 1000} {
		if got := led.Items[i].Match.HintAt; got != want {
			t.Errorf("L%d hint_at %d, want %d", i+1, got, want)
		}
	}
	e.h.a.now = func() time.Time { return time.Unix(2000, 0) }
	e.h.must("gate", "resolve", "spec.md")
	if got := e.load(t).Items[2].Match.HintAt; got != 1000 {
		t.Fatalf("second list moved hint_at to %d", got)
	}
}

func TestLedgerReportSection(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a", "finding-b"))
	want := "## Findings ledger\n" +
		"Ledger for spec.md: open 2, fixed 0, accepted 0, rejected 0, duplicate 0.\n" +
		"This run: new L1, L2.\n" +
		"L1 [blocker, adversary-a] where finding-a: claim finding-a\n" +
		"L2 [blocker, adversary-a] where finding-b: claim finding-b\n" +
		"Close an item: orch gate resolve spec.md <L-id> fixed|accepted|rejected|duplicate --reason \"...\" [--of <L-id>]\n\n" +
		"## Question cards\nsummary\n"
	if got := e.report(t); got != "# Gate report\n\nbody\n\n"+want {
		t.Fatalf("report:\n%s", got)
	}
	// A second run replaces the section rather than stacking it.
	e.record(ledgerRec("h2", "fail", "finding-b", "finding-c"))
	got := e.report(t)
	if strings.Count(got, "## Findings ledger") != 1 || !strings.Contains(got, "This run: new L3; seen again L2.\n") ||
		!strings.Contains(got, "Older open items not seen in this run: 1 (list: orch gate resolve spec.md).\n") ||
		strings.Contains(got, "L1 [blocker") || strings.Index(got, "## Findings ledger") > strings.Index(got, "## Question cards") {
		t.Fatalf("second report:\n%s", got)
	}
	// Rewriting the question cards keeps the ledger section and replaces only its own.
	if err := addReportSection(filepath.Join(e.dir, "report.md"), &enrichResult{}); err != nil {
		t.Fatal(err)
	}
	after := e.report(t)
	if strings.Count(after, "## Findings ledger") != 1 || strings.Count(after, "## Question cards") != 1 || strings.Contains(after, "\nsummary\n") {
		t.Fatalf("after addReportSection:\n%s", after)
	}
	if !strings.HasPrefix(after, "# Gate report\n\nbody\n\n## Findings ledger\n") {
		t.Fatalf("ledger section moved:\n%s", after)
	}
}

func TestSetReportSectionAppendsWhenThereAreNoQuestionCards(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	writeFiles(t, dir, map[string]string{"report.md": "# R\n\n## Other\nx\n"})
	if err := setReportSection(path, "## Findings ledger", "line\n"); err != nil {
		t.Fatal(err)
	}
	if err := setReportSection(path, "## Findings ledger", "again\n"); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, path)); got != "# R\n\n## Other\nx\n\n## Findings ledger\nagain\n" {
		t.Fatalf("%q", got)
	}
	// A section in the middle is replaced up to the next heading only.
	writeFiles(t, dir, map[string]string{"report.md": "# R\n\n## Findings ledger\nold\n\n## After\nkeep\n"})
	if err := setReportSection(path, "## Findings ledger", "new\n"); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, path)); got != "# R\n\n## Findings ledger\nnew\n\n## After\nkeep\n" {
		t.Fatalf("%q", got)
	}
}

func TestLedgerErrorIsReportedAndLeavesTheFileAlone(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a"))
	bad := []byte("{not json")
	if err := os.WriteFile(e.path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	e.h.errw.Reset()
	e.record(ledgerRec("h2", "fail", "finding-b"))
	stderr := e.h.errw.String()
	if !strings.Contains(stderr, "orch: gate ledger for spec.md not updated: gate ledger "+e.path) {
		t.Fatalf("stderr %q", stderr)
	}
	if got := e.report(t); !strings.Contains(got, "## Findings ledger\nLedger not updated: gate ledger "+e.path) || strings.Contains(got, "Ledger for spec.md") {
		t.Fatalf("report:\n%s", got)
	}
	for _, args := range [][]string{{"gate", "resolve", "spec.md"}, {"gate", "resolve", "spec.md", "L1", "fixed", "--reason", "x"}} {
		if code, out := e.h.run(args...); code != 1 || !strings.Contains(out, e.path) {
			t.Errorf("%v: %d %s", args, code, out)
		}
	}
	if got := mustRead(t, e.path); string(got) != string(bad) {
		t.Fatalf("file rewritten: %q", got)
	}
}

func TestLedgerOutcomeSeamRunsOnTheFirstCloseOnly(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a"))
	var calls []string
	var sawState string
	gateLedgerOutcome = func(root string, led gateLedger, it ledgerItem, state, of string) error {
		calls = append(calls, it.ID+"->"+state)
		sawState = led.item(it.ID).State
		// The seam runs before the save.
		if got := e.load(t).Items[0].State; got != "open" {
			t.Errorf("outcome ran after the save: %s", got)
		}
		return nil
	}
	t.Cleanup(func() { gateLedgerOutcome = nil })
	e.h.must("gate", "resolve", "spec.md", "L1", "accepted", "--reason", "a")
	e.h.must("gate", "resolve", "spec.md", "L1", "open", "--reason", "back")
	e.h.must("gate", "resolve", "spec.md", "L1", "fixed", "--reason", "later")
	e.h.must("gate", "resolve", "spec.md", "L1", "rejected", "--reason", "again")
	if strings.Join(calls, ",") != "L1->accepted" || sawState != "open" {
		t.Fatalf("outcome calls %v", calls)
	}
}

func TestLedgerOutcomeErrorDoesNotStopTheSaveAndFailedSaveLeavesOneCall(t *testing.T) {
	e := newLedgerEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a"))
	calls := 0
	gateLedgerOutcome = func(root string, led gateLedger, it ledgerItem, state, of string) error {
		calls++
		return fmt.Errorf("store down")
	}
	t.Cleanup(func() { gateLedgerOutcome = nil })
	// A save that fails: the ledger dir is not writable.
	dir := filepath.Dir(e.path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe")
	if f, err := os.Create(probe); err == nil {
		f.Close()
		os.Remove(probe)
		os.Chmod(dir, 0o755)
		t.Skip("directory permissions are not enforced here")
	}
	code, _ := e.h.run("gate", "resolve", "spec.md", "L1", "accepted", "--reason", "a")
	os.Chmod(dir, 0o755)
	if code != 1 || calls != 1 || e.load(t).Items[0].State != "open" {
		t.Fatalf("failed save: exit %d, calls %d", code, calls)
	}
	// The retry saves; the stub (like the real store) records nothing new.
	code, out := e.h.run("gate", "resolve", "spec.md", "L1", "accepted", "--reason", "a")
	if code != 0 || !strings.Contains(out, "orch: gate ledger outcome for L1 not recorded: store down") || !strings.Contains(out, "L1: open -> accepted") || e.load(t).Items[0].State != "accepted" {
		t.Fatalf("retry: %d %s", code, out)
	}
}
