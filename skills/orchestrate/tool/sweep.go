package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// `orch sweep <topic>` settles a pile of open questions without trusting any
// single model. Several passes (different models, fresh context each) answer
// every question card on their own, in files under <topic>/passes/<pass>/.
// This command compares them with code: an answer only counts when it picks
// one of the card's option ids, binds the exact card and subject, and has an
// attributable support assessment. Agreement alone is not support. Passes
// that differ go to a final pass, and
// questions in a human-only area always go to the user.
//
//	<topic>/questions/<key>.md          cards (see references/question-file.md)
//	<topic>/passes/<pass>/<key>.json    one answer per pass and question
//	<topic>/manifest.json               every source question collected
//	<topic>/dropped.json                required, even when empty
//	<topic>/sweep/for-user/<key>.md     written here: cards for `orch ask`
type sweepAnswer struct {
	Choice          string           `json:"choice"`
	Confidence      float64          `json:"confidence"`
	Reason          string           `json:"reason"`
	Evidence        []string         `json:"evidence"`
	Owner           string           `json:"owner"` // agent (default), user or an outside expert such as accountant
	CardRev         string           `json:"card_rev,omitempty"`
	Subject         *gateSubject     `json:"subject,omitempty"`
	EvidenceRecords []evidenceRecord `json:"evidence_records,omitempty"`
	Support         *claimSupport    `json:"claim_support,omitempty"`
}

type sweepDropped struct {
	Question        string           `json:"question"`
	Reason          string           `json:"reason"`
	DuplicateOf     string           `json:"duplicate_of,omitempty"` // a card key
	AnsweredBy      string           `json:"answered_by,omitempty"`  // path:line of the ADR or code
	SourceID        string           `json:"source_id,omitempty"`
	Disposition     string           `json:"disposition,omitempty"`
	Subject         *gateSubject     `json:"subject,omitempty"`
	EvidenceRecords []evidenceRecord `json:"evidence_records,omitempty"`
	Support         *claimSupport    `json:"claim_support,omitempty"`
}

type sweepSourceQuestion struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Card     string `json:"card,omitempty"`
}

type sweepManifest struct {
	SchemaVersion int                   `json:"schema_version"`
	Questions     []sweepSourceQuestion `json:"questions"`
}

// results.json is both the durable resolution record and the binding input
// for the next independent passes. It never upgrades model support to proof.
type sweepResolution struct {
	CardRev string                 `json:"card_rev"`
	Subject *gateSubject           `json:"subject"`
	Status  string                 `json:"status"`
	Choice  string                 `json:"choice,omitempty"`
	Why     string                 `json:"why,omitempty"`
	Passes  []sweepPassObservation `json:"passes"`
}

type sweepPassObservation struct {
	Name          string        `json:"name"`
	CitationValid bool          `json:"citation_valid"`
	ClaimSupport  *claimSupport `json:"claim_support,omitempty"`
	Issue         string        `json:"issue,omitempty"`
}

type sweepPass struct {
	name          string
	ans           sweepAnswer
	checked       int // evidence lines that resolve
	urls          int
	bad           []string // evidence that does not resolve
	supportErr    string
	bound         bool
	citationValid bool
}

// sweepEvidence counts what an answer's evidence is worth. roots maps a name
// to a directory outside the repo (a competitor's checkout); evidence reads
// "<name>/path:line" for those and "path:line" for the repo itself.
func sweepEvidence(repo string, roots map[string]string, ev []string) (ok, urls int, bad []string) {
	for _, e := range ev {
		e = strings.TrimSpace(e)
		switch {
		case strings.HasPrefix(e, "https://") || strings.HasPrefix(e, "http://"):
			urls++
			continue
		}
		base, rel := repo, e
		if name, rest, found := strings.Cut(e, "/"); found {
			if dir, isRoot := roots[name]; isRoot {
				base, rel = dir, rest
			}
		}
		if evidenceHolds(base, rel) {
			ok++
		} else {
			bad = append(bad, e)
		}
	}
	return ok, urls, bad
}

type sweepResult struct {
	key, question, status, why string
	passes                     []sweepPass
	card                       *card
	choice                     string
	cardRev                    string
	subject                    *gateSubject
}

func cmdSweep(a *app, args []string) error {
	fs := a.flags("sweep")
	rootsFlag := fs.String("roots", "", "evidence outside the repo as name=dir[,name=dir]; cite it as name/path:line")
	minPasses := fs.Int("min-passes", 2, "answers needed before a question can settle")
	final := fs.String("final", "settle", "name of the pass that breaks a disagreement")
	acceptURLs := fs.Bool("accept-urls", false, "let an https link count as evidence (it cannot be checked by code)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *minPasses < 2 {
		return &exitError{code: 2, msg: "usage: orch sweep <topic-dir> [--roots name=dir,...] [--min-passes N>=2] [--final PASS] [--accept-urls]"}
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	topic := pos[0]
	roots := map[string]string{}
	for _, kv := range splitList(*rootsFlag) {
		name, dir, ok := strings.Cut(kv, "=")
		if !ok || name == "" || dir == "" {
			return fail("--roots %q must be name=dir", kv)
		}
		roots[name] = expandHome(dir)
	}
	cards, err := filepath.Glob(filepath.Join(topic, "questions", "*.md"))
	if err != nil || len(cards) == 0 {
		return fail("no cards in %s", filepath.Join(topic, "questions"))
	}
	sort.Strings(cards)
	passDirs, _ := filepath.Glob(filepath.Join(topic, "passes", "*"))
	humanOnly := map[string]bool{}
	for _, h := range s.cfg.HumanOnly {
		humanOnly[strings.ToLower(strings.TrimSpace(h))] = true
	}

	stamp, err := checkoutFingerprint(s.repo)
	if err != nil {
		return fail("cannot bind sweep to the code subject: %v", err)
	}
	before, err := gateSubjectFromStamp(s.repo, nil, stamp)
	if err != nil {
		return err
	}
	var results []sweepResult
	for _, path := range cards {
		key := strings.TrimSuffix(filepath.Base(path), ".md")
		src, err := os.ReadFile(path)
		if err != nil {
			return fail("cannot read card %s: %v", key, err)
		}
		c, cerr := parseCard(string(src))
		subject, err := gateSubjectFromStamp(s.repo, src, stamp)
		if err != nil {
			return err
		}
		r := sweepResult{key: key, card: c, cardRev: subjectDigest(src), subject: subject}
		if cerr != nil {
			r.status, r.why = "user", "the card does not parse ("+cerr.Error()+")"
			results = append(results, r)
			continue
		}
		r.question = c.question
		results = append(results, a.judgeQuestion(&r, s.repo, roots, passDirs, humanOnly, *minPasses, *final, *acceptURLs))
	}

	issues := checkDropped(topic, cards, s.repo, roots, *acceptURLs)
	// Do not attach a decision to a checkout that changed during comparison.
	after, err := captureGateSubject(s.repo, nil)
	if err != nil || after.ID != before.ID {
		return fail("code subject changed or could not be checked during sweep; rerun before applying answers")
	}
	forUser := filepath.Join(topic, "sweep", "for-user")
	if err := os.RemoveAll(forUser); err != nil {
		return fail("cannot clear stale sweep/for-user cards: %v", err)
	}
	groups := map[string][]sweepResult{}
	for _, r := range results {
		groups[r.status] = append(groups[r.status], r)
		if r.status == "user" {
			if err := writeUserCard(forUser, r, cards); err != nil {
				return err
			}
		}
	}
	order := []struct{ status, title string }{
		{"settled", "settled: the passes agree, with assessed claim support"},
		{"final", "settled by the final pass after a disagreement"},
		{"disagree", "disagree: run the final pass (a stronger model) on these"},
		{"recheck", "recheck: an answer is missing, stale, invalid or lacks claim support"},
		{"user", "user: only a person decides these (written to sweep/for-user)"},
	}
	for _, g := range order {
		fmt.Fprintf(a.out, "%s[%d]:\n", g.title, len(groups[g.status]))
		for _, r := range groups[g.status] {
			line := r.key
			if r.choice != "" {
				line += " -> " + r.choice
			}
			if r.why != "" {
				line += " (" + r.why + ")"
			}
			fmt.Fprintf(a.out, "  %s\n", agentScalar(line))
		}
	}
	if err := writeSweepResults(topic, results, issues); err != nil {
		return err
	}
	for _, issue := range issues {
		fmt.Fprintf(a.out, "dropped: %s\n", issue)
	}
	if n := len(groups["user"]); n > 0 {
		fmt.Fprintf(a.out, "next: orch ask --cards %s\n", forUser)
	}
	if len(issues) > 0 {
		return fail("sweep accounting failed (%d issues); repair manifest.json and dropped.json, then rerun", len(issues))
	}
	return nil
}

func (a *app) judgeQuestion(r *sweepResult, repo string, roots map[string]string, passDirs []string, humanOnly map[string]bool, minPasses int, final string, acceptURLs bool) sweepResult {
	c := r.card
	options := map[string]bool{}
	for _, o := range c.options {
		options[o.id] = true
	}
	var invalid []string
	for _, d := range passDirs {
		name := filepath.Base(d)
		b, err := os.ReadFile(filepath.Join(d, r.key+".json"))
		if err != nil {
			if !os.IsNotExist(err) {
				invalid = append(invalid, name+": answer is unreadable")
			}
			continue
		}
		var ans sweepAnswer
		if json.Unmarshal(b, &ans) != nil {
			invalid = append(invalid, name+": not JSON")
			continue
		}
		p := sweepPass{name: name, ans: ans}
		p.bound = ans.CardRev == r.cardRev && ans.Subject.valid() && ans.Subject.ID == r.subject.ID
		if !p.bound {
			invalid = append(invalid, name+": card_rev or code subject changed or is unbound")
		}
		p.checked, p.urls, p.bad = sweepEvidence(repo, roots, ans.Evidence)
		p.citationValid = len(p.bad) == 0 && (p.checked > 0 || acceptURLs && p.urls > 0 || len(ans.EvidenceRecords) > 0)
		for _, e := range ans.EvidenceRecords {
			excerpt := ""
			if ans.Support != nil {
				excerpt = ans.Support.Excerpt
			}
			if err := e.Validate(); err != nil {
				p.citationValid = false
			} else if _, err := sweepEvidenceBytes(repo, roots, e, excerpt, acceptURLs); err != nil {
				p.citationValid = false
			}
		}
		if err := sweepClaimSupport(repo, roots, ans.EvidenceRecords, ans.Support, r.subject, acceptURLs); err != nil {
			p.supportErr = err.Error()
		}
		r.passes = append(r.passes, p)
	}
	supported := func(p sweepPass) bool { return p.bound && p.citationValid && p.supportErr == "" }

	// A question the user alone decides never settles on a model's say-so.
	area := strings.ToLower(strings.TrimSpace(c.area))
	for _, p := range r.passes {
		if o := strings.ToLower(strings.TrimSpace(p.ans.Owner)); p.bound && o != "" && o != "agent" {
			r.status, r.why = "user", fmt.Sprintf("pass %s says the owner is %s", p.name, o)
			return *r
		}
	}
	if humanOnly[area] {
		r.status, r.why = "user", "area "+area+" is human-only"
		return *r
	}
	if len(c.options) == 0 {
		r.status, r.why = "user", "a free-text question has no options to compare"
		return *r
	}

	var voting []sweepPass
	for _, p := range r.passes {
		if p.name == final {
			continue
		}
		switch {
		case !options[p.ans.Choice]:
			invalid = append(invalid, fmt.Sprintf("%s picked %q, which is not an option", p.name, p.ans.Choice))
		case len(p.bad) > 0:
			invalid = append(invalid, p.name+": evidence does not resolve: "+strings.Join(p.bad, ", "))
		case !supported(p):
			why := p.supportErr
			if why == "" {
				why = "citation is not valid for this sweep"
			}
			invalid = append(invalid, p.name+": "+why)
		default:
			voting = append(voting, p)
		}
	}
	if len(invalid) > 0 || len(voting) < minPasses {
		r.status = "recheck"
		r.why = strings.Join(invalid, "; ")
		if r.why == "" {
			r.why = fmt.Sprintf("%d of %d passes answered", len(voting), minPasses)
		}
		return *r
	}
	agree := true
	for _, p := range voting {
		if p.ans.Choice != voting[0].ans.Choice {
			agree = false
		}
	}
	if agree {
		r.status, r.choice = "settled", voting[0].ans.Choice
		return *r
	}
	for _, p := range r.passes {
		if p.name == final && options[p.ans.Choice] && len(p.bad) == 0 && supported(p) {
			r.status, r.choice, r.why = "final", p.ans.Choice, "final pass "+final
			return *r
		}
	}
	var picks []string
	for _, p := range voting {
		picks = append(picks, p.name+"="+p.ans.Choice)
	}
	r.status, r.why = "disagree", strings.Join(picks, ", ")
	return *r
}

// oneLine makes model output safe to put in a card: one line, no backticks,
// so it can never start a heading or an include, or close a code span.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "'")), " ")
	return short(s, max)
}

// writeUserCard copies a card for `orch ask` and adds what each pass said, so
// the user sees the evidence and any disagreement next to the question.
func writeUserCard(dir string, r sweepResult, cardPaths []string) error {
	var src []byte
	for _, p := range cardPaths {
		if strings.TrimSuffix(filepath.Base(p), ".md") == r.key {
			src, _ = os.ReadFile(p)
		}
	}
	var b strings.Builder
	b.Write(src)
	b.WriteString("\n\n## Passes\n\n")
	fmt.Fprintf(&b, "Why this is yours: %s.\n\n", oneLine(r.why, 300))
	for _, p := range r.passes {
		fmt.Fprintf(&b, "- **%s** chose `%s` (confidence %.2f): %s\n", oneLine(p.name, 40), oneLine(p.ans.Choice, 40), p.ans.Confidence, oneLine(p.ans.Reason, 600))
		fmt.Fprintf(&b, "  Current binding: %t; citation_valid: %t. Confidence is not proof.\n", p.bound, p.citationValid)
		if p.supportErr != "" {
			fmt.Fprintf(&b, "  Unresolved claim support: %s.\n", oneLine(p.supportErr, 400))
		} else if s := p.ans.Support; s != nil {
			fmt.Fprintf(&b, "  Support assessment: %s by %s (%s). Quote: %s. Failure scenario: %s.\n", oneLine(string(s.State), 40), oneLine(s.Assessor, 80), oneLine(s.Method, 40), oneLine(s.Excerpt, 400), oneLine(s.FailureScenario, 400))
		}
		if len(p.ans.Evidence) > 0 {
			ev := make([]string, len(p.ans.Evidence))
			for i, e := range p.ans.Evidence {
				ev[i] = oneLine(e, 120)
			}
			fmt.Fprintf(&b, "  Evidence: %s\n", strings.Join(ev, ", "))
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, r.key+".md"), []byte(b.String()), 0o644)
}

// sweepClaimSupport checks attribution and current source bytes, not the truth
// of a semantic model judgment. The assessment is retained with its method.
func sweepClaimSupport(repo string, roots map[string]string, records []evidenceRecord, support *claimSupport, subject *gateSubject, acceptURLs bool) error {
	if support == nil {
		return fmt.Errorf("no claim_support assessment; citation validity alone is insufficient")
	}
	index := map[string]evidenceRecord{}
	contents := map[string][]byte{}
	for _, e := range records {
		if err := e.Validate(); err != nil {
			return err
		}
		if _, exists := index[e.ID]; exists {
			return fmt.Errorf("duplicate evidence ID %q", e.ID)
		}
		if e.SubjectID != subject.ID || (e.Completeness != "" && e.Completeness != evidenceComplete) {
			return fmt.Errorf("evidence %s is stale or incomplete", e.ID)
		}
		if e.LocatorValid == nil || !*e.LocatorValid {
			return fmt.Errorf("evidence %s has no valid locator observation", e.ID)
		}
		b, err := sweepEvidenceBytes(repo, roots, e, support.Excerpt, acceptURLs)
		if err != nil {
			return err
		}
		if subjectDigest(b) != e.ContentHash {
			return fmt.Errorf("evidence %s content changed", e.ID)
		}
		index[e.ID], contents[e.ID] = e, b
	}
	if err := support.Validate(index); err != nil {
		return err
	}
	if support.State != claimSupported {
		return fmt.Errorf("claim_support is %s", support.State)
	}
	if strings.TrimSpace(support.Excerpt) == "" {
		return fmt.Errorf("claim_support requires a quoted excerpt")
	}
	quoted := false
	for _, id := range support.EvidenceIDs {
		e := index[id]
		quoted = quoted || bytes.Contains(contents[id], []byte(support.Excerpt))
		// Behavioral source/test claims conservatively require a failure scenario.
		if (e.SourceKind == evidenceSourceCode || e.SourceKind == evidenceSourceTest) && strings.TrimSpace(support.FailureScenario) == "" {
			return fmt.Errorf("source/test claim_support requires a concrete failure scenario")
		}
	}
	if !quoted {
		return fmt.Errorf("claim_support excerpt is not in the cited source range")
	}
	return nil
}

// Hash selected raw bytes, including line terminators. No column slicing or
// implicit symbol fallback: unsupported ranges stay unresolved. URL snapshots
// are explicitly opted-in quotations, never fetched or presented as verified.
func sweepEvidenceBytes(repo string, roots map[string]string, e evidenceRecord, excerpt string, acceptURLs bool) ([]byte, error) {
	if strings.HasPrefix(e.Path, "https://") || strings.HasPrefix(e.Path, "http://") {
		if !acceptURLs || e.Range != nil || strings.TrimSpace(excerpt) == "" {
			return nil, fmt.Errorf("URL citation %s requires --accept-urls and a quoted snapshot without a range", e.ID)
		}
		return []byte(excerpt), nil
	}
	base, rel := repo, e.Path
	if name, rest, found := strings.Cut(rel, "/"); found {
		if root, ok := roots[name]; ok {
			base, rel = root, rest
		}
	}
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	full, err := filepath.EvalSymlinks(filepath.Join(base, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("evidence %s does not resolve: %v", e.ID, err)
	}
	inside, err := filepath.Rel(base, full)
	if err != nil || filepath.IsAbs(rel) || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("evidence %s is outside its root", e.ID)
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("evidence %s is not a regular file", e.ID)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	if e.Range == nil {
		return b, nil
	}
	r := e.Range
	if r.StartColumn != nil || r.EndColumn != nil || r.ColumnEncoding != "" {
		return nil, fmt.Errorf("evidence %s column ranges are not supported by sweep", e.ID)
	}
	lines := bytes.SplitAfter(b, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if r.EndLine > len(lines) {
		return nil, fmt.Errorf("evidence %s range does not resolve", e.ID)
	}
	return bytes.Join(lines[r.StartLine-1:r.EndLine], nil), nil
}

// A dropped resolution must assess the claimed answered_by pointer, not some
// other valid citation. This deliberately accepts only exact line pointers.
func sweepSupportsPointer(pointer string, records []evidenceRecord, support *claimSupport) bool {
	if support == nil {
		return false
	}
	for _, e := range records {
		cited := false
		for _, id := range support.EvidenceIDs {
			cited = cited || id == e.ID
		}
		if !cited {
			continue
		}
		locator := e.Path
		if e.Range != nil {
			locator += fmt.Sprintf(":%d", e.Range.StartLine)
			if e.Range.EndLine != e.Range.StartLine {
				locator += fmt.Sprintf("-%d", e.Range.EndLine)
			}
		}
		if locator == pointer {
			return true
		}
	}
	return false
}

func writeSweepResults(topic string, results []sweepResult, issues []string) error {
	record := struct {
		SchemaVersion    int                        `json:"schema_version"`
		Questions        map[string]sweepResolution `json:"questions"`
		AccountingOK     bool                       `json:"accounting_ok"`
		AccountingIssues []string                   `json:"accounting_issues"`
		ManifestSubject  *gateSubject               `json:"manifest_subject"`
	}{1, map[string]sweepResolution{}, len(issues) == 0, issues, nil}
	if b, err := os.ReadFile(filepath.Join(topic, "manifest.json")); err == nil && len(results) > 0 {
		subject := *results[0].subject
		subject.SpecSHA256 = subjectDigest(b)
		subject.ID = subject.digest()
		record.ManifestSubject = &subject
	}
	for _, r := range results {
		resolution := sweepResolution{CardRev: r.cardRev, Subject: r.subject, Status: r.status, Choice: r.choice, Why: r.why, Passes: []sweepPassObservation{}}
		for _, p := range r.passes {
			issue := p.supportErr
			if !p.bound {
				issue = "card_rev or code subject changed or is unbound"
			} else if len(p.bad) > 0 {
				issue = "evidence does not resolve: " + strings.Join(p.bad, ", ")
			}
			resolution.Passes = append(resolution.Passes, sweepPassObservation{Name: p.name, CitationValid: p.citationValid, ClaimSupport: p.ans.Support, Issue: issue})
		}
		record.Questions[r.key] = resolution
	}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(topic, "sweep")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "results.json"), append(b, '\n'), 0o644)
}

// checkDropped holds collect to account against its explicit source inventory.
// It never reconstructs the manifest from surviving cards: that would hide loss.
func checkDropped(topic string, cardPaths []string, repo string, roots map[string]string, acceptURLs bool) []string {
	var out []string
	manifestBytes, err := os.ReadFile(filepath.Join(topic, "manifest.json"))
	var manifest sweepManifest
	if err != nil {
		out = append(out, "manifest.json is missing or unreadable")
	} else if json.Unmarshal(manifestBytes, &manifest) != nil || manifest.SchemaVersion != 1 || manifest.Questions == nil {
		out = append(out, "manifest.json must contain schema_version 1 and a questions list")
	}
	keys := map[string]bool{}
	for _, p := range cardPaths {
		keys[strings.TrimSuffix(filepath.Base(p), ".md")] = true
	}
	sources := map[string]sweepSourceQuestion{}
	accounted := map[string]bool{}
	mappedCards := map[string]bool{}
	for _, q := range manifest.Questions {
		if strings.TrimSpace(q.ID) == "" || strings.TrimSpace(q.Question) == "" {
			out = append(out, "manifest.json has a source without id or question")
			continue
		}
		if _, exists := sources[q.ID]; exists {
			out = append(out, fmt.Sprintf("manifest.json repeats source %q", q.ID))
			continue
		}
		sources[q.ID] = q
		if q.Card != "" {
			if !keys[q.Card] || mappedCards[q.Card] {
				out = append(out, fmt.Sprintf("source %q maps to a missing or already mapped card %q; use duplicate_of for duplicates", q.ID, q.Card))
				continue
			}
			accounted[q.ID], mappedCards[q.Card] = true, true
		}
	}
	for key := range keys {
		if !mappedCards[key] {
			out = append(out, fmt.Sprintf("card %q has no source in manifest.json", key))
		}
	}
	b, err := os.ReadFile(filepath.Join(topic, "dropped.json"))
	var list []sweepDropped
	if err != nil {
		out = append(out, "dropped.json is missing or unreadable; write [] when nothing was dropped")
	} else if json.Unmarshal(b, &list) != nil || list == nil {
		out = append(out, "dropped.json is not a JSON list")
	}
	var subject *gateSubject
	for _, d := range list {
		q, exists := sources[d.SourceID]
		if !exists || d.Question != q.Question || strings.TrimSpace(d.Reason) == "" {
			out = append(out, fmt.Sprintf("%q has no matching source_id/question or nonempty reason", short(d.Question, 50)))
			continue
		}
		if accounted[d.SourceID] {
			out = append(out, fmt.Sprintf("source %q is accounted for more than once", d.SourceID))
			continue
		}
		accounted[d.SourceID] = true
		n := 0
		for _, v := range []string{d.DuplicateOf, d.AnsweredBy, d.Disposition} {
			if strings.TrimSpace(v) != "" {
				n++
			}
		}
		switch {
		case n == 0:
			out = append(out, fmt.Sprintf("%q was dropped with no duplicate_of, answered_by or disposition", short(d.Question, 50)))
		case n != 1:
			out = append(out, fmt.Sprintf("%q must have exactly one duplicate_of, answered_by or disposition", short(d.Question, 50)))
		case d.DuplicateOf != "" && !keys[d.DuplicateOf]:
			out = append(out, fmt.Sprintf("%q is marked a duplicate of %q, which is not a card", short(d.Question, 50), d.DuplicateOf))
		case d.AnsweredBy != "":
			if ok, urls, bad := sweepEvidence(repo, roots, []string{d.AnsweredBy}); len(bad) > 0 || ok == 0 && !(acceptURLs && urls > 0) {
				out = append(out, fmt.Sprintf("%q says %s answers it, but that does not resolve", short(d.Question, 50), d.AnsweredBy))
				continue
			}
			if subject == nil {
				subject, err = captureGateSubject(repo, manifestBytes)
			}
			if err != nil || !d.Subject.valid() || subject == nil || d.Subject.ID != subject.ID {
				out = append(out, fmt.Sprintf("%q answered_by is not bound to the current manifest and code subject", short(d.Question, 50)))
				continue
			}
			if !sweepSupportsPointer(d.AnsweredBy, d.EvidenceRecords, d.Support) {
				out = append(out, fmt.Sprintf("%q answered_by is not cited by its support assessment", short(d.Question, 50)))
				continue
			}
			if err := sweepClaimSupport(repo, roots, d.EvidenceRecords, d.Support, subject, acceptURLs); err != nil {
				out = append(out, fmt.Sprintf("%q answered_by lacks claim support: %v", short(d.Question, 50), err))
			}
		}
	}
	for id := range sources {
		if !accounted[id] {
			out = append(out, fmt.Sprintf("source %q is unaccounted; retain a card or record a duplicate, resolution or justified disposition", id))
		}
	}
	sort.Strings(out)
	return out
}
