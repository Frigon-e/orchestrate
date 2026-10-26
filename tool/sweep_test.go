package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEvidenceReadersRejectSpecialFiles(t *testing.T) {
	for name, read := range map[string]func(string) error{
		"adjudication": func(repo string) error {
			_, err := adjudicationSource(repo, evidenceRecord{Path: "source"})
			return err
		},
		"sweep": func(repo string) error {
			_, err := sweepEvidenceBytes(repo, nil, evidenceRecord{ID: "source", Path: "source"}, "", false)
			return err
		},
		"pointer": func(repo string) error {
			_, err := checkPointer(repo, "source")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			path := filepath.Join(repo, "source")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- read(repo) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "not a regular file") {
					t.Fatalf("special-file evidence must be rejected: %v", err)
				}
			case <-time.After(time.Second):
				// Release a regressed FIFO reader before failing the test.
				if fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
					syscall.Close(fd)
				}
				t.Fatal("evidence reader blocked on a FIFO")
			}
		})
	}
}

func sweepCard(t *testing.T, topic, key, area string, options ...string) {
	t.Helper()
	dir := filepath.Join(topic, "questions")
	os.MkdirAll(dir, 0o755)
	body := "---\nquestion: Which way for " + key + "?\n"
	if area != "" {
		body += "area: " + area + "\n"
	}
	if len(options) > 0 {
		body += "recommend: " + options[0] + "\naction: Record the owner's chosen fixture path.\n"
	}
	body += "---\n## Context\nWe must choose.\nThe first option is the baseline recommendation, not an authorized decision.\n"
	if len(options) > 0 {
		body += "\n## Options\n"
		for _, o := range options {
			body += "### " + o + ": option " + o + "\nGain: x\nCost: y\n\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, key+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var manifest sweepManifest
	path := filepath.Join(topic, "manifest.json")
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &manifest); err != nil {
			t.Fatal(err)
		}
	} else {
		manifest = sweepManifest{SchemaVersion: 1, Questions: []sweepSourceQuestion{}}
	}
	found := false
	for _, q := range manifest.Questions {
		found = found || q.ID == key
	}
	if !found {
		manifest.Questions = append(manifest.Questions, sweepSourceQuestion{ID: key, Question: "Which way for " + key + "?", Card: key})
	}
	sweepJSON(t, path, manifest)
	if _, err := os.Stat(filepath.Join(topic, "dropped.json")); os.IsNotExist(err) {
		sweepJSON(t, filepath.Join(topic, "dropped.json"), []sweepDropped{})
	}
}

func sweepJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sweepExtraSources(t *testing.T, topic string, questions ...string) {
	t.Helper()
	var manifest sweepManifest
	path := filepath.Join(topic, "manifest.json")
	if err := json.Unmarshal(mustRead(t, path), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, q := range questions {
		manifest.Questions = append(manifest.Questions, sweepSourceQuestion{ID: q, Question: q})
	}
	sweepJSON(t, path, manifest)
}

func sweepPassFile(t *testing.T, topic, pass, key, raw string) {
	t.Helper()
	dir := filepath.Join(topic, "passes", pass)
	os.MkdirAll(dir, 0o755)
	var fixture struct {
		sweepAnswer
		WithSupport bool `json:"fixture_support"`
	}
	if err := json.Unmarshal([]byte(raw), &fixture); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(filepath.Dir(topic), "repo")
	card := mustRead(t, filepath.Join(topic, "questions", key+".md"))
	subject, err := captureGateSubject(repo, card)
	if err != nil {
		t.Fatal(err)
	}
	fixture.CardRev, fixture.Subject = subjectDigest(card), subject
	if fixture.WithSupport {
		fixture.Support = &claimSupport{SchemaVersion: 1, State: claimSupported, Assessor: pass, Method: "model", Rationale: "The quoted behavior supports this option.", FailureScenario: "Choosing the other option would leave the documented behavior unavailable."}
		for i, pointer := range fixture.Evidence {
			e := evidenceRecord{SchemaVersion: 1, ID: fmt.Sprintf("ev-%d", i), SubjectID: subject.ID, SourceKind: evidenceSourceDocument, LookupID: "fixture-lookup"}
			valid := true
			e.LocatorValid = &valid
			var content []byte
			if strings.HasPrefix(pointer, "https://") {
				e.Path, content = pointer, []byte("Quoted documentation.")
			} else {
				m := pointerRe.FindStringSubmatch(pointer)
				if m == nil || m[4] != "" {
					continue
				}
				e.Path = m[1]
				start, _ := strconv.Atoi(m[2])
				end := start
				if m[3] != "" {
					end, _ = strconv.Atoi(m[3])
				}
				e.Range = &evidenceRange{StartLine: start, EndLine: end}
				path := filepath.Join(repo, e.Path)
				if strings.HasPrefix(e.Path, "polar/") {
					path = filepath.Join(filepath.Dir(topic), e.Path)
				}
				b := mustRead(t, path)
				lines := bytes.SplitAfter(b, []byte("\n"))
				if start < 1 || end > len(lines) {
					continue
				}
				content = bytes.Join(lines[start-1:end], nil)
			}
			e.ContentHash = subjectDigest(content)
			fixture.EvidenceRecords = append(fixture.EvidenceRecords, e)
			fixture.Support.EvidenceIDs = append(fixture.Support.EvidenceIDs, e.ID)
			if fixture.Support.Excerpt == "" {
				fixture.Support.Excerpt = strings.TrimSuffix(string(content), "\n")
			}
		}
	}
	sweepJSON(t, filepath.Join(dir, key+".json"), fixture.sweepAnswer)
}

func ans(choice string, evidence ...string) string {
	ev := `"` + strings.Join(evidence, `","`) + `"`
	if len(evidence) == 0 {
		ev = ""
	}
	return `{"fixture_support":true,"choice":"` + choice + `","confidence":0.8,"reason":"because","evidence":[` + ev + `]}`
}

func TestSweepSettlesOnlyWhatIsProved(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte("human_only = [\"Billing\"]\n"), 0o644)
	os.MkdirAll(filepath.Join(h.repo, "adr"), 0o755)
	os.WriteFile(filepath.Join(h.repo, "adr", "1.md"), []byte("a\nb\nc\n"), 0o644)
	polar := filepath.Join(h.tmp, "polar")
	os.MkdirAll(filepath.Join(polar, "src"), 0o755)
	os.WriteFile(filepath.Join(polar, "src", "a.ts"), []byte("x\ny\n"), 0o644)
	topic := filepath.Join(h.tmp, "topic")

	for _, k := range []string{"agree", "differ", "fake", "short", "bad-choice", "url-only", "outside", "owned"} {
		sweepCard(t, topic, k, "", "a", "b")
	}
	sweepCard(t, topic, "money", "billing", "a", "b")
	sweepCard(t, topic, "free", "")
	for _, p := range []string{"flash", "sonnet"} {
		sweepPassFile(t, topic, p, "agree", ans("a", "adr/1.md:2"))
		sweepPassFile(t, topic, p, "money", ans("a", "adr/1.md:2"))
		sweepPassFile(t, topic, p, "free", ans("", "adr/1.md:2"))
		sweepPassFile(t, topic, p, "url-only", ans("a", "https://stripe.com/docs"))
		sweepPassFile(t, topic, p, "outside", ans("a", "polar/src/a.ts:2"))
	}
	sweepPassFile(t, topic, "flash", "differ", ans("a", "adr/1.md:1"))
	sweepPassFile(t, topic, "sonnet", "differ", ans("b", "adr/1.md:3"))
	sweepPassFile(t, topic, "flash", "fake", ans("a", "adr/1.md:2"))
	sweepPassFile(t, topic, "sonnet", "fake", ans("a", "adr/1.md:99")) // line does not exist
	sweepPassFile(t, topic, "flash", "short", ans("a", "adr/1.md:2"))  // only one pass answered
	sweepPassFile(t, topic, "flash", "bad-choice", ans("zzz", "adr/1.md:2"))
	sweepPassFile(t, topic, "sonnet", "bad-choice", ans("a", "adr/1.md:2"))
	sweepPassFile(t, topic, "flash", "owned", ans("a", "adr/1.md:2"))
	sweepPassFile(t, topic, "sonnet", "owned", `{"choice":"a","confidence":0.9,"reason":"tax","evidence":["adr/1.md:2"],"owner":"accountant"}`)
	os.WriteFile(filepath.Join(topic, "dropped.json"), []byte(`[
{"source_id":"same as agree","question":"same as agree","reason":"duplicate","duplicate_of":"agree"},
{"source_id":"ghost","question":"ghost","reason":"duplicate","duplicate_of":"nope"},
{"source_id":"vanished","question":"vanished","reason":"gone"},
{"source_id":"decided","question":"decided","reason":"adr","answered_by":"adr/1.md:99"}]`), 0o644)
	sweepExtraSources(t, topic, "same as agree", "ghost", "vanished", "decided")

	code, out := h.run("sweep", topic, "--roots", "polar="+polar)
	if code == 0 {
		t.Fatal("invalid dropped entries must fail the command")
	}
	section := func(title string) string {
		i := strings.Index(out, title)
		if i < 0 {
			t.Fatalf("no %q section:\n%s", title, out)
		}
		rest := out[i:]
		if j := strings.Index(rest[len(title):], "\n]:"); j >= 0 {
			rest = rest[:len(title)+j]
		}
		lines := strings.Split(rest, "\n")
		end := 1
		for end < len(lines) && strings.HasPrefix(lines[end], "  ") {
			end++
		}
		return strings.Join(lines[:end], "\n")
	}
	check := func(title string, want, notWant []string) {
		t.Helper()
		sec := section(title)
		for _, w := range want {
			if !strings.Contains(sec, w) {
				t.Errorf("%s lacks %q:\n%s", title, w, sec)
			}
		}
		for _, w := range notWant {
			if strings.Contains(sec, w) {
				t.Errorf("%s wrongly holds %q:\n%s", title, w, sec)
			}
		}
	}
	check("settled: the passes agree", []string{"agree -> a", "outside -> a"}, []string{"fake", "url-only", "money"})
	check("disagree:", []string{"differ (flash=a, sonnet=b)"}, nil)
	check("recheck:", []string{"fake", "does not resolve", "short", "1 of 2 passes", "bad-choice", "not an option", "url-only", "--accept-urls"}, []string{"differ", "agree"})
	check("user:", []string{"money (area billing is human-only)", "free", "owned (pass sonnet says the owner is accountant)"}, []string{"agree ->"})
	wantContains(t, out, "dropped: \"ghost\" is marked a duplicate of \"nope\"", "dropped: \"vanished\" was dropped with no duplicate_of", "dropped: \"decided\" says adr/1.md:99 answers it")
	if strings.Contains(out, "same as agree") {
		t.Errorf("a valid dropped entry was flagged:\n%s", out)
	}

	// Cards for the user carry what the passes said, and only those cards.
	fu := filepath.Join(topic, "sweep", "for-user")
	got := string(mustRead(t, filepath.Join(fu, "owned.md")))
	wantContains(t, got, "## Passes", "Why this is yours: pass sonnet says the owner is accountant", "**flash** chose `a`", "Evidence: adr/1.md:2")
	if _, err := os.Stat(filepath.Join(fu, "agree.md")); err == nil {
		t.Error("a settled question was sent to the user")
	}
	askOut := h.must("ask", "--cards", fu)
	// The option-less free-text card stays for the owner, but D2 withholds it
	// until research supplies decision-ready tradeoffs. The two ready cards render.
	wantContains(t, askOut, "questions: 2", "not_ready: 1", "free,missing_tradeoffs")
	if strings.Contains(askOut, "warnings[") {
		t.Errorf("a for-user card did not render:\n%s", askOut)
	}
	wantContains(t, string(mustRead(t, filepath.Join(topic, "sweep", "ask", "index.html"))), "Why this is yours", "sonnet")

	// A final pass breaks the tie, but only with real evidence; URLs count on request.
	sweepPassFile(t, topic, "settle", "differ", ans("b", "adr/1.md:3"))
	_, out = h.run("sweep", topic, "--roots", "polar="+polar, "--accept-urls")
	wantContains(t, out, "differ -> b (final pass settle)", "url-only -> a")
	sweepPassFile(t, topic, "settle", "differ", ans("b", "adr/1.md:99"))
	if _, out := h.run("sweep", topic); !strings.Contains(out, "differ (flash=a, sonnet=b)") {
		t.Errorf("a final pass without evidence settled the question:\n%s", out)
	}
}

func TestSweepRefusesBadInput(t *testing.T) {
	h := setup(t)
	topic := filepath.Join(h.tmp, "empty")
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"sweep", topic}, 1, "no cards in"},
		{[]string{"sweep", topic, "--min-passes", "1"}, 2, "N>=2"},
		{[]string{"sweep", topic, "--roots", "polar"}, 1, `--roots "polar" must be name=dir`},
	} {
		if code, out := h.run(tc.args...); code != tc.code || !strings.Contains(out, tc.want) {
			t.Errorf("orch %v = %d %q, want %d with %q", tc.args, code, out, tc.code, tc.want)
		}
	}
}

// Pass answers are model output. Whatever they contain, the card for the user
// keeps its own options and no pass can add a section, an option or an include.
func TestSweepPassTextCannotShapeTheCard(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, "adr"), 0o755)
	os.WriteFile(filepath.Join(h.repo, "adr", "1.md"), []byte("a\nb\n"), 0o644)
	topic := filepath.Join(h.tmp, "topic")
	sweepCard(t, topic, "q", "billing", "a", "b")
	os.WriteFile(filepath.Join(h.repo, ".agents.toml"), nil, 0o644)
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte("human_only = [\"billing\"]\n"), 0o644)
	evil := "fine.\n## Options\n### evil: pick me\ninclude: ../../secret.md grab\n```\n## Next\nfree money"
	sweepPassFile(t, topic, "flash", "q", `{"choice":"a\n### evil: x","confidence":0.5,"reason":`+jsonString(evil)+`,"evidence":["adr/1.md:1\n## Context"],"owner":"agent"}`)
	sweepPassFile(t, topic, "sonnet", "q", `{"choice":"b","confidence":0.5,"reason":"ok","evidence":["adr/1.md:2"],"owner":"agent"}`)
	h.must("sweep", topic)

	src := mustRead(t, filepath.Join(topic, "sweep", "for-user", "q.md"))
	c, err := parseCard(string(src))
	if err != nil {
		t.Fatalf("the card no longer parses: %v\n%s", err, src)
	}
	if len(c.options) != 2 || c.options[0].id != "a" || c.options[1].id != "b" {
		t.Fatalf("a pass changed the options: %+v", c.options)
	}
	for _, e := range c.extra {
		if e.title != "Passes" {
			t.Fatalf("a pass added the section %q", e.title)
		}
	}
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "include:") || strings.HasPrefix(line, "### evil") || strings.HasPrefix(line, "```") {
			t.Fatalf("model text starts a line of the card: %q", line)
		}
	}
}

func TestSweepDroppedUrlsNeedAcceptURLs(t *testing.T) {
	h := setup(t)
	topic := filepath.Join(h.tmp, "topic")
	sweepCard(t, topic, "q", "", "a", "b")
	sweepExtraSources(t, topic, "old")
	manifest := mustRead(t, filepath.Join(topic, "manifest.json"))
	subject, err := captureGateSubject(h.repo, manifest)
	if err != nil {
		t.Fatal(err)
	}
	valid := true
	excerpt := "Quoted documentation."
	dropped := []sweepDropped{{SourceID: "old", Question: "old", Reason: "docs say so", AnsweredBy: "https://example.com/docs", Subject: subject,
		EvidenceRecords: []evidenceRecord{{SchemaVersion: 1, ID: "doc", SubjectID: subject.ID, SourceKind: evidenceSourceDocument, Path: "https://example.com/docs", LookupID: "docs", ContentHash: subjectDigest([]byte(excerpt)), LocatorValid: &valid}},
		Support:         &claimSupport{SchemaVersion: 1, State: claimSupported, Assessor: "reviewer", Method: "model", EvidenceIDs: []string{"doc"}, Rationale: "Documentation explicitly answers the question.", Excerpt: excerpt}}}
	sweepJSON(t, filepath.Join(topic, "dropped.json"), dropped)
	code, out := h.run("sweep", topic)
	if code == 0 {
		t.Fatal("a URL without opt-in must fail accounting")
	}
	wantContains(t, out, "dropped: \"old\" says https://example.com/docs answers it")
	if out := h.must("sweep", topic, "--accept-urls"); strings.Contains(out, "dropped:") {
		t.Fatalf("--accept-urls did not count the explicitly assessed link:\n%s", out)
	}
	// Opt-in never turns a resolving URL alone into claim support.
	dropped[0].Support = nil
	sweepJSON(t, filepath.Join(topic, "dropped.json"), dropped)
	if code, out := h.run("sweep", topic, "--accept-urls"); code == 0 {
		t.Fatalf("an unassessed link justified dropping a question:\n%s", out)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func sweepReadAnswer(t *testing.T, topic, pass, key string) sweepAnswer {
	t.Helper()
	var answer sweepAnswer
	if err := json.Unmarshal(mustRead(t, filepath.Join(topic, "passes", pass, key+".json")), &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

func TestSweepCardRevisionInvalidatesSameKey(t *testing.T) {
	for _, change := range []struct{ name, from, to string }{
		{"question", "Which way for q?", "Which behavior for q?"},
		{"option label", "a: option a", "a: a different design"},
		{"option meaning", "Gain: x", "Gain: a different guarantee"},
		{"area", "area: infra", "area: storage"},
		{"context", "We must choose.", "Our requirements changed."},
	} {
		t.Run(change.name, func(t *testing.T) {
			h := setup(t)
			os.WriteFile(filepath.Join(h.repo, "decision.md"), []byte("Use a.\n"), 0o644)
			topic := filepath.Join(h.tmp, "topic")
			sweepCard(t, topic, "q", "infra", "a", "b")
			for _, pass := range []string{"flash", "sonnet"} {
				sweepPassFile(t, topic, pass, "q", ans("a", "decision.md:1"))
			}
			wantContains(t, h.must("sweep", topic), "q -> a")
			path := filepath.Join(topic, "questions", "q.md")
			src := mustRead(t, path)
			changed := bytes.ReplaceAll(src, []byte(change.from), []byte(change.to))
			if bytes.Equal(src, changed) {
				t.Fatalf("fixture has no mutation anchor %q", change.from)
			}
			os.WriteFile(path, changed, 0o644)
			out := h.must("sweep", topic)
			wantContains(t, out, "recheck:", "card_rev or code subject changed")
			if strings.Contains(out, "q -> a") {
				t.Fatalf("an old answer settled changed options under the same key:\n%s", out)
			}
		})
	}
}

func TestSweepCodeStateInvalidatesAnswers(t *testing.T) {
	h := setup(t)
	os.WriteFile(filepath.Join(h.repo, "decision.md"), []byte("Use a.\n"), 0o644)
	topic := filepath.Join(h.tmp, "topic")
	sweepCard(t, topic, "q", "", "a", "b")
	for _, pass := range []string{"flash", "sonnet"} {
		sweepPassFile(t, topic, pass, "q", ans("a", "decision.md:1"))
	}
	wantContains(t, h.must("sweep", topic), "q -> a")
	// The citation is unchanged. Any change elsewhere in the A2 subject invalidates.
	os.WriteFile(filepath.Join(h.repo, "new-code.go"), []byte("package example\n"), 0o644)
	out := h.must("sweep", topic)
	wantContains(t, out, "recheck:", "code subject changed")
	if strings.Contains(out, "q -> a") {
		t.Fatalf("old answers survived a code-state change:\n%s", out)
	}
}

func TestSweepExternalEvidenceChangeInvalidatesSupport(t *testing.T) {
	h := setup(t)
	polar := filepath.Join(h.tmp, "polar")
	os.MkdirAll(polar, 0o755)
	path := filepath.Join(polar, "decision.md")
	os.WriteFile(path, []byte("Use a.\n"), 0o644)
	topic := filepath.Join(h.tmp, "topic")
	sweepCard(t, topic, "q", "", "a", "b")
	for _, pass := range []string{"flash", "sonnet"} {
		sweepPassFile(t, topic, pass, "q", ans("a", "polar/decision.md:1"))
	}
	wantContains(t, h.must("sweep", topic, "--roots", "polar="+polar), "q -> a")
	os.WriteFile(path, []byte("Use b.\n"), 0o644)
	out := h.must("sweep", topic, "--roots", "polar="+polar)
	wantContains(t, out, "recheck:", "content changed")
	if strings.Contains(out, "q -> a") {
		t.Fatal("a still-resolving external pointer hid changed evidence")
	}
}

func TestSweepSupportAssessmentIsRequired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*sweepAnswer)
		want   string
	}{
		{"citation only", func(a *sweepAnswer) { a.Support = nil }, "no claim_support assessment"},
		{"unsupported", func(a *sweepAnswer) { a.Support.State = claimUnsupported }, "claim_support is unsupported"},
		{"conflicting", func(a *sweepAnswer) { a.Support.State = claimSupportConflicting }, "claim_support is conflicting"},
		{"unknown", func(a *sweepAnswer) { a.Support.State = claimSupportUnknown }, "claim_support is unknown"},
		{"assessor", func(a *sweepAnswer) { a.Support.Assessor = "" }, "requires assessor"},
		{"method", func(a *sweepAnswer) { a.Support.Method = "confidence" }, "assessment method"},
		{"rationale", func(a *sweepAnswer) { a.Support.Rationale = "" }, "requires assessor and rationale"},
		{"excerpt", func(a *sweepAnswer) { a.Support.Excerpt = "" }, "quoted excerpt"},
		{"fabricated excerpt", func(a *sweepAnswer) { a.Support.Excerpt = "It says the opposite." }, "excerpt is not in"},
		{"scenario", func(a *sweepAnswer) {
			a.EvidenceRecords[0].SourceKind = evidenceSourceCode
			a.Support.FailureScenario = ""
		}, "concrete failure scenario"},
		{"evidence ids", func(a *sweepAnswer) { a.Support.EvidenceIDs = []string{"missing"} }, "unknown evidence ID"},
		{"content hash", func(a *sweepAnswer) { a.EvidenceRecords[0].ContentHash = "old" }, "content changed"},
		{"evidence subject", func(a *sweepAnswer) { a.EvidenceRecords[0].SubjectID = "old" }, "stale or incomplete"},
		{"locator", func(a *sweepAnswer) { a.EvidenceRecords[0].LocatorValid = nil }, "valid locator observation"},
		{"range", func(a *sweepAnswer) { a.EvidenceRecords[0].Range.EndLine = 99 }, "range does not resolve"},
		{"legacy answer", func(a *sweepAnswer) { a.CardRev = ""; a.Subject = nil }, "changed or is unbound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			os.WriteFile(filepath.Join(h.repo, "decision.md"), []byte("Use a.\n"), 0o644)
			topic := filepath.Join(h.tmp, "topic")
			sweepCard(t, topic, "q", "", "a", "b")
			for _, pass := range []string{"flash", "sonnet"} {
				sweepPassFile(t, topic, pass, "q", ans("a", "decision.md:1"))
				answer := sweepReadAnswer(t, topic, pass, "q")
				answer.Confidence = 1 // A score cannot replace support.
				tc.mutate(&answer)
				sweepJSON(t, filepath.Join(topic, "passes", pass, "q.json"), answer)
			}
			out := h.must("sweep", topic)
			wantContains(t, out, "recheck:", tc.want)
			if strings.Contains(out, "q -> a") {
				t.Fatalf("invalid support settled the card:\n%s", out)
			}
			var record struct {
				Questions map[string]sweepResolution `json:"questions"`
			}
			if err := json.Unmarshal(mustRead(t, filepath.Join(topic, "sweep", "results.json")), &record); err != nil {
				t.Fatal(err)
			}
			if tc.name == "citation only" && (!record.Questions["q"].Passes[0].CitationValid || record.Questions["q"].Passes[0].ClaimSupport != nil) {
				t.Fatalf("citation validity and claim support were conflated: %+v", record)
			}
		})
	}
}

func TestSweepSupplementalURLDoesNotBlockLocalSupport(t *testing.T) {
	h := setup(t)
	os.WriteFile(filepath.Join(h.repo, "decision.md"), []byte("Use a.\n"), 0o644)
	topic := filepath.Join(h.tmp, "topic")
	sweepCard(t, topic, "q", "", "a", "b")
	for _, pass := range []string{"flash", "sonnet"} {
		sweepPassFile(t, topic, pass, "q", ans("a", "decision.md:1"))
		answer := sweepReadAnswer(t, topic, pass, "q")
		// As before, an optional link does not negate local evidence. The link
		// is not being used as support and is not a fetched/verified source.
		answer.Evidence = append(answer.Evidence, "https://example.com/background")
		sweepJSON(t, filepath.Join(topic, "passes", pass, "q.json"), answer)
	}
	wantContains(t, h.must("sweep", topic), "q -> a")
}

func TestSweepHumanOnlyWinsOverStaleAgreement(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte("human_only = [\"billing\"]\n"), 0o644)
	os.WriteFile(filepath.Join(h.repo, "decision.md"), []byte("Use a.\n"), 0o644)
	topic := filepath.Join(h.tmp, "topic")
	sweepCard(t, topic, "q", "billing", "a", "b")
	for _, pass := range []string{"flash", "sonnet"} {
		sweepPassFile(t, topic, pass, "q", ans("a", "decision.md:1"))
	}
	os.WriteFile(filepath.Join(h.repo, "other-code.go"), []byte("package example\n"), 0o644)
	out := h.must("sweep", topic)
	wantContains(t, out, "q (area billing is human-only)")
	if strings.Contains(out, "q -> a") {
		t.Fatal("stale model agreement settled a human-only question")
	}
	card := string(mustRead(t, filepath.Join(topic, "sweep", "for-user", "q.md")))
	wantContains(t, card, "Current binding: false", "Unresolved claim support:")
}

func TestSweepStaleFinalCannotSettleOrOverrideAgreement(t *testing.T) {
	h := setup(t)
	os.WriteFile(filepath.Join(h.repo, "decision.md"), []byte("Use a.\n"), 0o644)
	topic := filepath.Join(h.tmp, "topic")
	sweepCard(t, topic, "q", "", "a", "b")
	for _, pass := range []string{"flash", "sonnet", "settle"} {
		sweepPassFile(t, topic, pass, "q", ans("a", "decision.md:1"))
	}
	answer := sweepReadAnswer(t, topic, "settle", "q")
	answer.CardRev = "old"
	sweepJSON(t, filepath.Join(topic, "passes", "settle", "q.json"), answer)
	out := h.must("sweep", topic)
	wantContains(t, out, "recheck:", "settle: card_rev")
	if strings.Contains(out, "q -> a") {
		t.Fatal("agreement bypassed a stale final answer")
	}
}

func TestSweepAccountingErrorsFailCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, topic string)
		want   string
	}{
		{"missing", func(t *testing.T, p string) { os.Remove(filepath.Join(p, "dropped.json")) }, "dropped.json is missing or unreadable"},
		{"unreadable", func(t *testing.T, p string) {
			os.Remove(filepath.Join(p, "dropped.json"))
			os.Mkdir(filepath.Join(p, "dropped.json"), 0o755)
		}, "dropped.json is missing or unreadable"},
		{"bad JSON", func(t *testing.T, p string) { os.WriteFile(filepath.Join(p, "dropped.json"), []byte("["), 0o644) }, "not a JSON list"},
		{"null", func(t *testing.T, p string) { os.WriteFile(filepath.Join(p, "dropped.json"), []byte("null"), 0o644) }, "not a JSON list"},
		{"empty entry", func(t *testing.T, p string) { sweepJSON(t, filepath.Join(p, "dropped.json"), []sweepDropped{{}}) }, "matching source_id/question"},
		{"unaccounted", func(t *testing.T, p string) { sweepExtraSources(t, p, "lost") }, "source \"lost\" is unaccounted"},
		{"missing manifest", func(t *testing.T, p string) { os.Remove(filepath.Join(p, "manifest.json")) }, "manifest.json is missing or unreadable"},
		{"invalid manifest", func(t *testing.T, p string) { os.WriteFile(filepath.Join(p, "manifest.json"), []byte("{}"), 0o644) }, "schema_version 1"},
		{"empty reason", func(t *testing.T, p string) {
			sweepExtraSources(t, p, "old")
			sweepJSON(t, filepath.Join(p, "dropped.json"), []sweepDropped{{SourceID: "old", Question: "old", Disposition: "out of scope"}})
		}, "nonempty reason"},
		{"ambiguous entry", func(t *testing.T, p string) {
			sweepExtraSources(t, p, "old")
			sweepJSON(t, filepath.Join(p, "dropped.json"), []sweepDropped{{SourceID: "old", Question: "old", Reason: "old", DuplicateOf: "q", Disposition: "out of scope"}})
		}, "exactly one"},
		{"duplicate accounting", func(t *testing.T, p string) {
			sweepJSON(t, filepath.Join(p, "dropped.json"), []sweepDropped{{SourceID: "q", Question: "Which way for q?", Reason: "already mapped", DuplicateOf: "q"}})
		}, "more than once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			topic := filepath.Join(h.tmp, "topic")
			sweepCard(t, topic, "q", "", "a", "b")
			tc.change(t, topic)
			code, out := h.run("sweep", topic)
			if code == 0 {
				t.Fatalf("bad accounting succeeded:\n%s", out)
			}
			wantContains(t, out, tc.want, "sweep accounting failed")
		})
	}
}

func TestSweepRecordsEverySourceOutcome(t *testing.T) {
	h := setup(t)
	os.WriteFile(filepath.Join(h.repo, "decision.md"), []byte("Use a.\n"), 0o644)
	topic := filepath.Join(h.tmp, "topic")
	sweepCard(t, topic, "q", "", "a", "b")
	sweepCard(t, topic, "open", "", "a", "b")
	sweepExtraSources(t, topic, "duplicate", "resolved", "out of scope")
	for _, pass := range []string{"flash", "sonnet"} {
		sweepPassFile(t, topic, pass, "q", ans("a", "decision.md:1"))
	}
	// First comparison publishes bindings, even when accounting is incomplete.
	if code, _ := h.run("sweep", topic); code == 0 {
		t.Fatal("unaccounted sources succeeded")
	}
	var published struct {
		ManifestSubject *gateSubject `json:"manifest_subject"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(topic, "sweep", "results.json")), &published); err != nil || !published.ManifestSubject.valid() {
		t.Fatalf("no valid manifest binding: %v", err)
	}
	answer := sweepReadAnswer(t, topic, "flash", "q")
	for i := range answer.EvidenceRecords {
		answer.EvidenceRecords[i].SubjectID = published.ManifestSubject.ID
	}
	dropped := []sweepDropped{
		{SourceID: "duplicate", Question: "duplicate", Reason: "Same decision and options as q.", DuplicateOf: "q"},
		{SourceID: "resolved", Question: "resolved", Reason: "The current ADR records the answer.", AnsweredBy: "decision.md:1", Subject: published.ManifestSubject, Support: answer.Support, EvidenceRecords: answer.EvidenceRecords},
		{SourceID: "out of scope", Question: "out of scope", Reason: "The abandoned product is not shipped.", Disposition: "not applicable"},
	}
	sweepJSON(t, filepath.Join(topic, "dropped.json"), dropped)
	wantContains(t, h.must("sweep", topic), "q -> a", "open (0 of 2 passes answered)")
	var record struct {
		AccountingOK bool                       `json:"accounting_ok"`
		Questions    map[string]sweepResolution `json:"questions"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(topic, "sweep", "results.json")), &record); err != nil {
		t.Fatal(err)
	}
	if !record.AccountingOK || record.Questions["q"].Choice != "a" || record.Questions["open"].Status != "recheck" {
		t.Fatalf("source outcomes were not recorded: %+v", record)
	}
	// A valid pointer cannot support a different answered_by resolution.
	dropped[1].AnsweredBy = "decision.md:1-1"
	sweepJSON(t, filepath.Join(topic, "dropped.json"), dropped)
	if code, out := h.run("sweep", topic); code == 0 || !strings.Contains(out, "not cited by its support assessment") {
		t.Fatalf("unrelated support was attached to a resolution:\n%s", out)
	}
}

// One resolving citation must not hide an unresolved one in the same pass.
func TestSweepMixedEvidenceNeedsRecheck(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, "adr"), 0o755)
	os.WriteFile(filepath.Join(h.repo, "adr", "1.md"), []byte("a\nb\nc\n"), 0o644)
	topic := filepath.Join(h.tmp, "topic")
	for _, k := range []string{"mixed", "clean"} {
		sweepCard(t, topic, k, "", "a", "b")
	}
	for _, p := range []string{"flash", "sonnet"} {
		sweepPassFile(t, topic, p, "mixed", ans("a", "adr/1.md:2", "adr/1.md:99"))
		sweepPassFile(t, topic, p, "clean", ans("a", "adr/1.md:2", "adr/1.md:3"))
	}
	out := h.must("sweep", topic)
	wantContains(t, out, "clean -> a", "mixed", "does not resolve")
	if strings.Contains(out, "mixed -> a") {
		t.Errorf("a pass with an unresolved citation settled the question:\n%s", out)
	}
}
