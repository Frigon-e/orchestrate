package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// gateView checks the public default selection, not its TOON spelling.
func gateView(t *testing.T, out string) gateInformation {
	t.Helper()
	fields := decodedOutput(t, []byte(out), outputAgent).(map[string]any)
	if fields["schema_version"] != float64(2) || fields["status"] != nil {
		t.Fatalf("not a state-only v2 gate view: %#v", fields)
	}
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var view gateInformation
	if err := json.Unmarshal(b, &view); err != nil {
		t.Fatal(err)
	}
	if view.Gate != view.State {
		t.Fatalf("gate alias differs from state: %+v", view)
	}
	return view
}

func wantGateState(t *testing.T, out, state string) gateInformation {
	t.Helper()
	view := gateView(t, out)
	if view.State != state {
		t.Fatalf("state %q, want %q: %s", view.State, state, out)
	}
	return view
}

func reducerFixture(language string) (*gateSubject, []reviewCheck, []reviewAnswer, []reviewFinding, reviewPolicy) {
	p := evidenceTestPacket(language)
	if language == "go" {
		p.Evidence[0].Path = "src/export.go"
		p.Evidence[0].Range = nil
	}
	ids := []string{p.Evidence[0].ID}
	c := reviewCheck{Item: reviewItem{evidenceSchemaVersion, "export#atomic", "r1", "export", "Export a file without partial writes", []string{"reuse"}}, ID: "reuse", Required: true, Actor: "reviewer", Assessors: []string{"reviewer", "owner", "go"}, DecisionOwner: "owner", CanBlock: true}
	c.SearchScope = p.Lookups[0].Scope
	c.SearchQuery = p.Lookups[0].Query
	support := &claimSupport{SchemaVersion: evidenceSchemaVersion, State: claimSupported, Assessor: "reviewer", Method: "model", EvidenceIDs: ids, Rationale: "The writer uses an atomic destination replacement", Excerpt: "rename(temp, destination)", FailureScenario: "A write error must leave the previous destination intact"}
	a := reviewAnswer{SchemaVersion: evidenceSchemaVersion, ID: "answer-1", ItemID: c.Item.ID, ItemRevision: c.Item.Revision, CheckID: c.ID, AnswerState: reviewAnswerAnswered, Judgment: reviewJudgmentReuse, EvidenceIDs: ids, Support: support, Rationale: support.Rationale, MatchingBehavior: "Atomic replacement on successful write"}
	f := reviewFinding{SchemaVersion: evidenceSchemaVersion, ID: "finding-1", ItemID: c.Item.ID, ItemRevision: c.Item.Revision, CheckID: c.ID, Disposition: reviewAccepted, Claim: "Failure cleanup deletes the destination", EvidenceIDs: ids, Support: support, Assessor: "reviewer", AuthorityBasis: reviewReducerPolicyVersion, Rationale: "The error path must preserve the previous destination"}
	policy := reviewPolicy{Version: reviewReducerPolicyVersion, CurrentSubject: p.Subject, Packet: p, CurrentHashes: evidenceTestHashes(p)}
	return p.Subject, []reviewCheck{c}, []reviewAnswer{a}, []reviewFinding{f}, policy
}

func TestReduceStatePrecedenceAndReplay(t *testing.T) {
	// All combinations of corruption, staleness, accepted blockers and missing
	// answers exercise precedence, not just one example per overall state.
	for mask := range 16 {
		s, cs, as, fs, p := reducerFixture("python")
		want := "pass"
		if mask&1 != 0 {
			as = nil
			want = "incomplete"
		}
		if mask&2 == 0 {
			fs = nil
		} else {
			want = "fail"
		}
		if mask&4 != 0 {
			p.CurrentSubject = evidenceTestSubject("changed")
			want = "stale"
		}
		if mask&8 != 0 {
			p.SystemFailures = []string{"corrupt required policy data"}
			want = "error"
		}
		got := ReduceReview(s, cs, as, fs, p)
		if got.State != want {
			t.Fatalf("mask %d: state %s, want %s: %+v", mask, got.State, want, got)
		}
		if len(got.Findings) != len(fs) {
			t.Fatal("findings lost to precedence")
		}
		if mask&1 != 0 && got.Completion.Incomplete != 1 {
			t.Fatal("missing completion lost to precedence")
		}
		replay := ReduceReview(s, cs, as, fs, p)
		if !reflect.DeepEqual(got, replay) {
			t.Fatal("replay changed recorded result")
		}
		roundTrip := evidenceTestRoundTrip(t, struct {
			S *gateSubject
			C []reviewCheck
			A []reviewAnswer
			F []reviewFinding
			P reviewPolicy
		}{s, cs, as, fs, p})
		if !reflect.DeepEqual(got, ReduceReview(roundTrip.S, roundTrip.C, roundTrip.A, roundTrip.F, roundTrip.P)) {
			t.Fatal("JSON replay changed result")
		}
	}
}

func TestReduceExperimentRetainsNonApprovalDetails(t *testing.T) {
	for mask := range 16 {
		s, cs, as, fs, p := reducerFixture("go")
		if mask&1 != 0 {
			as = nil
		}
		if mask&2 == 0 {
			fs = nil
		}
		if mask&4 != 0 {
			p.CurrentSubject = evidenceTestSubject("changed")
		}
		if mask&8 != 0 {
			p.SystemFailures = []string{"system data corrupt"}
		}
		p.Experiment = true
		r := ReduceReview(s, cs, as, fs, p)
		want := "experiment"
		if mask&4 != 0 {
			want = "stale"
		}
		if mask&8 != 0 {
			want = "error"
		}
		if r.State != want || r.State == "pass" {
			t.Fatalf("experiment acquired approval: %+v", r)
		}
		if len(r.Findings) != len(fs) || len(r.ActiveBlockers) != len(fs) {
			t.Fatal("experiment hid blocker details")
		}
		if mask&1 != 0 && r.Completion.Incomplete != 1 {
			t.Fatal("experiment hid unfinished review")
		}
	}
}

func TestReduceMissingAnswerPermutations(t *testing.T) {
	s, base, answer, _, p := reducerFixture("go")
	var checks []reviewCheck
	var answers []reviewAnswer
	for _, id := range []string{"scope", "questions", "reuse"} {
		c := base[0]
		c.ID = id
		c.Item.Checks = []string{id}
		c.Item.ID = "library#" + id
		a := answer[0]
		a.ID = "answer-" + id
		a.ItemID = c.Item.ID
		a.CheckID = id
		checks = append(checks, c)
		answers = append(answers, a)
	}
	for mask := range 8 {
		var selected []reviewAnswer
		for i, a := range answers {
			if mask&(1<<i) != 0 {
				selected = append(selected, a)
			}
		}
		want := "incomplete"
		if mask == 7 {
			want = "pass"
		}
		before := ReduceReview(s, checks, selected, nil, p)
		if before.State != want {
			t.Fatalf("mask %d: %+v", mask, before)
		}
		for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
			shuffled := []reviewCheck{checks[order[0]], checks[order[1]], checks[order[2]]}
			slices.Reverse(selected)
			after := ReduceReview(s, shuffled, selected, nil, p)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("order changed result: %d, %v", mask, order)
			}
		}
	}
}

func TestReduceValidityAuthorityAndReasons(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*reviewAnswer, *reviewPolicy, *reviewCheck)
		reason reviewReason
	}{
		{"missing", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.AnswerState = reviewAnswerMissing }, reviewMissingAnswer},
		{"malformed", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.AnswerState = reviewAnswerInvalid }, reviewMissingAnswer},
		{"unavailable", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.AnswerState = reviewAnswerUnavailable }, reviewDependencyUnavailable},
		{"budget", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.AnswerState = reviewAnswerBudget }, reviewBudgetExhausted},
		{"unclear", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.Judgment = reviewJudgmentUnclear }, reviewAwaitingDecision},
		{"unsupported", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.Support = nil }, reviewUnsupportedClaim},
		{"unknown evidence", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.EvidenceIDs = []string{"not-present"} }, reviewUnsupportedClaim},
		{"unknown question", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.QuestionIDs = []string{"not-present"} }, reviewUnsupportedClaim},
		{"unknown execution", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.ExecutionID = "not-present" }, reviewUnsupportedClaim},
		{"wrong revision", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.ItemRevision = "r0" }, reviewUnsupportedClaim},
		{"no rationale", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.Rationale = "" }, reviewUnsupportedClaim},
		{"no matching behavior", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.MatchingBehavior = "" }, reviewUnsupportedClaim},
		{"extend no gap", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) { a.Judgment = reviewJudgmentExtend }, reviewUnsupportedClaim},
		{"unauthorized", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) {
			copy := *a.Support
			copy.Assessor = "author"
			a.Support = &copy
		}, reviewUnsupportedClaim},
		{"forged structural", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) {
			copy := *a.Support
			copy.Method = "structural"
			a.Support = &copy
		}, reviewUnsupportedClaim},
		{"new under incomplete search", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) {
			a.Judgment = reviewJudgmentNew
			p.Packet.Completeness = evidencePartial
		}, reviewUnsupportedClaim},
		{"new under unrelated search", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) {
			a.Judgment = reviewJudgmentNew
			p.Packet.Lookups[0].Unit = "review_reply"
		}, reviewUnsupportedClaim},
		{"tool loss is not exemption", func(a *reviewAnswer, p *reviewPolicy, c *reviewCheck) {
			a.AnswerState = reviewAnswerNotApplicable
			a.Rationale = "tools unavailable"
		}, reviewUnsupportedClaim},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, cs, as, _, p := reducerFixture("typescript")
			tc.mutate(&as[0], &p, &cs[0])
			r := ReduceReview(s, cs, as, nil, p)
			if r.State != "incomplete" || len(r.Reasons) == 0 || r.Reasons[0].Reason != tc.reason {
				t.Fatalf("bad incomplete reason: %+v", r)
			}
			if r.Reasons[0].Actor == "" || r.Reasons[0].Action == "" {
				t.Fatal("reason is not actionable")
			}
		})
	}
}

func TestReduceAcceptedBlockerRequiresAuthorizedResolution(t *testing.T) {
	for _, kind := range []string{"none", "newer satisfied answer", "unauthorized", "unrelated evidence", "missing rationale", "authorized"} {
		t.Run(kind, func(t *testing.T) {
			s, cs, as, fs, p := reducerFixture("go")
			resolution := reviewResolution{SchemaVersion: evidenceSchemaVersion, ID: "resolution", ItemID: fs[0].ItemID, ItemRevision: fs[0].ItemRevision, CheckID: fs[0].CheckID, Disposition: reviewAccepted, FindingIDs: []string{fs[0].ID}, EvidenceIDs: fs[0].EvidenceIDs, Support: fs[0].Support, Assessor: "reviewer", AuthorityBasis: reviewReducerPolicyVersion, Rationale: "The error cleanup preserves the prior destination"}
			want := "fail"
			switch kind {
			case "none":
			case "newer satisfied answer":
				newAnswer := as[0]
				newAnswer.ID = "newer"
				as = append(as, newAnswer)
			default:
				switch kind {
				case "unauthorized":
					resolution.Assessor = "author"
				case "unrelated evidence":
					resolution.FindingIDs = []string{"other"}
				case "missing rationale":
					resolution.Rationale = ""
				case "authorized":
					want = "pass"
				}
				p.Resolutions = []reviewResolution{resolution}
			}
			got := ReduceReview(s, cs, as, fs, p)
			if got.State != want {
				t.Fatalf("%s: %+v", kind, got)
			}
		})
	}
}

func TestReduceUnsupportedFindingsAndConflicts(t *testing.T) {
	for _, kind := range []string{"no support", "wrong actor", "invalid evidence", "proposed", "conflicting answers"} {
		t.Run(kind, func(t *testing.T) {
			s, cs, as, fs, p := reducerFixture("python")
			switch kind {
			case "no support":
				fs[0].Support = nil
			case "wrong actor":
				fs[0].Assessor = "author"
			case "invalid evidence":
				fs[0].EvidenceIDs = []string{"unknown"}
			case "proposed":
				fs[0].Disposition = reviewProposed
			case "conflicting answers":
				fs = nil
				other := as[0]
				other.ID = "other"
				other.Judgment = reviewJudgmentExtend
				other.MissingBehavior = "Add failure cleanup"
				as = append(as, other)
			}
			got := ReduceReview(s, cs, as, fs, p)
			if got.State != "incomplete" {
				t.Fatalf("claim became accepted: %+v", got)
			}
		})
	}
}

func TestReduceOptionalFailuresDoNotBlock(t *testing.T) {
	s, cs, as, _, p := reducerFixture("go")
	optional := cs[0]
	optional.Required = false
	optional.Item.ID = "library#diagnostics"
	cs = append(cs, optional)
	a := as[0]
	a.ID = "optional"
	a.ItemID = optional.Item.ID
	a.AnswerState = reviewAnswerUnavailable
	as = append(as, a)
	got := ReduceReview(s, cs, as, nil, p)
	if got.State != "pass" || got.Completion.Optional != 1 {
		t.Fatalf("optional failure blocked: %+v", got)
	}
	cs[1].Required = true
	if got = ReduceReview(s, cs, as, nil, p); got.State != "incomplete" {
		t.Fatal("required failure passed")
	}
}

func TestReduceNewRequiresItsOwnCompleteSearch(t *testing.T) {
	s, cs, as, _, p := reducerFixture("go")
	as[0].Judgment = reviewJudgmentNew
	if r := ReduceReview(s, cs, as, nil, p); r.State != "pass" {
		t.Fatalf("complete attached search: %+v", r)
	}
	p.Packet.Lookups[0].Completeness = evidencePartial
	p.Packet.Lookups[0].Status = "partial"
	if r := ReduceReview(s, cs, as, nil, p); r.State != "incomplete" {
		t.Fatalf("partial search approved absence: %+v", r)
	}
}

func TestGateStateChecksDerivedFromTaskKind(t *testing.T) {
	for _, kind := range []string{"ship", "scout"} {
		s := evidenceTestSubject("current")
		rec := &gateRecord{Subject: s, Groups: map[string][]string{"group-x": {"x"}}}
		pkg := &specPackage{id: "x", kind: kind, raw: "Package x"}
		initGateReview(rec, &spec{packages: []*specPackage{pkg}}, "Spec", []gatePass{{name: "group-x"}})
		var reuse reviewCheck
		for _, c := range rec.Review.Checks {
			if c.Item.Package == "x" && c.ID == "reuse" {
				reuse = c
			}
		}
		if reuse.ID == "" {
			t.Fatal("reuse check vanished instead of an identified exemption")
		}
		if (kind == "scout") != (len(reuse.NotApplicableReasons) > 0) {
			t.Fatalf("wrong kind exemption: %+v", reuse)
		}
	}
}

func TestReduceNonBlockingFindingsStillNeedRequiredSupport(t *testing.T) {
	s, cs, as, fs, p := reducerFixture("go")
	cs[0].CanBlock = false
	if r := ReduceReview(s, cs, as, fs, p); r.State != "pass" {
		t.Fatalf("supported nonblocking finding blocked: %+v", r)
	}
	fs[0].Support = nil
	if r := ReduceReview(s, cs, as, fs, p); r.State != "incomplete" {
		t.Fatal("no-block policy waived required support")
	}
	cs[0].Required = false
	if r := ReduceReview(s, cs, as, fs, p); r.State != "pass" {
		t.Fatal("optional unsupported diagnostic blocked")
	}
}

func TestReduceNotApplicableAndDecisionAuthority(t *testing.T) {
	s, cs, as, _, p := reducerFixture("typescript")
	cs[0].NotApplicableReasons = []string{"spelling-only correction"}
	as[0].AnswerState = reviewAnswerNotApplicable
	as[0].Rationale = "spelling-only correction"
	if r := ReduceReview(s, cs, as, nil, p); r.State != "pass" {
		t.Fatalf("validated exemption: %+v", r)
	}
	q := reviewQuestion{SchemaVersion: evidenceSchemaVersion, ID: "overwrite", ItemID: cs[0].Item.ID, ItemRevision: cs[0].Item.Revision, CheckID: cs[0].ID, Question: "May export overwrite an existing file?", Authority: reviewAuthority{Owner: "owner", Basis: "Public CLI behavior"}, Options: []reviewQuestionOption{{"yes", "Overwrite", "Existing file is replaced"}, {"no", "Refuse", "Existing file remains"}}, Answer: "yes", AnsweringActor: "reviewer"}
	p.Questions = []reviewQuestion{q}
	r := ReduceReview(s, cs, as, nil, p)
	if r.State != "incomplete" || r.Reasons[0].Reason != reviewAwaitingDecision || r.Reasons[0].Actor != "owner" {
		t.Fatalf("unauthorized decision passed: %+v", r)
	}
	p.Questions[0].AnsweringActor = "owner"
	if r = ReduceReview(s, cs, as, nil, p); r.State != "pass" {
		t.Fatalf("authorized decision refused: %+v", r)
	}
	p.Questions[0].Authority.Owner = "reviewer"
	p.Questions[0].AnsweringActor = "reviewer"
	if r = ReduceReview(s, cs, as, nil, p); r.State != "incomplete" {
		t.Fatal("question granted its own decision authority")
	}
	p.Questions[0].Authority.Owner = "owner"
	p.Questions[0].Authority.Delegated = true
	if r = ReduceReview(s, cs, as, nil, p); r.State != "incomplete" {
		t.Fatal("question granted its own delegation")
	}
	cs[0].DecisionDelegates = []string{"reviewer"}
	if r = ReduceReview(s, cs, as, nil, p); r.State != "pass" {
		t.Fatalf("policy-authorized delegate refused: %+v", r)
	}
	p.Questions = append(p.Questions, p.Questions[0])
	if r = ReduceReview(s, cs, as, nil, p); r.State != "incomplete" {
		t.Fatal("ambiguous duplicate decision records passed")
	}
}

func TestReduceMixedAnswerPermutationAndNoInputMutation(t *testing.T) {
	s, base, answer, _, p := reducerFixture("python")
	states := []reviewAnswerState{reviewAnswerMissing, reviewAnswerAnswered, reviewAnswerInvalid, reviewAnswerUnavailable, reviewAnswerBudget, reviewAnswerNotApplicable}
	for _, first := range states {
		for _, second := range states {
			for _, third := range states {
				var cs []reviewCheck
				var as []reviewAnswer
				for i, state := range []reviewAnswerState{first, second, third} {
					c := base[0]
					c.Item.ID += string(rune('a' + i))
					cs = append(cs, c)
					a := answer[0]
					a.ID += string(rune('a' + i))
					a.ItemID = c.Item.ID
					a.AnswerState = state
					as = append(as, a)
				}
				original, _ := json.Marshal(struct {
					C []reviewCheck
					A []reviewAnswer
					P reviewPolicy
				}{cs, as, p})
				got := ReduceReview(s, cs, as, nil, p)
				after, _ := json.Marshal(struct {
					C []reviewCheck
					A []reviewAnswer
					P reviewPolicy
				}{cs, as, p})
				if string(original) != string(after) {
					t.Fatal("reducer mutated input records")
				}
				slices.Reverse(cs)
				slices.Reverse(as)
				if !reflect.DeepEqual(got, ReduceReview(s, cs, as, nil, p)) {
					t.Fatalf("mixed states reordered: %s/%s/%s", first, second, third)
				}
				if (first != reviewAnswerAnswered || second != reviewAnswerAnswered || third != reviewAnswerAnswered) && got.State == "pass" {
					t.Fatal("unfinished required answer passed")
				}
			}
		}
	}
}

func TestGateStateLocatorIsNotSemanticSupport(t *testing.T) {
	for _, kind := range []string{"unsupported", "supported", "optional concern"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			dir := h.gateRepo(1)
			severity := "blocker"
			if kind == "optional concern" {
				severity = "concern"
			}
			finding := gateFinding{Severity: severity, Where: "Design rules", Claim: "The writer loses the previous file on failure", Evidence: "internal/store.go:3", Why: "Failure cleanup removes the destination"}
			if kind == "supported" {
				finding.Assessment = &claimSupport{SchemaVersion: evidenceSchemaVersion, State: claimSupported, Rationale: "The write failure path does not preserve the destination", Excerpt: "func Put()", FailureScenario: "An interrupted export replaces the old destination"}
			}
			raw, _ := json.Marshal(struct {
				Findings []gateFinding `json:"findings"`
			}{[]gateFinding{finding}})
			h.fakePi(gatePasses(map[string]string{"adv-a": string(raw)}))
			code, out := h.run("gate", "spec.md")
			rec := readRecord(t, dir)
			want := "incomplete"
			if kind == "supported" {
				want = "fail"
			}
			if kind == "optional concern" {
				want = "pass"
			}
			if rec.State != want || (code == 0) != (want == "pass") {
				t.Fatalf("%s: state %s: %s", kind, rec.State, out)
			}
			if len(rec.Findings) != 1 {
				t.Fatal("finding disappeared")
			}
			if kind == "unsupported" && (rec.Findings[0].Accepted || rec.State != "incomplete") {
				t.Fatal("a valid locator accepted an unsupported interpretation")
			}
		})
	}
}

func TestGateStateOutputQuoting(t *testing.T) {
	values := []string{"plain words", "true", "null", "", "group:api", " left ", "a,b", "line\nnext", `a"b\c`}
	for _, audience := range []outputAudience{outputAgent, outputHuman, outputJSON} {
		var out strings.Builder
		if err := writeOutput(&out, audience, gateInformation{SchemaVersion: 2, State: "incomplete", Gate: "incomplete", Notes: values}); err != nil {
			t.Fatal(err)
		}
		view := decodedOutput(t, []byte(out.String()), audience).(map[string]any)
		for i, value := range values {
			if got := view["notes"].([]any)[i]; got != value {
				t.Fatalf("audience %d changed string %q to %#v", audience, value, got)
			}
		}
	}
}

func TestGateStateRetiresSchemaOneBeforeExecution(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	dir := h.fakePi(gatePasses(nil))
	for _, flag := range []string{"", "--human", "--json"} {
		args := []string{"gate", "spec.md", "--schema-version", "1"}
		if flag != "" {
			args = append(args, flag)
		}
		code, out := h.run(args...)
		if code == 0 {
			t.Fatalf("retired schema accepted: %s", out)
		}
		fields := decodedOutput(t, []byte(out), explicitAudience(args)).(map[string]any)
		if fields["status"] != "failed" || !strings.Contains(fields["error"].(string), "--schema-version 1 is retired; use --schema-version 2") {
			t.Fatalf("missing migration hint: %#v", fields)
		}
	}
	if exists(dir + "/calls.log") {
		t.Fatal("retired schema invoked a reviewer")
	}
	code, out := h.run("gate", "spec.md", "--schema-version", "3")
	if code == 0 || !strings.Contains(out, "--schema-version is 2, not 3") {
		t.Fatalf("unknown schema accepted: %s", out)
	}
	for _, args := range [][]string{{"gate", "--help"}, {"gate", "--schema-version", "2", "--help"}, {"gate", "--human", "--help"}, {"gate", "--json", "--help"}} {
		code, help := h.run(args...)
		if code != 0 || strings.Contains(help, "help requested") || strings.Contains(help, "status: failed") {
			t.Fatalf("help became a failure for %v: exit %d\n%s", args, code, help)
		}
		wantContains(t, help, "2 (default)", "error|stale|fail|incomplete|pass", "only a current reducer pass authorizes plan")
	}
}

func TestGateStateOutputAudiencesShareV2Selection(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(map[bool]string{false: "pass", true: "incomplete"}[incomplete], func(t *testing.T) {
			h := setup(t)
			dir := h.gateRepo(1)
			over := map[string]string{}
			wantState, wantCode := "pass", 0
			if incomplete {
				over["adv-a"] = `{"findings":[{"severity":"blocker","where":"Design","claim":"Unsupported interpretation","evidence":"missing.py:1"}]}`
				wantState, wantCode = "incomplete", 1
			}
			h.fakePi(gatePasses(over))
			var selected any
			for _, audience := range []struct {
				flag string
				kind outputAudience
			}{{"", outputAgent}, {"--schema-version=2", outputAgent}, {"--human", outputHuman}, {"--json", outputJSON}} {
				args := []string{"gate", "spec.md"}
				if audience.flag != "" {
					args = append(args, audience.flag)
				}
				code, out := h.run(args...)
				if code != wantCode {
					t.Fatalf("%s: %d %s", audience.flag, code, out)
				}
				decoded := decodedOutput(t, []byte(out), audience.kind)
				view := decoded.(map[string]any)
				if view["schema_version"] != float64(2) || view["state"] != wantState || view["status"] != nil || view["gate"] != wantState {
					t.Fatalf("wrong public state: %+v", view)
				}
				if view["blockers"] != "0 verified" || !strings.HasPrefix(view["concerns"].(string), "0 verified, ") || !strings.HasPrefix(view["spec"].(string), "spec.md (version ") {
					t.Fatalf("public counts/spec changed: %#v", view)
				}
				completion := view["completion"].(map[string]any)
				if completion["required"].(float64) <= 0 {
					t.Fatal("required completion disappeared")
				}
				if incomplete {
					reasons := view["reasons"].([]any)
					actors := view["next_actors"].([]any)
					actions := view["next_actions"].([]any)
					if len(reasons) == 0 || len(actors) == 0 || len(actions) == 0 || reasons[0].(map[string]any)["reason"] != "unsupported_claim" {
						t.Fatal("incomplete output lost reason/actor/action")
					}
				}
				// E1 adds per-execution timing, so compare the complete budget to
				// this execution's persisted facts before comparing stable selections.
				budgetJSON, err := json.Marshal(readRecord(t, dir).Budget)
				if err != nil {
					t.Fatal(err)
				}
				expectedBudget := decodedOutput(t, budgetJSON, outputJSON)
				if !reflect.DeepEqual(view["budget"], expectedBudget) {
					t.Fatalf("%s budget differs from its recorded run: %#v != %#v", audience.flag, view["budget"], expectedBudget)
				}
				// Exact snapshot serialization is also checked by
				// TestGateRunBudgetV2OutputEquality; distinct runs have distinct clocks.
				stableBudget := view["budget"].(map[string]any)
				for _, key := range []string{"started", "elapsed_seconds", "attempt_seconds"} {
					delete(stableBudget, key)
				}
				delete(stableBudget["remaining"].(map[string]any), "seconds")
				// Concurrent passes may finish in a different order. Keep every note.
				slices.SortFunc(view["notes"].([]any), func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
				if selected != nil && !reflect.DeepEqual(selected, decoded) {
					t.Fatalf("audiences selected different stable facts: %#v != %#v", selected, decoded)
				}
				selected = decoded
			}
			report := string(mustRead(t, dir+"/report.md"))
			if !strings.Contains(report, "Review state: **"+wantState+"**") || !strings.Contains(report, "Completion:") {
				t.Fatal("human report omitted v2 state/completion")
			}
			if incomplete && !strings.Contains(report, "## Next actions") {
				t.Fatal("human report omitted next action")
			}
		})
	}
}

func TestGateStateExecutionFailuresHaveTypedReasons(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		state       reviewAnswerState
		reason      reviewReason
	}{
		{"provider", "FAIL", reviewAnswerUnavailable, reviewDependencyUnavailable},
		{"malformed", `{"findings":[{"severity":"unknown"}]}`, reviewAnswerInvalid, reviewMissingAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			dir := h.gateRepo(1)
			h.fakePi(gatePasses(map[string]string{"adv-a": tc.reply}))
			if code, out := h.run("gate", "spec.md", "--runner", "pi:"+gateModelDef); code != 1 {
				t.Fatalf("required execution failure approved: %s", out)
			}
			rec := readRecord(t, dir)
			if rec.State != "incomplete" || rec.PassAnswerStates["adversary-a"] != tc.state {
				t.Fatalf("incorrect failure class: %+v", rec.PassAnswerStates)
			}
			found := false
			for _, reason := range rec.Reduction.Reasons {
				if reason.Check == "@adversary-a/adversary-a" && reason.Reason == tc.reason {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing precise failure reason: %+v", rec.Reduction.Reasons)
			}
		})
	}
}

func TestGateStateSerializationHasNoLegacyStatus(t *testing.T) {
	for _, state := range []string{"error", "stale", "fail", "incomplete", "pass", "experiment"} {
		for _, value := range []any{
			reviewResult{SchemaVersion: 2, State: state},
			gateRecord{SchemaVersion: 2, State: state, Reduction: &reviewResult{SchemaVersion: 2, State: state}},
			benchRun{SchemaVersion: 2, State: state},
		} {
			b, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(b, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["state"]) != `"`+state+`"` || fields["status"] != nil || fields["historical_status"] != nil {
				t.Fatalf("new %T retained a legacy result: %s", value, b)
			}
		}
	}
}

func TestGateStateFindingsSurviveFinish(t *testing.T) {
	h := setup(t)
	h.commitSpec(planHeader + planPkg("export", "kind: ship\nowns: app.py", "") + planTail)
	g, err := newCheckoutGuard(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer g.cancel()
	rec := &gateRecord{Findings: []gateFinding{{Claim: "Keep this observation"}}, Questions: []gateQuestion{{Question: "Keep this question"}}, Items: []gateItemResult{{ID: "export#atomic"}}}
	writeFiles(t, h.repo, map[string]string{"app.py": "def export(): pass\n"})
	g.finish(rec)
	g.finish(rec)
	if len(rec.Findings) != 1 || len(rec.Questions) != 1 || len(rec.Items) != 1 || len(rec.TreeChanged) == 0 {
		t.Fatalf("finish erased records: %+v", rec)
	}
	if rec.State != "" {
		t.Fatal("finish decided state outside reducer")
	}
}

func TestGateStateProductionReplayAndMalformedRefusal(t *testing.T) {
	h := setup(t)
	dir := h.gateRepo(1)
	h.fakePi(gatePasses(nil))
	h.must("gate", "spec.md")
	rec := readRecord(t, dir)
	if rec.SchemaVersion != 2 || rec.State != "pass" || rec.Review == nil {
		t.Fatalf("missing v2 decision: %+v", rec)
	}
	replay := ReduceReview(rec.Subject, rec.Review.Checks, rec.Review.Answers, rec.Review.Findings, rec.Review.Policy)
	if !reflect.DeepEqual(*rec.Reduction, replay) {
		t.Fatal("production decision is not replayable")
	}
	if !gatePassed(h.a.siteRoot(t), h.repo, mustRead(t, h.repo+"/spec.md")) {
		t.Fatal("accepted pass cannot authorize plan")
	}
	rec.Review.Answers = nil
	if err := writeGate(dir, &rec); err != nil {
		t.Fatal(err)
	}
	if gatePassed(h.a.siteRoot(t), h.repo, mustRead(t, h.repo+"/spec.md")) {
		t.Fatal("state string authorized an incomplete record")
	}
	h.fakePi(gatePasses(map[string]string{"package-api": "{\"id\":\"api\"}"}))
	code, _ := h.run("gate", "spec.md")
	if code == 0 {
		t.Fatal("malformed reply improved approval")
	}
	rec = readRecord(t, dir)
	b, _ := json.Marshal(rec)
	if rec.State != "incomplete" || !strings.Contains(string(b), "next_action") {
		t.Fatalf("malformed reply not actionable incomplete: %s", b)
	}
}
