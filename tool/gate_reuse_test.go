package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func specPkgFrom(t *testing.T, src, id string) *specPackage {
	t.Helper()
	sp := parseSpec(src)
	sp.validate()
	for _, p := range sp.packages {
		if p.id == id {
			return p
		}
	}
	t.Fatalf("no package %s", id)
	return nil
}

func TestBuildsBulletsBecomeItemsVerbatim(t *testing.T) {
	src := planHeader + "### api: Queue API\nkind: ship\nowns: internal/queue\n\nBody text.\n\n#### Builds\n- Code that queues a transfer on the panel, when a user submits one, producing a row.\n- Code that retries a failed send\n  with backoff, when the provider errors.\n  search: Code that retries a failed send\n  reuse: internal/store.go#Retry, internal/store.go:3\n* Code that lists queued transfers\n  reuse: internal/store.go\n\n#### Acceptance\n- Tests pass.\n" + planTail
	p := specPkgFrom(t, src, "api")
	items := packageItems(p)
	var got []string
	for _, it := range items {
		got = append(got, it.ID+" "+it.Text+" ["+strings.Join(it.Reuse, ", ")+"]")
	}
	want := []string{
		"api#1 Code that queues a transfer on the panel, when a user submits one, producing a row. []",
		"api#2 Code that retries a failed send with backoff, when the provider errors. [internal/store.go#Retry, internal/store.go:3]",
		"api#3 Code that lists queued transfers [internal/store.go]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(p.body, "Code that") || !strings.Contains(p.acceptance, "Tests pass") {
		t.Errorf("body %q acceptance %q", p.body, p.acceptance)
	}
	dir := t.TempDir()
	if err := writeItems(dir, map[string][]gateItem{"api": items}, []string{"api"}); err != nil {
		t.Fatal(err)
	}
	var back []gateItem
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, "items.json")), &back); err != nil || len(back) != 3 || back[2].ID != "api#3" || strings.Join(back[1].Reuse, ", ") != "internal/store.go#Retry, internal/store.go:3" || len(back[1].Queries) != 1 {
		t.Errorf("items.json: %v %v", back, err)
	}
	// the task a lead gets shows the Builds list under Spec
	task := specText(p)
	wantContains(t, task, "Body text.", "Builds:", "- api#1: Code that queues a transfer", "  reuse: internal/store.go#Retry, internal/store.go:3", "  search: Code that retries a failed send")
}

func TestBuildsParserHandlesEmptyLongAndNumberedBullets(t *testing.T) {
	if its, pr := parseBuilds(""); len(its) != 0 || len(pr) != 0 {
		t.Errorf("empty: %v %v", its, pr)
	}
	its, pr := parseBuilds("not a bullet\n1. Code that a\n2) Code that b\n")
	if len(its) != 2 || its[1].Text != "Code that b" || len(pr) != 0 {
		t.Errorf("numbered: %v %v", its, pr)
	}
	its, pr = parseBuilds("- " + strings.Repeat("x", 301))
	if len(pr) != 0 || len(its[0].Text) != 301 {
		t.Errorf("long bullet must remain verbatim: %v %v", its, pr)
	}
}

func TestBuildsWarningsAndReusePointers(t *testing.T) {
	mk := func(builds string) string {
		return planHeader + "### api: Queue API\nkind: ship\nowns: internal/queue\n\nBody.\n\n" + builds + "#### Acceptance\nTests pass.\n\n### look: Look around\nkind: scout\n\nBody.\n\n#### Acceptance\nA note.\n" + planTail
	}
	sp := parseSpec(mk(""))
	sp.validate()
	if len(sp.problems) != 0 {
		t.Errorf("missing Builds (a scout is exempt): %v", sp.problems)
	}
	sp = parseSpec(mk("#### Builds\n\n"))
	sp.validate()
	if len(sp.problems) != 0 {
		t.Errorf("empty Builds must not block: %v", sp.problems)
	}
	var chk config
	chk.Check = "x"
	det := gateDeterministic(t.TempDir(), chk, sp, "")
	if len(det) != 1 || hasError(det) || !strings.Contains(det[0], "note: package api has no Builds bullets; reuse checklist is skipped for this package") {
		t.Errorf("missing Builds must warn for api only, not the scout: %v", det)
	}
	var many strings.Builder
	many.WriteString("#### Builds\n")
	for i := range 9 {
		fmt.Fprintf(&many, "- Code that does thing %d\n", i)
	}
	sp = parseSpec(mk(many.String() + "\n"))
	sp.validate()
	if len(sp.problems) != 0 {
		t.Fatalf("9 bullets must still parse: %v", sp.problems)
	}
	var warn string
	for _, d := range gateDeterministic(t.TempDir(), chk, sp, "") {
		if strings.Contains(d, "9 Builds bullets") {
			warn = d
		}
	}
	if !strings.HasPrefix(warn, "note: ") || !strings.Contains(warn, "split the package") || hasError([]string{warn}) {
		t.Errorf("warning %q", warn)
	}
	// a reuse pointer must exist
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "have.go"), []byte("package x\n"), 0o644)
	sp = parseSpec(mk("#### Builds\n- Does a\n  reuse: have.go\n- Does b\n  reuse: nope.go\n- Does c\n  reuse: have.go:9\n\n"))
	sp.validate()
	det = gateDeterministic(repo, chk, sp, "")
	joined := strings.Join(det, "\n")
	if strings.Contains(joined, "bullet 1 ") || !strings.Contains(joined, "bullet 2: pointer nope.go") || !strings.Contains(joined, "bullet 3: pointer have.go:9 does not resolve") {
		t.Errorf("reuse tails:\n%s", joined)
	}
}

func verdictRepo(t *testing.T, h *harness) string {
	t.Helper()
	os.MkdirAll(filepath.Join(h.repo, "internal"), 0o755)
	writeFiles(t, h.repo, map[string]string{"internal/store.go": "package internal\n\nfunc Put() {}\n"})
	h.commitSpec(planHeader + "### api: API\nkind: ship\nowns: internal/api.go\n\nBuild the API.\n#### Builds\n- Build api.\n- Add the POST /api/put endpoint\n#### Acceptance\nTests pass.\n" + planTail)
	return filepath.Join(filepath.Dir(h.dir("x")), "gate", specHash(mustRead(t, filepath.Join(h.repo, "spec.md"))))
}

func TestPostDecisionsWithContextSendsTypedQuestionsAndReadsUsage(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "no", 401)
			return
		}
		w.Write([]byte(`{"model":"served","answers":{"relation":{"type":"choice","choice":"none","confidence":0.9}},"usage":{"cost":0.01,"total_tokens":5}}`))
	}))
	defer srv.Close()
	t.Setenv("ORCH_TRIAGE_URL", srv.URL)
	t.Setenv("ORCH_TRIAGE_KEY", "k")
	questions := map[string]any{
		"relation":       map[string]any{"type": "choice", "criteria": map[string]any{"duplicate": "same behavior", "variation_site": "near it", "none": "unrelated"}},
		"best_candidate": map[string]any{"type": "choice", "criteria": map[string]any{"none": "no candidate decides it", "1": "candidate 1", "2": "candidate 2"}},
	}
	ans, model, u, err := postDecisionsWithContext(context.Background(), profile{Harness: "api", Provider: "openrouter", Model: "m"}, map[string]any{"item": "x"}, questions)
	if err != nil || model != "served" || ans["relation"].Choice != "none" || u.Tokens != 5 || u.Cost != 0.01 || got["model"] != "m" {
		t.Fatalf("%v %v %v %v %v", ans, model, u, err, got)
	}
	qs := got["questions"].(map[string]any)
	if best := qs["best_candidate"].(map[string]any)["criteria"].(map[string]any); len(best) != 3 {
		t.Errorf("best_candidate criteria %v", best)
	}
	if !reflect.DeepEqual(got["state"], map[string]any{"item": "x"}) {
		t.Errorf("request lost its state: %v", got)
	}
}

// ---- compare ----

func TestCompareShowsItemColumnsAgreementAndReferenceScores(t *testing.T) {
	items := []gateItem{{ID: "p#1", Package: "p", Text: "Build a retry queue for failed sends"}, {ID: "p#2", Package: "p", Text: "Add the gateway webhook table"}, {ID: "p#3", Package: "p", Text: "Create the settings page"}}
	a := &armData{name: "a", items: items, group: map[string]string{"p#1": "reuse", "p#2": "new", "p#3": "new"}, judges: map[string]map[string]string{"decider": {"p#1": "reuse", "p#2": "extend", "p#3": "new"}, "flash": {"p#1": "extend", "p#2": "extend"}}, finding: map[string]map[string]bool{},
		rows: []shadowRow{{Judge: "decider", Cost: 0.01, Seconds: 2, Tokens: 100}, {Judge: "flash", Cost: 0.001, Seconds: 1}}}
	b := &armData{name: "b", items: items, group: map[string]string{}, judges: map[string]map[string]string{}, finding: map[string]map[string]bool{"reuse": {"p#1": true, "p#3": true}}}
	a.locations = map[string]map[string]map[string]bool{}
	b.locations = map[string]map[string]map[string]bool{}
	a.addLocation("group", "reuse", "a.go:1")
	a.addLocation("decider", "reuse", "a.go:1")
	a.addLocation("decider", "extend", "b.go:2")
	b.addLocation("group", "reuse", "a.go:1")
	b.addLocation("group", "reuse", "c.go:3")
	refFile := filepath.Join(t.TempDir(), "ref.json")
	os.WriteFile(refFile, []byte(`[{"package":"p","item":"p#1","verdict":"duplicate","candidate":"a.go:1"},{"package":"p","item":"Add the gateway webhook table","verdict":"extend","candidate":"b.go:2","note":"add a column"},{"package":"p","item":"unrelated words entirely here","verdict":"new"}]`), 0o644)
	entries, err := loadReference(refFile)
	if err != nil || len(entries) != 3 {
		t.Fatalf("%v %v", entries, err)
	}
	ref := resolveReference(entries, items)
	if len(ref) != 3 || len(referenceIssues(ref)) != 3 {
		t.Fatalf("legacy references without revisions/maps must remain in the denominator, not fuzzy-match: %v", ref)
	}
	var sb strings.Builder
	printItemComparison(&sb, []*armData{a, b}, ref, true)
	out := sb.String()
	wantContains(t, out,
		`p#1 "Build a retry queue for failed sends" | A [group=reuse decider=reuse flash=extend] | B [group=- decider=- flash=-] | reference=-`,
		"legacy raw agreement A.group ~ A.decider: 2/3 (66%;",
		"legacy raw agreement A.decider ~ A.flash: 1/2 (50%;",
		`p#2 "Add the gateway webhook table": new vs extend`,
		"dependent quality unavailable",
		"answer coverage 0/3 (0%)",
		"judge A.decider: 1 rows",
		"legacy reference reuse, A.group (locations, not behavior quality): 1 found, 1 in the reference, 1 match; precision 100%, recall 100%",
		"legacy reference reuse, B.group (locations, not behavior quality): 2 found, 1 in the reference, 1 match; precision 50%, recall 100%",
		"legacy reference extend, A.decider (locations, not behavior quality): 1 found, 1 in the reference, 1 match; precision 100%, recall 100%")
	if _, err := loadReference(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("a missing reference file was accepted")
	}
	os.WriteFile(refFile, []byte(`[{"package":"p","item":"x","verdict":"maybe"}]`), 0o644)
	if _, err := loadReference(refFile); err == nil {
		t.Error("an unknown reference verdict was accepted")
	}
}

func TestGateCompareCommandReadsItemsShadowRowsAndReference(t *testing.T) {
	h := setup(t)
	gdir := verdictRepo(t, h)
	items := []gateItem{{ID: "api#1", Package: "api", Text: "Build api."}, {ID: "api#2", Package: "api", Text: "Add the POST /api/put endpoint"}}
	for i, name := range []string{"e1", "e2"} {
		dir := filepath.Join(gdir, "experiments", name)
		os.MkdirAll(dir, 0o755)
		rec := gateRecord{Spec: "spec.md", Experiment: name, State: "experiment", PassSeconds: map[string]int{}, PassTokens: map[string]passTokens{}}
		if i == 0 {
			rec.Items = []gateItemResult{{ID: "api#1", Package: "api", Text: "Build api.", Verdict: "reuse", Candidate: "internal/store.go:3", Verified: true}, {ID: "api#2", Package: "api", Text: items[1].Text, Verdict: "extend", Candidate: "internal/store.go:3", Verified: true}}
		}
		b, _ := json.Marshal(rec)
		os.WriteFile(filepath.Join(dir, "gate.json"), b, 0o644)
		ib, _ := json.Marshal(items)
		os.WriteFile(filepath.Join(dir, "items.json"), ib, 0o644)
		sd := shadowDir(gdir, name)
		os.MkdirAll(sd, 0o755)
		row, _ := json.Marshal(shadowRow{Item: "api#1", Package: "api", Judge: "decider", Verdict: "reuse", Cost: 0.002, Seconds: 3})
		os.WriteFile(filepath.Join(sd, "items.jsonl"), append(row, '\n'), 0o644)
	}
	ref := filepath.Join(h.tmp, "ref.json")
	os.WriteFile(ref, []byte(`[{"package":"api","item":"api#1","verdict":"duplicate","candidate":"internal/store.go:3"}]`), 0o644)
	out := h.must("gate", "compare", "spec.md", "e1", "e2", "--reference", ref)
	wantContains(t, out, "coverage A: items: 2, with a verdict 2", "per-item verdicts (2 items", `api#1 "Build api." | A [group=reuse decider=reuse flash=-] | B [group=- decider=reuse flash=-] | reference=-`,
		"reference A: 1 entries, 0 mapped to items; 1 unmapped/ambiguous", "judge B.decider: 1 rows", "legacy reference reuse, A.group (locations, not behavior quality): 1 found, 1 in the reference, 1 match; precision 100%, recall 100%")
	if code, out := h.run("gate", "compare", "spec.md", "e1", "e2", "--reference", filepath.Join(h.tmp, "nope.json")); code == 0 {
		t.Errorf("a missing reference file passed: %s", out)
	}
}

func TestJudgeVerdictClassesAndLegacyWords(t *testing.T) {
	for word, want := range map[string]string{
		"duplicate": verdictReuse, "variation_site": verdictExtend, "none": verdictNew,
		"yes": verdictReuse, "partly": verdictExtend, "no": verdictNew,
		"reuse": verdictReuse, "extend": verdictExtend, "new": verdictNew,
		" Duplicate ": verdictReuse, "other": "", "unknown": "", "": "",
	} {
		if got := judgeVerdict(word); got != want {
			t.Errorf("judgeVerdict(%q) = %q, want %q", word, got, want)
		}
	}
}

func TestComparePrefersBestAtAndFallsBackForLegacyRows(t *testing.T) {
	gdir := t.TempDir()
	dir := filepath.Join(gdir, "experiments", "e")
	os.MkdirAll(dir, 0o755)
	checklist := map[string][]itemCands{"p": {{Item: gateItem{ID: "p#1"}, Cands: []reuseCand{{Path: "searched.go", Start: 3}}}}}
	b, _ := json.Marshal(checklist)
	os.WriteFile(filepath.Join(dir, "checklist.json"), b, 0o644)
	sd := shadowDir(gdir, "e")
	os.MkdirAll(sd, 0o755)
	var rows strings.Builder
	for _, row := range []shadowRow{
		{Item: "p#1", Package: "p", Judge: "decider", Verdict: verdictReuse, Best: "1", At: "hint.go:7-10"},
		{Item: "p#1", Package: "p", Judge: "flash", Verdict: verdictExtend, Best: "1"},
	} {
		b, _ := json.Marshal(row)
		rows.Write(b)
		rows.WriteByte('\n')
	}
	os.WriteFile(filepath.Join(sd, "items.jsonl"), []byte(rows.String()), 0o644)
	a := loadArm(gdir, "e", dir, &gateRecord{})
	if !a.locations["decider"][verdictReuse]["hint.go:7-10"] || a.locations["decider"][verdictReuse]["searched.go:3"] || !a.locations["flash"][verdictExtend]["searched.go:3"] {
		t.Fatalf("compare locations: %+v", a.locations)
	}
}
