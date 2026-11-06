package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// matchEnv is a ledger env whose gate_match role resolves to the decider's api
// route, with the real seams installed and a fake decider underneath.
func matchEnv(t *testing.T) *ledgerEnv {
	t.Helper()
	t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
	oldM, oldO := gateLedgerMatch, gateLedgerOutcome
	gateLedgerMatch, gateLedgerOutcome = gateMatchAsk, gateMatchOutcome
	t.Cleanup(func() { gateLedgerMatch, gateLedgerOutcome = oldM, oldO })
	return newLedgerEnv(t)
}

func matchAnswers(choice string, p, noul float64) map[string]json.RawMessage {
	if choice == "none" {
		return answers("restates", fmt.Sprintf(`{"noul":%g}`, noul), "which", fmt.Sprintf(`{"choice":"none","probabilities":{"none":%g}}`, p))
	}
	return answers(
		"restates", fmt.Sprintf(`{"noul":%g}`, noul),
		"which", fmt.Sprintf(`{"choice":%q,"probabilities":{%q:%g,"none":%g}}`, choice, choice, p, 1-p),
	)
}

func matchItem(n int, check string, seenAt int64) ledgerItem {
	id := fmt.Sprintf("L%d", n)
	return ledgerItem{ID: id, State: "open", Check: check, Where: "w" + id, Claim: "c" + id, Evidence: "e" + id,
		Seen: []ledgerSighting{{Hash: "h", At: seenAt, Finding: "f" + id}}}
}

func freshItem(finding, check string) ledgerItem {
	return ledgerItem{ID: "", State: "open", Check: check, Where: "fresh where", Claim: "fresh claim", Evidence: "fresh.go:1", Why: "secret why",
		Seen: []ledgerSighting{{Hash: "h9", At: 9, Finding: finding}}}
}

func TestMatchAskSendsOnlyWhereClaimEvidenceAndBothQuestions(t *testing.T) {
	e := matchEnv(t)
	var state, questions map[string]any
	old := decisionCall
	decisionCall = func(ctx context.Context, p profile, st, q map[string]any) (map[string]json.RawMessage, string, shadowUsage, error) {
		state, questions = st, q
		return matchAnswers("L1", 0.9, 0.9), "m", shadowUsage{}, nil
	}
	t.Cleanup(func() { decisionCall = old })
	long := matchItem(1, "", 1)
	long.Claim = strings.Repeat("x", 900)
	got := gateMatchAsk(context.Background(), e.s, "spec.md", []ledgerItem{long}, []ledgerItem{freshItem("fnew", "")})

	if len(state) != 3 || state["where"] != "fresh where" || state["claim"] != "fresh claim" || state["evidence"] != "fresh.go:1" {
		t.Fatalf("state %v", state)
	}
	r, w := questions["restates"].(map[string]any), questions["which"].(map[string]any)
	if r["type"] != "noul" || w["type"] != "choice" || len(questions) != 2 {
		t.Fatalf("questions %v", questions)
	}
	crit := w["criteria"].(map[string]any)
	if v := crit["L1"].(string); len([]rune(v)) != 600 || !strings.HasPrefix(v, "wL1: ") {
		t.Fatalf("candidate criterion has %d chars: %.40s", len([]rune(v)), v)
	}
	if crit["none"] == nil || len(crit) != 2 {
		t.Fatalf("criteria %v", crit)
	}
	m := got["fnew"]
	if m.Choice != "L1" || m.P != 0.9 || !m.Restates || m.Decision == "" || m.HintAt != 0 || len(m.Candidates) != 1 {
		t.Fatalf("answer %+v", m)
	}
	recs := readDecisions(t, e.s, "gate_match")
	if len(recs) != 1 || recs[0].Subject != "spec.md" || recs[0].Meta["finding"] != "fnew" || recs[0].Meta["gate_hash"] != "h9" {
		t.Fatalf("record %+v", recs)
	}
}

func TestMatchCandidatesSameCheckFirstThenLatestThenLowerNumberCappedAtTwelve(t *testing.T) {
	var items []ledgerItem
	for n := 1; n <= 14; n++ {
		items = append(items, matchItem(n, "other", 5))
	}
	items[9].Check = "mine"   // L10 shares the check but was seen long ago
	items[12].Seen[0].At = 50 // L13 was seen latest
	items[3].Seen[0].At = 50  // L4 ties L13 and has the lower number
	got := gateMatchCandidates(items, "mine")
	var ids []string
	for _, it := range got {
		ids = append(ids, it.ID)
	}
	if len(got) != 12 || strings.Join(ids[:4], ",") != "L10,L4,L13,L1" {
		t.Fatalf("order %v", ids)
	}
}

func TestMatchCallsRunOneAtATimeAndStopAtTheRunDeadline(t *testing.T) {
	e := matchEnv(t)
	defer func(r time.Duration) { gateMatchRunTimeout = r }(gateMatchRunTimeout)
	gateMatchRunTimeout = 30 * time.Millisecond
	var inflight, peak, calls atomic.Int32
	old := decisionCall
	decisionCall = func(ctx context.Context, p profile, st, q map[string]any) (map[string]json.RawMessage, string, shadowUsage, error) {
		calls.Add(1)
		if n := inflight.Add(1); n > peak.Load() {
			peak.Store(n)
		}
		defer inflight.Add(-1)
		<-ctx.Done() // the first call holds until the run deadline
		return nil, "", shadowUsage{}, ctx.Err()
	}
	t.Cleanup(func() { decisionCall = old })
	var fresh []ledgerItem
	for i := range 5 {
		fresh = append(fresh, freshItem(fmt.Sprintf("f%d", i), ""))
	}
	got := gateMatchAsk(context.Background(), e.s, "spec.md", []ledgerItem{matchItem(1, "", 1)}, fresh)
	if calls.Load() != 1 || peak.Load() != 1 || len(got) != 0 {
		t.Fatalf("calls %d peak %d answers %v", calls.Load(), peak.Load(), got)
	}
	if recs := readDecisions(t, e.s, "gate_match"); len(recs) != 1 || recs[0].Reason != "timeout" {
		t.Fatalf("records %+v", recs)
	}
}

func TestMatchStopsAskingPastTheFindingCap(t *testing.T) {
	e := matchEnv(t)
	defer func(n int) { gateMatchMaxFindings = n }(gateMatchMaxFindings)
	gateMatchMaxFindings = 2
	old := decisionCall
	decisionCall = func(ctx context.Context, p profile, st, q map[string]any) (map[string]json.RawMessage, string, shadowUsage, error) {
		return matchAnswers("none", 0.9, 0.1), "m", shadowUsage{}, nil
	}
	t.Cleanup(func() { decisionCall = old })
	var fresh []ledgerItem
	for i := range 4 {
		fresh = append(fresh, freshItem(fmt.Sprintf("f%d", i), ""))
	}
	got := gateMatchAsk(context.Background(), e.s, "spec.md", []ledgerItem{matchItem(1, "", 1)}, fresh)
	if len(got) != 2 || got["f0"].Decision == "" || got["f1"].Decision == "" {
		t.Fatalf("answers %v", got)
	}
}

func TestMatchSlowCallTimesOutAndItemIsStillCreatedWithoutMatch(t *testing.T) {
	e := matchEnv(t)
	defer func(d time.Duration) { gateMatchCallTimeout = d }(gateMatchCallTimeout)
	gateMatchCallTimeout = 20 * time.Millisecond
	fakeDecider(t, func(ctx context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		<-ctx.Done()
		return nil, shadowUsage{}, ctx.Err()
	})
	e.record(ledgerRec("h1", "fail", "finding-a"))
	e.record(ledgerRec("h2", "fail", "finding-b"))
	led := e.load(t)
	if len(led.Items) != 2 || led.Items[1].Match != nil {
		t.Fatalf("items %+v", led.Items)
	}
	if recs := readDecisions(t, e.s, "gate_match"); len(recs) != 1 || recs[0].Reason != "timeout" {
		t.Fatalf("records %+v", recs)
	}
}

func TestMatchFailuresAreRecordedWithTheirReasonAndYieldNoAnswer(t *testing.T) {
	e := matchEnv(t)
	cases := []struct {
		reason string
		ans    map[string]json.RawMessage
		err    error
	}{
		{"http_500", nil, &decisionError{"http_500", errors.New("status 500")}},
		{"invalid_answer", answers("restates", `{"noul":0.9}`, "which", `{"choice":"L1"}`), nil},
	}
	for _, c := range cases {
		fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
			return c.ans, shadowUsage{}, c.err
		})
		got := gateMatchAsk(context.Background(), e.s, "spec.md", []ledgerItem{matchItem(1, "", 1)}, []ledgerItem{freshItem("fx", "")})
		if len(got) != 0 {
			t.Fatalf("%s: answer %v", c.reason, got)
		}
	}
	recs := readDecisions(t, e.s, "gate_match")
	if len(recs) != 2 || recs[0].Reason != "http_500" || recs[1].Reason != "invalid_answer" {
		t.Fatalf("records %+v", recs)
	}
}

func TestMatchDisabledRoleWritesNoRecord(t *testing.T) {
	e := matchEnv(t)
	off := false
	e.s.cfg.Roles = map[string]modelRole{"gate_match": {Enabled: &off}}
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		t.Fatal("decider called for a disabled role")
		return nil, shadowUsage{}, nil
	})
	got := gateMatchAsk(context.Background(), e.s, "spec.md", []ledgerItem{matchItem(1, "", 1)}, []ledgerItem{freshItem("fx", "")})
	if len(got) != 0 {
		t.Fatalf("answers %v", got)
	}
	if _, err := os.Stat(decisionFile(e.s.root, "gate_match")); err == nil {
		t.Fatal("decision file written for a disabled role")
	}
}

func (e *ledgerEnv) matchOutcomes(t *testing.T) []decisionOutcome {
	t.Helper()
	outs, err := readJSONLines[decisionOutcome](decisionOutcomeFile(e.s.root, "gate_match"))
	if err != nil {
		t.Fatal(err)
	}
	return outs
}

// matchedLedger leaves L1, L2 open and L3 open with a match on choice, p 0.86.
func matchedLedger(t *testing.T, choice string) *ledgerEnv {
	e := matchEnv(t)
	e.record(ledgerRec("h1", "fail", "finding-a", "finding-b"))
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return matchAnswers(choice, 0.86, 0.9), shadowUsage{}, nil
	})
	e.record(ledgerRec("h2", "fail", "finding-c"))
	if m := e.load(t).Items[2].Match; m == nil || m.Choice != choice || m.P != 0.86 {
		t.Fatalf("L3 match %+v", m)
	}
	return e
}

func TestMatchOutcomeDuplicateCloseConfirmingTheChoiceWithoutHint(t *testing.T) {
	e := matchedLedger(t, "L1")
	e.h.must("gate", "resolve", "spec.md", "L3", "duplicate", "--of", "L1", "--reason", "same")
	outs := e.matchOutcomes(t)
	if len(outs) != 1 {
		t.Fatalf("outcomes %+v", outs)
	}
	o := outs[0].Outcome
	want := map[string]any{"duplicate": true, "of": "L1", "state": "duplicate", "choice": "L1", "of_offered": true,
		"choice_correct": true, "hint": false, "confirmed": true, "confirmed_unhinted": true}
	for k, v := range want {
		if o[k] != v {
			t.Errorf("%s = %v, want %v", k, o[k], v)
		}
	}
	if _, ok := o["confirmed_hinted"]; ok || len(o) != len(want) {
		t.Fatalf("outcome %v", o)
	}
	if outs[0].ID != e.load(t).Items[2].Match.Decision {
		t.Fatal("outcome is not keyed by the decision")
	}
}

func TestMatchOutcomeDuplicateCloseAfterHintIsHinted(t *testing.T) {
	e := matchedLedger(t, "L1")
	if out := e.h.must("gate", "resolve", "spec.md"); !strings.Contains(out, "likely same as L1, 86%") {
		t.Fatalf("no hint in list:\n%s", out)
	}
	e.h.must("gate", "resolve", "spec.md", "L3", "duplicate", "--of", "L1", "--reason", "same")
	o := e.matchOutcomes(t)[0].Outcome
	if o["hint"] != true || o["confirmed"] != true || o["confirmed_hinted"] != true {
		t.Fatalf("outcome %v", o)
	}
	if _, ok := o["confirmed_unhinted"]; ok {
		t.Fatalf("outcome %v", o)
	}
}

func TestMatchOutcomeDuplicateOfAnotherItemIsWrongAndNotConfirmed(t *testing.T) {
	e := matchedLedger(t, "L1")
	e.h.must("gate", "resolve", "spec.md", "L3", "duplicate", "--of", "L2", "--reason", "other")
	o := e.matchOutcomes(t)[0].Outcome
	if o["duplicate"] != true || o["choice_correct"] != false || o["confirmed"] != false || o["confirmed_unhinted"] != false {
		t.Fatalf("outcome %v", o)
	}
}

func TestMatchOutcomeRejectedCloseIsNotADuplicate(t *testing.T) {
	e := matchedLedger(t, "none")
	e.h.must("gate", "resolve", "spec.md", "L3", "rejected", "--reason", "not real")
	o := e.matchOutcomes(t)[0].Outcome
	if o["duplicate"] != false || o["of"] != "" || o["state"] != "rejected" || o["choice_correct"] != true || o["confirmed"] != false || o["of_offered"] != false {
		t.Fatalf("outcome %v", o)
	}
}

func TestMatchOutcomeChoiceLaterClosedAsDuplicateCountsAsItsRoot(t *testing.T) {
	e := matchedLedger(t, "L2")
	e.h.must("gate", "resolve", "spec.md", "L2", "duplicate", "--of", "L1", "--reason", "same")
	e.h.must("gate", "resolve", "spec.md", "L3", "duplicate", "--of", "L1", "--reason", "same")
	o := e.matchOutcomes(t)[0].Outcome
	if o["choice"] != "L2" || o["choice_correct"] != true || o["confirmed"] != true {
		t.Fatalf("outcome %v", o)
	}
}

func TestMatchOutcomeSecondCloseRecordsNothing(t *testing.T) {
	e := matchedLedger(t, "L1")
	e.h.must("gate", "resolve", "spec.md", "L3", "accepted", "--reason", "known")
	e.h.must("gate", "resolve", "spec.md", "L3", "rejected", "--reason", "changed my mind")
	outs := e.matchOutcomes(t)
	if len(outs) != 1 || outs[0].Outcome["state"] != "accepted" {
		t.Fatalf("outcomes %+v", outs)
	}
}

func TestMatchOutcomeItemWithoutMatchRecordsNothing(t *testing.T) {
	e := matchedLedger(t, "L1")
	e.h.must("gate", "resolve", "spec.md", "L1", "fixed", "--reason", "patched")
	if outs := e.matchOutcomes(t); len(outs) != 0 {
		t.Fatalf("outcomes %+v", outs)
	}
}

func TestMatchReportShowsTheThreeMetrics(t *testing.T) {
	e := matchedLedger(t, "L1")
	out := e.h.must("decisions", "report", "--use", "gate_match")
	for _, line := range []string{
		"metric restates (restates.noul vs duplicate)",
		"metric confirmed-hinted (restates.noul vs confirmed_hinted)",
		"metric confirmed-unhinted (restates.noul vs confirmed_unhinted)",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("report lacks %q:\n%s", line, out)
		}
	}
}

func TestMatchRoleDefaultsToDeciderConnectorsAndIsApiOnly(t *testing.T) {
	h := setup(t)
	t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
	want := site{}.role("decider").Use
	got := site{}.role("gate_match")
	if len(want) != 1 || len(got.Use) != 1 || got.Use[0] != want[0] || !roleEnabled(got) {
		t.Fatalf("gate_match %+v, decider %+v", got, want)
	}
	path := filepath.Join(h.tmp, "match-roles.toml")
	t.Setenv("ORCH_CONFIG", path)
	_ = os.WriteFile(path, []byte("[roles.gate_match]\nenabled=false\nuse=[{harness=\"api\",provider=\"openrouter\",model=\"m\"}]\n"), 0o600)
	cfg, err := loadConfig(h.repo)
	if err != nil || roleEnabled((site{cfg: cfg}).role("gate_match")) {
		t.Fatalf("enabled=false not honoured: %v", err)
	}
	for _, bad := range []string{"claude", "pi"} {
		_ = os.WriteFile(path, []byte("[roles.gate_match]\nuse=[{harness=\""+bad+"\",model=\"m\"}]\n"), 0o600)
		if _, err := loadConfig(h.repo); err == nil {
			t.Fatalf("gate_match accepted harness %s", bad)
		}
	}
}
