package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// lookupJob is one text or concept search of a brief. A text job looks
// name up in the tracked files; a concept job searches query within tree.
type lookupJob struct {
	text       bool
	name, tree string
}

// jobOut is what a job returned and when it ran.
type jobOut struct {
	text    rgResult
	hits    []codeHit
	err     error
	started time.Time
	elapsed time.Duration
}

const (
	// lookupPoolWidth is the most concept searches a brief keeps in flight,
	// below the slot cap: the service answers 429 when it is asked too fast, and
	// a refused search is a lookup the brief loses. Measured on the CRM spec
	// (4 packages): width 4 had no refusal, 6 had 3, 8 had 25, 16 had 64.
	lookupPoolWidth = 5
	// lookupRateRounds is how many times the searches the service refused are
	// asked again, after the cooldown, one half as wide each time.
	lookupRateRounds = 2
)

// lookupCooldownWait waits until jbcontext is out of its cooldown, at most as
// long as the cooldown plus a margin. A variable so a test need not wait.
var lookupCooldownWait = func(bin string) {
	for end := time.Now().Add(jbCooldown + 5*time.Second); jbCooling(bin) && time.Now().Before(end); {
		time.Sleep(250 * time.Millisecond)
	}
}

// runJobs runs the jobs and returns their answers in job order, whatever the
// order they finished in. The text searches run a few at a time; the concept
// searches run as one pool, as wide as the slot cap allows up to
// lookupPoolWidth, at the same time. A concept search the service refused for
// rate is asked again after the cooldown; its timing is that of the last ask.
func (e *ctxEnv) runJobs(jobs []lookupJob) []jobOut {
	outs := make([]jobOut, len(jobs))
	var texts, concepts []int
	for i, j := range jobs {
		if j.text {
			texts = append(texts, i)
		} else {
			concepts = append(concepts, i)
		}
	}
	run := func(i int) {
		j := jobs[i]
		o := &outs[i]
		o.started = time.Now()
		if j.text {
			o.text, o.err = e.Text(rgQuery{Pattern: j.name, Fixed: true, PerFile: 3, AllFiles: true, CaseSensitive: true})
		} else {
			o.hits, o.err = e.Concept(j.name, j.tree, briefConceptHits)
		}
		o.elapsed = time.Since(o.started)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		e.parallel(len(texts), func(k int) { run(texts[k]) })
	}()
	width := min(jbSlotCount(), lookupPoolWidth)
	go func() {
		defer wg.Done()
		fanOut(width, len(concepts), func(k int) { run(concepts[k]) })
		for round := 1; round <= lookupRateRounds; round++ {
			var again []int
			for _, i := range concepts {
				if errors.Is(outs[i].err, errJBRateLimited) {
					again = append(again, i)
				}
			}
			if len(again) == 0 {
				return
			}
			lookupCooldownWait(e.JB)
			fanOut(max(1, width>>round), len(again), func(k int) { run(again[k]) })
		}
	}()
	wg.Wait()
	return outs
}

// ctxLookup contains only JSON value types. Keep the original selectors, not
// a shortened prose query, on every expansion route.
func mustLookupJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Errorf("internal lookup selector serialization: %w", err))
	}
	return b
}

// lookupRecord preserves B1 execution and completeness independently. The
// legacy semantic API has unknown global counts, even after execution completes.
func lookupRecord(query, scope, unit string, n, limit int, err error, unavailable string) evidenceLookup {
	zero, one := 0, 1
	l := evidenceLookup{SchemaVersion: evidenceSchemaVersion, ID: "lookup-" + subjectJSONDigest([]any{query, scope, unit, n, fmt.Sprint(err), unavailable})[:20], Query: query, Scope: orDefault(scope, "."), Unit: unit, Exclusions: []string{"spec input excluded; candidate selection separately excludes tests, generated files and owns"}, CountKind: evidenceCountUnknown, Completeness: evidenceUnknown, ExecutionStatus: lookupNotRequested, Counts: lookupCounts{Requested: &zero}}
	if unavailable != "" {
		l.ExecutionStatus = lookupUnavailable
		l.Counts.Requested = &one
		l.Errors = []string{unavailable}
		return l
	}
	l.Counts = lookupCounts{Requested: &one, Succeeded: &one, Failed: &zero, Total: &n}
	l.CountKind = evidenceCountExact
	l.Completeness = evidenceComplete
	l.ExecutionStatus = lookupSucceeded
	l.Status = "ok"
	if n == 0 {
		l.Status = "empty"
	}
	if limit > 0 && n >= limit {
		l.CountKind = evidenceCountLowerBound
		l.Completeness = evidencePartial
		l.Status = "partial"
		l.Omissions = []string{"bounded result page; narrow the same scope and retry"}
	}
	if unit == "concept" {
		// The legacy semantic API returns a filtered index window, not an
		// exhaustive repository count. Preserve A4's unknown completeness.
		l.Status = "partial"
		l.CountKind = evidenceCountUnknown
		l.Completeness = evidencePartial
		l.Counts.Total = nil
		l.Omissions = []string{fmt.Sprintf("completed bounded window: %d visible candidate(s); candidates never establish absence; expand lookup kind=concept query=%q path=%q tests=none generated=none", n, query, orDefault(scope, "."))}
	}
	if err != nil {
		l.ExecutionStatus = lookupFailed
		l.Status = "failed"
		l.Completeness = evidenceUnknown
		l.CountKind = evidenceCountUnknown
		l.Counts.Succeeded = &zero
		l.Counts.Failed = &one
		l.Counts.Total = nil
		l.Errors = []string{err.Error()}
	}
	return l
}

func symbolRecord(query, scope string, f *symFact) evidenceLookup {
	if f == nil {
		return lookupRecord(query, scope, "symbol", 0, 0, nil, "symbol backend returned no execution record")
	}
	var err error
	if len(f.Errors) > 0 {
		err = fmt.Errorf("%s", strings.Join(f.Errors, "; "))
	}
	l := lookupRecord(query, scope, "symbol", len(f.Defs), 0, err, "")
	l.ID = "lookup-" + subjectJSONDigest([]any{query, scope, "symbol", f})[:20]
	if err == nil {
		for _, d := range f.Defs {
			if d.Match != "exact" || d.RefsUnknown || len(d.Errors) > 0 {
				l.Status = "partial"
				l.Completeness = evidencePartial
				l.CountKind = evidenceCountLowerBound
				l.Omissions = append(l.Omissions, "candidate identity or references remain unresolved; expand the same scoped symbol lookup")
			}
		}
	}
	return l
}

func textRecord(query, scope string, r rgResult, err error) evidenceLookup {
	l := lookupRecord(query, scope, "text", r.Total, 0, err, "")
	l.Scope = string(mustLookupJSON(map[string]any{"path": orDefault(scope, "."), "fixed": true, "case_sensitive": true, "per_file": 3, "all_files": true}))
	if err == nil && (!r.Complete || r.Limited) {
		l.Status = "partial"
		l.Completeness = evidencePartial
		l.CountKind = evidenceCountLowerBound
		l.Omissions = []string{fmt.Sprintf("text retention/backend cap; expand lookup kind=text query=%q path=%q with the same case/word/glob selectors", query, orDefault(scope, "."))}
	}
	l.ID = "lookup-" + subjectJSONDigest([]any{query, scope, "text", r, fmt.Sprint(err)})[:20]
	return l
}

var (
	failureTemp    = regexp.MustCompile(`(^|[\s:(])(?:/private)?/(?:var/folders/[^/\s]+/[^/\s]+/T|tmp)`)
	failureMissing = regexp.MustCompile(`\S*/([^\s/]+?)(?: was)? not found`)
	failureKnown   = []struct {
		re *regexp.Regexp
		to string
	}{
		{regexp.MustCompile(`^(?:<repo>|<tmp>|~): `), ""},
		{regexp.MustCompile(`^no TypeScript facts: `), ""},
		{regexp.MustCompile(`no native TypeScript 7 server found(?: \([^)]*node_modules[^)]*\))?`), "typescript@7 server not found"},
		{regexp.MustCompile(`; set (ORCH_[A-Z0-9_]+) to one`), " (set $1)"},
	}
)

// shortPathText replaces the checkout root, a home prefix and a temp prefix, so
// the same path renders the same way on any machine.
func shortPathText(s, root string) string {
	if root != "" {
		s = strings.ReplaceAll(s, root, "<repo>")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return failureTemp.ReplaceAllString(s, "$1<tmp>")
}

func lookupSource(e *ctxEnv, path string, start, end int) evidenceRecord {
	ls := e.Lines(path)
	from := min(len(ls), max(0, start-1))
	to := min(len(ls), max(from, end))
	body := strings.Join(ls[from:to], "\n")
	id := lookupSourceID(path, start, end, body)
	valid := start >= 1 && end >= start && end <= len(ls)
	complete := evidenceComplete
	if !valid {
		complete = evidenceUnknown
	}
	kind := evidenceSourceCode
	if isTestPath(path) {
		kind = evidenceSourceTest
	} else if isDocPath(path) {
		kind = evidenceSourceDocument
	}
	return evidenceRecord{SchemaVersion: evidenceSchemaVersion, ID: id, Completeness: complete, SourceKind: kind, Language: langIDOf(path), Path: path, Range: &evidenceRange{StartLine: start, EndLine: end}, ContentHash: subjectDigest([]byte(body)), LookupID: "lookup-" + id, Selected: &valid, LocatorValid: &valid}
}

func lookupSourceID(path string, start, end int, body string) string {
	return "evidence-" + subjectJSONDigest([]any{path, start, end, subjectDigest([]byte(body))})[:20]
}
func readHint(path string, start, end int) string {
	return fmt.Sprintf("Read path=%q offset=%d limit=%d", path, max(1, start), max(1, end-start+1))
}
