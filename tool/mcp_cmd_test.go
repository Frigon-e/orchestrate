package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := newMcpServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func mcpCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func mcpGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, files)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@e.com", "commit", "-q", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestMcpListsSearchAndLspWithSchemas(t *testing.T) {
	cs := mcpSession(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s: input/output schema missing", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("%s: annotations = %+v", tool.Name, tool.Annotations)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"lsp", "search"}) {
		t.Errorf("tools = %v", names)
	}
}

func TestMcpLspValidationIsToolError(t *testing.T) {
	t.Setenv("ORCH_LSP_DAEMON", "off")
	cs := mcpSession(t)
	for name, args := range map[string]map[string]any{
		"def without names":       {"verb": "def"},
		"unknown verb":            {"verb": "nope"},
		"hover without position":  {"verb": "hover", "path": "a.go"},
		"symbols with names":      {"verb": "symbols", "path": "a.go", "names": []string{"X"}},
		"dir that does not exist": {"verb": "def", "names": []string{"X"}, "dir": "/no/such/dir"},
	} {
		res := mcpCall(t, cs, "lsp", args)
		if !res.IsError {
			t.Errorf("%s: want isError, got %+v", name, res)
		}
	}
	res := mcpCall(t, cs, "lsp", map[string]any{"verb": "def"})
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "names") {
		t.Errorf("error text = %q", txt)
	}
}

func TestMcpLspDiagnosticsTakesNamesAsPaths(t *testing.T) {
	for name, c := range map[string]struct {
		in   mcpLspIn
		want []string
		err  bool
	}{
		"paths":           {in: mcpLspIn{Verb: "diagnostics", Paths: []string{"a.go"}}, want: []string{"diag", "a.go", "--limit", "30", "--json"}},
		"names as paths":  {in: mcpLspIn{Verb: "diagnostics", Names: []string{"a.go", "b.go"}}, want: []string{"diag", "a.go", "b.go", "--limit", "30", "--json"}},
		"names and paths": {in: mcpLspIn{Verb: "diagnostics", Paths: []string{"a.go"}, Names: []string{"b.go"}}, err: true},
		"neither":         {in: mcpLspIn{Verb: "diagnostics"}, err: true},
		"flag in names":   {in: mcpLspIn{Verb: "diagnostics", Names: []string{"-x"}}, err: true},
	} {
		got, err := mcpLspArgv(c.in)
		if (err != nil) != c.err || !slices.Equal(got, c.want) {
			t.Errorf("%s: argv=%v err=%v", name, got, err)
		}
	}
}

func TestMcpLspDefReturnsStructuredResults(t *testing.T) {
	bin := goplsBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Skip("gopls is not installed")
	}
	t.Setenv("ORCH_GOPLS", bin)
	t.Setenv("ORCH_LSP_DAEMON", "off")
	dir := mcpGitRepo(t, map[string]string{"go.mod": "module fx\n\ngo 1.21\n", "fx.go": "package fx\n\n// Greet says hi.\nfunc Greet() string { return \"hi\" }\n"})
	cs := mcpSession(t)
	res := mcpCall(t, cs, "lsp", map[string]any{"verb": "def", "names": []string{"Greet"}, "dir": dir})
	if res.IsError {
		t.Fatalf("isError: %+v", res.Content)
	}
	var out mcpLspOut
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Verb != "def" || len(out.Results) != 1 || out.Results[0].Name != "Greet" || out.Results[0].Status != "ok" ||
		len(out.Results[0].Items) != 1 || out.Results[0].Items[0].Path != "fx.go" || out.Results[0].Items[0].Line != 4 {
		t.Errorf("out = %s", b)
	}
	// An unknown symbol is a normal not_found answer, not a tool error.
	res = mcpCall(t, cs, "lsp", map[string]any{"verb": "def", "names": []string{"NoSuchThing"}, "dir": dir})
	b, _ = json.Marshal(res.StructuredContent)
	if res.IsError || !strings.Contains(string(b), `"not_found"`) {
		t.Errorf("not_found: isError=%v %s", res.IsError, b)
	}
}

func TestMcpSearchStatuses(t *testing.T) {
	cs := mcpSession(t)
	search := func(args map[string]any) (*mcp.CallToolResult, mcpSearchOut) {
		res := mcpCall(t, cs, "search", args)
		var out mcpSearchOut
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
		return res, out
	}

	// No jbcontext binary: a no_index result, not an error.
	t.Setenv("ORCH_CACHE_DIR", t.TempDir())
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(t.TempDir(), "absent"))
	dir := mcpGitRepo(t, map[string]string{"a.go": "package a\n"})
	res, out := search(map[string]any{"query": "where is a handled", "dir": dir})
	if res.IsError || out.Status != "no_index" || out.Message == "" || out.Hits == nil {
		t.Errorf("no binary: isError=%v out=%+v", res.IsError, out)
	}

	// A working stand-in with an index: status ok, no hits.
	bin, code, _, _ := jbFixture(t, "HEAD")
	t.Setenv("ORCH_JBCONTEXT", bin)
	res, out = search(map[string]any{"query": "where is a handled", "dir": code, "limit": 500})
	if res.IsError || out.Status != "ok" || len(out.Hits) != 0 {
		t.Errorf("ok: isError=%v out=%+v", res.IsError, out)
	}

	// Bad input is a tool error.
	if res, _ := search(map[string]any{"query": "  "}); !res.IsError {
		t.Error("empty query: want isError")
	}
	if res, _ := search(map[string]any{"query": "x", "dir": filepath.Join(code, "a.go")}); !res.IsError {
		t.Error("dir that is a file: want isError")
	}
}

func TestMcpSubprocessWritesOnlyJSONRPCToStdout(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "orch")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "mcp")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "ORCH_LSP_DAEMON=off")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Wait() }()
	r := bufio.NewReader(stdout)
	send := func(msg string) {
		if _, err := stdin.Write([]byte(msg + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read stdout: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil || m["jsonrpc"] != "2.0" {
			t.Fatalf("stdout line is not JSON-RPC: %q", line)
		}
		return m
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	init := read()
	info := init["result"].(map[string]any)["serverInfo"].(map[string]any)
	if info["name"] != "orch" {
		t.Errorf("serverInfo = %v", info)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := read()
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Errorf("tools = %v", tools)
	}
	stdin.Close()
	if rest, _ := r.ReadString('\n'); strings.TrimSpace(rest) != "" {
		t.Errorf("extra stdout: %q", rest)
	}
}
