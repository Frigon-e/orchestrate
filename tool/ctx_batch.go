package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// A batch is many lookups in one call: a plan of concept, symbol, text and
// read lookups, run in parallel from the cache, answered in plan order with
// each answer capped. It is what an agent uses instead of a dozen turns.

const (
	ctxResultCaps = 1500 // characters kept of one lookup's answer in a batch
	ctxReadLines  = 60
)

// ctxLookup is one lookup of a plan. Kind is concept, symbol, text or read;
// the other fields are options of that kind, all optional. Why is a label for
// the reader and never part of any cache key.
type ctxLookup struct {
	present map[string]bool // explicitly supplied options, including false and zero
	Kind    string          `json:"kind"`
	Query   string          `json:"query"`
	Path    string          `json:"path,omitempty"`
	From    int             `json:"from,omitempty"`
	To      int             `json:"to,omitempty"`
	Why     string          `json:"why,omitempty"`
	Package string          `json:"package,omitempty"`   // the gate's planner names one; ignored here
	Tests   string          `json:"tests,omitempty"`     // none (default), include or only
	Gen     string          `json:"generated,omitempty"` // generated files: none (default), include or only
	Max     int             `json:"max,omitempty"`       // alias of limit
	Limit   int             `json:"limit,omitempty"`     // page size
	Page    int             `json:"page,omitempty"`      // 1-based page of the list
	// text options (ripgrep)
	Fixed        bool    `json:"fixed,omitempty"`
	Word         bool    `json:"word,omitempty"`
	IgnoreCase   bool    `json:"ignore_case,omitempty"`
	Glob         strList `json:"glob,omitempty"`
	Type         string  `json:"type,omitempty"`
	FilesOnly    bool    `json:"files_only,omitempty"`
	Count        bool    `json:"count,omitempty"`
	Context      *int    `json:"context,omitempty"` // lines around a hit (default 2)
	Multiline    bool    `json:"multiline,omitempty"`
	OnlyMatching bool    `json:"only_matching,omitempty"`
	PerFile      int     `json:"per_file,omitempty"`
	All          bool    `json:"all,omitempty"` // also lockfiles, generated and vendored files
	// symbol options
	Refs    *int   `json:"refs,omitempty"` // callers listed (non-test; default 10)
	Impl    *bool  `json:"impl,omitempty"` // implementers listed (default true)
	DefKind string `json:"def_kind,omitempty"`
	In      string `json:"in,omitempty"`
}

// Keep custom decoding on the CLI plan type, not ctxLookup: the gate embeds
// ctxLookup next to package metadata, and a promoted UnmarshalJSON would swallow it.
type ctxPlanLookup ctxLookup

// UnmarshalJSON tracks explicit false/zero options for per-kind validation.
func (l *ctxPlanLookup) UnmarshalJSON(b []byte) error {
	type plain ctxLookup
	var p plain
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	*l = ctxPlanLookup(p)
	l.present = map[string]bool{}
	for k := range fields {
		l.present[k] = true
	}
	return nil
}

// strList decodes either one string or a list of strings.
type strList []string

func (l *strList) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*l = strList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

type ctxResult struct {
	Lookup ctxLookup `json:"lookup"`
	Head   string    `json:"head"`
	Output string    `json:"output"`
}

func (r ctxResult) String() string {
	return r.Head + "\n" + indent(r.Output, "    ")
}

func indent(s, pre string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pre + lines[i]
	}
	return strings.Join(lines, "\n")
}

// cutLines keeps whole lines of s within max characters.
func cutLines(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndex(s[:max], "\n")
	if cut < 0 { // one long line: keep its start
		return s[:max] + " ... (cut)"
	}
	return s[:cut] + "\n... (cut)"
}

// repoPath cleans a path and refuses one outside the repo.
func repoPath(p string) (string, bool) {
	p = filepath.ToSlash(filepath.Clean(strings.TrimPrefix(strings.TrimSpace(p), "./")))
	if p == "." || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

// Batch runs the lookups. rootOf gives the TypeScript project root of lookup i
// (nil: found from the query). capChars caps each answer; 0 means ctxResultCaps.
func (e *ctxEnv) Batch(lks []ctxLookup, rootOf func(i int) string, capChars int) []ctxResult {
	if capChars <= 0 {
		capChars = ctxResultCaps
	}
	root := func(i int) string {
		if rootOf != nil {
			return rootOf(i)
		}
		return tsProjectRoot(e.Code, lks[i].Query, nil)
	}
	// Validate every lookup before starting dependencies. This legacy API has
	// no error return, so keep valid results and name each invalid lookup.
	invalid := make([]error, len(lks))
	for i := range lks {
		invalid[i] = lks[i].validate()
	}
	// Symbols in one batch, so one language server serves them all.
	goSeen := map[string]bool{}
	var goNames []string
	tsSeen := map[tsJob]bool{}
	var tsJobs []tsJob
	for i, l := range lks {
		q := strings.TrimSpace(l.Query)
		if invalid[i] != nil || l.Kind != "symbol" || !symIdent.MatchString(q) {
			continue
		}
		if !goSeen[q] {
			goSeen[q] = true
			goNames = append(goNames, q)
		}
		if r := root(i); r != "" && tsPlain.MatchString(q) {
			if j := (tsJob{r, q}); !tsSeen[j] {
				tsSeen[j] = true
				tsJobs = append(tsJobs, j)
			}
		}
	}
	var goFacts map[string]*symFact
	var tsFacts map[tsJob]*symFact
	if len(goNames) > 0 {
		goFacts, tsFacts = e.Symbols(goNames, tsJobs)
	}
	out := make([]ctxResult, len(lks))
	e.parallel(len(lks), func(i int) {
		l := lks[i]
		l.Kind = strings.ToLower(strings.TrimSpace(l.Kind))
		var res string
		what := l.Query
		if invalid[i] != nil {
			out[i] = ctxResult{Lookup: l, Head: fmt.Sprintf("- [%s] `%s`", l.Kind, short(flat(what), 80)), Output: "error: " + invalid[i].Error()}
			return
		}
		switch l.Kind {
		case "concept":
			res = e.lookupConcept(l.Query, l.Path)
		case "symbol":
			q := strings.TrimSpace(l.Query)
			f := goFacts[q]
			if f == nil || len(f.Defs) == 0 {
				if t := tsFacts[tsJob{root(i), q}]; t != nil {
					f = t
				}
			}
			if f == nil {
				res = "error: symbol lookup unavailable or failed; references unknown"
			} else {
				res = strings.Join(e.sheetLines(*f, sheetOpts{Callers: 6, Impls: true}), "\n")
			}
		case "text":
			res = e.lookupText(l.Query, l.Path)
		case "read":
			what = fmt.Sprintf("%s:%d-%d", l.Path, l.From, l.To)
			res = e.lookupRead(l.Path, l.From, l.To)
		default:
			res = "unknown kind"
		}
		head := fmt.Sprintf("- [%s] `%s`", l.Kind, short(flat(what), 80))
		if l.Why != "" {
			head += " - " + short(flat(l.Why), 80)
		}
		out[i] = ctxResult{Lookup: l, Head: head, Output: cutLines(strings.TrimRight(res, "\n"), capChars)}
	})
	return out
}

func (e *ctxEnv) lookupConcept(q, pathFilter string) string {
	q = short(flat(q), 300)
	hits, err := e.Concept(q, pathFilter, 3)
	if errors.Is(err, errJBRateLimited) {
		return e.conceptFromRg(q, pathFilter, err)
	}
	if err != nil {
		return "error: " + err.Error()
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d-%d\n", h.Path, h.Start, h.End)
		for _, l := range h.Snippet[:min(3, len(h.Snippet))] {
			b.WriteString("  ")
			b.WriteString(short(l, 120))
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "no semantic candidates in the searched index window; this does not establish absence"
	}
	return b.String()
}

// conceptFromRg answers a concept lookup that jbcontext refused with a rate
// limit: the longer words of the query searched with rg, the hits that match the
// most distinct words first. The answer says jbcontext was skipped.
func (e *ctxEnv) conceptFromRg(q, pathFilter string, cause error) string {
	var words []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		if len(w) >= 4 && !conceptStopwords[w] && !seen[w] && len(words) < 6 {
			seen[w] = true
			words = append(words, w)
		}
	}
	head := "jbcontext skipped: rate limited (429); " + cause.Error() + "\n"
	if len(words) == 0 {
		return head + "rg fallback: no searchable word in the query"
	}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = regexp.QuoteMeta(w)
	}
	var path string
	if p, ok := repoPath(pathFilter); ok && pathFilter != "" {
		path = p
	}
	res, err := e.Text(rgQuery{Pattern: strings.Join(quoted, "|"), Path: path, IgnoreCase: true, PerFile: 3})
	if err != nil {
		return head + "rg fallback failed: " + err.Error()
	}
	score := func(h rgHit) int {
		n, t := 0, strings.ToLower(h.Text)
		for _, w := range words {
			if strings.Contains(t, w) {
				n++
			}
		}
		return n
	}
	hits := append([]rgHit(nil), res.Hits...)
	sort.SliceStable(hits, func(i, j int) bool { return score(hits[i]) > score(hits[j]) })
	var b strings.Builder
	b.WriteString(head)
	b.WriteString("rg fallback on words: ")
	b.WriteString(strings.Join(words, ", "))
	b.WriteString("\n")
	if len(hits) == 0 {
		b.WriteString("no matches")
	}
	for _, h := range hits[:min(10, len(hits))] {
		fmt.Fprintf(&b, "%s:%d: %s\n", h.Path, h.Line, short(h.Text, 160))
	}
	return b.String()
}

var conceptStopwords = map[string]bool{"code": true, "that": true, "when": true, "with": true, "from": true, "into": true, "this": true, "where": true, "what": true, "which": true, "producing": true, "trigger": true, "result": true, "does": true, "while": true}

func (e *ctxEnv) lookupText(q, path string) string {
	q = strings.TrimSpace(q)
	if q == "" || len(q) > 200 {
		return "error: empty or too long query"
	}
	if path != "" {
		p, ok := repoPath(path)
		if !ok {
			return "error: path outside the repo"
		}
		path = p
	}
	res, err := e.Text(rgQuery{Pattern: q, Path: path, AllFiles: true, CaseSensitive: true})
	if err != nil {
		return "error: " + err.Error()
	}
	if len(res.Hits) == 0 {
		return "no matches"
	}
	var b strings.Builder
	for _, h := range res.Hits[:min(20, len(res.Hits))] {
		fmt.Fprintf(&b, "%s:%d: %s\n", h.Path, h.Line, short(h.Text, 160))
	}
	return b.String()
}

func (e *ctxEnv) lookupRead(path string, from, to int) string {
	p, ok := repoPath(path)
	if !ok {
		return "error: path outside the repo"
	}
	if real, err := filepath.EvalSymlinks(filepath.Join(e.Code, p)); err != nil || !strings.HasPrefix(real, evalOr(e.Code)+string(filepath.Separator)) {
		return "error: file not found in the repo"
	}
	ls := e.Lines(p)
	if len(ls) == 0 {
		return "error: file not found or empty"
	}
	from = max(from, 1)
	if to < from || to-from+1 > ctxReadLines {
		to = from + ctxReadLines - 1
	}
	to = min(to, len(ls))
	if from > to {
		return "error: range past the end of the file"
	}
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i, strings.TrimRight(ls[i-1], "\r"))
	}
	return b.String()
}
