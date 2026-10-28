package guard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// deny-search-first: the first rg of a user prompt that clearly looks for where
// code lives is blocked once and points at jbcontext or orch lsp. The retry
// passes, so a misjudged exact-text search loses one call. Anything that may be
// exact text passes: a block the model only retries teaches it nothing.

const (
	searchFirstID   = "search-first"
	searchFirstName = "deny-search-first"
)

var (
	conceptStrip = strings.NewReplacer(`\b`, "", `\<`, "", `\>`, "")
	lowerLetter  = regexp.MustCompile(`[a-z]`)
	upperLetter  = regexp.MustCompile(`[A-Z]`)
	constName    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	phraseRule   = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)*(?: [a-z]+(?:-[a-z]+)*)+$`)
	indexedLine  = regexp.MustCompile(`(?m)^  - revision `)
)

// jbIndexedProbe says whether jbcontext has the repository indexed; tests
// replace it. jbcontext status takes about 4 seconds.
var jbIndexedProbe = func(root string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "jbcontext", "status", "--project-path", root).Output()
	return err == nil && indexedLine.Match(out)
}

// How long a probe answer holds for every session. Indexing is rare and the
// probe slow; a repository indexed later is noticed within the shorter wait.
const (
	jbIndexedTTL    = 24 * time.Hour
	jbNotIndexedTTL = time.Hour
)

type jbIndexEntry struct {
	Indexed bool      `json:"indexed"`
	At      time.Time `json:"at"`
}

// jbIndexed says whether jbcontext has root indexed: the session's answer, else
// a fresh one any session saved in jbindex.json, else a probe.
func jbIndexed(st *State, root string) bool {
	if v, ok := st.JBIndexed[root]; ok {
		return v
	}
	path := filepath.Join(StateDir(), "jbindex.json")
	cache := map[string]jbIndexEntry{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	e, ok := cache[root]
	ttl := jbNotIndexedTTL
	if e.Indexed {
		ttl = jbIndexedTTL
	}
	if !ok || time.Since(e.At) > ttl {
		e = jbIndexEntry{jbIndexedProbe(root), time.Now()}
		cache[root] = e
		if data, err := json.Marshal(cache); err == nil && os.MkdirAll(filepath.Dir(path), 0o777) == nil {
			tmp := path + "." + strconv.Itoa(os.Getpid())
			if os.WriteFile(tmp, data, 0o644) == nil {
				_ = os.Rename(tmp, path)
			}
		}
	}
	if st.JBIndexed == nil {
		st.JBIndexed = map[string]bool{}
	}
	st.JBIndexed[root] = e.Indexed
	return e.Indexed
}

// jbRefreshEvery: a prompt starts a jbcontext refresh of its checkout at most this often.
const jbRefreshEvery = 10 * time.Minute

// jbRefreshStart starts the refresh and leaves it running; tests replace it.
var jbRefreshStart = func(root string) {
	cmd := exec.Command("jbcontext", "index", "--only-incremental", "--silent", "--project-path", root)
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// jbRefresh starts a background incremental jbcontext index of the checkout
// holding cwd, so jbcontext search answers for a recent HEAD; nothing else
// refreshes it. jbcontext itself skips a repository with no index. Off with
// ORCH_JB_REFRESH=off, and in read-only retrieval.
func jbRefresh(cwd string) {
	if os.Getenv("ORCH_JB_REFRESH") == "off" || os.Getenv("ORCH_CTX_READONLY") != "" || os.Getenv("ORCH_CTX_ROOT") != "" || !onPath("jbcontext") {
		return
	}
	root := gitRoot(cwd)
	if root == "" {
		return
	}
	dir := filepath.Join(StateDir(), "jbrefresh")
	if os.MkdirAll(dir, 0o777) != nil {
		return
	}
	sum := sha256.Sum256([]byte(root))
	stamp := filepath.Join(dir, hex.EncodeToString(sum[:8]))
	if info, err := os.Stat(stamp); err == nil && time.Since(info.ModTime()) < jbRefreshEvery {
		return
	}
	if os.WriteFile(stamp, []byte(root+"\n"), 0o644) != nil {
		return
	}
	jbRefreshStart(root)
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

// altKind is what one alternative of an rg pattern looks for.
type altKind int

const (
	altText   altKind = iota // maybe exact text: an anchor, punctuation, a digit, sentence case
	altSymbol                // an identifier: an inner capital, pkg.Name, or a bounded name after func or type
	altConst                 // ALL_CAPS: a constant, or an env var orch lsp cannot find
	altWord                  // lowercase: a word, a snake_case column or key, a hyphenated class
	altPhrase                // lowercase words with a space: a description of behavior
	altNone                  // a definition prefix with no name (func )
)

// classifyAlt is the kind of one alternative and, for a symbol or a constant,
// the name orch lsp takes.
func classifyAlt(raw string) (altKind, string) {
	alt := conceptStrip.Replace(raw)
	if strings.HasPrefix(alt, "^") || strings.HasSuffix(alt, "$") {
		return altText, ""
	}
	if phraseRule.MatchString(alt) {
		return altPhrase, ""
	}
	stripped := defPrefix.ReplaceAllString(alt, "")
	withPrefix := stripped != alt
	if withPrefix && strings.TrimSpace(stripped) == "" {
		return altNone, ""
	}
	// func Test without a boundary lists every test; type Gateway\b names one.
	bounded := strings.HasSuffix(raw, `\b`) || strings.HasSuffix(raw, `\>`) || nameTrail.MatchString(stripped)
	stripped = strings.ReplaceAll(nameTrail.ReplaceAllString(stripped, ""), `\.`, ".")
	switch {
	case qualifiedName.MatchString(stripped) && upperLetter.MatchString(stripped):
		// pkg.Name or Type.Method is a name orch lsp takes; refund.unmatched is a regex.
		return altSymbol, stripped
	case strings.HasPrefix(stripped, "-") || !nameRule.MatchString(strings.ReplaceAll(stripped, "-", "_")):
		return altText, ""
	case constName.MatchString(stripped):
		return altConst, stripped
	case strings.Contains(stripped, "-"):
		return altWord, ""
	case lowerLetter.MatchString(stripped) && upperLetter.MatchString(stripped[1:]), withPrefix && bounded:
		return altSymbol, stripped
	case !upperLetter.MatchString(stripped):
		return altWord, ""
	}
	// One capitalized word (Driver, Notes) is as often UI text as a type.
	return altText, ""
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
	pattern, paths, globs, types, _ := rgParts(args)
	if docOnly(globs, types, paths) {
		return "", ""
	}
	var names []string
	kinds := map[altKind]int{}
	for _, alt := range splitAlternatives(pattern) {
		kind, name := classifyAlt(alt)
		kinds[kind]++
		if name != "" && !contains(names, name) && len(names) < maxNames {
			names = append(names, name)
		}
	}
	once := "this guard blocks once per user message."
	if kinds[altText] > 0 {
		return "", ""
	}
	// Identifiers alone go to orch lsp; a plain word among them may be text.
	if kinds[altPhrase] == 0 {
		if kinds[altSymbol] == 0 || kinds[altWord] > 0 || !t.orch {
			return "", ""
		}
		return Marker + " (" + searchFirstName + "): rg finds text, not definitions or uses. Run: orch lsp def " + strings.Join(names, " ") +
			" (several names in one call), or orch lsp refs " + names[0] + " for uses.\n" +
			"If you need the text itself (strings, comments, config), run the same rg again; " + once, pattern
	}
	// A description of behavior goes to jbcontext.
	if !onPath("jbcontext") {
		return "", ""
	}
	root := gitRoot(cwd)
	if root == "" {
		return "", ""
	}
	if !jbIndexed(st, root) {
		return "", ""
	}
	extra := ""
	if t.orch && kinds[altSymbol] > 0 {
		extra = "For the names in it: orch lsp def " + strings.Join(names, " ") + "\n"
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
