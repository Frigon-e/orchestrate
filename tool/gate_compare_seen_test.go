package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestKeyViewSeparatesWhatTheJudgesSawFromHowTheyAgreed(t *testing.T) {
	yes, no := true, false
	a := &armData{
		items:         []gateItem{{ID: "p#one", Package: "p"}, {ID: "p#two", Package: "p", Reuse: []string{"src/view.ts#render"}}, {ID: "p#three", Package: "p"}, {ID: "p#four", Package: "p"}},
		checklist:     map[string][]itemCands{"p": {{Item: gateItem{ID: "p#one"}, Cands: []reuseCand{{Path: "src/store.py", Start: 12, End: 15}}}}},
		manifestKnown: true,
		evidence: []evidenceRecord{
			{SourceKind: evidenceSourceCode, Path: "src/store.py", Range: &evidenceRange{StartLine: 10, EndLine: 20}, Rendered: &yes, LocatorValid: &yes},
			{SourceKind: evidenceSourceCode, Path: "src/view.ts", Range: &evidenceRange{StartLine: 5, EndLine: 9}, Rendered: &no, LocatorValid: &yes},
		},
		answers: map[string]map[string]scoredAnswer{"group": {
			"p#one":   {Verdict: verdictReuse, Candidate: "src/store.py:12-15", Present: true, Valid: true, Supported: true},
			"p#two":   {Verdict: verdictExtend, Candidate: "src/view.ts:5-9", Present: true, Valid: true, Supported: true},
			"p#three": {Verdict: verdictNew, Present: true, Valid: true}, // unsupported answer remains in coverage
		}},
	}
	ref := map[string]refEntry{
		"p#one":   {Verdict: "duplicate", Candidate: "src/store.py:10-20"},
		"p#two":   {Verdict: "extend", Candidate: "src/view.ts:5-9"},
		"p#three": {Verdict: "new"},
		"p#four":  {Verdict: "unclear"}, // missing answer remains in coverage
	}
	v := keyViewOf(a, ref)
	if got := v.Seen["duplicate"]; got != (seenCount{Total: 1, Seen: 1, Retrieved: 1}) {
		t.Errorf("rendered vs retrieved: %+v", got)
	}
	if got := v.Seen["variation_site"]; got != (seenCount{Total: 1, Hint: 1}) {
		t.Errorf("hint is not a body: %+v", got)
	}
	got := v.Agree["group"]
	if got.Items != 4 || got.Answered != 3 || got.Supported != 2 || got.Both != 2 || got.Agree != 2 || got.Unsupported != 1 || got.Missing != 1 || got.SeenBoth != 1 || got.UnseenBoth != 1 {
		t.Fatalf("coverage and conditional accuracy: %+v", got)
	}
	var out bytes.Buffer
	printKeyView(&out, []*armData{a}, ref)
	for _, part := range []string{"rendered 0/1 targets; retrieved 0/1; author hint 1/1", "answer coverage 3/4 (75%)", "conditional accuracy 2/2 (100%)", "missing 1, invalid 0, unsupported 1"} {
		if !strings.Contains(out.String(), part) {
			t.Errorf("missing %q: %s", part, out.String())
		}
	}
	// No manifest, even with a checklist, does not establish delivery.
	a.manifestKnown = false
	if c := keyViewOf(a, ref).Seen["duplicate"]; c.Seen != 0 || c.Unknown != 1 {
		t.Fatalf("fabricated visibility: %+v", c)
	}
}

func TestSeenRequiresEntireRenderedSpanAcrossLanguages(t *testing.T) {
	yes := true
	for _, path := range []string{"src/store.py", "src/service.ts"} {
		t.Run(path, func(t *testing.T) {
			a := &armData{manifestKnown: true, evidence: []evidenceRecord{{SourceKind: evidenceSourceCode, Path: path, Range: &evidenceRange{StartLine: 10, EndLine: 12}, Rendered: &yes, LocatorValid: &yes}}}
			target := refEntry{Candidate: path + ":10-30"}
			if seen, _ := a.keySeen("p#one", target); seen {
				t.Fatal("a rendered signature was scored as the body")
			}
			a.evidence[0].Range.EndLine = 30
			if seen, _ := a.keySeen("p#one", target); !seen {
				t.Fatal("full rendered body was not visible")
			}
			a.evidence[0].SourceKind = evidenceSourceAuthorHint
			if seen, _ := a.keySeen("p#one", target); seen {
				t.Fatal("author hint source was treated as code")
			}
		})
	}
}
