package guard

import (
	"io"
	"os"
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

// tools is which replacements are installed, so a block never names a missing
// one. orch is only read for pi, which appends the lookup ladder to a search
// result while orch is on PATH.
type tools struct{ fd, rg, orch bool }

// harness is the agent the guard speaks to: the block texts name its own tools.
type harness int

const (
	forClaude harness = iota
	forPi
)

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

func installed() tools { return tools{fd: onPath("fd"), rg: onPath("rg")} }

func inPlaceEdit(args []string) bool {
	return anyOf(args, func(a string) bool { return strings.HasPrefix(a, "--in-place") || inPlace.MatchString(a) })
}

// perlInPlace reports a -i among perl's own switches. A switch cluster is read
// letter by letter: a letter that takes a value (-M, -I, -e ...) ends it, so
// -MTime::HiRes or -Ilib/util is not -i. Switches end at -- or the script name.
func perlInPlace(args []string) bool {
	for k := 0; k < len(args); k++ {
		a := args[k]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			return false
		}
		for j := 1; j < len(a); j++ {
			c := a[j]
			if c == 'i' {
				return true
			}
			if strings.IndexByte("eE", c) >= 0 {
				if j == len(a)-1 {
					k++ // the code is the next argument
				}
				break
			}
			if c == 'l' || c == '0' { // an optional number follows, then more switches
				digits := "01234567"
				if c == '0' && j+1 < len(a) && a[j+1] == 'x' {
					digits = "0123456789abcdefABCDEF"
					j++
				}
				for j+1 < len(a) && strings.IndexByte(digits, a[j+1]) >= 0 {
					j++
				}
				continue
			}
			if strings.IndexByte("MmIxCdDV", c) >= 0 {
				break
			}
		}
	}
	return false
}

type rule struct {
	id, name string
	match    func(line string, t tools) bool
	reason   func(t tools, line string) string
}

func piOr(h harness, pi, claude string) string {
	if h == forPi {
		return pi
	}
	return claude
}

// rules is in the order they run.
func rules(h harness) []rule {
	return []rule{
		{"sed-inplace", "deny-sed-inplace",
			func(line string, t tools) bool { _, hit := rewriteHit(line); return hit },
			func(t tools, line string) string {
				edit := piOr(h, "edit", "Edit")
				reason := Marker + " (deny-sed-inplace): sed -i and perl -pi rewrite a file in place.\n" +
					"Use the built-in " + edit + " tool. If the mtime must stay the same, run touch -r FILE REF " +
					"before the " + edit + " and touch -r REF FILE after. " +
					"Do not rewrite files with python, awk or tee."
				if rw, hit := rewriteHit(line); hit {
					if here := hereLine(rw, h); here != "" {
						reason += "\nHere: " + here
					}
				}
				return reason
			}},
		{"read-range", "deny-read-range",
			func(line string, t tools) bool { _, hit := readRangeHit(line); return hit },
			func(t tools, line string) string { return readRangeReason(line, h) }},
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
	t := installed()
	for _, r := range activeRules(forClaude) {
		if r.match(line, t) {
			io.WriteString(stderr, r.reason(t, line)+"\n")
			return 2
		}
	}
	return 0
}
