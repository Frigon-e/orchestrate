package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// lspServer is what differs between language servers; starting, handshaking,
// using and closing one is the same code (openServer, lspClient) for all.
type lspServer interface {
	// commands finds the server binaries to try, in order. why is the reason
	// to report when none is usable (or a skip note), empty when there is none.
	commands(code string) (cands []lspCmd, why string)
	// folders returns the workspace folders to send, given the project root and
	// every root the session serves.
	folders(root string, all []string) []string
	// afterStart runs once the handshake succeeded, e.g. to open a source file.
	afterStart(c *lspClient, roots []string)
	// tag names the session's server (empty: the binary's name) and says
	// whether it is the TypeScript server, which answers empty while loading.
	tag() (name string, ts bool)
	// needRefs says whether a server without references capability is rejected.
	needRefs() bool
	// failed is the error when every candidate failed; tried are their names
	// and err the last failure.
	failed(tried []string, err error) string
}

// serverFor picks the adapter of a language: gopls for Go, the native
// TypeScript server for TypeScript, the registry's list otherwise (also for a
// language whose servers the user configured).
func serverFor(e *ctxEnv, l *langDef) lspServer {
	switch {
	case l.native() && l.ID == "go":
		return goplsServer{bin: e.Gopls}
	case l.native() && l.ID == "typescript":
		return tsServer{}
	}
	return stdServer{l: l}
}

// native reports whether l is spoken to by dedicated adapters (Go, TypeScript)
// rather than the registry's server list: a user config with its own server
// commands makes it a registry language.
func (l *langDef) native() bool {
	return !l.Custom && (l.ID == "go" || l.ID == "typescript")
}

// stdServer is the default: the language's candidate list, or its override.
type stdServer struct{ l *langDef }

func (s stdServer) commands(code string) ([]lspCmd, string) {
	l := s.l
	servers := l.LSP
	override := l.LSPEnv != "" && len(strings.Fields(os.Getenv(l.LSPEnv))) > 0
	if override {
		bin, args, ok, why := langServerReason(code, l)
		if !ok {
			return nil, why
		}
		servers = []lspCmd{{bin, args}} // the override is the only candidate
	}
	var cands []lspCmd
	skipped := ""
	for _, c := range servers {
		tmp := *l
		tmp.LSP = []lspCmd{c}
		if !override {
			tmp.LSPEnv = ""
		}
		bin, args, found, why := langServerReason(code, &tmp)
		if why != "" {
			skipped = why
		}
		if found {
			cands = append(cands, lspCmd{bin, args})
		}
	}
	if len(cands) == 0 && skipped == "" {
		skipped = "no " + l.ID + " language server found"
	}
	return cands, skipped
}

func (stdServer) folders(root string, _ []string) []string { return []string{root} }
func (stdServer) afterStart(*lspClient, []string)          {}
func (stdServer) tag() (string, bool)                      { return "", false }
func (stdServer) needRefs() bool                           { return true }
func (s stdServer) failed(tried []string, _ error) string {
	return s.l.ID + " language server(s) " + strings.Join(tried, ", ") + " did not complete the handshake with references"
}

// goplsServer is the trusted gopls of the checkout, run as `gopls serve`.
type goplsServer struct{ bin string }

func (s goplsServer) commands(string) ([]lspCmd, string) {
	if s.bin == "" {
		return nil, "gopls is not trusted for this checkout"
	}
	if _, err := exec.LookPath(s.bin); err != nil {
		return nil, s.bin + " was not found"
	}
	return []lspCmd{{s.bin, []string{"serve"}}}, ""
}

func (goplsServer) folders(root string, _ []string) []string { return []string{root} }
func (goplsServer) afterStart(*lspClient, []string)          {}
func (goplsServer) tag() (string, bool)                      { return "gopls", false }
func (goplsServer) needRefs() bool                           { return false }
func (goplsServer) failed(_ []string, err error) string      { return errText(err) }

// tsServer is the newest native TypeScript server (see tsServerChoice), which
// serves every project root as a workspace folder.
type tsServer struct{}

func (tsServer) commands(code string) ([]lspCmd, string) {
	bin, why := tsServerChoice(code)
	if bin == "" {
		return nil, orDefault(why, "no native TypeScript 7 server found")
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, bin + " was not found"
	}
	return []lspCmd{{bin, []string{"--lsp", "--stdio"}}}, ""
}

func (tsServer) folders(_ string, all []string) []string { return all }
func (tsServer) afterStart(c *lspClient, roots []string) {
	for _, r := range roots {
		if src := firstSource(r); src != "" {
			c.open(src)
		}
	}
}
func (tsServer) tag() (string, bool)                 { return "", true }
func (tsServer) needRefs() bool                      { return false }
func (tsServer) failed(_ []string, err error) string { return errText(err) }

func errText(err error) string {
	if err == nil {
		return "the language server did not start"
	}
	return err.Error()
}

// openServer starts the first candidate of srv that completes the handshake on
// root (an absolute directory) and returns its session. roots are the project
// roots the session serves (just root for most servers).
func openServer(code string, srv lspServer, root string, roots []string) (*lspSession, string) {
	cands, why := srv.commands(code)
	if len(cands) == 0 {
		return nil, why
	}
	var tried []string
	var last error
	for _, cand := range cands {
		tried = append(tried, filepath.Base(cand.Bin))
		c, err := startLSP(cand.Bin, root, cand.Args...)
		if err != nil {
			last = err
			continue
		}
		caps, err := lspHandshake(c, root, srv.folders(root, roots))
		if err != nil {
			c.close()
			last = err
			continue
		}
		if srv.needRefs() && !caps.Refs {
			c.close()
			last = errors.New("no references capability")
			continue
		}
		srv.afterStart(c, roots)
		name, ts := srv.tag()
		if name == "" {
			name = filepath.Base(cand.Bin)
		}
		return &lspSession{c: c, caps: caps, bin: name, ts: ts}, ""
	}
	return nil, srv.failed(tried, last)
}

// lspHandshake initializes the server on root with the given workspace folders
// and announces `initialized`.
func lspHandshake(c *lspClient, root string, folders []string) (lspCaps, error) {
	var ws []map[string]string
	for _, f := range folders {
		ws = append(ws, map[string]string{"uri": fileURI(f), "name": filepath.Base(f)})
	}
	raw, err := c.call("initialize", map[string]any{
		"processId": os.Getpid(), "rootUri": fileURI(root), "rootPath": root, "workspaceFolders": ws,
		"clientInfo": map[string]any{"name": "orch"}, "capabilities": tsClientCapabilities,
	}, tsRequestLimit)
	if err != nil {
		return lspCaps{}, err
	}
	var init struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if json.Unmarshal(raw, &init) != nil || init.Capabilities == nil {
		return lspCaps{}, errors.New("initialize: malformed capabilities result")
	}
	c.notify("initialized", map[string]any{})
	return parseCaps(raw), nil
}
