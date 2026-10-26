package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// In a Herdr session the gate's model passes run in panes of one tab, so the
// user can watch them. The tab is created for the run and closed at its end;
// the panes are not leads and are never recorded as such.

// gatePaneMode reports whether passes should run in Herdr panes.
func gatePaneMode() bool {
	return os.Getenv("HERDR_ENV") == "1" && os.Getenv("HERDR_WORKSPACE_ID") != ""
}

// paneOrchOnPath fails a pane run with the ctx tool when orch is not on PATH:
// pane passes call orch ctx through the pane shell's own PATH, which orch never
// edits. A dry run starts no pane.
func paneOrchOnPath(opts gateOpts, dry bool) error {
	if !opts.ctxTool || opts.headless || dry || !gatePaneMode() {
		return nil
	}
	if _, err := exec.LookPath("orch"); err != nil {
		return fmt.Errorf("orch is not on PATH, so gate panes cannot run orch ctx: link it, for example ln -s %s ~/.local/bin/orch", selfPath())
	}
	return nil
}

// gateOutsideHerdr lets a gate run outside herdr, with its passes headless.
// Only tests set it; the command refuses to run gate passes outside herdr.
var gateOutsideHerdr bool

var unsafeFileRe = regexp.MustCompile(`[^A-Za-z0-9._+-]`)

// passFile maps a pass name such as "package:api" to a safe file name.
func passFile(name string) string { return unsafeFileRe.ReplaceAllString(name, "-") }

type gatePool struct {
	tab   string
	panes []string
	once  sync.Once
}

func (p *gatePool) close() {
	p.once.Do(func() { _, _ = herdrCmd(herdrTimeout, "tab", "close", p.tab) })
}

// openGatePool creates the run's tab and up to want panes in it. Herdr splits
// a pane in half, so the grid is three columns by two rows at most: the first
// row is split right (columns of 1/2, 1/4 and 1/4 of the width), then each
// column is split down. More panes would be too narrow or short to read, which
// is why the pool stops at gateParallel panes.
func openGatePool(repo, hash string, want int) (*gatePool, error) {
	var res struct {
		Result struct {
			Tab struct {
				TabID string `json:"tab_id"`
			} `json:"tab"`
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	label := "gate-" + hash[:min(8, len(hash))]
	if err := herdrJSON(&res, herdrTimeout, "tab", "create", "--workspace", os.Getenv("HERDR_WORKSPACE_ID"), "--cwd", repo, "--label", label, "--no-focus"); err != nil {
		return nil, err
	}
	pool := &gatePool{tab: res.Result.Tab.TabID, panes: []string{res.Result.RootPane.PaneID}}
	// Entry i splits panes[from[i]] in direction dir[i].
	from := []int{0, 1, 0, 1, 2}
	dir := []string{"right", "right", "down", "down", "down"}
	for i := 0; len(pool.panes) < want && i < len(from); i++ {
		var sp struct {
			Result struct {
				Pane struct {
					PaneID string `json:"pane_id"`
				} `json:"pane"`
			} `json:"result"`
		}
		if err := herdrJSON(&sp, herdrTimeout, "pane", "split", pool.panes[from[i]], "--direction", dir[i], "--cwd", repo, "--no-focus"); err != nil {
			break // fewer panes is slower, not wrong
		}
		pool.panes = append(pool.panes, sp.Result.Pane.PaneID)
	}
	return pool, nil
}

// gatePiExtension is herdr's pi integration. A pane pass loads it explicitly
// (--no-extensions would not) so herdr reports the agent's state; a missing
// file is skipped. It is a variable so tests can point it elsewhere.
var gatePiExtension = "~/.pi/agent/extensions/herdr-agent-state.ts"

func sharedFile(gdir string, p gatePass) string {
	return filepath.Join(gdir, "passes", "shared-"+passFile(p.class)+".md")
}

func writeSharedFile(gdir string, p gatePass) error {
	if err := os.MkdirAll(filepath.Join(gdir, "passes"), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Join(gdir, "passes"), "shared-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(p.shared); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), sharedFile(gdir, p))
}

func dropSharedFiles(gdir string, passes []gatePass) {
	for _, p := range passes {
		_ = os.Remove(sharedFile(gdir, p))
	}
}

var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]`)

// agentName is a herdr agent name ([a-z][a-z0-9_-]{0,31}) for a pass, unique
// among live agents: the pane suffix tells apart passes whose names truncate alike.
func agentName(gdir, pane, pass string) string {
	hash := filepath.Base(gdir)
	sfx := nonAlnumRe.ReplaceAllString(strings.ToLower(pane), "")
	head := "gate-" + hash[:min(4, len(hash))] + "-" + slug(pass)
	if room := 31 - len(sfx); len(head) > room {
		head = strings.TrimRight(head[:room], "-")
	}
	return head + "-" + sfx
}

// agentGone reports whether herdr has no agent in pane any more.
func agentGone(pane string) bool {
	_, err := herdrCmd(herdrTimeout, "agent", "get", pane)
	return err != nil && strings.Contains(err.Error(), "agent_not_found")
}

// leavePane exits pi (ctrl+d returns the pane to its shell prompt) and waits a
// short while for herdr to forget the agent. A pane that does not come back is
// replaced by a fresh split, so the next pass never types into a stuck pane.
func leavePane(pane *string, repo string, interrupt bool) {
	if interrupt {
		_, _ = herdrCmd(herdrTimeout, "agent", "send-keys", *pane, "ctrl+c")
		time.Sleep(gateLeavePoll)
	}
	_, _ = herdrCmd(herdrTimeout, "agent", "send-keys", *pane, "ctrl+d")
	for deadline := time.Now().Add(gateLeaveWait); time.Now().Before(deadline); time.Sleep(gateLeavePoll) {
		if agentGone(*pane) {
			return
		}
	}
	var sp struct {
		Result struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if err := herdrJSON(&sp, herdrTimeout, "pane", "split", *pane, "--direction", "down", "--cwd", repo, "--no-focus"); err == nil && sp.Result.Pane.PaneID != "" {
		_, _ = herdrCmd(herdrTimeout, "pane", "close", *pane)
		*pane = sp.Result.Pane.PaneID
	}
}

// gateLeaveWait bounds the wait for a pane to return to its shell after a pass;
// gateLeavePoll is its poll step. gateStartWait bounds the wait for the agent a
// launcher started to begin its turn. Variables so tests can shorten them.
var (
	gateLeaveWait = 10 * time.Second
	gateLeavePoll = 200 * time.Millisecond
	gateStartWait = 60 * time.Second
)

// waitPaneTurn waits until the agent in pane has begun its turn: herdr reports
// it working, blocked or done, or its session already holds an assistant
// message. pi sits idle at its prompt while it loads its extensions and before
// it sends the prompt from its launch arguments, so an idle agent with no
// answer yet is still starting, not finished.
func waitPaneTurn(ctx context.Context, pane, session, transcript string) error {
	deadline := time.Now().Add(gateStartWait)
	seen := false
	for {
		out, err := herdrCmdContext(ctx, herdrTimeout, "agent", "get", pane)
		gone := err != nil && strings.Contains(err.Error(), "agent_not_found")
		switch {
		case err != nil && !gone:
			return nil // herdr failed: the pass's wait loop reports it
		case !gone:
			seen = true
			if st := agentStatus(out); st == "working" || st == "blocked" || st == "done" {
				return nil
			}
		}
		if paneAnswered(session, transcript) {
			return nil
		}
		if gone && seen {
			return fmt.Errorf("the agent left pane %s before it began its turn", pane)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			if !seen {
				return fmt.Errorf("herdr saw no agent in pane %s within %s", pane, gateStartWait)
			}
			return fmt.Errorf("the agent in pane %s stayed idle for %s and never began its turn", pane, gateStartWait)
		}
		time.Sleep(gateLeavePoll)
	}
}

// paneAnswered reports whether a pass's session already holds an assistant
// message: the claude transcript when there is one, else the newest pi session
// file under session.
func paneAnswered(session, transcript string) bool {
	path := transcript
	if path == "" {
		files, _ := filepath.Glob(filepath.Join(session, "*.jsonl"))
		if len(files) == 0 {
			return false
		}
		sort.Strings(files)
		path = files[len(files)-1]
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	answered := false
	eachPiEvent(f, func(e *piEvent) {
		if e.Message.Role == "assistant" {
			answered = true
		}
	})
	return answered
}

// runPanePass runs one pass as a real pi agent in *pane and returns its reply:
// the text of the last assistant message in pi's saved session. The shared and
// task files are the prompt. The agent is addressed by pane id; its name is
// only a best-effort label.
func runPanePass(pane *string, repo, model, gdir string, p gatePass) (reply, hit string, err error) {
	return runPaneChoice(pane, repo, profile{Harness: "pi", Model: model}, gdir, p)
}

func runPaneChoice(pane *string, repo string, choice profile, gdir string, p gatePass) (reply, hit string, err error) {
	p, err = gateAgentPass(repo, choice.Harness, p)
	if err != nil {
		return "", "", err
	}
	defer os.Remove(p.cli.Ready)
	if err = writeSharedFile(gdir, p); err != nil {
		return "", "", err
	}
	herdrCmd := func(timeout time.Duration, args ...string) ([]byte, error) {
		return herdrCmdContext(passContext(p), timeout, args...)
	}
	name := passFile(p.name)
	task := filepath.Join(gdir, "passes", name+".md")
	errF := filepath.Join(gdir, "passes", name+".err")
	launcher := filepath.Join(gdir, "passes", name+".start.sh")
	defer os.Remove(launcher)
	session := p.sessionDir
	if session == "" {
		session = filepath.Join(gdir, "sessions", name)
	}
	defer func() {
		if err != nil {
			_ = os.WriteFile(errF, []byte(err.Error()+"\n"), 0o644)
		}
	}()
	if err = os.MkdirAll(filepath.Dir(task), 0o755); err != nil {
		return "", "", err
	}
	_ = os.Remove(errF)
	if err = os.WriteFile(task, []byte(p.task), 0o600); err != nil {
		return "", "", err
	}
	_, _ = herdrCmd(herdrTimeout, "pane", "rename", *pane, p.name) // best effort: label the pane
	argv := []string{"pi", "--no-approve", "--no-extensions"}
	if f := expandHome(gatePiExtension); fileExists(f) {
		argv = append(argv, "-e", f)
	}
	argv = append(argv, piGuardArgs(p)...)
	argv = append(argv, "--no-skills", "--no-context-files")
	argv = append(argv, piPassTools(p)...)
	if choice.Effort != "" {
		argv = append(argv, "--thinking", choice.Effort)
	}
	argv = append(argv, "--session-dir", session,
		"--model", choice.Model, "@"+sharedFile(gdir, p), "@"+task)
	transcript := ""
	if choice.Harness == "claude" {
		id := newClaudeSessionID()
		transcript = claudeTranscript(repo, id)
		argv = append([]string{"claude"}, claudePassArgs(choice, p)...)
		argv = append(argv, "--session-id", id, p.shared+"\n\n"+p.task)
	}
	agent := agentName(gdir, *pane, p.name)
	if err = os.WriteFile(launcher, []byte(paneLaunchScript(p.name+": herdr agent "+agent, p.cli.Env, argv)), 0o600); err != nil {
		return "", "", err
	}
	start := time.Now()
	// The pane's shell types one short line: the long command and its
	// environment stay in the launcher (see paneLaunchScript).
	if _, err = herdrCmd(herdrTimeout, "pane", "run", *pane, "sh "+shellQuote(launcher)); err != nil {
		leavePane(pane, repo, true)
		return "", "", fmt.Errorf("%s did not start: %v", choice.Harness, err)
	}
	if serr := waitPaneTurn(passContext(p), *pane, session, transcript); serr != nil {
		leavePane(pane, repo, true)
		if passContext(p).Err() != nil {
			return "", "", passContext(p).Err()
		}
		return "", "", fmt.Errorf("%s did not start: %v", choice.Harness, serr)
	}
	_, _ = herdrCmd(herdrTimeout, "agent", "rename", *pane, agent) // best effort: name the agent
	// Wait in steps of gatePoll: between steps, a pass whose session files and
	// agent status have not changed for gateStall is stalled, and one past the
	// wall cap has timed out.
	var out []byte
	lastSeen, lastChange := "", time.Now()
	softSent := false
	for {
		if passContext(p).Err() != nil {
			leavePane(pane, repo, true)
			return "", hit, passContext(p).Err()
		}
		left := time.Until(start.Add(gateTimeout))
		if left <= 0 {
			leavePane(pane, repo, true)
			return "", hit, &passLimit{fmt.Sprintf("timed out after %s", gateTimeout)}
		}
		step := max(min(left, gatePoll), 10*time.Millisecond)
		o, werr := herdrCmd(step+herdrTimeout, "agent", "wait", *pane, "--until", "idle", "--until", "done", "--until", "blocked",
			"--timeout", strconv.FormatInt(step.Milliseconds(), 10))
		if werr == nil {
			out = o
			break
		}
		if passContext(p).Err() != nil {
			leavePane(pane, repo, true)
			return "", hit, passContext(p).Err()
		}
		got, gerr := herdrCmd(herdrTimeout, "agent", "get", *pane)
		if gerr != nil { // the agent is gone, so this was no poll timeout
			leavePane(pane, repo, true)
			return "", hit, fmt.Errorf("herdr: %v", werr)
		}
		if transcript != "" {
			copyClaudeTranscript(transcript, session)
		}
		if p.budget > 0 {
			calls, context := sessionProgress(session)
			if hard := int(float64(p.budget) * gateBudgetHard); calls >= hard {
				// Past the hard limit: interrupt, ask for the answer, and take what comes.
				hit = "hard"
				_, _ = herdrCmd(herdrTimeout, "agent", "send-keys", *pane, "ctrl+c")
				time.Sleep(gateLeavePoll)
				if p.admission != nil {
					if e := p.admission.continueWith(max(int64(len(p.prompt)), int64(context)*4)); e != nil {
						leavePane(pane, repo, true)
						return "", hit, e
					}
				}
				o, perr := herdrCmd(185*time.Second, "agent", "prompt", *pane, "Reply now with the final JSON only.", "--wait", "--timeout", "180000")
				if perr != nil {
					leavePane(pane, repo, true)
					return "", hit, fmt.Errorf("turn budget exceeded (%d turns) and no reply to the final prompt: %v", calls, perr)
				}
				out = o
				break
			} else if (calls >= p.budget || context >= gateContextSoft) && !softSent {
				softSent, hit = true, "soft"
				why := fmt.Sprintf("%d turns", calls)
				if context >= gateContextSoft {
					why = fmt.Sprintf("context %d tokens", context)
				}
				if p.admission != nil {
					if e := p.admission.continueWith(max(int64(len(p.prompt)), int64(context)*4)); e != nil {
						leavePane(pane, repo, true)
						return "", hit, e
					}
				}
				_, _ = herdrCmd(herdrTimeout, "agent", "prompt", *pane, fmt.Sprintf("Budget reached (%s). Stop exploring and reply now with the final JSON from what you have.", why))
			}
		}
		if seen := sessionStamp(session) + "|" + agentStatus(got); seen != lastSeen {
			lastSeen, lastChange = seen, time.Now()
		} else if time.Since(lastChange) >= gateStall {
			leavePane(pane, repo, true)
			return "", hit, &passLimit{"stalled: no activity for " + gateStall.String()}
		}
	}
	if agentStatus(out) == "blocked" {
		screen, _ := herdrCmd(herdrTimeout, "agent", "read", *pane, "--source", "recent-unwrapped", "--lines", "30")
		leavePane(pane, repo, true)
		return "", hit, fmt.Errorf("%s is blocked waiting for input: %s", choice.Harness, short(strings.Join(strings.Fields(string(screen)), " "), 300))
	}
	if choice.Harness == "pi" {
		if failure := ctxOnlyReady(p); failure != nil {
			leavePane(pane, repo, true)
			return "", hit, failure
		}
	}
	if transcript != "" {
		copyClaudeTranscript(transcript, session)
		reply, err = claudeSessionReply(transcript)
	} else {
		reply, err = sessionReply(session)
	}
	if failure := sessionFailure(session); failure != nil {
		err = failure
	}
	leavePane(pane, repo, false)
	return reply, hit, err
}

// agentStatus reads agent_status from a herdr agent get/wait reply.
func agentStatus(out []byte) string {
	var res struct {
		Result struct {
			Agent struct {
				Status string `json:"agent_status"`
			} `json:"agent"`
		} `json:"result"`
	}
	_ = json.Unmarshal(out, &res)
	return res.Result.Agent.Status
}

// sessionReply is the last assistant message's text in the newest session
// file under dir.
func sessionReply(dir string) (string, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) == 0 {
		return "", fmt.Errorf("pi gave no reply")
	}
	sort.Strings(files)
	f, err := os.Open(files[len(files)-1])
	if err != nil {
		return "", fmt.Errorf("pi failed: %v", err)
	}
	defer f.Close()
	return lastReply(f)
}

// dropPassFiles removes what a pass that ran leaves behind; keepSession keeps
// its session to be read afterwards.
func dropPassFiles(gdir, name string, keepSession bool) {
	f := passFile(name)
	if !keepSession {
		_ = os.RemoveAll(filepath.Join(gdir, "sessions", f))
	}
	for _, ext := range []string{".md", ".err"} {
		_ = os.Remove(filepath.Join(gdir, "passes", f+ext))
	}
}

// keptFiles names what a pass that did not run leaves for reading. Headless
// passes keep only the session; pane passes also keep their output files.
func keptFiles(gdir, name string, pane bool) string {
	f := passFile(name)
	session := filepath.Join(gdir, "sessions", f)
	if !pane {
		return "session " + session
	}
	base := filepath.Join(gdir, "passes", f)
	return base + ".md, " + base + ".err and session " + session
}

// pruneGate removes the sessions and pass files of old gate runs. Runs older
// than gateKeepDays lose them, and so does every run beyond the newest
// gateKeepRuns (the run being started counts as the newest). gate.json,
// report.md and questions/ are never touched. keep is the current run's hash.
// A run with no gate.json is still running: it is skipped until its directory
// has not changed for gateKeepDays.
func pruneGate(root, keep string, now time.Time) {
	entries, err := os.ReadDir(filepath.Join(root, "gate"))
	if err != nil {
		return
	}
	type run struct {
		dir string
		at  int64
	}
	var runs []run
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep {
			continue
		}
		dir := filepath.Join(root, "gate", e.Name())
		var rec gateRecord
		if b, err := os.ReadFile(filepath.Join(dir, "gate.json")); err == nil {
			_ = json.Unmarshal(b, &rec)
		} else if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) <= gateKeepDays*24*time.Hour {
			continue
		}
		runs = append(runs, run{dir, rec.At})
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].at > runs[j].at })
	for i, r := range runs {
		if i >= gateKeepRuns-1 || now.Sub(time.Unix(r.at, 0)) > gateKeepDays*24*time.Hour {
			_ = os.RemoveAll(filepath.Join(r.dir, "sessions"))
			_ = os.RemoveAll(filepath.Join(r.dir, "passes"))
		}
	}
}
