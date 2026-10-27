package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The extension decides in pi, so the decision table runs the real file in node
// (which strips its types) against a stand-in for pi's extension API.
func TestCtxOnlyExtensionAllowsOnlyOneLookupCall(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	ext, err := writeCtxOnlyExtension(dir)
	if err != nil {
		t.Fatal(err)
	}
	allowed, blocked := ctxOnlyCases(t)
	script := `
import ext from ` + string(mustJSON(t, ext)) + `;
let handler, start, active;
ext({
  on: (name, fn) => { if (name === "tool_call") handler = fn; if (name === "session_start") start = fn; },
  setActiveTools: (tools) => { if (!handler) throw Error("shell enabled before guard"); active = tools; },
  getActiveTools: () => [...active],
});
const out = {};
await start();
if (active.join(",") !== "read,bash") throw Error("wrong active tools");
for (const c of JSON.parse(process.argv[1])) {
  const r = await handler({ toolName: "bash", input: { command: c } });
  out[c] = r && r.block ? r.reason : "";
}
const other = await handler({ toolName: "read", input: { path: "a.go" } });
out["<read tool>"] = other ? "blocked" : "";
console.log(JSON.stringify(out));
`
	all, _ := json.Marshal(append(append([]string{}, allowed...), blocked...))
	cmd := exec.Command(node, "--input-type=module", "-e", script, string(all))
	ready := filepath.Join(dir, "ready")
	cmd.Env = agentEnv(nil, []string{"ORCH_CTX_ONLY_READY=" + ready})
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if got := string(mustRead(t, ready)); got != "ctx-only\n" {
		t.Fatalf("extension did not acknowledge guarded activation: %q", got)
	}
	var got map[string]string
	start := strings.Index(string(out), "{")
	if start < 0 {
		t.Fatalf("node produced no JSON result:\n%s", out)
	}
	if err := json.Unmarshal(out[start:], &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, c := range allowed {
		if reason, ok := got[c]; !ok || reason != "" {
			t.Errorf("allowed call %q: present %v, reason %q", c, ok, reason)
		}
	}
	for _, c := range blocked {
		if !strings.Contains(got[c], "separate calls in one message") || !strings.Contains(got[c], "jbcontext search, orch lsp, rg, fd, ls or wc") {
			t.Errorf("did not block %q (reason %q)", c, got[c])
		}
	}
	if got["<read tool>"] != "" {
		t.Error("the extension must only look at the bash tool")
	}
}

func TestGateCtxShellDefaultAndExplicitOpen(t *testing.T) {
	for _, mode := range []string{"default", "open", "off"} {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			dir := h.gateRepo(1)
			h.fakePi(gatePasses(nil))
			t.Setenv("HERDR_ENV", "")
			args := []string{"gate", "spec.md"}
			want := "ctx-only"
			switch mode {
			case "open":
				args = append(args, "--ctx-shell", "open")
				want = "open"
			case "off":
				args = append(args, "--ctx-tool", "off")
				want = "open"
			}
			h.must(args...)
			rec := readRecord(t, dir)
			if rec.CtxShell != want || rec.ReviewPolicy.CtxShell != want {
				t.Fatalf("effective shell: %+v", rec)
			}
			calls := string(mustRead(t, filepath.Join(h.tmp, "fakepi", "calls.log")))
			if mode == "default" {
				wantContains(t, calls, "--no-builtin-tools --tools +read -e", "orch-ctx-only.ts", ctxOnlyNotice)
			} else if strings.Contains(calls, "orch-ctx-only.ts") {
				t.Fatal("open/off unexpectedly loaded the shell guard: " + calls)
			}
			if mode == "open" {
				wantContains(t, calls, "--tools read,grep,find,ls,bash")
			}
			wantContains(t, string(mustRead(t, filepath.Join(dir, "report.md"))), "ctx_shell: "+want)
		})
	}
}

func TestGateCtxShellCompareKeepsLegacyOpen(t *testing.T) {
	for _, mode := range []string{"", "open", "ctx-only"} {
		t.Run(orDefault(mode, "legacy"), func(t *testing.T) {
			var out strings.Builder
			describeExperiment(&out, "A", "shell", &gateRecord{CtxShell: mode}, t.TempDir())
			wantContains(t, out.String(), "ctx_shell: "+orDefault(mode, "open"))
		})
	}
}

// A record written while --ctx-profile existed still loads, and its profile still
// shows; a record without one shows none. The flag itself is gone.
func TestGateRecordWithRemovedCtxProfileStillLoads(t *testing.T) {
	var old gateRecord
	if err := json.Unmarshal(mustRead(t, filepath.Join("testdata", "gate-record-ctx-profile.json")), &old); err != nil {
		t.Fatal(err)
	}
	if old.CtxProfile != "lean" || old.ReviewPolicy == nil || old.ReviewPolicy.CtxProfile != "lean" {
		t.Fatalf("the stored profile was lost: %+v", old)
	}
	var out strings.Builder
	describeExperiment(&out, "A", "lean-vs-default", &old, t.TempDir())
	wantContains(t, out.String(), "(cache short, groups group)", "ctx_shell: ctx-only; context_files: off; ctx_profile: lean")
	out.Reset()
	describeExperiment(&out, "B", "new", &gateRecord{CtxShell: "ctx-only"}, t.TempDir())
	for _, gone := range []string{"ctx_profile", "context_files", "cache short"} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("a new record shows %s: %s", gone, out.String())
		}
	}
}

// The removed --context-files, --cache and --keep-sessions switches left their
// fields in old records: they load and the report still prints them.
func TestGateRecordWithRemovedSwitchesPrintsInReport(t *testing.T) {
	var old gateRecord
	if err := json.Unmarshal(mustRead(t, filepath.Join("testdata", "gate-record-ctx-profile.json")), &old); err != nil {
		t.Fatal(err)
	}
	if old.Cache != "short" || old.ContextFiles != "off" || old.ReviewPolicy.ContextFiles != "off" {
		t.Fatalf("a removed field was lost on load: %+v", old)
	}
	dir := t.TempDir()
	if err := writeGate(dir, &old); err != nil {
		t.Fatal(err)
	}
	wantContains(t, string(mustRead(t, filepath.Join(dir, "report.md"))), "ctx_tool: on; ctx_shell: ctx-only; context_files: off; ctx_profile: lean")
}

func TestGateCtxProfileFlagWasRemoved(t *testing.T) {
	h := setup(t)
	for _, arg := range []string{"--ctx-profile", "-ctx-profile", "--ctx-profile=lean"} {
		code, out := h.run("gate", "spec.md", arg, "lean")
		if code == 0 || !strings.Contains(out, "--ctx-profile was removed") {
			t.Errorf("%s: exit %d, output %q", arg, code, out)
		}
	}
}

func TestGateCtxOnlyExtensionWriteFailsClosed(t *testing.T) {
	h := setup(t)
	dir := h.gateRepo(1)
	h.fakePi(gatePasses(nil))
	cli, err := CtxCLIFor("pi", h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cli.Extension); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cli.Extension, 0o700); err != nil {
		t.Fatal(err)
	}
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("extension write failure approved: %d %s", code, out)
	}
	rec := readRecord(t, dir)
	b, _ := json.Marshal(rec.PassErrors)
	wantContains(t, string(b), "ctx-only extension setup failed; refusing an open shell")
	if rec.State == "pass" || len(rec.PassErrors) == 0 {
		t.Fatalf("write failure not recorded: %+v", rec)
	}
	if calls, _ := os.ReadFile(filepath.Join(h.tmp, "fakepi", "calls.log")); len(calls) != 0 {
		t.Fatalf("pi ran despite setup failure: %s", calls)
	}
}

func TestGateCtxOnlyExtensionLoadFailsClosed(t *testing.T) {
	for _, mode := range []string{"headless", "pane"} {
		t.Run(mode, func(t *testing.T) {
			h := setup(t)
			dir := h.gateRepo(1)
			h.sessionPi(gatePasses(nil))
			if mode == "headless" {
				t.Setenv("HERDR_ENV", "")
			}
			// Pi can report an import error and continue with a valid-looking reply.
			// Simulate exactly that, without running any installed harness/model.
			bin := filepath.Join(h.tmp, "fakepi", "pi.real")
			script := string(mustRead(t, bin))
			script = strings.Replace(script, "# Stand in for the loaded extension's session_start acknowledgement.", "echo 'Failed to load extension orch-ctx-only.ts' >&2\nunset ORCH_CTX_ONLY_READY", 1)
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			code, out := h.run("gate", "spec.md")
			if code != 1 {
				t.Fatalf("extension failure approved: %d %s", code, out)
			}
			rec := readRecord(t, dir)
			if rec.State == "pass" || len(rec.PassErrors) == 0 {
				t.Fatalf("extension failure not recorded: %+v", rec)
			}
			for _, reason := range rec.Reduction.Reasons {
				if reason.Reason != reviewDependencyUnavailable {
					t.Fatalf("unexpected execution reason: %+v", reason)
				}
			}
			b, _ := json.Marshal(rec.PassErrors)
			wantContains(t, string(b), "ctx-only extension did not load or activate")
			calls := string(mustRead(t, filepath.Join(h.tmp, "fakepi", "calls.log")))
			if mode == "pane" {
				calls = ""
				for _, c := range h.gateCalls("agent", "start") {
					calls += strings.Join(c.Args, " ") + "\n"
				}
			}
			wantContains(t, calls, "--no-builtin-tools --tools +read -e")
			if strings.Contains(calls, "--tools read,bash") || strings.Contains(calls, "read,grep") {
				t.Fatal("failed guard exposed shell at startup: " + calls)
			}
		})
	}
}

// ctxOnlyCases is the case table shared by the pi extension (node) and orch ctx-only-check.
func ctxOnlyCases(t *testing.T) (allowed, blocked []string) {
	t.Helper()
	var c struct{ Allowed, Blocked []string }
	if err := json.Unmarshal(mustRead(t, filepath.Join("testdata", "ctx_only_cases.json")), &c); err != nil {
		t.Fatal(err)
	}
	return c.Allowed, c.Blocked
}

func TestCtxOnlyRefusalMatchesTheSharedCaseTable(t *testing.T) {
	allowed, blocked := ctxOnlyCases(t)
	for _, c := range allowed {
		if why := ctxOnlyRefusal(c); why != "" {
			t.Errorf("allowed call %q refused: %s", c, why)
		}
	}
	for _, c := range blocked {
		if ctxOnlyRefusal(c) == "" {
			t.Errorf("did not refuse %q", c)
		}
	}
}

func TestCtxOnlyCheckHookExitCodes(t *testing.T) {
	run := func(payload string) (int, string) {
		var out strings.Builder
		a := &app{stdin: strings.NewReader(payload), out: &out, errw: &out}
		return a.run([]string{"ctx-only-check"}), out.String()
	}
	for _, c := range []struct {
		name, payload string
		code          int
		want          string
	}{
		{"one lookup", `{"tool_name":"Bash","tool_input":{"command":"rg -n -w Save internal"}}`, 0, ""},
		{"exec flag", `{"tool_name":"Bash","tool_input":{"command":"rg --pre=./evil.sh x"}}`, 2, "rg --pre runs another program"},
		{"fd exec", `{"tool_name":"Bash","tool_input":{"command":"fd -Hx rm"}}`, 2, "fd -Hx runs another program"},
		{"compound", `{"tool_name":"Bash","tool_input":{"command":"rg x; rm -rf y"}}`, 2, "Blocked: no unquoted"},
		{"other tool", `{"tool_name":"Read","tool_input":{"file_path":"a.go"}}`, 0, ""},
		{"bad json", `not json`, 2, "could not parse"},
	} {
		code, out := run(c.payload)
		if code != c.code || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d %q, want %d containing %q", c.name, code, out, c.code, c.want)
		}
	}
}

func TestClaudeGatePassCarriesTheCtxOnlyHook(t *testing.T) {
	h := setup(t)
	pass := func(ctxTool bool) []string {
		p, err := gateAgentPass(h.repo, "claude", gatePass{name: "a", shared: "s", task: "t", prompt: "st", ctxTool: ctxTool, ctxOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		return claudePassArgs(profile{Model: "m"}, p)
	}
	settings := func(args []string) string {
		for i, a := range args {
			if a == "--settings" {
				return args[i+1]
			}
		}
		t.Fatal("no --settings in " + strings.Join(args, " "))
		return ""
	}
	var got struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string
				Hooks   []struct{ Type, Command string }
			}
		}
	}
	if err := json.Unmarshal([]byte(settings(pass(true))), &got); err != nil {
		t.Fatal(err)
	}
	pre := got.Hooks.PreToolUse
	if len(pre) != 1 || pre[0].Matcher != "Bash" || len(pre[0].Hooks) != 1 || pre[0].Hooks[0].Type != "command" ||
		!strings.HasSuffix(pre[0].Hooks[0].Command, "/orch' ctx-only-check") {
		t.Fatalf("hook settings: %+v", got)
	}
	if strings.Contains(settings(pass(false)), "ctx-only-check") {
		t.Error("a pass without the ctx tool has no shell to guard")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCtxOnlyShellLimitsPiToReadAndTheExtension(t *testing.T) {
	h := setup(t)
	open, err := gateAgentPass(h.repo, "pi", gatePass{name: "a", shared: "s", task: "t", prompt: "st", ctxTool: true})
	if err != nil {
		t.Fatal(err)
	}
	only, err := gateAgentPass(h.repo, "pi", gatePass{name: "a", shared: "s", task: "t", prompt: "st", ctxTool: true, ctxOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(piPassTools(open), " "); got != "--tools read,grep,find,ls,bash" {
		t.Errorf("the open shell changed: %s", got)
	}
	want := "--no-builtin-tools --tools +read -e " + filepath.Join(filepath.Dir(only.cli.Extension), "orch-ctx-only.ts")
	if got := strings.Join(piPassTools(only), " "); got != want {
		t.Errorf("ctx-only tools = %q, want %q", got, want)
	}
	if _, err := os.Stat(only.cli.Extension); err != nil {
		t.Errorf("extension file: %v", err)
	}
	if !strings.Contains(only.shared, ctxOnlyNotice) || strings.Contains(open.shared, ctxOnlyNotice) {
		t.Error("only the ctx-only pass is told its shell runs only orch ctx")
	}
	// Claude has its own permission list and is untouched.
	cl, err := gateAgentPass(h.repo, "claude", gatePass{name: "a", shared: "s", task: "t", prompt: "st", ctxTool: true, ctxOnly: true})
	if err != nil || cl.cli.Extension != "" || strings.Contains(cl.shared, ctxOnlyNotice) {
		t.Errorf("claude pass: ext %q err %v", cl.cli.Extension, err)
	}
}
