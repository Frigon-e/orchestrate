package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// The language-server tier for the languages in the registry that have no
// dedicated code (Python, Rust, C/C++, Java, Kotlin, Ruby, PHP, shell, ...).
// A symbol's definition comes from the server's workspace/symbol when its
// initialize result advertises it, else from the declaration tier (some
// servers, Pyrefly among them, accept workspace/symbol and do not honor it);
// references and implementations always come from the server. The first server
// of the language's list that completes the handshake and can answer
// references is used; with none, the declaration tier answers alone.

type lspCaps struct{ WS, Refs, Impl, Def, Calls, DocSym, Hover bool }

func capOn(v json.RawMessage) bool {
	s := strings.TrimSpace(string(v))
	return s != "" && s != "null" && s != "false"
}

func parseCaps(raw json.RawMessage) lspCaps {
	var r struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	_ = json.Unmarshal(raw, &r)
	c := r.Capabilities
	return lspCaps{WS: capOn(c["workspaceSymbolProvider"]), Refs: capOn(c["referencesProvider"]),
		Impl: capOn(c["implementationProvider"]), Def: capOn(c["definitionProvider"]),
		Calls: capOn(c["callHierarchyProvider"]), DocSym: capOn(c["documentSymbolProvider"]), Hover: capOn(c["hoverProvider"])}
}

// lspSession is a started and initialized server for one project root.
type lspSession struct {
	c    *lspClient
	caps lspCaps
	bin  string
	// ts marks the TypeScript server, which answers empty until its project has
	// loaded; ready is set by its first non-empty answer (see ask in lsp_cmd.go).
	ts, ready bool
}

// openLSP starts the first server of l's list that is found, initializes it on
// root (an absolute directory) and checks it can answer references.
func openLSP(code string, l *langDef, root string) (*lspSession, string) {
	return openServer(code, stdServer{l: l}, root, []string{root})
}

func (s *lspSession) locOf(code, uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	for _, root := range []string{code, evalOr(code)} {
		if r, err := filepath.Rel(root, u.Path); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
	}
	return u.Path
}

// refine asks the server about the definitions of a name (found by the
// declaration tier, or by the server's own workspace symbols) and returns the
// fact with callers, test references and implementers from the server.
func (s *lspSession) refine(e *codeEnv, l *langDef, f *symFact, base string) *symFact {
	defs := f.Defs
	if s.caps.WS {
		if raw, err := s.c.call("workspace/symbol", map[string]any{"query": base}, tsRequestLimit); err == nil {
			var syms []struct {
				Name     string `json:"name"`
				Kind     int    `json:"kind"`
				Location lspLoc `json:"location"`
			}
			if err := json.Unmarshal(raw, &syms); err != nil {
				f.Errors = append(f.Errors, base+": workspace/symbol: malformed result: "+err.Error())
			}
			var ws []symDef
			for _, x := range syms {
				if err := validLSPSymbol(x.Name, x.Kind, x.Location); err != nil {
					f.Errors = append(f.Errors, base+": workspace/symbol: "+err.Error())
					continue
				}
				if x.Name != base {
					continue
				}
				p := s.locOf(e.Code, x.Location.URI)
				sourceLang := langOf(p)
				if len(f.Defs) == 0 {
					continue
				}
				defPath, _ := splitWhere(f.Defs[0].Where)
				targetRoot, _ := rootFor(e.Code, defPath, l.ID)
				symbolRoot, _ := rootFor(e.Code, p, l.ID)
				if sourceLang == nil || sourceLang.ID != l.ID || targetRoot.Dir != symbolRoot.Dir {
					continue
				}
				k := tsKindName[x.Kind]
				if k == "" {
					k = "Symbol"
				}
				ws = append(ws, symDef{Where: fmt.Sprintf("%s:%d", p, x.Location.Range.Start.Line+1), Kind: k, Match: "candidate"})
			}
			if len(ws) > 0 {
				// A server may return a bounded workspace-symbol window. Keep scanned
				// declarations it did not return as candidates instead of hiding them.
				seen := map[string]bool{}
				for _, d := range ws {
					seen[d.Where] = true
				}
				for _, d := range defs {
					if !seen[d.Where] {
						ws = append(ws, d)
					}
				}
				defs = ws
			}
		} else {
			f.Errors = append(f.Errors, base+": workspace/symbol: "+err.Error())
		}
	}
	out := &symFact{Symbol: f.Symbol, Lang: l.ID, Errors: append([]string(nil), f.Errors...)}
	for _, d := range defs {
		path, ln := splitWhere(d.Where)
		abs := filepath.Join(e.Code, filepath.FromSlash(path))
		s.c.open(abs)
		line, ch := ln-1, 0
		if ls := e.Lines(path); ln >= 1 && ln <= len(ls) {
			ch = max(strings.Index(ls[ln-1], base), 0)
		}
		pos := map[string]any{"textDocument": map[string]any{"uri": fileURI(abs)}, "position": map[string]any{"line": line, "character": ch}}
		nd := symDef{Where: d.Where, Kind: d.Kind, Match: d.Match}
		if d.Kind != "Module" {
			nd.Func = true
			rr, err := s.c.call("textDocument/references", withCtx(pos, map[string]any{"includeDeclaration": false}), tsRequestLimit)
			var refs []lspLoc
			if err == nil {
				refs, err = decodeLSPLocs(rr, false)
			}
			if err != nil {
				symDefError(&nd, "references", err)
			} else {
				for _, r := range refs {
					p := s.locOf(e.Code, r.URI)
					loc := fmt.Sprintf("%s:%d", p, r.Range.Start.Line+1)
					if loc == d.Where || e.Skip[p] {
						continue
					}
					if isTestPath(p) {
						nd.Tests++
						if len(nd.TestRefs) < ctxMaxRefs {
							nd.TestRefs = append(nd.TestRefs, loc)
						}
						continue
					}
					nd.Total++
					if len(nd.Callers) < ctxMaxRefs {
						nd.Callers = append(nd.Callers, loc)
					}
				}
			}
		}
		if s.caps.Impl && (d.Kind == "Class" || d.Kind == "Interface" || d.Kind == "Method" || d.Kind == "Type") {
			rr, err := s.c.call("textDocument/implementation", pos, tsRequestLimit)
			var locs []lspLoc
			if err == nil {
				locs, err = decodeLSPLocs(rr, true)
			}
			if err != nil {
				symDefError(&nd, "implementation", err)
			} else {
				for _, r := range locs {
					p := s.locOf(e.Code, r.URI)
					if len(nd.Impls) < ctxMaxRefs && !isTestPath(p) {
						nd.Impls = append(nd.Impls, fmt.Sprintf("%s:%d", p, r.Range.Start.Line+1))
					}
				}
			}
		}
		out.Defs = append(out.Defs, nd)
	}
	how := s.bin + " (refs/impl), declarations by rg"
	if s.caps.WS {
		how = s.bin
	}
	out.Tier = how
	return out
}

func withCtx(pos map[string]any, ctx map[string]any) map[string]any {
	m := map[string]any{}
	maps.Copy(m, pos)
	m["context"] = ctx
	return m
}

type lspGroup struct {
	lang  *langDef
	root  projectRoot
	names []string
}

// lspTier refines the declaration-tier facts with each language's server. It
// returns the facts it improved, keyed by name; the others stay as they were.
func (e *codeEnv) lspTier(decl map[string]*symFact) map[string]*symFact {
	groups := map[string]*lspGroup{}
	var order []string
	for name, f := range decl {
		for _, d := range f.Defs {
			path, _ := splitWhere(d.Where)
			l := langOf(path)
			if l == nil || l.native() || len(l.LSP) == 0 {
				continue
			}
			r, _ := rootFor(e.Code, path, l.ID)
			key := l.ID + "\x00" + r.Dir
			g := groups[key]
			if g == nil {
				g = &lspGroup{lang: l, root: r}
				groups[key] = g
				order = append(order, key)
			}
			g.names = addUniq(g.names, name, 100)
		}
	}
	sort.Strings(order)
	out := map[string]*symFact{}
	var mu sync.Mutex
	for _, key := range order {
		g := groups[key]
		root := filepath.Join(e.Code, filepath.FromSlash(strings.TrimSuffix(g.root.Dir, "/")))
		// Answers cached from an earlier run need no server.
		var todo []string
		for _, n := range g.names {
			if f := e.lspCached(g, n); f != nil {
				base := out[n]
				if base == nil {
					base = decl[n]
				}
				out[n] = mergeSymGroup(base, f, e.symGroupFact(decl[n], g))
				continue
			}
			todo = append(todo, n)
		}
		if len(todo) == 0 {
			e.use("sym "+g.lang.ID, out[g.names[0]].Tier)
			continue
		}
		if _, _, _, why := langServerReason(e.Code, g.lang); why != "" {
			e.note(why)
		}
		sess, why := openLSP(e.Code, g.lang, root)
		if sess == nil {
			for _, n := range todo {
				f := e.symGroupFact(decl[n], g)
				symUnavailable(f, g.root.Dir+" ("+g.lang.ID+")", why)
				base := out[n]
				if base == nil {
					base = decl[n]
				}
				out[n] = mergeSymGroup(base, f, e.symGroupFact(decl[n], g))
			}
			e.use("sym "+g.lang.ID, "declaration scan (rg), callers by name; "+why+" ("+g.lang.Hint+")")
			continue
		}
		for _, n := range todo {
			base := n[strings.LastIndex(n, ".")+1:]
			old := e.symGroupFact(decl[n], g)
			nf := sess.refine(e, g.lang, old, base)
			mu.Lock()
			previous := out[n]
			if previous == nil {
				previous = decl[n]
			}
			out[n] = mergeSymGroup(previous, nf, old)
			mu.Unlock()
			e.lspStore(g, n, nf)
		}
		sess.c.close()
		e.use("sym "+g.lang.ID, out[todo[0]].Tier)
	}
	return out
}

func (e *codeEnv) lspKey(g *lspGroup, name string) string {
	sum := sha256.Sum256([]byte(g.lang.ID + "\x00" + g.root.Dir + "\x00" + name))
	return filepath.Join(e.CacheDir, fmt.Sprintf("lsp-v2-%x.json", sum[:8]))
}

func (e *codeEnv) lspCached(g *lspGroup, name string) *symFact {
	b, err := os.ReadFile(e.lspKey(g, name))
	if err != nil {
		return nil
	}
	var f symFact
	if !decodeSymCache(b, &f, name, g.lang.ID) {
		return nil
	}
	return &f
}

func (e *codeEnv) lspStore(g *lspGroup, name string, f *symFact) {
	if symFactValid(f, name) {
		ctxCacheWrite(e.lspKey(g, name), f)
	}
}
