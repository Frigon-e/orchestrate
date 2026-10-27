package guard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConceptRg(t *testing.T) {
	concept := []string{"seed|demo", "verify|verif", "toast", "edge", "carbon|Carbon|oxide|Oxide", "minecraft|MINECRAFT",
		"not checked|unchecked|not verified", "Command-line tools"}
	notConcept := []string{"FAIL", "RESULT|FAIL|SUMMARY", "wave 2b|wave 3", "hideBelow|stack|mobile", "^## ", `func Poll|ErrPollTimeout`,
		"<DataTable", "review_status|ReviewStatus", `content\.New\(`}
	for _, p := range concept {
		if !conceptRg([]string{p, "src"}) {
			t.Errorf("%q should be concept", p)
		}
	}
	for _, p := range notConcept {
		if conceptRg([]string{p, "src"}) {
			t.Errorf("%q should not be concept", p)
		}
	}
	if conceptRg([]string{"toast", "-g", "*.md"}) {
		t.Error("a doc-only target is not a code search")
	}
}

// sfEnv: a git repo, a state dir and stubs for rg, orch and jbcontext alone on PATH.
func sfEnv(t *testing.T, indexed bool) (repo string) {
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
	old := jbIndexedProbe
	t.Cleanup(func() { jbIndexedProbe = old })
	jbIndexedProbe = func(string) bool { return indexed }
	return repo
}

func sfPre(t *testing.T, repo, command string) (int, string) {
	t.Helper()
	ev := piEvent(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "session_id": "s1", "cwd": repo,
		"tool_input": map[string]any{"command": command}})
	var stderr bytes.Buffer
	return Pre(ev, &stderr), stderr.String()
}

func sfEvent(t *testing.T, repo, name string) *Event {
	return piEvent(t, map[string]any{"hook_event_name": name, "session_id": "s1", "cwd": repo, "prompt": "go"})
}

func sfLog(t *testing.T) string {
	data, _ := os.ReadFile(LogPath())
	return string(data)
}

func TestSearchFirstBlocksOncePerPromptAndRearms(t *testing.T) {
	repo := sfEnv(t, true)
	Coach(sfEvent(t, repo, "UserPromptSubmit"), &bytes.Buffer{})
	code, msg := sfPre(t, repo, "rg -n toast src")
	if code != 2 || !strings.Contains(msg, "deny-search-first") || !strings.Contains(msg, "jbcontext search") {
		t.Fatalf("first concept rg: %d %q", code, msg)
	}
	if strings.Contains(msg, "—") {
		t.Error("em dash in reason")
	}
	log := sfLog(t)
	if !strings.Contains(log, `"tool":"blocked:rg"`) || !strings.Contains(log, `"reminder":"search-first"`) || !strings.Contains(log, `"query":"toast"`) {
		t.Fatalf("log row: %s", log)
	}
	if code, _ := sfPre(t, repo, "rg -n toast src"); code != 0 {
		t.Fatal("the retry was blocked")
	}
	if code, _ := sfPre(t, repo, "rg -n other src"); code != 0 {
		t.Fatal("a later rg in the same window was blocked")
	}
	Coach(sfEvent(t, repo, "UserPromptSubmit"), &bytes.Buffer{})
	if code, _ := sfPre(t, repo, "rg -n toast src"); code != 2 {
		t.Fatal("a new prompt did not re-arm the rule")
	}
}

func TestSearchFirstFollowThroughIsLogged(t *testing.T) {
	repo := sfEnv(t, true)
	if code, _ := sfPre(t, repo, "rg -n toast"); code != 2 {
		t.Fatal("not blocked")
	}
	post := piEvent(t, map[string]any{"hook_event_name": "PostToolUse", "session_id": "s1", "cwd": repo, "tool_name": "Bash",
		"tool_input":    map[string]any{"command": `jbcontext search "toast"`},
		"tool_response": map[string]any{"stdout": "ok", "stderr": ""}})
	Coach(post, &bytes.Buffer{})
	if log := sfLog(t); !strings.Contains(log, `"tool":"jbcontext"`) || !strings.Contains(log, `"followed":true`) {
		t.Fatalf("log: %s", log)
	}
}

func TestSearchFirstSymbolNamesTheSymbol(t *testing.T) {
	repo := sfEnv(t, false)
	code, msg := sfPre(t, repo, `rg -n "FooBar" src`)
	if code != 2 || !strings.Contains(msg, "orch lsp def FooBar") || !strings.Contains(msg, "orch lsp refs FooBar") {
		t.Fatalf("symbol rg: %d %q", code, msg)
	}
}

func TestSearchFirstStaysOutOfTheWay(t *testing.T) {
	repo := sfEnv(t, true)
	outside := t.TempDir()
	cases := map[string]string{
		"fixed string":  "rg -F toast src",
		"files":         "rg --files src",
		"file target":   "rg toast " + filepath.Join(repo, "main.go"),
		"outside repo":  "rg toast " + outside,
		"exact pattern": "rg -n '^## ' src",
	}
	for name, line := range cases {
		if code, msg := sfPre(t, repo, line); code != 0 {
			t.Errorf("%s: blocked %q", name, msg)
		}
	}
	if code, _ := sfPre(t, repo, `jbcontext search "x"`); code != 0 {
		t.Fatal("jbcontext blocked")
	}
	if code, _ := sfPre(t, repo, "rg -n toast src"); code != 0 {
		t.Fatal("blocked after a jbcontext call in the same window")
	}
}

func TestSearchFirstNeedsAnIndexedRepoForConcepts(t *testing.T) {
	repo := sfEnv(t, false)
	if code, _ := sfPre(t, repo, "rg -n toast src"); code != 0 {
		t.Fatal("blocked in a repo jbcontext has not indexed")
	}
}

func TestSearchFirstSkipAndGuardOff(t *testing.T) {
	for _, env := range []map[string]string{{"ORCH_TOOL_GUARD_SKIP": "search-first"}, {"ORCH_TOOL_GUARD_SKIP": "deny-search-first"}, {"ORCH_TOOL_GUARD": "off"}} {
		repo := sfEnv(t, true)
		for k, v := range env {
			t.Setenv(k, v)
		}
		if code, _ := sfPre(t, repo, "rg -n toast src"); code != 0 {
			t.Errorf("%v: blocked", env)
		}
	}
}

func TestSearchFirstProbeRunsOncePerRepo(t *testing.T) {
	repo := sfEnv(t, false)
	n := 0
	jbIndexedProbe = func(string) bool { n++; return false }
	sfPre(t, repo, "rg -n toast src")
	sfPre(t, repo, "rg -n edge src")
	if n != 1 {
		t.Fatalf("probe ran %d times", n)
	}
}

func TestPiSearchFirstBlocksOncePerPrompt(t *testing.T) {
	repo := sfEnv(t, true)
	run := func(command string) PiResponse {
		return PiPre(piEvent(t, map[string]any{"event": "tool_call", "session": "p1", "cwd": repo, "tool": "bash",
			"input": map[string]any{"command": command}, "resultHook": true}))
	}
	PiCoach(piEvent(t, map[string]any{"event": "before_agent_start", "session": "p1", "cwd": repo}))
	if res := run("rg -n toast src"); !res.Block || !strings.Contains(res.Reason, "deny-search-first") {
		t.Fatalf("first: %+v", res)
	}
	if !strings.Contains(sfLog(t), `"harness":"pi"`) {
		t.Fatalf("no pi row: %s", sfLog(t))
	}
	if res := run("rg -n toast src"); res.Block {
		t.Fatal("retry blocked")
	}
	PiCoach(piEvent(t, map[string]any{"event": "before_agent_start", "session": "p1", "cwd": repo}))
	if res := run("rg -n toast src"); !res.Block {
		t.Fatal("not re-armed")
	}
	PiCoach(piEvent(t, map[string]any{"event": "before_agent_start", "session": "p1", "cwd": repo}))
	if res := run(`jbcontext search "x"`); res.Block {
		t.Fatal("jbcontext blocked")
	}
	if res := run("rg -n toast src"); res.Block {
		t.Fatal("blocked after jbcontext in the window")
	}
}

func shortWait(t *testing.T) {
	old := searchFirstWait
	searchFirstWait = 150 * time.Millisecond
	t.Cleanup(func() { searchFirstWait = old })
}

func TestSearchFirstWaitsForAConcurrentSiblingLookup(t *testing.T) {
	shortWait(t)
	repo := sfEnv(t, true)
	done := make(chan int, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		code, _ := sfPre(t, repo, `jbcontext search "toast"`)
		done <- code
	}()
	if code, msg := sfPre(t, repo, "rg -n toast src"); code != 0 {
		t.Errorf("rg with a concurrent jbcontext sibling was blocked: %q", msg)
	}
	if <-done != 0 {
		t.Error("jbcontext blocked")
	}

	repo = sfEnv(t, true)
	sfPre(t, repo, `jbcontext search "toast"`)
	if code, _ := sfPre(t, repo, "rg -n toast src"); code != 0 {
		t.Error("rg after an earlier jbcontext was blocked")
	}

	repo = sfEnv(t, true)
	start := time.Now()
	if code, _ := sfPre(t, repo, "rg -n toast src"); code != 2 {
		t.Error("control: nothing concurrent must block")
	}
	if time.Since(start) < searchFirstWait {
		t.Error("the block did not wait for a sibling")
	}
	start = time.Now()
	sfPre(t, repo, "rg -n toast src")
	if time.Since(start) >= searchFirstWait {
		t.Error("the retry waited again")
	}
}

func TestPiSearchFirstWaitsForAConcurrentSiblingLookup(t *testing.T) {
	shortWait(t)
	repo := sfEnv(t, true)
	run := func(command string) PiResponse {
		return PiPre(piEvent(t, map[string]any{"event": "tool_call", "session": "p1", "cwd": repo, "tool": "bash",
			"input": map[string]any{"command": command}, "resultHook": true}))
	}
	done := make(chan PiResponse, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		done <- run(`jbcontext search "toast"`)
	}()
	if res := run("rg -n toast src"); res.Block {
		t.Errorf("rg with a concurrent jbcontext sibling was blocked: %q", res.Reason)
	}
	<-done
}

func TestSearchFirstAllCapsWordsAreNotSymbols(t *testing.T) {
	shortWait(t)
	repo := sfEnv(t, true)
	for _, p := range []string{"FAIL", "RESULT|FAIL|SUMMARY"} {
		if code, msg := sfPre(t, repo, "rg -n '"+p+"' src"); code != 0 {
			t.Errorf("%s blocked: %q", p, msg)
		}
	}
	if code, msg := sfPre(t, repo, "rg -n MAX_RETRIES src"); code != 2 || !strings.Contains(msg, "orch lsp def MAX_RETRIES") {
		t.Errorf("MAX_RETRIES must stay a symbol: %d %q", code, msg)
	}
}

func TestSearchFirstJudgesEveryRgSegmentAndNamesAllIdentifiers(t *testing.T) {
	shortWait(t)
	type want struct {
		block bool
		has   []string
		lacks []string
	}
	cases := []struct {
		name, line string
		want       want
	}{
		{"mixed first segment", `cd REPO && rg -n "func.*applyRefund|refund_unmatched|refund.unmatched" internal/crm docs/crm | head -30; rg -n "MaxAttempts|NextRetry|RetryPolicy" internal/crm internal/platform --glob '!*_test.go' | head -30`,
			want{true, []string{"jbcontext search", "For the names in it: orch lsp def applyRefund"}, nil}},
		{"several names", `rg -n "MaxAttempts|NextRetry|RetryPolicy" internal`,
			want{true, []string{"orch lsp def MaxAttempts NextRetry RetryPolicy", "orch lsp refs MaxAttempts"}, []string{"jbcontext search"}}},
		{"func prefix", `rg 'func.*applyRefund'`, want{true, []string{"orch lsp def applyRefund"}, nil}},
		{"type prefix", `rg 'type Gateway\b'`, want{true, []string{"orch lsp def Gateway"}, nil}},
		{"qualified names", `rg 'pay\.Gateway|Stripe.StartCheckout' src`,
			want{true, []string{"orch lsp def pay.Gateway Stripe.StartCheckout"}, []string{"jbcontext search"}}},
		{"piped stdin", `git log | rg foo`, want{false, nil, nil}},
		{"bare caps", `rg -n FAIL | rg PASS`, want{false, nil, nil}},
		{"doc only", `rg -n 'ErrNotFound|ErrConflict' -g '*.md'`, want{false, nil, nil}},
		{"second segment", `ls; rg -n FooBar src`, want{true, []string{"orch lsp def FooBar"}, nil}},
		{"first not candidate, second is", `rg -F toast src; rg -n 'RetryPolicy' src`, want{true, []string{"orch lsp def RetryPolicy"}, nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := sfEnv(t, true)
			code, msg := sfPre(t, repo, strings.ReplaceAll(c.line, "REPO", repo))
			if (code == 2) != c.want.block {
				t.Fatalf("exit %d, want block=%v: %q", code, c.want.block, msg)
			}
			for _, h := range c.want.has {
				if !strings.Contains(msg, h) {
					t.Errorf("reason lacks %q: %q", h, msg)
				}
			}
			for _, l := range c.want.lacks {
				if strings.Contains(msg, l) {
					t.Errorf("reason has %q: %q", l, msg)
				}
			}
		})
	}
}

func TestPiLadderNoteSkipsRgTheRuleCoversButKeepsOtherSearches(t *testing.T) {
	repo := sfEnv(t, true)
	hasLadder := func(command string) bool {
		res := PiCoach(piEvent(t, map[string]any{"event": "tool_result", "session": "p1", "cwd": repo, "tool": "bash", "turn": 1,
			"toolCallId": "c1", "input": map[string]any{"command": command}, "content": []string{"x"}}))
		return strings.Contains(strings.Join(res.AppendText, "\n"), "Lookup order:")
	}
	if hasLadder("rg -n toast src") {
		t.Error("ladder note on an rg the rule judged")
	}
	if !hasLadder("fd toast") {
		t.Error("no ladder note on fd")
	}
	if !hasLadder("rg -F toast src") {
		t.Error("no ladder note on an rg the rule did not judge")
	}
}
