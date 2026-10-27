package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func okLook(string) (string, error) { return "/bin/x", nil }

func hasMsg(fs []finding, sev, sub string) bool {
	for _, f := range fs {
		if f.severity == sev && strings.Contains(f.msg, sub) {
			return true
		}
	}
	return false
}

func mustWrite(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestSetupFindingsOrchLink(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	home := t.TempDir()
	if !hasMsg(setupFindings(home, home, filepath.Join(home, "r"), okLook), "warn", ".local/bin/orch is missing") {
		t.Fatal("want missing warn")
	}
	bin := filepath.Join(home, ".local", "bin", "orch")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.Symlink(filepath.Join(home, "nope"), bin)
	if !hasMsg(setupFindings(home, home, filepath.Join(home, "r"), okLook), "error", "dangling") {
		t.Fatal("want dangling error")
	}
	target := filepath.Join(home, "real")
	mustWrite(t, target, "x", 0o755)
	os.Remove(bin)
	os.Symlink(target, bin)
	fs := setupFindings(home, home, filepath.Join(home, "r"), okLook)
	if hasMsg(fs, "warn", ".local/bin/orch") || hasMsg(fs, "error", ".local/bin/orch") {
		t.Fatalf("unexpected: %v", fs)
	}
}

func TestSetupFindingsSkillLinks(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	home := t.TempDir()
	root := filepath.Join(home, "r")
	if !hasMsg(setupFindings(home, home, root, okLook), "warn", ".agents/skills/orchestrate is missing") {
		t.Fatal("want missing warn")
	}
	p := filepath.Join(home, ".agents", "skills", "orchestrate")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.Symlink(filepath.Join(home, "nope"), p)
	if !hasMsg(setupFindings(home, home, root, okLook), "error", ".agents/skills/orchestrate") {
		t.Fatal("want dangling error")
	}
	os.Remove(p)
	mustWrite(t, filepath.Join(p, "SKILL.md"), "x", 0o644)
	fs := setupFindings(home, home, root, okLook)
	if hasMsg(fs, "error", ".agents/skills/orchestrate") || hasMsg(fs, "warn", ".agents/skills/orchestrate") {
		t.Fatalf("unexpected: %v", fs)
	}
}

func TestSetupFindingsHooks(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	home := t.TempDir()
	root := filepath.Join(home, "r")
	if !hasMsg(setupFindings(home, home, root, okLook), "warn", "settings.json is missing") {
		t.Fatal("want absent warn")
	}
	set := filepath.Join(home, ".claude", "settings.json")
	mustWrite(t, set, "{nope", 0o644)
	if !hasMsg(setupFindings(home, home, root, okLook), "warn", "settings.json not readable") {
		t.Fatal("want unreadable warn")
	}
	mustWrite(t, set, `{"hooks":{"Stop":[{"hooks":[{"command":"echo hi"}]}]}}`, 0o644)
	if !hasMsg(setupFindings(home, home, root, okLook), "warn", "no orchestrate hook") {
		t.Fatal("want unregistered warn")
	}
	cfg := `{"hooks":{"Stop":[{"hooks":[{"command":"\"$HOME/.agents/skills/orchestrate/hooks/claude/stop.sh\""}]}]}}`
	mustWrite(t, set, cfg, 0o644)
	if !hasMsg(setupFindings(home, home, root, okLook), "error", "stop.sh") {
		t.Fatal("want missing script error")
	}
	script := filepath.Join(home, ".agents", "skills", "orchestrate", "hooks", "claude", "stop.sh")
	mustWrite(t, script, "#!/bin/sh\n", 0o644)
	if !hasMsg(setupFindings(home, home, root, okLook), "error", "stop.sh") {
		t.Fatal("want not executable error")
	}
	os.Chmod(script, 0o755)
	fs := setupFindings(home, home, root, okLook)
	if hasMsg(fs, "error", "stop.sh") || hasMsg(fs, "warn", "no orchestrate hook") {
		t.Fatalf("unexpected: %v", fs)
	}
}

func TestSetupFindingsTools(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "r")
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	t.Setenv("HERDR_ENV", "")
	fs := setupFindings(home, home, root, missing)
	if !hasMsg(fs, "error", "git is not") || !hasMsg(fs, "warn", "go is not") || hasMsg(fs, "error", "herdr") {
		t.Fatalf("got %v", fs)
	}
	t.Setenv("HERDR_ENV", "1")
	if !hasMsg(setupFindings(home, home, root, missing), "error", "herdr is not") {
		t.Fatal("want herdr error")
	}
	fs = setupFindings(home, home, root, okLook)
	if hasMsg(fs, "error", "is not on PATH") || hasMsg(fs, "warn", "is not on PATH") {
		t.Fatalf("unexpected: %v", fs)
	}
}

func TestSetupFindingsClaudeTrust(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "wt", "repo", ".orchestrate")
	dir := filepath.Dir(root)
	cj := filepath.Join(home, ".claude.json")
	t.Setenv("HERDR_ENV", "")
	mustWrite(t, cj, `{"projects":{}}`, 0o644)
	if hasMsg(setupFindings(home, home, root, okLook), "warn", "trust prompt") {
		t.Fatal("no check outside herdr")
	}
	t.Setenv("HERDR_ENV", "1")
	if !hasMsg(setupFindings(home, home, root, okLook), "warn", "trust prompt") {
		t.Fatal("want untrusted warn")
	}
	mustWrite(t, cj, `{"projects":{"`+dir+`":{"hasTrustDialogAccepted":true}}}`, 0o644)
	if hasMsg(setupFindings(home, home, root, okLook), "warn", "trust prompt") {
		t.Fatal("trusted dir must not warn")
	}
	os.Remove(cj)
	if hasMsg(setupFindings(home, home, root, okLook), "warn", "trust prompt") {
		t.Fatal("unreadable file is skipped")
	}
}

func TestSetupFindingsRootOwner(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	home := t.TempDir()
	root := filepath.Join(home, "r")
	repo := filepath.Join(home, "repo")
	other := filepath.Join(home, "other")
	os.MkdirAll(repo, 0o755)
	os.MkdirAll(other, 0o755)
	mustWrite(t, filepath.Join(root, ".repo"), repo+"\n", 0o644)
	if hasMsg(setupFindings(home, repo, root, okLook), "warn", "belongs to") {
		t.Fatal("same owner must not warn")
	}
	mustWrite(t, filepath.Join(root, ".repo"), other+"\n", 0o644)
	if !hasMsg(setupFindings(home, repo, root, okLook), "warn", "belongs to") {
		t.Fatal("want owner mismatch warn")
	}
}

func TestSetupFindingsPrunableWorktree(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	os.MkdirAll(repo, 0o755)
	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(repo, "init", "-q")
	run(repo, "commit", "-q", "--allow-empty", "-m", "init")
	root := filepath.Join(home, "r")
	if hasMsg(setupFindings(home, repo, root, okLook), "warn", "prunable") {
		t.Fatal("clean repo must not warn")
	}
	wt := filepath.Join(home, "wt")
	run(repo, "worktree", "add", "-q", "-b", "b", wt)
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if !hasMsg(setupFindings(home, repo, root, okLook), "warn", "prunable") {
		t.Fatal("want prunable warn")
	}
}
