package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// installGuards runs scripts/install-guards with a throwaway HOME, so the real
// ~/.claude and ~/.pi are never read or written.
func installGuards(t *testing.T, home string, args ...string) (string, int) {
	t.Helper()
	script, err := filepath.Abs("../scripts/install-guards")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, args...)
	cmd.Env = append(os.Environ(), "HOME="+home)
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
