package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func needRG(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
}

// pyFixture is a Python package with a class, a function, a test file and an
// entry point, and a config-free layout (no web/, internal/ or cmd/).
func pyFixture(t *testing.T) string {
	t.Helper()
	needRG(t)
	code := evalOr(t.TempDir())
	writeTree(t, code, map[string]string{
		"pyproject.toml":     "[project]\nname = \"shop\"\n",
		"shop/__init__.py":   "",
		"shop/cart.py":       "class Cart:\n    def add(self, item):\n        return item\n\n\ndef build_cart(items):\n    total = len(items)\n    return Cart()\n",
		"shop/checkout.py":   "from shop.cart import Cart, build_cart\n\n\ndef pay():\n    return build_cart([1])\n",
		"shop/__main__.py":   "from shop.checkout import pay\n\n\ndef main():\n    pay()\n",
		"tests/test_cart.py": "from shop.cart import build_cart\n\n\ndef test_build_cart():\n    assert build_cart([])\n",
	})
	t.Setenv("ORCH_CACHE_DIR", t.TempDir())
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	return code
}

func TestPythonDeclarationTierWithAServerThatLacksWorkspaceSymbol(t *testing.T) {
	code := pyFixture(t)
	tmp := t.TempDir()
	refs, _ := json.Marshal(map[string]any{"textDocument/references": []map[string]any{
		{"uri": "file://" + code + "/shop/checkout.py", "range": map[string]any{"start": map[string]int{"line": 4, "character": 11}, "end": map[string]int{"line": 4, "character": 21}}},
		{"uri": "file://" + code + "/tests/test_cart.py", "range": map[string]any{"start": map[string]int{"line": 4, "character": 11}, "end": map[string]int{"line": 4, "character": 21}}},
	}})
	refsF, logF := filepath.Join(tmp, "refs.json"), filepath.Join(tmp, "lsp.log")
	os.WriteFile(refsF, refs, 0o644)
	os.WriteFile(logF, nil, 0o644)
	t.Setenv("FAKE_LSP_REFS", refsF)
	t.Setenv("FAKE_LSP_LOG", logF)
	abs, _ := filepath.Abs("testdata/fake-pyls.py")
	t.Setenv("ORCH_PYLS", "python3 "+abs)

	e := newCodeEnv(code, codeOpts{})
	f := e.Sym("build_cart")
	if len(f.Defs) != 1 || f.Defs[0].Where != "shop/cart.py:6" || f.Defs[0].Kind != "Function" {
		t.Fatalf("definition from the declaration tier: %+v", f.Defs)
	}
	if got := f.Defs[0].Callers; len(got) != 1 || got[0] != "shop/checkout.py:5" || f.Defs[0].Tests != 1 || f.Defs[0].ByName {
		t.Errorf("references from the server: %+v", f.Defs[0])
	}
	if !strings.Contains(f.Tier, "declarations by rg") {
		t.Errorf("tier %q", f.Tier)
	}
	log := string(mustRead(t, logF))
	if strings.Contains(log, "workspace/symbol") {
		t.Errorf("workspace/symbol asked of a server that does not advertise it:\n%s", log)
	}
	wantContains(t, log, `"method": "textDocument/references", "line": 5, "character": 4`, `"lang": "python"`, `"root": "file://`+code+`"`)
	wantContains(t, e.Sources(), "sym python")

	// A word that is no declaration is not a symbol; a test and an entry point
	// are not look-alikes.
	if g := e.Sym("total"); len(g.Defs) != 0 {
		t.Errorf("a bare word became a symbol: %+v", g.Defs)
	}
	if g := e.Sym("test_build_cart"); len(g.Defs) != 0 {
		t.Errorf("a test declaration was listed: %+v", g.Defs)
	}
	if !isEntryPoint("shop/__main__.py") || lookAlikePath("shop/__main__.py", nil) || lookAlikePath("tests/test_cart.py", nil) || !lookAlikePath("shop/cart.py", nil) {
		t.Error("entry point or test not excluded from look-alikes")
	}
}

func TestPythonWithoutAServerGivesTheDeclarationTierAndTheInstallHint(t *testing.T) {
	code := pyFixture(t)
	rg, _ := exec.LookPath("rg")
	bin := t.TempDir()
	os.Symlink(rg, filepath.Join(bin, "rg"))
	t.Setenv("PATH", bin)
	t.Setenv("ORCH_PYLS", "")
	t.Setenv("ORCH_RG", rg)
	e := newCodeEnv(code, codeOpts{})
	f := e.Sym("Cart")
	if len(f.Defs) != 1 || f.Defs[0].Where != "shop/cart.py:1" || f.Defs[0].Kind != "Class" || !f.Defs[0].ByName {
		t.Fatalf("declaration tier: %+v", f.Defs)
	}
	if f.Tier != "declaration scan (rg)" {
		t.Errorf("tier %q", f.Tier)
	}
	src := e.Sources()
	wantContains(t, src, "declaration scan (rg)", "uv tool install pyrefly")
	var py *langStatus
	for _, s := range e.langStatuses() {
		if s.ID == "python" {
			s := s
			py = &s
		}
	}
	if py == nil || py.Server != "" || !strings.Contains(py.Hint, "pyrefly") {
		t.Errorf("status %+v", py)
	}
}

func TestCtxLangConfiguredServerNotUsable(t *testing.T) {
	for _, language := range []string{"python", "typescript"} {
		for _, kind := range []string{"missing", "directory", "non-executable"} {
			t.Run(language+"/"+kind, func(t *testing.T) {
				h := fixtureRepo(t, "mixed")
				bin := filepath.Join(h.tmp, "unusable-server")
				switch kind {
				case "directory":
					if err := os.Mkdir(bin, 0755); err != nil {
						t.Fatal(err)
					}
				case "non-executable":
					if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				env := "ORCH_PYLS"
				if language == "typescript" {
					env = "ORCH_TSLS"
				}
				t.Setenv(env, bin)
				e := newCodeEnv(h.repo, codeOpts{})
				for _, status := range e.langStatuses() {
					if status.ID != language {
						continue
					}
					if status.Server != "" || !strings.HasPrefix(status.Hint, "configured language server not usable: ") {
						t.Fatalf("inconsistent unusable-server diagnostic: %+v", status)
					}
					return
				}
				t.Fatal("language status missing")
			})
		}
	}
}

func TestCtxLangsConfigAddsALanguage(t *testing.T) {
	needRG(t)
	t.Cleanup(func() { applyCtxLangs(nil) })
	code := evalOr(t.TempDir())
	writeTree(t, code, map[string]string{"src/main.zig": "pub fn render() void {}\n"})
	t.Setenv("ORCH_CACHE_DIR", t.TempDir())
	if f := newCodeEnv(code, codeOpts{}).declFacts("render"); f != nil && len(f.Defs) > 0 {
		t.Fatal("zig known before it is configured")
	}
	var cfg config
	cfg.CtxLangs = []langCfg{{ID: "zig", Exts: []string{".zig"}, Decl: []string{`^\s*(?:pub\s+)?fn\s+{sym}\b`}}}
	e := newCodeEnv(code, codeOpts{Cfg: cfg})
	f := e.declFacts("render")
	if f == nil || len(f.Defs) != 1 || f.Defs[0].Where != "src/main.zig:1" {
		t.Fatalf("configured language: %+v", f)
	}
}

func TestProjectRootsComeFromManifestsNotDirectoryNames(t *testing.T) {
	code := evalOr(t.TempDir())
	writeTree(t, code, map[string]string{
		"go.mod":                      "module example.com/root\n\ngo 1.23\n",
		"main.go":                     "package main\n",
		"frontend/package.json":       "{}\n",
		"frontend/tsconfig.json":      "{}\n",
		"frontend/src/app.ts":         "export const a = 1\n",
		"services/api/pyproject.toml": "[project]\nname = \"api\"\n",
		"services/api/api/main.py":    "def run():\n    pass\n",
		"core/go.mod":                 "module example.com/core\n\ngo 1.23\n",
		"core/core.go":                "package core\n",
	})
	got := map[string][]string{}
	for _, r := range projectRoots(code) {
		got[r.Dir] = r.Langs
	}
	if len(got) != 4 {
		t.Fatalf("project roots = %v, want four manifest roots", got)
	}
	for dir, lang := range map[string]string{"": "go", "frontend/": "typescript", "services/api/": "python", "core/": "go"} {
		found := false
		for _, l := range got[dir] {
			found = found || l == lang
		}
		if !found {
			t.Errorf("root %q lacks %s: %v", dir, lang, got)
		}
	}
	if r, ok := rootFor(code, "core/core.go", "go"); !ok || r.Dir != "core/" {
		t.Errorf("nested go root = %q, found %v", r.Dir, ok)
	}
	if r, ok := rootFor(code, "main.go", "go"); !ok || r.Dir != "" {
		t.Errorf("root-level go root = %q, found %v", r.Dir, ok)
	}
	if r, ok := rootFor(code, "services/api/api/main.py", "python"); !ok || r.Dir != "services/api/" {
		t.Errorf("python root = %q, found %v", r.Dir, ok)
	}
	trees := codeTreesFor(code, []string{"services/api/api/main.py"})
	if len(trees) == 0 || !strings.HasPrefix(trees[0], "services/api") {
		t.Errorf("the root the package owns files in comes first: %v", trees)
	}
}

func TestParentWorkspaceIsOneRootOnlyWhenItDeclaresItsChildren(t *testing.T) {
	declared := evalOr(t.TempDir())
	writeTree(t, declared, map[string]string{
		"pnpm-workspace.yaml":   "packages:\n  - 'ui/*'\n",
		"package.json":          "{}\n",
		"ui/kit/package.json":   "{}\n",
		"ui/kit/src/a.ts":       "export const a = 1\n",
		"ui/shell/package.json": "{}\n",
		"ui/shell/src/b.ts":     "export const b = 1\n",
	})
	plain := evalOr(t.TempDir())
	writeTree(t, plain, map[string]string{
		"ui/kit/package.json":   "{}\n",
		"ui/kit/src/a.ts":       "export const a = 1\n",
		"ui/shell/package.json": "{}\n",
		"ui/shell/src/b.ts":     "export const b = 1\n",
	})
	if r, _ := rootFor(declared, "ui/kit/src/a.ts", "typescript"); r.Dir != "" {
		t.Errorf("a declared workspace is one root, got %q", r.Dir)
	}
	if r, _ := rootFor(plain, "ui/kit/src/a.ts", "typescript"); r.Dir != "ui/kit/" {
		t.Errorf("a plain monorepo keeps one root per manifest, got %q", r.Dir)
	}
	if r, _ := rootFor(plain, "ui/shell/src/b.ts", "typescript"); r.Dir != "ui/shell/" {
		t.Errorf("second child root = %q", r.Dir)
	}
}

func TestLivePyreflyTier(t *testing.T) {
	if _, err := exec.LookPath("pyrefly"); err != nil {
		if home, _ := os.UserHomeDir(); home != "" {
			if _, err := os.Stat(filepath.Join(home, ".local", "bin", "pyrefly")); err == nil {
				t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+filepath.Join(home, ".local", "bin"))
			}
		}
	}
	if _, err := exec.LookPath("pyrefly"); err != nil {
		t.Skip("pyrefly not installed")
	}
	code := pyFixture(t)
	t.Setenv("ORCH_PYLS", "")
	e := newCodeEnv(code, codeOpts{})
	f := e.Sym("build_cart")
	if len(f.Defs) != 1 || f.Defs[0].Where != "shop/cart.py:6" {
		t.Fatalf("definition: %+v", f.Defs)
	}
	if !strings.Contains(f.Tier, "pyrefly") || f.Defs[0].ByName {
		t.Fatalf("tier %q, def %+v", f.Tier, f.Defs[0])
	}
	if got := strings.Join(f.Defs[0].Callers, ","); !strings.Contains(got, "shop/checkout.py:5") || f.Defs[0].Tests < 1 {
		t.Errorf("callers %v tests %d", f.Defs[0].Callers, f.Defs[0].Tests)
	}
}
