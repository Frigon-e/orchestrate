package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// The herdr backend: every herdr command and JSON shape lives in this file.
// ORCH_HERDR names the binary (default herdr); the test binary doubles as a
// fake herdr through it.

const herdrTimeout = 60 * time.Second

// A new pane is not an available shell until its prompt is up; herdr refuses
// `agent start` at once with agent_pane_busy instead of waiting. Herdr 0.9.3
// has no readiness signal to poll (pane get reports agent_status "unknown"),
// so Start retries on that one error, bounded. Any other error returns unchanged.
var (
	agentStartBusyFor = 10 * time.Second
	agentStartBackoff = 100 * time.Millisecond
)

func init() {
	legacyBackendName = "herdr"
	registerBackend("herdr", "HERDR_", func(getenv func(string) string) bool { return getenv("HERDR_ENV") == "1" },
		func() (Backend, error) { return &herdrBackend{}, nil })
}

// herdrAgent is core's name for an agent row; callers outside the backend
// files only read it.
type herdrAgent = AgentInfo

// herdrAgentJSON is one row of `herdr agent list`.
type herdrAgentJSON struct {
	PaneID  string `json:"pane_id"`
	TabID   string `json:"tab_id"`
	Name    string `json:"name"`
	Agent   string `json:"agent"`
	Status  string `json:"agent_status"`
	Session struct {
		Value string `json:"value"`
	} `json:"agent_session"`
}

var herdrStates = map[string]State{
	"idle": StateIdle, "working": StateWorking, "blocked": StateBlocked, "done": StateDone, "unknown": StateUnknown,
}

func (ag herdrAgentJSON) info() AgentInfo {
	return AgentInfo{
		Handle:  Handle(ag.PaneID),
		Name:    ag.Name,
		Agent:   ag.Agent,
		Status:  AgentStatus{State: normaliseState(ag.Status, herdrStates)},
		Session: ag.Session.Value,
	}
}

func herdrCmd(timeout time.Duration, args ...string) ([]byte, error) {
	return herdrCmdContext(context.Background(), timeout, args...)
}

func herdrCmdContext(parent context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, envOr("ORCH_HERDR", "herdr"), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	what := strings.Join(args[:min(2, len(args))], " ")
	if parent.Err() != nil {
		return out, parent.Err()
	}
	if ctx.Err() != nil {
		return out, fail("herdr %s timed out after %s", what, timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return out, fail("herdr %s failed: %s", what, msg)
	}
	return out, nil
}

func herdrJSON(v any, timeout time.Duration, args ...string) error {
	out, err := herdrCmd(timeout, args...)
	if err != nil || v == nil || len(bytes.TrimSpace(out)) == 0 {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fail("herdr %s: unexpected output: %v", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return nil
}

func herdrEnvArgs(env map[string]string) []string {
	var out []string
	for _, kv := range childEnv("herdr", nil, env) {
		out = append(out, "--env", kv)
	}
	return out
}

func herdrGone(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "pane_not_found") || strings.Contains(m, "agent_not_found")
}

type herdrBackend struct{}

var (
	_ Backend         = (*herdrBackend)(nil)
	_ Layout          = (*herdrBackend)(nil)
	_ WorktreeManager = (*herdrBackend)(nil)
	_ StateDirer      = (*herdrBackend)(nil)
	_ PromptWaiter    = (*herdrBackend)(nil)
	_ SelfSessioner   = (*herdrBackend)(nil)
)

func (*herdrBackend) Name() string { return "herdr" }

func (*herdrBackend) Declares() []string { return []string{CapLayout, CapWorktreeManage, CapStateDir} }

func (*herdrBackend) Self(ctx context.Context) (Identity, error) {
	pane := os.Getenv("HERDR_PANE_ID")
	if os.Getenv("HERDR_ENV") != "1" || pane == "" {
		return Identity{}, ErrNotInside
	}
	return Identity{
		ID: pane, Handle: Handle(pane), Backend: "herdr",
		Tab: Handle(os.Getenv("HERDR_TAB_ID")), Workspace: Handle(os.Getenv("HERDR_WORKSPACE_ID")),
	}, nil
}

// SelfSession is the harness session herdr reports for the calling pane.
func (*herdrBackend) SelfSession(ctx context.Context) (string, error) {
	var res struct {
		Result struct {
			Pane herdrAgentJSON `json:"pane"`
		} `json:"result"`
	}
	if err := herdrJSON(&res, herdrTimeout, "pane", "current", "--current"); err != nil {
		return "", err
	}
	return res.Result.Pane.Session.Value, nil
}

// Start launches an agent. With spec.Place it starts in that pane; without,
// it makes a tab for it first. The prompt, when given, is sent once the agent
// is up.
func (h *herdrBackend) Start(ctx context.Context, spec Spec) (Identity, error) {
	id := Identity{Backend: "herdr", Handle: spec.Place, ID: string(spec.Place)}
	if spec.Place == "" {
		tab, err := h.NewTab(ctx, spec.Name, spec)
		if err != nil {
			return Identity{}, err
		}
		id = tab
	}
	kind := spec.Harness
	if kind == "" {
		kind = spec.Agent
	}
	args := append([]string{"agent", "start", spec.Name, "--kind", kind, "--pane", string(id.Handle), "--timeout", "90000", "--"}, spec.Args...)
	deadline := time.Now().Add(agentStartBusyFor)
	for wait := agentStartBackoff; ; wait = min(wait*2, time.Second) {
		_, err := herdrCmdContext(ctx, 120*time.Second, args...)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "agent_pane_busy") || time.Now().Add(wait).After(deadline) {
			return id, err
		}
		time.Sleep(wait)
	}
	if spec.Prompt != "" {
		if err := h.Prompt(ctx, id.Handle, spec.Prompt); err != nil {
			return id, err
		}
	}
	return id, nil
}

func (*herdrBackend) Prompt(ctx context.Context, h Handle, text string) error {
	_, err := herdrCmdContext(ctx, herdrTimeout, "agent", "prompt", string(h), text)
	return err
}

// PromptWait sends text and blocks until herdr sees the agent settle again.
func (*herdrBackend) PromptWait(ctx context.Context, h Handle, text string, timeout time.Duration) error {
	_, err := herdrCmdContext(ctx, timeout+time.Minute, "agent", "prompt", string(h), text, "--wait", "--timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	return err
}

// Read returns the last lines of the agent's screen. Herdr returns nothing for
// small --lines values, so it always asks for 200 or more and trims here.
func (*herdrBackend) Read(ctx context.Context, h Handle, lines int) (string, error) {
	out, err := herdrCmdContext(ctx, herdrTimeout, "agent", "read", string(h), "--source", "recent-unwrapped", "--lines", strconv.Itoa(max(lines, 200)))
	if err != nil {
		return "", err
	}
	all := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	return strings.Join(all[max(0, len(all)-lines):], "\n"), nil
}

func (*herdrBackend) Close(ctx context.Context, h Handle) error {
	_, err := herdrCmdContext(ctx, herdrTimeout, "pane", "close", string(h))
	if herdrGone(err) {
		return fmt.Errorf("%w: %v", ErrGone, err)
	}
	return err
}

func (*herdrBackend) Focus(ctx context.Context, h Handle) error {
	_, err := herdrCmdContext(ctx, herdrTimeout, "agent", "focus", string(h))
	return err
}

func (*herdrBackend) Rename(ctx context.Context, h Handle, name string) error {
	_, err := herdrCmdContext(ctx, herdrTimeout, "pane", "rename", string(h), name)
	return err
}

func herdrAgentStatus(out []byte) AgentStatus {
	var res struct {
		Result struct {
			Agent herdrAgentJSON `json:"agent"`
		} `json:"result"`
	}
	_ = json.Unmarshal(out, &res)
	return AgentStatus{State: normaliseState(res.Result.Agent.Status, herdrStates)}
}

func (*herdrBackend) Status(ctx context.Context, h Handle) (AgentStatus, error) {
	out, err := herdrCmdContext(ctx, herdrTimeout, "agent", "get", string(h))
	if herdrGone(err) {
		return AgentStatus{}, fmt.Errorf("%w: %v", ErrGone, err)
	}
	if err != nil {
		return AgentStatus{}, err
	}
	return herdrAgentStatus(out), nil
}

// Wait blocks until the agent is in one of until. Herdr's answer carries the
// state it ended on when it gives one; otherwise the state is unknown.
func (*herdrBackend) Wait(ctx context.Context, h Handle, until []State, timeout time.Duration) (AgentStatus, error) {
	args := []string{"agent", "wait", string(h)}
	for _, s := range until {
		args = append(args, "--until", string(s))
	}
	args = append(args, "--timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	out, err := herdrCmdContext(ctx, timeout+10*time.Second, args...)
	if herdrGone(err) {
		return AgentStatus{}, fmt.Errorf("%w: %v", ErrGone, err)
	}
	if err != nil {
		return AgentStatus{State: StateUnknown}, err
	}
	return herdrAgentStatus(out), nil
}

func (*herdrBackend) List(ctx context.Context) ([]AgentInfo, error) {
	var res struct {
		Result struct {
			Agents []herdrAgentJSON `json:"agents"`
		} `json:"result"`
	}
	out, err := herdrCmdContext(ctx, herdrTimeout, "agent", "list")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &res); err != nil {
			return nil, fail("herdr agent list: unexpected output: %v", err)
		}
	}
	rows := make([]AgentInfo, 0, len(res.Result.Agents))
	for _, ag := range res.Result.Agents {
		rows = append(rows, ag.info())
	}
	return rows, nil
}

// Resolve tells a closed pane apart from herdr being unreachable.
func (*herdrBackend) Resolve(ctx context.Context, h Handle) (Handle, error) {
	_, err := herdrCmdContext(ctx, herdrTimeout, "pane", "get", string(h))
	if err == nil {
		return h, nil
	}
	if strings.Contains(err.Error(), "pane_not_found") {
		return "", fmt.Errorf("%w: %s", ErrGone, h)
	}
	return "", err
}

// layout

type herdrPaneResult struct {
	Result struct {
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
		Tab struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	} `json:"result"`
}

func (*herdrBackend) Split(ctx context.Context, anchor Handle, spec Spec) (Identity, error) {
	dir := spec.Direction
	if dir == "" {
		dir = "right"
	}
	args := append([]string{"pane", "split", string(anchor), "--direction", dir, "--cwd", spec.Cwd, "--no-focus"}, herdrEnvArgs(spec.Env)...)
	var res herdrPaneResult
	if err := herdrJSON(&res, herdrTimeout, args...); err != nil {
		return Identity{}, err
	}
	p := res.Result.Pane.PaneID
	return Identity{ID: p, Handle: Handle(p), Backend: "herdr"}, nil
}

func (*herdrBackend) NewTab(ctx context.Context, name string, spec Spec) (Identity, error) {
	ws := string(spec.Workspace)
	if ws == "" {
		ws = os.Getenv("HERDR_WORKSPACE_ID")
	}
	args := append([]string{"tab", "create", "--workspace", ws, "--cwd", spec.Cwd, "--label", name, "--no-focus"}, herdrEnvArgs(spec.Env)...)
	var res herdrPaneResult
	if err := herdrJSON(&res, herdrTimeout, args...); err != nil {
		return Identity{}, err
	}
	p := res.Result.RootPane.PaneID
	return Identity{ID: p, Handle: Handle(p), Backend: "herdr", Tab: Handle(res.Result.Tab.TabID), Workspace: Handle(ws)}, nil
}

// Contains says whether tab belongs to workspace: herdr prefixes tab ids with
// their workspace id.
func (*herdrBackend) Contains(workspace, tab Handle) bool {
	return strings.HasPrefix(string(tab), string(workspace)+":")
}

// worktree.manage

type herdrWorktreeJSON struct {
	Result struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
			PaneCount   int    `json:"pane_count"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
			TabID  string `json:"tab_id"`
		} `json:"root_pane"`
	} `json:"result"`
}

func (r herdrWorktreeJSON) worktree(w WorktreeSpec) Worktree {
	return Worktree{
		Path: w.Path, Branch: w.Branch,
		Workspace: Handle(r.Result.Workspace.WorkspaceID), Root: Handle(r.Result.RootPane.PaneID),
		Tab: Handle(r.Result.RootPane.TabID), Panes: r.Result.Workspace.PaneCount,
	}
}

func (*herdrBackend) CreateWorktree(ctx context.Context, w WorktreeSpec) (Worktree, error) {
	var res herdrWorktreeJSON
	if err := herdrJSON(&res, herdrTimeout, "worktree", "create", "--cwd", w.Repo, "--branch", w.Branch, "--base", w.Base, "--path", w.Path, "--label", w.Name, "--no-focus"); err != nil {
		return Worktree{}, err
	}
	return res.worktree(w), nil
}

func (*herdrBackend) OpenWorktree(ctx context.Context, w WorktreeSpec) (Worktree, error) {
	var res herdrWorktreeJSON
	if err := herdrJSON(&res, herdrTimeout, "worktree", "open", "--cwd", w.Repo, "--path", w.Path, "--no-focus"); err != nil {
		return Worktree{}, err
	}
	return res.worktree(w), nil
}

// RemoveWorktree closes the worktree's workspace and removes the checkout. A
// dirty tree comes back as ErrWorktreeDirty; it never forces.
func (h *herdrBackend) RemoveWorktree(ctx context.Context, w WorktreeSpec) error {
	wt, err := h.OpenWorktree(ctx, w)
	if err != nil {
		return err
	}
	if _, err := herdrCmdContext(ctx, herdrTimeout, "worktree", "remove", "--workspace", string(wt.Workspace)); err != nil {
		if strings.Contains(err.Error(), "dirty_worktree_requires_force") {
			return fmt.Errorf("%w: %v", ErrWorktreeDirty, err)
		}
		return err
	}
	return nil
}

// OpenWorktrees maps each open worktree's checkout path to its workspace.
func (*herdrBackend) OpenWorktrees(ctx context.Context) (map[string]string, error) {
	var res struct {
		Result struct {
			Workspaces []struct {
				ID       string `json:"workspace_id"`
				Worktree struct {
					Checkout string `json:"checkout_path"`
				} `json:"worktree"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := herdrJSON(&res, herdrTimeout, "workspace", "list"); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, w := range res.Result.Workspaces {
		if w.Worktree.Checkout != "" {
			m[w.Worktree.Checkout] = w.ID
		}
	}
	return m, nil
}

// state.dir

// StateDir is herdr's worktrees directory when its config names one.
func (*herdrBackend) StateDir() (string, bool) {
	var cfg struct {
		Worktrees struct {
			Directory string `toml:"directory"`
		} `toml:"worktrees"`
	}
	path := expandHome(envOr("ORCH_HERDR_CONFIG", "~/.config/herdr/config.toml"))
	if _, err := toml.DecodeFile(path, &cfg); err != nil || cfg.Worktrees.Directory == "" {
		return "", false
	}
	return expandHome(cfg.Worktrees.Directory), true
}

