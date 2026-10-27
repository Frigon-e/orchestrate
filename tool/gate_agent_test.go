package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// A gate pass carries no lookup CLI: no notice, no shell, no environment.
func TestGateAgentPassHasNoShell(t *testing.T) {
	h := setup(t)
	base := newGatePass("a", "package", "shared", "own", "TAG", "task", `{}`, nil)
	for _, harness := range []string{"pi", "claude"} {
		p, err := gateAgentPass(h.repo, harness, base)
		if err != nil {
			t.Fatal(err)
		}
		if p.shared != base.shared || p.prompt != base.prompt || strings.Contains(p.prompt, "READ ONLY.") {
			t.Fatalf("%s changed the prompt: %+v", harness, p)
		}
		if harness == "pi" {
			if got := strings.Join(piPassTools(p), " "); got != "--tools read,grep,find,ls" {
				t.Fatal(got)
			}
		} else {
			args := claudePassArgs(profile{Harness: "claude", Model: "haiku"}, p)
			if strings.Contains(flagValue(args, "--tools"), "Bash") || strings.Contains(flagValue(args, "--allowedTools"), "Bash") || strings.Contains(flagValue(args, "--settings"), "hooks") {
				t.Fatal(args)
			}
		}
	}
	for _, fn := range []func(int) string{budgetSentence, budgetSentenceItems} {
		if strings.Contains(fn(10), "jbcontext") {
			t.Fatal("budget sentence names a lookup command")
		}
	}
}

func TestGateHeadlessArguments(t *testing.T) {
	for _, harness := range []string{"pi", "claude"} {
		t.Run(harness, func(t *testing.T) {
			h := setup(t)
			log := filepath.Join(h.tmp, "args")
			script := fmt.Sprintf("printf '%%s\\n' \"$@\" > %q\n", log)
			if harness == "pi" {
				script += "printf '{}'"
			} else {
				script += `printf '%s\n' '{"type":"result","result":"{}"}'`
			}
			t.Setenv("ORCH_"+strings.ToUpper(harness), fakeRunner(t, harness, script))
			p := gatePass{name: "test", shared: "shared", task: "task", prompt: "sharedtask"}
			if _, err := runHeadless(h.repo, profile{Harness: harness, Model: "test"}, p, filepath.Join(h.tmp, "sessions")); err != nil {
				t.Fatal(err)
			}
			args := string(mustRead(t, log))
			if strings.Contains(args, "READ ONLY.") || strings.Contains(args, "bash") || strings.Contains(args, "Bash") {
				t.Fatal(args)
			}
			if harness == "pi" {
				if !strings.Contains(args, "--no-context-files") || !strings.Contains(args, "read,grep,find,ls") {
					t.Fatal(args)
				}
			} else if !strings.Contains(args, "claudeMdExcludes") {
				t.Fatal(args)
			}
		})
	}
}

// A fresh pane's shell may still be starting when a pass begins, and a macOS
// terminal drops typed input past about 1 KB: typed exports plus the pi command
// once merged into a garbled line and pi never started. So a pass types one
// short line, whatever its arguments, and the launcher hands the agent its
// argv intact. PATH is the pane's own: orch never edits it.
func TestGatePaneTypesOneShortLineAndKeepsThePanesPath(t *testing.T) {
	for _, harness := range []string{"pi", "claude"} {
		t.Run(harness, func(t *testing.T) {
			h := setup(t)
			h.gateRepo(1)
			h.sessionPi(gatePasses(nil))
			t.Setenv("ORCH_FAKE_CLAUDE_REPO", h.repo)
			runner := "pi:" + gateModelDef
			if harness == "claude" {
				t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "exit 0"))
				runner = "claude:haiku:low"
			}
			h.must("gate", "spec.md", "--runner", runner)
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
				if !strings.Contains(script, "\nexec '"+harness+"' ") {
					t.Errorf("launcher lacks the exec line:\n%s", script)
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
		})
	}
}

// A gate pane gets no lookup setup: no notice in the prompt, no ctx exports in
// the launcher, and the record names no ctx settings.
func TestGatePaneHasNoCtxSetup(t *testing.T) {
	for _, harness := range []string{"pi", "claude"} {
		t.Run(harness, func(t *testing.T) {
			h := setup(t)
			gdir := h.gateRepo(1)
			h.fakePi(gatePasses(nil))
			t.Setenv("ORCH_FAKE_CLAUDE_REPO", h.repo)
			h.must("gate", "spec.md", "--runner", harness+":test")
			starts := 0
			for _, c := range h.gateCalls() {
				if script, _ := c.Extra["script"].(string); strings.Contains(script, "export ORCH_CTX") {
					t.Errorf("launcher exports ctx env:\n%s", script)
				}
				if len(c.Args) < 2 || c.Args[0] != "agent" || c.Args[1] != "start" {
					continue
				}
				starts++
				args := strings.Join(c.Args, " ")
				if prompt, _ := c.Extra["prompt"].(string); strings.HasPrefix(prompt, "READ ONLY.") {
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
			if starts == 0 {
				t.Fatal("no agent started")
			}
			rec := readRecord(t, gdir)
			if rec.CtxTool != "" || rec.CtxShell != "" || rec.ContextFiles != "" {
				t.Fatal(rec)
			}
		})
	}
}

func TestGateExperimentRecordsShowNoCtxSettings(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	h.fakePi(gatePasses(nil))
	h.must("gate", "spec.md", "--only", "api", "--experiment", "cli")
	h.must("gate", "spec.md", "--only", "api", "--experiment", "default")
	for _, name := range []string{"cli", "default"} {
		rec := readRecord(t, filepath.Join(gdir, "experiments", name))
		if rec.CtxTool != "" || rec.ContextFiles != "" || rec.Cache != "" {
			t.Fatal(rec)
		}
	}
	out := h.must("gate", "compare", "spec.md", "cli", "default")
	if strings.Contains(out, "ctx_") || strings.Contains(out, "context_files") || strings.Contains(out, "(cache ") {
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
