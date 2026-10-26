package main

import (
	"os"
	"path/filepath"
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
