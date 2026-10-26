package main

import (
	"fmt"
	"slices"
	"strings"
)

// These records are an additive contract, not a replacement for ctxView or the
// legacy gate JSON. Nested value objects (ranges, counts and routes) inherit the
// containing record's version. Subject identity reuses A2's gateSubject verbatim.
const evidenceSchemaVersion = 1

type evidenceCompleteness string

const (
	evidenceComplete evidenceCompleteness = "complete"
	evidencePartial  evidenceCompleteness = "partial"
	evidenceUnknown  evidenceCompleteness = "unknown"
)

type evidenceCountKind string

const (
	evidenceCountExact      evidenceCountKind = "exact"
	evidenceCountLowerBound evidenceCountKind = "lower_bound"
	evidenceCountUnknown    evidenceCountKind = "unknown"
)

type lookupExecution string

const (
	lookupNotRequested lookupExecution = "not_requested"
	lookupSucceeded    lookupExecution = "succeeded"
	lookupFailed       lookupExecution = "failed"
	lookupUnavailable  lookupExecution = "unavailable"
)

// evidenceEnvelope supplies the default subject and completeness once. Rows may
// override these with subject_id/completeness; omission means inheritance, not
// unknown. An explicit unknown completeness overrides a complete envelope.
type evidenceEnvelope struct {
	SchemaVersion int                  `json:"schema_version"`
	Subject       *gateSubject         `json:"subject"`
	Completeness  evidenceCompleteness `json:"completeness"`
}

func (e evidenceEnvelope) Validate() error {
	if err := validateEvidenceVersion(e.SchemaVersion); err != nil {
		return err
	}
	if !e.Subject.valid() {
		return fmt.Errorf("invalid or unbound evidence subject")
	}
	return validateEvidenceEnum("completeness", e.Completeness, evidenceComplete, evidencePartial, evidenceUnknown)
}

// Counts are nullable: an unmeasured total is not zero. Requested/succeeded/
// failed count query executions; Total counts results, whose unit is explicit.
type lookupCounts struct {
	Requested *int `json:"requested"`
	Succeeded *int `json:"succeeded"`
	Failed    *int `json:"failed"`
	Total     *int `json:"total"`
}

type evidenceLookup struct {
	SchemaVersion   int                  `json:"schema_version"`
	ID              string               `json:"id"`
	SubjectID       string               `json:"subject_id,omitempty"`
	Completeness    evidenceCompleteness `json:"completeness,omitempty"`
	Query           string               `json:"query"`
	Scope           string               `json:"scope"`
	Exclusions      []string             `json:"exclusions"`
	Counts          lookupCounts         `json:"counts"`
	Unit            string               `json:"unit"`
	ExecutionStatus lookupExecution      `json:"execution_status"`
	// Status uses ctxView's existing vocabulary, and is absent when no backend
	// ran. ExecutionStatus separately distinguishes unrequested/unavailable.
	Status    string            `json:"status,omitempty"` // ok, empty, partial, failed
	CountKind evidenceCountKind `json:"count_kind"`
	Errors    []string          `json:"errors"`
	Omissions []string          `json:"omissions"`
}

func (l evidenceLookup) Validate(env evidenceEnvelope) error {
	if err := validateEvidenceRow(l.SchemaVersion, l.ID, l.Completeness); err != nil {
		return err
	}
	if err := validateEvidenceEnum("execution_status", l.ExecutionStatus, lookupNotRequested, lookupSucceeded, lookupFailed, lookupUnavailable); err != nil {
		return err
	}
	if err := validateEvidenceEnum("count_kind", l.CountKind, evidenceCountExact, evidenceCountLowerBound, evidenceCountUnknown); err != nil {
		return err
	}
	if strings.TrimSpace(l.Scope) == "" || strings.TrimSpace(l.Unit) == "" {
		return fmt.Errorf("lookup %s requires scope and result unit", l.ID)
	}
	c := l.Counts
	for _, n := range []*int{c.Requested, c.Succeeded, c.Failed, c.Total} {
		if n != nil && *n < 0 {
			return fmt.Errorf("lookup %s has negative counts", l.ID)
		}
	}
	if c.Requested != nil {
		// Subtract instead of adding potentially overflowing measurements.
		remaining := *c.Requested
		for _, n := range []*int{c.Succeeded, c.Failed} {
			if n != nil {
				if *n > remaining {
					return fmt.Errorf("lookup %s execution counts exceed requested", l.ID)
				}
				remaining -= *n
			}
		}
	}
	if l.CountKind == evidenceCountUnknown && c.Total != nil || l.CountKind != evidenceCountUnknown && c.Total == nil {
		return fmt.Errorf("lookup %s total and count_kind disagree", l.ID)
	}
	complete := effectiveCompleteness(l.Completeness, env)
	switch l.ExecutionStatus {
	case lookupNotRequested:
		if l.Status != "" || complete != evidenceUnknown || l.CountKind != evidenceCountUnknown || nonzeroCount(c.Requested) || c.Succeeded != nil || c.Failed != nil {
			return fmt.Errorf("unrequested lookup %s cannot claim execution or coverage", l.ID)
		}
	case lookupUnavailable:
		if l.Status != "" || complete == evidenceComplete || l.CountKind != evidenceCountUnknown || nonzeroCount(c.Succeeded) || nonzeroCount(c.Failed) || len(l.Errors) == 0 {
			return fmt.Errorf("unavailable lookup %s requires a reason and cannot claim execution or coverage", l.ID)
		}
	case lookupSucceeded, lookupFailed:
		if strings.TrimSpace(l.Query) == "" || c.Requested == nil || *c.Requested == 0 || c.Succeeded == nil || c.Failed == nil {
			return fmt.Errorf("executed lookup %s requires query and measured execution counts", l.ID)
		}
		if *c.Succeeded != *c.Requested-*c.Failed {
			return fmt.Errorf("lookup %s has unaccounted executions", l.ID)
		}
		if l.ExecutionStatus == lookupSucceeded {
			if *c.Failed != 0 || len(l.Errors) != 0 || (l.Status != "ok" && l.Status != "empty" && l.Status != "partial") {
				return fmt.Errorf("successful lookup %s has inconsistent status/errors", l.ID)
			}
			if l.Status == "empty" && (c.Total == nil || *c.Total != 0 || l.CountKind != evidenceCountExact || complete != evidenceComplete) {
				return fmt.Errorf("empty lookup %s requires complete exact zero results", l.ID)
			}
			if l.Status == "ok" && (c.Total == nil || *c.Total == 0) {
				return fmt.Errorf("ok lookup %s requires a positive result count", l.ID)
			}
		} else if *c.Failed == 0 || len(l.Errors) == 0 || (l.Status != "failed" && l.Status != "partial") || complete == evidenceComplete {
			return fmt.Errorf("failed lookup %s requires failed counts, errors and incomplete coverage", l.ID)
		}
	}
	if l.Status == "partial" && complete != evidencePartial {
		return fmt.Errorf("partial lookup %s requires partial completeness", l.ID)
	}
	if l.CountKind == evidenceCountLowerBound && complete == evidenceComplete {
		return fmt.Errorf("lower-bound lookup %s cannot claim complete coverage", l.ID)
	}
	return nil
}

func nonzeroCount(n *int) bool { return n != nil && *n != 0 }

type evidenceSourceKind string

const (
	evidenceSourceCode       evidenceSourceKind = "source"
	evidenceSourceTest       evidenceSourceKind = "test"
	evidenceSourceSpec       evidenceSourceKind = "spec"
	evidenceSourceDocument   evidenceSourceKind = "documentation"
	evidenceSourceAuthorHint evidenceSourceKind = "author_hint"
	evidenceSourceToolOutput evidenceSourceKind = "tool_output"
)

// Lines are one-based and inclusive. Optional columns are one-based with an
// exclusive end, and MUST declare their encoding (LSP often uses UTF-16, not
// bytes). No language, repository layout or hashing algorithm is assumed.
type evidenceRange struct {
	StartLine      int    `json:"start_line"`
	EndLine        int    `json:"end_line"`
	StartColumn    *int   `json:"start_column,omitempty"`
	EndColumn      *int   `json:"end_column,omitempty"`
	ColumnEncoding string `json:"column_encoding,omitempty"` // utf8, utf16, codepoint
}

func (r evidenceRange) Validate() error {
	if r.StartLine < 1 || r.EndLine < r.StartLine {
		return fmt.Errorf("invalid source line range")
	}
	if r.StartColumn == nil && r.EndColumn == nil && r.ColumnEncoding == "" {
		return nil
	}
	if r.StartColumn == nil || r.EndColumn == nil || *r.StartColumn < 1 || *r.EndColumn < 1 || (r.StartLine == r.EndLine && *r.EndColumn < *r.StartColumn) {
		return fmt.Errorf("invalid source column range")
	}
	return validateEvidenceEnum("column_encoding", r.ColumnEncoding, "utf8", "utf16", "codepoint")
}

// Null observations mean unknown. False means explicitly not selected/delivered/
// read. Rendering proves delivery only; even a recorded read is not support for
// a semantic interpretation. LocatorValid records resolution, not claim support.
// ContentHash binds the exact source bytes selected by Range, or the entire
// source when Range is absent. Producers must use the same hash algorithm and
// byte basis for currentHashes; language/column encoding does not re-encode it.
type evidenceRecord struct {
	SchemaVersion int                  `json:"schema_version"`
	ID            string               `json:"id"`
	SubjectID     string               `json:"subject_id,omitempty"`
	Completeness  evidenceCompleteness `json:"completeness,omitempty"`
	SourceKind    evidenceSourceKind   `json:"source_kind"`
	Language      string               `json:"language,omitempty"`
	Path          string               `json:"path"`
	Range         *evidenceRange       `json:"range,omitempty"`
	ContentHash   string               `json:"content_hash"`
	LookupID      string               `json:"lookup_id"`
	Selected      *bool                `json:"selected"`
	Rendered      *bool                `json:"rendered"`
	Read          *bool                `json:"read"`
	LocatorValid  *bool                `json:"locator_valid"`
}

func (e evidenceRecord) Validate() error {
	if err := validateEvidenceRow(e.SchemaVersion, e.ID, e.Completeness); err != nil {
		return err
	}
	if err := validateEvidenceEnum("source_kind", e.SourceKind, evidenceSourceCode, evidenceSourceTest, evidenceSourceSpec, evidenceSourceDocument, evidenceSourceAuthorHint, evidenceSourceToolOutput); err != nil {
		return err
	}
	if strings.TrimSpace(e.Path) == "" || strings.TrimSpace(e.ContentHash) == "" || strings.TrimSpace(e.LookupID) == "" {
		return fmt.Errorf("evidence %s requires path, content_hash and origin lookup", e.ID)
	}
	if e.Range != nil {
		return e.Range.Validate()
	}
	return nil
}

type claimSupportState string

const (
	claimSupportUnknown     claimSupportState = "unknown"
	claimSupported          claimSupportState = "supported"
	claimUnsupported        claimSupportState = "unsupported"
	claimSupportConflicting claimSupportState = "conflicting"
)

// This is an attributable assessment, not a confidence score or proof that a
// model interpretation is true. Policy/authority acceptance belongs to B5a.
// Excerpt and FailureScenario allow sweep to retain behavioral support details.
type claimSupport struct {
	SchemaVersion   int               `json:"schema_version"`
	State           claimSupportState `json:"state"`
	Assessor        string            `json:"assessor"`
	Method          string            `json:"method"` // structural, model, human
	EvidenceIDs     []string          `json:"evidence_ids"`
	Rationale       string            `json:"rationale"`
	Excerpt         string            `json:"excerpt,omitempty"`
	FailureScenario string            `json:"failure_scenario,omitempty"`
}

func (s claimSupport) Validate(evidence map[string]evidenceRecord) error {
	return s.validate(evidence, false)
}

// ValidateSearch permits an absence assessment to cite search coverage instead
// of manufacturing a source observation for a lookup that returned nothing.
// Only complete exact zero-result lookups qualify for this structural relaxation.
// Requirement/query binding, scope and exclusion adequacy remain reducer policy.
func (s claimSupport) ValidateSearch(p evidencePacket, ids []string, current *gateSubject) error {
	if err := p.ValidateLookupAttachment(ids, current); err != nil {
		return err
	}
	for _, l := range p.Lookups {
		if slices.Contains(ids, l.ID) && (l.Status != "empty" || l.CountKind != evidenceCountExact || l.Counts.Total == nil || *l.Counts.Total != 0) {
			return fmt.Errorf("search-only support requires complete exact zero results: %s", l.ID)
		}
	}
	return s.validate(p.evidenceIndex(), len(ids) > 0)
}

func (s claimSupport) validate(evidence map[string]evidenceRecord, hasSearch bool) error {
	if err := validateEvidenceVersion(s.SchemaVersion); err != nil {
		return err
	}
	if err := validateEvidenceEnum("support state", s.State, claimSupportUnknown, claimSupported, claimUnsupported, claimSupportConflicting); err != nil {
		return err
	}
	if err := validateAssessment(s.Assessor, s.Method, s.Rationale); err != nil {
		return err
	}
	if err := validateEvidenceIDs(s.EvidenceIDs, evidence); err != nil {
		return err
	}
	if s.State == claimSupported && len(s.EvidenceIDs) == 0 && !hasSearch {
		return fmt.Errorf("supported claim requires evidence IDs")
	}
	return nil
}

// A stale observation may be attached only after an explicit, attributable
// revalidation of BOTH its old binding and its new binding. This never mutates
// the old record, and never asserts that an earlier claim remains supported.
type evidenceRevalidation struct {
	SchemaVersion   int    `json:"schema_version"`
	ID              string `json:"id"`
	EvidenceID      string `json:"evidence_id"`
	FromSubjectID   string `json:"from_subject_id"`
	FromContentHash string `json:"from_content_hash"`
	SubjectID       string `json:"subject_id"`
	ContentHash     string `json:"content_hash"`
	LocatorValid    bool   `json:"locator_valid"`
	Assessor        string `json:"assessor"`
	Method          string `json:"method"`
	Rationale       string `json:"rationale"`
}

func (r evidenceRevalidation) Validate() error {
	if err := validateEvidenceRow(r.SchemaVersion, r.ID, ""); err != nil {
		return err
	}
	for _, v := range []string{r.EvidenceID, r.FromSubjectID, r.FromContentHash, r.SubjectID, r.ContentHash} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("revalidation %s requires old and current evidence bindings", r.ID)
		}
	}
	return validateAssessment(r.Assessor, r.Method, r.Rationale)
}

type evidencePacket struct {
	evidenceEnvelope
	Lookups       []evidenceLookup       `json:"lookups"`
	Evidence      []evidenceRecord       `json:"evidence"`
	Revalidations []evidenceRevalidation `json:"revalidations"`
}

func (p evidencePacket) Validate() error {
	if err := p.evidenceEnvelope.Validate(); err != nil {
		return err
	}
	lookups := map[string]evidenceLookup{}
	for _, l := range p.Lookups {
		if err := l.Validate(p.evidenceEnvelope); err != nil {
			return err
		}
		if _, ok := lookups[l.ID]; ok {
			return fmt.Errorf("duplicate lookup ID %q", l.ID)
		}
		lookups[l.ID] = l
	}
	evidence := map[string]evidenceRecord{}
	for _, e := range p.Evidence {
		if err := e.Validate(); err != nil {
			return err
		}
		if _, ok := evidence[e.ID]; ok {
			return fmt.Errorf("duplicate evidence ID %q", e.ID)
		}
		l, ok := lookups[e.LookupID]
		if !ok {
			return fmt.Errorf("evidence %s has unknown lookup ID %q", e.ID, e.LookupID)
		}
		if l.ExecutionStatus == lookupNotRequested || l.ExecutionStatus == lookupUnavailable || (l.Counts.Total != nil && *l.Counts.Total == 0) {
			return fmt.Errorf("evidence %s originates from a lookup with no results", e.ID)
		}
		if effectiveSubjectID(e.SubjectID, p.evidenceEnvelope) != effectiveSubjectID(l.SubjectID, p.evidenceEnvelope) {
			return fmt.Errorf("evidence %s and origin lookup subjects differ", e.ID)
		}
		evidence[e.ID] = e
	}
	seen := map[string]bool{}
	bindings := map[string]bool{}
	for _, r := range p.Revalidations {
		if err := r.Validate(); err != nil {
			return err
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate revalidation ID %q", r.ID)
		}
		seen[r.ID] = true
		e, ok := evidence[r.EvidenceID]
		if !ok || r.FromSubjectID != effectiveSubjectID(e.SubjectID, p.evidenceEnvelope) || r.FromContentHash != e.ContentHash {
			return fmt.Errorf("revalidation %s does not identify its original evidence", r.ID)
		}
		// There can be no ambiguous competing revalidations for a target binding.
		key := subjectJSONDigest([]string{r.EvidenceID, r.SubjectID, r.ContentHash})
		if bindings[key] {
			return fmt.Errorf("duplicate evidence revalidation binding for %q", r.EvidenceID)
		}
		bindings[key] = true
	}
	return nil
}

// ValidateAttachment is pure: currentHashes must come from trusted retrieval,
// keyed by evidence ID (not path, since several ranges may share one file).
// Unknown current measurements fail closed. A successful check says the source
// binding is current and its locator valid, NOT that any claim is supported.
func (p evidencePacket) ValidateAttachment(ids []string, current *gateSubject, currentHashes map[string]string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if !current.valid() || current.ID != p.Subject.ID {
		return fmt.Errorf("answer envelope is not bound to the current subject")
	}
	index := p.evidenceIndex()
	if err := validateEvidenceIDs(ids, index); err != nil {
		return err
	}
	for _, id := range ids {
		e := index[id]
		hash := currentHashes[id]
		if strings.TrimSpace(hash) == "" {
			return fmt.Errorf("evidence %s has unknown current content hash", id)
		}
		if effectiveSubjectID(e.SubjectID, p.evidenceEnvelope) == current.ID && e.ContentHash == hash && e.LocatorValid != nil && *e.LocatorValid {
			continue
		}
		revalidated := false
		for _, r := range p.Revalidations {
			if r.EvidenceID == id && r.SubjectID == current.ID && r.ContentHash == hash && r.LocatorValid {
				revalidated = true
				break
			}
		}
		if !revalidated {
			return fmt.Errorf("evidence %s is stale or its locator is unvalidated; revalidation required", id)
		}
	}
	return nil
}

// ValidateLookupAttachment binds search coverage to known lookup records of
// the current subject, independently of whether any source evidence was shown.
func (p evidencePacket) ValidateLookupAttachment(ids []string, current *gateSubject) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if !current.valid() || current.ID != p.Subject.ID {
		return fmt.Errorf("search coverage is not bound to the current subject")
	}
	lookups := make(map[string]evidenceLookup, len(p.Lookups))
	for _, l := range p.Lookups {
		lookups[l.ID] = l
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			return fmt.Errorf("empty or duplicate lookup ID %q", id)
		}
		seen[id] = true
		l, ok := lookups[id]
		if !ok {
			return fmt.Errorf("unknown lookup ID %q", id)
		}
		if effectiveSubjectID(l.SubjectID, p.evidenceEnvelope) != current.ID {
			return fmt.Errorf("lookup %s is not bound to the current subject", id)
		}
	}
	return nil
}

func (p evidencePacket) evidenceIndex() map[string]evidenceRecord {
	out := make(map[string]evidenceRecord, len(p.Evidence))
	for _, e := range p.Evidence {
		out[e.ID] = e
	}
	return out
}

func validateEvidenceIDs(ids []string, evidence map[string]evidenceRecord) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("duplicate evidence ID %q", id)
		}
		seen[id] = true
		if _, ok := evidence[id]; !ok {
			return fmt.Errorf("unknown evidence ID %q", id)
		}
	}
	return nil
}

func validateAssessment(assessor, method, rationale string) error {
	if strings.TrimSpace(assessor) == "" || strings.TrimSpace(rationale) == "" {
		return fmt.Errorf("assessment requires assessor and rationale")
	}
	return validateEvidenceEnum("assessment method", method, "structural", "model", "human")
}

func effectiveSubjectID(id string, env evidenceEnvelope) string {
	if id != "" {
		return id
	}
	if env.Subject == nil {
		return ""
	}
	return env.Subject.ID
}

func effectiveCompleteness(c evidenceCompleteness, env evidenceEnvelope) evidenceCompleteness {
	if c != "" {
		return c
	}
	return env.Completeness
}

func validateEvidenceRow(version int, id string, completeness evidenceCompleteness) error {
	if err := validateEvidenceVersion(version); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("record requires a nonempty string ID")
	}
	if completeness != "" {
		return validateEvidenceEnum("completeness", completeness, evidenceComplete, evidencePartial, evidenceUnknown)
	}
	return nil
}

func validateEvidenceVersion(version int) error {
	if version != evidenceSchemaVersion {
		return fmt.Errorf("unsupported evidence schema_version %d", version)
	}
	return nil
}

func validateEvidenceEnum[T ~string](name string, value T, allowed ...T) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("invalid %s %q", name, value)
}
