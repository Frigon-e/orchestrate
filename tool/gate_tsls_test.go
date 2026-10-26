package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTSSymbolsPicksCamelAndPascalNamesAndSkipsTheRest(t *testing.T) {
	for _, c := range []struct {
		q    string
		skip map[string]bool
		want string
	}{
		{"Does createGatewaySchema in features/admin/gateways.tsx use ConnectGateway?", nil, "createGatewaySchema,ConnectGateway"},
		{"Is `useFoo` called by `FooForm(props)` and `account_id`?", nil, "useFoo,FooForm"},
		{"Why does the customer area sign out, and is it StartCheckout?", map[string]bool{"StartCheckout": true}, ""},
		{"Do aB, useX and Stripe exist? Is orders.go or the Order table involved?", nil, ""},
		{"Does fooBarOne, fooBarTwo, fooBarThree and fooBarFour agree?", nil, "fooBarOne,fooBarTwo,fooBarThree"},
	} {
		if got := strings.Join(tsSymbols(c.q, c.skip), ","); got != c.want {
			t.Errorf("%q -> %q, want %q", c.q, got, c.want)
		}
	}
}

func TestTsNativeServerTakesTheHighestTypeScript7WithAPlatformPackage(t *testing.T) {
	code := t.TempDir()
	plat := map[string]string{"darwin": "darwin", "linux": "linux", "windows": "win32"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	pnpm := filepath.Join(code, "web", "node_modules", ".pnpm")
	mkTS := func(v string, native bool) string {
		os.MkdirAll(filepath.Join(pnpm, "typescript@"+v, "node_modules", "typescript"), 0o755)
		exe := filepath.Join(pnpm, "@typescript+typescript-"+plat+"-"+arch+"@"+v, "node_modules", "@typescript", "typescript-"+plat+"-"+arch, "lib", "tsc")
		if native {
			os.MkdirAll(filepath.Dir(exe), 0o755)
			os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
		}
		return exe
	}
	mkTS("6.0.3", true) // an older compiler, never the language server
	mkTS("7.0.1", true)
	if got, want := tsNativeServer(code), mkTS("7.0.1", true); got != want {
		t.Errorf("picked %q, want %q", got, want)
	}
	newest := mkTS("7.0.10", true)
	if got := tsNativeServer(code); got != newest {
		t.Errorf("picked %q, want the numerically highest %q", got, newest)
	}
	os.RemoveAll(filepath.Dir(filepath.Dir(newest)))
	os.RemoveAll(filepath.Join(pnpm, "@typescript+typescript-"+plat+"-"+arch+"@7.0.10"))
	if got := tsNativeServer(code); !strings.Contains(got, "@7.0.1/") {
		t.Errorf("without the platform package of 7.0.10 got %q, want 7.0.1", got)
	}
	if got := tsNativeServer(t.TempDir()); got != "" {
		t.Errorf("no store = %q, want none", got)
	}
	t.Setenv("ORCH_TSLS", "/x/override")
	if got := tslsBin(code); got != "/x/override" {
		t.Errorf("ORCH_TSLS = %q", got)
	}
}

// tsFixture builds a checkout with a web app, a fake language server and its
// fixtures, and returns the pieces a test needs.
type tsEnv struct {
	code, log, hang string
	dir             string
	sp              *spec
	src             string
}

func newTSEnv(t *testing.T) *tsEnv {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed for the fake language server")
	}
	code, tmp := t.TempDir(), t.TempDir()
	app := filepath.Join(code, "web", "apps", "crm")
	os.MkdirAll(filepath.Join(app, "src", "features"), 0o755)
	os.MkdirAll(filepath.Join(app, "src", "pages"), 0o755)
	os.WriteFile(filepath.Join(app, "tsconfig.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(app, "src", "main.tsx"), []byte("export {}\n"), 0o644)
	os.WriteFile(filepath.Join(app, "src", "features", "gateways.tsx"), []byte("import x from 'y'\n\nexport const createGatewaySchema = () => ({\n  a: 1,\n})\n"), 0o644)

	uri := func(p string) string { return fileURI(p) }
	loc := func(p string, line int) map[string]any {
		return map[string]any{"uri": uri(p), "range": map[string]any{"start": map[string]int{"line": line, "character": 0}, "end": map[string]int{"line": line, "character": 5}}}
	}
	file := filepath.Join(app, "src", "features", "gateways.tsx")
	sym := func(name string, kind int, l map[string]any) map[string]any {
		return map[string]any{"name": name, "kind": kind, "location": l}
	}
	l := loc(file, 2)
	l["range"] = map[string]any{"start": map[string]int{"line": 2, "character": 0}, "end": map[string]int{"line": 4, "character": 1}}
	symbols := map[string]any{"createGatewaySchema": []any{
		sym("createGatewaySchema", 13, l),
		sym("createGatewaySchemaOld", 13, loc(file, 0)),
		sym("createGatewaySchema", 13, loc(filepath.Join(app, "node_modules", "dep", "x.ts"), 1)),
	}}
	refs := map[string]any{"default": []any{
		loc(filepath.Join(app, "src", "pages", "a.tsx"), 9),
		loc(filepath.Join(app, "src", "pages", "b.tsx"), 19),
		loc(filepath.Join(app, "src", "pages", "a.test.tsx"), 3),
	}}
	write := func(name string, v any) string {
		b, _ := json.Marshal(v)
		p := filepath.Join(tmp, name)
		os.WriteFile(p, b, 0o644)
		return p
	}
	logF := filepath.Join(tmp, "lsp.log")
	os.WriteFile(logF, nil, 0o644)
	t.Setenv("FAKE_LSP_LOG", logF)
	t.Setenv("FAKE_LSP_SYMBOLS", write("symbols.json", symbols))
	t.Setenv("FAKE_LSP_REFS", write("refs.json", refs))
	abs, _ := filepath.Abs("testdata/fake-tsls.py")
	t.Setenv("ORCH_TSLS", abs)
	t.Setenv("ORCH_GOPLS", filepath.Join(tmp, "absent"))
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(tmp, "absent"))
	qs := []gateQuestion{{Package: "api", Question: "Does createGatewaySchema in features/gateways.tsx need ConnectGateway, and is `account_id` involved?", Decisive: true}}
	dir, _, src, sp := enrichFixture(t, qs)
	return &tsEnv{code: code, log: logF, dir: dir, sp: sp, src: src}
}

func (e *tsEnv) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(mustRead(t, e.log))), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func TestTSFactsFromALanguageServerOverStdio(t *testing.T) {
	e := newTSEnv(t)
	old := tsReady
	tsReady = 5 * time.Second
	t.Cleanup(func() { tsReady = old })
	t.Setenv("FAKE_LSP_COLD", "2") // the project is still loading: the first two answers are empty
	res, err := enrichGate(e.dir, e.code, e.src, "spec.md", e.sp)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Join(res.Notes, "|"); res.SymbolFacts != 1 || strings.Contains(n, "TypeScript") || strings.Contains(n, "tsserver") {
		t.Fatalf("facts %d notes %v", res.SymbolFacts, res.Notes)
	}
	card := string(mustRead(t, filepath.Join(e.dir, "questions", "q1-api.md")))
	wantContains(t, card, "## Exact symbol facts",
		"- `createGatewaySchema` (typescript Variable) is defined at `web/apps/crm/src/features/gateways.tsx:3`; 2 callers outside tests: `web/apps/crm/src/pages/a.tsx:10`, `web/apps/crm/src/pages/b.tsx:20`; 1 test references",
		"- no symbol named `ConnectGateway` found (typescript)")
	facts := card[strings.Index(card, "## Exact symbol facts"):]
	if strings.Contains(facts, "createGatewaySchemaOld") || strings.Contains(facts, "node_modules") || strings.Contains(facts, "account_id") {
		t.Errorf("a lookalike, a dependency or a column leaked:\n%s", card)
	}

	// The conversation: initialize with the project root and full client
	// capabilities, initialized, a source file opened, symbol and reference
	// requests, then shutdown and exit.
	var methods []string
	for _, m := range e.logLines(t) {
		if s, ok := m["method"].(string); ok {
			methods = append(methods, s)
		}
		switch m["method"] {
		case "initialize":
			if fmt.Sprint(m["args"]) != "[--lsp --stdio]" || !strings.HasSuffix(fmt.Sprint(m["root"]), "/web/apps/crm") || m["config"] != true || m["watch"] != true {
				t.Errorf("initialize = %v", m)
			}
		case "textDocument/references":
			if m["line"].(float64) != 2 || m["character"].(float64) != 13 || m["decl"] != false {
				t.Errorf("references asked at %v", m)
			}
		}
	}
	asked := 0
	for _, m := range e.logLines(t) {
		if m["method"] == "workspace/symbol" && m["query"] == "createGatewaySchema" {
			asked++
		}
	}
	if asked != 3 {
		t.Errorf("createGatewaySchema was asked %d times, want 3 (two empty answers while the project loaded)", asked)
	}
	idx := func(m string) int {
		for i, x := range methods {
			if x == m {
				return i
			}
		}
		return -1
	}
	refCount := 0
	for _, m := range methods {
		if m == "textDocument/references" {
			refCount++
		}
	}
	if methods[0] != "initialize" || idx("initialized") != 1 || idx("textDocument/didOpen") < 0 || idx("textDocument/didOpen") > idx("workspace/symbol") || refCount != 1 ||
		methods[len(methods)-2] != "shutdown" || methods[len(methods)-1] != "exit" {
		t.Errorf("methods = %v", methods)
	}
	registered, configured := false, false
	for _, m := range e.logLines(t) {
		registered = registered || m["reply"] == float64(900)
		if r, ok := m["result"].([]any); m["reply"] == float64(901) && ok && len(r) == 2 && r[0] == nil && r[1] == nil {
			configured = true // one null per configuration item
		}
	}
	if !registered || !configured {
		t.Errorf("the client did not answer the server's own requests (register %v, configuration %v)", registered, configured)
	}

	// A rerun is served from the cache: no server is started.
	before := len(e.logLines(t))
	if _, err := enrichGate(e.dir, e.code, e.src, "spec.md", e.sp); err != nil {
		t.Fatal(err)
	}
	if after := len(e.logLines(t)); after != before {
		t.Errorf("a rerun talked to the server again: %d -> %d messages", before, after)
	}
}

func TestTSFactsMissingServerNoNativeServerAndTimeoutLeaveValidCards(t *testing.T) {
	check := func(t *testing.T, e *tsEnv, want string) {
		t.Helper()
		res, err := enrichGate(e.dir, e.code, e.src, "spec.md", e.sp)
		if err != nil {
			t.Fatal(err)
		}
		if res.SymbolFacts != 0 || !strings.Contains(strings.Join(res.Notes, "|"), want) {
			t.Fatalf("facts %d notes %v, want a note with %q", res.SymbolFacts, res.Notes, want)
		}
		if c, err := parseCard(string(mustRead(t, filepath.Join(e.dir, "questions", "q1-api.md")))); err != nil || c.question == "" {
			t.Fatalf("card invalid: %v", err)
		}
	}
	t.Run("server missing", func(t *testing.T) {
		e := newTSEnv(t)
		t.Setenv("ORCH_TSLS", filepath.Join(t.TempDir(), "absent-tsls"))
		check(t, e, "absent-tsls was not found")
	})
	t.Run("no native server", func(t *testing.T) {
		e := newTSEnv(t)
		t.Setenv("ORCH_TSLS", "")
		// Own PATH: only the JavaScript tsc of TypeScript 5, so the result does
		// not depend on a TypeScript 7 tsc installed on the machine.
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "tsc"), []byte("#!/bin/sh\necho 'Version 5.6.3'\n"), 0o755)
		t.Setenv("PATH", bin)
		check(t, e, "no native TypeScript 7 server found")
	})
	t.Run("timeout", func(t *testing.T) {
		e := newTSEnv(t)
		t.Setenv("FAKE_LSP_HANG", "1")
		old := tsRequestLimit
		tsRequestLimit = 200 * time.Millisecond
		t.Cleanup(func() { tsRequestLimit = old })
		check(t, e, "timed out")
		var last string
		for _, m := range e.logLines(t) {
			if s, ok := m["method"].(string); ok {
				last = s
			}
		}
		if last != "exit" {
			t.Errorf("the server was not shut down after the timeouts; last message %q", last)
		}
	})
}

func TestDropGoMissOnlyForNamesTypeScriptFound(t *testing.T) {
	facts := []symFact{
		{Symbol: "Foo", Lang: "go"},
		{Symbol: "Bar", Lang: "go"},
		{Symbol: "Foo", Lang: "go", Defs: []symDef{{Where: "a.go:1"}}},
	}
	got := dropGoMiss(facts, "Foo")
	if len(got) != 2 || got[0].Symbol != "Bar" || len(got[1].Defs) != 1 {
		t.Errorf("got %+v", got)
	}
}
