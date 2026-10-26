package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// briefTestTotal is the byte budget the brief tests render with.
const briefTestTotal = 24 * 1024

// rankRepo is a committed repo of the given files, with rg required and no
// language servers.
func rankRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg is not installed")
	}
	t.Setenv("ORCH_CACHE_DIR", t.TempDir())
	t.Setenv("ORCH_GOPLS", filepath.Join(t.TempDir(), "no-gopls"))
	t.Setenv("ORCH_TSLS", filepath.Join(t.TempDir(), "no-tsls"))
	code := t.TempDir()
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(code, name)), 0o755)
		os.WriteFile(filepath.Join(code, name), []byte(body), 0o644)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@e.com", "commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = code
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return code
}

func TestBriefRankingKeepsDocsBelowCodeAndCountsTextOnlyWhenRare(t *testing.T) {
	files := map[string]string{
		"svc/own.go":       "package svc\n",
		"svc/other.go":     "package svc\n\nvar rare_flag = 1\n",
		"docs/rare.md":     "the rare_flag is described here\n",
		"docs/features.md": "rare_flag and more\n",
	}
	for i := range 25 { // a name with 25 hits is noise
		files[fmt.Sprintf("svc/common%02d.go", i)] = "package svc\n\nvar common_name = 1\n"
	}
	code := rankRepo(t, files)
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "no-jbcontext"))
	e := newCtxEnv(code, ctxOpts{})
	b := e.Briefs([]briefInput{{ID: "p", Title: "Svc Thing", Text: "Build it. Uses `rare_flag` and `common_name`.", Owns: []string{"svc/own.go"}}})[0]

	if len(b.Files) < 2 || !strings.HasPrefix(b.Files[0], "- `svc/own.go` (score 2: own)") {
		t.Fatalf("owned code must come first with its kinds:\n%s", strings.Join(b.Files, "\n"))
	}
	all := strings.Join(b.Files, "\n")
	if strings.Contains(all, "docs/") {
		t.Errorf("a doc is listed among the ranked files:\n%s", all)
	}
	if !strings.Contains(all, "- `svc/other.go` (score 1: text)") {
		t.Errorf("a rare name's text hit counts 1:\n%s", all)
	}
	if strings.Contains(all, "common") {
		t.Errorf("a name with more than 20 hits must count 0:\n%s", all)
	}
	docs := strings.Join(b.Docs, "\n")
	if !strings.Contains(docs, "docs/rare.md") || !strings.Contains(docs, "(score 1: text)") || strings.Contains(docs, "common") {
		t.Errorf("docs section:\n%s", docs)
	}
	if out := b.Render("", "package", briefTestTotal); !strings.Contains(out, "### 1b. Docs that mention it\n") {
		t.Errorf("the docs section is not rendered:\n%s", out)
	}
}

func TestFileEvidenceScoresEachKindOnceWithAgreementBonus(t *testing.T) {
	for _, c := range []struct {
		kinds []string
		want  int
		list  string
	}{
		{[]string{"own"}, 2, "own"},
		{[]string{"text"}, 1, "text"},
		{[]string{"own", "sym"}, 6, "own+sym"},
		{[]string{"own", "jb", "sym"}, 8, "own+jb+sym"},
		{[]string{"jb", "text"}, 5, "jb+text"},
	} {
		ev := &fileEv{kinds: map[string]bool{}}
		for _, k := range c.kinds {
			ev.kinds[k] = true
		}
		if ev.score() != c.want || ev.kindList() != c.list {
			t.Errorf("%v: score %d (%s), want %d (%s)", c.kinds, ev.score(), ev.kindList(), c.want, c.list)
		}
	}
}

func TestBriefLookAlikesAreConceptChunksOfCodeOutsideTheOwns(t *testing.T) {
	// Each file holds the chunk its hit claims: hits that no longer read as
	// the file does are dropped before the look-alike rules run.
	chunk := "package svc\n\nfunc SvcThing() {}\nbody\n"
	files := map[string]string{
		"svc/own.go":       chunk,
		"svc/other.go":     chunk,
		"svc/x_test.go":    chunk,
		"api/openapi.json": chunk,
		"deploy/run.sh":    chunk,
		"svc/gen/x.pb.go":  chunk,
	}
	code := rankRepo(t, files)
	var results []string
	for _, p := range []string{"svc/own.go", "svc/other.go", "svc/x_test.go", "api/openapi.json", "deploy/run.sh", "svc/gen/x.pb.go"} {
		results = append(results, fmt.Sprintf(`{"result":{"sourcePosition":{"relativePath":"%s"}},"content":"\nfunc SvcThing() {}\nbody","contentStartLine":2}`, p))
	}
	for i := range 9 { // more look-alikes than the cap
		results = append(results, fmt.Sprintf(`{"result":{"sourcePosition":{"relativePath":"svc/more%d.go"}},"content":"\nfunc SvcThingMore%d() {}","contentStartLine":2}`, i, i))
	}
	for i := range 9 {
		os.WriteFile(filepath.Join(code, "svc", fmt.Sprintf("more%d.go", i)), fmt.Appendf(nil, "package svc\n\nfunc SvcThingMore%d() {}\n", i), 0o644)
	}
	dir := t.TempDir()
	jb := filepath.Join(dir, "jbcontext")
	os.WriteFile(jb, []byte("#!/bin/sh\ncat <<'EOF'\n{\"type\":\"search_result\",\"results\":["+strings.Join(results, ",")+"]}\nEOF\n"), 0o755)
	t.Setenv("ORCH_JBCONTEXT", jb)
	e := newCtxEnv(code, ctxOpts{})
	b := e.Briefs([]briefInput{{ID: "p", Title: "Svc Thing", Text: "Build the svc thing.\n\n#### Acceptance\n- the svc thing works\n", Owns: []string{"svc/own.go"}}})[0]

	like := strings.Join(b.Like, "\n")
	if !strings.Contains(like, "- `svc/other.go:3-4` in func SvcThing via concept \"") {
		t.Fatalf("a concept chunk of code outside the owns is missing (notes %v):\n%s", e.Notes(), like)
	}
	for _, bad := range []string{"svc/own.go", "x_test.go", "openapi.json", "run.sh", "x.pb.go"} {
		if strings.Contains(like, bad) {
			t.Errorf("%s must not be a look-alike:\n%s", bad, like)
		}
	}
	if !strings.Contains(like, "6 more cut") || len(b.LikeRows) != briefLikeTree {
		t.Errorf("the cap of %d look-alikes per tree is not applied and said (%d rows):\n%s", briefLikeTree, len(b.LikeRows), like)
	}
	if !strings.Contains(like, "func SvcThing() {}") {
		t.Errorf("no chunk lines shown:\n%s", like)
	}
}

func TestMissingNamesKeepIdentifiersRoutesAndPathsOnly(t *testing.T) {
	for n, want := range map[string]bool{
		"transfer_rows":       true,
		"Module.Webhooks":     true,
		"internal/gone.go":    true,
		"POST /api/transfers": true,
		"mod_*.go":            false,
		"charge.dispute.*":    false,
		"registry_test":       false,
		"a b":                 false,
	} {
		if got := missingWorthy(n); got != want {
			t.Errorf("missingWorthy(%q) = %v, want %v", n, got, want)
		}
	}
}

func TestBriefSheetKeepsOnlySymbolsThePackageDefinesExtendsOrCalls(t *testing.T) {
	pl := &briefPlan{in: briefInput{Owns: []string{"pay"}}, accept: []string{"Wired lists Named"}, ptrs: []pointer{{sym: "Pointed"}}}
	def := func(where string, callers ...string) *symFact {
		return &symFact{Defs: []symDef{{Where: where, Callers: callers}}}
	}
	for name, c := range map[string]struct {
		f    *symFact
		want bool
	}{
		"Defined":   {def("pay/a.go:3"), true},
		"Called":    {def("store/q.go:3", "pay/b.go:9"), true},
		"Extended":  {&symFact{Defs: []symDef{{Where: "store/q.go:3", Impls: []string{"pay/stripe.go:7"}}}}, true},
		"Elsewhere": {def("config/c.go:3", "config/d.go:4"), false},
		"Named":     {def("config/c.go:3"), true},
		"Pointed":   {def("config/c.go:3"), true},
	} {
		if got := pl.symKept(name, c.f); got != c.want {
			t.Errorf("symKept(%s) = %v, want %v", name, got, c.want)
		}
	}
}

func TestAcceptanceBulletsOwnsQueriesAndCodeTrees(t *testing.T) {
	text := "### p: P\n\nBuild.\n\n#### Acceptance\n- first thing works\n* `second` too\n1. third\n\n#### Other\n- not this\n"
	if got := strings.Join(acceptanceBullets(text), "|"); got != "first thing works|`second` too|third" {
		t.Errorf("acceptance bullets = %q", got)
	}
	code := rankRepo(t, map[string]string{"internal/a.go": "package a\n", "internal/b.go": "package a\n", "web/c.ts": "x\n", "docs/d.go": "x\n", "api/o.json": "{}\n"})
	got := codeTreesFor(code, nil)
	if strings.Join(got, ",") != "internal/,web/" {
		t.Errorf("code trees = %v", got)
	}
}

// fakeJbResults is a jbcontext that answers every search with these chunks:
// path, first line, content.
func fakeJbResults(t *testing.T, chunks [][3]string) {
	t.Helper()
	var results []string
	for _, c := range chunks {
		results = append(results, fmt.Sprintf(`{"result":{"sourcePosition":{"relativePath":"%s"}},"content":"%s","contentStartLine":%s}`, c[0], c[2], c[1]))
	}
	jb := filepath.Join(t.TempDir(), "jbcontext")
	os.WriteFile(jb, []byte("#!/bin/sh\ncat <<'EOF'\n{\"type\":\"search_result\",\"results\":["+strings.Join(results, ",")+"]}\nEOF\n"), 0o755)
	t.Setenv("ORCH_JBCONTEXT", jb)
}

func likeBrief(code string) *ctxBrief {
	e := newCtxEnv(code, ctxOpts{})
	return e.Briefs([]briefInput{{ID: "p", Title: "Svc Thing", Text: "Build the svc thing.", Owns: []string{"svc/own.go"}}})[0]
}

func TestLookAlikesMergeEveryChunkOfAFileIntoOneEntry(t *testing.T) {
	code := rankRepo(t, map[string]string{
		"svc/own.go":   "package svc\n",
		"svc/other.go": "package svc\n\nfunc SvcThing() {}\n// body\n\nfunc SvcSecond() {}\n// body\n\nfunc SvcThird() {}\n",
	})
	fakeJbResults(t, [][3]string{
		{"svc/other.go", "2", `\nfunc SvcThing() {}\n// body`},
		{"svc/other.go", "5", `\nfunc SvcSecond() {}\n// body`},
		{"svc/other.go", "8", `\nfunc SvcThird() {}`},
	})
	b := likeBrief(code)
	like := strings.Join(b.Like, "\n")
	if strings.Count(like, "svc/other.go") != 1 || len(b.LikeRows) != 1 {
		t.Fatalf("a file must be one entry:\n%s", like)
	}
	wantContains(t, like, "(lines 3-4, 6-7, 9)", "in func SvcThing, func SvcSecond, func SvcThird", "func SvcThing() {}")
}

func TestLookAlikesHaveAQuotaPerTreeAndTheOwnsTreeComesFirst(t *testing.T) {
	files := map[string]string{"svc/own.go": "package svc\n", "web/a.ts": "x\n", "svc/z.go": "package svc\n"}
	var chunks [][3]string
	for i := range 6 { // the web results come first and are more numerous
		files[fmt.Sprintf("web/p%d.ts", i)] = fmt.Sprintf("// svc helpers\n\nexport function svcThing%d() {}\n", i)
		chunks = append(chunks, [3]string{fmt.Sprintf("web/p%d.ts", i), "2", fmt.Sprintf(`\nexport function svcThing%d() {}`, i)})
	}
	for i := range 6 {
		files[fmt.Sprintf("svc/q%d.go", i)] = fmt.Sprintf("package svc\n\nfunc SvcThing%d() {}\n", i)
		chunks = append(chunks, [3]string{fmt.Sprintf("svc/q%d.go", i), "2", fmt.Sprintf(`\nfunc SvcThing%d() {}`, i)})
	}
	code := rankRepo(t, files)
	fakeJbResults(t, chunks)
	b := likeBrief(code)
	var svc, web int
	firstWeb, lastSvc := -1, -1
	for i, r := range b.LikeRows {
		if strings.HasPrefix(r.At, "svc/") {
			svc++
			lastSvc = i
		} else if strings.HasPrefix(r.At, "web/") {
			web++
			if firstWeb < 0 {
				firstWeb = i
			}
		}
	}
	if svc != briefLikeTree || web != briefLikeTree || lastSvc > firstWeb {
		t.Errorf("want %d per tree with svc/ first, got svc %d web %d:\n%s", briefLikeTree, svc, web, strings.Join(b.Like, "\n"))
	}
	if !strings.Contains(strings.Join(b.Like, "\n"), "4 more cut") {
		t.Errorf("the cut is not said:\n%s", strings.Join(b.Like, "\n"))
	}
}

func TestLookAlikesNeedAWordInCommonWithThePackageAndEmptyTreesAreSaid(t *testing.T) {
	code := rankRepo(t, map[string]string{
		"svc/own.go":  "package svc\n",
		"svc/near.go": "package svc\n\nfunc NewSvcClient() {}\n",
		"svc/far.go":  "package svc\n\nfunc BananaSplit() {}\n",
		"web/a.ts":    "// fruit\n\nexport const banana = 1\n",
	})
	fakeJbResults(t, [][3]string{
		{"svc/near.go", "2", `\nfunc NewSvcClient() {}`},
		{"svc/far.go", "2", `\nfunc BananaSplit() {}`},
		{"web/a.ts", "2", `\nexport const banana = 1`},
	})
	b := likeBrief(code)
	like := strings.Join(b.Like, "\n")
	wantContains(t, like, "svc/near.go", "(no similar code found in web/)")
	if strings.Contains(like, "far.go") || strings.Contains(like, "web/a.ts:") {
		t.Errorf("a chunk sharing no word with the package survived:\n%s", like)
	}
	// camelCase is split: SvcClient shares "svc".
	if got := wordsOf("NewSvcClient HTTPServer", nil); !got["svc"] || !got["client"] || !got["http"] || !got["server"] {
		t.Errorf("wordsOf = %v", got)
	}
}

func conceptFixture() *briefPlan {
	return &briefPlan{in: briefInput{Title: "Gateway v1, Stripe App, bank debits and PayPal through Stripe", Owns: []string{"pay/registry.go", "pay/fake/", "pay/stripe/"}},
		goSyms: []string{"Gateway", "Registry"}, facts: map[string]*symFact{},
		accept: []string{
			"Refund of a partial amount returns the remaining balance to the customer and records an event in the ledger. Extra detail goes here.",
			"Setup session for a new method is hosted and returns a url to the customer who then saves the method",
			"cd skills/orchestrate/tool && go test ./...",
			"`pay/stripe/gateway_test.go`",
			"go vet passes",
		}}
}

func TestConceptQueriesAreFullSentencesNotKeywordFragments(t *testing.T) {
	pl := conceptFixture()
	pl.makeQueries()
	t.Logf("concept queries: %q", pl.concepts)
	if len(pl.concepts) < 3 {
		t.Fatalf("queries = %q", pl.concepts)
	}
	for _, q := range pl.concepts {
		if n := len(strings.Fields(q)); n < 4 || n > 30 {
			t.Errorf("query %q has %d words, want 4 to 30", q, n)
		}
	}
}

func TestAcceptanceBulletIsSentWholeToItsFirstSentence(t *testing.T) {
	pl := conceptFixture()
	pl.makeQueries()
	if !contains(pl.concepts, "Refund of a partial amount returns the remaining balance to the customer and records an event in the ledger.") {
		t.Errorf("the first sentence of a behaviour acceptance bullet is not sent whole: %q", pl.concepts)
	}
	if strings.Contains(strings.Join(pl.concepts, "|"), "Extra detail") {
		t.Errorf("text past the first sentence was sent: %q", pl.concepts)
	}
	for _, q := range pl.concepts {
		if strings.Contains(q, "go test") || strings.Contains(q, "gateway_test.go") || q == "go vet passes" {
			t.Errorf("a command line or file name became a query: %q", q)
		}
	}
}

func TestOwnedDirectoriesAloneProduceNoConceptQuery(t *testing.T) {
	pl := &briefPlan{in: briefInput{Owns: []string{"pay/fake/", "pay/stripe/", "pay/registry.go"}}, facts: map[string]*symFact{}}
	pl.makeQueries()
	if len(pl.concepts) != 0 {
		t.Errorf("owns alone gave queries: %q", pl.concepts)
	}
}

func TestConceptQueriesFallBackToTheTitleWhenThereAreNoAcceptanceBullets(t *testing.T) {
	pl := &briefPlan{in: briefInput{Title: "Let staff create CRM API keys with selected permissions"}, facts: map[string]*symFact{}}
	pl.makeQueries()
	if len(pl.concepts) != 1 || pl.concepts[0] != "Let staff create CRM API keys with selected permissions" {
		t.Errorf("title fallback = %q", pl.concepts)
	}
}

func contains(l []string, s string) bool { return containsStr(l, s) }
