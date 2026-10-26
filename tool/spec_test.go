package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExplicitBuildQueriesAndPointers(t *testing.T) {
	text := "- Saves a payment method after checkout; a replay\n  refreshes its details.\n  search: Code that saves a payment method\n  search: Code that refreshes display details\n  reuse: store.go#Save, data.sql:2-4\n- Lists gateways.\n"
	got, problems := parseBuilds(text)
	if len(problems) != 0 || len(got) != 2 || got[0].Text != "Saves a payment method after checkout; a replay refreshes its details." || len(got[0].Queries) != 2 || len(got[0].Reuse) != 2 || len(got[1].Queries) != 0 {
		t.Fatalf("%+v %v", got, problems)
	}
	var old gateItem
	if err := json.Unmarshal([]byte(`{"id":"p#1","reuse":"store.go:3"}`), &old); err != nil || len(old.Reuse) != 1 {
		t.Fatalf("legacy items: %+v %v", old, err)
	}
	p := &specPackage{id: "p", body: "Body", builds: got}
	items := packageItems(p)
	if items[0].ID != "p#1" || items[1].ID != "p#2" {
		t.Fatalf("legacy item keys changed: %+v", items)
	}
	task := specText(p)
	wantContains(t, task, "store.go#Save, data.sql:2-4", "search: Code that refreshes display details")
}

func TestItemsAreNeverInferred(t *testing.T) {
	p := &specPackage{id: "p", body: "Builds a queue", acceptance: "- Add a sender"}
	if len(packageItems(p)) != 0 {
		t.Fatal("inferred items")
	}
}

func TestSymbolAndRangePointers(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"store.go": "package store\ntype Store struct{}\nfunc Save() {}\nfunc (s *Store) Save() {}\nvar called = Save\n",
		"data.odd": "the Token lives here\n",
		"view.ts":  "export class View {\n  render() { return 1; }\n}\n",
	})
	for _, ptr := range []string{"store.go#Save", "store.go#Store.Save", "store.go:2-4", "view.ts#View.render"} {
		note, err := checkPointer(repo, ptr)
		if err != nil || note != "" {
			t.Errorf("%s: %s %v", ptr, note, err)
		}
	}
	for _, ptr := range []string{"store.go#Absent", "store.go#Wrong.Save", "store.go:9", "store.go:4-2", "missing.go#Save", "../outside.go:1"} {
		_, err := checkPointer(repo, ptr)
		if err == nil || !strings.Contains(err.Error(), ptr) {
			t.Errorf("%s: %v", ptr, err)
		}
	}
	note, err := checkPointer(repo, "data.odd#Token")
	if err != nil || !strings.Contains(note, "word match") {
		t.Fatalf("fallback %s %v", note, err)
	}
	files := specFiles(repo, "store.go#Store.Save, view.ts#View.render, store.go:2")
	if strings.Join(files, ",") != "store.go,view.ts" {
		t.Fatal(files)
	}
}

func TestSpecQueriesParserGenerationAndSelection(t *testing.T) {
	dir := t.TempDir()
	src := planHeader + "### api: API\nkind: ship\nowns: api/\n\nBody.\n#### Builds\n- Saves methods.\n  search: Code that saves methods\n- Lists gateways.\n  reuse: store.go#List\n#### Acceptance\nTests.\n\n### legacy: Old\nkind: ship\nowns: old/\n\nMakes old things.\n#### Acceptance\nTests old things.\n" + planTail
	if err := os.WriteFile(filepath.Join(dir, "spec.md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errs strings.Builder
	a := &app{cwd: dir, out: &out, errw: &errs}
	run := func(args ...string) (int, string) {
		out.Reset()
		errs.Reset()
		code := a.run(append([]string{"spec", "queries", "spec.md"}, args...))
		return code, out.String() + errs.String()
	}
	if code, s := run("--package", "api"); code != 0 || !strings.Contains(s, "api#2: Lists gateways.") {
		t.Fatalf("%d %s", code, s)
	}
	if code, s := run("--item", "api#2", "--json"); code != 0 || !strings.Contains(s, `"reuse":["store.go#List"]`) || strings.Contains(s, "api#1") {
		t.Fatalf("%d %s", code, s)
	}
	if code, s := run("--generate"); code != 2 || !strings.Contains(s, "explicit --all") {
		t.Fatalf("%d %s", code, s)
	}
	if code, s := run("--item", "api#9"); code == 0 || !strings.Contains(s, "no matching") {
		t.Fatalf("%d %s", code, s)
	}
	if code, s := run("--package", "api", "--generate", "--dry-run"); code != 0 || strings.Contains(s, "- Saves methods.") || !strings.Contains(s, "- Lists gateways.") {
		t.Fatalf("%d %s", code, s)
	}
	old := queryModelRunner
	defer func() { queryModelRunner = old }()
	calls := 0
	queryModelRunner = func(role, prompt string) (string, error) {
		calls++
		if role != "spec_queries" || !strings.Contains(prompt, "Makes old things.") {
			t.Fatalf("%s %s", role, prompt)
		}
		return "#### Builds\n- Makes old things.", nil
	}
	if code, s := run("--package", "legacy", "--generate"); code != 0 || !strings.Contains(s, "#### Builds") || calls != 1 {
		t.Fatalf("%d %s calls %d", code, s, calls)
	}
	if code, s := run("--item", "api#1", "--generate"); code != 0 || !strings.Contains(s, "No selected items") || calls != 1 {
		t.Fatalf("%d %s calls %d", code, s, calls)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "spec.md")); err != nil || string(b) != src {
		t.Fatal("spec edited", err)
	}
}

func TestChecklistMetricsAndLegacyShadowLocations(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"dossier/p.md": "### 3a. Reuse checklist\n- `p#1` Saves.\n  1. `store.go:10-20` func Save\n     | func Save() {}\n- `p#2` Lists.\n  candidate `store.go:10-20` shown above\n### 3. Existing things\n"})
	legacy := legacyChecklist(dir)
	if len(legacy["p"]) != 2 || legacy["p"][0].Cands[0].at() != "store.go:10-20" {
		t.Fatalf("%+v", legacy)
	}
	legacy["p"][0].Item.Queries = []string{"Code that saves"}
	legacy["p"][0].QueriesRun = 1
	legacy["p"][1].Cands = legacy["p"][0].Cands
	got := checklistStats(dir, legacy)
	if got != "items with queries 1; queries run 1; distinct candidate locations 1; bodies shown 1" {
		t.Fatal(got)
	}
	entries := []refEntry{{Package: "p", Item: "old#1", Verdict: "duplicate", Candidate: "store.go:12"}, {Package: "p", Item: "old#2", Verdict: "duplicate", Candidate: "store.go:30"}}
	if ref := resolveReference(entries, []gateItem{{ID: "p#1", Package: "p", Text: "Different wording"}}); len(ref) != 2 {
		t.Fatal(ref)
	}
}

func TestReferenceScoresLocationsAcrossDifferentItems(t *testing.T) {
	a := &armData{locations: map[string]map[string]map[string]bool{}}
	a.addLocation("group", verdictReuse, "store.go:10-20")
	a.addLocation("group", verdictReuse, "store.go:15")
	var out strings.Builder
	printLocationScores(&out, []*armData{a}, map[string]refEntry{"old#7": {Verdict: "duplicate", Candidate: "store.go:12"}}, []string{"group"})
	wantContains(t, out.String(), "1 found, 1 in the reference, 1 match; precision 100%, recall 100%")
}

func TestBuildsExplicitIdentityRevisionAndReordering(t *testing.T) {
	first := "- Saves methods.\n  id: save.v1\n  search: Code that saves methods\n  reuse: store.py#Store.save\n"
	second := "- Lists methods.\n  id: list-methods\n  reuse: view.ts#View.render\n"
	items := func(text string) []gateItem {
		t.Helper()
		builds, problems := parseBuilds(text)
		if len(problems) > 0 {
			t.Fatal(problems)
		}
		return packageItems(&specPackage{id: "api", builds: builds})
	}
	before, after := items(first+second), items(second+first)
	if !reflect.DeepEqual(before[0], after[1]) || !reflect.DeepEqual(before[1], after[0]) || before[0].ID != "api#save.v1" {
		t.Fatalf("reordered identities changed: %+v %+v", before, after)
	}
	for _, changed := range []string{
		strings.Replace(first, "Saves methods.", "Saves methods durably.", 1),
		strings.Replace(first, "Code that saves methods", "Code that persists methods", 1),
		strings.Replace(first, "Store.save", "Store.persist", 1),
		strings.Replace(first, "Saves methods.", "Saves  methods.", 1),
		strings.Replace(first, "search: Code", "search:  Code", 1),
	} {
		got := items(changed)[0]
		if got.ID != before[0].ID || got.Revision == before[0].Revision || len(got.Revision) != 64 {
			t.Fatalf("edited content retained its revision: %+v %+v", before[0], got)
		}
	}
	if got := items(strings.Replace(first, "id: save.v1", "id: renamed", 1))[0]; got.Revision != before[0].Revision {
		t.Fatal("identity was included in content revision")
	}
	for _, input := range []string{"save.v1", "#save.v1", "api#save.v1"} {
		if got := itemKey("api", input); got != "api#save.v1" {
			t.Fatalf("%q: %q", input, got)
		}
	}
	for _, input := range []string{"2", "#2", "api#2"} {
		if got := itemKey("api", input); got != "api#2" {
			t.Fatalf("legacy %q: %q", input, got)
		}
	}
}

func TestBuildsIdentityErrorsAndSupersession(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"- One\n  id: same\n- Two\n  id: same\n", "duplicate Builds id"},
		{"- One\n  id: 1\n", "invalid id"},
		{"- One\n  id: Bad\n", "invalid id"},
		{"- One\n  id:\n", "invalid id"},
		{"- One\n  id: one\n  id: two\n", "repeated id"},
		{"- One\n  supersedes: old\n", "requires a new explicit id"},
		{"- One\n  id: one\n  supersedes: one\n", "cannot supersede itself"},
		{"- One\n  id: one\n  supersedes: old,\n", "invalid supersedes"},
		{"- One\n  id: one\n  supersedes: old, old\n", "repeats supersedes"},
		{"- One\n  id: one\n  supersedes: two\n- Two\n  id: two\n", "still-active id"},
	} {
		_, problems := parseBuilds(tc.text)
		if !strings.Contains(strings.Join(problems, "; "), tc.want) {
			t.Errorf("%q: %v", tc.text, problems)
		}
	}
	// A split maps both new identities to the same predecessor; a merge lists
	// both predecessors in declaration order. No source-function deduplication.
	builds, problems := parseBuilds("- Saves.\n  id: save\n  supersedes: durable\n- Restores.\n  id: restore\n  supersedes: durable\n- Shows transfers.\n  id: combined\n  supersedes: queued, running\n")
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	p := &specPackage{id: "api", builds: builds}
	its := packageItems(p)
	if !reflect.DeepEqual(its[0].Supersedes, []string{"api#durable"}) || !reflect.DeepEqual(its[1].Supersedes, its[0].Supersedes) || !reflect.DeepEqual(its[2].Supersedes, []string{"api#queued", "api#running"}) || its[0].Text != "Saves." {
		t.Fatalf("supersession: %+v", its)
	}
	wantContains(t, specText(p), "- api#save: Saves.", "supersedes: api#durable", "supersedes: api#queued, api#running")
	sp := parseSpec(planHeader + "### api: API\nkind: ship\nowns: api/\n\nBody.\n#### Builds\n- One\n  id: same\n- Two\n  id: same\n#### Acceptance\nTests.\n" + planTail)
	wantContains(t, strings.Join(sp.problems, "; "), "package api", "duplicate Builds id")
	other, problems := parseBuilds("- Another package's save.\n  id: save\n")
	if len(problems) != 0 || packageItems(&specPackage{id: "other", builds: other})[0].ID != "other#save" {
		t.Fatalf("slug was not package-local: %v", problems)
	}
}

func TestSpecIDsPreviewIsNonMutatingAndRejectsInvalidIDs(t *testing.T) {
	dir := t.TempDir()
	src := planHeader + "### api: API\nkind: ship\nowns: api/\n\nBody.\n#### Builds\n- Saves methods.\n  id: saves-methods\n- Saves methods.\n- Saves methods.\n- 123.\n#### Acceptance\nTests.\n" + planTail
	path := filepath.Join(dir, "spec.md")
	writeTree(t, dir, map[string]string{"spec.md": src})
	var out, errs strings.Builder
	a := &app{cwd: dir, out: &out, errw: &errs}
	if code := a.run([]string{"spec", "ids", "spec.md"}); code != 0 {
		t.Fatalf("preview: %d %s %s", code, &out, &errs)
	}
	wantContains(t, out.String(), "keep api#saves-methods", "api#2 -> api#saves-methods-2", "api#3 -> api#saves-methods-3", "id: requirement-123", "ambiguous history", "explicit mapping", "Proposed IDs: 3")
	first := out.String()
	out.Reset()
	if code := a.run([]string{"spec", "ids", path}); code != 0 || out.String() != first {
		t.Fatalf("unstable preview: %d %s", code, &out)
	}
	if string(mustRead(t, path)) != src {
		t.Fatal("preview modified the spec")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("preview wrote extra artifacts: %v %v", entries, err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(src, "id: saves-methods", "id: 123", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"ids", "queries"} {
		out.Reset()
		errs.Reset()
		if code := a.run([]string{"spec", sub, path}); code != 1 || !strings.Contains(out.String()+errs.String(), "invalid id") {
			t.Fatalf("invalid spec accepted: %d %s %s", code, &out, &errs)
		}
	}
}

func TestSpecIDsPreviewDoesNotReviveSupersededIDs(t *testing.T) {
	dir := t.TempDir()
	src := planHeader + "### api: API\nkind: ship\nowns: api/\n\nBody.\n#### Builds\n- Old save.\n- Persists values.\n  id: persist\n  supersedes: old-save\n#### Acceptance\nTests.\n" + planTail
	writeTree(t, dir, map[string]string{"spec.md": src})
	var out, errs strings.Builder
	a := &app{cwd: dir, out: &out, errw: &errs}
	if code := a.run([]string{"spec", "ids", "spec.md"}); code != 0 {
		t.Fatalf("%d %s", code, &errs)
	}
	wantContains(t, out.String(), "api#1 -> api#old-save-2", "id: old-save-2")
}

func TestSpecStableItemsPythonAndTypeScriptFixtures(t *testing.T) {
	for _, fixture := range []struct{ name, path, code, pointer string }{
		{"python", "store.py", "class Store:\n    def save(self, value):\n        return value\n", "store.py#Store.save"},
		{"typescript", "store.ts", "export class Store {\n  save(value: string): string { return value; }\n}\n", "store.ts#Store.save"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			repo := t.TempDir()
			src := planHeader + fmt.Sprintf("### api: Store\nkind: ship\nowns: %s\n\nBody.\n#### Builds\n- Stores values.\n  id: save\n  reuse: %s\n- Rejects invalid values before storing.\n  id: validate\n  reuse: %s\n#### Acceptance\nTests.\n", fixture.path, fixture.pointer, fixture.pointer) + planTail
			writeTree(t, repo, map[string]string{fixture.path: fixture.code, "spec.md": src})
			sp := parseSpec(src)
			if len(sp.problems) > 0 {
				t.Fatal(sp.problems)
			}
			items := packageItems(sp.packages[0])
			if len(items) != 2 || items[0].ID != "api#save" || items[1].ID != "api#validate" || items[0].Reuse[0] != items[1].Reuse[0] {
				t.Fatalf("shared function collapsed requirements: %+v", items)
			}
			if note, err := checkPointer(repo, fixture.pointer); err != nil || note != "" {
				t.Fatalf("qualified %s pointer did not resolve: %s %v", fixture.name, note, err)
			}
			if _, err := checkPointer(repo, strings.Replace(fixture.pointer, "Store.save", "Wrong.save", 1)); err == nil {
				t.Fatal("wrong class qualified method accepted")
			}
			var out, errs strings.Builder
			a := &app{cwd: repo, out: &out, errw: &errs}
			if code := a.run([]string{"spec", "queries", "spec.md", "--item", "api#validate", "--generate", "--dry-run"}); code != 0 {
				t.Fatalf("%d %s", code, &errs)
			}
			wantContains(t, out.String(), "id: validate", fixture.pointer)
		})
	}
}

func TestBenchItemsOldArchiveHashesStillVerify(t *testing.T) {
	// Frozen pre-ID-extension canonical bytes: unmarshalling must not populate
	// new fields, change field order, or silently upgrade old stored itemsets.
	canonical := `[{"id":"api#1","package":"api","text":"Saves.","kind":"builds","queries":["Code that saves"],"reuse":["store.py#Store.save"]}]`
	sum := sha256.Sum256([]byte(canonical))
	id := fmt.Sprintf("%x", sum)[:16]
	archive := fmt.Sprintf(`{"schema_version":1,"id":%q,"origin":"spec","items":%s}`, id, canonical)
	if err := benchObjectCheck("itemset", id, []byte(archive)); err != nil {
		t.Fatalf("old itemset no longer verifies: %v", err)
	}
	var set benchItemset
	if err := json.Unmarshal([]byte(archive), &set); err != nil {
		t.Fatal(err)
	}
	got, err := benchCanonicalItems(set.Items)
	if err != nil || string(got) != canonical || set.Items[0].Revision != "" || len(set.Items[0].Supersedes) != 0 {
		t.Fatalf("old itemset changed: %s %v", got, err)
	}
	builds, _ := parseBuilds("- Saves.\n  id: save\n  supersedes: old-save\n  reuse: store.py#Store.save\n")
	current := benchMakeItemset("spec", "", packageItems(&specPackage{id: "api", builds: builds}))
	encoded, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := benchObjectCheck("itemset", current.ID, encoded); err != nil {
		t.Fatal(err)
	}
	encoded = []byte(strings.Replace(string(encoded), "old-save", "different", 1))
	if err := benchObjectCheck("itemset", current.ID, encoded); err == nil {
		t.Fatal("tampered supersession retained an itemset hash")
	}
}

func TestSpecItemAnswersCannotMoveAcrossVersions(t *testing.T) {
	for _, tc := range []struct{ name, builds, changed, id string }{
		{"legacy-reorder", "- Saves values.\n- Lists values.\n", "- Lists values.\n- Saves values.\n", "api#1"},
		{"explicit-content-edit", "- Saves values.\n  id: save\n", "- Saves valid values only.\n  id: save\n", "api#save"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			src := planHeader + "### api: API\nkind: ship\nowns: api/\n\nBody.\n#### Builds\n" + tc.builds + "#### Acceptance\nTests.\n" + planTail
			h.commitSpec(src)
			old := packageItems(parseSpec(src).packages[0])
			if old[0].ID != tc.id {
				t.Fatal(old)
			}
			rec, _, oldDir := saveSubjectApproval(t, h, h.repo)
			rec.Items = []gateItemResult{{ID: old[0].ID, Package: "api", Text: old[0].Text, Verdict: verdictNew}}
			if err := writeGate(oldDir, rec); err != nil {
				t.Fatal(err)
			}
			if !gatePassed(h.a.siteRoot(t), h.repo, []byte(src)) {
				t.Fatal("original answer not usable in its exact subject")
			}
			changed := strings.Replace(src, tc.builds, tc.changed, 1)
			items := packageItems(parseSpec(changed).packages[0])
			if items[0].ID != old[0].ID || items[0].Revision == old[0].Revision {
				t.Fatal("item key or content revision incorrect")
			}
			// Even forcibly copying an old approval to the new storage locator
			// cannot attach an old answer to an edited or reordered requirement.
			rec.Hash = specHash([]byte(changed))
			if err := writeGate(gateDir(h.a.siteRoot(t), rec.Hash), rec); err != nil {
				t.Fatal(err)
			}
			if gatePassed(h.a.siteRoot(t), h.repo, []byte(changed)) {
				t.Fatal("coinciding ID inherited an old answer across spec versions")
			}
		})
	}
}

func TestSpecCompareRecoversExplicitRequirementIDs(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"dossier/api.md": "### Reuse checklist\n- `api#1` Legacy.\n- `api#save.v1` Saves.\n  1. `store.py:1-3` Store.save\n- `api#load_state-2` Restores.\n"})
	got := legacyChecklist(dir)["api"]
	if len(got) != 3 || got[0].Item.ID != "api#1" || got[1].Item.ID != "api#save.v1" || got[2].Item.ID != "api#load_state-2" || len(got[1].Cands) != 1 {
		t.Fatalf("lost legacy or explicit requirement IDs: %+v", got)
	}
}
