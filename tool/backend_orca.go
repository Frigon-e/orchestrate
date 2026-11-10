package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Orca backend: every orca command and JSON shape lives in this file.
// ORCH_ORCA names the binary (default orca). It wraps `orca ... --json` and never
// speaks Orca's socket.

const orcaTimeout = 60 * time.Second

// orcaPoll is how often Wait re-reads the agent's status.
var orcaPoll = 2 * time.Second

func init() {
	registerBackend("orca", "ORCA_", func(getenv func(string) string) bool {
		return getenv("ORCA_AGENT_SESSION_ID") != "" || getenv("ORCA_PANE_KEY") != "" || getenv("ORCA_TERMINAL_HANDLE") != ""
	}, func() (Backend, error) { return newOrcaBackend(), nil })
}

// orcaError is Orca's {"ok":false,"error":{...}} reply.
type orcaError struct {
	What    string
	Code    string
	Message string
	Data    map[string]any
}

func (e *orcaError) Error() string {
	return fmt.Sprintf("orca %s failed: %s: %s", e.What, e.Code, e.Message)
}

func (e *orcaError) exitCode() int {
	switch v := e.Data["exitCode"].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return -1
}

func asOrcaError(err error) *orcaError {
	oe, _ := errors.AsType[*orcaError](err)
	return oe
}

func orcaCode(err error) string {
	if oe := asOrcaError(err); oe != nil {
		return oe.Code
	}
	return ""
}

func orcaStale(err error) bool { return orcaCode(err) == "terminal_handle_stale" }

// orcaGone says Orca no longer knows the terminal or worktree.
func orcaGone(err error) bool {
	c := orcaCode(err)
	return strings.Contains(c, "not_found") || c == "terminal_gone"
}

// orcaRun runs one orca command with --json and returns the reply's result.
func orcaRun(ctx context.Context, timeout time.Duration, args ...string) (json.RawMessage, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	what := strings.Join(args[:min(2, len(args))], " ")
	cmd := exec.CommandContext(cctx, envOr("ORCH_ORCA", "orca"), append(slices.Clone(args), "--json")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if cctx.Err() != nil {
		return nil, fail("orca %s timed out after %s", what, timeout)
	}
	var env struct {
		OK     *bool           `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Data    map[string]any `json:"data"`
		} `json:"error"`
	}
	parsed := json.Unmarshal(bytes.TrimSpace(out), &env) == nil && (env.OK != nil || env.Error != nil)
	switch {
	case parsed && env.Error != nil:
		return nil, &orcaError{What: what, Code: env.Error.Code, Message: env.Error.Message, Data: env.Error.Data}
	case parsed && env.OK != nil && !*env.OK:
		return nil, &orcaError{What: what, Code: "unknown", Message: "reply says not ok"}
	case runErr != nil:
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return nil, fail("orca %s failed: %s", what, msg)
	case !parsed:
		return nil, fail("orca %s: unexpected output", what)
	}
	return env.Result, nil
}

// identity files: a launch command has no --env, so who an agent is lives in a
// file keyed by its terminal handle, which the agent's own orch calls read.

type orcaIdentityRecord struct {
	ID      string            `json:"id"`
	Handle  string            `json:"handle"`
	PaneKey string            `json:"pane_key,omitempty"`
	Backend string            `json:"backend"`
	Name    string            `json:"name,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func orcaIdentityDir() string {
	return expandHome(envOr("ORCH_IDENTITY_DIR", "~/.local/state/orchestrate/identities"))
}

func orcaIdentityPath(handle string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, handle)
	return filepath.Join(orcaIdentityDir(), safe+".json")
}

func readOrcaIdentity(handle string) (orcaIdentityRecord, bool) {
	var rec orcaIdentityRecord
	if handle == "" {
		return rec, false
	}
	b, err := os.ReadFile(orcaIdentityPath(handle))
	if err != nil || json.Unmarshal(b, &rec) != nil {
		return rec, false
	}
	return rec, true
}

func writeOrcaIdentity(rec orcaIdentityRecord) error {
	if err := os.MkdirAll(orcaIdentityDir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := orcaIdentityPath(rec.Handle)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// orca JSON shapes

// orcaTerminal is one `terminal list` row. Every field is optional: Orca's real
// rows carry handle, title, worktreeId, worktreePath and tabId, and no paneKey
// or status. A missing field is the empty string, never an error.
type orcaTerminal struct {
	Handle       string `json:"handle"`
	PaneKey      string `json:"paneKey"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	WorktreePath string `json:"worktreePath"`
}

// orcaAgentRow is one entry of a worktree's `agents` array in `worktree ps`.
// AgentWait is raw so absent (not looked at) differs from null (looked, found
// nothing).
type orcaAgentRow struct {
	State         string          `json:"state"`
	AgentType     string          `json:"agentType"`
	PaneKey       string          `json:"paneKey"`
	ParentPaneKey string          `json:"parentPaneKey"`
	Handle        string          `json:"terminalHandle"`
	Title         string          `json:"title"`
	SessionID     string          `json:"sessionId"`
	BlockedReason string          `json:"blockedReason"`
	ToolName      string          `json:"toolName"`
	AgentWait     json.RawMessage `json:"agentWait"`

	worktreePath string
}

type orcaWorktreeRow struct {
	ID string `json:"id"`
	// WorktreeID is the key `worktree ps` really uses; `worktree create` says id.
	WorktreeID string         `json:"worktreeId"`
	Path       string         `json:"path"`
	Branch     string         `json:"branch"`
	Agents     []orcaAgentRow `json:"agents"`
}

func (r orcaWorktreeRow) handle() string { return cmp3(r.ID, r.WorktreeID) }

var orcaStates = map[string]State{
	"idle": StateIdle, "working": StateWorking, "blocked": StateBlocked, "done": StateDone,
}

// orcaStatus maps a ps row into the normalised vocabulary. A waiting agent is
// blocked with its reason; an agentWait that was never looked up is unknown; a
// state Orca did not tell us about is unknown.
func orcaStatus(row orcaAgentRow) AgentStatus {
	wait := bytes.TrimSpace(row.AgentWait)
	waiting := len(wait) > 0 && string(wait) != "null" && string(wait) != "false"
	if waiting || row.BlockedReason != "" {
		reason := row.BlockedReason
		if reason == "" {
			var w struct {
				Reason        string `json:"reason"`
				BlockedReason string `json:"blockedReason"`
				Detail        string `json:"detail"`
			}
			var s string
			switch {
			case json.Unmarshal(wait, &s) == nil && s != "":
				reason = s
			case json.Unmarshal(wait, &w) == nil:
				reason = cmp3(w.BlockedReason, w.Reason, w.Detail)
			}
		}
		if reason == "" && row.ToolName != "" {
			reason = "waiting on a human: " + row.ToolName
		}
		if reason == "" {
			reason = "waiting on a human"
		}
		return AgentStatus{State: StateBlocked, BlockedReason: reason}
	}
	if len(wait) == 0 {
		return AgentStatus{State: StateUnknown}
	}
	return AgentStatus{State: normaliseState(row.State, orcaStates)}
}

func cmp3(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// orcaBackend. Handles are Orca terminal handles. A handle goes stale when Orca
// restarts; alias maps a stored handle to its live replacement.
type orcaBackend struct {
	mu    sync.Mutex
	alias map[Handle]Handle
	// setup records the worktree paths created with `--setup run`; armed the
	// paths whose next removal runs the archive hook.
	setup map[string]bool
	armed map[string]bool
}

func newOrcaBackend() *orcaBackend {
	return &orcaBackend{alias: map[Handle]Handle{}, setup: map[string]bool{}, armed: map[string]bool{}}
}

var (
	_ Backend         = (*orcaBackend)(nil)
	_ WorktreeManager = (*orcaBackend)(nil)
	_ SetupHook       = (*orcaBackend)(nil)
	_ ArchiveHook     = (*orcaBackend)(nil)
	_ BlockedReasoner = (*orcaBackend)(nil)
	_ SelfEnver       = (*orcaBackend)(nil)
)

func (*orcaBackend) Name() string { return "orca" }

// Declares does not list layout: Orca has no pane split or tab we drive.
func (*orcaBackend) Declares() []string {
	return []string{CapWorktreeManage, CapSetupHook, CapArchiveHook, CapBlockedReason, CapIdentityEnv}
}

func (*orcaBackend) ProvidesBlockedReason() {}

func (*orcaBackend) Self(ctx context.Context) (Identity, error) {
	handle, pane, sess := os.Getenv("ORCA_TERMINAL_HANDLE"), os.Getenv("ORCA_PANE_KEY"), os.Getenv("ORCA_AGENT_SESSION_ID")
	if handle == "" && pane == "" && sess == "" {
		return Identity{}, ErrNotInside
	}
	rec, _ := readOrcaIdentity(handle)
	return Identity{
		ID:      cmp3(rec.ID, pane, handle, sess),
		Handle:  Handle(cmp3(handle, pane, sess)),
		Backend: "orca",
	}, nil
}

func (b *orcaBackend) current(h Handle) Handle {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.alias[h]; ok {
		return c
	}
	return h
}

func (b *orcaBackend) terminals(ctx context.Context) ([]orcaTerminal, error) {
	res, err := orcaRun(ctx, orcaTimeout, "terminal", "list")
	if err != nil {
		return nil, err
	}
	var r struct {
		Terminals []orcaTerminal `json:"terminals"`
	}
	if len(res) > 0 {
		if err := json.Unmarshal(res, &r); err != nil {
			return nil, fail("orca terminal list: unexpected output: %v", err)
		}
	}
	return r.Terminals, nil
}

func (b *orcaBackend) ps(ctx context.Context) ([]orcaWorktreeRow, error) {
	res, err := orcaRun(ctx, orcaTimeout, "worktree", "ps", "--agent-wait")
	if err != nil {
		return nil, err
	}
	var r struct {
		Worktrees []orcaWorktreeRow `json:"worktrees"`
	}
	if len(res) > 0 {
		if err := json.Unmarshal(res, &r); err != nil {
			return nil, fail("orca worktree ps: unexpected output: %v", err)
		}
	}
	for i := range r.Worktrees {
		for j := range r.Worktrees[i].Agents {
			r.Worktrees[i].Agents[j].worktreePath = r.Worktrees[i].Path
		}
	}
	return r.Worktrees, nil
}

// Resolve returns the live handle for h. After an Orca restart the stored handle
// is stale: the terminal is found again by its pane key (kept in the identity
// file) and only the replacement is used from then on.
func (b *orcaBackend) Resolve(ctx context.Context, h Handle) (Handle, error) {
	cur := b.current(h)
	terms, err := b.terminals(ctx)
	if err != nil {
		return "", err
	}
	if slices.ContainsFunc(terms, func(t orcaTerminal) bool { return Handle(t.Handle) == cur }) {
		return cur, nil
	}
	rec, ok := readOrcaIdentity(string(h))
	if !ok {
		rec, ok = readOrcaIdentity(string(cur))
	}
	if ok && rec.PaneKey != "" {
		for _, t := range terms {
			// A real `terminal list` row has no paneKey; `terminal show` does.
			if t.Handle != "" && (t.PaneKey == rec.PaneKey || b.recordedPaneKey(ctx, Handle(t.Handle)) == rec.PaneKey) {
				b.mu.Lock()
				b.alias[h] = Handle(t.Handle)
				b.mu.Unlock()
				moved := rec
				moved.Handle = t.Handle
				_ = writeOrcaIdentity(moved) // the replacement's own file
				return Handle(t.Handle), nil
			}
		}
	}
	return "", fmt.Errorf("%w: %s", ErrGone, h)
}

// onHandle runs fn with h's live handle. On terminal_handle_stale it resolves
// again once and retries with the replacement only.
func (b *orcaBackend) onHandle(ctx context.Context, h Handle, fn func(live Handle) error) error {
	live := b.current(h)
	err := fn(live)
	if err == nil {
		return nil
	}
	if orcaStale(err) {
		nh, rerr := b.Resolve(ctx, h)
		if rerr != nil || nh == live {
			if rerr == nil {
				rerr = err
			}
			return rerr
		}
		err = fn(nh)
	}
	if orcaGone(err) {
		return fmt.Errorf("%w: %v", ErrGone, err)
	}
	return err
}

// Start launches an agent as a terminal in the worktree at spec.Cwd. It passes
// no permission flag and no env: Orca's own launch defaults apply, so a Claude
// agent may stop at Orca's confirmation prompt, which Status reports as blocked.
// The agent is not running until Status says so.
func (b *orcaBackend) Start(ctx context.Context, spec Spec) (Identity, error) {
	kind := cmp3(spec.Harness, spec.Agent)
	model, effort, prompt, err := orcaLaunchOptions(spec)
	if err != nil {
		return Identity{}, err
	}
	args := []string{"agent", "launch", "--agent", kind, "--surface", "terminal"}
	if spec.Cwd != "" {
		abs, err := filepath.Abs(spec.Cwd)
		if err != nil {
			return Identity{}, err
		}
		args = append(args, "--worktree", "path:"+abs)
	}
	if prompt != "" {
		args = append(args, "--prompt", prompt)
	}
	if model != "" {
		args = append(args, "--model", model)
		if effort != "" {
			args = append(args, "--effort", effort)
		}
	}
	res, err := orcaRun(ctx, 120*time.Second, args...)
	if err != nil {
		return Identity{}, err
	}
	handle, err := orcaLaunchHandle(res)
	if err != nil {
		return Identity{}, err
	}
	rec := orcaIdentityRecord{ID: handle, Handle: handle, Backend: "orca", Name: spec.Name}
	// The launcher's reply has no pane key; `terminal show` records it. If Orca
	// does not say yet, Status or List fills it in the first time they see it.
	if pk := b.recordedPaneKey(ctx, Handle(handle)); pk != "" {
		rec.PaneKey, rec.ID = pk, pk
	}
	// ORCA_* never reaches a child through the launcher; keep the rest of the
	// requested env in the identity file for the child's orch calls.
	rec.Env = map[string]string{}
	for _, kv := range childEnv("orca", nil, spec.Env) {
		if k, v, _ := strings.Cut(kv, "="); !strings.HasPrefix(k, "ORCA_") {
			rec.Env[k] = v // Orca sets its own ORCA_* in the terminal
		}
	}
	if err := writeOrcaIdentity(rec); err != nil {
		return Identity{}, err
	}
	if spec.Name != "" {
		_ = b.Rename(ctx, Handle(handle), spec.Name)
	}
	return Identity{ID: rec.ID, Handle: Handle(handle), Backend: "orca"}, nil
}

// orcaLaunchOptions folds spec.Args into the launch options. `agent launch`
// takes only --model and --effort (plus the prompt), so those are lifted when
// Model or Effort is empty. The pure session flag orch adds, --session-id <id>,
// is dropped: Orca chooses its own session. Any other argument cannot reach the
// agent, so it is an error rather than a silent drop.
func orcaLaunchOptions(spec Spec) (model, effort, prompt string, err error) {
	model, effort, prompt = spec.Model, spec.Effort, spec.Prompt
	var bad []string
	for i := 0; i < len(spec.Args); i++ {
		a := spec.Args[i]
		flag, val, hasVal := strings.Cut(a, "=")
		take := func() (string, bool) {
			if hasVal {
				return val, true
			}
			if i+1 < len(spec.Args) && !strings.HasPrefix(spec.Args[i+1], "--") {
				i++
				return spec.Args[i], true
			}
			return "", false
		}
		switch flag {
		case "--model", "--effort":
			v, ok := take()
			switch {
			case !ok:
				bad = append(bad, a)
			case flag == "--model" && model == "":
				model = v
			case flag == "--effort" && effort == "":
				effort = v
			}
		case "--session-id":
			if _, ok := take(); !ok {
				bad = append(bad, a)
			}
		default:
			bad = append(bad, a)
		}
	}
	if len(bad) > 0 {
		return "", "", "", fail("orca agent launch cannot pass these arguments to the agent: %s", strings.Join(bad, " "))
	}
	if effort != "" && model == "" {
		return "", "", "", fail("orca agent launch takes --effort only with --model")
	}
	return model, effort, prompt, nil
}

// orcaLaunchHandle reads the terminal handle of a launch. The reply names the
// surface (outcome.kind); only a terminal has a handle to drive.
func orcaLaunchHandle(res json.RawMessage) (string, error) {
	type outcome struct {
		Kind   string `json:"kind"`
		Handle string `json:"handle"`
	}
	var r struct {
		Outcome  outcome                   `json:"outcome"`
		Launch   struct{ Outcome outcome } `json:"launch"`
		Terminal string                    `json:"agentTerminalHandle"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return "", fail("orca launch: unexpected output: %v", err)
	}
	out := r.Launch.Outcome
	if out.Handle == "" {
		out = r.Outcome
	}
	if out.Kind != "" && out.Kind != "terminal" {
		return "", fail("orca started the agent as %q, not a terminal; orch drives terminals", out.Kind)
	}
	h := cmp3(out.Handle, r.Terminal)
	if h == "" {
		return "", fail("orca launch returned no terminal handle")
	}
	return h, nil
}

func (b *orcaBackend) Prompt(ctx context.Context, h Handle, text string) error {
	return b.onHandle(ctx, h, func(live Handle) error {
		_, err := orcaRun(ctx, orcaTimeout, "terminal", "send", "--terminal", string(live), "--text", text, "--enter")
		return err
	})
}

// Read returns the last lines of the rendered screen.
func (b *orcaBackend) Read(ctx context.Context, h Handle, lines int) (string, error) {
	var tail []string
	err := b.onHandle(ctx, h, func(live Handle) error {
		res, err := orcaRun(ctx, orcaTimeout, "terminal", "read", "--terminal", string(live), "--screen", "--limit", strconv.Itoa(max(lines, 1)))
		if err != nil {
			return err
		}
		var r struct {
			Terminal struct {
				Tail []string `json:"tail"`
			} `json:"terminal"`
		}
		if err := json.Unmarshal(res, &r); err != nil {
			return fail("orca terminal read: unexpected output: %v", err)
		}
		tail = r.Terminal.Tail
		return nil
	})
	if err != nil {
		return "", err
	}
	if lines > 0 && len(tail) > lines {
		tail = tail[len(tail)-lines:]
	}
	return strings.Join(tail, "\n"), nil
}

func (b *orcaBackend) Close(ctx context.Context, h Handle) error {
	return b.onHandle(ctx, h, func(live Handle) error {
		_, err := orcaRun(ctx, orcaTimeout, "terminal", "close", "--terminal", string(live))
		// A close right after a launch can answer tab_not_found while the
		// terminal is still alive (seen against Orca 1.4.253); retry once.
		if oe := asOrcaError(err); oe != nil && strings.Contains(oe.Message, "tab_not_found") {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			_, err = orcaRun(ctx, orcaTimeout, "terminal", "close", "--terminal", string(live))
		}
		return err
	})
}

func (b *orcaBackend) Focus(ctx context.Context, h Handle) error {
	return b.onHandle(ctx, h, func(live Handle) error {
		_, err := orcaRun(ctx, orcaTimeout, "terminal", "switch", "--terminal", string(live))
		return err
	})
}

func (b *orcaBackend) Rename(ctx context.Context, h Handle, name string) error {
	return b.onHandle(ctx, h, func(live Handle) error {
		_, err := orcaRun(ctx, orcaTimeout, "terminal", "rename", "--terminal", string(live), "--title", name)
		return err
	})
}

// findAgent picks the ps row of the terminal live (or its pane key).
func findOrcaAgent(rows []orcaWorktreeRow, live Handle, paneKey string) (orcaAgentRow, bool) {
	for _, w := range rows {
		for _, a := range w.Agents {
			if (a.Handle != "" && Handle(a.Handle) == live) || (paneKey != "" && a.PaneKey == paneKey) {
				return a, true
			}
		}
	}
	return orcaAgentRow{}, false
}

// Status maps Orca's answer. A terminal Orca lists but has no agent row for (a
// launch that has not come up, or one stopped at a confirmation prompt Orca does
// not report) is unknown: it is never reported running.
func (b *orcaBackend) Status(ctx context.Context, h Handle) (AgentStatus, error) {
	live, err := b.Resolve(ctx, h)
	if err != nil {
		return AgentStatus{}, err
	}
	rows, err := b.ps(ctx)
	if err != nil {
		return AgentStatus{}, err
	}
	rec, ok := readOrcaIdentity(string(h))
	if row, ok := findOrcaAgent(rows, live, rec.PaneKey); ok {
		return orcaStatus(row), nil
	}
	// A running agent's ps row names a pane key, never the terminal handle.
	// `terminal show` carries the same key as recordedPaneKey (Orca 1.4.253), so
	// the match is exact even when several agents share a worktree.
	pk := b.recordedPaneKey(ctx, live)
	if pk != "" {
		if ok && rec.PaneKey == "" {
			rec.PaneKey = pk
			_ = writeOrcaIdentity(rec)
		}
		for _, w := range rows {
			for _, a := range w.Agents {
				if a.PaneKey == pk {
					return orcaStatus(a), nil
				}
			}
		}
	} else if terms, err := b.terminals(ctx); err == nil {
		// No pane key to match: the lone agent row of the terminal's own worktree.
		for _, t := range terms {
			if Handle(t.Handle) != live || t.WorktreePath == "" {
				continue
			}
			var only []orcaAgentRow
			for _, w := range rows {
				if w.Path == t.WorktreePath {
					only = append(only, w.Agents...)
				}
			}
			if len(only) == 1 && only[0].Handle == "" {
				return orcaStatus(only[0]), nil
			}
		}
	}
	// No agent row yet. A first Claude launch stops at its bypass-permissions
	// confirmation, which Orca does not report: read it off the screen.
	if screen, err := b.Read(ctx, live, 40); err == nil && strings.Contains(screen, "Bypass Permissions") && strings.Contains(screen, "Enter to confirm") {
		return AgentStatus{State: StateBlocked, BlockedReason: "confirm Claude bypass-permissions mode"}, nil
	}
	return AgentStatus{State: StateUnknown}, nil
}

// recordedPaneKey is the pane key `terminal show` records for a terminal, or ""
// when Orca does not say.
func (b *orcaBackend) recordedPaneKey(ctx context.Context, live Handle) string {
	res, err := orcaRun(ctx, orcaTimeout, "terminal", "show", "--terminal", string(live))
	if err != nil {
		return ""
	}
	var r struct {
		RecordedPaneKey string `json:"recordedPaneKey"`
		Terminal        struct {
			RecordedPaneKey string `json:"recordedPaneKey"`
		} `json:"terminal"`
	}
	if json.Unmarshal(res, &r) != nil {
		return ""
	}
	return cmp3(r.RecordedPaneKey, r.Terminal.RecordedPaneKey)
}

// Wait polls Status until it is one of until or the timeout passes. For done and
// idle it also blocks in `terminal wait` (exit, tui-idle) between polls.
func (b *orcaBackend) Wait(ctx context.Context, h Handle, until []State, timeout time.Duration) (AgentStatus, error) {
	deadline := time.Now().Add(timeout)
	last := AgentStatus{State: StateUnknown}
	for {
		st, err := b.Status(ctx, h)
		if err != nil {
			return AgentStatus{State: StateUnknown}, err
		}
		last = st
		if slices.Contains(until, st.State) {
			return st, nil
		}
		remaining := max(time.Until(deadline), time.Millisecond)
		for _, w := range []struct {
			state State
			cond  string
		}{{StateDone, "exit"}, {StateIdle, "tui-idle"}} {
			if !slices.Contains(until, w.state) || slices.Contains(until, StateWorking) || slices.Contains(until, StateBlocked) {
				continue
			}
			slice := min(remaining, 30*time.Second)
			err := b.onHandle(ctx, h, func(live Handle) error {
				_, err := orcaRun(ctx, slice+10*time.Second, "terminal", "wait", "--terminal", string(live), "--for", w.cond, "--timeout-ms", strconv.FormatInt(slice.Milliseconds(), 10))
				return err
			})
			switch {
			case err == nil:
				return AgentStatus{State: w.state}, nil
			case orcaCode(err) == "timeout":
			case errors.Is(err, ErrGone):
				return AgentStatus{}, err
			default:
				return last, err
			}
			break
		}
		if time.Until(deadline) <= 0 {
			return last, fail("orca wait timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(min(orcaPoll, time.Until(deadline))):
		}
	}
}

// List keys each agent by its terminal handle, the key Status and Close take. A
// ps row carries only a pane key, so it is matched to a terminal through the
// identity files and then through each worktree terminal's recordedPaneKey. A
// row with no known terminal is listed under its pane key.
func (b *orcaBackend) List(ctx context.Context) ([]AgentInfo, error) {
	rows, err := b.ps(ctx)
	if err != nil {
		return nil, err
	}
	byKey := map[string]string{}
	for _, rec := range orcaIdentities() {
		if rec.PaneKey != "" {
			byKey[rec.PaneKey] = rec.Handle
		}
	}
	var terms []orcaTerminal
	termsRead := false
	var out []AgentInfo
	for _, w := range rows {
		for _, a := range w.Agents {
			h := a.Handle
			if h == "" && a.PaneKey != "" {
				h = byKey[a.PaneKey]
				if h == "" {
					if !termsRead {
						terms, _ = b.terminals(ctx)
						termsRead = true
					}
					for _, t := range terms {
						if t.WorktreePath != w.Path || t.Handle == "" {
							continue
						}
						pk := cmp3(t.PaneKey, b.recordedPaneKey(ctx, Handle(t.Handle)))
						if pk == "" {
							continue
						}
						byKey[pk] = t.Handle
						if rec, ok := readOrcaIdentity(t.Handle); ok && rec.PaneKey == "" {
							rec.PaneKey = pk
							_ = writeOrcaIdentity(rec)
						}
					}
					h = byKey[a.PaneKey]
				}
			}
			h = cmp3(h, a.PaneKey)
			if h == "" { // nothing to address it by
				continue
			}
			out = append(out, AgentInfo{
				Handle: Handle(h), Name: a.Title, Agent: a.AgentType, Cwd: w.Path,
				Status: orcaStatus(a), Session: a.SessionID,
			})
		}
	}
	return out, nil
}

// orcaIdentities reads every identity file.
func orcaIdentities() []orcaIdentityRecord {
	files, _ := filepath.Glob(filepath.Join(orcaIdentityDir(), "*.json"))
	var out []orcaIdentityRecord
	for _, f := range files {
		var rec orcaIdentityRecord
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &rec) == nil && rec.Handle != "" {
			out = append(out, rec)
		}
	}
	return out
}

// SelfEnv is the env recorded for the calling terminal's launch: found by
// ORCA_TERMINAL_HANDLE, else by ORCA_PANE_KEY. Empty when nothing is recorded.
func (*orcaBackend) SelfEnv(ctx context.Context) (map[string]string, error) {
	rec, ok := readOrcaIdentity(os.Getenv("ORCA_TERMINAL_HANDLE"))
	if pk := os.Getenv("ORCA_PANE_KEY"); !ok && pk != "" {
		for _, r := range orcaIdentities() {
			if r.PaneKey == pk {
				rec, ok = r, true
				break
			}
		}
	}
	env := map[string]string{}
	if ok {
		maps.Copy(env, rec.Env)
	}
	return env, nil
}

// orcaTerminalGone says `terminal list` no longer has h. A failed list is not
// proof either way, so it reports false.
func orcaTerminalGone(ctx context.Context, h Handle) bool {
	gone, err := newOrcaBackend().Gone(ctx, h)
	return err == nil && gone
}

// Gone reports whether the terminal has left `terminal list`; Close polls it.
func (b *orcaBackend) Gone(ctx context.Context, h Handle) (bool, error) {
	terms, err := b.terminals(ctx)
	if err != nil {
		return false, err
	}
	cur := b.current(h)
	return !slices.ContainsFunc(terms, func(t orcaTerminal) bool { return Handle(t.Handle) == cur || Handle(t.Handle) == h }), nil
}

// worktree.manage

type orcaWorktreeReply struct {
	Worktree orcaWorktreeRow `json:"worktree"`
}

func (r orcaWorktreeReply) worktree(branch string) Worktree {
	return Worktree{Path: r.Worktree.Path, Branch: cmp3(r.Worktree.Branch, branch), Workspace: Handle(r.Worktree.handle())}
}

// CreateWorktree makes a parentless worktree. Orca picks the location; the path
// comes from its reply. Setup runs natively only when asked (NativeSetup), and
// then Orca only starts it: the result is Started, never Succeeded.
func (b *orcaBackend) CreateWorktree(ctx context.Context, w WorktreeSpec) (Worktree, error) {
	setup := "skip"
	if w.NativeSetup {
		setup = "run"
	}
	args := []string{"worktree", "create", "--name", w.Name, "--branch", w.Branch, "--base-branch", w.Base, "--no-parent", "--setup", setup}
	if w.Repo != "" {
		args = append(args, "--repo", "path:"+w.Repo)
	}
	res, err := orcaRun(ctx, 120*time.Second, args...)
	if err != nil {
		return Worktree{}, err
	}
	var r orcaWorktreeReply
	if err := json.Unmarshal(res, &r); err != nil || r.Worktree.Path == "" {
		return Worktree{}, fail("orca worktree create: reply has no worktree path")
	}
	b.mu.Lock()
	b.setup[r.Worktree.Path] = w.NativeSetup
	b.mu.Unlock()
	return r.worktree(w.Branch), nil
}

func (b *orcaBackend) OpenWorktree(ctx context.Context, w WorktreeSpec) (Worktree, error) {
	rows, err := b.ps(ctx)
	if err != nil {
		return Worktree{}, err
	}
	for _, r := range rows {
		if r.Path == w.Path {
			return orcaWorktreeReply{Worktree: r}.worktree(w.Branch), nil
		}
	}
	return Worktree{}, fmt.Errorf("%w: worktree %s", ErrGone, w.Path)
}

func (b *orcaBackend) OpenWorktrees(ctx context.Context) (map[string]string, error) {
	rows, err := b.ps(ctx)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, r := range rows {
		if r.Path != "" {
			m[r.Path] = r.handle()
		}
	}
	return m, nil
}

// RemoveWorktree removes after orch's own merged and clean checks. It never
// forces, never deletes the branch and never allows a failed archive hook. The
// archive hook runs (--run-hooks) only when RunArchive armed this path.
func (b *orcaBackend) RemoveWorktree(ctx context.Context, w WorktreeSpec) error {
	args := []string{"worktree", "rm", "--worktree", "path:" + w.Path}
	b.mu.Lock()
	hooks := b.armed[w.Path]
	delete(b.armed, w.Path)
	b.mu.Unlock()
	if hooks {
		args = append(args, "--run-hooks")
	}
	_, err := orcaRun(ctx, 120*time.Second, args...)
	oe := asOrcaError(err)
	switch {
	case err == nil:
		return nil
	case oe != nil && oe.Code == "worktree_archive_hook_failed":
		return &HookFailedError{Hook: "archive", ExitCode: oe.exitCode(), Err: err}
	case oe != nil && hooks && (oe.Code == "unknown_flag" || oe.Code == "invalid_argument") && strings.Contains(oe.Message, "run-hooks"):
		return fmt.Errorf("%w: %v", ErrHookRefused, err)
	case oe != nil && (strings.Contains(oe.Code, "dirty") || strings.Contains(oe.Message, "uncommitted")):
		return fmt.Errorf("%w: %v", ErrWorktreeDirty, err)
	case orcaGone(err):
		return fmt.Errorf("%w: %v", ErrGone, err)
	}
	return err
}

// hooks

// RunSetup reports the native setup of a worktree this backend created with
// `--setup run`. Orca only starts the script: the result is Started. A worktree
// created without it (config listed the capability out, or created elsewhere)
// was not set up, so the hook is refused before running and orch's runner
// takes over.
func (b *orcaBackend) RunSetup(ctx context.Context, env HookEnv) (HookResult, error) {
	b.mu.Lock()
	ran := b.setup[env.WorktreePath]
	b.mu.Unlock()
	if !ran {
		return HookResult{}, fmt.Errorf("%w: worktree %s was not created with native setup", ErrHookRefused, env.WorktreePath)
	}
	return HookResult{Status: HookStarted}, nil
}

// RunArchive arms the next RemoveWorktree of the worktree to run Orca's archive
// hook (--run-hooks). The hook runs inside the removal; a failure comes back
// from RemoveWorktree as *HookFailedError and blocks it.
func (b *orcaBackend) RunArchive(ctx context.Context, env HookEnv) (HookResult, error) {
	b.mu.Lock()
	b.armed[env.WorktreePath] = true
	b.mu.Unlock()
	return HookResult{Status: HookStarted}, nil
}
