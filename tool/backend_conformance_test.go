package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// BackendFactory returns a fresh backend for one check. Backends that need a
// scratch directory or a fake binary take it from t.
type BackendFactory func(t testing.TB) Backend

type conformanceCheck struct {
	name string
	run  func(ctx context.Context, b Backend) error
}

// RunConformance runs the shared suite against a backend factory: every
// required operation, then each capability the backend declares. A new backend
// calls it from its own test file.
func RunConformance(t *testing.T, factory BackendFactory) {
	t.Helper()
	for _, c := range conformanceChecks() {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.run(ctx, factory(t)); err != nil {
				t.Error(err)
			}
		})
	}
}

// conformanceFailures runs the suite and returns each failed check's error,
// for tests that expect a backend to fail.
func conformanceFailures(t testing.TB, factory BackendFactory) map[string]error {
	t.Helper()
	failed := map[string]error{}
	for _, c := range conformanceChecks() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := c.run(ctx, factory(t)); err != nil {
			failed[c.name] = err
		}
		cancel()
	}
	return failed
}

func startOne(ctx context.Context, b Backend, name string) (Identity, error) {
	id, err := b.Start(ctx, Spec{Agent: "claude", Harness: "claude", Cwd: ".", Prompt: "hello " + name, Name: name})
	if err != nil {
		return id, fmt.Errorf("Start: %w", err)
	}
	if id.ID == "" || id.Handle == "" {
		return id, fmt.Errorf("Start returned an incomplete identity %+v", id)
	}
	if id.Backend != b.Name() {
		return id, fmt.Errorf("Start identity names backend %q, want %q", id.Backend, b.Name())
	}
	return id, nil
}

func checkStatus(st AgentStatus) error {
	if !st.State.valid() {
		return fmt.Errorf("state %q is not one of idle, working, blocked, done, unknown", st.State)
	}
	if st.BlockedReason != "" && st.State != StateBlocked {
		return fmt.Errorf("blocked reason %q set on state %q", st.BlockedReason, st.State)
	}
	return nil
}

func conformanceChecks() []conformanceCheck {
	checks := []conformanceCheck{
		{"Name", func(ctx context.Context, b Backend) error {
			if b.Name() == "" {
				return errors.New("Name is empty")
			}
			return nil
		}},
		{"Self", func(ctx context.Context, b Backend) error {
			id, err := b.Self(ctx)
			if errors.Is(err, ErrNotInside) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("Self: %w", err)
			}
			if id.ID == "" {
				return errors.New("Self returned an empty ID")
			}
			return nil
		}},
		{"StartStatus", func(ctx context.Context, b Backend) error {
			id, err := startOne(ctx, b, "a")
			if err != nil {
				return err
			}
			st, err := b.Status(ctx, id.Handle)
			if err != nil {
				return fmt.Errorf("Status: %w", err)
			}
			return checkStatus(st)
		}},
		{"PromptRead", func(ctx context.Context, b Backend) error {
			id, err := startOne(ctx, b, "a")
			if err != nil {
				return err
			}
			if err := b.Prompt(ctx, id.Handle, "ping-42"); err != nil {
				return fmt.Errorf("Prompt: %w", err)
			}
			out, err := b.Read(ctx, id.Handle, 50)
			if err != nil {
				return fmt.Errorf("Read: %w", err)
			}
			if !strings.Contains(out, "ping-42") {
				return fmt.Errorf("Read did not return the prompted text: %q", out)
			}
			return nil
		}},
		{"ListRenameFocus", func(ctx context.Context, b Backend) error {
			id, err := startOne(ctx, b, "before")
			if err != nil {
				return err
			}
			if err := b.Rename(ctx, id.Handle, "after"); err != nil {
				return fmt.Errorf("Rename: %w", err)
			}
			if err := b.Focus(ctx, id.Handle); err != nil {
				return fmt.Errorf("Focus: %w", err)
			}
			list, err := b.List(ctx)
			if err != nil {
				return fmt.Errorf("List: %w", err)
			}
			i := slices.IndexFunc(list, func(a AgentInfo) bool { return a.Handle == id.Handle })
			if i < 0 {
				return fmt.Errorf("List does not include %q", id.Handle)
			}
			if list[i].Name != "after" {
				return fmt.Errorf("List name is %q after Rename, want after", list[i].Name)
			}
			for _, a := range list {
				if err := checkStatus(a.Status); err != nil {
					return fmt.Errorf("List row %q: %w", a.Handle, err)
				}
			}
			return nil
		}},
		{"Wait", func(ctx context.Context, b Backend) error {
			id, err := startOne(ctx, b, "a")
			if err != nil {
				return err
			}
			st, err := b.Status(ctx, id.Handle)
			if err != nil {
				return fmt.Errorf("Status: %w", err)
			}
			got, err := b.Wait(ctx, id.Handle, []State{st.State}, time.Second)
			if err != nil {
				return fmt.Errorf("Wait for the current state %q: %w", st.State, err)
			}
			return checkStatus(got)
		}},
		{"ResolveClose", func(ctx context.Context, b Backend) error {
			id, err := startOne(ctx, b, "a")
			if err != nil {
				return err
			}
			h, err := b.Resolve(ctx, id.Handle)
			if err != nil || h == "" {
				return fmt.Errorf("Resolve of a live handle gave %q, %v", h, err)
			}
			if err := b.Close(ctx, h); err != nil {
				return fmt.Errorf("Close: %w", err)
			}
			if _, err := b.Resolve(ctx, id.Handle); !errors.Is(err, ErrGone) {
				return fmt.Errorf("Resolve after Close gave %v, want ErrGone", err)
			}
			if _, err := b.Status(ctx, id.Handle); !errors.Is(err, ErrGone) {
				return fmt.Errorf("Status after Close gave %v, want ErrGone", err)
			}
			return nil
		}},
		{"ResolveUnknown", func(ctx context.Context, b Backend) error {
			if _, err := b.Resolve(ctx, "no-such-handle"); !errors.Is(err, ErrGone) {
				return fmt.Errorf("Resolve of an unknown handle gave %v, want ErrGone", err)
			}
			return nil
		}},
		{"Declares", func(ctx context.Context, b Backend) error { return checkDeclared(b) }},
	}
	// One check per capability, run only when the backend declares it.
	for _, name := range capabilityNames() {
		checks = append(checks, conformanceCheck{"Capability/" + name, func(ctx context.Context, b Backend) error {
			if !slices.Contains(b.Declares(), name) {
				return nil
			}
			return capabilityConformance(ctx, b, name)
		}})
	}
	return checks
}

// capabilityConformance exercises one declared capability's contract.
func capabilityConformance(ctx context.Context, b Backend, name string) error {
	switch name {
	case CapLayout:
		l, ok := b.(Layout)
		if !ok {
			return errors.New("declares layout but does not implement Layout")
		}
		anchor, err := startOne(ctx, b, "anchor")
		if err != nil {
			return err
		}
		split, err := l.Split(ctx, anchor.Handle, Spec{Agent: "claude", Cwd: ".", Name: "split"})
		if err != nil || split.Handle == "" {
			return fmt.Errorf("Split: %+v, %v", split, err)
		}
		tab, err := l.NewTab(ctx, "tab", Spec{Agent: "claude", Cwd: "."})
		if err != nil || tab.Handle == "" {
			return fmt.Errorf("NewTab: %+v, %v", tab, err)
		}
		for _, h := range []Handle{split.Handle, tab.Handle} {
			if _, err := b.Resolve(ctx, h); err != nil {
				return fmt.Errorf("handle %q from layout does not resolve: %w", h, err)
			}
		}
	case CapWorktreeManage:
		m, ok := b.(WorktreeManager)
		if !ok {
			return errors.New("declares worktree.manage but does not implement WorktreeManager")
		}
		w, err := m.CreateWorktree(ctx, WorktreeSpec{Name: "t1", Branch: "orch/t1", Base: "main"})
		if err != nil || w.Path == "" {
			return fmt.Errorf("CreateWorktree: %+v, %v", w, err)
		}
		if _, err := m.OpenWorktree(ctx, w.Path); err != nil {
			return fmt.Errorf("OpenWorktree: %w", err)
		}
		if err := m.RemoveWorktree(ctx, w.Path); err != nil {
			return fmt.Errorf("RemoveWorktree: %w", err)
		}
	case CapSetupHook, CapArchiveHook:
		env := HookEnv{RootPath: "/r", WorktreePath: "/w", WorkspaceName: "n"}
		var res HookResult
		var err error
		if name == CapSetupHook {
			h, ok := b.(SetupHook)
			if !ok {
				return errors.New("declares worktree.setup-hook but does not implement SetupHook")
			}
			res, err = h.RunSetup(ctx, env)
		} else {
			h, ok := b.(ArchiveHook)
			if !ok {
				return errors.New("declares worktree.archive-hook but does not implement ArchiveHook")
			}
			res, err = h.RunArchive(ctx, env)
		}
		if err != nil && !errors.Is(err, ErrHookRefused) {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err == nil && res.Status != HookStarted && res.Status != HookDone {
			return fmt.Errorf("%s returned status %q, want started or done", name, res.Status)
		}
	case CapBlockedReason:
		if _, ok := b.(BlockedReasoner); !ok {
			return errors.New("declares status.blocked-reason but does not implement BlockedReasoner")
		}
	case CapStateDir:
		s, ok := b.(StateDirer)
		if !ok {
			return errors.New("declares state.dir but does not implement StateDirer")
		}
		if dir, ok := s.StateDir(); !ok || dir == "" {
			return fmt.Errorf("StateDir returned %q, %v", dir, ok)
		}
	default:
		// Reserved names: the contract type is the whole check.
		return checkDeclared(b)
	}
	return nil
}

func TestConformanceFake(t *testing.T) {
	RunConformance(t, func(t testing.TB) Backend {
		return newFakeBackend("fakemux", CapLayout, CapWorktreeManage, CapSetupHook, CapArchiveHook, CapBlockedReason, CapStateDir)
	})
}

func TestConformanceFakeNoCapabilities(t *testing.T) {
	RunConformance(t, func(t testing.TB) Backend { return newFakeBackend("fakemux") })
}

func TestConformanceFailsOnRawNativeStatus(t *testing.T) {
	failed := conformanceFailures(t, func(t testing.TB) Backend {
		f := newFakeBackend("fakemux")
		f.rawStatus = true
		return f
	})
	err, ok := failed["StartStatus"]
	if !ok {
		t.Fatalf("suite passed a backend that returns a raw native status; failures: %v", failed)
	}
	if !strings.Contains(err.Error(), "is not one of") {
		t.Errorf("unexpected failure text: %v", err)
	}
}

func TestConformanceFailsOnUndeclaredContract(t *testing.T) {
	// A backend that declares state.dir but embeds only the Backend interface
	// does not implement StateDirer.
	failed := conformanceFailures(t, func(t testing.TB) Backend {
		return &lyingBackend{Backend: newFakeBackend("fakemux")}
	})
	if _, ok := failed["Declares"]; !ok {
		t.Fatalf("suite passed a backend that declares a capability it does not implement: %v", failed)
	}
}

// lyingBackend declares state.dir without implementing StateDirer.
type lyingBackend struct{ Backend }

func (lyingBackend) Declares() []string { return []string{CapStateDir} }
