package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestGateAgentPrefixAndTools(t *testing.T) {
	h := setup(t)
	base := newGatePass("a", "package", "shared", "own", "TAG", "task", `{}`, nil)
	base.ctxTool = true
	var prefixes []string
	for _, harness := range []string{"pi", "claude"} {
		p, err := gateAgentPass(h.repo, harness, base)
		if err != nil {
			t.Fatal(err)
		}
		prefixes = append(prefixes, p.shared)
		if !strings.HasPrefix(p.shared, "READ ONLY.") || !strings.HasPrefix(p.prompt, p.shared) || !strings.Contains(p.task, ctxBudgetHint) {
			t.Fatalf("prompt: %+v", p)
		}
		if !strings.Contains(strings.Join(p.cli.Env, "\n"), "ORCH_CTX_ROOT="+evalOr(h.repo)) {
			t.Fatal(p.cli.Env)
		}
		if harness == "pi" && !strings.Contains(strings.Join(piPassTools(p), " "), "read,grep,find,ls,bash") {
			t.Fatal(piPassTools(p))
		}
		if harness == "claude" {
			args := claudePassArgs(profile{Harness: "claude", Model: "haiku"}, p)
			if flagValue(args, "--tools") != "Read,Grep,Glob,Bash" || !strings.Contains(flagValue(args, "--allowedTools"), "Bash(orch ctx:*)") {
				t.Fatal(args)
			}
		}
		twice, _ := gateAgentPass(h.repo, harness, p)
		if twice.prompt != p.prompt || twice.shared != p.shared {
			t.Fatal("notice duplicated")
		}
	}
	if prefixes[0] != prefixes[1] {
		t.Fatal("shared notice differs by harness")
	}
	off := base
	off.ctxTool = false
	p, err := gateAgentPass(h.repo, "pi", off)
	if err != nil || p.shared != off.shared || p.prompt != off.prompt || len(p.cli.Env) != 0 {
		t.Fatal("off changed prompt/env")
	}
	for _, fn := range []func(int, ...bool) string{budgetSentence, budgetSentenceItems} {
		if strings.Contains(fn(10), ctxBudgetHint) || !strings.Contains(fn(10, true), ctxBudgetHint) {
			t.Fatal("budget toggle")
		}
	}
}

func TestGateHeadlessArgumentsAndEnv(t *testing.T) {
	for _, harness := range []string{"pi", "claude"} {
		for _, ctxOn := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ctx-%v", harness, ctxOn), func(t *testing.T) {
				h := setup(t)
				log := filepath.Join(h.tmp, "args")
				envlog := filepath.Join(h.tmp, "env")
				script := fmt.Sprintf("printf '%%s\\n' \"$@\" > %q\nprintf '%%s\\n' \"$ORCH_CTX_ROOT\" > %q\n", log, envlog)
				if harness == "pi" {
					script += "printf '{}'"
				} else {
					script += `printf '%s\n' '{"type":"result","result":"{}"}'`
				}
				t.Setenv("ORCH_"+strings.ToUpper(harness), fakeRunner(t, harness, script))
				t.Setenv("ORCH_CTX_ROOT", "")
				p := gatePass{name: "test", shared: "shared", task: "task", prompt: "sharedtask", ctxTool: ctxOn}
				if _, err := runHeadless(h.repo, profile{Harness: harness, Model: "test"}, p, filepath.Join(h.tmp, "sessions")); err != nil {
					t.Fatal(err)
				}
				args := string(mustRead(t, log))
				env := strings.TrimSpace(string(mustRead(t, envlog)))
				if ctxOn && evalOr(env) != evalOr(h.repo) || !ctxOn && env != "" {
					t.Fatalf("env: %q", env)
				}
				if strings.Contains(args, "READ ONLY.") != ctxOn {
					t.Fatal(args)
				}
				if harness == "pi" {
					if !strings.Contains(args, "--no-context-files") {
						t.Fatal(args)
					}
					if strings.Contains(args, "read,grep,find,ls,bash") != ctxOn {
						t.Fatal(args)
					}
				} else {
					if !strings.Contains(args, "claudeMdExcludes") {
						t.Fatal(args)
					}
					if strings.Contains(args, "Bash(orch ctx:*)") != ctxOn {
						t.Fatal(args)
					}
				}
			})
		}
	}
}

// A fresh pane's shell may still be starting when a pass begins, and a macOS
// terminal drops typed input past about 1 KB: typed exports plus the pi command
// once merged into a garbled line and pi never started. So a pass types one
// short line, whatever its env and arguments, and the launcher hands pi the
// env and argv intact. PATH is the pane's own: orch never edits it.
func TestGatePaneTypesOneShortLineAndKeepsThePanesPath(t *testing.T) {
	for _, harness := range []string{"pi", "claude"} {
		t.Run(harness, func(t *testing.T) {
			h := setup(t)
			h.gateRepo(1)
			h.sessionPi(gatePasses(nil))
			t.Setenv("ORCH_FAKE_CLAUDE_REPO", h.repo)
			long := "/var/folders/" + strings.Repeat("deep/", 60) + "guard.jsonl"
			t.Setenv("ORCH_TOOL_GUARD_STATE_DIR", long)
			t.Setenv("ORCH_TOOL_GUARD_SKIP", "it's \"quoted\" $HOME `x`")
			t.Setenv("ORCH_TSLS", long+"-tsls")
			runner := "pi:" + gateModelDef
			if harness == "claude" {
				t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "exit 0"))
				runner = "claude:haiku:low"
			}
			h.must("gate", "spec.md", "--runner", runner, "--ctx-shell", "open")
			runs := h.gateCalls("pane", "run")
			if len(runs) != 3 {
				t.Fatalf("pane run calls = %d, want one per pass", len(runs))
			}
			for _, c := range runs {
				cmd := strings.Join(c.Args[3:], " ")
				if len(cmd) > 300 || !strings.HasPrefix(cmd, "sh '") {
					t.Fatalf("pane run typed %d characters, want one short sh line: %.120s", len(cmd), cmd)
				}
				script := fmt.Sprint(c.Extra["script"])
				if strings.Contains(script, "PATH=") {
					t.Errorf("launcher edits PATH:\n%s", script)
				}
				for _, want := range []string{"export ORCH_CTX_ROOT=", "export ORCH_CTX_READONLY='1'", "export ORCH_TSLS=", "\nexec '" + harness + "' "} {
					if !strings.Contains(script, want) {
						t.Errorf("launcher lacks %q:\n%s", want, script)
					}
				}
			}
			starts := h.gateCalls("agent", "start")
			if len(starts) != 3 {
				t.Fatalf("agents started = %d, want 3", len(starts))
			}
			renamed := map[string]bool{}
			for _, c := range h.gateCalls("agent", "rename") {
				renamed[fmt.Sprint(c.Extra["label"])] = true
			}
			for _, c := range starts {
				if !agentNameRe.MatchString(c.Args[2]) || !renamed[c.Args[2]] {
					t.Errorf("agent %q was not named after it started: %v", c.Args[2], renamed)
				}
				if harness == "claude" && !strings.Contains(fmt.Sprint(c.Extra["prompt"]), "\n\n") {
					t.Errorf("the claude prompt lost its line breaks in the launcher")
				}
			}
			// The env reached the agent byte for byte through the launcher's quoting.
			env := string(mustRead(t, filepath.Join(h.fake+".panes", flagValue(starts[0].Args, "--pane")+".launch.env")))
			for _, want := range []string{"ORCH_TOOL_GUARD_STATE_DIR=" + long + "\x00", "ORCH_TOOL_GUARD_SKIP=it's \"quoted\" $HOME `x`\x00"} {
				if harness == "claude" && strings.HasPrefix(want, "ORCH_TOOL_GUARD") {
					continue // the pi tool guard's knobs are only for pi
				}
				if !strings.Contains(env, want) {
					t.Errorf("agent env lacks %q", want)
				}
			}
		})
	}
}

func TestGatePaneCtxFlag(t *testing.T) {
	for _, harness := range []string{"pi", "claude"} {
		for _, on := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ctx-%v", harness, on), func(t *testing.T) {
				h := setup(t)
				gdir := h.gateRepo(1)
				h.fakePi(gatePasses(nil))
				t.Setenv("ORCH_FAKE_CLAUDE_REPO", h.repo)
				h.must("gate", "spec.md", "--runner", harness+":test", "--ctx-tool", onOff(on))
				calls := h.gateCalls()
				starts := 0
				exports := 0
				for _, c := range calls {
					if script, _ := c.Extra["script"].(string); strings.Contains(script, "\nexport ORCH_CTX_ROOT=") {
						exports++
					}
					if len(c.Args) < 2 || c.Args[0] != "agent" || c.Args[1] != "start" {
						continue
					}
					starts++
					args := strings.Join(c.Args, " ")
					prompt, _ := c.Extra["prompt"].(string)
					if strings.HasPrefix(prompt, "READ ONLY.") != on {
						t.Fatal(prompt)
					}
					if harness == "pi" {
						if flagValue(c.Args, "--kind") != "pi" || !strings.Contains(args, " -- --no-approve ") {
							t.Fatalf("pi start can wait on the project trust dialog: %s", args)
						}
						if !strings.Contains(args, "--no-context-files") {
							t.Fatal(args)
						}
					}
					if harness == "claude" && !strings.Contains(args, "claudeMdExcludes") {
						t.Fatal(args)
					}
				}
				if starts == 0 || on && exports == 0 || !on && exports != 0 {
					t.Fatalf("starts %d exports %d", starts, exports)
				}
				rec := readRecord(t, gdir)
				if rec.CtxTool != onOff(on) || rec.ContextFiles != "" {
					t.Fatal(rec)
				}
			})
		}
	}
}

func TestGateToggleValidationAndExperiment(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	h.fakePi(gatePasses(nil))
	if code, out := h.run("gate", "spec.md", "--ctx-tool", "maybe"); code != 1 || !strings.Contains(out, "is on or off") {
		t.Fatalf("%d %s", code, out)
	}
	h.must("gate", "spec.md", "--only", "api", "--experiment", "cli", "--ctx-tool", "off")
	rec := readRecord(t, filepath.Join(gdir, "experiments", "cli"))
	if rec.CtxTool != "off" || rec.ContextFiles != "" || rec.Cache != "" {
		t.Fatal(rec)
	}
	h.must("gate", "spec.md", "--only", "api", "--experiment", "default")
	rec = readRecord(t, filepath.Join(gdir, "experiments", "default"))
	if rec.CtxTool != "on" || rec.ContextFiles != "" || rec.Cache != "" {
		t.Fatal(rec)
	}
	out := h.must("gate", "compare", "spec.md", "cli", "default")
	wantContains(t, out, "ctx_tool: off;", "ctx_tool: on;")
	if strings.Contains(out, "context_files") || strings.Contains(out, "(cache ") {
		t.Fatalf("a new record shows a removed switch:\n%s", out)
	}
}

func TestGateRemovedFlagsFailWithOneLine(t *testing.T) {
	h := setup(t)
	h.gateRepo(1)
	for flag, want := range map[string]string{
		"--cache":         "--cache was removed",
		"--keep-sessions": "--keep-sessions was removed",
		"--context-files": "--context-files was removed",
		"--headless":      "--headless was removed; gate passes run in herdr panes",
	} {
		for _, args := range [][]string{{"gate", "spec.md", flag}, {"gate", "spec.md", flag + "=on"}} {
			if code, out := h.run(args...); code != 1 || !strings.Contains(out, want) {
				t.Errorf("%v: %d %s", args, code, out)
			}
		}
	}
}
