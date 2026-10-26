package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionFastPathNoInitialization(t *testing.T) {
	dir := t.TempDir() // No git repository, config, model, or retrieval backend.
	marker := filepath.Join(dir, "unexpected-backend")
	tripwire := filepath.Join(dir, "tripwire")
	if err := os.WriteFile(tripwire, []byte("#!/bin/sh\nprintf called > "+shellQuote(marker)+"\nexit 91\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "jbcontext", "gopls", "rg", "pi", "claude"} {
		if err := os.Symlink(tripwire, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("ORCH_CONFIG", filepath.Join(dir, "invalid-config"))
	t.Setenv("ORCH_CTX_ROOT", filepath.Join(dir, "missing-repo"))
	if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("not valid TOML = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"version", "--version", "-v", "-V"} {
		var out, errw bytes.Buffer
		a := &app{cwd: dir, out: &out, errw: &errw}
		if code := a.run([]string{alias}); code != 0 || out.String() != orchVersion+"\n" || errw.Len() != 0 {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q", alias, code, out.String(), errw.String())
		}
		if strings.Count(out.String(), "\n") != 1 {
			t.Fatalf("version is not a bare single line: %q", out.String())
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("version initialized a backend: %v", err)
	}
	var out, errw bytes.Buffer
	a := &app{cwd: dir, out: &out, errw: &errw}
	if code := a.run([]string{"--version", "unexpected"}); code != 2 {
		t.Fatalf("version silently ignored extra arguments: exit %d", code)
	}
}

// Exercise the user-facing launcher with a complete temporary copy of tool/,
// not the shared cache or checkout. The real Go tool builds every binary;
// a thin wrapper records rebuilds and simulates a toolchain identity change.
func TestCtxCLILauncherFingerprint(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	skill := filepath.Join(root, "skill")
	tool := filepath.Join(skill, "tool")
	if err := os.MkdirAll(filepath.Join(skill, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-R", ".", tool).CombinedOutput(); err != nil {
		t.Fatalf("copy tool/: %v\n%s", err, out)
	}
	launcher, err := os.ReadFile("../scripts/orch")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(skill, "scripts", "orch")
	if err := os.WriteFile(path, launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "builds")
	wrapper := "#!/bin/sh\nif [ \"$1\" = build ]; then printf 'build\\n' >> " + shellQuote(log) + "; fi\n" +
		"if [ \"$1\" = version ]; then " + shellQuote(goBin) + " \"$@\"; printf '%s\\n' \"${ORCH_TEST_TOOLCHAIN:-original}\"; exit; fi\nexec " + shellQuote(goBin) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "go"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	// A malformed config and invalid root would fail any ctx/backend setup.
	t.Setenv("ORCH_CONFIG", filepath.Join(root, "config.toml"))
	t.Setenv("ORCH_CTX_ROOT", filepath.Join(root, "missing-repo"))
	if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("invalid = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	launch := path
	run := func(alias string, builds int) string {
		t.Helper()
		cmd := exec.Command(launch, alias)
		cmd.Dir = root // Deliberately outside any git repository.
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("launcher %s: %v\nstdout: %s\nstderr: %s", alias, err, out, &stderr)
		}
		if strings.TrimSpace(string(out)) == "" || strings.Count(string(out), "\n") != 1 {
			t.Fatalf("not a bare version: %q", out)
		}
		data, err := os.ReadFile(log)
		if err != nil || strings.Count(string(data), "build\n") != builds {
			t.Fatalf("want %d builds, got %q: %v", builds, data, err)
		}
		return string(out)
	}
	first := run("version", 1)
	if got := run("--version", 1); got != first {
		t.Fatalf("unchanged tree rebuilt: %q != %q", got, first)
	}
	// A PATH entry such as ~/.local/bin/orch links to the launcher, possibly
	// through a relative link: it must find the skill, not the link's directory.
	links := filepath.Join(root, "links")
	if err := os.MkdirAll(links, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(links, "abs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("abs", filepath.Join(links, "orch")); err != nil {
		t.Fatal(err)
	}
	launch = filepath.Join(links, "orch")
	if got := run("version", 1); got != first {
		t.Fatalf("launcher through a symlink: %q != %q", got, first)
	}
	launch = path
	policy := filepath.Join(tool, "pi_ctx_only.ts")
	st, err := os.Stat(policy)
	if err != nil {
		t.Fatal(err)
	}
	appendFile := func(path, extra string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString(extra)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("append %s: %v %v", path, writeErr, closeErr)
		}
	}
	appendFile(policy, "\n// launcher fingerprint regression\n")
	if err := os.Chtimes(policy, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	second := run("version", 2)
	if err := os.Chtimes(policy, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	run("--version", 2) // An mtime-only change must not invalidate content.
	if second == first {
		t.Fatal("embedded policy change did not change the version")
	}
	appendFile(filepath.Join(tool, "ask.html.tmpl"), "\n<!-- launcher fingerprint regression -->\n")
	run("--version", 3) // Every embedded asset, not only the TS policy.
	extra := filepath.Join(tool, "launcher_removable.go")
	if err := os.WriteFile(extra, []byte("package main\nconst launcherRemovable = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("version", 4)
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	run("--version", 5) // Deletion is invisible to the old newer-than test.
	t.Setenv("ORCH_TEST_TOOLCHAIN", "changed")
	last := run("version", 6)
	if got := run("--version", 6); got != last {
		t.Fatalf("unchanged toolchain rebuilt: %q != %q", got, last)
	}
	t.Logf("temporary tool/ launcher: unchanged and mtime-only trees reused cache; policy, template, added/deleted source and toolchain changes rebuilt (%d builds)", 6)
}
