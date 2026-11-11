package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// benchRunFixture builds a target repo with two commits (the second adds x.go
// and x_test.go), a replay suite over it and a fake claude.
func benchRunFixture(t *testing.T, h *harness, claudeBody string) (target, second string) {
	t.Helper()
	target = filepath.Join(h.tmp, "target")
	git := func(args ...string) string {
		out, e := exec.Command("git", append([]string{"-C", target}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v\n%s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	if out, e := exec.Command("git", "init", "-q", "-b", "main", target).CombinedOutput(); e != nil {
		t.Fatalf("%v\n%s", e, out)
	}
	writeFiles(t, target, map[string]string{"a.txt": "a\n"})
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	writeFiles(t, target, map[string]string{"x.go": "package x\n", "x_test.go": "package x\n"})
	git("add", "-A")
	git("commit", "-q", "-m", "second")
	second = git("rev-parse", "HEAD")
	suite := filepath.Join(benchDir(), "rp")
	if e := os.MkdirAll(suite, 0o700); e != nil {
		t.Fatal(e)
	}
	writeFiles(t, suite, map[string]string{
		"suite.toml": "repo = \"" + target + "\"\n[arms.base]\nkind = \"replay\"\n[arms.plug]\nkind = \"replay\"\nplugin_dirs = [\"/p/plug\"]\n[arms.old]\ncache = \"x\"\n",
		"tasks.toml": "[[task]]\nid = \"t1\"\ncommit = \"" + second + "\"\nhidden = [\"x_test.go\"]\ncheck = \"test -f x_test.go\"\nprompt = \"Add x.\"\n" +
			"[[task]]\nid = \"t2\"\ncommit = \"" + second + "\"\nhidden = [\"x_test.go\"]\ncheck = \"test -f nope.go\"\nprompt = \"Add y.\"\n",
	})
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", claudeBody))
	return target, second
}

const fakeReplayClaude = `echo edited > edited.txt
cost=${FAKE_COST:-0.5}
cat <<EOF
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","name":"LSP","input":{}}]}}
not json
{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}
{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":$cost,"modelUsage":{"m1":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":300,"cacheCreationInputTokens":40,"costUSD":0.4},"m2":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,"costUSD":0.1}}}
EOF
exit ${FAKE_EXIT:-0}`

func benchReplayByLabel(t *testing.T, label string) *benchRun {
	t.Helper()
	runs, e := benchRuns("rp")
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range runs {
		if r.Label == label {
			return r
		}
	}
	t.Fatalf("no run labelled %s", label)
	return nil
}

func TestBenchRunCellRecordsReplayMetrics(t *testing.T) {
	h := benchHarness(t)
	target, _ := benchRunFixture(t, h, fakeReplayClaude)
	h.must("bench", "run", "rp", "--tasks", "t1", "--arms", "base")
	r := benchReplayByLabel(t, "t1-base-r1")
	if r.Kind != "replay" || r.Arm != "base" || !strings.HasPrefix(r.Note, "task t1 commit ") {
		t.Fatalf("run: %+v", r)
	}
	want := map[string]float64{"pass": 1, "lsp_calls": 1, "build_test_calls": 1, "bash_calls": 1, "read_calls": 1, "main_read_calls": 0, "tool_calls": 3,
		"tokens_total": 380, "tokens_input": 11, "tokens_cache_read": 303, "tokens_cache_write": 44, "tokens_output": 22,
		"cost_usd": 0.5, "turns": 3, "files_touched": 1, "files_outside_solution": 1, "agent_error": 0, "timed_out": 0}
	for k, v := range want {
		if got := r.Metrics[k].Value; got != v {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}
	patch := mustRead(t, filepath.Join(r.dir, "snapshot", "agent.patch"))
	if !strings.Contains(string(patch), "edited.txt") {
		t.Errorf("patch: %s", patch)
	}
	if argv := string(mustRead(t, filepath.Join(r.dir, "snapshot", "stream.jsonl"))); !strings.Contains(argv, "total_cost_usd") {
		t.Errorf("stream not stored: %s", argv)
	}
	if out, e := exec.Command("git", "-C", target, "worktree", "list", "--porcelain").Output(); e != nil || strings.Count(string(out), "worktree ") != 1 {
		t.Errorf("worktree left behind: %s %v", out, e)
	}
	if out := h.must("bench", "verify", "--suite", "rp"); !strings.Contains(out, "verify: 0 errors") {
		t.Errorf("verify: %s", out)
	}
}

func TestBenchRunFailuresStillRecord(t *testing.T) {
	h := benchHarness(t)
	benchRunFixture(t, h, fakeReplayClaude)
	h.must("bench", "run", "rp", "--tasks", "t2", "--arms", "base")
	if r := benchReplayByLabel(t, "t2-base-r1"); r.Metrics["pass"].Value != 0 || r.Metrics["agent_error"].Value != 0 {
		t.Errorf("check failure metrics: %+v", r.Metrics)
	}
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "echo boom >&2; exit 3"))
	h.must("bench", "run", "rp", "--tasks", "t1", "--arms", "plug")
	r := benchReplayByLabel(t, "t1-plug-r1")
	if r.Metrics["agent_error"].Value != 1 || r.Metrics["pass"].Value != 1 || len(r.Warnings) == 0 {
		t.Errorf("agent failure: %+v %v", r.Metrics, r.Warnings)
	}
}

func TestBenchRunDryRunValidatesAndRunsNothing(t *testing.T) {
	h := benchHarness(t)
	benchRunFixture(t, h, fakeReplayClaude)
	out := h.must("bench", "run", "rp", "--dry-run", "--tasks", "t1", "--arms", "plug")
	for _, want := range []string{"--setting-sources", "project,local", "--plugin-dir", "/p/plug", "--model", "sonnet"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run missing %q:\n%s", want, out)
		}
	}
	if runs, _ := benchRuns("rp"); len(runs) != 0 {
		t.Errorf("dry run recorded %d runs", len(runs))
	}
	for _, args := range [][]string{{"--arms", "nope"}, {"--arms", "old"}, {"--tasks", "ghost"}} {
		if code, out := h.run(append([]string{"bench", "run", "rp", "--dry-run"}, args...)...); code == 0 {
			t.Errorf("%v accepted: %s", args, out)
		}
	}
	tasks := filepath.Join(benchDir(), "rp", "tasks.toml")
	b := string(mustRead(t, tasks))
	writeFiles(t, filepath.Dir(tasks), map[string]string{"tasks.toml": strings.Replace(b, `hidden = ["x_test.go"]`, `hidden = ["missing_test.go"]`, 1)})
	if code, out := h.run("bench", "run", "rp", "--dry-run", "--tasks", "t1"); code == 0 || !strings.Contains(out, "missing_test.go") {
		t.Errorf("missing hidden path accepted: %d %s", code, out)
	}
}

func TestBenchRunSummaryMedians(t *testing.T) {
	h := benchHarness(t)
	benchRunFixture(t, h, "n=$(cat "+filepath.Join(h.tmp, "count")+" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "+filepath.Join(h.tmp, "count")+"\nFAKE_COST=$((n*2-1))\n"+fakeReplayClaude)
	h.must("bench", "run", "rp", "--tasks", "t1", "--arms", "base", "--repeats", "2", "--parallel", "1")
	out := h.must("bench", "run", "rp", "--summary")
	var row string
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, "t1") {
			row = l
		}
	}
	f := strings.Fields(row)
	// TASK ARM N PASS COST TOKENS
	if len(f) < 6 || f[2] != "2" || f[3] != "2/2" || f[4] != "2.0000" || f[5] != "380" {
		t.Errorf("summary row %q in:\n%s", row, out)
	}
}
