package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Go symbol facts for question cards, from gopls: where a symbol the question
// names is defined, who calls it outside tests, how many tests reference it
// and, for an interface method, what implements it. The facts are plain
// output of gopls with no judgement.

const (
	goplsParallel = 4
	goplsMaxSyms  = 3 // symbols taken from one question
	goplsMaxDefs  = 4
	goplsMaxLines = 6  // callers and implementers shown on a question card
	ctxMaxRefs    = 50 // callers, test references and implementers kept per definition
	goplsMaxFails = 3
	goplsMinCamel = 6
)

// goplsLimit bounds one gopls call; a variable so tests can shorten it.
var goplsLimit = 45 * time.Second

// goplsCommon are CamelCase words that are not Go symbols.
var goplsCommon = map[string]bool{
	"PostgreSQL": true, "GitHub": true, "GitLab": true, "JavaScript": true, "TypeScript": true, "OpenAPI": true,
	"WebSocket": true, "WebSockets": true, "OAuth": true, "GraphQL": true, "YouTube": true, "LinkedIn": true,
	"CamelCase": true, "ReadMe": true, "HashiCorp": true, "MySQL": true, "SQLite": true, "MariaDB": true,
}

var (
	symDotted = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*\.[A-Z][A-Za-z0-9]*\b`)
	symCamel  = regexp.MustCompile(`\b[A-Z][a-z0-9]+(?:[A-Z][A-Za-z0-9]*)+\b|\b[a-z]+(?:[A-Z][a-z0-9]*)+[A-Za-z0-9]*\b`)
	symIdent  = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)?$`)
	fileExtRe = regexp.MustCompile(`\.(go|sql|md|ts|tsx|js|json|ya?ml|toml|html|css|txt|sh)$`)
)

// goSymbols lists up to goplsMaxSyms Go-looking names in a question: dotted
// pkg.Func or Type.Method forms, CamelCase words of six or more characters and
// backticked names. Paths, file names and snake_case words are not symbols.
func goSymbols(q string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		last := s[strings.LastIndex(s, ".")+1:]
		switch {
		case s == "" || seen[s] || goplsCommon[last] || fileExtRe.MatchString(s):
			return
		case strings.Contains(s, "/") || !symIdent.MatchString(s):
			return
		case s == strings.ToLower(s) && !strings.Contains(s, "."): // a SQL name or an ordinary word
			return
		case !strings.Contains(s, ".") && len(s) < goplsMinCamel:
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	type found struct {
		at int
		s  string
	}
	var all []found
	for _, m := range idBacktick.FindAllStringSubmatchIndex(q, -1) {
		span := q[m[2]:m[3]]
		if i := strings.Index(span, "("); i > 0 {
			span = span[:i]
		}
		all = append(all, found{m[0], span})
	}
	for _, m := range symDotted.FindAllStringIndex(q, -1) {
		all = append(all, found{m[0], q[m[0]:m[1]]})
	}
	for _, m := range symCamel.FindAllStringIndex(q, -1) {
		all = append(all, found{m[0], q[m[0]:m[1]]})
	}
	// By position in the question, so the cap keeps the first ones mentioned.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].at < all[j-1].at; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	for _, f := range all {
		// A dotted form whose parts are also found alone adds nothing: keep the dotted one.
		add(f.s)
	}
	// Drop a bare name that a dotted symbol already names (Gateway.StartCheckout and StartCheckout).
	var kept []string
	for _, s := range out {
		dup := false
		if !strings.Contains(s, ".") {
			for _, o := range out {
				if strings.HasSuffix(o, "."+s) || strings.HasPrefix(o, s+".") {
					dup = true
				}
			}
		}
		if !dup {
			kept = append(kept, s)
		}
	}
	if len(kept) > goplsMaxSyms {
		kept = kept[:goplsMaxSyms]
	}
	return kept
}

type symDef struct {
	Match       string   `json:"match"` // exact or candidate; candidate never proves qualified identity
	Errors      []string `json:"errors,omitempty"`
	RefsUnknown bool     `json:"refs_unknown,omitempty"`
	Where       string   // rel:line
	Kind        string
	Callers     []string
	Total       int // call sites outside tests
	Tests       int
	TestRefs    []string // test references (up to ctxMaxRefs)
	Impls       []string
	Func        bool // references were looked up
	ByName      bool // callers are word matches by name, not a call graph
	GenHidden   int  // callers left out because they are in generated files
}

type symFact struct {
	Errors []string `json:"errors,omitempty"`
	Symbol string
	Lang   string // a language id: go, typescript, python, ...
	Tier   string // what answered: "gopls", "typescript server", "<server> (refs/impl), declarations by rg", "declaration scan (rg)"
	Defs   []symDef
}

func goplsBin() string {
	if p := os.Getenv("ORCH_GOPLS"); p != "" {
		return p
	}
	if p := expandHome("~/.local/bin/gopls"); fileExists(p) {
		return p
	}
	return "gopls"
}

type goplsRunner struct {
	bin, dir, cacheDir string
	root               string // the repo, for paths relative to it; dir when empty
	mu                 sync.Mutex
	fails              int
	note               string
}

// run executes one gopls command in the checkout, from the cache when the same
// call was made before. Only successful calls are cached.
func (g *goplsRunner) run(args ...string) (string, error) {
	g.mu.Lock()
	stop := g.fails >= goplsMaxFails
	g.mu.Unlock()
	if stop {
		return "", fmt.Errorf("gopls skipped after repeated failures")
	}
	sum := sha256.Sum256([]byte(g.dir + "\x00" + strings.Join(args, "\x00")))
	cache := filepath.Join(g.cacheDir, fmt.Sprintf("gopls-v2-%x.json", sum[:8]))
	if b, err := os.ReadFile(cache); err == nil {
		var out string
		if json.Unmarshal(b, &out) == nil && validGoplsOutput(args[0], out) == nil {
			return out, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), goplsLimit)
	defer cancel()
	bin, err := ctxServerBinary(orDefault(g.root, g.dir), g.bin)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = g.dir
	stderr := &ctxBoundedBuffer{limit: 64 << 10}
	cmd.Stderr = stderr
	outBuf := &ctxBoundedBuffer{limit: 16 << 20}
	cmd.Stdout = outBuf
	err = cmd.Run()
	out := outBuf.Bytes()
	if err == nil && outBuf.cut {
		return "", fmt.Errorf("gopls %s: response exceeded 16 MiB; narrow the symbol lookup", args[0])
	}
	if err != nil {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.fails++
		if g.note == "" {
			if ctx.Err() != nil {
				g.note = fmt.Sprintf("gopls timed out after %s on `%s`", goplsLimit, strings.Join(args, " "))
			} else {
				g.note = fmt.Sprintf("gopls failed on `%s`: %v: %s", strings.Join(args, " "), err, short(strings.TrimSpace(stderr.String()), 160))
			}
		}
		return "", fmt.Errorf("%s", g.note)
	}
	if err := validGoplsOutput(args[0], string(out)); err != nil {
		return "", err
	}
	ctxCacheWrite(cache, string(out))
	return string(out), nil
}

// goplsPos is "path:line:col-col" or "path:line:col"; it returns the path,
// the line and the start column.
var goplsPosRe = regexp.MustCompile(`^(.+?):(\d+):(\d+)(?:-\d+)?$`)

func parsePos(s string) (path string, line, col int, ok bool) {
	m := goplsPosRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0, 0, false
	}
	line, _ = strconv.Atoi(m[2])
	col, _ = strconv.Atoi(m[3])
	return m[1], line, col, true
}

// rel shows a path from gopls relative to the checkout.
func (g *goplsRunner) rel(p string) string {
	base := g.root
	if base == "" {
		base = g.dir
	}
	for _, root := range []string{base, evalOr(base)} {
		if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
	}
	return p
}

func evalOr(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// symbolFacts looks one symbol up: its exact definitions, and for a function
// or method its call sites and test references, and for an interface method
// its implementations.
func (g *goplsRunner) symbolFacts(sym string) (symFact, error) {
	f := symFact{Symbol: sym, Lang: "go"}
	typ, name := "", sym
	if i := strings.LastIndex(sym, "."); i >= 0 {
		typ, name = sym[:i], sym[i+1:]
	}
	// A package-qualified name is searched as such: a plain common name like
	// Service fills gopls' result cap with other packages.
	query := name
	if typ != "" && typ[0] >= 'a' && typ[0] <= 'z' {
		query = sym
	}
	out, err := g.run("workspace_symbol", query)
	if err != nil {
		return f, err
	}
	for _, line := range strings.Split(out, "\n") {
		pos, qname, kind, ok := parseGoplsSymbol(line)
		if !ok {
			continue
		}
		last := qname[strings.LastIndex(qname, ".")+1:]
		if last != name {
			continue
		}
		// A capitalised first part is a type that must be in the qualified name.
		if typ != "" && typ[0] >= 'A' && typ[0] <= 'Z' && qname != sym && !strings.HasSuffix(qname, "."+sym) {
			continue
		}
		path, ln, col, ok := parsePos(pos)
		if !ok {
			continue
		}
		match := "exact"
		if typ == "" && kind == "Method" {
			match = "candidate"
		}
		if typ != "" && typ[0] >= 'a' && typ[0] <= 'z' {
			pkg, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
			if err != nil || pkg.Name.Name != typ {
				continue
			}
			if kind == "Method" {
				match = "candidate"
			}
		}
		d := symDef{Where: fmt.Sprintf("%s:%d", g.rel(path), ln), Kind: kind, Match: match}
		if kind == "Function" || kind == "Method" {
			d.Func = true
			at := fmt.Sprintf("%s:%d:%d", path, ln, col)
			refs, err := g.run("references", at)
			if err != nil {
				symDefError(&d, "references", err)
			}
			for _, r := range strings.Split(refs, "\n") {
				rp, rl, rc, ok := parsePos(strings.TrimSpace(r))
				if !ok || (rp == path && rl == ln && rc == col) {
					continue
				}
				if isTestPath(g.rel(rp)) {
					d.Tests++
					if len(d.TestRefs) < ctxMaxRefs {
						d.TestRefs = append(d.TestRefs, fmt.Sprintf("%s:%d", g.rel(rp), rl))
					}
					continue
				}
				d.Total++
				if len(d.Callers) < ctxMaxRefs {
					d.Callers = append(d.Callers, fmt.Sprintf("%s:%d", g.rel(rp), rl))
				}
			}
			if kind == "Method" && isInterfaceMethod(path, ln) {
				impls, err := g.run("implementation", at)
				if err != nil {
					symDefError(&d, "implementation", err)
				}
				for _, r := range strings.Split(impls, "\n") {
					ip, il, _, ok := parsePos(strings.TrimSpace(r))
					if ok && len(d.Impls) < ctxMaxRefs {
						d.Impls = append(d.Impls, fmt.Sprintf("%s:%d", g.rel(ip), il))
					}
				}
			}
		}
		f.Defs = append(f.Defs, d)
	}
	return f, nil
}

// isInterfaceMethod reads the definition line: a method of a concrete type
// starts with func, a method of an interface is just its signature.
func isInterfaceMethod(path string, line int) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lines := strings.Split(string(b), "\n")
	return line >= 1 && line <= len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[line-1]), "func ")
}

// factLines renders the facts of one card as bullet lines.
func factLines(facts []symFact) []string {
	var out []string
	for _, f := range facts {
		if len(f.Defs) == 0 {
			if len(f.Errors) > 0 {
				out = append(out, "- symbol lookup unavailable for `"+f.Symbol+"`: "+strings.Join(f.Errors, "; "))
				continue
			}
			out = append(out, fmt.Sprintf("- no symbol named `%s` found (%s)", f.Symbol, f.Lang))
			continue
		}
		for _, d := range f.Defs {
			line := fmt.Sprintf("- `%s` (%s %s) is defined at `%s`", f.Symbol, f.Lang, d.Kind, d.Where)
			if d.Match == "candidate" {
				line += " (candidate, identity not verified)"
			}
			if d.Func && (d.RefsUnknown || d.ByName) {
				line += "; references unavailable as an exact count"
			} else if d.Func {
				if d.Total == 0 {
					line += "; no callers outside tests"
				} else {
					var cs []string
					for _, c := range d.Callers[:min(goplsMaxLines, len(d.Callers))] {
						cs = append(cs, "`"+c+"`")
					}
					line += fmt.Sprintf("; %d callers outside tests: %s", d.Total, strings.Join(cs, ", "))
					if d.Total > min(goplsMaxLines, len(d.Callers)) {
						line += ", ..."
					}
				}
				line += fmt.Sprintf("; %d test references", d.Tests)
			}
			if len(d.Impls) > 0 {
				var is []string
				for _, c := range d.Impls[:min(goplsMaxLines, len(d.Impls))] {
					is = append(is, "`"+c+"`")
				}
				line += "; implemented at " + strings.Join(is, ", ")
			}
			out = append(out, line)
		}
	}
	return out
}
