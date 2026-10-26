package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBriefSummaryPreservesLegacySheet(t *testing.T) {
	code := rankRepo(t, map[string]string{"cli/main.py": "def dispatch_command(argv):\n    return argv[0]\n"})
	e := newCtxEnv(code, ctxOpts{})
	pl := e.prepareBrief(briefInput{ID: "cli", Text: "Use dispatch_command", Owns: []string{"cli/main.py"}})
	f := &symFact{Symbol: "dispatch_command", Lang: "python", Tier: "fixture", Defs: []symDef{{Where: "cli/main.py:1", Kind: "function", Match: "exact"}}}
	pl.goSyms = []string{"dispatch_command"}
	pl.facts["dispatch_command"] = f
	d := e.assemble(pl)
	var legacy strings.Builder
	e.symView(*f, symOpts{Compact: true}).write(&legacy, ctxFmt{Compact: true}, nil, 0, 0)
	if len(d.Sheet) != 1 || d.Sheet[0] != strings.TrimRight(legacy.String(), "\n") {
		t.Fatalf("legacy Sheet changed: %v", d.Sheet)
	}
	if len(d.interfaceSummary) != 1 || !strings.Contains(d.interfaceSummary[0], "signature: def dispatch_command") {
		t.Fatal("dossier has no native signature summary")
	}
}

// jbOrderFake is a jbcontext stand-in for `search`. The query "q<k>" gets one hit
// in lib/f<k>.go after <dir>/delay.<k> seconds (0.05 when absent), or an error
// when <dir>/fail.<k> exists, and answers 429 to the first <dir>/limit calls. With <dir>/onefile every query hits lib/f0.go, q0
// at line 1 and any other at line 2. It logs the number of searches running at
// once to conc.log and the order the answers left in done.log.
func jbOrderFake(t *testing.T, n int, slots int) (code, dir string) {
	t.Helper()
	files := map[string]string{}
	for k := range n {
		files[fmt.Sprintf("lib/f%d.go", k)] = "package lib\nfunc F() {}\n"
	}
	code = rankRepo(t, files)
	dir = t.TempDir()
	bin := filepath.Join(dir, "jbcontext")
	script := `#!/bin/sh
D="` + dir + `"
[ "$1" = search ] || exit 0
for last; do :; done
k=${last#q}
echo "$$" >> "$D/calls.log"
if [ -f "$D/limit" ] && [ "$(wc -l < "$D/calls.log" | tr -d ' ')" -le "$(cat "$D/limit")" ]; then
  echo "Limit reached. Please try again later (HTTP 429)" >&2
  exit 1
fi
mkdir "$D/run.$$"
n=$(ls "$D" | grep -c '^run\.')
echo "$n" >> "$D/conc.log"
sleep "$(cat "$D/delay.$k" 2>/dev/null || echo 0.05)"
echo "$k" >> "$D/done.log"
rmdir "$D/run.$$"
if [ -f "$D/fail.$k" ]; then echo "jbcontext exploded" >&2; exit 1; fi
f=$k; c='package lib'; l=1
if [ -f "$D/onefile" ]; then
  f=0
  if [ "$k" != 0 ]; then c='func F() {}'; l=2; fi
fi
printf '{"type":"search_result","results":[{"result":{"sourcePosition":{"relativePath":"lib/f%s.go"}},"content":"%s\\n","contentStartLine":%s}]}\n' "$f" "$c" "$l"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_JBCONTEXT", bin)
	t.Setenv("ORCH_JB_SLOTS", fmt.Sprint(slots))
	t.Setenv("ORCH_JB_BACKOFF_MS", "10")
	return code, dir
}

func conceptJobs(n int) []lookupJob {
	var jobs []lookupJob
	for k := range n {
		jobs = append(jobs, lookupJob{name: fmt.Sprintf("q%d", k)})
	}
	return jobs
}

func TestRunJobsReturnsAnswersInJobOrderWhateverFinishesFirst(t *testing.T) {
	code, dir := jbOrderFake(t, 10, 4)
	for k := range 10 { // the earlier the job, the later its answer
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("delay.%d", k)), fmt.Appendf(nil, "0.%d", 9-k), 0o644)
	}
	outs := newCtxEnv(code, ctxOpts{}).runJobs(conceptJobs(10))
	for k, o := range outs {
		if o.err != nil || len(o.hits) != 1 || o.hits[0].Path != fmt.Sprintf("lib/f%d.go", k) {
			t.Fatalf("job %d got %+v", k, o)
		}
	}
	done := jbRateLines(dir, "done.log")
	if len(done) != 10 || strings.Join(done, " ") == "0 1 2 3 4 5 6 7 8 9" {
		t.Fatalf("the searches did not finish out of order, so the test proves nothing: %v", done)
	}
}

func TestRunJobsNeverRunsMoreSearchesThanTheSlotCap(t *testing.T) {
	for _, slots := range []int{2, 7} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			code, dir := jbOrderFake(t, 24, slots)
			os.WriteFile(filepath.Join(dir, "delay.0"), []byte("0.2"), 0o644)
			newCtxEnv(code, ctxOpts{}).runJobs(conceptJobs(24))
			if got := jbRateMax(dir); got > slots || got < min(slots, 2) {
				t.Fatalf("most searches at once = %d, cap %d", got, slots)
			}
		})
	}
}

func TestRunJobsReportsAFailedLookupInItsOwnPlace(t *testing.T) {
	code, dir := jbOrderFake(t, 6, 3)
	os.WriteFile(filepath.Join(dir, "fail.3"), nil, 0o644)
	outs := newCtxEnv(code, ctxOpts{}).runJobs(conceptJobs(6))
	for k, o := range outs {
		switch {
		case k == 3 && (o.err == nil || len(o.hits) != 0):
			t.Fatalf("job 3 should have failed: %+v", o)
		case k != 3 && (o.err != nil || len(o.hits) != 1 || o.hits[0].Path != fmt.Sprintf("lib/f%d.go", k)):
			t.Fatalf("job %d got %+v", k, o)
		}
	}
}

func TestRunJobsAsksAgainForSearchesTheServiceRefused(t *testing.T) {
	code, dir := jbOrderFake(t, 8, 4)
	os.WriteFile(filepath.Join(dir, "limit"), []byte("12"), 0o644)
	waits := 0
	old := lookupCooldownWait
	lookupCooldownWait = func(bin string) {
		waits++
		jbCoolMu.Lock()
		delete(jbCoolUntil, bin)
		jbCoolMu.Unlock()
	}
	defer func() { lookupCooldownWait = old }()
	outs := newCtxEnv(code, ctxOpts{}).runJobs(conceptJobs(8))
	for k, o := range outs {
		if o.err != nil || len(o.hits) != 1 || o.hits[0].Path != fmt.Sprintf("lib/f%d.go", k) {
			t.Fatalf("job %d got %+v", k, o)
		}
	}
	if waits == 0 {
		t.Fatal("no search was refused, so the test proves nothing")
	}
}

func readyCLIPacket() *gateQuestionPacket {
	return &gateQuestionPacket{Goal: "Make command dispatch predictable", KnownFacts: []string{"The parser accepts --help and rejects an empty command."}, Unknown: "Which empty-command behavior should ship?", Options: []gatePacketOption{{"a", "Print help", "Shows available commands", "More stdout output"}, {"b", "Return an error", "Scripts detect missing input", "Users must request help separately"}}, Recommendation: "a", Rationale: "The owner wants discoverable commands", Authority: "Owner decides user-visible defaults", NextAction: "Owner selects an option, then the lead adds CLI acceptance tests"}
}

func TestBriefLookupExecutionStatesAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lk           evidenceLookup
		exec         lookupExecution
		status       string
		completeness evidenceCompleteness
	}{
		{"failed", lookupRecord("dispatch", "cli", "concept", 0, 4, fmt.Errorf("index unavailable"), ""), lookupFailed, "failed", evidenceUnknown},
		{"empty", lookupRecord("dispatch", "cli", "text", 0, 0, nil, ""), lookupSucceeded, "empty", evidenceComplete},
		{"unavailable", lookupRecord("dispatch", "cli", "concept", 0, 4, nil, "backend missing"), lookupUnavailable, "", evidenceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.lk.Validate(evidenceEnvelope{Completeness: evidenceUnknown}); err != nil {
				t.Fatal(err)
			}
			if tc.lk.ExecutionStatus != tc.exec || tc.lk.Status != tc.status || tc.lk.Completeness != tc.completeness {
				t.Fatalf("lookup state = %s/%q/%s, want %s/%q/%s", tc.lk.ExecutionStatus, tc.lk.Status, tc.lk.Completeness, tc.exec, tc.status, tc.completeness)
			}
		})
	}
	l := lookupRecord("dispatch", "cli", "concept", 0, 4, nil, "")
	if l.ExecutionStatus != lookupSucceeded || l.CountKind != evidenceCountUnknown || l.Completeness != evidencePartial {
		t.Fatalf("semantic window manufactured absence: %+v", l)
	}
}

func TestBriefOneBudgetIncludesEverySection(t *testing.T) {
	d := &ctxBrief{ID: "cli", Files: []string{strings.Repeat("ranked ", 100)}, Docs: []string{strings.Repeat("docs ", 200)}, Sheet: []string{strings.Repeat("signature ", 200)}, Like: []string{strings.Repeat("candidate ", 200)}}
	for _, budget := range []int{500, 2000, 6000} {
		got := d.Render("## CLI", "package", budget)
		if len(got) > budget {
			t.Fatalf("budget %d produced %d bytes", budget, len(got))
		}
		again := d.Render("## CLI", "package", budget)
		if again != got {
			t.Fatal("render changed without new evidence")
		}
	}
}

func TestEnrichPacketsReuseReviewFieldsAndGateReadiness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		packet  *gateQuestionPacket
		failure string
		want    string
	}{{"ready", readyCLIPacket(), "", "ready"}, {"incomplete", nil, "", "missing_tradeoffs"}, {"failed", readyCLIPacket(), "Code search failed: backend exited", "failed_retrieval"}} {
		t.Run(tc.name, func(t *testing.T) {
			q := gateQuestion{Package: "cli", Question: "Which empty-command behavior?", Decisive: true, Packet: tc.packet}
			src := enrichedCard(enrichedQuestion{Q: q, Tag: "owner", CodeReview: tc.failure}, "spec.md")
			r := checkCardReady(src)
			if r.Status != tc.want {
				t.Fatalf("ready=%+v card=%s", r, src)
			}
			if tc.packet != nil {
				wantContains(t, src, tc.packet.Goal, tc.packet.Rationale, tc.packet.NextAction)
			}
		})
	}
}

func TestBriefAdversaryCoveragePersistsWithoutChangingTheReview(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{`{"findings":[]}`, "coverage: not reported"}, {`{"findings":[],"coverage_gaps":[]}`, "coverage: reviewer reported no further gaps"}, {`{"findings":[],"coverage_gaps":["incoming registrations not checked: turn budget"]}`, "reported coverage gap: incoming registrations not checked"}} {
		calls := 0
		apply := func(raw string, rec *gateRecord, repo string, sp *spec) error {
			calls++
			if raw != tc.raw {
				t.Fatal("coverage wrapper changed the review reply")
			}
			return nil
		}
		p := newGatePass("adversary-a", "adversary", "stable spec", "body", "TAG", "Inspect"+adversaryScope([]string{"cmd/main.go"}), `{"findings":[]}`, apply)
		p = gateEvidenceContract(p)
		p = gateEvidenceContract(p)
		if strings.Contains(p.task, "only when one of these imports") {
			t.Fatal("forward-only exploration rule survived")
		}
		rec := &gateRecord{}
		if err := p.apply(tc.raw, rec, "", nil); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || len(rec.Notes) != 1 || !strings.Contains(rec.Notes[0], "adversary-a "+tc.want) {
			t.Fatalf("coverage lost or wrapper repeated: %v calls=%d", rec.Notes, calls)
		}
	}
}

func TestBriefStablePrefixClassesIgnoreGroupOrder(t *testing.T) {
	shared := "same byte-identical spec and package index"
	for _, class := range []string{"adversary", "group"} {
		a := newGatePass("first", class, shared, "group B", "TAG", "task B", `{"questions":[]}`, nil)
		b := newGatePass("second", class, shared, "group A", "TAG", "task A", `{"questions":[]}`, nil)
		a = gateEvidenceContract(a)
		b = gateEvidenceContract(b)
		originalShared, _ := gatePromptParts(shared, "group B", "TAG", "task B", `{"questions":[]}`)
		if a.shared != originalShared {
			t.Fatal("evidence hook changed original shared prefix bytes")
		}
		if a.shared != b.shared || !strings.HasPrefix(a.prompt, a.shared) || !strings.HasPrefix(b.prompt, b.shared) {
			t.Fatal("per-group state contaminated shared prefix")
		}
		if class == "adversary" {
			wantContains(t, a.task, "incoming AND outgoing", "registrations/configuration", "tests", "coverage_gaps")
		}
	}
}

func TestGateCLIPythonAndTypeScriptPacketsRenderWithoutFormatterCalls(t *testing.T) {
	for _, lang := range []string{"py", "ts"} {
		t.Run(lang, func(t *testing.T) {
			h := setup(t)
			t.Setenv("HERDR_ENV", "")
			t.Setenv("ORCH_GOPLS", filepath.Join(h.tmp, "no-gopls"))
			t.Setenv("ORCH_TSLS", filepath.Join(h.tmp, "no-tsls"))
			path := "lib/dispatch." + lang
			body := "def dispatch_command(argv):\n    return argv[0]\n"
			snippet := `def dispatch_command(argv):\n    return argv[0]`
			if lang == "ts" {
				body = "export function dispatch_command(argv: string[]): string {\n  return argv[0];\n}\n"
				snippet = `export function dispatch_command(argv: string[]): string {\n  return argv[0];\n}`
			}
			writeTree(t, h.repo, map[string]string{path: body, "cli/main." + lang: body})
			src := planHeader + "### cli: Command dispatcher\nkind: ship\nowns: cli/main." + lang + "\n\nExpose `dispatch_command` from " + path + " to command-line users.\n#### Builds\n- Dispatch the selected subcommand.\n  id: dispatch\n  search: Code that dispatches the selected command using `dispatch_command`\n#### Acceptance\nTests pass.\n" + planTail
			h.commitSpec(src)
			fakeJbResults(t, [][3]string{{path, "1", snippet}})
			q := gateQuestion{Question: "Which default behavior should `dispatch_command` use when a command is missing?", Decisive: true, Packet: readyCLIPacket()}
			answer, _ := json.Marshal(map[string]any{"id": "cli", "will_change": []string{}, "claims": []any{}, "questions": []gateQuestion{q}, "reuse": []any{}})
			dir := h.fakePi(gatePasses(map[string]string{"package-cli": string(answer)}))
			code, out := h.run("gate", "spec.md")
			if code != 1 {
				t.Fatalf("gate exit=%d %s", code, out)
			}
			gdir := filepath.Join(runRoot(h.repo), "gate", specHash([]byte(src)))
			rec := readRecord(t, gdir)
			wantContains(t, strings.Join(rec.Notes, "\n"), "adversary-a coverage: not reported", "adversary-b coverage: not reported")
			packetJSON := mustRead(t, filepath.Join(gdir, "questions", "packets.json"))
			toon := mustRead(t, filepath.Join(gdir, "questions", "packets.toon"))
			if fmt.Sprint(decodedOutput(t, toon, outputAgent)) != fmt.Sprint(decodedOutput(t, packetJSON, outputHuman)) {
				t.Fatal("human/agent packet selection differs")
			}
			var before bytes.Buffer
			before.Write(mustRead(t, filepath.Join(dir, "calls.log")))
			ask := h.must("ask", "--cards", filepath.Join(gdir, "questions"))
			wantContains(t, ask, "questions: 1", "Print help", "Return an error")
			h.must("gate", "enrich", "spec.md")
			if before.String() != string(mustRead(t, filepath.Join(dir, "calls.log"))) {
				t.Fatal("rendering/enrichment made a formatter model request")
			}
			cache, _ := os.ReadFile(filepath.Join(dir, "cache.log"))
			if len(strings.Fields(string(cache))) > 3 {
				t.Fatal("unexpected additional model calls")
			}
		})
	}
}
