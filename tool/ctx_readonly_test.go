package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pinCtx(t *testing.T, c *ctxHarness) {
	t.Helper()
	t.Setenv("ORCH_CTX_ROOT", evalOr(c.repo))
	t.Setenv("ORCH_CTX_READONLY", "1")
}

func TestCtxReadonlyCommandsAndRoot(t *testing.T) {
	c := newCtxHarness(t)
	pinCtx(t, c)
	for _, sub := range []string{"refresh", "watch", "status", "brief", "nonesuch"} {
		err := cmdCtx(c.a, []string{sub})
		if err == nil {
			t.Fatalf("allowed %s", sub)
		}
	}
	c.a.cwd = t.TempDir()
	wantContains(t, c.must("ctx", "--readonly", "read", "pay/pay.go"), "type Gateway interface")
	wantContains(t, c.must("ctx", "batch", "-q", "read:pay/pay.go"), "type Gateway interface")
	plan := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(plan, []byte(`{"lookups":[{"kind":"read","path":"pay/pay.go"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := c.run("ctx", "batch", plan); code != 1 || !strings.Contains(out, "outside the pinned root") {
		t.Fatalf("batch plan outside root: exit %d, output %s", code, out)
	}
	t.Setenv("ORCH_CTX_ROOT", "")
	if code, out := c.run("ctx", "read", "pay/pay.go"); code != 2 || !strings.Contains(out, "requires an absolute ORCH_CTX_ROOT") {
		t.Fatalf("missing pinned root: exit %d, output %s", code, out)
	}
}

func TestCtxReadonlyDoesNotBuildOrRefresh(t *testing.T) {
	for _, rev := range []string{"", strings.Repeat("b", 40)} {
		t.Run(rev, func(t *testing.T) {
			c := newCtxHarness(t)
			pinCtx(t, c)
			if err := os.WriteFile(filepath.Join(c.jbDir, "rev"), []byte(rev), 0o644); err != nil {
				t.Fatal(err)
			}
			out := c.must("ctx", "find", "Code that starts a checkout")
			wantContains(t, out, "read-only ctx: index builds and refreshes are disabled")
			if strings.Contains(c.log(c.jbCalls), "index ") {
				t.Fatalf("index mutated: %s", c.log(c.jbCalls))
			}
			got, _ := os.ReadFile(filepath.Join(c.jbDir, "rev"))
			if string(got) != rev {
				t.Fatalf("revision changed: %s", got)
			}
		})
	}
}

func TestCtxReadonlyRejectsTraversalAndSymlinks(t *testing.T) {
	c := newCtxHarness(t)
	pinCtx(t, c)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(c.repo, "escape")); err != nil {
		t.Fatal(err)
	}
	traversal := filepath.Join(filepath.Dir(c.repo), "secret")
	if err := os.WriteFile(traversal, []byte("secret sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := ctxEnvFor(c.a)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, "../secret", "escape"} {
		if ls := e.Lines(p); len(ls) > 0 {
			t.Fatalf("read %s", p)
		}
		if _, err := e.Text(rgQuery{Pattern: "secret", Path: p}); err == nil {
			t.Fatalf("searched %s", p)
		}
		v := e.readLookup(ctxLookup{Path: p}, ctxFmt{})
		if v.Err == "" {
			t.Fatalf("read lookup %s", p)
		}
	}
	if c.log(c.rgCalls) != "" {
		t.Fatal("rg ran for rejected paths")
	}
	if ls := e.Lines("pay/pay.go"); len(ls) == 0 {
		t.Fatal("allowed repo file could not be read")
	}
	found := false
	for _, p := range e.files() {
		found = found || p == "pay/pay.go"
		if p == "escape" {
			t.Fatal("escape in tracked files")
		}
	}
	if !found {
		t.Fatal("allowed repo file missing from tracked files")
	}
}
