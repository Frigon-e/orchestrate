package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func leadDir(dir string) (string, error) {
	if dir == "" {
		dir = os.Getenv("ORCH_TASK_DIR")
	}
	if dir == "" {
		return "", fail("pass --dir <task dir> (or set ORCH_TASK_DIR)")
	}
	dir = expandHome(dir)
	if _, err := os.Stat(filepath.Join(dir, "task.md")); err != nil {
		return "", fail("%s is not a task directory", dir)
	}
	return dir, nil
}

func cmdReport(a *app, args []string) error {
	fs := a.flags("report")
	dirFlag := fs.String("dir", "", "task directory (default: $ORCH_TASK_DIR)")
	key := fs.String("key", "", "slug that a later `resolved` line closes")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return &exitError{code: 2, msg: "usage: orch report [--dir D] [--key K] <state> <text...>"}
	}
	dir, err := leadDir(*dirFlag)
	if err != nil {
		return err
	}
	if epoch := os.Getenv("ORCH_EPOCH"); epoch != "" {
		if l, err := loadLead(dir); err == nil && l != nil && l.Epoch != "" && l.Epoch != epoch {
			return fail("this lead session was replaced by a respawn; stop working")
		}
	}
	state, text := pos[0], strings.TrimSpace(strings.Join(pos[1:], " "))
	if !slices.Contains(leadStates, state) {
		return fail("state must be one of: %s", strings.Join(leadStates, ", "))
	}
	if text == "" {
		return fail("a report needs a one-line text")
	}
	if *key != "" && !keyRe.MatchString(*key) {
		return fail("--key must be a short lowercase slug")
	}
	if *key == contextKey && state != "needs-decision" {
		return fail("--key %s is the full-context handoff and must be reported as needs-decision (lead protocol, \"Context budget\")", contextKey)
	}
	if (state == "question" || state == "escalate") && *key == "" {
		return fail("%s needs --key <slug>, the name of its card at questions/<key>.md", state)
	}
	if state == "escalate" {
		if err := checkEscalateCard(dir, *key); err != nil {
			return err
		}
	}
	if err := a.appendStatus(dir, state, text, *key, ""); err != nil {
		return err
	}
	fmt.Fprintln(a.out, "reported "+state)
	if state == "question" {
		fmt.Fprintln(a.out, "queued: the orchestrator is not woken. Keep working; the user answers when they read the queue. If this stops all your work, report blocked instead")
	}
	if state == "paused" || state == "note" {
		// These are logged but never wake the orchestrator; say so, so a lead
		// that needs an answer does not wait on one. working is routine and
		// stays quiet.
		fmt.Fprintln(a.out, "the orchestrator is not woken by "+state+"; use needs-decision, blocked, done or failed to reach it")
	}
	return nil
}

// checkEscalateCard refuses an escalation whose card does not carry the
// evidence a stronger model needs: what the lead saw, what it tried, and
// what it cannot do without help. The card asks a question and never
// proposes a fix.
func checkEscalateCard(dir, key string) error {
	path := filepath.Join(dir, "questions", key+".md")
	src, err := os.ReadFile(path)
	if err != nil {
		return fail("escalate needs a card at %s with a `question:` front matter line and the sections `## Context` (what you observed, with command output), `## Tried` and `## Blocked`. Do not propose a solution", path)
	}
	c, err := parseCard(string(src))
	if err != nil {
		return fail("escalate card %s: %v", path, err)
	}
	have := map[string]string{"Context": c.context}
	for _, x := range c.extra {
		have[x.title] = x.body
	}
	for _, name := range []string{"Context", "Tried", "Blocked"} {
		if strings.TrimSpace(have[name]) == "" {
			return fail("escalate card %s needs a non-empty `## %s` section", path, name)
		}
	}
	return nil
}

func cmdInbox(a *app, args []string) error {
	fs := a.flags("inbox")
	dirFlag := fs.String("dir", "", "task directory (default: $ORCH_TASK_DIR)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	dir, err := leadDir(*dirFlag)
	if err != nil {
		return err
	}
	msgs := pendingMessages(dir)
	if len(msgs) == 0 {
		fmt.Fprintln(a.out, "no new messages")
		return nil
	}
	handled := filepath.Join(dir, "inbox", "handled")
	if err := os.MkdirAll(handled, 0o755); err != nil {
		return err
	}
	for _, p := range msgs {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "--- message %s ---\n%s\n", strings.TrimSuffix(filepath.Base(p), ".msg"), strings.TrimRight(string(b), "\n"))
		if err := os.Rename(p, filepath.Join(handled, filepath.Base(p))); err != nil {
			return err
		}
	}
	// New work arrives here, so this is where a lead learns its context is
	// too full to take on something unrelated.
	if l, _ := loadLead(dir); l != nil {
		if agents, err := agentMap(); err == nil {
			ag := agents[l.PaneID]
			uc, _ := loadUserConfig()
			if n := contextTokens(ag.Agent, sessionOf(ag, l)); n >= uc.LeadContextLimit {
				fmt.Fprintf(a.out, "context: %s tokens, over the %s lead limit. If these messages start work unrelated to what you hold, follow \"Context budget\" in the lead protocol before starting it.\n", kTokens(n), kTokens(uc.LeadContextLimit))
			}
		}
	}
	return nil
}

// sessionOf is the harness session of a lead's agent: the one Herdr reports,
// else the one orch chose at spawn.
func sessionOf(ag herdrAgent, l *lead) string {
	if ag.Session.Value != "" || l == nil {
		return ag.Session.Value
	}
	return l.Session
}

// newSessionID returns a random UUID for harnesses that take a session id.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// sessionPath finds a harness session's transcript file; "" when there is none.
func sessionPath(harness, session string) string {
	if session == "" || strings.ContainsAny(session, `/\*?[`) {
		return ""
	}
	var pattern string
	switch harness {
	case "claude":
		pattern = filepath.Join(envOr("CLAUDE_CONFIG_DIR", expandHome("~/.claude")), "projects", "*", session+".jsonl")
	case "pi":
		pattern = filepath.Join(envOr("ORCH_PI_DIR", expandHome("~/.pi/agent")), "sessions", "*", "*_"+session+".jsonl")
	default:
		return ""
	}
	paths, _ := filepath.Glob(pattern)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

// contextTokens is how many tokens an agent session held at its latest turn,
// read from the tail of its harness transcript; 0 when that can't be found.
func contextTokens(harness, session string) int {
	path := sessionPath(harness, session)
	if path == "" {
		return 0
	}
	lines := bytes.Split(readTail(path, 4<<20), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if n := turnTokens(lines[i]); n > 0 {
			return n
		}
	}
	return 0
}

// turnTokens reads the context size from one transcript line: a Claude
// assistant message's usage or compaction marker, or a pi assistant
// message's usage.
func turnTokens(line []byte) int {
	if !bytes.Contains(line, []byte(`"usage"`)) && !bytes.Contains(line, []byte(`"compact_boundary"`)) {
		return 0
	}
	var e struct {
		IsSidechain bool   `json:"isSidechain"`
		Subtype     string `json:"subtype"`
		Compact     struct {
			Post int `json:"postTokens"`
		} `json:"compactMetadata"`
		Message struct {
			Usage struct {
				Input       int `json:"input_tokens"`
				CacheRead   int `json:"cache_read_input_tokens"`
				CacheCreate int `json:"cache_creation_input_tokens"`
				// pi names its usage fields without the _tokens suffix.
				PiInput int `json:"input"`
				PiRead  int `json:"cacheRead"`
				PiWrite int `json:"cacheWrite"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.IsSidechain {
		return 0
	}
	if e.Subtype == "compact_boundary" {
		return e.Compact.Post
	}
	u := e.Message.Usage
	return u.Input + u.CacheRead + u.CacheCreate + u.PiInput + u.PiRead + u.PiWrite
}

// readTail returns up to the last n bytes of a file.
func readTail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > n {
		_, _ = f.Seek(st.Size()-n, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	return b
}

func kTokens(n int) string { return fmt.Sprintf("%dK", (n+500)/1000) }
