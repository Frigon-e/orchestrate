package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// When the backend has the layout capability, the gate's model passes run in
// panes of one tab, so the user can watch them. The tab is created for the run
// and closed at its end; the panes are not leads and are never recorded as
// such. Without layout, each pass is its own backend Start and is closed when
// the pass ends, with no pool placement.

// panePilot is what the pooled path needs beyond the required operations: type
// a command into a pane's shell, name the agent in it, send it a key and close
// the pool's tab. A backend with layout that does not implement it gets the
// separate-starts path.
type panePilot interface {
	RunInPane(ctx context.Context, h Handle, command string) error
	RenameAgent(ctx context.Context, h Handle, name string) error
	SendKeys(ctx context.Context, h Handle, key string) error
	CloseTab(ctx context.Context, tab Handle) error
}

// gateBackendFn selects the backend a gate run uses. Tests replace it.
var gateBackendFn = useBackend

// gateMux is the backend a gate run talks to.
type gateMux struct {
	b     *Active
	self  Identity
	lay   Layout    // nil without layout
	pilot panePilot // nil without layout
}

// pooled says passes run in placed panes of one tab.
func (m *gateMux) pooled() bool { return m.lay != nil && m.pilot != nil }

func openGateMux() (*gateMux, error) {
	b, err := gateBackendFn()
	if err != nil {
		return nil, err
	}
	self, err := b.Self(context.Background())
	if err != nil {
		return nil, err
	}
	m := &gateMux{b: b, self: self}
	if b.Has(CapLayout) {
		lay, _, ok := capabilityAs[Layout](b, CapLayout)
		pilot, isPilot := b.Backend.(panePilot)
		if ok && isPilot {
			if self.Workspace == "" {
				return nil, fmt.Errorf("the backend reported no workspace for the gate tab")
			}
			m.lay, m.pilot = lay, pilot
		}
	}
	return m, nil
}

// gatePaneMode reports whether passes should run through a backend.
func gatePaneMode() bool {
	_, err := openGateMux()
	return err == nil
}

// paneOrchOnPath fails a pane run with the ctx tool when orch is not on PATH:
// pane passes call orch lsp through the pane shell's own PATH, which orch never
// edits. A dry run starts no pane.
func paneOrchOnPath(opts gateOpts, dry bool) error {
	if !opts.lookupTool || opts.headless || dry || !gatePaneMode() {
		return nil
	}
	if _, err := exec.LookPath("orch"); err != nil {
		return fmt.Errorf("orch is not on PATH, so gate panes cannot run orch lsp: link it, for example ln -s %s ~/.local/bin/orch", selfPath())
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
	mux   *gateMux
	tab   Handle
	panes []string
	once  sync.Once
}

func (p *gatePool) close() {
	p.once.Do(func() {
		if p.mux.pooled() {
			_ = p.mux.pilot.CloseTab(context.Background(), p.tab)
		}
	})
}

// openGatePool creates the run's tab and up to want panes in it. A pane is
// split in half, so the grid is three columns by two rows at most: the first
// row is split right (columns of 1/2, 1/4 and 1/4 of the width), then each
// column is split down. More panes would be too narrow or short to read, which
// is why the pool stops at gateParallel panes. Without layout there is no tab:
// the pool holds want slots, and each pass starts and closes its own agent.
func openGatePool(repo, hash string, want int) (*gatePool, error) {
	m, err := openGateMux()
	if err != nil {
		return nil, err
	}
	if !m.pooled() {
		pool := &gatePool{mux: m}
		for i := range max(want, 1) {
			pool.panes = append(pool.panes, fmt.Sprintf("slot-%d", i+1))
		}
		return pool, nil
	}
	ctx := context.Background()
	label := "gate-" + hash[:min(8, len(hash))]
	tab, err := m.lay.NewTab(ctx, label, Spec{Cwd: repo, Workspace: m.self.Workspace})
	if err != nil {
		return nil, err
	}
	pool := &gatePool{mux: m, tab: tab.Tab, panes: []string{string(tab.Handle)}}
	// Entry i splits panes[from[i]] in direction dir[i].
	from := []int{0, 1, 0, 1, 2}
	dir := []string{"right", "right", "down", "down", "down"}
	for i := 0; len(pool.panes) < want && i < len(from); i++ {
		sp, err := m.lay.Split(ctx, Handle(pool.panes[from[i]]), Spec{Cwd: repo, Direction: dir[i]})
		if err != nil {
			break // fewer panes is slower, not wrong
		}
		pool.panes = append(pool.panes, string(sp.Handle))
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

// agentGone reports whether the backend has no agent in pane any more.
func (m *gateMux) agentGone(pane string) bool {
	_, err := m.b.Status(context.Background(), Handle(pane))
	return errors.Is(err, ErrGone)
}

// leavePane exits the agent (ctrl+d returns the pane to its shell prompt) and
// waits a short while for the backend to forget it. A pane that does not come
// back is replaced by a fresh split, so the next pass never types into a stuck
// pane. Without layout the agent is closed instead.
func (m *gateMux) leavePane(pane *string, repo string, interrupt bool) {
	ctx := context.Background()
	if !m.pooled() {
		if *pane != "" {
			_ = m.b.Close(ctx, Handle(*pane))
		}
		return
	}
	if interrupt {
		_ = m.pilot.SendKeys(ctx, Handle(*pane), "ctrl+c")
		time.Sleep(gateLeavePoll)
	}
	_ = m.pilot.SendKeys(ctx, Handle(*pane), "ctrl+d")
	for deadline := time.Now().Add(gateLeaveWait); time.Now().Before(deadline); time.Sleep(gateLeavePoll) {
		if m.agentGone(*pane) {
			return
		}
	}
	if sp, err := m.lay.Split(ctx, Handle(*pane), Spec{Cwd: repo, Direction: "down"}); err == nil && sp.Handle != "" {
		_ = m.b.Close(ctx, Handle(*pane))
		*pane = string(sp.Handle)
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
func waitPaneTurn(ctx context.Context, pane, session, transcript string, old map[string]bool) error {
	b, err := gateBackendFn()
	if err != nil {
		return nil // the backend failed: the pass's wait loop reports it
	}
	return (&gateMux{b: b}).waitPaneTurn(ctx, pane, session, transcript, old)
}

func (m *gateMux) waitPaneTurn(ctx context.Context, pane, session, transcript string, old map[string]bool) error {
	deadline := time.Now().Add(gateStartWait)
	seen := false
	for {
		st, err := m.b.Status(ctx, Handle(pane))
		gone := errors.Is(err, ErrGone)
		switch {
		case err != nil && !gone:
			return nil // the backend failed: the pass's wait loop reports it
		case !gone:
			seen = true
			if st.State == StateWorking || st.State == StateBlocked || st.State == StateDone {
				return nil
			}
		}
		if paneAnswered(session, transcript, old) {
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
// file under session that is not in old (files a previous run left behind).
func paneAnswered(session, transcript string, old map[string]bool) bool {
	path := transcript
	if path == "" {
		var files []string
		all, _ := filepath.Glob(filepath.Join(session, "*.jsonl"))
		for _, f := range all {
			if !old[filepath.Base(f)] {
				files = append(files, f)
			}
		}
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

// sessionFiles names the *.jsonl files already in session.
func sessionFiles(session string) map[string]bool {
	old := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(session, "*.jsonl"))
	for _, f := range files {
		old[filepath.Base(f)] = true
	}
	return old
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
	m, err := openGateMux()
	if err != nil {
		return "", "", err
	}
	ctx := passContext(p)
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
	// cur is where the agent runs: the pool's pane, or the agent a separate
	// Start made. Without layout the slot in *pane is only a name seed and is
	// never changed; the started agent is closed when the pass leaves it.
	cur := *pane
	leave := func(interrupt bool) {
		if !m.pooled() {
			m.leavePane(&cur, repo, interrupt)
			return
		}
		m.leavePane(&cur, repo, interrupt)
		*pane = cur
	}
	start := time.Now()
	old := sessionFiles(session)
	if m.pooled() {
		_ = m.b.Rename(ctx, Handle(cur), p.name) // best effort: label the pane
		if err = os.WriteFile(launcher, []byte(paneLaunchScript(p.name+": herdr agent "+agent, p.cli.Env, argv)), 0o600); err != nil {
			return "", "", err
		}
		// The pane's shell types one short line: the long command and its
		// environment stay in the launcher (see paneLaunchScript).
		if err = m.pilot.RunInPane(ctx, Handle(cur), "sh "+shellQuote(launcher)); err != nil {
			leave(true)
			return "", "", fmt.Errorf("%s did not start: %v", choice.Harness, err)
		}
	} else {
		env := map[string]string{}
		for _, kv := range p.cli.Env {
			if k, v, _ := strings.Cut(kv, "="); k != "" && k != "PATH" {
				env[k] = v
			}
		}
		id, serr := m.b.Start(ctx, Spec{Agent: argv[0], Harness: argv[0], Cwd: repo, Name: agent, Env: env, Args: argv[1:]})
		cur = string(id.Handle) // a failed Start may still have made an agent
		if serr != nil {
			leave(true)
			return "", "", fmt.Errorf("%s did not start: %v", choice.Harness, serr)
		}
	}
	defer func() {
		if !m.pooled() {
			leave(false) // a no-op once the pass has left the agent
		}
	}()
	if serr := m.waitPaneTurn(ctx, cur, session, transcript, old); serr != nil {
		leave(true)
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return "", "", fmt.Errorf("%s did not start: %v", choice.Harness, serr)
	}
	if m.pooled() {
		_ = m.pilot.RenameAgent(ctx, Handle(cur), agent) // best effort: name the agent
	}
	// Wait in steps of gatePoll: between steps, a pass whose session files and
	// agent status have not changed for gateStall is stalled, and one past the
	// wall cap has timed out.
	var final AgentStatus
	lastSeen, lastChange := "", time.Now()
	softSent := false
	for {
		if ctx.Err() != nil {
			leave(true)
			return "", hit, ctx.Err()
		}
		left := time.Until(start.Add(gateTimeout))
		if left <= 0 {
			leave(true)
			return "", hit, &passLimit{fmt.Sprintf("timed out after %s", gateTimeout)}
		}
		step := max(min(left, gatePoll), 10*time.Millisecond)
		st, werr := m.b.Wait(ctx, Handle(cur), []State{StateIdle, StateDone, StateBlocked}, step)
		if werr == nil {
			final = st
			break
		}
		if ctx.Err() != nil {
			leave(true)
			return "", hit, ctx.Err()
		}
		got, gerr := m.b.Status(ctx, Handle(cur))
		if gerr != nil { // the agent is gone, so this was no poll timeout
			leave(true)
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
				m.interrupt(ctx, cur)
				if p.admission != nil {
					if e := p.admission.continueWith(max(int64(len(p.prompt)), int64(context)*4)); e != nil {
						leave(true)
						return "", hit, e
					}
				}
				fin, perr := m.promptAndWait(ctx, cur, "Reply now with the final JSON only.", 180*time.Second)
				if perr != nil {
					leave(true)
					return "", hit, fmt.Errorf("turn budget exceeded (%d turns) and no reply to the final prompt: %v", calls, perr)
				}
				final = fin
				break
			} else if (calls >= p.budget || context >= gateContextSoft) && !softSent {
				softSent, hit = true, "soft"
				why := fmt.Sprintf("%d turns", calls)
				if context >= gateContextSoft {
					why = fmt.Sprintf("context %d tokens", context)
				}
				if p.admission != nil {
					if e := p.admission.continueWith(max(int64(len(p.prompt)), int64(context)*4)); e != nil {
						leave(true)
						return "", hit, e
					}
				}
				_ = m.b.Prompt(ctx, Handle(cur), fmt.Sprintf("Budget reached (%s). Stop exploring and reply now with the final JSON from what you have.", why))
			}
		}
		if seen := sessionStamp(session) + "|" + string(got.State); seen != lastSeen {
			lastSeen, lastChange = seen, time.Now()
		} else if time.Since(lastChange) >= gateStall {
			leave(true)
			return "", hit, &passLimit{"stalled: no activity for " + gateStall.String()}
		}
	}
	if final.State == StateBlocked {
		screen, _ := m.b.Read(ctx, Handle(cur), 30)
		leave(true)
		return "", hit, fmt.Errorf("%s is blocked waiting for input: %s", choice.Harness, short(strings.Join(strings.Fields(screen), " "), 300))
	}
	if choice.Harness == "pi" {
		if failure := restrictedShellReady(p); failure != nil {
			leave(true)
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
	leave(false)
	return reply, hit, err
}

// interrupt sends ctrl+c where the backend can type keys; elsewhere the
// agent keeps running until it is prompted or closed.
func (m *gateMux) interrupt(ctx context.Context, pane string) {
	if m.pooled() {
		_ = m.pilot.SendKeys(ctx, Handle(pane), "ctrl+c")
		time.Sleep(gateLeavePoll)
	}
}

// promptAndWait sends text and returns the state the agent settles in.
func (m *gateMux) promptAndWait(ctx context.Context, pane, text string, timeout time.Duration) (AgentStatus, error) {
	h := Handle(pane)
	if pw, ok := m.b.Backend.(PromptWaiter); ok {
		if err := pw.PromptWait(ctx, h, text, timeout); err != nil {
			return AgentStatus{}, err
		}
		st, err := m.b.Status(ctx, h)
		if err != nil {
			return AgentStatus{State: StateUnknown}, nil
		}
		return st, nil
	}
	if err := m.b.Prompt(ctx, h, text); err != nil {
		return AgentStatus{}, err
	}
	return m.b.Wait(ctx, h, []State{StateIdle, StateDone, StateBlocked}, timeout)
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
