package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// worktreeWithSpec adds a linked worktree on branch feat holding a committed
// spec whose base is feat; the main checkout stays on main without it.
func worktreeWithSpec(t *testing.T, h *harness) string {
	t.Helper()
	wt := filepath.Join(h.tmp, "wt")
	h.git("worktree", "add", "-q", "-b", "feat", wt)
	body := strings.Replace(threePkgSpec(), "base: main", "base: feat", 1)
	writeFiles(t, wt, map[string]string{"spec.md": body})
	for _, args := range [][]string{{"add", "spec.md"}, {"commit", "-q", "-m", "spec"}} {
		if out, err := execGit(wt, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return wt
}

func execGit(dir string, args ...string) (string, error) {
	out, err := git(dir, args...)
	return out, err
}

func TestPlanFromLinkedWorktree(t *testing.T) {
	h := setup(t)
	wt := worktreeWithSpec(t, h)
	h.a.cwd = wt

	out := h.must("plan", "spec.md", "--dry-run", "--skip-gate")
	wantContains(t, out, "spec: spec.md", "packages: 3", "  store,ship,haiku,,would-create")
	if entries := allTasks(runRoot(h.repo)); len(entries) > 0 {
		t.Fatalf("dry run created tasks: %v", entries)
	}

	out = h.must("plan", "spec.md", "--skip-gate")
	wantContains(t, out, "  store,ship,haiku,,created", "  api,ship,sonnet,store,created")
	fm, _, err := readTask(filepath.Join(h.dir("store"), "task.md"))
	if err != nil || fm["spec"] != "spec.md#store" || !strings.HasPrefix(fm["base"], "orch/int-spec@") {
		t.Fatalf("store: spec = %q base = %q, err %v", fm["spec"], fm["base"], err)
	}
	// The spec has dependencies, so tasks start from the integration branch,
	// which plan cut from the spec's own base branch (feat).
	feat, _ := git(h.repo, "rev-parse", "--short=12", "feat")
	if !strings.HasSuffix(fm["base"], "@"+feat) {
		t.Fatalf("integration branch was not cut from feat (%s): base = %q", feat, fm["base"])
	}

	h.a.cwd = h.repo
	out = h.must("plan", filepath.Join(wt, "spec.md"), "--skip-gate")
	wantContains(t, out, "  store,ship,haiku,,exists", "  api,ship,sonnet,store,exists")
}

func TestPlanLinkedWorktreeUncommittedEdits(t *testing.T) {
	h := setup(t)
	wt := worktreeWithSpec(t, h)
	b, err := os.ReadFile(filepath.Join(wt, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, wt, map[string]string{"spec.md": string(b) + "more\n"})
	h.a.cwd = wt
	code, out := h.run("plan", "spec.md", "--skip-gate")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	wantContains(t, out, "spec has changes not in feat", "nothing was created")
}

func TestPlanSpecInUnrelatedRepo(t *testing.T) {
	h := setup(t)
	other := filepath.Join(h.tmp, "other")
	if out, err := execGit(h.tmp, "init", "-q", "-b", "main", other); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	writeFiles(t, other, map[string]string{"spec.md": threePkgSpec()})
	code, out := h.run("plan", filepath.Join(other, "spec.md"), "--skip-gate")
	if code == 0 || !strings.Contains(out, "is not inside the repo") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestGateChecksTheCheckoutHoldingTheSpec(t *testing.T) {
	h := setup(t)
	wt := worktreeWithSpec(t, h)
	if err := os.MkdirAll(filepath.Join(wt, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, wt, map[string]string{"lib/only.go": "package lib\n"})
	body := strings.Replace(string(mustRead(t, filepath.Join(wt, "spec.md"))), "Tests for api pass.", "Tests for api pass; see lib/only.go:1.", 1)
	writeFiles(t, wt, map[string]string{"spec.md": body})
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "branch-only code"}} {
		if out, err := execGit(wt, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	h.fakePi(gatePasses(nil))
	out := h.must("gate", filepath.Join(wt, "spec.md"))
	wantGateState(t, out, "pass")
	hash := specHash(mustRead(t, filepath.Join(wt, "spec.md")))
	report := string(mustRead(t, filepath.Join(runRoot(h.repo), "gate", hash, "report.md")))
	if strings.Contains(report, "does not resolve") {
		t.Fatalf("a pointer into the spec's own checkout was reported missing:\n%s", report)
	}
}

func TestGateRunsOneGroupPassForRelatedPackages(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	dir := h.fakePi(gatePasses(nil))
	h.must("gate", "spec.md")
	calls := string(mustRead(t, filepath.Join(dir, "calls.log")))
	if n := strings.Count(calls, "GATE-GROUP"); n != 1 {
		t.Errorf("GATE-GROUP ran %d times, want once for the 3 related packages", n)
	}
	if strings.Contains(calls, "GATE-SCOPE") || strings.Contains(calls, "GATE-QUESTIONS") || strings.Contains(calls, "GATE-PACKAGE") {
		t.Error("separate scope, questions or per-package passes still run")
	}
	if !strings.Contains(calls, "Packages in this pass: store, api, docs.") {
		t.Errorf("the group pass does not name its packages:\n%s", calls)
	}
	for _, want := range []string{`Cite a spec line as "spec.md:<line>"`, "set answered_by to that spec line as `spec.md:<line>`", "Gaps in a contract that two packages share", "(3) reuse:", `"reuse":[{"builds":""`, "two packages of this spec building the same thing"} {
		if !strings.Contains(calls, want) {
			t.Errorf("no prompt holds %q", want)
		}
	}
	if n := strings.Count(calls, "Gaps in a contract"); n != 1 {
		t.Errorf("the contract sentence is in %d prompts, want only adversary-b", n)
	}
	hash := specHash(mustRead(t, filepath.Join(h.repo, "spec.md")))
	var rec gateRecord
	if err := json.Unmarshal(mustRead(t, filepath.Join(runRoot(h.repo), "gate", hash, "gate.json")), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.PassSeconds["group:store+2"]; !ok {
		t.Errorf("pass_seconds lacks group:store+2: %v", rec.PassSeconds)
	}
	if g := rec.Groups["group:store+2"]; strings.Join(g, ",") != "store,api,docs" {
		t.Errorf("groups = %v", rec.Groups)
	}
	wantContains(t, string(mustRead(t, filepath.Join(runRoot(h.repo), "gate", hash, "report.md"))), "group:store+2: store, api, docs")
}

func TestGateGroupWithoutAnEntryForAPackageIsIncomplete(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	h.fakePi(gatePasses(map[string]string{"group": `{"packages":[{"id":"store","will_change":[],"claims":[],"questions":[],"reuse":[]},{"id":"api","will_change":[],"claims":[],"questions":[],"reuse":[]}]}`}))
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("a group that skipped a package passed: %d %s", code, out)
	}
	view := wantGateState(t, out, "incomplete")
	wantContains(t, view.PassErrors["group:store+2"], "no entry for docs")
}

func TestGateRecordsAndPrintsPassTokens(t *testing.T) {
	for _, mode := range []string{"pane", "headless"} {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			gdir := h.gateRepo(3)
			h.sessionPi(gatePasses(nil))
			if mode == "headless" {
				t.Setenv("HERDR_ENV", "")
			}
			out := h.must("gate", "spec.md")
			// 3 passes of 100 input, 300 cache read, 5 cache write, 10 output each.
			view := wantGateState(t, out, "pass")
			wantContains(t, view.Tokens, "input 300, cache read 900 (75% of input), cache write 15, output 30 over 3 pass(es), 3 requests, max context 400 in adversary-a")
			var rec gateRecord
			if err := json.Unmarshal(mustRead(t, filepath.Join(gdir, "gate.json")), &rec); err != nil {
				t.Fatal(err)
			}
			if view.Tokens != rec.tokenSummary() {
				t.Fatalf("public tokens differ from recorded summary: %q != %q", view.Tokens, rec.tokenSummary())
			}
			want := passTokens{Input: 100, Output: 10, CacheRead: 300, CacheWrite: 5, MaxContext: 400, Requests: 1, NoCost: 1}
			if mode == "pane" {
				want.Turns = 1 // the fake herdr session holds one assistant message with a toolCall
			}
			if len(rec.PassTokens) != 3 || rec.PassTokens["group:api+2"] != want {
				t.Errorf("pass_tokens = %v", rec.PassTokens)
			}
			wantContains(t, string(mustRead(t, filepath.Join(gdir, "report.md"))), "Tokens: input 300, cache read 900 (75% of input), cache write 15, output 30 over 3 pass(es), 3 requests, max context 400 in adversary-a")
			if exists(filepath.Join(gdir, "sessions", "group-api+2")) {
				t.Error("a succeeding pass kept its session")
			}
		})
	}
}

func TestSessionTokensSumsAssistantUsage(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.MkdirAll(filepath.Join(dir, "b"), 0o755)
	writeFiles(t, dir, map[string]string{
		"a/one.jsonl": `{"type":"session"}
{"type":"message","message":{"role":"user","usage":{"input":999}}}
{"type":"message","message":{"role":"assistant","usage":{"input":10,"output":2,"cacheRead":30,"cacheWrite":4,"totalTokens":46}}}
not json
{"type":"message","message":{"role":"assistant","usage":{"input":1,"output":1,"cacheRead":1,"cacheWrite":1}}}`,
		"a/two.jsonl": `{"type":"message","message":{"role":"assistant","usage":{"input":5,"output":6,"cacheRead":7,"cacheWrite":8}}}` + "\n",
		"a/note.txt":  `{"type":"message","message":{"role":"assistant","usage":{"input":1000}}}`,
	})
	got, ok := sessionTokens(dir)
	if want := (passTokens{Input: 16, Output: 9, CacheRead: 38, CacheWrite: 13, MaxContext: 40, Requests: 3, NoCost: 3}); !ok || got != want {
		t.Fatalf("sessionTokens = %+v %v, want %+v", got, ok, want)
	}
	if _, ok := sessionTokens(filepath.Join(dir, "missing")); ok {
		t.Error("a missing session dir reported usage")
	}
	writeFiles(t, dir, map[string]string{"b/zero.jsonl": `{"type":"message","message":{"role":"assistant","usage":{"input":0,"output":0}}}`})
	if _, ok := sessionTokens(filepath.Join(dir, "b")); ok {
		t.Error("an all-zero session reported usage")
	}
}

func TestApplyGroupFeedsEachPackageItsOwnEntry(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "internal"), 0o755)
	writeFiles(t, repo, map[string]string{"internal/store.go": "package internal\n\nfunc Put() {}\n"})
	sp := parseSpec(threePkgSpec())
	apply := applyGroup("group:api+1", []string{"api", "docs"})

	rec := &gateRecord{PassErrors: map[string]string{}}
	reply := `Here you go: {"packages":[
{"id":"api","will_change":["internal/store.go","internal/api.go"],"claims":[{"claim":"c","evidence":"internal/nope.go:1"}],
"questions":[{"package":"api","question":"Soft delete?","changes_result":true},{"package":"api","question":"  ","changes_result":false}],"reuse":[]},
{"id":"docs","will_change":[],"claims":[],"reuse":[],"questions":[{"package":"docs","question":"Where?","changes_result":false}]}]}`
	if err := apply(reply, rec, repo, sp); err != nil {
		t.Fatal(err)
	}
	if len(rec.Findings) != 1 || rec.Findings[0].Severity != "blocker" || rec.Findings[0].Where != "package api" || !strings.Contains(rec.Findings[0].Claim, "internal/store.go") {
		t.Errorf("scope half: findings = %+v", rec.Findings)
	}
	if len(rec.Questions) != 2 || rec.Questions[0].Question != "Soft delete?" || !rec.Questions[0].blocks() || rec.Questions[1].Package != "docs" {
		t.Errorf("questions half: %+v", rec.Questions)
	}
	if rec.Dropped != 1 || len(rec.PassErrors) != 0 {
		t.Errorf("dropped = %d, errors = %v, want 1 and none", rec.Dropped, rec.PassErrors)
	}

	// Incomplete valid JSON is rejected before applying any findings.
	rec = &gateRecord{PassErrors: map[string]string{}}
	if err := apply(`{"packages":[{"id":"api","will_change":["internal/store.go"]}]}`, rec, repo, sp); err == nil {
		t.Fatal("partial reply accepted")
	}
	if len(rec.Findings) != 0 || len(rec.Questions) != 0 {
		t.Errorf("partial reply was applied: %+v", rec)
	}
	rec = &gateRecord{PassErrors: map[string]string{}}
	if err := apply(`{}`, rec, repo, sp); err == nil {
		t.Error("empty object accepted")
	}
	if err := apply("no json here", &gateRecord{PassErrors: map[string]string{}}, repo, sp); err == nil {
		t.Error("an unparseable reply was not an error")
	}
}

func TestGatePackagePromptHoldsOnlyTheAskedPackage(t *testing.T) {
	src := strings.Replace(threePkgSpec(), "## Packages", "## Decisions made for the owner\nUse sqlite.\n\n## Packages", 1)
	sp := parseSpec(src)
	byID := map[string]*specPackage{}
	for _, p := range sp.packages {
		byID[p.id] = p
	}
	blocks := packageBlocks(sp, src)
	api := gatePrompt(gatePackageShared(sp, src, "spec.md"), gatePackageOwn(blocks["api"]), "GATE-PACKAGE", "task", "{}")
	for _, want := range []string{"Build api.", "Tests for api pass.", "## Design rules\n" + numberedLine(src, "One queue per panel."), "## Decisions made for the owner\n" + numberedLine(src, "Use sqlite."),
		"- store: Package store (owns: internal/store.go, migrations/)", "- api: Package api (owns: internal/api.go)", "- docs: Package docs (owns: )"} {
		if !strings.Contains(api, want) {
			t.Errorf("api context lacks %q", want)
		}
	}
	for _, not := range []string{"Build store.", "Build docs.", "## Packages"} {
		if strings.Contains(api, not) {
			t.Errorf("api context holds %q", not)
		}
	}
	if strings.Index(api, "## Behaviour") > strings.Index(api, "## Decisions made for the owner") || strings.Index(api, "## Design rules") > strings.Index(api, "## Authority") {
		t.Error("sections are not in spec order")
	}
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// numberedLine is how the prompt prints the first spec line equal to text.
func numberedLine(src, text string) string {
	for i, l := range specLines(src) {
		if l == text {
			return fmt.Sprintf("%d: %s", i+1, l)
		}
	}
	return "missing: " + text
}

func TestGatePromptsOfOneClassShareTheirContextAsPrefix(t *testing.T) {
	src := threePkgSpec()
	sp := parseSpec(src)
	blocks := packageBlocks(sp, src)
	task := func(id string) string { return "do it for " + id }

	// Package passes: header, shared sections and index come first.
	shared := gatePackageShared(sp, src, "spec.md")
	a := gatePrompt(shared, gatePackageOwn(blocks["store"]), "GATE-PACKAGE", task("store"), `{"x":1}`)
	b := gatePrompt(shared, gatePackageOwn(blocks["api"]), "GATE-PACKAGE", task("api"), `{"x":2}`)
	if commonPrefix(a, b) < len(gateHeader+shared) {
		t.Errorf("package prompts share %d bytes, want at least %d", commonPrefix(a, b), len(gateHeader+shared))
	}
	if i := strings.Index(a, "GATE-PACKAGE"); i < len(gateHeader+shared) {
		t.Errorf("the pass tag sits at %d, inside the shared text", i)
	}

	// Adversaries share header and the full, numbered spec.
	ctx := gateSpecContext(src, "spec.md")
	x := gatePrompt(ctx, "", "GATE-ADVERSARY-A", "attack", `{"f":1}`)
	y := gatePrompt(ctx, "", "GATE-ADVERSARY-B", "assume", `{"f":2}`)
	if commonPrefix(x, y) < len(gateHeader+ctx) || !strings.Contains(x, numberedLine(src, "One queue per panel.")) {
		t.Errorf("adversary prompts share %d bytes, want at least %d", commonPrefix(x, y), len(gateHeader+ctx))
	}
}

func TestGatePackageBlockCarriesTheSpecsRealLineNumbers(t *testing.T) {
	src := threePkgSpec()
	sp := parseSpec(src)
	blocks := packageBlocks(sp, src)
	lines := specLines(src)
	for _, p := range sp.packages {
		first, _, _ := strings.Cut(blocks[p.id], "\n")
		n, text, _ := strings.Cut(first, ": ")
		i, err := strconv.Atoi(n)
		if err != nil || lines[i-1] != text || !strings.HasPrefix(text, "### "+p.id+":") {
			t.Errorf("package %s block starts %q, want its real heading line", p.id, first)
		}
	}
	if !strings.Contains(blocks["api"], numberedLine(src, "Build api.")) || strings.Contains(blocks["api"], "Build store.") {
		t.Errorf("api block: %s", blocks["api"])
	}
	shared := gatePackageShared(sp, src, "spec.md")
	if strings.Contains(shared, "Build ") || !strings.Contains(shared, numberedLine(src, "## Open questions")) {
		t.Errorf("shared context should hold the numbered sections after the packages and no package body:\n%s", shared)
	}
}

func TestGateQuestionAnsweredBySpecLineCountsAndFakeLinesDoNot(t *testing.T) {
	repo := t.TempDir()
	src := threePkgSpec()
	writeFiles(t, repo, map[string]string{"spec.md": src})
	sp := parseSpec(src)
	var n int
	for i, l := range specLines(src) {
		if l == "One queue per panel." {
			n = i + 1
		}
	}
	rec := &gateRecord{}
	reply := fmt.Sprintf(`{"packages":[{"id":"api","will_change":[],"claims":[],"reuse":[],"questions":[{"package":"api","question":"One queue?","answered_by":"spec.md:%d","changes_result":true},{"package":"api","question":"Beyond?","answered_by":"spec.md:%d","changes_result":true}]}]}`, n, len(specLines(src))+50)
	if err := applyGroup("group:api", []string{"api"})(reply, rec, repo, sp); err != nil {
		t.Fatal(err)
	}
	if len(rec.Questions) != 2 || rec.Questions[0].blocks() || !rec.Questions[1].blocks() || rec.Dropped != 1 {
		t.Fatalf("questions %+v dropped %d", rec.Questions, rec.Dropped)
	}
}

func TestGateNotesAShipPackageThatOwnsTooMuch(t *testing.T) {
	// owns builds n file entries spread over d parent directories.
	owns := func(n, d int) string {
		var p []string
		for i := range n {
			p = append(p, fmt.Sprintf("web/d%d/f%d.go", i%d, i))
		}
		return "owns: " + strings.Join(p, ", ")
	}
	det := func(kind string, n, d int) []string {
		sp := parseSpec(planHeader + planPkg("big", "kind: "+kind+"\n"+owns(n, d), "") + planTail)
		return gateDeterministic(t.TempDir(), config{repoConfig: repoConfig{Check: "go test ./..."}}, sp, "")
	}
	if got := det("ship", 7, 7); len(got) != 1 || got[0] != "note: package big owns 7 paths across 7 directories; consider splitting it, a lead holds all of it in one context" || hasError(got) {
		t.Errorf("7 directories: %v", got)
	}
	if got := det("ship", 41, 1); len(got) != 1 || !strings.HasPrefix(got[0], "note: package big owns 41 paths across 1 directories") {
		t.Errorf("41 entries: %v", got)
	}
	for _, c := range []struct {
		kind string
		n, d int
	}{{"ship", 6, 6}, {"ship", 40, 1}, {"scout", 7, 7}, {"scout", 41, 1}} {
		if got := det(c.kind, c.n, c.d); len(got) != 0 {
			t.Errorf("%s with %d paths in %d directories: %v", c.kind, c.n, c.d, got)
		}
	}
	if ownDirs([]string{"a/b/", "a/b/f.go", "a/c.go", "top.go"}) != 3 {
		t.Error("a directory entry should count as its own parent, a root file as '.'")
	}
	if hasError([]string{"note: x", "no `check` command in the repo config"}) || !hasError([]string{"note: x", "pointer a.go:1 does not resolve to a line in the repo"}) {
		t.Error("hasError mishandles advisory lines")
	}
}

func TestGateOtherWorkersWaitForTheWarmUpDelay(t *testing.T) {
	h := setup(t)
	h.gateRepo(30) // 2 adversaries and 8 groups: 10 passes for 6 workers
	t.Setenv("HERDR_ENV", "")
	dir := h.fakePi(gatePasses(nil))
	bin := filepath.Join(dir, "pi")
	os.Rename(bin, bin+".real")
	os.WriteFile(bin, []byte(`#!/bin/sh
t=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
case "$*" in *GATE-ADVERSARY-A*) n=adv-a;; *GATE-ADVERSARY-B*) n=adv-b;; *) n=pkg;; esac
echo "$t $n" >> "`+dir+`/times.log"
sleep 1
exec "`+bin+`.real" "$@"
`), 0o755)
	gateWarmDelay = 600 * time.Millisecond
	h.must("gate", "spec.md")

	var at []float64
	var name []string
	for _, l := range strings.Split(strings.TrimSpace(string(mustRead(t, filepath.Join(dir, "times.log")))), "\n") {
		f := strings.Fields(l)
		v, _ := strconv.ParseFloat(f[0], 64)
		at, name = append(at, v), append(name, f[1])
	}
	if len(at) != 10 {
		t.Fatalf("%d passes ran, want 10", len(at))
	}
	if got := name[0] + "," + name[1]; got != "adv-a,pkg" && got != "pkg,adv-a" {
		t.Errorf("the first two passes were %s, want one adversary and one package pass", got)
	}
	for i := 2; i < 10; i++ {
		if at[i]-at[1] < 0.4 {
			t.Errorf("pass %d started %.2fs after the second, before the warm-up delay", i, at[i]-at[1])
		}
	}
}

func TestApplyGroupSalvagesRepliesThatAreNotValidJSON(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "internal"), 0o755)
	writeFiles(t, repo, map[string]string{"internal/store.go": "package internal\n\nfunc Put() {}\n"})
	sp := parseSpec(threePkgSpec())
	ids := []string{"api", "docs"}
	apply := func(raw string) (*gateRecord, error) {
		rec := &gateRecord{PassErrors: map[string]string{}}
		return rec, applyGroup("group:api+1", ids)(raw, rec, repo, sp)
	}

	// The shape seen in a real run: the scope object is never closed before
	// the next key, so the whole reply is invalid although every part is whole.
	rec, err := apply(`{"packages":[{"id":"api","scope":{"will_change":["internal/store.go"],"claims":[{"claim":"c","evidence":"internal/store.go:3"}],"questions":[{"question":"Soft delete?","answered_by":"","changes_result":true}]},{"id":"docs","will_change":[],"claims":[],"questions":[{"question":"Where?","changes_result":false}]}]}`)
	if err == nil {
		t.Fatal("salvaged reply must not establish completion")
	}
	if len(rec.Findings) != 1 || rec.Findings[0].Severity != "blocker" || len(rec.Questions) != 2 || rec.Questions[0].Package != "api" || !rec.Questions[0].blocks() || rec.Questions[1].Package != "docs" {
		t.Errorf("findings %+v questions %+v", rec.Findings, rec.Questions)
	}
	if len(rec.PassErrors) != 0 || len(rec.Salvaged) != 1 || rec.Salvaged[0] != "group:api+1" {
		t.Errorf("errors %v salvaged %v", rec.PassErrors, rec.Salvaged)
	}

	// A reply cut short keeps what was complete before the cut.
	rec, err = apply(`{"packages":[{"id":"api","will_change":["internal/store.go"],"claims":[{"claim":"c","evid`)
	if err == nil || len(rec.Findings) != 1 || len(rec.Salvaged) != 1 {
		t.Errorf("truncated reply: err %v findings %+v errors %v salvaged %v", err, rec.Findings, rec.PassErrors, rec.Salvaged)
	}

	// Nothing decodable is still an error, and valid JSON is not "salvaged".
	if _, err := apply(`{"packages":[{"id":"api","will_ch`); err == nil {
		t.Error("a reply with nothing decodable was not an error")
	}
	rec, err = apply(`{"packages":[{"id":"api"},{"id":"docs"}]}`)
	if err == nil || len(rec.Salvaged) != 0 {
		t.Errorf("incomplete valid JSON: err %v salvaged %v", err, rec.Salvaged)
	}

	// The real failing reply of a one-package group (no id in the old shape).
	one := parseSpec(planHeader + planPkg("crm1-orders", "kind: ship\nowns: internal/crm/orders/", "") + planTail)
	rec = &gateRecord{PassErrors: map[string]string{}}
	raw := mustRead(t, "testdata/gate-reply-unclosed-scope.txt")
	if err := applyGroup("group:crm1-orders", []string{"crm1-orders"})(string(raw), rec, repo, one); err == nil {
		t.Fatal("real salvaged reply must not establish completion")
	}
	if len(rec.Salvaged) != 1 || len(rec.Findings) < 10 || len(rec.Questions) < 1 || rec.Questions[0].Package != "crm1-orders" || rec.Dropped < 1 {
		t.Errorf("real reply: salvaged %v findings %d questions %d dropped %d", rec.Salvaged, len(rec.Findings), len(rec.Questions), rec.Dropped)
	}
}

func groupSpec(owns map[string]string, deps map[string]string, order ...string) *spec {
	src := planHeader
	for _, id := range order {
		keys := "kind: ship\nowns: " + owns[id]
		if d := deps[id]; d != "" {
			keys += "\ndepends: " + d
		}
		src += planPkg(id, keys, "")
	}
	return parseSpec(src + planTail)
}

func groupIDs(gs [][]*specPackage) string {
	var out []string
	for _, g := range gs {
		var ids []string
		for _, p := range g {
			ids = append(ids, p.id)
		}
		out = append(out, strings.Join(ids, "+"))
	}
	return strings.Join(out, " | ")
}

func TestGateGroupsKeepRelatedPackagesTogetherUnderTheCaps(t *testing.T) {
	sp := groupSpec(
		map[string]string{"x1": "web/a/", "y1": "api/", "x2": "web/b/", "z1": "docs/", "y2": "lib/"},
		map[string]string{"y2": "y1"},
		"x1", "y1", "x2", "z1", "y2")
	big := 1 << 20
	if got := groupIDs(gateGroups(sp, 5, big, 6)); got != "x1+x2 | y1+y2 | z1" { // related packages merge, the unrelated z1 stays alone
		t.Errorf("related packages only: %s", got)
	}
	// Related packages (same top directory, depends edge) stay together; the count cap splits between clusters.
	if got := groupIDs(gateGroups(sp, 2, big, 6)); got != "x1+x2 | y1+y2 | z1" {
		t.Errorf("max 2: %s", got)
	}
	// More groups than workers: the smallest adjacent pair that fits is merged.
	if got := groupIDs(gateGroups(sp, 3, big, 2)); got != "x1+x2+z1 | y1+y2" {
		t.Errorf("max 3, 2 workers: %s", got)
	}
	// No merge can stay under the caps: the groups stay as they are.
	if got := groupIDs(gateGroups(sp, 2, big, 1)); got != "x1+x2 | y1+y2 | z1" {
		t.Errorf("unmergeable: %s", got)
	}
	// A package over the text cap forms its own group.
	if got := groupIDs(gateGroups(sp, 5, 1, 6)); got != "x1 | y1 | x2 | z1 | y2" {
		t.Errorf("text cap: %s", got)
	}
	if got := groupKey(gateGroups(sp, 5, big, 6)[0]); got != "group:x1+1" {
		t.Errorf("group key %q", got)
	}
}

func TestApplyGroupTurnsReuseIntoConcerns(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "internal"), 0o755)
	writeFiles(t, repo, map[string]string{"internal/store.go": "package internal\n\nfunc Put() {}\n"})
	sp := parseSpec(threePkgSpec())
	rec := &gateRecord{PassErrors: map[string]string{}}
	reply := `{"packages":[{"id":"api","will_change":[],"claims":[],"questions":[],"reuse":[
{"builds":"internal/put.go","existing":"Put in store.go","evidence":"internal/store.go:3","why":"same write"},
{"builds":"a queue table","existing":"imaginary","evidence":"internal/nope.go:9"},
{"builds":"  ","existing":"ignored","evidence":"internal/store.go:1"}]}]}`
	if err := applyGroup("group:api", []string{"api"})(reply, rec, repo, sp); err != nil {
		t.Fatal(err)
	}
	if len(rec.Findings) != 2 || rec.Dropped != 1 {
		t.Fatalf("findings %+v dropped %d", rec.Findings, rec.Dropped)
	}
	f := rec.Findings[0]
	if f.Severity != "concern" || f.Source != "reuse" || f.Where != "package api" || f.Claim != "internal/put.go may duplicate Put in store.go" || f.Evidence != "internal/store.go:3" || !f.Verified || f.Why != "same write" {
		t.Errorf("finding = %+v", f)
	}
	if g := rec.Findings[1]; g.Verified || g.Severity != "concern" {
		t.Errorf("a reuse claim without real evidence must be unverified and never a blocker: %+v", g)
	}

	// The salvage decoder reads reuse as one more key.
	rec = &gateRecord{PassErrors: map[string]string{}}
	if err := applyGroup("group:api", []string{"api"})(`{"packages":[{"id":"api","scope":{"reuse":[{"builds":"b","existing":"e","evidence":"internal/store.go:3"}]},`, rec, repo, sp); err == nil {
		t.Fatal("salvaged reuse must not establish completion")
	}
	if len(rec.Findings) != 1 || len(rec.Salvaged) != 1 {
		t.Errorf("salvaged reuse: findings %+v salvaged %v", rec.Findings, rec.Salvaged)
	}
}

func TestGateDryRunListsThePassesAndCallsNothing(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	dir := h.fakePi(gatePasses(nil))
	out := h.must("gate", "spec.md", "--dry-run")
	wantContains(t, out, "gate dry run: spec.md", "no model is called and nothing is written", "deterministic: 1 issue(s)",
		"pass: adversary-a\n  packages: the whole spec\n", "pass: adversary-b\n", "pass: group:store+2\n  packages: store, api, docs\n",
		"shared prefix: ", "chars (~", "starts: ", "totals: 3 passes, 3 workers, ~")
	if exists(filepath.Join(dir, "calls.log")) {
		t.Error("pi was called")
	}
	if st := h.fakeState(); len(st.Calls) != 0 || exists(h.fake+".gate.log") {
		t.Errorf("herdr was called: %v", st.Calls)
	}
	if exists(filepath.Join(runRoot(h.repo), "gate")) {
		t.Error("a dry run wrote a gate dir")
	}

	// Format problems still fail, as for a real run.
	h.commitSpec(strings.Replace(threePkgSpec(), "## Behaviour", "## Behavior", 1))
	if code, out := h.run("gate", "spec.md", "--dry-run"); code != 1 || !strings.Contains(out, "format problem") {
		t.Errorf("dry run of a bad spec = %d %s", code, out)
	}
}

func TestGateGroupsKeepAFamilyOfUIPackagesTogetherAmongMixedBackends(t *testing.T) {
	owns := map[string]string{
		"crm1-orders":       "internal/crm/orders/, internal/crm/api/admin/orders.go, web/apps/crm/src/routes/orders.tsx",
		"crm1-customers":    "internal/crm/customers/, internal/crm/api/admin/customers.go",
		"crm1-promotions":   "internal/crm/promotions/, web/apps/crm/src/routes/promotions.tsx",
		"crm1-polar":        "internal/crm/polar/, internal/crm/api/webhooks/polar.go",
		"crm1-reports":      "internal/crm/reports/, web/apps/crm/src/routes/reports.tsx",
		"crm1-ui-shell":     "web/apps/crm/src/shell/, web/packages/ui/src/",
		"crm1-ui-commerce":  "web/apps/crm/src/routes/commerce/, web/apps/crm/src/lib/cart.ts",
		"crm1-ui-money":     "web/apps/crm/src/routes/money/, web/apps/crm/src/lib/format.ts",
		"crm1-ui-people":    "web/apps/crm/src/routes/people/",
		"crm1-ui-account":   "web/apps/crm/src/routes/account/",
		"crm1-ui-support":   "web/apps/crm/src/routes/support/",
		"crm1-inbound-mail": "internal/crm/mail/, web/apps/crm/src/routes/mail.tsx",
		"crm1-renewals":     "internal/crm/renewals/",
		"crm1-privacy":      "internal/crm/privacy/, web/apps/crm/src/routes/privacy.tsx",
	}
	deps := map[string]string{"crm1-ui-commerce": "crm1-orders", "crm1-ui-money": "crm1-polar", "crm1-renewals": "crm1-orders"}
	// Backends and UI packages interleaved, as a real spec lists them.
	order := []string{"crm1-orders", "crm1-ui-shell", "crm1-customers", "crm1-promotions", "crm1-ui-commerce", "crm1-renewals", "crm1-polar",
		"crm1-ui-money", "crm1-reports", "crm1-ui-people", "crm1-inbound-mail", "crm1-ui-account", "crm1-privacy", "crm1-ui-support"}
	sp := groupSpec(owns, deps, order...)
	groups := gateGroups(sp, 4, 24000, 6)
	uiGroups := map[int]bool{}
	seen := map[string]bool{}
	for i, g := range groups {
		if len(g) > 4 {
			t.Errorf("group %d has %d packages", i, len(g))
		}
		for _, p := range g {
			if _, ok := owns[p.id]; !ok || seen[p.id] {
				t.Errorf("unexpected or duplicate package %q in %s", p.id, groupIDs(groups))
			}
			seen[p.id] = true
			if strings.HasPrefix(p.id, "crm1-ui-") {
				uiGroups[i] = true
			}
		}
	}
	for _, id := range order {
		if !seen[id] {
			t.Errorf("package %s is missing from %s", id, groupIDs(groups))
		}
	}
	if len(uiGroups) > 2 {
		t.Errorf("the six ui packages are spread over %d groups: %s", len(uiGroups), groupIDs(groups))
	}
	if len(groups) > 6 {
		t.Errorf("%d groups for 6 workers: %s", len(groups), groupIDs(groups))
	}
	if pkgAffinity(sp.packages[1], sp.packages[4]) <= pkgAffinity(sp.packages[1], sp.packages[2]) {
		t.Error("two ui packages should be closer than a ui package and a backend one")
	}
}

func TestSessionTokensSumsCost(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"c.jsonl": `{"message":{"role":"assistant","usage":{"input":10,"output":2,"cost":{"input":0.01,"output":0.02,"cacheRead":0.003,"cacheWrite":0.004,"total":0.037}}}}
{"message":{"role":"assistant","usage":{"input":5,"output":1,"cost":{"input":0.01,"output":0.01,"cacheRead":0,"cacheWrite":0,"total":0.02}}}}
{"message":{"role":"assistant","usage":{"input":1,"output":1}}}
`})
	got, ok := sessionTokens(dir)
	if !ok || got.NoCost != 1 || got.Cost < 0.0569 || got.Cost > 0.0571 || got.CostParts.Input < 0.0199 || got.CostParts.Input > 0.0201 || got.CostParts.CacheWrite != 0.004 {
		t.Fatalf("sessionTokens = %+v %v", got, ok)
	}
	rec := &gateRecord{PassTokens: map[string]passTokens{"p": got, "q": {Input: 1, Cost: 0.5}}}
	wantContains(t, rec.tokenSummary(), "cost $0.56 (incomplete: 1 request(s) without cost)")
	rec = &gateRecord{PassTokens: map[string]passTokens{"q": {Input: 1, Cost: 0.5}}}
	wantContains(t, rec.tokenSummary(), "cost $0.50")
}
