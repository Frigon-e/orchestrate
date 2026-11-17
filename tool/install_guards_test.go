package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude stands in for the claude CLI: it logs its arguments to $HOME/claude.log
// and edits $HOME/.claude.json the way `claude mcp add|remove --scope user` does.
const fakeClaude = `#!/bin/sh
echo "$@" >> "$HOME/claude.log"
[ "$1 $2" = "mcp add" ] || [ "$1 $2" = "mcp remove" ] || exit 2
python3 - "$HOME/.claude.json" "$@" <<'PY'
import json, os, sys
path, args = sys.argv[1], sys.argv[2:]
d = json.load(open(path)) if os.path.exists(path) else {}
servers = d.setdefault("mcpServers", {})
if args[1] == "add":
    rest = args[args.index("--") + 1:]
    servers[args[4]] = {"type": "stdio", "command": rest[0], "args": rest[1:], "env": {}}
else:
    servers.pop(args[4], None)
json.dump(d, open(path, "w"))
PY
`

// installGuards runs scripts/install-guards with a throwaway HOME, so the real
// ~/.claude and ~/.pi are never read or written, and a fake claude first on PATH,
// so the real one never registers anything.
func installGuards(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	fakeBin := filepath.Join(home, "fakebin")
	if _, err := os.Stat(filepath.Join(fakeBin, "claude")); err != nil {
		if err := os.MkdirAll(fakeBin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fakeBin, "claude"), []byte(fakeClaude), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs("../scripts/install-guards")
	if err != nil {
		t.Fatal(err)
	}
	// The guard build keeps the real Go caches: under the throwaway HOME it would
	// download modules again, and their read-only files break TempDir cleanup.
	caches, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	dirs := strings.Fields(string(caches))
	cmd := exec.Command(script, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"), "CLAUDE_CONFIG_DIR=", "GOMODCACHE="+dirs[0], "GOCACHE="+dirs[1])
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestInstallGuards(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("install-guards needs python3")
	}
	repo, _ := filepath.Abs("..")
	want := filepath.Join(repo, "tool", "pi_ctx_reminder.ts")
	home := t.TempDir()
	ext := filepath.Join(home, ".pi", "agent", "extensions", "ctx-reminder.ts")
	settings := filepath.Join(home, ".claude", "settings.json")

	out, code := installGuards(t, home, "--check")
	if code != 1 || !strings.Contains(out, "pi: "+ext+" is missing") || !strings.Contains(out, "claude: missing:") {
		t.Fatalf("check on an empty HOME: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(ext); err == nil {
		t.Fatal("--check created the extension")
	}
	if _, err := os.Stat(settings); err == nil {
		t.Fatal("--check wrote the settings")
	}

	// A regular file in the way is moved aside, never deleted.
	if err := os.MkdirAll(filepath.Dir(ext), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ext, []byte("old extension"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code = installGuards(t, home, "--check"); code != 1 || !strings.Contains(out, "is a regular file") {
		t.Fatalf("check with a regular file: exit %d\n%s", code, out)
	}
	if out, code = installGuards(t, home); code != 0 {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	if target, err := os.Readlink(ext); err != nil || target != want {
		t.Fatalf("link = %q, %v; want %q", target, err, want)
	}
	backups, _ := filepath.Glob(ext + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	if data, _ := os.ReadFile(backups[0]); string(data) != "old extension" {
		t.Fatalf("backup holds %q", data)
	}
	if data, _ := os.ReadFile(settings); !strings.Contains(string(data), "orch-guard") {
		t.Fatalf("settings lack the hooks: %s", data)
	}
	if out, code = installGuards(t, home, "--check"); code != 0 {
		t.Fatalf("check after install: exit %d\n%s", code, out)
	}

	// Running it again changes nothing: no second backup, same link.
	if out, code = installGuards(t, home); code != 0 || !strings.Contains(out, "already linked") {
		t.Fatalf("second install: exit %d\n%s", code, out)
	}
	if again, _ := filepath.Glob(ext + ".bak-*"); len(again) != 1 {
		t.Fatalf("re-run left backups %v", again)
	}

	// A link to somewhere else is stale: --check says so, install repoints it.
	os.Remove(ext)
	os.Symlink("/elsewhere/ctx-reminder.ts", ext)
	if out, code = installGuards(t, home, "--check"); code != 1 || !strings.Contains(out, "/elsewhere/ctx-reminder.ts") {
		t.Fatalf("check with a foreign link: exit %d\n%s", code, out)
	}
	if out, code = installGuards(t, home); code != 0 {
		t.Fatalf("repoint: exit %d\n%s", code, out)
	}
	if target, _ := os.Readlink(ext); target != want {
		t.Fatalf("link = %q after repoint", target)
	}

	if out, code = installGuards(t, home, "--remove"); code != 0 {
		t.Fatalf("remove: exit %d\n%s", code, out)
	}
	if _, err := os.Lstat(ext); err == nil {
		t.Fatal("--remove left the link")
	}
	if data, _ := os.ReadFile(settings); strings.Contains(string(data), "orch-guard") {
		t.Fatalf("--remove left the hooks: %s", data)
	}
	if left, _ := filepath.Glob(ext + ".bak-*"); len(left) != 1 {
		t.Fatalf("--remove touched the backup: %v", left)
	}
}

// --remove only unlinks its own link; a user's regular file stays.
func TestInstallGuardsRemoveKeepsARegularFile(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("install-guards needs python3")
	}
	home := t.TempDir()
	ext := filepath.Join(home, ".pi", "agent", "extensions", "ctx-reminder.ts")
	os.MkdirAll(filepath.Dir(ext), 0o755)
	os.WriteFile(ext, []byte("mine"), 0o644)
	if out, code := installGuards(t, home, "--remove"); code != 0 || !strings.Contains(out, "left "+ext+" alone") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(ext); string(data) != "mine" {
		t.Fatalf("file = %q", data)
	}
}

func claudeLog(home string) string {
	data, _ := os.ReadFile(filepath.Join(home, "claude.log"))
	return string(data)
}

func TestInstallGuardsRegistersTheMCPServer(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("install-guards needs python3")
	}
	repo, _ := filepath.Abs("..")
	cmd := filepath.Join(repo, "scripts", "orch")
	home := t.TempDir()

	if out, code := installGuards(t, home, "--check"); code != 1 || !strings.Contains(out, "orch MCP server not registered") {
		t.Fatalf("check when missing: exit %d\n%s", code, out)
	}
	if out, code := installGuards(t, home); code != 0 {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	wantLog := "mcp remove --scope user orch\nmcp add --scope user orch -- " + cmd + " mcp\n"
	if got := claudeLog(home); got != wantLog {
		t.Fatalf("claude calls:\n%s\nwant:\n%s", got, wantLog)
	}
	if out, code := installGuards(t, home, "--check"); code != 0 {
		t.Fatalf("check after install: exit %d\n%s", code, out)
	}

	// Already right: claude is not called again.
	if out, code := installGuards(t, home); code != 0 || !strings.Contains(out, "MCP server already registered") {
		t.Fatalf("second install: exit %d\n%s", code, out)
	}
	if got := claudeLog(home); got != wantLog {
		t.Fatalf("a no-op install called claude:\n%s", got)
	}

	// Pointing elsewhere: --check says so, install re-registers.
	cfg := filepath.Join(home, ".claude.json")
	os.WriteFile(cfg, []byte(`{"mcpServers":{"orch":{"command":"/elsewhere/orch","args":["mcp"]}}}`), 0o644)
	if out, code := installGuards(t, home, "--check"); code != 1 || !strings.Contains(out, "orch MCP server points to /elsewhere/orch mcp") {
		t.Fatalf("check with another command: exit %d\n%s", code, out)
	}
	if out, code := installGuards(t, home); code != 0 {
		t.Fatalf("repoint: exit %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(cfg); !strings.Contains(string(data), cmd) {
		t.Fatalf("not repointed: %s", data)
	}

	if out, code := installGuards(t, home, "--remove"); code != 0 {
		t.Fatalf("remove: exit %d\n%s", code, out)
	}
	if !strings.HasSuffix(claudeLog(home), "mcp remove --scope user orch\n") {
		t.Fatalf("remove did not call claude: %s", claudeLog(home))
	}
	if data, _ := os.ReadFile(cfg); strings.Contains(string(data), cmd) {
		t.Fatalf("--remove left the server: %s", data)
	}
}

// Without claude on PATH the step is skipped with a warning, not a failure.
func TestInstallGuardsWithoutClaudeWarns(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("install-guards needs python3")
	}
	// A PATH with go and python3 (links in a temp dir) and the system dirs, but no claude.
	tools := t.TempDir()
	for _, name := range []string{"go", "python3"} {
		if err := os.Symlink(mustLook(t, name), filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools+":/usr/bin:/bin")
	if _, err := exec.LookPath("claude"); err == nil {
		t.Skip("claude is in a system dir")
	}
	home := t.TempDir()
	script, _ := filepath.Abs("../scripts/install-guards")
	caches, _ := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	dirs := strings.Fields(string(caches))
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), "HOME="+home, "GOMODCACHE="+dirs[0], "GOCACHE="+dirs[1])
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "claude is not on PATH") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func mustLook(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skip(name + " not found")
	}
	return p
}
