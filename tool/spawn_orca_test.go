package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Orca picks its own worktree path, so spawn must put that path in the brief and
// hand the launch text to Start, not type it into a terminal afterwards.
func TestSpawnOrcaRewritesTaskPathLaunchesWithPromptAndModel(t *testing.T) {
	w := wiringSetup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("max_leads = 5\nclaude_args = [\"--permission-mode\", \"auto\"]\n"), 0o644)
	w.task("t1")
	task := w.dir("t1") + "/task.md"
	computed := worktreePath(w.dir("t1"), "t1")
	if !strings.Contains(string(mustRead(t, task)), computed) {
		t.Fatalf("setup: task.md does not name the computed path %s", computed)
	}
	w.must("spawn", "t1")

	text := string(mustRead(t, task))
	if strings.Contains(text, computed) || !strings.Contains(text, w.orcaPath()) {
		t.Fatalf("task.md still names the computed path, or lacks %s:\n%s", w.orcaPath(), text)
	}
	if got := w.orca.calls("agent launch"); len(got) != 0 {
		t.Fatalf("agent launch calls = %v; a lead's args and env need terminal create", got)
	}
	creates := w.orca.calls("terminal create")
	if len(creates) != 1 {
		t.Fatalf("terminal create calls = %v, want 1", creates)
	}
	l := w.lead("t1")
	if l.Session == "" {
		t.Fatal("claude lead has no session id under Orca")
	}
	path := strings.Trim(strings.TrimPrefix(flagValue(creates[0], "--command"), "sh "), "'")
	script := string(mustRead(t, path))
	for _, want := range []string{"'--session-id' '" + l.Session + "'", "'--model' 'sonnet'", "'--effort' 'high'", "'ORCH_ROLE=lead'",
		"You are orchestrate lead \"t1\"", w.orcaPath()} {
		if !strings.Contains(script, want) {
			t.Fatalf("launcher script lacks %q:\n%s", want, script)
		}
	}
	if !strings.Contains(script, "'--permission-mode' 'auto'") || strings.Contains(script, "--dangerously-skip-permissions") {
		t.Fatalf("launcher script must carry claude_args and no bypass flag with a permission mode:\n%s", script)
	}
	if got := w.orca.calls("terminal send"); len(got) != 0 {
		t.Fatalf("terminal send calls = %v; the launch text must not be typed after Start", got)
	}
}

// On Herdr the paths agree, so task.md is not touched, the prompt is typed after
// the start, and a Claude lead gets a session id for context and limit tracking.
func TestSpawnHerdrKeepsTaskAndTypesPromptAndMintsClaudeSession(t *testing.T) {
	h := setup(t)
	h.task("t1")
	task := h.dir("t1") + "/task.md"
	computed := worktreePath(h.dir("t1"), "t1")
	h.must("spawn", "t1")
	if after := string(mustRead(t, task)); !strings.Contains(after, "in your own worktree `"+computed+"`") {
		t.Fatalf("task.md lost the herdr worktree path %s:\n%s", computed, after)
	}
	if l := h.lead("t1"); l.WorktreePath != "" {
		t.Fatalf("herdr lead stored a worktree path %q though it is the computed one", l.WorktreePath)
	}
	l := h.lead("t1")
	if l.Session == "" {
		t.Fatal("claude lead has no session id")
	}
	var prompted bool
	for _, ag := range h.fakeState().Agents {
		prompted = prompted || strings.Contains(ag.Prompt, "You are orchestrate lead \"t1\"")
	}
	if !prompted {
		t.Fatal("herdr agent was never prompted with the launch text")
	}
}

func TestRewriteTaskPath(t *testing.T) {
	p := t.TempDir() + "/task.md"
	body := "worktree add /a/t1 and /a/t1/sub\n"
	os.WriteFile(p, []byte(body), 0o640)
	if err := rewriteTaskPath(p, "/a/t1", "/a/t1"); err != nil || string(mustRead(t, p)) != body {
		t.Fatalf("equal paths changed the file: %v", err)
	}
	if err := rewriteTaskPath(p, "/zzz", "/b"); err != nil || string(mustRead(t, p)) != body {
		t.Fatalf("absent path changed the file: %v", err)
	}
	if err := rewriteTaskPath(p, "/a/t1", "/b/x"); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, p)); got != "worktree add /b/x and /b/x/sub\n" {
		t.Fatalf("rewritten = %q", got)
	}
}

func TestLaunchSettingsKeepsModelOutOfArgsWithoutLayout(t *testing.T) {
	fm := map[string]string{"harness": "claude", "model": "sonnet", "effort": "high"}
	cfg := config{ClaudeArgs: []string{"--x"}}
	args, m, e := launchSettings(fm, cfg, "S", false)
	if strings.Join(args, " ") != "--x" || m != "sonnet" || e != "high" {
		t.Fatalf("no layout: %v %q %q", args, m, e)
	}
	args, m, e = launchSettings(fm, cfg, "S", true)
	if strings.Join(args, " ") != "--model sonnet --effort high --session-id S --x" || m != "sonnet" || e != "high" {
		t.Fatalf("layout: %v %q %q", args, m, e)
	}
	fm["model"], fm["effort"] = "default", "default"
	if _, m, e = launchSettings(fm, cfg, "", false); m != "" || e != "" {
		t.Fatalf("default model/effort leaked: %q %q", m, e)
	}
}

func TestPollGoneWaitsForAsyncClose(t *testing.T) {
	calls := 0
	live := func() (bool, error) { calls++; return calls < 4, nil }
	if !pollGone(live, time.Minute, time.Microsecond) || calls != 4 {
		t.Fatalf("pollGone = false or calls = %d, want true after 4 checks", calls)
	}
	if pollGone(func() (bool, error) { return true, nil }, 5*time.Millisecond, time.Microsecond) {
		t.Fatal("pollGone succeeded for a pane that never closes")
	}
	if pollGone(func() (bool, error) { return false, errors.New("down") }, 5*time.Millisecond, time.Microsecond) {
		t.Fatal("a lookup error must count as still open")
	}
}
