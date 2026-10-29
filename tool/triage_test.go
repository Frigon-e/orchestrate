package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func triageAnswersFor(v decVals) triageAnswers {
	var r struct {
		Answers map[string]decAnswer `json:"answers"`
	}
	json.Unmarshal([]byte(decReply(v)), &r)
	return triageAnswersOf(r.Answers)
}

func TestTierFor(t *testing.T) {
	with := func(v decVals, f func(*decVals)) decVals { f(&v); return v }
	cases := []struct {
		name string
		v    decVals
		want string
	}{
		{"easy and verifiable", settledAnswers, "settled"},
		{"medium", standardAnswers, "standard"},
		{"hard and unclear", planningAnswers, "planning"},
		// P(difficulty=3) 0.3 sits on the planning line; 0.29 is just under it.
		{"P(hard) on the line", with(settledAnswers, func(v *decVals) { v.d0, v.d3 = 0.7, 0.3 }), "planning"},
		{"P(hard) just under the line", with(settledAnswers, func(v *decVals) { v.d0, v.d3 = 0.71, 0.29 }), "settled"},
		{"P(cause known) just under the line", with(settledAnswers, func(v *decVals) { v.cause = 0.19 }), "planning"},
		{"P(cause known) on the line", with(settledAnswers, func(v *decVals) { v.cause = 0.2 }), "settled"},
		// Expected difficulty 1.5 is not below 1.5; 1.48 is.
		{"difficulty on the settled line", with(settledAnswers, func(v *decVals) { v.d0, v.d1, v.d2 = 0.25, 0, 0.75 }), "standard"},
		{"difficulty just under the settled line", with(settledAnswers, func(v *decVals) { v.d0, v.d1, v.d2 = 0.26, 0, 0.74 }), "settled"},
		{"P(verifiable) on the line", with(settledAnswers, func(v *decVals) { v.verifiable = 0.8 }), "settled"},
		{"P(verifiable) just under the line", with(settledAnswers, func(v *decVals) { v.verifiable = 0.79 }), "standard"},
		{"low confidence keeps settled", with(settledAnswers, func(v *decVals) { v.conf = 0.1 }), "settled"},
		{"low confidence keeps standard", with(standardAnswers, func(v *decVals) { v.conf = 0.1 }), "standard"},
		{"open-ended alone does not force planning", with(settledAnswers, func(v *decVals) { v.clear, v.guided, v.open = 0.1, 0.1, 0.8 }), "settled"},
		{"cross-cutting alone does not block settled", with(settledAnswers, func(v *decVals) { v.cross = 0.9 }), "settled"},
	}
	for _, c := range cases {
		if got, why := tierFor(triageAnswersFor(c.v)); got != c.want {
			t.Errorf("%s: tier = %s (%s), want %s", c.name, got, why, c.want)
		}
	}
}

func TestTriageRecordCarriesTheDecision(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.dec.set(200, decReply(standardAnswers))
	h.must("triage", "t1")
	root := filepath.Dir(h.dir("t1"))
	decs, err := readJSONLines[decisionRecord](decisionFile(root, "triage"))
	if err != nil || len(decs) != 1 {
		t.Fatalf("decisions = %+v, err %v", decs, err)
	}
	d := decs[0]
	if d.Use != "triage" || d.Role != "decider" || d.Subject != "t1" || d.Action != "standard" || d.Reason != "" {
		t.Fatalf("decision = %+v", d)
	}
	recs := readTriage(root)
	if len(recs) != 1 || recs[0].Decision != d.ID || recs[0].Tier != "standard" {
		t.Fatalf("triage records = %+v, want decision %s", recs, d.ID)
	}
}

func TestTriageOutcomeResolverSplitsSignals(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.dec.set(200, decReply(settledAnswers))
	h.must("triage", "t1")
	s, _ := h.a.site()
	dir := h.dir("t1")
	h.a.appendStatus(dir, "escalate", "need a design call", "", "")
	h.a.appendStatus(dir, "note", "stalled: no report", "", "orch")
	h.a.appendStatus(dir, "note", "verify fail: check exit 1", "", "orch")
	h.a.appendStatus(dir, "done", "ok", "", "")

	resolveOutcomes(s)
	if outs, _ := readJSONLines[decisionOutcome](decisionOutcomeFile(s.root, "triage")); len(outs) != 0 {
		t.Fatalf("a task that is not closed got an outcome: %+v", outs)
	}

	h.a.appendStatus(dir, "note", "closed by orchestrator", "", "orch")
	resolveOutcomes(s)
	outs, _ := readJSONLines[decisionOutcome](decisionOutcomeFile(s.root, "triage"))
	rec := readTriage(s.root)[0]
	if len(outs) != 1 || outs[0].ID != rec.Decision {
		t.Fatalf("outcomes = %+v, want one for %s", outs, rec.Decision)
	}
	o := outs[0].Outcome
	toStrings := func(v any) []string {
		var out []string
		for _, x := range v.([]any) {
			out = append(out, x.(string))
		}
		return out
	}
	if o["outcome"] != "hard" || o["hard"] != true || o["tier"] != "settled" ||
		!reflect.DeepEqual(toStrings(o["lead_signals"]), []string{"escalated"}) ||
		!reflect.DeepEqual(toStrings(o["run_signals"]), []string{"stalled", "verify failed"}) {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestTriageRespawnGetsNoOutcomeUntilClose(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.dec.set(200, decReply(settledAnswers))
	h.must("triage", "t1")
	s, _ := h.a.site()
	dir := h.dir("t1")
	h.a.appendStatus(dir, "done", "first lead", "", "")
	h.a.appendStatus(dir, "note", "closed for respawn on pi", "", "orch")
	resolveOutcomes(s)
	if outs, _ := readJSONLines[decisionOutcome](decisionOutcomeFile(s.root, "triage")); len(outs) != 0 {
		t.Fatalf("a respawn wrote an outcome: %+v", outs)
	}
	h.a.appendStatus(dir, "done", "second lead", "", "")
	h.a.appendStatus(dir, "note", "closed by orchestrator", "", "orch")
	resolveOutcomes(s)
	outs, _ := readJSONLines[decisionOutcome](decisionOutcomeFile(s.root, "triage"))
	if len(outs) != 1 || outs[0].Outcome["outcome"] != "hard" {
		t.Fatalf("outcomes after close = %+v, want one hard (respawned)", outs)
	}
}

func TestTriageTwiceJoinsTheLatestDecision(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.dec.set(200, decReply(settledAnswers))
	h.must("triage", "t1")
	h.dec.set(200, decReply(planningAnswers))
	h.must("triage", "t1", "--force")
	s, _ := h.a.site()
	recs := readTriage(s.root)
	if len(recs) != 2 || recs[0].Decision == recs[1].Decision {
		t.Fatalf("records = %+v", recs)
	}
	dir := h.dir("t1")
	h.a.appendStatus(dir, "done", "ok", "", "")
	h.a.appendStatus(dir, "note", "closed by orchestrator", "", "orch")
	resolveOutcomes(s)
	outs, _ := readJSONLines[decisionOutcome](decisionOutcomeFile(s.root, "triage"))
	if len(outs) != 1 || outs[0].ID != recs[1].Decision || outs[0].Outcome["tier"] != "planning" || outs[0].Outcome["outcome"] != "clean" {
		t.Fatalf("outcomes = %+v, want one clean for %s", outs, recs[1].Decision)
	}
}
