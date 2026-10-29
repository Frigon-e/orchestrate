package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func decisionTestSite(t *testing.T, enabled bool) site {
	t.Helper()
	var s site
	s.root = t.TempDir()
	s.cfg.Roles = map[string]modelRole{"decider": {
		Enabled: &enabled,
		Use:     []profile{{Harness: "api", Provider: "openrouter", Model: "m"}},
	}}
	return s
}

func fakeDecider(t *testing.T, fn func(ctx context.Context) (map[string]json.RawMessage, shadowUsage, error)) {
	t.Helper()
	old := decisionCall
	decisionCall = func(ctx context.Context, p profile, state, q map[string]any) (map[string]json.RawMessage, string, shadowUsage, error) {
		a, u, err := fn(ctx)
		return a, "served-model", u, err
	}
	t.Cleanup(func() { decisionCall = old })
}

func answers(kv ...string) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = json.RawMessage(kv[i+1])
	}
	return m
}

func decisionReq(use string) decisionRequest {
	return decisionRequest{
		Use: use, Role: "decider", Subject: "task-1",
		State:     map[string]any{"text": "hello"},
		Questions: map[string]any{"q": "?"},
		Keys:      []string{"q"},
		Fields:    map[string][]string{"q": {"noul"}},
		Meta:      map[string]any{"idle_min": 7},
	}
}

func readDecisions(t *testing.T, s site, use string) []decisionRecord {
	t.Helper()
	recs, err := readJSONLines[decisionRecord](decisionFile(s.root, use))
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestDecisionMissingKeyIsInvalidNotZero(t *testing.T) {
	s := decisionTestSite(t, true)
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return answers("q", `{"type":"scalar"}`), shadowUsage{}, nil
	})
	_, err := askDecision(context.Background(), s, decisionReq("u"))
	if err == nil || !strings.Contains(err.Error(), `"noul"`) {
		t.Fatalf("want missing noul error, got %v", err)
	}
	recs := readDecisions(t, s, "u")
	if len(recs) != 1 || recs[0].Reason != "invalid_answer" {
		t.Fatalf("want one invalid_answer record, got %+v", recs)
	}
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return answers("other", `{"noul":0.5}`), shadowUsage{}, nil
	})
	if _, err := askDecision(context.Background(), s, decisionReq("u")); err == nil {
		t.Fatal("missing key q must be invalid")
	}
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return answers("q", `{"noul":0}`), shadowUsage{}, nil
	})
	if _, err := askDecision(context.Background(), s, decisionReq("u")); err != nil {
		t.Fatalf("an explicit zero is valid: %v", err)
	}
}

func TestDecisionFailuresRecordReason(t *testing.T) {
	s := decisionTestSite(t, true)
	fakeDecider(t, func(ctx context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		<-ctx.Done()
		return nil, shadowUsage{}, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := askDecision(ctx, s, decisionReq("fail")); err == nil {
		t.Fatal("timeout must return an error")
	}
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return nil, shadowUsage{}, &decisionError{"http_500", errors.New("decisions API returned 500: boom")}
	})
	if _, err := askDecision(context.Background(), s, decisionReq("fail")); err == nil {
		t.Fatal("HTTP 500 must return an error")
	}
	recs := readDecisions(t, s, "fail")
	if len(recs) != 2 || recs[0].Reason != "timeout" || recs[1].Reason != "http_500" || recs[1].Error == "" {
		t.Fatalf("reasons not recorded: %+v", recs)
	}
}

func TestDecisionDisabledRoleWritesNothing(t *testing.T) {
	s := decisionTestSite(t, false)
	called := false
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		called = true
		return nil, shadowUsage{}, nil
	})
	_, err := askDecision(context.Background(), s, decisionReq("off"))
	if !errors.Is(err, errDecisionDisabled) || called {
		t.Fatalf("err %v called %v", err, called)
	}
	if _, serr := os.Stat(decisionsDir(s.root)); !os.IsNotExist(serr) {
		t.Fatalf("decisions/ must not exist: %v", serr)
	}
}

func TestDecisionRecordHasCostLatencyAndCreatesDir(t *testing.T) {
	s := decisionTestSite(t, true)
	if _, err := os.Stat(decisionsDir(s.root)); !os.IsNotExist(err) {
		t.Fatal("decisions/ must start absent")
	}
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		time.Sleep(15 * time.Millisecond)
		return answers("q", `{"noul":0.25}`), shadowUsage{Cost: 0.0004, Tokens: 321}, nil
	})
	req := decisionReq("ok")
	req.State = map[string]any{"text": strings.Repeat("x", 5000)}
	req.Action = func(r decisionResult) string { return fmt.Sprintf("tier-%.2f", r.Answers["q"].Noul) }
	res, err := askDecision(context.Background(), s, req)
	if err != nil || res.ID == "" || res.Action != "tier-0.25" || res.Answers["q"].Noul != 0.25 {
		t.Fatalf("res %+v err %v", res, err)
	}
	recs := readDecisions(t, s, "ok")
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.ID != res.ID || r.Cost != 0.0004 || r.Tokens != 321 || r.LatencyMS < 15 || r.Model != "served-model" ||
		r.Action != "tier-0.25" || r.Subject != "task-1" || r.Meta["idle_min"] != float64(7) || r.State == "" || r.Error != "" {
		t.Fatalf("record %+v", r)
	}
	if got := len([]rune(capDecisionState(req.State)["text"].(string))); got != decisionStateFieldCap {
		t.Fatalf("state field cap %d", got)
	}
}

func TestDecisionNilActionIsShadow(t *testing.T) {
	s := decisionTestSite(t, true)
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return answers("q", `{"noul":1}`), shadowUsage{}, nil
	})
	if _, err := askDecision(context.Background(), s, decisionReq("sh")); err != nil {
		t.Fatal(err)
	}
	if got := readDecisions(t, s, "sh")[0].Action; got != "shadow" {
		t.Fatalf("action %q", got)
	}
}

func TestDecisionConcurrentAppendsLand(t *testing.T) {
	s := decisionTestSite(t, true)
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return answers("q", `{"noul":0.5}`), shadowUsage{}, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := askDecision(context.Background(), s, decisionReq("conc")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ids := map[string]bool{}
	for _, r := range readDecisions(t, s, "conc") {
		ids[r.ID] = true
	}
	if len(ids) != 20 {
		t.Fatalf("want 20 distinct records, got %d", len(ids))
	}
}

func TestDecisionSecondOutcomeIgnored(t *testing.T) {
	root := t.TempDir()
	if err := recordDecisionOutcome(root, "u", "id1", map[string]any{"hit": true}); err != nil {
		t.Fatal(err)
	}
	if err := recordDecisionOutcome(root, "u", "id1", map[string]any{"hit": false}); err != nil {
		t.Fatal(err)
	}
	outs, _ := readJSONLines[decisionOutcome](decisionOutcomeFile(root, "u"))
	if len(outs) != 1 || outs[0].Outcome["hit"] != true {
		t.Fatalf("outcomes %+v", outs)
	}
}

func TestDecisionAUCAndCalibration(t *testing.T) {
	xs := []scored{{0.9, true}, {0.8, true}, {0.7, false}, {0.3, true}, {0.2, false}, {0.1, false}}
	auc, ok := aucOf(append([]scored(nil), xs...))
	if !ok || auc < 0.8888 || auc > 0.8890 {
		t.Fatalf("auc %v %v", auc, ok)
	}
	if auc, _ := aucOf([]scored{{0.5, true}, {0.5, false}}); auc != 0.5 {
		t.Fatalf("ties count half, got %v", auc)
	}
	if _, ok := aucOf([]scored{{0.5, true}}); ok {
		t.Fatal("one class has no AUC")
	}
	bands := calibration(xs)
	if bands[9].N != 1 || bands[9].Pos != 1 || bands[1].N != 1 || bands[1].Pos != 0 || bands[0].N != 0 {
		t.Fatalf("bands %+v", bands)
	}
}

func seedDecisions(t *testing.T, s site, use string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		score := float64(i%10)/10 + 0.05
		ans := fmt.Sprintf(`{"noul":%v,"choice":"c%d"}`, score, i%2)
		rec := decisionRecord{
			ID: fmt.Sprintf("%s-%d", use, i), Time: time.Now().UTC(), Use: use, Role: "decider",
			Answers: answers("q", ans), Cost: 0.001, LatencyMS: int64(100 + i), Action: "shadow",
		}
		if err := appendDecisionLine(decisionFile(s.root, use), rec); err != nil {
			t.Fatal(err)
		}
		if err := recordDecisionOutcome(s.root, use, rec.ID, map[string]any{"hard": score > 0.5}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecisionReportAUCAndFewOutcomes(t *testing.T) {
	s := decisionTestSite(t, true)
	registerDecisionMetric("rep", decisionMetric{Name: "noul", Key: "q", Field: "noul", Outcome: "hard"})
	registerDecisionMetric("rep100", decisionMetric{Name: "noul", Key: "q", Field: "noul", Outcome: "hard"})
	seedDecisions(t, s, "rep", 20)
	seedDecisions(t, s, "rep100", 100)
	var buf bytes.Buffer
	a := &app{out: &buf, now: time.Now}
	if err := reportDecisions(a, s, "rep", time.Time{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"calls 20, failures 0", "cost $0.0200", "outcome coverage 20/20", "AUC 1.000", "fewer than 100 outcomes", "c0=10", "role decider: enabled", "p50"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := reportDecisions(a, s, "rep100", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "fewer than 100") || !strings.Contains(buf.String(), "AUC 1.000") {
		t.Fatalf("100 outcomes must not warn:\n%s", buf.String())
	}
}

func TestDecisionReportRunsResolversAndFiltersSince(t *testing.T) {
	s := decisionTestSite(t, true)
	old := decisionRecord{ID: "old", Time: time.Now().Add(-48 * time.Hour).UTC(), Use: "res", Role: "decider", Answers: answers("q", `{"noul":0.1}`)}
	recent := decisionRecord{ID: "new", Time: time.Now().UTC(), Use: "res", Role: "decider", Answers: answers("q", `{"noul":0.9}`)}
	for _, r := range []decisionRecord{old, recent} {
		if err := appendDecisionLine(decisionFile(s.root, "res"), r); err != nil {
			t.Fatal(err)
		}
	}
	ran := false
	registerOutcomeResolver("res", func(s site) error {
		ran = true
		return recordDecisionOutcome(s.root, "res", "new", map[string]any{"hit": true})
	})
	var buf bytes.Buffer
	a := &app{out: &buf, now: time.Now}
	if err := reportDecisions(a, s, "res", time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !ran || !strings.Contains(buf.String(), "calls 1,") || !strings.Contains(buf.String(), "outcome coverage 1/1") {
		t.Fatalf("ran %v:\n%s", ran, buf.String())
	}
}

func TestDecisionReportShowsDisabledRole(t *testing.T) {
	s := decisionTestSite(t, false)
	seedDecisions(t, s, "dis", 1)
	var buf bytes.Buffer
	if err := reportDecisions(&app{out: &buf, now: time.Now}, s, "dis", time.Time{}); err != nil || !strings.Contains(buf.String(), "role decider: disabled") {
		t.Fatalf("%v\n%s", err, buf.String())
	}
}
