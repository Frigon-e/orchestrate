package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// orch lsp answers symbol questions by name in one call: definition,
// references, callers, implementations, a file outline and hover. Definitions
// and references come from the same path as `orch ctx sym` (ctxEnv.Symbols:
// gopls, the TypeScript server, the registry languages' servers, then the
// declaration scan); callers, implementations of types, outlines and hover ask
// a language server directly, one server per language and project root for the
// whole call. Output is plain text, one hit per line.

type lspHelp struct {
	usage, summary string
	examples       []string
}

var lspVerbs = []string{"def", "refs", "callers", "impls", "symbols", "hover"}

var lspHelps = map[string]lspHelp{
	"def": {"orch lsp def <Name|pkg.Name|Type.Method> [Name...] [--json]",
		"Where a symbol is defined: path:line, kind, signature line and the first line of its doc comment. An ambiguous name lists each candidate; qualify it.",
		[]string{"orch lsp def StartCheckout", "orch lsp def Stripe.StartCheckout Gateway", "orch lsp def pay.Gateway --json"}},
	"refs": {"orch lsp refs <Name|pkg.Name|Type.Method> [Name...] [--limit N] [--json]",
		"References grouped by file, path:line: text, tests included. Default limit 30.",
		[]string{"orch lsp refs lookupConcept", "orch lsp refs Stripe.StartCheckout --limit 10", "orch lsp refs Handle Retry"}},
	"callers": {"orch lsp callers <Name|pkg.Name|Type.Method> [Name...] [--limit N] [--json]",
		"Incoming calls from the language server's call hierarchy: path:line in EnclosingFunc. Default limit 30.",
		[]string{"orch lsp callers Handle", "orch lsp callers Stripe.StartCheckout --limit 5"}},
	"impls": {"orch lsp impls <Name|pkg.Name|Type.Method> [Name...] [--json]",
		"Implementations of an interface or an interface method, path:line: text.",
		[]string{"orch lsp impls Gateway", "orch lsp impls Gateway.StartCheckout"}},
	"symbols": {"orch lsp symbols <path/to/file> [--json]",
		"Outline of a file: kind name line, nested members indented.",
		[]string{"orch lsp symbols pay/pay.go", "orch lsp symbols src/cart.ts --json"}},
	"hover": {"orch lsp hover <path:line:col> [--json]",
		"Type and doc at a position (line and column are 1-based).",
		[]string{"orch lsp hover pay/pay.go:7:18"}},
}

const lspHomeText = `orch lsp - symbol questions by name, answered by the language servers (read-only)

  orch lsp def Name [Name...]        definition: path:line, kind, signature line, first doc line
  orch lsp refs Name [--limit N]     references grouped by file (path:line: text), default limit 30
  orch lsp callers Name [--limit N]  incoming calls: path:line in EnclosingFunc
  orch lsp impls Name                implementations of an interface or method
  orch lsp symbols path/to/file      outline of the file: kind name line
  orch lsp hover path:line:col       type and doc at a position (1-based)

Name is Name, pkg.Name or Type.Method. An ambiguous name lists its candidates; qualify it.
Several names for def, refs, callers or impls are answered under "## Name" headers.
--json prints the same data as JSON. Exit 1: no such symbol. Exit 2: usage error or no language server.
orch lsp <verb> --help shows examples.
`

// lspItem is one hit; which fields are set depends on the verb.
type lspItem struct {
	Path  string `json:"path,omitempty"`
	Line  int    `json:"line,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Name  string `json:"name,omitempty"`
	Text  string `json:"text,omitempty"`
	Doc   string `json:"doc,omitempty"`
	In    string `json:"in,omitempty"`
	Depth int    `json:"depth,omitempty"`
}

// lspAnswer is the answer for one name (or one file). Status is ok, ambiguous,
// not_found, no_server or error.
type lspAnswer struct {
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Message string    `json:"message,omitempty"`
	Total   int       `json:"total,omitempty"` // hits found, when more than Items
	Items   []lspItem `json:"items"`
}

func lspPrintHelp(a *app, verb string) {
	if verb == "" {
		fmt.Fprint(a.out, lspHomeText)
		return
	}
	h := lspHelps[verb]
	fmt.Fprintf(a.out, "usage: %s\n\n%s\n\nexamples:\n", h.usage, h.summary)
	for _, ex := range h.examples {
		fmt.Fprintf(a.out, "  %s\n", ex)
	}
}

func cmdLsp(a *app, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		lspPrintHelp(a, "")
		return nil
	}
	verb, rest := args[0], args[1:]
	h, ok := lspHelps[verb]
	if !ok {
		return &exitError{code: 2, msg: fmt.Sprintf("unknown lsp verb %q (valid: %s); orch lsp --help", verb, strings.Join(lspVerbs, ", "))}
	}
	for _, s := range rest {
		if s == "--" {
			break
		}
		if s == "-h" || s == "--help" {
			lspPrintHelp(a, verb)
			return nil
		}
	}
	fs := a.flags("lsp " + verb)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	asJSON := fs.Bool("json", false, "print JSON")
	limit := 30
	if verb == "refs" || verb == "callers" {
		fs.IntVar(&limit, "limit", 30, "most hits shown")
	}
	pos, err := parseArgs(fs, rest)
	if err != nil {
		return &exitError{code: 2, msg: "bad flags for lsp " + verb + "; usage: " + h.usage}
	}
	switch {
	case len(pos) == 0, (verb == "symbols" || verb == "hover") && len(pos) != 1:
		return &exitError{code: 2, msg: "usage: " + h.usage}
	case limit < 1:
		return &exitError{code: 2, msg: "--limit must be at least 1"}
	}
	e, err := ctxEnvFor(a)
	if err != nil {
		return err
	}
	r := &lspRun{e: e, sessions: map[string]*lspSession{}, whys: map[string]string{}}
	defer r.close()
	var answers []lspAnswer
	switch verb {
	case "symbols":
		answers = []lspAnswer{r.symbols(pos[0])}
	case "hover":
		answers = []lspAnswer{r.hover(pos[0])}
	default:
		answers = r.byName(verb, pos, limit)
	}
	code := 0
	for _, an := range answers {
		switch an.Status {
		case "no_server":
			code = 2
		case "not_found", "error":
			code = max(code, 1)
		}
	}
	if *asJSON {
		if err := json.NewEncoder(a.out).Encode(map[string]any{"verb": verb, "results": answers}); err != nil {
			return err
		}
	} else {
		var b strings.Builder
		for i, an := range answers {
			if len(answers) > 1 {
				if i > 0 {
					b.WriteString("\n")
				}
				fmt.Fprintf(&b, "## %s\n", an.Name)
			}
			b.WriteString(lspRender(verb, an))
		}
		fmt.Fprint(a.out, b.String())
	}
	if code != 0 {
		return &exitError{code: code}
	}
	return nil
}

func lspRender(verb string, an lspAnswer) string {
	var b strings.Builder
	switch an.Status {
	case "not_found", "no_server", "error":
		return an.Message + "\n"
	case "ambiguous":
		b.WriteString(an.Message + "\n")
		for _, it := range an.Items {
			fmt.Fprintf(&b, "  %s:%d %s\n", it.Path, it.Line, it.Kind)
		}
		return b.String()
	}
	if len(an.Items) == 0 {
		return an.Message + "\n"
	}
	for _, it := range an.Items {
		switch verb {
		case "def":
			fmt.Fprintf(&b, "%s:%d %s", it.Path, it.Line, it.Kind)
			if it.Text != "" {
				b.WriteString(": " + it.Text)
			}
			if it.Doc != "" {
				b.WriteString("; doc: " + it.Doc)
			}
			b.WriteString("\n")
		case "callers":
			fmt.Fprintf(&b, "%s:%d in %s\n", it.Path, it.Line, it.In)
		case "symbols":
			fmt.Fprintf(&b, "%s%s %s %d\n", strings.Repeat("  ", it.Depth), it.Kind, it.Name, it.Line)
		case "hover":
			b.WriteString(it.Text + "\n")
		default: // refs, impls
			fmt.Fprintf(&b, "%s:%d: %s\n", it.Path, it.Line, it.Text)
		}
	}
	if an.Total > len(an.Items) {
		fmt.Fprintf(&b, "... %d more (%d total; raise --limit)\n", an.Total-len(an.Items), an.Total)
	}
	return b.String()
}

// lspRun is one orch lsp call: the environment and the servers started so far.
type lspRun struct {
	e        *ctxEnv
	sessions map[string]*lspSession
	whys     map[string]string
}

func (r *lspRun) close() {
	for _, s := range r.sessions {
		if s != nil {
			s.c.close()
		}
	}
}

func noServer(lang, name string) lspAnswer {
	return lspAnswer{Name: name, Status: "no_server", Message: fmt.Sprintf("no language server for %s; use rg -n -w %s", lang, name), Items: []lspItem{}}
}

// byName answers def, refs, callers or impls for every name, sharing one
// symbol pass (and so one server per language) between them.
func (r *lspRun) byName(verb string, names []string, limit int) []lspAnswer {
	e := r.e
	goSeen := map[string]bool{}
	var goNames []string
	tsSeen := map[tsJob]bool{}
	var tsJobs []tsJob
	for _, q := range names {
		if !symIdent.MatchString(q) || goSeen[q] {
			continue
		}
		goSeen[q] = true
		goNames = append(goNames, q)
		if root := tsProjectRoot(e.Code, q, nil); root != "" && tsPlain.MatchString(q) {
			if j := (tsJob{root, q}); !tsSeen[j] {
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
	var out []lspAnswer
	for _, q := range names {
		notFound := lspAnswer{Name: q, Status: "not_found", Message: "no symbol " + q, Items: []lspItem{}}
		f := goFacts[q]
		if f == nil || len(f.Defs) == 0 {
			if t := tsFacts[tsJob{tsProjectRoot(e.Code, q, nil), q}]; t != nil {
				f = t
			}
		}
		if f == nil {
			out = append(out, notFound)
			continue
		}
		var defs []symDef
		seen := map[string]bool{}
		for _, d := range f.Defs {
			if !seen[d.Where] {
				seen[d.Where] = true
				defs = append(defs, d)
			}
		}
		// Definitions outside the checkout (the standard library) only add noise
		// while the repo has its own.
		var inRepo []symDef
		for _, d := range defs {
			if p, _ := splitWhere(d.Where); !filepath.IsAbs(p) {
				inRepo = append(inRepo, d)
			}
		}
		if len(inRepo) > 0 {
			defs = inRepo
		}
		switch {
		case len(defs) == 0 && len(f.Errors) > 0:
			out = append(out, noServer(orDefault(f.Lang, "go and typescript"), q))
		case len(defs) == 0:
			out = append(out, notFound)
		case len(defs) > 1:
			an := lspAnswer{Name: q, Status: "ambiguous", Items: []lspItem{},
				Message: fmt.Sprintf("%s is ambiguous (%d candidates); qualify it as Type.%s or pkg.%s:", q, len(defs), q, q)}
			for _, d := range defs {
				p, ln := splitWhere(d.Where)
				an.Items = append(an.Items, lspItem{Path: p, Line: ln, Kind: d.Kind})
			}
			out = append(out, an)
		default:
			out = append(out, r.one(verb, q, f, defs[0], limit))
		}
	}
	return out
}

func (r *lspRun) one(verb, name string, f *symFact, d symDef, limit int) lspAnswer {
	path, ln := splitWhere(d.Where)
	base := name[strings.LastIndex(name, ".")+1:]
	an := lspAnswer{Name: name, Status: "ok", Items: []lspItem{}}
	lang := f.Lang
	if l := langOf(path); l != nil {
		lang = l.ID
	}
	switch verb {
	case "def":
		it := lspItem{Path: path, Line: ln, Kind: d.Kind}
		if ls := r.e.Lines(path); ln >= 1 && ln <= len(ls) {
			it.Text = short(strings.TrimSpace(ls[ln-1]), 140)
			it.Doc = docLine(ls, ln)
		}
		an.Items = append(an.Items, it)
		return an
	case "refs":
		if d.Func && !d.RefsUnknown && !d.ByName && len(d.Errors) == 0 {
			for _, loc := range append(append([]string(nil), d.Callers...), d.TestRefs...) {
				p, n := splitWhere(loc)
				p = r.rel(p)
				an.Items = append(an.Items, lspItem{Path: p, Line: n, Text: r.lineText(p, n)})
			}
			an.Total = d.Total + d.Tests
		} else {
			s, pos, why := r.target(path, ln, base)
			if s == nil {
				return noServer(orDefault(lang, "this language"), name).withWhy(why)
			}
			raw, err := s.ask("textDocument/references", withCtx(pos, map[string]any{"includeDeclaration": false}))
			if err != nil {
				return lspFailed(name, err)
			}
			locs, err := decodeLSPLocs(raw, false)
			if err != nil {
				return lspFailed(name, err)
			}
			an.Items = r.locItems(s, locs, d.Where)
			an.Total = len(an.Items)
		}
		an.Items, an.Total = lspCut(an.Items, an.Total, limit)
		if len(an.Items) == 0 {
			an.Message = "no references to " + name
		}
	case "callers":
		s, pos, why := r.target(path, ln, base)
		if s == nil {
			return noServer(orDefault(lang, "this language"), name).withWhy(why)
		}
		if !s.caps.Calls {
			return lspAnswer{Name: name, Status: "no_server", Items: []lspItem{},
				Message: fmt.Sprintf("%s has no call hierarchy for %s; use orch lsp refs %s", s.bin, lang, name)}
		}
		raw, err := s.ask("textDocument/prepareCallHierarchy", pos)
		if err != nil {
			return lspFailed(name, err)
		}
		var prepared []json.RawMessage
		_ = json.Unmarshal(raw, &prepared)
		if len(prepared) > 0 {
			raw, err = s.c.call("callHierarchy/incomingCalls", map[string]any{"item": prepared[0]}, tsRequestLimit)
			if err != nil {
				return lspFailed(name, err)
			}
			var calls []struct {
				From struct {
					Name string `json:"name"`
					URI  string `json:"uri"`
				} `json:"from"`
				FromRanges []lspRange `json:"fromRanges"`
			}
			if err := json.Unmarshal(raw, &calls); err != nil {
				return lspFailed(name, err)
			}
			seen := map[string]bool{}
			for _, c := range calls {
				p := s.locOf(r.e.Code, c.From.URI)
				for _, rg := range c.FromRanges {
					key := fmt.Sprintf("%s:%d", p, rg.Start.Line+1)
					if !seen[key] {
						seen[key] = true
						an.Items = append(an.Items, lspItem{Path: p, Line: rg.Start.Line + 1, In: c.From.Name})
					}
				}
			}
		}
		an.Total = len(an.Items)
		an.Items, an.Total = lspCut(an.Items, an.Total, limit)
		if len(an.Items) == 0 {
			an.Message = "no callers of " + name
		}
	case "impls":
		s, pos, why := r.target(path, ln, base)
		if s == nil {
			return noServer(orDefault(lang, "this language"), name).withWhy(why)
		}
		if !s.caps.Impl {
			return lspAnswer{Name: name, Status: "no_server", Items: []lspItem{},
				Message: fmt.Sprintf("%s has no implementation lookup for %s; use rg -n -w %s", s.bin, lang, name)}
		}
		raw, err := s.ask("textDocument/implementation", pos)
		if err != nil {
			return lspFailed(name, err)
		}
		locs, err := decodeLSPLocs(raw, true)
		if err != nil {
			return lspFailed(name, err)
		}
		an.Items = r.locItems(s, locs, d.Where)
		an.Total = len(an.Items)
		if len(an.Items) == 0 {
			an.Message = "no implementations of " + name
		}
	}
	return an
}

// withWhy replaces the generic no-server line by the server's own reason when
// it is a trust or availability refusal worth showing.
func (an lspAnswer) withWhy(why string) lspAnswer {
	if why != "" {
		an.Message += " (" + short(flat(why), 160) + ")"
	}
	return an
}

func lspFailed(name string, err error) lspAnswer {
	return lspAnswer{Name: name, Status: "error", Items: []lspItem{}, Message: "language server failed on " + name + ": " + short(flat(err.Error()), 200)}
}

// lspCut sorts hits by file and line and keeps the first limit of them.
func lspCut(items []lspItem, total, limit int) ([]lspItem, int) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		return items[i].Line < items[j].Line
	})
	var uniq []lspItem
	for _, it := range items {
		if n := len(uniq); n > 0 && uniq[n-1].Path == it.Path && uniq[n-1].Line == it.Line {
			continue
		}
		uniq = append(uniq, it)
	}
	total -= len(items) - len(uniq)
	if len(uniq) > limit {
		uniq = uniq[:limit]
	}
	return uniq, max(total, len(uniq))
}

// rel shows an absolute path of the checkout (however its symlinks are
// spelled) repo-relative; anything else is returned as it came.
func (r *lspRun) rel(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	for _, root := range []string{r.e.Code, evalOr(r.e.Code)} {
		if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return p
}

func (r *lspRun) lineText(path string, ln int) string {
	if ls := r.e.Lines(path); ln >= 1 && ln <= len(ls) {
		return short(strings.TrimSpace(ls[ln-1]), 140)
	}
	return ""
}

// locItems turns server locations into hits, leaving out the definition itself.
func (r *lspRun) locItems(s *lspSession, locs []lspLoc, except string) []lspItem {
	var out []lspItem
	for _, l := range locs {
		p := s.locOf(r.e.Code, l.URI)
		n := l.Range.Start.Line + 1
		if fmt.Sprintf("%s:%d", p, n) == except || r.e.Skip[p] || strings.Contains(p, "/node_modules/") {
			continue
		}
		out = append(out, lspItem{Path: p, Line: n, Text: r.lineText(p, n)})
	}
	return out
}

// session starts (or reuses) the server for the language and project root of
// a repo-relative file.
func (r *lspRun) session(path string) (*lspSession, string) {
	e := r.e
	l := langOf(path)
	if l == nil {
		return nil, "unknown language"
	}
	root, _ := rootFor(e.Code, path, l.ID)
	dir := filepath.Join(e.Code, filepath.FromSlash(strings.TrimSuffix(root.Dir, "/")))
	key := l.ID + "\x00" + dir
	if s, ok := r.sessions[key]; ok {
		return s, r.whys[key]
	}
	var s *lspSession
	var why string
	switch l.Special {
	case "gopls":
		s, why = r.openGopls(dir)
	case "ts":
		s, why = r.openTS(dir)
	default:
		s, why = openLSP(e.Code, l, dir)
	}
	r.sessions[key], r.whys[key] = s, why
	return s, why
}

func lspHandshake(c *lspClient, dir string) (lspCaps, error) {
	raw, err := c.call("initialize", map[string]any{
		"processId": os.Getpid(), "rootUri": fileURI(dir), "rootPath": dir,
		"workspaceFolders": []map[string]string{{"uri": fileURI(dir), "name": filepath.Base(dir)}},
		"clientInfo":       map[string]any{"name": "orch"}, "capabilities": tsClientCapabilities,
	}, tsRequestLimit)
	if err != nil {
		return lspCaps{}, err
	}
	c.notify("initialized", map[string]any{})
	return parseCaps(raw), nil
}

func (r *lspRun) openGopls(dir string) (*lspSession, string) {
	if r.e.Gopls == "" {
		return nil, "gopls is not trusted for this checkout"
	}
	if _, err := exec.LookPath(r.e.Gopls); err != nil {
		return nil, r.e.Gopls + " was not found"
	}
	c, err := startLSP(r.e.Gopls, dir, "serve")
	if err != nil {
		return nil, err.Error()
	}
	caps, err := lspHandshake(c, dir)
	if err != nil {
		c.close()
		return nil, err.Error()
	}
	return &lspSession{c: c, caps: caps, bin: "gopls"}, ""
}

func (r *lspRun) openTS(dir string) (*lspSession, string) {
	bin, why := tsServerChoice(r.e.Code)
	if bin == "" {
		return nil, orDefault(why, "no native TypeScript 7 server found")
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, bin + " was not found"
	}
	c, err := startLSP(bin, dir, "--lsp", "--stdio")
	if err != nil {
		return nil, err.Error()
	}
	caps, err := lspHandshake(c, dir)
	if err != nil {
		c.close()
		return nil, err.Error()
	}
	if src := firstSource(dir); src != "" {
		c.open(src)
	}
	return &lspSession{c: c, caps: caps, bin: filepath.Base(bin), ts: true}, ""
}

// target opens the server for a definition and builds the position of its name.
func (r *lspRun) target(path string, ln int, base string) (*lspSession, map[string]any, string) {
	s, why := r.session(path)
	if s == nil {
		return nil, nil, why
	}
	abs := filepath.Join(r.e.Code, filepath.FromSlash(path))
	s.c.open(abs)
	col := 0
	if ls := r.e.Lines(path); ln >= 1 && ln <= len(ls) {
		col = max(nameIndex(ls[ln-1], base), 0)
	}
	return s, lspPos(abs, ln-1, col), ""
}

func lspPos(abs string, line, col int) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": fileURI(abs)}, "position": map[string]any{"line": line, "character": col}}
}

// ask is a request whose empty answer, on the TypeScript server, is asked
// again for a while: it answers empty until its project has loaded.
func (s *lspSession) ask(method string, params any) (json.RawMessage, error) {
	deadline := time.Now().Add(tsReady)
	for {
		raw, err := s.c.call(method, params, tsRequestLimit)
		if err != nil {
			return nil, err
		}
		if !lspEmpty(raw) {
			s.ready = true
			return raw, nil
		}
		if !s.ts || s.ready || !time.Now().Before(deadline) {
			return raw, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func lspEmpty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == "[]"
}

// nameIndex is the column of a declaration's name. In a Go method the receiver
// comes first, and its type may share the method's name (func (s *Save) Save),
// so the search starts after the receiver.
func nameIndex(line, base string) int {
	if strings.HasPrefix(strings.TrimSpace(line), "func (") {
		if j := strings.Index(line, ")"); j >= 0 {
			if k := wordIndex(line[j+1:], base); k >= 0 {
				return j + 1 + k
			}
		}
	}
	return wordIndex(line, base)
}

// wordIndex is the index of base in line as a whole identifier, or -1.
func wordIndex(line, base string) int {
	isID := func(b byte) bool {
		return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
	}
	for from := 0; from <= len(line); {
		i := strings.Index(line[from:], base)
		if i < 0 || base == "" {
			return -1
		}
		i += from
		end := i + len(base)
		if (i == 0 || !isID(line[i-1])) && (end >= len(line) || !isID(line[end])) {
			return i
		}
		from = i + 1
	}
	return -1
}

type lspRange struct {
	Start struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"start"`
}

type lspDocSym struct {
	Name           string    `json:"name"`
	Kind           int       `json:"kind"`
	SelectionRange *lspRange `json:"selectionRange"`
	Range          *lspRange `json:"range"`
	Location       *struct {
		Range lspRange `json:"range"`
	} `json:"location"`
	Children []lspDocSym `json:"children"`
}

var lspKindNames = map[int]string{1: "File", 2: "Module", 3: "Namespace", 4: "Package", 5: "Class", 6: "Method", 7: "Property", 8: "Field",
	9: "Constructor", 10: "Enum", 11: "Interface", 12: "Function", 13: "Variable", 14: "Constant", 15: "String", 16: "Number",
	17: "Boolean", 18: "Array", 19: "Object", 20: "Key", 21: "Null", 22: "EnumMember", 23: "Struct", 24: "Event", 25: "Operator", 26: "TypeParameter"}

// repoFile checks that p names a file inside the checkout and returns it
// repo-relative.
func (r *lspRun) repoFile(p string) (string, *lspAnswer) {
	fail := func(msg string) (string, *lspAnswer) {
		return "", &lspAnswer{Name: p, Status: "error", Message: msg, Items: []lspItem{}}
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(evalOr(r.e.Code), evalOr(p))
		if err != nil {
			return fail(p + ": outside the repository")
		}
		p = rel
	}
	rp, ok := repoPath(p)
	if !ok || !ctxWithinRoot(r.e.Code, rp) {
		return fail(p + ": not a file inside the repository")
	}
	return rp, nil
}

func (r *lspRun) symbols(arg string) lspAnswer {
	path, bad := r.repoFile(arg)
	if bad != nil {
		return *bad
	}
	an := lspAnswer{Name: path, Status: "ok", Items: []lspItem{}}
	s, why := r.session(path)
	if s == nil {
		lang := "this file"
		if l := langOf(path); l != nil {
			lang = l.ID
		}
		return noServer(lang, path).withWhy(why)
	}
	if !s.caps.DocSym {
		return lspAnswer{Name: path, Status: "no_server", Items: []lspItem{}, Message: fmt.Sprintf("%s has no document symbols; use rg -n -w %s", s.bin, path)}
	}
	abs := filepath.Join(r.e.Code, filepath.FromSlash(path))
	s.c.open(abs)
	raw, err := s.ask("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": fileURI(abs)}})
	if err != nil {
		return lspFailed(path, err)
	}
	var syms []lspDocSym
	if err := json.Unmarshal(raw, &syms); err != nil {
		return lspFailed(path, err)
	}
	var walk func(list []lspDocSym, depth int)
	walk = func(list []lspDocSym, depth int) {
		for _, d := range list {
			line := 0
			switch {
			case d.SelectionRange != nil:
				line = d.SelectionRange.Start.Line
			case d.Range != nil:
				line = d.Range.Start.Line
			case d.Location != nil:
				line = d.Location.Range.Start.Line
			}
			kind := lspKindNames[d.Kind]
			if kind == "" {
				kind = "Symbol"
			}
			an.Items = append(an.Items, lspItem{Kind: kind, Name: d.Name, Line: line + 1, Depth: depth})
			walk(d.Children, depth+1)
		}
	}
	walk(syms, 0)
	if len(an.Items) == 0 {
		an.Message = "no symbols in " + path
	}
	return an
}

func (r *lspRun) hover(arg string) lspAnswer {
	p, ln, col, ok := parsePos(arg)
	if !ok || ln < 1 || col < 1 {
		return lspAnswer{Name: arg, Status: "error", Items: []lspItem{}, Message: "hover needs path:line:col with 1-based line and column, got " + arg}
	}
	path, bad := r.repoFile(p)
	if bad != nil {
		return *bad
	}
	an := lspAnswer{Name: arg, Status: "ok", Items: []lspItem{}}
	s, why := r.session(path)
	if s == nil {
		lang := "this file"
		if l := langOf(path); l != nil {
			lang = l.ID
		}
		return noServer(lang, path).withWhy(why)
	}
	abs := filepath.Join(r.e.Code, filepath.FromSlash(path))
	s.c.open(abs)
	raw, err := s.ask("textDocument/hover", lspPos(abs, ln-1, col-1))
	if err != nil {
		return lspFailed(arg, err)
	}
	text := hoverText(raw)
	if text == "" {
		an.Message = "no hover information at " + arg
		return an
	}
	an.Items = append(an.Items, lspItem{Path: path, Line: ln, Text: text})
	return an
}

// hoverText flattens a hover result (a string, a MarkupContent, a marked
// string or a list of them) into compact text without code fences.
func hoverText(raw json.RawMessage) string {
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(raw, &h) != nil {
		return ""
	}
	var parts []string
	var add func(json.RawMessage)
	add = func(c json.RawMessage) {
		var s string
		if json.Unmarshal(c, &s) == nil {
			parts = append(parts, s)
			return
		}
		var arr []json.RawMessage
		if json.Unmarshal(c, &arr) == nil {
			for _, x := range arr {
				add(x)
			}
			return
		}
		var o struct {
			Value string `json:"value"`
		}
		if json.Unmarshal(c, &o) == nil {
			parts = append(parts, o.Value)
		}
	}
	add(h.Contents)
	var lines []string
	for _, line := range strings.Split(strings.Join(parts, "\n"), "\n") {
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "```") {
			continue
		}
		lines = append(lines, strings.TrimRight(line, " \t\r"))
	}
	if len(lines) > 30 {
		lines = append(lines[:30], "...")
	}
	return strings.Join(lines, "\n")
}
