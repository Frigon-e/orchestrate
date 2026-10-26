package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCtxSymbolMissingServerIsNotZeroReferences(t *testing.T) {
	c := newCtxHarness(t)
	rg, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg required")
	}
	writeTree(t, c.repo, map[string]string{"pyproject.toml": "[project]\nname = 'truth'\n", "cart.py": "class Cart:\n    def add(self, item):\n        return item\n"})
	t.Setenv("ORCH_RG", rg)
	t.Setenv("ORCH_PYLS", filepath.Join(t.TempDir(), "missing-pyls"))
	c.git("add", ".")
	c.git("commit", "-qm", "python")
	out := c.must("ctx", "sym", "Cart", "--json")
	var env struct {
		Result ctxView `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err, out)
	}
	if env.Result.Status != "partial" || env.Result.CountKind == "exact" || len(env.Result.Errors) == 0 || strings.Contains(out, "used by 0") || strings.Contains(out, "none in tests") {
		t.Fatalf("missing server became zero references: %s", out)
	}
}

func TestCtxSymbolFifthScopedDefinitionAndReceiver(t *testing.T) {
	code := pyFixture(t)
	for _, dir := range []string{"aa", "ab", "ac", "ad", "ae", "aextra"} {
		writeTree(t, code, map[string]string{dir + "/item.py": "class Real:\n    def refresh_cart(self):\n        pass\n"})
	}
	t.Setenv("ORCH_PYLS", filepath.Join(t.TempDir(), "missing"))
	e := newCtxEnv(code, ctxOpts{})
	f := e.Sym("Wrong.refresh_cart")
	v := e.symView(f, symOpts{In: "ae"})
	v.finishTruth()
	if v.Shown != 1 {
		t.Fatalf("fifth scoped definition hidden: %+v fact=%+v", v, f)
	}
	b, _ := json.Marshal(f)
	if !strings.Contains(string(b), `"match":"candidate"`) || strings.Contains(string(b), `"match":"exact"`) {
		t.Fatalf("wrong receiver became exact: %s", b)
	}
	if v = e.symView(f, symOpts{In: "a"}); v.Shown != 0 {
		t.Fatalf("path prefix crossed a component: %+v", v)
	}
}

// Pipe-backed client exercises framing and calls without fixture-server shortcuts.
func truthClient(t *testing.T, answer func(string) any) *lspClient {
	t.Helper()
	requests, in := io.Pipe()
	out, responses := io.Pipe()
	c := &lspClient{in: in, waits: map[int]chan lspReply{}, opened: map[string]bool{}, dead: make(chan struct{})}
	go c.read(bufio.NewReader(out))
	go func() {
		defer responses.Close()
		r := bufio.NewReader(requests)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			var n int
			if _, err = fmt.Sscanf(strings.TrimSpace(line), "Content-Length: %d", &n); err != nil {
				return
			}
			if _, err = r.ReadString('\n'); err != nil {
				return
			}
			b := make([]byte, n)
			if _, err = io.ReadFull(r, b); err != nil {
				return
			}
			var m struct {
				ID     int
				Method string
			}
			if json.Unmarshal(b, &m) != nil {
				return
			}
			v := answer(m.Method)
			if m.ID == 0 {
				continue
			}
			reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
			if e, ok := v.(error); ok {
				reply["error"] = map[string]any{"code": -32603, "message": e.Error()}
			} else {
				reply["result"] = v
			}
			body, _ := json.Marshal(reply)
			fmt.Fprintf(responses, "Content-Length: %d\r\n\r\n%s", len(body), body)
		}
	}()
	t.Cleanup(func() { in.Close(); out.Close(); requests.Close(); responses.Close() })
	return c
}

func TestTSFactsErrorsAndMalformedResultsAreNotEmptyOrCached(t *testing.T) {
	for _, mode := range []string{"rpc", "malformed_symbols", "malformed_refs", "invalid_location", "missing_coordinates"} {
		t.Run(mode, func(t *testing.T) {
			code := evalOr(t.TempDir())
			writeTree(t, code, map[string]string{"app.ts": "export function RefreshCart() {}\n"})
			loc := map[string]any{"uri": fileURI(filepath.Join(code, "app.ts")), "range": map[string]any{"start": map[string]int{"line": 0, "character": 0}, "end": map[string]int{"line": 0, "character": 30}}}
			c := truthClient(t, func(method string) any {
				if mode == "rpc" {
					return fmt.Errorf("backend exploded")
				}
				if method == "workspace/symbol" {
					if mode == "malformed_symbols" {
						return map[string]any{"unexpected": "object"}
					}
					if mode == "invalid_location" {
						return []any{map[string]any{"name": "RefreshCart", "kind": 12, "location": map[string]any{}}}
					}
					if mode == "missing_coordinates" {
						return []any{map[string]any{"name": "RefreshCart", "kind": 12, "location": map[string]any{"uri": fileURI(filepath.Join(code, "app.ts"))}}}
					}
					return []any{map[string]any{"name": "RefreshCart", "kind": 12, "location": loc}}
				}
				return map[string]any{"unexpected": "object"}
			})
			f, err := tsFacts(c, code, "RefreshCart")
			if err == nil && (len(f.Defs) == 0 || !f.Defs[0].RefsUnknown) {
				t.Fatalf("malformed backend accepted: %+v %v", f, err)
			}
			if err == nil && symFactValid(&f, "RefreshCart") {
				t.Fatal("failed reference fact is cacheable")
			}
			if err == nil {
				v := newCtxEnv(code, ctxOpts{}).symView(f, symOpts{})
				if v.Status != "partial" || v.CountKind != "unknown" || len(v.Errors) == 0 {
					t.Fatalf("availability lost: %+v", v)
				}
			}
		})
	}
}

func TestCtxSymbolCacheValidationAndFailedLookupRetry(t *testing.T) {
	code := pyFixture(t)
	e := newCtxEnv(code, ctxOpts{})
	g := &lspGroup{lang: langByID("python"), root: projectRoot{}}
	f := &symFact{Symbol: "build_cart", Lang: "python", Defs: []symDef{{Where: "shop/cart.py:6", Kind: "Function", Match: "candidate", Func: true, RefsUnknown: true, Errors: []string{"shop/cart.py:6: references: timeout"}}}}
	e.lspStore(g, f.Symbol, f)
	if _, err := os.Stat(e.lspKey(g, f.Symbol)); !os.IsNotExist(err) {
		t.Fatalf("failed fact was cached: %v", err)
	}
	for _, bad := range []string{`{}`, `{"Symbol":"other","Lang":"python"}`, `{"Symbol":"build_cart","Lang":"python"}`, `{"Symbol":"build_cart","Lang":"python","Defs":[{"Where":"bad","match":"exact"}]}`} {
		os.MkdirAll(e.CacheDir, 0755)
		os.WriteFile(e.lspKey(g, f.Symbol), []byte(bad), 0644)
		if e.lspCached(g, f.Symbol) != nil {
			t.Fatalf("corrupt cache accepted: %s", bad)
		}
	}
	server := filepath.Join(t.TempDir(), "gopls")
	marker := filepath.Join(t.TempDir(), "retry")
	os.WriteFile(server, []byte("#!/bin/sh\nif [ ! -f '"+marker+"' ]; then echo retry > '"+marker+"'; echo malformed; else echo '"+filepath.Join(code, "shop/cart.py")+":6:5 RefreshCart Function'; fi\n"), 0755)
	r := &goplsRunner{bin: server, dir: code, cacheDir: t.TempDir()}
	if _, err := r.symbolFacts("RefreshCart"); err == nil {
		t.Fatal("malformed gopls output accepted")
	}
	f2, err := r.symbolFacts("RefreshCart")
	if err != nil || len(f2.Defs) != 1 {
		t.Fatalf("failed lookup poisoned retry: %+v %v", f2, err)
	}
}

func TestPythonFailedReferencesArePartialAndRetryable(t *testing.T) {
	code := pyFixture(t)
	e := newCtxEnv(code, ctxOpts{})
	l := langByID("python")
	f := e.declFacts("build_cart")
	if f == nil {
		t.Fatal("missing fixture")
	}
	g := &lspGroup{lang: l}
	c := truthClient(t, func(string) any { return fmt.Errorf("references failed") })
	failed := (&lspSession{c: c, caps: lspCaps{Refs: true}, bin: "probe"}).refine(e, l, f, "build_cart")
	v := e.symView(*failed, symOpts{})
	if v.Status != "partial" || v.CountKind != "unknown" || len(v.Errors) == 0 || !strings.Contains(strings.Join(v.Errors, ";"), "shop/cart.py:6: references") {
		t.Fatalf("reference failure lost: %+v", v)
	}
	e.lspStore(g, f.Symbol, failed)
	if e.lspCached(g, f.Symbol) != nil {
		t.Fatal("failed references were cached")
	}
	c2 := truthClient(t, func(string) any { return []any{} })
	success := (&lspSession{c: c2, caps: lspCaps{Refs: true}, bin: "probe"}).refine(e, l, f, "build_cart")
	e.lspStore(g, f.Symbol, success)
	if got := e.lspCached(g, f.Symbol); got == nil || got.Defs[0].RefsUnknown {
		t.Fatalf("retry not cached: %+v", got)
	}
}

func TestCtxSymbolGoplsIdentityAndFifthScopedDefinition(t *testing.T) {
	code := evalOr(t.TempDir())
	server := filepath.Join(t.TempDir(), "gopls")
	script := "#!/bin/sh\n[ \"$1\" = workspace_symbol ] || exit 0\n"
	for i, receiver := range []string{"SomeWrong", "Wrong", "Other", "Other", "Other"} {
		p := fmt.Sprintf("p%d/item.go", i)
		writeTree(t, code, map[string]string{p: fmt.Sprintf("package item\ntype %s struct{}\nfunc (%s) DoThing() {}\n", receiver, receiver)})
		script += fmt.Sprintf("echo '%s:3:10 %s.DoThing Method'\n", filepath.Join(code, p), receiver)
	}
	os.WriteFile(server, []byte(script), 0755)
	r := &goplsRunner{bin: server, dir: code, cacheDir: t.TempDir()}
	exact, err := r.symbolFacts("Wrong.DoThing")
	if err != nil || len(exact.Defs) != 1 || exact.Defs[0].Where != "p1/item.go:3" || exact.Defs[0].Match != "exact" {
		t.Fatalf("receiver identity lost: %+v %v", exact, err)
	}
	bare, err := r.symbolFacts("DoThing")
	if err != nil || len(bare.Defs) != 5 {
		t.Fatalf("definitions capped: %+v %v", bare, err)
	}
	for _, d := range bare.Defs {
		if d.Match != "candidate" {
			t.Fatal("unqualified receiver marked exact", d)
		}
	}
	v := newCtxEnv(code, ctxOpts{}).symView(bare, symOpts{In: "p4", Kind: "Method"})
	if v.Shown != 1 {
		t.Fatalf("fifth scoped Go definition hidden: %+v", v)
	}
}

func TestTSFactsFifthScopedDefinition(t *testing.T) {
	code := evalOr(t.TempDir())
	var syms []any
	for i := range 5 {
		p := fmt.Sprintf("p%d/app.ts", i)
		writeTree(t, code, map[string]string{p: "export const RefreshCart = () => {}\n"})
		syms = append(syms, map[string]any{"name": "RefreshCart", "kind": 13, "location": map[string]any{"uri": fileURI(filepath.Join(code, p)), "range": map[string]any{"start": map[string]int{"line": 0, "character": 13}, "end": map[string]int{"line": 0, "character": 24}}}})
	}
	c := truthClient(t, func(method string) any {
		if method == "workspace/symbol" {
			return syms
		}
		return []any{}
	})
	f, err := tsFacts(c, code, "RefreshCart")
	if err != nil || len(f.Defs) != 5 {
		t.Fatalf("TS definitions capped: %+v %v", f, err)
	}
	v := newCtxEnv(code, ctxOpts{}).symView(f, symOpts{In: "p4"})
	if v.Shown != 1 {
		t.Fatalf("fifth scoped TS definition hidden: %+v", v)
	}
}

func TestCtxSymbolHiddenDefinitionFailuresRemainVisible(t *testing.T) {
	code := pyFixture(t)
	e := newCtxEnv(code, ctxOpts{})
	f := symFact{Symbol: "Cart", Lang: "python"}
	for i := range 5 {
		d := symDef{Where: "shop/cart.py:1", Kind: "Class", Match: "exact"}
		if i == 4 {
			symDefError(&d, "references", fmt.Errorf("timeout"))
		}
		f.Defs = append(f.Defs, d)
	}
	v := e.symView(f, symOpts{})
	if v.Shown != 4 || v.Total != 5 || v.Status != "partial" || v.CountKind != "unknown" || len(v.Errors) != 1 {
		t.Fatalf("cap hid fifth definition's failed references: %+v", v)
	}
}

func TestPythonWorkspaceSymbolsKeepScannedFifthDefinition(t *testing.T) {
	needRG(t)
	code := evalOr(t.TempDir())
	var syms []any
	for i := range 5 {
		p := fmt.Sprintf("src/item%d.py", i)
		writeTree(t, code, map[string]string{p: "def refresh_cart():\n    pass\n"})
		if i < 4 {
			syms = append(syms, map[string]any{"name": "refresh_cart", "kind": 12, "location": map[string]any{"uri": fileURI(filepath.Join(code, p)), "range": map[string]any{"start": map[string]int{"line": 0, "character": 4}, "end": map[string]int{"line": 0, "character": 16}}}})
		}
	}
	e := newCtxEnv(code, ctxOpts{})
	f := e.declFacts("refresh_cart")
	if f == nil || len(f.Defs) != 5 {
		t.Fatalf("fixture: %+v", f)
	}
	c := truthClient(t, func(method string) any {
		if method == "workspace/symbol" {
			return syms
		}
		return []any{}
	})
	out := (&lspSession{c: c, caps: lspCaps{WS: true, Refs: true}, bin: "probe"}).refine(e, langByID("python"), f, "refresh_cart")
	v := e.symView(*out, symOpts{In: "src/item4.py"})
	if v.Shown != 1 || len(out.Defs) != 5 {
		t.Fatalf("workspace-symbol window hid scanned fifth definition: %+v %+v", v, out)
	}
}

func TestPythonProjectRefinementPreservesOtherRoots(t *testing.T) {
	needRG(t)
	code := evalOr(t.TempDir())
	writeTree(t, code, map[string]string{"one/pyproject.toml": "[project]\nname='one'\n", "two/pyproject.toml": "[project]\nname='two'\n", "one/a.py": "def refresh_cart():\n    pass\n", "two/a.py": "def refresh_cart():\n    pass\n"})
	e := newCtxEnv(code, ctxOpts{})
	f := e.declFacts("refresh_cart")
	if f == nil || len(f.Defs) != 2 {
		t.Fatalf("fixture: %+v", f)
	}
	g := &lspGroup{lang: langByID("python"), root: projectRoot{Dir: "one/"}}
	subset := e.symGroupFact(f, g)
	if len(subset.Defs) != 1 || subset.Defs[0].Where != "one/a.py:1" {
		t.Fatalf("cross-root lookup: %+v", subset)
	}
	out := mergeSymGroup(f, subset, subset)
	if len(out.Defs) != 2 {
		t.Fatalf("refinement discarded other project: %+v", out)
	}
	if _, ok := rootFor(code, "one-extra/a.py", "python"); ok {
		t.Fatal("project root crossed path component")
	}
}

func TestTSFactsConcurrentOpenAndTimeoutCleanup(t *testing.T) {
	code := t.TempDir()
	path := filepath.Join(code, "app.ts")
	os.WriteFile(path, []byte("export {}"), 0644)
	var opens atomic.Int32
	c := truthClient(t, func(method string) any {
		if method == "textDocument/didOpen" {
			opens.Add(1)
		}
		return nil
	})
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() { defer wg.Done(); c.open(path) }()
	}
	wg.Wait()
	// A response after the concurrent opens ensures every earlier notification
	// has reached the server before checking the actual protocol transcript.
	if _, err := c.call("barrier", nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("sent %d didOpen notifications for one file", got)
	}
	// A zero budget may race a valid response but must always remove the waiter.
	for range 10 {
		_, _ = c.call("test", nil, time.Nanosecond)
	}
	c.mu.Lock()
	n := len(c.waits)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("timed out requests retained: %d", n)
	}
}
