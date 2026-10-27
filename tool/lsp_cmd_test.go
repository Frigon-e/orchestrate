package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestLspHelpAndUsageErrors(t *testing.T) {
	h := setup(t)
	out := h.must("lsp", "--help")
	wantContains(t, out, "orch lsp def Name", "orch lsp refs Name", "orch lsp callers Name", "orch lsp impls Name", "orch lsp symbols path/to/file", "orch lsp hover path:line:col")
	for _, verb := range lspVerbs {
		got := h.must("lsp", verb, "--help")
		wantContains(t, got, "usage: orch lsp "+verb, "examples:", "  orch lsp "+verb)
	}
	for _, args := range [][]string{
		{"lsp", "bogus"},
		{"lsp", "def"},
		{"lsp", "refs", "X", "--limit", "0"},
		{"lsp", "refs", "X", "--nope"},
		{"lsp", "symbols", "a.go", "b.go"},
		{"lsp", "hover"},
	} {
		if code, out := h.run(args...); code != 2 {
			t.Errorf("%v: exit %d %s", args, code, out)
		}
	}
	// --limit belongs to refs and callers only.
	if code, _ := h.run("lsp", "def", "X", "--limit", "3"); code != 2 {
		t.Errorf("def accepted --limit: exit %d", code)
	}
}

func TestLspDefRefsNamesAmbiguityAndNotFound(t *testing.T) {
	c := newCtxHarness(t)
	def := c.must("lsp", "def", "Stripe.StartCheckout")
	wantContains(t, def, "pay/pay.go:7 Method: func (s *Stripe) StartCheckout(ctx int) error")
	if strings.Contains(def, "##") || strings.Contains(def, c.repo) {
		t.Errorf("one name has no header and paths are repo-relative: %q", def)
	}

	code, amb := c.run("lsp", "def", "StartCheckout")
	wantContains(t, amb, "StartCheckout is ambiguous (2 candidates); qualify it as Type.StartCheckout or pkg.StartCheckout", "  pay/pay.go:4 Method", "  pay/pay.go:7 Method")
	if code != 0 {
		t.Errorf("ambiguous exit = %d", code)
	}

	code, nf := c.run("lsp", "def", "zzzNone")
	if code != 1 || strings.TrimSpace(nf) != "no symbol zzzNone" {
		t.Errorf("not found: exit %d %q", code, nf)
	}

	code, many := c.run("lsp", "def", "Stripe.StartCheckout", "zzzNone")
	wantContains(t, many, "## Stripe.StartCheckout\npay/pay.go:7 Method", "## zzzNone\nno symbol zzzNone")
	if code != 1 {
		t.Errorf("exit with one missing name = %d", code)
	}

	both := c.must("lsp", "def", "Stripe.StartCheckout", "Gateway.StartCheckout")
	wantContains(t, both, "## Stripe.StartCheckout\npay/pay.go:7 Method", "## Gateway.StartCheckout\npay/pay.go:4 Method")

	refs := c.must("lsp", "refs", "Gateway.StartCheckout", "--limit", "3")
	wantContains(t, refs, "pay/pay_test.go:3: ", "svc/svc.go:1: ", "svc/svc.go:2: ", "... 6 more (9 total; raise --limit)")
	if strings.Contains(refs, "svc/svc.go:3:") {
		t.Errorf("limit not applied: %s", refs)
	}
	all := c.must("lsp", "refs", "Gateway.StartCheckout")
	wantContains(t, all, "pay/pay_test.go:3: func TestPay(g Gateway) { g.StartCheckout(1) }", "svc/svc.go:7: return g.StartCheckout(1)")
	if strings.Contains(all, "more") {
		t.Errorf("default limit 30 should show all 9: %s", all)
	}
}

func TestLspJSONAndReadOnlyRoot(t *testing.T) {
	c := newCtxHarness(t)
	t.Setenv("ORCH_CTX_ROOT", evalOr(c.repo))
	out := c.must("lsp", "refs", "Stripe.StartCheckout", "--json")
	var got struct {
		Verb    string      `json:"verb"`
		Results []lspAnswer `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got.Verb != "refs" || len(got.Results) != 1 || got.Results[0].Status != "ok" || len(got.Results[0].Items) != 1 || got.Results[0].Items[0].Path != "svc/svc.go" || got.Results[0].Items[0].Line != 30 {
		t.Fatalf("json = %+v", got)
	}
	// An outline of a file outside the pinned root is refused.
	code, out := c.run("lsp", "symbols", "../outside.go")
	if code != 1 || !strings.Contains(out, "not a file inside the repository") {
		t.Errorf("outside path: exit %d %s", code, out)
	}
}

func TestLspMissingGoplsIsNoServerNotZero(t *testing.T) {
	c := newCtxHarness(t)
	t.Setenv("ORCH_GOPLS", c.jbDir+"/no-such-gopls")
	code, out := c.run("lsp", "refs", "Handle")
	if code != 2 || !strings.Contains(out, "no language server for go; use rg -n -w Handle") {
		t.Errorf("exit %d %q", code, out)
	}
}

func TestLspWordIndex(t *testing.T) {
	for _, tc := range []struct {
		line, base string
		want       int
	}{
		{"func (s *Save) Save(x int)", "Save", 9},
		{"func (save Save) save(x int)", "Save", 11},
		{"func SaveAll() {}", "Save", -1},
		{"Save()", "Save", 0},
		{"const x = 1", "y", -1},
	} {
		if got := wordIndex(tc.line, tc.base); got != tc.want {
			t.Errorf("wordIndex(%q, %q) = %d, want %d", tc.line, tc.base, got, tc.want)
		}
	}
}

func TestLspHoverText(t *testing.T) {
	got := hoverText(json.RawMessage(`{"contents":{"kind":"markdown","value":"` + "```go\\nfunc Greet(s Speaker) string\\n```\\n\\nGreet speaks." + `"}}`))
	if got != "func Greet(s Speaker) string\nGreet speaks." {
		t.Errorf("markdown: %q", got)
	}
	if got := hoverText(json.RawMessage(`{"contents":["a",{"language":"go","value":"b"}]}`)); got != "a\nb" {
		t.Errorf("list: %q", got)
	}
}

// The verbs that need a live server run against the real gopls, as the other
// live tests do, and are skipped without one.
func TestLspLiveGoplsCallersImplsSymbolsHover(t *testing.T) {
	// Found before setup replaces HOME, which hides ~/.local/bin/gopls.
	bin := goplsBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Skip("gopls is not installed")
	}
	h := setup(t)
	t.Setenv("ORCH_GOPLS", bin)
	src := "package fx\n\ntype Speaker interface{ Speak() string }\n\ntype Dog struct{}\n\n// Speak barks.\nfunc (Dog) Speak() string { return \"woof\" }\n\n// Greet asks s to speak.\nfunc Greet(s Speaker) string { return s.Speak() }\n\nfunc Run() string { return Greet(Dog{}) }\n"
	writeTree(t, h.repo, map[string]string{"go.mod": "module fx\n\ngo 1.21\n", "fx.go": src})
	h.git("add", ".")
	h.git("commit", "-q", "-m", "fx")

	wantContains(t, h.must("lsp", "def", "Greet"), "fx.go:11 Function: func Greet(s Speaker) string", "doc: Greet asks s to speak.")
	wantContains(t, h.must("lsp", "refs", "Greet"), "fx.go:13: ")
	wantContains(t, h.must("lsp", "callers", "Greet"), "fx.go:13 in Run")
	wantContains(t, h.must("lsp", "impls", "Speaker"), "fx.go:5: ")
	out := h.must("lsp", "symbols", "fx.go")
	wantContains(t, out, "Interface Speaker 3", "Function Greet 11", "Function Run 13")
	wantContains(t, h.must("lsp", "hover", "fx.go:11:6"), "Greet")
	if code, out := h.run("lsp", "hover", "fx.go:99:1"); code == 0 && !strings.Contains(out, "no hover") {
		t.Errorf("hover past the end: %d %s", code, out)
	}
	// Several names share the call: each answers under its own header.
	both := h.must("lsp", "callers", "Greet", "Run")
	wantContains(t, both, "## Greet\nfx.go:13 in Run", "## Run\nno callers of Run")
}
