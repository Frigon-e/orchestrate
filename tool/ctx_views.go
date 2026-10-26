package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ---- symbols ----

type symOpts struct {
	SchemaVersion int    // v2 source bytes stay native, legacy JSON remains unchanged
	Refs          int    // non-test callers listed (each with its line, enclosing function and the line before)
	Impl          bool   // implementers listed
	Tests         string // none (default), include or only: how test references are shown
	Kind          string // only definitions of this kind
	In            string // only definitions under this path
	Compact       bool   // signature, first doc line and counts only
	Full          bool   // whole bodies and every caller kept
	Context       int    // lines above each caller
	Generated     string // generated callers: none (hidden, counted), include or only
	Limit         int    // callers and implementers per page; 0: Refs for callers, 10 for implementers
	Page          int    // 1-based page of both lists
	Batch         bool   // inside orch ctx batch: hints use -q keys
}

const (
	symRefsDefault   = 5
	ctxFindLookahead = 8
	symBodyLines     = 15
	symTypeLines     = 40
)

func defaultSymOpts(f ctxFmt) symOpts {
	if f.Compact {
		return symOpts{Compact: true, SchemaVersion: f.SchemaVersion}
	}
	o := symOpts{Refs: symRefsDefault, Impl: true, Full: f.Full, Batch: f.Batch, SchemaVersion: f.SchemaVersion}
	if f.Full {
		o.Refs = ctxMaxRefs
	}
	return o
}

// refLines adds the place loc ("path:line") to a part as a match with its
// enclosing declaration and up to before lines above it.
func (e *ctxEnv) refLines(p *ctxPart, loc string, before int, width int) (path string) {
	path, ln := splitWhere(loc)
	ls := e.Lines(path)
	if ln < 1 || ln > len(ls) {
		return path
	}
	for d := before; d >= 1; d-- {
		if t := lineAt(ls, ln-d); t != "" {
			p.add(path, ctxLine{N: ln - d, Text: short(t, width)})
		}
	}
	p.add(path, ctxLine{N: ln, Text: short(lineAt(ls, ln), width), Match: true, In: enclosing(ls, ln, path)})
	return path
}

func distinctFiles(locs []string) int {
	seen := map[string]bool{}
	for _, l := range locs {
		p, _ := splitWhere(l)
		seen[p] = true
	}
	return len(seen)
}

func isTypeKind(k string) bool {
	switch k {
	case "Struct", "Interface", "Class", "Enum", "Namespace", "Module", "TypeParameter":
		return true
	}
	return false
}

// wired follows the first caller of a definition one level up by name: the
// function it sits in, and where that function is referenced. It is an rg word
// search, not a call graph, and says so.
func (e *ctxEnv) wired(name string, d symDef) string {
	if len(d.Callers) == 0 {
		return ""
	}
	cp, cl := splitWhere(d.Callers[0])
	in := enclosing(e.Lines(cp), cl, cp)
	_, fn, ok := strings.Cut(in, " ")
	if !ok {
		return ""
	}
	fn = fn[strings.LastIndex(fn, ".")+1:]
	if fn == "" || fn == name {
		return ""
	}
	res, err := e.Text(rgQuery{Pattern: fn, Fixed: true, Word: true, CaseSensitive: true})
	if err != nil {
		return ""
	}
	var sites []string
	for _, h := range res.Hits {
		ls := e.Lines(h.Path)
		if isTestPath(h.Path) || !isCodePath(h.Path) {
			continue
		}
		if k, _ := declOf(lineAt(ls, h.Line), filepath.Ext(h.Path)); k != "" && indentOf(ls[h.Line-1]) == 0 {
			continue
		}
		site := fmt.Sprintf("%s:%d", h.Path, h.Line)
		if h.Path == cp {
			site = fmt.Sprintf("line %d", h.Line)
		}
		if enc := enclosing(ls, h.Line, h.Path); enc != "" {
			site += " in " + enc
		}
		sites = append(sites, site)
	}
	at := d.Callers[0]
	if dp, _ := splitWhere(d.Where); dp == cp {
		at = fmt.Sprintf("line %d", cl)
	}
	line := fmt.Sprintf("  wired: %s is called in %s (%s); by name (rg), %s is referenced ", name, in, at, fn)
	switch {
	case len(sites) == 0:
		return line + "nowhere else outside tests"
	case len(sites) > 6:
		return "" // a name that common says nothing about how this one is wired
	}
	return line + "at " + strings.Join(sites, "; ")
}

// genFilter leaves generated files out of a definition's callers unless the
// mode asks for them; the callers it hides are counted.
func (e *ctxEnv) genFilter(d symDef, mode string) symDef {
	var keep []string
	hidden := 0
	for _, c := range d.Callers {
		if p, _ := splitWhere(c); e.passGen(mode, p) {
			keep = append(keep, c)
		} else {
			hidden++
		}
	}
	if hidden == 0 {
		return d
	}
	d.Callers = keep
	d.Total = max(d.Total-hidden, len(keep))
	if mode == "" || mode == "none" {
		d.GenHidden = hidden
	}
	return d
}

// inName is the name of an enclosing declaration without its kind word.
func inName(in string) string {
	if _, n, ok := strings.Cut(in, " "); ok {
		return n
	}
	return in
}

// usedBy states a definition's use once: callers outside tests, the files they
// are in and the test references.
func usedBy(d symDef, _ int, tests string) string {
	if d.ByName {
		s := fmt.Sprintf("at least %d non-test name matches (by name, not a call graph)", d.Total)
		if d.GenHidden > 0 {
			s += fmt.Sprintf(", +%d in generated", d.GenHidden)
		}
		if d.RefsUnknown {
			return s + "; references unavailable (counts unknown)"
		}
		return s + "; test reference count unknown"
	}
	if d.RefsUnknown {
		return "references unavailable (counts unknown)"
	}
	s := fmt.Sprintf("used by %d", d.Total)
	if n := distinctFiles(d.Callers); n > 1 {
		s += fmt.Sprintf(" in %d%s files", n, map[bool]string{true: "+", false: ""}[d.Total > len(d.Callers)])
	}
	switch {
	case tests == "only":
		return fmt.Sprintf("in %d tests (used by %d outside tests)", d.Tests, d.Total)
	case d.Tests == 0:
		s += ", none in tests"
	default:
		s += fmt.Sprintf(", +%d in tests", d.Tests)
	}
	if d.GenHidden > 0 {
		s += fmt.Sprintf(", +%d in generated", d.GenHidden)
	}
	return s
}

// refPart lists places ("path:line") as call-site rows: the home file's rows
// first without a path, other files under their path once.
func (e *ctxEnv) refPart(v *ctxView, title, home string, locs []string, before, width int) *ctxPart {
	p := &ctxPart{Title: title, Refs: true, Home: home}
	p.file(home)
	for _, loc := range locs {
		v.XPaths = append(v.XPaths, e.refLines(p, loc, before, width))
	}
	p.normalize()
	if f := p.file(home); len(f.Lines) == 0 { // no rows in the home file
		p.Files = p.Files[1:]
	}
	if len(p.Files) == 0 { // nothing to list under the title
		p.Title = strings.TrimSuffix(p.Title, ":")
	}
	return p
}

func testsTitle(d symDef, o symOpts) string {
	if o.Tests == "only" {
		return usedBy(d, 0, "only") + ":"
	}
	return "tests:"
}

// symView builds the view of a symbol: per definition a header (name, kind,
// place), the doc comment and a body preview sized by kind, one "used by" line
// with the callers outside tests, the implementers and where it is wired.
func (e *ctxEnv) symView(f symFact, o symOpts) *ctxView {
	v := &ctxView{Kind: "sym", Query: f.Symbol, Unit: "definitions", TestsIn: o.Tests, Total: len(f.Defs), Errors: append([]string(nil), f.Errors...), SearchedScope: orDefault(o.In, "repository declarations")}
	defer func() {
		v.finishTruth()
		if v.Head != "" && v.Status == "partial" {
			v.Head += " (partial; counts not established)"
		}
	}()
	if len(f.Defs) == 0 {
		v.Zero = fmt.Sprintf("no symbol named %s found (declarations searched in every language of the repo; `orch ctx text` finds a name that is not a declaration)", quoteQuery(f.Symbol))
		return v
	}
	var defs []symDef
	for _, d := range f.Defs {
		path, _ := splitWhere(d.Where)
		if o.Kind != "" && !strings.EqualFold(d.Kind, o.Kind) {
			continue
		}
		if o.In != "" && !symPathWithin(path, o.In) {
			continue
		}
		v.Errors = append(v.Errors, d.Errors...)
		if d.Match == "candidate" || d.ByName || d.RefsUnknown {
			v.Status = "partial"
			v.CountKind = "unknown"
		}
		defs = append(defs, d)
	}
	v.Total = len(defs)
	v.Shown = min(len(defs), goplsMaxDefs)
	if len(defs) > goplsMaxDefs {
		v.Cut = fmt.Sprintf("showing %d of %d scoped definitions; narrow with --in and --kind", goplsMaxDefs, len(defs))
		defs = defs[:goplsMaxDefs]
	}
	if len(defs) == 0 {
		v.Zero = fmt.Sprintf("0 of %d definitions match kind=%q in=%q", len(f.Defs), o.Kind, o.In)
		return v
	}
	if len(defs) == 1 {
		v.Bare = true
	} else {
		v.Head = fmt.Sprintf("%d definitions of %s", len(defs), quoteQuery(f.Symbol))
		if len(defs) != len(f.Defs) {
			v.Head = fmt.Sprintf("%d of %d definitions of %s", len(defs), len(f.Defs), quoteQuery(f.Symbol))
		}
	}
	width := ctxLineCap
	if o.Compact {
		width = ctxLineCapCompact
	}
	sameCallers, wiredSeen := map[string]bool{}, map[string]bool{}
	for _, d := range defs {
		d = e.genFilter(d, o.Generated)
		path, ln := splitWhere(d.Where)
		v.XPaths = append(v.XPaths, path)
		ls := e.Lines(path)
		kind := strings.ToLower(d.Kind)
		if k, _ := declOf(lineAt(ls, ln), filepath.Ext(path)); k != "" {
			kind = k
		}
		start := ln
		var body, own []string // own: what the definition says about itself
		var more int
		window := false
		switch {
		case o.Full:
			own, body, start, more = defBody(ls, ln, path, 400)
		case isTypeKind(d.Kind):
			own, body, start, more = defBody(ls, ln, path, defTypeLines) // the whole field or method list
		case d.Func:
			own, body, start, more = defBody(ls, ln, path, defFuncLines) // the signature and the lines of the body after it
		default:
			// A const or var reads best in its function or among its neighbours.
			var end int
			start, end = constWindow(ls, ln, path)
			if start >= 1 && end >= start && end <= len(ls) {
				body = ls[start-1 : end]
			}
			window = true
		}
		place := d.Where
		if end := start + len(body) + more - 1; window && start != ln {
			place = fmt.Sprintf("%s (lines %d-%d)", d.Where, start, end)
		} else if end > start {
			place = fmt.Sprintf("%s:%d-%d", path, start, end)
		}
		head := fmt.Sprintf("%s  %s  %s", f.Symbol, kind, place)
		if d.Match == "candidate" {
			head += "  candidate (identity not verified)"
		}
		if e.isGenerated(path) {
			head += "  generated" // the real definition, even though the file is generated
		}
		def := &ctxPart{Raw: []string{head}}
		v.Parts = append(v.Parts, def)
		docAt := ln // comments above the position, then what the definition says itself
		if window {
			docAt = start
		}
		doc := append(docBlock(ls, docAt, path), own...)
		switch {
		case o.Compact:
			if len(doc) > 0 {
				def.Raw = append(def.Raw, short(doc[0], 140))
			}
			if len(body) > 0 {
				if o.SchemaVersion == 2 {
					def.add(path, ctxLine{N: start, Text: body[0], Match: true})
				} else {
					def.Raw = append(def.Raw, "  "+short(keepIndent(body[0]), width))
				}
			}
		default:
			for _, l := range doc[:min(len(doc), 10)] {
				def.Raw = append(def.Raw, short(l, width))
			}
			if o.Tests != "only" {
				for i, l := range body {
					if o.SchemaVersion == 2 {
						def.add(path, ctxLine{N: start + i, Text: l, Match: true})
					} else {
						def.Raw = append(def.Raw, "  "+short(keepIndent(l), width))
					}
				}
				if more > 0 {
					def.Raw = append(def.Raw, fmt.Sprintf("  (+%d lines; --full)", more))
				}
			}
		}
		if f.Lang == "go" && d.Kind == "Struct" && !o.Compact {
			if res, err := e.Text(rgQuery{Pattern: `^func \(\w+ \*?` + regexp.QuoteMeta(f.Symbol) + `\) \w+`, Type: "go", CaseSensitive: true}); err == nil {
				var ms []string
				for _, h := range res.Hits {
					if !isTestPath(h.Path) && len(ms) < 12 {
						ms = append(ms, fmt.Sprintf("%s (%s:%d)", methodName(h.Text), h.Path, h.Line))
					}
				}
				if len(ms) > 0 {
					def.Raw = append(def.Raw, "methods: "+strings.Join(ms, ", "))
				}
			}
		}
		page := max(o.Page, 1)
		if d.Func {
			title := usedBy(d, o.Refs, o.Tests)
			size := o.Refs
			if o.Limit > 0 {
				size = o.Limit
			}
			var shown []string
			var lo, hi int
			var past bool
			sorted := sortLocs(d.Callers)
			if size > 0 && o.Tests != "only" {
				lo, hi, past = pageRange(d.Total, size, page)
				shown = sorted[min(lo, len(sorted)):min(hi, len(sorted))]
			}
			key := strings.Join(shown, ",")
			if len(shown) > 0 && sameCallers[key] {
				v.Parts = append(v.Parts, &ctxPart{Raw: []string{title + " (same call sites as above)"}})
			} else if len(shown) > 0 {
				sameCallers[key] = true
				p := e.refPart(v, title+":", path, shown, o.Context, width)
				if foot := pageFooter(lo, hi, d.Total, page, false, o.Batch, narrowSym); foot != "" {
					p.Foot = "  " + foot + restNote(locFiles(sorted[min(hi, len(sorted)):]))
				}
				v.Parts = append(v.Parts, p)
			} else if past {
				v.Parts = append(v.Parts, &ctxPart{Raw: []string{title + " - " + pastEnd(d.Total, size, page, "callers")}})
			} else if size > 0 && o.Tests != "only" && d.Total > len(sorted) {
				v.Parts = append(v.Parts, &ctxPart{Raw: []string{fmt.Sprintf("%s - page %d needs callers past the %d kept; narrow with %s", title, page, len(sorted), narrowSym)}})
			} else if o.Tests != "only" {
				hint := ""
				if size == 0 && d.Total > 0 && o.Tests != "only" {
					hint = " (--refs N lists them)"
				}
				v.Parts = append(v.Parts, &ctxPart{Raw: []string{title + hint}})
			}
		}
		if o.Impl && len(d.Impls) > 0 {
			size := symRefsDefault
			if o.Limit > 0 {
				size = o.Limit
			}
			if o.Full {
				size = ctxMaxRefs
			}
			title := fmt.Sprintf("implemented by %d:", len(d.Impls))
			lo, hi, past := pageRange(len(d.Impls), size, page)
			if past {
				v.Parts = append(v.Parts, &ctxPart{Raw: []string{strings.TrimSuffix(title, ":") + " - " + pastEnd(len(d.Impls), size, page, "implementers")}})
			} else {
				p := e.refPart(v, title, path, sortLocs(d.Impls)[lo:hi], 0, width)
				if foot := pageFooter(lo, hi, len(d.Impls), page, false, o.Batch, narrowSym); foot != "" {
					p.Foot = "  " + foot + restNote(locFiles(sortLocs(d.Impls)[hi:]))
				}
				v.Parts = append(v.Parts, p)
			}
		}
		if (o.Tests == "include" || o.Tests == "only") && len(d.TestRefs) > 0 {
			v.Parts = append(v.Parts, e.refPart(v, testsTitle(d, o), path, d.TestRefs, 0, width))
		}
		if o.Refs > 0 && o.Tests != "only" && len(d.Callers) > 0 {
			if w := e.wired(f.Symbol, d); w != "" && !wiredSeen[w] {
				wiredSeen[w] = true
				v.Parts = append(v.Parts, &ctxPart{Raw: []string{strings.TrimPrefix(w, "  ")}})
			}
		}
	}
	return v
}

var methodRe = regexp.MustCompile(`^func\s+\([^)]*\)\s*(\w+)`)

func methodName(line string) string {
	if m := methodRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return line
}

// ---- lookups ----

var ctxKinds = []string{"concept", "symbol", "text", "read"}

var ctxOptionKeys = []string{"tests", "generated", "max", "limit", "page", "fixed", "word", "ignore_case", "glob", "type", "files_only", "count", "context", "multiline",
	"only_matching", "per_file", "all", "refs", "impl", "def_kind", "in", "path", "from", "to", "why"}

// These per-kind option definitions drive inline/JSON validation and help.
var ctxOptionsByKind = map[string]string{
	"concept": "tests generated max limit page path why",
	"text":    "tests generated max limit page fixed word ignore_case glob type files_only count context multiline only_matching per_file all path why",
	"symbol":  "tests generated max limit page refs impl def_kind in context why",
	"read":    "path from to why",
}

func lookupOptionKeys(kind string) []string { return strings.Fields(ctxOptionsByKind[canonKind(kind)]) }

func canonKind(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "concept", "find":
		return "concept"
	case "symbol", "sym":
		return "symbol"
	case "text":
		return "text"
	case "read":
		return "read"
	}
	return ""
}

// validate checks one lookup before anything runs.
func (l *ctxLookup) validate() error {
	k := canonKind(l.Kind)
	if k == "" {
		return fmt.Errorf("unknown kind %q; valid kinds: %s", l.Kind, strings.Join(ctxKinds, ", "))
	}
	l.Kind = k
	allowed := map[string]bool{"kind": true, "query": true, "package": true}
	for _, key := range lookupOptionKeys(k) {
		allowed[key] = true
	}
	// Marshal supplies nonzero options of programmatic plans; present also
	// retains explicitly supplied false/zero options from inline and JSON plans.
	b, _ := json.Marshal(l)
	var supplied map[string]json.RawMessage
	_ = json.Unmarshal(b, &supplied)
	for key := range l.present {
		if supplied == nil {
			supplied = map[string]json.RawMessage{}
		}
		supplied[key] = nil
	}
	for key := range supplied {
		if !allowed[key] {
			return fmt.Errorf("option %q is not valid for %s; valid options: %s", key, k, strings.Join(lookupOptionKeys(k), ", "))
		}
	}
	for _, key := range []string{"limit", "max", "page"} {
		if l.present[key] {
			n := map[string]int{"limit": l.Limit, "max": l.Max, "page": l.Page}[key]
			if n < 1 {
				return fmt.Errorf("%s must be at least 1", key)
			}
		}
	}
	if l.PerFile < 0 || l.From < 0 || l.To < 0 || l.Context != nil && *l.Context < 0 || l.Refs != nil && *l.Refs < 0 {
		return fmt.Errorf("numeric options cannot be negative")
	}
	if l.FilesOnly && l.Count {
		return fmt.Errorf("files_only and count cannot be combined")
	}
	if l.Limit > ctxRgMax || l.Max > ctxRgMax || l.Page > ctxRgMax {
		return fmt.Errorf("limit, max and page are capped at %d; narrow the lookup", ctxRgMax)
	}
	if l.Limit < 0 || l.Max < 0 || l.Page < 0 {
		return fmt.Errorf("limit, max and page must be at least 1")
	}
	if !validTests(l.Tests) {
		return fmt.Errorf("tests=%q; use none, include or only", l.Tests)
	}
	if !validGenerated(l.Gen) {
		return fmt.Errorf("generated=%q; use none, include or only", l.Gen)
	}
	if k != "read" && strings.TrimSpace(l.Query) == "" {
		return fmt.Errorf("a %s lookup needs a query", k)
	}
	if k == "read" && l.Path == "" {
		return fmt.Errorf("a read lookup needs a path")
	}
	return nil
}

// tokens splits an inline lookup on spaces; a double- or single-quoted part is
// one literal token.
func inlineTokens(s string) (toks []string, quoted []bool) {
	var cur strings.Builder
	var q rune
	hasQ, in := false, false
	flush := func() {
		if in || cur.Len() > 0 {
			toks = append(toks, cur.String())
			quoted = append(quoted, hasQ)
		}
		cur.Reset()
		hasQ, in = false, false
	}
	for _, r := range s {
		switch {
		case q != 0:
			if r == q {
				q = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			q, hasQ, in = r, true, true
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	flush()
	return toks, quoted
}

func atoiOpt(key, v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q is not a number", key, v)
	}
	return n, nil
}

func boolOpt(key, v string) (bool, error) {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%s=%q is not true or false", key, v)
}

// parseInlineLookup reads `kind:query key=value ...`, for example
// `text:polar tests=none glob=internal/**`. A quoted token is part of the query
// even if it looks like an option.
func parseInlineLookup(s string) (ctxLookup, error) {
	var l ctxLookup
	kind, rest, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return l, fmt.Errorf("%q: write kind:query, with kind one of %s", s, strings.Join(ctxKinds, ", "))
	}
	l.Kind = kind
	l.present = map[string]bool{}
	toks, quoted := inlineTokens(rest)
	var query []string
	for i, t := range toks {
		key, val, isOpt := strings.Cut(t, "=")
		if quoted[i] || !isOpt || key == "" || strings.ContainsAny(key, " /.:") {
			query = append(query, t)
			continue
		}
		l.present[key] = true
		var err error
		switch key {
		case "tests":
			l.Tests = val
		case "generated":
			l.Gen = val
		case "why":
			l.Why = val
		case "path":
			l.Path = val
		case "type":
			l.Type = val
		case "glob":
			l.Glob = append(l.Glob, val)
		case "def_kind":
			l.DefKind = val
		case "in":
			l.In = val
		case "max":
			l.Max, err = atoiOpt(key, val)
		case "limit":
			l.Limit, err = atoiOpt(key, val)
			if err == nil && l.Limit < 1 {
				err = fmt.Errorf("limit must be at least 1")
			}
		case "page":
			l.Page, err = atoiOpt(key, val)
			if err == nil && l.Page < 1 {
				err = fmt.Errorf("page must be at least 1")
			}
		case "context":
			var n int
			n, err = atoiOpt(key, val)
			l.Context = &n
		case "per_file":
			l.PerFile, err = atoiOpt(key, val)
		case "refs":
			var n int
			n, err = atoiOpt(key, val)
			l.Refs = &n
		case "from":
			l.From, err = atoiOpt(key, val)
		case "to":
			l.To, err = atoiOpt(key, val)
		case "fixed":
			l.Fixed, err = boolOpt(key, val)
		case "word":
			l.Word, err = boolOpt(key, val)
		case "ignore_case":
			l.IgnoreCase, err = boolOpt(key, val)
		case "files_only":
			l.FilesOnly, err = boolOpt(key, val)
		case "count":
			l.Count, err = boolOpt(key, val)
		case "multiline":
			l.Multiline, err = boolOpt(key, val)
		case "only_matching":
			l.OnlyMatching, err = boolOpt(key, val)
		case "all":
			l.All, err = boolOpt(key, val)
		case "impl":
			var on bool
			on, err = boolOpt(key, val)
			l.Impl = &on
		default:
			return l, fmt.Errorf("%q: unknown option %q; valid options: %s (quote a query word that contains '=')", s, key, strings.Join(ctxOptionKeys, ", "))
		}
		if err != nil {
			return l, fmt.Errorf("%q: %v", s, err)
		}
	}
	l.Query = strings.Join(query, " ")
	if canonKind(kind) == "read" && l.Path == "" {
		l.Path, l.Query = l.Query, ""
	}
	return l, nil
}

// symOpts are the symbol options of a lookup over the defaults of the format.
func (l ctxLookup) symOpts(f ctxFmt) symOpts {
	o := defaultSymOpts(f)
	o.Tests, o.Kind, o.In, o.Generated = l.Tests, l.DefKind, l.In, l.Gen
	if l.Refs != nil {
		o.Refs = *l.Refs
	}
	if l.Impl != nil {
		o.Impl = *l.Impl
	}
	if l.Context != nil {
		o.Context = *l.Context
	}
	o.Limit, o.Page = l.pageSize(0), l.Page
	return o
}

func (l ctxLookup) rgQuery() rgQuery {
	gen := l.Gen
	if l.All && (gen == "" || gen == "none") {
		gen = "include" // --all also searches generated and vendored files
	}
	return rgQuery{Pattern: strings.TrimSpace(l.Query), Fixed: l.Fixed, Word: l.Word, IgnoreCase: l.IgnoreCase, Multiline: l.Multiline,
		OnlyMatching: l.OnlyMatching, FilesOnly: l.FilesOnly, Count: l.Count, PerFile: l.PerFile,
		Globs: l.Glob, Type: l.Type, Path: l.Path, AllFiles: l.All, Generated: gen, Tests: l.Tests, ViewFilters: true}
}

// textLookup runs a text lookup and builds its view.
func (e *ctxEnv) textLookup(l ctxLookup, f ctxFmt) *ctxView {
	q := l.rgQuery()
	v := &ctxView{Kind: "text", Query: q.Pattern, Why: l.Why}
	if q.Path != "" && q.Path != "." {
		p, ok := repoPath(q.Path)
		if !ok {
			v.Err = "path " + q.Path + " is outside the repo"
			return v
		}
		q.Path = p
	}
	res, err := e.Text(q)
	ctx := ctxContextDefault
	if f.Compact {
		ctx = 0
	}
	if l.Context != nil {
		ctx = *l.Context
	}
	v = e.textView(q, l.Tests, res, l.pageSize(0), l.pageNo(), ctx, f)
	v.Why = l.Why
	v.SearchedScope = "checkout root " + e.Code
	if q.Path != "" {
		v.SearchedScope += "; path " + q.Path
	}
	if len(q.Globs) > 0 {
		v.SearchedScope += "; globs " + strings.Join(q.Globs, ",")
	}
	if q.Type != "" {
		v.SearchedScope += "; type " + q.Type
	}
	v.Excluded = []string{"gitignored and hidden files", "tests=" + orDefault(l.Tests, "none"), "generated=" + orDefault(q.Generated, "none")}
	if !q.AllFiles {
		v.Excluded = append(v.Excluded, "default lockfile, minified, generated and vendor globs (use --all; gitignore still applies)")
	}
	if q.PerFile > 0 {
		v.Excluded = append(v.Excluded, fmt.Sprintf("at most %d matches per file (--per-file)", q.PerFile))
	}
	if err != nil {
		v.Err = err.Error()
		v.CountKind = "unknown"
		if res.Total > 0 {
			v.Status, v.CountKind = "partial", "lower_bound"
		} else {
			v.Status = "failed"
		}
	}
	if res.Limited && v.Shown == 0 {
		v.Zero = ""
		v.Head = "requested page is beyond retained excerpts; narrow with --glob/-p"
	}
	if len(l.Glob) > 0 {
		v.Scope = strings.TrimSpace(v.Scope + " glob " + strings.Join(l.Glob, ","))
		v.setZero("the whole repo")
	}
	v.setZero("the whole repo")
	return v
}

// readLookup builds the view of a read lookup.
func (e *ctxEnv) readLookup(l ctxLookup, f ctxFmt) *ctxView {
	v := &ctxView{Kind: "read", Query: l.Path, Why: l.Why}
	p, ok := repoPath(l.Path)
	if !ok {
		v.Err = "path " + l.Path + " is outside the repo"
		return v
	}
	if real, err := filepath.EvalSymlinks(filepath.Join(e.Code, p)); err != nil || !strings.HasPrefix(real, evalOr(e.Code)+string(filepath.Separator)) {
		v.Err = "file " + p + " not found in the repo"
		return v
	}
	ls := e.Lines(p)
	from, to := max(l.From, 1), l.To
	if to < from || to-from+1 > ctxReadLines {
		to = from + ctxReadLines - 1
	}
	to = min(to, len(ls))
	if from > to {
		v.Err = fmt.Sprintf("%s has %d lines; the range starts past the end", p, len(ls))
		return v
	}
	v = readView(p, from, ls[from-1:to], f.width())
	v.Why = l.Why
	v.Query = p
	return v
}

// conceptLookup runs a concept lookup and builds its view. jbcontext has only
// a limit, so page P asks for P*N results and keeps the last N of them.
func (e *ctxEnv) conceptLookup(l ctxLookup, f ctxFmt) *ctxView {
	def := ctxFindPage
	if f.Compact {
		def = 3
	}
	limit, page := min(l.pageSize(def), ctxRgMax), min(l.pageNo(), ctxRgMax)
	fetch := min(limit*page+ctxFindLookahead, ctxRgMax)
	// A few ranked hits past the page are fetched to name their files in the footer.
	hits, err := e.Concept(short(flat(l.Query), 300), l.Path, fetch)
	rawFull := len(hits) >= fetch
	if err != nil && len(hits) == 0 {
		return &ctxView{Kind: "find", Query: l.Query, Why: l.Why, Err: err.Error(), Status: "failed", CountKind: "unknown"}
	}
	// Filtering before slicing gives pages over visible candidates within the
	// fetched raw-rank window. The window limitation is explicit below.
	hidden, hiddenG := 0, 0
	var visible []codeHit
	for _, h := range hits {
		if !passTests(l.Tests, h.Path) {
			hidden++
			continue
		}
		if !e.passGen(l.Gen, h.Path) {
			hiddenG++
			continue
		}
		visible = append(visible, h)
	}
	hits = visible
	var ahead []codeHit
	if len(hits) > limit*page {
		hits, ahead = hits[:limit*page], hits[limit*page:]
	}
	full := len(hits) >= limit*page
	lo, hi, past := pageRange(len(hits), limit, page)
	v := e.conceptViewG(l.Query, l.Path, l.Tests, l.Gen, hits[lo:hi], f)
	v.Why = l.Why
	v.Hidden, v.HiddenG = hidden, hiddenG
	v.SearchedScope = "semantic index candidate window; path=" + orDefault(l.Path, ".")
	v.CountKind = "unknown"
	v.Cut = fmt.Sprintf("semantic pagination uses visible candidates within the first %d raw ranks (backend cap %d); filters or stale locations can hide later candidates; narrow with -p/path= or increase limit within the cap; candidates never establish absence", fetch, ctxRgMax)
	if rawFull {
		v.Status, v.CountKind = "partial", "lower_bound"
	}
	if hidden+hiddenG > 0 {
		v.Status = "partial"
	}
	if err != nil {
		v.Err, v.Status, v.CountKind = err.Error(), "partial", "lower_bound"
	}
	if conceptTip(l.Query, len(hits)) {
		v.Tip = conceptTipText
	}
	switch {
	case past:
		if rawFull || hidden+hiddenG > 0 {
			v.Head = fmt.Sprintf("page %d is beyond visible candidates in the fetched raw-rank window; narrow with -p/path=", page)
		} else {
			v.Zero = pastEnd(len(hits), limit, page, "hits")
		}
	case full && hi-lo == limit:
		v.Pager = pageFooter(lo, hi, hi+len(ahead), page, true, f.Batch, narrowFind)
	default:
		v.Pager = pageFooter(lo, hi, len(hits), page, false, f.Batch, narrowFind)
	}
	// The header counts the hits on this page (the view counts merged blocks);
	// how many exist is the footer's business.
	v.Shown = hi - lo
	v.Total = v.Shown
	if v.Pager != "" {
		var left []string
		for _, h := range append(append([]codeHit(nil), hits[hi:]...), ahead...) {
			if passTests(l.Tests, h.Path) && e.passGen(l.Gen, h.Path) {
				left = append(left, h.Path)
			}
		}
		v.Pager += restNote(left)
	}
	if v.Total == 0 && v.Zero == "" && v.Err == "" {
		v.Zero = "no visible candidates in the searched index window; this does not establish absence"
	}
	return v
}

// locFiles is the files of "path:line" places, in order.
func locFiles(locs []string) []string {
	var out []string
	for _, l := range locs {
		p, _ := splitWhere(l)
		out = append(out, p)
	}
	return out
}

// BatchViews validates every lookup first (an unknown kind or option is a
// usage error before anything runs), then runs them in parallel; a lookup that
// fails is a row with its error, never a failed batch.
func (e *ctxEnv) BatchViews(lks []ctxLookup, f ctxFmt) ([]*ctxView, error) {
	f.Batch = true
	for i := range lks {
		if err := lks[i].validate(); err != nil {
			return nil, fmt.Errorf("lookup %d: %v", i+1, err)
		}
	}
	// Symbols in one batch, so one language server serves them all.
	goSeen := map[string]bool{}
	var goNames []string
	tsSeen := map[tsJob]bool{}
	var tsJobs []tsJob
	for _, l := range lks {
		q := strings.TrimSpace(l.Query)
		if l.Kind != "symbol" || !symIdent.MatchString(q) {
			continue
		}
		if !goSeen[q] {
			goSeen[q] = true
			goNames = append(goNames, q)
		}
		if r := tsProjectRoot(e.Code, q, nil); r != "" && tsPlain.MatchString(q) {
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
	views := make([]*ctxView, len(lks))
	e.parallel(len(lks), func(i int) {
		l := lks[i]
		switch l.Kind {
		case "concept":
			views[i] = e.conceptLookup(l, f)
		case "text":
			views[i] = e.textLookup(l, f)
		case "read":
			views[i] = e.readLookup(l, f)
		case "symbol":
			q := strings.TrimSpace(l.Query)
			fact := goFacts[q]
			if fact == nil || len(fact.Defs) == 0 {
				if t := tsFacts[tsJob{tsProjectRoot(e.Code, q, nil), q}]; t != nil {
					fact = t
				}
			}
			if fact == nil {
				views[i] = &ctxView{Kind: "sym", Query: q, Err: "symbol lookup unavailable or failed"}
			} else {
				views[i] = e.symView(*fact, l.symOpts(f))
			}
		}
		views[i].Why = l.Why
		views[i].finishTruth()
	})
	return views, nil
}
