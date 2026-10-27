package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// The lookup coach: a non-blocking log of every lookup and a few reminders. The
// pi side is tool/pi_ctx_reminder.ts; both read hooks/claude/coach_cases.json.

const coachPrefix = "orch lookup coach: "

var coachText = map[string]string{
	"concept-first": `Location unknown? Ask jbcontext first: jbcontext search "<one behavior>", ` +
		"one concept per query, several queries in one message.",
	"symbol-via-lsp": "For a definition, references or callers use orch lsp def|refs|callers <Name>; rg finds text, not uses.",
	"batch-lookups":  "Send independent lookups together as parallel calls in one message; each one alone costs a turn.",
	"read-batch":     "Read the ranges you already know as parallel reads in one message.",
	"lookup-ladder": `Before searching: location unknown -> jbcontext search "<one behavior>"; known symbol -> ` +
		"orch lsp def|refs|callers|impls <Name>; exact text -> rg. " +
		"Send independent lookups as parallel calls in one message. " +
		"Read known files or ranges with Read, not cat or sed -n.",
}

const (
	queryMax       = 200
	transcriptTail = 512 * 1024
	noResult       = "No relevant results found"
)

var sessionUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// State is one session's coach state: coach/<session>.json.
type State struct {
	Pending         *string  `json:"pending"`
	WindowFirst     *string  `json:"window_first"`
	Turns           []Turn   `json:"turns"`
	FirstSearchDone bool     `json:"first_search_done"`
	ConceptSeen     bool     `json:"concept_seen"`
	Fired           []string `json:"fired"`
	Prompt          int      `json:"prompt"`
}

// Turn is one assistant message and how many lookups it made: nr non-read, rd reads.
type Turn struct {
	ID string `json:"id"`
	NR int    `json:"nr"`
	RD int    `json:"rd"`
}

func isSearch(tool string) bool { return tool == "rg" || tool == "fd" }
func isGood(tool string) bool   { return tool == "jbcontext" || strings.HasPrefix(tool, "lsp:") }

func strPtr(s string) *string { return &s }

// step advances the state for one lookup and returns the reminder that fires,
// and whether the reminder before it was followed.
func (s *State) step(tool string, symbol bool, turn *string, adjacent func(a, b, c string) bool, reminders bool) (*string, *bool) {
	var followed *bool
	if reminders && s.Pending != nil && *s.Pending != "" {
		var f bool
		switch *s.Pending {
		case "concept-first":
			f = tool == "jbcontext"
		case "lookup-ladder":
			f = isGood(tool)
		default:
			f = strings.HasPrefix(tool, "lsp:")
		}
		followed = &f
		s.Pending = nil
	}
	if s.WindowFirst == nil {
		if isSearch(tool) {
			s.WindowFirst = strPtr("bad")
		} else if isGood(tool) {
			s.WindowFirst = strPtr("good")
		}
	}
	newTurn := turn != nil && !(len(s.Turns) > 0 && s.Turns[len(s.Turns)-1].ID == *turn)
	var candidates []string
	if isSearch(tool) && !s.FirstSearchDone {
		s.FirstSearchDone = true
		if !s.ConceptSeen {
			candidates = append(candidates, "concept-first")
		}
	}
	if isGood(tool) {
		s.ConceptSeen = true
	}
	if tool == "rg" && symbol {
		candidates = append(candidates, "symbol-via-lsp")
	}
	if n := len(s.Turns); newTurn && n >= 2 && adjacent(s.Turns[n-2].ID, s.Turns[n-1].ID, *turn) {
		a, b := s.Turns[n-2], s.Turns[n-1]
		if a.NR == 1 && b.NR == 1 {
			candidates = append(candidates, "batch-lookups")
		}
		if a.RD == 1 && b.RD == 1 && a.NR == 0 && b.NR == 0 {
			candidates = append(candidates, "read-batch")
		}
	}
	var reminder *string
	if s.Fired == nil {
		s.Fired = []string{}
	}
	if reminders {
		for _, kind := range candidates {
			key := kind
			if kind == "concept-first" || kind == "symbol-via-lsp" {
				key = fmt.Sprintf("%s@%d", kind, s.Prompt)
			}
			if !contains(s.Fired, key) {
				reminder = strPtr(kind)
				s.Fired = append(s.Fired, key)
				break
			}
		}
		if reminder != nil && (*reminder == "concept-first" || *reminder == "symbol-via-lsp") {
			s.Pending = strPtr(*reminder)
		}
	}
	if turn != nil {
		if newTurn {
			s.Turns = append(s.Turns, Turn{ID: *turn})
			if len(s.Turns) > 3 {
				s.Turns = s.Turns[len(s.Turns)-3:]
			}
		}
		last := &s.Turns[len(s.Turns)-1]
		if tool == "read" {
			last.RD++
		} else {
			last.NR++
		}
	}
	return reminder, followed
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// promptStep opens a new prompt window and reports whether the lookup ladder is
// due: on the session's first prompt, and after a window whose first search was rg or fd.
func (s *State) promptStep(reminders bool) bool {
	due := reminders && (s.Prompt == 0 || (s.WindowFirst != nil && *s.WindowFirst == "bad"))
	s.Prompt++
	s.WindowFirst = nil
	s.FirstSearchDone = false
	s.ConceptSeen = false
	kept := []string{}
	for _, k := range s.Fired {
		if !strings.Contains(k, "@") || strings.HasSuffix(k, fmt.Sprintf("@%d", s.Prompt)) {
			kept = append(kept, k)
		}
	}
	s.Fired = kept
	if due {
		s.Pending = strPtr("lookup-ladder")
	}
	return due
}

// transcriptTurn is the turn id of a tool call and the ordered distinct
// assistant message ids around it, from the transcript tail.
func transcriptTurn(path, toolUseID string) (*string, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.Seek(max(0, size-transcriptTail), io.SeekStart); err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	type entry struct {
		side    bool
		id      string
		content any
	}
	var entries []entry
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if !bytes.Contains(raw, []byte(`"message"`)) {
			continue
		}
		var line map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&line) != nil {
			continue
		}
		message, ok := line["message"].(map[string]any)
		if !ok {
			continue
		}
		role, hasRole := message["role"]
		if hasRole && role != "assistant" {
			continue
		}
		id, ok := message["id"].(string)
		if !ok || id == "" {
			continue
		}
		entries = append(entries, entry{truthy(line["isSidechain"]), id, message["content"]})
	}
	var match *entry
	for i, e := range entries {
		blocks, _ := e.content.([]any)
		for _, b := range blocks {
			if block, ok := b.(map[string]any); ok && block["type"] == "tool_use" && block["id"] == toolUseID {
				match = &entries[i]
			}
		}
	}
	if match == nil {
		return nil, nil, nil
	}
	var order []string
	for _, e := range entries {
		if e.side == match.side && !contains(order, e.id) {
			order = append(order, e.id)
		}
	}
	id := match.id
	return &id, order, nil
}

func repoOf(cwd string) string {
	here, err := filepath.Abs(cwd)
	if cwd == "" || err != nil {
		here, _ = os.Getwd()
		here = filepath.Clean(here)
	}
	for probe := here; ; {
		if _, err := os.Stat(filepath.Join(probe, ".git")); err == nil {
			return probe
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return here
		}
		probe = parent
	}
}

// resultText is what the tool returned, as the log counts its bytes.
func resultText(ev *Event) string {
	raw := ev.fields["tool_response"]
	if s, ok := decodeString(raw); ok {
		return s
	}
	var obj map[string]json.RawMessage
	if raw != nil && json.Unmarshal(raw, &obj) == nil && obj != nil {
		_, hasOut := obj["stdout"]
		_, hasErr := obj["stderr"]
		if hasOut || hasErr {
			out, _ := ev.Value("tool_response")
			m := out.(map[string]any)
			text := func(key string) string {
				if v := m[key]; truthy(v) {
					return pyStr(v)
				}
				return ""
			}
			return text("stdout") + text("stderr")
		}
	}
	if raw == nil || string(bytes.TrimSpace(raw)) == "null" {
		return errorText(ev)
	}
	return pyDumpRaw(raw)
}

func errorText(ev *Event) string {
	if v, _ := ev.Value("error"); truthy(v) {
		return pyStr(v)
	}
	return ""
}

// outcome is (ok, empty) of a lookup; a failure event is an error unless it is a clean exit 1.
func outcome(ev *Event, tool, text string) (bool, bool) {
	if ev.Str("hook_event_name") == "PostToolUseFailure" {
		if rest, ok := strings.CutPrefix(errorText(ev), "Exit code 1"); ok &&
			strings.TrimSpace(strings.TrimLeftFunc(rest, unicode.IsSpace)) == "" && (isSearch(tool) || strings.HasPrefix(tool, "lsp:")) {
			return true, true
		}
		return false, false
	}
	return true, tool == "jbcontext" && strings.Contains(text, noResult)
}

func sessionName(ev *Event) (session, name string) {
	session = "unknown"
	if v, _ := ev.Value("session_id"); truthy(v) {
		session = pyStr(v)
	}
	return session, sessionUnsafe.ReplaceAllString(session, "_")
}

// lockState opens the session's state file under an exclusive flock and reads
// it. Closing the file releases the lock. A state that is not readable starts empty.
func lockState(dir, name string) (*os.File, *State, error) {
	f, err := os.OpenFile(filepath.Join(dir, "coach", name+".json"), os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, nil, err
	}
	data, _ := io.ReadAll(f)
	state := &State{}
	if json.Unmarshal(data, state) != nil {
		state = &State{}
	}
	return f, state, nil
}

func saveState(f *os.File, s *State) error {
	if s.Turns == nil {
		s.Turns = []Turn{}
	}
	if s.Fired == nil {
		s.Fired = []string{}
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt(data, 0)
	return err
}

func writeReminder(stdout io.Writer, eventName, kind string) {
	fmt.Fprintf(stdout, `{"hookSpecificOutput": {"hookEventName": %s, "additionalContext": %s}}`,
		pyQuote(eventName), pyQuote(coachPrefix+coachText[kind]))
}

func coachPrompt(ev *Event, stdout io.Writer) error {
	if os.Getenv("ORCH_LOOKUP_COACH") == "off" {
		return nil
	}
	_, name := sessionName(ev)
	dir := StateDir()
	if err := os.MkdirAll(filepath.Join(dir, "coach"), 0o777); err != nil {
		return err
	}
	f, state, err := lockState(dir, name)
	if err != nil {
		return err
	}
	due := state.promptStep(true)
	err = saveState(f, state)
	f.Close()
	if err != nil {
		return err
	}
	if due {
		writeReminder(stdout, "UserPromptSubmit", "lookup-ladder")
	}
	return nil
}

// Coach handles a PostToolUse, PostToolUseFailure or UserPromptSubmit event: it
// logs the lookup and may write a reminder as hook JSON on stdout. It never
// blocks; the caller ignores the error, so a coach bug cannot disturb a session.
func Coach(ev *Event, stdout io.Writer) error {
	if os.Getenv("ORCH_TOOL_GUARD") == "off" {
		return nil
	}
	if ev.Str("hook_event_name") == "UserPromptSubmit" {
		return coachPrompt(ev, stdout)
	}
	found, ok := lookupClaude(ev)
	if !ok {
		return nil
	}
	session, name := sessionName(ev)
	dir := StateDir()
	if err := os.MkdirAll(filepath.Join(dir, "coach"), 0o777); err != nil {
		return err
	}
	var turn *string
	var order []string
	path, pathOK := ev.fieldString("transcript_path")
	useID, useOK := ev.fieldString("tool_use_id")
	if pathOK && useOK {
		if t, o, err := transcriptTurn(path, useID); err == nil {
			turn, order = t, o
		}
	}
	position := map[string]int{}
	for i, id := range order {
		position[id] = i
	}
	adjacent := func(a, b, c string) bool {
		pa, okA := position[a]
		pb, okB := position[b]
		pc, okC := position[c]
		return okA && okB && okC && pb == pa+1 && pc == pb+1
	}
	coaching := os.Getenv("ORCH_LOOKUP_COACH") != "off"

	f, state, err := lockState(dir, name)
	if err != nil {
		return err
	}
	defer f.Close()
	reminder, followed := state.step(found.tool, found.symbol, turn, adjacent, coaching)
	if err := saveState(f, state); err != nil {
		return err
	}
	if os.Getenv("ORCH_LOOKUP_LOG") != "off" {
		text := resultText(ev)
		isOK, empty := outcome(ev, found.tool, text)
		query := found.query
		if r := []rune(query); len(r) > queryMax {
			query = string(r[:queryMax])
		}
		line := LookupLine{
			TS:       time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"),
			Harness:  "claude",
			Session:  session,
			Repo:     repoOf(ev.Str("cwd")),
			Tool:     found.tool,
			Query:    query,
			OK:       isOK,
			Empty:    empty,
			Bytes:    len(text),
			Reminder: reminder,
			Followed: followed,
		}
		if turn != nil {
			line.Turn = *turn
		}
		if v, _ := ev.Value("duration_ms"); v != nil {
			if n, ok := v.(json.Number); ok {
				if d, err := n.Float64(); err == nil {
					line.DurationMS = &d
				}
			}
		}
		if err := appendLine(filepath.Join(dir, "lookups.jsonl"), line); err != nil {
			return err
		}
	}
	if reminder != nil {
		eventName := ev.Str("hook_event_name")
		if eventName == "" {
			eventName = "PostToolUse"
		}
		writeReminder(stdout, eventName, *reminder)
	}
	return nil
}
