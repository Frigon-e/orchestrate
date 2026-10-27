package guard

import (
	"fmt"
	"io"
	"os"
	"regexp"
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

var (
	findChain       = set("-exec", "-execdir", "-ok", "-okdir", "-delete")
	searchSource    = set("find", "fd", "rg", "ag")
	pipeSink        = set("grep", "ag")
	xargsSink       = set("grep", "rg", "ag")
	plainSearch     = set("grep", "egrep", "fgrep", "rg", "ag")
	recursiveByDflt = set("rg", "ag")
	grepValue       = set("-e", "-f", "-A", "-B", "-C", "-m", "-t", "-g", "-j", "--regexp", "--file", "--type",
		"--glob", "--max-count", "--after-context", "--before-context", "--context")
	sedRange = regexp.MustCompile(`^\s*([0-9]+)(?:\s*,\s*([0-9]+))?\s*p\s*$`)
)

const readTail = "as a parallel call in the same message as your other commands."

// tools is which replacements are installed, so a block never names a missing one.
type tools struct{ fd, rg bool }

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

// denyFindChains: find running a command, or deleting, and a search tool feeding a grep.
func denyFindChains(line string) bool {
	listing := segments(line, false)
	for _, s := range listing {
		if name, args := command(s); name == "find" && anyOf(args, func(a string) bool { return findChain[a] }) {
			return true
		}
	}
	for at, s := range listing {
		if name, _ := command(s); !s.stdout || !searchSource[name] {
			continue
		}
		for nxt := at + 1; nxt < len(listing) && listing[nxt].stdin; nxt++ {
			name, args := command(listing[nxt])
			var sink bool
			if name == "xargs" {
				sink = anyOf(args, func(a string) bool { return xargsSink[base(a)] || a == "find" })
			} else {
				sink = pipeSink[name]
			}
			if sink {
				return true
			}
			if !listing[nxt].stdout {
				break
			}
		}
	}
	return false
}

// denyFind: find over a tree is a file search; a find reading a pipe formats stdin.
func denyFind(line string) bool {
	for _, s := range segments(line, false) {
		if name, _ := command(s); !s.stdin && name == "find" {
			return true
		}
	}
	return false
}

func denySedInplace(line string) bool {
	for _, s := range segments(line, false) {
		name, args := command(s)
		if (name == "sed" || name == "gsed") && inPlaceEdit(args) {
			return true
		}
		if name == "perl" && anyOf(args, inPlace.MatchString) {
			return true
		}
	}
	return false
}

// plainFiles: cat takes no counted flags, so every other word names a file.
func plainFiles(args []string) []string {
	var out []string
	for _, a := range args {
		if a != "-" && !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// sedFiles are the file words of a sed, which follow its script.
func sedFiles(args []string) []string {
	var out []string
	scripted := false
	for at := 0; at < len(args); at++ {
		arg := args[at]
		if strings.HasPrefix(arg, "-") && arg != "-" {
			if arg == "-e" || arg == "-f" || arg == "--expression" || arg == "--file" {
				scripted = true
				at++
			}
			continue
		}
		if !scripted {
			scripted = true
			continue
		}
		out = append(out, arg)
	}
	return out
}

func sedPrints(args []string) bool {
	return anyOf(args, func(a string) bool { return a == "-n" || sedFlagN.MatchString(a) }) ||
		anyOf(args, sedPrint.MatchString)
}

// searchTargets are the file and directory arguments of a grep, which follow its pattern.
func searchTargets(args []string) []string {
	var out []string
	patterned := false
	for at := 0; at < len(args); at++ {
		arg := args[at]
		if strings.HasPrefix(arg, "-") && arg != "-" {
			if grepValue[arg] {
				at++
			}
			continue
		}
		if !patterned {
			patterned = true
			continue
		}
		out = append(out, arg)
	}
	return out
}

// grepSearches: one segment is a raw content search over files.
func grepSearches(s segment, t tools) bool {
	name, args := command(s)
	if name == "" || !t.rg {
		return false
	}
	if name == "git" {
		return len(args) > 0 && args[0] == "grep" && !s.stdin
	}
	if name == "rg" || !plainSearch[name] {
		return false
	}
	if len(searchTargets(args)) > 0 {
		return true
	}
	recurses := recursiveByDflt[name] || anyOf(args, func(a string) bool {
		return strings.HasPrefix(a, "--recursive") || recursive.MatchString(a)
	})
	return recurses && !s.stdin
}

// denyGrepRg: a grep with a file or directory argument, or one that recurses,
// searches files. One reading stdin is a filter. rg is the replacement, so it is
// never blocked, and with no rg on PATH there is nothing to name, so nothing is.
func denyGrepRg(line string, t tools) bool {
	for _, s := range segments(line, false) {
		if grepSearches(s, t) {
			return true
		}
	}
	return false
}

// catHeadTailHit is the (name, args) of the first segment deny-cat-head-tail blocks.
func catHeadTailHit(line string) (string, []string, bool) {
	for _, s := range segments(line, false) {
		if s.stdin || s.stdout || s.redirect {
			continue
		}
		name, args := command(s)
		switch name {
		case "cat":
			if len(plainFiles(args)) == 1 {
				return name, args, true
			}
		case "sed":
			if inPlaceEdit(args) {
				continue
			}
			if sedPrints(args) && len(sedFiles(args)) == 1 {
				return name, args, true
			}
		}
	}
	return "", nil, false
}

// sedScript is the script word of a sed: the value of -e, else the first word that is not a flag.
func sedScript(args []string) string {
	for at := 0; at < len(args); at++ {
		arg := args[at]
		if arg == "-e" || arg == "--expression" {
			if at+1 < len(args) {
				return args[at+1]
			}
			return ""
		}
		if arg == "-f" || arg == "--file" {
			return ""
		}
		if !(strings.HasPrefix(arg, "-") && arg != "-") {
			return arg
		}
	}
	return ""
}

// catHeadTailReason is the block text: what the output is not, then the exact Read call to send.
func catHeadTailReason(line string) string {
	head := Marker + " (deny-cat-head-tail): cat and sed -n output is not a Read, so a later Edit of the file fails.\n"
	name, args, ok := catHeadTailHit(line)
	if !ok {
		return head + "Send Read with offset and limit instead, " + readTail
	}
	files := sedFiles(args)
	if name == "cat" {
		files = plainFiles(args)
	}
	path := files[0]
	call := "Read file_path=" + path
	var found []string
	if name == "sed" {
		found = sedRange.FindStringSubmatch(sedScript(args))
	}
	var text string
	if found != nil && rangeOK(found) {
		start, _ := strconv.Atoi(found[1])
		end := start
		if found[2] != "" {
			end, _ = strconv.Atoi(found[2])
		}
		text = fmt.Sprintf("Send %s offset=%d limit=%d instead, %s", call, start, end-start+1, readTail)
	} else {
		text = "Send " + call + " instead (add offset and limit for a section), " + readTail
	}
	if !strings.HasPrefix(path, "/") {
		text += " Read needs an absolute path."
	}
	return head + text
}

// rangeOK is true when the sed range ends at or after its start.
func rangeOK(found []string) bool {
	start, err := strconv.Atoi(found[1])
	if err != nil {
		return false
	}
	if found[2] == "" {
		return true
	}
	end, err := strconv.Atoi(found[2])
	return err == nil && end >= start
}

// findReason is a find rule's text: fd when it is installed, else the built-in tool.
func findReason(t tools, name, first, fallback string) string {
	if t.fd {
		return Marker + " (" + name + "): " + first + "\n" + ladder
	}
	return Marker + " (" + name + "): " + fallback
}

type rule struct {
	id, name string
	match    func(line string, t tools) bool
	reason   func(t tools, line string) string
}

// rules is in the order they run; a find chain is the sharper name for a plain find.
func rules() []rule {
	return []rule{
		{"find-chains", "deny-find-chains",
			func(line string, t tools) bool { return denyFindChains(line) },
			func(t tools, line string) string {
				return findReason(t, "deny-find-chains",
					"find -exec, -delete and find | grep chains are blocked; use fd, or rg -g 'GLOB' PATTERN for content.",
					"find -exec, -delete and find | grep chains are blocked; run a plain find and act on its output, or rg for content (fd is not installed).")
			}},
		{"find", "deny-find",
			func(line string, t tools) bool { return t.fd && denyFind(line) },
			func(t tools, line string) string {
				return findReason(t, "deny-find",
					"find over a tree is blocked; use fd (fd PATTERN dir, fd -e go, fd -g '*.ts').",
					"find over a tree is blocked; use fd.")
			}},
		{"sed-inplace", "deny-sed-inplace",
			func(line string, t tools) bool { return denySedInplace(line) },
			func(t tools, line string) string {
				return Marker + " (deny-sed-inplace): sed -i and perl -pi rewrite a file in place.\n" +
					"Use the built-in Edit tool. If the mtime must stay the same, run touch -r FILE REF " +
					"before the Edit and touch -r REF FILE after. " +
					"Do not rewrite files with python, awk or tee."
			}},
		{"grep-rg", "deny-grep-rg", denyGrepRg,
			func(t tools, line string) string {
				return Marker + " (deny-grep-rg): grep, egrep, fgrep, ag and git grep over files are blocked; " +
					"use rg (rg -n PATTERN path, -g 'GLOB', -F literal, -w word, -i).\n" + ladder
			}},
		{"cat-head-tail", "deny-cat-head-tail",
			func(line string, t tools) bool { _, _, ok := catHeadTailHit(line); return ok },
			func(t tools, line string) string { return catHeadTailReason(line) }},
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
func activeRules() []rule {
	if os.Getenv("ORCH_TOOL_GUARD") == "off" {
		return nil
	}
	skip := skipped()
	var out []rule
	for _, r := range rules() {
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
	active := activeRules()
	if len(active) == 0 {
		return 0
	}
	t := installed()
	for _, r := range active {
		if r.match(line, t) {
			io.WriteString(stderr, r.reason(t, line)+"\n")
			return 2
		}
	}
	return 0
}
