package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func gateStateTestSupport(id string) *claimSupport {
	return &claimSupport{SchemaVersion: 1, State: claimSupported, Assessor: "reviewer-1", Method: "model", EvidenceIDs: []string{id}, Rationale: "The source addresses the required output behavior.", Excerpt: "return 'héllo 😀'", FailureScenario: "No destination file can be partially overwritten by this pure function."}
}

func gateStateTestState() reviewState {
	p := evidenceTestPacket("python")
	id := p.Evidence[0].ID
	route := executionRoute{Provider: "configured-provider", Model: "configured-model", Harness: "pi", Role: "gate"}
	return reviewState{
		evidencePacket: p,
		Items:          []reviewItem{{SchemaVersion: 1, ID: "export#write", Revision: "r-001", Package: "export", Requirement: "Return a CSV string without modifying destination files.", Checks: []string{"reuse", "scope", "decisions"}}},
		Answers:        []reviewAnswer{{SchemaVersion: 1, ID: "answer-1", ItemID: "export#write", ItemRevision: "r-001", CheckID: "reuse", AnswerState: reviewAnswerAnswered, Judgment: reviewJudgmentReuse, EvidenceIDs: []string{id}, Support: gateStateTestSupport(id), Rationale: "The existing pure writer satisfies the requirement.", MissingInformation: []string{}, MatchingBehavior: "Returns a string.", MissingBehavior: "", NextAction: "Reuse the writer.", QuestionIDs: []string{"question-1"}, ExecutionID: "execution-1"}},
		Questions:      []reviewQuestion{{SchemaVersion: 1, ID: "question-1", ItemID: "export#write", ItemRevision: "r-001", CheckID: "reuse", Question: "Which output should the command expose?", Options: []reviewQuestionOption{{ID: "string", Text: "Return the string.", Impact: "The caller chooses how to save it."}, {ID: "file", Text: "Write a file.", Impact: "Requires destination and overwrite policy."}}, Authority: reviewAuthority{Owner: "product-owner", Delegated: false, Basis: "spec authority section"}, Answer: "string", AnsweringActor: "product-owner"}},
		Findings:       []reviewFinding{{SchemaVersion: 1, ID: "finding-1", ItemID: "export#write", ItemRevision: "r-001", CheckID: "reuse", Disposition: reviewProposed, Claim: "The caller might need file output.", EvidenceIDs: []string{id}, Assessor: "reviewer-1", Rationale: "Keep the potential behavior gap visible.", SupersedesIDs: []string{}}},
		Resolutions:    []reviewResolution{},
		Executions:     []reviewExecution{{SchemaVersion: 1, ID: "execution-1", Route: route, PromptVersion: "prompt-v1", ToolVersion: "tool-v1", PolicyVersion: "policy-v1", Attempts: []reviewAttempt{{SchemaVersion: 1, ID: "attempt-1", Route: route, Status: "succeeded", Usage: executionUsage{Availability: "unavailable"}}}}},
	}
}

func gateStateTestValidate(t *testing.T, s reviewState) {
	t.Helper()
	if err := s.Validate(s.Subject, evidenceTestHashes(s.evidencePacket)); err != nil {
		t.Fatal(err)
	}
}

func TestGateStateRoundTrip(t *testing.T) {
	s := evidenceTestRoundTrip(t, gateStateTestState())
	gateStateTestValidate(t, s)
	// All answer states remain distinct from judgment and support. Incomplete
	// model work is representable and serializable, not discarded as no answer.
	for _, state := range []reviewAnswerState{reviewAnswerMissing, reviewAnswerAnswered, reviewAnswerInvalid, reviewAnswerUnavailable, reviewAnswerBudget, reviewAnswerNotApplicable} {
		t.Run(string(state), func(t *testing.T) {
			x := gateStateTestState()
			x.Answers[0].AnswerState = state
			x.Answers[0].MissingInformation = []string{"overwrite policy"}
			x.Answers[0].NextAction = "Ask the product owner."
			if state != reviewAnswerAnswered {
				x.Answers[0].Judgment, x.Answers[0].Support = "", nil
			}
			x = evidenceTestRoundTrip(t, x)
			gateStateTestValidate(t, x)
		})
	}
	for _, judgment := range []reviewJudgment{reviewJudgmentReuse, reviewJudgmentExtend, reviewJudgmentNew, reviewJudgmentUnclear, reviewJudgmentSatisfied, reviewJudgmentViolated} {
		x := gateStateTestState()
		x.Answers[0].Judgment = judgment
		gateStateTestValidate(t, evidenceTestRoundTrip(t, x))
	}
	for _, state := range []claimSupportState{claimSupportUnknown, claimSupported, claimUnsupported, claimSupportConflicting} {
		for _, method := range []string{"structural", "model", "human"} {
			x := gateStateTestState()
			x.Answers[0].Support.State, x.Answers[0].Support.Method = state, method
			gateStateTestValidate(t, evidenceTestRoundTrip(t, x))
		}
	}
	for _, delegated := range []bool{true, false} {
		for _, answered := range []bool{true, false} {
			x := gateStateTestState()
			x.Questions[0].Authority.Delegated = delegated
			if !answered {
				x.Questions[0].Answer, x.Questions[0].AnsweringActor = "", ""
			}
			gateStateTestValidate(t, evidenceTestRoundTrip(t, x))
		}
	}
}

func TestGateStateLocatorValidityIsNotClaimSupport(t *testing.T) {
	s := gateStateTestState()
	a := &s.Answers[0]
	a.Support = nil
	a.Rationale = "A model claims the located function implements the full requirement."
	a.MissingInformation = []string{"No behavioral support assessment was supplied."}
	a.NextAction = "Assess matching and missing behavior before accepting this claim."
	gateStateTestValidate(t, s)
	s = evidenceTestRoundTrip(t, s)
	if s.Answers[0].HasSupportAssessment() {
		t.Fatal("a valid, selected, rendered locator was promoted to claim support")
	}
	if s.Answers[0].AnswerState != reviewAnswerAnswered || s.Answers[0].Judgment != reviewJudgmentReuse || len(s.Answers[0].MissingInformation) != 1 || s.Answers[0].NextAction == "" {
		t.Fatal("unsupported model work disappeared instead of remaining visible and unresolved")
	}
	for _, state := range []claimSupportState{claimSupportUnknown, claimUnsupported, claimSupportConflicting} {
		x := gateStateTestState()
		x.Answers[0].Support.State = state
		gateStateTestValidate(t, x)
		if x.Answers[0].HasSupportAssessment() {
			t.Fatalf("%s promoted to positive support", state)
		}
	}
	// Unsupported findings cannot be persisted as accepted or silently resolved.
	for _, disposition := range []reviewDisposition{reviewAccepted, reviewResolved} {
		x := gateStateTestState()
		x.Findings[0].Disposition, x.Findings[0].AuthorityBasis = disposition, "policy-v1"
		if err := x.Validate(x.Subject, evidenceTestHashes(x.evidencePacket)); err == nil {
			t.Fatalf("unsupported %s finding accepted", disposition)
		}
	}
}

func TestGateStateAnswerEvidenceFreshnessAndIDs(t *testing.T) {
	cases := map[string]func(*reviewState){
		"duplicate-evidence-id": func(s *reviewState) {
			s.Answers[0].EvidenceIDs = append(s.Answers[0].EvidenceIDs, s.Answers[0].EvidenceIDs[0])
		},
		"unknown-evidence-id": func(s *reviewState) { s.Answers[0].EvidenceIDs = []string{"unknown"} },
		"stale-revision":      func(s *reviewState) { s.Answers[0].ItemRevision = "r-old" },
		"stale-subject": func(s *reviewState) {
			s.Evidence[0].SubjectID = evidenceTestSubject("old").ID
			s.Lookups[0].SubjectID = s.Evidence[0].SubjectID
		},
		"invalid-locator":          func(s *reviewState) { s.Evidence[0].LocatorValid = evidenceTestPtr(false) },
		"unknown-support-evidence": func(s *reviewState) { s.Answers[0].Support.EvidenceIDs = []string{"unknown"} },
		"duplicate-support-evidence": func(s *reviewState) {
			s.Answers[0].Support.EvidenceIDs = append(s.Answers[0].Support.EvidenceIDs, s.Evidence[0].ID)
		},
		"unattached-support-evidence": func(s *reviewState) { s.Answers[0].EvidenceIDs = []string{} },
		"support-without-assessor":    func(s *reviewState) { s.Answers[0].Support.Assessor = "" },
		"support-without-method":      func(s *reviewState) { s.Answers[0].Support.Method = "" },
		"support-without-rationale":   func(s *reviewState) { s.Answers[0].Support.Rationale = "" },
		"support-without-evidence":    func(s *reviewState) { s.Answers[0].Support.EvidenceIDs = nil },
		"support-version":             func(s *reviewState) { s.Answers[0].Support.SchemaVersion = 0 },
		"invalid-support-state":       func(s *reviewState) { s.Answers[0].Support.State = "verified" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := gateStateTestState()
			mutate(&s)
			if err := s.Validate(s.Subject, evidenceTestHashes(s.evidencePacket)); err == nil {
				t.Fatal("invalid answer binding or support accepted")
			}
		})
	}
	s := gateStateTestState()
	hashes := evidenceTestHashes(s.evidencePacket)
	hashes[s.Evidence[0].ID] = "new-current-content-hash"
	if err := s.Validate(s.Subject, hashes); err == nil {
		t.Fatal("answer with stale evidence hash accepted")
	}
	s.Revalidations = []evidenceRevalidation{{SchemaVersion: 1, ID: "rv", EvidenceID: s.Evidence[0].ID, FromSubjectID: s.Subject.ID, FromContentHash: s.Evidence[0].ContentHash, SubjectID: s.Subject.ID, ContentHash: hashes[s.Evidence[0].ID], LocatorValid: true, Assessor: "reviewer-2", Method: "model", Rationale: "Reread current range."}}
	s.Answers[0].Support = nil
	if err := s.Validate(s.Subject, hashes); err != nil {
		t.Fatal(err)
	}
	if s.Answers[0].HasSupportAssessment() {
		t.Fatal("revalidation invented semantic support")
	}
}

func TestGateStateExplicitResolutionsAndSupersession(t *testing.T) {
	for _, disposition := range []reviewDisposition{reviewProposed, reviewAccepted, reviewRejected, reviewResolved} {
		t.Run(string(disposition), func(t *testing.T) {
			s := gateStateTestState()
			f := &s.Findings[0]
			f.Disposition, f.AuthorityBasis, f.Support = disposition, "policy-v1", gateStateTestSupport(s.Evidence[0].ID)
			r := reviewResolution{SchemaVersion: 1, ID: "resolution-1", ItemID: f.ItemID, ItemRevision: f.ItemRevision, CheckID: f.CheckID, Disposition: disposition, FindingIDs: []string{f.ID}, EvidenceIDs: f.EvidenceIDs, Support: gateStateTestSupport(s.Evidence[0].ID), Assessor: "authorized-reviewer", AuthorityBasis: "policy-v1 delegated technical decisions", Rationale: "The requirement explicitly excludes file writes.", SupersedesIDs: []string{}}
			s.Resolutions = []reviewResolution{r}
			if disposition == reviewResolved {
				f.ResolutionID = r.ID
			}
			gateStateTestValidate(t, evidenceTestRoundTrip(t, s))
		})
	}
	s := gateStateTestState()
	f := s.Findings[0]
	f.ID, f.SupersedesIDs = "finding-2", []string{"finding-1"}
	s.Findings = append(s.Findings, f)
	gateStateTestValidate(t, evidenceTestRoundTrip(t, s))
	s.Findings[0].SupersedesIDs = []string{"finding-2"}
	if err := s.Validate(s.Subject, evidenceTestHashes(s.evidencePacket)); err == nil || !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("cyclic supersession accepted: %v", err)
	}
	for _, change := range []string{"missing-resolution", "unrelated-resolution", "unknown-finding", "duplicate-finding", "unknown-supersedes", "duplicate-supersedes", "resolution-cycle", "no-authority"} {
		t.Run(change, func(t *testing.T) {
			s := gateStateTestState()
			f := &s.Findings[0]
			f.Disposition, f.ResolutionID, f.AuthorityBasis, f.Support = reviewResolved, "resolution-1", "policy-v1", gateStateTestSupport(s.Evidence[0].ID)
			r := reviewResolution{SchemaVersion: 1, ID: "resolution-1", ItemID: f.ItemID, ItemRevision: f.ItemRevision, CheckID: f.CheckID, Disposition: reviewAccepted, FindingIDs: []string{f.ID}, EvidenceIDs: f.EvidenceIDs, Support: gateStateTestSupport(s.Evidence[0].ID), Assessor: "authorized-reviewer", AuthorityBasis: "policy-v1", Rationale: "Explicitly addressed the identified finding.", SupersedesIDs: []string{}}
			s.Resolutions = []reviewResolution{r}
			switch change {
			case "missing-resolution":
				f.ResolutionID = ""
			case "unrelated-resolution":
				s.Resolutions[0].Disposition = reviewProposed
			case "unknown-finding":
				s.Resolutions[0].FindingIDs = []string{"unknown"}
			case "duplicate-finding":
				s.Resolutions[0].FindingIDs = []string{f.ID, f.ID}
			case "unknown-supersedes":
				f.SupersedesIDs = []string{"unknown"}
			case "duplicate-supersedes":
				f.SupersedesIDs = []string{f.ID, f.ID}
			case "resolution-cycle":
				s.Resolutions[0].SupersedesIDs = []string{r.ID}
			case "no-authority":
				s.Resolutions[0].AuthorityBasis = ""
			}
			if err := s.Validate(s.Subject, evidenceTestHashes(s.evidencePacket)); err == nil {
				t.Fatal("invalid resolution/supersession accepted")
			}
		})
	}
}

func TestGateStateRejectsInvalidMembershipAndEnums(t *testing.T) {
	cases := map[string]func(*reviewState){
		"item-version":          func(s *reviewState) { s.Items[0].SchemaVersion = 2 },
		"duplicate-item":        func(s *reviewState) { s.Items = append(s.Items, s.Items[0]) },
		"item-without-revision": func(s *reviewState) { s.Items[0].Revision = "" },
		"duplicate-check":       func(s *reviewState) { s.Items[0].Checks = []string{"reuse", "reuse"} },
		"unknown-answer-item":   func(s *reviewState) { s.Answers[0].ItemID = "unknown" },
		"unknown-check":         func(s *reviewState) { s.Answers[0].CheckID = "unknown" },
		"duplicate-answer":      func(s *reviewState) { s.Answers = append(s.Answers, s.Answers[0]) },
		"unknown-answer-state":  func(s *reviewState) { s.Answers[0].AnswerState = "verified" },
		"unknown-judgment":      func(s *reviewState) { s.Answers[0].Judgment = "duplicate" },
		"missing-judgment":      func(s *reviewState) { s.Answers[0].Judgment = "" },
		"missing-rationale":     func(s *reviewState) { s.Answers[0].Rationale = "" },
		"unknown-question":      func(s *reviewState) { s.Answers[0].QuestionIDs = []string{"unknown"} },
		"duplicate-question":    func(s *reviewState) { s.Questions = append(s.Questions, s.Questions[0]) },
		"unrelated-question":    func(s *reviewState) { s.Questions[0].CheckID = "scope" },
		"unknown-question-item": func(s *reviewState) { s.Questions[0].ItemID = "unknown" },
		"no-authority":          func(s *reviewState) { s.Questions[0].Authority.Basis = "" },
		"no-impact":             func(s *reviewState) { s.Questions[0].Options[0].Impact = "" },
		"duplicate-option":      func(s *reviewState) { s.Questions[0].Options[1].ID = s.Questions[0].Options[0].ID },
		"unknown-option":        func(s *reviewState) { s.Questions[0].Answer = "unknown" },
		"no-answering-actor":    func(s *reviewState) { s.Questions[0].AnsweringActor = "" },
		"actor-without-answer":  func(s *reviewState) { s.Questions[0].Answer = "" },
		"duplicate-finding":     func(s *reviewState) { s.Findings = append(s.Findings, s.Findings[0]) },
		"unknown-finding-item":  func(s *reviewState) { s.Findings[0].ItemID = "unknown" },
		"invalid-disposition":   func(s *reviewState) { s.Findings[0].Disposition = "fixed" },
		"missing-claim":         func(s *reviewState) { s.Findings[0].Claim = "" },
		"missing-assessor":      func(s *reviewState) { s.Findings[0].Assessor = "" },
		"unknown-execution":     func(s *reviewState) { s.Answers[0].ExecutionID = "unknown" },
		"duplicate-execution":   func(s *reviewState) { s.Executions = append(s.Executions, s.Executions[0]) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := gateStateTestState()
			mutate(&s)
			if err := s.Validate(s.Subject, evidenceTestHashes(s.evidencePacket)); err == nil {
				t.Fatal("invalid state accepted")
			}
		})
	}
	// IDs and revisions remain strings at the model-reply boundary too.
	for _, data := range []string{`{"id":123}`, `{"item_id":123}`, `{"item_revision":123}`, `{"evidence_ids":[123]}`} {
		var a reviewAnswer
		if err := json.Unmarshal([]byte(data), &a); err == nil {
			t.Fatalf("numeric answer identifier accepted: %s", data)
		}
	}
}

func TestGateStateExecutionUsageRoundTrip(t *testing.T) {
	for _, availability := range []string{"available", "partial", "unavailable", "unknown"} {
		for _, status := range []string{"succeeded", "failed", "unavailable", "cancelled"} {
			s := gateStateTestState()
			a := &s.Executions[0].Attempts[0]
			a.Status = status
			if status != "succeeded" {
				a.Error = "Recorded failure, cancellation or unavailable route."
			}
			a.Usage.Availability = availability
			if availability == "available" || availability == "partial" {
				a.Usage.InputTokens = evidenceTestPtr(int64(0))
			}
			if availability == "available" {
				a.Usage.OutputTokens, a.Usage.CostUSD = evidenceTestPtr(int64(10)), evidenceTestPtr(0.0)
			}
			gateStateTestValidate(t, evidenceTestRoundTrip(t, s))
		}
	}
	s := gateStateTestState()
	fallback := s.Executions[0].Attempts[0]
	fallback.ID, fallback.Route.Model = "fallback-2", "different-model"
	s.Executions[0].Attempts[0].Status, s.Executions[0].Attempts[0].Error = "unavailable", "Provider unavailable."
	s.Executions[0].Attempts = append(s.Executions[0].Attempts, fallback)
	gateStateTestValidate(t, evidenceTestRoundTrip(t, s))
	if s.Executions[0].Attempts[1].Route.Model == s.Executions[0].Attempts[0].Route.Model {
		t.Fatal("fallback route lost attribution")
	}
	for name, mutate := range map[string]func(*reviewExecution){
		"missing-route":           func(e *reviewExecution) { e.Route.Model = "" },
		"missing-policy-version":  func(e *reviewExecution) { e.PolicyVersion = "" },
		"missing-tool-version":    func(e *reviewExecution) { e.ToolVersion = "" },
		"missing-prompt-version":  func(e *reviewExecution) { e.PromptVersion = "" },
		"duplicate-attempt":       func(e *reviewExecution) { e.Attempts = append(e.Attempts, e.Attempts[0]) },
		"attempt-version":         func(e *reviewExecution) { e.Attempts[0].SchemaVersion = 0 },
		"unknown-attempt-state":   func(e *reviewExecution) { e.Attempts[0].Status = "empty" },
		"failed-without-error":    func(e *reviewExecution) { e.Attempts[0].Status = "failed" },
		"success-with-error":      func(e *reviewExecution) { e.Attempts[0].Error = "failure" },
		"unknown-usage-enum":      func(e *reviewExecution) { e.Attempts[0].Usage.Availability = "estimated" },
		"unknown-is-not-zero":     func(e *reviewExecution) { e.Attempts[0].Usage.InputTokens = evidenceTestPtr(int64(0)) },
		"missing-available-usage": func(e *reviewExecution) { e.Attempts[0].Usage.Availability = "available" },
		"missing-partial-usage":   func(e *reviewExecution) { e.Attempts[0].Usage.Availability = "partial" },
		"negative-usage": func(e *reviewExecution) {
			e.Attempts[0].Usage = executionUsage{Availability: "partial", InputTokens: evidenceTestPtr(int64(-1))}
		},
		"negative-cost": func(e *reviewExecution) {
			e.Attempts[0].Usage = executionUsage{Availability: "partial", CostUSD: evidenceTestPtr(-1.0)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := gateStateTestState().Executions[0]
			mutate(&e)
			if err := e.Validate(); err == nil {
				t.Fatal("invalid execution accepted")
			}
		})
	}
}
