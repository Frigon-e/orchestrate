package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Diagnostics: the language servers' current errors and warnings for a file,
// pulled with textDocument/diagnostic when the server advertises it, else
// taken from the textDocument/publishDiagnostics notifications it pushes.

type lspDiagnostic struct {
	Line     int // 0-based
	Severity int // 1 error, 2 warning, 3 info, 4 hint
	Message  string
	Source   string
	Code     string
}

// lspDiagState is what is known of one document's diagnostics: the items, the
// document version they answer (push servers that send one), when they came
// and the result id of the last pull.
type lspDiagState struct {
	items    []lspDiagnostic
	version  *int
	at       time.Time
	resultID string
}

type lspRawDiag struct {
	Range struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
	} `json:"range"`
	Severity int             `json:"severity"`
	Code     json.RawMessage `json:"code"`
	Source   string          `json:"source"`
	Message  string          `json:"message"`
}

func lspDiagItems(raw []lspRawDiag) []lspDiagnostic {
	out := make([]lspDiagnostic, 0, len(raw))
	for _, d := range raw {
		sev := d.Severity
		if sev < 1 || sev > 4 {
			sev = 1
		}
		code := strings.Trim(strings.TrimSpace(string(d.Code)), `"`)
		if code == "null" {
			code = ""
		}
		out = append(out, lspDiagnostic{Line: d.Range.Start.Line, Severity: sev, Message: d.Message, Source: d.Source, Code: code})
	}
	return out
}

// storePublished keeps a publishDiagnostics notification and wakes the waiters.
func (c *lspClient) storePublished(body []byte) {
	var m struct {
		Params struct {
			URI         string       `json:"uri"`
			Version     *int         `json:"version"`
			Diagnostics []lspRawDiag `json:"diagnostics"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &m) != nil || m.Params.URI == "" {
		return
	}
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	if c.diags == nil {
		c.diags = map[string]*lspDiagState{}
	}
	c.diags[m.Params.URI] = &lspDiagState{items: lspDiagItems(m.Params.Diagnostics), version: m.Params.Version, at: time.Now()}
	if c.diagWake != nil {
		close(c.diagWake)
	}
	c.diagWake = nil
}

// forgetDiags drops what is stored for a document the server was told to close.
func (c *lspClient) forgetDiags(path string) {
	c.diagMu.Lock()
	delete(c.diags, fileURI(path))
	c.diagMu.Unlock()
}

// syncDoc brings the server's copy of a file up to date with the disk: didOpen
// when it was not opened in this session, a full-text didChange when the disk
// text differs from what was last sent. sent says whether it told the server
// something; version is the document version now current.
func (c *lspClient) syncDoc(path string) (version int, sent bool, err error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return 0, false, err
	}
	c.openedMu.Lock()
	defer c.openedMu.Unlock()
	if !c.opened[path] {
		return c.didOpenLocked(path, text), true, nil
	}
	if sum := sha256.Sum256(text); sum != c.docHash[path] {
		c.docVer[path]++
		c.docHash[path] = sum
		c.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": fileURI(path), "version": c.docVer[path]},
			"contentChanges": []map[string]any{{"text": string(text)}},
		})
		return c.docVer[path], true, nil
	}
	return c.docVer[path], false, nil
}

// diagnose syncs the file and returns its diagnostics. A non-empty note means
// the answer is partial: the server did not settle within wait.
func (c *lspClient) diagnose(path string, pull bool, wait time.Duration) (items []lspDiagnostic, note string, err error) {
	since := time.Now()
	ver, sent, err := c.syncDoc(path)
	if err != nil {
		return nil, "", err
	}
	uri := fileURI(path)
	if pull {
		return c.pullDiag(uri, sent, wait)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		c.diagMu.Lock()
		st := c.diags[uri]
		fresh := st != nil && (!sent || (!st.at.Before(since) && (st.version == nil || *st.version >= ver)))
		if fresh {
			items = append([]lspDiagnostic(nil), st.items...)
			c.diagMu.Unlock()
			return items, "", nil
		}
		if c.diagWake == nil {
			c.diagWake = make(chan struct{})
		}
		wake := c.diagWake
		if st != nil {
			items = append([]lspDiagnostic(nil), st.items...)
		}
		c.diagMu.Unlock()
		select {
		case <-wake:
		case <-c.dead:
			return nil, "", fmt.Errorf("the language server exited")
		case <-deadline.C:
			return items, fmt.Sprintf("the language server published no diagnostics within %s; raise --wait-ms", wait), nil
		}
	}
}

func (c *lspClient) pullDiag(uri string, sent bool, wait time.Duration) ([]lspDiagnostic, string, error) {
	params := map[string]any{"textDocument": map[string]any{"uri": uri}}
	c.diagMu.Lock()
	prev := c.diags[uri]
	if prev != nil && prev.resultID != "" && !sent {
		params["previousResultId"] = prev.resultID
	}
	c.diagMu.Unlock()
	raw, err := c.call("textDocument/diagnostic", params, wait)
	if err != nil {
		if strings.Contains(err.Error(), "timed out after") {
			var items []lspDiagnostic
			if prev != nil {
				items = prev.items
			}
			return items, fmt.Sprintf("the language server did not answer textDocument/diagnostic within %s; raise --wait-ms", wait), nil
		}
		return nil, "", err
	}
	var rep struct {
		Kind     string       `json:"kind"`
		ResultID string       `json:"resultId"`
		Items    []lspRawDiag `json:"items"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, "", fmt.Errorf("textDocument/diagnostic: malformed result: %w", err)
	}
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	if c.diags == nil {
		c.diags = map[string]*lspDiagState{}
	}
	if rep.Kind == "unchanged" && prev != nil {
		prev.at = time.Now()
		if rep.ResultID != "" {
			prev.resultID = rep.ResultID
		}
		return append([]lspDiagnostic(nil), prev.items...), "", nil
	}
	items := lspDiagItems(rep.Items)
	c.diags[uri] = &lspDiagState{items: items, at: time.Now(), resultID: rep.ResultID}
	return items, "", nil
}

var lspDiagKinds = [...]string{1: "error", 2: "warning", 3: "info", 4: "hint"}

// diag answers one path: its diagnostics from the language server of its
// language. Relative paths resolve against the caller's directory.
func (r *lspRun) diag(arg string, limit int, wait time.Duration) lspAnswer {
	abs := arg
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(r.cwd, abs)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return lspAnswer{Name: arg, Status: "not_found", Items: []lspItem{}, Message: "no such file: " + arg}
	}
	if st.IsDir() {
		return lspAnswer{Name: arg, Status: "error", Items: []lspItem{}, Message: arg + ": a directory, not a file"}
	}
	path, bad := r.repoFile(abs)
	if bad != nil {
		bad.Name = arg
		return *bad
	}
	s, why := r.session(path)
	if s == nil {
		lang := "this file"
		if l := langOf(path); l != nil {
			lang = l.ID
		}
		return lspAnswer{Name: path, Status: "no_server", Items: []lspItem{}, Message: "no language server for " + lang + "; no diagnostics for " + path}.withWhy(why)
	}
	abs = filepath.Join(r.e.Code, filepath.FromSlash(path))
	items, note, err := s.c.diagnose(abs, s.caps.Diag, wait)
	if err != nil {
		return lspFailed(path, err)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Severity != items[j].Severity {
			return items[i].Severity < items[j].Severity
		}
		return items[i].Line < items[j].Line
	})
	an := lspAnswer{Name: path, Status: "ok", Items: []lspItem{}, Total: len(items)}
	if note != "" {
		an.Status, an.Message = "partial", note
	}
	for _, d := range items {
		if len(an.Items) == limit {
			break
		}
		an.Items = append(an.Items, lspItem{Path: path, Line: d.Line + 1, Kind: lspDiagKinds[d.Severity], Text: flat(d.Message), Name: orDefault(d.Source, d.Code)})
	}
	if len(items) == 0 && note == "" {
		an.Message = "no diagnostics in " + path
	}
	return an
}

func lspRenderDiag(an lspAnswer) string {
	var b strings.Builder
	for _, it := range an.Items {
		fmt.Fprintf(&b, "%s:%d: %s: %s", it.Path, it.Line, it.Kind, it.Text)
		if it.Name != "" {
			fmt.Fprintf(&b, " (%s)", it.Name)
		}
		b.WriteString("\n")
	}
	if an.Total > len(an.Items) {
		fmt.Fprintf(&b, "... %d more (%d total; raise --limit)\n", an.Total-len(an.Items), an.Total)
	}
	if an.Message != "" {
		b.WriteString(an.Message + "\n")
	}
	return b.String()
}
