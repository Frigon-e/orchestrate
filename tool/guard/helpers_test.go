package guard

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is the repository, two levels above tool/guard where the tests run.
func repoRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func readJSON(t testing.TB, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "hooks", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

type toolguardCase struct {
	Name        string
	Situation   string
	Rule        string
	Command     string
	ReasonHas   map[string][]string
	ReasonLacks map[string][]string
}

type readCase struct {
	Name      string
	Situation string
	File      string
	Bytes     int
	Kind      string
	Input     map[string]any
	Block     bool
}

type toolguardFile struct {
	PathSituations map[string]struct{ Tools []string }
	ReadCases      []readCase
	Cases          []toolguardCase
}

func loadToolguard(t testing.TB) toolguardFile {
	var f toolguardFile
	readJSON(t, "toolguard_cases.json", &f)
	return f
}

// situationPaths makes one directory per PATH situation holding a stub for each
// tool it names, so PATH holds exactly those tools and not the machine's.
func situationPaths(t testing.TB, f toolguardFile) map[string]string {
	t.Helper()
	root := t.TempDir()
	out := map[string]string{}
	for name, spec := range f.PathSituations {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, tool := range spec.Tools {
			if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		out[name] = dir
	}
	return out
}

// guardEnv is every variable the guards read; a run sets the ones it names and clears the rest.
var guardEnv = []string{"ORCH_TOOL_GUARD", "ORCH_TOOL_GUARD_SKIP", "ORCH_TOOL_GUARD_STATE_DIR", "ORCH_LOOKUP_COACH", "ORCH_LOOKUP_LOG"}

// A runner runs one hook event through some implementation of the guards.
type runner interface {
	name() string
	// pre runs a PreToolUse event with PATH set to pathDir.
	pre(t *testing.T, payload []byte, pathDir string, env map[string]string) (code int, stderr string)
	// coach runs a Post or prompt event with its state in stateDir.
	coach(t *testing.T, payload []byte, stateDir string, env map[string]string) (code int, stdout string)
}

// inProc calls the library, with the environment set for the call.
type inProc struct{}

func (inProc) name() string { return "go library" }

func setEnv(t *testing.T, env map[string]string) {
	for _, k := range guardEnv {
		t.Setenv(k, env[k])
	}
}

func (inProc) pre(t *testing.T, payload []byte, pathDir string, env map[string]string) (int, string) {
	setEnv(t, env)
	t.Setenv("PATH", pathDir)
	ev, ok := ParseEvent(payload)
	if !ok {
		return 0, ""
	}
	var stderr bytes.Buffer
	code := Pre(ev, &stderr)
	return code, stderr.String()
}

func (inProc) coach(t *testing.T, payload []byte, stateDir string, env map[string]string) (int, string) {
	e := map[string]string{"ORCH_TOOL_GUARD_STATE_DIR": stateDir}
	for k, v := range env {
		e[k] = v
	}
	setEnv(t, e)
	ev, ok := ParseEvent(payload)
	if !ok {
		return 0, ""
	}
	var stdout bytes.Buffer
	Coach(ev, &stdout)
	return 0, stdout.String()
}

// childEnv is the environment of a spawned hook: the process's own minus every
// guard variable, plus the given ones.
func childEnv(env map[string]string, extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		keep := true
		for _, k := range guardEnv {
			if strings.HasPrefix(kv, k+"=") {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	out = append(out, extra...)
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func execHook(t *testing.T, env []string, payload []byte, name string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s: %v", name, err)
		}
		code = ee.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

// goBinary runs the built orch-guard.
type goBinary struct{}

func (goBinary) name() string { return "orch-guard binary" }

func (goBinary) pre(t *testing.T, payload []byte, pathDir string, env map[string]string) (int, string) {
	code, _, stderr := execHook(t, childEnv(env, "PATH="+pathDir), payload, guardBin, "pre")
	return code, stderr
}

func (goBinary) coach(t *testing.T, payload []byte, stateDir string, env map[string]string) (int, string) {
	code, stdout, _ := execHook(t, childEnv(env, "ORCH_TOOL_GUARD_STATE_DIR="+stateDir), payload, guardBin, "coach")
	return code, stdout
}

// python runs toolguard.py, which phase 2 deletes. Its Bash and Read rules sit
// in five groups behind five hook scripts; orch-guard pre is all of them, so
// the first group that blocks stands for the whole.
type python struct{ script string }

func (python) name() string { return "toolguard.py" }

func (p python) pre(t *testing.T, payload []byte, pathDir string, env map[string]string) (int, string) {
	for _, group := range []string{"find-chains", "sed-inplace", "grep-rg", "cat-head-tail", "big-read"} {
		code, _, stderr := execHook(t, childEnv(env, "PATH="+pathDir), payload, "python3", p.script, group)
		if code != 0 {
			return code, stderr
		}
	}
	return 0, ""
}

func (p python) coach(t *testing.T, payload []byte, stateDir string, env map[string]string) (int, string) {
	code, stdout, _ := execHook(t, childEnv(env, "ORCH_TOOL_GUARD_STATE_DIR="+stateDir), payload, "python3", p.script, "coach")
	return code, stdout
}

func jsonBytes(t testing.TB, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// bashEvent is the PreToolUse event Claude Code sends for a Bash call.
func bashEvent(command string) map[string]any {
	return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": command}}
}

// readEvent is the PreToolUse event for a Read of path with extra arguments.
func readEvent(path string, extra map[string]any) map[string]any {
	input := map[string]any{"file_path": path}
	for k, v := range extra {
		input[k] = v
	}
	return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_input": input}
}

// sandbox is a temp state dir and a synthetic transcript for the coach.
type sandbox struct {
	t          *testing.T
	dir        string
	state      string
	transcript string
}

func newSandbox(t *testing.T) *sandbox {
	dir := t.TempDir()
	return &sandbox{t, dir, filepath.Join(dir, "state"), filepath.Join(dir, "transcript.jsonl")}
}

// claudeCall is the Claude tool_use block for a neutral call spec.
func claudeCall(spec map[string]any, id string) map[string]any {
	block := func(name string, input any) map[string]any {
		return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
	}
	switch {
	case spec["bash"] != nil:
		return block("Bash", map[string]any{"command": spec["bash"]})
	case spec["read"] != nil:
		args := map[string]any{}
		for k, v := range spec["read"].(map[string]any) {
			args[k] = v
		}
		args["file_path"] = args["path"]
		delete(args, "path")
		return block("Read", args)
	case spec["lsp"] != nil:
		return block("LSP", spec["lsp"])
	}
	return block("mcp__jbcontext__search", spec["mcp"])
}

type spec = map[string]any

// writeTranscript writes one assistant message per turn, holding a tool_use per call.
func (b *sandbox) writeTranscript(turns [][]spec) {
	var out bytes.Buffer
	line := func(v any) { out.Write(jsonBytes(b.t, v)); out.WriteByte('\n') }
	line(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "go"}})
	for ti, calls := range turns {
		for ci, call := range calls {
			id := "tu_" + itoa(ti) + "_" + itoa(ci)
			// Claude writes one entry per content block.
			line(map[string]any{"type": "assistant", "message": map[string]any{
				"id": "msg_" + itoa(ti), "role": "assistant", "content": []any{claudeCall(call, id)}}})
		}
	}
	if err := os.WriteFile(b.transcript, out.Bytes(), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

type runOpts struct {
	session  string
	failure  *string
	env      map[string]string
	response any
	extra    map[string]any
}

// postEvent is the PostToolUse (or PostToolUseFailure) event for a call spec.
func (b *sandbox) postEvent(s spec, id string, o runOpts) map[string]any {
	call := claudeCall(s, id)
	if o.session == "" {
		o.session = "s1"
	}
	ev := map[string]any{
		"hook_event_name": "PostToolUse", "session_id": o.session, "transcript_path": b.transcript, "cwd": b.dir,
		"tool_name": call["name"], "tool_input": call["input"], "tool_use_id": id,
	}
	if o.failure != nil {
		ev["hook_event_name"] = "PostToolUseFailure"
		ev["error"] = *o.failure
	} else if o.response != nil {
		ev["tool_response"] = o.response
	} else {
		ev["tool_response"] = map[string]any{"stdout": "ok", "stderr": ""}
	}
	for k, v := range o.extra {
		ev[k] = v
	}
	return ev
}

func (b *sandbox) run(r runner, s spec, id string, o runOpts) (int, string) {
	return r.coach(b.t, jsonBytes(b.t, b.postEvent(s, id, o)), b.state, o.env)
}

func (b *sandbox) prompt(r runner, env map[string]string) (int, string) {
	ev := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s1", "prompt": "go", "cwd": b.dir}
	return r.coach(b.t, jsonBytes(b.t, ev), b.state, env)
}

// log is the rows of lookups.jsonl.
func (b *sandbox) log() []map[string]any {
	data, err := os.ReadFile(filepath.Join(b.state, "lookups.jsonl"))
	if err != nil {
		return nil
	}
	var rows []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			b.t.Fatalf("log line is not JSON: %q", line)
		}
		rows = append(rows, row)
	}
	return rows
}
