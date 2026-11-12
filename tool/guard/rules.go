package guard

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Marker opens every block reason.
const Marker = "orch tool guard"

// ladder is the line after the first line of a search block: where to look, in order.
const ladder = `Lookup order: jbcontext search "<one behavior>" if the location is unknown, ` +
	"orch lsp def|refs|callers <Name> for a known symbol, rg for exact text, " +
	"fd for file names; send independent lookups in one message."

var grepValue = set("-e", "-f", "-A", "-B", "-C", "-m", "-t", "-g", "-j", "--regexp", "--file", "--type",
	"--glob", "--max-count", "--after-context", "--before-context", "--context")

// harness is the agent the guard speaks to. A block text names that agent's
// own tools and spells each call with their real parameters, so the agent can
// send the call as written.
type harness struct {
	read, edit string // tool names
	path       string // the file parameter of read and edit
	pair       string // edit arguments for one replacement, a format of the quoted old and new text
	newText    string // the edit parameter that holds the new text
	everyMatch string // added to a replacement that must change every occurrence
	absPath    bool   // read and edit reject a relative path
}

var (
	forClaude = harness{read: "Read", edit: "Edit", path: "file_path",
		pair: "old_string=%s new_string=%s", newText: "new_string",
		everyMatch: " replace_all=true", absPath: true}
	// pi's edit has no replace-all: each oldText must match one place in the
	// file, or the edit fails.
	forPi = harness{read: "read", edit: "edit", path: "path",
		pair: `edits=[{"oldText":%s,"newText":%s}]`, newText: "newText",
		everyMatch: ", one edits entry per occurrence, each oldText with enough context to be unique"}
)

// readCall is the read of lines offset to offset+limit-1; limit 0 reads to the end.
func (h harness) readCall(file string, offset, limit int) string {
	call := fmt.Sprintf("%s %s=%s offset=%d", h.read, h.path, file, offset)
	if limit > 0 {
		call += fmt.Sprintf(" limit=%d", limit)
	}
	return call
}

// replacement is the edit arguments that change old to new, once or everywhere.
func (h harness) replacement(old, new string, all bool) string {
	args := fmt.Sprintf(h.pair, strconv.Quote(old), strconv.Quote(new))
	if all {
		args += h.everyMatch
	}
	return args
}

// absNote is the reminder that the tools need an absolute path, "" when the
// agent's tools take a relative one or every path is absolute.
func (h harness) absNote(files ...string) string {
	if !h.absPath {
		return ""
	}
	for _, f := range files {
		if !strings.HasPrefix(f, "/") {
			return " " + h.read + " and " + h.edit + " need an absolute path."
		}
	}
	return ""
}

func onPath(name string) bool {
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		path := dir + "/" + name
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && syscall.Access(path, 1) == nil {
			return true
		}
	}
	return false
}

func inPlaceEdit(args []string) bool {
	return anyOf(args, func(a string) bool { return strings.HasPrefix(a, "--in-place") || inPlace.MatchString(a) })
}

type rule struct {
	id, name string
	match    func(line string) bool
	reason   func(line string) string
}

// rules is in the order they run.
func rules(h harness) []rule {
	return []rule{
		{"sed-inplace", "deny-sed-inplace",
			func(line string) bool { _, hit := rewriteHit(line); return hit },
			func(line string) string {
				reason := Marker + " (deny-sed-inplace): sed -i and perl -pi rewrite a file in place.\n" +
					"Use the built-in " + h.edit + " tool. If the mtime must stay the same, run touch -r FILE REF " +
					"before the " + h.edit + " and touch -r REF FILE after. " +
					"Do not rewrite files with python, awk or tee."
				if rw, hit := rewriteHit(line); hit {
					if here := hereLine(rw, h); here != "" {
						reason += "\nHere: " + here
					}
				}
				return reason
			}},
		{"read-range", "deny-read-range",
			func(line string) bool { _, hit := readRangeHit(line); return hit },
			func(line string) string { return readRangeReason(line, h) }},
	}
}

// skipped is the ORCH_TOOL_GUARD_SKIP list.
func skipped() map[string]bool {
	out := map[string]bool{}
	for _, name := range strings.Split(os.Getenv("ORCH_TOOL_GUARD_SKIP"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			out[name] = true
		}
	}
	return out
}

// activeRules: ORCH_TOOL_GUARD=off disables every rule; ORCH_TOOL_GUARD_SKIP
// disables single rules by short name or by full name.
func activeRules(h harness) []rule {
	if os.Getenv("ORCH_TOOL_GUARD") == "off" {
		return nil
	}
	skip := skipped()
	var out []rule
	for _, r := range rules(h) {
		if !skip[r.id] && !skip[r.name] {
			out = append(out, r)
		}
	}
	return out
}

// Pre handles one PreToolUse event: a Read runs deny-big-read, a Bash call runs
// the deny rules in order and the first hit blocks. It returns the hook exit
// code (2 is a block, with the reason written to stderr). Anything it cannot
// read is allowed.
func Pre(ev *Event, stderr io.Writer) int {
	tool, present := ev.Value("tool_name")
	if tool == "Read" {
		return bigRead(ev, stderr)
	}
	if present && tool != nil && tool != "Bash" {
		return 0
	}
	input, ok := ev.Object("tool_input")
	if !ok {
		return 0
	}
	raw, ok := input["command"].(string)
	if !ok {
		return 0
	}
	line := strings.TrimSpace(raw)
	if line == "" {
		return 0
	}
	for _, r := range activeRules(forClaude) {
		if r.match(line) {
			io.WriteString(stderr, r.reason(line)+"\n")
			return 2
		}
	}
	return 0
}
