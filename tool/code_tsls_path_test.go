package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeTsc writes an executable tsc that prints version.
func fakeTsc(t *testing.T, dir, version string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "tsc")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'Version "+version+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTSServerChoiceTscOnPath(t *testing.T) {
	t.Setenv("ORCH_TSLS", "")
	repo := t.TempDir()
	cases := []struct {
		name    string
		version string // empty: no tsc on PATH
		inRepo  bool
		want    bool
	}{
		{"tsc 7 accepted", "7.0.2", false, true},
		{"tsc 6 rejected", "6.0.3", false, false},
		{"none found", "", false, false},
		{"tsc 7 in untrusted checkout rejected", "7.0.2", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			var tsc string
			if c.version != "" {
				dir := bin
				if c.inRepo {
					dir = filepath.Join(repo, "node_modules", ".bin")
				}
				tsc = fakeTsc(t, dir, c.version)
			}
			if c.inRepo {
				t.Setenv("PATH", filepath.Join(repo, "node_modules", ".bin"))
			} else {
				t.Setenv("PATH", bin)
			}
			tscNativeCache.Range(func(k, _ any) bool { tscNativeCache.Delete(k); return true })
			got, _ := tsServerChoice(repo)
			if c.want {
				if got != tsc {
					t.Fatalf("got %q, want %q", got, tsc)
				}
			} else if got != "" {
				t.Fatalf("got %q, want none", got)
			}
		})
	}
}

// An untrusted checkout's tsc must not run, not even for --version.
func TestTSServerChoiceDoesNotRunUntrustedTsc(t *testing.T) {
	t.Setenv("ORCH_TSLS", "")
	repo := t.TempDir()
	dir := filepath.Join(repo, "node_modules", ".bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	script := "#!/bin/sh\n/usr/bin/touch '" + marker + "'\necho 'Version 7.0.2'\n"
	if err := os.WriteFile(filepath.Join(dir, "tsc"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	tscNativeCache.Range(func(k, _ any) bool { tscNativeCache.Delete(k); return true })
	got, _ := tsServerChoice(repo)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("untrusted tsc was executed")
	}
	if got != "" {
		t.Fatalf("got %q, want none", got)
	}
}

// The newest native server wins: the repo-local one from the pnpm store, tsgo
// or tsc on PATH. A tie goes to the repo-local one, then tsgo, then tsc.
func TestTSServerChoicePicksNewestVersion(t *testing.T) {
	plat := map[string]string{"darwin": "darwin", "linux": "linux", "windows": "win32"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	cases := []struct {
		name      string
		repo      string // pnpm store version, empty: none
		trusted   bool
		tsc, tsgo string // PATH versions, empty: none
		override  bool
		want      string // repo, tsc, tsgo, override or none
	}{
		{"PATH newer than repo", "7.0.2", true, "7.1.0", "", false, "tsc"},
		{"repo newer than PATH", "7.1.0", true, "7.0.2", "", false, "repo"},
		{"equal prefers repo", "7.0.2", true, "7.0.2", "", false, "repo"},
		{"untrusted repo ignored", "7.9.0", false, "7.0.2", "", false, "tsc"},
		{"override wins", "7.0.2", true, "7.1.0", "", true, "override"},
		{"PATH tsc 6 ignored", "", true, "6.0.3", "", false, "none"},
		{"tsgo beats tsc on a tie", "", true, "7.0.2", "7.0.2", false, "tsgo"},
		{"newer tsc beats tsgo", "", true, "7.2.0", "7.0.2", false, "tsc"},
		{"release beats prerelease", "7.0.0-dev.20260101.1", true, "7.0.0", "", false, "tsc"},
		{"minor beats patch", "7.0.10", true, "7.1.0", "", false, "tsc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newCodeHarness(t)
			repo := h.repo
			t.Setenv("ORCH_TSLS", "")
			if c.trusted {
				t.Setenv("ORCH_SKILL_DIR", filepath.Join(repo, "skills", "orchestrate"))
				os.MkdirAll(os.Getenv("ORCH_SKILL_DIR"), 0o755)
			}
			var repoBin string
			if c.repo != "" {
				pnpm := filepath.Join(repo, "web", "node_modules", ".pnpm")
				repoBin = filepath.Join(pnpm, "@typescript+typescript-"+plat+"-"+arch+"@"+c.repo, "node_modules", "@typescript", "typescript-"+plat+"-"+arch, "lib", "tsc")
				os.MkdirAll(filepath.Join(pnpm, "typescript@"+c.repo, "node_modules", "typescript"), 0o755)
				os.MkdirAll(filepath.Dir(repoBin), 0o755)
				os.WriteFile(repoBin, []byte("#!/bin/sh\n"), 0o755)
			}
			bin := t.TempDir()
			// Trust needs git, and nothing else on PATH may look like a server.
			gitBin, err := exec.LookPath("git")
			if err != nil {
				t.Skip("git not found")
			}
			if err := os.Symlink(gitBin, filepath.Join(bin, "git")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			var tsc, tsgo string
			if c.tsc != "" {
				tsc = fakeTsc(t, bin, c.tsc)
			}
			if c.tsgo != "" {
				tsgo = filepath.Join(bin, "tsgo")
				os.WriteFile(tsgo, []byte("#!/bin/sh\necho 'Version "+c.tsgo+"'\n"), 0o755)
			}
			if c.override {
				t.Setenv("ORCH_TSLS", "/x/override")
			}
			tscNativeCache.Range(func(k, _ any) bool { tscNativeCache.Delete(k); return true })
			want := map[string]string{"repo": repoBin, "tsc": tsc, "tsgo": tsgo, "override": "/x/override", "none": ""}[c.want]
			got, _ := tsServerChoiceUncached(repo)
			if c.want == "repo" || c.want == "tsgo" {
				got, want = evalOr(got), evalOr(want)
			}
			if got != want {
				t.Fatalf("got %q, want %s %q", got, c.want, want)
			}
		})
	}
}
