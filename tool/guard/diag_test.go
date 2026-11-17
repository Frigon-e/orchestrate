package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake `orch` that answers `lsp diag` with $FAKE_DIR/diag.json (exit $FAKE_EXIT)
// and records `lsp warm`.
const fakeDiagOrch = `#!/bin/sh
[ "$1" = lsp ] || exit 2
if [ "$2" = warm ]; then echo "$3" > "$FAKE_DIR/warm.marker"; exit 0; fi
[ "$2" = diag ] || exit 2
echo "$@" > "$FAKE_DIR/diag.args"
if [ -n "$FAKE_SLOW" ]; then exec sleep 5; fi
cat "$FAKE_DIR/diag.json"
exit ${FAKE_EXIT:-0}
`

type diagBox struct {
	t    *testing.T
	repo string
	fake string
	seq  int
}

func newDiagBox(t *testing.T) *diagBox {
	t.Helper()
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := t.TempDir()
	bin := filepath.Join(fake, "orch")
	if err := os.WriteFile(bin, []byte(fakeDiagOrch), 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"ORCH_BIN": bin, "FAKE_DIR": fake, "FAKE_SLOW": "", "FAKE_EXIT": "", "ORCH_TOOL_GUARD_STATE_DIR": t.TempDir(),
		"ORCH_CACHE_DIR": t.TempDir(), "ORCH_LSP_DIAG": "", "ORCH_LSP_DIAG_BUDGET_MS": "5000", "ORCH_TOOL_GUARD": "",
		"ORCH_LOOKUP_LOG": "off",
	} {
		t.Setenv(k, v)
	}
	return &diagBox{t: t, repo: repo, fake: fake}
}

func (b *diagBox) file(name string) string { return filepath.Join(b.repo, name) }

type diagErr struct {
	file string
	line int
	text string
}

// answer sets what the fake language server returns: errors per file (a file
// with none gets an empty ok result), plus extra raw results.
func (b *diagBox) answer(status string, files []string, errs ...diagErr) {
	b.t.Helper()
	var results []map[string]any
	for _, f := range files {
		items := []map[string]any{}
		for _, e := range errs {
			if e.file == f {
				items = append(items, map[string]any{"path": b.file(f), "line": e.line, "kind": "error", "text": e.text})
			}
		}
		results = append(results, map[string]any{"name": b.file(f), "status": status, "total": len(items), "items": items})
	}
	data, _ := json.Marshal(map[string]any{"verb": "diag", "results": results})
	if err := os.WriteFile(filepath.Join(b.fake, "diag.json"), data, 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *diagBox) event(session, tool, id string, input map[string]any) *Event {
	b.t.Helper()
	ev, ok := ParseEvent(jsonBytes(b.t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": session, "cwd": b.repo, "tool_name": tool,
		"tool_use_id": id, "tool_input": input, "tool_response": map[string]any{},
	}))
	if !ok {
		b.t.Fatal("bad event")
	}
	return ev
}

// context is the additionalContext Coach wrote for the event, "" for no output.
func (b *diagBox) context(ev *Event) string {
	b.t.Helper()
	var buf bytes.Buffer
	if err := Coach(ev, &buf); err != nil {
		b.t.Fatal(err)
	}
	if buf.Len() == 0 {
		return ""
	}
	var out struct {
		H struct {
			Ctx string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		b.t.Fatalf("bad output %q: %v", buf.String(), err)
	}
	return out.H.Ctx
}

func (b *diagBox) edit(session, file string) string {
	b.seq++
	return b.context(b.event(session, "Edit", fmt.Sprintf("toolu_%d", b.seq), map[string]any{"file_path": b.file(file)}))
}

func (b *diagBox) scriptEdit(session, file string) string {
	b.seq++
	return b.context(b.event(session, "Edit", fmt.Sprintf("repl_%d", b.seq), map[string]any{"file_path": b.file(file)}))
}

func (b *diagBox) repl(session string) string {
	b.seq++
	return b.context(b.event(session, "REPL", fmt.Sprintf("toolu_%d", b.seq), map[string]any{"code": "x"}))
}

func (b *diagBox) pending(session string) []string {
	b.t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("ORCH_TOOL_GUARD_STATE_DIR"), "coach", session+".json"))
	if err != nil {
		return nil
	}
	var st State
	_ = json.Unmarshal(data, &st)
	return st.DiagPending
}

func (b *diagBox) rows() []map[string]any {
	b.t.Helper()
	data, _ := os.ReadFile(filepath.Join(os.Getenv("ORCH_CACHE_DIR"), "lsp-diag.jsonl"))
	var rows []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			b.t.Fatalf("bad row %q", l)
		}
		rows = append(rows, m)
	}
	return rows
}

func TestDiagDirectEditReportsNewErrorsOnly(t *testing.T) {
	b := newDiagBox(t)
	e1 := diagErr{"a.go", 10, "undefined: foo"}
	b.answer("ok", []string{"a.go"}, e1)
	want := "orch diagnostics:\na.go:10: undefined: foo"
	eq(t, b.edit("s1", "a.go"), want)
	if got := b.edit("s1", "a.go"); got != "" {
		t.Errorf("the same error was reported twice: %q", got)
	}
	// The same message on a shifted line is the same error.
	b.answer("ok", []string{"a.go"}, diagErr{"a.go", 14, "undefined: foo"})
	if got := b.edit("s1", "a.go"); got != "" {
		t.Errorf("a shifted error was reported again: %q", got)
	}
	// Fixed, then reintroduced: reported again.
	b.answer("ok", []string{"a.go"})
	if got := b.edit("s1", "a.go"); got != "" {
		t.Errorf("a clean file produced output: %q", got)
	}
	b.answer("ok", []string{"a.go"}, e1)
	eq(t, b.edit("s1", "a.go"), want)
	// Another session has its own record.
	eq(t, b.edit("s2", "a.go"), want)
}

func TestDiagCapsAtEightLines(t *testing.T) {
	b := newDiagBox(t)
	var errs []diagErr
	for i := 1; i <= 11; i++ {
		errs = append(errs, diagErr{"a.go", i, fmt.Sprintf("bad %d", i)})
	}
	b.answer("ok", []string{"a.go"}, errs...)
	got := b.edit("s1", "a.go")
	lines := strings.Split(got, "\n")
	if len(lines) != 10 || lines[0] != "orch diagnostics:" || lines[8] != "a.go:8: bad 8" || lines[9] != "... 3 more: orch lsp diag a.go" {
		t.Errorf("got:\n%s", got)
	}
	// The three past the cap were not reported, so the next edit shows them.
	eq(t, b.edit("s1", "a.go"), "orch diagnostics:\na.go:9: bad 9\na.go:10: bad 10\na.go:11: bad 11")
}

func TestDiagIgnoresWarningsAndPassesBudgetToOrch(t *testing.T) {
	b := newDiagBox(t)
	data := fmt.Sprintf(`{"verb":"diag","results":[{"name":%q,"status":"ok","total":1,"items":[{"path":%q,"line":3,"kind":"warning","text":"unused"}]}]}`,
		b.file("a.go"), b.file("a.go"))
	if err := os.WriteFile(filepath.Join(b.fake, "diag.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_LSP_DIAG_BUDGET_MS", "2500")
	if got := b.edit("s1", "a.go"); got != "" {
		t.Errorf("a warning was reported: %q", got)
	}
	args, _ := os.ReadFile(filepath.Join(b.fake, "diag.args"))
	eq(t, strings.TrimSpace(string(args)), "lsp diag "+b.file("a.go")+" --json --wait-ms 2300 --limit 50")
}

func TestDiagScriptEditsAreReportedOnceAtREPL(t *testing.T) {
	b := newDiagBox(t)
	b.answer("ok", []string{"a.go", "b.go"}, diagErr{"a.go", 1, "bad a"}, diagErr{"b.go", 2, "bad b"})
	if got := b.scriptEdit("s1", "a.go"); got != "" {
		t.Errorf("a script's edit produced output: %q", got)
	}
	b.scriptEdit("s1", "b.go")
	b.scriptEdit("s1", "a.go") // a repeat is recorded once
	if got := b.pending("s1"); len(got) != 2 {
		t.Fatalf("pending %v", got)
	}
	eq(t, b.repl("s1"), "orch diagnostics:\na.go:1: bad a\nb.go:2: bad b")
	if got := b.pending("s1"); len(got) != 0 {
		t.Errorf("pending not cleared: %v", got)
	}
	if got := b.repl("s1"); got != "" {
		t.Errorf("a second REPL repeated the message: %q", got)
	}
}

func TestDiagBeyondTheBudgetStartsWarmAndKeepsPending(t *testing.T) {
	b := newDiagBox(t)
	b.answer("ok", []string{"a.go"}, diagErr{"a.go", 1, "bad a"})
	b.scriptEdit("s1", "a.go")
	t.Setenv("FAKE_SLOW", "1")
	t.Setenv("ORCH_LSP_DIAG_BUDGET_MS", "300")
	if got := b.repl("s1"); got != "" {
		t.Errorf("a slow server still answered: %q", got)
	}
	marker := filepath.Join(b.fake, "warm.marker")
	waitFor(t, "orch lsp warm", func() bool { _, err := os.Stat(marker); return err == nil })
	if got := b.pending("s1"); len(got) != 1 {
		t.Errorf("pending %v, want the path kept", got)
	}
	t.Setenv("FAKE_SLOW", "")
	t.Setenv("ORCH_LSP_DIAG_BUDGET_MS", "5000")
	eq(t, b.repl("s1"), "orch diagnostics:\na.go:1: bad a")
	var outcomes []string
	for _, r := range b.rows() {
		outcomes = append(outcomes, fmt.Sprint(r["outcome"]))
	}
	eq(t, strings.Join(outcomes, ","), "pending,timeout,reported")
}

func TestDiagNoServerIsSilent(t *testing.T) {
	b := newDiagBox(t)
	data := fmt.Sprintf(`{"verb":"diag","results":[{"name":%q,"status":"no_server","message":"no language server","items":[]}]}`, b.file("a.go"))
	if err := os.WriteFile(filepath.Join(b.fake, "diag.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_EXIT", "2")
	if got := b.edit("s1", "a.go"); got != "" {
		t.Errorf("no_server produced output: %q", got)
	}
	rows := b.rows()
	if len(rows) != 1 || rows[0]["outcome"] != "no_server" || rows[0]["harness"] != "claude" || rows[0]["origin"] != "direct" {
		t.Errorf("rows %v", rows)
	}
}

func TestDiagOffSwitches(t *testing.T) {
	for _, env := range []string{"ORCH_LSP_DIAG", "ORCH_TOOL_GUARD"} {
		t.Run(env, func(t *testing.T) {
			b := newDiagBox(t)
			b.answer("ok", []string{"a.go"}, diagErr{"a.go", 1, "bad a"})
			t.Setenv(env, "off")
			if got := b.edit("s1", "a.go"); got != "" {
				t.Errorf("%s=off still reported: %q", env, got)
			}
			if _, err := os.Stat(filepath.Join(b.fake, "diag.args")); err == nil {
				t.Errorf("%s=off still ran orch", env)
			}
		})
	}
}

func TestPiDiagEditWriteAndCodemode(t *testing.T) {
	b := newDiagBox(t)
	b.answer("ok", []string{"a.go"}, diagErr{"a.go", 1, "bad a"})
	res := func(session, tool, parent, path string) PiResponse {
		req := map[string]any{"event": "tool_result", "session": session, "cwd": b.repo, "tool": tool, "toolCallId": "c1",
			"input": map[string]any{"path": b.file(path)}, "content": []string{"ok"}}
		if parent != "" {
			req["parent"] = parent
		}
		ev, ok := ParseEvent(jsonBytes(t, req))
		if !ok {
			t.Fatal("bad event")
		}
		return PiCoach(ev)
	}
	got := res("p1", "edit", "", "a.go")
	if len(got.AppendText) != 1 || got.AppendText[0] != "orch diagnostics:\na.go:1: bad a" {
		t.Errorf("direct edit: %+v", got)
	}
	if got := res("p2", "edit", "cm1", "a.go"); len(got.AppendText) != 0 {
		t.Errorf("nested edit got text: %+v", got)
	}
	res("p2", "write", "cm1", "b.go")
	b.answer("ok", []string{"a.go", "b.go"}, diagErr{"a.go", 1, "bad a"}, diagErr{"b.go", 2, "bad b"})
	got = res("p2", "codemode", "", "a.go")
	if len(got.AppendText) != 1 || got.AppendText[0] != "orch diagnostics:\na.go:1: bad a\nb.go:2: bad b" {
		t.Errorf("codemode: %+v", got)
	}
}
