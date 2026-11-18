package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpInstructions = "orch code intelligence. Use search for concepts (what does this code do, where is X handled), lsp for known symbols (definitions, references, callers, implementations, outlines, hover, diagnostics), and rg for exact text. Batch several lookups in one script and return only what you need."

const (
	mcpSearchDesc = "Semantic code search over the repo's jbcontext index: finds code by concept, not by exact text. " +
		"Returns {status: \"ok\"|\"no_index\"|\"rate_limited\"|\"error\", message?: string, hits: {path: string, start: number, end: number, snippet: string[]}[], notes?: string[]}"
	mcpLspDesc = "Symbol questions answered by the language servers: def, refs, callers, impls (names), symbols (path), hover (path, line, col), diagnostics (paths). " +
		"Returns {verb: string, results: {name: string, status: \"ok\"|\"ambiguous\"|\"not_found\"|\"no_server\"|\"error\", message?: string, total?: number, items: {path?: string, line?: number, kind?: string, name?: string, text?: string, doc?: string, in?: string, depth?: number}[]}[]}"
)

type mcpSearchIn struct {
	Query string `json:"query" jsonschema:"what to look for, in plain language"`
	Path  string `json:"path,omitempty" jsonschema:"limit the search to this path (relative to the repo root)"`
	Limit int    `json:"limit,omitempty" jsonschema:"most hits, default 10, max 50"`
	Dir   string `json:"dir,omitempty" jsonschema:"repo directory, absolute or relative to the server's working directory; default the working directory"`
}

type mcpHit struct {
	Path    string   `json:"path"`
	Start   int      `json:"start"`
	End     int      `json:"end"`
	Snippet []string `json:"snippet"`
}

type mcpSearchOut struct {
	Status  string   `json:"status" jsonschema:"ok, no_index, rate_limited or error"`
	Message string   `json:"message,omitempty"`
	Hits    []mcpHit `json:"hits"`
	Notes   []string `json:"notes,omitempty"`
}

type mcpLspIn struct {
	Verb  string   `json:"verb" jsonschema:"def, refs, callers, impls, symbols, hover or diagnostics"`
	Names []string `json:"names,omitempty" jsonschema:"symbol names (Name, pkg.Name or Type.Method); required for def, refs, callers, impls; for diagnostics, taken as paths when paths is empty"`
	Path  string   `json:"path,omitempty" jsonschema:"file path; required for symbols and hover"`
	Paths []string `json:"paths,omitempty" jsonschema:"file paths; required for diagnostics"`
	Line  int      `json:"line,omitempty" jsonschema:"1-based line; required for hover"`
	Col   int      `json:"col,omitempty" jsonschema:"1-based column; required for hover"`
	Limit int      `json:"limit,omitempty" jsonschema:"most hits per name, default 30"`
	Dir   string   `json:"dir,omitempty" jsonschema:"repo directory, absolute or relative to the server's working directory; default the working directory"`
}

type mcpLspOut struct {
	Verb    string      `json:"verb"`
	Results []lspAnswer `json:"results"`
}

func cmdMcp(a *app, args []string) error {
	if len(args) > 0 {
		return &exitError{code: 2, msg: "usage: orch mcp (runs an MCP server on stdin/stdout)"}
	}
	server := newMcpServer()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, &mcp.StdioTransport{})
}

func newMcpServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "orch", Version: orchVersion}, &mcp.ServerOptions{Instructions: mcpInstructions})
	f := false
	ann := func(title string) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &f, Title: title}
	}
	mcp.AddTool(server, &mcp.Tool{Name: "search", Description: mcpSearchDesc, Annotations: ann("Search code by concept")}, mcpSafe(mcpSearch))
	mcp.AddTool(server, &mcp.Tool{Name: "lsp", Description: mcpLspDesc, Annotations: ann("Symbol lookups")}, mcpSafe(mcpLsp))
	return server
}

// mcpSafe turns a panic in a handler into a tool error instead of a dead server.
func mcpSafe[In, Out any](h func(context.Context, In) (Out, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "orch mcp: handler panic: %v\n", r)
				res, out, err = nil, *new(Out), fmt.Errorf("internal error: %v", r)
			}
		}()
		out, err = h(ctx, in)
		return nil, out, err
	}
}

// mcpDir resolves dir (default: the server's working directory) to an existing directory.
func mcpDir(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("dir %q is not an existing directory", dir)
	}
	return abs, nil
}

func mcpSearch(_ context.Context, in mcpSearchIn) (mcpSearchOut, error) {
	out := mcpSearchOut{Hits: []mcpHit{}}
	if strings.TrimSpace(in.Query) == "" {
		return out, errors.New("query is required")
	}
	dir, err := mcpDir(in.Dir)
	if err != nil {
		return out, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 10
	}
	limit = min(max(limit, 1), 50)
	var errw bytes.Buffer
	e, err := codeEnvFor(&app{cwd: dir, out: &bytes.Buffer{}, errw: &errw, now: time.Now})
	if err != nil {
		out.Status, out.Message = "error", err.Error()
		return out, nil
	}
	if why := e.jbMissing(); why != "" {
		out.Status, out.Message = "no_index", why
		return out, nil
	}
	hits, err := e.Concept(in.Query, in.Path, limit)
	for _, h := range hits {
		out.Hits = append(out.Hits, mcpHit{Path: h.Path, Start: h.Start, End: h.End, Snippet: append([]string{}, h.Snippet...)})
	}
	out.Notes = e.Notes()
	switch {
	case err == nil:
		out.Status = "ok"
	case errors.Is(err, errJBRateLimited):
		out.Status, out.Message = "rate_limited", err.Error()
	default:
		out.Status, out.Message = "error", err.Error()
	}
	return out, nil
}

func mcpLsp(_ context.Context, in mcpLspIn) (mcpLspOut, error) {
	out := mcpLspOut{Verb: in.Verb, Results: []lspAnswer{}}
	argv, err := mcpLspArgv(in)
	if err != nil {
		return out, err
	}
	dir, err := mcpDir(in.Dir)
	if err != nil {
		return out, err
	}
	var stdout, stderr bytes.Buffer
	a := &app{cwd: dir, out: &stdout, errw: &stderr, now: time.Now}
	runErr := cmdLsp(a, argv)
	var body struct {
		Results []lspAnswer `json:"results"`
	}
	if jerr := json.Unmarshal(stdout.Bytes(), &body); jerr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && runErr != nil {
			msg = runErr.Error()
		}
		if msg == "" {
			msg = "orch lsp printed no JSON"
		}
		return out, errors.New(msg)
	}
	if body.Results != nil {
		out.Results = body.Results
	}
	return out, nil
}

// mcpLspArgv validates the fields of one verb and builds the `orch lsp` argv.
func mcpLspArgv(in mcpLspIn) ([]string, error) {
	need := map[string]string{
		"def": "names", "refs": "names", "callers": "names", "impls": "names",
		"symbols": "path", "hover": "path, line, col", "diagnostics": "paths",
	}
	req, ok := need[in.Verb]
	if !ok {
		return nil, fmt.Errorf("unknown verb %q (valid: def, refs, callers, impls, symbols, hover, diagnostics)", in.Verb)
	}
	// Models often send diagnostics' files as names, the field every other list verb uses.
	if in.Verb == "diagnostics" && len(in.Paths) == 0 {
		in.Paths, in.Names = in.Names, nil
	}
	bad := func(why string) error { return fmt.Errorf("verb %s %s; it takes %s", in.Verb, why, req) }
	has := map[string]bool{
		"names": len(in.Names) > 0, "path": in.Path != "", "paths": len(in.Paths) > 0,
		"line": in.Line != 0, "col": in.Col != 0,
	}
	want := map[string]bool{}
	for f := range strings.SplitSeq(req, ", ") {
		want[f] = true
		if !has[f] {
			return nil, bad("is missing " + f)
		}
	}
	for f, set := range has {
		if set && !want[f] {
			return nil, bad("does not take " + f)
		}
	}
	for _, s := range append(append([]string{in.Path}, in.Names...), in.Paths...) {
		if strings.HasPrefix(s, "-") {
			return nil, fmt.Errorf("%q looks like a flag", s)
		}
	}
	limit := in.Limit
	if limit == 0 {
		limit = 30
	}
	limit = max(limit, 1)
	switch in.Verb {
	case "def":
		return append(append([]string{"def"}, in.Names...), "--json"), nil
	case "refs", "callers", "impls":
		return append(append([]string{in.Verb}, in.Names...), "--limit", fmt.Sprint(limit), "--json"), nil
	case "symbols":
		return []string{"symbols", in.Path, "--json"}, nil
	case "hover":
		if in.Line < 1 || in.Col < 1 {
			return nil, bad("needs 1-based line and col")
		}
		return []string{"hover", fmt.Sprintf("%s:%d:%d", in.Path, in.Line, in.Col), "--json"}, nil
	}
	return append(append([]string{"diag"}, in.Paths...), "--limit", fmt.Sprint(limit), "--json"), nil
}
