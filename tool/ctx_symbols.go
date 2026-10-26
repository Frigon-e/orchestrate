package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Symbols looks the Go names up with gopls and the TypeScript ones (those Go
// did not find) with the TypeScript 7 server, one server for all of them. A
// missing tool leaves an availability error, never a zero-reference fact.
func (e *ctxEnv) Symbols(goNames []string, ts []tsJob) (map[string]*symFact, map[tsJob]*symFact) {
	goFacts := map[string]*symFact{}
	if len(goNames) > 0 && repoHasLang(e.Code, "go") {
		if _, err := exec.LookPath(e.Gopls); err != nil {
			why := "Go language server unavailable: " + e.Gopls + " was not found"
			e.note(why)
			for _, n := range goNames {
				goFacts[n] = &symFact{Symbol: n, Lang: "go", Errors: []string{e.Code + ": " + why}}
			}
		} else {
			e.use("gopls", e.Gopls)
			// gopls works inside a Go module: one runner per module of the repo.
			var runners []*goplsRunner
			for _, dir := range e.goModuleDirs() {
				runners = append(runners, &goplsRunner{bin: e.Gopls, dir: dir, root: e.Code, cacheDir: e.CacheDir})
			}
			var mu sync.Mutex
			symParallel(len(goNames), func(i int) {
				merged := symFact{Symbol: goNames[i], Lang: "go", Tier: "gopls"}
				got := false
				for _, g := range runners {
					f, err := g.symbolFacts(goNames[i])
					if err != nil {
						merged.Errors = append(merged.Errors, g.dir+": workspace/symbol: "+err.Error())
						continue
					}
					got = true
					merged.Defs = append(merged.Defs, f.Defs...)
				}
				if got || len(merged.Errors) > 0 {
					mu.Lock()
					goFacts[goNames[i]] = &merged
					mu.Unlock()
				}
			})
			for _, g := range runners {
				if g.note != "" {
					e.note(g.note)
				}
			}
		}
	}
	var jobs []tsJob
	roots := map[string]int{}
	for _, j := range ts {
		if f := goFacts[j.sym]; f != nil && len(f.Defs) > 0 {
			continue
		}
		jobs = append(jobs, j)
		roots[j.root]++
	}
	var tsFacts map[tsJob]*symFact
	if len(jobs) > 0 {
		if _, why := tsServerChoice(e.Code); why != "" {
			e.note(why)
		}
		var note string
		tsFacts, note = tsFactsFor(jobs, e.CacheDir, e.Code, roots)
		if note != "" {
			e.note(note)
			if tsFacts == nil {
				tsFacts = map[tsJob]*symFact{}
			}
			for _, j := range jobs {
				if tsFacts[j] == nil {
					tsFacts[j] = &symFact{Symbol: j.sym, Lang: "typescript", Errors: []string{j.root + ": " + note}}
				}
			}
		} else if bin := tslsBin(e.Code); bin != "" {
			e.use("ts server", bin)
		}
	}
	// Names neither gopls nor the TypeScript server resolved: the declaration
	// tier verifies them, and a language server refines what it confirmed.
	tsHas := func(n string) bool {
		for j, f := range tsFacts {
			if j.sym == n && f != nil && len(f.Defs) > 0 {
				return true
			}
		}
		return false
	}
	var rest []string
	for _, n := range goNames {
		if f := goFacts[n]; (f == nil || len(f.Defs) == 0) && !tsHas(n) {
			rest = append(rest, n)
		}
	}
	if len(rest) > 0 {
		e.otherSymbols(rest, goFacts)
	}
	// A declaration fallback must retain the backend's availability failures.
	for n, f := range goFacts {
		if f.Tier != "declaration scan (rg)" {
			continue
		}
		for j, tf := range tsFacts {
			if j.sym == n && tf != nil {
				f.Errors = append(f.Errors, tf.Errors...)
			}
		}
		if len(f.Errors) > 0 {
			for i := range f.Defs {
				if f.Defs[i].Func && f.Defs[i].ByName {
					symDefError(&f.Defs[i], "references", fmt.Errorf("%s", strings.Join(f.Errors, "; ")))
				}
			}
		}
	}
	// If no tier found a definition, an empty answer from another language
	// cannot erase an unavailable backend.
	for n, gf := range goFacts {
		if len(gf.Defs) > 0 {
			continue
		}
		for j, tf := range tsFacts {
			if j.sym != n || tf == nil || len(tf.Defs) > 0 {
				continue
			}
			errs := append(append([]string(nil), gf.Errors...), tf.Errors...)
			gf.Errors = append([]string(nil), errs...)
			tf.Errors = append([]string(nil), errs...)
		}
	}
	return goFacts, tsFacts
}

// otherSymbols resolves names by tier b (declarations by rg, any language of
// the registry) and tier a for the languages without dedicated code (their
// language server). A name no declaration form matches gets no fact.
func (e *ctxEnv) otherSymbols(names []string, out map[string]*symFact) {
	decl := map[string]*symFact{}
	var mu sync.Mutex
	symParallel(len(names), func(i int) {
		if f := e.declFacts(names[i]); f != nil {
			mu.Lock()
			decl[names[i]] = f
			mu.Unlock()
		}
	})
	for _, f := range decl {
		e.use("sym "+f.Lang, "declaration scan (rg), callers by name, not a call graph")
	}
	refined := e.lspTier(decl)
	for n, f := range decl {
		if r := refined[n]; r != nil {
			f = r
		}
		if prev := out[n]; prev != nil {
			f.Errors = append(f.Errors, prev.Errors...)
		}
		out[n] = f
	}
}

// Sym looks one name up: gopls first, then the TypeScript server for a plain
// identifier. The result has the definitions found, or none.
func (e *ctxEnv) Sym(name string) symFact {
	var ts []tsJob
	if root := tsProjectRoot(e.Code, name, nil); root != "" && tsPlain.MatchString(name) {
		ts = append(ts, tsJob{root, name})
	}
	var goNames []string
	if symIdent.MatchString(name) {
		goNames = append(goNames, name)
	}
	g, t := e.Symbols(goNames, ts)
	if f := g[name]; f != nil && len(f.Defs) > 0 {
		return *f
	}
	for _, f := range t {
		if f != nil && len(f.Defs) > 0 {
			return *f
		}
	}
	if f := g[name]; f != nil {
		return *f
	}
	for _, f := range t {
		if f != nil {
			return *f
		}
	}
	return symFact{Symbol: name}
}

// sheetOpts say how much of a definition's references a sheet line shows.
type sheetOpts struct {
	Callers int  // callers listed (0: only the counts)
	Impls   bool // implementers listed
}

// sheetLines renders one line per definition of f: kind, location, signature,
// the first line of its doc comment and how it is used. A symbol with no
// definition gets a "no symbol named" line.
func (e *ctxEnv) sheetLines(f symFact, o sheetOpts) []string {
	if len(f.Defs) == 0 {
		if len(f.Errors) > 0 {
			return []string{"- symbol lookup unavailable for `" + f.Symbol + "`: " + strings.Join(f.Errors, "; ")}
		}
		return []string{fmt.Sprintf("- no symbol named `%s` found (%s)", f.Symbol, orDefault(f.Lang, "go and typescript"))}
	}
	var out []string
	for _, df := range f.Defs {
		line := fmt.Sprintf("- `%s` %s at `%s`", f.Symbol, df.Kind, df.Where)
		path, ln := splitWhere(df.Where)
		if ls := e.Lines(path); ln >= 1 && ln <= len(ls) {
			if sig := short(strings.TrimSpace(ls[ln-1]), 140); sig != "" {
				line += ": `" + sig + "`"
			}
			if doc := docLine(ls, ln); doc != "" {
				line += "; doc: " + doc
			}
		}
		if df.Match == "candidate" {
			line += "; candidate (identity not verified)"
		}
		if df.Func && (df.RefsUnknown || df.ByName) {
			line += "; references unavailable as an exact count"
		} else if df.Func {
			switch {
			case df.Total == 0:
				line += "; no callers outside tests"
			case o.Callers > 0:
				line += fmt.Sprintf("; %d callers outside tests: %s", df.Total, quoteList(df.Callers[:min(o.Callers, len(df.Callers))]))
				if df.Total > o.Callers {
					line += ", ..."
				}
			default:
				line += fmt.Sprintf("; %d callers outside tests", df.Total)
			}
			line += fmt.Sprintf("; %d test references", df.Tests)
		}
		if o.Impls && len(df.Impls) > 0 {
			line += "; implemented at " + quoteList(df.Impls)
		}
		out = append(out, line)
	}
	return out
}

// goModuleDirs lists the directories of the repo's Go project roots (a go.work
// parent stands for its modules); the repo itself when it has none.
func (e *ctxEnv) goModuleDirs() []string {
	var dirs []string
	for _, r := range rootsOf(e.Code, "go") {
		dirs = append(dirs, filepath.Join(e.Code, filepath.FromSlash(strings.TrimSuffix(r.Dir, "/"))))
	}
	if len(dirs) == 0 {
		return []string{e.Code}
	}
	return dirs
}
