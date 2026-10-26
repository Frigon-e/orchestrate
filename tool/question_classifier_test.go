package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func classifierFixture() classifierBatch {
	return classifierBatch{SchemaVersion: 1, Rubric: classifierRubric, Group: "api:0", Authority: "May decide: cache layout", Model: profile{Harness: "api", Provider: "openrouter", Model: "fake-decider"}, Limits: defaultClassifierLimits(), EvidenceScope: "bounded card evidence", Cards: []classifierCard{{ID: "q1-api", Revision: "rev1", Text: "Which cache layout?", Route: "unclear", Readiness: cardReadiness{Status: "ready"}}, {ID: "q2-api", Revision: "rev2", Text: "Which pricing policy?", Route: "owner", Readiness: cardReadiness{Status: "ready"}}}}
}
func classifierFakeAnswers(b classifierBatch) map[string]decAnswer {
	out := map[string]decAnswer{}
	for key, q := range classifierQuestions(b) {
		criteria := q.(map[string]any)["criteria"].(map[string]string)
		value := "implementation_choice"
		if strings.HasPrefix(key, "ready") {
			value = "ready"
		}
		if strings.HasPrefix(key, "relation") {
			value = "related"
		}
		p := map[string]float64{}
		for k := range criteria {
			p[k] = 0
		}
		p[value] = 1
		out[key] = decAnswer{Type: "choice", Choice: value, Confidence: 1, Probabilities: p}
	}
	return out
}
func classifierReadTest(t *testing.T, path string) []classifierRecord {
	t.Helper()
	rows, e := readClassifier(path)
	if e != nil {
		t.Fatal(e)
	}
	return rows
}
func classifierRunTest(t *testing.T, b classifierBatch, spent *classifierSpend, current func() bool, post classifierTransport) []classifierRecord {
	t.Helper()
	var records []classifierRecord
	if e := executeClassifier(context.Background(), b, b.key(), spent, current, post, func(r classifierRecord) error { records = append(records, r); return nil }); e != nil {
		t.Fatal(e)
	}
	return records
}
func TestClassifierRoleDefaultsAndIndependentConfig(t *testing.T) {
	h := setup(t)
	t.Setenv("ORCH_PI", filepath.Join(h.tmp, "absent"))
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("ORCH_TRIAGE_KEY", "")
	r, _ := (site{}).newRoleRun(classifierRole, modelOptions{})
	if ps, _ := r.available(); len(ps) != 0 {
		t.Fatal("default without Decider must be unavailable")
	}
	p := profile{Harness: "api", Provider: "openrouter", Model: "fake"}
	cfg := config{userConfig: userConfig{Roles: map[string]modelRole{"decider": {Use: []profile{p}}}}}
	beforeDecider := subjectJSONDigest((site{cfg: cfg}).role("decider"))
	beforeJudge := subjectJSONDigest((site{cfg: cfg}).role(adjudicatorRole))
	role := (site{cfg: cfg}).role(classifierRole)
	if len(role.Use) != 1 || role.Use[0] != p || role.Promote || role.Enabled != nil && !*role.Enabled {
		t.Fatalf("default %+v", role)
	}
	rr, _ := (site{cfg: cfg}).newRoleRun(classifierRole, modelOptions{})
	rr.keys = map[string]keyReadiness{"openrouter": {state: "ok", lane: "pi:openrouter"}}
	if ps, _ := rr.available(); len(ps) != 1 {
		t.Fatal("inherited connector unavailable")
	}
	for _, enabled := range []bool{false, true} {
		cfg.Roles[classifierRole] = modelRole{Enabled: &enabled, Use: []profile{{Harness: "api", Provider: "other", Model: "changed"}}, Promote: true}
		if subjectJSONDigest((site{cfg: cfg}).role("decider")) != beforeDecider || subjectJSONDigest((site{cfg: cfg}).role(adjudicatorRole)) != beforeJudge {
			t.Fatal("classifier changed another role")
		}
	}
	// Changing the returned inherited list cannot mutate triage's route.
	role.Use[0].Model = "mutated"
	if cfg.Roles["decider"].Use[0].Model != "fake" {
		t.Fatal("shared role backing array")
	}
	path := filepath.Join(h.tmp, "classifier-config.toml")
	t.Setenv("ORCH_CONFIG", path)
	for _, harness := range []string{"api", "claude", "pi"} {
		raw := fmt.Sprintf("[roles.question_classifier]\nuse=[{harness=%q,provider=\"openrouter\",model=\"fake\"}]\npromote=false\n[roles.question_classifier.classifier_limits]\nbatch_requests=2\nrun_requests=4\nseconds=20\nretries=1\n", harness)
		if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		loaded, e := loadConfig(h.repo)
		if (e == nil) != (harness == "api") {
			t.Fatalf("harness %s: %v", harness, e)
		}
		if e == nil && loaded.Roles[classifierRole].ClassifierLimits.RunRequests != 4 {
			t.Fatal("limits not loaded")
		}
	}
}
func TestClassifierPromotionCannotOverrideOwnerOrChecks(t *testing.T) {
	guess := classifierGuess{Outcome: "selected", Value: "implementation_choice", Probabilities: map[string]float64{"implementation_choice": 1}}
	for _, tc := range []struct {
		route, text, authority string
		on, mandatory          bool
		want                   string
	}{
		{"owner", "cache layout", "May decide: cache layout", true, false, "owner"},
		{"unclear", "cache layout", "May decide: cache layout", false, false, "unclear"},
		{"unclear", "cache layout", "May decide: cache layout", true, false, "lead-may-decide"},
		{"unclear", "cache layout", "", true, false, "unclear"},
		{"unclear", "pricing policy", "May decide: pricing policy", true, false, "unclear"},
		{"unclear", "cache layout", "May decide: cache layout", true, true, "unclear"},
		{"unclear", "cache layout", "May decide: cache layout\nMust escalate: storage changes", true, false, "unclear"},
	} {
		if got := classifierRoute(tc.route, tc.text, guess, tc.authority, tc.on, tc.mandatory); got != tc.want {
			t.Fatalf("%+v: %s", tc, got)
		}
	}
}
func TestClassifierRecordsTypedProbabilitiesAndUnknownUsage(t *testing.T) {
	b := classifierFixture()
	b.Promote = true
	records := classifierRunTest(t, b, &classifierSpend{}, func() bool { return true }, func(context.Context, profile, map[string]any, map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
		return classifierFakeAnswers(b), "served-v2", shadowUsage{}, nil
	})
	r := records[len(records)-1]
	if r.Status != "recorded" || r.ServedModel != "served-v2" || len(r.Results) != 2 || len(r.Relations) != 1 || r.Relations[0].QuestionRelation.Value != "related" || r.Results[1].EffectiveRoute != "owner" || r.Results[0].EffectiveRoute != "lead-may-decide" {
		t.Fatalf("record %+v", r)
	}
	if r.Usage.Availability != "unknown" || r.Usage.InputTokens != nil || r.Usage.OutputTokens != nil || r.Usage.CostUSD != nil || r.TotalTokens != nil {
		t.Fatal("unknown usage became measured zero")
	}
	raw, _ := json.Marshal(r)
	for _, field := range []string{"input_hash", "evidence_scope", "model", "rubric_version", "grouping", "probabilities", "latency_ms", "usage", "subsequent_outcome", "limits", "spent_requests", "card_hashes"} {
		if !strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatal("missing record field", field)
		}
	}
	if r.LaterOutcome != "" {
		t.Fatal("fabricated later outcome")
	}
}
func TestClassifierUncertaintyAndInvalidDistributions(t *testing.T) {
	for _, a := range []decAnswer{
		{Type: "choice", Choice: "unclear", Probabilities: map[string]float64{"equivalent": 0, "related": 0, "distinct": 0, "unclear": 1}},
		{Type: "choice", Choice: "related", Probabilities: map[string]float64{"equivalent": 0.1, "related": 0.5, "distinct": 0.3, "unclear": 0.1}},
	} {
		g, e := classifierSelect(a, classifierRelations)
		if e != nil || g.Outcome != "unresolved" || g.Value != "unclear" {
			t.Fatalf("%+v %v", g, e)
		}
	}
	for _, a := range []decAnswer{{}, {Type: "choice", Choice: "related", Probabilities: map[string]float64{"equivalent": 0, "related": 2, "distinct": 0, "unclear": 0}}, {Type: "choice", Choice: "bogus", Probabilities: map[string]float64{"equivalent": 0, "related": 1, "distinct": 0, "unclear": 0}}} {
		if _, e := classifierSelect(a, classifierRelations); e == nil {
			t.Fatal("accepted malformed distribution")
		}
	}
}
func TestClassifierFailureRetriesAndExhaustionStopsRequests(t *testing.T) {
	b := classifierFixture()
	calls := 0
	post := func(context.Context, profile, map[string]any, map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
		calls++
		return nil, "", shadowUsage{}, errors.New("fake API failure")
	}
	spent := &classifierSpend{}
	records := classifierRunTest(t, b, spent, func() bool { return true }, post)
	if calls != 2 || spent.Requests != 2 || records[len(records)-1].Status != "unavailable" {
		t.Fatalf("calls=%d records=%+v", calls, records)
	}
	spent.Requests = b.Limits.RunRequests
	b.Group = "new-batch-after-run-exhaustion"
	records = classifierRunTest(t, b, spent, func() bool { return true }, post)
	if calls != 2 || records[0].Status != "unresolved" || !strings.Contains(records[0].Note, "budget_exhausted") {
		t.Fatal("exhausted job called API or disappeared")
	}
}
func TestClassifierLateResultDiscarded(t *testing.T) {
	b := classifierFixture()
	current := true
	records := classifierRunTest(t, b, &classifierSpend{}, func() bool { return current }, func(context.Context, profile, map[string]any, map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
		current = false
		return classifierFakeAnswers(b), "fake", shadowUsage{}, nil
	})
	if len(records) != 1 || records[0].Status != "stale" || len(records[0].Results) != 0 {
		t.Fatalf("late result accepted %+v", records)
	}
}
func TestClassifierCacheCoversAllInputs(t *testing.T) {
	base := classifierFixture()
	base.Subject = &gateSubject{ID: "old"}
	base.Cards[0].Packet = &gateQuestionPacket{Options: []gatePacketOption{{ID: "A", Text: "original"}}}
	base.Evidence = []evidenceRecord{{ID: "e1", ContentHash: "hash"}}
	for name, change := range map[string]func(*classifierBatch){
		"text": func(b *classifierBatch) { b.Cards[0].Text += "changed" }, "options": func(b *classifierBatch) { b.Cards[0].Packet.Options[0].Text = "new meaning" },
		"revision": func(b *classifierBatch) { b.Cards[0].Revision = "new" }, "evidence id": func(b *classifierBatch) { b.Evidence[0].ID = "e2" }, "evidence hash": func(b *classifierBatch) { b.Evidence[0].ContentHash = "different" },
		"subject": func(b *classifierBatch) { b.Subject.ID = "new" }, "rubric": func(b *classifierBatch) { b.Rubric = "v2" }, "model": func(b *classifierBatch) { b.Model.Model = "next" }, "group": func(b *classifierBatch) { b.Group = "another" }, "authority": func(b *classifierBatch) { b.Authority = "Never: storage" }, "scope": func(b *classifierBatch) { b.EvidenceScope = "other" }, "readiness": func(b *classifierBatch) { b.Cards[0].Readiness.Status = "missing_evidence" }, "route": func(b *classifierBatch) { b.Cards[0].Route = "owner" },
	} {
		raw, _ := json.Marshal(base)
		var b classifierBatch
		_ = json.Unmarshal(raw, &b)
		change(&b)
		if b.key() == base.key() {
			t.Fatal("cache key omitted", name)
		}
	}
}
func TestClassifierQueueReturnsWithoutRunningJobAndDoesNotRepeat(t *testing.T) {
	dir := t.TempDir()
	b := classifierFixture()
	launches := 0
	launch := func(string) error { launches++; return nil }
	for range 2 {
		if e := classifierQueue(dir, b, launch); e != nil {
			t.Fatal(e)
		}
	}
	rows := classifierReadTest(t, classifierLog(dir))
	if launches != 1 || len(rows) != 1 || rows[0].Status != "queued" {
		t.Fatalf("launches=%d records=%+v", launches, rows)
	}
	if e := classifierQueue(dir, func() classifierBatch { b.Group = "second"; return b }(), func(string) error { return errors.New("cannot start") }); e != nil {
		t.Fatal(e)
	}
	rows = classifierReadTest(t, classifierLog(dir))
	if rows[len(rows)-1].Status != "unavailable" {
		t.Fatal("launch failure invisible")
	}
}
func TestClassifierHardWallLimit(t *testing.T) {
	b := classifierFixture()
	b.Limits.Seconds = 1
	spent := &classifierSpend{Started: time.Now().Add(-950 * time.Millisecond)}
	released := make(chan struct{})
	defer close(released)
	started := time.Now()
	rows := classifierRunTest(t, b, spent, func() bool { return true }, func(context.Context, profile, map[string]any, map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
		<-released
		return nil, "", shadowUsage{}, nil
	})
	if time.Since(started) > time.Second || rows[len(rows)-1].Status != "unresolved" {
		t.Fatalf("deadline not hard: %+v", rows)
	}
}
func TestClassifierLogReaderToleratesMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	_ = os.WriteFile(path, []byte("{\"status\":\"recorded\"}\n{broken\n"), 0600)
	rows := classifierReadTest(t, path)
	if len(rows) != 1 || rows[0].Status != "recorded" || rows[0].LaterOutcome != "" {
		t.Fatal(rows)
	}
}
func TestClassifierNoDeciderLeavesProductionBytesIdentical(t *testing.T) {
	t.Setenv("ORCH_PI", filepath.Join(t.TempDir(), "missing-pi"))
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("ORCH_TRIAGE_KEY", "")
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "missing-jb"))
	dir, code, src, sp := enrichFixture(t, fixtureQuestions())
	configPath := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("ORCH_CONFIG", configPath)
	_ = os.WriteFile(configPath, []byte("[roles.question_classifier]\nenabled=false\n"), 0600)
	originalReport := mustRead(t, filepath.Join(dir, "report.md"))
	first, e := enrichGate(dir, code, src, "spec.md", sp)
	if e != nil {
		t.Fatal(e)
	}
	snapshot := func() map[string]string {
		out := map[string]string{}
		paths, _ := filepath.Glob(filepath.Join(dir, "questions", "*"))
		paths = append(paths, filepath.Join(dir, "gate.json"), filepath.Join(dir, "report.md"))
		for _, p := range paths {
			raw, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			out[p] = string(raw)
		}
		return out
	}
	before := snapshot()
	_ = os.WriteFile(configPath, []byte(""), 0600)
	// Compare identical publication inputs. Existing report-section rewriting
	// normalizes a blank line on reruns, unrelated to classifier availability.
	if e := os.WriteFile(filepath.Join(dir, "report.md"), originalReport, 0600); e != nil {
		t.Fatal(e)
	}
	second, e := enrichGate(dir, code, src, "spec.md", sp)
	if e != nil {
		t.Fatal(e)
	}
	after := snapshot()
	if !reflect.DeepEqual(before, after) || first.summary() != second.summary() {
		for p, old := range before {
			if old != after[p] {
				t.Logf("changed %s\nBEFORE:\n%s\nAFTER:\n%s", p, old, after[p])
			}
		}
		t.Fatal("no-Decider fallback changed cards, route, packets or report")
	}
	rows := classifierReadTest(t, classifierLog(dir))
	if len(rows) == 0 || rows[len(rows)-1].Status != "unavailable" {
		t.Fatal("no-Decider skip invisible")
	}
}
func TestClassifierStaleIgnoredSourceEvidence(t *testing.T) {
	h := setup(t)
	dir := t.TempDir()
	src := []byte("# Subject\n")
	_ = os.WriteFile(filepath.Join(h.repo, "spec.md"), src, 0600)
	_ = os.WriteFile(filepath.Join(h.repo, ".gitignore"), []byte("ignored.go\n"), 0600)
	ignored := filepath.Join(h.repo, "ignored.go")
	_ = os.WriteFile(ignored, []byte("original"), 0600)
	subject, e := captureGateSubject(h.repo, src)
	if e != nil {
		t.Fatal(e)
	}
	b := classifierFixture()
	b.Subject = subject
	b.Code = h.repo
	b.SpecPath = "spec.md"
	b.Evidence = []evidenceRecord{{ID: "ignored", SourceKind: evidenceSourceCode, Path: "ignored.go", ContentHash: subjectDigest([]byte("original")), LookupID: "source"}}
	rec := gateRecord{Subject: subject, Review: &gateReviewRecords{}}
	rec.Review.Policy.Packet.Evidence = b.Evidence
	rec.Review.Policy.Packet.Lookups = []evidenceLookup{{ID: "source", Unit: "source_read"}}
	raw, _ := json.Marshal(rec)
	_ = os.WriteFile(filepath.Join(dir, "gate.json"), raw, 0600)
	_ = os.MkdirAll(filepath.Join(dir, "questions"), 0700)
	for i := range b.Cards {
		c := &b.Cards[i]
		c.Revision = subjectDigest([]byte(c.Text))
		_ = os.WriteFile(filepath.Join(dir, "questions", c.ID+".md"), []byte(c.Text), 0600)
	}
	if !classifierCurrent(dir, b) {
		t.Fatal("fresh evidence rejected")
	}
	rows := classifierRunTest(t, b, &classifierSpend{}, func() bool { return classifierCurrent(dir, b) }, func(context.Context, profile, map[string]any, map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
		_ = os.WriteFile(ignored, []byte("changed"), 0600)
		return classifierFakeAnswers(b), "fake", shadowUsage{}, nil
	})
	if len(rows) != 1 || rows[0].Status != "stale" {
		t.Fatal("changed ignored evidence accepted", rows)
	}
}
func TestClassifierTruncatedTailAppendPreservesNextRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	if e := os.WriteFile(path, []byte(`{"status":`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := appendClassifier(path, classifierRecord{Status: "recorded", Key: "complete"}); e != nil {
		t.Fatal(e)
	}
	rows := classifierReadTest(t, path)
	if len(rows) != 1 || rows[0].Key != "complete" {
		t.Fatal("append lost after truncated tail", rows)
	}
}
func TestClassifierSpendDurableBeforeTransportAndPerRun(t *testing.T) {
	dir := t.TempDir()
	spent, e := classifierLoadSpend(dir, "review-1")
	if e != nil {
		t.Fatal(e)
	}
	b := classifierFixture()
	b.RunID = "review-1"
	job := b.key()
	rows := classifierRunTest(t, b, spent, func() bool { return true }, func(_ context.Context, _ profile, state map[string]any, _ map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
		if state["batch"].(classifierBatch).RunID != "" {
			return nil, "", shadowUsage{}, fmt.Errorf("accounting run identity leaked into decision inputs")
		}
		saved, e := classifierLoadSpend(dir, "review-1")
		if e != nil {
			return nil, "", shadowUsage{}, e
		}
		if saved.Requests != 1 || saved.Batches[job] != 1 {
			return nil, "", shadowUsage{}, fmt.Errorf("admission not durable before transport")
		}
		return classifierFakeAnswers(b), "fake", shadowUsage{}, nil
	})
	if rows[len(rows)-1].Status != "recorded" {
		t.Fatal(rows)
	}
	restored, e := classifierLoadSpend(dir, "review-1")
	if e != nil || restored.Requests != 1 {
		t.Fatalf("lost persisted spend %+v %v", restored, e)
	}
	fresh, e := classifierLoadSpend(dir, "review-2")
	if e != nil || fresh.Requests != 0 {
		t.Fatal("new gate run inherited old spend", e)
	}
	b.RunID = "new invocation, unchanged decision inputs"
	if b.key() != job {
		t.Fatal("run accounting identity changed decision cache key")
	}
}
func TestClassifierAdmissionUsesImmutableJobIdentity(t *testing.T) {
	b := classifierFixture()
	job := b.key()
	spent := &classifierSpend{Batches: map[string]int{job: 2}}
	b.Model.Model = "fallback-model"
	calls := 0
	post := func(context.Context, profile, map[string]any, map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
		calls++
		return nil, "", shadowUsage{}, nil
	}
	var rows []classifierRecord
	if e := executeClassifier(context.Background(), b, job, spent, func() bool { return true }, post, func(r classifierRecord) error { rows = append(rows, r); return nil }); e != nil {
		t.Fatal(e)
	}
	if calls != 0 || rows[len(rows)-1].Status != "unavailable" {
		t.Fatal("fallback reset batch allowance")
	}
}
func TestClassifierWorkerBindsRealCardAndSubjectAndCaches(t *testing.T) {
	h := setup(t)
	dir := t.TempDir()
	src := []byte("# Subject\n")
	if e := os.WriteFile(filepath.Join(h.repo, "spec.md"), src, 0600); e != nil {
		t.Fatal(e)
	}
	subject, e := captureGateSubject(h.repo, src)
	if e != nil {
		t.Fatal(e)
	}
	b := classifierFixture()
	b.Code = h.repo
	b.SpecPath = "spec.md"
	b.Subject = subject
	_ = os.MkdirAll(filepath.Join(dir, "questions"), 0700)
	for i := range b.Cards {
		c := &b.Cards[i]
		c.Revision = subjectDigest([]byte(c.Text))
		_ = os.WriteFile(filepath.Join(dir, "questions", c.ID+".md"), []byte(c.Text), 0600)
	}
	// A malformed neighboring manifest must not poison the valid job.
	_ = os.MkdirAll(filepath.Join(dir, "question-classifier-jobs"), 0700)
	_ = os.WriteFile(filepath.Join(dir, "question-classifier-jobs", "000-broken.json"), []byte("{broken"), 0600)
	raw, _ := json.Marshal(gateRecord{Subject: subject})
	_ = os.WriteFile(filepath.Join(dir, "gate.json"), raw, 0600)
	if !classifierCurrent(dir, b) {
		t.Fatal("fixture not current")
	}
	disabled := classifierBase(b)
	disabled.Status = "unavailable"
	disabled.Note = "question_classifier disabled"
	if e = appendClassifier(classifierLog(dir), disabled); e != nil {
		t.Fatal(e)
	}
	if e = classifierQueue(dir, b, func(string) error { return nil }); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	post := func(context.Context, profile, map[string]any, map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
		calls.Add(1)
		return classifierFakeAnswers(b), "fake", shadowUsage{}, nil
	}
	resolve := func(classifierBatch) (profile, error) { return b.Model, nil }
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- classifierWorkerWith(dir, resolve, post) }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if e = classifierWorkerWith(dir, resolve, post); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal("waiting reissued model request", calls.Load())
	}
	if _, e = os.Stat(filepath.Join(dir, "question-classifier-jobs", "000-broken.json.invalid")); e != nil {
		t.Fatal("malformed job not quarantined", e)
	}
	_ = os.WriteFile(filepath.Join(dir, "questions", b.Cards[0].ID+".md"), []byte("new revision"), 0600)
	if classifierCurrent(dir, b) {
		t.Fatal("older card revision accepted")
	}
	_ = os.WriteFile(filepath.Join(dir, "questions", b.Cards[0].ID+".md"), []byte(b.Cards[0].Text), 0600)
	_ = os.WriteFile(filepath.Join(h.repo, "spec.md"), []byte("new subject"), 0600)
	if classifierCurrent(dir, b) {
		t.Fatal("older subject accepted")
	}
}
