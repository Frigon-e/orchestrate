package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const briefSpec = planHeader + "### api: Queue API\nkind: ship\nowns: internal/queue\n\nBuild the queue. It adds `SendTransfer`, a `transfer_rows` table and `Nonexistent`, and serves POST /api/transfers. See internal/queue/queue.go:4 and internal/gone.go:9.\n\n#### Builds\n- Code that queues a transfer and serves POST /api/transfers.\n\n#### Acceptance\nTests pass.\n\n" + planTail

// briefRepo is a committed repo with a queue package and a spec of one
// package that owns it, names things that exist and things that do not.
func briefRepo(t *testing.T) (code, src string) {
	t.Helper()
	t.Setenv("ORCH_CACHE_DIR", t.TempDir())
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg is not installed")
	}
	t.Setenv("ORCH_GOPLS", filepath.Join(t.TempDir(), "no-gopls"))
	t.Setenv("ORCH_TSLS", filepath.Join(t.TempDir(), "no-tsls"))
	code = t.TempDir()
	for name, body := range map[string]string{
		"internal/queue/queue.go":      "package queue\n\n// Enqueue adds a transfer to the queue.\nfunc Enqueue() {}\n",
		"internal/queue/queue_test.go": "package queue\n",
		"internal/store.go":            "package internal\n\nfunc SendTransfer() {}\n// POST /api/transfers is served here\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(code, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(code, name), []byte(body), 0o644)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@e.com", "commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = code
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	src = briefSpec
	return code, src
}

// briefFor builds the brief of the api package of src.
func briefFor(t *testing.T, code, src, cache string) (*ctxBrief, string) {
	t.Helper()
	sp := parseSpec(src)
	sp.validate()
	if len(sp.problems) > 0 {
		t.Fatalf("spec problems: %v", sp.problems)
	}
	env := newCtxEnv(code, ctxOpts{Skip: []string{"spec.md"}})
	env.CacheDir = cache
	b := env.Briefs([]briefInput{{ID: "api", Title: "Code that queues a transfer and serves POST /api/transfers", Text: packageBlocks(sp, src)["api"], Owns: []string{"internal/queue"}}})[0]
	return b, b.Render("## Package api", "package", briefTestTotal)
}

func TestBriefRanksFilesAndFindsWhatExistsAndWhatDoesNot(t *testing.T) {
	code, src := briefRepo(t)
	jb, _, _ := fakeJbcontext(t)
	t.Setenv("ORCH_JBCONTEXT", jb)
	// The indexed Put chunk does not exist in this checkout. It must not
	// contribute semantic ranking points to the real SendTransfer text hit.
	_, stale := briefFor(t, code, src, t.TempDir())
	wantContains(t, stale, "`internal/store.go` (score 1: text): text `SendTransfer`",
		"- `internal/store.go` (lines 3, 4) in func SendTransfer via text `SendTransfer`; text `POST /api/transfers`\n    func SendTransfer() {}")
	if strings.Contains(stale, "concept search") {
		t.Fatalf("a stale semantic hit contributed ranking evidence:\n%s", stale)
	}
	// A truthful semantic anchor still gets the original cross-tool bonus.
	fakeJbResults(t, [][3]string{{"internal/store.go", "2", `\nfunc SendTransfer() {}`}})
	_, text := briefFor(t, code, src, t.TempDir())

	wantContains(t, text,
		"### 1. Ranked files", "### 3. Existing things that look like what this package builds (outside its owns)", "### Search notes\n",
		// owned and named in the text: 2 + 2 is not it; owned file also pointed at gets both reasons
		"`internal/queue/queue.go` (score 2: own): owned; named in the text",
		// a concept hit (2) and a rare rg hit (1) in the same file, plus 2 for two kinds agreeing
		"`internal/store.go` (score 5: jb+text): concept search; text `SendTransfer`",
		// outside the owns, with the line and a snippet
		"- `internal/store.go` (lines 3, 4) in func SendTransfer via concept \"Code that queues a transfer and serves POST /api/transfers\"; text `SendTransfer`; text `POST /api/transfers`\n    func SendTransfer() {}",
		// names that exist nowhere, and a pointer to a file that does not exist
		"Names looked up that were not found: ", "`Nonexistent`", "`transfer_rows`", "`internal/gone.go`")
	if strings.Contains(text, "`internal/queue/queue.go:") {
		t.Errorf("a hit inside the owns was listed as an existing thing:\n%s", text)
	}
	if names := text[strings.Index(text, "Names looked up that were not found"):]; strings.Contains(names, "SendTransfer") {
		t.Errorf("a found name was listed as missing:\n%s", text)
	}
}

func TestBriefIsDeterministicCachedAndCapped(t *testing.T) {
	code, src := briefRepo(t)
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "no-jbcontext"))
	cache := t.TempDir()
	_, first := briefFor(t, code, src, cache)
	_, second := briefFor(t, code, src, cache)
	if first != second {
		t.Errorf("a rebuild from the cache differs:\n%s\n---\n%s", first, second)
	}
	if entries, _ := filepath.Glob(filepath.Join(cache, "rg3-*.json")); len(entries) == 0 {
		t.Error("rg results were not cached")
	}

	// 60 owned files: 40 are taken, 12 are ranked, and long sections are cut between entries.
	for i := range 60 {
		os.WriteFile(filepath.Join(code, "internal", "queue", fmt.Sprintf("f%02d.go", i)), []byte("package queue\n"), 0o644)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = code
	cmd.Run()
	env := newCtxEnv(code, ctxOpts{})
	if n := len(env.ownedFiles([]string{"internal/queue"})); n != briefOwnsCap {
		t.Errorf("owned files = %d, want the cap %d", n, briefOwnsCap)
	}
	d, _ := briefFor(t, code, src, t.TempDir())
	if len(d.Files) != briefFiles {
		t.Errorf("ranked files = %d, want %d", len(d.Files), briefFiles)
	}
}

func TestCapEntriesKeepsWholeEntries(t *testing.T) {
	entries := []string{"- first\n  continuation", "- second", "- third"}
	if got, want := capEntries(entries, len(entries[0])+1), entries[0]+"\n- ... 2 more omitted\n"; got != want {
		t.Errorf("capEntries at the first entry's boundary = %q, want %q", got, want)
	}
	all := strings.Join(entries, "\n") + "\n"
	if got := capEntries(entries, len(all)); got != all {
		t.Errorf("capEntries with an exact fit = %q, want %q", got, all)
	}
	if capEntries(nil, 10) != "(none)\n" {
		t.Error("an empty section is not marked")
	}
}

func TestDocLineFindsFirstCommentLine(t *testing.T) {
	lines := []string{"package a", "", "// Enqueue adds a transfer.", "// More detail.", "func Enqueue() {}"}
	if got := docLine(lines, 5); got != "Enqueue adds a transfer." {
		t.Errorf("docLine = %q", got)
	}
	if got := docLine([]string{"/**", " * Makes a schema.", " */", "export const x = 1"}, 4); got != "Makes a schema." {
		t.Errorf("jsdoc docLine = %q", got)
	}
	if got := docLine([]string{"package a", "", "func B() {}"}, 3); got != "" {
		t.Errorf("no comment gave %q", got)
	}
}

func TestScopeFindingsKnowWhoOwnsTheFileAndStripRemarks(t *testing.T) {
	src := briefSpec
	src = strings.Replace(src, planTail, "### web: Web\nkind: ship\ndepends: api\nowns: internal/web/\n\nWeb.\n\n#### Builds\n- Code that shows a transfer.\n\n#### Acceptance\nOk.\n\n### core: Core\nkind: ship\nowns: internal/core/\n\nCore.\n\n#### Builds\n- Code that stores a transfer.\n\n#### Acceptance\nOk.\n\n"+planTail, 1)
	src = strings.Replace(src, "### api: Queue API\nkind: ship\n", "### api: Queue API\nkind: ship\ndepends: core\n", 1)
	sp := parseSpec(src)
	sp.validate()
	if len(sp.problems) > 0 {
		t.Fatalf("spec problems: %v", sp.problems)
	}
	rec := &gateRecord{}
	scopeFor("api", scopeReply{WillChange: []string{
		"internal/web/signup.go",                                  // owned by a package that builds on api: it wires api in
		"internal/core/session.go",                                // owned by a package api builds on
		"internal/other/thing.go",                                 // owned by nobody
		"internal/queue/review.go (new review queue, thresholds)", // api's own, with a remark
		"internal/queue/extra.go (new)",
	}}, rec, t.TempDir(), sp)
	got := map[string]string{}
	for _, f := range rec.Findings {
		got[f.Evidence] = f.Severity + "|" + f.Claim
	}
	if len(got) != 3 {
		t.Fatalf("findings = %v, want three (the package's own files are not findings)", got)
	}
	if v := got["internal/web/signup.go:1"]; !strings.HasPrefix(v, "concern|") || !strings.Contains(v, "owned by web, which uses this package and wires it in") {
		t.Errorf("a file owned downstream is a question for its owner: %q", v)
	}
	if v := got["internal/core/session.go:1"]; !strings.HasPrefix(v, "blocker|") || !strings.HasSuffix(v, ", owned by core") {
		t.Errorf("a file owned upstream is a blocker: %q", v)
	}
	if v := got["internal/other/thing.go:1"]; !strings.HasPrefix(v, "concern|") {
		t.Errorf("a file nobody owns is a concern: %q", v)
	}
}

func TestChangePathDropsTrailingRemarks(t *testing.T) {
	for in, want := range map[string]string{
		"internal/a.go":       "internal/a.go",
		"internal/a.go (new)": "internal/a.go",
		"internal/a.go (new review queue, block list)": "internal/a.go",
		"`internal/a.go`":  "internal/a.go",
		"internal/a(1).go": "internal/a(1).go",
	} {
		if got := changePath(in); got != want {
			t.Errorf("changePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBriefMixedLookupsRenderOnlyTheSearchNotes: lookups of every state
// leave no lookup record in the prompt, only the notes.
func TestBriefMixedLookupsRenderOnlyTheSearchNotes(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "bin", "jbcontext")
	ok := lookupRecord("SendTransfer", "internal/queue", "text", 3, 0, nil, "")
	empty := lookupRecord("Nonexistent", ".", "text", 0, 0, nil, "")
	unavailable := lookupRecord("admin staff", "web/", "concept", 0, 4, nil, "no concept search: "+binary+" was not found")
	failed := lookupRecord("SettingsPage", "web/", "symbol", 0, 1, fmt.Errorf("%s: no TypeScript facts: no native TypeScript 7 server found (typescript@7 in a node_modules/.pnpm store); set ORCH_TSLS to one", root), "")
	lks := []evidenceLookup{ok, empty, unavailable, failed}
	d := &ctxBrief{
		ID:      "web",
		root:    root,
		Lookups: lks,
	}
	text := d.Render("## Package web", "package", briefTestTotal)
	for _, l := range lks {
		if strings.Contains(text, l.ID) {
			t.Errorf("lookup id %s is in the prompt:\n%s", l.ID, text)
		}
	}
	wantContains(t, text,
		"Lookups: 4 run, 2 ok, 0 partial, 1 failed, 1 unavailable.",
		"1 lookup failed: typescript@7 server not found (set ORCH_TSLS)",
		"1 lookup unavailable: no concept search: jbcontext not found",
	)
	if strings.Contains(text, root) || strings.Contains(text, binary) {
		t.Errorf("a machine path survived rendering:\n%s", text)
	}
	for _, bad := range []string{"completeness=", "status=", "count=", "observation "} {
		if strings.Contains(text, bad) {
			t.Errorf("the prompt contains %q:\n%s", bad, text)
		}
	}
}
