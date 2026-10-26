package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const gateStatusSchemaVersion = 2
const reviewReducerPolicyVersion = "review-reducer-v2"

type reviewReason string

const (
	reviewMissingAnswer         reviewReason = "missing_answer"
	reviewUnsupportedClaim      reviewReason = "unsupported_claim"
	reviewAwaitingDecision      reviewReason = "awaiting_decision"
	reviewDependencyUnavailable reviewReason = "dependency_unavailable"
	reviewBudgetExhausted       reviewReason = "budget_exhausted"
)

// Checks are derived by Go from the parsed spec and requested review jobs. They
// are not model-selected requirements. Authority is scoped to an identified check.
type reviewCheck struct {
	Item                 reviewItem `json:"item"`
	ID                   string     `json:"id"`
	Required             bool       `json:"required"`
	Actor                string     `json:"actor"`
	Assessors            []string   `json:"assessors"`
	DecisionOwner        string     `json:"decision_owner,omitempty"`
	DecisionDelegates    []string   `json:"decision_delegates,omitempty"`
	CanBlock             bool       `json:"can_block"`
	NotApplicableReasons []string   `json:"not_applicable_reasons,omitempty"`
	// SearchScope is a trusted repository-relative coverage target, not a
	// reviewer-selected path. New judgments need a search of this whole scope.
	SearchScope string `json:"search_scope,omitempty"`
	// SearchQuery is assigned by Go, never by the reviewer. Omission uses the
	// canonical behavior query derived from the trusted item's requirement.
	SearchQuery string `json:"search_query,omitempty"`
	// AllowedSearchExclusions are exact labels assigned by Go, not glob patterns.
	// Omission permits only the standard non-production classes below.
	AllowedSearchExclusions []string `json:"allowed_search_exclusions,omitempty"`
}

func (c reviewCheck) key() string { return c.Item.ID + "/" + c.ID }

type reviewNextAction struct {
	Check  string       `json:"check"`
	Reason reviewReason `json:"reason"`
	Actor  string       `json:"actor"`
	Action string       `json:"next_action"`
}
type reviewCompletion struct {
	Required   int `json:"required"`
	Completed  int `json:"completed"`
	Incomplete int `json:"incomplete"`
	Optional   int `json:"optional"`
}
type reviewAction struct {
	Actor  string `json:"actor"`
	Action string `json:"next_action"`
}
type reviewResult struct {
	SchemaVersion  int                `json:"schema_version"`
	State          string             `json:"state"`
	Completion     reviewCompletion   `json:"completion"`
	Reasons        []reviewNextAction `json:"reasons"`
	NextActors     []string           `json:"next_actors"`
	NextActions    []reviewAction     `json:"next_actions"`
	Failures       []string           `json:"failures"`
	Findings       []reviewFinding    `json:"findings"`
	ActiveBlockers []string           `json:"active_blockers"`
}

// Policy is explicit recorded input. CurrentHashes are trusted measurements,
// keyed by evidence ID, never values selected by a model reply.
type reviewPolicy struct {
	Version        string             `json:"version"`
	CurrentSubject *gateSubject       `json:"current_subject"`
	Packet         evidencePacket     `json:"packet"`
	CurrentHashes  map[string]string  `json:"current_hashes"`
	Questions      []reviewQuestion   `json:"questions"`
	Resolutions    []reviewResolution `json:"resolutions"`
	Executions     []reviewExecution  `json:"executions,omitempty"`
	SystemFailures []string           `json:"system_failures,omitempty"`
	Stale          bool               `json:"stale,omitempty"`
	Experiment     bool               `json:"experiment,omitempty"`
}

// ReduceReview performs no I/O, model calls or clock reads. Invalid system
// inputs are errors; malformed reviewer records remain incomplete claims. In
// particular, validating a locator never supplies the separate support assessment.
// A lookup-only producer must assign the requirement/query binding in Go code.
func ReduceReview(subject *gateSubject, requiredChecks []reviewCheck, answers []reviewAnswer, findings []reviewFinding, policy reviewPolicy) reviewResult {
	r := reviewResult{SchemaVersion: gateStatusSchemaVersion, State: "pass", Reasons: []reviewNextAction{}, NextActors: []string{}, Failures: []string{}, Findings: append([]reviewFinding{}, findings...), ActiveBlockers: []string{}}
	checks := append([]reviewCheck(nil), requiredChecks...)
	sort.Slice(checks, func(i, j int) bool { return checks[i].key() < checks[j].key() })
	index := map[string]reviewCheck{}
	if len(checks) == 0 {
		r.Failures = append(r.Failures, "required-check records are missing")
	}
	bindings := map[string]string{}
	for _, c := range checks {
		binding := subjectJSONDigest([]string{c.Item.Revision, c.Item.Package, c.Item.Requirement})
		if old, ok := bindings[c.Item.ID]; ok && old != binding {
			r.Failures = append(r.Failures, "conflicting item binding "+c.Item.ID)
		}
		bindings[c.Item.ID] = binding
		if err := validateUniqueReviewIDs("assessor", c.Assessors); err != nil {
			r.Failures = append(r.Failures, err.Error())
		}
		if c.Required {
			r.Completion.Required++
		} else {
			r.Completion.Optional++
		}
		if err := c.Item.Validate(); err != nil {
			r.Failures = append(r.Failures, err.Error())
		}
		if c.ID == "" || !slices.Contains(c.Item.Checks, c.ID) || c.Actor == "" || len(c.Assessors) == 0 {
			r.Failures = append(r.Failures, "invalid required check "+c.key())
		}
		if _, ok := index[c.key()]; ok {
			r.Failures = append(r.Failures, "duplicate required check "+c.key())
		}
		index[c.key()] = c
	}
	r.Failures = append(r.Failures, policy.SystemFailures...)
	if !subject.valid() || !policy.CurrentSubject.valid() {
		r.Failures = append(r.Failures, "review/current subject cannot be established")
	}
	if policy.Version != reviewReducerPolicyVersion {
		r.Failures = append(r.Failures, "unknown reducer policy")
	}
	if err := policy.Packet.Validate(); err != nil {
		r.Failures = append(r.Failures, "invalid evidence packet: "+err.Error())
	}
	if subject.valid() && (policy.Packet.Subject == nil || policy.Packet.Subject.ID != subject.ID) {
		r.Failures = append(r.Failures, "evidence packet does not identify reviewed subject")
	}
	// Validate against the reviewed binding first. Staleness of the current
	// subject has its own precedence and must not turn a valid old claim corrupt.
	addReason := func(c reviewCheck, reason reviewReason, action string) {
		if !c.Required {
			return
		}
		if action == "" {
			action = "Resume review of " + c.key()
		}
		r.Reasons = append(r.Reasons, reviewNextAction{c.key(), reason, c.Actor, action})
	}
	questionCounts := map[string]int{}
	for _, q := range policy.Questions {
		questionCounts[q.ID]++
	}
	executions := map[string]bool{}
	executionCounts := map[string]int{}
	for _, e := range policy.Executions {
		executionCounts[e.ID]++
		executions[e.ID] = e.Validate() == nil
	}
	byCheck := map[string][]reviewAnswer{}
	answerIDs := map[string]int{}
	for _, a := range answers {
		answerIDs[a.ID]++
		byCheck[a.ItemID+"/"+a.CheckID] = append(byCheck[a.ItemID+"/"+a.CheckID], a)
	}
	findingIDs := map[string]int{}
	for _, f := range findings {
		findingIDs[f.ID]++
	}
	for id, n := range findingIDs {
		if n > 1 {
			r.Failures = append(r.Failures, "duplicate finding ID "+id)
		}
	}
	unresolved := map[string]bool{}
	accepted := map[string]bool{}
	acceptedChecks := map[string]bool{}
	for _, f := range findings {
		c, ok := index[f.ItemID+"/"+f.CheckID]
		if !ok {
			r.Failures = append(r.Failures, "finding references unknown check: "+f.ID)
			continue
		}
		valid := findingIDs[f.ID] == 1 && validReviewFinding(f, c, policy, subject) == nil
		supported := valid && allowedSupport(f.Support, c) && f.Assessor == f.Support.Assessor && slices.Contains(c.Assessors, f.Assessor)
		if supported && (f.Disposition == reviewAccepted || f.Disposition == reviewResolved) {
			if c.CanBlock {
				accepted[f.ID] = true
			}
			acceptedChecks[c.key()] = true
		} else if f.Disposition != reviewRejected || !supported {
			unresolved[c.key()] = true
		}
	}
	// A newer verdict, rejected claim, or supersedes link cannot clear an accepted
	// blocker. Only an explicit authorized resolution identifying its evidence can.
	resolved := map[string]bool{}
	resolutionIDs := map[string]int{}
	for _, v := range policy.Resolutions {
		resolutionIDs[v.ID]++
	}
	for _, v := range policy.Resolutions {
		c, ok := index[v.ItemID+"/"+v.CheckID]
		if !ok {
			continue
		}
		if resolutionIDs[v.ID] != 1 || (v.Disposition != reviewAccepted && v.Disposition != reviewResolved) || !slices.Contains(c.Assessors, v.Assessor) || !allowedSupport(v.Support, c) || v.Support.Assessor != v.Assessor {
			continue
		}
		if validateEvidenceRow(v.SchemaVersion, v.ID, "") != nil || validateReviewItemLink(v.ItemID, v.ItemRevision, v.CheckID, c.Item) != nil || validateReviewDisposition(v.Disposition, v.Assessor, v.AuthorityBasis, v.Rationale, v.Support) != nil || policy.Packet.ValidateAttachment(v.EvidenceIDs, subject, policy.CurrentHashes) != nil || validateReviewSupport(v.Support, v.EvidenceIDs, policy.Packet.evidenceIndex()) != nil || len(v.FindingIDs) == 0 || validateUniqueReviewIDs("finding", v.FindingIDs) != nil {
			continue
		}
		for _, id := range v.FindingIDs {
			for _, f := range findings {
				if f.ID == id && f.ItemID == v.ItemID && f.ItemRevision == v.ItemRevision && f.CheckID == v.CheckID && accepted[id] {
					addressed := true
					for _, eid := range f.EvidenceIDs {
						if !slices.Contains(v.EvidenceIDs, eid) {
							addressed = false
						}
					}
					if addressed {
						resolved[id] = true
					}
				}
			}
		}
	}
	for id := range accepted {
		if !resolved[id] {
			r.ActiveBlockers = append(r.ActiveBlockers, id)
		}
	}
	for _, c := range checks {
		before := len(r.Reasons)
		as := byCheck[c.key()]
		if len(as) == 0 {
			addReason(c, reviewMissingAnswer, "")
		}
		judgments := map[reviewJudgment]bool{}
		validCount := 0
		for _, a := range as {
			linksValid := true
			if a.ExecutionID != "" && (!executions[a.ExecutionID] || executionCounts[a.ExecutionID] != 1) {
				linksValid = false
			}
			for _, qid := range a.QuestionIDs {
				found := false
				for _, q := range policy.Questions {
					if q.ID == qid && q.ItemID == a.ItemID && q.ItemRevision == a.ItemRevision && q.CheckID == a.CheckID && questionCounts[qid] == 1 && q.Validate(c.Item) == nil {
						found = true
					}
				}
				if !found {
					linksValid = false
				}
			}
			if !linksValid || answerIDs[a.ID] != 1 || a.Validate(c.Item, policy.Packet, subject, policy.CurrentHashes) != nil {
				addReason(c, reviewUnsupportedClaim, "Repair the invalid answer for "+c.key())
				continue
			}
			switch a.AnswerState {
			case reviewAnswerMissing, reviewAnswerInvalid:
				addReason(c, reviewMissingAnswer, a.NextAction)
			case reviewAnswerUnavailable:
				addReason(c, reviewDependencyUnavailable, a.NextAction)
			case reviewAnswerBudget:
				addReason(c, reviewBudgetExhausted, a.NextAction)
			case reviewAnswerNotApplicable:
				if !slices.Contains(c.NotApplicableReasons, a.Rationale) || !allowedSupport(a.Support, c) {
					addReason(c, reviewUnsupportedClaim, "Validate the not_applicable reason for "+c.key())
				} else {
					validCount++
				}
			case reviewAnswerAnswered:
				if !allowedSupport(a.Support, c) {
					addReason(c, reviewUnsupportedClaim, "Retrieve support for "+c.key())
					continue
				}
				if a.Judgment == reviewJudgmentUnclear {
					addReason(c, reviewAwaitingDecision, a.NextAction)
					continue
				}
				if a.Judgment == reviewJudgmentViolated {
					if acceptedChecks[c.key()] {
						validCount++
					} else {
						addReason(c, reviewUnsupportedClaim, "Record an accepted finding or resolution for "+c.key())
					}
					continue
				}
				if (a.Judgment == reviewJudgmentReuse || a.Judgment == reviewJudgmentExtend) && strings.TrimSpace(a.MatchingBehavior) == "" || a.Judgment == reviewJudgmentExtend && strings.TrimSpace(a.MissingBehavior) == "" {
					addReason(c, reviewUnsupportedClaim, "Describe matching and missing behavior for "+c.key())
					continue
				}
				if a.Judgment == reviewJudgmentNew && !adequateScopedReviewSearch(policy.Packet, c, a.LookupIDs, a.EvidenceIDs...) {
					addReason(c, reviewUnsupportedClaim, "Complete the recorded search before a new judgment")
					continue
				}
				validCount++
				judgments[a.Judgment] = true
			}
		}
		if len(judgments) > 1 || unresolved[c.key()] {
			addReason(c, reviewUnsupportedClaim, "Resolve conflicting or unsupported claims for "+c.key())
		}
		// Decisions are validated independently of a model claiming they are settled.
		for _, q := range policy.Questions {
			if q.ItemID != c.Item.ID || q.CheckID != c.ID {
				continue
			}
			if questionCounts[q.ID] != 1 || q.Validate(c.Item) != nil || q.Answer == "" || !authorizedReviewDecision(q, c) {
				addReason(c, reviewAwaitingDecision, "Ask the authorized owner to answer "+q.ID)
				if c.Required && len(r.Reasons) > 0 && c.DecisionOwner != "" {
					r.Reasons[len(r.Reasons)-1].Actor = c.DecisionOwner
				}
			}
		}
		if c.Required && validCount > 0 && len(r.Reasons) == before {
			r.Completion.Completed++
		}
	}
	r.Completion.Incomplete = r.Completion.Required - r.Completion.Completed
	if r.Completion.Incomplete > 0 {
		r.State = "incomplete"
	}
	if len(r.ActiveBlockers) > 0 {
		r.State = "fail"
	}
	if policy.Stale || subject.valid() && policy.CurrentSubject.valid() && len(gateSubjectChanges(subject, policy.CurrentSubject)) > 0 {
		r.State = "stale"
	}
	if len(r.Failures) > 0 {
		r.State = "error"
	}
	// Experiment is the existing non-approval run mode, not a readiness claim.
	// Preserve its legacy precedence while retaining blockers and unfinished work.
	if policy.Experiment && r.State != "error" && r.State != "stale" {
		r.State = "experiment"
	}
	sort.Slice(r.Reasons, func(i, j int) bool {
		a, b := r.Reasons[i], r.Reasons[j]
		return a.Check+string(a.Reason)+a.Actor+a.Action < b.Check+string(b.Reason)+b.Actor+b.Action
	})
	r.Reasons = slices.Compact(r.Reasons)
	for _, v := range r.Reasons {
		r.NextActions = append(r.NextActions, reviewAction{v.Actor, v.Action})
	}
	for _, id := range r.ActiveBlockers {
		r.NextActions = append(r.NextActions, reviewAction{"reviewer", "Resolve accepted finding " + id + " with an authorized evidence-linked resolution"})
	}
	if r.State == "stale" {
		r.NextActions = append(r.NextActions, reviewAction{"reviewer", "Review the changed subject"})
	}
	if r.State == "error" {
		for _, failure := range r.Failures {
			r.NextActions = append(r.NextActions, reviewAction{"operator", "Repair system data: " + failure})
		}
	}
	sort.Slice(r.NextActions, func(i, j int) bool {
		return r.NextActions[i].Actor+r.NextActions[i].Action < r.NextActions[j].Actor+r.NextActions[j].Action
	})
	r.NextActions = slices.Compact(r.NextActions)
	for _, v := range r.NextActions {
		if v.Actor != "" {
			r.NextActors = append(r.NextActors, v.Actor)
		}
	}
	sort.Strings(r.NextActors)
	r.NextActors = slices.Compact(r.NextActors)
	sort.Strings(r.Failures)
	r.Failures = slices.Compact(r.Failures)
	sort.Strings(r.ActiveBlockers)
	sort.Slice(r.Findings, func(i, j int) bool { return r.Findings[i].ID < r.Findings[j].ID })
	return r
}
func authorizedReviewDecision(q reviewQuestion, c reviewCheck) bool {
	// A question cannot grant its own owner or delegation merely by claiming it.
	if c.DecisionOwner == "" || q.Authority.Owner != c.DecisionOwner {
		return false
	}
	return q.AnsweringActor == c.DecisionOwner || q.Authority.Delegated && slices.Contains(c.DecisionDelegates, q.AnsweringActor)
}

func allowedSupport(s *claimSupport, c reviewCheck) bool {
	return s != nil && s.State == claimSupported && slices.Contains(c.Assessors, s.Assessor) && (s.Method != "structural" || s.Assessor == "go")
}
func validReviewFinding(f reviewFinding, c reviewCheck, p reviewPolicy, subject *gateSubject) error {
	if err := validateEvidenceRow(f.SchemaVersion, f.ID, ""); err != nil {
		return err
	}
	if strings.TrimSpace(f.Claim) == "" {
		return fmt.Errorf("missing finding claim")
	}
	if f.Support != nil && f.Support.State == claimSupported && f.Support.Method != "structural" && (strings.TrimSpace(f.Support.Excerpt) == "" || strings.TrimSpace(f.Support.FailureScenario) == "") {
		return fmt.Errorf("semantic finding requires excerpt and failure scenario")
	}
	if err := validateReviewItemLink(f.ItemID, f.ItemRevision, f.CheckID, c.Item); err != nil {
		return err
	}
	if err := validateReviewDisposition(f.Disposition, f.Assessor, f.AuthorityBasis, f.Rationale, f.Support); err != nil {
		return err
	}
	if err := p.Packet.ValidateAttachment(f.EvidenceIDs, subject, p.CurrentHashes); err != nil {
		return err
	}
	return validateReviewSupport(f.Support, f.EvidenceIDs, p.Packet.evidenceIndex())
}

// gateReviewRecords persists all inputs needed to replay the production decision.
// This is additive; old gate records remain readable but cannot authorize work.
type reviewAnswerRepair struct {
	Previous      reviewAnswer `json:"previous"`
	ReplacementID string       `json:"replacement_id"`
	Actor         string       `json:"actor"`
	Rationale     string       `json:"rationale"`
}

type gateReviewRecords struct {
	Repairs  []reviewAnswerRepair `json:"answer_repairs,omitempty"`
	Checks   []reviewCheck        `json:"required_checks"`
	Answers  []reviewAnswer       `json:"answers"`
	Findings []reviewFinding      `json:"findings"`
	Policy   reviewPolicy         `json:"policy"`
}

func initGateReview(rec *gateRecord, sp *spec, src string, passes []gatePass) {
	rec.Review = &gateReviewRecords{Policy: reviewPolicy{Version: reviewReducerPolicyVersion, CurrentSubject: rec.Subject, Packet: evidencePacket{evidenceEnvelope: evidenceEnvelope{evidenceSchemaVersion, rec.Subject, evidenceComplete}}, CurrentHashes: map[string]string{}}}
	add := func(id, pkg, text, revision string, checks []string, actor string) {
		item := reviewItem{evidenceSchemaVersion, id, revision, pkg, text, checks}
		assessors := []string{actor}
		if actor != "go" {
			assessors = append(assessors, "go")
		}
		for _, check := range checks {
			rec.Review.Checks = append(rec.Review.Checks, reviewCheck{Item: item, ID: check, Required: true, Actor: actor, Assessors: assessors, DecisionOwner: "owner", CanBlock: true, SearchScope: "."})
		}
	}
	add("@spec", "@spec", "Spec structure and ownership", subjectDigest([]byte(src)), []string{"structural"}, "go")
	recordGateAnswer(rec, "@spec", "structural", "go", src, reviewJudgmentSatisfied, "Go checked spec structure and ownership", "", "")
	for _, p := range passes {
		if ids, ok := rec.Groups[p.name]; ok {
			for _, id := range ids {
				for _, pkg := range sp.packages {
					if pkg.id == id {
						checks := []string{"scope", "questions", "reuse"}
						add(id+"@package", id, "Package scope, decisions and reuse", subjectDigest([]byte(packageBlocks(sp, src)[id])), checks, p.name)
						if pkg.kind == "scout" {
							for i := range rec.Review.Checks {
								c := &rec.Review.Checks[i]
								if c.Item.ID == id+"@package" && c.ID == "reuse" {
									c.NotApplicableReasons = []string{"scout_no_implementation"}
								}
							}
							recordGateAnswer(rec, id+"@package", "reuse", "go", pkg.raw, reviewJudgmentSatisfied, "scout_no_implementation", "", "")
							rec.Review.Answers[len(rec.Review.Answers)-1].AnswerState = reviewAnswerNotApplicable
						}
					}
				}
			}
		} else if p.apply != nil {
			add("@"+p.name, "@spec", "Cross-package review "+p.name, subjectDigest([]byte(src)), []string{p.name}, p.name)
		}
	}
}

func addGateObservation(rec *gateRecord, key, raw, actor string) string {
	g := rec.Review
	id := "observation-" + subjectJSONDigest([]string{key, raw, actor})
	for _, e := range g.Policy.Packet.Evidence {
		if e.ID == id {
			return id
		}
	}
	one := 1
	yes := true
	lookup := "lookup-" + id
	g.Policy.Packet.Lookups = append(g.Policy.Packet.Lookups, evidenceLookup{SchemaVersion: evidenceSchemaVersion, ID: lookup, Query: key, Scope: "recorded review", Unit: "review_reply", ExecutionStatus: lookupSucceeded, Status: "ok", CountKind: evidenceCountExact, Counts: lookupCounts{&one, &one, new(int), &one}})
	hash := subjectDigest([]byte(raw))
	g.Policy.Packet.Evidence = append(g.Policy.Packet.Evidence, evidenceRecord{SchemaVersion: evidenceSchemaVersion, ID: id, SourceKind: evidenceSourceToolOutput, Path: key, ContentHash: hash, LookupID: lookup, Selected: &yes, Rendered: &yes, Read: &yes, LocatorValid: &yes})
	g.Policy.CurrentHashes[id] = hash
	return id
}

// Capture trusted bytes separately from the model's support assessment.
func captureGateSource(rec *gateRecord, repo, locator, actor string) string {
	if rec.Review == nil || !evidenceHolds(repo, locator) {
		return ""
	}
	m := pointerRe.FindStringSubmatch(locator)
	if m == nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(repo, m[1]))
	if err != nil {
		return ""
	}
	id := addGateObservation(rec, "source:"+m[0], string(b), actor)
	for i := range rec.Review.Policy.Packet.Evidence {
		e := &rec.Review.Policy.Packet.Evidence[i]
		if e.ID == id {
			e.SourceKind = evidenceSourceCode
			e.Path = m[1]
			// A post-review Go read establishes source bytes, not model delivery/read.
			e.Rendered = nil
			e.Read = nil
			for j := range rec.Review.Policy.Packet.Lookups {
				l := &rec.Review.Policy.Packet.Lookups[j]
				if l.ID == e.LookupID {
					l.Unit = "source_read"
					l.Scope = m[1]
				}
			}
		}
	}
	return id
}

func recordGateAnswer(rec *gateRecord, itemID, check, actor, raw string, judgment reviewJudgment, rationale, matching, missing string) {
	if rec.Review == nil {
		return
	}
	for _, c := range rec.Review.Checks {
		if c.Item.ID != itemID || c.ID != check {
			continue
		}
		evidence := addGateObservation(rec, c.key(), raw, actor)
		a := reviewAnswer{SchemaVersion: evidenceSchemaVersion, ID: "answer-" + subjectJSONDigest([]string{c.key(), raw, actor}), ItemID: itemID, ItemRevision: c.Item.Revision, CheckID: check, AnswerState: reviewAnswerAnswered, Judgment: judgment, EvidenceIDs: []string{evidence}, Rationale: rationale, MatchingBehavior: matching, MissingBehavior: missing}
		method := "model"
		if actor == "go" {
			method = "structural"
		}
		a.Support = &claimSupport{SchemaVersion: evidenceSchemaVersion, State: claimSupported, Assessor: actor, Method: method, EvidenceIDs: a.EvidenceIDs, Rationale: rationale}
		for _, old := range rec.Review.Answers {
			if old.ID == a.ID {
				return
			}
		}
		rec.Review.Answers = append(rec.Review.Answers, a)
		return
	}
}

func recordGateGroup(rec *gateRecord, key, raw string, e groupEntry) {
	if rec.Review == nil {
		return
	}
	recordGateAnswer(rec, e.ID+"@package", "scope", key, raw, reviewJudgmentSatisfied, "Explicit scope and claims arrays were reviewed", "", "")
	recordGateAnswer(rec, e.ID+"@package", "questions", key, raw, reviewJudgmentSatisfied, "Explicit decision questions were reviewed", "", "")
	for _, c := range rec.Review.Checks {
		if c.Item.ID == e.ID+"@package" && c.ID == "reuse" && len(c.NotApplicableReasons) > 0 {
			return
		}
	}
	recordGateAnswer(rec, e.ID+"@package", "reuse", key, raw, reviewJudgmentSatisfied, "Explicit package reuse assessment", "", "")
}

// Normalize legacy findings without upgrading locator validity to semantic
// support. Structural ownership is established by Go; semantic blockers require
// a distinct attributable assessment and behavioral support, not confidence.
func prepareGateReview(rec *gateRecord, repo string) {
	if rec.Review == nil {
		return
	}
	g := rec.Review
	g.Findings = nil
	seenFindings := map[string]bool{}
	for i := range rec.Findings {
		f := &rec.Findings[i]
		if f.Severity != "blocker" {
			continue
		}
		keyItem, keyCheck := "@"+f.Source, f.Source
		if f.Source == "scope" {
			keyItem = strings.TrimPrefix(f.Where, "package ") + "@package"
			keyCheck = "scope"
		}
		for _, c := range g.Checks {
			if c.Item.ID != keyItem || c.ID != keyCheck {
				continue
			}
			actor := f.Source
			if f.Support == "structural" {
				actor = "go"
			}
			id := "finding-" + subjectJSONDigest([]any{c.key(), f.Claim, f.Evidence, f.Source, f.Why, f.Assessment})
			rf := reviewFinding{SchemaVersion: evidenceSchemaVersion, ID: id, ItemID: c.Item.ID, ItemRevision: c.Item.Revision, CheckID: c.ID, Disposition: reviewProposed, Claim: f.Claim, Assessor: actor, Rationale: f.Why}
			if rf.Rationale == "" {
				rf.Rationale = f.Claim
			}
			if actor == "go" {
				eid := addGateObservation(rec, c.key(), f.Claim+"\n"+f.Evidence, "go")
				rf.EvidenceIDs = []string{eid}
				rf.Support = &claimSupport{SchemaVersion: evidenceSchemaVersion, State: claimSupported, Assessor: "go", Method: "structural", EvidenceIDs: rf.EvidenceIDs, Rationale: f.Claim}
				rf.Disposition = reviewAccepted
				rf.AuthorityBasis = reviewReducerPolicyVersion
			} else if f.Assessment != nil && f.Assessment.SchemaVersion == evidenceSchemaVersion && f.Assessment.State == claimSupported && strings.TrimSpace(f.Assessment.Rationale) != "" && f.Verified && strings.TrimSpace(f.Assessment.Excerpt) != "" && strings.TrimSpace(f.Assessment.FailureScenario) != "" {
				// Read exact source bytes at the I/O boundary, before pure reduction.
				if m := pointerRe.FindStringSubmatch(f.Evidence); m != nil {
					if b, err := os.ReadFile(filepath.Join(repo, m[1])); err == nil && strings.Contains(string(b), f.Assessment.Excerpt) {
						eid := captureGateSource(rec, repo, m[0], actor)
						rf.EvidenceIDs = []string{eid}
						support := *f.Assessment
						support.Assessor = actor
						support.Method = "model"
						support.EvidenceIDs = rf.EvidenceIDs
						rf.Support = &support
						rf.Disposition = reviewAccepted
						rf.AuthorityBasis = reviewReducerPolicyVersion
					}
				}
			}
			f.ID = id
			if !seenFindings[id] {
				g.Findings = append(g.Findings, rf)
				seenFindings[id] = true
			}
		}
	}
	for _, d := range rec.Deterministic {
		if !advisory(d) {
			c := g.Checks[0]
			eid := addGateObservation(rec, c.key(), d, "go")
			g.Findings = append(g.Findings, reviewFinding{SchemaVersion: evidenceSchemaVersion, ID: "structural-" + subjectDigest([]byte(d)), ItemID: c.Item.ID, ItemRevision: c.Item.Revision, CheckID: c.ID, Disposition: reviewAccepted, Claim: d, Assessor: "go", AuthorityBasis: reviewReducerPolicyVersion, Rationale: d, EvidenceIDs: []string{eid}, Support: &claimSupport{SchemaVersion: evidenceSchemaVersion, State: claimSupported, Assessor: "go", Method: "structural", EvidenceIDs: []string{eid}, Rationale: d}})
		}
	}
	// Every consequential unanswered question remains a required decision. A code
	// citation can identify an existing answer, but a model cannot invent an owner.
	g.Policy.Questions = nil
	for i, q := range rec.Questions {
		if q.blocks() {
			for _, c := range g.Checks {
				if c.Item.Package == q.Package && c.ID == "questions" {
					g.Policy.Questions = append(g.Policy.Questions, reviewQuestion{SchemaVersion: evidenceSchemaVersion, ID: fmt.Sprintf("question-%d", i+1), ItemID: c.Item.ID, ItemRevision: c.Item.Revision, CheckID: c.ID, Question: q.Question, Authority: reviewAuthority{Owner: "owner", Basis: "consequential public behavior"}, Options: []reviewQuestionOption{{"resolve", "Resolve the decision", "Spec behavior is fixed"}, {"revise", "Revise the spec", "Review the changed subject"}}})
				}
			}
		}
	}
}

// Apply the single readiness decision after all observation I/O and the final
// checkout/subject checks. Preparation never assigns status.
func reduceGateRecord(rec *gateRecord) {
	if rec.Review == nil {
		return
	}
	g := rec.Review
	// Execution failures are recorded as incomplete answers, never system errors.
	for name, msg := range rec.PassErrors {
		for _, c := range g.Checks {
			if name == "all" && c.Actor != "go" || c.Actor == name {
				state := rec.PassAnswerStates[name]
				if state == "" {
					state = reviewAnswerUnavailable
				}
				a := reviewAnswer{SchemaVersion: evidenceSchemaVersion, ID: "execution-" + subjectJSONDigest([]string{c.key(), name, msg}), ItemID: c.Item.ID, ItemRevision: c.Item.Revision, CheckID: c.ID, AnswerState: state, Rationale: msg, NextAction: "Resume " + c.Actor + ": " + msg}
				exists := false
				for _, old := range g.Answers {
					if old.ID == a.ID {
						exists = true
					}
				}
				if !exists {
					g.Answers = append(g.Answers, a)
				}
			}
		}
	}
	g.Policy.Stale = len(rec.SubjectStale) > 0 || len(rec.TreeChanged) > 0
	g.Policy.Experiment = rec.Experiment != ""
	g.Policy.SystemFailures = nil
	if rec.TreeCheckError != "" {
		g.Policy.SystemFailures = []string{rec.TreeCheckError}
	}
	result := ReduceReview(rec.Subject, g.Checks, g.Answers, g.Findings, g.Policy)
	rec.SchemaVersion = gateStatusSchemaVersion
	rec.State = result.State
	rec.Completion = result.Completion
	rec.NextActors = result.NextActors
	rec.NextActions = result.NextActions
	rec.Reduction = &result
	for i := range rec.Findings {
		rec.Findings[i].Accepted = slices.Contains(result.ActiveBlockers, rec.Findings[i].ID)
	}
	for _, reason := range result.Reasons {
		rec.Reasons = append(rec.Reasons, string(reason.Reason)+": "+reason.Check+" ("+reason.Actor+"): "+reason.Action)
	}
	rec.Reasons = append(rec.Reasons, result.Failures...)
	sort.Strings(rec.Reasons)
	rec.Reasons = slices.Compact(rec.Reasons)
}

// Legacy item-reply validation uses the repository-wide policy target, so a
// narrower lookup cannot cover it. Scoped checks also accept an ancestor lookup;
// containment is path-component-aware, never a lexical prefix match.
func adequateReviewSearch(p evidencePacket, ids ...string) bool {
	return adequateScopedReviewSearch(p, reviewCheck{SearchScope: "."}, nil, ids...)
}

// adequateScopedReviewSearch checks recorded search coverage, not reviewer prose.
// A future lookup-only producer must set the check's query binding in Go code.
func adequateScopedReviewSearch(p evidencePacket, c reviewCheck, lookupIDs []string, ids ...string) bool {
	target, ok := reviewSearchScope(c.SearchScope)
	query := c.SearchQuery
	if query == "" && c.Item.ID != "" {
		query = "Code that implements " + c.Item.Requirement
	}
	if !ok || p.Completeness != evidenceComplete {
		return false
	}
	n := 0
	attached := map[string]bool{}
	for _, id := range lookupIDs {
		attached[id] = true
	}
	for _, e := range p.Evidence {
		if slices.Contains(ids, e.ID) {
			attached[e.LookupID] = true
		}
	}
	for _, l := range p.Lookups {
		// Tool output observations establish reply validity, not repository search.
		if !attached[l.ID] {
			continue
		}
		if l.Unit == "review_reply" || l.Unit == "source_read" {
			// Such observations may accompany valid search coverage, but may
			// not themselves be cited as search coverage via lookup_ids.
			if slices.Contains(lookupIDs, l.ID) {
				return false
			}
			continue
		}
		if query != "" && l.Query != query {
			return false
		}
		allowed := c.AllowedSearchExclusions
		if len(allowed) == 0 {
			// Standard non-production classes, not arbitrary paths or globs.
			allowed = []string{"tests", "generated", "vendor"}
		}
		for _, exclusion := range l.Exclusions {
			if exclusion == "everything" || !slices.Contains(allowed, exclusion) {
				return false
			}
		}
		if len(ids) == 0 && (l.Status != "empty" || l.Counts.Total == nil || *l.Counts.Total != 0 || l.CountKind != evidenceCountExact) {
			return false
		}
		actual, valid := reviewSearchScope(l.Scope)
		if !valid || (actual != "." && actual != target && !strings.HasPrefix(target, actual+"/")) {
			return false
		}
		n++
		if l.ExecutionStatus != lookupSucceeded || (l.Status != "ok" && l.Status != "empty") || l.CountKind != evidenceCountExact || effectiveCompleteness(l.Completeness, p.evidenceEnvelope) != evidenceComplete || len(l.Omissions) > 0 || len(l.Errors) > 0 || effectiveSubjectID(l.SubjectID, p.evidenceEnvelope) != p.Subject.ID {
			return false
		}
	}
	return n > 0
}

func reviewSearchScope(scope string) (string, bool) {
	if strings.TrimSpace(scope) == "" {
		scope = "." // Historical checks omitted the repository-wide default.
	}
	if strings.ContainsAny(scope, "\\\\*?[]") || filepath.IsAbs(scope) {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(scope))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}
