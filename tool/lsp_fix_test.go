package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGoplsEnv is the ctx harness with a fake gopls whose `serve` mode is the
// python fake language server (testdata/fake-lsp.py), logging what it is told.
type fakeGoplsEnv struct {
	*ctxHarness
	log string
}

func newFakeGoplsEnv(t *testing.T) *fakeGoplsEnv {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed for the fake language server")
	}
	c := newCtxHarness(t)
	logF := filepath.Join(t.TempDir(), "server.log")
	os.WriteFile(logF, nil, 0o644)
	fake, _ := filepath.Abs("testdata/fake-lsp.py")
	wrapper := filepath.Join(c.jbDir, "gopls-wrapper")
	os.WriteFile(wrapper, []byte("#!/bin/sh\nif [ \"$1\" = serve ]; then exec python3 "+fake+" \"$@\"; fi\nexec "+os.Getenv("ORCH_GOPLS")+" \"$@\"\n"), 0o755)
	t.Setenv("ORCH_GOPLS", wrapper)
	t.Setenv("FAKE_LSP_LOG", logF)
	t.Setenv("FAKE_LSP_REPO", evalOr(c.repo))
	return &fakeGoplsEnv{ctxHarness: c, log: logF}
}

func (w *fakeGoplsEnv) events(method string) []map[string]any {
	var out []map[string]any
	for _, l := range strings.Split(w.ctxHarness.log(w.log), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["method"] == method {
			out = append(out, m)
		}
	}
	return out
}

func TestLspExitAndShutdownCarryNoParams(t *testing.T) {
	w := newFakeGoplsEnv(t)
	if out := w.must("lsp", "callers", "Stripe.StartCheckout"); out != "svc/svc.go:7 in Handle\n" {
		t.Fatalf("callers = %q", out)
	}
	for _, m := range []string{"shutdown", "exit"} {
		ev := w.events(m)
		if len(ev) != 1 || ev[0]["has_params"] != false {
			t.Errorf("%s was sent as %v; tsgo rejects a null params", m, ev)
		}
	}
	if ev := w.events("initialized"); len(ev) != 1 || ev[0]["has_params"] != true {
		t.Errorf("initialized must carry {}: %v", ev)
	}
}

// A Go name is resolved on the gopls serve session the request then uses: one
// gopls start per call, no command line gopls pass before it.
func TestLspGoNameResolvesOnTheServeSession(t *testing.T) {
	w := newFakeGoplsEnv(t)
	if out := w.must("lsp", "refs", "Stripe.StartCheckout"); out != "svc/svc.go:7: return g.StartCheckout(1)\n" {
		t.Fatalf("refs = %q", out)
	}
	if n := len(w.events("start")); n != 1 {
		t.Errorf("gopls serve started %d times, want 1", n)
	}
	if n := len(w.events("workspace/symbol")); n != 1 {
		t.Errorf("workspace/symbol asked %d times on the session, want 1", n)
	}
	if cli := w.ctxHarness.log(w.gCall); cli != "" {
		t.Errorf("the command line gopls ran: %q", cli)
	}
	code, out := w.run("lsp", "def", "zzzNone")
	if code != 1 || strings.TrimSpace(out) != "no symbol zzzNone" {
		t.Errorf("not found: exit %d %q", code, out)
	}
}

func TestLspImplsAcceptsLimit(t *testing.T) {
	w := newFakeGoplsEnv(t)
	out := w.must("lsp", "impls", "Gateway.StartCheckout", "--limit", "1")
	if strings.Count(out, "\n") != 2 || !strings.Contains(out, "... 1 more (2 total; raise --limit)") {
		t.Errorf("impls --limit 1 = %q", out)
	}
	if code, _ := w.run("lsp", "impls", "Gateway.StartCheckout", "--limit", "0"); code != 2 {
		t.Errorf("--limit 0 exit %d, want 2", code)
	}
}

func TestLspSymbolsHintIsAnOutlineSearchNotThePathAsAWord(t *testing.T) {
	c := newCtxHarness(t)
	t.Setenv("ORCH_GOPLS", filepath.Join(c.jbDir, "no-such-gopls"))
	code, out := c.run("lsp", "symbols", "pay/pay.go")
	if code != 2 {
		t.Errorf("exit %d", code)
	}
	wantContains(t, out, `no language server for go; use rg -n "^(func|type|export|class|def) " pay/pay.go`)
	if strings.Contains(out, "-w pay/pay.go") {
		t.Errorf("the path is suggested as a word: %q", out)
	}
}

// tsRepo is the ctx harness repo with two TypeScript projects, web/a and web/b,
// and the fake TypeScript server, whose project loads only once a file of the
// project named by needsOpen is opened. WebThing is declared in web/b.
func tsRepo(t *testing.T, needsOpen string) (*ctxHarness, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed for the fake language server")
	}
	c := newCtxHarness(t)
	for _, p := range []string{"a", "b"} {
		os.MkdirAll(filepath.Join(c.repo, "web", p, "src"), 0o755)
		os.WriteFile(filepath.Join(c.repo, "web", p, "tsconfig.json"), []byte("{}"), 0o644)
		os.WriteFile(filepath.Join(c.repo, "web", p, "src", "main.ts"), []byte("export const x = 1\n"), 0o644)
	}
	thing := filepath.Join(c.repo, "web", "b", "src", "thing.ts")
	os.WriteFile(thing, []byte("export const WebThing = 1\n"), 0o644)
	c.git("add", ".")
	c.git("commit", "-q", "-m", "web")
	tmp := t.TempDir()
	syms, _ := json.Marshal(map[string]any{"WebThing": []any{map[string]any{"name": "WebThing", "kind": 13, "location": map[string]any{
		"uri": fileURI(evalOr(thing)), "range": map[string]any{"start": map[string]int{"line": 0, "character": 13}, "end": map[string]int{"line": 0, "character": 21}}}}}})
	os.WriteFile(filepath.Join(tmp, "symbols.json"), syms, 0o644)
	os.WriteFile(filepath.Join(tmp, "refs.json"), []byte("{}"), 0o644)
	logF := filepath.Join(tmp, "log")
	t.Setenv("FAKE_LSP_LOG", logF)
	t.Setenv("FAKE_LSP_SYMBOLS", filepath.Join(tmp, "symbols.json"))
	t.Setenv("FAKE_LSP_REFS", filepath.Join(tmp, "refs.json"))
	t.Setenv("FAKE_LSP_NEEDS_OPEN", needsOpen)
	abs, _ := filepath.Abs("testdata/fake-tsls.py")
	t.Setenv("ORCH_TSLS", abs)
	return c, logF
}

func countMethod(t *testing.T, log, method string) int {
	t.Helper()
	b, _ := os.ReadFile(log)
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["method"] == method {
			n++
		}
	}
	return n
}

// A name given without a file is looked up in every TypeScript project, not
// only the first one.
func TestLspBareNameIsLookedUpInEveryTypeScriptProject(t *testing.T) {
	c, log := tsRepo(t, "/web/b/")
	code, out := c.run("lsp", "def", "WebThing")
	if code != 0 || !strings.HasPrefix(out, "web/b/src/thing.ts:1 ") {
		t.Errorf("exit %d %q, want the definition in web/b", code, out)
	}
	b, _ := os.ReadFile(log)
	if n := strings.Count(string(b), "src/main.ts"); n < 2 || !strings.Contains(string(b), "/web/a/src/main.ts") || !strings.Contains(string(b), "/web/b/src/main.ts") {
		t.Errorf("a file of each of the two projects should be opened:\n%s", b)
	}
}

// Once every project has proved it loaded, an empty answer is a real absence:
// exit 1 at once, with no polling until tsReady and no readiness caveat.
func TestLspMissingNameAfterReadinessIsConfirmedIsNotFound(t *testing.T) {
	c, log := tsRepo(t, "/web/b/")
	old := tsReady
	tsReady = 5 * time.Minute // a poll-until-ready would never return in this test
	t.Cleanup(func() { tsReady = old })
	code, out := c.run("lsp", "def", "zzzNoneXyz")
	if code != 1 || strings.TrimSpace(out) != "no symbol zzzNoneXyz" {
		t.Errorf("exit %d %q", code, out)
	}
	if q, p := countMethod(t, log, "workspace/symbol"), countMethod(t, log, "textDocument/documentSymbol"); q != 2 || p != 2 {
		t.Errorf("want one empty ask, a probe of each of the 2 projects, one more ask; got %d asks and %d probes", q, p)
	}
}

// A project that never proves it loaded keeps the caveat: the name may exist.
// It is still a missing name (exit 1), never a missing server.
func TestLspMissingNameWithAnUnconfirmedProjectExitsOne(t *testing.T) {
	c, _ := tsRepo(t, "")
	t.Setenv("FAKE_LSP_NODOC", "1")
	code, out := c.run("lsp", "def", "zzzNoneXyz")
	if code != 1 || strings.TrimSpace(out) != "no symbol zzzNoneXyz" {
		t.Errorf("exit %d %q, want exit 1 and the not-found line", code, out)
	}
}
