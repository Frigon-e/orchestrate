package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// withBackends replaces the registry for one test and restores it after.
func withBackends(t *testing.T, regs ...backendReg) {
	t.Helper()
	backendMu.Lock()
	saved := backendRegs
	backendRegs = slices.Clone(regs)
	backendMu.Unlock()
	t.Cleanup(func() {
		backendMu.Lock()
		backendRegs = saved
		backendMu.Unlock()
	})
}

func envHas(key string) func(func(string) string) bool {
	return func(getenv func(string) string) bool { return getenv(key) != "" }
}

func fakeReg(name, prefix string, declare ...string) backendReg {
	return backendReg{
		name:   name,
		prefix: prefix,
		detect: envHas(prefix + "PANE"),
		ctor:   func() (Backend, error) { return newFakeBackend(name, declare...), nil },
	}
}

func TestBackendSelectionDetectsAndHonoursOverride(t *testing.T) {
	withBackends(t, fakeReg("alpha", "ALPHA_"), fakeReg("beta", "BETA_"))
	pick := func(env ...string) string {
		t.Helper()
		a, err := selectBackend(env, nil)
		if err != nil {
			t.Fatalf("selectBackend(%v): %v", env, err)
		}
		return a.Name()
	}
	if got := pick("BETA_PANE=1"); got != "beta" {
		t.Errorf("detected %q, want beta", got)
	}
	// Both detect: registration order wins.
	if got := pick("BETA_PANE=1", "ALPHA_PANE=1"); got != "alpha" {
		t.Errorf("with both detected got %q, want alpha (first registered)", got)
	}
	// ORCH_BACKEND beats detection, and works with no detecting env at all.
	if got := pick("ALPHA_PANE=1", "ORCH_BACKEND=beta"); got != "beta" {
		t.Errorf("ORCH_BACKEND got %q, want beta", got)
	}
	if got := pick("ORCH_BACKEND=alpha"); got != "alpha" {
		t.Errorf("ORCH_BACKEND without env got %q, want alpha", got)
	}
}

func TestBackendSecondFakeUsedWithoutCoreEdits(t *testing.T) {
	// A backend registered only by this test is detected, selected and driven
	// through the interface; no core file names it.
	withBackends(t, fakeReg("alpha", "ALPHA_"))
	registerBackend("gamma", "GAMMA_", envHas("GAMMA_PANE"), func() (Backend, error) { return newFakeBackend("gamma"), nil })
	a, err := selectBackend([]string{"GAMMA_PANE=1"}, nil)
	if err != nil || a.Name() != "gamma" {
		t.Fatalf("detect gamma: %v, %v", a, err)
	}
	b, err := selectBackend([]string{"ORCH_BACKEND=gamma"}, nil)
	if err != nil || b.Name() != "gamma" {
		t.Fatalf("ORCH_BACKEND=gamma: %v, %v", b, err)
	}
	id, err := b.Start(t.Context(), Spec{Agent: "x", Name: "n"})
	if err != nil || id.Backend != "gamma" {
		t.Fatalf("Start through Active: %+v, %v", id, err)
	}
}

func TestBackendUnknownOverrideNamesRegistered(t *testing.T) {
	withBackends(t, fakeReg("alpha", "ALPHA_"), fakeReg("beta", "BETA_"))
	_, err := selectBackend([]string{"ORCH_BACKEND=nope"}, nil)
	if err == nil || !strings.Contains(err.Error(), "alpha, beta") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error %v should name nope and the registered backends", err)
	}
}

func TestBackendNoneDetectedFailsEarlyNamingRegistered(t *testing.T) {
	withBackends(t, fakeReg("alpha", "ALPHA_"), fakeReg("beta", "BETA_"))
	// Env with a registered prefix that detect does not accept still shows up.
	_, err := requireBackend("spawn", []string{"ALPHA_OTHER=1", "PATH=/bin"}, nil)
	if err == nil {
		t.Fatal("spawn with no detected backend must fail")
	}
	for _, want := range []string{"registered backends: alpha, beta", "ALPHA_OTHER", "ORCH_BACKEND"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "PATH") {
		t.Errorf("error %q lists unrelated env", err)
	}
}

func TestBackendNoRegisteredBackendsMessage(t *testing.T) {
	withBackends(t)
	_, err := requireBackend("spawn", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "registered backends: none") {
		t.Fatalf("got %v, want a message saying no backends are registered", err)
	}
}

func TestBackendFreeCommandsNeverTouchBackend(t *testing.T) {
	withBackends(t) // nothing registered: selecting would fail
	for _, cmd := range []string{"plan", "verify", "gate", "bench", "lsp", "version"} {
		a, err := requireBackend(cmd, nil, nil)
		if a != nil || err != nil {
			t.Errorf("%s: got %v, %v; want nil, nil", cmd, a, err)
		}
	}
	for _, cmd := range []string{"spawn", "sub", "prune", "adopt", "become"} {
		if !commandNeedsBackend(cmd) {
			t.Errorf("%s must need a backend", cmd)
		}
	}
}

func TestBackendRegisterRejectsDuplicates(t *testing.T) {
	withBackends(t, fakeReg("alpha", "ALPHA_"))
	for name, reg := range map[string]backendReg{
		"name":   fakeReg("alpha", "OTHER_"),
		"prefix": fakeReg("other", "ALPHA_"),
		"empty":  {name: "x"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("duplicate %s did not panic", name)
				}
			}()
			registerBackend(reg.name, reg.prefix, reg.detect, reg.ctor)
		}()
	}
}

func TestBackendSelectRejectsLyingDeclaration(t *testing.T) {
	withBackends(t, backendReg{
		name: "liar", prefix: "LIAR_", detect: envHas("LIAR_PANE"),
		ctor: func() (Backend, error) { return &lyingBackend{Backend: newFakeBackend("liar")}, nil },
	})
	if _, err := selectBackend([]string{"LIAR_PANE=1"}, nil); err == nil || !strings.Contains(err.Error(), "state.dir") {
		t.Fatalf("got %v, want an error naming the undeclared-contract capability", err)
	}
}

func TestCapabilityNativeWhenDeclared(t *testing.T) {
	a := &Active{Backend: newFakeBackend("fakemux", CapLayout)}
	if !a.Has(CapLayout) {
		t.Error("declared layout must be native")
	}
	impl, native := a.Capability(CapLayout)
	if !native || impl != a.Backend {
		t.Errorf("Capability = %v, %v; want the backend, true", impl, native)
	}
}

func TestCapabilityFallbackWhenUndeclared(t *testing.T) {
	a := &Active{Backend: newFakeBackend("fakemux", CapLayout)}
	if a.Has(CapStateDir) {
		t.Error("undeclared state.dir must not be native")
	}
	// The fake implements StateDirer but does not declare it: the orch-owned
	// fallback answers, not the backend.
	dir, native, ok := capabilityAs[StateDirer](a, CapStateDir)
	if !ok || native {
		t.Fatalf("capabilityAs = %v, native %v, ok %v; want the fallback", dir, native, ok)
	}
	got, _ := dir.StateDir()
	if !strings.HasSuffix(got, filepath.Join(".local", "state", "orchestrate")) {
		t.Errorf("fallback state dir = %q", got)
	}
	if a.Has("no.such.capability") {
		t.Error("an unknown capability must not be native")
	}
}

func TestCapabilityListedOutForcesFallback(t *testing.T) {
	declare := []string{CapLayout, CapStateDir}
	cases := []struct {
		name   string
		native []string
		set    bool
		layout bool
		dir    bool
	}{
		{"no config keeps every declared capability native", nil, false, true, true},
		{"list keeps only what it names", []string{CapLayout}, true, true, false},
		{"empty list forces every fallback", []string{}, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withBackends(t, fakeReg("fakemux", "FAKEMUX_", declare...))
			cfg := backendConfig{}
			if c.set {
				cfg["fakemux"] = c.native
			}
			a, err := selectBackend([]string{"ORCH_BACKEND=fakemux"}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := a.Has(CapLayout); got != c.layout {
				t.Errorf("Has(layout) = %v, want %v", got, c.layout)
			}
			if got := a.Has(CapStateDir); got != c.dir {
				t.Errorf("Has(state.dir) = %v, want %v", got, c.dir)
			}
		})
	}
}

func TestCapabilityConfigOfOtherBackendDoesNotApply(t *testing.T) {
	withBackends(t, fakeReg("fakemux", "FAKEMUX_", CapLayout))
	a, err := selectBackend([]string{"ORCH_BACKEND=fakemux"}, backendConfig{"other": {}})
	if err != nil || !a.Has(CapLayout) {
		t.Fatalf("config for another backend changed this one: %v, %v", a, err)
	}
}

func TestCapabilityParseConfig(t *testing.T) {
	cfg, err := parseBackendConfig(map[string]any{
		"orca":  map[string]any{"native": []any{CapWorktreeManage, CapSetupHook}},
		"herd":  map[string]any{"native": []string{}},
		"other": map[string]any{"native": []any{CapLayout}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg["orca"], []string{CapWorktreeManage, CapSetupHook}) {
		t.Errorf("orca = %v", cfg["orca"])
	}
	if l, ok := cfg["herd"]; !ok || len(l) != 0 {
		t.Errorf("an empty list must stay present and empty, got %v, %v", l, ok)
	}
	bad := map[string]map[string]any{
		"unknown capability": {"orca": map[string]any{"native": []any{"telepathy"}}},
		"unknown key":        {"orca": map[string]any{"nativ": []any{}}},
		"not a list":         {"orca": map[string]any{"native": "layout"}},
		"not a string":       {"orca": map[string]any{"native": []any{1}}},
	}
	for name, raw := range bad {
		if _, err := parseBackendConfig(raw); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := parseBackendConfig(map[string]any{"orca": "x"}); err == nil {
		t.Error("a non-table backend entry must fail")
	}
	if got := backendNativeKey("orca"); got != "backend.orca.native" {
		t.Errorf("key = %q", got)
	}
}

func TestCapabilityRegistryHasEveryReservedName(t *testing.T) {
	for _, n := range []string{CapLayout, CapWorktreeManage, CapSetupHook, CapArchiveHook, CapBlockedReason, CapStateDir, CapWake, CapGate, CapDispatch, CapBrowser} {
		def, ok := lookupCapability(n)
		if !ok || def.Contract == nil || def.FallbackDoc == "" {
			t.Errorf("capability %q is not fully registered: %+v", n, def)
		}
	}
}

func TestCapabilitySetFallbackChecksContract(t *testing.T) {
	t.Cleanup(func() { _ = setCapabilityFallback(CapBrowser, nil) })
	if err := setCapabilityFallback(CapBrowser, struct{}{}); err == nil {
		t.Error("a value that does not implement the contract must be refused")
	}
	if err := setCapabilityFallback("nope", nil); err == nil {
		t.Error("an unknown capability must be refused")
	}
	if err := setCapabilityFallback(CapStateDir, defaultStateDirFallback{}); err != nil {
		t.Errorf("a valid fallback was refused: %v", err)
	}
}

func TestBackendEnvStripsForeignPrefixesKeepsOwn(t *testing.T) {
	withBackends(t, fakeReg("alpha", "ALPHA_"), fakeReg("beta", "BETA_"))
	base := []string{"ALPHA_PANE_KEY=a", "BETA_PANE_KEY=b", "PATH=/bin", "ALPHAZ=keep"}
	got := childEnv("alpha", base, map[string]string{"ORCH_ROLE": "lead", "BETA_SNEAK": "x", "ALPHA_EXTRA": "y"})
	want := []string{"ALPHA_PANE_KEY=a", "PATH=/bin", "ALPHAZ=keep", "ALPHA_EXTRA=y", "ORCH_ROLE=lead"}
	if !slices.Equal(got, want) {
		t.Errorf("childEnv = %v, want %v", got, want)
	}
}

func TestBackendEnvExtraOverridesBase(t *testing.T) {
	withBackends(t, fakeReg("alpha", "ALPHA_"))
	got := childEnv("alpha", []string{"ORCH_ROLE=old", "X=1"}, map[string]string{"ORCH_ROLE": "new"})
	if !slices.Equal(got, []string{"X=1", "ORCH_ROLE=new"}) {
		t.Errorf("childEnv = %v", got)
	}
}

func TestBackendEnvNewlyRegisteredPrefixIsStripped(t *testing.T) {
	withBackends(t, fakeReg("alpha", "ALPHA_"))
	base := []string{"ALPHA_A=1", "NEWMUX_PANE=2"}
	if got := childEnv("alpha", base, nil); !slices.Equal(got, base) {
		t.Fatalf("before registering newmux: %v", got)
	}
	registerBackend("newmux", "NEWMUX_", envHas("NEWMUX_PANE"), func() (Backend, error) { return newFakeBackend("newmux"), nil })
	if got := childEnv("alpha", base, nil); !slices.Equal(got, []string{"ALPHA_A=1"}) {
		t.Errorf("after registering newmux: %v", got)
	}
	if got := childEnv("newmux", base, nil); !slices.Equal(got, []string{"NEWMUX_PANE=2"}) {
		t.Errorf("newmux child should keep its own and lose alpha's: %v", got)
	}
}

func TestBackendFakeStartStripsForeignEnvFromProcess(t *testing.T) {
	withBackends(t, fakeReg("fakemux", "FAKEMUX_"), fakeReg("other", "OTHERMUX_"))
	t.Setenv("OTHERMUX_PANE_KEY", "leak")
	t.Setenv("FAKEMUX_PANE_KEY", "mine")
	f := newFakeBackend("fakemux")
	if _, err := f.Start(t.Context(), Spec{Agent: "x"}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.startEnv, "OTHERMUX_PANE_KEY=leak") {
		t.Error("foreign prefix reached the child")
	}
	if !slices.Contains(f.startEnv, "FAKEMUX_PANE_KEY=mine") {
		t.Error("own prefix was stripped")
	}
}

func TestBackendNormaliseStateMapsUnrecognisedToUnknown(t *testing.T) {
	for native, want := range map[string]State{"READY": StateIdle, "BUSY": StateWorking, "PROMPT": StateBlocked, "FINISHED": StateDone, "weird": StateUnknown, "": StateUnknown} {
		if got := normaliseState(native, fakeStates); got != want {
			t.Errorf("normaliseState(%q) = %q, want %q", native, got, want)
		}
	}
	// A table that maps to a value outside the vocabulary is not trusted.
	if got := normaliseState("x", map[string]State{"x": "RUNNING"}); got != StateUnknown {
		t.Errorf("out-of-vocabulary mapping gave %q", got)
	}
}

// scanCoreFiles reports core files (every non-test .go file that is not a
// backend_*.go file) that compare against a backend name or contain a backend
// env prefix in a string literal.
func scanCoreFiles(dir string, names, prefixes []string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var hits []string
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || strings.HasPrefix(n, "backend_") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			return nil, err
		}
		isName := func(x ast.Expr) (string, bool) {
			lit, ok := x.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(lit.Value)
			return s, err == nil && slices.Contains(names, s)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				s, _ := strconv.Unquote(x.Value)
				for _, p := range prefixes {
					if strings.Contains(s, p) {
						hits = append(hits, n+": string "+x.Value+" contains backend env prefix "+p)
					}
				}
			case *ast.BinaryExpr:
				if x.Op == token.EQL || x.Op == token.NEQ {
					for _, side := range []ast.Expr{x.X, x.Y} {
						if s, ok := isName(side); ok {
							hits = append(hits, n+": compares against backend name "+strconv.Quote(s))
						}
					}
				}
			case *ast.CaseClause:
				for _, c := range x.List {
					if s, ok := isName(c); ok {
						hits = append(hits, n+": case on backend name "+strconv.Quote(s))
					}
				}
			}
			return true
		})
	}
	return hits, nil
}

func TestBackendScanFlagsCoreFilesOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package main\n"+src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("clean.go", `func f(b string) bool { return b == "other" }`+"\n"+`// mentions ALPHA_ and "alpha" only in a comment`+"\n")
	write("cmp.go", `func f(b string) bool { return b == "alpha" }`)
	write("neq.go", `func f(b string) bool { return "alpha" != b }`)
	write("sw.go", `func f(b string) int { switch b { case "alpha": return 1 }; return 0 }`)
	write("env.go", `var k = "ALPHA_PANE_ID"`)
	// Exempt: backend files and tests.
	write("backend_alpha.go", `var k = "ALPHA_PANE_ID"; func f(b string) bool { return b == "alpha" }`)
	write("cmp_test.go", `var k = "ALPHA_X"`)
	hits, err := scanCoreFiles(dir, []string{"alpha"}, []string{"ALPHA_"})
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[string]bool{}
	for _, h := range hits {
		flagged[strings.SplitN(h, ":", 2)[0]] = true
	}
	for _, want := range []string{"cmp.go", "neq.go", "sw.go", "env.go"} {
		if !flagged[want] {
			t.Errorf("%s was not flagged; hits: %v", want, hits)
		}
	}
	for _, bad := range []string{"clean.go", "backend_alpha.go", "cmp_test.go"} {
		if flagged[bad] {
			t.Errorf("%s was flagged but is allowed; hits: %v", bad, hits)
		}
	}
}

// TestBackendCoreFilesNameNoRegisteredBackend runs the scan over the real
// tool/ directory with whatever backends the build registers. With backend
// files present, any core file that names one fails the build.
func TestBackendCoreFilesNameNoRegisteredBackend(t *testing.T) {
	hits, err := scanCoreFiles(".", registeredBackendNames(), registeredPrefixes())
	if err != nil {
		t.Fatal(err)
	}
	// doctor_setup.go is a machine-setup check outside the backend interface.
	pending := []string{"doctor_setup.go:"}
	hits = slices.DeleteFunc(hits, func(h string) bool {
		return slices.ContainsFunc(pending, func(p string) bool { return strings.HasPrefix(h, p) })
	})
	if len(hits) > 0 {
		t.Errorf("core files must use Backend.Has(cap), not backend names or env prefixes:\n%s", strings.Join(hits, "\n"))
	}
}
