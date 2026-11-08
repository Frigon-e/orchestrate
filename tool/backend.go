package main

import (
	"context"
	"errors"
	"time"
)

// Handle names one pane, terminal or agent inside a backend. It is a
// backend-owned string: core stores it, compares it for equality and passes it
// back, and never parses it. A stored handle can go stale; Resolve says so.
type Handle string

// State is the normalised agent state. A backend maps its native values into
// this set and maps anything it does not recognise to StateUnknown, never an
// error.
type State string

const (
	StateIdle    State = "idle"
	StateWorking State = "working"
	StateBlocked State = "blocked"
	StateDone    State = "done"
	StateUnknown State = "unknown"
)

func (s State) valid() bool {
	switch s {
	case StateIdle, StateWorking, StateBlocked, StateDone, StateUnknown:
		return true
	}
	return false
}

// normaliseState maps a backend's native status text through table. Anything
// the table does not list is StateUnknown.
func normaliseState(native string, table map[string]State) State {
	if s, ok := table[native]; ok && s.valid() {
		return s
	}
	return StateUnknown
}

// AgentStatus is an agent's state. BlockedReason is for display and is set
// only with StateBlocked, and only when the backend knows why.
type AgentStatus struct {
	State         State
	BlockedReason string
}

// Identity says who an agent is to the backend and to orch. ID is the stable
// key orch uses for the role marker and lead owner. A backend that cannot
// inject env into a child writes the identity to a file keyed by Handle, and
// the child's orch calls read it from there.
type Identity struct {
	ID      string
	Handle  Handle
	Backend string
}

// Spec is what Start needs to launch an agent.
type Spec struct {
	Agent   string
	Harness string
	Model   string
	Effort  string
	Cwd     string
	Prompt  string
	Name    string
	Env     map[string]string
}

// AgentInfo is one row of List.
type AgentInfo struct {
	Handle Handle
	Name   string
	Agent  string
	Cwd    string
	Status AgentStatus
}

var (
	// ErrGone is returned by Resolve (and any operation) for a handle that no
	// longer names a live agent.
	ErrGone = errors.New("backend handle is gone")
	// ErrNotInside is returned by Self when the process is not running inside
	// the backend.
	ErrNotInside = errors.New("not running inside the backend")
)

// Backend is the program that holds the panes, terminals and worktrees the
// leads live in. The required operations are the ones orch core uses; a
// feature only some backends have is a capability (see backend_caps.go).
type Backend interface {
	// Name is the registered backend name.
	Name() string
	// Self is the orchestrator or lead identity of the calling process, or
	// ErrNotInside.
	Self(ctx context.Context) (Identity, error)
	// Start launches an agent and returns its identity. It strips every other
	// registered backend's env prefix from the child environment (childEnv).
	Start(ctx context.Context, spec Spec) (Identity, error)
	// Prompt sends text to a running agent.
	Prompt(ctx context.Context, h Handle, text string) error
	// Read returns the last lines of the agent's output.
	Read(ctx context.Context, h Handle, lines int) (string, error)
	// Close ends the agent. Closing a gone handle is ErrGone.
	Close(ctx context.Context, h Handle) error
	// Focus brings the agent to the user's attention.
	Focus(ctx context.Context, h Handle) error
	// Rename sets the display name.
	Rename(ctx context.Context, h Handle, name string) error
	// Status reports the normalised state.
	Status(ctx context.Context, h Handle) (AgentStatus, error)
	// Wait blocks until the agent is in one of until, or the timeout passes,
	// and returns the status it ended on.
	Wait(ctx context.Context, h Handle, until []State, timeout time.Duration) (AgentStatus, error)
	// List returns every agent the backend knows.
	List(ctx context.Context) ([]AgentInfo, error)
	// Resolve returns the live handle for h, or ErrGone. It tells a closed
	// agent apart from the backend being unreachable (the latter is another
	// error).
	Resolve(ctx context.Context, h Handle) (Handle, error)
	// Declares lists the capability names the backend implements natively.
	Declares() []string
}

// Active is the selected backend plus the config that decides which declared
// capabilities run natively. Core code holds an *Active and asks Has.
type Active struct {
	Backend
	native    []string
	nativeSet bool
}
