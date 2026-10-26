package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func absenceReview(languages ...string) (*gateSubject, []reviewCheck, []reviewAnswer, reviewPolicy) {
	language := "go"
	if len(languages) > 0 {
		language = languages[0]
	}
	s, cs, as, _, p := reducerFixture(language)
	as[0].Judgment = reviewJudgmentNew
	as[0].LookupIDs = []string{p.Packet.Lookups[0].ID}
	as[0].EvidenceIDs = nil
	as[0].Support.EvidenceIDs = nil
	as[0].Support.Rationale = "The complete in-scope search found no implementation"
	as[0].Rationale = as[0].Support.Rationale
	p.Packet.Evidence = nil
	p.CurrentHashes = nil
	p.Packet.Lookups[0].Status = "empty"
	p.Packet.Lookups[0].Counts.Total = evidenceTestPtr(0)
	return s, cs, as, p
}

func TestReduceAbsenceUsesCompletedZeroResultLookup(t *testing.T) {
	s, cs, as, p := absenceReview()
	if err := p.Packet.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := as[0].Validate(cs[0].Item, p.Packet, s, nil); err != nil {
		t.Fatal(err)
	}
	r := ReduceReview(s, cs, as, nil, p)
	if r.State != "pass" || r.Completion.Completed != 1 {
		t.Fatalf("zero-result search could not support new behavior: %+v", r)
	}
	replay := evidenceTestRoundTrip(t, struct {
		S *gateSubject
		C []reviewCheck
		A []reviewAnswer
		P reviewPolicy
	}{s, cs, as, p})
	if got := ReduceReview(replay.S, replay.C, replay.A, nil, replay.P); got.State != "pass" {
		t.Fatalf("lookup coverage lost on replay: %+v", got)
	}
	// There is still no evidence row for the empty lookup. That schema rule is
	// intentionally unchanged by allowing direct search coverage.
	_, _, _, _, control := reducerFixture("go")
	p.Packet.Evidence = control.Packet.Evidence
	if err := p.Packet.Validate(); err == nil || !strings.Contains(err.Error(), "no results") {
		t.Fatalf("fake zero-result evidence accepted: %v", err)
	}
}

func TestReduceAbsenceCoverageFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*reviewCheck, *reviewAnswer, *reviewPolicy)
	}{
		{"unknown ID", func(_ *reviewCheck, a *reviewAnswer, _ *reviewPolicy) { a.LookupIDs = []string{"missing"} }},
		{"duplicate ID", func(_ *reviewCheck, a *reviewAnswer, _ *reviewPolicy) {
			a.LookupIDs = append(a.LookupIDs, a.LookupIDs[0])
		}},
		{"stale lookup subject", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) {
			p.Packet.Lookups[0].SubjectID = "different-subject"
		}},
		{"wrong scope", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) { p.Packet.Lookups[0].Scope = "other/" }},
		{"prefix sibling", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) { p.Packet.Lookups[0].Scope = "src2/" }},
		{"narrower scope", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) { p.Packet.Lookups[0].Scope = "src/nested/" }},
		{"undeclared scope", func(c *reviewCheck, _ *reviewAnswer, _ *reviewPolicy) { c.SearchScope = "" }},
		{"escaping scope", func(c *reviewCheck, _ *reviewAnswer, p *reviewPolicy) {
			c.SearchScope = "../outside"
			p.Packet.Lookups[0].Scope = "../outside"
		}},
		{"review reply is not search", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) { p.Packet.Lookups[0].Unit = "review_reply" }},
		{"source read is not search", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) { p.Packet.Lookups[0].Unit = "source_read" }},
		{"omitted window", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) {
			p.Packet.Lookups[0].Omissions = []string{"retention cap"}
		}},
		{"partial", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) {
			l := &p.Packet.Lookups[0]
			l.Status = "partial"
			l.Completeness = evidencePartial
		}},
		{"lower bound", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) {
			l := &p.Packet.Lookups[0]
			l.Status = "partial"
			l.Completeness = evidencePartial
			l.CountKind = evidenceCountLowerBound
		}},
		{"failed", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) {
			l := &p.Packet.Lookups[0]
			l.ExecutionStatus = lookupFailed
			l.Status = "failed"
			l.Completeness = evidencePartial
			l.CountKind = evidenceCountUnknown
			l.Counts.Total = nil
			l.Counts.Succeeded = evidenceTestPtr(0)
			l.Counts.Failed = evidenceTestPtr(1)
			l.Errors = []string{"provider failed"}
		}},
		{"not requested", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) {
			l := &p.Packet.Lookups[0]
			l.ExecutionStatus = lookupNotRequested
			l.Status = ""
			l.Completeness = evidenceUnknown
			l.CountKind = evidenceCountUnknown
			l.Counts = lookupCounts{}
		}},
		{"unavailable", func(_ *reviewCheck, _ *reviewAnswer, p *reviewPolicy) {
			l := &p.Packet.Lookups[0]
			l.ExecutionStatus = lookupUnavailable
			l.Status = ""
			l.Completeness = evidenceUnknown
			l.CountKind = evidenceCountUnknown
			l.Counts = lookupCounts{}
			l.Errors = []string{"server unavailable"}
		}},
		{"missing assessment", func(_ *reviewCheck, a *reviewAnswer, _ *reviewPolicy) { a.Support = nil }},
		{"unauthorized assessment", func(_ *reviewCheck, a *reviewAnswer, _ *reviewPolicy) { a.Support.Assessor = "author" }},
		{"missing lookup coverage", func(_ *reviewCheck, a *reviewAnswer, _ *reviewPolicy) { a.LookupIDs = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, cs, as, p := absenceReview()
			tc.mutate(&cs[0], &as[0], &p)
			if err := p.Packet.Validate(); err != nil {
				t.Fatalf("invalid regression setup: %v", err)
			}
			r := ReduceReview(s, cs, as, nil, p)
			if r.State != "incomplete" || r.Completion.Completed != 0 {
				t.Fatalf("inadequate absence coverage passed: %+v", r)
			}
		})
	}
}

func TestReduceAbsenceQueryBinding(t *testing.T) {
	for _, language := range []string{"python", "typescript"} {
		t.Run(language, func(t *testing.T) {
			s, cs, as, p := absenceReview(language)
			p.Packet.Lookups[0].Query = "zzzz_unrelated_token"
			if err := p.Packet.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := ReduceReview(s, cs, as, nil, p); got.State != "incomplete" {
				t.Fatalf("unrelated query authorized new behavior: %s", got.State)
			}
		})
	}
}

func TestReduceAbsenceRejectsExcludedProduction(t *testing.T) {
	for _, excluded := range []string{"everything", "src", "src/secret.py", "src/secret.ts", "tests/**", "vendor/**"} {
		t.Run(excluded, func(t *testing.T) {
			s, cs, as, p := absenceReview()
			p.Packet.Lookups[0].Exclusions = append(p.Packet.Lookups[0].Exclusions, excluded)
			if err := p.Packet.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := ReduceReview(s, cs, as, nil, p); got.State != "incomplete" {
				t.Fatalf("excluded production authorized new behavior: %s", got.State)
			}
		})
	}
}

func TestReduceAbsenceRejectsUnretainedHits(t *testing.T) {
	s, cs, as, p := absenceReview()
	p.Packet.Lookups[0].Status = "ok"
	p.Packet.Lookups[0].Counts.Total = evidenceTestPtr(7)
	if err := p.Packet.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := ReduceReview(s, cs, as, nil, p); got.State != "incomplete" {
		t.Fatalf("search with hits but no evidence authorized new behavior: %s", got.State)
	}
}

func TestReduceAbsenceLegacyScopeReplay(t *testing.T) {
	s, cs, as, _, p := reducerFixture("python")
	as[0].Judgment = reviewJudgmentNew
	cs[0].SearchScope = ""
	cs[0].SearchQuery = ""
	p.Packet.Lookups[0].Scope = "."
	p.Packet.Lookups[0].Query = "Code that implements " + cs[0].Item.Requirement
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "search_scope") || strings.Contains(string(b), "search_query") || strings.Contains(string(b), "allowed_search_exclusions") {
		t.Fatalf("not an old check record shape: %s", b)
	}
	replay := evidenceTestRoundTrip(t, struct {
		S *gateSubject
		C []reviewCheck
		A []reviewAnswer
		P reviewPolicy
	}{s, cs, as, p})
	if got := ReduceReview(replay.S, replay.C, replay.A, nil, replay.P); got.State != "pass" {
		t.Fatalf("legacy root search no longer replays: %s", got.State)
	}
}

func TestReduceAbsenceBroaderScopeCoversCheck(t *testing.T) {
	for _, scope := range []string{".", "src", "src/", "./src"} {
		t.Run(scope, func(t *testing.T) {
			s, cs, as, p := absenceReview()
			cs[0].SearchScope = "src/nested"
			p.Packet.Lookups[0].Scope = scope
			if got := ReduceReview(s, cs, as, nil, p); got.State != "pass" {
				t.Fatalf("broader search did not cover narrower check: %s", got.State)
			}
		})
	}
}

// These call the adequacy unit directly. Packet/answer validation must not
// mask a missing reducer guard, especially for reply/source observations.
func TestReduceAbsenceDirectSearchGuards(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*reviewCheck, *evidenceLookup)
	}{
		{"unrelated query", func(_ *reviewCheck, l *evidenceLookup) { l.Query = "unrelated" }},
		{"production exclusion", func(_ *reviewCheck, l *evidenceLookup) { l.Exclusions = []string{"src/hidden.py"} }},
		{"everything even when allowed", func(c *reviewCheck, l *evidenceLookup) {
			c.AllowedSearchExclusions = []string{"everything"}
			l.Exclusions = []string{"everything"}
		}},
		{"unretained hits", func(_ *reviewCheck, l *evidenceLookup) { l.Status = "ok"; l.Counts.Total = evidenceTestPtr(7) }},
		{"ok is not empty", func(_ *reviewCheck, l *evidenceLookup) { l.Status = "ok" }},
		{"empty is not zero", func(_ *reviewCheck, l *evidenceLookup) { l.Counts.Total = evidenceTestPtr(7) }},
		{"unknown is not zero", func(_ *reviewCheck, l *evidenceLookup) { l.Counts.Total = nil; l.CountKind = evidenceCountUnknown }},
		{"exact count required", func(_ *reviewCheck, l *evidenceLookup) { l.CountKind = evidenceCountLowerBound }},
		{"review reply", func(_ *reviewCheck, l *evidenceLookup) { l.Unit = "review_reply" }},
		{"source read", func(_ *reviewCheck, l *evidenceLookup) { l.Unit = "source_read" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cs, as, p := absenceReview()
			if !adequateScopedReviewSearch(p.Packet, cs[0], as[0].LookupIDs) {
				t.Fatal("control did not reach search-adequacy guards")
			}
			tc.mutate(&cs[0], &p.Packet.Lookups[0])
			if adequateScopedReviewSearch(p.Packet, cs[0], as[0].LookupIDs) {
				t.Fatal("inadequate lookup passed direct reducer guard")
			}
		})
	}
}

func TestReduceAbsenceChecksEveryCitedLookup(t *testing.T) {
	for _, kind := range []string{"query", "exclusions", "hits"} {
		t.Run(kind, func(t *testing.T) {
			s, cs, as, p := absenceReview()
			second := p.Packet.Lookups[0]
			second.ID = "second-search"
			switch kind {
			case "query":
				second.Query = "unrelated"
			case "exclusions":
				second.Exclusions = []string{"everything"}
			case "hits":
				second.Status = "ok"
				second.Counts.Total = evidenceTestPtr(3)
			}
			p.Packet.Lookups = append(p.Packet.Lookups, second)
			as[0].LookupIDs = append(as[0].LookupIDs, second.ID)
			if err := p.Packet.Validate(); err != nil {
				t.Fatal(err)
			}
			if adequateScopedReviewSearch(p.Packet, cs[0], as[0].LookupIDs) {
				t.Fatal("one adequate search hid an inadequate cited lookup")
			}
			if got := ReduceReview(s, cs, as, nil, p); got.State != "incomplete" {
				t.Fatalf("inadequate second lookup authorized new behavior: %s", got.State)
			}
		})
	}
}

func TestReduceAbsenceTrustedSearchPolicy(t *testing.T) {
	s, cs, as, p := absenceReview()
	// An omitted query uses a behavior-specific Go-derived binding, not an
	// unbound wildcard. An explicit query permits trusted retriever query syntax.
	cs[0].SearchQuery = ""
	p.Packet.Lookups[0].Query = "Code that implements " + cs[0].Item.Requirement
	if got := ReduceReview(s, cs, as, nil, p); got.State != "pass" {
		t.Fatalf("derived query binding failed: %s", got.State)
	}
	p.Packet.Lookups[0].Query = "unrelated"
	if got := ReduceReview(s, cs, as, nil, p); got.State != "incomplete" {
		t.Fatalf("omitted query binding became wildcard: %s", got.State)
	}
	cs[0].SearchQuery = "trusted behavior query"
	p.Packet.Lookups[0].Query = cs[0].SearchQuery
	for _, exclusions := range [][]string{nil, {"tests", "generated", "vendor"}, {"optional-module"}} {
		p.Packet.Lookups[0].Exclusions = exclusions
		cs[0].AllowedSearchExclusions = nil
		if len(exclusions) == 1 {
			cs[0].AllowedSearchExclusions = []string{"optional-module"}
		}
		if got := ReduceReview(s, cs, as, nil, p); got.State != "pass" {
			t.Fatalf("trusted exclusions %v failed: %s", exclusions, got.State)
		}
	}
	// A custom allow-list replaces the defaults instead of silently widening it.
	p.Packet.Lookups[0].Exclusions = []string{"tests"}
	if adequateScopedReviewSearch(p.Packet, cs[0], as[0].LookupIDs) {
		t.Fatal("custom exclusion policy silently included default labels")
	}
}

func TestReduceAbsenceEvidenceSearchStillNeedsBinding(t *testing.T) {
	for _, language := range []string{"python", "typescript"} {
		t.Run(language, func(t *testing.T) {
			s, cs, as, _, p := reducerFixture(language)
			as[0].Judgment = reviewJudgmentNew
			if got := ReduceReview(s, cs, as, nil, p); got.State != "pass" {
				t.Fatalf("evidence-backed control failed: %s", got.State)
			}
			p.Packet.Lookups[0].Query = "unrelated"
			if adequateScopedReviewSearch(p.Packet, cs[0], nil, as[0].EvidenceIDs...) {
				t.Fatal("evidence rows bypassed the query binding")
			}
		})
	}
}

func TestReviewAnswerLookupIDsAreAdditive(t *testing.T) {
	_, _, as, _, _ := reducerFixture("go")
	b, err := json.Marshal(as[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "lookup_ids") {
		t.Fatalf("empty additive field changed old encoding: %s", b)
	}
	s, cs, newAnswers, p := absenceReview()
	newAnswers[0].Judgment = reviewJudgmentReuse
	if err := newAnswers[0].Validate(cs[0].Item, p.Packet, s, nil); err == nil {
		t.Fatal("lookup-only support authorized reuse without source evidence")
	}
}
