package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests drive orch end to end on the Orca fake (ORCH_ORCA): a backend with
// worktree.manage and no layout. The Orca fake makes plain directories; a test
// that needs a git worktree swaps the directory for a real one.

type wiringOrca struct {
	*harness
	orca *orcaHarness
}

func wiringSetup(t *testing.T) *wiringOrca {
	t.Helper()
	h := setup(t)
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_WORKSPACE_ID", "")
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("ORCH_BACKEND", "orca")
	oh := newOrcaHarness(t)
	t.Setenv("ORCA_PANE_KEY", "pane:orchestrator") // the orchestrator runs inside an Orca terminal
	return &wiringOrca{harness: h, orca: oh}
}

// repoFile commits a file in the repo, so the task base includes it.
func (w *wiringOrca) repoFile(rel, text string) {
	w.t.Helper()
	p := filepath.Join(w.repo, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		w.t.Fatal(err)
	}
	w.gitIn(w.repo, "add", rel)
	w.gitIn(w.repo, "commit", "-q", "-m", "add "+rel)
}

// orcaPath is where the Orca fake put t1's worktree.
func (w *wiringOrca) orcaPath() string {
	w.t.Helper()
	return filepath.Join(w.orca.tmp, "orca-home", "workspaces", "t1")
}

// realWorktree swaps the fake's plain directory for a git worktree on orch/t1.
func (w *wiringOrca) realWorktree() string {
	w.t.Helper()
	p := w.orcaPath()
	if err := os.RemoveAll(p); err != nil {
		w.t.Fatal(err)
	}
	w.gitIn(w.repo, "worktree", "add", "-q", "-b", "orch/t1", p, "main")
	return p
}

// shipped spawns t1, gives it a real worktree and marks it done.
func (w *wiringOrca) shipped() string {
	w.t.Helper()
	w.task("t1")
	w.must("spawn", "t1")
	p := w.realWorktree()
	w.must("report", "--dir", w.dir("t1"), "done", "branch orch/t1")
	return p
}

func (w *wiringOrca) removeCalls() [][]string { return w.orca.calls("worktree rm") }

func TestWiringSpawnPlacesLeadOnBackendWithoutLayout(t *testing.T) {
	w := wiringSetup(t)
	w.task("t1")
	out := w.must("spawn", "t1")
	wantContains(t, out, "spawned lead-t1")

	if n := len(w.orca.calls("worktree create")); n != 1 {
		t.Fatalf("worktree create calls = %d, want 1", n)
	}
	launch := w.orca.calls("terminal create")
	if len(launch) != 1 || flagValue(launch[0], "--worktree") != "path:"+w.orcaPath() {
		t.Fatalf("terminal create = %v, want one start in the Orca-chosen path %s", launch, w.orcaPath())
	}
	l := w.lead("t1")
	if l.WorktreePath != w.orcaPath() || l.WorkspaceID == "" || l.Backend != "orca" {
		t.Fatalf("lead = %+v, want the backend path and workspace stored", l)
	}
	if got := leadWorktree(w.dir("t1"), "t1"); got != w.orcaPath() {
		t.Fatalf("leadWorktree = %q, want %q", got, w.orcaPath())
	}
	wantContains(t, w.must("status"), "t1")
	// The launch prompt names the real path, not the one orch would have chosen.
	prompt := w.orca.term(Handle(l.PaneID)).Out
	if !strings.Contains(strings.Join(prompt, "\n"), w.orcaPath()) {
		t.Fatalf("launch prompt lacks the backend path: %v", prompt)
	}
}

func TestWiringSetupHookNativeStartedAndFallbackRunner(t *testing.T) {
	log := func(w *wiringOrca) string { return filepath.Join(w.tmp, "setup.log") }
	script := func(w *wiringOrca) string {
		return "[hooks]\nsetup = \"echo $ORCH_TASK_ID >> " + log(w) + "\"\n"
	}
	t.Run("native Started does not run the repo script and does not block", func(t *testing.T) {
		w := wiringSetup(t)
		w.repoFile(".agents/orchestrate.toml", script(w))
		w.task("t1")
		w.must("spawn", "t1")
		create := w.orca.calls("worktree create")
		if len(create) != 1 || flagValue(create[0], "--setup") != "run" {
			t.Fatalf("create = %v, want one with --setup run", create)
		}
		if _, err := os.Stat(log(w)); err == nil {
			t.Fatal("the runner ran the script next to a native setup")
		}
	})
	t.Run("capability listed out runs the configured script once", func(t *testing.T) {
		w := wiringSetup(t)
		os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[backend.orca]\nnative = [\"worktree.manage\", \"worktree.archive-hook\"]\n"), 0o644)
		w.repoFile(".agents/orchestrate.toml", script(w))
		w.task("t1")
		w.must("spawn", "t1")
		create := w.orca.calls("worktree create")
		if len(create) != 1 || flagValue(create[0], "--setup") != "skip" {
			t.Fatalf("create = %v, want one with --setup skip", create)
		}
		if got := strings.TrimSpace(string(mustRead(t, log(w)))); got != "t1" {
			t.Fatalf("setup log = %q, want exactly one run with ORCH_TASK_ID=t1", got)
		}
	})
}

func TestWiringCloseAndPruneKeepWorktreesThatWouldLoseWork(t *testing.T) {
	cases := map[string]struct {
		dirty func(w *wiringOrca, p string)
		want  string
	}{
		"uncommitted file": {func(w *wiringOrca, p string) { os.WriteFile(filepath.Join(p, "wip.go"), []byte("x"), 0o644) }, "uncommitted changes"},
		"unmerged commit": {func(w *wiringOrca, p string) {
			os.WriteFile(filepath.Join(p, "done.go"), []byte("x"), 0o644)
			w.gitIn(p, "add", ".")
			w.gitIn(p, "commit", "-q", "-m", "work")
		}, "not merged into main"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := wiringSetup(t)
			p := w.shipped()
			c.dirty(w, p)
			wantContains(t, w.must("close", "t1"), "kept worktree "+p, c.want)
			wantContains(t, w.must("prune", "--apply"), "kept "+p, c.want)
			if n := len(w.removeCalls()); n != 0 {
				t.Fatalf("orca worktree rm called %d times for a worktree that holds work", n)
			}
		})
	}
}

func TestWiringCloseRemovesMergedCleanWorktreeWithRunHooks(t *testing.T) {
	w := wiringSetup(t)
	p := w.shipped()
	wantContains(t, w.must("close", "t1"), "removed worktree "+p)
	rm := w.removeCalls()
	if len(rm) != 1 || !slices.Contains(rm[0], "--run-hooks") || slices.Contains(rm[0], "--force") || flagValue(rm[0], "--worktree") != "path:"+p {
		t.Fatalf("worktree rm = %v, want one for %s with --run-hooks and no --force", rm, p)
	}
}

func TestWiringPruneRemovesMergedCleanWorktreeWithRunHooks(t *testing.T) {
	w := wiringSetup(t)
	p := w.shipped()
	f := filepath.Join(p, "wip.go")
	os.WriteFile(f, []byte("x"), 0o644)
	wantContains(t, w.must("close", "t1"), "kept worktree "+p) // closed lead, worktree stays
	if len(w.removeCalls()) != 0 {
		t.Fatal("close removed a dirty worktree")
	}
	if out := w.must("prune"); !strings.Contains(out, "kept "+p) {
		t.Fatalf("prune dry run does not list the backend-placed worktree:\n%s", out)
	}
	os.Remove(f)
	wantContains(t, w.must("prune"), "would remove "+p)
	if len(w.removeCalls()) != 0 {
		t.Fatal("a dry run removed a worktree")
	}
	wantContains(t, w.must("prune", "--apply"), "removed "+p)
	rm := w.removeCalls()
	if len(rm) != 1 || !slices.Contains(rm[0], "--run-hooks") {
		t.Fatalf("worktree rm = %v, want one with --run-hooks", rm)
	}
}

func TestWiringFailingArchiveHookKeepsTheWorktree(t *testing.T) {
	w := wiringSetup(t)
	p := w.shipped()
	w.orca.edit(func(st *orcaFakeState) { st.ArchiveExit = 3 })
	wantContains(t, w.must("close", "t1"), "kept worktree "+p, "worktree_archive_hook_failed")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("worktree gone after a failed archive hook: %v", err)
	}
	if n := len(w.removeCalls()); n != 1 {
		t.Fatalf("worktree rm calls = %d, want 1 (a failed hook is never rerun)", n)
	}
	out := w.must("prune", "--apply")
	wantContains(t, out, "kept "+p, "worktree_archive_hook_failed")
	if n := len(w.removeCalls()); n != 2 {
		t.Fatalf("worktree rm calls after prune = %d, want 2", n)
	}
	if _, err := git(w.repo, "show-ref", "--verify", "--quiet", "refs/heads/orch/t1"); err != nil {
		t.Fatal("branch orch/t1 was deleted with the worktree kept")
	}
}

func TestWiringArchiveRunnerFallbackWithoutOrca(t *testing.T) {
	h := setup(t) // herdr fake: no archive-hook capability
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte("[hooks]\narchive = \"exit 3\"\n"), 0o644)
	h.gitIn(h.repo, "add", ".agents")
	h.gitIn(h.repo, "commit", "-q", "-m", "hooks")
	shippedLead(t, h)
	wantContains(t, h.must("close", "t1"), "kept worktree "+h.wt("t1"), "worktree_archive_hook_failed")
	if len(h.herdrCalls("worktree", "remove")) != 0 {
		t.Fatal("the removal ran after a failing archive script")
	}
	if _, err := os.Stat(h.wt("t1")); err != nil {
		t.Fatalf("worktree gone: %v", err)
	}
	wantContains(t, h.must("prune", "--apply"), "kept "+h.wt("t1"), "worktree_archive_hook_failed")
	if _, err := os.Stat(h.wt("t1")); err != nil {
		t.Fatalf("prune removed the worktree past a failing archive script: %v", err)
	}
}

func TestWiringHerdrLeadRecordHasNoWorktreePath(t *testing.T) {
	h := setup(t)
	shippedLead(t, h)
	raw := string(mustRead(t, filepath.Join(h.dir("t1"), "lead.json")))
	if strings.Contains(raw, "worktree_path") {
		t.Fatalf("herdr lead.json gained a key:\n%s", raw)
	}
	if got := leadWorktree(h.dir("t1"), "t1"); got != h.wt("t1") {
		t.Fatalf("leadWorktree = %q, want %q", got, h.wt("t1"))
	}
}

func TestWiringRepoConfigAcceptsHooksTable(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	repoFile := filepath.Join(h.repo, ".agents", "orchestrate.toml")
	os.WriteFile(repoFile, []byte("[hooks]\nsetup = \"make setup\"\narchive = \"make archive\"\n"), 0o644)
	cfg, err := loadConfig(h.repo)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Hooks.Setup != "make setup" || cfg.Hooks.Archive != "make archive" {
		t.Fatalf("hooks = %+v", cfg.Hooks)
	}
	os.WriteFile(repoFile, []byte("[hooks]\nsetup = \"x\"\nteardown = \"y\"\n"), 0o644)
	if _, err := loadConfig(h.repo); err == nil || !strings.Contains(err.Error(), `unknown key "hooks.teardown"`) {
		t.Fatalf("unknown hooks key: err = %v", err)
	}
	os.Remove(repoFile)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[hooks]\nsetup = \"x\"\n"), 0o644)
	if _, err := loadConfig(h.repo); err == nil || !strings.Contains(err.Error(), "is not allowed here") {
		t.Fatalf("hooks in the user config: err = %v, want a repo-key refusal", err)
	}
}

// fakeOrcaScript points ORCH_ORCA at a script that answers with the given JSON
// for `terminal list` and `worktree ps`, to feed rows shaped as Orca really
// sends them.
func fakeOrcaScript(t *testing.T, terminals, ps string) {
	t.Helper()
	newOrcaHarness(t)
	t.Setenv("ORCH_FAKE_ORCA", "") // the test binary must not act as the fake
	t.Setenv("ORCH_ORCA", fakeRunner(t, "orca", `
case "$1 $2" in
"terminal list") cat <<'EOF'
{"id":"r","ok":true,"result":`+terminals+`}
EOF
;;
"worktree ps") cat <<'EOF'
{"id":"r","ok":true,"result":`+ps+`}
EOF
;;
*) echo '{"id":"r","ok":false,"error":{"code":"unknown_command","message":"x"}}'; exit 1;;
esac`))
}

func TestWiringOrcaRowsMissingFieldsParseAsUnknown(t *testing.T) {
	// Terminal rows as Orca sends them: no paneKey, no status. The ps agent row
	// names no terminalHandle, no sessionId, no blockedReason and no agentWait.
	fakeOrcaScript(t,
		`{"terminals":[{"handle":"term_1","title":"lead","worktreeId":"w","worktreePath":"/wt/a","tabId":"t"}]}`,
		`{"worktrees":[{"id":"w","path":"/wt/a","branch":"b","agents":[{"paneKey":"","state":"working","agentType":"claude"}]}]}`)
	b := newOrcaBackend()
	ctx := context.Background()
	if h, err := b.Resolve(ctx, "term_1"); err != nil || h != "term_1" {
		t.Fatalf("Resolve = %q, %v; a terminal row without paneKey or status must resolve", h, err)
	}
	st, err := b.Status(ctx, "term_1")
	if err != nil || st.State != StateUnknown {
		t.Fatalf("Status = %+v, %v; want unknown (agentWait never looked up) and no error", st, err)
	}
	list, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List = %+v; a row with no handle or pane key cannot be addressed and is skipped", list)
	}
}

func TestWiringOrcaAgentRowWithoutHandleIsMatchedByItsWorktree(t *testing.T) {
	fakeOrcaScript(t,
		`{"terminals":[{"handle":"term_1","worktreePath":"/wt/a"},{"handle":"term_2","worktreePath":"/wt/b"}]}`,
		`{"worktrees":[`+
			`{"id":"a","path":"/wt/a","agents":[{"state":"working","agentType":"claude","agentWait":null}]},`+
			`{"id":"b","path":"/wt/b","agents":[{"state":"idle","agentType":"claude","agentWait":"permission"}]}]}`)
	b := newOrcaBackend()
	ctx := context.Background()
	if st, err := b.Status(ctx, "term_1"); err != nil || st.State != StateWorking {
		t.Fatalf("term_1 Status = %+v, %v; want working", st, err)
	}
	// agentWait set without a blockedReason: the wait value is the reason.
	st, err := b.Status(ctx, "term_2")
	if err != nil || st.State != StateBlocked || st.BlockedReason != "permission" {
		t.Fatalf("term_2 Status = %+v, %v; want blocked: permission", st, err)
	}
}

// A running agent's real ps row carries a paneKey (never shown by terminal list)
// and no terminalHandle; the lone agent of the terminal's worktree is its row.
func TestWiringOrcaRunningAgentRowWithPaneKeyIsMatchedByItsWorktree(t *testing.T) {
	fakeOrcaScript(t,
		`{"terminals":[{"handle":"term_1","worktreePath":"/wt/a"}]}`,
		`{"worktrees":[{"worktreeId":"a","path":"/wt/a","agents":[{"paneKey":"tab:leaf","state":"done","agentType":"claude","agentWait":null}]}]}`)
	st, err := newOrcaBackend().Status(context.Background(), "term_1")
	if err != nil || st.State != StateDone {
		t.Fatalf("Status = %+v, %v; want done", st, err)
	}
}

// Shapes below were captured from Orca 1.4.253: terminal show records the pane
// key a running agent's ps row carries; a plan-approval wait is state waiting
// with an agentWait object; the bypass confirmation has no ps row at all.
func liveShapedOrca(t *testing.T, ps, screen string) {
	t.Helper()
	newOrcaHarness(t)
	t.Setenv("ORCH_FAKE_ORCA", "")
	t.Setenv("ORCH_ORCA", fakeRunner(t, "orca", `
case "$1 $2" in
"terminal list") echo '{"id":"r","ok":true,"result":{"terminals":[{"handle":"term_1","worktreePath":"/wt"},{"handle":"term_2","worktreePath":"/wt"}]}}';;
"terminal show")
  case "$4" in
  term_1) echo '{"id":"r","ok":true,"result":{"recordedPaneKey":"tab1:leaf1"}}';;
  *) echo '{"id":"r","ok":true,"result":{"recordedPaneKey":"tab2:leaf2"}}';;
  esac;;
"terminal read") cat <<'EOF'
{"id":"r","ok":true,"result":{"terminal":{"tail":[`+screen+`]}}}
EOF
;;
"worktree ps") cat <<'EOF'
{"id":"r","ok":true,"result":{"worktrees":[`+ps+`]}}
EOF
;;
*) echo '{"id":"r","ok":false,"error":{"code":"unknown_command","message":"x"}}'; exit 1;;
esac`))
}

func TestWiringOrcaAgentsSharingAWorktreeAreToldApartByPaneKey(t *testing.T) {
	liveShapedOrca(t, `{"worktreeId":"w","path":"/wt","agents":[`+
		`{"paneKey":"tab1:leaf1","state":"done","agentType":"claude","agentWait":null},`+
		`{"paneKey":"tab2:leaf2","state":"waiting","agentType":"claude","toolName":"ExitPlanMode","agentWait":{"source":"hook","since":1}}]}`, ``)
	b := newOrcaBackend()
	ctx := context.Background()
	if st, err := b.Status(ctx, "term_1"); err != nil || st.State != StateDone {
		t.Fatalf("term_1 Status = %+v, %v; want done", st, err)
	}
	st, err := b.Status(ctx, "term_2")
	if err != nil || st.State != StateBlocked || st.BlockedReason != "waiting on a human: ExitPlanMode" {
		t.Fatalf("term_2 Status = %+v, %v; want blocked: waiting on a human: ExitPlanMode", st, err)
	}
}

func TestWiringOrcaBypassConfirmationReadsAsBlocked(t *testing.T) {
	liveShapedOrca(t, `{"worktreeId":"w","path":"/wt","agents":[]}`,
		`"  WARNING: Claude Code running in Bypass Permissions mode","  Yes, I accept","  Enter to confirm · Esc to cancel"`)
	st, err := newOrcaBackend().Status(context.Background(), "term_1")
	if err != nil || st.State != StateBlocked || !strings.Contains(st.BlockedReason, "bypass") {
		t.Fatalf("Status = %+v, %v; want blocked on the bypass confirmation", st, err)
	}
}

func TestWiringOrcaTerminalWithNoAgentRowAndPlainScreenIsUnknown(t *testing.T) {
	liveShapedOrca(t, `{"worktreeId":"w","path":"/wt","agents":[]}`, `"$ "`)
	st, err := newOrcaBackend().Status(context.Background(), "term_1")
	if err != nil || st.State != StateUnknown {
		t.Fatalf("Status = %+v, %v; want unknown", st, err)
	}
}
