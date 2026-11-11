package guard

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// sfEnv: a git repo, a state dir and stubs for rg, orch and jbcontext alone on PATH.
func sfEnv(t *testing.T) (repo string) {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"rg", "orch", "jbcontext"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repo, _ = filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("ORCH_TOOL_GUARD_STATE_DIR", t.TempDir())
	for _, k := range []string{"ORCH_TOOL_GUARD", "ORCH_TOOL_GUARD_SKIP", "ORCH_LOOKUP_COACH", "ORCH_LOOKUP_LOG"} {
		t.Setenv(k, "")
	}
	return repo
}

func sfEvent(t *testing.T, repo, name string) *Event {
	return piEvent(t, map[string]any{"hook_event_name": name, "session_id": "s1", "cwd": repo, "prompt": "go"})
}

// A pi prompt refreshes its checkout's jbcontext index in the background, at
// most once per jbRefreshEvery for each checkout. Claude prompts leave it to
// jbcontext's own session hooks.
func TestPiPromptRefreshesTheIndexOncePerCheckout(t *testing.T) {
	repo := sfEnv(t)
	t.Setenv("ORCH_JB_REFRESH", "")
	other, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(other, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var started []string
	old := jbRefreshStart
	t.Cleanup(func() { jbRefreshStart = old })
	jbRefreshStart = func(root string) { started = append(started, root) }
	claude := func(cwd string) { Coach(sfEvent(t, cwd, "UserPromptSubmit"), &bytes.Buffer{}) }
	pi := func(cwd string) {
		PiCoach(piEvent(t, map[string]any{"event": "before_agent_start", "session": "p1", "cwd": cwd}))
	}
	claude(repo)
	if len(started) != 0 {
		t.Fatalf("a Claude prompt started a refresh: %v", started)
	}
	pi(filepath.Join(repo, "."))
	pi(repo)
	pi(other) // a worktree is its own checkout
	if want := []string{repo, other}; !reflect.DeepEqual(started, want) {
		t.Fatalf("started %v, want %v", started, want)
	}
	// Once the wait is over, the next prompt refreshes again.
	for _, stamp := range must(filepath.Glob(filepath.Join(StateDir(), "jbrefresh", "*"))) {
		old := time.Now().Add(-jbRefreshEvery - time.Minute)
		if err := os.Chtimes(stamp, old, old); err != nil {
			t.Fatal(err)
		}
	}
	claude(repo)
	pi(repo)
	t.Setenv("ORCH_JB_REFRESH", "off")
	pi(other)
	if len(started) != 3 || started[2] != repo {
		t.Fatalf("after the wait and with ORCH_JB_REFRESH=off: %v", started)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
