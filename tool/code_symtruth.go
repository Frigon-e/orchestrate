package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Symbol work has its own bounded pool: callers may already hold e.sem.
func symParallel(n int, fn func(int)) {
	var wg sync.WaitGroup
	jobs := make(chan int)
	for i := 0; i < min(n, goplsParallel); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				fn(j)
			}
		}()
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

// symPathWithin compares path components, not directory-name prefixes.
func symPathWithin(path, root string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	root = filepath.ToSlash(filepath.Clean(root))
	return root == "." || root == "" || path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

var goplsSymbolRow = regexp.MustCompile(`^(.+?:\d+:\d+(?:-\d+)?)\s+(\S+)\s+(\S+)$`)

func parseGoplsSymbol(line string) (pos, name, kind string, ok bool) {
	m := goplsSymbolRow.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

func validGoplsOutput(op, out string) error {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		pos := strings.TrimSpace(line)
		if op == "workspace_symbol" {
			var ok bool
			pos, _, _, ok = parseGoplsSymbol(line)
			if !ok {
				return fmt.Errorf("gopls %s: malformed row %q", op, line)
			}
		}
		_, ln, col, ok := parsePos(pos)
		if !ok || ln < 1 || col < 1 {
			return fmt.Errorf("gopls %s: malformed position %q", op, line)
		}
	}
	return nil
}

func decodeSymCache(b []byte, f *symFact, name, lang string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return false
	}
	for _, key := range []string{"Symbol", "Lang", "Tier", "Defs"} {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	var defs []map[string]json.RawMessage
	if json.Unmarshal(fields["Defs"], &defs) != nil {
		return false
	}
	for _, d := range defs {
		for _, key := range []string{"Where", "Kind", "match", "Func", "ByName", "Total", "Tests", "Callers", "TestRefs", "Impls"} {
			if _, ok := d[key]; !ok {
				return false
			}
		}
	}
	if json.Unmarshal(b, f) != nil || f.Lang != lang {
		return false
	}
	return symFactValid(f, name)
}

func symFactValid(f *symFact, name string) bool {
	if f == nil || f.Symbol != name || f.Lang == "" || len(f.Errors) > 0 {
		return false
	}
	for _, d := range f.Defs {
		p, n := splitWhere(d.Where)
		if p == "" || n < 1 || (d.Match != "exact" && d.Match != "candidate") || d.Kind == "" || d.Total < len(d.Callers) || d.Tests < len(d.TestRefs) || d.RefsUnknown || len(d.Errors) > 0 || d.ByName {
			return false
		}
		for _, loc := range append(append(append([]string(nil), d.Callers...), d.TestRefs...), d.Impls...) {
			p, n := splitWhere(loc)
			if p == "" || n < 1 {
				return false
			}
		}
	}
	return true
}

func symDefError(d *symDef, op string, err error) {
	d.Errors = append(d.Errors, fmt.Sprintf("%s: %s: %v", d.Where, op, err))
	if op == "references" {
		d.RefsUnknown = true
	}
}

func symUnavailable(f *symFact, scope, why string) {
	f.Errors = append(f.Errors, scope+": "+why)
	for i := range f.Defs {
		if f.Defs[i].Func {
			symDefError(&f.Defs[i], "references", fmt.Errorf("%s", why))
		}
	}
}

func (e *codeEnv) symGroupFact(f *symFact, g *lspGroup) *symFact {
	out := &symFact{Symbol: f.Symbol, Lang: g.lang.ID, Tier: f.Tier, Errors: append([]string(nil), f.Errors...)}
	for _, d := range f.Defs {
		path, _ := splitWhere(d.Where)
		l := langOf(path)
		if l == nil || l.ID != g.lang.ID {
			continue
		}
		r, _ := rootFor(e.Code, path, l.ID)
		if r.Dir == g.root.Dir {
			out.Defs = append(out.Defs, d)
		}
	}
	return out
}

// Replace only this project's definitions; other languages/roots stay available.
func mergeSymGroup(base, group *symFact, old *symFact) *symFact {
	out := *base
	out.Defs = append([]symDef(nil), base.Defs...)
	out.Errors = append(append([]string(nil), base.Errors...), group.Errors...)
	remove := map[string]bool{}
	for _, d := range old.Defs {
		remove[d.Where] = true
	}
	var kept []symDef
	for _, d := range out.Defs {
		if !remove[d.Where] {
			kept = append(kept, d)
		}
	}
	out.Defs = append(kept, group.Defs...)
	out.Tier = group.Tier
	return &out
}

// A null result is permitted by LSP. A non-location object is not.
func decodeLSPLocs(raw json.RawMessage, single bool) ([]lspLoc, error) {
	var locs []lspLoc
	if err := json.Unmarshal(raw, &locs); err != nil {
		if !single {
			return nil, fmt.Errorf("malformed location result: %w", err)
		}
		var one lspLoc
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("malformed location result: %w", err)
		}
		locs = []lspLoc{one}
	}
	for _, l := range locs {
		if err := validLSPLoc(l); err != nil {
			return nil, err
		}
	}
	return locs, nil
}

// Require source coordinates to be present; omitted coordinates must not turn
// into a fabricated line-one location through Go's zero values.
func (l *lspLoc) UnmarshalJSON(raw []byte) error {
	var v struct {
		URI   string `json:"uri"`
		Range *struct {
			Start *struct {
				Line      *int `json:"line"`
				Character *int `json:"character"`
			} `json:"start"`
			End *struct {
				Line      *int `json:"line"`
				Character *int `json:"character"`
			} `json:"end"`
		} `json:"range"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Range == nil || v.Range.Start == nil || v.Range.End == nil || v.Range.Start.Line == nil || v.Range.Start.Character == nil || v.Range.End.Line == nil || v.Range.End.Character == nil {
		return fmt.Errorf("malformed language-server location: missing range coordinates")
	}
	l.URI = v.URI
	l.Range.Start.Line, l.Range.Start.Character = *v.Range.Start.Line, *v.Range.Start.Character
	l.Range.End.Line, l.Range.End.Character = *v.Range.End.Line, *v.Range.End.Character
	return validLSPLoc(*l)
}

func validLSPSymbol(name string, kind int, loc lspLoc) error {
	if name == "" || kind < 1 || kind > 26 {
		return fmt.Errorf("malformed language-server symbol %q (kind %d)", name, kind)
	}
	return validLSPLoc(loc)
}

func validLSPLoc(l lspLoc) error {
	u, err := url.Parse(l.URI)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || !filepath.IsAbs(u.Path) || l.Range.Start.Line < 0 || l.Range.Start.Character < 0 || l.Range.End.Line < l.Range.Start.Line || l.Range.End.Character < 0 || (l.Range.End.Line == l.Range.Start.Line && l.Range.End.Character < l.Range.Start.Character) {
		return fmt.Errorf("malformed language-server location %q", l.URI)
	}
	return nil
}
