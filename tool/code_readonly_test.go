package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pinCtx(t *testing.T, c *codeHarness) {
	t.Helper()
	t.Setenv("ORCH_CTX_ROOT", evalOr(c.repo))
	t.Setenv("ORCH_CTX_READONLY", "1")
}

func TestReadonlyDoesNotBuildOrRefresh(t *testing.T) {
	for _, rev := range []string{"", strings.Repeat("b", 40)} {
		t.Run(rev, func(t *testing.T) {
			c := newCodeHarness(t)
			pinCtx(t, c)
			if err := os.WriteFile(filepath.Join(c.jbDir, "rev"), []byte(rev), 0o644); err != nil {
				t.Fatal(err)
			}
			e, err := codeEnvFor(c.a)
			if err != nil {
				t.Fatal(err)
			}
			e.checkIndex(true)
			wantContains(t, strings.Join(e.Notes(), "\n"), "read-only ctx: index builds and refreshes are disabled")
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

func TestReadonlyRejectsTraversalAndSymlinks(t *testing.T) {
	c := newCodeHarness(t)
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
	e, err := codeEnvFor(c.a)
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

func TestReadonlyNeedsAnAbsolutePinnedRoot(t *testing.T) {
	c := newCodeHarness(t)
	pinCtx(t, c)
	t.Setenv("ORCH_CTX_ROOT", "relative/root")
	if _, err := codeEnvFor(c.a); err == nil || !strings.Contains(err.Error(), "requires an absolute ORCH_CTX_ROOT") {
		t.Fatalf("relative pinned root accepted: %v", err)
	}
}
