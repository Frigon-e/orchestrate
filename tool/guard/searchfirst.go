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

// rgSegments are the args of every rg in a line (any segment, after a cd or a
// pipe). An rg after a pipe with no path reads stdin, so it filters output and
// is never a candidate.
func rgSegments(line string) [][]string {
	var out [][]string
	for _, s := range segments(line, true) {
		name, args := command(s)
		if name != "rg" {
			continue
		}
		if _, paths, _, _, _ := rgParts(args); s.stdin && len(paths) == 0 {
			continue
		}
		out = append(out, args)
	}
	return out
}

// rgCandidate: the rg args pass the exact-text, file, repository and doc exclusions.
func rgCandidate(args []string, cwd string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--files" || a == "--type-list" || a == "--fixed-strings" || a == "-F" ||
			(flagWord.MatchString(a) && strings.Contains(a[1:], "F")) {
			return false
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
			return false
		}
		if !insideRepository(cwd, p) {
			return false
		}
	}
	return len(paths) > 0 || insideRepository(cwd, ".")
}

var (
	nameRule      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	qualifiedName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*$`)
	defPrefix     = regexp.MustCompile(`^(?:func(?: |\\s[+*])\\\(.*?\\\)(?: |\\s[+*])|(?:func|type|const|var|class|def|interface)(?:\\s[+*]| |\.\*)+)`)
	nameTrail     = regexp.MustCompile(`\\?\($`)
)

const maxNames = 6

// splitAlternatives splits a pattern on its top-level |.
func splitAlternatives(pattern string) []string {
	var out []string
	depth, from := 0, 0
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case '|':
			if depth == 0 {
				out = append(out, pattern[from:i])
				from = i + 1
			}
		}
	}
	return append(out, pattern[from:])
}

// patternNames are the identifiers an rg pattern names, in order, without
// duplicates and at most maxNames; allNames says every alternative is one.
// symbolLike are the names that look like symbols rather than plain words: an
// inner capital, an underscore or a definition prefix.
func patternNames(pattern string) (names, symbolLike []string, allNames bool) {
	text := strings.TrimSuffix(strings.TrimPrefix(conceptStrip.Replace(pattern), "^"), "$")
	allNames = true
	for _, alt := range splitAlternatives(text) {
		stripped := defPrefix.ReplaceAllString(alt, "")
		withPrefix := stripped != alt
		stripped = strings.ReplaceAll(nameTrail.ReplaceAllString(stripped, ""), `\.`, ".")
		// pkg.Name or Type.Method is a name orch lsp takes; refund.unmatched is a regex.
		qualified := qualifiedName.MatchString(stripped) && upperLetter.MatchString(stripped)
		if !nameRule.MatchString(stripped) && !qualified {
			allNames = false
			continue
		}
		if contains(names, stripped) || len(names) >= maxNames {
			continue
		}
		names = append(names, stripped)
		if withPrefix || qualified || strings.Contains(stripped, "_") || capital.MatchString(stripped[1:]) {
			symbolLike = append(symbolLike, stripped)
		}
	}
	return names, symbolLike, allNames
}

func allBare(names []string) bool {
	for _, n := range names {
		if !upperWord.MatchString(n) {
			return false
		}
	}
	return len(names) > 0
}

// searchFirstReason is the block text for the first rg in a line the rule
// blocks, with that rg's pattern, or "". It does not mark the window as blocked;
// the caller does that once it blocks.
func searchFirstReason(line, cwd string, st *State, t tools) (string, string) {
	if st.ConceptSeen || st.SearchFirstPrompt == st.Prompt+1 {
		return "", ""
	}
	for _, args := range rgSegments(line) {
		if !rgCandidate(args, cwd) {
			continue
		}
		if reason, pattern := searchFirstSegment(args, cwd, st, t); reason != "" {
			return reason, pattern
		}
	}
	return "", ""
}

func searchFirstSegment(args []string, cwd string, st *State, t tools) (string, string) {
	pattern, paths, globs, types, word := rgParts(args)
	if docOnly(globs, types, paths) {
		return "", ""
	}
	names, symbolLike, allNames := patternNames(pattern)
	if word {
		symbolLike = names
	}
	once := "this guard blocks once per user message."
	if allNames && len(symbolLike) > 0 && !allBare(names) && t.orch {
		return Marker + " (" + searchFirstName + "): rg finds text, not definitions or uses. Run: orch lsp def " + strings.Join(names, " ") +
			" (several names in one call), or orch lsp refs " + names[0] + " for uses.\n" +
			"If you need the text itself (strings, comments, config), run the same rg again; " + once, pattern
	}
	// A concept, or a mix of names and other text, goes to jbcontext; names alone
	// that are not symbol-like (FAIL, toast) are exact text.
	mixed := !allNames && len(names) > 0 && !allBare(names)
	if !(conceptRg(args) || mixed) || !onPath("jbcontext") {
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
	extra := ""
	if !allNames && t.orch && len(symbolLike) > 0 {
		extra = "For the names in it: orch lsp def " + strings.Join(symbolLike, " ") + "\n"
	}
	return Marker + " (" + searchFirstName + "): this rg looks for where code lives, and no jbcontext or orch lsp lookup ran since the last user message.\n" +
		"Ask jbcontext first: jbcontext search \"Code that <does what you are looking for>\" --limit 3 (one behavior per query; send several queries as parallel calls in one message).\n" +
		extra +
		"If you already know where it is or need exact text, run the same rg again; " + once, pattern
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

// searchFirstRelevant: only these lines need the session state - an rg in any
// segment, or a good lookup first.
func searchFirstRelevant(line string) bool {
	if len(rgSegments(line)) > 0 {
		return true
	}
	found, ok := lookupShell(line)
	return ok && isGood(found.tool)
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

// rgCandidateLine: a pi shell call holding an rg that deny-search-first judges, so
// the rule's block already told the model where to look and the ladder note is noise.
func rgCandidateLine(tool string, input map[string]any, cwd string) bool {
	if tool != "bash" || !searchFirstActive() {
		return false
	}
	for _, args := range rgSegments(jsString(input["command"])) {
		if rgCandidate(args, cwd) {
			return true
		}
	}
	return false
}
