package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Hooks are guardrails for agents that mean well, not a sandbox. They act only
// for a pane marked by `orch become` (or ORCH_ROLE=orchestrator) and for leads
// (ORCH_ROLE=lead); everywhere else they exit 0 at once. A hook never fails a
// session: errors are printed and the tool call goes ahead.

var (
	scoutAgents   = []string{"Explore", "claude-code-guide"}
	gitMutationRe = regexp.MustCompile(`\bgit\s+(?:-C\s+\S+\s+)?(?:commit|merge|push|rebase|reset|cherry-pick|am|apply|stash|checkout\s+-[bB]|switch\s+-[cC]|worktree\s+add|branch\s+-[dDmM])\b|\bgh\s+pr\s+(?:merge|create)\b`)
)

type hookPayload struct {
	SessionID      string         `json:"session_id"`
	Cwd            string         `json:"cwd"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	StopHookActive bool           `json:"stop_hook_active"`
}

func cmdHook(a *app, args []string) (err error) {
	if len(args) != 1 || !slices.Contains([]string{"pretool", "stop", "session-start"}, args[0]) {
		return &exitError{code: 2, msg: "usage: orch hook pretool|stop|session-start"}
	}
	event := args[0]
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(a.errw, "orch hook %s: %v\n", event, r)
			err = nil
		}
	}()
	raw, _ := io.ReadAll(a.stdin)
	var p hookPayload
	if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &p) != nil {
		return nil
	}
	h := *a
	if p.Cwd != "" {
		h.cwd = p.Cwd
	}
	var block string
	var herr error
	switch {
	case orchestratorActive(p):
		block, herr = h.orchestratorHook(event, p)
	case os.Getenv("ORCH_ROLE") == "lead" && os.Getenv("ORCH_TASK_DIR") != "":
		block, herr = h.leadHook(event, p)
	}
	if herr != nil {
		fmt.Fprintf(a.errw, "orch hook %s: %v\n", event, herr)
		return nil
	}
	if block != "" {
		return &exitError{code: 2, msg: block}
	}
	return nil
}

func orchestratorActive(p hookPayload) bool {
	switch os.Getenv("ORCH_ROLE") {
	case "orchestrator":
		return true
	case "lead":
		return false
	}
	pane := os.Getenv("HERDR_PANE_ID")
	if pane == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(rolesDir(), pane))
	if err != nil {
		return false
	}
	var m struct {
		Session string `json:"session"`
	}
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	// A marker from an earlier session in a reused pane does not apply.
	return m.Session == "" || p.SessionID == "" || m.Session == p.SessionID
}

func (a *app) orchestratorHook(event string, p hookPayload) (string, error) {
	if event == "pretool" {
		return pretoolDenial(p, a.cwd), nil
	}
	s, err := a.site()
	if err != nil {
		return "", err
	}
	me := orchID()
	leads := myLeads(openLeads(s.root), me)
	switch event {
	case "stop":
		if p.StopHookActive || len(leads) == 0 || watcherAlive(s.root, me) != 0 {
			return "", nil
		}
		return fmt.Sprintf("%d lead(s) in flight and no watcher is running, so nothing will wake you. "+
			"Start `%s watch` with the Monitor tool.",
			len(leads), selfPath()), nil
	case "session-start":
		fmt.Fprintln(a.out, "You are the orchestrator for this repo: load the `orchestrate` skill before acting.")
		fmt.Fprintln(a.out, a.board(s, leadRows(s.root, false)))
		if len(leads) > 0 && watcherAlive(s.root, me) == 0 {
			fmt.Fprintf(a.out, "Watcher stopped: start `%s watch` with the Monitor tool.\n", selfPath())
		}
	}
	return "", nil
}

func pretoolDenial(p hookPayload, cwd string) string {
	str := func(k string) string {
		s, _ := p.ToolInput[k].(string)
		return s
	}
	switch p.ToolName {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		path := str("file_path")
		if path == "" {
			path = str("notebook_path")
		}
		if path == "" {
			return ""
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if underRunRoot(path, cwd) || !insideGitWorktree(path) {
			return ""
		}
		return fmt.Sprintf("orchestrator role: %s is in a repository. Project changes belong to a lead: "+
			"write a task with `orch new`, then `orch spawn`.", path)
	case "Agent", "Task":
		kind := str("subagent_type")
		if kind == "" {
			kind = "general-purpose"
		}
		if slices.Contains(scoutAgents, kind) {
			return ""
		}
		return fmt.Sprintf("orchestrator role: only read-only scouts (%s) run in-process; `%s` work goes to a lead "+
			"via `orch new` + `orch spawn`.", strings.Join(scoutAgents, ", "), kind)
	case "Workflow":
		return "orchestrator role: fan-out goes through leads (`orch spawn`), not in-process workflows."
	case "Bash":
		if gitMutationRe.MatchString(str("command")) {
			return "orchestrator role: git and PR changes belong to the lead that owns the branch; " +
				"send it an instruction with `orch send`."
		}
	}
	return ""
}

func underRunRoot(path, cwd string) bool {
	repo, err := repoRoot(cwd)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(runRoot(repo), path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func insideGitWorktree(path string) bool {
	probe := path
	for {
		if st, err := os.Stat(probe); err == nil && st.IsDir() {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return false
		}
		probe = parent
	}
	out, _ := git(probe, "rev-parse", "--is-inside-work-tree")
	return out == "true"
}

func (a *app) leadHook(event string, p hookPayload) (string, error) {
	dir, err := leadDir("")
	if err != nil {
		return "", err
	}
	me := selfPath()
	switch event {
	case "stop":
		return a.leadStop(dir, p), nil
	case "session-start":
		fmt.Fprintf(a.out, "You are orchestrate lead %q. Re-read %s before continuing; report with `%s report --dir %s`.\n",
			filepath.Base(dir), filepath.Join(dir, "task.md"), me, dir)
		if n := len(pendingMessages(dir)); n > 0 {
			fmt.Fprintf(a.out, "%d unread message(s): run `%s inbox --dir %s`.\n", n, me, dir)
		}
	}
	return "", nil
}

// leadStop makes sure no lead turn ends blind: a turn that wrote no status
// line is sent back once, unless the lead is already waiting on the
// orchestrator with nothing new to read.
func (a *app) leadStop(dir string, p hookPayload) string {
	entries := readStatus(dir)
	mine := leadEntries(entries)
	mark := filepath.Join(dir, ".stop-mark")
	prev := 0
	if b, err := os.ReadFile(mark); err == nil {
		prev, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	_ = os.WriteFile(mark, []byte(strconv.Itoa(len(mine))), 0o644)
	if p.StopHookActive {
		return ""
	}
	me := selfPath()
	if len(mine) > prev {
		return a.unreadOnce(dir, me)
	}
	answered := false
	for i := len(entries) - 1; i >= 0 && entries[i].By == "orch"; i-- {
		answered = answered || entries[i].State == "resolved"
	}
	unread := len(pendingMessages(dir))
	if waitingOnOrch[lastState(mine)] && unread == 0 && !answered {
		return ""
	}
	msg := fmt.Sprintf("orchestrate lead: this turn wrote no status line, so the orchestrator cannot tell why you stopped. "+
		"Run `%s report --dir %s <state> \"<one line>\"` first: working (say what you are waiting on), done, "+
		"needs-decision, blocked, paused or failed.", me, dir)
	if unread > 0 {
		msg += fmt.Sprintf(" You also have %d unread message(s): run `%s inbox --dir %s`.", unread, me, dir)
		markUnread(dir)
	}
	return msg
}

// markUnread records the pending messages as mentioned, in .stop-unread, and
// reports whether any of them was new.
func markUnread(dir string) (n int, fresh bool) {
	mark := filepath.Join(dir, ".stop-unread")
	told := map[string]bool{}
	if b, err := os.ReadFile(mark); err == nil {
		for _, name := range strings.Fields(string(b)) {
			told[name] = true
		}
	}
	var names []string
	for _, p := range pendingMessages(dir) {
		name := filepath.Base(p)
		fresh = fresh || !told[name]
		names = append(names, name)
	}
	_ = os.WriteFile(mark, []byte(strings.Join(names, "\n")), 0o644)
	return len(names), fresh
}

// unreadOnce is the block message for a turn that did write a status line: it
// names unread mail, but only mail no earlier Stop hook has mentioned. Pi
// leads have no Stop guard, so only Claude leads reach it.
func (a *app) unreadOnce(dir, me string) string {
	n, fresh := markUnread(dir)
	if !fresh {
		return ""
	}
	return fmt.Sprintf("orchestrate lead: you have %d unread message(s): run `%s inbox --dir %s`.", n, me, dir)
}
