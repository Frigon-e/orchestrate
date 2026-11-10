package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake `orca` when ORCH_FAKE_ORCA names its state
// file. It speaks the {"ok","result"|"error"} envelope the real CLI does, so the
// backend runs unchanged against it. No test touches a real Orca.

type orcaFakeTerm struct {
	Handle   string `json:"handle"`
	PaneKey  string `json:"pane_key"`
	Title    string `json:"title"`
	Worktree string `json:"worktree"`
	Agent    string `json:"agent"`
	State    string `json:"state"`
	// Wait is how agentWait renders: "null", "absent", or a raw JSON value.
	Wait   string   `json:"wait"`
	Reason string   `json:"reason"`
	Out    []string `json:"out"`
	Exited bool     `json:"exited"`
	// Unlisted hides the terminal from `terminal list` (its ps row stays).
	Unlisted bool `json:"unlisted"`
}

type orcaFakeTree struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

type orcaFakeState struct {
	Next  int             `json:"next"`
	Terms []*orcaFakeTerm `json:"terms"`
	Trees []*orcaFakeTree `json:"trees"`
	Calls [][]string      `json:"calls"`
	// Stale holds handles Orca no longer accepts (after a restart).
	Stale []string `json:"stale"`
	// ArchiveExit is the exit code of the archive hook run by `rm --run-hooks`.
	ArchiveExit int `json:"archive_exit"`
	// RefuseRunHooks makes `rm --run-hooks` fail as an unsupported flag.
	RefuseRunHooks bool `json:"refuse_run_hooks"`
	Dirty          bool `json:"dirty"`
}

func init() {
	if p := os.Getenv("ORCH_FAKE_ORCA"); p != "" {
		os.Exit(fakeOrca(p, os.Args[1:]))
	}
	// Tests must not depend on the multiplexer they run in: a developer running
	// them from an Orca terminal would otherwise get Orca detected everywhere.
	// A test that needs ORCA_* sets it with t.Setenv.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ORCA_") {
			os.Unsetenv(k)
		}
	}
}

func loadOrcaFake(path string) *orcaFakeState {
	st := &orcaFakeState{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, st)
	}
	return st
}

func saveOrcaFake(path string, st *orcaFakeState) {
	b, _ := json.Marshal(st)
	_ = os.WriteFile(path, b, 0o644)
}

func fakeOrca(path string, args []string) int {
	st := loadOrcaFake(path)
	st.Calls = append(st.Calls, slices.Clone(args))
	defer saveOrcaFake(path, st)
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--json" })
	val := func(flag string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	has := func(flag string) bool { return slices.Contains(args, flag) }
	reply := func(result any) int {
		out, _ := json.Marshal(map[string]any{"id": "r", "ok": true, "result": result, "_meta": map[string]any{}})
		fmt.Println(string(out))
		return 0
	}
	errReply := func(code, msg string, data map[string]any) int {
		out, _ := json.Marshal(map[string]any{"id": "r", "ok": false, "error": map[string]any{"code": code, "message": msg, "data": data}, "_meta": map[string]any{}})
		fmt.Println(string(out))
		return 1
	}
	if len(args) < 2 {
		return errReply("usage", "no command", nil)
	}
	cmd := args[0] + " " + args[1]
	term := func() (*orcaFakeTerm, int) {
		h := val("--terminal")
		if slices.Contains(st.Stale, h) {
			return nil, errReply("terminal_handle_stale", "handle "+h+" is stale", nil)
		}
		for _, t := range st.Terms {
			if t.Handle == h {
				return t, 0
			}
		}
		return nil, errReply("terminal_not_found", "no terminal "+h, nil)
	}
	switch cmd {
	case "agent launch":
		st.Next++
		wt := strings.TrimPrefix(val("--worktree"), "path:")
		if !slices.ContainsFunc(st.Trees, func(w *orcaFakeTree) bool { return w.Path == wt }) {
			st.Trees = append(st.Trees, &orcaFakeTree{ID: "repo1::" + wt, Path: wt})
		}
		t := &orcaFakeTerm{Handle: fmt.Sprintf("term_%d", st.Next), PaneKey: fmt.Sprintf("pane:%d", st.Next),
			Worktree: strings.TrimPrefix(val("--worktree"), "path:"), Agent: val("--agent"), State: "working", Wait: "null", Out: []string{"started", val("--prompt")}}
		st.Terms = append(st.Terms, t)
		return reply(map[string]any{"outcome": map[string]any{"kind": "terminal", "handle": t.Handle}})
	case "terminal create":
		st.Next++
		wt := strings.TrimPrefix(val("--worktree"), "path:")
		if !slices.ContainsFunc(st.Trees, func(w *orcaFakeTree) bool { return w.Path == wt }) {
			st.Trees = append(st.Trees, &orcaFakeTree{ID: "repo1::" + wt, Path: wt})
		}
		// The command is `sh '<script>'`; the script's first line names the agent.
		script, _ := os.ReadFile(strings.Trim(strings.TrimPrefix(val("--command"), "sh "), "'"))
		first, _, _ := strings.Cut(string(script), "\n")
		t := &orcaFakeTerm{Handle: fmt.Sprintf("term_%d", st.Next), PaneKey: fmt.Sprintf("pane:%d", st.Next), Title: val("--title"),
			Worktree: wt, Agent: strings.TrimPrefix(first, "# orch orca launch "), State: "working", Wait: "null", Out: []string{"started", string(script)}}
		st.Terms = append(st.Terms, t)
		return reply(map[string]any{"terminal": map[string]any{"handle": t.Handle, "tabId": "tab", "paneKey": t.PaneKey, "worktreeId": "repo1::" + wt}})
	case "terminal list":
		var rows []map[string]any
		for _, t := range st.Terms {
			if t.Unlisted {
				continue
			}
			rows = append(rows, map[string]any{"handle": t.Handle, "worktreePath": t.Worktree, "tabId": "tab", "leafId": "leaf", "title": t.Title})
		}
		return reply(map[string]any{"terminals": rows})
	case "terminal show":
		t, code := term()
		if t == nil {
			return code
		}
		return reply(map[string]any{"terminal": map[string]any{"handle": t.Handle, "title": t.Title}, "recordedPaneKey": t.PaneKey})
	case "terminal read":
		t, code := term()
		if t == nil {
			return code
		}
		return reply(map[string]any{"terminal": map[string]any{"handle": t.Handle, "status": "running", "tail": t.Out}})
	case "terminal send":
		t, code := term()
		if t == nil {
			return code
		}
		t.Out = append(t.Out, val("--text"))
		return reply(map[string]any{})
	case "terminal rename":
		t, code := term()
		if t == nil {
			return code
		}
		t.Title = val("--title")
		return reply(map[string]any{})
	case "terminal switch":
		if t, code := term(); t == nil {
			return code
		}
		return reply(map[string]any{})
	case "terminal close":
		t, code := term()
		if t == nil {
			return code
		}
		st.Terms = slices.DeleteFunc(st.Terms, func(x *orcaFakeTerm) bool { return x == t })
		return reply(map[string]any{})
	case "terminal wait":
		t, code := term()
		if t == nil {
			return code
		}
		if val("--for") == "exit" && t.Exited {
			return reply(map[string]any{})
		}
		return errReply("timeout", "timed out", nil)
	case "worktree ps":
		var rows []map[string]any
		for _, w := range st.Trees {
			var agents []map[string]any
			for _, t := range st.Terms {
				if t.Worktree != w.Path || t.Agent == "" {
					continue
				}
				a := map[string]any{"state": t.State, "agentType": t.Agent, "paneKey": t.PaneKey, "title": t.Title}
				switch t.Wait {
				case "null":
					a["agentWait"] = nil
				case "absent":
				default:
					a["agentWait"] = json.RawMessage(t.Wait)
				}
				if t.Reason != "" {
					a["blockedReason"] = t.Reason
				}
				agents = append(agents, a)
			}
			rows = append(rows, map[string]any{"id": w.ID, "path": w.Path, "branch": w.Branch, "agents": agents})
		}
		return reply(map[string]any{"worktrees": rows})
	case "worktree create":
		p := filepath.Join(filepath.Dir(path), "orca-home", "workspaces", val("--name"))
		_ = os.MkdirAll(p, 0o755)
		w := &orcaFakeTree{ID: "repo1::" + p, Path: p, Branch: val("--branch")}
		st.Trees = append(st.Trees, w)
		return reply(map[string]any{"worktree": w})
	case "worktree rm":
		p := strings.TrimPrefix(val("--worktree"), "path:")
		if !slices.ContainsFunc(st.Trees, func(w *orcaFakeTree) bool { return w.Path == p }) {
			return errReply("worktree_not_found", "no worktree", nil)
		}
		if st.Dirty {
			return errReply("worktree_dirty", "worktree has uncommitted changes", nil)
		}
		if has("--run-hooks") && st.RefuseRunHooks {
			return errReply("unknown_flag", "unknown flag --run-hooks", nil)
		}
		if has("--run-hooks") && st.ArchiveExit != 0 {
			return errReply("worktree_archive_hook_failed", "archive hook failed", map[string]any{"exitCode": st.ArchiveExit})
		}
		st.Trees = slices.DeleteFunc(st.Trees, func(w *orcaFakeTree) bool { return w.Path == p })
		return reply(map[string]any{})
	}
	return errReply("unknown_command", cmd, nil)
}

type orcaHarness struct {
	t     testing.TB
	state string
	tmp   string
}

func newOrcaHarness(t testing.TB) *orcaHarness {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &orcaHarness{t: t, tmp: tmp, state: filepath.Join(tmp, "orca.json")}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ORCA_") {
			t.Setenv(k, "")
		}
	}
	t.Setenv("ORCH_ORCA", os.Args[0])
	t.Setenv("ORCH_FAKE_ORCA", h.state)
	t.Setenv("ORCH_IDENTITY_DIR", filepath.Join(tmp, "identities"))
	old := orcaPoll
	orcaPoll = time.Millisecond
	t.Cleanup(func() { orcaPoll = old })
	return h
}

func (h *orcaHarness) load() *orcaFakeState { return loadOrcaFake(h.state) }

func (h *orcaHarness) edit(f func(*orcaFakeState)) {
	st := h.load()
	f(st)
	saveOrcaFake(h.state, st)
}

// calls returns every recorded orca call whose first two words are cmd.
func (h *orcaHarness) calls(cmd string) [][]string {
	var out [][]string
	for _, c := range h.load().Calls {
		if len(c) >= 2 && c[0]+" "+c[1] == cmd {
			out = append(out, c)
		}
	}
	return out
}

func (h *orcaHarness) term(handle Handle) *orcaFakeTerm {
	for _, t := range h.load().Terms {
		if t.Handle == string(handle) {
			return t
		}
	}
	h.t.Fatalf("fake has no terminal %s", handle)
	return nil
}

func (h *orcaHarness) startAgent(b Backend) Identity {
	h.t.Helper()
	wt := filepath.Join(h.tmp, "wt")
	h.edit(func(st *orcaFakeState) {
		if !slices.ContainsFunc(st.Trees, func(w *orcaFakeTree) bool { return w.Path == wt }) {
			st.Trees = append(st.Trees, &orcaFakeTree{ID: "repo1::" + wt, Path: wt, Branch: "orch/t1"})
		}
	})
	id, err := b.Start(context.Background(), Spec{Agent: "claude", Harness: "claude", Cwd: wt, Prompt: "go", Name: "lead-t1"})
	if err != nil {
		h.t.Fatalf("Start: %v", err)
	}
	return id
}

func orcaActive(native ...string) *Active {
	a := &Active{Backend: newOrcaBackend()}
	if native != nil {
		a.native, a.nativeSet = native, true
	}
	return a
}

func TestConformanceOrca(t *testing.T) {
	newOrcaHarness(t)
	RunConformance(t, func(t testing.TB) Backend { return newOrcaBackend() })
}

func TestOrcaDetectionAndSelection(t *testing.T) {
	for _, key := range []string{"ORCA_AGENT_SESSION_ID", "ORCA_PANE_KEY", "ORCA_TERMINAL_HANDLE"} {
		a, err := selectBackend([]string{key + "=x"}, nil)
		if err != nil || a.Name() != "orca" {
			t.Errorf("env %s selected %v, %v; want orca", key, a, err)
		}
	}
	if _, err := selectBackend([]string{"ORCA_UNRELATED=1"}, nil); err == nil || !strings.Contains(err.Error(), "orca") {
		t.Errorf("unrelated ORCA_ var: err = %v, want the registry message naming orca", err)
	}
	both := []string{"HERDR_ENV=1", "ORCA_PANE_KEY=p"}
	if a, err := selectBackend(both, nil); err != nil || a.Name() != "herdr" {
		t.Errorf("both detected without ORCH_BACKEND = %v, %v; want herdr (registration order)", a, err)
	}
	if a, err := selectBackend(append(both, "ORCH_BACKEND=orca"), nil); err != nil || a.Name() != "orca" {
		t.Errorf("ORCH_BACKEND=orca with both = %v, %v; want orca", a, err)
	}
	// worktree.manage is on by default; listing it out turns it off.
	a, _ := selectBackend([]string{"ORCA_PANE_KEY=p"}, nil)
	for _, c := range []string{CapWorktreeManage, CapSetupHook, CapArchiveHook, CapBlockedReason} {
		if !a.Has(c) {
			t.Errorf("orca should have %s by default", c)
		}
	}
	if a.Has(CapLayout) {
		t.Error("orca must not declare layout")
	}
	off, _ := selectBackend([]string{"ORCA_PANE_KEY=p"}, backendConfig{"orca": {CapSetupHook}})
	if off.Has(CapWorktreeManage) || !off.Has(CapSetupHook) {
		t.Error("backend.orca.native = [worktree.setup-hook] should turn worktree.manage off and keep setup")
	}
}

// A lead started by the herdr backend from an Orca terminal gets no ORCA_*.
func TestHerdrChildFromOrcaTerminalHasNoOrcaEnv(t *testing.T) {
	args := herdrEnvArgs(map[string]string{"ORCA_PANE_KEY": "p", "ORCA_TERMINAL_HANDLE": "h", "ORCH_ROLE": "lead"})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "ORCA_") || !strings.Contains(joined, "ORCH_ROLE=lead") {
		t.Errorf("herdr --env args = %q, want ORCH_ROLE only", joined)
	}
	env := childEnv("herdr", []string{"ORCA_PANE_KEY=p", "HERDR_PANE_ID=w:1", "PATH=/bin"}, nil)
	if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "ORCA_") }) || !slices.Contains(env, "HERDR_PANE_ID=w:1") {
		t.Errorf("childEnv(herdr) = %v", env)
	}
}

func TestOrcaStatusMapping(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	id := h.startAgent(b)
	set := func(f func(*orcaFakeTerm)) {
		h.edit(func(st *orcaFakeState) { f(st.Terms[0]) })
	}
	cases := []struct {
		name string
		f    func(*orcaFakeTerm)
		want AgentStatus
	}{
		{"working", func(x *orcaFakeTerm) { x.State, x.Wait, x.Reason = "working", "null", "" }, AgentStatus{State: StateWorking}},
		{"idle", func(x *orcaFakeTerm) { x.State, x.Wait, x.Reason = "idle", "null", "" }, AgentStatus{State: StateIdle}},
		{"done", func(x *orcaFakeTerm) { x.State, x.Wait, x.Reason = "done", "null", "" }, AgentStatus{State: StateDone}},
		{"prompt blockedReason", func(x *orcaFakeTerm) { x.State, x.Wait, x.Reason = "working", "null", "Bypass Permissions prompt" }, AgentStatus{State: StateBlocked, BlockedReason: "Bypass Permissions prompt"}},
		{"agentWait object", func(x *orcaFakeTerm) { x.State, x.Wait, x.Reason = "working", `{"reason":"needs a human"}`, "" }, AgentStatus{State: StateBlocked, BlockedReason: "needs a human"}},
		{"unrecognised state", func(x *orcaFakeTerm) { x.State, x.Wait, x.Reason = "hibernating", "null", "" }, AgentStatus{State: StateUnknown}},
		{"absent agentWait", func(x *orcaFakeTerm) { x.State, x.Wait, x.Reason = "working", "absent", "" }, AgentStatus{State: StateUnknown}},
	}
	for _, c := range cases {
		set(c.f)
		got, err := b.Status(context.Background(), id.Handle)
		if err != nil || got != c.want {
			t.Errorf("%s: Status = %+v, %v; want %+v", c.name, got, err, c.want)
		}
		rows, err := b.List(context.Background())
		if err != nil || len(rows) != 1 || rows[0].Status != c.want {
			t.Errorf("%s: List = %+v, %v; want status %+v", c.name, rows, err, c.want)
		}
	}
}

// A launched agent has no ps row yet (Orca lists none while Claude waits at its
// confirmation prompt): it is unknown, never working.
func TestOrcaLaunchedAgentWithoutRowIsNotRunning(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	id := h.startAgent(b)
	h.edit(func(st *orcaFakeState) { st.Terms[0].Agent = "" }) // terminal exists, no agent row
	got, err := b.Status(context.Background(), id.Handle)
	if err != nil || got.State != StateUnknown {
		t.Fatalf("Status = %+v, %v; want unknown", got, err)
	}
}

func TestOrcaStartWithoutArgsOrEnvUsesAgentLaunchAndWritesIdentity(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	wt := filepath.Join(h.tmp, "wt")
	h.edit(func(st *orcaFakeState) { st.Trees = append(st.Trees, &orcaFakeTree{ID: "r::" + wt, Path: wt}) })
	id, err := b.Start(context.Background(), Spec{Agent: "claude", Harness: "claude", Cwd: wt, Prompt: "go", Name: "n", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	launch := h.calls("agent launch")
	if len(launch) != 1 {
		t.Fatalf("launch calls = %v", launch)
	}
	joined := strings.Join(launch[0], " ")
	for _, bad := range []string{"--env", "permission", "--dangerously", "leak"} {
		if strings.Contains(joined, bad) {
			t.Errorf("launch args %q contain %q", joined, bad)
		}
	}
	for _, want := range []string{"--surface terminal", "--worktree path:" + wt, "--model sonnet", "--prompt go"} {
		if !strings.Contains(joined, want) {
			t.Errorf("launch args %q lack %q", joined, want)
		}
	}
	if id.Backend != "orca" || id.Handle != "term_1" || id.ID != "pane:1" {
		t.Errorf("identity = %+v, want orca term_1 keyed by pane:1", id)
	}
	rec, ok := readOrcaIdentity(string(id.Handle))
	if !ok || rec.ID != id.ID || len(rec.Env) != 0 {
		t.Errorf("identity file = %+v, %v; want no env", rec, ok)
	}
	// The agent's own orch calls find the same identity from its terminal env.
	t.Setenv("ORCA_TERMINAL_HANDLE", "term_1")
	t.Setenv("ORCA_PANE_KEY", "pane:1")
	self, err := b.Self(context.Background())
	if err != nil || self.ID != id.ID {
		t.Errorf("Self = %+v, %v; want ID %s", self, err, id.ID)
	}
	if h.term(id.Handle).Title != "n" {
		t.Errorf("terminal title = %q, want n", h.term(id.Handle).Title)
	}
}

func TestOrcaLaunchHandleShapes(t *testing.T) {
	for _, c := range []struct {
		name, json, want, err string
	}{
		{"agent launch", `{"outcome":{"kind":"terminal","handle":"t1"}}`, "t1", ""},
		{"worktree create", `{"worktree":{},"launch":{"outcome":{"kind":"terminal","handle":"t2"}}}`, "t2", ""},
		{"older host", `{"agentTerminalHandle":"t3"}`, "t3", ""},
		{"chat", `{"outcome":{"kind":"chat","handle":"c1"}}`, "", "not a terminal"},
		{"no handle", `{"outcome":{"kind":"terminal"}}`, "", "no terminal handle"},
	} {
		got, err := orcaLaunchHandle(json.RawMessage(c.json))
		if c.err == "" && (err != nil || got != c.want) || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: got %q, %v; want %q / %q", c.name, got, err, c.want, c.err)
		}
	}
}

func TestOrcaStaleHandleResolvedOnceAndNeverUsedAgain(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	id := h.startAgent(b)
	// Orca restarts: the terminal lives on under a new handle, the old one is stale.
	h.edit(func(st *orcaFakeState) {
		st.Stale = append(st.Stale, "term_1")
		st.Terms[0].Handle = "term_9"
	})
	ctx := context.Background()
	before := len(h.load().Calls)
	if err := b.Prompt(ctx, id.Handle, "after-restart"); err != nil {
		t.Fatalf("Prompt through a stale handle: %v", err)
	}
	if got := h.term("term_9").Out; got[len(got)-1] != "after-restart" {
		t.Fatalf("prompt did not reach the replacement terminal: %v", got)
	}
	if _, err := b.Status(ctx, id.Handle); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if err := b.Prompt(ctx, id.Handle, "again"); err != nil {
		t.Fatal(err)
	}
	stale := 0
	for _, c := range h.load().Calls[before:] {
		if len(c) >= 2 && c[0] == "terminal" && c[1] != "list" && slices.Contains(c, "term_1") {
			stale++
		}
	}
	// Only the first send reached Orca with the old handle; every later call used term_9.
	if stale != 1 {
		t.Errorf("old handle used in %d terminal calls, want exactly 1 (the one that found it stale)", stale)
	}
	// A fresh process has no alias: the identity file's pane key finds the replacement.
	got, err := newOrcaBackend().Resolve(ctx, id.Handle)
	if err != nil || got != "term_9" {
		t.Errorf("Resolve from a fresh process = %q, %v; want term_9", got, err)
	}
}

func TestOrcaClosedTerminalIsGone(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	id := h.startAgent(b)
	if err := b.Close(context.Background(), id.Handle); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"Close":  b.Close(context.Background(), id.Handle),
		"Prompt": b.Prompt(context.Background(), id.Handle, "x"),
	} {
		if !errors.Is(err, ErrGone) {
			t.Errorf("%s of a closed terminal = %v, want ErrGone", name, err)
		}
	}
	if _, err := b.Resolve(context.Background(), id.Handle); !errors.Is(err, ErrGone) {
		t.Errorf("Resolve = %v, want ErrGone", err)
	}
}

func TestOrcaWaitForExitUsesTerminalWait(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	id := h.startAgent(b)
	ctx := context.Background()
	if _, err := b.Wait(ctx, id.Handle, []State{StateDone}, 30*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Wait on a running agent = %v, want a timeout", err)
	}
	if calls := h.calls("terminal wait"); len(calls) == 0 || !slices.Contains(calls[0], "exit") {
		t.Errorf("terminal wait calls = %v, want --for exit", calls)
	}
	h.edit(func(st *orcaFakeState) { st.Terms[0].Exited = true })
	got, err := b.Wait(ctx, id.Handle, []State{StateDone}, time.Second)
	if err != nil || got.State != StateDone {
		t.Errorf("Wait after exit = %+v, %v; want done", got, err)
	}
}

// worktree.manage and the hooks

func hookRepo(t *testing.T, tmp, setup, archive string) string {
	t.Helper()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[hooks]\nsetup = %q\narchive = %q\n", setup, archive)
	if err := os.WriteFile(filepath.Join(repo, ".agents", "orchestrate.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestOrcaCreateWorktreeIsParentlessAndReadsPathFromReply(t *testing.T) {
	h := newOrcaHarness(t)
	a := orcaActive()
	repo := hookRepo(t, h.tmp, "", "")
	mgr := a.Backend.(WorktreeManager)
	w, err := mgr.CreateWorktree(context.Background(), WorktreeSpec{Name: "t1", Branch: "orch/t1", Base: "main", Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(h.tmp, "orca-home", "workspaces", "t1") // the fake's choice, not orch's
	if w.Path != want || w.Branch != "orch/t1" || w.Workspace != Handle("repo1::"+want) {
		t.Errorf("worktree = %+v, want path %s", w, want)
	}
	args := strings.Join(h.calls("worktree create")[0], " ")
	for _, wantArg := range []string{"--name t1", "--branch orch/t1", "--base-branch main", "--no-parent", "--setup skip", "--repo path:" + repo} {
		if !strings.Contains(args, wantArg) {
			t.Errorf("create args %q lack %q", args, wantArg)
		}
	}
	got, err := mgr.OpenWorktrees(context.Background())
	if err != nil || got[want] == "" {
		t.Errorf("OpenWorktrees = %v, %v", got, err)
	}
	if _, err := mgr.OpenWorktree(context.Background(), WorktreeSpec{Path: filepath.Join(h.tmp, "nope")}); !errors.Is(err, ErrGone) {
		t.Errorf("OpenWorktree of an unknown path = %v, want ErrGone", err)
	}
}

func TestOrcaNativeSetupIsRequestedAndStartedNeverDone(t *testing.T) {
	h := newOrcaHarness(t)
	repo := hookRepo(t, h.tmp, "touch SHOULD_NOT_RUN", "")
	a := orcaActive()
	made, res, err := createWorktreeWithSetup(context.Background(), a, WorktreeSpec{Name: "t1", Branch: "orch/t1", Base: "main", Repo: repo},
		HookEnv{RootPath: repo, WorkspaceName: "t1"}, &scriptHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(h.calls("worktree create")[0], " "), "--setup run") {
		t.Errorf("create calls = %v, want --setup run", h.calls("worktree create"))
	}
	if res.Status != HookStarted {
		t.Errorf("native setup result = %+v, want Started", res)
	}
	if _, err := os.Stat(filepath.Join(made.Path, "SHOULD_NOT_RUN")); err == nil {
		t.Error("orch ran the setup script itself on the native path")
	}
}

func TestOrcaNativePathNeverReadsHookConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	h := newOrcaHarness(t)
	repo := hookRepo(t, h.tmp, "exit 1", "exit 1")
	yaml := filepath.Join(repo, "orca.yaml")
	if err := os.WriteFile(yaml, []byte("scripts:\n  setup: exit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(repo, ".agents", "orchestrate.toml")
	for _, p := range []string{yaml, cfg} {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(p, 0o644)
	}
	a := orcaActive()
	ctx := context.Background()
	env := HookEnv{RootPath: repo, WorkspaceName: "t1"}
	made, res, err := createWorktreeWithSetup(ctx, a, WorktreeSpec{Name: "t1", Branch: "orch/t1", Base: "main", Repo: repo}, env, &scriptHooks{})
	if err != nil || res.Status != HookStarted {
		t.Fatalf("native create with unreadable config = %+v, %v", res, err)
	}
	if err := removeWorktreeWithArchive(ctx, a, WorktreeSpec{Repo: repo, Path: made.Path}, env, nil, &scriptHooks{}); err != nil {
		t.Fatalf("native remove with unreadable config: %v", err)
	}
	// Control: the runner does read the config, so the unreadable file would have failed.
	if _, err := (&scriptHooks{}).RunSetup(ctx, HookEnv{RootPath: repo, WorktreePath: made.Path}); err == nil {
		t.Error("the fallback runner read an unreadable config without error; the control is void")
	}
}

func TestOrcaNativeArchiveFlagsAndChecksFirst(t *testing.T) {
	h := newOrcaHarness(t)
	repo := hookRepo(t, h.tmp, "", "touch SHOULD_NOT_RUN")
	a := orcaActive()
	ctx := context.Background()
	env := HookEnv{RootPath: repo, WorkspaceName: "t1"}
	made, _, err := createWorktreeWithSetup(ctx, a, WorktreeSpec{Name: "t1", Branch: "orch/t1", Base: "main", Repo: repo}, env, &scriptHooks{})
	if err != nil {
		t.Fatal(err)
	}
	spec := WorktreeSpec{Repo: repo, Path: made.Path}

	// A failed merged/clean check stops before any orca call or script.
	checkErr := errors.New("branch is not merged")
	err = removeWorktreeWithArchive(ctx, a, spec, env, func() error { return checkErr }, &scriptHooks{})
	if !errors.Is(err, checkErr) || len(h.calls("worktree rm")) != 0 {
		t.Fatalf("failed check: err = %v, rm calls = %v; want the check error and no rm", err, h.calls("worktree rm"))
	}

	var order []string
	checks := func() error {
		order = append(order, fmt.Sprintf("checks(rm calls so far: %d)", len(h.calls("worktree rm"))))
		return nil
	}
	if err := removeWorktreeWithArchive(ctx, a, spec, env, checks, &scriptHooks{}); err != nil {
		t.Fatal(err)
	}
	if len(order) != 1 || order[0] != "checks(rm calls so far: 0)" {
		t.Errorf("checks ran as %v, want once before any rm", order)
	}
	rm := h.calls("worktree rm")
	if len(rm) != 1 {
		t.Fatalf("rm calls = %v", rm)
	}
	joined := strings.Join(rm[0], " ")
	if !strings.Contains(joined, "--run-hooks") {
		t.Errorf("rm args %q lack --run-hooks", joined)
	}
	for _, bad := range []string{"--force", "--delete-branch", "--allow-failed-archive-hook"} {
		if strings.Contains(joined, bad) {
			t.Errorf("rm args %q contain %s", joined, bad)
		}
	}
	if _, err := os.Stat(filepath.Join(made.Path, "SHOULD_NOT_RUN")); err == nil {
		t.Error("orch ran the archive script itself on the native path")
	}
}

func TestOrcaFailingArchiveHookBlocksAndIsNotRetried(t *testing.T) {
	h := newOrcaHarness(t)
	repo := hookRepo(t, h.tmp, "", "touch RERAN")
	a := orcaActive()
	ctx := context.Background()
	env := HookEnv{RootPath: repo}
	made, _, err := createWorktreeWithSetup(ctx, a, WorktreeSpec{Name: "t1", Branch: "orch/t1", Base: "main", Repo: repo}, env, &scriptHooks{})
	if err != nil {
		t.Fatal(err)
	}
	h.edit(func(st *orcaFakeState) { st.ArchiveExit = 4 })
	err = removeWorktreeWithArchive(ctx, a, WorktreeSpec{Repo: repo, Path: made.Path}, env, nil, &scriptHooks{})
	hf, ok := errors.AsType[*HookFailedError](err)
	if !ok || hf.ExitCode != 4 {
		t.Fatalf("err = %v, want HookFailedError with exit 4", err)
	}
	rm := h.calls("worktree rm")
	if len(rm) != 1 || strings.Contains(strings.Join(rm[0], " "), "--allow-failed-archive-hook") {
		t.Errorf("rm calls = %v, want exactly one without --allow-failed-archive-hook", rm)
	}
	if got, _ := a.Backend.(WorktreeManager).OpenWorktrees(ctx); got[made.Path] == "" {
		t.Error("worktree was removed past a failing archive hook")
	}
	if _, err := os.Stat(filepath.Join(made.Path, "RERAN")); err == nil {
		t.Error("orch reran the archive hook itself after it failed natively")
	}
}

func TestOrcaRefusalBeforeRunningFallsBackToRunner(t *testing.T) {
	h := newOrcaHarness(t)
	repo := hookRepo(t, h.tmp, "touch SETUP_RAN", "touch ARCHIVE_RAN")
	a := orcaActive()
	ctx := context.Background()
	env := HookEnv{RootPath: repo, WorkspaceName: "t1"}

	// Setup: a worktree this backend did not create with --setup run was never set up.
	h.edit(func(st *orcaFakeState) {
		p := filepath.Join(h.tmp, "other")
		_ = os.MkdirAll(p, 0o755)
		st.Trees = append(st.Trees, &orcaFakeTree{ID: "r::" + p, Path: p})
	})
	env.WorktreePath = filepath.Join(h.tmp, "other")
	res, native, err := runWorktreeHook(ctx, a, "setup", env, &scriptHooks{})
	if err != nil || native || res.Status != HookDone || res.ExitCode != 0 {
		t.Fatalf("refused setup = %+v native=%v err=%v; want the runner's Done", res, native, err)
	}
	if _, err := os.Stat(filepath.Join(env.WorktreePath, "SETUP_RAN")); err != nil {
		t.Error("the fallback did not run the setup script")
	}

	// Archive: Orca rejects --run-hooks before running anything.
	made, _, err := createWorktreeWithSetup(ctx, a, WorktreeSpec{Name: "t1", Branch: "orch/t1", Base: "main", Repo: repo}, env, &scriptHooks{})
	if err != nil {
		t.Fatal(err)
	}
	h.edit(func(st *orcaFakeState) { st.RefuseRunHooks = true })
	env.WorktreePath = ""
	if err := removeWorktreeWithArchive(ctx, a, WorktreeSpec{Repo: repo, Path: made.Path}, env, nil, &scriptHooks{}); err != nil {
		t.Fatalf("remove after a refused --run-hooks: %v", err)
	}
	rm := h.calls("worktree rm")
	if len(rm) != 2 || !slices.Contains(rm[0], "--run-hooks") || slices.Contains(rm[1], "--run-hooks") {
		t.Errorf("rm calls = %v, want one refused with --run-hooks then one without", rm)
	}
	if _, err := os.Stat(filepath.Join(made.Path, "ARCHIVE_RAN")); err != nil {
		// the worktree directory is still on disk in the fake
		t.Error("the fallback did not run the archive script")
	}
}

func TestHookRunnerEnvCwdAndListedOutNative(t *testing.T) {
	h := newOrcaHarness(t)
	repo := hookRepo(t, h.tmp, `printf '%s|%s|%s|%s|%s|%s\n' "$ORCH_ROOT_PATH" "$ORCH_WORKTREE_PATH" "$ORCH_WORKSPACE_NAME" "$ORCH_BRANCH" "$ORCH_TASK_ID" "$(pwd -P)" > out.txt; env | grep -c '^ORCA_' >> out.txt || true`, "")
	t.Setenv("ORCA_PANE_KEY", "leak")
	// worktree.setup-hook listed out: the capability resolves to orch's runner.
	a := orcaActive(CapWorktreeManage)
	if a.Has(CapSetupHook) {
		t.Fatal("setup-hook should be off")
	}
	env := HookEnv{RootPath: repo, WorkspaceName: "t1"}
	made, res, err := createWorktreeWithSetup(context.Background(), a, WorktreeSpec{Name: "t1", Branch: "orch/t1", Base: "main", Repo: repo}, env,
		&scriptHooks{Branch: "orch/t1", TaskID: "t1"})
	if err != nil || res.Status != HookDone || res.ExitCode != 0 {
		t.Fatalf("runner setup = %+v, %v", res, err)
	}
	if !strings.Contains(strings.Join(h.calls("worktree create")[0], " "), "--setup skip") {
		t.Error("with setup-hook off, create must pass --setup skip")
	}
	got, err := os.ReadFile(filepath.Join(made.Path, "out.txt"))
	if err != nil {
		t.Fatalf("script did not run in the worktree: %v", err)
	}
	want := strings.Join([]string{repo, made.Path, "t1", "orch/t1", "t1", made.Path}, "|") + "\n0\n"
	if string(got) != want {
		t.Errorf("script saw %q, want %q", got, want)
	}
}

func TestHookRunnerExitCodeTimeoutAndNoScript(t *testing.T) {
	tmp := t.TempDir()
	repo := hookRepo(t, tmp, "exit 3", "")
	env := HookEnv{RootPath: repo, WorktreePath: tmp}
	res, err := (&scriptHooks{}).RunSetup(context.Background(), env)
	if err != nil || res != (HookResult{Status: HookDone, ExitCode: 3}) {
		t.Errorf("exit 3 = %+v, %v; want Done(3)", res, err)
	}
	res, err = (&scriptHooks{}).RunArchive(context.Background(), env)
	if err != nil || res != (HookResult{Status: HookDone}) {
		t.Errorf("no archive script = %+v, %v; want Done(0)", res, err)
	}
	old := hookTimeout
	hookTimeout = 50 * time.Millisecond
	defer func() { hookTimeout = old }()
	slow := hookRepo(t, t.TempDir(), "sleep 5", "")
	if _, err := (&scriptHooks{}).RunSetup(context.Background(), HookEnv{RootPath: slow, WorktreePath: tmp}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("slow script err = %v, want timeout", err)
	}
}

func TestHookScriptSources(t *testing.T) {
	repo := t.TempDir()
	yaml := "name: x\nscripts:\n  setup: |\n    npm ci\n    npm run build\n  archive: \"echo bye\"\nother: 1\n"
	if err := os.WriteFile(filepath.Join(repo, "orca.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]string{"setup": "npm ci\nnpm run build\n", "archive": "echo bye"} {
		if got, err := repoHookScript(repo, kind); err != nil || got != want {
			t.Errorf("orca.yaml %s = %q, %v; want %q", kind, got, err, want)
		}
	}
	// orch's own key wins over orca.yaml; an absent key falls through.
	if err := os.MkdirAll(filepath.Join(repo, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agents", "orchestrate.toml"), []byte("[hooks]\nsetup = \"make setup\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := repoHookScript(repo, "setup"); got != "make setup" {
		t.Errorf("setup = %q, want the orch key", got)
	}
	if got, _ := repoHookScript(repo, "archive"); got != "echo bye" {
		t.Errorf("archive = %q, want the orca.yaml fallthrough", got)
	}
}

// A backend with no native archive hook gets the runner, and a failing script
// blocks removal.
func TestRunnerArchiveFailureBlocksRemovalOnAnyBackend(t *testing.T) {
	tmp := t.TempDir()
	repo := hookRepo(t, tmp, "", "exit 4")
	f := newFakeBackend("fakemux", CapWorktreeManage)
	a := &Active{Backend: f}
	made, _ := f.CreateWorktree(context.Background(), WorktreeSpec{Name: "t1"})
	err := removeWorktreeWithArchive(context.Background(), a, WorktreeSpec{Path: made.Path}, HookEnv{RootPath: repo, WorktreePath: tmp}, nil, &scriptHooks{})
	hf, ok := errors.AsType[*HookFailedError](err)
	if !ok || hf.ExitCode != 4 {
		t.Fatalf("err = %v, want HookFailedError(4)", err)
	}
	if m, _ := f.OpenWorktrees(context.Background()); m[made.Path] == "" {
		t.Error("worktree removed past a failing archive script")
	}
}

func TestOrcaRemoveDirtyAndGone(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	ctx := context.Background()
	made, err := b.CreateWorktree(ctx, WorktreeSpec{Name: "t1", Branch: "orch/t1", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	h.edit(func(st *orcaFakeState) { st.Dirty = true })
	if err := b.RemoveWorktree(ctx, WorktreeSpec{Path: made.Path}); !errors.Is(err, ErrWorktreeDirty) {
		t.Errorf("dirty = %v, want ErrWorktreeDirty", err)
	}
	if slices.Contains(h.calls("worktree rm")[0], "--force") {
		t.Error("remove must never force")
	}
	if err := b.RemoveWorktree(ctx, WorktreeSpec{Path: filepath.Join(h.tmp, "nope")}); !errors.Is(err, ErrGone) {
		t.Errorf("unknown path = %v, want ErrGone", err)
	}
}

// A lead record written under Orca names the backend; herdr refuses to resolve it.
func TestOrcaLeadRecordIsRefusedByHerdr(t *testing.T) {
	h := newOrcaHarness(t)
	id := h.startAgent(newOrcaBackend())
	l := &lead{ID: "t1", PaneID: string(id.Handle), Backend: id.Backend}
	t.Setenv("ORCH_BACKEND", "herdr")
	if _, err := leadLive(l); err == nil || !strings.Contains(err.Error(), "orca") || !strings.Contains(err.Error(), "herdr") {
		t.Errorf("leadLive under herdr = %v, want an error naming orca", err)
	}
	t.Setenv("ORCH_BACKEND", "orca")
	if live, err := leadLive(l); err != nil || !live {
		t.Errorf("leadLive under orca = %v, %v; want live", live, err)
	}
}

// Real `orca worktree ps` rows key the worktree as worktreeId, not id.
func TestOrcaWorktreeRowHandleReadsWorktreeID(t *testing.T) {
	var r orcaWorktreeRow
	if err := json.Unmarshal([]byte(`{"worktreeId":"r::/p","path":"/p"}`), &r); err != nil {
		t.Fatal(err)
	}
	if got := r.handle(); got != "r::/p" {
		t.Fatalf("handle = %q, want r::/p", got)
	}
}

func TestOrcaListKeysAgentsByTerminalHandle(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	id := h.startAgent(b)
	// Drop the identity file so List must find the terminal through terminal show.
	if err := os.Remove(orcaIdentityPath(string(id.Handle))); err != nil {
		t.Fatal(err)
	}
	rows, err := b.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Handle != id.Handle {
		t.Fatalf("List = %+v, %v; want one row keyed by %s", rows, err, id.Handle)
	}
}

func TestOrcaListKeepsPaneKeyWhenTerminalUnknown(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	id := h.startAgent(b)
	_ = os.Remove(orcaIdentityPath(string(id.Handle)))
	h.edit(func(st *orcaFakeState) { st.Terms[0].Unlisted = true })
	rows, err := b.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Handle != "pane:1" {
		t.Fatalf("List = %+v, %v; want one row keyed by its pane key", rows, err)
	}
}

func TestOrcaStartStoresPaneKey(t *testing.T) {
	h := newOrcaHarness(t)
	id := h.startAgent(newOrcaBackend())
	rec, ok := readOrcaIdentity(string(id.Handle))
	if !ok || rec.PaneKey != "pane:1" {
		t.Fatalf("identity = %+v, %v; want pane key pane:1", rec, ok)
	}
}

func TestOrcaStatusFillsMissingPaneKey(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	id := h.startAgent(b)
	rec, _ := readOrcaIdentity(string(id.Handle))
	rec.PaneKey = ""
	if err := writeOrcaIdentity(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Status(context.Background(), id.Handle); err != nil {
		t.Fatal(err)
	}
	if rec, _ := readOrcaIdentity(string(id.Handle)); rec.PaneKey != "pane:1" {
		t.Errorf("pane key after Status = %q, want pane:1", rec.PaneKey)
	}
}

// orcaScript runs Start and returns the launcher script the terminal create
// command named, plus the create call.
func orcaScript(t *testing.T, h *orcaHarness, spec Spec) (script string, call []string, id Identity) {
	t.Helper()
	wt := filepath.Join(h.tmp, "wt")
	h.edit(func(st *orcaFakeState) { st.Trees = append(st.Trees, &orcaFakeTree{ID: "r::" + wt, Path: wt}) })
	spec.Cwd = wt
	id, err := newOrcaBackend().Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(h.calls("agent launch")); n != 0 {
		t.Fatalf("agent launch ran %d times; Args/Env must go through terminal create", n)
	}
	creates := h.calls("terminal create")
	if len(creates) != 1 {
		t.Fatalf("terminal create calls = %v", creates)
	}
	call = creates[0]
	cmd := flagValue(call, "--command")
	path, ok := strings.CutPrefix(cmd, "sh ")
	if !ok || len(cmd) > 200 {
		t.Fatalf("command = %q; want a short `sh <script>`", cmd)
	}
	path = strings.Trim(path, "'")
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("script %s: %v, %v; want mode 0600", path, st, err)
	}
	b, _ := os.ReadFile(path)
	return string(b), call, id
}

func TestOrcaStartWithArgsAndEnvRunsLauncherScript(t *testing.T) {
	h := newOrcaHarness(t)
	prompt := "read 'task.md'\nthen \"go\" $HOME `x`"
	script, call, id := orcaScript(t, h, Spec{Agent: "claude", Harness: "claude", Name: "lead-t1", Model: "sonnet", Effort: "high", Prompt: prompt,
		Args: []string{"--session-id", "S1", "--tools", "a b"},
		Env:  map[string]string{"ORCH_ROLE": "lead", "ORCA_PANE_KEY": "leak", "HERDR_PANE_ID": "leak", "Q": "it's"}})
	if got := flagValue(call, "--title"); got != "lead-t1" {
		t.Errorf("title = %q", got)
	}
	if got := flagValue(call, "--worktree"); got != "path:"+filepath.Join(h.tmp, "wt") {
		t.Errorf("worktree = %q", got)
	}
	want := "exec env 'ORCH_ROLE=lead' 'Q=it'\"'\"'s' 'claude' '--dangerously-skip-permissions' '--model' 'sonnet' '--effort' 'high' '--session-id' 'S1' '--tools' 'a b' " + shellQuote(prompt) + "\n"
	if !strings.HasSuffix(script, want) || strings.Contains(script, "leak") {
		t.Errorf("script = %q\nwant suffix %q and no leaked env", script, want)
	}
	if id.Handle != "term_1" || id.ID != "pane:1" {
		t.Errorf("identity = %+v, want term_1 keyed by the reply pane key", id)
	}
	rec, ok := readOrcaIdentity("term_1")
	if !ok || rec.PaneKey != "pane:1" || rec.Env["ORCH_ROLE"] != "lead" || rec.Env["Q"] != "it's" || len(rec.Env) != 2 || rec.Name != "lead-t1" {
		t.Errorf("identity file = %+v, %v", rec, ok)
	}
	// The prompt in the script survives sh: run the script's words back through sh.
	out, err := exec.Command("sh", "-c", strings.Replace(script, "exec env", "printf '%s\\n'", 1)).Output()
	if err != nil || !strings.Contains(string(out), prompt+"\n") {
		t.Errorf("prompt did not round-trip through sh: %q, %v", out, err)
	}
}

func TestOrcaStartClaudePermissionFlagRule(t *testing.T) {
	for _, c := range []struct {
		args   []string
		bypass bool
	}{
		{[]string{"--session-id", "S"}, true},
		{[]string{"--dangerously-skip-permissions"}, true}, // present once, not doubled
		{[]string{"--permission-mode", "dontAsk"}, false},
		{[]string{"--permission-mode=plan"}, false},
		{[]string{"--allow-dangerously-skip-permissions"}, false},
	} {
		h := newOrcaHarness(t)
		script, _, _ := orcaScript(t, h, Spec{Agent: "claude", Harness: "claude", Args: c.args})
		if n := strings.Count(script, "'--dangerously-skip-permissions'"); (n == 1) != c.bypass || n > 1 {
			t.Errorf("args %v: bypass count %d, want present=%v\n%s", c.args, n, c.bypass, script)
		}
	}
	// pi never gets the claude flag, and maps effort to --thinking.
	h := newOrcaHarness(t)
	script, _, _ := orcaScript(t, h, Spec{Agent: "pi", Harness: "pi", Model: "m", Effort: "high", Args: []string{"-p"}, Prompt: "go"})
	if !strings.HasSuffix(script, "'pi' '--model' 'm' '--thinking' 'high' '-p' 'go'\n") {
		t.Errorf("pi script = %q", script)
	}
}

func TestOrcaStartDoesNotDuplicateModelFlagsFromArgs(t *testing.T) {
	h := newOrcaHarness(t)
	script, _, _ := orcaScript(t, h, Spec{Agent: "claude", Harness: "claude", Model: "sonnet", Effort: "high", Args: []string{"--model", "sonnet", "--effort=high"}})
	if strings.Count(script, "model") != 1 || strings.Count(script, "effort") != 1 {
		t.Errorf("script repeats model/effort flags: %q", script)
	}
}

func TestOrcaStartAgentLaunchRefusesEffortWithoutModel(t *testing.T) {
	h := newOrcaHarness(t)
	_, err := newOrcaBackend().Start(context.Background(), Spec{Agent: "claude", Cwd: h.tmp, Effort: "high"})
	if err == nil || !strings.Contains(err.Error(), "--effort") {
		t.Fatalf("err = %v", err)
	}
}

func TestOrcaSelfEnvReturnsRecordedEnv(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	wt := filepath.Join(h.tmp, "wt")
	h.edit(func(st *orcaFakeState) { st.Trees = append(st.Trees, &orcaFakeTree{ID: "r::" + wt, Path: wt}) })
	id, err := b.Start(context.Background(), Spec{Agent: "claude", Cwd: wt, Env: map[string]string{"ORCH_ROLE": "lead"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCA_TERMINAL_HANDLE", string(id.Handle))
	if env, err := b.SelfEnv(context.Background()); err != nil || env["ORCH_ROLE"] != "lead" {
		t.Errorf("by handle: %v, %v", env, err)
	}
	t.Setenv("ORCA_TERMINAL_HANDLE", "")
	t.Setenv("ORCA_PANE_KEY", "pane:1")
	if env, _ := b.SelfEnv(context.Background()); env["ORCH_ROLE"] != "lead" {
		t.Errorf("by pane key: %v", env)
	}
	t.Setenv("ORCA_PANE_KEY", "nope")
	if env, err := b.SelfEnv(context.Background()); err != nil || len(env) != 0 {
		t.Errorf("unknown = %v, %v; want empty", env, err)
	}
}

func TestOrcaGoneAfterClose(t *testing.T) {
	h := newOrcaHarness(t)
	b := newOrcaBackend()
	id := h.startAgent(b)
	if g, err := b.Gone(context.Background(), id.Handle); err != nil || g {
		t.Fatalf("Gone before close = %v, %v", g, err)
	}
	if err := b.Close(context.Background(), id.Handle); err != nil {
		t.Fatal(err)
	}
	if !orcaTerminalGone(context.Background(), id.Handle) {
		t.Error("orcaTerminalGone = false after close")
	}
}
