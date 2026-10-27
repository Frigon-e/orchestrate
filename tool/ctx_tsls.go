package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeScript symbol facts for question cards, from the TypeScript 7 native
// language server (`tsc --lsp --stdio`, found in the project's own
// typescript@7 package) spoken to over stdio with a small JSON-RPC client. One
// server serves a whole run: starting it is the real cost. It loads a project
// when a file of it is opened, so a source file of each project is opened first.

type tsJob struct{ root, sym string }

const (
	tsMaxSyms  = 3
	tsMinLen   = 5
	tsMaxDefs  = 4
	tsMaxFails = 3
)

// tsRequestLimit bounds one LSP request; a variable so tests can shorten it.
var tsRequestLimit = 45 * time.Second

var (
	tsLower   = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:[A-Z][a-z0-9]*)+\b`)
	tsPascal  = regexp.MustCompile(`\b[A-Z][a-z0-9]+(?:[A-Z][A-Za-z0-9]*)+\b`)
	tsFileRe  = regexp.MustCompile(`[A-Za-z0-9_@./-]+\.tsx?\b`)
	tsPlain   = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	tsKindRef = map[int]bool{5: true, 6: true, 10: true, 11: true, 12: true, 13: true, 14: true}
)

var tsKindName = map[int]string{2: "Module", 5: "Class", 6: "Method", 7: "Property", 8: "Field", 10: "Enum", 11: "Interface", 12: "Function", 13: "Variable", 14: "Constant", 23: "Struct", 26: "TypeParameter"}

// tsSymbols lists up to tsMaxSyms TypeScript-looking names: camelCase and
// PascalCase words with an uppercase letter and five or more characters, and
// backticked names. Names in skip (already resolved as Go) and file names are left out.
func tsSymbols(q string, skip map[string]bool) []string {
	type found struct {
		at int
		s  string
	}
	var all []found
	for _, m := range idBacktick.FindAllStringSubmatchIndex(q, -1) {
		span := q[m[2]:m[3]]
		if i := strings.Index(span, "("); i > 0 {
			span = span[:i]
		}
		all = append(all, found{m[0], span})
	}
	for _, re := range []*regexp.Regexp{tsLower, tsPascal} {
		for _, m := range re.FindAllStringIndex(q, -1) {
			all = append(all, found{m[0], q[m[0]:m[1]]})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at < all[j].at })
	var out []string
	seen := map[string]bool{}
	for _, f := range all {
		s := strings.TrimSpace(f.s)
		switch {
		case seen[s] || skip[s] || goplsCommon[s] || !tsPlain.MatchString(s) || len(s) < tsMinLen:
			continue
		case s == strings.ToLower(s): // no uppercase letter: a plain word or column
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == tsMaxSyms {
			break
		}
	}
	return out
}

// pnpmStores finds the node_modules/.pnpm directories of the checkout and of
// the other worktrees of its repository (a linked worktree has no install of
// its own), the checkout's own first.
func pnpmStores(code string) []string {
	var stores []string
	for _, root := range ctxRepoRoots(code) {
		stores = append(stores, pnpmStoresIn(root)...)
	}
	return stores
}

// pnpmStoresIn is a bounded walk (five levels) that goes into no node_modules
// and no tool directory, so no project layout is assumed.
func pnpmStoresIn(code string) []string {
	var stores []string
	_ = filepath.WalkDir(code, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(code, p)
		if rel != "." && strings.Count(rel, string(filepath.Separator)) >= 5 {
			return filepath.SkipDir
		}
		switch d.Name() {
		case ".git", "vendor", "target", ".venv", "__pycache__":
			return filepath.SkipDir
		case "node_modules":
			if fileExists(filepath.Join(p, ".pnpm")) {
				stores = append(stores, filepath.Join(p, ".pnpm"))
			}
			return filepath.SkipDir
		}
		return nil
	})
	return stores
}

// tsNativeServer finds the native TypeScript 7 server of the checkout: the
// executable inside the highest typescript@7 version of any pnpm store, in the
// platform package that version depends on. It is empty when there is none.
func tsNativeServer(code string) string {
	plat := map[string]string{"darwin": "darwin", "linux": "linux", "windows": "win32"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if plat == "" || arch == "" {
		return ""
	}
	best, bestVer := "", []int(nil)
	for _, pnpm := range pnpmStores(code) {
		dirs, _ := filepath.Glob(filepath.Join(pnpm, "typescript@7*"))
		for _, d := range dirs {
			name, _, _ := strings.Cut(strings.TrimPrefix(filepath.Base(d), "typescript@"), "_")
			var ver []int
			for _, p := range strings.Split(name, ".") {
				n, _ := strconv.Atoi(strings.SplitN(p, "-", 2)[0])
				ver = append(ver, n)
			}
			pkg := "typescript-" + plat + "-" + arch
			exe := filepath.Join(pnpm, "@typescript+"+pkg+"@"+name, "node_modules", "@typescript", pkg, "lib", "tsc")
			if runtime.GOOS == "windows" {
				exe += ".exe"
			}
			if fileExists(exe) && (best == "" || versionLess(bestVer, ver)) {
				best, bestVer = exe, ver
			}
		}
	}
	return best
}

func versionLess(a, b []int) bool {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// tsProjectRoot is the TypeScript project root (by manifest: tsconfig.json,
// package.json, ...) a question is about: the root of a .ts/.tsx file it
// mentions, else of an owned path, else the root with the most files. It is an
// absolute directory, or "" when the repo has no TypeScript root.
func tsProjectRoot(code, q string, owns []string) string {
	abs := func(r projectRoot) string {
		return filepath.Join(code, filepath.FromSlash(strings.TrimSuffix(r.Dir, "/")))
	}
	roots := rootsOf(code, "typescript")
	if len(roots) == 0 {
		return ""
	}
	files := trackedFiles(code)
	for _, m := range tsFileRe.FindAllString(q, -1) {
		m = strings.TrimLeft(m, "./")
		for _, f := range files {
			if f == m || strings.HasSuffix(f, "/"+m) {
				if r, ok := rootFor(code, f, "typescript"); ok {
					return abs(r)
				}
			}
		}
	}
	for _, o := range owns {
		p := strings.TrimPrefix(strings.TrimSpace(o), "./")
		if strings.HasSuffix(p, "/") || !strings.Contains(filepath.Base(p), ".") {
			p = strings.TrimSuffix(p, "/") + "/_"
		}
		if r, ok := rootFor(code, p, "typescript"); ok {
			return abs(r)
		}
	}
	return abs(roots[0])
}

// firstSource is a source file of a project to open so tsserver loads it (not
// a config file next to tsconfig.json).
func firstSource(root string) string {
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == "dist" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		if (strings.HasSuffix(p, ".tsx") || strings.HasSuffix(p, ".ts")) && !strings.HasSuffix(p, ".d.ts") && !isTestPath(p) && !strings.Contains(base, ".config.") {
			found = p
		}
		return nil
	})
	return found
}

type lspReply struct {
	raw json.RawMessage
	err error
}

// lspClient is a minimal JSON-RPC client over a child's stdio.
type lspClient struct {
	cmd      *exec.Cmd
	in       io.WriteCloser
	mu       sync.Mutex
	nextID   int
	waits    map[int]chan lspReply
	writeMu  sync.Mutex
	openedMu sync.Mutex
	opened   map[string]bool
	dead     chan struct{}
}

func startLSP(bin string, dir string, args ...string) (*lspClient, error) {
	resolved, err := ctxServerBinary(dir, bin)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(resolved, args...)
	// Run the checked file, but under the name it was found by: a multi-call
	// shim such as vite-plus `vp` acts as tsc only when argv[0] says tsc.
	cmd.Args[0] = bin
	cmd.Dir = dir
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &lspClient{cmd: cmd, in: in, waits: map[int]chan lspReply{}, opened: map[string]bool{}, dead: make(chan struct{})}
	go c.read(bufio.NewReaderSize(out, 1<<20))
	return c, nil
}

func (c *lspClient) send(v any) {
	b, _ := json.Marshal(v)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n", len(b))
	_, _ = c.in.Write(b)
}

func (c *lspClient) read(r *bufio.Reader) {
	defer close(c.dead)
	for {
		n := -1
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
				n, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		if n < 0 || n > 16<<20 {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Params struct {
				Items []json.RawMessage `json:"items"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &m) != nil {
			return
		}
		if len(m.ID) == 0 {
			continue
		} // a notification
		if m.Method != "" {
			// A request from the server must be answered or the server waits for
			// it: there is nothing to configure (one null per configuration item),
			// and capabilities it registers need no reply beyond null.
			var result any
			if m.Method == "workspace/configuration" {
				result = make([]any, len(m.Params.Items))
			}
			c.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(m.ID), "result": result})
			continue
		}
		id, _ := strconv.Atoi(string(m.ID))
		c.mu.Lock()
		ch := c.waits[id]
		delete(c.waits, id)
		c.mu.Unlock()
		if ch != nil {
			reply := lspReply{raw: m.Result}
			if m.Error != nil {
				reply.err = fmt.Errorf("JSON-RPC error %d: %s", m.Error.Code, m.Error.Message)
			} else if len(m.Result) == 0 {
				reply.err = fmt.Errorf("malformed JSON-RPC response: result missing")
			}
			ch <- reply
		}
	}
}

func (c *lspClient) call(method string, params any, limit time.Duration) (json.RawMessage, error) {
	ch := make(chan lspReply, 1)
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.waits[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.waits, id); c.mu.Unlock() }()
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("%s: %w", method, r.err)
		}
		return r.raw, nil
	case <-c.dead:
		return nil, fmt.Errorf("the language server exited")
	case <-time.After(limit):
		return nil, fmt.Errorf("%s timed out after %s", method, limit)
	}
}

func (c *lspClient) notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func fileURI(p string) string { return (&url.URL{Scheme: "file", Path: p}).String() }

// open sends didOpen for a source file once.
func (c *lspClient) open(path string) {
	c.openedMu.Lock()
	defer c.openedMu.Unlock()
	if c.opened[path] {
		return
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return
	}
	c.opened[path] = true
	lang := "typescript"
	if l := langOf(path); l != nil {
		lang = l.LangID
	}
	if l := langOf(path); l != nil && l.ID == "typescript" && strings.HasSuffix(path, "x") {
		lang = "typescriptreact"
	}
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(path), "languageId": lang, "version": 1, "text": string(text)}})
}

// close asks the server to shut down and kills it if it does not.
func (c *lspClient) close() {
	_, _ = c.call("shutdown", nil, 3*time.Second)
	c.notify("exit", nil)
	_ = c.in.Close()
	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
	}
}

type lspLoc struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct{ Line, Character int } `json:"start"`
		End   struct{ Line, Character int } `json:"end"`
	} `json:"range"`
}

// tsFacts looks a name up on an open server: exact workspace symbols, and for
// declarations that can be referenced, their reference sites.
func tsFacts(c *lspClient, code, name string) (symFact, error) {
	f := symFact{Symbol: name, Lang: "typescript"}
	raw, err := c.call("workspace/symbol", map[string]any{"query": name}, tsRequestLimit)
	if err != nil {
		return f, err
	}
	var syms []struct {
		Name     string `json:"name"`
		Kind     int    `json:"kind"`
		Location lspLoc `json:"location"`
	}
	if err := json.Unmarshal(raw, &syms); err != nil {
		return f, fmt.Errorf("workspace/symbol: malformed result: %w", err)
	}
	for _, s := range syms {
		if err := validLSPSymbol(s.Name, s.Kind, s.Location); err != nil {
			return f, fmt.Errorf("workspace/symbol: %w", err)
		}
	}
	relPath := func(uri string) string {
		u, err := url.Parse(uri)
		if err != nil {
			return uri
		}
		for _, root := range []string{code, evalOr(code)} {
			if r, err := filepath.Rel(root, u.Path); err == nil && !strings.HasPrefix(r, "..") {
				return filepath.ToSlash(r)
			}
		}
		return u.Path
	}
	for _, s := range syms {
		if s.Name != name || strings.Contains(s.Location.URI, "/node_modules/") {
			continue
		}
		file := relPath(s.Location.URI)
		d := symDef{Where: fmt.Sprintf("%s:%d", file, s.Location.Range.Start.Line+1), Kind: tsKindName[s.Kind], Match: "exact"}
		if s.Kind == 6 {
			d.Match = "candidate"
		}
		if d.Kind == "" {
			d.Kind = "Symbol"
		}
		if tsKindRef[s.Kind] {
			d.Func = true
			u, _ := url.Parse(s.Location.URI)
			c.open(u.Path)
			line, ch := s.Location.Range.Start.Line, s.Location.Range.Start.Character
			if text, err := os.ReadFile(u.Path); err == nil {
				ls := strings.Split(string(text), "\n")
				for l := line; l < len(ls) && l <= s.Location.Range.End.Line && l < line+5; l++ {
					if i := strings.Index(ls[l], name); i >= 0 {
						line, ch = l, i
						break
					}
				}
			}
			rr, err := c.call("textDocument/references", map[string]any{
				"textDocument": map[string]any{"uri": s.Location.URI},
				"position":     map[string]any{"line": line, "character": ch},
				"context":      map[string]any{"includeDeclaration": false},
			}, tsRequestLimit)
			var refs []lspLoc
			if err == nil {
				refs, err = decodeLSPLocs(rr, false)
			}
			if err != nil {
				symDefError(&d, "references", err)
			}
			for _, r := range refs {
				p := relPath(r.URI)
				if isTestPath(p) {
					d.Tests++
					if len(d.TestRefs) < ctxMaxRefs {
						d.TestRefs = append(d.TestRefs, fmt.Sprintf("%s:%d", p, r.Range.Start.Line+1))
					}
					continue
				}
				d.Total++
				if len(d.Callers) < ctxMaxRefs {
					d.Callers = append(d.Callers, fmt.Sprintf("%s:%d", p, r.Range.Start.Line+1))
				}
			}
		}
		f.Defs = append(f.Defs, d)

	}
	return f, nil
}

// tsFactsFor answers the jobs from the cache, and the rest from one server. It
// returns a note when something was missing or failed.
func tsFactsFor(order []tsJob, cacheDir, code string, roots map[string]int) (map[tsJob]*symFact, string) {
	facts := map[tsJob]*symFact{}
	var missing []tsJob
	for _, j := range order {
		sum := sha256.Sum256([]byte(j.root + "\x00" + j.sym))
		if b, err := os.ReadFile(filepath.Join(cacheDir, fmt.Sprintf("ts-v2-%x.json", sum[:8]))); err == nil {
			var f symFact
			if decodeSymCache(b, &f, j.sym, "typescript") {
				facts[j] = &f
				continue
			}
		}
		missing = append(missing, j)
	}
	note := ""
	if len(missing) > 0 {
		note = runTSServer(missing, facts, cacheDir, code, roots)
	}
	return facts, note
}

// tslsBin selects only a server permitted by the checkout trust policy.
func tslsBin(code string) string {
	bin, _ := tsServerChoice(code)
	return bin
}

func tsServerChoice(code string) (string, string) {
	if p := os.Getenv("ORCH_TSLS"); p != "" {
		if real, err := ctxServerBinary(code, p); err == nil {
			return real, ""
		}
		if ctxPathInRoot(code, p) {
			return "", ctxServerSkip("ts")
		}
		return p, "" // a missing override is reported by the caller
	}
	why := ""
	if ctxRepoTrusted(code) {
		if p := tsNativeServer(code); p != "" {
			if real, err := ctxServerBinary(code, p); err == nil {
				return real, ""
			}
		}
	} else if ctxLocalTSExists(code) {
		why = ctxServerSkip("ts")
	}
	// Generic tsc on PATH is usually the JavaScript compiler, not an LSP, so
	// tsc is accepted only when it reports TypeScript 7 or newer.
	for _, name := range []string{"tsgo", "tsc"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		// Trust first: tscIsNative executes p, so a candidate that fails the
		// trust check must never reach it.
		real, err := ctxServerBinary(code, p)
		if err != nil {
			if ctxPathInRoot(code, p) {
				why = ctxServerSkip("ts")
			}
			continue
		}
		if name == "tsc" {
			if !tscIsNative(p) {
				continue
			}
			// A shim such as vite-plus `vp` acts as tsc only under that
			// name, so launch the PATH entry, not its resolved target.
			return p, why
		}
		return real, why
	}
	return "", why
}

var tscVersionRe = regexp.MustCompile(`(?m)^Version (\d+)\.`)

// tscNativeCache holds the version answer per binary path, so the check runs
// once per process.
var tscNativeCache sync.Map

// tscIsNative reports whether the tsc at path is TypeScript 7 or newer, the
// native server, and not the JavaScript compiler of 5 or 6.
func tscIsNative(path string) bool {
	if v, ok := tscNativeCache.Load(path); ok {
		return v.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	native := false
	if out, err := exec.CommandContext(ctx, path, "--version").Output(); err == nil {
		if m := tscVersionRe.FindSubmatch(out); m != nil {
			n, _ := strconv.Atoi(string(m[1]))
			native = n >= 7
		}
	}
	tscNativeCache.Store(path, native)
	return native
}

// Presence checks are only for the skip note, never execution candidates.
func ctxLocalTSExists(code string) bool {
	for _, name := range []string{"tsgo", "tsc"} {
		if fileExists(filepath.Join(code, "node_modules", ".bin", name)) {
			return true
		}
	}
	for _, store := range pnpmStores(code) {
		paths, _ := filepath.Glob(filepath.Join(store, "@typescript+typescript-*", "node_modules", "@typescript", "typescript-*", "lib", "tsc*"))
		if len(paths) > 0 {
			return true
		}
	}
	return false
}

// tsClientCapabilities is what the native server needs to see to load a
// project and answer: configuration requests and dynamic registration of file
// watching and configuration changes.
var tsClientCapabilities = map[string]any{
	"textDocument": map[string]any{
		"synchronization": map[string]any{"dynamicRegistration": true},
		"documentSymbol":  map[string]any{"hierarchicalDocumentSymbolSupport": true},
		"definition":      map[string]any{"linkSupport": true},
		"references":      map[string]any{},
		"hover":           map[string]any{},
		"callHierarchy":   map[string]any{},
	},
	"workspace": map[string]any{
		"workspaceFolders": true, "configuration": true, "symbol": map[string]any{},
		"didChangeConfiguration": map[string]any{"dynamicRegistration": true},
		"didChangeWatchedFiles":  map[string]any{"dynamicRegistration": true},
	},
	"window": map[string]any{"workDoneProgress": true},
}

// tsReady bounds the wait for tsserver to load a project. It loads lazily
// after the first file is opened and answers empty until then (about two
// seconds on a large app). Zero skips the wait; tests set it so.
var tsReady = 20 * time.Second

// runTSServer starts one server, opens a file of each project and answers the
// jobs, caching each fact. It returns a note when something was missing or failed.
func runTSServer(jobs []tsJob, facts map[tsJob]*symFact, cacheDir, code string, roots map[string]int) string {
	bin, why := tsServerChoice(code)
	if why != "" && bin == "" {
		return why
	}
	if bin == "" {
		return "no TypeScript facts: no native TypeScript 7 server found (typescript@7 in a node_modules/.pnpm store, or a TypeScript 7 tsc on PATH, is found automatically); set ORCH_TSLS to one"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return "no TypeScript facts: " + bin + " was not found"
	}
	var rootList []string
	for r := range roots {
		rootList = append(rootList, r)
	}
	sort.Slice(rootList, func(i, j int) bool {
		if roots[rootList[i]] != roots[rootList[j]] {
			return roots[rootList[i]] > roots[rootList[j]]
		}
		return rootList[i] < rootList[j]
	})
	c, err := startLSP(bin, rootList[0], "--lsp", "--stdio")
	if err != nil {
		return "no TypeScript facts: " + err.Error()
	}
	defer c.close()
	var folders []map[string]string
	for _, r := range rootList {
		folders = append(folders, map[string]string{"uri": fileURI(r), "name": filepath.Base(r)})
	}
	raw, err := c.call("initialize", map[string]any{
		"processId": os.Getpid(), "rootUri": fileURI(rootList[0]), "rootPath": rootList[0], "workspaceFolders": folders,
		"clientInfo": map[string]any{"name": "orch"}, "capabilities": tsClientCapabilities,
	}, tsRequestLimit)
	if err != nil {
		return "no TypeScript facts: " + err.Error()
	}
	var init struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if json.Unmarshal(raw, &init) != nil || init.Capabilities == nil {
		return "no TypeScript facts: initialize: malformed capabilities result"
	}
	c.notify("initialized", map[string]any{})
	for _, r := range rootList {
		if src := firstSource(r); src != "" {
			c.open(src)
		}
	}
	note, fails := "", 0
	readyRoots := map[string]bool{}
	deadlines := map[string]time.Time{}
	for _, j := range jobs {
		ready := readyRoots[j.root]
		deadline := deadlines[j.root]
		if deadline.IsZero() {
			deadline = time.Now().Add(tsReady)
			deadlines[j.root] = deadline
		}
		if fails >= tsMaxFails {
			break
		}
		f, err := tsFacts(c, code, j.sym)
		// tsserver answers empty until its project has loaded: until a name
		// has been found, an empty answer is asked again for a while.
		for !ready && err == nil && len(f.Defs) == 0 && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			f, err = tsFacts(c, code, j.sym)
		}
		ready = ready || len(f.Defs) > 0
		readyRoots[j.root] = ready
		if err != nil {
			if fails++; note == "" {
				note = "TypeScript facts stopped: " + err.Error()
			}
			continue
		}
		if !ready && len(f.Defs) == 0 {
			f.Errors = append(f.Errors, j.root+": workspace/symbol: project readiness unconfirmed; empty response is not proof of absence")
		}
		facts[j] = &f
		if symFactValid(&f, j.sym) {
			sum := sha256.Sum256([]byte(j.root + "\x00" + j.sym))
			ctxCacheWrite(filepath.Join(cacheDir, fmt.Sprintf("ts-v2-%x.json", sum[:8])), f)
		}
	}
	return note
}

// dropGoMiss removes the Go "no symbol named" fact for a name that TypeScript did find.
func dropGoMiss(facts []symFact, name string) []symFact {
	var out []symFact
	for _, f := range facts {
		if f.Symbol == name && f.Lang != "typescript" && len(f.Defs) == 0 {
			continue
		}
		out = append(out, f)
	}
	return out
}
