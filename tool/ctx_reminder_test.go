package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	guardBinOnce sync.Once
	guardBinDir  string
	guardBinPath string
	guardBinErr  error
)

// guardBin is orch-guard built from this checkout, once for the package's tests.
// The extension is only wiring, so its tests run it against the real binary.
func guardBin(t *testing.T) string {
	t.Helper()
	guardBinOnce.Do(func() {
		if guardBinDir, guardBinErr = os.MkdirTemp("", "orch-guard-test"); guardBinErr != nil {
			return
		}
		guardBinPath = filepath.Join(guardBinDir, "orch-guard")
		if out, err := exec.Command("go", "build", "-o", guardBinPath, "./cmd/orch-guard").CombinedOutput(); err != nil {
			guardBinErr = fmt.Errorf("building orch-guard: %v\n%s", err, out)
		}
	})
	if guardBinErr != nil {
		t.Fatal(guardBinErr)
	}
	return guardBinPath
}

func removeGuardBin() {
	if guardBinDir != "" {
		os.RemoveAll(guardBinDir)
	}
}

// The extension decides in pi, so the decision tables run the real file in node
// (which strips its types) against a stand-in for pi's extension API.
type reminderCase struct {
	Name string `json:"name"`
	Tool string `json:"tool"`
	// Blocks is the rule that must block the call: "", sed-inplace or big-read.
	Blocks string   `json:"-"`
	Needs  []string `json:"-"` // substrings the block reason must carry
	Remind bool     `json:"-"` // the note is appended to the result
	// Repeat marks the 3rd block in a row, whose reason carries one extra line.
	Repeat bool           `json:"-"`
	Input  map[string]any `json:"input"`
	// Parent is the id of the Code Mode script call that made this call.
	Parent string `json:"parent,omitempty"`
}

type reminderRun struct {
	Handlers []string `json:"handlers"`
	Out      map[string]struct {
		Block string `json:"block"`
		Added string `json:"added"`
		Kept  string `json:"kept"`
	} `json:"out"`
}

const (
	reminderMarker = "Lookup order:"
	guardMarker    = "orch tool guard"
	// repeatedBlock is the one extra line a reason carries from the 3rd block
	// in a row on.
	repeatedBlock = "Blocked 3 times: use the replacement above; this guard does not give way."
	// ladderLine is the last line of every search block, byte for byte, and the
	// note appended to a search result.
	ladderLine = `Lookup order: jbcontext search "<one behavior>" if the location is unknown, orch lsp def|refs|callers <Name> for a known symbol, rg for exact text, fd for file names; send independent lookups in one message.`
)

// runCtxReminder runs a case group with `orch`, `fd` and `rg` stand-ins on PATH.
func runCtxReminder(t *testing.T, env []string, cases []reminderCase) reminderRun {
	t.Helper()
	return runCtxReminderWith(t, []string{"orch", "fd", "rg"}, env, cases)
}

// runCtxReminderWith copies the real extension into a temp dir, puts a stub for
// each named tool on PATH and runs the cases through the loaded extension in
// node, which calls the real orch-guard binary. PATH is only that temp dir, so the real system tools never leak in and
// the case group decides which replacements the guard can name.
func runCtxReminderWith(t *testing.T, onPath []string, env []string, cases []reminderCase) reminderRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	ext := filepath.Join(dir, "orch-ctx-reminder.ts")
	if err := os.WriteFile(ext, mustRead(t, "pi_ctx_reminder.ts"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range onPath {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import ext from ` + string(mustJSON(t, ext)) + `;
if (process.env.ORCH_TEST_CWD) process.chdir(process.env.ORCH_TEST_CWD);
const handlers = {};
ext({ on: (name, fn) => {
  if (name === "tool_result" && process.env.NO_RESULT_HOOK) throw new Error("unsupported event");
  handlers[name] = fn;
} });
const out = {};
for (const c of JSON.parse(process.argv[1])) {
  const call = { type: "tool_call", toolCallId: "t1", toolName: c.tool, input: c.input, parentToolCallId: c.parent || undefined };
  const blocked = handlers.tool_call ? await handlers.tool_call(call) : undefined;
  const raw = { type: "tool_result", toolCallId: "t1", toolName: c.tool, input: c.input, content: [{ type: "text", text: "ORIGINAL" }], isError: false, parentToolCallId: c.parent || undefined };
  const res = handlers.tool_result ? await handlers.tool_result(raw) : undefined;
  out[c.name] = {
    block: blocked && blocked.block ? blocked.reason : "",
    added: res ? res.content.slice(1).map((p) => p.text).join("") : "",
    kept: res ? res.content[0].text : "ORIGINAL",
  };
}
console.log(JSON.stringify({ handlers: Object.keys(handlers).sort(), out }));
`
	cmd := exec.Command(node, "--input-type=module", "-e", script, string(mustJSON(t, cases)))
	cmd.Env = agentEnv(nil, append([]string{"PATH=" + bin, "ORCH_LOOKUP_READY=", "ORCH_LOOKUP_COACH=off", "ORCH_LOOKUP_LOG=off",
		"ORCH_GUARD_BIN=" + guardBin(t), "ORCH_TOOL_GUARD_STATE_DIR=" + filepath.Join(dir, "state")}, env...))
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got reminderRun
	if err := json.Unmarshal(raw[strings.Index(string(raw), "{"):], &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return got
}

func bash(command string) map[string]any { return map[string]any{"command": command} }

// checkReminder asserts the whole table: which calls are blocked, by which
// rule, with which replacement, and which get the note appended.
func checkReminder(t *testing.T, got reminderRun, cases []reminderCase) (reminded, quiet, blocked int) {
	t.Helper()
	for _, c := range cases {
		res, ok := got.Out[c.Name]
		if !ok {
			t.Fatalf("missing result for %q", c.Name)
		}
		if res.Kept != "ORIGINAL" {
			t.Errorf("%q lost the original result content: %q", c.Name, res.Kept)
		}
		if c.Blocks == "" {
			if res.Block != "" {
				t.Errorf("%q was blocked but should be allowed: %s", c.Name, res.Block)
			}
		} else {
			blocked++
			if want := "deny-" + c.Blocks; !strings.Contains(res.Block, want) {
				t.Errorf("%q must be blocked by %s: %s", c.Name, want, res.Block)
			}
			if !strings.Contains(res.Block, guardMarker) {
				t.Errorf("%q block reason does not name the guard: %q", c.Name, res.Block)
			}
			for _, need := range c.Needs {
				if !strings.Contains(res.Block, need) {
					t.Errorf("%q block reason must name %q: %q", c.Name, need, res.Block)
				}
			}
			// A reason is two or three short lines; the 3rd block in a row adds
			// the repeated-block line.
			limit := 3
			if strings.Contains(res.Block, repeatedBlock) {
				limit = 4
			}
			// A big-read block also says a repeat passes, one line more.
			if c.Blocks == "big-read" {
				limit++
			}
			if lines := strings.Count(res.Block, "\n") + 1; lines > limit {
				t.Errorf("%q block reason is longer than %d lines: %q", c.Name, limit, res.Block)
			}
			if c.Repeat && !strings.Contains(res.Block, repeatedBlock) {
				t.Errorf("%q is the 3rd block in a row but the reason lacks %q: %q", c.Name, repeatedBlock, res.Block)
			}
		}
		// A blocked call never reaches its result hook in pi, so the note is
		// only checked on the calls that run.
		switch {
		case c.Blocks != "":
		case c.Remind:
			reminded++
			if res.Added != ladderLine {
				t.Errorf("%q did not get the ladder line: %q", c.Name, res.Added)
			}
		default:
			quiet++
			if res.Added != "" {
				t.Errorf("%q got a reminder it should not have: %q", c.Name, res.Added)
			}
		}
	}
	return reminded, quiet, blocked
}

// The shape of every decision: whose the call is, and whether it is blocked.
func TestCtxReminderDecisionTable(t *testing.T) {
	cases := []reminderCase{
		// A grep or rg over files is no longer blocked; the result only gets the ladder note.
		{Name: "rg", Tool: "bash", Input: bash(`rg -n Gateway internal`), Remind: true},
		{Name: "grep -r", Tool: "bash", Input: bash(`grep -rn Gateway internal`), Remind: true},
		{Name: "egrep", Tool: "bash", Input: bash(`egrep -n Gateway internal`), Remind: true},
		{Name: "fgrep", Tool: "bash", Input: bash(`fgrep -n Gateway internal`), Remind: true},
		{Name: "ag", Tool: "bash", Input: bash(`ag Gateway internal`), Remind: true},
		{Name: "absolute rg", Tool: "bash", Input: bash(`/usr/bin/rg -n Gateway .`), Remind: true},
		{Name: "env prefix", Tool: "bash", Input: bash(`RG_OPTS=1 rg Gateway .`), Remind: true},
		{Name: "cd && rg", Tool: "bash", Input: bash(`cd internal && rg -n Gateway .`), Remind: true},
		{Name: "git grep", Tool: "bash", Input: bash(`git grep -n Gateway`), Remind: true},
		// fd only gets the note; a plain find and a grep fed by a pipe are quiet.
		{Name: "fd", Tool: "bash", Input: bash(`fd gateway`), Remind: true},
		{Name: "find", Tool: "bash", Input: bash(`find . -name "*.go"`), Remind: true},
		{Name: "find by type", Tool: "bash", Input: bash(`find src -type f`), Remind: true},
		{Name: "pipe find", Tool: "bash", Input: bash(`cat list.txt | find . -name "*.ts"`), Remind: true},
		{Name: "pipe grep", Tool: "bash", Input: bash(`cat file | grep -n Gateway`), Remind: true},
		// pi's own grep and find are no longer blocked.
		{Name: "builtin grep", Tool: "grep", Input: map[string]any{"pattern": "Gateway"}, Remind: true},
		{Name: "builtin find", Tool: "find", Input: map[string]any{"pattern": "*.go"}, Remind: true},
		{Name: "builtin ls", Tool: "ls", Input: map[string]any{"path": "."}, Remind: true},
		// Everything else stays quiet.
		{Name: "orch lsp def", Tool: "bash", Input: bash(`orch lsp def Gateway`)},
		{Name: "orch lsp symbols", Tool: "bash", Input: bash(`orch lsp symbols internal/gateway.go`)},
		{Name: "go test", Tool: "bash", Input: bash(`go test ./...`)},
		{Name: "plain ls", Tool: "bash", Input: bash(`ls`)},
		{Name: "echo quoted rg", Tool: "bash", Input: bash(`echo "rg"`)},
		{Name: "cat pipe wc", Tool: "bash", Input: bash(`cat file | wc -l`)},
		{Name: "read tool", Tool: "read", Input: map[string]any{"path": "a.go"}},
		// A call a Code Mode script makes never gets a note: its result is the script's data.
		{Name: "script rg", Tool: "bash", Input: bash(`rg -n Gateway internal`), Parent: "codemode-call-1"},
		{Name: "script builtin grep", Tool: "grep", Input: map[string]any{"pattern": "Gateway"}, Parent: "codemode-call-1"},
	}
	got := runCtxReminder(t, nil, cases)
	if len(got.Handlers) != 2 || got.Handlers[0] != "tool_call" || got.Handlers[1] != "tool_result" {
		t.Fatalf("expected the guard and the reminder hooks, got %v", got.Handlers)
	}
	reminded, quiet, blocked := checkReminder(t, got, cases)
	t.Logf("base table: %d reminded, %d quiet, %d blocked", reminded, quiet, blocked)
}

// deny-sed-inplace: sed -i and perl -pi rewrite a file, so the edit tool owns it.
func TestCtxReminderGuardDenySedInplace(t *testing.T) {
	edit := []string{"edit"}
	cases := []reminderCase{
		{Name: "sed -i", Tool: "bash", Input: bash(`sed -i 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: edit},
		{Name: "sed -i.bak", Tool: "bash", Input: bash(`sed -i.bak 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: edit},
		{Name: "sed -i BSD", Tool: "bash", Input: bash(`sed -i '' 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: edit},
		{Name: "gsed -i", Tool: "bash", Input: bash(`gsed -i 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: edit},
		{Name: "sed --in-place", Tool: "bash", Input: bash(`sed --in-place 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: edit},
		{Name: "perl -pi", Tool: "bash", Input: bash(`perl -pi -e 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: edit},
		{Name: "perl -i -pe", Tool: "bash", Input: bash(`perl -i -pe 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: edit},
		{Name: "absolute sed -i", Tool: "bash", Input: bash(`/usr/bin/sed -i 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: edit},
		{Name: "cd && sed -i", Tool: "bash", Input: bash(`cd internal && sed -i 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: edit},
		{Name: "sudo sed -i", Tool: "bash", Input: bash(`sudo sed -i 's/a/b/' /etc/hosts`), Blocks: "sed-inplace", Needs: edit},
		// Does not touch: sed that prints to stdout, a piped transform, grep -i.
		{Name: "sed print to stdout", Tool: "bash", Input: bash(`sed 's/a/b/' file.go`)},
		{Name: "piped sed transform", Tool: "bash", Input: bash(`git diff | sed 's/^/  /'`)},
		{Name: "grep -i", Tool: "bash", Input: bash(`grep -rn -i Gateway internal`), Remind: true},
	}
	got := runCtxReminder(t, nil, cases)
	reminded, quiet, blocked := checkReminder(t, got, cases)
	t.Logf("deny-sed-inplace: %d blocked, %d allowed, %d reminded", blocked, quiet, reminded)
}

// Without orch on PATH the sed -i block still points at the built-ins, and
// every search the lookup rules used to block just runs.
func TestCtxReminderSilentInRestrictedShellAndGuardsWithoutOrch(t *testing.T) {
	t.Run("no orch on PATH", func(t *testing.T) {
		cases := []reminderCase{
			{Name: "find | grep", Tool: "bash", Input: bash(`find . -name "*.go" | grep Gateway`)},
			{Name: "sed -i", Tool: "bash", Input: bash(`sed -i 's/a/b/' file.go`), Blocks: "sed-inplace", Needs: []string{"edit"}},
			{Name: "cat one file", Tool: "bash", Input: bash(`cat notes.txt`)},
			{Name: "rg Foo internal/", Tool: "bash", Input: bash(`rg Foo internal/`)},
			{Name: "find by name", Tool: "bash", Input: bash(`find . -name "*.ts"`)},
			{Name: "fd", Tool: "bash", Input: bash(`fd gateway`)},
			{Name: "go test | tail", Tool: "bash", Input: bash(`go test ./... | tail -n 20`)},
		}
		got := runCtxReminderWith(t, nil, nil, cases)
		if len(got.Handlers) != 1 || got.Handlers[0] != "tool_call" {
			t.Fatalf("without orch only the guard hook is expected, got %v", got.Handlers)
		}
		checkReminder(t, got, cases)
	})
	t.Run("restricted gate shell", func(t *testing.T) {
		cases := []reminderCase{{Name: "rg", Tool: "bash", Input: bash(`rg -n Gateway .`)}}
		got := runCtxReminder(t, []string{"ORCH_LOOKUP_READY=/tmp/ready"}, cases)
		if len(got.Handlers) != 0 || got.Out["rg"].Added != "" || got.Out["rg"].Block != "" {
			t.Fatalf("extension acted inside the restricted shell: %v %+v", got.Handlers, got.Out["rg"])
		}
	})
}

// A blocked command stays blocked on every retry, so the model learns not to
// use it; the 3rd block in a row adds one line saying the guard will not give
// way, and an allowed call ends the run. ORCH_TOOL_GUARD=off or a skip by name
// are the human's escapes. sed -i is the one blocking shell rule left.
func TestCtxReminderLoopProtectionAndSwitches(t *testing.T) {
	edit := []string{"edit"}
	t.Run("same command stays blocked", func(t *testing.T) {
		cases := []reminderCase{
			{Name: "sed dir", Tool: "bash", Input: bash(`sed -i 's/a/b/' x.go`), Blocks: "sed-inplace", Needs: edit},
			{Name: "sed dir retry", Tool: "bash", Input: bash(`sed -i 's/a/b/' x.go`), Blocks: "sed-inplace", Needs: edit},
			{Name: "sed dir third", Tool: "bash", Input: bash(`sed -i 's/a/b/' x.go`), Blocks: "sed-inplace", Needs: edit, Repeat: true},
			{Name: "orch lsp call", Tool: "bash", Input: bash(`orch lsp def Gateway`)},
			{Name: "sed again", Tool: "bash", Input: bash(`sed -i 's/a/b/' x.go`), Blocks: "sed-inplace", Needs: edit},
		}
		got := runCtxReminder(t, nil, cases)
		checkReminder(t, got, cases)
		if strings.Contains(got.Out["sed again"].Block, repeatedBlock) {
			t.Errorf("an allowed call did not end the run of blocks: %q", got.Out["sed again"].Block)
		}
	})
	t.Run("ORCH_TOOL_GUARD=off", func(t *testing.T) {
		cases := []reminderCase{
			{Name: "sed -i", Tool: "bash", Input: bash(`sed -i 's/a/b/' file.go`)},
			{Name: "plain rg", Tool: "bash", Input: bash(`rg -n Gateway .`), Remind: true},
		}
		got := runCtxReminder(t, []string{"ORCH_TOOL_GUARD=off"}, cases)
		checkReminder(t, got, cases)
	})
	t.Run("skip sed-inplace by short and full name", func(t *testing.T) {
		for _, skip := range []string{"sed-inplace", "deny-sed-inplace"} {
			cases := []reminderCase{{Name: "sed -i", Tool: "bash", Input: bash(`sed -i 's/a/b/' file.go`)}}
			got := runCtxReminder(t, []string{"ORCH_TOOL_GUARD_SKIP=" + skip}, cases)
			checkReminder(t, got, cases)
		}
	})
}

// A pi build without the tool_result hook falls back to blocking the first
// search once, so the note still reaches the model, and the retry always runs.
func TestCtxReminderFallbackBlocksOnceThenAllowsRetry(t *testing.T) {
	cases := []reminderCase{
		{Name: "go test first", Tool: "bash", Input: bash(`go test ./...`)},
		{Name: "plain ls", Tool: "bash", Input: bash(`ls`)},
		{Name: "first grep", Tool: "bash", Input: bash(`grep -rn Gateway .`)},
		{Name: "same grep retry", Tool: "bash", Input: bash(`grep -rn Gateway .`)},
		{Name: "first fd", Tool: "bash", Input: bash(`fd gateway`)},
		{Name: "later fd", Tool: "bash", Input: bash(`fd gateway.go`)},
	}
	got := runCtxReminder(t, []string{"NO_RESULT_HOOK=1"}, cases)
	if len(got.Handlers) != 1 || got.Handlers[0] != "tool_call" {
		t.Fatalf("fallback expected only a tool_call hook, got %v", got.Handlers)
	}
	for _, name := range []string{"go test first", "plain ls"} {
		if reason := got.Out[name].Block; reason != "" {
			t.Errorf("%q was blocked: %s", name, reason)
		}
	}
	first := got.Out["first grep"].Block
	if !strings.Contains(first, reminderMarker) || !strings.Contains(first, "jbcontext search") {
		t.Fatalf("the fallback did not block the first search with the reminder: %q", first)
	}
	for _, name := range []string{"same grep retry", "first fd", "later fd"} {
		if reason := got.Out[name].Block; reason != "" {
			t.Errorf("%q was blocked after the one reminder: %s", name, reason)
		}
	}
}

// sharedToolguardFile is one decision table for both harness guards. The Claude
// hooks in hooks/claude run every case through their own test; this runs the
// same file through the pi extension so the two implementations cannot drift.
// Rule names and replacement tools differ by harness, so only blocked vs
// allowed and the rule that blocks are compared.
type sharedToolguardFile struct {
	PathSituations map[string]struct {
		Tools []string `json:"tools"`
	} `json:"pathSituations"`
	Cases []struct {
		Origin    string `json:"origin"`
		Name      string `json:"name"`
		Situation string `json:"situation"`
		Rule      string `json:"rule"`
		Command   string `json:"command"`
		// ReasonHas and ReasonLacks list substrings the block reason must and
		// must not carry, by harness (claude, pi): the tool and parameter names
		// differ, the structure does not.
		ReasonHas   map[string][]string `json:"reasonHas"`
		ReasonLacks map[string][]string `json:"reasonLacks"`
	} `json:"cases"`
	// BuiltinCases is the third table: the built-in search tools, where the tool
	// name and the rule id differ by harness. Tool is harness-neutral, and Rule
	// names the rule each harness must block with, empty for allowed.
	BuiltinCases []struct {
		Name      string            `json:"name"`
		Situation string            `json:"situation"`
		Tool      string            `json:"tool"`
		Pattern   string            `json:"pattern"`
		Path      string            `json:"path"`
		Rule      map[string]string `json:"rule"`
	} `json:"builtinCases"`
	// ReadCases is the fourth table: a whole-file read of a big file. File is a
	// name made in a temp directory; Kind is file (default), dir or missing.
	ReadCases []struct {
		Origin    string         `json:"origin"`
		Name      string         `json:"name"`
		Situation string         `json:"situation"`
		File      string         `json:"file"`
		Kind      string         `json:"kind"`
		Bytes     int            `json:"bytes"`
		Lines     int            `json:"lines"`
		Input     map[string]any `json:"input"`
		Block     bool           `json:"block"`
	} `json:"readCases"`
}

// parentFor is the parent tool call id pi's extension sends for a script's call.
func parentFor(origin string) string {
	if origin == "script" {
		return "codemode-call-1"
	}
	return ""
}

func sharedGuardFile(t *testing.T) sharedToolguardFile {
	t.Helper()
	path := filepath.Join("..", "hooks", "claude", "toolguard_cases.json")
	var file sharedToolguardFile
	if err := json.Unmarshal(mustRead(t, path), &file); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return file
}

func TestCtxReminderSharedToolguardCases(t *testing.T) {
	file := sharedGuardFile(t)
	if len(file.Cases) == 0 || len(file.PathSituations) == 0 {
		t.Fatal("the shared case file has no cases or no PATH situations")
	}
	for situation, spec := range file.PathSituations {
		t.Run(situation, func(t *testing.T) {
			var cases []reminderCase
			for _, c := range file.Cases {
				if c.Situation != situation {
					continue
				}
				cases = append(cases, reminderCase{Name: c.Name, Tool: "bash", Input: bash(c.Command), Parent: parentFor(c.Origin)})
			}
			if len(cases) == 0 {
				t.Fatalf("no cases for PATH situation %q", situation)
			}
			got := runCtxReminderWith(t, spec.Tools, nil, cases)
			blocked := 0
			for _, c := range file.Cases {
				if c.Situation != situation {
					continue
				}
				reason := got.Out[c.Name].Block
				if c.Rule == "" {
					if reason != "" {
						t.Errorf("%q is allowed in the shared cases but pi blocked it: %s", c.Name, reason)
					}
					continue
				}
				blocked++
				// The parens keep one rule id from matching a longer one.
				if want := "(deny-" + c.Rule + ")"; !strings.Contains(reason, want) {
					t.Errorf("%q must be blocked by %s in pi: %s", c.Name, want, reason)
				}
				if strings.Contains(reason, "orch ctx") {
					t.Errorf("%q: the reason names orch ctx: %s", c.Name, reason)
				}
				for _, want := range c.ReasonHas["pi"] {
					if !strings.Contains(reason, want) {
						t.Errorf("%q: the pi reason must carry %q: %s", c.Name, want, reason)
					}
				}
				for _, bad := range c.ReasonLacks["pi"] {
					if strings.Contains(reason, bad) {
						t.Errorf("%q: the pi reason must not carry %q: %s", c.Name, bad, reason)
					}
				}
			}
			t.Logf("shared cases, %s PATH: %d cases, %d must block", situation, len(cases), blocked)
		})
	}
}

// piToolFor is the harness-neutral tool of the shared built-in table, in pi.
var piToolFor = map[string]string{"content-search": "grep", "file-search": "find"}

// The built-in search table: pi's grep and find reach the tree without a shell,
// so blocking them is the same decision in both harnesses. The shared file
// cannot know this repository's root, so it carries relative and outside
// absolute paths only; the tests below add the absolute-inside case.
func TestCtxReminderSharedBuiltinSearchCases(t *testing.T) {
	file := sharedGuardFile(t)
	if len(file.BuiltinCases) == 0 {
		t.Fatal("the shared case file has no builtinCases")
	}
	for situation, spec := range file.PathSituations {
		t.Run(situation, func(t *testing.T) {
			var cases []reminderCase
			for _, c := range file.BuiltinCases {
				if c.Situation != situation {
					continue
				}
				toolName, ok := piToolFor[c.Tool]
				if !ok {
					t.Fatalf("%q has an unknown tool %q", c.Name, c.Tool)
				}
				input := map[string]any{"pattern": c.Pattern}
				if c.Path != "" {
					input["path"] = c.Path
				}
				cases = append(cases, reminderCase{Name: c.Name, Tool: toolName, Input: input, Blocks: c.Rule["pi"]})
			}
			if len(cases) == 0 {
				t.Skip("no built-in cases for this PATH situation")
			}
			got := runCtxReminderWith(t, spec.Tools, nil, cases)
			_, quiet, blocked := checkReminder(t, got, cases)
			t.Logf("shared built-in cases, %s PATH: %d blocked, %d allowed", situation, blocked, quiet)
		})
	}
}

// The read table: pi's read tool takes the same decision as Claude's Read, a
// whole-file read of a file over 30720 bytes is refused and a range passes.
func TestCtxReminderSharedReadCases(t *testing.T) {
	file := sharedGuardFile(t)
	if len(file.ReadCases) == 0 {
		t.Fatal("the shared case file has no readCases")
	}
	dir := t.TempDir()
	for situation, spec := range file.PathSituations {
		t.Run(situation, func(t *testing.T) {
			var cases []reminderCase
			for _, c := range file.ReadCases {
				if c.Situation != situation {
					continue
				}
				path := filepath.Join(dir, situation+"-"+c.File)
				switch c.Kind {
				case "dir":
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				case "missing":
				default:
					if err := os.WriteFile(path, fileContent(c.Bytes, c.Lines), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				input := map[string]any{"path": path}
				maps.Copy(input, c.Input)
				blocks := ""
				if c.Block {
					blocks = "big-read"
				}
				needs := []string(nil)
				if c.Block {
					needs = []string{"offset and limit"}
				}
				cases = append(cases, reminderCase{Name: c.Name, Tool: "read", Input: input, Blocks: blocks, Needs: needs, Parent: parentFor(c.Origin)})
			}
			if len(cases) == 0 {
				t.Skip("no read cases for this PATH situation")
			}
			got := runCtxReminderWith(t, spec.Tools, nil, cases)
			_, quiet, blocked := checkReminder(t, got, cases)
			t.Logf("shared read cases, %s PATH: %d blocked, %d allowed", situation, blocked, quiet)
		})
	}
}

// The read switches: ORCH_TOOL_GUARD=off and the rule's short and full names.
func TestCtxReminderBigReadSwitches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 31744)), 0o644); err != nil {
		t.Fatal(err)
	}
	bare := reminderCase{Name: "bare read", Tool: "read", Input: map[string]any{"path": path}}
	blocked := bare
	blocked.Blocks = "big-read"
	checkReminder(t, runCtxReminder(t, nil, []reminderCase{blocked}), []reminderCase{blocked})
	for _, env := range []string{"ORCH_TOOL_GUARD=off", "ORCH_TOOL_GUARD_SKIP=big-read", "ORCH_TOOL_GUARD_SKIP=deny-big-read"} {
		checkReminder(t, runCtxReminder(t, []string{env}, []reminderCase{bare}), []reminderCase{bare})
	}
}

// fileContent is bytes long and split into lines lines (one line when lines is 0).
func fileContent(bytes, lines int) []byte {
	if lines <= 1 {
		return []byte(strings.Repeat("x", bytes))
	}
	per := bytes / lines
	body := strings.Repeat(strings.Repeat("x", per-1)+"\n", lines-1)
	return []byte(body + strings.Repeat("x", bytes-len(body)))
}

// A repeat read of the same path passes in pi, a different path still blocks.
func TestCtxReminderBigReadRepeatPasses(t *testing.T) {
	dir := t.TempDir()
	var paths [2]string
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("big%d.txt", i))
		if err := os.WriteFile(paths[i], []byte(strings.Repeat("x", 31744)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []reminderCase{
		{Name: "first", Tool: "read", Input: map[string]any{"path": paths[0]}, Blocks: "big-read", Needs: []string{"send this same read again and it passes"}},
		{Name: "second", Tool: "read", Input: map[string]any{"path": paths[0]}},
		{Name: "third", Tool: "read", Input: map[string]any{"path": paths[0]}},
		{Name: "other path", Tool: "read", Input: map[string]any{"path": paths[1]}, Blocks: "big-read"},
	}
	checkReminder(t, runCtxReminder(t, nil, cases), cases)
}

// runMCPRegistration loads the extension against a pi stand-in that has
// registerMcpServer, then runs one `edit` and one nested `edit` through the
// tool_result handler. The fake orch on PATH answers `lsp diag` with one error.
func runMCPRegistration(t *testing.T, withAPI bool, env []string) (registered []map[string]any, edited, nested string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	ext := filepath.Join(dir, "orch-ctx-reminder.ts")
	if err := os.WriteFile(ext, mustRead(t, "pi_ctx_reminder.ts"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, _ := filepath.EvalSymlinks(filepath.Join(dir, "repo"))
	if err := os.MkdirAll(filepath.Join(dir, "repo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, _ = filepath.EvalSymlinks(filepath.Join(dir, "repo"))
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := `#!/bin/sh
[ "$1 $2" = "lsp diag" ] || exit 0
printf '{"verb":"diag","results":[{"name":"%s/a.go","status":"ok","total":1,"items":[{"path":"%s/a.go","line":4,"kind":"error","text":"undefined: x"}]}]}' "$PWD" "$PWD"
`
	if err := os.WriteFile(filepath.Join(bin, "orch"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `
import ext from ` + string(mustJSON(t, ext)) + `;
import { mkdirSync, writeFileSync } from "node:fs";
process.chdir(process.env.ORCH_TEST_CWD);
for (const [dir, body] of [[process.env.PI_CODING_AGENT_DIR, process.env.ORCH_TEST_USER_SETTINGS], [process.env.ORCH_TEST_CWD + "/.pi", process.env.ORCH_TEST_PROJECT_SETTINGS]]) {
  if (body) { mkdirSync(dir, { recursive: true }); writeFileSync(dir + "/settings.json", body); }
}
// As pi runs it: node, pi's entry script, then the flags.
if (process.env.ORCH_TEST_ARGV) process.argv.splice(1, process.argv.length, "/pi/dist/cli.js", ...process.env.ORCH_TEST_ARGV.split(" "));
const handlers = {};
const registered = [];
const pi = { on: (name, fn) => { handlers[name] = fn; } };
if (process.env.WITH_API === "1") pi.registerMcpServer = (name, config) => registered.push({ name, config });
ext(pi);
const result = async (parent) => !handlers.tool_result ? undefined : handlers.tool_result({ type: "tool_result", toolCallId: parent ? "c/1" : "c1", toolName: "edit",
  input: { path: process.env.ORCH_TEST_CWD + "/a.go" }, content: [{ type: "text", text: "ORIGINAL" }], isError: false, parentToolCallId: parent || undefined });
const text = (res) => res ? res.content.slice(1).map((p) => p.text).join("") : "";
const edited = text(await result(""));
const nested = text(await result("cm1"));
console.log(JSON.stringify({ registered, edited, nested }));
`
	cmd := exec.Command(node, "--input-type=module", "-e", script)
	cmd.Env = agentEnv(nil, append([]string{"PATH=" + bin, "ORCH_LOOKUP_READY=", "ORCH_BIN=", "ORCH_MCP=", "ORCH_LSP_DIAG=",
		"ORCH_TEST_CWD=" + repo, "WITH_API=" + map[bool]string{true: "1", false: "0"}[withAPI],
		"ORCH_GUARD_BIN=" + guardBin(t), "ORCH_TOOL_GUARD_STATE_DIR=" + filepath.Join(dir, "state"),
		"ORCH_CACHE_DIR=" + filepath.Join(dir, "cache"), "PI_CODING_AGENT_DIR=" + filepath.Join(dir, "agent"),
		"ORCH_TEST_USER_SETTINGS=", "ORCH_TEST_PROJECT_SETTINGS=", "ORCH_TEST_ARGV="}, env...))
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got struct {
		Registered []map[string]any `json:"registered"`
		Edited     string           `json:"edited"`
		Nested     string           `json:"nested"`
	}
	if err := json.Unmarshal(raw[strings.Index(string(raw), "{"):], &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return got.Registered, got.Edited, got.Nested
}

func TestCtxReminderRegistersTheOrchMCPServer(t *testing.T) {
	reg, edited, nested := runMCPRegistration(t, true, nil)
	if len(reg) != 1 || reg[0]["name"] != "orch" {
		t.Fatalf("registrations %v", reg)
	}
	cfg := reg[0]["config"].(map[string]any)
	args, _ := json.Marshal(cfg["args"])
	if !strings.HasSuffix(fmt.Sprint(cfg["command"]), "/bin/orch") || string(args) != `["mcp"]` || cfg["exposure"] != "codemode" || cfg["description"] == "" {
		t.Errorf("config %v", cfg)
	}
	// The same extension appends the error of a direct edit and nothing to a nested one.
	if edited != "orch diagnostics:\na.go:4: undefined: x" {
		t.Errorf("edit result got %q", edited)
	}
	if nested != "" {
		t.Errorf("nested edit result got %q", nested)
	}
}

func TestCtxReminderMCPRegistrationSwitches(t *testing.T) {
	for name, tc := range map[string]struct {
		withAPI bool
		env     []string
		want    int
	}{
		"pi without the API":    {false, nil, 0},
		"ORCH_MCP=off":          {true, []string{"ORCH_MCP=off"}, 0},
		"restricted gate shell": {true, []string{"ORCH_LOOKUP_READY=1"}, 0},
		"no orch binary":        {true, []string{"PATH=/nonexistent"}, 0},
		// pi reports a registered server that no MCP support connects as an
		// extension error in every session, so nothing is registered then.
		"user settings turn off pi MCP": {true, []string{`ORCH_TEST_USER_SETTINGS={"extensions":["-builtin:mcp"]}`}, 0},
		"project settings turn it back on": {true, []string{`ORCH_TEST_USER_SETTINGS={"extensions":["-builtin:mcp"]}`,
			`ORCH_TEST_PROJECT_SETTINGS={"extensions":["+builtin:mcp"]}`}, 1},
		"project settings turn it off":      {true, []string{`ORCH_TEST_PROJECT_SETTINGS={"extensions":["-builtin:mcp"]}`}, 0},
		"other extensions turned off":       {true, []string{`ORCH_TEST_USER_SETTINGS={"extensions":["-builtin:llama.cpp"]}`}, 1},
		"--no-mcp":                          {true, []string{"ORCH_TEST_ARGV=--no-mcp"}, 0},
		"--no-extensions":                   {true, []string{"ORCH_TEST_ARGV=-ne"}, 0},
		"-e builtin:mcp loads it for a run": {true, []string{`ORCH_TEST_USER_SETTINGS={"extensions":["-builtin:mcp"]}`, "ORCH_TEST_ARGV=-e builtin:mcp"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			if reg, _, _ := runMCPRegistration(t, tc.withAPI, tc.env); len(reg) != tc.want {
				t.Errorf("registrations %v, want %d", reg, tc.want)
			}
		})
	}
	// $ORCH_BIN wins over PATH.
	fake := filepath.Join(t.TempDir(), "my-orch")
	os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755)
	reg, _, _ := runMCPRegistration(t, true, []string{"ORCH_BIN=" + fake})
	if len(reg) != 1 || reg[0]["config"].(map[string]any)["command"] != fake {
		t.Errorf("ORCH_BIN not used: %v", reg)
	}
}
