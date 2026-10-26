package main

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Review-side records only. Policy acceptance, overall status and ReduceReview
// deliberately do not live in B1. Legacy gateItem/gateItemResult are unchanged.
type reviewItem struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Revision      string   `json:"revision"`
	Package       string   `json:"package"`
	Requirement   string   `json:"requirement"`
	Checks        []string `json:"checks"`
}

func (i reviewItem) Validate() error {
	if err := validateEvidenceRow(i.SchemaVersion, i.ID, ""); err != nil {
		return err
	}
	if strings.TrimSpace(i.Revision) == "" || strings.TrimSpace(i.Package) == "" || strings.TrimSpace(i.Requirement) == "" {
		return fmt.Errorf("item %s requires revision, package and requirement", i.ID)
	}
	return validateUniqueReviewIDs("check", i.Checks)
}

type reviewAnswerState string

const (
	reviewAnswerMissing       reviewAnswerState = "missing"
	reviewAnswerAnswered      reviewAnswerState = "answered"
	reviewAnswerInvalid       reviewAnswerState = "invalid"
	reviewAnswerUnavailable   reviewAnswerState = "unavailable"
	reviewAnswerBudget        reviewAnswerState = "budget_exhausted"
	reviewAnswerNotApplicable reviewAnswerState = "not_applicable"
)

type reviewJudgment string

const (
	reviewJudgmentReuse     reviewJudgment = "reuse"
	reviewJudgmentExtend    reviewJudgment = "extend"
	reviewJudgmentNew       reviewJudgment = "new"
	reviewJudgmentUnclear   reviewJudgment = "unclear"
	reviewJudgmentSatisfied reviewJudgment = "satisfied"
	reviewJudgmentViolated  reviewJudgment = "violated"
)

type reviewAnswer struct {
	SchemaVersion      int               `json:"schema_version"`
	ID                 string            `json:"id"`
	ItemID             string            `json:"item_id"`
	ItemRevision       string            `json:"item_revision"`
	CheckID            string            `json:"check_id"`
	AnswerState        reviewAnswerState `json:"answer_state"`
	Judgment           reviewJudgment    `json:"judgment,omitempty"`
	EvidenceIDs        []string          `json:"evidence_ids"`
	LookupIDs          []string          `json:"lookup_ids,omitempty"` // search coverage, including zero-result searches
	Support            *claimSupport     `json:"support,omitempty"`
	Rationale          string            `json:"rationale"`
	MissingInformation []string          `json:"missing_information"`
	MatchingBehavior   string            `json:"matching_behavior,omitempty"`
	MissingBehavior    string            `json:"missing_behavior,omitempty"`
	NextAction         string            `json:"next_action,omitempty"`
	QuestionIDs        []string          `json:"question_ids"`
	ExecutionID        string            `json:"execution_id,omitempty"`
}

// Validate retains structurally valid but unsupported claims as answers. It
// rejects stale source attachments even for an incomplete answer. A current
// locator alone does not supply the separate Support field.
func (a reviewAnswer) Validate(item reviewItem, packet evidencePacket, current *gateSubject, hashes map[string]string) error {
	if err := validateEvidenceRow(a.SchemaVersion, a.ID, ""); err != nil {
		return err
	}
	if err := validateReviewItemLink(a.ItemID, a.ItemRevision, a.CheckID, item); err != nil {
		return err
	}
	if err := validateEvidenceEnum("answer_state", a.AnswerState, reviewAnswerMissing, reviewAnswerAnswered, reviewAnswerInvalid, reviewAnswerUnavailable, reviewAnswerBudget, reviewAnswerNotApplicable); err != nil {
		return err
	}
	if a.Judgment != "" {
		if err := validateEvidenceEnum("judgment", a.Judgment, reviewJudgmentReuse, reviewJudgmentExtend, reviewJudgmentNew, reviewJudgmentUnclear, reviewJudgmentSatisfied, reviewJudgmentViolated); err != nil {
			return err
		}
	}
	if (a.AnswerState == reviewAnswerAnswered && a.Judgment == "") || ((a.AnswerState == reviewAnswerAnswered || a.AnswerState == reviewAnswerNotApplicable) && strings.TrimSpace(a.Rationale) == "") {
		return fmt.Errorf("answer %s requires judgment/rationale for its answer state", a.ID)
	}
	if err := packet.ValidateAttachment(a.EvidenceIDs, current, hashes); err != nil {
		return err
	}
	if len(a.LookupIDs) > 0 {
		if err := packet.ValidateLookupAttachment(a.LookupIDs, current); err != nil {
			return err
		}
	}
	if a.Judgment == reviewJudgmentNew && a.Support != nil && len(a.Support.EvidenceIDs) == 0 && len(a.LookupIDs) > 0 {
		if err := a.Support.ValidateSearch(packet, a.LookupIDs, current); err != nil {
			return err
		}
	} else if err := validateReviewSupport(a.Support, a.EvidenceIDs, packet.evidenceIndex()); err != nil {
		return err
	}
	return validateUniqueReviewIDs("question", a.QuestionIDs)
}

// HasSupportAssessment reports only the presence of explicit positive support.
// It does NOT establish authority, resolve conflicts, approve a not_applicable
// exemption, or mark a check complete. Those are versioned review-policy rules.
func (a reviewAnswer) HasSupportAssessment() bool {
	return a.Support != nil && a.Support.State == claimSupported
}

type reviewAuthority struct {
	Owner     string `json:"owner"`
	Delegated bool   `json:"delegated"`
	Basis     string `json:"basis"`
}

type reviewQuestionOption struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Impact string `json:"impact"`
}

type reviewQuestion struct {
	SchemaVersion  int                    `json:"schema_version"`
	ID             string                 `json:"id"`
	ItemID         string                 `json:"item_id"`
	ItemRevision   string                 `json:"item_revision"`
	CheckID        string                 `json:"check_id"`
	Question       string                 `json:"question"`
	Options        []reviewQuestionOption `json:"options"`
	Authority      reviewAuthority        `json:"authority"`
	Answer         string                 `json:"answer,omitempty"` // selected option ID
	AnsweringActor string                 `json:"answering_actor,omitempty"`
}

func (q reviewQuestion) Validate(item reviewItem) error {
	if err := validateEvidenceRow(q.SchemaVersion, q.ID, ""); err != nil {
		return err
	}
	if err := validateReviewItemLink(q.ItemID, q.ItemRevision, q.CheckID, item); err != nil {
		return err
	}
	if strings.TrimSpace(q.Question) == "" || strings.TrimSpace(q.Authority.Owner) == "" || strings.TrimSpace(q.Authority.Basis) == "" || len(q.Options) < 2 {
		return fmt.Errorf("question %s requires question, options and recorded authority", q.ID)
	}
	var ids []string
	for _, o := range q.Options {
		if strings.TrimSpace(o.Text) == "" || strings.TrimSpace(o.Impact) == "" {
			return fmt.Errorf("question %s option requires text and impact", q.ID)
		}
		ids = append(ids, o.ID)
	}
	if err := validateUniqueReviewIDs("option", ids); err != nil {
		return err
	}
	if q.Answer == "" && q.AnsweringActor != "" || q.Answer != "" && (strings.TrimSpace(q.AnsweringActor) == "" || !containsReviewID(ids, q.Answer)) {
		return fmt.Errorf("question %s answer requires a known option and answering actor", q.ID)
	}
	return nil
}

type reviewDisposition string

const (
	reviewProposed reviewDisposition = "proposed"
	reviewAccepted reviewDisposition = "accepted"
	reviewRejected reviewDisposition = "rejected"
	reviewResolved reviewDisposition = "resolved"
)

type reviewFinding struct {
	SchemaVersion  int               `json:"schema_version"`
	ID             string            `json:"id"`
	ItemID         string            `json:"item_id"`
	ItemRevision   string            `json:"item_revision"`
	CheckID        string            `json:"check_id"`
	Disposition    reviewDisposition `json:"disposition"`
	Claim          string            `json:"claim"`
	EvidenceIDs    []string          `json:"evidence_ids"`
	Support        *claimSupport     `json:"support,omitempty"`
	Assessor       string            `json:"assessor"`
	AuthorityBasis string            `json:"authority_basis"`
	Rationale      string            `json:"rationale"`
	ResolutionID   string            `json:"resolution_id,omitempty"`
	SupersedesIDs  []string          `json:"supersedes_ids"`
}

type reviewResolution struct {
	SchemaVersion  int               `json:"schema_version"`
	ID             string            `json:"id"`
	ItemID         string            `json:"item_id"`
	ItemRevision   string            `json:"item_revision"`
	CheckID        string            `json:"check_id"`
	Disposition    reviewDisposition `json:"disposition"`
	FindingIDs     []string          `json:"finding_ids"`
	EvidenceIDs    []string          `json:"evidence_ids"`
	Support        *claimSupport     `json:"support,omitempty"`
	Assessor       string            `json:"assessor"`
	AuthorityBasis string            `json:"authority_basis"`
	Rationale      string            `json:"rationale"`
	SupersedesIDs  []string          `json:"supersedes_ids"`
}

func validateReviewDisposition(d reviewDisposition, assessor, basis, rationale string, support *claimSupport) error {
	if err := validateEvidenceEnum("disposition", d, reviewProposed, reviewAccepted, reviewRejected, reviewResolved); err != nil {
		return err
	}
	if strings.TrimSpace(assessor) == "" || strings.TrimSpace(rationale) == "" {
		return fmt.Errorf("disposition requires assessor and rationale")
	}
	if d != reviewProposed && strings.TrimSpace(basis) == "" {
		return fmt.Errorf("non-proposed disposition requires authority basis")
	}
	if (d == reviewAccepted || d == reviewResolved) && (support == nil || support.State != claimSupported) {
		return fmt.Errorf("accepted/resolved disposition requires explicit support")
	}
	return nil
}

type executionRoute struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Harness  string `json:"harness"`
	Role     string `json:"role,omitempty"`
}

func (r executionRoute) Validate() error {
	if strings.TrimSpace(r.Provider) == "" || strings.TrimSpace(r.Model) == "" || strings.TrimSpace(r.Harness) == "" {
		return fmt.Errorf("execution requires provider, model and harness route")
	}
	return nil
}

type executionUsage struct {
	Availability string   `json:"availability"` // available, partial, unavailable, unknown
	InputTokens  *int64   `json:"input_tokens"`
	OutputTokens *int64   `json:"output_tokens"`
	CostUSD      *float64 `json:"cost_usd"`
}

func (u executionUsage) Validate() error {
	if err := validateEvidenceEnum("usage availability", u.Availability, "available", "partial", "unavailable", "unknown"); err != nil {
		return err
	}
	for _, n := range []*int64{u.InputTokens, u.OutputTokens} {
		if n != nil && *n < 0 {
			return fmt.Errorf("negative token usage")
		}
	}
	if u.CostUSD != nil && (*u.CostUSD < 0 || math.IsNaN(*u.CostUSD) || math.IsInf(*u.CostUSD, 0)) {
		return fmt.Errorf("invalid usage cost")
	}
	if (u.Availability == "unknown" || u.Availability == "unavailable") && (u.InputTokens != nil || u.OutputTokens != nil || u.CostUSD != nil) {
		return fmt.Errorf("unavailable/unknown usage cannot contain measurements")
	}
	if u.Availability == "available" && (u.InputTokens == nil || u.OutputTokens == nil) {
		return fmt.Errorf("available usage requires input and output token measurements")
	}
	if u.Availability == "partial" && u.InputTokens == nil && u.OutputTokens == nil && u.CostUSD == nil {
		return fmt.Errorf("partial usage requires at least one measurement")
	}
	return nil
}

type reviewAttempt struct {
	SchemaVersion int            `json:"schema_version"`
	ID            string         `json:"id"`
	Route         executionRoute `json:"route"`
	Status        string         `json:"status"` // succeeded, failed, unavailable, cancelled
	Error         string         `json:"error,omitempty"`
	Usage         executionUsage `json:"usage"`
}

type reviewExecution struct {
	SchemaVersion int             `json:"schema_version"`
	ID            string          `json:"id"`
	Route         executionRoute  `json:"route"`
	PromptVersion string          `json:"prompt_version"`
	ToolVersion   string          `json:"tool_version"`
	PolicyVersion string          `json:"policy_version"`
	Attempts      []reviewAttempt `json:"attempts"`
}

func (e reviewExecution) Validate() error {
	if err := validateEvidenceRow(e.SchemaVersion, e.ID, ""); err != nil {
		return err
	}
	if err := e.Route.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(e.PromptVersion) == "" || strings.TrimSpace(e.ToolVersion) == "" || strings.TrimSpace(e.PolicyVersion) == "" {
		return fmt.Errorf("execution %s requires prompt/tool/policy versions", e.ID)
	}
	var ids []string
	for _, a := range e.Attempts {
		if err := validateEvidenceRow(a.SchemaVersion, a.ID, ""); err != nil {
			return err
		}
		ids = append(ids, a.ID)
		if err := a.Route.Validate(); err != nil {
			return err
		}
		if err := validateEvidenceEnum("attempt status", a.Status, "succeeded", "failed", "unavailable", "cancelled"); err != nil {
			return err
		}
		if (a.Status == "succeeded") != (a.Error == "") {
			return fmt.Errorf("attempt %s status and error disagree", a.ID)
		}
		if err := a.Usage.Validate(); err != nil {
			return err
		}
	}
	return validateUniqueReviewIDs("attempt", ids)
}

// Review records share the evidence envelope's subject and completeness. This
// validator checks references and freshness, not authority-policy eligibility,
// whether searches were adequate for a 'new' judgment, or overall gate status.
type reviewState struct {
	evidencePacket
	Items       []reviewItem       `json:"items"`
	Answers     []reviewAnswer     `json:"answers"`
	Questions   []reviewQuestion   `json:"questions"`
	Findings    []reviewFinding    `json:"findings"`
	Resolutions []reviewResolution `json:"resolutions"`
	Executions  []reviewExecution  `json:"executions"`
}

func (s reviewState) Validate(current *gateSubject, hashes map[string]string) error {
	if err := s.evidencePacket.ValidateAttachment(nil, current, hashes); err != nil {
		return err
	}
	items := map[string]reviewItem{}
	for _, i := range s.Items {
		if err := i.Validate(); err != nil {
			return err
		}
		if _, ok := items[i.ID]; ok {
			return fmt.Errorf("duplicate item ID %q", i.ID)
		}
		items[i.ID] = i
	}
	questions := map[string]reviewQuestion{}
	for _, q := range s.Questions {
		if err := q.Validate(items[q.ItemID]); err != nil {
			return err
		}
		if _, ok := questions[q.ID]; ok {
			return fmt.Errorf("duplicate question ID %q", q.ID)
		}
		questions[q.ID] = q
	}
	executions := map[string]bool{}
	for _, e := range s.Executions {
		if err := e.Validate(); err != nil {
			return err
		}
		if executions[e.ID] {
			return fmt.Errorf("duplicate execution ID %q", e.ID)
		}
		executions[e.ID] = true
	}
	answerIDs := map[string]bool{}
	for _, a := range s.Answers {
		if err := a.Validate(items[a.ItemID], s.evidencePacket, current, hashes); err != nil {
			return err
		}
		if answerIDs[a.ID] {
			return fmt.Errorf("duplicate answer ID %q", a.ID)
		}
		answerIDs[a.ID] = true
		if a.ExecutionID != "" && !executions[a.ExecutionID] {
			return fmt.Errorf("unknown answer execution ID %q", a.ExecutionID)
		}
		for _, id := range a.QuestionIDs {
			q, ok := questions[id]
			if !ok || q.ItemID != a.ItemID || q.CheckID != a.CheckID {
				return fmt.Errorf("unknown or unrelated answer question ID %q", id)
			}
		}
	}
	findings := map[string]reviewFinding{}
	for _, f := range s.Findings {
		if err := validateEvidenceRow(f.SchemaVersion, f.ID, ""); err != nil {
			return err
		}
		if _, ok := findings[f.ID]; ok {
			return fmt.Errorf("duplicate finding ID %q", f.ID)
		}
		if strings.TrimSpace(f.Claim) == "" {
			return fmt.Errorf("finding %s requires a claim", f.ID)
		}
		if err := s.validateDispositionRecord(f.ItemID, f.ItemRevision, f.CheckID, items, f.Disposition, f.Assessor, f.AuthorityBasis, f.Rationale, f.Support, f.EvidenceIDs, current, hashes); err != nil {
			return err
		}
		findings[f.ID] = f
	}
	resolutions := map[string]reviewResolution{}
	for _, r := range s.Resolutions {
		if err := validateEvidenceRow(r.SchemaVersion, r.ID, ""); err != nil {
			return err
		}
		if _, ok := resolutions[r.ID]; ok {
			return fmt.Errorf("duplicate resolution ID %q", r.ID)
		}
		if err := s.validateDispositionRecord(r.ItemID, r.ItemRevision, r.CheckID, items, r.Disposition, r.Assessor, r.AuthorityBasis, r.Rationale, r.Support, r.EvidenceIDs, current, hashes); err != nil {
			return err
		}
		if len(r.FindingIDs) == 0 {
			return fmt.Errorf("resolution %s must explicitly address findings", r.ID)
		}
		if err := validateUniqueReviewIDs("finding", r.FindingIDs); err != nil {
			return err
		}
		for _, id := range r.FindingIDs {
			f, ok := findings[id]
			if !ok || f.ItemID != r.ItemID || f.CheckID != r.CheckID {
				return fmt.Errorf("unknown or unrelated resolution finding ID %q", id)
			}
		}
		resolutions[r.ID] = r
	}
	findingLinks, resolutionLinks := map[string][]string{}, map[string][]string{}
	for _, f := range s.Findings {
		findingLinks[f.ID] = f.SupersedesIDs
		for _, id := range f.SupersedesIDs {
			old, ok := findings[id]
			if !ok || old.ItemID != f.ItemID || old.CheckID != f.CheckID {
				return fmt.Errorf("unknown or unrelated superseded finding ID %q", id)
			}
		}
		if f.Disposition == reviewResolved && f.ResolutionID == "" {
			return fmt.Errorf("resolved finding %s requires an explicit resolution ID", f.ID)
		}
		if f.ResolutionID != "" {
			r, ok := resolutions[f.ResolutionID]
			if !ok || !containsReviewID(r.FindingIDs, f.ID) || (f.Disposition == reviewResolved && r.Disposition != reviewAccepted && r.Disposition != reviewResolved) {
				return fmt.Errorf("finding %s has an invalid resolution link", f.ID)
			}
		}
	}
	for _, r := range s.Resolutions {
		resolutionLinks[r.ID] = r.SupersedesIDs
		for _, id := range r.SupersedesIDs {
			old, ok := resolutions[id]
			if !ok || old.ItemID != r.ItemID || old.CheckID != r.CheckID {
				return fmt.Errorf("unknown or unrelated superseded resolution ID %q", id)
			}
		}
	}
	if err := validateReviewSupersession(findingLinks); err != nil {
		return err
	}
	return validateReviewSupersession(resolutionLinks)
}

func (s reviewState) validateDispositionRecord(itemID, revision, check string, items map[string]reviewItem, disposition reviewDisposition, assessor, basis, rationale string, support *claimSupport, ids []string, current *gateSubject, hashes map[string]string) error {
	if err := validateReviewItemLink(itemID, revision, check, items[itemID]); err != nil {
		return err
	}
	if err := validateReviewDisposition(disposition, assessor, basis, rationale, support); err != nil {
		return err
	}
	if err := s.evidencePacket.ValidateAttachment(ids, current, hashes); err != nil {
		return err
	}
	return validateReviewSupport(support, ids, s.evidencePacket.evidenceIndex())
}

func validateReviewSupport(support *claimSupport, ids []string, evidence map[string]evidenceRecord) error {
	if support == nil {
		return nil
	}
	if err := support.Validate(evidence); err != nil {
		return err
	}
	for _, id := range support.EvidenceIDs {
		if !containsReviewID(ids, id) {
			return fmt.Errorf("support evidence %q is not attached to its claim", id)
		}
	}
	return nil
}

func validateReviewItemLink(id, revision, check string, item reviewItem) error {
	if err := item.Validate(); err != nil {
		return err
	}
	if id != item.ID || revision != item.Revision || !containsReviewID(item.Checks, check) {
		return fmt.Errorf("unknown/stale item revision or inapplicable check: %s/%s/%s", id, revision, check)
	}
	return nil
}

func validateUniqueReviewIDs(kind string, ids []string) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			return fmt.Errorf("empty or duplicate %s ID %q", kind, id)
		}
		seen[id] = true
	}
	return nil
}

func containsReviewID(ids []string, id string) bool {
	return slices.Contains(ids, id)
}

func validateReviewSupersession(links map[string][]string) error {
	visited, active := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if active[id] {
			return fmt.Errorf("cyclic supersession at %q", id)
		}
		if visited[id] {
			return nil
		}
		if err := validateUniqueReviewIDs("superseded", links[id]); err != nil {
			return err
		}
		active[id] = true
		for _, old := range links[id] {
			if err := visit(old); err != nil {
				return err
			}
		}
		active[id], visited[id] = false, true
		return nil
	}
	for id := range links {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}
