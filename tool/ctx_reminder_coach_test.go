package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The lookup coach runs in pi through the same extension file, which calls
// orch-guard. These tests drive the real extension in node (against the real
// binary) with a stand-in for pi's extension API, feeding the
// events pi sends (turn_start, tool_result, tool_execution_end) and reading the
// lookup log it writes. The classification and reminder tables are shared with
// the Claude hook (hooks/claude/coach_cases.json, run by tool/guard/coach_test.go).

const coachPrefix = "orch lookup coach: "

var coachTexts = map[string]string{
	"concept-first":  `Location unknown? Ask jbcontext first: jbcontext search "<one behavior>", one concept per query, several queries in one message.`,
	"symbol-via-lsp": "For a definition, references or callers use orch lsp def|refs|callers <Name>; rg finds text, not uses.",
	"batch-lookups":  "Send independent lookups together as parallel calls in one message; each one alone costs a turn.",
	"read-batch":     "Read the ranges you already know as parallel reads in one message.",
	"lookup-ladder":  `Before searching: location unknown -> jbcontext search "<one behavior>"; known symbol -> orch lsp def|refs|callers|impls <Name>; exact text -> rg. Send independent lookups as parallel calls in one message. Read known files or ranges with Read, not cat or sed -n.`,
}

type coachCall struct {
	Name string         `json:"name,omitempty"`
	Bash *string        `json:"bash,omitempty"`
	Read map[string]any `json:"read,omitempty"`
	LSP  map[string]any `json:"lsp,omitempty"`
	MCP  map[string]any `json:"mcp,omitempty"`
	Text string         `json:"text,omitempty"`
	// Prompt marks a user prompt (pi: before_agent_start), not a tool call.
	Prompt bool `json:"prompt,omitempty"`
	Failed bool `json:"failed,omitempty"`
	// Harness limits a case to one harness; "" runs in both.
	Harness string `json:"harness,omitempty"`
	Lookup  *struct {
		Tool  string `json:"tool"`
		Query string `json:"query"`
	} `json:"lookup,omitempty"`
	Symbol *bool `json:"symbol,omitempty"`
}

type coachSequence struct {
	Name     string        `json:"name"`
	Harness  string        `json:"harness,omitempty"`
	Turns    [][]coachCall `json:"turns"`
	Expect   [][3]any      `json:"expect"`
	Followed [][3]any      `json:"followed"`
}

// coachPromptSequence is a session of user prompts (a bare {"prompt": true}) and
// turns (a list of calls); Expect and Followed index [step, call, value].
type coachPromptSequence struct {
	Name     string            `json:"name"`
	Steps    []json.RawMessage `json:"steps"`
	Expect   [][3]any          `json:"expect"`
	Followed [][3]any          `json:"followed"`
}

type coachFile struct {
	Classify  []coachCall           `json:"classify"`
	Sequences []coachSequence       `json:"sequences"`
	Prompts   []coachPromptSequence `json:"prompts"`
}

type coachLine struct {
	TS         string  `json:"ts"`
	Harness    string  `json:"harness"`
	Session    string  `json:"session"`
	Repo       string  `json:"repo"`
	Turn       *int    `json:"turn"`
	Tool       string  `json:"tool"`
	Query      string  `json:"query"`
	OK         bool    `json:"ok"`
	Empty      bool    `json:"empty"`
	Bytes      int     `json:"bytes"`
	DurationMS *int    `json:"duration_ms"`
	Reminder   *string `json:"reminder"`
	Followed   *bool   `json:"followed"`
}

type coachRun struct {
	Handlers []string       `json:"handlers"`
	Lines    []coachLine    `json:"lines"`
	Added    map[string]any `json:"added"`
	LogFile  bool           `json:"logFile"`
	// Logged lists the calls ("turn.call") that wrote a log line, in order.
	Logged []string `json:"logged"`
}

func loadCoachCases(t *testing.T) coachFile {
	t.Helper()
	var file coachFile
	if err := json.Unmarshal(mustRead(t, filepath.Join("..", "hooks", "claude", "coach_cases.json")), &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Classify) == 0 || len(file.Sequences) == 0 {
		t.Fatal("the shared coach case file is empty")
	}
	return file
}

// runCoach plays one session of turns through a fresh extension and returns the
// log lines, the coach text appended to each call's result ("t.j") and which
// handlers were registered.
func runCoach(t *testing.T, turns [][]coachCall, env ...string) coachRun {
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
	for _, name := range []string{"orch", "fd", "rg"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(dir, "state")
	script := `
import ext from ` + string(mustJSON(t, ext)) + `;
import { readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
const handlers = {};
ext({ on: (name, fn) => { handlers[name] = fn; } });
const added = {};
const logged = [];
const count = () => {
  const file = join(process.env.ORCH_TOOL_GUARD_STATE_DIR, "lookups.jsonl");
  return existsSync(file) ? readFileSync(file, "utf8").split("\n").filter(Boolean).length : 0;
};
const turns = JSON.parse(process.argv[1]);
const ctx = { sessionManager: { getSessionId: () => "pi-session" } };
for (let t = 0; t < turns.length; t += 1) {
  if (turns[t].length === 1 && turns[t][0].prompt) {
    const res = handlers.before_agent_start ? await handlers.before_agent_start({ type: "before_agent_start", messageType: "user", prompt: "go", systemPrompt: "", systemPromptOptions: {} }, ctx) : undefined;
    if (res && res.message) added[t + ".0"] = res.message.content;
    continue;
  }
  if (handlers.turn_start) await handlers.turn_start({ type: "turn_start", turnIndex: t, timestamp: 0 }, ctx);
  for (let j = 0; j < turns[t].length; j += 1) {
    const c = turns[t][j];
    const toolName = c.bash !== undefined ? "bash" : c.read ? "read" : "";
    if (!toolName || !handlers.tool_result) continue;
    const input = c.bash !== undefined ? { command: c.bash } : c.read;
    const id = "call-" + t + "-" + j;
    const text = c.text ?? "ok";
    const before = count();
    const res = await handlers.tool_result({ type: "tool_result", toolCallId: id, toolName, input, content: [{ type: "text", text }], isError: !!c.failed }, ctx);
    if (handlers.tool_execution_end) await handlers.tool_execution_end({ type: "tool_execution_end", toolCallId: id, toolName, result: {}, isError: !!c.failed, durationMs: 7 }, ctx);
    if (count() > before) logged.push(t + "." + j);
    const notes = res ? res.content.slice(1).map((p) => p.text).filter((x) => x.startsWith("orch lookup coach: ")) : [];
    if (notes.length) added[t + "." + j] = notes.join("|");
  }
}
const file = join(process.env.ORCH_TOOL_GUARD_STATE_DIR, "lookups.jsonl");
const lines = existsSync(file) ? readFileSync(file, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l)) : [];
console.log(JSON.stringify({ handlers: Object.keys(handlers).sort(), lines, added, logged, logFile: existsSync(file) }));
`
	cmd := exec.Command(node, "--input-type=module", "-e", script, string(mustJSON(t, turns)))
	cmd.Env = agentEnv(nil, append([]string{"PATH=" + bin, "ORCH_CTX_ONLY_READY=", "ORCH_TOOL_GUARD_STATE_DIR=" + state, "ORCH_GUARD_BIN=" + guardBin(t), "ORCH_LOOKUP_COACH=", "ORCH_LOOKUP_LOG=", "ORCH_TOOL_GUARD="}, env...))
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got coachRun
	if err := json.Unmarshal(raw[strings.Index(string(raw), "{"):], &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return got
}

func coachBash(command string) coachCall { return coachCall{Bash: &command} }

func TestCtxReminderCoachClassifiesEveryLookup(t *testing.T) {
	for _, c := range loadCoachCases(t).Classify {
		if c.Harness == "claude" || strings.HasPrefix(c.Name, "symbol ") {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			got := runCoach(t, [][]coachCall{{c}})
			if c.Lookup == nil {
				if len(got.Lines) != 0 || len(got.Added) != 0 {
					t.Fatalf("not a lookup, but logged %+v", got.Lines)
				}
				return
			}
			if len(got.Lines) != 1 {
				t.Fatalf("want one log line, got %+v", got.Lines)
			}
			if got.Lines[0].Tool != c.Lookup.Tool || got.Lines[0].Query != c.Lookup.Query {
				t.Errorf("logged %q %q, want %q %q", got.Lines[0].Tool, got.Lines[0].Query, c.Lookup.Tool, c.Lookup.Query)
			}
		})
	}
}

func TestCtxReminderCoachSymbolRule(t *testing.T) {
	prime := coachBash(`jbcontext search "x"`)
	for _, c := range loadCoachCases(t).Classify {
		if !strings.HasPrefix(c.Name, "symbol ") {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			got := runCoach(t, [][]coachCall{{prime}, {c}})
			if len(got.Lines) != 2 {
				t.Fatalf("want two log lines, got %+v", got.Lines)
			}
			fired := got.Lines[1].Reminder != nil && *got.Lines[1].Reminder == "symbol-via-lsp"
			if fired != *c.Symbol {
				t.Errorf("symbol-via-lsp fired=%v, want %v", fired, *c.Symbol)
			}
		})
	}
}

func TestCtxReminderCoachSharedSequences(t *testing.T) {
	for _, seq := range loadCoachCases(t).Sequences {
		if seq.Harness == "claude" {
			continue
		}
		t.Run(seq.Name, func(t *testing.T) {
			got := runCoach(t, seq.Turns)
			if len(got.Logged) != len(got.Lines) {
				t.Fatalf("%d logged calls, %d log lines", len(got.Logged), len(got.Lines))
			}
			var fired, followed [][3]any
			for i, line := range got.Lines {
				key := got.Logged[i]
				ts, js, _ := strings.Cut(key, ".")
				pt, _ := strconv.Atoi(ts)
				pj, _ := strconv.Atoi(js)
				p := struct{ t, j int }{pt, pj}
				if line.Turn == nil || *line.Turn != p.t {
					t.Errorf("line %d has turn %v, want %d", i, line.Turn, p.t)
				}
				note, noted := got.Added[key]
				if line.Reminder != nil {
					fired = append(fired, [3]any{float64(p.t), float64(p.j), *line.Reminder})
					if want := coachPrefix + coachTexts[*line.Reminder]; note != want {
						t.Errorf("%s appended %q, want %q", key, note, want)
					}
				} else if noted {
					t.Errorf("%s got a note without a logged reminder: %v", key, note)
				}
				if line.Followed != nil {
					followed = append(followed, [3]any{float64(p.t), float64(p.j), *line.Followed})
				}
			}
			if !sameTriples(fired, seq.Expect) {
				t.Errorf("reminders %v, want %v", fired, seq.Expect)
			}
			if !sameTriples(followed, seq.Followed) {
				t.Errorf("followed %v, want %v", followed, seq.Followed)
			}
		})
	}
}

func sameTriples(a, b [][3]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCtxReminderCoachPromptSequences(t *testing.T) {
	for _, seq := range loadCoachCases(t).Prompts {
		t.Run(seq.Name, func(t *testing.T) {
			var turns [][]coachCall
			for _, raw := range seq.Steps {
				var calls []coachCall
				if err := json.Unmarshal(raw, &calls); err != nil {
					var one coachCall
					if err := json.Unmarshal(raw, &one); err != nil || !one.Prompt {
						t.Fatalf("bad step %s", raw)
					}
					calls = []coachCall{one}
				}
				turns = append(turns, calls)
			}
			got := runCoach(t, turns)
			if len(got.Logged) != len(got.Lines) {
				t.Fatalf("%d logged calls, %d log lines", len(got.Logged), len(got.Lines))
			}
			// Reminders in step order: a prompt's note, then its calls' log lines.
			var fired, followed [][3]any
			next := 0
			for step, calls := range turns {
				if calls[0].Prompt {
					if note, ok := got.Added[strconv.Itoa(step)+".0"]; ok {
						if want := coachPrefix + coachTexts["lookup-ladder"]; note != want {
							t.Errorf("prompt %d added %q, want %q", step, note, want)
						}
						fired = append(fired, [3]any{float64(step), float64(0), "lookup-ladder"})
					}
					continue
				}
				for j := range calls {
					if next >= len(got.Lines) || got.Logged[next] != strconv.Itoa(step)+"."+strconv.Itoa(j) {
						t.Fatalf("call %d.%d was not logged in order: %v", step, j, got.Logged)
					}
					line := got.Lines[next]
					next++
					if line.Reminder != nil {
						fired = append(fired, [3]any{float64(step), float64(j), *line.Reminder})
					}
					if line.Followed != nil {
						followed = append(followed, [3]any{float64(step), float64(j), *line.Followed})
					}
				}
			}
			if !sameTriples(fired, seq.Expect) {
				t.Errorf("reminders %v, want %v", fired, seq.Expect)
			}
			if !sameTriples(followed, seq.Followed) {
				t.Errorf("followed %v, want %v", followed, seq.Followed)
			}
		})
	}
}

func TestCtxReminderCoachLadderSwitches(t *testing.T) {
	turns := [][]coachCall{{{Prompt: true}}}
	if got := runCoach(t, turns); got.Added["0.0"] == "" {
		t.Errorf("the first prompt got no ladder: %+v", got)
	}
	for _, env := range []string{"ORCH_LOOKUP_COACH=off", "ORCH_TOOL_GUARD=off"} {
		if got := runCoach(t, turns, env); len(got.Added) != 0 || got.LogFile {
			t.Errorf("%s: %+v", env, got)
		}
	}
}

func TestCtxReminderCoachLogRecord(t *testing.T) {
	read := coachCall{Read: map[string]any{"path": "a.go", "offset": 5, "limit": 10}, Text: "héllo"}
	got := runCoach(t, [][]coachCall{{read}})
	if len(got.Lines) != 1 {
		t.Fatalf("lines: %+v", got.Lines)
	}
	l := got.Lines[0]
	if l.Harness != "pi" || l.Session != "pi-session" || l.Tool != "read" || l.Query != "a.go:5-10" {
		t.Errorf("record: %+v", l)
	}
	if l.Bytes != 6 || l.DurationMS == nil || *l.DurationMS != 7 || !l.OK || l.Empty || l.Turn == nil || *l.Turn != 0 {
		t.Errorf("record: %+v", l)
	}
	if !strings.HasSuffix(l.TS, "Z") || l.Repo == "" || l.Reminder != nil || l.Followed != nil {
		t.Errorf("record: %+v", l)
	}
}

func TestCtxReminderCoachOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		call      coachCall
		ok, empty bool
	}{
		{"rg exit 1 without output is an empty result", coachCall{Bash: ptr("rg zzz src"), Failed: true, Text: "\n\nCommand exited with code 1"}, true, true},
		{"rg exit 2 is an error", coachCall{Bash: ptr("rg ( src"), Failed: true, Text: "regex parse error\n\nCommand exited with code 2"}, false, false},
		{"rg exit 1 with stderr is an error", coachCall{Bash: ptr("rg x src"), Failed: true, Text: "rg: oops\n\nCommand exited with code 1"}, false, false},
		{"jbcontext no result is empty", coachCall{Bash: ptr(`jbcontext search "zzqq"`), Text: "Searched revision: 4cd05cebea6c\n\nNo relevant results found. Try rephrasing your query.\n"}, true, true},
		{"jbcontext hits are not empty", coachCall{Bash: ptr(`jbcontext search "x"`), Text: "1. File: a.go"}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runCoach(t, [][]coachCall{{c.call}})
			if len(got.Lines) != 1 || got.Lines[0].OK != c.ok || got.Lines[0].Empty != c.empty {
				t.Errorf("got %+v, want ok=%v empty=%v", got.Lines, c.ok, c.empty)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestCtxReminderCoachSwitches(t *testing.T) {
	turns := [][]coachCall{{coachBash("rg -n gateway src")}}
	t.Run("coach off logs without a reminder", func(t *testing.T) {
		got := runCoach(t, turns, "ORCH_LOOKUP_COACH=off")
		if len(got.Added) != 0 || len(got.Lines) != 1 || got.Lines[0].Reminder != nil {
			t.Errorf("%+v", got)
		}
	})
	t.Run("log off reminds without a file", func(t *testing.T) {
		got := runCoach(t, turns, "ORCH_LOOKUP_LOG=off")
		if len(got.Added) != 1 || got.LogFile {
			t.Errorf("%+v", got)
		}
	})
	t.Run("guard off disables both", func(t *testing.T) {
		got := runCoach(t, turns, "ORCH_TOOL_GUARD=off")
		if len(got.Added) != 0 || got.LogFile {
			t.Errorf("%+v", got)
		}
	})
	t.Run("silent in the ctx-only gate shell", func(t *testing.T) {
		got := runCoach(t, turns, "ORCH_CTX_ONLY_READY=1")
		if len(got.Handlers) != 0 || got.LogFile {
			t.Errorf("%+v", got)
		}
	})
	t.Run("both off registers only the guard hooks", func(t *testing.T) {
		got := runCoach(t, turns, "ORCH_LOOKUP_COACH=off", "ORCH_LOOKUP_LOG=off")
		if strings.Join(got.Handlers, ",") != "tool_call,tool_result" {
			t.Errorf("handlers: %v", got.Handlers)
		}
	})
}
