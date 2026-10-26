package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// What a pass did with its tools, read from the pi session files (jsonl).
// Assistant messages carry toolCall blocks (name, arguments.path); the
// results come back as toolResult messages (toolName, text parts).

// eachSessionEvent calls fn for every well-formed event of every .jsonl file
// under dir, files in name order.
func eachSessionEvent(dir string, fn func(*piEvent)) {
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".jsonl" {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	for _, path := range files {
		if f, err := os.Open(path); err == nil {
			eachPiEvent(f, fn)
			f.Close()
		}
	}
}

type toolCallBlock struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Text      string `json:"text"`
	Arguments struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
	} `json:"arguments"`
}

// blocks decodes the content parts of a message; a string content gives none.
func (e *piEvent) blocks() []toolCallBlock {
	var parts []toolCallBlock
	if json.Unmarshal(e.Message.Content, &parts) != nil {
		return nil
	}
	return parts
}

// sessionProgress is the number of turns of a pass so far (assistant messages
// with at least one toolCall) and the context of its newest request (input
// plus cache read of the newest assistant usage).
func sessionProgress(dir string) (turns, context int) {
	eachSessionEvent(dir, func(e *piEvent) {
		if e.Message.Role != "assistant" {
			return
		}
		for _, b := range e.blocks() {
			if b.Type == "toolCall" {
				turns++
				break
			}
		}
		if u := e.Message.Usage; u.Input+u.CacheRead > 0 {
			context = u.Input + u.CacheRead
		}
	})
	return turns, context
}

type pathCount struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

type bigResult struct {
	Tool  string `json:"tool"`
	Chars int    `json:"chars"`
}

// sessionProfile is the exploration a pass did.
type sessionProfile struct {
	Requests    int            `json:"requests"`
	Calls       map[string]int `json:"calls"`
	ResultChars int            `json:"result_chars"`
	Largest     []bigResult    `json:"largest"`
	Repeated    []pathCount    `json:"repeated_reads"`
	SpecReads   int            `json:"spec_reads"`
	// Shell splits the bash calls by what they ran (see shellKind);
	// BatchLookups counts the -q lookups inside orch ctx batch calls; the
	// reads are split by whether they named a range.
	Shell               map[string]int `json:"shell,omitempty"`
	BatchLookups        int            `json:"batch_lookups,omitempty"`
	RangedReads         int            `json:"ranged_reads"`
	WholeReads          int            `json:"whole_reads"`
	readBreakdownKnown  bool
	rangedReadsRecorded bool
	wholeReadsRecorded  bool
}

// Preserve presence independently from value. Historical profiles can record
// total read calls without measuring their range breakdown.
func (p *sessionProfile) UnmarshalJSON(b []byte) error {
	type plain sessionProfile
	var v plain
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	*p = sessionProfile(v)
	var calls map[string]json.RawMessage
	if json.Unmarshal(fields["calls"], &calls) == nil {
		for name, value := range calls {
			if string(value) == "null" {
				delete(p.Calls, name)
			}
		}
	}
	p.rangedReadsRecorded = fields["ranged_reads"] != nil && string(fields["ranged_reads"]) != "null"
	p.wholeReadsRecorded = fields["whole_reads"] != nil && string(fields["whole_reads"]) != "null"
	return nil
}

func (p sessionProfile) MarshalJSON() ([]byte, error) {
	type plain sessionProfile
	b, err := json.Marshal(plain(p))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	if !p.rangedReadsKnown() {
		delete(fields, "ranged_reads")
	}
	if !p.wholeReadsKnown() {
		delete(fields, "whole_reads")
	}
	return json.Marshal(fields)
}
func (p sessionProfile) rangedReadsKnown() bool {
	return p.readBreakdownKnown || p.rangedReadsRecorded || p.RangedReads > 0
}
func (p sessionProfile) wholeReadsKnown() bool {
	return p.readBreakdownKnown || p.wholeReadsRecorded || p.WholeReads > 0
}
func (p sessionProfile) rangeReadsKnown() bool { return p.rangedReadsKnown() && p.wholeReadsKnown() }
func (p sessionProfile) totalReads() (int, bool) {
	if n, ok := p.Calls["read"]; ok {
		return n, true
	}
	if p.rangeReadsKnown() {
		return p.RangedReads + p.WholeReads, true
	}
	return 0, false
}

var (
	shellWord = regexp.MustCompile(`[A-Za-z0-9_.-]+`)
	// pipedRe finds a pipe, a list separator or &&; 2>&1 is not one.
	pipedRe     = regexp.MustCompile(`[|;]|&&`)
	batchLookup = regexp.MustCompile(`(^|\s)-q\s`)
)

// shellKind names what a bash command ran: orch ctx (batch, another lookup, or
// either one piped into another tool), a raw search, a raw read of file text, or
// something else.
func shellKind(cmd string) string {
	c := strings.TrimSpace(cmd)
	if strings.Contains(c, "orch ctx") {
		kind := "orch ctx"
		if strings.Contains(c, "orch ctx batch") {
			kind = "orch ctx batch"
		}
		if pipedRe.MatchString(c) {
			kind += " piped"
		}
		return kind
	}
	for _, seg := range regexp.MustCompile(`&&|\|\||[|;]`).Split(c, -1) {
		switch w := shellWord.FindString(strings.TrimSpace(seg)); w {
		case "grep", "egrep", "fgrep", "rg", "find", "fd", "ls", "tree":
			return "shell search"
		case "sed", "cat", "head", "tail", "awk", "wc", "less", "nl":
			return "shell read"
		}
	}
	return "shell other"
}

// profileSession reads the session files under dir. specRel is the spec's
// repo-relative path ("" if unknown). It reports false when dir holds no
// assistant message.
func profileSession(dir, specRel string) (sessionProfile, bool) {
	p := sessionProfile{Calls: map[string]int{}, Shell: map[string]int{}, readBreakdownKnown: true}
	reads := map[string]int{}
	var results []bigResult
	eachSessionEvent(dir, func(e *piEvent) {
		switch e.Message.Role {
		case "assistant":
			p.Requests++
			for _, b := range e.blocks() {
				if b.Type != "toolCall" {
					continue
				}
				p.Calls[b.Name]++
				if b.Name == "bash" && b.Arguments.Command != "" {
					kind := shellKind(b.Arguments.Command)
					p.Shell[kind]++
					if kind == "orch ctx batch" || kind == "orch ctx batch piped" {
						p.BatchLookups += max(len(batchLookup.FindAllString(b.Arguments.Command, -1)), 1)
					}
				}
				if b.Name == "read" {
					if b.Arguments.Offset > 0 || b.Arguments.Limit > 0 {
						p.RangedReads++
					} else {
						p.WholeReads++
					}
				}
				if b.Name == "read" && b.Arguments.Path != "" {
					path := strings.TrimPrefix(filepath.ToSlash(b.Arguments.Path), "./")
					reads[path]++
					if specRel != "" && (path == specRel || strings.HasSuffix(path, "/"+specRel)) {
						p.SpecReads++
					}
				}
			}
		case "toolResult":
			n := 0
			for _, b := range e.blocks() {
				if b.Type == "text" {
					n += len(b.Text)
				}
			}
			p.ResultChars += n
			results = append(results, bigResult{e.Message.ToolName, n})
		}
	})
	if p.Requests == 0 {
		return p, false
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Chars > results[j].Chars })
	p.Largest = results[:min(5, len(results))]
	for path, n := range reads {
		if n > 1 {
			p.Repeated = append(p.Repeated, pathCount{path, n})
		}
	}
	sort.Slice(p.Repeated, func(i, j int) bool {
		if p.Repeated[i].Count != p.Repeated[j].Count {
			return p.Repeated[i].Count > p.Repeated[j].Count
		}
		return p.Repeated[i].Path < p.Repeated[j].Path
	})
	p.Repeated = p.Repeated[:min(5, len(p.Repeated))]
	return p, true
}

// String renders the profile on a few lines.
func (p sessionProfile) String() string {
	var names []string
	for n := range p.Calls {
		names = append(names, n)
	}
	sort.Strings(names)
	var calls []string
	for _, n := range names {
		calls = append(calls, fmt.Sprintf("%s %d", n, p.Calls[n]))
	}
	var big []string
	for _, b := range p.Largest {
		big = append(big, fmt.Sprintf("%s %d", b.Tool, b.Chars))
	}
	var rep []string
	for _, r := range p.Repeated {
		rep = append(rep, fmt.Sprintf("%s x%d", r.Path, r.Count))
	}
	var shell []string
	for _, k := range []string{"orch ctx batch", "orch ctx", "orch ctx batch piped", "orch ctx piped", "shell search", "shell read", "shell other"} {
		if n := p.Shell[k]; n > 0 {
			if k == "orch ctx batch" {
				shell = append(shell, fmt.Sprintf("%s %d (%d lookups)", k, n, p.BatchLookups))
			} else {
				shell = append(shell, fmt.Sprintf("%s %d", k, n))
			}
		}
	}
	out := fmt.Sprintf("requests %d; calls: %s; result chars %d; largest: %s; read more than once: %s; spec reads %d",
		p.Requests, orNone(calls), p.ResultChars, orNone(big), orNone(rep), p.SpecReads)
	if len(shell) > 0 {
		out += fmt.Sprintf("; bash: %s", orNone(shell))
	}
	if p.rangeReadsKnown() {
		out += fmt.Sprintf("; reads: %d ranged, %d whole file", p.RangedReads, p.WholeReads)
	} else if total, known := p.totalReads(); known {
		out += fmt.Sprintf("; reads: %d total; range breakdown not recorded", total)
	} else {
		out += "; reads: not recorded"
	}
	return out
}

func orNone(ss []string) string {
	if len(ss) == 0 {
		return "none"
	}
	return strings.Join(ss, ", ")
}
