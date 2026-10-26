package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Every `orch ctx` answer is a ctxView: hits grouped by file (the path once,
// then `  <line>: <text>` rows, with the enclosing declaration on a hit), counted
// against a total, with a definite sentence when nothing was found. Hits are
// plain text because code snippets are full of the delimiters a table would
// have to quote; only short uniform tables (status, counts, the common files of
// a batch) are TOON. A batch renders its views together so a place hit twice is
// printed once and a file hit by several kinds of lookup is listed first.

const (
	ctxLineCap        = 300 // characters of a snippet line
	ctxLineCapFull    = 600
	ctxLineCapCompact = 160
	ctxHitsDefault    = 50
	ctxHitsFull       = 1000
	ctxSurround       = 6     // lines added above and below a concept hit
	ctxContextDefault = 6     // lines of context around a text hit
	ctxAnswerCap      = 12000 // characters of one lookup run alone
	ctxViewCap        = 9000  // characters of one lookup in a batch
	ctxViewCapCompact = 1500
	ctxBatchBudget    = 70000 // characters of a batch
	ctxBatchBudgetCmp = 12000
)

// ctxFmt carries the output options shared by every command.
type ctxFmt struct {
	SchemaVersion int             // 0/1: legacy JSON/prose; 2: faithful, shared selection
	Full          bool            // raise bounded caps
	Compact       bool            // the shortest answer: counts, signatures, first lines
	Batch         bool            // the lookup runs inside orch ctx batch, so hints use -q keys
	Fields        map[string]bool // src
}

var ctxFieldNames = []string{"src"}

func (f ctxFmt) width() int {
	switch {
	case f.Full:
		return ctxLineCapFull
	case f.Compact:
		return ctxLineCapCompact
	}
	return ctxLineCap
}

func (f ctxFmt) answerCap() int {
	switch {
	case f.Full:
		return ctxAnswerCap * 4
	case f.Compact:
		return ctxViewCapCompact
	}
	return ctxAnswerCap
}

// ctxLine is one line of a file: a match (or the line of a chunk, caller or
// definition) or a context line around one.
type ctxLine struct {
	ID        string `json:"id,omitempty"` // v2 evidence reference; absent in legacy JSON
	Reference string `json:"reference,omitempty"`
	N         int    `json:"n"`
	Text      string `json:"text"`
	Match     bool   `json:"match,omitempty"`
	In        string `json:"in,omitempty"` // the declaration that holds a match
}

type ctxFileHits struct {
	ID         string    `json:"id,omitempty"`         // v2 stable source reference, absent from legacy JSON
	References []string  `json:"references,omitempty"` // source lines already emitted in this selected result
	Path       string    `json:"path"`
	Lines      []ctxLine `json:"lines"`
	Notes      []string  `json:"notes,omitempty"`
}

// MarshalJSON retains exact legacy line bytes/schema, while v2 references keep
// lookup-specific match/enclosing metadata without repeating the source text.
func (l ctxLine) MarshalJSON() ([]byte, error) {
	type plain ctxLine
	if l.Reference == "" {
		return json.Marshal(plain(l))
	}
	ref := map[string]any{"reference": l.Reference, "n": l.N}
	if l.Match {
		ref["match"] = true
	}
	if l.In != "" {
		ref["in"] = l.In
	}
	return json.Marshal(ref)
}

// ctxPart is a titled section of an answer: verbatim lines and/or files.
type ctxPart struct {
	Title string        `json:"title,omitempty"`
	Raw   []string      `json:"raw,omitempty"`
	Files []ctxFileHits `json:"files,omitempty"`
	Foot  string        `json:"foot,omitempty"`
	Refs  bool          `json:"-"` // call-site rows: `N  text  in Name`, the home file without a path
	Home  string        `json:"-"`
}

type ctxView struct {
	Status        string     `json:"status,omitempty"`     // ok, empty, partial, failed
	CountKind     string     `json:"count_kind,omitempty"` // exact, lower_bound, unknown
	SearchedScope string     `json:"searched_scope,omitempty"`
	Excluded      []string   `json:"excluded,omitempty"`
	Errors        []string   `json:"errors,omitempty"`
	Kind          string     `json:"kind"`
	Query         string     `json:"query"`
	Why           string     `json:"why,omitempty"`
	Scope         string     `json:"scope,omitempty"`
	Unit          string     `json:"unit"` // hits, matches, ...
	Total         int        `json:"total"`
	Shown         int        `json:"shown"`
	Hidden        int        `json:"in_tests,omitempty"`     // hits left out by the tests filter
	HiddenG       int        `json:"in_generated,omitempty"` // hits left out because they are in generated files
	Parts         []*ctxPart `json:"parts,omitempty"`
	XPaths        []string   `json:"-"` // files this lookup touched, for cross-reference
	Zero          string     `json:"zero,omitempty"`
	Err           string     `json:"error,omitempty"`
	Cut           string     `json:"cut,omitempty"`   // what the lookup left out and the flag that shows it
	Pager         string     `json:"pager,omitempty"` // which page of a list this is and how to get the next
	Tip           string     `json:"tip,omitempty"`   // advice on how to ask better, shown at most once per batch
	TestsIn       string     `json:"-"`               // the tests mode asked for
	Head          string     `json:"head,omitempty"`
	Context       int        `json:"-"` // context lines around a hit
	Bare          bool       `json:"-"` // a lookup with one answer: no header line outside a batch
}

// finishTruth also covers views constructed by the symbol tier. Backend
// producers may set partial/lower_bound explicitly; never overwrite them.
func (v *ctxView) finishTruth() {
	if v.Err == "" && len(v.Errors) > 0 {
		v.Err = v.Errors[0]
	}
	if v.Status == "failed" && v.Err == "" {
		v.Err = "lookup failed"
	}
	if v.Status == "" {
		switch {
		case v.Err != "" && !v.hasContent():
			v.Status = "failed"
		case v.Err != "" || len(v.Errors) > 0:
			v.Status = "partial"
		case v.Total == 0 && !v.hasContent():
			v.Status = "empty"
		default:
			v.Status = "ok"
		}
	}
	if v.CountKind == "" {
		v.CountKind = "exact"
		if v.Status == "failed" || v.Status == "partial" {
			v.CountKind = "unknown"
		}
	}
	if v.Err != "" && len(v.Errors) == 0 {
		v.Errors = []string{v.Err}
	}
	if v.Status == "failed" || v.Status == "partial" {
		v.Zero = ""
	}
}

func ctxBatchStatus(views []*ctxView) string {
	failed, empty, partial := 0, 0, false
	for _, v := range views {
		v.finishTruth()
		if v.Status == "failed" {
			failed++
		}
		if v.Status == "empty" {
			empty++
		}
		partial = partial || v.Status == "partial"
	}
	if failed == len(views) && failed > 0 {
		return "failed"
	}
	if failed > 0 || partial {
		return "partial"
	}
	if empty == len(views) {
		return "empty"
	}
	return "ok"
}

func (v *ctxView) part(i int) *ctxPart {
	for len(v.Parts) <= i {
		v.Parts = append(v.Parts, &ctxPart{})
	}
	return v.Parts[i]
}

func (v *ctxView) hasContent() bool {
	for _, p := range v.Parts {
		if len(p.Raw) > 0 || len(p.Files) > 0 {
			return true
		}
	}
	return false
}

func (v *ctxView) fileCount() int {
	seen := map[string]bool{}
	for _, p := range v.Parts {
		for _, f := range p.Files {
			seen[f.Path] = true
		}
	}
	return len(seen)
}

func (p *ctxPart) file(path string) *ctxFileHits {
	for i := range p.Files {
		if p.Files[i].Path == path {
			return &p.Files[i]
		}
	}
	p.Files = append(p.Files, ctxFileHits{Path: path})
	return &p.Files[len(p.Files)-1]
}

func (p *ctxPart) add(path string, l ctxLine) {
	f := p.file(path)
	f.Lines = append(f.Lines, l)
}

// normalize sorts each file's lines and merges the ones that overlap: a place
// reached twice (two chunks, a match inside another's context) is one line.
func (p *ctxPart) normalize() {
	for i := range p.Files {
		ls := p.Files[i].Lines
		sort.SliceStable(ls, func(a, b int) bool { return ls[a].N < ls[b].N })
		var out []ctxLine
		for _, l := range ls {
			if n := len(out); n > 0 && out[n-1].N == l.N {
				if l.Match && !out[n-1].Match {
					out[n-1] = l
				} else if out[n-1].In == "" {
					out[n-1].In = l.In
				}
				continue
			}
			out = append(out, l)
		}
		p.Files[i].Lines = out
	}
}

// blocks counts the runs of consecutive lines that hold a match: the hits left
// once overlapping and adjacent ranges are merged.
func (p *ctxPart) blocks() int {
	n := 0
	for _, f := range p.Files {
		open := false
		last := -2
		for _, l := range f.Lines {
			if l.N > last+1 {
				open = false
			}
			if l.Match && !open {
				n++
				open = true
			}
			last = l.N
		}
	}
	return n
}

// compactLines renders sorted line numbers as ranges: 58-66,70.
func compactLines(ns []int) string {
	sort.Ints(ns)
	var parts []string
	for i := 0; i < len(ns); {
		j := i
		for j+1 < len(ns) && ns[j+1] <= ns[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", ns[i], ns[j]))
		} else {
			parts = append(parts, strconv.Itoa(ns[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// passTests applies the tests mode (none, include or only) to a path.
func passTests(mode, path string) bool {
	switch mode {
	case "include":
		return true
	case "only":
		return isTestPath(path)
	}
	return !isTestPath(path)
}

func validTests(mode string) bool {
	return mode == "" || mode == "none" || mode == "include" || mode == "only"
}

// lineAt is line n of ls without its indentation, "" past the ends.
func lineAt(ls []string, n int, version ...int) string {
	if n < 1 || n > len(ls) {
		return ""
	}
	if len(version) > 0 && version[0] == 2 {
		return ls[n-1]
	}
	return strings.TrimSpace(ls[n-1])
}

func sourceExcerpt(s string, f ctxFmt) string {
	if f.SchemaVersion == 2 {
		return s
	}
	return short(keepIndent(s), f.width())
}

// addAround adds line n of a file as a match, with up to ctx non-blank lines
// on each side as context.
func addAround(p *ctxPart, path string, ls []string, n int, text, in string, ctx, width int, version ...int) {
	faithful := len(version) > 0 && version[0] == 2
	clip := func(s string) string {
		if faithful {
			return s
		}
		return short(s, width)
	}
	p.add(path, ctxLine{N: n, Text: clip(text), Match: true, In: in})
	for d := -ctx; d <= ctx; d++ {
		if d == 0 || n+d < 1 || n+d > len(ls) {
			continue
		}
		if t := lineAt(ls, n+d, version...); t != "" || faithful {
			p.add(path, ctxLine{N: n + d, Text: clip(t)})
		}
	}
}

// conceptView builds the view of a concept search: each chunk in full (the
// whole function when it is one), merged per file, with the declarations in
// it and the size of the package around it.
func (e *ctxEnv) conceptView(query, path, tests string, hits []codeHit, f ctxFmt) *ctxView {
	return e.conceptViewG(query, path, tests, "", hits, f)
}

// conceptViewG is conceptView with a generated-file mode (none, include or only).
func (e *ctxEnv) conceptViewG(query, path, tests, gen string, hits []codeHit, f ctxFmt) *ctxView {
	v := &ctxView{Kind: "find", Query: query, Unit: "hits", TestsIn: tests}
	if path != "" {
		v.Scope = "path " + path
	}
	p := v.part(0)
	for _, h := range hits {
		if !passTests(tests, h.Path) {
			v.Hidden++
			continue
		}
		if !e.passGen(gen, h.Path) {
			v.HiddenG++
			continue
		}
		// Semantic content is a location hint, never source to splice into the
		// checkout. Resolve even direct callers of conceptViewG here.
		current, ok := e.checkoutHit(h)
		if !ok {
			v.Errors = append(v.Errors, fmt.Sprintf("cannot validate semantic range %s:%d-%d; use ctx text or refresh the index", h.Path, h.Start, h.End))
			v.Status, v.CountKind = "partial", "unknown"
			continue
		}
		h = current
		body := h.Body
		if f.Compact {
			body = h.Snippet
		}
		if f.Compact && len(body) > 3 {
			body = body[:3]
		}
		if left := h.End - h.Start + 1 - len(body); left > 0 {
			p.file(h.Path).Notes = append(p.file(h.Path).Notes, fmt.Sprintf("%d lines of this range not shown; concept excerpts are capped at %d lines (3 with --compact); use a native read for the checkout range", left, ctxBodyLines))
		}
		before, after := e.surround(h, len(body), f)
		for i, l := range before {
			p.add(h.Path, ctxLine{N: h.Start - len(before) + i, Text: sourceExcerpt(l, f)})
		}
		for i, l := range body {
			p.add(h.Path, ctxLine{N: h.Start + i, Text: sourceExcerpt(l, f), Match: true})
		}
		for i, l := range after {
			p.add(h.Path, ctxLine{N: h.Start + len(body) + i, Text: sourceExcerpt(l, f)})
		}
	}
	p.normalize()
	for i := range p.Files {
		fh := &p.Files[i]
		v.XPaths = append(v.XPaths, fh.Path)
		if f.Compact {
			continue
		}
		var chunk []string
		for _, l := range fh.Lines {
			chunk = append(chunk, l.Text)
		}
		if names := declNames(chunk, fh.Path); len(names) > 0 {
			fh.Notes = append(fh.Notes, "declares "+strings.Join(names[:min(6, len(names))], ", "))
		}
		if pk := e.pkgExports(fh.Path); pk != "" {
			fh.Notes = append(fh.Notes, pk)
		}
	}
	v.Total = p.blocks()
	v.Shown = v.Total
	v.CountKind = "unknown" // ranked semantic candidates do not prove absence
	return v
}

// textView builds the view of an rg search; max caps the matches shown, ctx
// is the lines of context around each.
func (e *ctxEnv) textView(q rgQuery, tests string, res rgResult, max, page, ctx int, f ctxFmt) *ctxView {
	v := &ctxView{Kind: "text", Query: q.Pattern, Unit: "matches", TestsIn: tests, Context: ctx, CountKind: "exact"}
	if res.Filtered {
		v.Hidden, v.HiddenG = res.Hidden, res.HiddenG
	}
	if res.Limited {
		v.Status = "partial"
		v.Cut = fmt.Sprintf("retained excerpt cap %d; counting completed separately; narrow with --glob/-p (glob=/path= in batch); --full remains bounded", ctxRgMax)
	}
	if q.Path != "" {
		v.Scope = "path " + q.Path
	}
	if max <= 0 {
		max = ctxTextPage
		if f.Full {
			max = ctxHitsFull
		}
	}
	if page < 1 {
		page = 1
	}
	// pager sets the footer, or the zero line when the page is past the end.
	pager := func(rows, lo, hi int, past bool, unit string) {
		if past {
			v.Zero = pastEnd(rows, max, page, unit)
			return
		}
		v.Pager = pageFooter(lo, hi, rows, page, false, f.Batch, narrowText)
	}
	switch {
	case q.FilesOnly:
		v.Unit = "files"
		var kept []string
		for _, p := range res.Files {
			if !passTests(tests, p) {
				v.Hidden++
				continue
			}
			if !e.passGen(q.Generated, p) {
				v.HiddenG++
				continue
			}
			kept = append(kept, p)
		}
		v.Total = len(kept)
		if res.Filtered {
			v.Total = res.Total
		}
		lo, hi, past := pageRange(len(kept), max, page)
		var list []string
		for _, p := range kept[lo:hi] {
			list = append(list, "  "+p)
			v.XPaths = append(v.XPaths, p)
		}
		v.Shown = len(list)
		if len(list) > 0 {
			v.part(0).Raw = append([]string{fmt.Sprintf("files[%d]:", len(list))}, list...)
		}
		pager(v.Total, lo, hi, past, "files")
		return v
	case q.Count:
		var kept []rgCount
		for _, c := range res.Counts {
			if !passTests(tests, c.Path) {
				v.Hidden += c.Count
				continue
			}
			if !e.passGen(q.Generated, c.Path) {
				v.HiddenG += c.Count
				continue
			}
			v.Total += c.Count
			kept = append(kept, c)
		}
		if res.Filtered {
			v.Total = res.Total
		}
		lo, hi, past := pageRange(len(kept), max, page)
		var rows []string
		for _, c := range kept[lo:hi] {
			v.Shown += c.Count
			rows = append(rows, fmt.Sprintf("  %s,%d", agentScalar(c.Path), c.Count))
			v.XPaths = append(v.XPaths, c.Path)
		}
		if len(rows) > 0 {
			v.part(0).Raw = append([]string{fmt.Sprintf("counts[%d]{path,count}:", len(rows))}, rows...)
		}
		rowsTotal := len(kept)
		if res.Filtered && res.Rows > 0 {
			rowsTotal = res.Rows
		}
		pager(rowsTotal, lo, hi, past, "files")
		return v
	}
	p := v.part(0)
	if q.OnlyMatching || q.Multiline {
		ctx = 0
	}
	var kept []rgHit
	for _, h := range res.Hits {
		if !passTests(tests, h.Path) {
			v.Hidden++
			continue
		}
		if !e.passGen(q.Generated, h.Path) {
			v.HiddenG++
			continue
		}
		kept = append(kept, h)
	}
	v.Total = len(kept)
	if res.Filtered {
		v.Total = res.Total
	}
	lo, hi, past := pageRange(len(kept), max, page)
	for _, h := range kept[lo:hi] {
		ls := e.Lines(h.Path)
		text := h.Text
		if f.SchemaVersion == 2 && !q.OnlyMatching && h.Line > 0 && h.Line <= len(ls) {
			text = lineAt(ls, h.Line, 2)
		}
		addAround(p, h.Path, ls, h.Line, text, enclosing(ls, h.Line, h.Path), ctx, f.width(), f.SchemaVersion)
	}
	v.Shown = hi - lo
	p.normalize()
	for _, fh := range p.Files {
		v.XPaths = append(v.XPaths, fh.Path)
	}
	pager(v.Total, lo, hi, past, "hits")
	if v.Pager != "" && hi < len(kept) {
		var left []string
		for _, h := range kept[hi:] {
			left = append(left, h.Path)
		}
		v.Pager += restNote(left)
	}
	return v
}

// readView builds the view of a read lookup: the lines of one file.
func readView(path string, from int, lines []string, width int) *ctxView {
	v := &ctxView{Kind: "read", Query: path, Unit: "lines", Scope: fmt.Sprintf("%d-%d", from, from+len(lines)-1), XPaths: []string{path}}
	for i, l := range lines {
		v.part(0).add(path, ctxLine{N: from + i, Text: short(keepIndent(l), width), Match: true})
	}
	v.Total, v.Shown = len(lines), len(lines)
	return v
}

// ---- rendering ----

// ctxSeen remembers the lines a batch printed, per file, with the number of
// the lookup that printed them.
type ctxSeen map[string]map[int]int

func (s ctxSeen) hit(path string, n int) int { return s[path][n] }

func (s ctxSeen) add(path string, n, num int) {
	if s[path] == nil {
		s[path] = map[int]int{}
	}
	if s[path][n] == 0 {
		s[path][n] = num
	}
}

func (v *ctxView) scope() string {
	if v.Scope != "" && v.Kind != "read" {
		return "(" + v.Scope + ")"
	}
	return ""
}

// header is the one line that counts the result.
func (v *ctxView) header(num int) string {
	var b strings.Builder
	label := ""
	if v.Why != "" {
		label = " [" + v.Why + "]"
	}
	prefix := v.Kind + ": "
	if v.Bare && v.Err == "" && v.Zero == "" && v.Status != "partial" {
		if num > 0 {
			return fmt.Sprintf("#%d %s %s%s", num, v.Kind, quoteQuery(v.Query), label)
		}
		return ""
	}
	if num > 0 {
		prefix = fmt.Sprintf("#%d %s %s%s: ", num, v.Kind, quoteQuery(v.Query), label)
	}
	b.WriteString(prefix)
	switch {
	case v.Err != "" && v.Status != "partial":
		return b.String() + "error: " + v.Err
	case v.Zero != "":
		return b.String() + v.Zero
	case v.Head != "":
		return b.String() + v.Head
	}
	files := v.fileCount()
	fmt.Fprintf(&b, "%d of %d %s", v.Shown, v.Total, v.Unit)
	if v.Kind != "read" {
		if num == 0 {
			fmt.Fprintf(&b, " for %s", quoteQuery(v.Query))
		}
		if files > 0 {
			fmt.Fprintf(&b, " in %d file%s", files, plural(files))
		}
	}
	if v.CountKind == "lower_bound" {
		b.WriteString(" (count is a lower bound)")
	}
	if v.Status == "partial" {
		b.WriteString(" (partial)")
	}
	if s := v.scope(); s != "" {
		b.WriteString(" ")
		b.WriteString(s)
	}
	if v.Hidden > 0 && v.TestsIn != "include" {
		fmt.Fprintf(&b, " (+%d in tests; tests=include|only)", v.Hidden)
	}
	if v.HiddenG > 0 {
		fmt.Fprintf(&b, " (+%d in generated; --generated include|only)", v.HiddenG)
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func quoteQuery(q string) string { return `"` + strings.ReplaceAll(short(flat(q), 80), `"`, `'`) + `"` }

// setZero states an empty result definitely.
func (v *ctxView) setZero(wholeRepo string) {
	if v.Total > 0 || v.Err != "" || v.Zero != "" || v.hasContent() || v.Status == "partial" || v.Status == "failed" {
		return
	}
	where := wholeRepo
	if v.Scope != "" {
		where = wholeRepo + " (" + v.Scope + ")"
	}
	switch {
	case v.Hidden > 0 && v.TestsIn != "only":
		v.Zero = fmt.Sprintf("0 %s outside tests in %s (+%d in tests; tests=include|only)", v.Unit, where, v.Hidden)
	default:
		v.Zero = fmt.Sprintf("0 %s found in %s", v.Unit, where)
	}
	if v.HiddenG > 0 {
		v.Zero += fmt.Sprintf(" (+%d in generated; --generated include|only)", v.HiddenG)
	}
}

// sameLines finds the match lines that read the same in several files, so the
// text is printed once. It returns the lines to leave out and the summary rows.
func (v *ctxView) sameLines() (drop map[string]bool, rows []string) {
	type loc struct {
		path string
		n    int
	}
	by := map[string][]loc{}
	var order []string
	for _, p := range v.Parts {
		for _, fh := range p.Files {
			for _, l := range fh.Lines {
				if !l.Match || len(l.Text) < 8 {
					continue
				}
				if _, ok := by[l.Text]; !ok {
					order = append(order, l.Text)
				}
				by[l.Text] = append(by[l.Text], loc{fh.Path, l.N})
			}
		}
	}
	drop = map[string]bool{}
	for _, t := range order {
		locs := by[t]
		files := map[string]bool{}
		for _, l := range locs {
			files[l.path] = true
		}
		if len(files) < 2 {
			continue
		}
		var first []string
		for _, l := range locs[:min(3, len(locs))] {
			first = append(first, fmt.Sprintf("%s:%d", l.path, l.n))
		}
		more := ""
		if len(locs) > 3 {
			more = fmt.Sprintf(", +%d more", len(locs)-3)
		}
		rows = append(rows, fmt.Sprintf("same line in %d files: %s (%s%s)", len(files), t, strings.Join(first, ", "), more))
		for _, l := range locs {
			drop[fmt.Sprintf("%s:%d", l.path, l.n)] = true
		}
	}
	return drop, rows
}

// write renders the view. num is its number in a batch (0 for a single
// lookup); seen collapses places an earlier lookup already printed; limit (0:
// none) stops the output near that many characters and says what was left out.
func (v *ctxView) write(b *strings.Builder, f ctxFmt, seen ctxSeen, num, limit int) {
	v.finishTruth()
	start := b.Len()
	if h := v.header(num); h != "" {
		b.WriteString(h)
		b.WriteString("\n")
	}
	drop, sameRows := v.sameLines()
	total, printed, skipped := 0, 0, 0
	for _, p := range v.Parts {
		for _, fh := range p.Files {
			total += len(fh.Lines)
		}
	}
	over := func() bool { return limit > 0 && b.Len()-start > limit }
	filesLeft := 0
parts:
	for pi, p := range v.Parts {
		if p.Title != "" {
			b.WriteString(p.Title)
			b.WriteString("\n")
		}
		for _, r := range p.Raw {
			b.WriteString(r)
			b.WriteString("\n")
		}
		for fi, fh := range p.Files {
			if over() {
				filesLeft += len(p.Files) - fi
				for _, q := range v.Parts[pi+1:] {
					filesLeft += len(q.Files)
				}
				break parts
			}
			var keep []ctxLine
			also := map[int][]int{}
			dropped := map[int]bool{}
			for _, l := range fh.Lines {
				if l.Match && drop[fmt.Sprintf("%s:%d", fh.Path, l.N)] {
					dropped[l.N] = true
				}
			}
			for _, l := range fh.Lines {
				if dropped[l.N] {
					skipped++
					continue
				}
				if !l.Match && len(dropped) > 0 && nearDropped(fh.Lines, dropped, l.N, v.Context+1) {
					skipped++
					continue
				}
				if seen != nil {
					if n := seen.hit(fh.Path, l.N); n != 0 && n != num {
						if l.Match {
							also[n] = append(also[n], l.N)
						}
						skipped++
						continue
					}
				}
				keep = append(keep, l)
			}
			if len(keep) > 0 {
				head := fh.Path
				if f.Fields["src"] {
					head += " (" + map[string]string{"find": "jb", "text": "rg", "sym": "sym", "read": "read"}[v.Kind] + ")"
				}
				ind := "  "
				switch {
				case p.Refs && fh.Path == p.Home:
				case p.Refs:
					b.WriteString("  ")
					b.WriteString(head)
					b.WriteString("\n")
					ind = "    "
				default:
					b.WriteString(head)
					b.WriteString("\n")
				}
				lastIn := ""
				for _, l := range keep {
					if over() {
						break
					}
					var line string
					switch {
					case p.Refs && l.Match:
						line = fmt.Sprintf("%s%d  %s", ind, l.N, l.Text)
						if l.In != "" {
							line += "  in " + inName(l.In)
						}
					case p.Refs:
						line = fmt.Sprintf("%s%d- %s", ind, l.N, l.Text)
					default:
						sep := "-"
						if l.Match {
							sep = ":"
						}
						line = fmt.Sprintf("  %d%s %s", l.N, sep, l.Text)
						if l.In != "" && l.In != lastIn {
							line += "  [in " + l.In + "]"
							lastIn = l.In
						}
					}
					b.WriteString(line)
					b.WriteString("\n")
					printed++
					if seen != nil {
						seen.add(fh.Path, l.N, num)
					}
				}
				for _, n := range fh.Notes {
					b.WriteString("  ")
					b.WriteString(n)
					b.WriteString("\n")
				}
			}
			var ns []int
			for n := range also {
				ns = append(ns, n)
			}
			sort.Ints(ns)
			for _, n := range ns {
				fmt.Fprintf(b, "%s also #%d: %s\n", fh.Path, n, compactLines(also[n]))
			}
		}
		if p.Foot != "" {
			b.WriteString(p.Foot)
			b.WriteString("\n")
		}
	}
	for _, r := range sameRows {
		b.WriteString(r)
		b.WriteString("\n")
	}
	if left := total - printed - skipped; left > 0 {
		fmt.Fprintf(b, "cut at about %d characters: %d more lines (%d more files) not shown; --full raises the bounded cap, or narrow the lookup\n", limit, left, filesLeft)
	}
	for _, err := range v.Errors {
		if v.Status == "partial" {
			b.WriteString("error: ")
			b.WriteString(err)
			b.WriteString("\n")
		}
	}
	if v.Kind == "text" && v.Total == 0 && v.Err == "" {
		if v.SearchedScope != "" {
			b.WriteString("searched_scope: ")
			b.WriteString(v.SearchedScope)
			b.WriteString("\n")
		}
		if len(v.Excluded) > 0 {
			b.WriteString("excluded: ")
			b.WriteString(strings.Join(v.Excluded, "; "))
			b.WriteString("\n")
		}
	}
	if v.Cut != "" {
		b.WriteString("cut: ")
		b.WriteString(v.Cut)
		b.WriteString("\n")
	}
	if v.Pager != "" {
		b.WriteString(v.Pager)
		b.WriteString("\n")
	}
	if v.Tip != "" {
		b.WriteString(v.Tip)
		b.WriteString("\n")
	}
}

// nearDropped reports whether line n is a context line of a dropped match.
func nearDropped(lines []ctxLine, dropped map[int]bool, n, within int) bool {
	near := false
	for _, l := range lines {
		if !l.Match || l.N < n-within || l.N > n+within {
			continue
		}
		if !dropped[l.N] {
			return false
		}
		near = true
	}
	return near
}

// commonFiles lists the files hit by more than one kind of lookup, most kinds
// first, with the kinds that found them and the lookups' numbers.
func commonFiles(views []*ctxView) []string {
	type info struct {
		kinds map[string]bool
		nums  map[int]bool
		hits  int
	}
	files := map[string]*info{}
	for i, v := range views {
		src := map[string]string{"find": "jb", "text": "rg", "sym": "sym", "read": "read"}[v.Kind]
		for _, p := range v.XPaths {
			in := files[p]
			if in == nil {
				in = &info{kinds: map[string]bool{}, nums: map[int]bool{}}
				files[p] = in
			}
			in.kinds[src] = true
			in.nums[i+1] = true
			in.hits++
		}
	}
	type row struct {
		path string
		in   *info
	}
	var rows []row
	for p, in := range files {
		if len(in.kinds) >= 2 {
			rows = append(rows, row{p, in})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, c := rows[i], rows[j]
		if len(a.in.kinds) != len(c.in.kinds) {
			return len(a.in.kinds) > len(c.in.kinds)
		}
		if a.in.hits != c.in.hits {
			return a.in.hits > c.in.hits
		}
		return a.path < c.path
	})
	var out []string
	for _, r := range rows[:min(10, len(rows))] {
		var ks []string
		for k := range r.in.kinds {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		var ns []int
		for n := range r.in.nums {
			ns = append(ns, n)
		}
		out = append(out, fmt.Sprintf("  %s,%s,%s", agentScalar(r.path), strings.Join(ks, "+"), agentScalar(compactLines(ns))))
	}
	return out
}

// renderBatch writes the views of a batch: the common files first, then each
// lookup in plan order. Each answer is held to ctxViewCap characters and the
// batch to ctxBatchBudget, and what was left out is said, with the flag that
// shows it.
func renderBatch(views []*ctxView, f ctxFmt) string {
	var b strings.Builder
	status := ctxBatchStatus(views)
	empty, failed := 0, 0
	for _, v := range views {
		switch v.Status {
		case "failed":
			failed++
		case "empty":
			empty++
		}
	}
	fmt.Fprintf(&b, "batch: %d lookups, %d with results, %d empty, %d failed\n", len(views), len(views)-empty-failed, empty, failed)
	fmt.Fprintf(&b, "status: %s\n", status)
	if rows := commonFiles(views); len(rows) > 0 {
		fmt.Fprintf(&b, "common[%d]{path,sources,lookups}:\n%s\n", len(rows), strings.Join(rows, "\n"))
	}
	viewCap, budget := ctxViewCap, ctxBatchBudget
	if f.Compact {
		viewCap, budget = ctxViewCapCompact, ctxBatchBudgetCmp
	}
	if f.Full {
		viewCap, budget = ctxAnswerCap*4, ctxBatchBudget*4
	}
	seen := ctxSeen{}
	tipShown := false
	for i, v := range views {
		if v.Tip != "" {
			if tipShown {
				v.Tip = ""
			}
			tipShown = true
		}
		limit := viewCap
		if budget > 0 {
			left := budget - b.Len()
			if left < 300 {
				fmt.Fprintf(&b, "#%d %s %s: not shown, the batch budget of %d characters is spent; run it alone or use --full\n", i+1, v.Kind, quoteQuery(v.Query), budget)
				continue
			}
			if limit == 0 || left < limit {
				limit = left
			}
		}
		v.write(&b, f, seen, i+1, limit)
	}
	return b.String()
}

// surround is the checkout's lines just above and below a concept hit's shown
// chunk, ctxSurround of each, so a hit reads as code in its place. A
// compact answer, a hit whose file cannot be read and a stale hit get none.
func (e *ctxEnv) surround(h codeHit, shown int, f ctxFmt) (before, after []string) {
	n := ctxSurround
	if f.Compact || shown == 0 {
		return nil, nil
	}
	ls := e.Lines(h.Path)
	if h.Start < 1 || h.Start+shown-1 > len(ls) {
		return nil, nil
	}
	lo := max(h.Start-n, 1)
	hi := min(h.Start+shown-1+n, len(ls))
	return ls[lo-1 : h.Start-1], ls[h.Start+shown-1 : hi]
}
