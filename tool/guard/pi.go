package guard

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The pi side of the guard. pi runs the extension tool/pi_ctx_reminder.ts, which
// holds no logic: it sends one JSON object per pi event to `orch-guard pre
// --harness pi` or `orch-guard coach --harness pi` on stdin and applies the one
// JSON object it gets back on stdout. Both exit 0, a block is the "block" field.
//
// Request (fields a mode does not use are absent):
//
//	event       "tool_call" (pre), or "tool_result", "tool_execution_end",
//	            "before_agent_start" (coach)
//	session     pi's session id, or "pi-<pid>"
//	turn        pi's turn index, or null before the first turn_start
//	cwd         the session's working directory
//	tool        pi's tool name: bash, read, grep, find, ls ...
//	input       the tool call's arguments
//	toolCallId  ties tool_result to tool_execution_end
//	content     tool_result: the text of each content part, "" for a non-text part
//	isError     tool_result: pi reported the call as failed
//	durationMs  tool_execution_end
//	resultHook  tool_call: a tool_result handler is registered (false selects the
//	            fallback that blocks the first search once)
//
// Response (all fields optional):
//
//	block       true to refuse the tool call; reason is the text the model sees
//	reason      the block reason
//	appendText  texts to append to a tool result, each as its own text part
//	message     before_agent_start: the text of the hidden message to add
//
// The per-session state (coach windows, the run of blocks, the one-time fallback
// block and a result waiting for its duration) lives in the coach state file of
// the session, so every call can be its own process.

// PiResponse is the one JSON object written to stdout.
type PiResponse struct {
	Block      bool     `json:"block,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	AppendText []string `json:"appendText,omitempty"`
	Message    string   `json:"message,omitempty"`
}

// giveWay is the line a block carries from the third block in a row.
const giveWay = "\nBlocked 3 times: use the replacement above; this guard does not give way."

// openMax is how many results wait for their duration (or the other way round).
const openMax = 200

// Open is a lookup row waiting for tool_execution_end, or a duration waiting for
// its tool_result: pi sends the two in either order.
type Open struct {
	ID         string      `json:"id"`
	Record     *LookupLine `json:"record,omitempty"`
	Ended      bool        `json:"ended,omitempty"`
	DurationMS *float64    `json:"duration_ms,omitempty"`
}

var shellSearches = set("rg", "grep", "egrep", "fgrep", "ag", "fd", "find")

// builtinRule refuses one of pi's own search tools while its shell replacement is on PATH.
type builtinRule struct {
	name, id, group, tool string
	replaced              func(t tools) bool
	reason                string
}

var builtinRules = []builtinRule{
	{"deny-builtin-grep", "builtin-grep", "builtin-search", "grep", func(t tools) bool { return t.rg },
		Marker + " (deny-builtin-grep): the built-in grep tool is blocked; " +
			"use rg in bash (rg -n PATTERN path, -g 'GLOB', -F literal, -w word, -i).\n" + ladder},
	{"deny-builtin-find", "builtin-find", "builtin-search", "find", func(t tools) bool { return t.fd },
		Marker + " (deny-builtin-find): the built-in find tool is blocked; " +
			"use fd in bash (fd PATTERN dir, fd -e go, fd -g '*.ts').\n" + ladder},
}

func activeBuiltinRules() []builtinRule {
	skip := skipped()
	var out []builtinRule
	for _, r := range builtinRules {
		if !skip[r.id] && !skip[r.name] && !skip[r.group] {
			out = append(out, r)
		}
	}
	return out
}

// jsString is JavaScript's String(v ?? "") for the scalars a tool call carries.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		if f, err := t.Float64(); err == nil && math.Abs(f) < 1e21 && (math.Abs(f) >= 1e-6 || f == 0) {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return t.String()
	}
	return pyStr(v)
}

func piInstalled() tools {
	t := installed()
	t.orch = onPath("orch")
	return t
}

func segmentSearch(s segment) bool {
	name, args := command(s)
	switch {
	case name == "":
		return false
	case name == "git":
		return len(args) > 0 && args[0] == "grep"
	case shellSearches[name]:
		return true
	case name == "xargs":
		return anyOf(args, func(a string) bool { return a == "git" || shellSearches[base(a)] })
	}
	return false
}

func commandSearch(line string) bool {
	for _, s := range segments(line, false) {
		if segmentSearch(s) {
			return true
		}
	}
	return false
}

// piIsSearch is a call whose result gets the ladder note: pi's grep, find and ls, or a shell search.
func piIsSearch(tool string, input map[string]any) bool {
	switch tool {
	case "grep", "find", "ls":
		return true
	case "bash":
		return commandSearch(jsString(input["command"]))
	}
	return false
}

// gitRoot is the real path of the git repository containing cwd (a worktree root
// has a .git file, so any entry counts), or "" outside one.
func gitRoot(cwd string) string {
	here, err := filepath.Abs(cwd)
	if cwd == "" || err != nil {
		if here, err = os.Getwd(); err != nil {
			return ""
		}
	}
	for dir := here; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				return real
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// insideRepository: relative and missing paths resolve from cwd, and a search
// outside the session's git repository is allowed.
func insideRepository(cwd, path string) bool {
	root := gitRoot(cwd)
	if root == "" {
		return false
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	resolved := filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	}
	rel, err := filepath.Rel(root, resolved)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// piBigReadReason is the deny-big-read text for pi's read call, or "" when it passes.
func piBigReadReason(cwd string, input map[string]any) string {
	path, ok := input["path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return ""
	}
	for _, key := range []string{"offset", "limit", "pages"} {
		if v, present := input[key]; present && v != nil && v != "" {
			return ""
		}
	}
	if readExemptSuffixes[strings.ToLower(splitExt(path))] {
		return ""
	}
	target := path
	if !filepath.IsAbs(target) && cwd != "" {
		target = filepath.Join(cwd, target)
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= bigReadBytes {
		return ""
	}
	return bigReadReason(path, info.Size(), forPi)
}

// lockedState is a session's state held under its flock; a state that cannot be
// opened is a throwaway, so a broken state dir only loses the session's memory.
type lockedState struct {
	*State
	file *os.File
}

func openState(session string) lockedState {
	name := sessionUnsafe.ReplaceAllString(session, "_")
	dir := StateDir()
	if os.MkdirAll(filepath.Join(dir, "coach"), 0o777) == nil {
		if f, state, err := lockState(dir, name); err == nil {
			return lockedState{state, f}
		}
	}
	return lockedState{State: &State{}}
}

func (l lockedState) close() {
	if l.file != nil {
		saveState(l.file, l.State)
		l.file.Close()
	}
}

// PiPre handles a pi tool_call event.
func PiPre(ev *Event) PiResponse {
	tool := ev.Str("tool")
	input, _ := ev.Object("input")
	cwd := ev.Str("cwd")
	guardOn := os.Getenv("ORCH_TOOL_GUARD") != "off"
	t := piInstalled()
	state := openState(ev.Str("session"))
	defer func() { state.close() }() // state is reopened after a wait for a sibling lookup

	block := func(reason string) PiResponse {
		state.BlocksInRow++
		if state.BlocksInRow >= 3 {
			reason += giveWay
		}
		return PiResponse{Block: true, Reason: reason}
	}
	// The built-in searches are the way around the shell rules, so they are
	// refused the same way while the shell replacement is on PATH.
	if guardOn {
		for _, r := range activeBuiltinRules() {
			if r.tool == tool && r.replaced(t) && insideRepository(cwd, jsString(input["path"])) {
				return block(r.reason)
			}
		}
		if tool == "read" && !bigReadSkipped() {
			if reason := piBigReadReason(cwd, input); reason != "" {
				return block(reason)
			}
		}
	}
	line := ""
	if tool == "bash" {
		line = strings.TrimSpace(jsString(input["command"]))
	}
	if line == "" {
		state.BlocksInRow = 0
		return PiResponse{}
	}
	if guardOn {
		for _, r := range activeRules(forPi) {
			if r.match(line, t) {
				return block(r.reason(t, line))
			}
		}
	}
	if guardOn && searchFirstActive() && searchFirstRelevant(line) {
		if found, ok := lookupShell(line); ok && isGood(found.tool) {
			state.ConceptSeen = true
		} else if reason, pattern := searchFirstReason(line, cwd, state.State, t); reason != "" {
			session := ev.Str("session")
			if state.SearchFirstWaited != state.Prompt+1 {
				state.SearchFirstWaited = state.Prompt + 1
				state.close() // release the lock while a sibling lookup may run
				if waitForLookup(func() bool {
					fresh := openState(session)
					fresh.close()
					return fresh.ConceptSeen
				}) {
					state = openState(session)
					return PiResponse{}
				}
				state = openState(session)
				reason, pattern = searchFirstReason(line, cwd, state.State, t)
			}
			if reason != "" {
				state.SearchFirstPrompt = state.Prompt + 1
				state.Pending = strPtr(searchFirstID)
				repo := gitRoot(cwd)
				if repo == "" {
					repo = cwd
				}
				searchFirstLog("pi", session, repo, pattern)
				return block(reason)
			}
		}
	}
	state.BlocksInRow = 0
	// No result hook available: remind by blocking the first shell search only,
	// then let every later search through so the retry always runs.
	hook, _ := ev.Value("resultHook")
	if !t.orch || !guardOn || truthy(hook) || state.FallbackReminded || !commandSearch(line) {
		return PiResponse{}
	}
	state.FallbackReminded = true
	return PiResponse{Block: true, Reason: ladder}
}

func piLookup(tool string, input map[string]any) (lookup, bool) {
	switch tool {
	case "bash":
		return lookupShell(jsString(input["command"]))
	case "read":
		path, ok := input["path"].(string)
		if !ok || path == "" {
			return lookup{}, false
		}
		return lookup{"read", piReadQuery(path, input["offset"], input["limit"]), false}, true
	}
	return lookup{}, false
}

func piReadQuery(path string, offset, limit any) string {
	blank := func(v any) bool { return v == nil || v == "" }
	if blank(offset) && blank(limit) {
		return path
	}
	part := func(v any) string {
		if blank(v) {
			return ""
		}
		return jsString(v)
	}
	return path + ":" + part(offset) + "-" + part(limit)
}

var piExitOne = regexp.MustCompile(`\n*(Command )?exited with code 1\s*$`)

// piOutcome: rg, fd and orch lsp exit 1 with no output when nothing matched; pi
// reports that as an error result ending in "exited with code 1".
func piOutcome(found lookup, text string, isError bool) (ok, empty bool) {
	if isError {
		body := piExitOne.ReplaceAllString(text, "")
		clean := body != text && strings.TrimSpace(body) == ""
		quiet := isSearch(found.tool) || strings.HasPrefix(found.tool, "lsp:")
		return clean && quiet, clean && quiet
	}
	return true, found.tool == "jbcontext" && strings.Contains(text, noResult)
}

// settle joins a lookup row with its duration; it returns the row to write once it has both.
func (s *State) settle(id string, record *LookupLine, ended bool, durationMS *float64) *LookupLine {
	at := -1
	for i, o := range s.Open {
		if o.ID == id {
			at = i
		}
	}
	entry := Open{ID: id}
	if at >= 0 {
		entry = s.Open[at]
		s.Open = append(s.Open[:at], s.Open[at+1:]...)
	}
	if record != nil {
		entry.Record = record
	}
	if ended {
		entry.Ended, entry.DurationMS = true, durationMS
	}
	if entry.Record != nil && entry.Ended {
		entry.Record.DurationMS = entry.DurationMS
		return entry.Record
	}
	s.Open = append(s.Open, entry)
	if len(s.Open) > openMax {
		s.Open = s.Open[len(s.Open)-openMax:]
	}
	return nil
}

func piTurn(ev *Event) *string {
	v, _ := ev.Value("turn")
	if n, ok := v.(json.Number); ok {
		s := n.String()
		return &s
	}
	return nil
}

// piAdjacent: three turn indexes in a row.
func piAdjacent(a, b, c string) bool {
	x, errA := strconv.Atoi(a)
	y, errB := strconv.Atoi(b)
	z, errC := strconv.Atoi(c)
	return errA == nil && errB == nil && errC == nil && y == x+1 && z == y+1
}

// PiCoach handles a pi tool_result, tool_execution_end or before_agent_start event.
func PiCoach(ev *Event) PiResponse {
	guardOn := os.Getenv("ORCH_TOOL_GUARD") != "off"
	coachOn := guardOn && os.Getenv("ORCH_LOOKUP_COACH") != "off"
	logOn := guardOn && os.Getenv("ORCH_LOOKUP_LOG") != "off"
	event := ev.Str("event")
	session := ev.Str("session")
	cwd := ev.Str("cwd")

	switch event {
	case "before_agent_start":
		if !coachOn {
			return PiResponse{}
		}
		state := openState(session)
		defer state.close()
		if state.promptStep(true) {
			if logOn {
				repo := gitRoot(cwd)
				if repo == "" {
					repo = cwd
				}
				ladderLog("pi", session, repo)
			}
			return PiResponse{Message: coachPrefix + coachText["lookup-ladder"]}
		}
	case "tool_execution_end":
		if !logOn || (ev.Str("tool") != "bash" && ev.Str("tool") != "read") {
			return PiResponse{}
		}
		var duration *float64
		if v, _ := ev.Value("durationMs"); v != nil {
			if n, ok := v.(json.Number); ok {
				if d, err := n.Float64(); err == nil && !math.IsNaN(d) && !math.IsInf(d, 0) {
					duration = &d
				}
			}
		}
		state := openState(session)
		defer state.close()
		if row := state.settle(ev.Str("toolCallId"), nil, true, duration); row != nil {
			appendLine(LogPath(), *row)
		}
	case "tool_result":
		return piResult(ev, session, cwd, coachOn, logOn)
	}
	return PiResponse{}
}

func piResult(ev *Event, session, cwd string, coachOn, logOn bool) PiResponse {
	tool := ev.Str("tool")
	input, _ := ev.Object("input")
	var content []string
	if v, _ := ev.Value("content"); v != nil {
		if list, ok := v.([]any); ok {
			for _, part := range list {
				s, _ := part.(string)
				content = append(content, s)
			}
		}
	}
	var out PiResponse
	// A search outside the repository is not guarded, so it gets no note.
	if onPath("orch") && piIsSearch(tool, input) && insideRepository(cwd, jsString(input["path"])) {
		marked := false
		for _, part := range content {
			marked = marked || strings.Contains(part, "Lookup order:")
		}
		if !marked {
			out.AppendText = append(out.AppendText, ladder)
		}
	}
	if !coachOn && !logOn {
		return out
	}
	found, ok := piLookup(tool, input)
	if !ok {
		return out
	}
	text := strings.Join(content, "")
	isError, _ := ev.Value("isError")
	state := openState(session)
	defer state.close()
	turn := piTurn(ev)
	reminder, followed := state.step(found.tool, found.symbol, turn, piAdjacent, coachOn)
	if reminder != nil {
		out.AppendText = append(out.AppendText, coachPrefix+coachText[*reminder])
	}
	if logOn {
		isOK, empty := piOutcome(found, text, truthy(isError))
		query := found.query
		if r := []rune(query); len(r) > queryMax {
			query = string(r[:queryMax])
		}
		repo := gitRoot(cwd)
		if repo == "" {
			repo = cwd
		}
		line := LookupLine{
			TS: time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"), Harness: "pi", Session: session, Repo: repo,
			Tool: found.tool, Query: query, OK: isOK, Empty: empty, Bytes: len(text), Reminder: reminder, Followed: followed,
		}
		if turn != nil {
			line.Turn = json.Number(*turn)
		}
		if row := state.settle(ev.Str("toolCallId"), &line, false, nil); row != nil {
			appendLine(LogPath(), *row)
		}
	}
	return out
}
