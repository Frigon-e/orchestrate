package guard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// deny-search-first: the first rg of a user prompt that looks like a "where does
// this live" search is blocked once and points at jbcontext or orch lsp. The
// retry passes, so exact-text searches lose one call at most.

const (
	searchFirstID   = "search-first"
	searchFirstName = "deny-search-first"
)

var (
	conceptWord  = regexp.MustCompile(`^[A-Za-z]+(?:[ -][A-Za-z]+)*$`)
	conceptStrip = strings.NewReplacer(`\b`, "", `\<`, "", `\>`, "")
	lowerLetter  = regexp.MustCompile(`[a-z]`)
	upperLetter  = regexp.MustCompile(`[A-Z]`)
	upperWord    = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)
	indexedLine  = regexp.MustCompile(`(?m)^  - revision `)
)

// jbIndexedProbe says whether jbcontext has the repository indexed; tests replace it.
var jbIndexedProbe = func(root string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "jbcontext", "status", "--project-path", root).Output()
	return err == nil && indexedLine.Match(out)
}

// conceptWordOK: letters and single spaces or hyphens, capitals only first or all of it.
func conceptWordOK(alt string) bool {
	if !conceptWord.MatchString(alt) {
		return false
	}
	if upperLetter.MatchString(alt[1:]) && strings.ToUpper(alt) != alt {
		return false
	}
	return true
}

// conceptRg: an rg whose pattern is plain words, a topic rather than a symbol or exact text.
func conceptRg(args []string) bool {
	pattern, paths, globs, types, _ := rgParts(args)
	if docOnly(globs, types, paths) {
		return false
	}
	text := strings.TrimSuffix(strings.TrimPrefix(conceptStrip.Replace(pattern), "^"), "$")
	lower := false
	for _, alt := range strings.Split(text, "|") {
		if !conceptWordOK(alt) {
			return false
		}
		lower = lower || lowerLetter.MatchString(alt)
	}
	return lower
}

// firstSegment is the first command of a line after a leading cd.
func firstSegment(line string) (string, []string) {
	listing := segments(line, true)
	if len(listing) > 0 {
		if name, _ := command(listing[0]); name == "cd" {
			listing = listing[1:]
		}
	}
	if len(listing) == 0 {
		return "", nil
	}
	return command(listing[0])
}

// rgCandidate is the args of an rg line the rule may apply to.
func rgCandidate(line, cwd string) ([]string, bool) {
	if found, ok := lookupShell(line); !ok || found.tool != "rg" {
		return nil, false
	}
	name, args := firstSegment(line)
	if name != "rg" {
		return nil, false
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--files" || a == "--type-list" || a == "--fixed-strings" || a == "-F" ||
			(flagWord.MatchString(a) && strings.Contains(a[1:], "F")) {
			return nil, false
		}
	}
	_, paths, _, _, _ := rgParts(args)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	for _, p := range paths {
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(cwd, full)
		}
		if info, err := os.Stat(full); err == nil && info.Mode().IsRegular() {
			return nil, false
		}
		if !insideRepository(cwd, p) {
			return nil, false
		}
	}
	if len(paths) == 0 && !insideRepository(cwd, ".") {
		return nil, false
	}
	return args, true
}

// searchFirstReason is the block text for an rg line, with the rg pattern, or "".
// It does not mark the window as blocked; the caller does that once it blocks.
func searchFirstReason(line, cwd string, st *State, t tools) (string, string) {
	if st.ConceptSeen || st.SearchFirstPrompt == st.Prompt+1 {
		return "", ""
	}
	args, ok := rgCandidate(line, cwd)
	if !ok {
		return "", ""
	}
	pattern, _, _, _, _ := rgParts(args)
	name := symbolName(args)
	if upperWord.MatchString(name) {
		name = "" // FAIL, TODO: exact text, not a symbol worth an lsp lookup
	}
	if name != "" {
		if !t.orch {
			return "", ""
		}
		return Marker + " (" + searchFirstName + "): rg finds text, not uses. For the symbol " + name +
			" run orch lsp def " + name + " or orch lsp refs " + name + " (also callers, impls).\n" +
			"If you need the text itself (strings, comments, config), run the same rg again; this guard blocks once per user message.", pattern
	}
	if !conceptRg(args) || !onPath("jbcontext") {
		return "", ""
	}
	root := gitRoot(cwd)
	if root == "" {
		return "", ""
	}
	indexed, known := st.JBIndexed[root]
	if !known {
		indexed = jbIndexedProbe(root)
		if st.JBIndexed == nil {
			st.JBIndexed = map[string]bool{}
		}
		st.JBIndexed[root] = indexed
	}
	if !indexed {
		return "", ""
	}
	return Marker + " (" + searchFirstName + "): this rg looks for where code lives, and no jbcontext or orch lsp lookup ran since the last user message.\n" +
		"Ask jbcontext first: jbcontext search \"Code that <does what you are looking for>\" --limit 3 (one behavior per query; send several queries as parallel calls in one message).\n" +
		"If you already know where it is or need exact text, run the same rg again; this guard blocks once per user message.", pattern
}

const searchFirstPoll = 25 * time.Millisecond

// searchFirstWait is how long a would-be block waits for a sibling lookup in the
// same message, whose PreToolUse hook may run concurrently; tests shorten it.
var searchFirstWait = 300 * time.Millisecond

// waitForLookup polls seen until it reports true or the wait is over.
func waitForLookup(seen func() bool) bool {
	for waited := time.Duration(0); waited < searchFirstWait; waited += searchFirstPoll {
		time.Sleep(searchFirstPoll)
		if seen() {
			return true
		}
	}
	return false
}

// searchFirstActive: the rule is on unless the guard is off or the rule is skipped.
func searchFirstActive() bool {
	if os.Getenv("ORCH_TOOL_GUARD") == "off" {
		return false
	}
	skip := skipped()
	return !skip[searchFirstID] && !skip[searchFirstName]
}

// searchFirstRelevant: only these lines need the session state.
func searchFirstRelevant(line string) bool {
	found, ok := lookupShell(line)
	return ok && (found.tool == "rg" || isGood(found.tool))
}

// searchFirstLog records a block of the rule, unless logging is off.
func searchFirstLog(harness, session, repo, pattern string) {
	if os.Getenv("ORCH_LOOKUP_LOG") == "off" {
		return
	}
	query := pattern
	if r := []rune(query); len(r) > queryMax {
		query = string(r[:queryMax])
	}
	appendLine(LogPath(), LookupLine{
		TS: nowTS(), Harness: harness, Session: session, Repo: repo,
		Tool: "blocked:rg", Query: query, OK: false, Reminder: strPtr(searchFirstID),
	})
}

// ladderLog records that the lookup ladder reminder fired.
func ladderLog(harness, session, repo string) {
	if os.Getenv("ORCH_LOOKUP_LOG") == "off" {
		return
	}
	appendLine(LogPath(), LookupLine{
		TS: nowTS(), Harness: harness, Session: session, Repo: repo,
		Tool: "prompt", OK: true, Reminder: strPtr("lookup-ladder"),
	})
}

func nowTS() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z") }
