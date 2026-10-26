package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func eq(id int, pkg, text string) enrichedQuestion {
	return enrichedQuestion{N: id, Q: gateQuestion{Package: pkg, Question: text, Decisive: true}}
}

func TestDedupeOnlyExactNormalizedDuplicates(t *testing.T) {
	items := []enrichedQuestion{
		eq(1, "api", "Should the refund endpoint return 404 or 410 when the order is already deleted?"),
		eq(2, "api", "Which table holds the session rows?"),
		eq(3, "api", "Does the refund endpoint return 404 or 410 when an order was already deleted, and which body does it send?"),
		eq(4, "api", "Where is `settings.Key` resolved against `store/queries.sql` at start-up?"),
		eq(5, "api", "Is the lookup of `store/queries.sql` through `settings.Key` cached between requests, and for how long?"),
		eq(6, "api", "Which table\n  holds the session rows?"),
		eq(7, "other", "Which table holds the session rows?"),
	}
	got := dedupeQuestions(items)
	if len(got) != 6 {
		t.Fatalf("%d cards, want 6: %+v", len(got), got)
	}
	for i, n := range []int{1, 2, 3, 4, 5, 7} {
		if got[i].N != n {
			t.Errorf("card %d has ID %d, want %d", i, got[i].N, n)
		}
	}
	if len(got[1].Merged) != 1 || got[1].Merged[0] != items[5].Q.Question || len(got[1].Members) != 2 || got[1].Members[1].N != 6 {
		t.Errorf("exact duplicate lost its original text or identity: %+v", got[1])
	}
	// Negation, word order, case-sensitive identifiers and punctuation are not normalization.
	for _, q := range []string{"Do not retain `Cache`?", "Do retain `Cache`?", "Do retain `cache`?", "Retain do `cache`?", "Do retain `cache`!"} {
		items = append(items, eq(len(items)+1, "api", q))
	}
	if got := dedupeQuestions(items); len(got) != 11 {
		t.Errorf("different decisions collapsed: %+v", got)
	}
	answered := eq(8, "api", items[0].Q.Question)
	answered.Q.AnsweredBy = "spec.md:3"
	if got := dedupeQuestions([]enrichedQuestion{items[0], answered}); len(got) != 2 {
		t.Errorf("different answer states collapsed: %+v", got)
	}
}

func TestDedupeKeepsMostRestrictiveMemberAuthority(t *testing.T) {
	items := []enrichedQuestion{eq(1, "api", "Which test layout?"), eq(2, "api", "Which test layout?")}
	items[0].Tag, items[0].Why = "lead-may-decide", "delegated"
	items[1].Tag, items[1].Why = "owner", "declared owner"
	got := dedupeQuestions(items)
	if len(got) != 1 || got[0].Tag != "owner" || len(got[0].Members) != 2 || got[0].Members[0].Tag != "lead-may-decide" {
		t.Fatalf("member authority lost: %+v", got)
	}
	wantContains(t, enrichedCard(got[0], "spec.md"), "q1-api", "q2-api", "settle-by-lead", "owner")
}

// Exercise the gate, card reader and enrich rerun as an orchestrator uses them.
func TestGateEnrichQuestionAuthorityAndAnswerHistory(t *testing.T) {
	for _, path := range []string{"src/cache.py", "src/cache.ts"} {
		t.Run(filepath.Ext(path), func(t *testing.T) {
			h := setup(t)
			src := strings.Replace(threePkgSpec(), "May decide: names", "May decide: names, test layout", 1)
			src = strings.Replace(src, "One queue per panel.", "One queue per panel. `"+path+"` uses `cache_days`.", 1)
			if err := os.MkdirAll(filepath.Join(h.repo, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			body := "export const cache_days = 30;\n"
			if filepath.Ext(path) == ".py" {
				body = "cache_days = 30\n"
			}
			writeFiles(t, h.repo, map[string]string{path: body})
			h.commitSpec(src)
			bin, _, _ := fakeJbcontext(t)
			t.Setenv("ORCH_JBCONTEXT", bin)
			qs := []gateQuestion{
				{Question: "What retention applies to `" + path + "` and `cache_days`?", Decisive: true},
				{Question: "Which test layout should verify `" + path + "` and `cache_days` using deterministic fixtures and isolated clocks?", Decisive: true},
			}
			for i := range qs {
				qs[i].Packet = &gateQuestionPacket{Goal: "Resolve the cache behavior and its acceptance checks", KnownFacts: []string{path + " defines cache_days = 30"}, Unknown: qs[i].Question, Options: []gatePacketOption{{"a", "Keep the current setting and deterministic fixtures", "Retains current behavior", "Does not add broader coverage"}, {"b", "Change the setting and expand the fixtures", "Covers the new behavior", "Requires new assertions"}}, Recommendation: "a", Rationale: "The current setting is explicit in the fixture", Authority: "Owner decides retention; the lead decides test layout", NextAction: "Record the authorized choice and update the fixture checks"}
			}
			reply, err := json.Marshal(map[string]any{
				"id": "api", "will_change": []string{}, "claims": []any{}, "reuse": []any{}, "questions": qs,
			})
			if err != nil {
				t.Fatal(err)
			}
			h.fakePi(gatePasses(map[string]string{"package-api": string(reply)}))
			code, out := h.run("gate", "spec.md")
			if code != 1 || !strings.Contains(out, "questions: 2 need a decision") {
				t.Fatalf("gate did not accept both unresolved questions (exit %d): %s", code, out)
			}
			gdir := filepath.Join(runRoot(h.repo), "gate", specHash(mustRead(t, filepath.Join(h.repo, "spec.md"))))
			qdir := filepath.Join(gdir, "questions")
			owner := string(mustRead(t, filepath.Join(qdir, "q1-api.md")))
			lead := string(mustRead(t, filepath.Join(qdir, "q2-api.md")))
			wantContains(t, owner, "route: owner", "## The spec may already answer this", qs[0].Question)
			wantContains(t, lead, "route: lead-may-decide", "settle-by-lead", qs[1].Question)
			for _, card := range []string{owner, lead} {
				if strings.Contains(card, "No file in the repo answers") || strings.Contains(card, "no answer exists") {
					t.Errorf("unsupported absence claim: %s", card)
				}
			}
			index := string(mustRead(t, filepath.Join(qdir, "INDEX.md")))
			wantContains(t, index, "| q1-api | owner |", "| q2-api | lead-may-decide |", "settle-by-lead")
			// D2 publishes ready owner questions and reports delegated cards separately.
			wantContains(t, h.must("ask", "--cards", qdir), "questions: 1", "settled_by_lead: 1")
			before := string(mustRead(t, filepath.Join(gdir, "gate.json")))
			answered := filepath.Join(qdir, "answered", "q1-api.md")
			if err := os.MkdirAll(filepath.Dir(answered), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, qdir, map[string]string{"answered/q1-api.md": owner + "\n## Answer\nKeep for 30 days.\n"})
			if err := os.Remove(filepath.Join(qdir, "q1-api.md")); err != nil {
				t.Fatal(err)
			}
			h.must("gate", "enrich", "spec.md")
			wantContains(t, string(mustRead(t, answered)), "Keep for 30 days.", qs[0].Question)
			if exists(filepath.Join(qdir, "q1-api.md")) {
				t.Error("enrichment reopened the answered question")
			}
			wantContains(t, string(mustRead(t, filepath.Join(qdir, "q2-api.md"))), qs[1].Question, "route: lead-may-decide")
			wantContains(t, h.must("ask", "--cards", qdir), "questions: 0", "settled_by_lead: 1")
			if after := string(mustRead(t, filepath.Join(gdir, "gate.json"))); after != before {
				t.Error("enrichment changed the original questions or answer evidence in gate.json")
			}
		})
	}
}

func TestEnrichKeepsLegacyCardIDsAndRecordedAnswers(t *testing.T) {
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	qs := []gateQuestion{
		{Package: "api", Question: "Already answered in the spec?", Decisive: true, AnsweredBy: "spec.md:3"},
		{Package: "api", Question: "How long is retention?", Decisive: true},
		{Package: "api", Question: "Where should the test layout put fixtures?", Decisive: true},
	}
	dir, code, src, sp := enrichFixture(t, qs)
	rec := gateRecord{Questions: qs}
	// Initial/legacy gate output numbers only unresolved questions.
	if err := writeGate(dir, &rec); err != nil {
		t.Fatal(err)
	}
	sp.sections["Authority"] = "May decide: test layout"
	res, err := enrichGate(dir, code, src, "spec.md", sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 || res.Items[0].N != 1 || res.Items[1].N != 2 {
		t.Fatalf("legacy card filenames changed: %+v", res.Items)
	}
	qs[1].AnsweredBy = "spec.md:7"
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"gate.json": string(b)})
	res, err = enrichGate(dir, code, src, "spec.md", sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].N != 2 || res.Items[0].Tag != "lead-may-decide" || len(res.Answered) != 2 {
		t.Fatalf("one recorded answer renumbered or closed another question: %+v", res)
	}
	qdir := filepath.Join(dir, "questions")
	if exists(filepath.Join(qdir, "q1-api.md")) {
		t.Error("recorded answer stayed open")
	}
	wantContains(t, string(mustRead(t, filepath.Join(qdir, "q2-api.md"))), qs[2].Question)
	wantContains(t, string(mustRead(t, filepath.Join(qdir, "INDEX.md"))), qs[0].AnsweredBy, qs[1].AnsweredBy, "| q2-api | lead-may-decide |")
	if got := string(mustRead(t, filepath.Join(dir, "gate.json"))); got != string(b) {
		t.Error("enrichment rewrote answer history")
	}
}

func TestEnrichExactDuplicatesKeepIndependentAnswerState(t *testing.T) {
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	qs := []gateQuestion{
		{Package: "api", Question: "How long is retention?", Decisive: true},
		{Package: "api", Question: "How long\n  is retention?", Decisive: true},
	}
	dir, code, src, sp := enrichFixture(t, qs)
	res, err := enrichGate(dir, code, src, "spec.md", sp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cards != 1 || len(res.Items[0].Members) != 2 {
		t.Fatalf("exact duplicate: %+v", res)
	}
	qdir := filepath.Join(dir, "questions")
	card := string(mustRead(t, filepath.Join(qdir, "q1-api.md")))
	wantContains(t, card, "q1-api", "q2-api", qs[1].Question)
	wantContains(t, string(mustRead(t, filepath.Join(qdir, "INDEX.md"))), "Member q2-api [owner; settle-by-owner]")
	if err := os.MkdirAll(filepath.Join(qdir, "answered"), 0o755); err != nil {
		t.Fatal(err)
	}
	answered := filepath.Join(qdir, "answered", "q1-api.md")
	if err := os.Rename(filepath.Join(qdir, "q1-api.md"), answered); err != nil {
		t.Fatal(err)
	}
	res, err = enrichGate(dir, code, src, "spec.md", sp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cards != 1 || res.Items[0].N != 2 || len(res.Answered) != 1 {
		t.Fatalf("member answer leaked: %+v", res)
	}
	if got := string(mustRead(t, answered)); got != card {
		t.Error("archived original was rewritten")
	}
	wantContains(t, string(mustRead(t, filepath.Join(qdir, "q2-api.md"))), "route: owner")
}

func TestEnrichRecordedDuplicateAnswerKeepsSurvivorIDAcrossReruns(t *testing.T) {
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	qs := []gateQuestion{
		{Package: "api", Question: "How long is retention?", Decisive: true},
		{Package: "api", Question: "How long is retention?", Decisive: true},
	}
	dir, code, src, sp := enrichFixture(t, qs)
	if _, err := enrichGate(dir, code, src, "spec.md", sp); err != nil {
		t.Fatal(err)
	}
	qs[0].AnsweredBy = "spec.md:8"
	b, err := json.Marshal(gateRecord{Questions: qs})
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"gate.json": string(b)})
	for i := range 3 {
		res, err := enrichGate(dir, code, src, "spec.md", sp)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) != 1 || res.Items[0].N != 2 || res.Items[0].Record != 2 || len(res.Answered) != 1 || res.Answered[0].N != 1 {
			t.Fatalf("rerun %d changed member identity/state: %+v", i, res)
		}
	}
	wantContains(t, string(mustRead(t, filepath.Join(dir, "questions", "answered", "q1-api.md"))), "gate-question: 1", "## Answer evidence\n"+qs[0].AnsweredBy)
	wantContains(t, string(mustRead(t, filepath.Join(dir, "questions", "q2-api.md"))), "gate-question: 2")
}

func TestEnrichArchiveConflictPreservesBothOriginalCards(t *testing.T) {
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	qs := []gateQuestion{{Package: "api", Question: "How long is retention?", Decisive: true}}
	dir, code, src, sp := enrichFixture(t, qs)
	if _, err := enrichGate(dir, code, src, "spec.md", sp); err != nil {
		t.Fatal(err)
	}
	qdir := filepath.Join(dir, "questions")
	original := string(mustRead(t, filepath.Join(qdir, "q1-api.md")))
	if err := os.MkdirAll(filepath.Join(qdir, "answered"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "---\nquestion: An unrelated old question?\n---\n## Answer\nKeep this history.\n"
	writeFiles(t, qdir, map[string]string{"answered/q1-api.md": legacy})
	qs[0].AnsweredBy = "spec.md:9"
	b, err := json.Marshal(gateRecord{Questions: qs})
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"gate.json": string(b)})
	if _, err := enrichGate(dir, code, src, "spec.md", sp); err == nil || !strings.Contains(err.Error(), "different question identity") {
		t.Fatalf("archive collision: %v", err)
	}
	if got := string(mustRead(t, filepath.Join(qdir, "answered", "q1-api.md"))); got != legacy {
		t.Error("existing archive overwritten")
	}
	if got := string(mustRead(t, filepath.Join(qdir, "q1-api.md"))); got != original {
		t.Error("original card deleted on archive conflict")
	}
}

func TestEnrichInitialCardDoesNotClaimAbsence(t *testing.T) {
	q := gateQuestion{Package: "api", Question: "How long is retention?", Decisive: true}
	card := gateQuestionCard(q)
	wantContains(t, card, "route: owner", "authority: owner", "action: settle-by-owner", "review reported this question as unresolved")
	if strings.Contains(card, "No file in the repo answers") || strings.Contains(card, "no answer exists") {
		t.Errorf("unsupported initial-card claim: %s", card)
	}
}

func TestEnrichLegacyMergedArchiveDoesNotAnswerMembers(t *testing.T) {
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	qs := fixtureQuestions()[:3]
	dir, code, src, sp := enrichFixture(t, qs)
	qdir := filepath.Join(dir, "questions")
	if err := os.MkdirAll(filepath.Join(qdir, "answered"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "---\nquestion: " + qs[2].Question + " (+1 similar)\n---\n## Answer\nUse 404.\n"
	writeFiles(t, qdir, map[string]string{"answered/q3-api.md": legacy})
	res, err := enrichGate(dir, code, src, "spec.md", sp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cards != 3 || len(res.Answered) != 0 {
		t.Fatalf("ambiguous merged answer closed a member: %+v", res)
	}
	if got := string(mustRead(t, filepath.Join(qdir, "answered", "q3-api.md"))); got != legacy {
		t.Error("legacy answer history lost")
	}
}

func TestQuestionIdentifiersAndSpecHints(t *testing.T) {
	ids := questionIdentifiers("Does FRD-03 keep `retention_days` at 30 days, or read internal/crm/ledger/ and api.Scope(ctx)?")
	want := map[string]bool{"frd-03": true, "retention_days": true, "30 days": true, "internal/crm/ledger/": true, "api.scope": true}
	for _, id := range ids {
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("identifiers %v lack %v", ids, want)
	}
	lines := []string{
		"# Spec",
		"unrelated text about colours",
		"FRD-03 keeps the audit rows; `retention_days` is 30 days",
		"and nothing else here",
		"api.Scope is exported for crm1-support",
		"more filler",
	}
	hints := specHints(lines, []string{"frd-03", "retention_days", "30 days"})
	if len(hints) != 1 || hints[0].Line != 3 || !strings.Contains(hints[0].Text, "retention_days") {
		t.Errorf("hints = %+v", hints)
	}
	if h := specHints(lines, []string{"frd-03", "bogus_name", "other_name"}); len(h) != 0 {
		t.Errorf("a line holding 1 of 3 identifiers matched: %+v", h)
	}
	if h := specHints(lines, []string{"frd-03"}); len(h) != 0 {
		t.Errorf("one identifier is not enough: %+v", h)
	}
	if h := specHints(lines, questionIdentifiers("Which colour does the footer use?")); len(h) != 0 {
		t.Errorf("unrelated question matched: %+v", h)
	}
}

func TestRouteQuestionTagsAndOwnerWinsOnConflict(t *testing.T) {
	may := []string{"template wording", "test layout"}
	for _, c := range []struct{ q, tag string }{
		{"Which refund policy applies to a partly shipped order?", "owner"},
		{"Is the retention period for tickets 30 or 90 days?", "owner"},
		{"What wording does the customer see when consent is withdrawn?", "owner"},
		{"Where should the new test layout put fixtures?", "lead-may-decide"},
		{"Should the template wording say shipped or sent?", "owner"}, // "wording" is an owner word
		{"Does the template use a 2-column wrapper?", "unclear"},
		{"Where do the helper files go for the test layout?", "lead-may-decide"},
		{"Which index backs the lookup?", "unclear"},
	} {
		if tag, _ := routeQuestion(c.q, may); tag != c.tag {
			t.Errorf("%q tagged %s, want %s", c.q, tag, c.tag)
		}
	}
	// Both an owner word and a May-decide phrase: the owner decides.
	if tag, _ := routeQuestion("Is the test layout for the price tests the same?", may); tag != "owner" {
		t.Errorf("conflict tagged %s", tag)
	}
	sp := parseSpec(threePkgSpec())
	if got := mayDecidePhrases(sp); len(got) != 0 {
		t.Errorf("single-word phrases should not count: %v", got)
	}
	sp.sections["Authority"] = "May decide: names, internal structure, `store/queries.go`, test layout"
	if got := strings.Join(mayDecidePhrases(sp), ";"); got != "internal structure;store/queries.go;test layout" {
		t.Errorf("phrases = %q", got)
	}
}

// fakeJbcontext installs a jbcontext stand-in that logs each query; it answers
// with one code chunk and one chunk of the spec, or fails when <dir>/fail exists.
func fakeJbcontext(t *testing.T) (bin, calls, failFlag string) {
	t.Helper()
	dir := t.TempDir()
	bin, calls, failFlag = filepath.Join(dir, "jbcontext"), filepath.Join(dir, "calls.log"), filepath.Join(dir, "fail")
	script := `#!/bin/sh
echo "$*" >> "` + calls + `"
[ -f "` + failFlag + `" ] && { echo "not logged in" >&2; exit 1; }
cat <<'EOF'
{"type":"search_result","results":[
{"result":{"scoredText":{"similarity":0.9},"sourcePosition":{"relativePath":"internal/store.go","startOffset":0,"endOffset":9}},"content":"\nfunc Put() {}\nline2\nline3\nline4\nline5\nline6\nline7\nline8","contentStartLine":2},
{"result":{"sourcePosition":{"relativePath":"spec.md"}},"content":"spec text","contentStartLine":1}]}
EOF
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls, failFlag
}

// enrichFixture writes a gate dir with the given questions and returns what
// enrichGate needs.
func enrichFixture(t *testing.T, qs []gateQuestion) (dir, code, src string, sp *spec) {
	t.Helper()
	if os.Getenv("ORCH_CACHE_DIR") == "" {
		t.Setenv("ORCH_CACHE_DIR", t.TempDir())
	}
	dir, code = t.TempDir(), t.TempDir()
	// The fake jbcontext hit is for this file; a hit for a file the checkout
	// lacks is dropped.
	if err := os.MkdirAll(filepath.Join(code, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := "package internal\n\nfunc Put() {}\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	if err := os.WriteFile(filepath.Join(code, "internal", "store.go"), []byte(store), 0o644); err != nil {
		t.Fatal(err)
	}
	src = threePkgSpec()
	rec := gateRecord{Spec: "spec.md", Questions: qs}
	b, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(dir, "gate.json"), b, 0o644)
	os.WriteFile(filepath.Join(dir, "report.md"), []byte("# Gate report\n\nStatus: **fail**\n"), 0o644)
	return dir, code, src, parseSpec(src)
}

func fixtureQuestions() []gateQuestion {
	return []gateQuestion{
		{Package: "api", Question: "Should the refund endpoint return 404 or 410 when the order is already deleted?", Decisive: true},
		{Package: "api", Question: "Which table holds the session rows? Is `sessions` in internal/store.go?", Decisive: true},
		{Package: "api", Question: "Does the refund endpoint return 404 or 410 when an order was already deleted, and with which body?", Decisive: true},
		// Matching wording across packages must remain separate cards.
		{Package: "store", Question: "Which table holds the session rows? Is `sessions` in internal/store.go? Please clarify.", Decisive: true},
		{Package: "store", Question: "Minor thing about names?", Decisive: false},
		{Package: "store", Question: "Answered one", Decisive: true, AnsweredBy: "internal/store.go:3"},
	}
}

func TestEnrichRewritesCardsWithTagsHintsAndAnIndex(t *testing.T) {
	bin, calls, _ := fakeJbcontext(t)
	t.Setenv("ORCH_JBCONTEXT", bin)
	dir, code, src, sp := enrichFixture(t, fixtureQuestions())
	res, err := enrichGate(dir, code, src, "spec.md", sp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 4 || res.Cards != 4 || res.Owner != 2 || res.Unclear != 2 || res.CodeHits != 4 || len(res.Minor) != 1 {
		t.Fatalf("result = %+v", res)
	}
	wantSummary := "4 -> 4 after merging; owner 2, lead-may-decide 0, unclear 2; spec hints 0; code hits 4; symbol facts 0"
	if res.summary() != wantSummary {
		t.Errorf("summary %q, want %q", res.summary(), wantSummary)
	}

	qdir := filepath.Join(dir, "questions")
	entries, _ := filepath.Glob(filepath.Join(qdir, "*.md"))
	if len(entries) != 5 { // Four independent cards and INDEX.md.
		t.Fatalf("files = %v", entries)
	}
	card := string(mustRead(t, filepath.Join(qdir, "q3-api.md")))
	wantContains(t, card, "Does the refund endpoint", "## Tag\nowner (mentions refund)", "## Possibly relevant code",
		"- `internal/store.go:3-10`", "func Put() {}")
	if strings.Contains(card, "spec text") || strings.Contains(card, "line7") {
		t.Errorf("a spec hit or more than %d snippet lines leaked:\n%s", enrichSnippet, card)
	}
	wantContains(t, string(mustRead(t, filepath.Join(qdir, "q1-api.md"))), "Should the refund endpoint", "route: owner")
	if c, err := parseCard(string(mustRead(t, filepath.Join(qdir, "q4-store.md")))); err != nil || c.question != fixtureQuestions()[3].Question {
		t.Errorf("q4-store.md lost its package's question: %v %+v", err, c)
	}
	for _, f := range entries {
		if filepath.Base(f) == "INDEX.md" {
			continue
		}
		c, err := parseCard(string(mustRead(t, f)))
		if err != nil || c.question == "" || !strings.Contains(c.context, "Raised by the spec gate") {
			t.Errorf("%s is not a valid card: %v %+v", f, err, c)
		}
	}
	index := string(mustRead(t, filepath.Join(qdir, "INDEX.md")))
	wantContains(t, index, wantSummary, "## api", "| q1-api | owner |", "| q3-api | owner |", "## Minor open questions", "store [unclear]: Minor thing about names?")
	wantContains(t, string(mustRead(t, filepath.Join(dir, "report.md"))), "## Question cards\n"+wantSummary)

	// A rerun reads the same input, stacks nothing, and the cache avoids new searches.
	before := string(mustRead(t, calls))
	if _, err := enrichGate(dir, code, src, "spec.md", sp); err != nil {
		t.Fatal(err)
	}
	if after := string(mustRead(t, calls)); after != before {
		t.Errorf("a rerun called jbcontext again:\n%s", after)
	}
	if n := strings.Count(string(mustRead(t, filepath.Join(dir, "report.md"))), "## Question cards"); n != 1 {
		t.Errorf("report has %d question sections", n)
	}
	if n := strings.Count(before, "search --limit 3 --json-output"); n != 4 {
		t.Errorf("%d searches, want one per card (4)", n)
	}
}

func TestEnrichWithoutAWorkingJbcontextStillWritesValidCards(t *testing.T) {
	check := func(t *testing.T, want string) {
		t.Helper()
		dir, code, src, sp := enrichFixture(t, fixtureQuestions())
		res, err := enrichGate(dir, code, src, "spec.md", sp)
		if err != nil {
			t.Fatal(err)
		}
		if res.CodeHits != 0 || len(res.Notes) == 0 || !strings.Contains(strings.Join(res.Notes, "|"), want) {
			t.Fatalf("notes %v hits %d, want a note with %q", res.Notes, res.CodeHits, want)
		}
		wantContains(t, string(mustRead(t, filepath.Join(dir, "questions", "INDEX.md"))), "Note: ")
		c, err := parseCard(string(mustRead(t, filepath.Join(dir, "questions", "q2-api.md"))))
		if err != nil || c.question == "" {
			t.Fatalf("card invalid: %v", err)
		}
	}
	t.Run("missing", func(t *testing.T) {
		t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
		check(t, "was not found")
	})
	t.Run("not logged in", func(t *testing.T) {
		bin, _, failFlag := fakeJbcontext(t)
		os.WriteFile(failFlag, nil, 0o644)
		t.Setenv("ORCH_JBCONTEXT", bin)
		check(t, "not logged in")
	})
}

func TestEnrichWithoutAGateJSONIsAnError(t *testing.T) {
	_, err := enrichGate(t.TempDir(), t.TempDir(), "", "spec.md", parseSpec(threePkgSpec()))
	if err == nil || !strings.Contains(err.Error(), "run `orch gate` first") {
		t.Errorf("err = %v", err)
	}
}

func TestGateEnrichesItsCardsAndAskStillParsesThem(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, "internal"), 0o755)
	os.WriteFile(filepath.Join(h.repo, "internal", "store.go"), []byte("package internal\n\nfunc Put() {}\n"), 0o644)
	h.commitSpec(threePkgSpec())
	bin, _, _ := fakeJbcontext(t)
	t.Setenv("ORCH_JBCONTEXT", bin)
	h.fakePi(gatePasses(map[string]string{"package-api": `{"id":"api","will_change":[],"claims":[],"reuse":[],"questions":[{"question":"What refund policy applies to a deleted order?","answered_by":"","changes_result":true}]}`}))
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, out)
	}
	view := wantGateState(t, out, "incomplete")
	if view.Questions != "1 need a decision" || view.QuestionCards != "1 -> 1 after merging; owner 1, lead-may-decide 0, unclear 0; spec hints 0; code hits 1; symbol facts 0" {
		t.Fatalf("question-card accounting changed: %+v", view)
	}
	gdir := filepath.Join(runRoot(h.repo), "gate", specHash(mustRead(t, filepath.Join(h.repo, "spec.md"))))
	wantContains(t, string(mustRead(t, filepath.Join(gdir, "questions", "q1-api.md"))), "## Tag\nowner (mentions refund)")
	ask := h.must("ask", "--cards", filepath.Join(gdir, "questions"))
	wantContains(t, ask, "questions: 0")
	// Question context now includes legitimate retrieval uncertainty such as
	// "Spec hint search ... skipped". Inspect warnings, not arbitrary prose.
	info := decodedOutput(t, []byte(ask), outputAgent).(map[string]any)
	if _, ok := info["warnings"]; ok {
		t.Errorf("ask tripped over the index:\n%s", ask)
	}

	// `orch gate enrich` redoes it from gate.json; a spec never gated is an error.
	qdir := filepath.Join(gdir, "questions")
	if err := os.MkdirAll(filepath.Join(qdir, "attachments"), 0o755); err != nil {
		t.Fatal(err)
	}
	preserved := map[string]string{
		"notes.md":               "Keep these review notes.\n",
		"attachments/context.md": "Keep this question attachment.\n",
	}
	writeFiles(t, qdir, preserved)
	writeFiles(t, qdir, map[string]string{"q99-api.md": "obsolete generated card"})
	wantContains(t, h.must("gate", "enrich", "spec.md"), "question cards: 1 -> 1 after merging", "INDEX.md")
	for path, want := range preserved {
		if got := string(mustRead(t, filepath.Join(qdir, path))); got != want {
			t.Errorf("enrichment changed %s: %q", path, got)
		}
	}
	if exists(filepath.Join(qdir, "q99-api.md")) {
		t.Error("enrichment retained an obsolete generated card")
	}
	h.commitSpec(strings.Replace(threePkgSpec(), "Users queue transfers.", "Users queue transfers fast.", 1))
	if code, out := h.run("gate", "enrich", "spec.md"); code != 1 || !strings.Contains(out, "run `orch gate` first") {
		t.Errorf("enrich without a gate run = %d %s", code, out)
	}
}

func TestGoSymbolsKeepsGoLookingNamesOnly(t *testing.T) {
	for _, c := range []struct {
		q    string
		want string
	}{
		{"Does Gateway.StartCheckout return an error, and what does payments.NewGateway do?", "Gateway.StartCheckout,payments.NewGateway"},
		{"Why is StartCheckout called twice, and does OrderService retry?", "StartCheckout,OrderService"},
		{"Does `newOrders(ctx)` wire `cart.Deps` and `account_id`?", "newOrders,cart.Deps"},
		{"Is invoice_line_id set in internal/crm/store/queries.sql, or in orders.sql.go and the `invoices` table?", ""},
		{"Is GitHub or PostgreSQL used, and who owns the Stripe sync?", ""},
		{"Do Alpha.Beta, Gamma.Delta, Epsilon.Zeta and Eta.Theta all agree?", "Alpha.Beta,Gamma.Delta,Epsilon.Zeta"}, // capped at 3
		{"Is `Gateway.StartCheckout` the same as StartCheckout?", "Gateway.StartCheckout"},                           // the bare name adds nothing
	} {
		if got := strings.Join(goSymbols(c.q), ","); got != c.want {
			t.Errorf("%q -> %q, want %q", c.q, got, c.want)
		}
	}
}

// goplsFixture makes a checkout with an interface and an implementation and a
// gopls stand-in that answers for it, logging every call.
func goplsFixture(t *testing.T) (code, calls, bin string) {
	t.Helper()
	code, dir := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(code, "pay"), 0o755)
	os.WriteFile(filepath.Join(code, "pay", "pay.go"), []byte("package pay\n\ntype Gateway interface {\n\tStartCheckout(ctx int) error\n}\ntype Stripe struct{}\nfunc (s *Stripe) StartCheckout(ctx int) error { return nil }\n"), 0o644)
	bin, calls = filepath.Join(dir, "gopls"), filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$*" >> "` + calls + `"
[ -f "` + dir + `/sleep" ] && exec sleep 5
C="` + code + `"
case "$1 $2" in
"workspace_symbol StartCheckout")
	echo "$C/pay/pay.go:4:2-15 Gateway.StartCheckout Method"
	echo "$C/pay/pay.go:7:18-31 Stripe.StartCheckout Method"
	echo "$C/pay/pay_test.go:9:6-23 TestStartCheckout Function"
	echo "$C/other/o.go:3:6-19 Other.StartCheckout Method";;
"workspace_symbol NoSuchThing") ;;
"workspace_symbol pay.Build")
	echo "$C/pay/b.go:3:6-11 pay.Build Function"
	echo "$C/other/b.go:3:6-11 other.Build Function";;
"references $C/pay/b.go:3:6") echo "$C/svc/svc.go:40:2-7";;
"references $C/pay/pay.go:4:2")
	echo "$C/pay/pay.go:4:2-15"
	echo "$C/svc/svc.go:10:5-18"
	echo "$C/svc/svc.go:20:5-18"
	echo "$C/pay/pay_test.go:30:3-16";;
"implementation $C/pay/pay.go:4:2") echo "$C/pay/pay.go:7:18-31";;
"references $C/pay/pay.go:7:18") echo "$C/svc/svc.go:30:5-18";;
esac
`
	os.WriteFile(bin, []byte(script), 0o755)
	return code, calls, bin
}

func TestGoplsFactsSplitCallSitesFromTestsAndListImplementations(t *testing.T) {
	code, calls, bin := goplsFixture(t)
	if err := os.MkdirAll(filepath.Join(code, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, code, map[string]string{
		"pay/b.go":   "package pay\n\nfunc Build() {}\n",
		"other/b.go": "package other\n\nfunc Build() {}\n",
		"other/o.go": "package other\ntype Other struct{}\nfunc (Other) StartCheckout() {}\n",
	})
	t.Setenv("ORCH_GOPLS", bin)
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	qs := []gateQuestion{
		{Package: "api", Question: "Does Gateway.StartCheckout retry, and is NoSuchThing involved? Also `invoice_line_id` and internal/crm/store/queries.sql.", Decisive: true},
		{Package: "api", Question: "Who else calls StartCheckout?", Decisive: true},
	}
	dir, _, src, sp := enrichFixture(t, qs)
	res, err := enrichGate(dir, code, src, "spec.md", sp)
	if err != nil {
		t.Fatal(err)
	}
	if res.SymbolFacts != 2 {
		t.Fatalf("symbol facts = %d, notes %v", res.SymbolFacts, res.Notes)
	}
	card := string(mustRead(t, filepath.Join(dir, "questions", "q1-api.md")))
	wantContains(t, card, "## Exact symbol facts",
		"- `Gateway.StartCheckout` (go Method) is defined at `pay/pay.go:4`; 2 callers outside tests: `svc/svc.go:10`, `svc/svc.go:20`; 1 test references; implemented at `pay/pay.go:7`",
		"- no symbol named `NoSuchThing` found (go)")
	for _, not := range []string{"Stripe.StartCheckout", "other/o.go", "TestStartCheckout", "invoice_line_id"} {
		if strings.Contains(card[strings.Index(card, "## Exact symbol facts"):], not) {
			t.Errorf("the facts hold %q:\n%s", not, card)
		}
	}
	// A bare method name returns candidates with unverified receiver identity,
	// while preserving each definition's callers, tests and implementations.
	card2 := string(mustRead(t, filepath.Join(dir, "questions", "q2-api.md")))
	wantContains(t, card2, "`StartCheckout` (go Method) is defined at `pay/pay.go:4` (candidate, identity not verified); 2 callers outside tests: `svc/svc.go:10`, `svc/svc.go:20`; 1 test references; implemented at `pay/pay.go:7`", "`StartCheckout` (go Method) is defined at `pay/pay.go:7` (candidate, identity not verified); 1 callers outside tests: `svc/svc.go:30`; 0 test references")
	wantContains(t, string(mustRead(t, filepath.Join(dir, "questions", "INDEX.md"))), "| go facts |", "| yes |")
	wantContains(t, res.summary(), "symbol facts 2")

	// A lowercase first part is a package: only definitions with that actual
	// package clause count.
	pkgDir, _, _, _ := enrichFixture(t, []gateQuestion{{Package: "api", Question: "Does pay.Build set the field?", Decisive: true}})
	if _, err := enrichGate(pkgDir, code, src, "spec.md", sp); err != nil {
		t.Fatal(err)
	}
	pkgCard := string(mustRead(t, filepath.Join(pkgDir, "questions", "q1-api.md")))
	wantContains(t, pkgCard, "`pay.Build` (go Function) is defined at `pay/b.go:3`; 1 callers outside tests: `svc/svc.go:40`; 0 test references")
	if strings.Contains(pkgCard, "other/b.go") {
		t.Errorf("a definition in another package leaked:\n%s", pkgCard)
	}

	// A rerun is served from the cache: gopls is not called again.
	before := string(mustRead(t, calls))
	if _, err := enrichGate(dir, code, src, "spec.md", sp); err != nil {
		t.Fatal(err)
	}
	if after := string(mustRead(t, calls)); after != before {
		t.Errorf("a rerun called gopls again:\n%s", after)
	}
	if n, m := strings.Count(before, "workspace_symbol StartCheckout"), strings.Count(before, "workspace_symbol NoSuchThing"); n < 1 || n > 2 || m != 1 {
		t.Errorf("workspace_symbol calls: StartCheckout %d (once, or twice when two symbols race), NoSuchThing %d (once)", n, m)
	}
}

func TestGoplsMissingOrSlowLeavesValidCardsAndANote(t *testing.T) {
	qs := []gateQuestion{{Package: "api", Question: "Does Gateway.StartCheckout retry?", Decisive: true}}
	check := func(t *testing.T, want string) {
		t.Helper()
		dir, code, src, sp := enrichFixture(t, qs)
		res, err := enrichGate(dir, code, src, "spec.md", sp)
		if err != nil {
			t.Fatal(err)
		}
		if res.SymbolFacts != 0 || !strings.Contains(strings.Join(res.Notes, "|"), want) {
			t.Fatalf("facts %d notes %v, want a note with %q", res.SymbolFacts, res.Notes, want)
		}
		wantContains(t, string(mustRead(t, filepath.Join(dir, "questions", "INDEX.md"))), "Note: ")
		if c, err := parseCard(string(mustRead(t, filepath.Join(dir, "questions", "q1-api.md")))); err != nil || c.question == "" {
			t.Fatalf("card invalid: %v", err)
		}
	}
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	t.Run("missing", func(t *testing.T) {
		t.Setenv("ORCH_GOPLS", filepath.Join(t.TempDir(), "absent"))
		check(t, "was not found")
	})
	t.Run("timeout", func(t *testing.T) {
		_, _, bin := goplsFixture(t)
		os.WriteFile(filepath.Join(filepath.Dir(bin), "sleep"), nil, 0o644)
		t.Setenv("ORCH_GOPLS", bin)
		old := goplsLimit
		goplsLimit = 150 * time.Millisecond
		t.Cleanup(func() { goplsLimit = old })
		check(t, "timed out")
	})
}
