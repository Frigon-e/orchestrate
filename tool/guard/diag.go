package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Diagnostics after edits: when the model edits a file (Claude Edit, Write,
// MultiEdit; pi edit, write) the guard asks `orch lsp diag` for the file's type
// errors and adds the ones not yet reported to the tool result. A Code Mode
// script's edits are only recorded; the script's own tool call (Claude REPL,
// pi codemode) then gets one message for all of them. See docs/specs/orch-mcp-code-mode.md.

const (
	diagBudgetDefault = 2500 * time.Millisecond
	diagShowMax       = 8
	diagPendingMax    = 50
	diagHeader        = "orch diagnostics:"
)

// diagOff: ORCH_LSP_DIAG=off, or the whole guard off.
func diagOff() bool {
	return os.Getenv("ORCH_LSP_DIAG") == "off" || os.Getenv("ORCH_TOOL_GUARD") == "off"
}

func diagBudget() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("ORCH_LSP_DIAG_BUDGET_MS")); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return diagBudgetDefault
}

type diagLog struct {
	TS       string   `json:"ts"`
	Harness  string   `json:"harness"`
	Session  string   `json:"session"`
	Origin   string   `json:"origin"`
	Paths    []string `json:"paths"`
	Outcome  string   `json:"outcome"`
	Errors   int      `json:"errors"`
	Reported int      `json:"reported"`
	MS       int64    `json:"ms"`
}

func (l diagLog) write() {
	dir := cacheDir()
	if dir == "" || os.MkdirAll(dir, 0o777) != nil {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(l) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "lsp-diag.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(buf.Bytes())
}

// diagAnswer is one result of `orch lsp diag --json`.
type diagAnswer struct {
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Items  []lspItem `json:"items"`
}

// runDiag runs `orch lsp diag` for paths. timedOut: the deadline passed.
func runDiag(ctx context.Context, bin, cwd string, paths []string, waitMS int) (answers []diagAnswer, timedOut, failed bool) {
	args := append([]string{"lsp", "diag"}, paths...)
	args = append(args, "--json", "--wait-ms", strconv.Itoa(waitMS), "--limit", "50")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 50 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, true, false
	}
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		return nil, false, true
	}
	var doc struct {
		Results []diagAnswer `json:"results"`
	}
	if json.Unmarshal(out, &doc) != nil {
		return nil, false, true
	}
	return doc.Results, false, false
}

func diagKey(path, text string) string { return path + "\x00" + text }

func dropKeys(keys []string, path string) []string {
	out := []string{}
	for _, k := range keys {
		if !strings.HasPrefix(k, path+"\x00") {
			out = append(out, k)
		}
	}
	return out
}

func absIn(cwd, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

// diagCheck runs the diagnostics for paths and returns the text to add ("" for
// nothing). It updates the session state: the reported errors, and the pending
// paths when aggregate (they are cleared unless the budget ran out). The state
// lock is taken only after `orch lsp diag` returns, so hooks of parallel tool
// calls do not queue behind a slow language server.
func diagCheck(harness, session, cwd, origin string, paths []string, aggregate bool) string {
	start := time.Now()
	entry := diagLog{TS: start.UTC().Format(time.RFC3339Nano), Harness: harness, Session: session, Origin: origin, Paths: paths}
	finish := func(outcome string, errs, reported int) {
		entry.Outcome, entry.Errors, entry.Reported = outcome, errs, reported
		entry.MS = time.Since(start).Milliseconds()
		entry.write()
	}
	bin := orchBin()
	if bin == "" {
		finish("error", 0, 0)
		return ""
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	budget := diagBudget()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(budget))
	defer cancel()
	waitMS := int((budget - 200*time.Millisecond).Milliseconds())
	if waitMS < 1 {
		waitMS = 1
	}
	answers, timedOut, failed := runDiag(ctx, bin, cwd, paths, waitMS)
	if timedOut {
		startWarm(bin, repoOf(cwd))
		finish("timeout", 0, 0)
		return ""
	}
	state := openState(session)
	defer state.close()
	st := state.State
	if aggregate {
		for _, p := range paths {
			st.DiagPending = withoutKey(st.DiagPending, p)
		}
	}
	if failed {
		finish("error", 0, 0)
		return ""
	}
	root := repoOf(cwd)
	type found struct{ path, line, text string }
	var fresh []found
	total, served := 0, 0
	for _, a := range answers {
		if a.Status != "ok" && a.Status != "partial" {
			continue
		}
		served++
		file := absIn(cwd, a.Name)
		errs := 0
		for _, it := range a.Items {
			if it.Kind != "error" {
				continue
			}
			errs++
			p := absIn(cwd, it.Path)
			file = p
			key := diagKey(p, it.Text)
			if contains(st.DiagReported, key) {
				continue
			}
			rel := p
			if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
				rel = r
			}
			fresh = append(fresh, found{rel, strconv.Itoa(it.Line), it.Text})
			st.DiagReported = append(st.DiagReported, key)
		}
		total += errs
		if errs == 0 && a.Status == "ok" {
			st.DiagReported = dropKeys(st.DiagReported, file)
		}
	}
	if served == 0 {
		finish("no_server", 0, 0)
		return ""
	}
	if len(fresh) == 0 {
		finish("none", total, 0)
		return ""
	}
	shown := fresh
	if len(shown) > diagShowMax {
		shown = shown[:diagShowMax]
		// Errors past the cap stay unreported, so the next check can show them.
		for _, f := range fresh[diagShowMax:] {
			st.DiagReported = withoutKey(st.DiagReported, diagKey(absIn(root, f.path), f.text))
		}
	}
	lines := []string{diagHeader}
	for _, f := range shown {
		lines = append(lines, fmt.Sprintf("%s:%s: %s", f.path, f.line, f.text))
	}
	if more := len(fresh) - len(shown); more > 0 {
		lines = append(lines, fmt.Sprintf("... %d more: orch lsp diag %s", more, fresh[len(shown)].path))
	}
	finish("reported", total, len(shown))
	return strings.Join(lines, "\n")
}

// diagEdit handles the edit of one file: direct runs now, script records the path.
func diagEdit(harness, session, cwd, origin, path string) string {
	if diagOff() || path == "" {
		if diagOff() {
			diagLog{TS: time.Now().UTC().Format(time.RFC3339Nano), Harness: harness, Session: session, Origin: origin,
				Paths: []string{path}, Outcome: "off"}.write()
		}
		return ""
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	path = absIn(cwd, path)
	if origin == originScript {
		state := openState(session)
		defer state.close()
		if !contains(state.DiagPending, path) {
			state.DiagPending = append(state.DiagPending, path)
			if n := len(state.DiagPending); n > diagPendingMax {
				state.DiagPending = state.DiagPending[n-diagPendingMax:]
			}
		}
		diagLog{TS: time.Now().UTC().Format(time.RFC3339Nano), Harness: harness, Session: session, Origin: origin,
			Paths: []string{path}, Outcome: "pending"}.write()
		return ""
	}
	return diagCheck(harness, session, cwd, origin, []string{path}, false)
}

// diagFlush handles the end of a script call (REPL, codemode): one check for
// every path its edits recorded.
func diagFlush(harness, session, cwd string) string {
	if diagOff() {
		return ""
	}
	state := openState(session)
	paths := append([]string(nil), state.DiagPending...)
	state.close()
	if len(paths) == 0 {
		return ""
	}
	return diagCheck(harness, session, cwd, originScript, paths, true)
}

// claudeEditPath is the file of a Claude Edit, Write or MultiEdit event.
func claudeEditPath(ev *Event) string {
	switch ev.Str("tool_name") {
	case "Edit", "Write", "MultiEdit":
		input, _ := ev.Object("tool_input")
		p, _ := input["file_path"].(string)
		return p
	}
	return ""
}

func claudeDiagEvent(ev *Event) bool {
	name := ev.Str("hook_event_name")
	return name == "" || name == "PostToolUse"
}
