package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// diagFake is a pipe-backed language server: handle sees every message the
// client sends and may answer through reply or push notifications through send.
type diagFake struct {
	mu   sync.Mutex
	seen []string
}

func (f *diagFake) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func newDiagClient(t *testing.T, handle func(method string, params json.RawMessage, send func(any)) any) (*lspClient, *diagFake) {
	t.Helper()
	requests, in := io.Pipe()
	out, responses := io.Pipe()
	c := &lspClient{in: in, waits: map[int]chan lspReply{}, opened: map[string]bool{}, dead: make(chan struct{})}
	go c.read(bufio.NewReader(out))
	fake := &diagFake{}
	frame := func(v any) {
		body, _ := json.Marshal(v)
		fmt.Fprintf(responses, "Content-Length: %d\r\n\r\n%s", len(body), body)
	}
	go func() {
		defer responses.Close()
		r := bufio.NewReader(requests)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			var n int
			if _, err = fmt.Sscanf(strings.TrimSpace(line), "Content-Length: %d", &n); err != nil {
				return
			}
			if _, err = r.ReadString('\n'); err != nil {
				return
			}
			b := make([]byte, n)
			if _, err = io.ReadFull(r, b); err != nil {
				return
			}
			var m struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(b, &m) != nil {
				return
			}
			fake.mu.Lock()
			fake.seen = append(fake.seen, m.Method)
			fake.mu.Unlock()
			v := handle(m.Method, m.Params, func(v any) { frame(v) })
			if m.ID != 0 {
				frame(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": v})
			}
		}
	}()
	t.Cleanup(func() { in.Close(); out.Close(); requests.Close(); responses.Close() })
	return c, fake
}

func diagPush(uri string, version any, line int, msg string) map[string]any {
	p := map[string]any{"uri": uri, "diagnostics": []any{}}
	if msg != "" {
		p["diagnostics"] = []any{map[string]any{"range": map[string]any{"start": map[string]any{"line": line, "character": 0}}, "severity": 1, "source": "fake", "message": msg}}
	}
	if version != nil {
		p["version"] = version
	}
	return map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": p}
}

func diagFile(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(evalOr(t.TempDir()), "a.go")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiagPullSyncsChangesAndReusesUnchanged(t *testing.T) {
	p := diagFile(t, "package a\nvar x int = \"s\"\n")
	var prevIDs []any
	var changed struct {
		Version int
		Text    string
	}
	c, fake := newDiagClient(t, func(method string, params json.RawMessage, send func(any)) any {
		var m map[string]any
		_ = json.Unmarshal(params, &m)
		switch method {
		case "textDocument/didChange":
			td := m["textDocument"].(map[string]any)
			changed.Version = int(td["version"].(float64))
			changed.Text = m["contentChanges"].([]any)[0].(map[string]any)["text"].(string)
		case "textDocument/diagnostic":
			prev := m["previousResultId"]
			prevIDs = append(prevIDs, prev)
			switch {
			case prev == "r1":
				return map[string]any{"kind": "unchanged", "resultId": "r1"}
			case changed.Version > 0:
				return map[string]any{"kind": "full", "resultId": "r2", "items": []any{}}
			}
			return map[string]any{"kind": "full", "resultId": "r1", "items": []any{map[string]any{
				"range": map[string]any{"start": map[string]any{"line": 1}}, "severity": 1, "code": 2322, "message": "cannot use"}}}
		}
		return nil
	})
	items, note, err := c.diagnose(p, true, time.Second)
	if err != nil || note != "" || len(items) != 1 || items[0].Line != 1 || items[0].Code != "2322" {
		t.Fatalf("first pull: %+v %q %v", items, note, err)
	}
	items, _, err = c.diagnose(p, true, time.Second)
	if err != nil || len(items) != 1 {
		t.Fatalf("unchanged must reuse the last items: %+v %v", items, err)
	}
	if err := os.WriteFile(p, []byte("package a\nvar x int = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, _, err = c.diagnose(p, true, time.Second)
	if err != nil || len(items) != 0 {
		t.Fatalf("after the fix: %+v %v", items, err)
	}
	if changed.Version != 2 || changed.Text != "package a\nvar x int = 1\n" {
		t.Errorf("didChange = %+v, want version 2 with the new text", changed)
	}
	want := "textDocument/didOpen,textDocument/diagnostic,textDocument/diagnostic,textDocument/didChange,textDocument/diagnostic"
	if got := strings.Join(fake.methods(), ","); got != want {
		t.Errorf("messages = %s", got)
	}
	// previousResultId is sent only while the document is unchanged.
	if len(prevIDs) != 3 || prevIDs[0] != nil || prevIDs[1] != "r1" || prevIDs[2] != nil {
		t.Errorf("previousResultId sequence = %v", prevIDs)
	}
}

func TestDiagPushWaitsForTheVersionOfTheSync(t *testing.T) {
	p := diagFile(t, "package a\nvar x int = \"s\"\n")
	c, _ := newDiagClient(t, func(method string, params json.RawMessage, send func(any)) any {
		var m struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
				Text    string `json:"text"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		_ = json.Unmarshal(params, &m)
		switch method {
		case "textDocument/didOpen", "textDocument/didChange":
			text := m.TextDocument.Text
			if len(m.ContentChanges) > 0 {
				text = m.ContentChanges[0].Text
			}
			msg := ""
			if strings.Contains(text, `"s"`) {
				msg = "cannot use"
			}
			send(diagPush(m.TextDocument.URI, m.TextDocument.Version, 1, msg))
		}
		return nil
	})
	items, note, err := c.diagnose(p, false, time.Second)
	if err != nil || note != "" || len(items) != 1 || items[0].Line != 1 || items[0].Source != "fake" {
		t.Fatalf("push after open: %+v %q %v", items, note, err)
	}
	if err := os.WriteFile(p, []byte("package a\nvar x int = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, note, err = c.diagnose(p, false, time.Second)
	if err != nil || note != "" || len(items) != 0 {
		t.Fatalf("push after the fix must be empty: %+v %q %v", items, note, err)
	}
	// Unchanged file: the stored publish answers at once.
	if items, note, _ = c.diagnose(p, false, 10*time.Millisecond); note != "" || len(items) != 0 {
		t.Errorf("unchanged file: %+v %q", items, note)
	}
}

func TestDiagPushThatNeverPublishesIsPartialWithinTheBudget(t *testing.T) {
	p := diagFile(t, "package a\n")
	c, _ := newDiagClient(t, func(string, json.RawMessage, func(any)) any { return nil })
	start := time.Now()
	items, note, err := c.diagnose(p, false, 150*time.Millisecond)
	if err != nil || len(items) != 0 || !strings.Contains(note, "raise --wait-ms") {
		t.Fatalf("%+v %q %v", items, note, err)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Errorf("waited %s for a 150ms budget", d)
	}
}

func TestDiagPushOfAnOlderVersionIsNotTaken(t *testing.T) {
	p := diagFile(t, "package a\n")
	c, _ := newDiagClient(t, func(method string, params json.RawMessage, send func(any)) any {
		if method == "textDocument/didOpen" {
			var m struct {
				TextDocument struct{ URI string } `json:"textDocument"`
			}
			_ = json.Unmarshal(params, &m)
			send(diagPush(m.TextDocument.URI, 0, 0, "stale"))
		}
		return nil
	})
	items, note, _ := c.diagnose(p, false, 100*time.Millisecond)
	if note == "" || len(items) != 1 {
		t.Errorf("a publish for version 0 answers nothing for version 1: %+v %q", items, note)
	}
}

func TestLspDiagRendersAndCapsErrorsFirst(t *testing.T) {
	an := lspAnswer{Name: "a.go", Status: "ok", Total: 3, Items: []lspItem{
		{Path: "a.go", Line: 4, Kind: "error", Text: "boom", Name: "gopls"},
		{Path: "a.go", Line: 2, Kind: "warning", Text: "meh"},
	}}
	want := "a.go:4: error: boom (gopls)\na.go:2: warning: meh\n... 1 more (3 total; raise --limit)\n"
	if got := lspRender("diag", an); got != want {
		t.Errorf("render = %q", got)
	}
	if got := lspRender("diag", lspAnswer{Name: "a.go", Status: "ok", Message: "no diagnostics in a.go"}); got != "no diagnostics in a.go\n" {
		t.Errorf("empty render = %q", got)
	}
}

// diagFx is a Go module with one type error on line 4.
const diagFxBad = "package fx\n\nfunc Run() int {\n\tvar x int = \"s\"\n\treturn x\n}\n"
const diagFxGood = "package fx\n\nfunc Run() int {\n\tvar x int = 1\n\treturn x\n}\n"

type diagOut struct {
	Verb    string      `json:"verb"`
	Results []lspAnswer `json:"results"`
}

func diagRun(t *testing.T, cwd string, args ...string) (int, diagOut) {
	t.Helper()
	r := runLspIn(cwd, append([]string{"diag"}, args...)...)
	var o diagOut
	if r.out != "" && strings.HasPrefix(r.out, "{") {
		if err := json.Unmarshal([]byte(r.out), &o); err != nil {
			t.Fatalf("%v\n%s", err, r.out)
		}
	}
	return r.code, o
}

func checkDiagLive(t *testing.T, h *harness) {
	t.Helper()
	writeTree(t, h.repo, map[string]string{"fx.go": diagFxBad})
	code, o := diagRun(t, h.repo, "fx.go", "--json", "--wait-ms", "20000")
	if code != 0 || len(o.Results) != 1 || len(o.Results[0].Items) == 0 {
		t.Fatalf("bad file: exit %d %+v", code, o)
	}
	it := o.Results[0].Items[0]
	if it.Kind != "error" || it.Line != 4 || it.Path != "fx.go" {
		t.Errorf("first item = %+v, want an error at fx.go:4", it)
	}
	writeTree(t, h.repo, map[string]string{"fx.go": diagFxGood})
	code, o = diagRun(t, h.repo, "fx.go", "--json", "--wait-ms", "20000")
	if code != 0 || len(o.Results) != 1 || o.Results[0].Status != "ok" || len(o.Results[0].Items) != 0 {
		t.Errorf("after the fix: exit %d %+v", code, o)
	}
	// A relative path resolves against the caller's directory.
	writeTree(t, h.repo, map[string]string{"fx.go": diagFxBad})
	r := runLspIn(h.repo, "diag", "fx.go", "missing.go", "--wait-ms", "20000")
	if r.code != 1 || !strings.Contains(r.out, "fx.go:4: error: ") || !strings.Contains(r.out, "no such file: missing.go") {
		t.Errorf("text output: %+v", r)
	}
}

func TestLspDiagLiveGoplsInProcess(t *testing.T) {
	h := daemonEnv(t)
	t.Setenv("ORCH_LSP_DAEMON", "off")
	checkDiagLive(t, h)
}

func TestLspDiagLiveGoplsThroughDaemon(t *testing.T) {
	h := daemonEnv(t)
	d := startTestDaemon(t, "test-build")
	checkDiagLive(t, h)
	if roots := d.pool.status(); len(roots) != 1 || len(roots[0].Sessions) != 1 {
		t.Errorf("the daemon should reuse one warm gopls: %+v", roots)
	}
}

func TestLspDiagPathsWithoutServerOrOutsideTheRepo(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	h := setup(t)
	t.Setenv("ORCH_LSP_DAEMON", "off")
	writeTree(t, h.repo, map[string]string{"notes.txt": "x\n"})
	outside := filepath.Join(t.TempDir(), "o.go")
	_ = os.WriteFile(outside, []byte("package o\n"), 0o644)
	code, o := diagRun(t, h.repo, "--json", "notes.txt", outside, "nope.go")
	if code != 2 || len(o.Results) != 3 {
		t.Fatalf("exit %d %+v", code, o)
	}
	if o.Results[0].Status != "no_server" || o.Results[1].Status != "error" || o.Results[2].Status != "not_found" {
		t.Errorf("statuses = %s %s %s", o.Results[0].Status, o.Results[1].Status, o.Results[2].Status)
	}
}
