package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// piEnv puts rg, fd and orch (stubs) alone on PATH and the state in a temp dir.
func piEnv(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"rg", "fd", "orch"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	state := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("ORCH_TOOL_GUARD_STATE_DIR", state)
	for _, k := range []string{"ORCH_TOOL_GUARD", "ORCH_TOOL_GUARD_SKIP", "ORCH_LOOKUP_COACH", "ORCH_LOOKUP_LOG"} {
		t.Setenv(k, "")
	}
	return state
}

func piEvent(t *testing.T, v map[string]any) *Event {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := ParseEvent(raw)
	if !ok {
		t.Fatalf("event %s does not parse", raw)
	}
	return ev
}

func piBash(t *testing.T, session, command string, extra map[string]any) PiResponse {
	t.Helper()
	req := map[string]any{"event": "tool_call", "session": session, "cwd": t.TempDir(), "tool": "bash", "input": map[string]any{"command": command}, "resultHook": true}
	for k, v := range extra {
		req[k] = v
	}
	return PiPre(piEvent(t, req))
}

func TestPiGiveWayStartsAtTheThirdBlockInARowAndAnAllowedCallResetsIt(t *testing.T) {
	piEnv(t)
	for i := 1; i <= 4; i++ {
		res := piBash(t, "giveway", "grep -rn Normalize src", nil)
		if !res.Block {
			t.Fatalf("block %d: not blocked", i)
		}
		if got := strings.Contains(res.Reason, "Blocked 3 times"); got != (i >= 3) {
			t.Fatalf("block %d: give-way text present = %v", i, got)
		}
	}
	if res := piBash(t, "giveway", "ls", nil); res.Block {
		t.Fatalf("ls blocked: %q", res.Reason)
	}
	if res := piBash(t, "giveway", "grep -rn Normalize src", nil); !res.Block || strings.Contains(res.Reason, "Blocked 3 times") {
		t.Fatalf("the counter survived an allowed call: %+v", res)
	}
}

func TestPiFallbackBlocksOnlyTheFirstSearchWithoutAResultHook(t *testing.T) {
	piEnv(t)
	noHook := map[string]any{"resultHook": false}
	first := piBash(t, "fallback", "rg -n Foo src", noHook)
	if !first.Block || !strings.Contains(first.Reason, "Lookup order:") {
		t.Fatalf("first search: %+v", first)
	}
	if again := piBash(t, "fallback", "rg -n Foo src", noHook); again.Block {
		t.Fatalf("the retry was blocked: %q", again.Reason)
	}
	if other := piBash(t, "with-hook", "rg -n Foo src", nil); other.Block {
		t.Fatalf("a result hook exists, so nothing blocks: %q", other.Reason)
	}
}

func TestPiBuiltinSearchIsBlockedInsideTheRepoAndAllowedOutside(t *testing.T) {
	piEnv(t)
	repo := t.TempDir()
	repo, _ = filepath.EvalSymlinks(repo)
	for _, dir := range []string{".git", "src"} {
		if err := os.Mkdir(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	call := func(tool, path string) PiResponse {
		return PiPre(piEvent(t, map[string]any{"event": "tool_call", "session": "builtin", "cwd": repo, "tool": tool, "input": map[string]any{"pattern": "x", "path": path}, "resultHook": true}))
	}
	if res := call("grep", filepath.Join(repo, "src")); !res.Block || !strings.Contains(res.Reason, "deny-builtin-grep") {
		t.Fatalf("grep inside the repo: %+v", res)
	}
	if res := call("find", filepath.Join(repo, "src")); !res.Block || !strings.Contains(res.Reason, "deny-builtin-find") {
		t.Fatalf("find inside the repo: %+v", res)
	}
	if res := call("grep", "/etc"); res.Block {
		t.Fatalf("grep outside the repo was blocked: %q", res.Reason)
	}
}

// A result and its end event arrive in either order; the row waits for both.
func TestPiLookupRowGetsItsDurationWhicheverEventComesFirst(t *testing.T) {
	state := piEnv(t)
	cwd := t.TempDir()
	result := func(id string) {
		PiCoach(piEvent(t, map[string]any{"event": "tool_result", "session": "rows", "cwd": cwd, "turn": 1, "tool": "bash", "toolCallId": id, "input": map[string]any{"command": "rg -n Foo"}, "content": []string{"a.go:1:Foo"}}))
	}
	ended := func(id string, ms float64) {
		PiCoach(piEvent(t, map[string]any{"event": "tool_execution_end", "session": "rows", "cwd": cwd, "tool": "bash", "toolCallId": id, "durationMs": ms}))
	}
	result("a")
	if _, err := os.Stat(filepath.Join(state, "lookups.jsonl")); err == nil {
		t.Fatal("a row was written before its duration arrived")
	}
	ended("a", 12.5)
	ended("b", 40)
	result("b")
	data, err := os.ReadFile(filepath.Join(state, "lookups.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("rows = %d, want 2:\n%s", len(lines), data)
	}
	for i, want := range []float64{12.5, 40} {
		var row LookupLine
		if err := json.Unmarshal([]byte(lines[i]), &row); err != nil {
			t.Fatal(err)
		}
		if row.DurationMS == nil || *row.DurationMS != want || row.Harness != "pi" || row.Tool != "rg" {
			t.Fatalf("row %d = %+v", i, row)
		}
	}
}

func TestPiModeFailsOpenOnUnreadableInput(t *testing.T) {
	state := piEnv(t)
	if guardBin == "" {
		t.Skip("no orch-guard binary")
	}
	env := childEnv(nil, "ORCH_TOOL_GUARD_STATE_DIR="+state)
	for _, mode := range []string{"pre", "coach"} {
		for _, payload := range []string{"", "not json", "[1]"} {
			code, stdout, _ := execHook(t, env, []byte(payload), guardBin, mode, "--harness", "pi")
			var res PiResponse
			if code != 0 || json.Unmarshal([]byte(stdout), &res) != nil || res.Block || len(res.AppendText) != 0 {
				t.Fatalf("%s %q: exit %d stdout %q", mode, payload, code, stdout)
			}
		}
	}
}
