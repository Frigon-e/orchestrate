package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The herdr backend passes the shared conformance suite against the herdr fake
// (the test binary acting as `herdr`, see ORCH_FAKE_HERDR).
func TestConformanceHerdr(t *testing.T) {
	h := setup(t)
	t.Setenv("ORCH_FAKE_HERDR_ECHO", "1")
	cfg := filepath.Join(h.tmp, "herdr-config.toml")
	if err := os.WriteFile(cfg, []byte("[worktrees]\ndirectory = \""+filepath.Join(h.tmp, "wts")+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_HERDR_CONFIG", cfg)
	RunConformance(t, func(t testing.TB) Backend { return &conformanceHerdr{herdrBackend: &herdrBackend{}, repo: h.repo, dir: filepath.Join(h.tmp, "wts")} })
}

// conformanceHerdr gives the suite's repo-less worktree spec the repo and path
// herdr needs; the rest is the real backend.
type conformanceHerdr struct {
	*herdrBackend
	repo, dir string
}

func (c *conformanceHerdr) CreateWorktree(ctx context.Context, w WorktreeSpec) (Worktree, error) {
	w.Repo, w.Path = c.repo, filepath.Join(c.dir, w.Name)
	return c.herdrBackend.CreateWorktree(ctx, w)
}

// A lead.json written before the Backend field existed reads as herdr, is
// rewritten with it, and the role marker keyed by the same pane id still counts.
func TestLegacyLeadReadsAsHerdrAndKeepsRoleMarker(t *testing.T) {
	h := setup(t)
	h.task("t1")
	dir := h.dir("t1")
	legacy := `{"id":"t1","name":"lead-t1","harness":"claude","model":"sonnet","pane_id":"w1:p7","tab_id":"w1:t1","spawned_at":1,"owner":"w1:p0"}`
	if err := os.WriteFile(filepath.Join(dir, "lead.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	l := h.lead("t1")
	if l.Backend != "herdr" {
		t.Fatalf("Backend = %q, want herdr", l.Backend)
	}
	if err := saveLead(dir, l); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "lead.json"))
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil || got["backend"] != "herdr" {
		t.Fatalf("rewritten lead.json = %s (%v)", raw, err)
	}

	// The role marker is keyed by the pane id, as before the refactor.
	if err := os.MkdirAll(rolesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rolesDir(), "w1:p0"), []byte(`{"session":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !orchestratorActive(hookPayload{}) {
		t.Fatal("role marker for the existing pane id was not found")
	}
}

// Outside a backend, commands that need one fail with the registry message;
// plan, verify and gate still run.
func TestOutsideBackendNeedsRegistryMessage(t *testing.T) {
	h := setup(t)
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("HERDR_WORKSPACE_ID", "")
	for _, args := range [][]string{{"spawn", "t1"}, {"adopt", "t1"}, {"become"}} {
		code, out := h.run(args...)
		if code == 0 || !strings.Contains(out, "no multiplexer backend found") || !strings.Contains(out, "herdr") {
			t.Errorf("orch %v = %d %q, want the registry message", args, code, out)
		}
	}
	for _, cmd := range []string{"plan", "verify", "gate"} {
		if commandNeedsBackend(cmd) {
			t.Errorf("%s must not need a backend", cmd)
		}
	}
}

// Outside herdr, prune still runs and keeps what it cannot place.
func TestPruneOutsideBackendKeepsUnknownState(t *testing.T) {
	h := setup(t)
	t.Setenv("HERDR_ENV", "")
	code, out := h.run("prune")
	if code != 0 {
		t.Fatalf("prune exited %d: %s", code, out)
	}
}

// Herdr names stay inside the backend files.
func TestHerdrNamesStayInBackendFiles(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	out, _ := exec.Command("rg", "-n", `HERDR_|herdr `, ".",
		"--glob", "!backend_herdr*", "--glob", "!*_test.go", "--glob", "!gate_pane.go", "--glob", "!doctor_setup.go", "--glob", "!gate.go").Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Comments may name herdr.
		_, rest, _ := strings.Cut(line, ":")
		_, text, _ := strings.Cut(rest, ":")
		if !strings.HasPrefix(strings.TrimSpace(text), "//") && !strings.Contains(text, "// ") {
			t.Errorf("herdr named outside the backend files: %s", line)
		}
	}
}
