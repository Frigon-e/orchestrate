package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readRecord(t *testing.T, dir string) gateRecord {
	t.Helper()
	var rec gateRecord
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "gate.json")), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func piCalls(t *testing.T, h *harness) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(h.tmp, "fakepi", "calls.log"))
	return string(b)
}

func TestGatePutsThePackageBlocksBeforeTheTagAndHasNoDossier(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(2)
	h.sessionPi(gatePasses(nil))
	t.Setenv("HERDR_ENV", "")
	out := h.must("gate", "spec.md")
	calls := piCalls(t, h)
	// the fake pi logs "$*" on several lines; use the whole log from the first package line
	i, k := strings.Index(calls, "PACKAGES UNDER REVIEW"), strings.Index(calls, "GATE-GROUP")
	if i < 0 || k < i {
		t.Fatalf("order must be package blocks, tag (%d %d):\n%s", i, k, calls)
	}
	wantContains(t, calls, "api: Package api", "Budget: about 20 turns.", "Budget: about 40 turns.")
	if strings.Contains(calls, "Use the evidence dossier") || strings.Contains(calls, "dossier-manifest.json") || exists(filepath.Join(gdir, "dossier")) {
		t.Errorf("the gate produced a dossier:\n%s\n%s", out, calls)
	}
	if rec := readRecord(t, gdir); rec.Cache != "" || rec.ContextFiles != "" {
		t.Errorf("record: cache %q, context_files %q", rec.Cache, rec.ContextFiles)
	}
	if code, _ := h.run("gate", "spec.md", "--dossier", "maybe"); code == 0 {
		t.Error("a --dossier value was accepted")
	}
}

func TestGateDryRunShowsNoDossierSizesAndRunsNothing(t *testing.T) {
	h := setup(t)
	h.gateRepo(2)
	dir := h.fakePi(gatePasses(nil))
	out := h.must("gate", "spec.md", "--dry-run")
	wantContains(t, out, "turn budget: 20 soft, 28 hard")
	if strings.Contains(out, "    api: ") || strings.Contains(out, "dossier") || strings.Contains(out, "cache: ") {
		t.Errorf("the dry run showed a dossier:\n%s", out)
	}
	if exists(filepath.Join(dir, "calls.log")) {
		t.Error("pi was called")
	}
	// --only works in a dry run and names the valid keys when it is wrong.
	out = h.must("gate", "spec.md", "--dry-run", "--only", "group:api+1", "--experiment", "x")
	if strings.Contains(out, "adversary-a") || !strings.Contains(out, "pass: group:api+1") {
		t.Errorf("--only dry run:\n%s", out)
	}
	code, out := h.run("gate", "spec.md", "--dry-run", "--only", "nope", "--experiment", "x")
	if code == 0 || !strings.Contains(out, "group:api+1") {
		t.Errorf("a wrong key: %d %s", code, out)
	}
}

func TestGateExperimentIsolatedAndCompared(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(3)
	over := map[string]string{"package-api": `{"id":"api","will_change":[],"claims":[],"questions":[{"package":"api","question":"Which table holds the queue rows?","changes_result":true}],"reuse":[{"builds":"Put wrapper","existing":"Put","evidence":"internal/store.go:3","why":"same job"}]}`}
	h.sessionPi(gatePasses(over))
	t.Setenv("HERDR_ENV", "")

	out := h.must("gate", "spec.md", "--only", "api+2", "--experiment", "e1")
	view := wantGateState(t, out, "experiment")
	if view.Experiment != "e1 (groups group:api+2)" || view.Tokens == "" {
		t.Fatalf("experiment metadata/tokens missing: %+v", view)
	}
	if strings.Contains(piCalls(t, h), "GATE-ADVERSARY") {
		t.Error("an experiment ran an adversary")
	}
	for _, f := range []string{"gate.json", "report.md", "status"} {
		if exists(filepath.Join(gdir, f)) {
			t.Errorf("an experiment wrote the real %s", f)
		}
	}
	if exists(filepath.Join(gdir, "questions")) {
		t.Error("an experiment wrote the real questions dir")
	}
	exp := filepath.Join(gdir, "experiments", "e1")
	rec := readRecord(t, exp)
	if rec.Experiment != "e1" || rec.State != "experiment" || len(rec.Only) != 1 || len(rec.Questions) != 1 || len(rec.Findings) != 1 || rec.Findings[0].Source != "reuse" {
		t.Errorf("experiment record: %+v", rec)
	}
	for _, p := range []string{"report.md", "sessions/group-api+2"} {
		if !exists(filepath.Join(exp, p)) {
			t.Errorf("experiment lacks %s", p)
		}
	}
	wantContains(t, string(mustRead(t, filepath.Join(exp, "report.md"))), "Exploration per pass")

	h.sessionPi(gatePasses(over))
	h.must("gate", "spec.md", "--only", "group:api+2", "--experiment", "e2")
	cmp := h.must("gate", "compare", "spec.md", "e1", "e2")
	wantContains(t, cmp, "experiment A: e1 (groups group:api+2)", "experiment B: e2 (groups group:api+2)",
		"requests 1; tokens: input 100, cache read 300 (75% of input), cache write 5, output 10; max context 400 in group:api+2",
		"cost $0.00 (incomplete: 1 request(s) without cost)", "findings: 1 (1 verified; by source: reuse 1); questions: 1 (1 need a decision)",
		"pass group:api+2: ", "exploration: requests 1;", "overlap of findings: 1 matched, 1 in e1, 1 in e2, Jaccard 1.00", "overlap of questions: 1 matched")
	if code, out := h.run("gate", "compare", "spec.md", "e1", "missing"); code == 0 || !strings.Contains(out, "experiment missing: no record") {
		t.Errorf("compare with a missing experiment: %d %s", code, out)
	}
	// Argument checks.
	for _, args := range [][]string{{"gate", "spec.md", "--only", "api+2"}, {"gate", "spec.md", "--experiment", "e3"}, {"gate", "spec.md", "--only", "nope", "--experiment", "e3"}, {"gate", "spec.md", "--only", "api+2", "--experiment", "Bad Name"}} {
		if code, _ := h.run(args...); code == 0 {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestMatchItemsPairsByWords(t *testing.T) {
	m, onlyA, onlyB := matchItems(
		[]string{"Which table holds the queue rows?", "Should deletes be soft?"},
		[]string{"Which table holds queue rows", "Is the endpoint paginated?"})
	if m != 1 || len(onlyA) != 1 || !strings.Contains(onlyA[0], "soft") || len(onlyB) != 1 || !strings.Contains(onlyB[0], "paginated") {
		t.Errorf("matched %d, only A %v, only B %v", m, onlyA, onlyB)
	}
}

func TestGatePaneLauncherLeavesTheCacheSettingAlone(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	h.sessionPi(gatePasses(nil))
	h.must("gate", "spec.md", "--ctx-tool", "off")
	runs := h.gateCalls("pane", "run")
	if len(runs) != 3 {
		t.Fatalf("pane run calls = %d, want one per pass", len(runs))
	}
	for _, r := range runs {
		if script := fmt.Sprint(r.Extra["script"]); strings.Contains(script, "PI_CACHE_RETENTION") {
			t.Errorf("the launcher touched PI_CACHE_RETENTION: %s", script)
		}
	}
}

func TestGateTurnBudgetPromptsOnceAndInterruptsAtTheHardLimit(t *testing.T) {
	old := [2]int{gateTurnsBase, gateTurnsPerPackage}
	gateTurnsBase, gateTurnsPerPackage = 2, 2 // one package: soft 4, hard 5
	t.Cleanup(func() { gateTurnsBase, gateTurnsPerPackage = old[0], old[1] })

	t.Run("soft", func(t *testing.T) {
		h := setup(t)
		gdir := h.gateRepo(1)
		h.sessionPi(gatePasses(nil))
		os.WriteFile(h.fake+".budget", []byte("soft:4"), 0o644)
		out := h.must("gate", "spec.md")
		wantContains(t, wantGateState(t, out, "pass").Tokens, "budget hits: soft 1, hard 0")
		var prompts []string
		for _, c := range h.gateCalls("agent", "prompt") {
			prompts = append(prompts, fmt.Sprint(c.Extra["text"]))
			if c.Extra["wait"] != false {
				t.Errorf("the soft prompt waited: %v", c.Args)
			}
		}
		if len(prompts) != 1 || prompts[0] != "Budget reached (4 turns). Stop exploring and reply now with the final JSON from what you have." {
			t.Errorf("prompts: %q", prompts)
		}
		if n := len(h.gateCalls("agent", "send-keys")); n != 3 { // ctrl+d of each of the three passes only
			t.Errorf("send-keys calls = %d", n)
		}
		tk := readRecord(t, gdir).PassTokens["group:api"]
		if tk.BudgetHit != "soft" || tk.Turns != 4 {
			t.Errorf("group tokens: %+v", tk)
		}
		wantContains(t, piCalls(t, h), "Budget: about 4 turns.")
	})
	t.Run("hard", func(t *testing.T) {
		h := setup(t)
		gdir := h.gateRepo(1)
		h.sessionPi(gatePasses(nil))
		os.WriteFile(h.fake+".budget", []byte("hard:6"), 0o644)
		out := h.must("gate", "spec.md")
		wantContains(t, wantGateState(t, out, "pass").Tokens, "budget hits: soft 0, hard 1")
		var keys []string
		for _, c := range h.gateCalls("agent", "send-keys") {
			keys = append(keys, c.Args[3])
		}
		if !strings.Contains(strings.Join(keys, " "), "ctrl+c") {
			t.Errorf("no interrupt: %v", keys)
		}
		prompts := h.gateCalls("agent", "prompt")
		if len(prompts) != 1 || prompts[0].Extra["text"] != "Reply now with the final JSON only." || prompts[0].Extra["wait"] != true {
			t.Errorf("prompts: %+v", prompts)
		}
		if tk := readRecord(t, gdir).PassTokens["group:api"]; tk.BudgetHit != "hard" {
			t.Errorf("group tokens: %+v", tk)
		}
	})
	t.Run("context", func(t *testing.T) {
		old := gateContextSoft
		gateContextSoft = 50 // the fake session's requests read 110 tokens
		t.Cleanup(func() { gateContextSoft = old })
		h := setup(t)
		h.gateRepo(1)
		h.sessionPi(gatePasses(nil))
		os.WriteFile(h.fake+".budget", []byte("soft:1"), 0o644)
		h.must("gate", "spec.md")
		prompts := h.gateCalls("agent", "prompt")
		if len(prompts) != 1 || !strings.HasPrefix(fmt.Sprint(prompts[0].Extra["text"]), "Budget reached (context 110 tokens).") {
			t.Errorf("prompts: %+v", prompts)
		}
	})
}

func TestSessionProgressAndProfile(t *testing.T) {
	dir := t.TempDir()
	call := func(name, path string) string {
		return fmt.Sprintf(`{"name":%q,"type":"toolCall","arguments":{"path":%q}}`, name, path)
	}
	res := func(tool string, n int) string {
		return fmt.Sprintf(`{"type":"message","message":{"role":"toolResult","toolName":%q,"content":[{"type":"text","text":%q}]}}`, tool, strings.Repeat("x", n))
	}
	asst := func(in, cr int, parts ...string) string {
		return fmt.Sprintf(`{"type":"message","message":{"role":"assistant","content":[%s],"usage":{"input":%d,"cacheRead":%d,"output":1}}}`, strings.Join(parts, ","), in, cr)
	}
	writeFiles(t, dir, map[string]string{"s.jsonl": strings.Join([]string{
		`{"type":"session"}`,
		asst(10, 0, call("read", "docs/spec.md"), call("grep", "")),
		res("read", 300), res("grep", 5000),
		asst(20, 100, call("read", "docs/spec.md"), call("read", "a.go")),
		res("read", 40), res("read", 60),
		asst(30, 200, call("read", "a.go")),
		res("read", 10),
		asst(40, 300, `{"type":"text","text":"{}"}`),
	}, "\n") + "\n"})
	turns, ctx := sessionProgress(dir)
	if turns != 3 || ctx != 340 {
		t.Errorf("progress = %d turns, context %d; want 3 and 340", turns, ctx)
	}
	p, ok := profileSession(dir, "docs/spec.md")
	if !ok || p.Requests != 4 || p.Calls["read"] != 4 || p.Calls["grep"] != 1 || p.ResultChars != 5410 || p.SpecReads != 2 {
		t.Fatalf("profile = %+v %v", p, ok)
	}
	if len(p.Largest) != 5 || p.Largest[0] != (bigResult{"grep", 5000}) || p.Largest[1] != (bigResult{"read", 300}) {
		t.Errorf("largest = %+v", p.Largest)
	}
	if len(p.Repeated) != 2 || p.Repeated[0] != (pathCount{"a.go", 2}) || p.Repeated[1] != (pathCount{"docs/spec.md", 2}) {
		t.Errorf("repeated = %+v", p.Repeated)
	}
	wantContains(t, p.String(), "requests 4", "grep 1, read 4", "result chars 5410", "grep 5000", "a.go x2", "spec reads 2")
	tk, _ := sessionTokens(dir)
	if tk.Turns != 3 || tk.Requests != 4 {
		t.Errorf("tokens: %+v", tk)
	}
	if _, ok := profileSession(t.TempDir(), ""); ok {
		t.Error("an empty dir has a profile")
	}
}

func TestGateInsideHerdrNeedsItsTab(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	h.sessionPi(gatePasses(nil))
	os.WriteFile(h.fake+".tabfail", nil, 0o644)
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, out)
	}
	wantContains(t, wantGateState(t, out, "incomplete").PassErrors["all"], "the gate tab could not be opened in herdr, so no pass ran")
	if exists(filepath.Join(h.tmp, "fakepi", "calls.log")) {
		t.Error("a pass ran headless after the tab failed")
	}
	if rec := readRecord(t, gdir); rec.State != "incomplete" {
		t.Errorf("state %s", rec.State)
	}
}

func TestGateOutsideHerdrRefusesButDryRunWorks(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	h.sessionPi(gatePasses(nil))
	t.Setenv("HERDR_ENV", "")
	t.Setenv("ORCH_HEADLESS", "1")
	gateOutsideHerdr = false
	t.Cleanup(func() { gateOutsideHerdr = true })
	code, out := h.run("gate", "spec.md")
	if code != 1 || !strings.Contains(out, "gate passes run in herdr panes; start orch gate inside herdr") {
		t.Fatalf("outside herdr: %d %s", code, out)
	}
	if exists(filepath.Join(h.tmp, "fakepi", "calls.log")) {
		t.Error("a pass ran outside herdr")
	}
	if code, out := h.run("gate", "spec.md", "--dry-run"); code != 0 {
		t.Errorf("dry run outside herdr: %d %s", code, out)
	}
}

func TestGateDropsTheSessionsOfPassesThatSucceeded(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	t.Setenv("HERDR_ENV", "")
	h.sessionPi(gatePasses(nil))
	h.must("gate", "spec.md")
	if exists(filepath.Join(gdir, "sessions", "group-api")) {
		t.Error("a succeeding pass kept its session")
	}
}

func TestSessionProfileSeparatesOrchCtxFromRawShellAndRangedReads(t *testing.T) {
	cases := map[string]string{
		`orch ctx batch -q "find:a" -q "text:b"`:   "orch ctx batch",
		`orch ctx text foo --compact`:              "orch ctx",
		`orch ctx text foo | head -20`:             "orch ctx piped",
		`orch ctx batch -q "text:b" | head`:        "orch ctx batch piped",
		`grep -rn Gateway internal | head -40`:     "shell search",
		`cd internal && rg -n Gateway`:             "shell search",
		`sed -n '1,40p' internal/a.go`:             "shell read",
		`head -80 a.go`:                            "shell read",
		`go test ./...`:                            "shell other",
		`ls internal/crm | tail -5`:                "shell search",
		`orch ctx batch -q "text:b" 2>&1`:          "orch ctx batch",
		`echo hi`:                                  "shell other",
		`orch ctx find "Code that lists gateways"`: "orch ctx",
	}
	for cmd, want := range cases {
		if got := shellKind(cmd); got != want {
			t.Errorf("shellKind(%q) = %q, want %q", cmd, got, want)
		}
	}
	dir := t.TempDir()
	call := func(name, args string) string {
		return fmt.Sprintf(`{"type":"toolCall","name":%q,"arguments":%s}`, name, args)
	}
	asst := func(parts ...string) string {
		return fmt.Sprintf(`{"type":"message","message":{"role":"assistant","content":[%s]}}`, strings.Join(parts, ","))
	}
	writeFiles(t, dir, map[string]string{"s.jsonl": strings.Join([]string{
		asst(call("bash", `{"command":"orch ctx batch -q \"find:a\" -q \"text:b\" -q \"sym:c\""}`), call("bash", `{"command":"grep -rn x ."}`)),
		asst(call("read", `{"path":"a.go","offset":10,"limit":40}`), call("read", `{"path":"b.go"}`), call("read", `{"path":"c.go","limit":30}`)),
	}, "\n") + "\n"})
	p, ok := profileSession(dir, "")
	if !ok || p.Shell["orch ctx batch"] != 1 || p.Shell["shell search"] != 1 || p.BatchLookups != 3 || p.RangedReads != 2 || p.WholeReads != 1 {
		t.Fatalf("profile = %+v %v", p, ok)
	}
	wantContains(t, p.String(), "bash: orch ctx batch 1 (3 lookups), shell search 1", "reads: 2 ranged, 1 whole file")
}

func TestBenchToolRowsCountOrchCtxAgainstRawShell(t *testing.T) {
	r := &benchRun{Groups: map[string]benchGroup{
		"g1": {Exploration: &sessionProfile{Calls: map[string]int{"bash": 5, "read": 3}, Shell: map[string]int{"orch ctx batch": 2, "orch ctx": 1, "shell search": 2}, BatchLookups: 7, RangedReads: 2, WholeReads: 1}},
		"g2": {Exploration: &sessionProfile{Calls: map[string]int{"bash": 1, "read": 0}, Shell: map[string]int{"orch ctx piped": 1}, readBreakdownKnown: true}},
	}}
	want := map[string]float64{"orch ctx calls": 4, "orch ctx lookups (a batch counts each -q)": 9, "orch ctx piped into another tool": 1, "Raw shell search (grep, rg, find, ls)": 2, "Native reads": 3, "Native reads with a range": 2}
	for _, m := range benchToolRows {
		got := benchToolSum(r, nil, m.count)
		if w, ok := want[m.name]; ok {
			if got == nil || *got != w {
				t.Errorf("%s = %v, want %v", m.name, got, w)
			}
			delete(want, m.name)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing benchmark tool rows: %v", want)
	}
	// A profile recorded before shell calls were split cannot say what its bash calls were.
	old := &benchRun{Groups: map[string]benchGroup{"g": {Exploration: &sessionProfile{Calls: map[string]int{"bash": 4}}}}}
	if benchToolSum(old, nil, benchToolRows[0].count) != nil {
		t.Error("an old profile with bash calls must read as not recorded")
	}
}
