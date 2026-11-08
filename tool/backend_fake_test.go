package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// fakeBackend is the in-process backend the conformance suite and the core
// tests run against. It implements every contract in backend_caps.go; which of
// them it declares is up to declare.
type fakeBackend struct {
	name    string
	declare []string
	// rawStatus makes Status return the native status text instead of a
	// normalised State, as a broken backend would.
	rawStatus bool
	// startEnv records the env of the last Start, after foreign stripping.
	startEnv []string

	mu     sync.Mutex
	seq    int
	agents map[Handle]*fakeBackendAgent
	trees  map[string]bool
}

type fakeBackendAgent struct {
	info   AgentInfo
	native string // native status text
	out    []string
}

func newFakeBackend(name string, declare ...string) *fakeBackend {
	return &fakeBackend{name: name, declare: declare, agents: map[Handle]*fakeBackendAgent{}, trees: map[string]bool{}}
}

var fakeStates = map[string]State{"READY": StateIdle, "BUSY": StateWorking, "PROMPT": StateBlocked, "FINISHED": StateDone}

func (f *fakeBackend) Name() string       { return f.name }
func (f *fakeBackend) Declares() []string { return f.declare }

func (f *fakeBackend) Self(ctx context.Context) (Identity, error) {
	if os.Getenv("FAKEMUX_SELF") == "" {
		return Identity{ID: "self-" + f.name, Handle: "h-self", Backend: f.name}, nil
	}
	return Identity{}, ErrNotInside
}

func (f *fakeBackend) Start(ctx context.Context, spec Spec) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	h := Handle(fmt.Sprintf("%s-%d", f.name, f.seq))
	f.startEnv = childEnv(f.name, os.Environ(), spec.Env)
	f.agents[h] = &fakeBackendAgent{
		info:   AgentInfo{Handle: h, Name: spec.Name, Agent: spec.Agent, Cwd: spec.Cwd},
		native: "BUSY",
		out:    []string{"started", spec.Prompt},
	}
	return Identity{ID: string(h) + "-id", Handle: h, Backend: f.name}, nil
}

func (f *fakeBackend) get(h Handle) (*fakeBackendAgent, error) {
	a, ok := f.agents[h]
	if !ok {
		return nil, ErrGone
	}
	return a, nil
}

func (f *fakeBackend) Prompt(ctx context.Context, h Handle, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, err := f.get(h)
	if err != nil {
		return err
	}
	a.out = append(a.out, text)
	return nil
}

func (f *fakeBackend) Read(ctx context.Context, h Handle, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, err := f.get(h)
	if err != nil {
		return "", err
	}
	out := a.out
	if lines > 0 && len(out) > lines {
		out = out[len(out)-lines:]
	}
	return strings.Join(out, "\n"), nil
}

func (f *fakeBackend) Close(ctx context.Context, h Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.get(h); err != nil {
		return err
	}
	delete(f.agents, h)
	return nil
}

func (f *fakeBackend) Focus(ctx context.Context, h Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.get(h)
	return err
}

func (f *fakeBackend) Rename(ctx context.Context, h Handle, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, err := f.get(h)
	if err != nil {
		return err
	}
	a.info.Name = name
	return nil
}

func (f *fakeBackend) status(a *fakeBackendAgent) AgentStatus {
	st := normaliseState(a.native, fakeStates)
	s := AgentStatus{State: st}
	if st == StateBlocked {
		s.BlockedReason = "needs a decision"
	}
	if f.rawStatus {
		s.State = State(a.native)
	}
	return s
}

func (f *fakeBackend) Status(ctx context.Context, h Handle) (AgentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, err := f.get(h)
	if err != nil {
		return AgentStatus{}, err
	}
	return f.status(a), nil
}

// setNative changes an agent's native status text (test control).
func (f *fakeBackend) setNative(h Handle, native string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents[h].native = native
}

func (f *fakeBackend) Wait(ctx context.Context, h Handle, until []State, timeout time.Duration) (AgentStatus, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := f.Status(ctx, h)
		if err != nil || slices.Contains(until, st.State) {
			return st, err
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("wait timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (f *fakeBackend) List(ctx context.Context) ([]AgentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []AgentInfo
	for _, a := range f.agents {
		info := a.info
		info.Status = f.status(a)
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b AgentInfo) int { return strings.Compare(string(a.Handle), string(b.Handle)) })
	return out, nil
}

func (f *fakeBackend) Resolve(ctx context.Context, h Handle) (Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.get(h); err != nil {
		return "", ErrGone
	}
	return h, nil
}

// layout
func (f *fakeBackend) Split(ctx context.Context, anchor Handle, spec Spec) (Identity, error) {
	if _, err := f.Resolve(ctx, anchor); err != nil {
		return Identity{}, err
	}
	return f.Start(ctx, spec)
}

func (f *fakeBackend) NewTab(ctx context.Context, name string, spec Spec) (Identity, error) {
	spec.Name = name
	return f.Start(ctx, spec)
}

func (f *fakeBackend) Contains(workspace, tab Handle) bool { return true }

// worktree.manage
func (f *fakeBackend) OpenWorktrees(ctx context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]string{}
	for p := range f.trees {
		m[p] = "ws-" + p
	}
	return m, nil
}

func (f *fakeBackend) CreateWorktree(ctx context.Context, w WorktreeSpec) (Worktree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := "/fake/worktrees/" + w.Name
	f.trees[p] = true
	return Worktree{Path: p, Branch: w.Branch}, nil
}

func (f *fakeBackend) OpenWorktree(ctx context.Context, w WorktreeSpec) (Worktree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := w.Path
	if !f.trees[path] {
		return Worktree{}, ErrGone
	}
	return Worktree{Path: path}, nil
}

func (f *fakeBackend) RemoveWorktree(ctx context.Context, w WorktreeSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := w.Path
	if !f.trees[path] {
		return ErrGone
	}
	delete(f.trees, path)
	return nil
}

// hooks
func (f *fakeBackend) RunSetup(ctx context.Context, env HookEnv) (HookResult, error) {
	return HookResult{Status: HookStarted}, nil
}

func (f *fakeBackend) RunArchive(ctx context.Context, env HookEnv) (HookResult, error) {
	return HookResult{Status: HookDone}, nil
}

// markers and state dir
func (f *fakeBackend) ProvidesBlockedReason() {}

func (f *fakeBackend) StateDir() (string, bool) { return "/fake/state", true }

var _ Backend = (*fakeBackend)(nil)
