package guard

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// guardBin is orch-guard built from this checkout, once for the package's tests.
var guardBin string

func TestMain(m *testing.M) {
	// No test starts a real jbcontext index, here or in a spawned hook.
	os.Setenv("ORCH_JB_REFRESH", "off")
	dir, err := os.MkdirTemp("", "orch-guard-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	guardBin = filepath.Join(dir, "orch-guard")
	build := exec.Command("go", "build", "-o", guardBin, "../cmd/orch-guard")
	build.Stderr = os.Stderr
	code := 1
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building orch-guard:", err)
	} else {
		code = m.Run()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestBinaryBlocksWithExit2AndTheReasonOnStderr(t *testing.T) {
	paths := situationPaths(t, loadToolguard(t))
	code, stdout, stderr := execHook(t, childEnv(nil, "PATH="+paths["fd-rg"]), jsonBytes(t, bashEvent("grep -rn x src")), guardBin, "pre")
	want := "orch tool guard (deny-grep-rg): grep, egrep, fgrep, ag and git grep over files are blocked; " +
		"use rg (rg -n PATTERN path, -g 'GLOB', -F literal, -w word, -i).\n" + testLadder + "\n"
	if code != 2 || stderr != want || stdout != "" {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
}

func TestBinaryBlocksABigReadAndAllowsARange(t *testing.T) {
	big := filepath.Join(t.TempDir(), "big.txt")
	os.WriteFile(big, []byte(strings.Repeat("x", 31744)), 0o644)
	env := childEnv(nil)
	code, _, stderr := execHook(t, env, jsonBytes(t, readEvent(big, nil)), guardBin, "pre")
	if code != 2 || !strings.Contains(stderr, "(deny-big-read)") || !strings.Contains(stderr, "31 KB") {
		t.Fatalf("bare read: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := execHook(t, env, jsonBytes(t, readEvent(big, map[string]any{"limit": 100})), guardBin, "pre"); code != 0 || stderr != "" {
		t.Fatalf("ranged read: exit %d, stderr %q", code, stderr)
	}
}

func TestBinaryCoachWritesHookJSONOnStdout(t *testing.T) {
	box := rgBox(t)
	code, stdout := box.run(goBinary{}, rg, "tu_0_0", runOpts{})
	var out struct {
		Out struct{ HookEventName, AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || code != 0 {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	if out.Out.HookEventName != "PostToolUse" || out.Out.AdditionalContext != testPrefix+coachText["concept-first"] {
		t.Fatalf("stdout %q", stdout)
	}
	if rows := box.log(); len(rows) != 1 || rows[0]["tool"] != "rg" {
		t.Fatalf("log %v", rows)
	}
}

func TestBinaryFirstPromptGetsTheLadderAndTheSecondDoesNot(t *testing.T) {
	box := newSandbox(t)
	_, first := box.prompt(goBinary{}, nil)
	want := `{"hookSpecificOutput": {"hookEventName": "UserPromptSubmit", "additionalContext": ` + pyQuote(testPrefix+testLadder2) + `}}`
	if first != want {
		t.Fatalf("first prompt printed %q", first)
	}
	// With no lookup in between, the window never opened with rg or fd, so no ladder.
	if _, second := box.prompt(goBinary{}, nil); second != "" {
		t.Fatalf("second prompt printed %q", second)
	}
}

func TestBinaryFailsOpenAndReportsItsVersion(t *testing.T) {
	for _, mode := range []string{"pre", "coach"} {
		if code, stdout, stderr := execHook(t, childEnv(nil), []byte("not json"), guardBin, mode); code != 0 || stdout != "" || stderr != "" {
			t.Errorf("%s on broken input: exit %d, stdout %q, stderr %q", mode, code, stdout, stderr)
		}
	}
	if code, stdout, _ := execHook(t, childEnv(nil), nil, guardBin, "--version"); code != 0 || stdout != "dev\n" {
		t.Errorf("--version: exit %d, stdout %q", code, stdout)
	}
	// An unknown mode must not be exit 2, which Claude Code reads as a block.
	if code, _, _ := execHook(t, childEnv(nil), nil, guardBin, "nope"); code != 1 {
		t.Errorf("unknown mode: exit %d, want 1", code)
	}
}

// Eight hook processes of one session at once: one row each, one reminder.
func TestBinaryParallelHooksKeepOneLineEachAndOneReminder(t *testing.T) {
	var calls []spec
	for i := 0; i < 8; i++ {
		calls = append(calls, spec{"bash": "rg -n gateway" + itoa(i) + " src"})
	}
	box := newSandbox(t)
	box.writeTranscript([][]spec{calls})
	outs := make([]string, len(calls))
	var wg sync.WaitGroup
	for j := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, outs[j] = box.run(goBinary{}, calls[j], "tu_0_"+itoa(j), runOpts{})
		}()
	}
	wg.Wait()
	printed, logged := 0, 0
	for _, o := range outs {
		if o != "" {
			printed++
		}
	}
	for _, row := range box.log() {
		if row["reminder"] != nil {
			logged++
		}
	}
	if len(box.log()) != 8 || printed != 1 || logged != 1 {
		t.Errorf("rows %d, reminders printed %d, logged %d; want 8, 1, 1", len(box.log()), printed, logged)
	}
}
