package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func gateBudgetRecord(l adjudicationLimits) *gateRecord {
	return &gateRecord{Budget: newRunBudget(l, time.Now()), PassErrors: map[string]string{}, PassSeconds: map[string]int{}, PassKept: map[string]string{}, PassTokens: map[string]passTokens{}, Groups: map[string][]string{}}
}

func TestGateRunBudgetConfigUserOnly(t *testing.T) {
	h := setup(t)
	path := userConfigPath()
	writeFiles(t, filepath.Dir(path), map[string]string{filepath.Base(path): "[gate_budget]\nrequests=17\nrepairs=0\nfallbacks=0\n"})
	cfg, err := loadConfig(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	l := gateRunLimits([]gatePass{{budget: 40}}, cfg.GateBudget)
	if l.Requests != 17 || l.Repairs != 0 || l.Fallbacks != 0 || l.Seconds < 2700 {
		t.Fatal(l)
	}
	writeFiles(t, filepath.Dir(path), map[string]string{filepath.Base(path): "[gate_budget]\noutput_reserve=100000\n"})
	if _, err := loadConfig(h.repo); err != nil {
		t.Fatalf("reserve incorrectly validated against zero-pass defaults: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(h.repo, ".agents"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"gate_budget", "GateBudget", "GATE_BUDGET"} {
		writeFiles(t, h.repo, map[string]string{".agents/orchestrate.toml": "[" + key + "]\nrequests=1000000\n"})
		if _, err := loadConfig(h.repo); err == nil {
			t.Fatalf("repository controlled %s", key)
		}
	}
	os.Remove(filepath.Join(h.repo, ".agents", "orchestrate.toml"))
	for _, invalid := range []string{"requests=0", "seconds=-1", "repairs=-1", "output_reserve=0", "max_usd=1.0"} {
		writeFiles(t, filepath.Dir(path), map[string]string{filepath.Base(path): "[gate_budget]\n" + invalid + "\n"})
		if _, err := loadConfig(h.repo); err == nil {
			t.Fatalf("invalid config accepted: %s", invalid)
		}
	}
}

func TestGateRunBudgetAtomicReservationsAndUnknown(t *testing.T) {
	l := defaultAdjudicationLimits()
	l.Requests, l.InputTokens, l.OutputTokens = 100, 100, 4096
	b := newRunBudget(l, time.Now())
	var wg sync.WaitGroup
	var mu sync.Mutex
	var tickets []*budgetAdmission
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := b.begin(80, false, false)
			if err == nil {
				mu.Lock()
				tickets = append(tickets, a)
				mu.Unlock()
			} else if !errors.Is(err, errAdjudicationBudget) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(tickets) != 1 || b.Requests != 1 {
		t.Fatalf("over-admission: %d %d", len(tickets), b.Requests)
	}
	if err := tickets[0].finish(executionUsage{Availability: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := b.observeRequests(0); err != nil {
		t.Fatal(err)
	}
	if !b.UnknownInput || !b.UnknownOutput || !b.UnknownCost || !b.UnknownRequests || b.ReportedInput != 0 || b.ReportedOutput != 0 || b.EstimatedInput != 80 || b.EstimatedOutput != 4096 {
		t.Fatalf("unknown invented measured usage: %+v", b)
	}
	if b.UnknownCounts != (budgetUnknown{1, 1, 1, 1}) || b.Remaining.Input != 20 || b.Remaining.Output != 0 {
		t.Fatalf("lost counters/allowances: %+v", b)
	}
	if _, err := b.begin(1, false, false); !errors.Is(err, errAdjudicationBudget) {
		t.Fatal("unknown output reservation was reset", err)
	}
}

func TestGateRunBudgetObservedAndEstimatedCannotOverflow(t *testing.T) {
	l := gateRunLimits([]gatePass{{budget: 40}}, nil)
	l.InputTokens = math.MaxInt64
	b := newRunBudget(l, time.Now())
	a, err := b.begin(math.MaxInt64/2, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.finish(executionUsage{Availability: "unknown"}); err != nil {
		t.Fatal(err)
	}
	a, err = b.begin(1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	err = a.finish(executionUsage{Availability: "partial", InputTokens: evidenceTestPtr(int64(math.MaxInt64 - 1)), OutputTokens: evidenceTestPtr(int64(0))})
	if !errors.Is(err, errAdjudicationBudget) || b.Remaining.Input != 0 {
		t.Fatal("overflow invented allowance", err, b.Remaining)
	}
	if _, err = b.begin(1, false, false); !errors.Is(err, errAdjudicationBudget) {
		t.Fatal("overspent run admitted", err)
	}
}

func TestGateRunBudgetReportedRequestsChargeReliableEstimate(t *testing.T) {
	l := defaultAdjudicationLimits()
	l.Requests = 5
	l.MaxUSD = 0.25
	l.EstimatedRequestUSD = 0.1
	l.ReliableEstimate = true
	b := newRunBudget(l, time.Now())
	a, err := b.begin(1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	err = a.finishUsage(executionUsage{Availability: "unknown"}, budgetUnknown{}, 3)
	if !errors.Is(err, errAdjudicationBudget) || b.Requests != 3 || b.EstimatedUSD < 0.29 {
		t.Fatalf("lost extra request estimate: %v %+v", err, b)
	}
}

func TestGateRunBudgetMixedTelemetryKeepsReportedSubset(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"pi.jsonl": `{"message":{"role":"assistant","usage":{"input":7,"output":2,"cost":{"total":0.01}}}}
{"message":{"role":"assistant","usage":{"input":5}}}
`})
	r := gateInvocationReport(dir)
	if r.requests != 2 || r.usage.InputTokens == nil || *r.usage.InputTokens != 12 || r.usage.OutputTokens == nil || *r.usage.OutputTokens != 2 || r.unknown.Output != 1 || r.unknown.Cost != 1 {
		t.Fatalf("lost reported subset: %+v", r)
	}
	b := newRunBudget(gateRunLimits([]gatePass{{budget: 40}}, nil), time.Now())
	a, err := b.begin(20, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.finishUsage(r.usage, r.unknown, r.requests); err != nil {
		t.Fatal(err)
	}
	if b.ReportedInput != 12 || b.ReportedOutput != 2 || !b.UnknownOutput || !b.UnknownCost || b.UnknownInput || b.EstimatedOutput != 4094 {
		t.Fatalf("partial telemetry became complete/zero: %+v", b)
	}
}

func TestGateRunBudgetNativeMessageDedupAndOverflow(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"claude.jsonl": `{"message":{"id":"msg-1","role":"assistant","usage":{"input_tokens":10,"output_tokens":2}}}
{"message":{"id":"msg-1","role":"assistant","usage":{"input_tokens":10,"output_tokens":3}}}
`})
	r := gateInvocationReport(dir)
	if r.requests != 1 || r.usage.InputTokens == nil || *r.usage.InputTokens != 10 || r.usage.OutputTokens == nil || *r.usage.OutputTokens != 3 {
		t.Fatalf("native content blocks double charged: %+v", r)
	}
	writeFiles(t, dir, map[string]string{"claude.jsonl": fmt.Sprintf("{\"message\":{\"role\":\"assistant\",\"usage\":{\"input\":%d,\"cacheRead\":2}}}\n", int64(math.MaxInt64))})
	r = gateInvocationReport(dir)
	if !r.overflow || r.usage.InputTokens == nil || *r.usage.InputTokens != math.MaxInt64 {
		t.Fatalf("native usage overflow lost spend: %+v", r)
	}
}

func TestGateRunBudgetInvocationUsage(t *testing.T) {
	for _, tc := range []struct {
		name, file, raw string
		in, out         *int64
		requests        int
	}{
		{"unknown", "pi.jsonl", `{"type":"message","message":{"role":"assistant","content":[]}}`, nil, nil, 0},
		{"input only", "pi.jsonl", `{"type":"message","message":{"role":"assistant","usage":{"input":7,"cacheRead":2}}}`, evidenceTestPtr(int64(9)), nil, 1},
		{"reported zero output", "pi.jsonl", `{"type":"message","message":{"role":"assistant","usage":{"input":7,"output":0}}}`, evidenceTestPtr(int64(7)), evidenceTestPtr(int64(0)), 1},
		{"claude", "claude.json", `{"type":"result","num_turns":2,"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":5},"total_cost_usd":0.1}`, evidenceTestPtr(int64(15)), evidenceTestPtr(int64(3)), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "attempt-2"), 0755); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, dir, map[string]string{tc.file: tc.raw + "\n", "attempt-2/old.jsonl": `{"message":{"role":"assistant","usage":{"input":1000,"output":1000}}}`})
			u, n := gateInvocationUsage(dir)
			if !reflect.DeepEqual(u.InputTokens, tc.in) || !reflect.DeepEqual(u.OutputTokens, tc.out) || n != tc.requests {
				t.Fatalf("%+v requests %d", u, n)
			}
		})
	}
}

func TestGateRunBudgetRepairsFallbacksAndAdjudicationShareTotal(t *testing.T) {
	l := gateRunLimits([]gatePass{{budget: 40}}, nil)
	l.Requests = 2
	b := newRunBudget(l, time.Now())
	calls := 0
	p := gatePass{prompt: "shared-prefix-and-dossier", sessionDir: t.TempDir()}
	run := func(gatePass) (string, string, error) { calls++; return "{}", "", nil }
	if _, _, err := runBudgetedGatePass(b, p, false, false, run); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runBudgetedGatePass(b, p, true, true, run); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runBudgetedGatePass(b, p, true, true, run); !errors.Is(err, errAdjudicationBudget) {
		t.Fatal("fallback was not refused", err)
	}
	if calls != 2 || b.Requests != 2 || b.Repairs != 1 || b.Fallbacks != 1 {
		t.Fatalf("budget reset: calls %d %+v", calls, b)
	}
	env := adjudicationFixture(t)
	rr := adjudicationRoleFixture(t, profile{Harness: "claude", Model: "haiku"})
	job := seedAdjudicationBudget(&gateRecord{Budget: b}, defaultAdjudicationLimits(), time.Now())
	audit := executeAdjudication(rr, job, env, func(context.Context, profile, adjudicationRequest) (adjudicationResponse, error) {
		calls++
		return adjudicationResponse{}, nil
	})
	if audit.Reason != reviewBudgetExhausted || calls != 2 || b.Requests != 2 {
		t.Fatalf("adjudication reset total: %+v %+v", audit, b)
	}
}

func TestGateRunBudgetObservedRequestsTokensAndTime(t *testing.T) {
	for _, kind := range []string{"requests", "input", "output", "time"} {
		t.Run(kind, func(t *testing.T) {
			l := gateRunLimits([]gatePass{{budget: 40}}, nil)
			l.Requests = 2
			l.InputTokens = 100
			l.OutputTokens = 8192
			b := newRunBudget(l, time.Now())
			now := time.Now()
			b.Started = now
			b.now = func() time.Time { return now }
			p := gatePass{prompt: "prompt", sessionDir: t.TempDir()}
			run := func(gatePass) (string, string, error) {
				in, out, requests := 1, 1, 1
				switch kind {
				case "requests":
					requests = 3
				case "input":
					in = 101
				case "output":
					out = 8193
				case "time":
					now = now.Add(time.Duration(l.Seconds) * time.Second)
				}
				var raw strings.Builder
				for i := 0; i < requests; i++ {
					fmt.Fprintf(&raw, "{\"type\":\"message\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input\":%d,\"output\":%d}}}\n", in, out)
				}
				writeFiles(t, p.sessionDir, map[string]string{"usage.jsonl": raw.String()})
				return "{}", "", nil
			}
			if _, _, err := runBudgetedGatePass(b, p, false, false, run); !errors.Is(err, errAdjudicationBudget) {
				t.Fatal("over-budget result accepted", err)
			}
			if b.Requests < 1 || b.ReportedInput < 1 || b.ReportedOutput < 1 {
				t.Fatal("failure lost observed usage", b)
			}
		})
	}
}

func TestGateRunBudgetFallbackRefusedAtScheduler(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	t.Setenv("HERDR_ENV", "")
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", `echo 'rate limit' >&2; exit 1`))
	log := filepath.Join(h.tmp, "fallback.log")
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", `echo called >> '`+log+`'; echo '{"type":"result","result":"{}"}'`))
	runner, err := roleSite(profile{Harness: "pi", Model: "test"}, profile{Harness: "claude", Model: "test"}).newRoleRun("gate", modelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	l := gateRunLimits([]gatePass{{budget: 40}}, nil)
	l.Requests = 1
	rec := gateBudgetRecord(l)
	p := gatePass{name: "group:test", class: "package", prompt: "review", apply: func(string, *gateRecord, string, *spec) error { t.Error("unreviewed result applied"); return nil }}
	runGatePasses(filepath.Join(h.tmp, "gate"), h.repo, "test", []gatePass{p}, rec, nil, gateOpts{runner: runner, headless: true})
	if exists(log) || rec.Budget.Requests != 1 || rec.PassAnswerStates[p.name] != reviewAnswerBudget || !strings.Contains(rec.PassErrors[p.name], "budget_exhausted") {
		t.Fatalf("fallback escaped: %+v", rec)
	}
}

func TestGateRunBudgetSchedulerRetriesShareUsage(t *testing.T) {
	for _, kind := range []string{"schema", "stall"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			args := []string{"gate", "spec.md", "--runner", "pi:" + gateModelDef}
			dir := h.gateRepo(1)
			if kind == "schema" {
				h.fakePi(gatePasses(map[string]string{"group": `{}`}))
			} else {
				h.sessionPi(gatePasses(nil))
				writeFiles(t, filepath.Join(h.tmp, "fakepi"), map[string]string{"group.sleep.once": ""})
				gateStall = 300 * time.Millisecond
			}
			t.Setenv("HERDR_ENV", "")
			code, out := h.run(args...)
			if kind == "stall" && code != 0 {
				t.Fatal(out)
			}
			rec := readRecord(t, dir)
			if rec.Budget.Requests != 4 || rec.Budget.Repairs != 1 {
				t.Fatalf("retry reset accounting: kind %s calls %d repairs %d, %s", kind, rec.Budget.Requests, rec.Budget.Repairs, out)
			}
			if kind == "stall" && rec.Budget.ReportedInput != 1620 {
				t.Fatalf("stall overwrite lost usage: %+v", rec.Budget)
			}
		})
	}
}

func TestGateRunBudgetCompletedGroupsRetained(t *testing.T) {
	h := setup(t)
	h.gateRepo(5)
	h.sessionPi(gatePasses(nil))
	t.Setenv("HERDR_ENV", "")
	gateWarmDelay = time.Minute
	gs, err := loadGateSpec(h.a, "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	rec := gateBudgetRecord(gateRunLimits([]gatePass{{budget: 40}, {budget: 40}}, nil))
	rec.Budget.Limits.Requests = 2
	guard, err := newCheckoutGuard(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.cancel()
	rec.Subject, err = gateSubjectFromStamp(h.repo, []byte(gs.src), guard.before)
	if err != nil {
		t.Fatal(err)
	}
	passes := buildGatePasses(h.repo, gs.src, gs.rel, gs.sp, rec.Groups, nil)
	slices.SortStableFunc(passes, func(a, b gatePass) int {
		if a.class == b.class {
			return 0
		}
		if a.class == "package" {
			return -1
		}
		return 1
	})
	initGateReview(rec, gs.sp, gs.src, passes)
	runGatePasses(filepath.Join(h.tmp, "gate"), h.repo, "test", passes, rec, gs.sp, gateOpts{guard: guard, headless: true})
	prepareGateReview(rec, h.repo)
	reduceGateRecord(rec)
	if rec.State != "incomplete" || rec.Budget.Requests != 2 {
		t.Fatalf("%+v", rec)
	}
	completed := 0
	for _, p := range passes {
		if p.class == "package" && rec.PassErrors[p.name] == "" {
			completed++
			for _, reason := range rec.Reduction.Reasons {
				if reason.Actor == p.name {
					t.Fatalf("completed group lost result: %+v", reason)
				}
			}
		}
		if msg := rec.PassErrors[p.name]; msg != "" && (!strings.Contains(msg, "budget_exhausted") || rec.PassAnswerStates[p.name] != reviewAnswerBudget) {
			t.Fatalf("wrong unfinished state: %s %s", p.name, msg)
		}
	}
	if completed < 1 {
		t.Fatal("fixture did not complete a group")
	}
	for _, reason := range rec.Reduction.Reasons {
		if reason.Reason != reviewBudgetExhausted || reason.Action == "" {
			t.Fatalf("unfinished check lacks budget reason/action: %+v", reason)
		}
	}
}

func TestGateRunBudgetDefaultCallsAndPrefixes(t *testing.T) {
	for _, mode := range []string{"headless", "pane"} {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			dir := h.gateRepo(1)
			h.sessionPi(gatePasses(nil))
			if mode == "headless" {
				t.Setenv("HERDR_ENV", "")
			}
			h.must("gate", "spec.md")
			rec := readRecord(t, dir)
			if rec.Budget == nil || rec.Budget.Requests != 3 || rec.State != "pass" || len(rec.PassErrors) != 0 {
				t.Fatalf("default changed normal run: %+v", rec)
			}
			if calls := string(mustRead(t, filepath.Join(h.tmp, "fakepi", "calls.log"))); strings.Count(calls, "GATE-") != 3 {
				t.Fatal(calls)
			}
			base := newGatePass("a", "package", "shared", "own", "TAG", "task", `{}`, nil)
			base.lookupTool = true
			before, err := gateAgentPass(h.repo, "pi", base)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = runBudgetedGatePass(newRunBudget(gateRunLimits([]gatePass{base}, nil), time.Now()), before, false, false, func(p gatePass) (string, string, error) {
				if p.prompt != before.prompt || p.shared != before.shared || p.task != before.task {
					t.Fatal("budget altered prefix or suffix")
				}
				return "{}", "", nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGateRunBudgetV2OutputEquality(t *testing.T) {
	b := newRunBudget(gateRunLimits([]gatePass{{budget: 40}}, nil), time.Now())
	a, err := b.begin(50, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.finish(executionUsage{Availability: "unknown"}); err != nil {
		t.Fatal(err)
	}
	b.observeRequests(0)
	view := gateInformation{SchemaVersion: 2, State: "incomplete", Budget: b}
	s, err := selectOutput(view)
	if err != nil {
		t.Fatal(err)
	}
	var want any
	for _, audience := range []outputAudience{outputJSON, outputHuman, outputAgent} {
		raw, err := s.render(audience)
		if err != nil {
			t.Fatal(err)
		}
		got := decodedOutput(t, raw, audience)
		if want == nil {
			want = got
		} else if !reflect.DeepEqual(got, want) {
			t.Fatalf("budget selection differs for %v: %#v vs %#v", audience, got, want)
		}
	}
}

// Exercise the user-facing CLI with local fake panes/runners, never a model.
func TestGateRunBudgetCLIExhaustion(t *testing.T) {
	for _, mode := range []string{"headless", "pane"} {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			dir := h.gateRepo(1)
			h.sessionPi(gatePasses(nil))
			if mode == "headless" {
				t.Setenv("HERDR_ENV", "")
			}
			writeFiles(t, filepath.Dir(userConfigPath()), map[string]string{filepath.Base(userConfigPath()): "[gate_budget]\nrequests = 1\n"})
			code, out := h.run("gate", "spec.md", "--schema-version", "2", "--json")
			if code != 1 {
				t.Fatalf("exit %d: %s", code, out)
			}
			var rec gateRecord
			if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "gate.json")), &rec); err != nil {
				t.Fatal(err)
			}
			if rec.State != "incomplete" || len(rec.PassErrors) != 2 {
				t.Fatalf("want one completed and two incomplete checks: %+v", rec)
			}
			for _, err := range rec.PassErrors {
				if !strings.Contains(err, "budget_exhausted") {
					t.Fatal(err)
				}
			}
			if mode == "pane" && len(h.gateCalls("agent", "start")) != 1 {
				t.Fatal("unadmitted pane started")
			}
		})
	}
}
