package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const daemonFxSrc = "package fx\n\n// Greet says hi.\nfunc Greet() string { return \"hi\" }\n\nfunc Run() string { return Greet() }\n"

// daemonEnv prepares a Go repo (committed fx module) and the environment of
// the lsp commands: the real gopls, a private cache, no daemon yet. It skips
// the test without gopls.
func daemonEnv(t *testing.T) *harness {
	t.Helper()
	bin := goplsBin() // before setup hides it
	if _, err := exec.LookPath(bin); err != nil {
		t.Skip("gopls is not on PATH; the daemon tests need a real language server")
	}
	h := setup(t)
	t.Setenv("ORCH_GOPLS", bin)
	oldVersion, oldSpawn := lspVersionFn, lspSpawn
	lspVersionFn = func() string { return "test-build" }
	lspSpawn = func() error { return errors.New("tests start the daemon themselves") }
	t.Cleanup(func() { lspVersionFn, lspSpawn = oldVersion, oldSpawn })
	writeGoFx(t, h, h.repo)
	return h
}

func writeGoFx(t *testing.T, h *harness, repo string) {
	t.Helper()
	writeTree(t, repo, map[string]string{"go.mod": "module fx\n\ngo 1.21\n", "fx.go": daemonFxSrc})
	h.gitIn(repo, "add", ".")
	h.gitIn(repo, "commit", "-q", "-m", "fx")
}

func startTestDaemon(t *testing.T, id string) *lspDaemon {
	t.Helper()
	dir, err := lspSockDir()
	if err != nil {
		t.Fatal(err)
	}
	d, err := startLspDaemon(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.shutdown() })
	return d
}

type lspResult struct {
	code        int
	out, errOut string
}

func runLspIn(cwd string, args ...string) lspResult {
	var out, errb bytes.Buffer
	a := &app{cwd: cwd, stdin: strings.NewReader(""), out: &out, errw: &errb, now: time.Now}
	code := a.run(append([]string{"lsp"}, args...))
	return lspResult{code, out.String(), errb.String()}
}

func TestLspDaemonMatchesInProcessOutput(t *testing.T) {
	h := daemonEnv(t)
	d := startTestDaemon(t, "test-build")
	for _, args := range [][]string{
		{"def", "Greet"}, {"refs", "Greet"}, {"callers", "Greet"}, {"callers", "Greet", "Run", "--json"},
		{"def", "NoSuchName"}, {"refs", "Greet", "--limit", "0"},
	} {
		viaDaemon := runLspIn(h.repo, args...)
		t.Setenv("ORCH_LSP_DAEMON", "off")
		inProcess := runLspIn(h.repo, args...)
		t.Setenv("ORCH_LSP_DAEMON", "")
		if viaDaemon != inProcess {
			t.Errorf("lsp %v differs\n daemon:     %+v\n in-process: %+v", args, viaDaemon, inProcess)
		}
	}
	if r := runLspIn(h.repo, "callers", "Greet"); r.code != 0 || !strings.Contains(r.out, "fx.go:6 in Run") {
		t.Errorf("callers Greet: %+v", r)
	}
	roots := d.pool.status()
	if len(roots) != 1 || len(roots[0].Sessions) != 1 || roots[0].Sessions[0].Lang != "go" || roots[0].Sessions[0].Pid == 0 {
		t.Errorf("the daemon should hold one warm gopls session: %+v", roots)
	}
}

func TestLspDaemonSeesEditsWithoutRestart(t *testing.T) {
	h := daemonEnv(t)
	startTestDaemon(t, "test-build")
	repo := h.repo
	def := func(name string) lspResult { return runLspIn(repo, "def", name) }

	if r := def("Foo"); r.code != 1 || r.out != "no symbol Foo\n" {
		t.Fatalf("Foo before it exists: %+v", r)
	}
	// refs opens fx.go in the server; the edits below must reach that open copy.
	if r := runLspIn(repo, "refs", "Greet"); r.code != 0 || !strings.Contains(r.out, "fx.go:6: ") {
		t.Fatalf("refs Greet: %+v", r)
	}

	foo := filepath.Join(repo, "foo.go")
	if err := os.WriteFile(foo, []byte("package fx\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := def("Foo"); r.code != 0 || !strings.Contains(r.out, "foo.go:3 Function") {
		t.Fatalf("an untracked new file must be found: %+v", r)
	}
	if err := os.Remove(foo); err != nil {
		t.Fatal(err)
	}
	if r := def("Foo"); r.code != 1 {
		t.Fatalf("a deleted file must be gone: %+v", r)
	}

	// An edit to a file the server already holds open.
	src := daemonFxSrc + "\nfunc Again() string { return Greet() }\n"
	if err := os.WriteFile(filepath.Join(repo, "fx.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runLspIn(repo, "refs", "Greet"); !strings.Contains(r.out, "fx.go:8: ") {
		t.Fatalf("an edit to an open file must show in refs: %+v", r)
	}
	h.gitIn(repo, "checkout", "-q", "fx.go")

	// A rename committed on a branch, then checked out and back.
	h.gitIn(repo, "checkout", "-q", "-b", "rename")
	renamed := strings.ReplaceAll(daemonFxSrc, "Greet", "Hello")
	if err := os.WriteFile(filepath.Join(repo, "fx.go"), []byte(renamed), 0o644); err != nil {
		t.Fatal(err)
	}
	h.gitIn(repo, "commit", "-q", "-am", "rename")
	if r := def("Hello"); r.code != 0 || !strings.Contains(r.out, "fx.go:4 Function") {
		t.Fatalf("the renamed symbol must be found after a commit: %+v", r)
	}
	if r := def("Greet"); r.code != 1 {
		t.Fatalf("the old name must be gone: %+v", r)
	}
	h.gitIn(repo, "checkout", "-q", "main")
	if r := def("Greet"); r.code != 0 {
		t.Fatalf("Greet must be back after checking out main: %+v", r)
	}
	if r := def("Hello"); r.code != 1 {
		t.Fatalf("Hello must be gone after checking out main: %+v", r)
	}
}

func TestLspDaemonTypeScriptSeesEdits(t *testing.T) {
	bin := os.Getenv("ORCH_TEST_TSLS")
	if bin == "" {
		bin, _ = exec.LookPath("tsc")
	}
	if bin == "" {
		t.Skip("no TypeScript 7 tsc: set ORCH_TEST_TSLS")
	}
	if out, err := exec.Command(bin, "--version").Output(); err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "Version 7") {
		t.Skip("tsc is not TypeScript 7")
	}
	h := setup(t)
	t.Setenv("ORCH_TSLS", bin)
	oldReady := tsReady
	tsReady = 20 * time.Second
	t.Cleanup(func() { tsReady = oldReady })
	oldVersion, oldSpawn := lspVersionFn, lspSpawn
	lspVersionFn = func() string { return "test-build" }
	lspSpawn = func() error { return errors.New("no spawn") }
	t.Cleanup(func() { lspVersionFn, lspSpawn = oldVersion, oldSpawn })
	repo := h.repo
	writeTree(t, repo, map[string]string{
		"tsconfig.json": `{"compilerOptions":{"strict":true,"target":"es2020","module":"esnext","moduleResolution":"bundler"},"include":["*.ts"]}`,
		"a.ts":          "export function barFn(): number { return 1 }\n",
	})
	h.gitIn(repo, "add", ".")
	h.gitIn(repo, "commit", "-q", "-m", "ts")
	t.Setenv("ORCH_LSP_DAEMON", "off")
	if r := runLspIn(repo, "def", "barFn"); r.code != 0 {
		t.Skipf("this tsc does not run as a language server here: %+v", r)
	}
	t.Setenv("ORCH_LSP_DAEMON", "")
	startTestDaemon(t, "test-build")
	def := func(name string) lspResult { return runLspIn(repo, "def", name) }

	if r := def("fooFn"); r.code != 1 {
		t.Fatalf("fooFn before it exists: %+v", r)
	}
	b := filepath.Join(repo, "b.ts")
	if err := os.WriteFile(b, []byte("export function fooFn(): number { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := def("fooFn"); r.code != 0 || !strings.Contains(r.out, "b.ts:1") {
		t.Fatalf("an untracked new ts file must be found: %+v", r)
	}
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	if r := def("fooFn"); r.code != 1 {
		t.Fatalf("a deleted ts file must be gone: %+v", r)
	}
	h.gitIn(repo, "checkout", "-q", "-b", "rename")
	if err := os.WriteFile(filepath.Join(repo, "a.ts"), []byte("export function bazFn(): number { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.gitIn(repo, "commit", "-q", "-am", "rename")
	if r := def("bazFn"); r.code != 0 {
		t.Fatalf("renamed ts symbol must be found: %+v", r)
	}
	h.gitIn(repo, "checkout", "-q", "main")
	if r := def("barFn"); r.code != 0 {
		t.Fatalf("barFn must be back: %+v", r)
	}
}

func TestLspDaemonClosesLeastRecentlyUsedRoot(t *testing.T) {
	h := daemonEnv(t)
	t.Setenv("ORCH_LSP_MAX_ROOTS", "1")
	d := startTestDaemon(t, "test-build")
	second := filepath.Join(h.tmp, "second")
	h.gitIn(h.tmp, "init", "-q", "-b", "main", second)
	writeGoFx(t, h, second)

	if r := runLspIn(h.repo, "def", "Greet"); r.code != 0 {
		t.Fatalf("first root: %+v", r)
	}
	first := d.pool.status()
	if len(first) != 1 || len(first[0].Sessions) != 1 {
		t.Fatalf("first root should be live: %+v", first)
	}
	pid := first[0].Sessions[0].Pid
	if r := runLspIn(second, "def", "Greet"); r.code != 0 {
		t.Fatalf("second root: %+v", r)
	}
	var st lspStatus
	r := runLspIn(second, "status", "--json")
	if err := json.Unmarshal([]byte(r.out), &st); err != nil {
		t.Fatalf("%v\n%+v", err, r)
	}
	if len(st.Roots) != 1 || st.Roots[0].Repo != evalOr(second) || st.Roots[0].Sessions[0].Pid == pid {
		t.Errorf("only the second root should be live, with its own server: %+v", st.Roots)
	}
}

func TestLspDaemonOfAnotherBuildIsReplaced(t *testing.T) {
	h := daemonEnv(t)
	old := startTestDaemon(t, "old-build")
	var successor *lspDaemon
	lspSpawn = func() error {
		dir, err := lspSockDir()
		if err != nil {
			return err
		}
		successor, err = startLspDaemon(dir, "test-build")
		return err
	}
	t.Cleanup(func() {
		if successor != nil {
			successor.shutdown()
		}
	})
	if r := runLspIn(h.repo, "def", "Greet"); r.code != 0 || !strings.Contains(r.out, "fx.go:4 Function") {
		t.Fatalf("answer through the replacement: %+v", r)
	}
	select {
	case <-old.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the old daemon did not exit")
	}
	if successor == nil || successor.id != "test-build" {
		t.Fatalf("no replacement was started: %+v", successor)
	}
	if st := successor.pool.status(); len(st) != 1 {
		t.Errorf("the replacement should hold the repo: %+v", st)
	}
}

func TestLspDaemonUnusableSocketFallsBackToInProcess(t *testing.T) {
	h := daemonEnv(t)
	t.Setenv("ORCH_LSP_DAEMON", "off")
	want := runLspIn(h.repo, "callers", "Greet")
	if want.code != 0 || !strings.Contains(want.out, "fx.go:6 in Run") {
		t.Fatalf("in-process baseline: %+v", want)
	}
	t.Setenv("ORCH_LSP_DAEMON", "")
	// The cache directory sits under a regular file: no socket can be made.
	blocker := filepath.Join(h.tmp, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_CACHE_DIR", filepath.Join(blocker, "cache"))
	if got := runLspIn(h.repo, "callers", "Greet"); got != want {
		t.Errorf("unusable socket dir: %+v, want %+v", got, want)
	}
	// A socket file nobody listens on, and a daemon that cannot be started.
	t.Setenv("ORCH_CACHE_DIR", filepath.Join(h.tmp, "stale"))
	dir, err := lspSockDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runLspIn(h.repo, "callers", "Greet"); got != want {
		t.Errorf("stale socket: %+v, want %+v", got, want)
	}
}

func TestLspDaemonStopAndStatusWithoutDaemon(t *testing.T) {
	h := setup(t)
	if r := runLspIn(h.repo, "status"); r.code != 0 || r.out != "no lsp daemon running\n" {
		t.Errorf("status without a daemon: %+v", r)
	}
	if r := runLspIn(h.repo, "stop"); r.code != 0 || r.out != "no lsp daemon running\n" {
		t.Errorf("stop without a daemon: %+v", r)
	}
	t.Setenv("ORCH_LSP_DAEMON", "off")
	if r := runLspIn(h.repo, "warm"); r.code != 1 {
		t.Errorf("warm with the daemon off: %+v", r)
	}
}

func TestLspDiffSnapsReportsWhatAWatcherWould(t *testing.T) {
	repo := t.TempDir()
	for name, body := range map[string]string{"kept.go": "x", "now_clean.go": "x"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := &lspSnap{head: "a", files: map[string]lspFileState{
		"kept.go": {mtime: 1, size: 1}, "now_clean.go": {mtime: 1, size: 1}, "gone.go": {mtime: 1, size: 1},
	}}
	cur := &lspSnap{head: "a", files: map[string]lspFileState{
		"kept.go": {mtime: 1, size: 1}, "new.go": {mtime: 2, size: 1, untracked: true}, "gone.go": {missing: true},
	}}
	got := diffLspSnaps(repo, old, cur)
	want := map[string]int{"now_clean.go": lspChanged, "new.go": lspCreated, "gone.go": lspDeleted}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("%s: kind %d, want %d", p, got[p], k)
		}
	}
}
