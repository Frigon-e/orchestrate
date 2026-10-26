package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// jbFixture is a git repo and a jbcontext stand-in whose indexed revision is
// the content of <dir>/rev (empty: no index at all); `index` logs itself,
// fails when <dir>/fail exists and otherwise indexes HEAD.
func jbFixture(t *testing.T, indexed string) (bin, code, head, calls string) {
	t.Helper()
	t.Setenv("ORCH_CACHE_DIR", t.TempDir())
	code = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@e.com"}, args...)...)
		cmd.Dir = code
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(code, "a.go"), []byte("package a\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "init")
	head = run("rev-parse", "HEAD")
	dir := t.TempDir()
	bin, calls = filepath.Join(dir, "jbcontext"), filepath.Join(dir, "calls.log")
	if indexed == "HEAD" {
		indexed = head
	}
	os.WriteFile(filepath.Join(dir, "rev"), []byte(indexed), 0o644)
	script := `#!/bin/sh
echo "$*" >> "` + calls + `"
case "$1" in
status) r=$(cat "` + dir + `/rev"); s=""; [ -n "$r" ] && s='{"revision":"'$r'","createdAt":1}'
	echo '{"type":"status_result","indices":[{"snapshots":['$s']}]}';;
index) [ -f "` + dir + `/fail" ] && { echo boom >&2; exit 1; }; echo ` + head + ` > "` + dir + `/rev";;
search) echo '{"type":"search_result","results":[]}';;
esac
`
	os.WriteFile(bin, []byte(script), 0o755)
	return bin, code, head, calls
}

func jbEnv(bin, code string, cfg config) *ctxEnv {
	e := newCtxEnv(code, ctxOpts{Cfg: cfg})
	e.JB = bin
	return e
}

func callLog(calls string) string {
	b, _ := os.ReadFile(calls)
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", "|")), " ")
}

func TestJbcontextIndexFreshIsNotRefreshedAndSearchesArePinned(t *testing.T) {
	bin, code, head, calls := jbFixture(t, "HEAD")
	e := jbEnv(bin, code, config{})
	st := e.Ready()
	if st.rev != head || st.note != "" {
		t.Fatalf("state = %+v", st)
	}
	e.Ready() // once per run
	log := callLog(calls)
	if strings.Contains(log, "index") || strings.Count(log, "status") != 1 {
		t.Errorf("calls: %s", log)
	}
	if _, err := e.Concept("q", "", 3); err != nil {
		t.Fatal(err)
	}
	wantContains(t, callLog(calls), "--revision "+head)
	wantContains(t, e.Sources(), "jbcontext index "+sha7(head)+" (HEAD "+sha7(head)+")")
}

func TestJbcontextStaleIndexIsRefreshedOnce(t *testing.T) {
	bin, code, head, calls := jbFixture(t, strings.Repeat("a", 40))
	e := jbEnv(bin, code, config{})
	st := e.Ready()
	e.Ready()
	if st.rev != head || st.note != "" {
		t.Errorf("state after refresh = %+v", st)
	}
	log := callLog(calls)
	if strings.Count(log, "index --only-incremental") != 1 || !strings.Contains(log, "--project-path "+code) || strings.Count(log, "status") != 2 {
		t.Errorf("calls: %s", log)
	}
}

func TestJbcontextRefreshFailureIsANoteAndSearchesGoOn(t *testing.T) {
	bin, code, head, calls := jbFixture(t, strings.Repeat("a", 40))
	os.WriteFile(filepath.Join(filepath.Dir(bin), "fail"), nil, 0o644)
	e := jbEnv(bin, code, config{})
	st := e.Ready()
	want := fmt.Sprintf("jbcontext index is at aaaaaaa, working tree is at %s: hits may be stale (index refresh failed:", sha7(head))
	if st.rev != "" || !strings.HasPrefix(st.note, want) {
		t.Errorf("state = %+v, want note starting %q", st, want)
	}
	if _, err := e.Concept("q", "", 3); err != nil {
		t.Fatal(err)
	}
	if log := callLog(calls); strings.Contains(log, "--revision") {
		t.Errorf("a search was pinned to a revision that is not indexed: %s", log)
	}
	if notes := strings.Join(e.Notes(), "|"); !strings.Contains(notes, want) {
		t.Errorf("notes: %s", notes)
	}
	wantContains(t, e.Sources(), "stale")
}

func TestJbcontextDirtyTreeIsNotedEvenWhenTheIndexIsFresh(t *testing.T) {
	bin, code, head, _ := jbFixture(t, "HEAD")
	os.WriteFile(filepath.Join(code, "b.go"), []byte("package a\n"), 0o644)
	st := jbEnv(bin, code, config{}).Ready()
	want := fmt.Sprintf("jbcontext index is at %s, working tree is at %s with uncommitted changes: hits may be stale", sha7(head), sha7(head))
	if st.rev != head || st.note != want {
		t.Errorf("state = %+v, want note %q", st, want)
	}
}

func TestJbcontextOutsideAGitRepoIsLeftAlone(t *testing.T) {
	bin, _, _, calls := jbFixture(t, "HEAD")
	if st := jbEnv(bin, t.TempDir(), config{}).Ready(); st != (jbState{}) {
		t.Errorf("state = %+v", st)
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Errorf("jbcontext was called: %s", b)
	}
}

func TestCtxIndexPolicyDenyListAndPrivateWork(t *testing.T) {
	cfgWith := func(policy string, deny ...string) config {
		return config{userConfig: userConfig{CtxIndex: policy, CtxDeny: deny}}
	}
	t.Run("full builds a missing index", func(t *testing.T) {
		bin, code, head, calls := jbFixture(t, "")
		e := jbEnv(bin, code, config{}) // the default policy is full
		if st := e.Ready(); st.rev != head {
			t.Errorf("state = %+v", st)
		}
		log := callLog(calls)
		if !strings.Contains(log, "index --project-path "+code) || strings.Contains(log, "--only-incremental") {
			t.Errorf("calls: %s", log)
		}
		wantContains(t, strings.Join(e.Notes(), "|"), "this repo has no index, so a full index was started")
	})
	t.Run("incremental never builds a first index", func(t *testing.T) {
		bin, code, _, calls := jbFixture(t, "")
		e := jbEnv(bin, code, cfgWith("incremental"))
		if st := e.Ready(); st.rev != "" {
			t.Errorf("state = %+v", st)
		}
		if strings.Contains(callLog(calls), "index") {
			t.Errorf("calls: %s", callLog(calls))
		}
		wantContains(t, strings.Join(e.Notes(), "|"), "ctx_index is incremental")
		// ... unless refresh --full asks for it.
		if st := e.Refresh(true); st.rev == "" {
			t.Errorf("refresh --full: %+v", st)
		}
		wantContains(t, callLog(calls), "index --project-path "+code)
	})
	for name, mk := range map[string]func(code string) *ctxEnv{
		"off": func(code string) *ctxEnv { return newCtxEnv(code, ctxOpts{Cfg: cfgWith("off")}) },
		"deny": func(code string) *ctxEnv {
			return newCtxEnv(code, ctxOpts{Cfg: cfgWith("full", "/nowhere", code)})
		},
		"private": func(code string) *ctxEnv { return newCtxEnv(code, ctxOpts{Private: true}) },
	} {
		t.Run(name+" never calls jbcontext", func(t *testing.T) {
			bin, code, _, calls := jbFixture(t, "")
			e := mk(code)
			e.JB = bin
			if st := e.Ready(); st != (jbState{}) {
				t.Errorf("state = %+v", st)
			}
			if _, err := e.Concept("q", "", 3); err == nil {
				t.Error("a concept search ran")
			}
			if b, _ := os.ReadFile(calls); len(b) != 0 {
				t.Errorf("jbcontext was called: %s", b)
			}
		})
	}
	if _, err := loadConfig(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestCtxDenyPreventsIndexingAndSearchInLinkedWorktrees(t *testing.T) {
	c := newCtxHarness(t)
	work := filepath.Join(t.TempDir(), "worktree")
	c.git("worktree", "add", "--detach", work, "HEAD")
	if err := os.WriteFile(os.Getenv("ORCH_CONFIG"), fmt.Appendf(nil, "ctx_deny = [%q]\n", c.repo), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{c.repo, work} {
		c.a.cwd = root
		code, out := c.run("ctx", "find", "Code that starts a checkout", "--json")
		if calls := c.log(c.jbCalls); calls != "" {
			t.Fatalf("denied repository %s reached jbcontext: %s", root, calls)
		}
		if code != 1 || !strings.Contains(out, "ctx_deny") {
			t.Fatalf("denied repository %s: exit %d, output %s", root, code, out)
		}
	}
}

func TestCtxCacheIsKeyedByHeadAndTheDirtyDiff(t *testing.T) {
	_, code, _, _ := jbFixture(t, "HEAD")
	root := os.Getenv("ORCH_CACHE_DIR")
	clean := ctxCacheDir(code)
	if !strings.HasPrefix(clean, filepath.Join(root, "ctx")+string(filepath.Separator)) || !strings.HasSuffix(clean, "-clean") {
		t.Fatalf("clean cache dir = %s", clean)
	}
	if again := ctxCacheDir(code); again != clean {
		t.Errorf("same state, different dir: %s %s", clean, again)
	}
	os.WriteFile(filepath.Join(code, "a.go"), []byte("package a\n\nfunc X() {}\n"), 0o644)
	dirty := ctxCacheDir(code)
	if dirty == clean {
		t.Error("an uncommitted change kept the cache dir")
	}
	os.WriteFile(filepath.Join(code, "a.go"), []byte("package a\n\nfunc Y() {}\n"), 0o644)
	if other := ctxCacheDir(code); other == dirty || other == clean {
		t.Error("a different diff kept the cache dir")
	}
	for _, args := range [][]string{{"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@e.com", "commit", "-q", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = code
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if after := ctxCacheDir(code); after == clean || strings.HasSuffix(after, "-clean") == false {
		t.Errorf("a new commit: %s (was %s)", after, clean)
	}
}

func TestEnrichNotesAStaleJbcontextIndex(t *testing.T) {
	bin, code, head, _ := jbFixture(t, strings.Repeat("b", 40))
	os.WriteFile(filepath.Join(filepath.Dir(bin), "fail"), nil, 0o644)
	t.Setenv("ORCH_JBCONTEXT", bin)
	dir, _, src, sp := enrichFixture(t, fixtureQuestions())
	t.Setenv("ORCH_CACHE_DIR", t.TempDir())
	res, err := enrichGate(dir, code, src, "spec.md", sp)
	if err != nil {
		t.Fatal(err)
	}
	want := "jbcontext index is at bbbbbbb, working tree is at " + sha7(head)
	found := false
	for _, n := range res.Notes {
		found = found || strings.HasPrefix(n, want)
	}
	if !found {
		t.Errorf("notes: %v", res.Notes)
	}
	wantContains(t, string(mustRead(t, filepath.Join(dir, "questions", "INDEX.md"))), want)
}

// A worktree shares jbcontext's repository id with the main checkout, so a
// search can return text the file no longer has or a file that is only in
// another worktree. Hits must read the way this checkout's files read now.
func TestJbcontextHitsMatchThisWorktreesFiles(t *testing.T) {
	setup := func(t *testing.T) (e *ctxEnv, code, calls string) {
		bin, code, _, calls := jbFixture(t, "HEAD")
		run := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@e.com"}, args...)...)
			cmd.Dir = code
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		os.WriteFile(filepath.Join(code, "b.go"), []byte("package a\n\nfunc Old() {}\n"), 0o644)
		run("add", ".")
		run("commit", "-q", "-m", "b")
		head, _ := git(code, "rev-parse", "HEAD")
		dir := filepath.Dir(bin)
		os.WriteFile(filepath.Join(dir, "rev"), []byte(head), 0o644)
		hit := func(path, content string, line int) string {
			return fmt.Sprintf(`{"result":{"sourcePosition":{"relativePath":%q}},"content":%q,"contentStartLine":%d}`, path, content, line)
		}
		script := `#!/bin/sh
echo "$*" >> "` + calls + `"
case "$1" in
status) echo '{"type":"status_result","indices":[{"snapshots":[{"revision":"` + head + `","createdAt":1}]}]}';;
search) case "$*" in
	*--revision*) echo '{"type":"search_result","results":[` + hit("b.go", "func Old() {}", 3) + `,` + hit("ghost.go", "func Ghost() {}", 1) + `]}';;
	*) echo '{"type":"search_result","results":[` + hit("b.go", "func New() {}", 3) + `,` + hit("ghost.go", "func Ghost() {}", 1) + `]}';;
	esac;;
esac
`
		os.WriteFile(bin, []byte(script), 0o755)
		return jbEnv(bin, code, config{}), code, calls
	}
	t.Run("an edited file shows its current text and a file from another worktree is dropped", func(t *testing.T) {
		e, code, calls := setup(t)
		os.WriteFile(filepath.Join(code, "b.go"), []byte("package a\n\nfunc New() {}\n"), 0o644)
		hits, err := e.Concept("q", "", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Path != "b.go" || !strings.Contains(strings.Join(hits[0].Body, "\n"), "func New()") {
			t.Fatalf("hits = %+v", hits)
		}
		log := callLog(calls)
		if strings.Count(log, "search --limit") != 2 || !strings.Contains(log, "--revision") {
			t.Errorf("a dirty tree needs a pinned and an unpinned search: %s", log)
		}
		wantContains(t, strings.Join(e.Notes(), "|"), "2 hits dropped", "edited in this worktree, so jbcontext may not show their current text: b.go")
	})
	t.Run("a clean tree is searched once, pinned, and still loses a file it does not have", func(t *testing.T) {
		e, _, calls := setup(t)
		hits, err := e.Concept("q", "", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Path != "b.go" || !strings.Contains(strings.Join(hits[0].Body, "\n"), "func Old()") {
			t.Fatalf("hits = %+v", hits)
		}
		if log := callLog(calls); strings.Count(log, "search --limit") != 1 || !strings.Contains(log, "--revision") {
			t.Errorf("calls: %s", log)
		}
	})
}
