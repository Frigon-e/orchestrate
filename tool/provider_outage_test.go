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

func TestOutageTriageAutoNeverInventsTier(t *testing.T) {
	for _, mode := range outageModes {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
			calls := installOutageTransport(t, mode)
			if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(autoConfig), 0600); err != nil {
				t.Fatal(err)
			}
			h.task("a", "--harness", "auto", "--model", "auto", "--effort", "auto")
			_, out := h.run("spawn", "a", "--auto")
			wantContains(t, out, "choose a profile by hand", "--tier settled|standard|planning")
			fm, _, _ := readTask(filepath.Join(h.dir("a"), "task.md"))
			if fm["tier"] != "" || strings.Contains(out, "would use:") {
				t.Fatalf("outage invented a tier: %v %s", fm, out)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls=%d", calls.Load())
			}
			rows := readTriage(filepath.Dir(h.dir("a")))
			if len(rows) == 0 || rows[len(rows)-1].Skipped == "" {
				t.Fatalf("outage skip not recorded: %+v", rows)
			}
		})
	}
}

func TestOutageGateConnectionRefusedUsesFallback(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	t.Setenv("HERDR_ENV", "")
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", `echo 'dial tcp: connection refused' >&2; exit 1`))
	log := filepath.Join(h.tmp, "fallback.log")
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", `echo called >> '`+log+`'; echo '{"type":"result","result":"{}"}'`))
	rr, _ := roleSite(profile{Harness: "pi", Model: "test"}, profile{Harness: "claude", Model: "haiku"}).newRoleRun("gate", modelOptions{})
	rec := gateBudgetRecord(gateRunLimits([]gatePass{{budget: 40}}, nil))
	p := gatePass{name: "group:test", class: "package", prompt: "review", apply: func(string, *gateRecord, string, *spec) error { return nil }}
	runGatePasses(filepath.Join(h.tmp, "gate"), h.repo, "test", []gatePass{p}, rec, nil, gateOpts{runner: rr, headless: true})
	if !exists(log) {
		t.Fatalf("provider outage did not fall back: %+v", rec.PassErrors)
	}
	if rec.Budget.Requests != 2 {
		t.Fatalf("fallback reset budget: %+v", rec.Budget)
	}
	if exists(healthPath()) {
		t.Fatal("outage wrote health")
	}
}

func TestOutageTriageSpawnAndQuick(t *testing.T) {
	// Transport/parser variants are covered by auto triage and API role tests.
	// This boundary checks each command's shadow/enforce policy and cleanup.
	for _, mode := range []string{"refused"} {
		for _, enforce := range []bool{false, true} {
			for _, command := range []string{"spawn", "quick"} {
				t.Run(fmt.Sprintf("%s/%v/%s", mode, enforce, command), func(t *testing.T) {
					h := setup(t)
					quickRepo(t, h, "true")
					t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
					calls := installOutageTransport(t, mode)
					policy := "shadow"
					if enforce {
						policy = "enforce"
					}
					cfg := fmt.Sprintf("triage_mode=%q\nquick_mode=%q\n", policy, policy) + autoConfig
					if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(cfg), 0600); err != nil {
						t.Fatal(err)
					}
					args := []string{"spawn", "a"}
					if command == "spawn" {
						h.task("a")
					} else {
						args = []string{"quick", "a", "--owns", "slug.py", quickRequest}
					}
					code, out := h.run(args...)
					if enforce {
						if code == 0 {
							t.Fatalf("enforce started lead: %s", out)
						}
						wantContains(t, out, "choose a profile by hand", "--tier settled|standard|planning")
					} else {
						if code != 0 {
							t.Fatal(out)
						}
						wantContains(t, out, "spawned lead-a")
					}
					rows := readTriage(filepath.Dir(h.dir("a")))
					if len(rows) != 1 || rows[0].Skipped == "" || rows[0].Tier != "" {
						t.Fatalf("missing/duplicate skip or invented tier: %+v", rows)
					}
					raw, _ := json.Marshal(rows)
					if strings.Contains(string(raw), "fake-test-key") || strings.Contains(string(raw), "127.0.0.1") || strings.Contains(string(raw), "{broken") || strings.Contains(string(raw), "unauthorized") {
						t.Fatalf("raw provider data leaked: %s", raw)
					}
					if command == "quick" && enforce && exists(h.dir("a")) {
						t.Fatal("quick failed to clean up")
					}
					if calls.Load() != 1 {
						t.Fatalf("calls=%d", calls.Load())
					}
					if exists(healthPath()) {
						t.Fatal("outage changed health")
					}
				})
			}
		}
	}
}

func TestOutageAPIAdjudicatorAndClassifier(t *testing.T) {
	for _, mode := range outageModes {
		t.Run(mode, func(t *testing.T) {
			setup(t)
			t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
			calls := installOutageTransport(t, mode)
			env := adjudicationFixture(t)
			rr := adjudicationRoleFixture(t, profile{Harness: "api", Provider: "openrouter", Model: "fake-decider"})
			budget := newRunBudget(defaultAdjudicationLimits(), time.Now())
			audit := executeAdjudication(rr, budget, env, func(ctx context.Context, p profile, req adjudicationRequest) (adjudicationResponse, error) {
				a, m, u, e := postDecisionsWithContext(ctx, p, req.State, req.Questions)
				return adjudicationResponse{Decisions: a, ServedModel: m, Usage: executionUsage{Availability: "unknown"}, NativeUsage: &u}, e
			})
			if audit.Normalized != nil || audit.Reason != reviewDependencyUnavailable || budget.Requests < 1 || budget.Requests > 2 {
				t.Fatalf("outage resolved or looped: %+v budget=%+v", audit, budget)
			}
			applyAdjudication(env.Review, audit)
			result := ReduceReview(env.Subject, env.Review.Checks, env.Review.Answers, nil, env.Review.Policy)
			if result.State != "incomplete" {
				t.Fatalf("outage approved check: %+v", result)
			}
			b := classifierFixture()
			before, _ := json.Marshal(b)
			spent := &classifierSpend{}
			records := classifierRunTest(t, b, spent, func() bool { return true }, postDecisionsWithContext)
			after, _ := json.Marshal(b)
			last := records[len(records)-1]
			if string(before) != string(after) || last.Status != "unavailable" || len(last.Results) != 0 || spent.Requests > 2 {
				t.Fatalf("classifier changed production input or hid outage: %+v", records)
			}
			if calls.Load() > 4 {
				t.Fatalf("retry loop: %d", calls.Load())
			}
		})
	}
}

func TestOutageHealthHierarchyAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		limit   string
		blocked []string
	}{
		{"pi", []string{"pi:openrouter", "pi:openrouter:anthropic", "pi:anthropic", "pi:openai"}},
		{"pi:openrouter", []string{"pi:openrouter", "pi:openrouter:anthropic"}},
		{"pi:openrouter:anthropic", []string{"pi:openrouter:anthropic"}},
		{"pi-anthropic", []string{"pi:anthropic"}},
		{"pi:openai", []string{"pi:openai"}},
		{"claude", []string{"claude"}},
	} {
		t.Run(tc.limit, func(t *testing.T) {
			h := setup(t)
			h.must("health", "set", tc.limit, "limited", "--for", "3h")
			lanes := []string{"pi:openrouter", "pi:openrouter:anthropic", "pi:anthropic", "pi:openai", "claude"}
			for _, l := range lanes {
				want := false
				for _, b := range tc.blocked {
					want = want || b == l
				}
				_, _, got := limitedBy(loadHealth(h.now), l)
				if got != want {
					t.Fatalf("limit %s lane %s got %v", tc.limit, l, got)
				}
			}
			h.must("health", "set", tc.limit, "ok")
			for _, l := range lanes {
				if _, _, down := limitedBy(loadHealth(h.now), l); down {
					t.Fatalf("recovery missed %s", l)
				}
			}
		})
	}
}

func TestOutageTriageRecoveryNextRun(t *testing.T) {
	h := setup(t)
	h.task("a")
	t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
	installOutageTransport(t, "503")
	h.must("triage", "a")
	rows := readTriage(filepath.Dir(h.dir("a")))
	if len(rows) != 1 || rows[0].Skipped == "" {
		t.Fatal(rows)
	}
	installOutageTransport(t, "recovered")
	wantContains(t, h.must("triage", "a"), "a: settled")
	h.must("health", "set", "pi:openrouter", "limited", "--for", "3h")
	wantContains(t, h.must("triage", "a", "--force"), "skipped: decider off")
	h.must("health", "set", "pi:openrouter", "ok")
	wantContains(t, h.must("triage", "a", "--force"), "a: settled")
}

func TestOutageSweepCannotSettleWithoutIndependentPasses(t *testing.T) {
	for _, missing := range []string{"missing", "unreadable", "malformed"} {
		t.Run(missing, func(t *testing.T) {
			h := setup(t)
			topic := filepath.Join(h.tmp, "topic")
			if e := os.MkdirAll(filepath.Join(h.repo, ".agents"), 0700); e != nil {
				t.Fatal(e)
			}
			writeFiles(t, h.repo, map[string]string{"source.py": "print('same')\n", ".agents/orchestrate.toml": "human_only=[\"Billing\"]\n"})
			sweepCard(t, topic, "q", "", "a", "b")
			sweepCard(t, topic, "human", "Billing", "a", "b")
			sweepPassFile(t, topic, "healthy", "q", ans("a", "source.py:1"))
			path := filepath.Join(topic, "passes", "down", "q.json")
			if missing == "unreadable" {
				if e := os.MkdirAll(path, 0700); e != nil {
					t.Fatal(e)
				}
			}
			if missing == "malformed" {
				if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
					t.Fatal(e)
				}
				writeFiles(t, topic, map[string]string{"passes/down/q.json": "{broken"})
			}
			out := h.must("sweep", topic)
			var record struct {
				Questions map[string]struct {
					Status string `json:"status"`
				} `json:"questions"`
			}
			if e := json.Unmarshal(mustRead(t, filepath.Join(topic, "sweep", "results.json")), &record); e != nil {
				t.Fatal(e)
			}
			if record.Questions["q"].Status != "recheck" || record.Questions["human"].Status != "user" {
				t.Fatalf("outage settled insufficient passes or human question: %s %+v", out, record)
			}
		})
	}
}

func TestOutageSpecQueriesCLIUsesConfiguredRole(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	writeFiles(t, h.repo, map[string]string{"spec.md": planHeader + "### api: API\nkind: ship\nowns: app.py\n\nBody.\n#### Builds\n- Lists gateways.\n#### Acceptance\nTests.\n" + planTail})
	log := filepath.Join(h.tmp, "spec-calls")
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", `echo called >> '`+log+`'; echo 'HTTP 503' >&2; exit 1`))
	writeFiles(t, filepath.Dir(os.Getenv("ORCH_CONFIG")), map[string]string{filepath.Base(os.Getenv("ORCH_CONFIG")): "[roles.spec_queries]\nuse=[{harness=\"claude\",model=\"haiku\"}]\n"})
	h.must("spec", "queries", "spec.md", "--generate", "--all", "--dry-run")
	if exists(log) {
		t.Fatal("dry-run contacted the runner")
	}
	_, out := h.run("spec", "queries", "spec.md", "--generate", "--all")
	wantContains(t, out, "spec_queries", "skipped", "503")
	if strings.Contains(out, "no runner configured") {
		t.Fatal("CLI never reached configured role")
	}
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", `printf '%s' '{"type":"result","result":"#### Builds\\n- Lists gateways.\\n  search: Code that lists gateways"}'`))
	wantContains(t, h.must("spec", "queries", "spec.md", "--generate", "--all"), "search: Code that lists gateways")
	if exists(healthPath()) {
		t.Fatal("spec outage wrote health")
	}
}

func TestOutageOptionalRolesNeverReturnSilentEmpty(t *testing.T) {
	for _, role := range []string{"spec_queries"} {
		for _, mode := range []string{"refused", "429", "503", "401", "empty"} {
			t.Run(role+"/"+mode, func(t *testing.T) {
				h := setup(t)
				script := "echo 'HTTP " + mode + "' >&2; exit 1"
				if mode == "refused" {
					script = "echo 'connection refused' >&2; exit 1"
				}
				if mode == "empty" {
					script = "exit 0"
				}
				t.Setenv("ORCH_PI", fakeRunner(t, "pi", script))
				s := site{repo: h.repo, cfg: config{userConfig: userConfig{Roles: map[string]modelRole{role: {Use: []profile{{Harness: "pi", Model: "test"}}}}}}}
				text, e := s.runModel(role, "Propose read-only optional hints")
				if e == nil || text != "" {
					t.Fatalf("optional role silently succeeded: %q %v", text, e)
				}
				if exists(healthPath()) {
					t.Fatal("optional outage changed health")
				}
			})
		}
	}
}

func TestOutageLeadDispatchAndRespawnDecision(t *testing.T) {
	h := setup(t)
	h.task("running", "--harness", "pi", "--model", "openai/gpt-test", "--effort", "high")
	header := strings.Replace(strings.Replace(planHeader, "harness: claude", "harness: pi", 1), "model: sonnet", "model: openai/gpt-test", 1)
	h.commitSpec(header + planPkg("api", "kind: ship\nowns: app.py", "") + planTail)
	h.must("health", "set", "pi:openai", "limited", "--for", "3h")
	for _, args := range [][]string{
		{"new", "blocked", "--kind", "ship", "--harness", "pi", "--model", "openai/gpt-test", "--effort", "high"},
		{"spawn", "running"},
		{"plan", "spec.md", "--skip-gate"},
	} {
		code, out := h.run(args...)
		if code == 0 {
			t.Fatalf("limited lane accepted: %v %s", args, out)
		}
		wantContains(t, out, "pi:openai is limited")
	}
	s, e := h.a.site()
	if e != nil {
		t.Fatal(e)
	}
	rule := &dispatchRule{Use: []profile{{Harness: "pi", Model: "openai/gpt-test"}, {Harness: "claude", Model: "sonnet", Degraded: true}}}
	if c := h.a.chooseProfile(s, "settled", "", rule, false, false); c.profile != nil || c.degradedOption == nil {
		t.Fatalf("degraded fallback needs approval: %+v", c)
	}
	if c := h.a.chooseProfile(s, "settled", "", rule, false, true); c.profile == nil || c.profile.Harness != "claude" {
		t.Fatalf("approved fallback unavailable: %+v", c)
	}
	// Respawn uses this exact preflight seam before mutating task or lead state.
	// Check the decision, not a real respawn or a new model process.
	_, _, _, _, e = h.a.preflight("running", &spawnOverride{harness: "claude", model: "sonnet", effort: "high"})
	if e != nil {
		t.Fatalf("healthy cross-harness recovery refused: %v", e)
	}
	_, _, _, _, e = h.a.preflight("running", &spawnOverride{harness: "pi", model: "openai/gpt-test", effort: "high"})
	if e == nil {
		t.Fatal("respawn onto dead lane accepted")
	}
	h.must("health", "set", "pi:openai", "ok")
	_, _, _, _, e = h.a.preflight("running", nil)
	if e != nil {
		t.Fatalf("recovered lane refused: %v", e)
	}
}

func TestOutageClassifierWorkerPreservesPublishedBytes(t *testing.T) {
	for _, mode := range outageModes {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			dir := t.TempDir()
			t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
			installOutageTransport(t, mode)
			src := []byte("# Subject\n")
			writeFiles(t, h.repo, map[string]string{"spec.md": string(src)})
			subject, e := captureGateSubject(h.repo, src)
			if e != nil {
				t.Fatal(e)
			}
			b := classifierFixture()
			b.Code = h.repo
			b.SpecPath = "spec.md"
			b.Subject = subject
			b.Choices = []profile{b.Model}
			writeFiles(t, filepath.Dir(os.Getenv("ORCH_CONFIG")), map[string]string{filepath.Base(os.Getenv("ORCH_CONFIG")): "[roles.question_classifier]\nuse=[{harness=\"api\",provider=\"openrouter\",model=\"fake-decider\"}]\n"})
			before := map[string]string{"report.md": "deterministic report\n"}
			for i := range b.Cards {
				c := &b.Cards[i]
				c.Revision = subjectDigest([]byte(c.Text))
				before["questions/"+c.ID+".md"] = c.Text
			}
			raw, _ := json.Marshal(gateRecord{Subject: subject})
			before["gate.json"] = string(raw)
			if e := os.MkdirAll(filepath.Join(dir, "questions"), 0700); e != nil {
				t.Fatal(e)
			}
			writeFiles(t, dir, before)
			if !classifierCurrent(dir, b) {
				t.Fatal("worker fixture is not current")
			}
			if e = classifierQueue(dir, b, func(string) error { return nil }); e != nil {
				t.Fatal(e)
			}
			if e = classifierWorker(dir, postDecisionsWithContext); e != nil {
				t.Fatal(e)
			}
			for path, want := range before {
				if got := string(mustRead(t, filepath.Join(dir, path))); got != want {
					t.Fatalf("outage changed published %s", path)
				}
			}
			rows := classifierReadTest(t, classifierLog(dir))
			last := rows[len(rows)-1]
			if last.Status != "unavailable" || len(last.Results) != 0 || len(last.Relations) != 0 {
				t.Fatalf("outage invisible or accepted: %+v", rows)
			}
		})
	}
}

func TestOutageDirectAnthropicFallback(t *testing.T) {
	for _, mode := range outageModes {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			dir := h.gateRepo(1)
			t.Setenv("HERDR_ENV", "")
			t.Setenv("ANTHROPIC_API_KEY", "fake-test-key")
			t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
			installOutageTransport(t, "recovered")
			failure := "echo 'HTTP " + mode + "' >&2; exit 1"
			switch mode {
			case "refused":
				failure = "echo 'connection refused' >&2; exit 1"
			case "timeout":
				failure = "echo 'request timed out' >&2; exit 1"
			case "malformed":
				failure = "printf '{broken'; exit 0"
			case "empty":
				failure = "exit 0"
			}
			script := `case " $* " in *" --model anthropic/"*) ` + failure + `;; esac
printf '%s' '{"packages":[{"id":"api","claims":[],"questions":[],"will_change":[],"reuse":[]}],"findings":[]}'`
			t.Setenv("ORCH_PI", fakeRunner(t, "pi", script))
			h.run("gate", "spec.md", "--json", "--runner", "pi:anthropic/claude-test,pi:openrouter/deepseek/flash")
			rec := readRecord(t, dir)
			if len(rec.PassErrors) != 0 {
				t.Fatalf("direct Anthropic did not fall back: %+v", rec.PassErrors)
			}
			for _, name := range []string{"group:api", "adversary-a", "adversary-b"} {
				used := rec.PassRunners[name]
				if mode == "malformed" || mode == "empty" {
					used = rec.PassRunners[name+"#retry"]
				}
				if used != "pi openrouter/deepseek/flash" {
					t.Fatalf("%s used %s", name, used)
				}
			}
			if exists(healthPath()) {
				t.Fatal("direct provider outage wrote health")
			}
		})
	}
}

func TestOutageProviderCombinationsTriage(t *testing.T) {
	for mask := 1; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			h := setup(t)
			h.task("a")
			t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
			calls := installOutageTransport(t, "recovered")
			for i, lane := range []string{"pi:openrouter", "claude", "pi:openai"} {
				if mask&(1<<i) != 0 {
					h.must("health", "set", lane, "limited", "--for", "3h")
				}
			}
			h.must("triage", "a")
			rows := readTriage(filepath.Dir(h.dir("a")))
			if len(rows) != 1 {
				t.Fatal(rows)
			}
			if mask&1 != 0 {
				if rows[0].Skipped == "" || rows[0].Tier != "" || calls.Load() != 0 {
					t.Fatalf("limited Decider used: %+v %d", rows, calls.Load())
				}
			} else {
				if rows[0].Tier != "settled" || calls.Load() != 1 {
					t.Fatalf("unrelated provider disabled Decider: %+v %d", rows, calls.Load())
				}
			}
		})
	}
}

func TestOutageTransportHangHonorsDeadline(t *testing.T) {
	setup(t)
	t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
	calls := installOutageTransport(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, _, e := postDecisionsWithContext(ctx, profile{Harness: "api", Provider: "openrouter", Model: "fake"}, nil, triageQuestions())
	if e == nil || time.Since(started) > time.Second || calls.Load() != 1 {
		t.Fatalf("hang escaped deadline: %v calls=%d", e, calls.Load())
	}
}

func TestOutageGateStallAndWallDeadline(t *testing.T) {
	for _, kind := range []string{"stall-fallback", "all-stalled", "wall-cap"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			dir := h.gateRepo(1)
			t.Setenv("HERDR_ENV", "")
			gateTimeout = 2 * time.Second
			gateStall = 200 * time.Millisecond
			gatePoll = 5 * time.Millisecond
			// A real runner can leave children holding its output pipes open.
			marker := filepath.Join(h.tmp, "escaped-child")
			hang := fmt.Sprintf("(sleep 1; printf leaked > %q) & wait", marker)
			t.Setenv("ORCH_PI", fakeRunner(t, "pi", hang))
			script := `printf '%s\n' '{"type":"result","result":"{\"packages\":[{\"id\":\"api\",\"claims\":[],\"questions\":[],\"will_change\":[],\"reuse\":[]}],\"findings\":[]}"}'`
			if kind != "stall-fallback" {
				script = hang
			}
			if kind == "wall-cap" {
				gateTimeout = 250 * time.Millisecond
				gateStall = time.Second
			}
			t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", script))
			started := time.Now()
			h.run("gate", "spec.md", "--json", "--runner", "pi:test,claude:haiku")
			rec := readRecord(t, dir)
			if time.Since(started) > 3*time.Second {
				t.Fatal("hang escaped original pass deadline")
			}
			if kind == "stall-fallback" {
				for _, name := range []string{"group:api", "adversary-a", "adversary-b"} {
					if rec.PassRunners[name] != "claude haiku" {
						t.Errorf("%s did not use healthy fallback: %+v", name, rec.PassRunners)
					}
				}
				if len(rec.PassErrors) != 0 {
					t.Fatalf("healthy fallback failed: %+v", rec.PassErrors)
				}
			} else {
				if rec.Reduction == nil || rec.Reduction.State != "incomplete" {
					t.Fatalf("hung gate passed: %+v", rec.Reduction)
				}
				want := reviewDependencyUnavailable
				if kind == "wall-cap" {
					want = reviewBudgetExhausted
				}
				found := false
				for _, r := range rec.Reduction.Reasons {
					found = found || r.Reason == want
				}
				if !found {
					t.Fatalf("%s reason: %+v", kind, rec.Reduction.Reasons)
				}
				if kind == "wall-cap" && !strings.Contains(fmt.Sprint(rec.PassErrors), "timed out after") {
					t.Fatal(rec.PassErrors)
				}
			}
			if rec.Budget.Requests > 6 {
				t.Fatalf("outage retry loop: %+v", rec.Budget)
			}
			if kind == "all-stalled" || kind == "wall-cap" {
				// Wait beyond the child's marker write to catch a killed shell
				// whose descendants survived cancellation.
				if left := time.Until(started.Add(1200 * time.Millisecond)); left > 0 {
					time.Sleep(left)
				}
				if exists(marker) {
					t.Fatal("a canceled runner's child kept running")
				}
			}
		})
	}
}

func TestOutageExitedCheckStopsChildren(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "escaped-child")
	started := time.Now()
	code, _ := runCheck(dir, fmt.Sprintf("(sleep 2; printf leaked > %q) &", marker))
	if code == 0 || time.Since(started) > 3*time.Second {
		t.Fatalf("check with orphaned output pipes: exit %d after %s", code, time.Since(started))
	}
	// The parent already exited, so context cancellation does not run Cancel.
	// Wait beyond the child's write to ensure return also cleaned up its group.
	if left := time.Until(started.Add(2200 * time.Millisecond)); left > 0 {
		time.Sleep(left)
	}
	if exists(marker) {
		t.Fatal("the exited check's child survived output-pipe cleanup")
	}
}

// Bit masks enumerate every nonempty subset of the three independent lanes.
// A Claude subscription and direct Anthropic API are distinct health lanes;
// the injected Anthropic outage covers both, without pretending OpenRouter is
// down just because one of its upstream model families is down.
func TestOutageGateProviderMatrix(t *testing.T) {
	for mask := 1; mask < 8; mask++ {
		// Exercise every lane subset once; error classification is shared by
		// those routes. Keep the full mode table for one failed provider and
		// the distinct invalid-reply diagnostics when every provider is down.
		modes := []string{"refused"}
		switch mask {
		case 1:
			modes = outageModes
		case 2, 7:
			modes = append(modes, "malformed", "empty")
		}
		for _, mode := range modes {
			t.Run(fmt.Sprintf("providers-%d/%s", mask, mode), func(t *testing.T) {
				h := setup(t)
				// This fixture has no Go production source or Go manifest.
				writeFiles(t, h.repo, map[string]string{"app.py": "def put(value):\n    return value\n", "app.ts": "export function put(value: string) { return value; }\n"})
				h.commitSpec(planHeader + planPkg("api", "kind: ship\nowns: app.py, app.ts", "") + planTail)
				dir := filepath.Join(filepath.Dir(h.dir("x")), "gate", specHash(mustRead(t, filepath.Join(h.repo, "spec.md"))))
				t.Setenv("HERDR_ENV", "")
				installOutageTransport(t, "recovered")
				t.Setenv("ORCH_TRIAGE_KEY", "fake-test-key")
				t.Setenv("OPENAI_API_KEY", "fake-test-key")
				t.Setenv("ANTHROPIC_API_KEY", "fake-test-key")
				t.Setenv("ORCH_TEST_OUTAGE_MASK", fmt.Sprint(mask))
				t.Setenv("ORCH_TEST_OUTAGE_MODE", mode)
				log := filepath.Join(h.tmp, "invocations")
				t.Setenv("ORCH_TEST_OUTAGE_LOG", log)
				script := `provider=2
while [ "$#" -gt 0 ]; do
 if [ "$1" = --model ]; then shift; case "$1" in openrouter/*) provider=1;; openai/*) provider=4;; esac; fi
 shift
done
printf '%s\n' "$provider" >> "$ORCH_TEST_OUTAGE_LOG"
if [ $((ORCH_TEST_OUTAGE_MASK & provider)) -ne 0 ]; then
 case "$ORCH_TEST_OUTAGE_MODE" in
 refused) echo 'connection refused' >&2; exit 1;;
 429|503|401) echo "HTTP $ORCH_TEST_OUTAGE_MODE" >&2; exit 1;;
 timeout) echo 'request timed out' >&2; exit 1;;
 malformed) if [ "$harness" = claude ]; then printf '%s' '{"type":"result","result":"{broken"}'; else printf '{broken'; fi; exit 0;;
 empty) if [ "$harness" = claude ]; then printf '%s' '{"type":"result","result":""}'; fi; exit 0;;
 esac
fi
printf '%s\n' '{"type":"result","result":"{\"packages\":[{\"id\":\"api\",\"claims\":[],\"questions\":[],\"will_change\":[],\"reuse\":[]}],\"findings\":[]}"}'
`
				// Pi writes final plain text; Claude wraps that text in its result event.
				piScript := strings.Replace(script, `printf '%s\n' '{"type":"result","result":"{\"packages\":[{\"id\":\"api\",\"claims\":[],\"questions\":[],\"will_change\":[],\"reuse\":[]}],\"findings\":[]}"}'`, `printf '%s\n' '{"packages":[{"id":"api","claims":[],"questions":[],"will_change":[],"reuse":[]}],"findings":[]}'`, 1)
				t.Setenv("ORCH_PI", fakeRunner(t, "pi", "harness=pi\n"+piScript))
				t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "harness=claude\n"+script))
				var order []string
				for _, down := range []bool{true, false} {
					for i, route := range []string{"pi:openrouter/deepseek/flash", "claude:haiku", "pi:openai/gpt-test"} {
						if (mask&(1<<i) != 0) == down {
							order = append(order, route)
						}
					}
				}
				_, out := h.run("gate", "spec.md", "--json", "--runner", strings.Join(order, ","))
				rec := readRecord(t, dir)
				if rec.Review == nil || rec.Reduction == nil {
					t.Fatalf("missing v2 records: %s", out)
				}
				if rec.Reduction.State == "error" {
					t.Fatalf("provider outage became system error: %+v", rec.Reduction)
				}
				if mask == 1 || mask == 2 || mask == 4 {
					if len(rec.PassErrors) != 0 {
						t.Errorf("single-provider failure did not recover on next route: %+v", rec.PassErrors)
					}
				}
				if rec.Budget.Requests > 12 {
					t.Fatalf("unbounded retries: %+v", rec.Budget)
				}
				if mask == 7 {
					if rec.Reduction.State != "incomplete" {
						t.Fatalf("all-down gate: %+v", rec.Reduction)
					}
					found := false
					for _, r := range rec.Reduction.Reasons {
						want := reviewDependencyUnavailable
						if mode == "malformed" || mode == "empty" {
							want = reviewMissingAnswer
						}
						if r.Reason == want {
							found = true
						}
					}
					if !found {
						t.Fatalf("all-down reason does not match transport-vs-invalid-reply contract: %+v", rec.Reduction.Reasons)
					}
					if mode == "empty" || mode == "malformed" {
						errors := fmt.Sprint(rec.PassErrors)
						cause := "no JSON object in the reply"
						if mode == "empty" {
							cause = "empty reply"
						}
						if !strings.Contains(errors, cause) {
							t.Fatalf("missing visible cause %q: %s", cause, errors)
						}
						for _, runner := range rec.PassRunners {
							if !strings.Contains(errors, runner) {
								t.Errorf("lost failed lane %s from diagnostic: %s", runner, errors)
							}
						}
					}
				}
				if !exists(filepath.Join(dir, "report.md")) {
					t.Fatal("outage erased deterministic artifacts")
				}
				if exists(healthPath()) {
					t.Fatal("outage wrote persistent health")
				}
				if mask == 7 && mode == "refused" {
					notes := strings.Join(rec.Notes, " ")
					for _, used := range rec.PassRunners {
						var lane string
						switch {
						case strings.HasPrefix(used, "pi openrouter/"):
							lane = "pi:openrouter"
						case strings.HasPrefix(used, "pi openai/"):
							lane = "pi:openai"
						case strings.HasPrefix(used, "claude "):
							lane = "claude"
						}
						if !strings.Contains(notes, "orch health set "+lane+" limited --for 3h") {
							t.Fatalf("missing exact manual hint for %s: %s", used, notes)
						}
					}
					t.Setenv("ORCH_TEST_OUTAGE_MASK", "0")
					h.run("gate", "spec.md", "--json", "--runner", strings.Join(order, ","))
					recovered := readRecord(t, dir)
					if len(recovered.PassErrors) != 0 || recovered.PassRunners["group:api"] != "pi openrouter/deepseek/flash" {
						t.Fatalf("run-local failure survived recovery: %+v %+v", recovered.PassErrors, recovered.PassRunners)
					}
				}
			})
		}
	}
}
