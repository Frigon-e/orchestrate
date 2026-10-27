package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func ctxProbeServer(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf ran > '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func ctxNoServerPATH(t *testing.T) string {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(gitBin, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return dir
}

func ctxFakeNativeTS(t *testing.T, code, marker string) string {
	t.Helper()
	plat := map[string]string{"darwin": "darwin", "linux": "linux", "windows": "win32"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if plat == "" || arch == "" {
		t.Skip("native TS fixture unsupported on this platform")
	}
	store := filepath.Join(code, "node_modules", ".pnpm")
	if err := os.MkdirAll(filepath.Join(store, "typescript@7.0.1", "node_modules", "typescript"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := "typescript-" + plat + "-" + arch
	path := filepath.Join(store, "@typescript+"+pkg+"@7.0.1", "node_modules", "@typescript", pkg, "lib", "tsc")
	ctxProbeServer(t, path, marker)
	return path
}

func TestCtxTrustRepoLocalServersRequireExplicitTrust(t *testing.T) {
	for _, mode := range []string{"unlisted", "readonly", "denied", "trusted"} {
		t.Run(mode, func(t *testing.T) {
			c := newCtxHarness(t)
			marker := filepath.Join(t.TempDir(), "ran")
			local := filepath.Join(c.repo, "node_modules", ".bin", "ctx-probe")
			ctxProbeServer(t, local, marker)
			native := ctxFakeNativeTS(t, c.repo, marker)
			// Listing is required, and read-only mode still never runs local servers.
			switch mode {
			case "readonly", "trusted":
				if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), fmt.Appendf(nil, "ctx_trust = [%q]\n", c.repo), 0o644); err != nil {
					t.Fatal(err)
				}
			case "denied":
				if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), fmt.Appendf(nil, "ctx_deny = [%q]\n", c.repo), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "readonly" {
				pinCtx(t, c)
			}
			ctxNoServerPATH(t)
			t.Setenv("ORCH_TSLS", "")
			l := &langDef{ID: "probe", LSP: []lspCmd{{Bin: "ctx-probe"}}, LocalDirs: []string{"node_modules/.bin"}}
			bin, _, ok, why := langServerReason(c.repo, l)
			ts, tsWhy := tsServerChoice(c.repo)
			if mode == "trusted" {
				if !ok || bin != evalOr(local) || ts != evalOr(native) {
					t.Fatalf("trusted choices: %s %v %s %s", bin, ok, ts, why)
				}
				_, _ = openLSP(c.repo, l, c.repo)
				if raw, err := os.ReadFile(marker); err != nil || string(raw) != "ran" {
					t.Fatalf("trusted local server did not run: %q %v", raw, err)
				}
				_ = os.Remove(marker)
				_ = runTSServer([]tsJob{{root: c.repo, sym: "X"}}, map[tsJob]*symFact{}, t.TempDir(), c.repo, map[string]int{c.repo: 1})
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("trusted TS server did not run: %v", err)
				}
			} else {
				if ok || bin != "" || ts != "" || why == "" || tsWhy == "" {
					t.Fatalf("local server accepted: %s %v %s %s %s", bin, ok, ts, why, tsWhy)
				}
				_, _ = openLSP(c.repo, l, c.repo)
				note := runTSServer([]tsJob{{root: c.repo, sym: "X"}}, map[tsJob]*symFact{}, t.TempDir(), c.repo, map[string]int{c.repo: 1})
				if !strings.Contains(note, ctxServerSkip("ts")) {
					t.Fatal(note)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("repo-local binary ran: %v", err)
				}
				cfg, err := loadConfig(c.repo)
				if err != nil {
					t.Fatal(err)
				}
				e := newCtxEnv(c.repo, ctxOpts{Cfg: cfg})
				readiness := ctxReadiness(e)
				if !strings.Contains(readiness, ctxServerSkip("ts")) {
					t.Fatal(readiness)
				}
			}
		})
	}
}

func TestCtxServerOverridesPATHAndSymlinks(t *testing.T) {
	c := newCtxHarness(t)
	marker := filepath.Join(t.TempDir(), "ran")
	local := filepath.Join(c.repo, "server")
	ctxProbeServer(t, local, marker)
	outside := t.TempDir()
	alias := filepath.Join(outside, "ctx-probe")
	if err := os.Symlink(local, alias); err != nil {
		t.Fatal(err)
	}
	path := ctxNoServerPATH(t)
	t.Setenv("PATH", outside+string(os.PathListSeparator)+path)
	l := &langDef{ID: "probe", LSPEnv: "ORCH_PROBE_LS", LSP: []lspCmd{{Bin: "ctx-probe"}}}
	// Denial applies to overrides and PATH symlinks too.
	if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), fmt.Appendf(nil, "ctx_deny = [%q]\n", c.repo), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, readonly := range []bool{false, true} {
		t.Setenv("ORCH_CTX_ROOT", "")
		t.Setenv("ORCH_CTX_READONLY", "")
		if readonly {
			pinCtx(t, c)
		}
		if _, err := ctxServerBinary(c.repo, alias); err == nil {
			t.Fatal("symlink into repo accepted")
		}
		t.Setenv("ORCH_PROBE_LS", alias)
		if bin, _, ok, _ := langServerReason(c.repo, l); ok || bin != "" {
			t.Fatal("env symlink accepted")
		}
		t.Setenv("ORCH_PROBE_LS", "")
		if bin, _, ok, _ := langServerReason(c.repo, l); ok || bin != "" {
			t.Fatal("PATH symlink accepted")
		}
		if _, err := startLSP(alias, c.repo); err == nil {
			t.Fatal("execution boundary allowed symlink")
		}
		t.Setenv("ORCH_TSLS", alias)
		if bin, why := tsServerChoice(c.repo); bin != "" || why == "" {
			t.Fatalf("TS symlink accepted: %s %s", bin, why)
		}
		t.Setenv("ORCH_GOPLS", alias)
		e := newCtxEnv(c.repo, ctxOpts{})
		if e.Gopls != "" || len(e.Notes()) != 1 {
			t.Fatalf("Go symlink accepted: %s %v", e.Gopls, e.Notes())
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("rejected server ran")
	}
	external := filepath.Join(outside, "external-probe")
	ctxProbeServer(t, external, marker)
	t.Setenv("ORCH_PROBE_LS", external+" --stdio")
	if bin, args, ok, _ := langServerReason(c.repo, l); !ok || bin != evalOr(external) || strings.Join(args, " ") != "--stdio" {
		t.Fatalf("external env server rejected: %s %v %v", bin, args, ok)
	}
	t.Setenv("ORCH_PROBE_LS", "")
	l.LSP = []lspCmd{{Bin: "external-probe"}}
	if bin, _, ok, _ := langServerReason(c.repo, l); !ok || bin != evalOr(external) {
		t.Fatal("external PATH server rejected")
	}
	t.Setenv("ORCH_TSLS", external)
	if bin, _ := tsServerChoice(c.repo); bin != evalOr(external) {
		t.Fatal("external TS env rejected")
	}
	if err := os.Symlink(external, filepath.Join(outside, "tsgo")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_TSLS", "")
	if bin, _ := tsServerChoice(c.repo); bin != evalOr(external) {
		t.Fatal("external native TS PATH server rejected")
	}
	client, err := startLSP(external, c.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	// The process may finish after startLSP returns; openLSP waits for EOF.
	_, _ = openLSP(c.repo, l, c.repo)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("external server did not run: %v", err)
	}
}

// A shim found as tsc that links to a multi-call binary (vite-plus vp) must
// start under the name tsc, or it runs as something else and exits.
func TestStartLSPKeepsTheNameItWasFoundBy(t *testing.T) {
	c := newCtxHarness(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "tsc")
	if err := os.Symlink(self, shim); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "argv0")
	t.Setenv("ORCH_FAKE_ARGV0", marker)
	client, err := startLSP(shim, c.repo, "--lsp", "--stdio")
	if err != nil {
		t.Fatal(err)
	}
	<-client.dead
	werr := client.cmd.Wait()
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("%v (server: %v %v)", err, client.cmd.Args, werr)
	}
	if filepath.Base(string(got)) != "tsc" {
		t.Fatalf("started as %q, want the name tsc", got)
	}
}

func TestCtxTrustInstalledRepositoryOnly(t *testing.T) {
	c := newCtxHarness(t)
	t.Setenv("ORCH_SKILL_DIR", filepath.Join(c.repo, "skills", "orchestrate"))
	os.MkdirAll(os.Getenv("ORCH_SKILL_DIR"), 0755)
	if !ctxRepoTrusted(c.repo) {
		t.Fatal("installed repository not trusted")
	}
	other := t.TempDir()
	writeTree(t, other, map[string]string{"a": "a"})
	if _, err := git(other, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if ctxRepoTrusted(other) {
		t.Fatal("unlisted clone trusted")
	}
	os.WriteFile(os.Getenv("ORCH_CONFIG"), fmt.Appendf(nil, "ctx_deny = [%q]\n", c.repo), 0644)
	if ctxRepoTrusted(c.repo) {
		t.Fatal("installed repo bypassed deny")
	}
}

func TestCtxTrustCoversGitReposAndTheirWorktrees(t *testing.T) {
	c := newCtxHarness(t)
	work := filepath.Join(t.TempDir(), "worktree")
	c.git("worktree", "add", "--detach", work, "HEAD")
	if ctxRepoTrusted(c.repo) || ctxRepoTrusted(work) {
		t.Fatal("unlisted git repositories must not be trusted")
	}
	if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), fmt.Appendf(nil, "ctx_trust = [%q]\n", c.repo), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ctxRepoTrusted(c.repo) || !ctxRepoTrusted(work) {
		t.Fatal("listing must cover linked worktrees")
	}
	plain := t.TempDir()
	if ctxRepoTrusted(plain) {
		t.Fatal("a directory outside any git repo was trusted")
	}
	alias := filepath.Join(t.TempDir(), "plain-link")
	if err := os.Symlink(plain, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), fmt.Appendf(nil, "ctx_trust = [%q]\n", alias), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ctxRepoTrusted(plain) {
		t.Fatal("ctx_trust did not cover a non-git directory through a symlink")
	}
	// ctx_deny names the main checkout; the worktree is the same repo, and the
	// exclusion wins over a ctx_trust entry for it.
	cfg := fmt.Sprintf("ctx_trust = [%q]\nctx_deny = [%q]\n", c.repo, c.repo)
	if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if ctxRepoTrusted(c.repo) || ctxRepoTrusted(work) {
		t.Fatal("ctx_deny did not exclude the repo and its worktree")
	}
	if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pinCtx(t, c)
	if ctxRepoTrusted(c.repo) || ctxRepoTrusted(work) {
		t.Fatal("read-only mode trusted a git repo")
	}
}

func TestCtxTrustNativeTSServerFoundThroughAnotherWorktree(t *testing.T) {
	c := newCtxHarness(t)
	if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), fmt.Appendf(nil, "ctx_trust = [%q]\n", c.repo), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	native := ctxFakeNativeTS(t, c.repo, marker)
	work := filepath.Join(t.TempDir(), "worktree")
	c.git("worktree", "add", "--detach", work, "HEAD")
	ctxNoServerPATH(t)
	t.Setenv("ORCH_TSLS", "")
	if got, why := tsServerChoice(work); got != evalOr(native) || why != "" {
		t.Fatalf("worktree without node_modules: %q %q, want %q", got, why, evalOr(native))
	}
	pinCtx(t, c)
	if got, _ := tsServerChoice(work); got != "" {
		t.Fatalf("read-only mode picked a server from the repo family: %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("server ran: %v", err)
	}
}
