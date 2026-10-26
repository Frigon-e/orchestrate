package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Ranking, look-alikes and the other sections of a brief. Evidence is counted
// by kind, each once per file: own (owns or a named pointer) 2, concept hit 2,
// symbol definition, caller or implementer 2, text hit 1 and only when the name
// is rare; two or more different kinds in one file add 2. Docs never outrank
// code: they have their own short section.

var (
	acceptHeadRe = regexp.MustCompile(`(?im)^#{1,6}\s*acceptance\b.*$`)
	bulletRe     = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(.*\S)`)
	missingRe    = regexp.MustCompile(`^[A-Za-z_@][A-Za-z0-9_.:/@-]*$`)
)

// acceptanceBullets are the list items under an Acceptance heading.
func acceptanceBullets(text string) []string {
	loc := acceptHeadRe.FindStringIndex(text)
	if loc == nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(text[loc[1]:], "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			break
		}
		if m := bulletRe.FindStringSubmatch(l); m != nil && len(out) < briefAccept {
			out = append(out, flat(m[1]))
		}
	}
	return out
}

// missingWorthy keeps the names a "not found" line is useful for: identifiers,
// routes and file paths, not globs or bare test-file names.
func missingWorthy(n string) bool {
	if endpointRe.FindString(n) == n {
		return true
	}
	if !missingRe.MatchString(n) || strings.HasSuffix(n, "_test") && !strings.Contains(n, "/") && !strings.Contains(n, ".") {
		return false
	}
	return true
}

type fileEv struct {
	kinds map[string]bool
	why   []string
}

var kindOrder = []string{"own", "jb", "sym", "text"}

func (f *fileEv) score() int {
	s := 0
	for k, n := range map[string]int{"own": 2, "jb": 2, "sym": 2, "text": 1} {
		if f.kinds[k] {
			s += n
		}
	}
	if len(f.kinds) >= 2 {
		s += 2
	}
	return s
}

func (f *fileEv) kindList() string {
	var ks []string
	for _, k := range kindOrder {
		if f.kinds[k] {
			ks = append(ks, k)
		}
	}
	return strings.Join(ks, "+")
}

// symKept reports whether the package defines, extends or calls the symbol, or
// the spec's pointers or acceptance name it.
func (pl *briefPlan) symKept(name string, f *symFact) bool {
	owns := pl.in.Owns
	for _, df := range f.Defs {
		p, _ := splitWhere(df.Where)
		if inOwns(p, owns) {
			return true
		}
		for _, c := range append(append([]string{}, df.Callers...), df.Impls...) {
			cp, _ := splitWhere(c)
			if inOwns(cp, owns) {
				return true
			}
		}
	}
	for _, ptr := range pl.ptrs {
		if ptr.sym == name {
			return true
		}
	}
	for _, a := range pl.accept {
		if strings.Contains(a, name) {
			return true
		}
	}
	return false
}

func (e *ctxEnv) chunkSnippet(lines []string) []string {
	var out []string
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" && len(out) < 4 {
			out = append(out, short(keepIndent(l), 120))
		}
	}
	return out
}

func (e *ctxEnv) assemble(pl *briefPlan) *ctxBrief {
	d := &ctxBrief{ID: pl.in.ID, root: e.Code, Lookups: append([]evidenceLookup{}, pl.lookups...)}
	sort.Slice(d.Lookups, func(i, j int) bool { return d.Lookups[i].ID < d.Lookups[j].ID })
	owns := pl.in.Owns
	files := map[string]*fileEv{}
	mark := func(path, why, kind string) {
		if path == "" || e.Skip[path] || (kind != "own" && e.isGenerated(path)) {
			return
		}
		r := files[path]
		if r == nil {
			r = &fileEv{kinds: map[string]bool{}}
			files[path] = r
		}
		r.kinds[kind] = true
		if slices.Contains(r.why, why) {
			return
		}
		r.why = append(r.why, why)
	}
	for _, f := range pl.owned {
		mark(f, "owned", "own")
	}
	for _, ptr := range pl.ptrs {
		if ptr.exists {
			mark(ptr.path, "named in the text", "own")
		}
	}
	// Interface sheet: only symbols the package defines, extends or calls.
	var syms []string
	for _, s := range append(append([]string{}, pl.goSyms...), pl.tsSyms...) {
		syms = addUniq(syms, s, 100)
	}
	kept := map[string]*symFact{}
	for _, s := range syms {
		f := pl.facts[s]
		if f == nil || !pl.symKept(s, f) {
			continue
		}
		kept[s] = f
		for _, df := range f.Defs {
			path, _ := splitWhere(df.Where)
			mark(path, "defines "+s, "sym")
			for _, c := range df.Callers {
				cp, _ := splitWhere(c)
				mark(cp, "calls "+s, "sym")
			}
			for _, c := range df.Impls {
				cp, _ := splitWhere(c)
				mark(cp, "implements "+s, "sym")
			}
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "- %s (%s; %s)\n", f.Symbol, f.Lang, f.Tier)
		for _, df := range f.Defs {
			path, ln := splitWhere(df.Where)
			ls := e.Lines(path)
			if ln < 1 || ln > len(ls) {
				continue
			}
			er := lookupSource(e, path, ln, ln)
			callers := fmt.Sprint(df.Total)
			if (!df.Func && !df.ByName) || df.RefsUnknown || len(df.Errors) > 0 {
				callers = "unknown"
			}
			fmt.Fprintf(&sb, "  `%s` [%s] match=%s signature: %s; callers: %s (by_name=%t); tests: %d\n", df.Where, er.ID, df.Match, ls[ln-1], callers, df.ByName, df.Tests)
			for _, where := range append(append([]string{}, df.Callers...), df.Impls...) {
				fmt.Fprintf(&sb, "  reference: %s\n", where)
			}
		}
		d.interfaceSummary = append(d.interfaceSummary, strings.TrimRight(sb.String(), "\n"))
		var legacy strings.Builder
		e.symView(*f, symOpts{Compact: true}).write(&legacy, ctxFmt{Compact: true}, nil, 0, 0)
		d.Sheet = append(d.Sheet, strings.TrimRight(legacy.String(), "\n"))
		d.Facts = append(d.Facts, *f)
	}

	type likeItem struct {
		path, rng, in, via string
		snippet            []string
		agree              int // queries that returned this file; symbols and exact names count 2
	}
	var like []*likeEntry
	likeByPath := map[string]*likeEntry{}
	addLike := func(it likeItem) {
		en := likeByPath[it.path]
		if en == nil {
			en = &likeEntry{path: it.path}
			likeByPath[it.path] = en
			like = append(like, en)
		}
		en.ranges = addUniq(en.ranges, it.rng, 8)
		if it.in != "" {
			en.ins = addUniq(en.ins, it.in, 4)
		}
		en.vias = addUniq(en.vias, it.via, 3)
		if len(en.snippet) == 0 {
			en.snippet = it.snippet
		}
		en.agree = max(en.agree, it.agree)
	}
	ownL := map[string]bool{}
	for _, id := range ownsLangs(e.Code, owns) {
		ownL[id] = true
	}
	generic := pathGeneric(e.Code)
	relevant := pl.relevantWords(kept, generic)
	// Concept hits: evidence and look-alikes.
	asked := map[string]map[string]bool{}
	for _, q := range pl.concepts {
		for _, tree := range pl.trees {
			for _, h := range pl.concept[q+"\x00"+tree] {
				if asked[h.Path] == nil {
					asked[h.Path] = map[string]bool{}
				}
				asked[h.Path][q] = true
			}
		}
	}
	for _, q := range pl.concepts {
		for _, tree := range pl.trees {
			for _, h := range pl.concept[q+"\x00"+tree] {
				if e.Skip[h.Path] || e.isGenerated(h.Path) {
					continue
				}
				mark(h.Path, "concept search", "jb")
				if inOwns(h.Path, owns) || !lookAlikePath(h.Path, ownL) {
					continue
				}
				body := h.Body
				if len(body) == 0 {
					body = h.Snippet
				}
				shares := false
				for w := range wordsOf(strings.Join(body, " ")+" "+filepath.Base(h.Path), generic) {
					shares = shares || relevant[w]
				}
				if !shares {
					continue
				}
				in := enclosing(e.Lines(h.Path), h.Start, h.Path)
				if names := declNames(body, h.Path); in == "" && len(names) > 0 {
					in = names[0]
				}
				addLike(likeItem{path: h.Path, rng: rangeText(h.Start, h.End), in: in, via: "concept \"" + short(q, 60) + "\"", snippet: e.chunkSnippet(body), agree: len(asked[h.Path])})
			}
		}
	}
	// Symbol definitions outside the owns.
	for _, s := range syms {
		f := kept[s]
		if f == nil {
			continue
		}
		for _, df := range f.Defs {
			path, ln := splitWhere(df.Where)
			if inOwns(path, owns) || !lookAlikePath(path, ownL) || e.Skip[path] || e.isGenerated(path) {
				continue
			}
			ls := e.Lines(path)
			bodyLines, _ := bodyPreview(ls, ln, 4)
			addLike(likeItem{path: path, rng: fmt.Sprint(ln), via: "symbol `" + s + "`", snippet: e.chunkSnippet(bodyLines), agree: 2})
		}
	}
	// Text hits: evidence when the name is rare, look-alikes for exact names with few hits.
	notFound := map[string]bool{}
	for _, n := range pl.names {
		hits := pl.rg[n]
		if len(hits) > 0 && len(hits) <= briefRare {
			for _, h := range hits {
				mark(h.Path, "text `"+short(n, 40)+"`", "text")
			}
		}
		if len(hits) > 0 && len(hits) <= briefTextLike {
			for _, h := range hits {
				if inOwns(h.Path, owns) || !lookAlikePath(h.Path, ownL) || e.Skip[h.Path] || e.isGenerated(h.Path) {
					continue
				}
				ls := e.Lines(h.Path)
				var sn []string
				for i := h.Line - 1; i < len(ls) && i < h.Line+2; i++ {
					if t := strings.TrimSpace(ls[i]); t != "" {
						sn = append(sn, short(keepIndent(ls[i]), 120))
					}
				}
				addLike(likeItem{path: h.Path, rng: fmt.Sprint(h.Line), in: enclosing(ls, h.Line, h.Path), via: "text `" + short(n, 40) + "`", snippet: sn, agree: 2})
			}
		}
		_, isSym := pl.facts[n]
		if len(hits) == 0 && !isSym {
			if _, searched := pl.rg[n]; searched {
				notFound[n] = true
			}
		}
	}
	for _, f := range pl.files {
		found := false
		for _, t := range e.files() {
			if filepath.Base(t) == f {
				found = true
				mark(t, "file name `"+f+"`", "text")
			}
		}
		if !found {
			notFound[f] = true
		}
	}
	// One entry per file, a quota per code tree (the owns' trees first), the total capped.
	trees := orderTrees(pl.trees, owns)
	// Preserve backend rank for equal agreement. Plans and query iteration are
	// stable already; alphabetic ties can crowd out the most relevant hit.
	sort.SliceStable(like, func(i, j int) bool { return like[i].agree > like[j].agree })
	searched := map[string]bool{}
	for _, q := range pl.concepts {
		for _, t := range pl.trees {
			if _, ok := pl.concept[q+"\x00"+t]; ok {
				searched[t] = true
			}
		}
	}
	shown, cut := 0, 0
	for _, t := range append(append([]string{}, trees...), "\x00other") {
		n := 0
		for _, en := range like {
			tree := treeOf(pl.trees, en.path)
			if t == "\x00other" {
				if tree != "" || containsStr(trees, "") {
					continue
				}
			} else if tree != t {
				continue
			}
			if n >= briefLikeTree || shown >= briefLikeMax {
				cut++
				continue
			}
			n++
			shown++
			at := en.path + ":" + en.ranges[0]
			head := "- `" + at + "`"
			if len(en.ranges) > 1 {
				head = "- `" + en.path + "` (lines " + strings.Join(en.ranges, ", ") + ")"
			}
			if len(en.ins) > 0 {
				head += " in " + strings.Join(en.ins, ", ")
			}
			via := strings.Join(en.vias, "; ")
			head += " via " + via
			start, end := 1, 1
			fmt.Sscanf(en.ranges[0], "%d-%d", &start, &end)
			end = max(start, end)
			ls := e.Lines(en.path)
			preview := en.snippet
			legacy := head + "\n" + indent(strings.Join(en.snippet, "\n"), "    ")
			if start >= 1 && start <= len(ls) {
				end = min(len(ls), min(end, start+ctxSnippet-1))
				preview = append([]string{}, ls[start-1:end]...)
				er := lookupSource(e, en.path, start, end)
				if d.nativeLike == nil {
					d.nativeLike = map[string]briefNativeLike{}
				}
				d.nativeLike[legacy] = briefNativeLike{head + "\n" + indent(strings.Join(preview, "\n"), "    "), er}
			}
			d.Like = append(d.Like, legacy)
			d.LikeRows = append(d.LikeRows, briefLike{At: at, In: strings.Join(en.ins, ", "), Via: via, Snippet: en.snippet})
		}
		if n == 0 && t != "\x00other" && searched[t] {
			d.Like = append(d.Like, "(no similar code found in "+orDefault(t, "the repo")+")")
		}
	}
	if cut > 0 {
		d.Like = append(d.Like, fmt.Sprintf("- ... %d more cut (up to %d per tree and %d in all are kept; `orch ctx find` with -p narrows further)", cut, briefLikeTree, briefLikeMax))
	}
	for _, ptr := range pl.ptrs {
		if !ptr.exists {
			notFound[ptr.path] = true
		}
	}
	for n := range notFound {
		if missingWorthy(n) {
			d.Names = append(d.Names, n)
		}
	}
	sort.Strings(d.Names)
	for _, n := range d.Names {
		d.Missing = append(d.Missing, "- `"+n+"`")
	}

	// Rank: code in section 1, docs in their own section.
	type ranked struct {
		path  string
		score int
		ev    *fileEv
	}
	var code, docs []ranked
	for path, ev := range files {
		r := ranked{path, ev.score(), ev}
		switch {
		case isDocPath(path):
			if ev.kinds["jb"] || ev.kinds["text"] {
				docs = append(docs, r)
			}
		case ev.kinds["own"] || (isCodePath(path) && !e.isGenerated(path)):
			code = append(code, r)
		}
	}
	order := func(rs []ranked) {
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].score != rs[j].score {
				return rs[i].score > rs[j].score
			}
			return rs[i].path < rs[j].path
		})
	}
	order(code)
	order(docs)
	line := func(r ranked) string {
		return fmt.Sprintf("- `%s` (score %d: %s): %s", r.path, r.score, r.ev.kindList(), strings.Join(r.ev.why[:min(4, len(r.ev.why))], "; "))
	}
	for _, r := range code[:min(briefFiles, len(code))] {
		d.FileRows = append(d.FileRows, briefFile{r.path, r.score, r.ev.why[:min(4, len(r.ev.why))]})
		d.Files = append(d.Files, line(r))
	}
	for _, r := range docs[:min(briefDocs, len(docs))] {
		d.Docs = append(d.Docs, line(r))
	}
	return d
}

var queryStop = map[string]bool{"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "from": true, "when": true, "then": true,
	"not": true, "its": true, "are": true, "was": true, "will": true, "can": true, "one": true, "into": true, "via": true, "each": true, "any": true,
	"all": true, "per": true, "than": true, "also": true, "must": true, "should": true, "build": true, "builds": true, "add": true, "adds": true,
	"new": true, "create": true, "creates": true, "implement": true, "implements": true, "introduce": true}

// genericWords are language keywords and words too common in any code to show
// relevance. Words that are common in this repo's paths are added per repo
// (see pathGeneric).
var genericWords = map[string]bool{"test": true, "tests": true, "testdata": true, "main": true, "index": true, "type": true, "func": true, "function": true,
	"const": true, "var": true, "string": true, "error": true, "context": true, "return": true, "import": true, "export": true, "package": true,
	"the": true, "and": true, "for": true, "with": true, "nil": true, "null": true, "true": true, "false": true, "int": true, "bool": true,
	"get": true, "set": true, "new": true, "data": true, "name": true, "list": true, "from": true, "this": true, "that": true, "self": true,
	"def": true, "class": true, "pub": true, "use": true, "let": true, "void": true, "public": true, "private": true, "static": true}

var pathGenericCache sync.Map // repo -> map[string]bool

// pathGeneric are the words that appear in the directory names of at least a
// fifth of the repo's code files: layout words (src, internal, apps, ...) that
// say nothing about what a file is for. They are read from the repo, not listed.
func pathGeneric(code string) map[string]bool {
	if v, ok := pathGenericCache.Load(code); ok {
		return v.(map[string]bool)
	}
	count := map[string]int{}
	total := 0
	for _, f := range trackedFiles(code) {
		if !isCodePath(f) || isTestPath(f) {
			continue
		}
		total++
		seen := map[string]bool{}
		for _, seg := range strings.Split(filepath.ToSlash(filepath.Dir(f)), "/") {
			for w := range wordsOf(seg, nil) {
				seen[w] = true
			}
		}
		for w := range seen {
			count[w]++
		}
	}
	out := map[string]bool{}
	for w, n := range count {
		if total >= 10 && n*5 >= total {
			out[w] = true
		}
	}
	pathGenericCache.Store(code, out)
	return out
}

// wordsOf splits text into lowercase words on non-alphanumerics and camelCase,
// without the short and the generic ones.
func wordsOf(s string, extra map[string]bool) map[string]bool {
	out := map[string]bool{}
	var cur []rune
	flush := func() {
		if w := strings.ToLower(string(cur)); len(w) >= 3 && !genericWords[w] && !extra[w] {
			out[w] = true
		}
		cur = cur[:0]
	}
	rs := []rune(s)
	for i, r := range rs {
		alnum := r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !alnum {
			flush()
			continue
		}
		if n := len(cur); n > 0 {
			isDigit := func(c rune) bool { return c >= '0' && c <= '9' }
			if isDigit(r) != isDigit(cur[n-1]) {
				flush()
			}
		}
		if len(cur) > 0 && r >= 'A' && r <= 'Z' {
			prev := cur[len(cur)-1]
			if prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9' || (prev >= 'A' && prev <= 'Z' && i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z') {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// keyPhrase trims a sentence to its key nouns and verbs: at most n words,
// without stop words, in order.
func keyPhrase(s string, n int) string {
	var out []string
	seen := map[string]bool{}
	var cur []rune
	flush := func() {
		if w := strings.ToLower(string(cur)); len(w) >= 3 && !queryStop[w] && !seen[w] && len(out) < n {
			seen[w] = true
			out = append(out, w)
		}
		cur = cur[:0]
	}
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return strings.Join(out, " ")
}

const (
	queryMinWords = 4  // a concept query carries meaning or is not sent
	queryMaxWords = 30 // a sentence is cut here
)

var (
	sentenceEndRe = regexp.MustCompile(`[.!?]+(?:\s|$)`)
	commandRe     = regexp.MustCompile(`^\s*(?:[$>]\s*|` + "`" + `?(?:cd|go|npm|pnpm|yarn|make|orch|git|bash|sh|python3?|pytest|cargo|rg|fd)\s)`)
	plainWordRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z'-]*[,;:.]?$`)
	verbishRe     = regexp.MustCompile(`(?i)^(?:is|are|be|has|have|do|does|can|must|should|will|show|shows|run|runs|fail|fails|pass|passes|return|returns|send|sends|create|creates|reject|rejects|keep|keeps|print|prints|write|writes|read|reads|use|uses|build|builds|add|adds|remove|removes|record|records|match|matches)[,;:.]?$|(?i)^[a-z]{4,}(?:ed|es|s)[,;:.]?$`)
)

// sentenceQuery is the first sentence of a spec bullet, whole, cut at
// queryMaxWords. It is "" when the result has fewer than queryMinWords words.
func sentenceQuery(s string) string {
	s = flat(s)
	if loc := sentenceEndRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]+1]
	}
	w := strings.Fields(s)
	if len(w) > queryMaxWords {
		w = w[:queryMaxWords]
	}
	if len(w) < queryMinWords {
		return ""
	}
	return strings.Join(w, " ")
}

// behaviourQuery is an acceptance bullet as a query, only when it describes
// behaviour: not a command line, a file name or a flag list, and it has plain
// words with a verb in them.
func behaviourQuery(s string) string {
	if commandRe.MatchString(s) || strings.Contains(s, "&&") {
		return ""
	}
	plain, verb := 0, false
	for _, w := range strings.Fields(s) {
		if plainWordRe.MatchString(w) {
			plain++
			verb = verb || verbishRe.MatchString(w)
		}
	}
	if plain < queryMinWords || !verb {
		return ""
	}
	return sentenceQuery(s)
}

// makeQueries sets the concept queries of a brief: the first sentence of each
// of the first three "builds" lines, whole, then the acceptance bullets that
// describe behaviour; with no builds line, the package title or the first
// sentence of its body. Owned directories narrow where the queries run (the
// code trees) and are not queries. No query has fewer than queryMinWords words.
func (pl *briefPlan) makeQueries() {
	var names []string
	add := func(n string) {
		if n != "" && len(names) < 3 {
			names = addUniq(names, n, 3)
		}
	}
	all := append(append([]string{}, pl.goSyms...), pl.tsSyms...)
	for _, s := range all {
		if f := pl.facts[s]; f != nil && pl.symKept(s, f) {
			add(s)
		}
	}
	for _, s := range all {
		add(s)
	}
	for _, o := range pl.in.Owns {
		b := strings.TrimSuffix(filepath.Base(strings.TrimSuffix(o, "/")), filepath.Ext(o))
		if !strings.HasSuffix(b, "_test") && !strings.HasSuffix(o, "/") {
			add(b)
		}
	}
	pl.qnames = names
	pl.purpose = keyPhrase(pl.in.Title, 8)
	var qs []string
	push := func(q string) {
		if len(strings.Fields(q)) >= queryMinWords {
			qs = addUniq(qs, q, 40)
		}
	}
	for i, l := range pl.accept {
		if i < 3 {
			push(behaviourQuery(l))
		}
	}
	if push(sentenceQuery(pl.in.Title)); len(qs) == 0 {
		body := pl.in.Body
		if body == "" {
			body = pl.in.Text
		}
		push(sentenceQuery(body))
	}
	pl.concepts = qs
}

// relevantWords are the words a look-alike must share one of: the leaf names of
// the owns, the names the package defines and its purpose phrase.
func (pl *briefPlan) relevantWords(kept map[string]*symFact, generic map[string]bool) map[string]bool {
	w := map[string]bool{}
	merge := func(s string) {
		for k := range wordsOf(s, generic) {
			w[k] = true
		}
	}
	for _, o := range pl.in.Owns {
		o = strings.Trim(strings.TrimSuffix(strings.TrimSpace(o), filepath.Ext(o)), "/")
		parts := strings.Split(o, "/")
		merge(strings.Join(parts[max(0, len(parts)-2):], " "))
	}
	for _, n := range pl.qnames {
		merge(n)
	}
	for n := range kept {
		merge(n)
	}
	merge(pl.purpose)
	return w
}

// treeOf is the code tree a path belongs to ("" when none matches).
func treeOf(trees []string, path string) string {
	best := ""
	for _, t := range trees {
		if t != "" && strings.HasPrefix(path, t) && len(t) > len(best) {
			best = t
		}
	}
	return best
}

// orderTrees puts the trees the owns live in first.
func orderTrees(trees, owns []string) []string {
	var first, rest []string
	for _, t := range trees {
		own := false
		for _, o := range owns {
			own = own || (t != "" && strings.HasPrefix(strings.TrimPrefix(o, "./"), t))
		}
		if own {
			first = append(first, t)
		} else {
			rest = append(rest, t)
		}
	}
	return append(first, rest...)
}

// likeEntry is one file among the look-alikes: every chunk of it merged.
type likeEntry struct {
	path    string
	ranges  []string
	ins     []string
	vias    []string
	snippet []string
	agree   int
}

func containsStr(l []string, s string) bool {
	return slices.Contains(l, s)
}

func rangeText(a, b int) string {
	if b <= a {
		return fmt.Sprint(a)
	}
	return fmt.Sprintf("%d-%d", a, b)
}
