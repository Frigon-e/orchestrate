package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// The decision tables are hooks/claude/coach_cases.json, shared with the pi
// extension; these tests assert the Claude side of them, as coach_test.py did.

type coachFile struct {
	Classify []struct {
		Name    string
		Harness string
		Lookup  *struct{ Tool, Query string }
		Symbol  bool
	}
	Sequences []struct {
		Name     string
		Turns    [][]spec
		Expect   [][]any
		Followed [][]any
	}
	Prompts []struct {
		Name     string
		Steps    []json.RawMessage
		Expect   [][]any
		Followed [][]any
	}
	ClassifyRaw []spec `json:"-"`
}

func loadCoach(t testing.TB) coachFile {
	var f coachFile
	readJSON(t, "coach_cases.json", &f)
	var raw struct{ Classify []spec }
	readJSON(t, "coach_cases.json", &raw)
	f.ClassifyRaw = raw.Classify
	return f
}

const (
	testPrefix  = "orch lookup coach: "
	testLadder2 = `Before searching: location unknown -> jbcontext search "<one behavior>"; known symbol -> ` +
		"orch lsp def|refs|callers|impls <Name>; exact text -> rg. " +
		"Send independent lookups as parallel calls in one message. " +
		"Read known files or ranges with Read, not cat or sed -n."
)

func isClaudeCase(c spec) bool { return c["harness"] != "pi" }

func TestEveryClassificationCase(t *testing.T) {
	cases := loadCoach(t)
	for i, c := range cases.ClassifyRaw {
		name := c["name"].(string)
		if !isClaudeCase(c) || strings.HasPrefix(name, "symbol ") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			box := newSandbox(t)
			box.writeTranscript([][]spec{{c}})
			if code, _ := box.run(inProc{}, c, "tu_0_0", runOpts{}); code != 0 {
				t.Fatalf("exit %d", code)
			}
			rows := box.log()
			want := cases.Classify[i].Lookup
			if want == nil {
				if len(rows) != 0 {
					t.Fatalf("logged %v, want no lookup", rows)
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("logged %d rows, want 1", len(rows))
			}
			if rows[0]["tool"] != want.Tool || rows[0]["query"] != want.Query {
				t.Fatalf("logged %v %q, want %v %q", rows[0]["tool"], rows[0]["query"], want.Tool, want.Query)
			}
		})
	}
}

func TestSymbolRuleCases(t *testing.T) {
	cases := loadCoach(t)
	for i, c := range cases.ClassifyRaw {
		name := c["name"].(string)
		if !strings.HasPrefix(name, "symbol ") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			box := newSandbox(t)
			prime := spec{"bash": `jbcontext search "x"`}
			box.writeTranscript([][]spec{{prime}, {c}})
			box.run(inProc{}, prime, "tu_0_0", runOpts{})
			box.run(inProc{}, c, "tu_1_0", runOpts{})
			var want any
			if cases.Classify[i].Symbol {
				want = "symbol-via-lsp"
			}
			if got := box.log()[1]["reminder"]; got != want {
				t.Fatalf("reminder %v, want %v", got, want)
			}
		})
	}
}

// triples is [][]any as the case file writes it and as a run reports it.
func triples(rows [][]any) string {
	if rows == nil {
		rows = [][]any{}
	}
	out, _ := json.Marshal(rows)
	return string(out)
}

func TestEveryReminderSequence(t *testing.T) {
	for _, seq := range loadCoach(t).Sequences {
		t.Run(seq.Name, func(t *testing.T) {
			box := newSandbox(t)
			box.writeTranscript(seq.Turns)
			fired, followed := [][]any{}, [][]any{}
			for ti, calls := range seq.Turns {
				for ci, s := range calls {
					before := len(box.log())
					code, stdout := box.run(inProc{}, s, "tu_"+itoa(ti)+"_"+itoa(ci), runOpts{})
					if code != 0 {
						t.Fatalf("exit %d", code)
					}
					rows := box.log()[before:]
					if len(rows) == 0 {
						if stdout != "" {
							t.Fatalf("no row but stdout %q", stdout)
						}
						continue
					}
					row := rows[0]
					if row["turn"] != "msg_"+itoa(ti) {
						t.Fatalf("turn %v, want msg_%d", row["turn"], ti)
					}
					if row["reminder"] != nil {
						fired = append(fired, []any{ti, ci, row["reminder"]})
						var out struct {
							Out struct{ HookEventName, AdditionalContext string } `json:"hookSpecificOutput"`
						}
						if err := json.Unmarshal([]byte(stdout), &out); err != nil {
							t.Fatalf("stdout is not hook JSON: %q", stdout)
						}
						if out.Out.HookEventName != "PostToolUse" || !strings.HasPrefix(out.Out.AdditionalContext, testPrefix) {
							t.Fatalf("bad reminder output %q", stdout)
						}
					} else if stdout != "" {
						t.Fatalf("no reminder but stdout %q", stdout)
					}
					if row["followed"] != nil {
						followed = append(followed, []any{ti, ci, row["followed"]})
					}
				}
			}
			if got, want := triples(fired), triples(seq.Expect); got != want {
				t.Errorf("reminders fired %s, want %s", got, want)
			}
			if got, want := triples(followed), triples(seq.Followed); got != want {
				t.Errorf("followed %s, want %s", got, want)
			}
		})
	}
}

func TestEveryPromptSequence(t *testing.T) {
	for _, seq := range loadCoach(t).Prompts {
		t.Run(seq.Name, func(t *testing.T) {
			box := newSandbox(t)
			var turns [][]spec
			for _, raw := range seq.Steps {
				var calls []spec
				if json.Unmarshal(raw, &calls) == nil {
					turns = append(turns, calls)
				}
			}
			box.writeTranscript(turns)
			fired, followed := [][]any{}, [][]any{}
			turn := 0
			for i, raw := range seq.Steps {
				var calls []spec
				if json.Unmarshal(raw, &calls) != nil {
					code, stdout := box.prompt(inProc{}, nil)
					if code != 0 {
						t.Fatalf("exit %d", code)
					}
					if stdout != "" {
						var out struct {
							Out struct{ HookEventName, AdditionalContext string } `json:"hookSpecificOutput"`
						}
						if err := json.Unmarshal([]byte(stdout), &out); err != nil {
							t.Fatalf("stdout is not hook JSON: %q", stdout)
						}
						if out.Out.HookEventName != "UserPromptSubmit" || out.Out.AdditionalContext != testPrefix+testLadder2 {
							t.Fatalf("bad ladder output %q", stdout)
						}
						fired = append(fired, []any{i, 0, "lookup-ladder"})
					}
					continue
				}
				for ci, s := range calls {
					before := len(box.log())
					box.run(inProc{}, s, "tu_"+itoa(turn)+"_"+itoa(ci), runOpts{})
					row := box.log()[before]
					if row["reminder"] != nil {
						fired = append(fired, []any{i, ci, row["reminder"]})
					}
					if row["followed"] != nil {
						followed = append(followed, []any{i, ci, row["followed"]})
					}
				}
				turn++
			}
			if got, want := triples(fired), triples(seq.Expect); got != want {
				t.Errorf("reminders fired %s, want %s", got, want)
			}
			if got, want := triples(followed), triples(seq.Followed); got != want {
				t.Errorf("followed %s, want %s", got, want)
			}
		})
	}
}

func TestPromptWritesOnlyTheLadderRow(t *testing.T) {
	box := newSandbox(t)
	box.prompt(inProc{}, nil)
	rows := box.log()
	if len(rows) != 1 || rows[0]["tool"] != "prompt" || rows[0]["reminder"] != "lookup-ladder" {
		t.Fatalf("a prompt logged %v, want one prompt row for lookup-ladder", rows)
	}
	box.prompt(inProc{}, nil)
	if rows := box.log(); len(rows) != 1 {
		t.Fatalf("a prompt without the ladder logged %v", rows)
	}
}

func TestCoachOffAndGuardOffSilenceTheLadder(t *testing.T) {
	for _, env := range []map[string]string{{"ORCH_LOOKUP_COACH": "off"}, {"ORCH_TOOL_GUARD": "off"}} {
		box := newSandbox(t)
		if code, stdout := box.prompt(inProc{}, env); code != 0 || stdout != "" {
			t.Errorf("%v: exit %d, stdout %q", env, code, stdout)
		}
	}
}

// brokenState is a state dir that cannot be made: a path below a regular file.
func brokenState(t *testing.T, box *sandbox) map[string]string {
	blocker := filepath.Join(box.dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"ORCH_TOOL_GUARD_STATE_DIR": filepath.Join(blocker, "x")}
}

func TestBrokenStateDirFailsOpen(t *testing.T) {
	box := newSandbox(t)
	env := brokenState(t, box)
	if code, stdout := box.prompt(inProc{}, env); code != 0 || stdout != "" {
		t.Errorf("prompt: exit %d, stdout %q", code, stdout)
	}
	rg := spec{"bash": "rg -n gateway src"}
	box.writeTranscript([][]spec{{rg}})
	if code, stdout := box.run(inProc{}, rg, "tu_0_0", runOpts{env: env}); code != 0 || stdout != "" {
		t.Errorf("lookup: exit %d, stdout %q", code, stdout)
	}
}

var rg = spec{"bash": "rg -n gateway src"}

func rgBox(t *testing.T) *sandbox {
	box := newSandbox(t)
	box.writeTranscript([][]spec{{rg}})
	return box
}

func TestLogLineFields(t *testing.T) {
	box := newSandbox(t)
	os.MkdirAll(filepath.Join(box.dir, ".git"), 0o755)
	box.writeTranscript([][]spec{{rg}})
	box.run(inProc{}, rg, "tu_0_0", runOpts{
		response: map[string]any{"stdout": "ab", "stderr": "é"}, extra: map[string]any{"duration_ms": 12}})
	row := box.log()[0]
	if row["harness"] != "claude" || row["session"] != "s1" {
		t.Errorf("harness/session: %v", row)
	}
	want, _ := filepath.EvalSymlinks(box.dir)
	if got, _ := filepath.EvalSymlinks(row["repo"].(string)); got != want {
		t.Errorf("repo %v, want %v", got, want)
	}
	// stdout and stderr are joined, and the byte count is UTF-8: "ab" + "é" is 4.
	if row["bytes"] != 4.0 || row["duration_ms"] != 12.0 || row["ok"] != true || row["empty"] != false {
		t.Errorf("bytes/duration/ok/empty: %v", row)
	}
	if ts, _ := row["ts"].(string); !strings.HasSuffix(ts, "Z") {
		t.Errorf("ts %v does not end in Z", row["ts"])
	}
}

func TestCleanExit1IsAnEmptyResultNotAnError(t *testing.T) {
	box := rgBox(t)
	failure := "Exit code 1"
	box.run(inProc{}, rg, "tu_0_0", runOpts{failure: &failure})
	if row := box.log()[0]; row["ok"] != true || row["empty"] != true {
		t.Errorf("%v", row)
	}
}

func TestExit1WithStderrAndExit2AreErrors(t *testing.T) {
	for _, failure := range []string{"Exit code 1\nrg: bad regex", "Exit code 2\n", "boom"} {
		box := rgBox(t)
		code, _ := box.run(inProc{}, rg, "tu_0_0", runOpts{failure: &failure})
		if row := box.log()[0]; row["ok"] != false || row["empty"] != false || code != 0 {
			t.Errorf("%q: %v exit %d", failure, row, code)
		}
	}
}

func TestJbcontextNoResultTextIsEmpty(t *testing.T) {
	s := spec{"bash": `jbcontext search "zzqq"`}
	box := newSandbox(t)
	box.writeTranscript([][]spec{{s}})
	out := "Searched revision: 4cd05cebea6c\n\nNo relevant results found. Try rephrasing your query.\n"
	box.run(inProc{}, s, "tu_0_0", runOpts{response: map[string]any{"stdout": out, "stderr": ""}})
	if row := box.log()[0]; row["ok"] != true || row["empty"] != true {
		t.Errorf("%v", row)
	}
}

func TestFailureEventNamesItselfInTheReminder(t *testing.T) {
	box := rgBox(t)
	failure := "Exit code 1"
	_, stdout := box.run(inProc{}, rg, "tu_0_0", runOpts{failure: &failure})
	if !strings.Contains(stdout, `"hookEventName": "PostToolUseFailure"`) {
		t.Errorf("stdout %q", stdout)
	}
}

func TestUnknownToolUseIDLogsANullTurn(t *testing.T) {
	box := rgBox(t)
	box.run(inProc{}, rg, "tu_missing", runOpts{})
	if row := box.log()[0]; row["turn"] != nil {
		t.Errorf("turn %v, want null", row["turn"])
	}
}

func TestCoachOffLogsWithoutReminders(t *testing.T) {
	box := rgBox(t)
	_, stdout := box.run(inProc{}, rg, "tu_0_0", runOpts{env: map[string]string{"ORCH_LOOKUP_COACH": "off"}})
	if stdout != "" || box.log()[0]["reminder"] != nil {
		t.Errorf("stdout %q, row %v", stdout, box.log()[0])
	}
}

func TestLogOffStillReminds(t *testing.T) {
	box := rgBox(t)
	_, stdout := box.run(inProc{}, rg, "tu_0_0", runOpts{env: map[string]string{"ORCH_LOOKUP_LOG": "off"}})
	if !strings.Contains(stdout, `"additionalContext": "`+testPrefix) || len(box.log()) != 0 {
		t.Errorf("stdout %q, log %v", stdout, box.log())
	}
}

func TestGuardOffDisablesBoth(t *testing.T) {
	box := rgBox(t)
	_, stdout := box.run(inProc{}, rg, "tu_0_0", runOpts{env: map[string]string{"ORCH_TOOL_GUARD": "off"}})
	if stdout != "" || len(box.log()) != 0 {
		t.Errorf("stdout %q, log %v", stdout, box.log())
	}
}

func TestCoachBrokenInputExits0(t *testing.T) {
	if code, stdout := (inProc{}).coach(t, []byte("not json"), t.TempDir(), nil); code != 0 || stdout != "" {
		t.Errorf("exit %d, stdout %q", code, stdout)
	}
}

func TestSessionsDoNotShareReminders(t *testing.T) {
	box := rgBox(t)
	box.run(inProc{}, rg, "tu_0_0", runOpts{session: "a"})
	if _, stdout := box.run(inProc{}, rg, "tu_0_0", runOpts{session: "b"}); stdout == "" {
		t.Error("session b got no reminder after session a's")
	}
}

// Hooks of one session run in parallel: every one logs a row, and exactly one
// reminds. In-process goroutines open their own file descriptions, so the flock
// serialises them as it does separate processes.
func TestParallelHooksKeepOneLineEachAndOneReminder(t *testing.T) {
	var calls []spec
	for i := 0; i < 8; i++ {
		calls = append(calls, spec{"bash": "rg -n gateway" + itoa(i) + " src"})
	}
	box := newSandbox(t)
	box.writeTranscript([][]spec{calls})
	setEnv(t, map[string]string{"ORCH_TOOL_GUARD_STATE_DIR": box.state, "ORCH_TOOL_GUARD_SKIP": "search-first"})
	var wg sync.WaitGroup
	outs := make([]string, len(calls))
	for j := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev, _ := ParseEvent(jsonBytes(t, box.postEvent(calls[j], "tu_0_"+itoa(j), runOpts{})))
			var out strings.Builder
			Coach(ev, &out)
			outs[j] = out.String()
		}()
	}
	wg.Wait()
	reminded, rowsWithReminder := 0, 0
	for _, o := range outs {
		if o != "" {
			reminded++
		}
	}
	for _, row := range box.log() {
		if row["reminder"] != nil {
			rowsWithReminder++
		}
	}
	if len(box.log()) != 8 || reminded != 1 || rowsWithReminder != 1 {
		t.Errorf("rows %d, reminders printed %d, reminders logged %d; want 8, 1, 1", len(box.log()), reminded, rowsWithReminder)
	}
}

// The state file is the interface to a session that switches between the Python
// hook and this one: it holds the same keys and the same values.
func TestStateFileKeepsThePythonShape(t *testing.T) {
	box := rgBox(t)
	box.prompt(inProc{}, nil)
	box.run(inProc{}, rg, "tu_0_0", runOpts{})
	data, err := os.ReadFile(filepath.Join(box.state, "coach", "s1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(data, &got)
	want := map[string]any{
		"pending": "concept-first", "window_first": "bad", "first_search_done": true, "concept_seen": false, "prompt": 1.0,
		"fired": []any{"concept-first@1"}, "turns": []any{map[string]any{"id": "msg_0", "nr": 1.0, "rd": 0.0}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("state %v, want %v", got, want)
	}
	// A state written by the Python hook (a session mid-way) is read and continued.
	py := `{"prompt": 1, "window_first": "bad", "first_search_done": true, "concept_seen": false, "pending": null, "fired": ["concept-first@1"], "turns": []}`
	os.WriteFile(filepath.Join(box.state, "coach", "s2.json"), []byte(py), 0o644)
	if _, stdout := box.run(inProc{}, rg, "tu_0_0", runOpts{session: "s2"}); stdout != "" {
		t.Errorf("a session that already heard concept-first heard it again: %q", stdout)
	}
}

// With deny-search-first on, the first rg of a window gets no concept-first
// reminder (the block says it), while symbol-via-lsp still fires.
func TestSearchFirstReplacesConceptFirstReminder(t *testing.T) {
	box := newSandbox(t)
	box.writeTranscript([][]spec{{{"bash": "rg -n gateway src"}}, {{"bash": "rg -n FooBar src"}}})
	_, out := box.run(inProc{}, spec{"bash": "rg -n gateway src"}, "tu_0_0", runOpts{env: map[string]string{"ORCH_TOOL_GUARD_SKIP": ""}})
	if strings.Contains(out, "jbcontext") {
		t.Errorf("concept-first fired: %s", out)
	}
	_, out = box.run(inProc{}, spec{"bash": "rg -n FooBar src"}, "tu_1_0", runOpts{env: map[string]string{"ORCH_TOOL_GUARD_SKIP": ""}})
	if !strings.Contains(out, "orch lsp") {
		t.Errorf("symbol-via-lsp did not fire: %s", out)
	}
}
