package main

import (
	"fmt"
	"strings"
	"testing"
)

const (
	score429     = `jbcontext failed: exit status 1: {"type":"error","message":"Exception during request to https://api.jetbrains.ai/user/v5/indexing/search with code 429: Limit reached. Please try again later","statusCode":429}`
	scoreLicense = `working-tree semantic search failed: jbcontext failed: exit status 1: {"type":"error","message":"JetBrains AI rejected license OZQIDX5ZGG: The exchange used the JetBrains Daemon (IDE/Toolbox) session (dev@example.com). Pick an option"}`
)

// scoreFixture has the shape of the reported crm1-ui-shell brief: concept
// queries over three scopes, exact-string counts, failed and unavailable
// lookups, and enough lookups to exhaust the 24 KiB budget before the scored
// sections when they are printed one by one.
func scoreFixture() *ctxBrief {
	d := &ctxBrief{ID: "crm", root: "/work/repo"}
	scopes := []string{".", "web/", "internal/"}
	for qi := range 24 {
		q := fmt.Sprintf("Code that handles shell concern number %d for storefront pages", qi)
		for si, sc := range scopes {
			switch {
			case qi%6 == 5:
				d.Lookups = append(d.Lookups, lookupRecord(q, sc, "concept", 0, 4, fmt.Errorf("%s", score429), ""))
			case qi%6 == 4 && si == 0:
				d.Lookups = append(d.Lookups, lookupRecord(q, sc, "concept", 0, 4, fmt.Errorf("%s", scoreLicense), ""))
			default:
				d.Lookups = append(d.Lookups, lookupRecord(q, sc, "concept", si+1, 4, nil, ""))
			}
		}
	}
	for ni := range 10 {
		d.Lookups = append(d.Lookups, lookupRecord(fmt.Sprintf("crm1-name%d", ni), ".", "text", 40+ni, 0, nil, ""))
	}
	d.Lookups = append(d.Lookups,
		lookupRecord("brandCSS", "web", "symbol", 0, 0, nil, "symbol backend returned no execution record"),
		lookupRecord("renderStatus", "web", "symbol", 0, 0, nil, "symbol backend returned no execution record"))
	d.Files = []string{"- `internal/alpha/alpha.go` (score 5: jb+sym): defines Alpha", "- `web/beta.ts` (score 4: jb): concept search", "- `internal/gamma/gamma.go` (score 2: text): text `Gamma`"}
	d.interfaceSummary = []string{"- Alpha (go; exact)\n  `internal/alpha/alpha.go:10` [evidence-sig] match=exact signature: func Alpha(); callers: 3 (by_name=false); tests: 1"}
	d.Facts = []symFact{{Symbol: "Alpha", Defs: []symDef{{Where: "internal/alpha/alpha.go:10"}}}}
	for i := range 12 {
		d.Like = append(d.Like, fmt.Sprintf("- `web/like%02d.ts` (lines 1-3) in func Like%02d via text `Like`\n    func Like%02d() {}", i, i, i))
	}
	d.Missing = []string{"- `Nonexistent`", "- `transfer_rows`"}
	return d
}

func sectionOf(t *testing.T, text, from string) string {
	t.Helper()
	i := strings.Index(text, from)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", from, text)
	}
	rest := text[i+len(from):]
	if j := strings.Index(rest, "\n###"); j >= 0 {
		rest = rest[:j]
	}
	if j := strings.Index(rest, "\nCut for"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestBriefScoreFirstLeadsWithTheScoredPictureAndKeepsTheLookupLogOut(t *testing.T) {
	d := scoreFixture()
	out := d.Render("## Package crm", "package", briefTestTotal)
	t.Logf("fixture brief: %d lines, %d bytes (budget %d)\n%s", strings.Count(out, "\n"), len(out), briefTestTotal, out)
	if len(out) > briefTestTotal {
		t.Errorf("brief is %d bytes, past the budget", len(out))
	}
	for _, bad := range []string{"lookup-", "completeness=", "status=", "count=", "never establish absence", "unresolved:", `{"type"`, "observation ", "statusCode", "OZQIDX5ZGG", "dev@example.com", "Recorded omissions"} {
		if strings.Contains(out, bad) {
			t.Errorf("the prompt still contains %q", bad)
		}
	}
	if n := strings.Count(out, "Absence of a hit is not proof of absence."); n != 1 {
		t.Errorf("the absence sentence appears %d times, want once", n)
	}
	if strings.Contains(out, "dossier") || strings.Contains(out, "planner") {
		t.Errorf("the brief names the removed dossier or planner:\n%s", out)
	}
	pos := func(s string) int {
		i := strings.Index(out, s)
		if i < 0 {
			t.Fatalf("missing %q", s)
		}
		return i
	}
	if !(pos("### 1. Ranked files") < pos("### 2. Interface sheet") && pos("### 2. Interface sheet") < pos("### 3. Existing things") && pos("### 3. Existing things") < pos("### Search notes")) {
		t.Errorf("sections are not in the order ranked files, interface sheet, look-alikes, notes:\n%s", out)
	}
	if body := sectionOf(t, out, "### 1. Ranked files\n"); !strings.HasPrefix(body, "- `internal/alpha/alpha.go`") {
		t.Errorf("ranked files is empty or unordered:\n%s", body)
	}
	if body := sectionOf(t, out, "### 2. Interface sheet\n"); !strings.Contains(body, "Alpha") {
		t.Errorf("interface sheet is empty:\n%s", body)
	}
	notes := sectionOf(t, out, "### Search notes\n")
	if n := strings.Count(strings.TrimRight(notes, "\n"), "\n") + 2; n > 8 {
		t.Errorf("search notes are %d lines, want at most 8:\n%s", n, notes)
	}
	wantContains(t, notes, "Lookups: ", " failed, ", " unavailable", "`Nonexistent`", "`transfer_rows`")
	// One line per distinct failure reason, not per lookup or per scope.
	if n := strings.Count(notes, "429"); n != 1 {
		t.Errorf("the 429 reason is listed %d times, want once:\n%s", n, notes)
	}
	if n := strings.Count(notes, "rejected license"); n != 1 {
		t.Errorf("the license reason is listed %d times, want once:\n%s", n, notes)
	}
	if n := strings.Count(notes, "symbol backend returned no execution record"); n != 1 {
		t.Errorf("the unavailable reason is listed %d times, want once:\n%s", n, notes)
	}
}

func TestBriefScoreFirstOmitsSectionsWithNothingToSay(t *testing.T) {
	d := &ctxBrief{ID: "web", root: "/work/repo", Lookups: []evidenceLookup{lookupRecord("q", "web/", "concept", 2, 4, nil, "")}}
	out := d.Render("## Package web", "package", briefTestTotal)
	for _, heading := range []string{"### 1. Ranked files", "### 1b. Docs", "### 2. Interface sheet", "### 3. Existing", "### 3b.", "### 5."} {
		if strings.Contains(out, heading) {
			t.Errorf("%q is rendered with nothing under it:\n%s", heading, out)
		}
	}
	// Whatever the budget, a heading has a line under it.
	full := scoreFixture()
	for total := 200; total <= briefTestTotal; total += 97 {
		out := full.Render("## Package crm", "package", total)
		lines := strings.Split(out, "\n")
		for i, l := range lines {
			if !strings.HasPrefix(l, "### ") {
				continue
			}
			if i+1 >= len(lines) || lines[i+1] == "" || strings.HasPrefix(lines[i+1], "### ") {
				t.Fatalf("budget %d: %q has nothing under it:\n%s", total, l, out)
			}
		}
	}
}

func TestBriefScoreFirstBudgetCutsLookAlikesBeforeTheNotes(t *testing.T) {
	full := scoreFixture()
	likeMissing := func(out string) bool {
		for i := range 12 {
			if !strings.Contains(out, fmt.Sprintf("web/like%02d.ts", i)) {
				return true
			}
		}
		return false
	}
	notesGoneAt, likeGoneAt := 0, 0
	for total := briefTestTotal; total >= 600; total -= 40 {
		out := full.Render("## Package crm", "package", total)
		if !strings.Contains(out, "### Search notes") && notesGoneAt == 0 {
			notesGoneAt = total
			if !strings.Contains(out, "the search notes") {
				t.Errorf("budget %d: the cut notice does not say the notes were cut:\n%s", total, out)
			}
		}
		if likeMissing(out) && likeGoneAt == 0 {
			likeGoneAt = total
		}
		if n := strings.Count(out, "### Search notes"); n > 1 {
			t.Fatalf("budget %d: notes rendered more than once", total)
		}
		if len(out) > total {
			t.Fatalf("budget %d: brief is %d bytes", total, len(out))
		}
	}
	if notesGoneAt == 0 || likeGoneAt == 0 {
		t.Fatalf("the budget sweep never cut the notes (%d) and a look-alike (%d)", notesGoneAt, likeGoneAt)
	}
	if notesGoneAt > likeGoneAt {
		t.Errorf("the notes were cut at budget %d before a look-alike at %d", notesGoneAt, likeGoneAt)
	}
	out := full.Render("## Package crm", "package", likeGoneAt-40)
	if strings.Count(out, "Cut for") != 1 {
		t.Errorf("cut lines %d:\n%s", strings.Count(out, "Cut for"), out)
	}
	wantContains(t, out, "Read the cited files with the Read tool for the rest.")
	if strings.Contains(out, "dossier") || strings.Contains(out, "planner") {
		t.Errorf("the cut line names the removed dossier or planner:\n%s", out)
	}
	if strings.Count(out, "Recorded omissions") != 0 {
		t.Errorf("the omission list is printed in the prompt")
	}
}
