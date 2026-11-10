package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"sync"
)

// Capability contracts. A capability has a name, a Go interface (the contract),
// an orch-owned fallback and a conformance check. The registry below is the one
// place that maps names to contracts.

// Layout is the `layout` capability: split a pane and create a tab.
type Layout interface {
	Split(ctx context.Context, anchor Handle, spec Spec) (Identity, error)
	NewTab(ctx context.Context, name string, spec Spec) (Identity, error)
	// Contains says whether tab belongs to workspace, so placement only fills
	// tabs of the caller's own workspace.
	Contains(workspace, tab Handle) bool
}

// WorktreeSpec asks for a worktree for one lead. An empty Parent means no
// parent.
type WorktreeSpec struct {
	Name   string
	Branch string
	Base   string
	Parent string
	// Repo is the main checkout. Path is where orch wants the worktree; a
	// backend that picks its own location ignores it and reports the real one.
	Repo string
	Path string
	// NativeSetup asks CreateWorktree to have the backend run the repo's setup
	// script (worktree.setup-hook). Callers set it only when Has(setup-hook);
	// without it the backend must not run the script, so orch's runner can.
	NativeSetup bool
}

// Worktree is a created or opened worktree. Path is read from the backend's
// answer, never computed by orch.
type Worktree struct {
	Path   string
	Branch string
	// Workspace, Root and Tab are the workspace made for the worktree, its first
	// pane and that pane's tab. Panes counts the panes the workspace holds.
	Workspace Handle
	Root      Handle
	Tab       Handle
	Panes     int
}

// ErrWorktreeDirty is returned by RemoveWorktree when the backend refuses to
// remove a worktree with uncommitted changes.
var ErrWorktreeDirty = errors.New("backend reports the worktree dirty")

// WorktreeManager is the `worktree.manage` capability. Remove runs only after
// orch's own merged and clean checks.
type WorktreeManager interface {
	CreateWorktree(ctx context.Context, w WorktreeSpec) (Worktree, error)
	OpenWorktree(ctx context.Context, w WorktreeSpec) (Worktree, error)
	RemoveWorktree(ctx context.Context, w WorktreeSpec) error
	// OpenWorktrees maps the checkout path of every worktree the backend holds
	// open to its workspace handle.
	OpenWorktrees(ctx context.Context) (map[string]string, error)
}

// HookEnv is the variable set both runners give a hook script.
type HookEnv struct {
	RootPath      string
	WorktreePath  string
	WorkspaceName string
}

// HookStatus is the outcome of a hook request.
type HookStatus string

const (
	// HookStarted means the hook was started and its result is not known. A
	// native setup run is always Started.
	HookStarted HookStatus = "started"
	// HookDone means the hook ran to the end; ExitCode is its result.
	HookDone HookStatus = "done"
)

// HookResult is what a hook run reports.
type HookResult struct {
	Status   HookStatus
	ExitCode int
}

// ErrHookRefused means the backend refused before running any script; the
// caller falls back to orch's runner. A hook that ran and failed is a
// HookDone result with a non-zero ExitCode, or its own error, and is not rerun.
var ErrHookRefused = errors.New("backend refused the hook before running it")

// HookFailedError means a native hook ran and failed. It is not a refusal: the
// caller reports it and does not rerun the hook or fall back.
type HookFailedError struct {
	Hook     string
	ExitCode int
	Err      error
}

func (e *HookFailedError) Error() string {
	return fmt.Sprintf("%s hook failed with exit code %d: %v", e.Hook, e.ExitCode, e.Err)
}

func (e *HookFailedError) Unwrap() error { return e.Err }

// SetupHook is the `worktree.setup-hook` capability.
type SetupHook interface {
	RunSetup(ctx context.Context, env HookEnv) (HookResult, error)
}

// ArchiveHook is the `worktree.archive-hook` capability. A non-zero exit or an
// error blocks the removal.
type ArchiveHook interface {
	RunArchive(ctx context.Context, env HookEnv) (HookResult, error)
}

// BlockedReasoner is the `status.blocked-reason` capability: Status carries
// BlockedReason for a blocked agent. The method is a marker.
type BlockedReasoner interface {
	ProvidesBlockedReason()
}

// StateDirer is the `state.dir` capability: name the run-state directory.
type StateDirer interface {
	StateDir() (string, bool)
}

// SelfEnver is the `identity.env` capability: the variables a backend could not
// pass to an agent at launch (ORCH_ROLE, ORCH_TASK_DIR, ...), read back by the
// agent's own orch process at startup. See bridgeSelfEnv.
type SelfEnver interface {
	SelfEnv(ctx context.Context) (map[string]string, error)
}

// noSelfEnv is orch's fallback: no env to add.
type noSelfEnv struct{}

func (noSelfEnv) SelfEnv(context.Context) (map[string]string, error) { return nil, nil }

// Reserved contracts. Their names are taken; no backend implements them yet.

// Waker is the `wake` capability: wake the orchestrator on a lead's report.
type Waker interface {
	Wake(ctx context.Context, h Handle, message string) error
}

// Gater is the `gate` capability: host a decision gate.
type Gater interface {
	OpenGate(ctx context.Context, id, question string) error
}

// Dispatcher is the `dispatch` capability: hand a task to an agent. It never
// offers lead-to-lead messaging.
type Dispatcher interface {
	Dispatch(ctx context.Context, h Handle, task string) error
}

// Browser is the `browser` capability: the backend's browser for ui-verifier.
type Browser interface {
	BrowserURL(ctx context.Context) (string, error)
}

// Capability names.
const (
	CapLayout         = "layout"
	CapWorktreeManage = "worktree.manage"
	CapSetupHook      = "worktree.setup-hook"
	CapArchiveHook    = "worktree.archive-hook"
	CapBlockedReason  = "status.blocked-reason"
	CapStateDir       = "state.dir"
	CapWake           = "wake"
	CapGate           = "gate"
	CapDispatch       = "dispatch"
	CapBrowser        = "browser"
	CapIdentityEnv    = "identity.env"
)

// capabilityDef is one registry row.
type capabilityDef struct {
	Name     string
	Contract reflect.Type
	// Fallback is orch's own implementation, used when the backend does not
	// declare the capability or config lists it out. Nil means "no fallback
	// behaviour": callers skip the feature (layout, browser) or the owning
	// package installs one with setCapabilityFallback.
	Fallback any
	// FallbackDoc says what the fallback does.
	FallbackDoc string
}

func contractOf[T any]() reflect.Type { return reflect.TypeFor[T]() }

// defaultStateDirFallback is orch's own run-state directory.
type defaultStateDirFallback struct{}

func (defaultStateDirFallback) StateDir() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".local", "state", "orchestrate"), true
}

var (
	capMu       sync.RWMutex
	capRegistry = map[string]*capabilityDef{
		CapLayout:         {Name: CapLayout, Contract: contractOf[Layout](), FallbackDoc: "separate starts, no placement"},
		CapWorktreeManage: {Name: CapWorktreeManage, Contract: contractOf[WorktreeManager](), FallbackDoc: "orch runs git worktree"},
		CapSetupHook:      {Name: CapSetupHook, Contract: contractOf[SetupHook](), FallbackDoc: "orch runs the setup script"},
		CapArchiveHook:    {Name: CapArchiveHook, Contract: contractOf[ArchiveHook](), FallbackDoc: "orch runs the archive script"},
		CapBlockedReason:  {Name: CapBlockedReason, Contract: contractOf[BlockedReasoner](), FallbackDoc: "none, blocked only"},
		CapStateDir:       {Name: CapStateDir, Contract: contractOf[StateDirer](), Fallback: defaultStateDirFallback{}, FallbackDoc: "~/.local/state/orchestrate"},
		CapWake:           {Name: CapWake, Contract: contractOf[Waker](), FallbackDoc: "orch's own wake"},
		CapGate:           {Name: CapGate, Contract: contractOf[Gater](), FallbackDoc: "orch's own gate"},
		CapDispatch:       {Name: CapDispatch, Contract: contractOf[Dispatcher](), FallbackDoc: "orch's own dispatch"},
		CapBrowser:        {Name: CapBrowser, Contract: contractOf[Browser](), FallbackDoc: "none"},
		CapIdentityEnv:    {Name: CapIdentityEnv, Contract: contractOf[SelfEnver](), Fallback: noSelfEnv{}, FallbackDoc: "no env, nil"},
	}
)

// setCapabilityFallback installs orch's own implementation for name. It must
// satisfy the capability's contract.
func setCapabilityFallback(name string, impl any) error {
	capMu.Lock()
	defer capMu.Unlock()
	def, ok := capRegistry[name]
	if !ok {
		return fail("unknown capability %q", name)
	}
	if impl != nil && !reflect.TypeOf(impl).Implements(def.Contract) {
		return fail("fallback for capability %q does not implement %s", name, def.Contract)
	}
	def.Fallback = impl
	return nil
}

func lookupCapability(name string) (capabilityDef, bool) {
	capMu.RLock()
	defer capMu.RUnlock()
	def, ok := capRegistry[name]
	if !ok {
		return capabilityDef{}, false
	}
	return *def, true
}

// capabilityNames lists every registered capability name, sorted.
func capabilityNames() []string {
	capMu.RLock()
	defer capMu.RUnlock()
	names := make([]string, 0, len(capRegistry))
	for n := range capRegistry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// checkDeclared fails when b declares an unknown capability or one whose
// contract it does not implement.
func checkDeclared(b Backend) error {
	for _, name := range b.Declares() {
		def, ok := lookupCapability(name)
		if !ok {
			return fail("backend %s declares unknown capability %q", b.Name(), name)
		}
		if !reflect.TypeOf(b).Implements(def.Contract) {
			return fail("backend %s declares capability %q but does not implement %s", b.Name(), name, def.Contract)
		}
	}
	return nil
}

// Has reports whether capability name runs natively on this backend: the
// backend declares it and config does not list it out. Core code asks this and
// never compares backend names.
func (a *Active) Has(name string) bool {
	if !slices.Contains(a.Declares(), name) {
		return false
	}
	def, ok := lookupCapability(name)
	if !ok || !reflect.TypeOf(a.Backend).Implements(def.Contract) {
		return false
	}
	if a.nativeSet {
		return slices.Contains(a.native, name)
	}
	return true
}

// Capability returns the implementation to use for name: the backend itself
// when Has, otherwise orch's fallback (which may be nil). native says which.
func (a *Active) Capability(name string) (impl any, native bool) {
	if a.Has(name) {
		return a.Backend, true
	}
	def, _ := lookupCapability(name)
	return def.Fallback, false
}

// capabilityAs is Capability with the contract type applied. ok is false when
// neither a native nor a fallback implementation exists.
func capabilityAs[T any](a *Active, name string) (impl T, native, ok bool) {
	v, native := a.Capability(name)
	impl, ok = v.(T)
	return impl, native, ok
}

// backendNativeKey returns the config key for a backend's native list.
func backendNativeKey(name string) string { return "backend." + name + ".native" }
