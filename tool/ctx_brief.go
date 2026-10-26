package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// A brief is what code can establish about a piece of work before anyone
// reads it: which files matter and why (ranked by how many independent lookups
// agree), the exact interfaces it touches, existing code that looks like what
// it builds, and names that exist nowhere. It is built from the task text, an
// optional owns list and the lookups of the layer, with no model. The text can
// be a spec package, a task.md or a question.

const (
	briefOwnsCap     = 40 // files taken from the owns
	briefFiles       = 12 // ranked files kept
	briefNames       = 20 // names looked up with rg
	briefSyms        = 8  // symbols per language looked up
	briefConceptHits = 4  // chunks kept per concept query
	briefAccept      = 6  // acceptance bullets used as concept queries
	briefLikeMax     = 8  // look-alikes kept
	briefLikeTree    = 4  // look-alikes kept per code tree
	briefDocs        = 5  // docs listed
	briefRare        = 20 // a name with more hits than this is noise
	briefTextLike    = 5  // a name with more hits than this is not shown as a look-alike
)

type briefInput struct {
	ID    string
	Title string
	Text  string   // the whole task text: pointers, symbols and names are read from it
	Body  string   // the part that says what is built; Text when empty
	Owns  []string // files or directories the work owns
}

var (
	briefPtrRe = regexp.MustCompile(`([A-Za-z0-9_@.][A-Za-z0-9_@./-]*\.[A-Za-z0-9]{1,5})(?::(\d+)|#([A-Za-z_][A-Za-z0-9_.]*))`)
	endpointRe = regexp.MustCompile(`\b(?:GET|POST|PUT|PATCH|DELETE)\s+/[A-Za-z0-9_/{}:.-]+`)
	snakeRe    = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)
)

// inOwns reports whether path is one of the owns entries or below one.
func inOwns(path string, owns []string) bool {
	for _, o := range owns {
		o = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(o), "./"), "/")
		if o != "" && (path == o || strings.HasPrefix(path, o+"/")) {
			return true
		}
	}
	return false
}

// ownedFiles lists the existing files of the owns entries: a file as itself,
// a directory by its tracked files, up to briefOwnsCap.
func (e *ctxEnv) ownedFiles(owns []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(f string) {
		if len(out) < briefOwnsCap && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, o := range owns {
		o = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(o), "./"), "/")
		if o == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(e.Code, o)); err == nil && !st.IsDir() {
			add(o)
			continue
		}
		for _, t := range e.files() {
			if strings.HasPrefix(t, o+"/") {
				add(t)
			}
		}
	}
	return out
}

type pointer struct {
	path, sym string
	exists    bool
}

// pointers are the `path:line` and `path#Symbol` references in text.
func (e *ctxEnv) pointers(text string) []pointer {
	var out []pointer
	seen := map[string]bool{}
	for _, m := range briefPtrRe.FindAllStringSubmatch(text, -1) {
		path := strings.TrimPrefix(m[1], "./")
		if e.Skip[path] || !strings.Contains(path, ".") {
			continue
		}
		key := path + "#" + m[3]
		if seen[key] {
			continue
		}
		seen[key] = true
		st, err := os.Stat(filepath.Join(e.Code, path))
		out = append(out, pointer{path: path, sym: m[3], exists: err == nil && !st.IsDir()})
	}
	return out
}

// briefPlan is what is looked up for one brief and what came back.
type briefPlan struct {
	in       briefInput
	owned    []string
	ptrs     []pointer
	goSyms   []string
	tsSyms   []string
	tsRoot   string
	names    []string // looked up with rg
	files    []string // file names looked up among the tracked files
	concepts []string
	trees    []string // code trees concept searches are narrowed to ("" when the repo has none)

	facts   map[string]*symFact // by symbol, the language that found it
	rg      map[string][]rgHit
	concept map[string][]codeHit // by query + "\x00" + tree
	accept  []string             // acceptance bullets
	qnames  []string             // names the concept queries use
	purpose string               // the short purpose phrase of the queries
	lookups []evidenceLookup
}

func addUniq(list []string, s string, max int) []string {
	if slices.Contains(list, s) {
		return list
	}
	if len(list) >= max {
		return list
	}
	return append(list, s)
}

func (e *ctxEnv) prepareBrief(in briefInput) *briefPlan {
	pl := &briefPlan{in: in, facts: map[string]*symFact{}, rg: map[string][]rgHit{}, concept: map[string][]codeHit{}}
	block := in.Text
	pl.owned = e.ownedFiles(in.Owns)
	pl.ptrs = e.pointers(block)
	lines := strings.Split(block, "\n")
	for _, l := range lines {
		for _, s := range goSymbols(l) {
			pl.goSyms = addUniq(pl.goSyms, s, briefSyms)
		}
	}
	// Names that might be symbols in any language; the declaration tier decides.
	for _, l := range lines {
		for _, s := range genericSymbols(l) {
			pl.goSyms = addUniq(pl.goSyms, s, briefSyms+8)
		}
	}
	pl.tsRoot = tsProjectRoot(e.Code, block, in.Owns)
	if pl.tsRoot != "" {
		for _, l := range lines {
			for _, s := range tsSymbols(l, nil) {
				pl.tsSyms = addUniq(pl.tsSyms, s, briefSyms)
			}
		}
	}
	for _, ptr := range pl.ptrs {
		if ptr.sym == "" || !ptr.exists {
			continue
		}
		if langIDOf(ptr.path) == "typescript" && pl.tsRoot != "" {
			if tsPlain.MatchString(ptr.sym) {
				pl.tsSyms = addUniq(pl.tsSyms, ptr.sym, briefSyms+4)
			}
		} else if langIDOf(ptr.path) != "" {
			pl.goSyms = addUniq(pl.goSyms, ptr.sym, briefSyms+4)
		}
	}
	// Names to find by text: backticked names, endpoints, snake_case names, then the symbols.
	for _, m := range idBacktick.FindAllStringSubmatch(block, -1) {
		span := strings.TrimSpace(m[1])
		switch {
		case briefPtrRe.MatchString(span) && !strings.Contains(span, " "):
			continue
		case strings.ContainsAny(span, "*?") || (strings.HasSuffix(span, "_test") && !strings.ContainsAny(span, "./")):
			continue // a glob or a bare test-file name is not a thing to find
		case fileExtRe.MatchString(span) && !strings.Contains(span, " "):
			pl.files = addUniq(pl.files, filepath.Base(span), briefNames)
		case len(span) >= 3 && !strings.ContainsAny(span, " \t"):
			pl.names = addUniq(pl.names, span, briefNames)
		}
	}
	for _, s := range endpointRe.FindAllString(block, -1) {
		pl.names = addUniq(pl.names, strings.TrimRight(s, ".,;:)"), briefNames)
	}
	for _, s := range snakeRe.FindAllString(block, -1) {
		if len(s) >= 5 {
			pl.names = addUniq(pl.names, s, briefNames)
		}
	}
	for _, s := range append(append([]string{}, pl.goSyms...), pl.tsSyms...) {
		pl.names = addUniq(pl.names, s, briefNames)
	}
	pl.accept = acceptanceBullets(block)
	pl.trees = codeTreesFor(e.Code, in.Owns)
	return pl
}

// briefRun is Briefs in halves: briefsPrepare plans every lookup, symbols runs
// the symbol lookups, and finish builds the briefs from the answers. The symbol
// lookups and the jobs may run at the same time between prepare and finish.
type briefRun struct {
	e       *ctxEnv
	plans   []*briefPlan
	goNames []string
	tsJobs  []tsJob
	jobs    []lookupJob
	owner   []*briefPlan // the plan of jobs[i]
	jbReady bool

	goFacts map[string]*symFact
	tsFacts map[tsJob]*symFact
}

func (e *ctxEnv) briefsPrepare(ins []briefInput) *briefRun {
	r := &briefRun{e: e, jbReady: e.jbMissing() == ""}
	for _, in := range ins {
		r.plans = append(r.plans, e.prepareBrief(in))
	}
	goSeen := map[string]bool{}
	tsSeen := map[tsJob]bool{}
	for _, pl := range r.plans {
		for _, s := range pl.goSyms {
			if !goSeen[s] {
				goSeen[s] = true
				r.goNames = append(r.goNames, s)
			}
		}
		for _, s := range pl.tsSyms {
			if j := (tsJob{pl.tsRoot, s}); !tsSeen[j] {
				tsSeen[j] = true
				r.tsJobs = append(r.tsJobs, j)
			}
		}
	}
	// The concept queries do not depend on the symbols (only the names they
	// report do), so the jobs are known now; finish sets the queries again.
	for _, pl := range r.plans {
		pl.makeQueries()
		for _, n := range pl.names {
			r.jobs = append(r.jobs, lookupJob{text: true, name: n})
			r.owner = append(r.owner, pl)
		}
		if r.jbReady {
			for _, q := range pl.concepts {
				for _, tr := range pl.trees {
					r.jobs = append(r.jobs, lookupJob{name: q, tree: tr})
					r.owner = append(r.owner, pl)
				}
			}
		}
	}
	return r
}

// symbols looks up the symbols of all inputs in one batch, so one language
// server serves them all.
func (r *briefRun) symbols() {
	r.goFacts, r.tsFacts = r.e.Symbols(r.goNames, r.tsJobs)
}

// finish builds one brief per input from the symbol facts and the answers of the
// jobs, which are in the order of r.jobs.
func (r *briefRun) finish(outs []jobOut) []*ctxBrief {
	e := r.e
	for _, pl := range r.plans {
		for _, s := range pl.goSyms {
			pl.lookups = append(pl.lookups, symbolRecord(s, ".", r.goFacts[s]))
			if f := r.goFacts[s]; f != nil && len(f.Defs) > 0 {
				pl.facts[s] = f
			}
		}
		for _, s := range pl.tsSyms {
			pl.lookups = append(pl.lookups, symbolRecord(s, pl.tsRoot, r.tsFacts[tsJob{pl.tsRoot, s}]))
			if f := r.tsFacts[tsJob{pl.tsRoot, s}]; f != nil && len(f.Defs) > 0 && pl.facts[s] == nil {
				pl.facts[s] = f
			}
		}
	}
	for _, pl := range r.plans {
		pl.makeQueries()
	}
	if !r.jbReady {
		e.note(e.jbMissing())
		for _, pl := range r.plans {
			for _, q := range pl.concepts {
				for _, tree := range pl.trees {
					pl.lookups = append(pl.lookups, lookupRecord(q, tree, "concept", 0, briefConceptHits, nil, e.jbMissing()))
				}
			}
		}
	}
	for i, j := range r.jobs {
		pl, o := r.owner[i], outs[i]
		if j.text {
			pl.lookups = append(pl.lookups, textRecord(j.name, ".", o.text, o.err))
			if o.err != nil {
				e.note(o.err.Error())
				continue
			}
			pl.rg[j.name] = o.text.Hits
			continue
		}
		pl.lookups = append(pl.lookups, lookupRecord(j.name, j.tree, "concept", len(o.hits), briefConceptHits, o.err, ""))
		if o.err != nil {
			e.note(o.err.Error())
			continue
		}
		pl.concept[j.name+"\x00"+j.tree] = o.hits
	}
	out := make([]*ctxBrief, len(r.plans))
	for i, pl := range r.plans {
		out[i] = e.assemble(pl)
	}
	return out
}

// Briefs builds one brief per input. The symbols of all inputs go in one batch,
// so one language server serves them all, and run beside the searches.
func (e *ctxEnv) Briefs(ins []briefInput) []*ctxBrief {
	r := e.briefsPrepare(ins)
	var sym sync.WaitGroup
	sym.Add(1)
	go func() {
		defer sym.Done()
		r.symbols()
	}()
	outs := e.runJobs(r.jobs)
	sym.Wait()
	return r.finish(outs)
}

type briefFile struct {
	Path  string
	Score int
	Why   []string
}

type briefLike struct {
	At, In, Via string
	Snippet     []string
}

type briefNativeLike struct {
	text   string
	source evidenceRecord
}

type ctxBrief struct {
	// Render-only views keep legacy --json Sheet/Like/LikeRows unchanged.
	root                              string
	interfaceSummary                  []string
	nativeLike                        map[string]briefNativeLike
	ID                                string
	Files, Docs, Sheet, Like, Missing []string
	Lookups                           []evidenceLookup

	// The same content as data, for the TOON view of `orch ctx brief`.
	FileRows []briefFile
	Facts    []symFact
	LikeRows []briefLike
	Names    []string
}

// capEntries keeps whole entries while they fit in max characters; the rest
// is counted in a last line.
func capEntries(entries []string, max int) string {
	if len(entries) == 0 {
		return "(none)\n"
	}
	var b strings.Builder
	for i, en := range entries {
		if b.Len()+len(en)+1 > max {
			fmt.Fprintf(&b, "- ... %d more omitted\n", len(entries)-i)
			break
		}
		b.WriteString(en)
		b.WriteString("\n")
	}
	return b.String()
}

// docLine is the first line of the comment block right above line ln (1-based).
func docLine(lines []string, ln int) string {
	top := -1
	for i := ln - 2; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*") {
			top = i
			continue
		}
		break
	}
	if top < 0 {
		return ""
	}
	for i := top; i < ln-1; i++ {
		t := strings.TrimSpace(lines[i])
		t = strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(t, "/**"), "/*"), "/*"))
		t = strings.TrimSpace(strings.TrimSuffix(t, "*/"))
		if t != "" && t != "*" {
			return short(t, 140)
		}
	}
	return ""
}

func splitWhere(where string) (string, int) {
	i := strings.LastIndex(where, ":")
	if i < 0 {
		return where, 0
	}
	var n int
	fmt.Sscanf(where[i+1:], "%d", &n)
	return where[:i], n
}

func quoteList(ss []string) string {
	var out []string
	for _, s := range ss {
		out = append(out, "`"+s+"`")
	}
	return strings.Join(out, ", ")
}

const (
	briefIntro         = "Absence of a hit is not proof of absence."
	searchNotesMaxWays = 4 // failure reasons listed in the search notes
	searchNotesNames   = 12
)

var (
	noteJSONMessage = regexp.MustCompile(`\{[^{}]*"message":\s*"((?:[^"\\]|\\.)*)"[^{}]*\}?`)
	noteLeadIn      = regexp.MustCompile(`^(?:working-tree semantic search failed: )?(?:jbcontext failed: (?:exit status \d+: )?)?`)
	noteEmail       = regexp.MustCompile(`\s*\([^()]*@[^()]*\)`)
	noteLicense     = regexp.MustCompile(`(license) [A-Z0-9]{6,}`)
)

// noteReason is a failure reason for the search notes: the message inside a
// JSON error body, without license ids, e-mail addresses or machine paths, cut
// at its first sentence. Two failures with the same cause give the same text.
func noteReason(msg, root string) string {
	s := shortPathText(failureMissing.ReplaceAllString(strings.TrimSpace(msg), "$1 not found"), root)
	s = noteJSONMessage.ReplaceAllString(s, "$1")
	if i := strings.Index(s, `{"`); i >= 0 {
		s = strings.TrimRight(s[:i], ": ")
	}
	for _, rw := range failureKnown {
		s = rw.re.ReplaceAllString(s, rw.to)
	}
	s = noteLeadIn.ReplaceAllString(s, "")
	s = noteEmail.ReplaceAllString(s, "")
	s = noteLicense.ReplaceAllString(s, "$1")
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i]
	}
	return short(strings.Join(strings.Fields(s), " "), 110)
}

// searchNotes is the closing block of the brief: how the lookups ended, one
// line per distinct failure reason, and the names that exist nowhere. At most
// 8 lines, "" when there is nothing to say.
func searchNotes(lks []evidenceLookup, missing []string, root string) string {
	var run, ok, partial, failed, unavailable int
	type group struct {
		status, reason string
		n              int
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, l := range lks {
		switch l.ExecutionStatus {
		case lookupNotRequested:
			continue
		case lookupFailed:
			failed++
		case lookupUnavailable:
			unavailable++
		default:
			if l.Status == "partial" {
				partial++
			} else {
				ok++
			}
		}
		run++
		if len(l.Errors) == 0 {
			continue
		}
		reason := noteReason(strings.Join(l.Errors, "; "), root)
		key := string(l.ExecutionStatus) + "\x00" + reason
		g := byKey[key]
		if g == nil {
			g = &group{status: string(l.ExecutionStatus), reason: reason}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.n++
	}
	var names []string
	for _, m := range missing {
		if m = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m), "- ")); m != "" {
			names = append(names, m)
		}
	}
	if run == 0 && len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Search notes\n")
	if run > 0 {
		fmt.Fprintf(&b, "Lookups: %d run, %d ok, %d partial, %d failed, %d unavailable.\n", run, ok, partial, failed, unavailable)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].n != groups[j].n {
			return groups[i].n > groups[j].n
		}
		return groups[i].reason < groups[j].reason
	})
	for i, g := range groups {
		if i == searchNotesMaxWays-1 && len(groups) > searchNotesMaxWays {
			fmt.Fprintf(&b, "%d more failure reasons; see the manifest.\n", len(groups)-i)
			break
		}
		noun := "lookups"
		if g.n == 1 {
			noun = "lookup"
		}
		fmt.Fprintf(&b, "%d %s %s: %s\n", g.n, noun, g.status, g.reason)
	}
	if len(names) > 0 {
		more := ""
		if len(names) > searchNotesNames {
			more, names = fmt.Sprintf(" (+%d more)", len(names)-searchNotesNames), names[:searchNotesNames]
		}
		fmt.Fprintf(&b, "Names looked up that were not found: %s%s.\n", strings.Join(names, ", "), more)
	}
	return b.String()
}

// Render has one byte budget for ALL sections, including headings, source,
// docs and expansion instructions. No section receives a second cap.
//
// The brief leads with what the scoring established (ranked files, interface
// sheet, look-alikes) and ends with a short note on the searches. Selection
// runs in that order of value, so a tight budget cuts the search notes, then
// the look-alikes; the lookup records are not part of the markdown at all.
func (d *ctxBrief) Render(heading, noun string, total int) string {
	// Selection tiers, first to last: the scored picture and its structure, the
	// search notes, then the look-alikes.
	const (
		tierCore = iota
		tierNotes
		tierExtra
	)
	type entry struct {
		text, id string
		tier     int
		sec      string // the section whose heading this entry needs
		head     bool   // this entry is the heading of sec
		kind     string // what a cut of this entry is called in the cut line
	}
	var entries []entry
	headOf := map[string]int{}
	add := func(text string) { entries = append(entries, entry{text: text + "\n"}) }
	section := func(sec, text string) {
		headOf[sec] = len(entries)
		entries = append(entries, entry{text: text + "\n", sec: sec, head: true})
	}
	in := func(sec, text string) { entries = append(entries, entry{text: text + "\n", sec: sec}) }

	add(heading)
	add(briefIntro)
	if len(d.Files) > 0 {
		section("files", "### 1. Ranked files")
		for _, text := range d.Files {
			in("files", text)
		}
	}
	if len(d.Docs) > 0 {
		section("docs", "### 1b. Docs that mention it")
		for _, text := range d.Docs {
			in("docs", text)
		}
	}
	sheet := d.Sheet
	if d.interfaceSummary != nil {
		sheet = d.interfaceSummary
	}
	if len(sheet) > 0 {
		section("sheet", "### 2. Interface sheet")
		for i, text := range sheet {
			entries = append(entries, entry{text: text + "\n", id: fmt.Sprintf("signature-summary:%d", i), sec: "sheet"})
		}
	}
	seen := map[string]bool{}
	if len(d.Like) > 0 {
		section("like", "### 3. Existing things that look like what this "+noun+" builds (outside its owns)")
		for _, text := range d.Like {
			native, ok := d.nativeLike[text]
			if !ok {
				entries = append(entries, entry{text: text + "\n", sec: "like", tier: tierExtra, kind: "other"})
				continue
			} // notes/cut notices have no source body
			er := native.source
			if seen[er.ID] {
				in("like", "- source "+er.ID+" referenced above; expand: "+readHint(er.Path, er.Range.StartLine, er.Range.EndLine))
				continue
			}
			seen[er.ID] = true
			entries = append(entries, entry{text: native.text + "\n", id: er.ID, sec: "like", tier: tierExtra, kind: "other"})
		}
	}
	add("Coverage gaps: candidate relevance is unverified. Expand incoming/outgoing references, registrations/configuration and tests where material; record any uncompleted checks.")
	if notes := searchNotes(d.Lookups, d.Missing, d.root); notes != "" {
		entries = append(entries, entry{text: notes, id: "search-notes", tier: tierNotes, kind: "notes"})
	}
	// Reserve room for the one-line cut notice before selecting.
	reserve := min(total/3, 400)
	limit := max(0, total-reserve)
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return entries[order[a]].tier < entries[order[b]].tier })
	kept := make([]bool, len(entries))
	headKept := map[string]bool{}
	used := 0
	for _, i := range order {
		en := entries[i]
		if en.head {
			continue
		}
		cost := len(en.text)
		if en.sec != "" && !headKept[en.sec] {
			cost += len(entries[headOf[en.sec]].text)
		}
		if used+cost > limit {
			continue
		}
		kept[i], used = true, used+cost
		if en.sec != "" {
			headKept[en.sec] = true
		}
	}
	var b strings.Builder
	cut := map[string]int{}
	cutAny := 0
	for i, en := range entries {
		if en.head {
			if headKept[en.sec] {
				b.WriteString(en.text)
			}
			continue
		}
		if kept[i] {
			b.WriteString(en.text)
			continue
		}
		if en.id == "" && en.kind == "" && en.tier == tierCore && en.sec == "" {
			continue // structure lines carry no evidence
		}
		cutAny++
		if en.kind != "" {
			cut[en.kind]++
		}
	}
	if cutAny > 0 {
		var what []string
		if cut["notes"] > 0 {
			what = append(what, "the search notes")
		}
		if n := cut["other"]; n > 0 {
			what = append(what, fmt.Sprintf("%d look-alike entr(ies)", n))
		}
		if len(what) == 0 {
			what = []string{fmt.Sprintf("%d entries", cutAny)}
		}
		footer := fmt.Sprintf("Cut for the %d-byte budget: %s. Read the cited files with the Read tool for the rest.\n", total, strings.Join(what, ", "))
		if b.Len()+len(footer) <= total {
			b.WriteString(footer)
		}
	}
	return b.String()
}
