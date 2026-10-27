package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a fake `herdr` when ORCH_FAKE_HERDR names its
// state file, so the commands run against a recorded, scriptable Herdr.
func TestMain(m *testing.M) {
	// A fake multi-call binary: records the name it was started under.
	if path := os.Getenv("ORCH_FAKE_ARGV0"); path != "" {
		_ = os.WriteFile(path, []byte(os.Args[0]), 0o600)
		os.Exit(0)
	}
	if path := os.Getenv("ORCH_FAKE_HERDR"); path != "" {
		os.Exit(fakeHerdr(path, os.Args[1:]))
	}
	gateOutsideHerdr = true
	os.Exit(m.Run())
}

type fakeAgent struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Session string `json:"session"`
}

type fakeState struct {
	Agents  map[string]*fakeAgent `json:"agents"`
	Panes   map[string]string     `json:"panes"`
	Next    int                   `json:"next"`
	Session string                `json:"session"`
	Down    bool                  `json:"down"`
	Calls   [][]string            `json:"calls"`
	// Workspaces maps a worktree workspace id to its worktree path.
	Workspaces map[string]string `json:"workspaces"`
	Wsn        int               `json:"wsn"`
	WsRepos    map[string]string `json:"ws_repos"`
}

func loadFake(path string) *fakeState {
	st := &fakeState{Agents: map[string]*fakeAgent{}, Panes: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, st)
	}
	return st
}

func saveFake(path string, st *fakeState) {
	b, _ := json.Marshal(st)
	_ = os.WriteFile(path, b, 0o644)
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func fakeHerdr(path string, args []string) int {
	if code, ok := fakeGatePaneVerbs(path, args); ok {
		return code
	}
	st := loadFake(path)
	st.Calls = append(st.Calls, args)
	defer saveFake(path, st)
	if len(args) < 2 {
		return 2
	}
	print := func(v any) int {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
		return 0
	}
	switch args[0] + " " + args[1] {
	case "agent list":
		if st.Down {
			fmt.Fprintln(os.Stderr, "connection refused")
			return 1
		}
		var agents []map[string]any
		for pane, ag := range st.Agents {
			agents = append(agents, map[string]any{"pane_id": pane, "tab_id": st.Panes[pane], "agent": ag.Kind, "agent_status": ag.Status,
				"agent_session": map[string]any{"value": ag.Session}})
		}
		return print(map[string]any{"result": map[string]any{"agents": agents}})
	case "pane get":
		if st.Down {
			fmt.Fprintln(os.Stderr, "connection refused")
			return 1
		}
		if _, ok := st.Panes[args[2]]; !ok {
			fmt.Fprintf(os.Stderr, `{"error":{"code":"pane_not_found","message":"pane %s not found"}}`+"\n", args[2])
			return 1
		}
		return print(map[string]any{"result": map[string]any{"pane": map[string]any{"pane_id": args[2]}}})
	case "tab create":
		if exists(path + ".tabfail") {
			fmt.Fprintln(os.Stderr, `{"error":{"code":"internal","message":"cannot create a tab"}}`)
			return 1
		}
		st.Next++
		tab, pane := fmt.Sprintf("w1:t%d", st.Next), fmt.Sprintf("w1:p%d", st.Next)
		st.Panes[pane] = tab
		return print(map[string]any{"result": map[string]any{"tab": map[string]any{"tab_id": tab}, "root_pane": map[string]any{"pane_id": pane}}})
	case "pane split":
		st.Next++
		pane := fmt.Sprintf("%s:p%d", strings.SplitN(args[2], ":", 2)[0], st.Next)
		st.Panes[pane] = st.Panes[args[2]]
		return print(map[string]any{"result": map[string]any{"pane": map[string]any{"pane_id": pane}}})
	case "agent start":
		st.Agents[flagValue(args, "--pane")] = &fakeAgent{Name: args[2], Kind: flagValue(args, "--kind"), Status: "idle"}
		return print(map[string]any{})
	case "agent prompt":
		if ag := st.Agents[args[2]]; ag != nil {
			ag.Status = "working"
		}
		return print(map[string]any{})
	case "agent read":
		for i := 1; i <= 250; i++ {
			fmt.Printf("line %d\n", i)
		}
		return 0
	case "pane close":
		delete(st.Panes, args[2])
		delete(st.Agents, args[2])
		ws, _, _ := strings.Cut(args[2], ":") // the last pane of a worktree workspace closes the workspace
		if _, ok := st.Workspaces[ws]; ok && fakePanes(st, ws) == 0 {
			delete(st.Workspaces, ws)
		}
		return print(map[string]any{})
	case "worktree create", "worktree open":
		return fakeWorktree(st, args, print)
	case "worktree remove":
		ws := flagValue(args, "--workspace")
		path, ok := st.Workspaces[ws]
		if !ok {
			fmt.Fprintln(os.Stderr, `{"error":{"code":"workspace_not_found","message":"no such workspace"}}`)
			return 1
		}
		if out, _ := exec.Command("git", "-C", path, "status", "--porcelain").Output(); len(out) > 0 && !slices.Contains(args, "--force") {
			fmt.Fprintln(os.Stderr, `{"error":{"code":"dirty_worktree_requires_force","message":"worktree is dirty"}}`)
			return 1
		}
		rm := []string{"worktree", "remove", path}
		if slices.Contains(args, "--force") {
			rm = append(rm, "--force")
		}
		if out, err := exec.Command("git", append([]string{"-C", st.WsRepos[ws]}, rm...)...).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "git worktree remove: %s\n", out)
			return 1
		}
		for p := range st.Panes {
			if strings.HasPrefix(p, ws+":") {
				delete(st.Panes, p)
				delete(st.Agents, p)
			}
		}
		delete(st.Workspaces, ws)
		return print(map[string]any{"result": map[string]any{"type": "worktree_removed", "workspace_id": ws}})
	case "pane current":
		return print(map[string]any{"result": map[string]any{"pane": map[string]any{"agent_session": map[string]any{"value": st.Session}}}})
	case "pane rename", "agent wait", "agent focus":
		return print(map[string]any{})
	}
	fmt.Fprintf(os.Stderr, "fake herdr: unhandled %v\n", args)
	return 2
}

func fakePanes(st *fakeState, ws string) int {
	n := 0
	for p := range st.Panes {
		if strings.HasPrefix(p, ws+":") {
			n++
		}
	}
	return n
}

// fakeWorktree makes real git worktrees, as Herdr does, and one workspace per path.
func fakeWorktree(st *fakeState, args []string, print func(any) int) int {
	path, cwd := flagValue(args, "--path"), flagValue(args, "--cwd")
	if st.Workspaces == nil {
		st.Workspaces = map[string]string{}
	}
	ws := ""
	for id, p := range st.Workspaces {
		if p == path {
			ws = id
		}
	}
	if args[1] == "create" {
		if exists(os.Getenv("ORCH_FAKE_HERDR") + ".createfail") {
			fmt.Fprintln(os.Stderr, `{"error":{"code":"internal","message":"cannot create a worktree"}}`)
			return 1
		}
		if out, err := exec.Command("git", "-C", cwd, "worktree", "add", "-b", flagValue(args, "--branch"), path, flagValue(args, "--base")).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "git worktree add: %s\n", out)
			return 1
		}
	} else if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(os.Stderr, `{"error":{"code":"worktree_not_found","message":"worktree path not found"}}`)
		return 1
	}
	if ws == "" {
		st.Wsn++
		ws = fmt.Sprintf("wt%d", st.Wsn)
		st.Workspaces[ws] = path
		if st.WsRepos == nil {
			st.WsRepos = map[string]string{}
		}
		st.WsRepos[ws] = cwd
		st.Panes[ws+":p0"] = ws + ":t1"
	}
	root := ws + ":p0"
	for p := range st.Panes {
		if strings.HasPrefix(p, ws+":") && p < root {
			root = p
		}
	}
	return print(map[string]any{"result": map[string]any{
		"workspace": map[string]any{"workspace_id": ws, "pane_count": fakePanes(st, ws)},
		"root_pane": map[string]any{"pane_id": root, "tab_id": st.Panes[root]}}})
}

type harness struct {
	t         *testing.T
	a         *app
	stdin     *bytes.Buffer
	out, errw *bytes.Buffer
	tmp       string
	repo      string
	fake      string
	now       time.Time
	dec       *decStub
}

// decStub stands in for the decisions API so no test reaches the network.
type decStub struct {
	mu     sync.Mutex
	hits   int
	status int
	reply  string
	last   map[string]any
	auth   string
}

func newDecStub(t *testing.T) *decStub {
	d := &decStub{status: 200, reply: decReply(settledAnswers)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/key" {
			fmt.Fprint(w, `{"data":{"limit_remaining":100}}`)
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.hits++
		d.auth = r.Header.Get("Authorization")
		d.last = nil
		json.NewDecoder(r.Body).Decode(&d.last)
		w.WriteHeader(d.status)
		fmt.Fprint(w, d.reply)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ORCH_TRIAGE_URL", srv.URL)
	t.Setenv("ORCH_OPENROUTER_KEY_URL", srv.URL+"/key")
	t.Setenv("ORCH_TRIAGE_KEY", "test-key")
	return d
}

func (d *decStub) set(status int, reply string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status, d.reply = status, reply
}

func (d *decStub) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hits
}

type decVals struct{ clear, guided, open, d0, d1, d2, d3, verifiable, cross, cause, conf float64 }

var (
	settledAnswers  = decVals{0.99, 0.01, 0, 0.9, 0.1, 0, 0, 0.95, 0.05, 0.95, 0.95}
	standardAnswers = decVals{0.2, 0.7, 0.1, 0.1, 0.3, 0.5, 0.1, 0.8, 0.4, 0.9, 0.9}
	planningAnswers = decVals{0.05, 0.15, 0.8, 0, 0.1, 0.4, 0.5, 0.2, 0.9, 0.1, 0.9}
)

func decReply(v decVals) string {
	choice := "clear"
	if v.open > v.clear && v.open > v.guided {
		choice = "open_ended"
	} else if v.guided > v.clear {
		choice = "guided"
	}
	score := v.d1 + 2*v.d2 + 3*v.d3
	return fmt.Sprintf(`{"model":"perplexity/pplx-decider-v1.2-test","answers":{
"clarity":{"type":"choice","choice":%q,"confidence":%g,"probabilities":{"clear":%g,"guided":%g,"open_ended":%g}},
"difficulty":{"type":"score","score":%g,"confidence":%g,"probabilities":{"0":%g,"1":%g,"2":%g,"3":%g}},
"verifiable":{"type":"noul","noul":%g},"cross_cutting":{"type":"noul","noul":%g},"cause_known":{"type":"noul","noul":%g}}}`,
		choice, v.conf, v.clear, v.guided, v.open, score, v.conf, v.d0, v.d1, v.d2, v.d3, v.verifiable, v.cross, v.cause)
}

func setup(t *testing.T) *harness {
	t.Helper()
	oldExt := gatePiExtension
	t.Cleanup(func() { gatePiExtension = oldExt })
	oldWarm := gateWarmDelay
	gateWarmDelay = 0
	oldLimits := [3]time.Duration{gateTimeout, gateStall, gatePoll}
	gatePoll = 20 * time.Millisecond
	t.Cleanup(func() { gateTimeout, gateStall, gatePoll = oldLimits[0], oldLimits[1], oldLimits[2] })
	oldWait, oldPoll := gateLeaveWait, gateLeavePoll
	gateLeaveWait, gateLeavePoll = time.Second, 10*time.Millisecond
	t.Cleanup(func() { gateLeaveWait, gateLeavePoll = oldWait, oldPoll })
	oldStart := gateStartWait
	gateStartWait = 5 * time.Second
	t.Cleanup(func() { gateStartWait = oldStart })
	t.Cleanup(func() { gateWarmDelay = oldWarm })
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	skill, _ := filepath.Abs("..")
	gatePiExtension = filepath.Join(tmp, "no-extension.ts")
	// Gate panes find orch on the pane's own PATH; a pane run checks it is there.
	onPath := filepath.Join(tmp, "path-bin")
	if err := os.MkdirAll(onPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(onPath, "orch"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ORCH_JBCONTEXT", filepath.Join(tmp, "no-jbcontext"))
	t.Setenv("ORCH_GOPLS", filepath.Join(tmp, "no-gopls"))
	t.Setenv("ORCH_TSLS", filepath.Join(tmp, "no-tsls"))
	oldTSReady := tsReady
	tsReady = 0
	t.Cleanup(func() { tsReady = oldTSReady })
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"ORCH_HOME": filepath.Join(tmp, "state"), "ORCH_CACHE_DIR": filepath.Join(tmp, "cache"), "ORCH_ROLES_DIR": filepath.Join(tmp, "roles"),
		"ORCH_CONFIG":    filepath.Join(tmp, "config.toml"),
		"ORCH_LOCKS_DIR": filepath.Join(tmp, "locks"), "ORCH_LOCK_POLL_MS": "50",
		"ORCH_HERDR_CONFIG": filepath.Join(tmp, "none.toml"),
		"ORCH_SKILL_DIR":    skill, "ORCH_SELF": "/skill/scripts/orch",
		"HERDR_ENV": "1", "HERDR_WORKSPACE_ID": "w1", "HERDR_PANE_ID": "w1:p0",
		"ORCH_HERDR": os.Args[0], "ORCH_FAKE_HERDR": filepath.Join(tmp, "herdr.json"),
		"ORCH_ROLE": "", "ORCH_TASK_DIR": "",
		"ORCH_PI": fakeRunner(t, "pi", `case "$*" in *"auth check"*) printf '{"status":"ready"}';; *"auth print-"*) printf 'fake-key';; esac`), "ORCH_CLAUDE": fakeRunner(t, "claude", "exit 0"),
		"CLAUDE_CONFIG_DIR": filepath.Join(tmp, "claude"), "ORCH_PI_DIR": filepath.Join(tmp, "pi"), "ORCH_HEALTH": filepath.Join(tmp, "health.json"),
	} {
		t.Setenv(k, v)
	}
	repo := filepath.Join(tmp, "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo}, {"-C", repo, "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	h := &harness{t: t, stdin: &bytes.Buffer{}, out: &bytes.Buffer{}, errw: &bytes.Buffer{}, tmp: tmp, repo: repo,
		fake: filepath.Join(tmp, "herdr.json"), now: time.Unix(1_800_000_000, 0)}
	// The fake claude in a pane saves its transcript where the runner reads it.
	t.Setenv("ORCH_FAKE_CLAUDE_REPO", repo)
	h.dec = newDecStub(t)
	h.a = &app{cwd: repo, stdin: h.stdin, out: h.out, errw: h.errw, now: func() time.Time { return h.now }}
	return h
}

func (h *harness) run(args ...string) (int, string) {
	h.out.Reset()
	h.errw.Reset()
	code := h.a.run(args)
	return code, h.out.String() + h.errw.String()
}

func (h *harness) must(args ...string) string {
	h.t.Helper()
	code, out := h.run(args...)
	if code != 0 {
		h.t.Fatalf("orch %v exited %d:\n%s", args, code, out)
	}
	return out
}

func (h *harness) hook(event string, payload map[string]any) (int, string) {
	b, _ := json.Marshal(payload)
	h.stdin.Reset()
	h.stdin.Write(b)
	return h.run("hook", event)
}

func (h *harness) dir(id string) string { return filepath.Join(h.tmp, "state", "repo", id) }

var placeholderRe = regexp.MustCompile(`\{\{[^}]*\}\}`)

// task scaffolds and fills a brief ready to spawn.
func (h *harness) task(id string, extra ...string) {
	h.t.Helper()
	h.must(append([]string{"new", id, "--kind", "ship", "--harness", "claude", "--model", "sonnet", "--effort", "high"}, extra...)...)
	path := filepath.Join(h.dir(id), "task.md")
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, placeholderRe.ReplaceAll(b, []byte("filled in")), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) fakeState() *fakeState { return loadFake(h.fake) }

func (h *harness) editFake(fn func(*fakeState)) {
	st := h.fakeState()
	fn(st)
	saveFake(h.fake, st)
}

func (h *harness) lead(id string) *lead {
	h.t.Helper()
	l, err := loadLead(h.dir(id))
	if err != nil || l == nil {
		h.t.Fatalf("no lead.json for %s: %v", id, err)
	}
	return l
}

func callsOf(st *fakeState, prefix ...string) [][]string {
	var out [][]string
	for _, c := range st.Calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			out = append(out, c)
		}
	}
	return out
}

func wantContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output lacks %q:\n%s", w, got)
		}
	}
}

func TestLifecycle(t *testing.T) {
	h := setup(t)
	h.must("new", "t1", "--kind", "ship", "--harness", "claude", "--model", "sonnet", "--effort", "high")
	code, out := h.run("check", "t1")
	if code != 1 {
		t.Fatalf("check on a fresh scaffold exited %d, want 1", code)
	}
	wantContains(t, out, "refused: unfilled {{placeholder}}")

	brief, _ := os.ReadFile(filepath.Join(h.dir("t1"), "task.md"))
	wantContains(t, string(brief), "id: t1", "base: main@", "/skill/scripts/orch report --dir "+h.dir("t1"), "references/lead.md",
		"commits on branch `orch/t1`, in your own worktree `"+filepath.Join(h.tmp, "state", "repo", "worktrees", "t1")+"`", "do not run `git worktree add`")
	os.Remove(filepath.Join(h.dir("t1"), "task.md"))
	h.task("t1")
	wantContains(t, h.must("check", "t1"), "t1: ok")

	out = h.must("spawn", "t1")
	wantContains(t, out, "spawned lead-t1 (claude sonnet) in pane wt1:p1", "no watcher running")
	l := h.lead("t1")
	if l.PaneID != "wt1:p1" || l.TabID != "wt1:t1" || l.WorkspaceID != "wt1" {
		t.Fatalf("lead placed at %s/%s in %s", l.TabID, l.PaneID, l.WorkspaceID)
	}
	st := h.fakeState()
	if len(callsOf(st, "tab", "create")) != 0 {
		t.Fatal("a ship lead lives in its own workspace, not a tab of the orchestrator's")
	}
	start := callsOf(st, "agent", "start", "lead-t1")
	if len(start) != 1 || !slices.Equal(start[0][len(start[0])-5:], []string{"--", "--model", "sonnet", "--effort", "high"}) {
		t.Fatalf("agent start = %v", start)
	}
	if p := callsOf(st, "agent", "prompt", "wt1:p1"); len(p) != 1 || !strings.Contains(p[0][3], filepath.Join(h.dir("t1"), "task.md")) {
		t.Fatalf("launch prompt = %v", p)
	}

	h.must("report", "--dir", h.dir("t1"), "working", "reading the code")
	h.must("report", "--dir", h.dir("t1"), "done", "branch feat/x at abc123")
	out = h.must("watch", "--once")
	if want := "orch: t1 done: branch feat/x at abc123\n"; out != want {
		t.Fatalf("watch printed %q, want %q", out, want)
	}
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("second scan repeated events: %q", out)
	}
	wantContains(t, h.must("status"), "t1", "done: branch feat/x", "leads open: 1/3")

	wantContains(t, h.must("close", "t1"), "closed lead-t1")
	if _, ok := h.fakeState().Panes["wt1:p1"]; ok {
		t.Fatal("pane still open after close")
	}
	if out := h.must("watch", "--once"); !strings.Contains(out, "no open leads") {
		t.Fatalf("watch with no leads printed %q", out)
	}
}

func TestSpawnGridAndCap(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("max_leads = 5\nclaude_args = [\"--permission-mode\", \"auto\"]\n"), 0o644)
	ids := []string{"a", "b", "c", "d", "e", "f"}
	for _, id := range ids {
		h.task(id, "--kind", "scout") // ship leads get a workspace of their own, scouts share the grid
	}
	for _, id := range ids[:5] {
		h.now = h.now.Add(time.Second)
		h.must("spawn", id)
	}
	pa, pb, pc := h.lead("a").PaneID, h.lead("b").PaneID, h.lead("c").PaneID
	var splits [][2]string
	for _, c := range callsOf(h.fakeState(), "pane", "split") {
		splits = append(splits, [2]string{c[2], flagValue(c, "--direction")})
	}
	want := [][2]string{{pa, "right"}, {pa, "down"}, {pb, "down"}}
	if !slices.Equal(splits, want) {
		t.Fatalf("splits = %v, want %v (c=%s)", splits, want, pc)
	}
	creates := callsOf(h.fakeState(), "tab", "create")
	if len(creates) != 2 || flagValue(creates[1], "--label") != "leads-2" {
		t.Fatalf("tab creates = %v", creates)
	}
	if h.lead("d").TabID != h.lead("a").TabID || h.lead("e").TabID == h.lead("a").TabID {
		t.Fatal("fifth lead should open a second tab, the first four share one")
	}
	start := callsOf(h.fakeState(), "agent", "start", "lead-a")[0]
	if !slices.Equal(start[len(start)-2:], []string{"--permission-mode", "auto"}) {
		t.Fatalf("claude_args not passed: %v", start)
	}
	code, out := h.run("spawn", "f")
	if code != 75 || !strings.Contains(out, "deferred: 5/5 leads open") {
		t.Fatalf("spawn over the cap = %d %q", code, out)
	}
	code, out = h.run("spawn", "a")
	if code != 1 || !strings.Contains(out, "already open") {
		t.Fatalf("respawn of an open lead = %d %q", code, out)
	}
}

func TestCheckRefusals(t *testing.T) {
	h := setup(t)
	h.task("dep")
	h.task("t1", "--depends", "dep")
	path := filepath.Join(h.dir("t1"), "task.md")
	b, _ := os.ReadFile(path)
	text := strings.Replace(string(b), "Out: filled in", "", 1)
	text = strings.Replace(text, "# Pointers\n\nfilled in", "# Pointers\n", 1)
	os.WriteFile(path, []byte(text), 0o644)
	code, out := h.run("check", "t1")
	if code != 1 {
		t.Fatalf("check exited %d", code)
	}
	wantContains(t, out, "Scope needs a non-empty `Out:` line", "section `# Pointers` is empty", `depends_on: "dep" is not done yet`)

	h.must("report", "--dir", h.dir("dep"), "done", "merged")
	code, out = h.run("check", "t1")
	if strings.Contains(out, "depends_on") || code != 1 {
		t.Fatalf("dependency done but still refused: %s", out)
	}

	code, out = h.run("new", "Bad_ID", "--kind", "ship", "--harness", "claude", "--model", "x", "--effort", "y")
	if code != 1 || !strings.Contains(out, "must match") {
		t.Fatalf("bad id accepted: %d %s", code, out)
	}
	code, out = h.run("new", "t2", "--kind", "ship", "--harness", "claude", "--model", "x", "--effort", "y", "--depends", "../x")
	if code != 1 || !strings.Contains(out, "not a task id") {
		t.Fatalf("path-like dependency accepted: %d %s", code, out)
	}
}

func TestSpawnPiLead(t *testing.T) {
	h := setup(t)
	h.task("t1", "--harness", "pi", "--model", "openrouter/deepseek/deepseek-chat-v3.1", "--effort", "high")
	h.must("spawn", "t1")
	start := callsOf(h.fakeState(), "agent", "start", "lead-t1")
	if len(start) != 1 || flagValue(start[0], "--kind") != "pi" || len(start[0]) < 7 ||
		!slices.Equal(start[0][len(start[0])-7:len(start[0])-2], []string{"--", "--model", "openrouter/deepseek/deepseek-chat-v3.1", "--thinking", "high"}) {
		t.Fatalf("agent start = %v", start)
	}
	l, _ := loadLead(h.dir("t1"))
	if l.Session == "" || flagValue(start[0], "--session-id") != l.Session {
		t.Fatalf("pi session id %q not passed to the agent: %v", l.Session, start)
	}
	if code, out := h.run("new", "t2", "--kind", "ship", "--harness", "codex", "--model", "x", "--effort", "y"); code != 1 || !strings.Contains(out, "claude or pi") {
		t.Fatalf("codex harness accepted: %d %s", code, out)
	}
}

func TestHealthRefusesLimitedHarness(t *testing.T) {
	h := setup(t)
	h.task("t3") // made before any limit
	wantContains(t, h.must("health", "set", "claude", "limited", "--for", "2h", "--reason", "weekly cap"), "claude: limited since", "(weekly cap)")
	wantContains(t, h.must("health"), "claude: limited", "pi: ok")
	wantContains(t, h.must("status"), "health: claude limited since")
	code, out := h.run("new", "t1", "--kind", "ship", "--harness", "claude", "--model", "sonnet", "--effort", "high")
	if code != 1 || !strings.Contains(out, "claude is limited") || !strings.Contains(out, "orch health set claude ok") {
		t.Fatalf("limited harness accepted: %d %s", code, out)
	}
	if code, out := h.run("new", "t2", "--kind", "ship", "--harness", "pi", "--model", "m", "--effort", "high"); code != 0 {
		t.Fatalf("healthy harness refused: %d %s", code, out)
	}
	// a task made before the limit cannot spawn into it
	if code, out := h.run("spawn", "t3"); code != 1 || !strings.Contains(out, "claude is limited") {
		t.Fatalf("spawn into a limited harness: %d %s", code, out)
	}
	h.must("health", "set", "claude", "ok")
	wantContains(t, h.must("health"), "claude: ok")
	// the limit clears itself after its duration
	h.must("health", "set", "pi", "limited", "--for", "1h")
	if got := loadHealth(h.now.Add(2 * time.Hour)); len(got) != 0 {
		t.Fatalf("limit did not expire: %v", got)
	}
	if got := loadHealth(h.now.Add(30 * time.Minute)); len(got) != 1 {
		t.Fatalf("limit expired early: %v", got)
	}
}

func TestDegradedFallbackNeedsApproval(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(`[[dispatch]]
when = "design work"
use = [{ harness = "claude", model = "opus" }, { harness = "pi", model = "flash", degraded = true }]
`), 0o644)
	wantContains(t, h.must("dispatch"), "now: claude opus")
	h.must("health", "set", "claude", "limited")
	wantContains(t, h.must("dispatch"), "now: parked (the primary is limited). Degraded option: pi flash [degraded]")
	code, out := h.run("new", "d1", "--kind", "ship", "--harness", "pi", "--model", "flash", "--effort", "high")
	if code != 1 || !strings.Contains(out, "degraded fallback") {
		t.Fatalf("degraded profile used without approval: %d %s", code, out)
	}
	h.must("new", "d1", "--kind", "ship", "--harness", "pi", "--model", "flash", "--effort", "high", "--degraded")
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("d1"), "task.md"))), "degraded: true")
	wantContains(t, h.must("status"), "degraded: d1")
	// a non-degraded use of the same harness is not caught
	h.must("new", "d2", "--kind", "ship", "--harness", "pi", "--model", "other", "--effort", "high")
}

func TestRespawnOnAnotherHarness(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	if code, out := h.run("respawn", "t1"); code != 1 || !strings.Contains(out, "no handoff.md") {
		t.Fatalf("respawn without a handoff or a harness: %d %s", code, out)
	}
	if code, out := h.run("respawn", "t1", "--harness", "pi"); code != 1 || !strings.Contains(out, "needs --model") {
		t.Fatalf("a harness switch kept a model name from the old harness: %d %s", code, out)
	}
	h.must("health", "set", "pi", "limited")
	if code, out := h.run("respawn", "t1", "--harness", "pi", "--model", "flash"); code != 1 || !strings.Contains(out, "pi is limited") {
		t.Fatalf("respawn onto a limited harness: %d %s", code, out)
	}
	h.must("health", "set", "pi", "ok")
	h.must("respawn", "t1", "--harness", "pi", "--model", "flash")
	fm, _, _ := readTask(filepath.Join(h.dir("t1"), "task.md"))
	if fm["harness"] != "pi" || fm["model"] != "flash" || fm["effort"] != "high" {
		t.Fatalf("task front matter after respawn = %v", fm)
	}
	starts := callsOf(h.fakeState(), "agent", "start", "lead-t1")
	if len(starts) != 2 || flagValue(starts[1], "--kind") != "pi" {
		t.Fatalf("agent starts = %v", starts)
	}
	prompts := callsOf(h.fakeState(), "agent", "prompt", flagValue(starts[1], "--pane"))
	last := prompts[len(prompts)-1][3]
	if !strings.Contains(last, "left no handoff") || !strings.Contains(last, "status.log") {
		t.Fatalf("recovery brief = %q", last)
	}
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("t1"), "status.log"))), "respawned on pi")
}

func TestFrozenReason(t *testing.T) {
	esc := entry{State: "escalate", Text: "x"}
	resp := entry{State: "note", Text: "closed for respawn on pi", By: "orch"}
	cases := []struct {
		name    string
		entries []entry
		frozen  bool
	}{
		{"two escalations are allowed", []entry{esc, esc}, false},
		{"the third escalation freezes", []entry{esc, esc, esc}, true},
		{"two respawns are allowed", []entry{resp, resp}, false},
		{"the third respawn freezes", []entry{resp, resp, resp}, true},
		{"unfreeze resets the count", []entry{esc, esc, esc, {State: "note", Text: unfreezeNote, By: "orch"}, esc}, false},
		{"two stalls are allowed", []entry{{State: "note", Text: "stalled: x", By: "orch"}, {State: "note", Text: "looping: y", By: "orch"}}, false},
		{"the third stall freezes", []entry{{State: "note", Text: "stalled: x", By: "orch"}, {State: "note", Text: "looping: y", By: "orch"}, {State: "note", Text: "stalled: z", By: "orch"}}, true},
		{"three identical reports freeze", []entry{{State: "working", Text: "retrying"}, {State: "working", Text: "retrying"}, {State: "working", Text: "retrying"}}, true},
		{"progress between reports does not", []entry{{State: "working", Text: "a"}, {State: "working", Text: "b"}, {State: "working", Text: "a"}}, false},
		{"orch notes are not the lead's loop", []entry{{State: "working", Text: "a"}, {State: "note", Text: "n", By: "orch"}, {State: "working", Text: "a"}}, false},
	}
	for _, c := range cases {
		if got := frozenReason(c.entries) != ""; got != c.frozen {
			t.Errorf("%s: frozen = %v, want %v", c.name, got, c.frozen)
		}
	}
}

func TestFreezeHoldAndDoctor(t *testing.T) {
	h := setup(t)
	t.Setenv("HOME", t.TempDir())
	for _, line := range strings.Split(h.must("doctor"), "\n") {
		if strings.HasPrefix(line, "  ") && !strings.Contains(line, ",setup,") {
			t.Errorf("unexpected doctor finding: %s", line)
		}
	}
	for _, id := range []string{"t1", "t2"} {
		h.task(id)
		h.must("spawn", id)
		for range 3 {
			h.must("report", "--dir", h.dir(id), "working", "retrying the migration")
		}
	}
	wantContains(t, h.must("status"), "FROZEN: t1 - the same working report 3 times in a row", "FROZEN: t2")
	if code, out := h.run("respawn", "t1", "--harness", "pi", "--model", "flash"); code != 1 || !strings.Contains(out, "t1 is frozen") {
		t.Fatalf("respawn of a frozen task: %d %s", code, out)
	}
	h.task("t3")
	if code, out := h.run("spawn", "t3"); code != 1 || !strings.Contains(out, "2 tasks are frozen") {
		t.Fatalf("spawn with two frozen tasks: %d %s", code, out)
	}
	code, out := h.run("doctor")
	if code != 1 {
		t.Fatalf("doctor exit = %d, want 1\n%s", code, out)
	}
	wantContains(t, out, `error,t1,"frozen:`, "orch unfreeze t1", "error,watcher,2 lead(s) in flight and no watcher")
	wantContains(t, h.must("unfreeze", "t1"), "unfroze t1")
	if strings.Contains(h.must("status"), "FROZEN: t1") {
		t.Fatal("unfreeze did not clear the freeze")
	}
	h.must("spawn", "t3") // only one frozen task left
	h.must("close", "t3", "--force")
	wantContains(t, h.must("hold", "on", "checking", "the", "database"), "hold: on (checking the database)")
	wantContains(t, h.must("status"), "HOLD: checking the database")
	h.task("t4")
	if code, out := h.run("spawn", "t4"); code != 1 || !strings.Contains(out, "spawning is on hold") {
		t.Fatalf("spawn during a hold: %d %s", code, out)
	}
	h.must("hold", "off")
	h.must("spawn", "t4")
}

// fakePi installs a stand-in for pi that answers each gate pass from
// <dir>/<pass>.json and records every call in <dir>/calls.log.
func (h *harness) fakePi(passes map[string]string) string {
	h.t.Helper()
	dir := filepath.Join(h.tmp, "fakepi")
	os.MkdirAll(dir, 0o755)
	script := `#!/bin/sh
echo "$*" >> "` + dir + `/calls.log"
echo "$PI_CACHE_RETENTION" >> "` + dir + `/cache.log"
# Stand in for the loaded extension's session_start acknowledgement.
[ -n "$ORCH_CTX_ONLY_READY" ] && printf 'ctx-only\n' > "$ORCH_CTX_ONLY_READY"
case "$*" in *GATE-PLAN*) case "$*" in *--no-tools*) echo no-tools >> "` + dir + `/plan.log";; *) echo tools >> "` + dir + `/plan.log";; esac;; esac
case "$*" in
*GATE-GROUP*) f=group; ids=$(echo "$*" | sed -n 's/.*Packages in this pass: \([a-z0-9 ,_-]*\)\..*/\1/p' | head -1 | tr -d ,)
	[ -f "` + dir + `/group.json" ] && { cat "` + dir + `/group.json"; exit 0; }
	out=""; sep=""
	for id in $ids; do
		if [ -f "` + dir + `/package-$id.json" ]; then frag=$(cat "` + dir + `/package-$id.json"); else frag=$(sed "s/@ID@/$id/g" "` + dir + `/package.json"); fi
		out="$out$sep$frag"; sep=","
	done
	echo "{\"packages\":[$out]}"; exit 0;;
*GATE-ADVERSARY-A*) f=adv-a;; *GATE-ADVERSARY-B*) f=adv-b;; *GATE-PLAN*) f=plan;; esac
[ -f "` + dir + `/$f.fail" ] && { echo boom >&2; exit 1; }
cat "` + dir + `/$f.json"
`
	bin := filepath.Join(dir, "pi")
	os.WriteFile(bin, []byte(script), 0o755)
	old, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	fails, _ := filepath.Glob(filepath.Join(dir, "*.fail"))
	for _, f := range append(old, fails...) {
		os.Remove(f)
	}
	os.Remove(filepath.Join(dir, "calls.log"))
	os.Remove(filepath.Join(dir, "cache.log"))
	os.Remove(filepath.Join(dir, "plan.log"))
	for name, body := range passes {
		if body == "FAIL" {
			os.WriteFile(filepath.Join(dir, name+".fail"), nil, 0o644)
		} else {
			os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644)
		}
	}
	h.t.Setenv("ORCH_PI", bin)
	return dir
}

var gateClean = map[string]string{
	// package answers every package pass; package-api answers the api one, so a
	// package never reports another package's files.
	"package":     `{"id":"@ID@","will_change":[],"claims":[{"claim":"store has Put","evidence":"internal/store.go:3"}],"questions":[],"reuse":[]}`,
	"package-api": `{"id":"api","will_change":["internal/api.go"],"claims":[{"claim":"store has Put","evidence":"internal/store.go:3"}],"questions":[{"package":"api","question":"Which table?","answered_by":"internal/store.go:3","changes_result":false}],"reuse":[]}`,
	"adv-a":       `{"findings":[]}`,
	"adv-b":       `{"findings":[]}`,
}

func gatePasses(over map[string]string) map[string]string {
	m := map[string]string{}
	maps.Copy(m, gateClean)
	maps.Copy(m, over)
	return m
}

func TestGateEvidenceDecidesTheVerdict(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, "internal"), 0o755)
	os.WriteFile(filepath.Join(h.repo, "internal", "store.go"), []byte("package internal\n\nfunc Put() {}\n"), 0o644)
	h.commitSpec(threePkgSpec())

	// Nothing has run the gate yet: plan refuses.
	if code, out := h.run("plan", "spec.md"); code != 1 || !strings.Contains(out, "gate has not passed") {
		t.Fatalf("plan without a gate: %d %s", code, out)
	}

	// An unsupported blocking claim is not accepted, but the required check
	// stays incomplete until evidence supports or rejects the interpretation.
	h.fakePi(gatePasses(map[string]string{"adv-a": `{"findings":[{"severity":"blocker","where":"Design rules","claim":"x","evidence":"internal/nope.go:9"}]}`}))
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("unsupported blocking claim passed: %d %s", code, out)
	}
	view := wantGateState(t, out, "incomplete")
	if view.Blockers != "0 verified" || view.Concerns != "0 verified, 1 unverified" || view.Dropped != "1 claim(s) without real evidence" {
		t.Fatalf("incorrect finding accounting: %+v", view)
	}
	gdir := gateDir(h.a.siteRoot(t), specHash(mustRead(t, filepath.Join(h.repo, "spec.md"))))
	rec := readRecord(t, gdir)
	if rec.State != "incomplete" || len(rec.Reduction.Reasons) == 0 || rec.Reduction.Reasons[0].Reason != reviewUnsupportedClaim {
		t.Fatalf("unsupported claim did not remain incomplete: %+v", rec.Reduction)
	}
	if code, _ := h.run("plan", "spec.md"); code != 1 {
		t.Fatal("unsupported claim authorized plan")
	}
	h.fakePi(gatePasses(nil))
	h.must("gate", "spec.md")
	h.must("plan", "spec.md") // only a complete supported review unlocks plan

	// A valid locator plus separate behavioral support is an accepted blocker.
	h.fakePi(gatePasses(map[string]string{"adv-b": `{"findings":[{"severity":"blocker","where":"Design rules","claim":"Put ignores errors","evidence":"internal/store.go:3","why":"The failure path does not report storage errors","support_assessment":{"schema_version":1,"state":"supported","rationale":"The Put implementation has no error return","excerpt":"func Put() {}","failure_scenario":"A failed write cannot be reported to its caller"}}]}`}))
	code, out = h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("a verified blocker passed the gate: %d %s", code, out)
	}
	if view := wantGateState(t, out, "fail"); view.Blockers != "1 verified" {
		t.Fatalf("missing accepted blocker count: %+v", view)
	}
	wantContains(t, string(mustRead(t, filepath.Join(filepath.Dir(h.dir("x")), "gate", specHash(mustRead(t, filepath.Join(h.repo, "spec.md"))), "report.md"))), "Put ignores errors")
}

func TestGateQuestionsScopeAndFailures(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, "internal"), 0o755)
	os.WriteFile(filepath.Join(h.repo, "internal", "store.go"), []byte("package internal\n\nfunc Put() {}\n"), 0o644)
	h.commitSpec(threePkgSpec())
	hash := specHash(mustRead(t, filepath.Join(h.repo, "spec.md")))
	gdir := filepath.Join(filepath.Dir(h.dir("x")), "gate", hash)

	// An unanswered, decision-ready question fails the gate and becomes a card orch ask renders.
	jb, _, _ := fakeJbcontext(t)
	t.Setenv("ORCH_JBCONTEXT", jb)
	question := gateQuestion{Package: "api", Question: "Soft or hard delete?", Decisive: true, Packet: &gateQuestionPacket{
		Goal: "Choose deletion behavior", KnownFacts: []string{"internal/store.go defines Put; deletion behavior is not specified"}, Unknown: "Should deleted records remain recoverable?",
		Options:        []gatePacketOption{{"soft", "Soft delete", "Retains recoverable records", "Requires filtering deleted records"}, {"hard", "Hard delete", "Removes records permanently", "Prevents recovery"}},
		Recommendation: "soft", Rationale: "Retains recovery until the owner confirms permanent deletion", Authority: "Owner decides retention", NextAction: "Record the owner's deletion choice",
	}}
	reply, err := json.Marshal(map[string]any{"id": "api", "will_change": []string{}, "claims": []any{}, "reuse": []any{}, "questions": []gateQuestion{question}})
	if err != nil {
		t.Fatal(err)
	}
	h.fakePi(gatePasses(map[string]string{"package-api": string(reply)}))
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("an open question passed the gate: %d %s", code, out)
	}
	view := wantGateState(t, out, "incomplete")
	if view.Questions != "1 need a decision" || len(view.Help) != 1 || view.Help[0] != "orch ask --cards "+filepath.Join(gdir, "questions") {
		t.Fatalf("missing question or next action: %+v", view)
	}
	wantContains(t, h.must("ask", "--cards", filepath.Join(gdir, "questions")), "questions: 1")

	// A question whose answer changes nothing visible does not block.
	h.fakePi(gatePasses(map[string]string{"package-api": `{"id":"api","will_change":[],"claims":[],"reuse":[],"questions":[{"package":"api","question":"Where does sub go?","answered_by":"","changes_result":false}]}`}))
	if view := wantGateState(t, h.must("gate", "spec.md"), "pass"); view.Questions != "0 need a decision" {
		t.Fatalf("nondecisive question blocked: %+v", view)
	}

	// An answer that cites a line the repo does not have is no answer.
	h.fakePi(gatePasses(map[string]string{"package-api": `{"id":"api","will_change":[],"claims":[],"reuse":[],"questions":[{"package":"api","question":"Which table?","answered_by":"internal/store.go:99","changes_result":true}]}`}))
	if code, out := h.run("gate", "spec.md"); code != 1 || gateView(t, out).Questions != "1 need a decision" {
		t.Fatalf("a fake answer counted: %d %s", code, out)
	}

	// A package that must change a file another package owns is a blocker.
	h.fakePi(gatePasses(map[string]string{"package-api": `{"id":"api","will_change":["internal/store.go"],"claims":[],"questions":[],"reuse":[]}`}))
	if code, out := h.run("gate", "spec.md"); code != 1 || gateView(t, out).Blockers != "1 verified" {
		t.Fatalf("ownership conflict passed: %d %s", code, out)
	}

	// A pass that fails to run leaves required review incomplete, never passed.
	h.fakePi(gatePasses(map[string]string{"adv-a": "FAIL"}))
	code, out = h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("a failed pass passed the gate: %d %s", code, out)
	}
	if view := wantGateState(t, out, "incomplete"); view.PassErrors["adversary-a"] == "" {
		t.Fatalf("missing pass failure: %+v", view)
	}
	if code, _ := h.run("plan", "spec.md"); code != 1 {
		t.Fatal("plan accepted an incomplete gate")
	}

	// With only a pi choice and pi limited, no model pass runs.
	dir := h.fakePi(gatePasses(nil))
	h.must("health", "set", "pi", "limited")
	code, out = h.run("gate", "spec.md", "--runner", "pi:"+gateModelDef)
	if code != 1 || gateView(t, out).State != "incomplete" {
		t.Fatalf("gate with pi limited: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls.log")); err == nil {
		t.Fatal("pi was called although it is marked limited")
	}
}

func TestGateDeterministicChecks(t *testing.T) {
	h := setup(t)
	spec := strings.Replace(threePkgSpec(), "Tests for api pass.", "`scripts/missing-check go ./...` passes; see internal/ghost.go:12.", 1)
	if spec == threePkgSpec() {
		t.Fatal("test spec was not modified")
	}
	h.commitSpec(spec)
	h.fakePi(gatePasses(nil))
	code, out := h.run("gate", "spec.md")
	if code != 1 {
		t.Fatalf("a spec with a missing command and pointer passed: %d %s", code, out)
	}
	if view := wantGateState(t, out, "fail"); view.Deterministic != "3 issue(s)" {
		t.Fatalf("incorrect deterministic count: %+v", view)
	}
	hash := specHash(mustRead(t, filepath.Join(h.repo, "spec.md")))
	report := string(mustRead(t, filepath.Join(filepath.Dir(h.dir("x")), "gate", hash, "report.md")))
	wantContains(t, report, `acceptance command "scripts/missing-check" is not in the repo`, "pointer internal/ghost.go:12 does not resolve")
}

func TestLoopingCall(t *testing.T) {
	rep := func(sig string, n int) []string { return slices.Repeat([]string{sig}, n) }
	cases := []struct {
		name string
		sigs []string
		want bool
	}{
		{"nothing", nil, false},
		{"four in a row is not enough", rep("bash\x00a", 4), false},
		{"five in a row", rep("bash\x00a", 5), true},
		{"varied calls", []string{"a\x001", "b\x002", "a\x003", "c\x004", "a\x005", "d\x006"}, false},
		{"eight of twelve with others between", append(append(rep("bash\x00a", 4), "x\x00", "y\x00"), append(rep("bash\x00a", 4), "z\x00", "w\x00")...), true},
		{"an old run outside the window", append(rep("bash\x00a", 9), []string{"b\x00", "c\x00", "d\x00", "e\x00", "f\x00", "g\x00", "h\x00", "i\x00", "j\x00", "k\x00", "l\x00", "m\x00"}...), false},
	}
	for _, c := range cases {
		if name, _, ok := loopingCall(c.sigs); ok != c.want || (ok && name == "") {
			t.Errorf("%s: ok = %v (name %q), want %v", c.name, ok, name, c.want)
		}
	}
}

func TestCallSignaturesAndFingerprint(t *testing.T) {
	h := setup(t)
	h.transcript("claude", "c1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test","description":"d"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"description":"d","command":"go test"}}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"sub"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"tool_use in prose"}]}}`,
	)
	got := callSignatures("claude", "c1")
	if len(got) != 2 || got[0] != got[1] || !strings.HasPrefix(got[0], "Bash\x00") {
		t.Fatalf("claude signatures = %q (same call with keys reordered must match; a subagent's call must not count)", got)
	}
	h.transcript("pi", "p1", `{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{"path":"a.go"}}]}}`)
	if got := callSignatures("pi", "p1"); len(got) != 1 || !strings.HasPrefix(got[0], "read\x00") {
		t.Fatalf("pi signatures = %q", got)
	}

	wt := filepath.Join(h.tmp, "fpwt")
	if out, err := exec.Command("git", "-C", h.repo, "worktree", "add", "-q", "-b", "fp", wt, "main").CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v\n%s", err, out)
	}
	base := worktreeFingerprint(wt)
	if base == "" || base != worktreeFingerprint(wt) {
		t.Fatalf("fingerprint must be stable for an unchanged worktree: %q", base)
	}
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("one\n"), 0o644)
	withNew := worktreeFingerprint(wt)
	if withNew == base {
		t.Fatal("a new untracked file did not change the fingerprint")
	}
	h.gitIn(wt, "add", "a.txt")
	h.gitIn(wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "a")
	committed := worktreeFingerprint(wt)
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("two\n"), 0o644)
	if worktreeFingerprint(wt) == committed {
		t.Fatal("an edit to a tracked file did not change the fingerprint")
	}
	if worktreeFingerprint(filepath.Join(h.tmp, "nope")) != "" {
		t.Fatal("a missing worktree must fingerprint as empty")
	}
}

func (h *harness) gitIn(dir string, args ...string) {
	h.t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestWatchStalled(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	wt := filepath.Join(filepath.Dir(h.dir("t1")), "worktrees", "t1")
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("a fresh working lead alerted: %q", out)
	}
	h.now = h.now.Add(10 * time.Minute)
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("alerted before stall_minutes: %q", out)
	}
	// Work in the tree counts as progress, so the clock restarts.
	os.WriteFile(filepath.Join(wt, "x.go"), []byte("package x\n"), 0o644)
	h.now = h.now.Add(10 * time.Minute)
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("alerted although the worktree changed: %q", out)
	}
	h.now = h.now.Add(16 * time.Minute)
	wantContains(t, h.must("watch", "--once"), "orch: t1 stalled: no report and no change to the worktree for 16m")
	h.now = h.now.Add(time.Hour)
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("stalled repeated: %q", out)
	}
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("t1"), "status.log"))), "stalled: no report")
	// A report restarts the clock and re-arms the alert.
	h.must("report", "--dir", h.dir("t1"), "working", "back on it")
	h.must("watch", "--once") // the watcher sees the report as it is written
	h.now = h.now.Add(16 * time.Minute)
	wantContains(t, h.must("watch", "--once"), "orch: t1 stalled")
}

func TestWatchLooping(t *testing.T) {
	h := setup(t)
	h.task("t1", "--harness", "pi", "--model", "m", "--effort", "low")
	h.must("spawn", "t1")
	l, _ := loadLead(h.dir("t1"))
	call := `{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{"command":"go test ./..."}}]}}`
	h.transcript("pi", l.Session, slices.Repeat([]string{call}, 8)...)
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("looping alerted before the worktree stood still: %q", out)
	}
	h.now = h.now.Add(3 * time.Minute)
	wantContains(t, h.must("watch", "--once"), "orch: t1 looping: it called bash with the same arguments 8 times")
	h.now = h.now.Add(3 * time.Minute)
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("looping repeated for the same call: %q", out)
	}
}

func TestTierFor(t *testing.T) {
	mk := func(v decVals) triageAnswers {
		var r struct {
			Answers map[string]decAnswer `json:"answers"`
		}
		json.Unmarshal([]byte(decReply(v)), &r)
		c, d := r.Answers["clarity"], r.Answers["difficulty"]
		return triageAnswers{Clarity: c.Choice, ClarityP: c.Probabilities, ClarityConf: c.Confidence, Difficulty: d.Score,
			DifficultyP: d.Probabilities, DiffConf: d.Confidence, Verifiable: r.Answers["verifiable"].Noul,
			CrossCutting: r.Answers["cross_cutting"].Noul, CauseKnown: r.Answers["cause_known"].Noul}
	}
	with := func(v decVals, f func(*decVals)) decVals { f(&v); return v }
	cases := []struct {
		name string
		v    decVals
		want string
	}{
		{"clear, easy, verifiable", settledAnswers, "settled"},
		{"guided and medium", standardAnswers, "standard"},
		{"open-ended", planningAnswers, "planning"},
		{"hard but otherwise clear", with(settledAnswers, func(v *decVals) { v.d0, v.d1, v.d3 = 0.4, 0.3, 0.3 }), "planning"},
		{"unknown cause", with(settledAnswers, func(v *decVals) { v.cause = 0.1 }), "planning"},
		{"no check that can fail", with(settledAnswers, func(v *decVals) { v.verifiable = 0.5 }), "standard"},
		{"touches a shared interface", with(settledAnswers, func(v *decVals) { v.cross = 0.6 }), "standard"},
		{"just under the clear threshold", with(settledAnswers, func(v *decVals) { v.clear, v.guided = 0.79, 0.21 }), "standard"},
		{"low confidence moves settled up", with(settledAnswers, func(v *decVals) { v.conf = 0.5 }), "standard"},
		{"low confidence moves standard up", with(standardAnswers, func(v *decVals) { v.conf = 0.5 }), "planning"},
	}
	for _, c := range cases {
		if got, why := tierFor(mk(c.v)); got != c.want {
			t.Errorf("%s: tier = %s (%s), want %s", c.name, got, why, c.want)
		}
	}
}

func TestTriageShadowRecordsAndNeverBlocks(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("weak_models = [\"flash\"]\n"), 0o644)
	h.dec.set(200, decReply(standardAnswers))
	h.task("t1", "--harness", "pi", "--model", "flash", "--effort", "low")
	out := h.must("spawn", "t1")
	wantContains(t, out, "warning: triage rates t1 as standard work", "shadow mode: not blocked", "spawned lead-t1")
	if h.dec.count() != 1 || h.dec.auth != "Bearer test-key" {
		t.Fatalf("decider hits %d, auth %q", h.dec.count(), h.dec.auth)
	}
	body, _ := json.Marshal(h.dec.last)
	wantContains(t, string(body), "perplexity/pplx-decider-v1.2-20261016", `"acceptance"`, `"clarity"`, `"cause_known"`)
	fm, _, _ := readTask(filepath.Join(h.dir("t1"), "task.md"))
	if fm["tier"] != "standard" {
		t.Fatalf("task.md tier = %q", fm["tier"])
	}
	recs := readTriage(filepath.Join(filepath.Dir(h.dir("t1"))))
	if len(recs) != 1 || recs[0].Tier != "standard" || recs[0].Mode != "shadow" || recs[0].Actual != "pi flash" {
		t.Fatalf("records = %+v", recs)
	}
	// Unchanged task text is not asked twice; --force asks again.
	h.must("triage", "t1")
	if h.dec.count() != 1 {
		t.Fatalf("an unchanged task was asked again: %d hits", h.dec.count())
	}
	h.must("triage", "t1", "--force")
	if h.dec.count() != 2 {
		t.Fatalf("--force did not ask again: %d hits", h.dec.count())
	}
	// An unreachable decision model warns and still spawns in shadow mode.
	h.task("t2")
	h.dec.set(500, `{"error":"down"}`)
	wantContains(t, h.must("spawn", "t2"), "warning: triage unavailable", "spawned lead-t2")
}

func TestTriageEnforce(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("max_leads = 8\ntriage_mode = \"enforce\"\nweak_models = [\"flash\"]\n"), 0o644)
	h.task("t1", "--harness", "pi", "--model", "flash", "--effort", "low")
	h.dec.set(200, decReply(standardAnswers))
	if code, out := h.run("spawn", "t1"); code != 1 || !strings.Contains(out, "flash is for settled work only") {
		t.Fatalf("a weak model took standard work: %d %s", code, out)
	}
	wantContains(t, h.must("spawn", "t1", "--override-tier"), "spawned lead-t1")
	// A settled task may use the weak model.
	h.task("t2", "--harness", "pi", "--model", "flash", "--effort", "low")
	h.dec.set(200, decReply(settledAnswers))
	h.must("spawn", "t2")
	// A strong model is never blocked, whatever the tier.
	h.task("t3")
	h.dec.set(200, decReply(planningAnswers))
	h.must("spawn", "t3")
	// Enforce mode will not guess when the decision model is down.
	h.task("t4", "--harness", "pi", "--model", "flash", "--effort", "low")
	h.dec.set(500, `{"error":"down"}`)
	if code, out := h.run("spawn", "t4"); code != 1 || !strings.Contains(out, "triage is unavailable") {
		t.Fatalf("enforce spawned without a verdict: %d %s", code, out)
	}
	wantContains(t, h.must("spawn", "t4", "--override-tier"), "spawned lead-t4")
}

func TestTriageOffAndBadMode(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("triage_mode = \"off\"\n"), 0o644)
	h.task("t1")
	h.must("spawn", "t1")
	if h.dec.count() != 0 {
		t.Fatalf("triage ran although it is off: %d hits", h.dec.count())
	}
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("triage_mode = \"maybe\"\n"), 0o644)
	if code, out := h.run("status"); code != 1 || !strings.Contains(out, "triage_mode must be shadow, enforce or off") {
		t.Fatalf("bad mode accepted: %d %s", code, out)
	}
}

func TestTriageReportJoinsOutcomes(t *testing.T) {
	h := setup(t)
	if out := h.must("triage", "report"); !strings.Contains(out, "no records yet") {
		t.Fatalf("empty report = %q", out)
	}
	mk := func(id string, v decVals) {
		h.task(id)
		h.dec.set(200, decReply(v))
		h.must("triage", id)
	}
	mk("a", settledAnswers)
	mk("b", settledAnswers)
	mk("c", planningAnswers)
	mk("d", settledAnswers) // still open: no outcome
	h.must("report", "--dir", h.dir("a"), "done", "ok")
	h.must("report", "--dir", h.dir("b"), "failed", "could not")
	h.must("report", "--dir", h.dir("c"), "done", "ok")
	out := h.must("triage", "report")
	wantContains(t, out,
		"4 task(s) scored, 3 with an outcome, 1 still open",
		"too few outcomes (3 of 20)",
		"settled,2,1,1,50%",
		"planning,1,0,1,0%",
		"settled_but_hard[1]:", "b (failed; ran on claude sonnet)",
		"cautious_but_clean: 1 task(s)")
}

func TestTriageOutcome(t *testing.T) {
	orch := func(text string) entry { return entry{State: "note", Text: text, By: "orch"} }
	cases := []struct {
		name    string
		entries []entry
		want    string
	}{
		{"open", []entry{{State: "working", Text: "x"}}, ""},
		{"clean", []entry{{State: "working", Text: "x"}, {State: "done", Text: "y"}}, "clean"},
		{"done after an escalation is still hard", []entry{{State: "escalate", Text: "x"}, {State: "done", Text: "y"}}, "hard"},
		{"verify failed", []entry{{State: "done", Text: "y"}, orch("verify fail: check exit 1")}, "hard"},
		{"respawned", []entry{orch("closed for respawn on pi"), {State: "done", Text: "y"}}, "hard"},
		{"stalled", []entry{orch("stalled: no report"), {State: "done", Text: "y"}}, "hard"},
		{"failed", []entry{{State: "failed", Text: "z"}}, "hard"},
	}
	for _, c := range cases {
		if got, _ := triageOutcome(c.entries); got != c.want {
			t.Errorf("%s: outcome = %q, want %q", c.name, got, c.want)
		}
	}
}

const autoConfig = `max_leads = 8
[allowed_models]
claude = ["opus", "sonnet"]
pi = ["flash", "pro"]

[[dispatch]]
when = "planning"
tier = "planning"
use = [{ harness = "claude", model = "opus" }, { harness = "pi", model = "pro", degraded = true }]

[[dispatch]]
when = "settled"
tier = "settled"
use = [{ harness = "pi", model = "flash" }, { harness = "claude", model = "sonnet" }]

[[dispatch]]
when = "standard"
tier = "standard"
use = [{ harness = "claude", model = "sonnet" }, { harness = "pi", model = "pro", degraded = true }]
`

func TestAllowedModels(t *testing.T) {
	h := setup(t)
	wantContains(t, h.must("models"), "allowed_models: not set")
	h.must("new", "free", "--kind", "ship", "--harness", "claude", "--model", "anything-goes", "--effort", "high")

	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[allowed_models]\nclaude = [\"opus\", \"sonnet\"]\npi = [\"flash\"]\n"), 0o644)
	wantContains(t, h.must("models"), "claude (ok):", "  sonnet", "pi (ok):", "  flash")
	for _, c := range [][]string{
		{"claude", "haiku"}, {"claude", "default"}, {"pi", "pro"}, {"pi", "default"},
	} {
		code, out := h.run("new", "x", "--kind", "ship", "--harness", c[0], "--model", c[1], "--effort", "high")
		if code != 1 || !strings.Contains(out, "is not on the allowed list") || !strings.Contains(out, "allowed_models") {
			t.Errorf("%v accepted: %d %s", c, code, out)
		}
	}
	h.must("new", "ok1", "--kind", "ship", "--harness", "pi", "--model", "flash", "--effort", "low")

	// A recovery respawn cannot reach a model outside the list either.
	h.task("t1")
	h.must("spawn", "t1")
	if code, out := h.run("respawn", "t1", "--harness", "pi", "--model", "pro"); code != 1 || !strings.Contains(out, "not on the allowed list") {
		t.Fatalf("respawn onto a disallowed model: %d %s", code, out)
	}
	// So the gate model.
	h.commitSpec(threePkgSpec())
	if code, out := h.run("gate", "spec.md", "--runner", "pi:weird/model"); code != 1 || gateView(t, out).State != "incomplete" || !strings.Contains(gateView(t, out).PassErrors["all"], "not on the allowed list") {
		t.Fatalf("gate ran a disallowed model: %d %s", code, out)
	}

	// A dispatch profile outside the list is a config error, not a surprise later.
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[allowed_models]\nclaude = [\"sonnet\"]\n[[dispatch]]\nwhen = \"x\"\nuse = [{ harness = \"claude\", model = \"opus\" }]\n"), 0o644)
	if code, out := h.run("status"); code != 1 || !strings.Contains(out, "dispatch rule 1") || !strings.Contains(out, "not on the allowed list") {
		t.Fatalf("dispatch profile outside the list loaded: %d %s", code, out)
	}
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[allowed_models]\ngemini = [\"x\"]\n"), 0o644)
	if code, out := h.run("status"); code != 1 || !strings.Contains(out, "claude or pi") {
		t.Fatalf("unknown harness in allowed_models: %d %s", code, out)
	}
}

func TestSpawnAuto(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(autoConfig), 0o644)
	auto := []string{"--harness", "auto", "--model", "auto", "--effort", "auto"}
	if code, out := h.run("new", "bad", "--kind", "ship", "--harness", "auto", "--model", "sonnet", "--effort", "high"); code != 1 || !strings.Contains(out, "needs --model auto") {
		t.Fatalf("half-auto task accepted: %d %s", code, out)
	}

	h.task("a", auto...)
	if code, out := h.run("spawn", "a"); code != 1 || !strings.Contains(out, "run `orch spawn a --auto`") {
		t.Fatalf("an unresolved auto task spawned: %d %s", code, out)
	}
	h.dec.set(200, decReply(settledAnswers))
	out := h.must("spawn", "a", "--auto")
	wantContains(t, out, "auto: a is settled work", "would use: pi flash (effort low)", "dry run: nothing changed")
	fm, _, _ := readTask(filepath.Join(h.dir("a"), "task.md"))
	if fm["harness"] != "auto" || len(callsOf(h.fakeState(), "agent", "start")) != 0 {
		t.Fatalf("the dry run changed something: %v", fm)
	}
	wantContains(t, h.must("spawn", "a", "--auto", "--apply"), "using: pi flash (effort low)", "spawned lead-a (pi flash)")
	fm, _, _ = readTask(filepath.Join(h.dir("a"), "task.md"))
	if fm["harness"] != "pi" || fm["model"] != "flash" || fm["effort"] != "low" || fm["tier"] != "settled" {
		t.Fatalf("task after auto = %v", fm)
	}
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("a"), "status.log"))), "auto: settled -> pi flash")
	recs := readTriage(filepath.Dir(h.dir("a")))
	if got := recs[len(recs)-1].Actual; got != "pi flash" {
		t.Fatalf("triage records %q as the actual profile", got)
	}

	// Planning work with Claude limited parks; a degraded stand-in needs approval.
	h.task("b", auto...)
	h.dec.set(200, decReply(planningAnswers))
	h.must("health", "set", "claude", "limited")
	code, out := h.run("spawn", "b", "--auto", "--apply")
	if code != 1 {
		t.Fatalf("planning work was not parked: %d %s", code, out)
	}
	wantContains(t, out, "b is planning work", "claude opus: claude is limited", "parked", "degraded option: pi pro [degraded]")
	wantContains(t, h.must("spawn", "b", "--auto", "--apply", "--degraded"), "using: pi pro (effort high)")
	fm, _, _ = readTask(filepath.Join(h.dir("b"), "task.md"))
	if fm["degraded"] != "true" {
		t.Fatalf("a degraded pick was not marked: %v", fm)
	}
	h.must("health", "set", "claude", "ok")

	// Private work is never sent to the triage model, so --auto cannot rate it.
	h.task("c", auto...)
	setFrontMatter(filepath.Join(h.dir("c"), "task.md"), "private", "true")
	h.dec.set(200, decReply(settledAnswers))
	hits := h.dec.count()
	if code, out := h.run("spawn", "c", "--auto"); code != 1 || !strings.Contains(out, "c is private") {
		t.Fatalf("a private task was auto-routed: %d %s", code, out)
	}
	if h.dec.count() != hits {
		t.Fatal("a private task's text reached the triage model")
	}
	h.task("c2")
	setFrontMatter(filepath.Join(h.dir("c2"), "task.md"), "private", "true")
	setFrontMatter(filepath.Join(h.dir("c2"), "task.md"), "harness", "pi")
	setFrontMatter(filepath.Join(h.dir("c2"), "task.md"), "model", "flash")
	if code, out := h.run("spawn", "c2"); code != 1 || !strings.Contains(out, "marked private") {
		t.Fatalf("a private task ran on pi: %d %s", code, out)
	}

	// A hand-chosen profile is left alone.
	h.task("d")
	if code, out := h.run("spawn", "d", "--auto"); code != 1 || !strings.Contains(out, "hand-chosen profile") {
		t.Fatalf("auto overrode a hand-chosen profile: %d %s", code, out)
	}
	if code, out := h.run("spawn", "d", "--apply"); code != 1 || !strings.Contains(out, "go with --auto") {
		t.Fatalf("--apply without --auto: %d %s", code, out)
	}
}

const conditionalConfig = `max_leads = 8
[allowed_models]
claude = ["opus", "sonnet"]
pi = ["flash"]

[[allowed_when_limited]]
harness = "pi"
allow = { claude = ["haiku"] }

[[dispatch]]
when = "settled"
tier = "settled"
use = [{ harness = "pi", model = "flash" }, { harness = "claude", model = "haiku" }, { harness = "claude", model = "sonnet" }]
`

func TestHaikuComesInWhenPiIsDown(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(conditionalConfig), 0o644)
	wantContains(t, h.must("models"), "haiku (only while pi is limited: not allowed now)")

	// While pi is healthy Haiku is not an allowed lead model.
	code, out := h.run("new", "x", "--kind", "ship", "--harness", "claude", "--model", "haiku", "--effort", "low")
	if code != 1 || !strings.Contains(out, "allowed only while pi is limited, and it is not") {
		t.Fatalf("haiku allowed while pi is healthy: %d %s", code, out)
	}
	// auto skips it, takes Flash, and for private work falls to Sonnet, never Haiku.
	h.task("a", "--harness", "auto", "--model", "auto", "--effort", "auto")
	h.dec.set(200, decReply(settledAnswers))
	wantContains(t, h.must("spawn", "a", "--auto"), "would use: pi flash")
	h.task("p", "--harness", "auto", "--model", "auto", "--effort", "auto")
	setFrontMatter(filepath.Join(h.dir("p"), "task.md"), "private", "true")
	if code, out := h.run("spawn", "p", "--auto"); code != 1 || !strings.Contains(out, "p is private") {
		t.Fatalf("a private task was auto-routed: %d %s", code, out)
	}

	// With pi down Haiku is allowed and becomes the cheap stand-in.
	h.must("health", "set", "pi", "limited")
	wantContains(t, h.must("models"), "haiku (only while pi is limited: allowed now)")
	wantContains(t, h.must("dispatch"), "now: claude haiku")
	wantContains(t, h.must("spawn", "a", "--auto"), "triage is off", "pi:openrouter", "pass --tier settled|standard|planning")
	wantContains(t, h.must("spawn", "a", "--auto", "--tier", "settled"), "skipped pi flash: pi is limited", "would use: claude haiku (effort low)")
	h.must("new", "y", "--kind", "ship", "--harness", "claude", "--model", "haiku", "--effort", "low")
	// And it goes away again when pi recovers.
	h.must("health", "set", "pi", "ok")
	if code, _ := h.run("new", "z", "--kind", "ship", "--harness", "claude", "--model", "haiku", "--effort", "low"); code != 1 {
		t.Fatal("haiku stayed allowed after pi recovered")
	}

	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[[allowed_when_limited]]\nharness = \"gemini\"\nallow = { claude = [\"haiku\"] }\n"), 0o644)
	if code, out := h.run("status"); code != 1 || !strings.Contains(out, "allowed_when_limited entry 1") {
		t.Fatalf("bad conditional entry accepted: %d %s", code, out)
	}
}

func TestSpawnAutoNeedsATierRule(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[[dispatch]]\nwhen = \"x\"\ntier = \"settled\"\nuse = [{ harness = \"claude\", model = \"sonnet\" }]\n"), 0o644)
	h.task("a", "--harness", "auto", "--model", "auto", "--effort", "auto")
	h.dec.set(200, decReply(planningAnswers))
	if code, out := h.run("spawn", "a", "--auto"); code != 1 || !strings.Contains(out, "no dispatch rule has tier = \"planning\"") {
		t.Fatalf("missing tier rule: %d %s", code, out)
	}
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[[dispatch]]\nwhen = \"x\"\ntier = \"easy\"\nuse = [{ harness = \"claude\", model = \"sonnet\" }]\n"), 0o644)
	if code, out := h.run("status"); code != 1 || !strings.Contains(out, "tier must be settled, standard or planning") {
		t.Fatalf("bad tier accepted: %d %s", code, out)
	}
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("triage_mode = \"off\"\n"), 0o644)
	if code, out := h.run("spawn", "a", "--auto"); code != 0 || !strings.Contains(out, "triage is off (triage_mode off, pi:openrouter)") {
		t.Fatalf("auto with triage off: %d %s", code, out)
	}
}

func TestPlanPreflightsHealth(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	h.must("health", "set", "claude", "limited")
	code, out := h.run("plan", "spec.md", "--skip-gate")
	if code != 1 || !strings.Contains(out, "claude is limited") {
		t.Fatalf("plan ignored a limited harness: %d %s", code, out)
	}
	if _, err := os.Stat(h.dir("store")); err == nil {
		t.Fatal("plan created a task although it failed")
	}
}

func TestEscalateNeedsEvidence(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	if code, out := h.run("report", "--dir", d, "escalate", "stuck"); code != 1 || !strings.Contains(out, "needs --key") {
		t.Fatalf("keyless escalate accepted: %d %s", code, out)
	}
	if code, out := h.run("report", "--dir", d, "--key", "stuck", "escalate", "stuck"); code != 1 || !strings.Contains(out, "## Tried") {
		t.Fatalf("escalate without a card accepted: %d %s", code, out)
	}
	card := filepath.Join(d, "questions", "stuck.md")
	os.MkdirAll(filepath.Dir(card), 0o755)
	os.WriteFile(card, []byte("---\nquestion: Why does the migration deadlock?\n---\n## Context\nThe test hangs: `go test ./db` times out after 60s.\n\n## Blocked\nI cannot reproduce it outside the test.\n"), 0o644)
	if code, out := h.run("report", "--dir", d, "--key", "stuck", "escalate", "stuck"); code != 1 || !strings.Contains(out, "## Tried") {
		t.Fatalf("escalate card without Tried accepted: %d %s", code, out)
	}
	os.WriteFile(card, []byte("---\nquestion: Why does the migration deadlock?\n---\n## Context\nThe test hangs: `go test ./db` times out after 60s.\n\n## Tried\nRaised the timeout; ran it alone.\n\n## Blocked\nI cannot reproduce it outside the test.\n"), 0o644)
	h.must("report", "--dir", d, "--key", "stuck", "escalate", "migration test deadlocks")
	wantContains(t, h.must("status"), "open decisions: 1 (1 blocking, 0 queued)", "t1 [stuck] escalate: migration test deadlocks")
	out := h.must("ask")
	if strings.Contains(out, "warnings") {
		t.Fatalf("escalate card did not render: %s", out)
	}
	wantContains(t, out, "questions: 1")
}

func TestVerifyRunsCheckAndScope(t *testing.T) {
	h := setup(t)
	h.task("t1")
	d := h.dir("t1")
	path := filepath.Join(d, "task.md")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.Replace(b, []byte("\ndepends_on:"), []byte("\nowns: src/\ndepends_on:"), 1), 0o644)
	wt := filepath.Join(filepath.Dir(d), "worktrees", "t1")
	if out, err := exec.Command("git", "-C", h.repo, "worktree", "add", "-q", "-b", "orch/t1", wt, "main").CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v\n%s", err, out)
	}
	os.MkdirAll(filepath.Join(wt, "src"), 0o755)

	if code, out := h.run("verify", "t1"); code != 1 || !strings.Contains(out, "verify: unverified") {
		t.Fatalf("no check command must not pass: %d %s", code, out)
	}
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte("check = \"test -f src/ok\"\n"), 0o644)

	if code, out := h.run("verify", "t1"); code != 1 || !strings.Contains(out, "verify: fail") || !strings.Contains(out, "exit 1") {
		t.Fatalf("failing check passed: %d %s", code, out)
	}
	os.WriteFile(filepath.Join(wt, "src", "ok"), []byte("x\n"), 0o644)
	out := h.must("verify", "t1")
	wantContains(t, out, "verify: pass", "scope: all files inside")

	os.MkdirAll(filepath.Join(wt, "docs"), 0o755)
	os.WriteFile(filepath.Join(wt, "docs", "x.md"), []byte("x\n"), 0o644)
	if code, out := h.run("verify", "t1"); code != 1 || !strings.Contains(out, "verify: fail") || !strings.Contains(out, "outside_owns[1]:") || !strings.Contains(out, "docs/x.md") {
		t.Fatalf("file outside owns passed: %d %s", code, out)
	}
	wantContains(t, string(mustRead(t, filepath.Join(d, "status.log"))), "verify fail: check exit 0")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestQueuedQuestion(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	if code, out := h.run("report", "--dir", d, "question", "no key?"); code != 1 || !strings.Contains(out, "needs --key") {
		t.Fatalf("keyless question accepted: %d %s", code, out)
	}
	h.must("report", "--dir", d, "--key", "db", "needs-decision", "Postgres or MySQL?")
	wantContains(t, h.must("report", "--dir", d, "--key", "naming", "question", "snake or camel?"), "queued")
	wantContains(t, h.must("status"), "open decisions: 2 (1 blocking, 1 queued)", "t1 [naming] question: snake or camel?")
	var st struct {
		Leads []struct {
			State string `json:"state"`
		} `json:"leads"`
	}
	if err := json.Unmarshal([]byte(h.must("status", "--json")), &st); err != nil || len(st.Leads) != 1 || st.Leads[0].State != "needs-decision" {
		t.Fatalf("a queued question replaced the lead's own state: %v %+v", err, st)
	}
	h.must("send", "t1", "--resolve", "naming", "snake")
	if strings.Contains(h.must("status"), "[naming]") {
		t.Fatal("resolve did not close the queued question")
	}
}

func TestSendInboxAndDecisions(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	h.must("report", "--dir", d, "--key", "db", "needs-decision", "Postgres or MySQL?")
	h.must("report", "--dir", d, "--key", "port", "needs-decision", "which port?")
	wantContains(t, h.must("status"), "open decisions:", "t1 [db] needs-decision: Postgres or MySQL?", "t1 [port]")

	out := h.must("send", "t1", "--resolve", "db", "Use", "Postgres")
	wantContains(t, out, "sent message 001 to lead-t1")
	bells := callsOf(h.fakeState(), "agent", "prompt", "wt1:p1")
	if len(bells) != 2 || !strings.HasPrefix(bells[1][3], "orch: new message for you. Run: /skill/scripts/orch inbox --dir ") {
		t.Fatalf("doorbell = %v", bells)
	}
	status := h.must("status")
	if strings.Contains(status, "[db]") || !strings.Contains(status, "[port]") {
		t.Fatalf("resolve closed the wrong decision:\n%s", status)
	}

	out = h.must("inbox", "--dir", d)
	wantContains(t, out, "--- message 001 ---", "from: orchestrator", "re: db", "Use Postgres")
	if out := h.must("inbox", "--dir", d); out != "no new messages\n" {
		t.Fatalf("message not acknowledged: %q", out)
	}

	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Status = "blocked" })
	h.stdin.WriteString("line one\nline two\n")
	wantContains(t, h.must("send", "t1", "-"), "stored message 002", "no doorbell")
	msg, _ := os.ReadFile(filepath.Join(d, "inbox", "002.msg"))
	wantContains(t, string(msg), "line one\nline two\n")

	// The orchestrator's own lines never count as the lead's state.
	if got := lastState(leadEntries(readStatus(d))); got != "needs-decision" {
		t.Fatalf("lead state = %q", got)
	}
}

func TestWatchHerdrEvents(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("working lead produced %q", out)
	}
	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Status = "blocked" })
	wantContains(t, h.must("watch", "--once"), "orch: t1 ui-blocked")
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("ui-blocked repeated: %q", out)
	}

	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Status = "idle" })
	h.must("watch", "--once")
	h.now = h.now.Add(31 * time.Minute)
	wantContains(t, h.must("watch", "--once"), "orch: t1 quiet: idle 31m without a report")
	h.now = h.now.Add(time.Hour)
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("quiet repeated: %q", out)
	}

	h.editFake(func(st *fakeState) { st.Down = true })
	wantContains(t, h.must("watch", "--once"), "herdr unreachable")
	h.editFake(func(st *fakeState) { st.Down = false; delete(st.Agents, "wt1:p1") })
	wantContains(t, h.must("watch", "--once"), "orch: t1 gone: agent exited (pane wt1:p1)")
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("gone repeated: %q", out)
	}
	wantContains(t, h.must("status"), "gone")
}

func TestWatchQuietSkipsWaitingLeads(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	h.must("report", "--dir", h.dir("t1"), "paused", "waiting for the dependency")
	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Status = "idle" })
	h.must("watch", "--once")
	h.now = h.now.Add(2 * time.Hour)
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("paused lead raised %q", out)
	}
}

func TestReportContextKeyNeedsDecision(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	code, out := h.run("report", "--dir", h.dir("t1"), "--key", "context", "paused", "handoff written")
	if code == 0 || !strings.Contains(out, "needs-decision") {
		t.Fatalf("paused --key context: %d %q", code, out)
	}
	wantContains(t, h.must("report", "--dir", h.dir("t1"), "--key", "context", "needs-decision", "full"), "reported needs-decision")
	wantContains(t, h.must("report", "--dir", h.dir("t1"), "paused", "waiting"), "not woken by paused")
}

func TestWatchCheckpointTimeout(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	// Advance the injected clock instead of waiting a wall-clock second.
	h.a.now = func() time.Time {
		now := h.now
		h.now = h.now.Add(250 * time.Millisecond)
		return now
	}
	code, out := h.run("watch", "--timeout", "1", "--interval", "0")
	if code != 124 || !strings.Contains(out, "checkpoint") {
		t.Fatalf("checkpoint = %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(h.tmp, "state", "repo", "watch-w1-p0.pid")); !os.IsNotExist(err) {
		t.Fatal("watch.pid left behind")
	}
	h.must("report", "--dir", h.dir("t1"), "failed", "tests do not build")
	code, out = h.run("watch", "--timeout", "5", "--interval", "0.05")
	if code != 0 || !strings.Contains(out, "orch: t1 failed: tests do not build") {
		t.Fatalf("checkpoint with an event = %d %q", code, out)
	}
}

// Two orchestrators in one repo each watch and list only the leads they
// spawned, while max_leads and locks stay shared.
func TestTwoOrchestratorsOneRepo(t *testing.T) {
	h := setup(t)
	h.task("a1")
	h.task("b1")
	h.must("spawn", "a1")
	t.Setenv("HERDR_PANE_ID", "w2:p0")
	h.must("spawn", "b1")
	if h.lead("a1").Owner != "w1:p0" || h.lead("b1").Owner != "w2:p0" {
		t.Fatalf("owners = %q, %q", h.lead("a1").Owner, h.lead("b1").Owner)
	}
	h.must("report", "--dir", h.dir("a1"), "done", "a finished")
	h.must("report", "--dir", h.dir("b1"), "done", "b finished")

	if out := h.must("watch", "--once"); out != "orch: b1 done: b finished\n" {
		t.Fatalf("second orchestrator's watcher printed %q", out)
	}
	if _, err := os.Stat(filepath.Join(h.tmp, "state", "repo", "watch-w2-p0.pid")); !os.IsNotExist(err) {
		t.Fatal("watch pid file left behind")
	}
	status := h.must("status")
	wantContains(t, status, "b1", "leads open: 1 yours, 2/3 total", "other orchestrators: w1:p0 has 1 open (a1)")
	if strings.Contains(status, "a finished") {
		t.Fatalf("board lists another orchestrator's lead:\n%s", status)
	}
	// A live owner pane has a watcher; a gone one leaves its leads unwatched.
	h.editFake(func(st *fakeState) { st.Panes["w1:p0"] = "w1:t0" })
	if out := h.must("status"); strings.Contains(out, "pane gone") {
		t.Fatalf("live owner flagged:\n%s", out)
	}
	h.editFake(func(st *fakeState) { delete(st.Panes, "w1:p0") })
	wantContains(t, h.must("status"), "other orchestrators: w1:p0 has 1 open (a1) - pane gone", "orch adopt")

	t.Setenv("HERDR_PANE_ID", "w1:p0")
	if out := h.must("watch", "--once"); out != "orch: a1 done: a finished\n" {
		t.Fatalf("first orchestrator's watcher printed %q", out)
	}

	// Taking over a lead keeps the watcher's place, so old events do not repeat.
	wantContains(t, h.must("adopt", "b1"), "b1 now belongs to w1:p0")
	if out := h.must("watch", "--once"); out != "" {
		t.Fatalf("adopted lead replayed old events: %q", out)
	}
	h.must("report", "--dir", h.dir("b1"), "failed", "b broke")
	if out := h.must("watch", "--once"); out != "orch: b1 failed: b broke\n" {
		t.Fatalf("adopted lead event = %q", out)
	}
}

// Leads spawned before owners existed stay visible to every orchestrator,
// and the board says how to claim them.
func TestUnownedLeadsAreShared(t *testing.T) {
	h := setup(t)
	h.task("old")
	h.must("spawn", "old")
	l := h.lead("old")
	l.Owner = ""
	saveLead(h.dir("old"), l)
	wantContains(t, h.must("status"), "old", "unowned: old", "orch adopt <id>")
	h.must("report", "--dir", h.dir("old"), "done", "legacy")
	wantContains(t, h.must("watch", "--once"), "orch: old done: legacy")
}

// A watcher from before per-orchestrator watchers keeps counting until it
// exits, so the upgrade neither nags nor starts a duplicate.
func TestLegacyWatcherCountsDuringUpgrade(t *testing.T) {
	h := setup(t)
	pidFile := filepath.Join(h.tmp, "empty-watch.pid")
	for _, contents := range []string{"", " \t\n"} {
		writeFiles(t, h.tmp, map[string]string{filepath.Base(pidFile): contents})
		if pid := pidRunsWatch(pidFile); pid != 0 {
			t.Fatalf("empty watcher pid %q reported running process %d", contents, pid)
		}
	}
	h.task("t1")
	h.must("spawn", "t1")
	old := exec.Command("bash", "-c", `exec -a "orch watch" sleep 30`)
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { old.Process.Kill(); old.Wait() })
	time.Sleep(200 * time.Millisecond)
	os.WriteFile(filepath.Join(h.tmp, "state", "repo", "watch.pid"), fmt.Appendf(nil, "%d 0\n", old.Process.Pid), 0o644)
	wantContains(t, h.must("status"), "watcher: running")
	code, out := h.run("watch", "--once")
	if code != 1 || !strings.Contains(out, "already running") {
		t.Fatalf("watch beside a legacy watcher = %d %q", code, out)
	}
	old.Process.Kill()
	old.Wait()
	wantContains(t, h.must("status"), "watcher: stopped")
}

func TestCloseNeedsDoneOrForce(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	code, out := h.run("close", "t1")
	if code != 1 || !strings.Contains(out, "last reported nothing") {
		t.Fatalf("close of a working lead = %d %q", code, out)
	}
	wantContains(t, h.must("close", "t1", "--force"), "closed lead-t1")
	if l := h.lead("t1"); l.ClosedAt == 0 {
		t.Fatal("closed_at not recorded")
	}
	wantContains(t, h.must("status", "--all"), "closed")
	if code, _ := h.run("peek", "t1"); code != 1 {
		t.Fatal("peek of a closed lead should fail")
	}
}

// transcript writes a session transcript where contextTokens looks for it.
func (h *harness) transcript(harness, session string, lines ...string) {
	h.t.Helper()
	path := filepath.Join(h.tmp, "claude", "projects", "-repo", session+".jsonl")
	if harness == "pi" {
		path = filepath.Join(h.tmp, "pi", "sessions", "--repo--", "2026-10-14T10-00-00-000Z_"+session+".jsonl")
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func claudeTurn(input, read, create int, sidechain bool) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":%t,"message":{"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":9}}}`, sidechain, input, read, create)
}

func TestContextTokens(t *testing.T) {
	h := setup(t)
	h.transcript("claude", "s1",
		claudeTurn(10, 1000, 0, false),
		claudeTurn(5, 180_000, 2_000, false),
		claudeTurn(1, 3_000, 0, true), // a subagent's turn is not the lead's context
		`{"type":"user","message":{"content":"the word usage alone"}}`,
	)
	if got := contextTokens("claude", "s1"); got != 182_005 {
		t.Fatalf("claude context = %d, want 182005", got)
	}
	h.transcript("claude", "s3",
		claudeTurn(1, 150_000, 0, false),
		`{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"manual","preTokens":150001,"postTokens":7379}}`,
	)
	if got := contextTokens("claude", "s3"); got != 7379 {
		t.Fatalf("context after compaction = %d, want 7379", got)
	}
	h.transcript("pi", "p1",
		`{"type":"message","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","message":{"role":"assistant","usage":{"input":100,"output":50,"cacheRead":17000,"cacheWrite":900,"totalTokens":18050}}}`,
		`{"type":"message","message":{"role":"assistant","usage":{"input":120,"output":144,"cacheRead":20000,"cacheWrite":0,"totalTokens":20264}}}`,
	)
	if got := contextTokens("pi", "p1"); got != 20_120 {
		t.Fatalf("pi context = %d, want 20120 (input + cache, not output)", got)
	}
	for _, c := range [][2]string{{"claude", "missing"}, {"claude", ""}, {"claude", "*"}, {"other", "s1"}} {
		if got := contextTokens(c[0], c[1]); got != 0 {
			t.Errorf("contextTokens(%q, %q) = %d, want 0", c[0], c[1], got)
		}
	}
}

func TestContextLimitWarnsLeadAndBoard(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("lead_context_limit = 100000\n"), 0o644)
	h.task("t1")
	h.must("spawn", "t1")
	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Session = "s1" })
	h.transcript("claude", "s1", claudeTurn(1, 90_000, 0, false))
	d := h.dir("t1")

	h.must("send", "t1", "next: fix the footer")
	if out := h.must("inbox", "--dir", d); strings.Contains(out, "context:") {
		t.Fatalf("warned under the limit:\n%s", out)
	}
	wantContains(t, h.must("status"), "CTX", " 90K ")

	h.transcript("claude", "s1", claudeTurn(1, 160_000, 0, false))
	h.must("send", "t1", "next: an unrelated page")
	wantContains(t, h.must("inbox", "--dir", d), "an unrelated page", "context: 160K tokens, over the 100K lead limit", "Context budget")
	wantContains(t, h.must("status"), "160K!")
	if out := h.must("inbox", "--dir", d); out != "no new messages\n" {
		t.Fatalf("warned with no new work: %q", out)
	}

	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("lead_context_limit = 0\n"), 0o644)
	if code, out := h.run("status"); code != 1 || !strings.Contains(out, "lead_context_limit") {
		t.Fatalf("zero limit accepted: %d %q", code, out)
	}
}

func TestCompact(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	h.must("report", "--dir", d, "--key", "context", "needs-decision", "180K and the next work is unrelated: compact or fresh lead?")

	if code, out := h.run("compact", "t1"); code != 1 || !strings.Contains(out, "is working") {
		t.Fatalf("compact mid-turn = %d %q", code, out)
	}
	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Status = "idle" })
	wantContains(t, h.must("compact", "t1", "keep", "the", "footer", "work"), "compacted lead-t1")
	prompts := callsOf(h.fakeState(), "agent", "prompt", "wt1:p1")
	if len(prompts) != 3 {
		t.Fatalf("prompts = %v", prompts)
	}
	if c := prompts[1]; c[3] != "/compact keep the footer work" || !slices.Contains(c, "--wait") {
		t.Fatalf("compact prompt = %v", c)
	}
	wantContains(t, prompts[2][3], "your context was compacted", filepath.Join(d, "task.md"), filepath.Join(d, "handoff.md"))
	if strings.Contains(h.must("status"), "[context]") {
		t.Fatal("context decision still open after compact")
	}

	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Status = "idle" })
	h.must("compact", "t1")
	if c := callsOf(h.fakeState(), "agent", "prompt", "wt1:p1"); c[3][3] != "/compact "+compactFocus {
		t.Fatalf("default focus = %q", c[3][3])
	}
}

func TestRespawn(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	h.must("report", "--dir", d, "--key", "context", "needs-decision", "full; fresh lead?")
	if code, out := h.run("respawn", "t1"); code != 1 || !strings.Contains(out, "no handoff.md") {
		t.Fatalf("respawn without a handoff = %d %q", code, out)
	}
	os.WriteFile(filepath.Join(d, "handoff.md"), []byte("branch orch/t1; next: the settings page\n"), 0o644)

	wantContains(t, h.must("respawn", "t1"), "spawned lead-t1")
	if _, ok := h.fakeState().Panes["wt1:p1"]; ok {
		t.Fatal("old pane still open")
	}
	l := h.lead("t1")
	if l.PaneID == "wt1:p1" || l.ClosedAt != 0 {
		t.Fatalf("new lead = %+v", l)
	}
	kept := filepath.Join(d, fmt.Sprintf("handoff-%d.md", h.now.Unix()))
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("handoff not kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d, "handoff.md")); err == nil {
		t.Fatal("handoff.md left in place for the next respawn")
	}
	launch := callsOf(h.fakeState(), "agent", "prompt", l.PaneID)
	if len(launch) != 1 {
		t.Fatalf("launch = %v", launch)
	}
	wantContains(t, launch[0][3], filepath.Join(d, "task.md"), "read its handoff at "+kept, "worktree and branch already exist")
	status := h.must("status")
	if strings.Contains(status, "[context]") {
		t.Fatalf("context decision still open:\n%s", status)
	}
	wantContains(t, status, "leads open: 1/3")
	code, out := h.run("respawn", "t1")
	if code != 1 || !strings.Contains(out, "no handoff.md") {
		t.Fatalf("second respawn reused the old handoff: %d %q", code, out)
	}
}

func TestPeekReadsEnoughLines(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	out := h.must("peek", "t1", "--lines", "3")
	if out != "line 248\nline 249\nline 250\n" {
		t.Fatalf("peek = %q", out)
	}
	read := callsOf(h.fakeState(), "agent", "read")[0]
	if flagValue(read, "--lines") != "200" {
		t.Fatalf("peek asked Herdr for %v", read)
	}
}

func TestOrchestratorHooks(t *testing.T) {
	h := setup(t)
	file := filepath.Join(h.repo, "main.go")
	edit := map[string]any{"session_id": "s1", "cwd": h.repo, "tool_name": "Edit", "tool_input": map[string]any{"file_path": file}}
	if code, _ := h.hook("pretool", edit); code != 0 {
		t.Fatal("hook acted without the orchestrator role")
	}

	h.editFake(func(st *fakeState) { st.Session = "s1" })
	wantContains(t, h.must("become"), "orchestrator role on for pane w1:p0 (session s1)")

	code, out := h.hook("pretool", edit)
	if code != 2 || !strings.Contains(out, "Project changes belong to a lead") {
		t.Fatalf("edit in repo = %d %q", code, out)
	}
	edit["session_id"] = "s2"
	if code, _ := h.hook("pretool", edit); code != 0 {
		t.Fatal("marker applied to another session in the same pane")
	}

	cases := []struct {
		tool  string
		input map[string]any
		deny  bool
	}{
		{"Write", map[string]any{"file_path": filepath.Join(h.tmp, "notes.md")}, false},
		{"Agent", map[string]any{"subagent_type": "context-explorer"}, true},
		{"Agent", map[string]any{"subagent_type": "Explore"}, false},
		{"Agent", map[string]any{"subagent_type": "implementer"}, true},
		{"Agent", map[string]any{}, true},
		{"Workflow", map[string]any{}, true},
		{"Bash", map[string]any{"command": "git status && git log -3"}, false},
		{"Bash", map[string]any{"command": "cd x && git commit -m wip"}, true},
		{"Bash", map[string]any{"command": "git -C ../wt merge feat"}, true},
		{"Bash", map[string]any{"command": "gh pr merge 12 --squash"}, true},
		{"Bash", map[string]any{"command": "gh pr view 12"}, false},
		{"Read", map[string]any{"file_path": file}, false},
	}
	for _, c := range cases {
		code, out := h.hook("pretool", map[string]any{"session_id": "s1", "cwd": h.repo, "tool_name": c.tool, "tool_input": c.input})
		if (code == 2) != c.deny {
			t.Errorf("%s %v: exit %d %q, deny=%v", c.tool, c.input, code, out, c.deny)
		}
	}

	// A run root inside a repository stays writable for briefs.
	t.Setenv("ORCH_HOME", filepath.Join(h.repo, ".state"))
	brief := filepath.Join(h.repo, ".state", "repo", "t1", "task.md")
	if code, out := h.hook("pretool", map[string]any{"session_id": "s1", "cwd": h.repo, "tool_name": "Write", "tool_input": map[string]any{"file_path": brief}}); code != 0 {
		t.Fatalf("brief write denied: %q", out)
	}
	t.Setenv("ORCH_HOME", filepath.Join(h.tmp, "state"))

	stop := map[string]any{"session_id": "s1", "cwd": h.repo}
	if code, _ := h.hook("stop", stop); code != 0 {
		t.Fatal("stop blocked with no leads")
	}
	h.task("t1")
	h.must("spawn", "t1")
	code, out = h.hook("stop", stop)
	if code != 2 || !strings.Contains(out, "no watcher is running") {
		t.Fatalf("stop with a lead and no watcher = %d %q", code, out)
	}
	stop["stop_hook_active"] = true
	if code, _ := h.hook("stop", stop); code != 0 {
		t.Fatal("stop hook looped")
	}

	code, out = h.hook("session-start", map[string]any{"session_id": "s1", "cwd": h.repo})
	if code != 0 {
		t.Fatalf("session-start exited %d", code)
	}
	wantContains(t, out, "load the `orchestrate` skill", "t1", "Watcher stopped")

	edit["session_id"] = "s1"
	if code, _ := h.hook("pretool", edit); code != 2 {
		t.Fatal("original session lost the orchestrator role before become --off")
	}
	h.must("become", "--off")
	if code, _ := h.hook("pretool", edit); code != 0 {
		t.Fatal("role still on after become --off")
	}
}

func TestLeadStopGuard(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	t.Setenv("ORCH_ROLE", "lead")
	t.Setenv("ORCH_TASK_DIR", d)
	stop := func(active bool) int {
		code, _ := h.hook("stop", map[string]any{"session_id": "x", "stop_hook_active": active})
		return code
	}
	if stop(false) != 2 {
		t.Fatal("silent first turn not sent back")
	}
	if stop(true) != 0 {
		t.Fatal("guard looped")
	}
	h.must("report", "working", "waiting on two implementers")
	if stop(false) != 0 {
		t.Fatal("turn with a report sent back")
	}
	if stop(false) != 2 {
		t.Fatal("silent working turn not sent back")
	}
	h.must("report", "--key", "db", "needs-decision", "which db?")
	stop(false)
	if stop(false) != 0 {
		t.Fatal("lead waiting on the orchestrator sent back")
	}
	t.Setenv("ORCH_ROLE", "")
	h.must("send", "t1", "--resolve", "db", "postgres")
	t.Setenv("ORCH_ROLE", "lead")
	code, out := h.hook("stop", map[string]any{"session_id": "x"})
	if code != 2 || !strings.Contains(out, "1 unread message(s)") {
		t.Fatalf("answered lead stopping silently = %d %q", code, out)
	}

	code, out = h.hook("session-start", map[string]any{"session_id": "x"})
	if code != 0 {
		t.Fatalf("lead session-start = %d", code)
	}
	wantContains(t, out, `You are orchestrate lead "t1"`, "task.md", "1 unread message(s)")
}

func TestRunRootFollowsHerdrConfig(t *testing.T) {
	h := setup(t)
	t.Setenv("ORCH_HOME", "")
	cfg := filepath.Join(h.tmp, "herdr.toml")
	os.WriteFile(cfg, []byte("[ui]\ntheme = \"x\"\n\n[worktrees]\ndirectory = \""+filepath.Join(h.tmp, "wt")+"\"\n"), 0o644)
	t.Setenv("ORCH_HERDR_CONFIG", cfg)
	if got, want := runRoot(h.repo), filepath.Join(h.tmp, "wt", "repo", ".orchestrate"); got != want {
		t.Fatalf("runRoot = %s, want %s", got, want)
	}
	t.Setenv("ORCH_HERDR_CONFIG", filepath.Join(h.tmp, "missing.toml"))
	if got := runRoot(h.repo); !strings.HasSuffix(got, filepath.Join(".local", "state", "orchestrate", "repo")) {
		t.Fatalf("fallback runRoot = %s", got)
	}
	// From a linked worktree the run root is still the main checkout's.
	wt := filepath.Join(h.tmp, "linked")
	if out, err := exec.Command("git", "-C", h.repo, "worktree", "add", "-q", wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}
	if got, _ := repoRoot(wt); got != h.repo {
		t.Fatalf("repoRoot from worktree = %s", got)
	}
	t.Setenv("ORCH_HERDR_CONFIG", cfg)
	h.a.cwd = wt
	if got, want := h.a.siteRoot(t), runRoot(h.repo); got != want {
		t.Fatalf("run root from worktree = %s, want %s", got, want)
	}
}

// Two repos with the same folder name never share run state. Locks stay
// machine-wide on purpose (see locks.go).
func TestSameNameReposStayApart(t *testing.T) {
	h := setup(t)
	other := filepath.Join(h.tmp, "elsewhere", "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", other}, {"-C", other, "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("max_leads = 5\n"), 0o644)
	h.task("t1")
	h.must("spawn", "t1")
	first := runRoot(h.repo)

	h.a.cwd = other
	second := runRoot(other)
	if second == first {
		t.Fatalf("both repos use run root %s", second)
	}
	if out := h.must("status"); !strings.Contains(out, "no tasks yet") {
		t.Fatalf("second repo sees the first repo's task:\n%s", out)
	}
	h.task("t1")
	if got := runRoot(other); got != second || !strings.HasPrefix(filepath.Base(got), "repo-") {
		t.Fatalf("second repo's root is not a stable hashed name: %s", got)
	}
	if _, err := os.Stat(filepath.Join(runRoot(other), "t1", "task.md")); err != nil {
		t.Fatalf("second repo's task not in its own root: %v", err)
	}
	if runRoot(h.repo) != first {
		t.Fatal("first repo's run root moved")
	}

}

func TestConfigRejectsUnknownKeys(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte("max_lead = 2\n"), 0o644)
	code, out := h.run("status")
	if code != 1 || !strings.Contains(out, `unknown key "max_lead"`) {
		t.Fatalf("typo accepted: %d %q", code, out)
	}
}

// A repository must not choose how leads launch or how many run here.
func TestRepoConfigCannotSetUserKeys(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	repoFile := filepath.Join(h.repo, ".agents", "orchestrate.toml")
	for _, line := range []string{
		`claude_args = ["--dangerously-skip-permissions"]`,
		`pi_args = ["--approve"]`,
		`allowed_models = { claude = ["anything"] }`,
		"max_leads = 50",
		`CLAUDE_ARGS = ["--dangerously-skip-permissions"]`,
		`Pi_Args = ["--approve"]`,
	} {
		os.WriteFile(repoFile, []byte(line+"\n"), 0o644)
		code, out := h.run("status")
		if code != 1 || !strings.Contains(out, "is not allowed here") || !strings.Contains(out, os.Getenv("ORCH_CONFIG")) {
			t.Fatalf("repo set %s: %d %q", line, code, out)
		}
	}
	os.Remove(repoFile)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("check = \"make test\"\n"), 0o644)
	if code, out := h.run("status"); code != 1 || !strings.Contains(out, `"check" is not allowed here`) {
		t.Fatalf("user config set a repo key: %d %q", code, out)
	}
}

// Notes reach the orchestrator on every board, so their size is capped.
func TestRepoNotes(t *testing.T) {
	h := setup(t)
	wantContains(t, h.must("status"), "repo config: none")
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	repoFile := filepath.Join(h.repo, ".agents", "orchestrate.toml")
	os.WriteFile(repoFile, []byte(`notes = ["one test VM: run-e2e one lead at a time"]`+"\n"), 0o644)
	out := h.must("status")
	wantContains(t, out, "note: one test VM: run-e2e one lead at a time")
	if strings.Contains(out, "repo config: none") {
		t.Fatalf("config file present but board says none:\n%s", out)
	}
	for name, notes := range map[string]string{
		"too many":  `["a", "b", "c", "d", "e", "f"]`,
		"too long":  `["` + strings.Repeat("x", maxNoteLen+1) + `"]`,
		"multiline": `["a\nb"]`,
		"blank":     `["  "]`,
	} {
		os.WriteFile(repoFile, []byte("notes = "+notes+"\n"), 0o644)
		if code, out := h.run("status"); code != 1 || !strings.Contains(out, "note") {
			t.Errorf("%s accepted: %d %q", name, code, out)
		}
		if code, _ := h.run("config-check", repoFile); code != 1 {
			t.Errorf("config-check accepted %s", name)
		}
	}
	// A draft elsewhere (a setup lead's worktree) is checked on its own.
	draft := filepath.Join(h.tmp, "draft.toml")
	os.WriteFile(draft, []byte(`check = "make test"`+"\nmax_leads = 9\n"), 0o644)
	if code, out := h.run("config-check", draft); code != 1 || !strings.Contains(out, "is not allowed here") {
		t.Errorf("config-check accepted a user key: %d %q", code, out)
	}
	os.WriteFile(draft, []byte(`notes = ["one VM"]`+"\n"), 0o644)
	wantContains(t, h.must("config-check", draft), "ok:", "1 of 5 notes")
}

// A lock gives one lead a shared resource across many steps, and frees when
// that lead closes or its pane goes away.
func TestLocks(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"),
		[]byte(`locks = { e2e-vm = "from deploy until you have read the results" }`+"\n"), 0o644)
	for _, id := range []string{"t1", "t2", "t3"} {
		h.task(id)
	}
	brief, _ := os.ReadFile(filepath.Join(h.dir("t1"), "task.md"))
	wantContains(t, string(brief), "Lock `e2e-vm`: hold it from deploy until you have read the results",
		"lock acquire e2e-vm --dir "+h.dir("t1")+" --wait 3600")
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("max_leads = 5\n"), 0o644)
	for _, id := range []string{"t1", "t2", "t3"} {
		h.must("spawn", id)
	}

	wantContains(t, h.must("lock", "acquire", "e2e-vm", "--dir", h.dir("t1")), "acquired e2e-vm")
	h.must("lock", "acquire", "e2e-vm", "--dir", h.dir("t1")) // a holder may take it again
	code, out := h.run("lock", "acquire", "e2e-vm", "--dir", h.dir("t2"))
	if code != 75 || !strings.Contains(out, "held by t1") {
		t.Fatalf("second holder: %d %q", code, out)
	}
	if code, out := h.run("lock", "release", "e2e-vm", "--dir", h.dir("t2")); code != 1 || !strings.Contains(out, "held by t1") {
		t.Fatalf("non-holder released: %d %q", code, out)
	}
	wantContains(t, h.must("status"), "lock: e2e-vm held by t1")

	// A waiter reports paused, so the board shows why it is idle.
	// One failed attempt covers this outcome; retry timing is not under test.
	t.Setenv("ORCH_LOCK_POLL_MS", "1000")
	code, _ = h.run("lock", "acquire", "e2e-vm", "--dir", h.dir("t2"), "--wait", "1")
	if code != 75 || !strings.Contains(lastLine(h.dir("t2")), "paused") || !strings.Contains(lastLine(h.dir("t2")), "waiting for lock e2e-vm") {
		t.Fatalf("waiter: %d, status %q", code, lastLine(h.dir("t2")))
	}

	// Closing the holder frees the lock.
	h.must("report", "--dir", h.dir("t1"), "done", "ran e2e")
	h.must("close", "t1")
	if !strings.Contains(lastLine(h.dir("t1")), "released e2e-vm") {
		t.Fatalf("close note: %q", lastLine(h.dir("t1")))
	}
	h.must("lock", "acquire", "e2e-vm", "--dir", h.dir("t2"))

	// A transport failure cannot prove that the holder's pane is gone.
	h.editFake(func(st *fakeState) { st.Down = true })
	if code, out := h.run("lock", "acquire", "e2e-vm", "--dir", h.dir("t3")); code != 75 || !strings.Contains(out, "held by t2") {
		t.Fatalf("unreachable Herdr freed a live lock: %d %q", code, out)
	}
	if held, err := readLock("e2e-vm"); err != nil || held == nil || held.Dir != h.dir("t2") {
		t.Fatalf("unreachable Herdr changed the holder: %+v, %v", held, err)
	}
	h.editFake(func(st *fakeState) { st.Down = false })

	// A holder whose pane died is stale: the next acquire takes over.
	h.editFake(func(st *fakeState) { delete(st.Panes, h.lead("t2").PaneID) })
	wantContains(t, h.must("lock", "list"), "held by t2", "stale")
	h.must("lock", "acquire", "e2e-vm", "--dir", h.dir("t3"))
	wantContains(t, h.must("lock", "release", "e2e-vm", "--dir", h.dir("t3")), "released e2e-vm")
	wantContains(t, h.must("lock", "list"), "no locks held")

	// The orchestrator can force a stuck lock free (with the user's agreement).
	h.must("lock", "acquire", "e2e-vm", "--dir", h.dir("t3"))
	wantContains(t, h.must("lock", "release", "e2e-vm", "--force"), "released e2e-vm (forced)")

	for _, bad := range []string{`locks = { "E2E VM" = "x" }`, `locks = { vm = "" }`} {
		os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte(bad+"\n"), 0o644)
		if code, _ := h.run("status"); code != 1 {
			t.Errorf("config accepted %s", bad)
		}
	}
}

// Racing takers get exactly one winner.
func TestLockRace(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ORCH_LOCKS_DIR", filepath.Join(tmp, "locks"))
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	start := make(chan struct{})
	for i := range 12 {
		dir := filepath.Join(tmp, fmt.Sprintf("task%d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, dir, map[string]string{"task.md": "x"})
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			held, err := tryLock(lockHolder{Name: "vm", Lead: filepath.Base(dir), Dir: dir})
			if err != nil {
				t.Error(err)
				return
			}
			if held == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d leads took the lock", winners)
	}
}

// Dispatch rules live in the user config only and are printed, not matched.
func TestDispatchRules(t *testing.T) {
	h := setup(t)
	wantContains(t, h.must("dispatch"), "no dispatch rules")
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(`
[[dispatch]]
when = "mechanical edits a test can verify"
use = [{ harness = "claude", model = "haiku", effort = "low" }]
why = "cheap and fast"

[[dispatch]]
when = "large or ambiguous multi-file work"
use = [{ harness = "claude", model = "opus", effort = "high" }, { harness = "pi", model = "openrouter/deepseek/deepseek-chat-v3.1", effort = "high" }]
`), 0o644)
	wantContains(t, h.must("dispatch"),
		"- mechanical edits a test can verify -> claude haiku low (cheap and fast)",
		"- large or ambiguous multi-file work -> claude opus high | pi openrouter/deepseek/deepseek-chat-v3.1 high")
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[[dispatch]]\nwhen = \"x\"\nuse = [{ harness = \"gemini\", model = \"m\" }]\n"), 0o644)
	if code, _ := h.run("dispatch"); code != 1 {
		t.Fatal("unknown harness accepted")
	}
	os.Remove(os.Getenv("ORCH_CONFIG"))
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte("[[dispatch]]\nwhen = \"x\"\n"), 0o644)
	if code, out := h.run("dispatch"); code != 1 || !strings.Contains(out, "is not allowed here") {
		t.Fatalf("repo set dispatch: %d %q", code, out)
	}
}

func lastLine(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "status.log"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

func TestParseArgsInterspersed(t *testing.T) {
	a := &app{errw: &bytes.Buffer{}}
	fs := a.flags("send")
	resolve := fs.String("resolve", "", "")
	pos, err := parseArgs(fs, []string{"t1", "--resolve", "k", "hello", "-", "--", "--world"})
	if err != nil || *resolve != "k" || !slices.Equal(pos, []string{"t1", "hello", "-", "--world"}) {
		t.Fatalf("pos=%v resolve=%q err=%v", pos, *resolve, err)
	}
}

func TestOpenDecisions(t *testing.T) {
	entries := []entry{
		{State: "needs-decision", Key: "a", Text: "1"},
		{State: "blocked", Text: "keyless"},
		{State: "needs-decision", Key: "b", Text: "2"},
		{State: "resolved", Key: "a", By: "orch"},
		{State: "working", By: "orch"},
	}
	got := openDecisions(entries)
	if len(got) != 2 || got[0].Key != "b" || got[1].Text != "keyless" {
		t.Fatalf("open = %+v", got)
	}
	got = openDecisions(append(entries, entry{State: "working", Text: "moving on"}))
	if len(got) != 1 || got[0].Key != "b" {
		t.Fatalf("lead moving on should close only keyless decisions: %+v", got)
	}
}

func TestStatusReopenedDecisionAppearsOnce(t *testing.T) {
	h := setup(t)
	h.task("t1")
	if err := saveLead(h.dir("t1"), &lead{ID: "t1"}); err != nil {
		t.Fatal(err)
	}
	h.must("report", "--dir", h.dir("t1"), "--key", "context", "needs-decision", "First question")
	h.must("send", "t1", "--resolve", "context", "First answer")
	h.must("report", "--dir", h.dir("t1"), "--key", "context", "needs-decision", "Reopened question")
	var status struct {
		Leads []row `json:"leads"`
	}
	if err := json.Unmarshal([]byte(h.must("status", "--json")), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Leads) != 1 || len(status.Leads[0].Decisions) != 1 || status.Leads[0].Decisions[0].Text != "Reopened question" {
		t.Fatalf("reopened decision must appear once with its latest text: %+v", status.Leads)
	}
}

const askCard = `---
question: Does a script make the page?
recommend: orch
change: hard
---
## Context
Some <b>context</b> here.

include: ctx.md

## Options
### script: A script makes the page
Gain: fast.
include: diagram.svg A diagram

### orch: The orchestrator writes it
Gain: flexible.
include: shot.png A screenshot

## Next
Then we go on.
`

// askLead spawns a lead t1 with the given open decisions and returns its questions dir.
func askLead(t *testing.T, h *harness, keys ...string) string {
	t.Helper()
	h.task("t1")
	h.must("spawn", "t1")
	for _, k := range keys {
		h.must("report", "--dir", h.dir("t1"), "--key", k, "needs-decision", "Question for "+k+"?")
	}
	qd := filepath.Join(h.dir("t1"), "questions")
	if err := os.MkdirAll(qd, 0o755); err != nil {
		t.Fatal(err)
	}
	return qd
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *harness) askPage() string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.tmp, "state", "repo", "ask", "index.html"))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func TestAsk(t *testing.T) {
	h := setup(t)
	qd := askLead(t, h, "page", "other")
	writeFiles(t, qd, map[string]string{
		"page.md": askCard, "ctx.md": "Included **bold** text.\n", "diagram.svg": `<svg id="inline-svg"></svg>`,
		"shot.png": "PNGDATA",
	})
	out := h.must("ask")
	wantContains(t, out, "page: ", "index.html", "questions: 2\n", "help[2]:", "lavish-axi ", "lavish-axi poll ")
	info := decodedOutput(t, []byte(out), outputAgent).(map[string]any)
	if len(info["decisions"].([]any)) != 2 || len(info["help"].([]any)) != 2 {
		t.Fatalf("incomplete structured ask view: %#v", info)
	}
	page := h.askPage()
	wantContains(t, page, "Does a script make the page?", "Question for other?", "Recommended", "Change later: hard",
		"<strong>bold</strong>", `-diagram.svg"`, `alt="A diagram"`, `-shot.png"`, `alt="A screenshot"`,
		`data-lavish-question="t1/page"`, `data-lavish-question="t1/other"`, "Then we go on.", "Some ")
	if strings.Contains(page, "<b>context") {
		t.Error("raw markdown HTML was emitted")
	}
	if strings.Index(page, "The orchestrator writes it") > strings.Index(page, "A script makes the page") {
		t.Error("recommended option is not first")
	}
	shot := regexp.MustCompile(`src="(files/[^"]+-shot\.png)" alt="A screenshot"`).FindStringSubmatch(page)
	if len(shot) != 2 {
		t.Fatal("screenshot has no image source")
	}
	shotPath := filepath.Join(h.tmp, "state", "repo", "ask", shot[1])
	if b, err := os.ReadFile(shotPath); err != nil || string(b) != "PNGDATA" {
		t.Errorf("png not copied: %v", err)
	}
	h.must("send", "t1", "--resolve", "page", "go")
	h.must("ask")
	page = h.askPage()
	if strings.Contains(page, "Does a script make the page?") || !strings.Contains(page, "Question for other?") {
		t.Errorf("resolved question should leave the page:\n%s", page)
	}
	if _, err := os.Stat(shotPath); err == nil {
		t.Error("files/ should be rebuilt without the resolved png")
	}
}

func TestAskRefusals(t *testing.T) {
	h := setup(t)
	qd := askLead(t, h, "k")
	outside := filepath.Join(h.tmp, "secret.svg")
	writeFiles(t, h.tmp, map[string]string{"secret.svg": "<svg>SECRET</svg>"})
	writeFiles(t, qd, map[string]string{"a.png": "x", "a.txt": "x"})
	if err := os.Symlink(outside, filepath.Join(qd, "link.svg")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ card, want string }{
		"outside":   {"---\nquestion: Q one?\n---\n## Context\ninclude: ../../x.svg\n", "outside the task directory"},
		"symlink":   {"---\nquestion: Q one?\n---\n## Context\ninclude: link.svg\n", "outside the task directory"},
		"noalt":     {"---\nquestion: Q one?\n---\n## Context\ninclude: a.png\n", "needs alt text"},
		"ext":       {"---\nquestion: Q one?\n---\n## Context\ninclude: a.txt\n", "unsupported extension"},
		"missing":   {"---\nquestion: Q one?\n---\n## Context\ninclude: nope.md\n", "file not found"},
		"oneoption": {"---\nquestion: Q one?\n---\n## Options\n### a: Only\nx\n", "one option"},
		"badrec":    {"---\nquestion: Q one?\nrecommend: z\n---\n## Options\n### a: A\nx\n### b: B\ny\n", "not an option"},
		"noq":       {"---\nchange: easy\n---\n", "needs a question"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			writeFiles(t, qd, map[string]string{"k.md": c.card})
			code, out := h.run("ask")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			wantContains(t, out, "t1/k,", c.want)
			page := h.askPage()
			wantContains(t, page, "Question for k?", "Warning: ", `data-lavish-question="t1/k"`)
			if strings.Contains(page, "SECRET") {
				t.Error("outside file leaked into page")
			}
		})
	}
}

func TestAskEscaping(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	h.must("report", "--dir", h.dir("t1"), "--key", "k", "needs-decision", "Use <b>bold</b>?")
	h.must("report", "--dir", h.dir("t1"), "--key", "c", "needs-decision", "card?")
	qd := filepath.Join(h.dir("t1"), "questions")
	os.MkdirAll(qd, 0o755)
	writeFiles(t, qd, map[string]string{"c.md": "---\nquestion: Card <i>q</i>?\n---\n## Context\n<script>alert(1)</script>\n\nplain\n"})
	h.must("ask")
	page := h.askPage()
	wantContains(t, page, "Use &lt;b&gt;bold&lt;/b&gt;?", "Card &lt;i&gt;q&lt;/i&gt;?", "plain")
	for _, bad := range []string{"<b>bold", "alert(1)</script>", "<i>q"} {
		if strings.Contains(page, bad) {
			t.Errorf("page contains unescaped %q", bad)
		}
	}
}

func TestAskEmpty(t *testing.T) {
	h := setup(t)
	out := h.must("ask")
	wantContains(t, out, "No open questions.")
	wantContains(t, h.askPage(), "No open questions.")
}

func TestRenderMarkdownKeepsLineBreaks(t *testing.T) {
	got, err := renderMarkdown("Gain: a\nCost: b\n")
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, got, "Gain: a<br>\nCost: b")
}

func TestRenderMarkdownTables(t *testing.T) {
	got, err := renderMarkdown("| Option | Cost |\n|---|---|\n| S3 | $5 |\n")
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, got, "<th>Option</th>")
	wantContains(t, got, "<td>S3</td>")
}

// A key from status.log goes into a shell command on the page, so a key
// that does not match keyRe must never reach data-resolve.
func TestAskRejectsUnsafeKey(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	f, err := os.OpenFile(filepath.Join(h.dir("t1"), "status.log"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "needs-decision [key=x; touch pwned]: Pick one?")
	f.Close()
	out := h.must("ask")
	wantContains(t, out, "questions: 1\n")
	page := h.askPage()
	wantContains(t, page, `data-resolve=""`, "keyless-1", "must match")
	if strings.Contains(page, "touch pwned\"") || strings.Contains(page, `data-key="x;`) {
		t.Errorf("unsafe key reached the page attributes")
	}
}

// Leads can copy outside text into an include, so .html is cleaned and .svg
// is an <img>, where no script runs.
func TestAskIncludeSafety(t *testing.T) {
	h := setup(t)
	qd := askLead(t, h, "k")
	writeFiles(t, qd, map[string]string{
		"k.md": "---\nquestion: Q?\n---\n## Context\ninclude: mock.html\n\ninclude: d.svg A diagram\n",
		"mock.html": `<div class="card" style="padding: 8px" id="lavish" onclick="x()"><table><tr><td>Cell</td></tr></table>` +
			`<script>window.lavish.queuePrompt("evil")</script><a href="javascript:x()">link</a>` +
			`<button>Go</button><input name="note"><form data-lavish-question="t1/k"></form></div>`,
		"d.svg": `<svg><script>alert(1)</script></svg>`,
	})
	h.must("ask")
	page := h.askPage()
	wantContains(t, page, `class="card"`, `style="padding: 8px"`, "<td>Cell</td>")
	if !regexp.MustCompile(`<img src="files/[^"]+-d\.svg" alt="A diagram">`).MatchString(page) {
		t.Error("SVG include was not emitted as an image")
	}
	for _, bad := range []string{`id="lavish"`, "onclick", `queuePrompt("evil")`, "javascript:", "<button>Go", `<input name="note"`, "alert(1)"} {
		if strings.Contains(page, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	if n := strings.Count(page, `data-lavish-question=`); n != 1 {
		t.Errorf("page has %d answer forms, want 1", n)
	}
}

// --cards renders a folder of cards with no orchestrate run: no lead, no
// orch send line, answered/ ignored, includes confined to the topic folder.
func TestAskCards(t *testing.T) {
	h := setup(t)
	topic := filepath.Join(h.tmp, "brainstorm", "pages")
	qd := filepath.Join(topic, "questions")
	if err := os.MkdirAll(filepath.Join(qd, "answered"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, topic, map[string]string{"map.md": "Idea **map**.\n"})
	writeFiles(t, qd, map[string]string{
		"b-pick.md":       "---\nquestion: Pick one?\nrecommend: x\n---\n## Context\ninclude: ../map.md\n\n## Options\n### x: Ex\nGain: a.\nCost: less b.\n\n### y: Why\nGain: b.\nCost: less a.\n\n## Next\nThe lead implements the choice.\n",
		"a-out.md":        strings.Replace(readyAskCard, "The Python API uses asyncio.TaskGroup.", "include: ../../secret.md\n\nThe Python API uses asyncio.TaskGroup.", 1),
		"Bad Name.md":     "---\nquestion: Skipped?\n---\n",
		"answered/old.md": "---\nquestion: Old?\n---\n",
	})
	writeFiles(t, filepath.Dir(topic), map[string]string{"secret.md": "SECRET"})
	out := h.must("ask", "--cards", qd)
	page, err := os.ReadFile(filepath.Join(topic, "ask", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, out, "page: "+filepath.Join(topic, "ask", "index.html"), "questions: 1\n", "cards: 3\n", "not_ready: 2\n", "a-out,", "outside the task directory")
	warnings, ok := decodedOutput(t, []byte(out), outputAgent).(map[string]any)["warnings"].([]any)
	if !ok {
		t.Fatal("Ask output has no decoded warning rows")
	}
	found := false
	for _, row := range warnings {
		warning, ok := row.(map[string]any)
		if ok && warning["question"] == "Bad Name.md" && warning["message"] == fmt.Sprintf("skipped: the name must match %s.md", keyRe) {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing bad-name warning attached to Bad Name.md: %v", warnings)
	}
	wantContains(t, string(page), "Pick one?", "Idea <strong>map</strong>.", `data-lavish-question="b-pick"`, `data-lead=""`, "Answer for question ")
	for _, bad := range []string{"SECRET", "Old?", "Skipped?", `data-lavish-question="/`} {
		if strings.Contains(string(page), bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	if strings.Contains(string(page), `data-lavish-question="a-out"`) {
		t.Error("a card with an unavailable include was asked as complete")
	}
	if code, out := h.run("ask", "--cards", filepath.Join(h.tmp, "nope")); code == 0 || !strings.Contains(out, "is not a directory") {
		t.Errorf("missing --cards dir: exit %d, %s", code, out)
	}
}

const planHeader = "---\nharness: claude\nmodel: sonnet\neffort: medium\nbase: main\n---\n# Transfer queue\n\n## Behaviour\nUsers queue transfers.\n\n## Design rules\nOne queue per panel.\n\n## Authority\nMay decide: names\nMust escalate: API shape\nNever: push\n\n## Packages\n\n"

const planTail = "\n## Open questions\nNone.\n"

func planPkg(id, keys, extra string) string {
	builds := ""
	if strings.Contains(keys, "kind: ship") {
		builds = "#### Builds\n- Code that builds " + id + ".\n\n"
	}
	return "### " + id + ": Package " + id + "\n" + keys + "\n\nBuild " + id + ".\n\n" + builds + "#### Acceptance\nTests for " + id + " pass." + extra + "\n\n"
}

func (h *harness) git(args ...string) {
	h.t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", h.repo}, args...)...).CombinedOutput(); err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (h *harness) commitSpec(body string) {
	h.t.Helper()
	writeFiles(h.t, h.repo, map[string]string{"spec.md": body})
	h.git("add", "spec.md")
	h.git("commit", "-q", "-m", "spec")
}

func (h *harness) planRefused(want ...string) {
	h.t.Helper()
	code, out := h.run("plan", "spec.md", "--skip-gate")
	if code != 1 {
		h.t.Fatalf("plan exited %d, want 1:\n%s", code, out)
	}
	wantContains(h.t, out, append([]string{"problems[", "nothing was created"}, want...)...)
	if entries := allTasks(runRoot(h.repo)); len(entries) > 0 {
		h.t.Fatalf("refused plan created tasks: %v", entries)
	}
}

func threePkgSpec() string {
	return planHeader +
		planPkg("store", "kind: ship\nmodel: haiku\nowns: internal/store.go, migrations/", "") +
		planPkg("api", "kind: ship\ndepends: store\nowns: internal/api.go", "") +
		planPkg("docs", "kind: scout\ndepends: api, store", "") +
		planTail
}

func TestPlan(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	out := h.must("plan", "spec.md", "--skip-gate")
	wantContains(t, out, "spec: spec.md", "packages: 3", "tasks[3]{id,kind,model,depends,status}:",
		"  store,ship,haiku,,created", "  api,ship,sonnet,store,created", "  docs,scout,sonnet,api store,created",
		"waves[3]:", "  store\n  api\n  docs\n", "orch check <id>")
	for _, id := range []string{"store", "api", "docs"} {
		fm, _, err := readTask(filepath.Join(h.dir(id), "task.md"))
		if err != nil || fm["spec"] != "spec.md#"+id {
			t.Fatalf("%s: spec = %q, err %v", id, fm["spec"], err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(h.dir("api"), "task.md"))
	text := string(b)
	wantContains(t, text, "id: api\nspec: spec.md#api\n", "owns: internal/api.go\n", "depends_on: [store]", "effort: medium",
		"Transfer queue: Package api. The feature is specified in `spec.md`; read the sections under Pointers",
		"In: internal/api.go\nOut: files owned by other packages of this spec: internal/store.go, migrations/\n",
		"`spec.md:", "### api - this package", "Build api.", "Tests for api pass.",
		"May decide: names\nMust escalate: API shape\nNever: push")
	if strings.Contains(text, "{{") {
		t.Errorf("placeholders left:\n%s", text)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir("docs"), "task.md")); !strings.Contains(string(b), "In: research only, no repo edits") {
		t.Errorf("scout scope wrong:\n%s", b)
	}
	problems, _, err := checkTask(h.a.siteRoot(t), "store")
	if err != nil || len(problems) != 0 {
		t.Errorf("store should pass check: %v %v", problems, err)
	}
	problems, _, _ = checkTask(h.a.siteRoot(t), "api")
	if len(problems) != 1 || !strings.Contains(problems[0], `depends_on: "store" is not done yet`) {
		t.Errorf("api should only wait on store: %v", problems)
	}
}

func TestCheckWarnsOnLargeBrief(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	h.must("plan", "spec.md", "--skip-gate")
	root := h.a.siteRoot(t)
	_, warnings, err := checkTask(root, "store")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "characters") {
			t.Errorf("small brief warned about size: %s", w)
		}
	}
	path := filepath.Join(h.dir("store"), "task.md")
	b, _ := os.ReadFile(path)
	big := strings.Replace(string(b), "\n# Acceptance\n", "\n# Padding\n\n"+strings.Repeat("x", briefCharLimit)+"\n\n# Acceptance\n", 1)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	problems, warnings, err := checkTask(root, "store")
	if err != nil || len(problems) != 0 {
		t.Fatalf("a large brief must only warn: %v %v", problems, err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "characters") && strings.Contains(w, "Largest sections: Padding (") {
			found = true
		}
	}
	if !found {
		t.Errorf("no size warning naming Padding: %v", warnings)
	}
}

func TestPlanScopeOutIsCapped(t *testing.T) {
	h := setup(t)
	spec := planHeader
	for i := range 30 {
		spec += planPkg(fmt.Sprintf("pkg%d", i), "kind: ship\nowns: "+strings.Repeat("internal/some/long/dir/name/", 2)+fmt.Sprintf("file%d.go", i), "")
	}
	h.commitSpec(spec + planTail)
	h.must("plan", "spec.md", "--skip-gate")
	b, _ := os.ReadFile(filepath.Join(h.dir("pkg3"), "task.md"))
	text := string(b)
	wantContains(t, text, "Out: every file owned by another package of this spec (29 files cut from this list;", "pkg0, pkg1,", "`spec.md:")
	if strings.Contains(text, "file0.go") {
		t.Errorf("file list not cut:\n%s", text)
	}
	problems, _, _ := checkTask(h.a.siteRoot(t), "pkg3")
	if len(problems) != 0 {
		t.Errorf("capped brief should pass check: %v", problems)
	}
}

func (a *app) siteRoot(t *testing.T) string {
	t.Helper()
	s, err := a.site()
	if err != nil {
		t.Fatal(err)
	}
	return s.root
}

func TestPlanRerunAndDryRun(t *testing.T) {
	h := setup(t)
	h.commitSpec(threePkgSpec())
	dry := h.must("plan", "spec.md", "--dry-run", "--skip-gate")
	wantContains(t, dry, "  store,ship,haiku,,would-create", "  docs,scout,sonnet,api store,would-create")
	if _, err := os.Stat(filepath.Join(h.tmp, "state", "repo")); err == nil {
		t.Fatal("dry run created the run directory")
	}
	h.must("plan", "spec.md", "--skip-gate")
	before, _ := os.ReadFile(filepath.Join(h.dir("api"), "task.md"))
	out := h.must("plan", "spec.md", "--skip-gate")
	if strings.Contains(out, "created") || strings.Count(out, ",exists") != 3 {
		t.Errorf("rerun should only report exists:\n%s", out)
	}
	if after, _ := os.ReadFile(filepath.Join(h.dir("api"), "task.md")); !bytes.Equal(before, after) {
		t.Error("rerun rewrote task.md")
	}
	h.commitSpec(strings.TrimSuffix(threePkgSpec(), planTail) + planPkg("extra", "kind: ship\nowns: internal/extra.go", "") + planTail)
	out = h.must("plan", "spec.md", "--skip-gate")
	wantContains(t, out, "packages: 4", "  extra,ship,sonnet,,created")
	if strings.Count(out, ",exists") != 3 {
		t.Errorf("want 3 existing:\n%s", out)
	}
}

func TestPlanRefusals(t *testing.T) {
	pkgs := func(ps ...string) string { return planHeader + strings.Join(ps, "") + planTail }
	a := planPkg("alpha", "kind: ship\nowns: internal/", "")
	cases := []struct {
		name, spec string
		want       []string
	}{
		{"open questions", strings.Replace(pkgs(a), "None.", "Which db?", 1), []string{"open questions"}},
		{"overlap", pkgs(a, planPkg("beta", "kind: ship\nowns: ./internal/x.go", "")), []string{"ownership overlap: alpha owns", "beta owns"}},
		{"unknown depends", pkgs(planPkg("alpha", "kind: ship\nowns: a.go\ndepends: ghost", "")), []string{"ghost", "which is not a package of this spec"}},
		{"cycle", pkgs(planPkg("alpha", "kind: ship\nowns: a.go\ndepends: beta", ""), planPkg("beta", "kind: ship\nowns: b.go\ndepends: alpha", "")), []string{"dependency cycle among: alpha beta"}},
		{"no acceptance", pkgs("### alpha: A\nkind: ship\nowns: a.go\n\nBody.\n\n"), []string{"package alpha: needs a non-empty `#### Acceptance`"}},
		{"no owns and bad kind", pkgs(planPkg("alpha", "kind: ship", ""), planPkg("beta", "kind: odd\nwat: 1", "")), []string{"needs `owns:`", "kind must be ship or scout", "unknown key"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := setup(t)
			h.commitSpec(c.spec)
			h.planRefused(c.want...)
		})
	}

	t.Run("not committed", func(t *testing.T) {
		h := setup(t)
		writeFiles(t, h.repo, map[string]string{"spec.md": pkgs(a)})
		h.planRefused("commit the spec to main first")
	})
	t.Run("uncommitted edits", func(t *testing.T) {
		h := setup(t)
		h.commitSpec(pkgs(a))
		writeFiles(t, h.repo, map[string]string{"spec.md": pkgs(a) + "more\n"})
		h.planRefused("spec has changes not in main")
	})
	t.Run("id clash", func(t *testing.T) {
		h := setup(t)
		h.commitSpec(pkgs(a))
		h.task("alpha")
		code, out := h.run("plan", "spec.md", "--skip-gate")
		if code != 1 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		wantContains(t, out, "task id alpha is already used by another task", "nothing was created")
		if b, _ := os.ReadFile(filepath.Join(h.dir("alpha"), "task.md")); !strings.Contains(string(b), "filled in") {
			t.Error("existing task was rewritten")
		}
	})
}

// Anthropic credits through pi are their own lane: a Claude subscription limit
// sends work there first, and an OpenRouter limit does not touch them.
func TestAnthropicCreditsViaPiAreTheFirstFallback(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(`max_leads = 8
[allowed_models]
claude = ["sonnet"]
pi = ["flash", "anthropic/claude-sonnet-5-5"]

[[dispatch]]
when = "standard"
tier = "standard"
use = [{ harness = "claude", model = "sonnet" }, { harness = "pi", model = "anthropic/claude-sonnet-5-5" }, { harness = "pi", model = "flash", degraded = true }]
`), 0o644)
	h.task("a", "--harness", "auto", "--model", "auto", "--effort", "auto")
	h.dec.set(200, decReply(standardAnswers))

	h.must("health", "set", "claude", "limited")
	wantContains(t, h.must("dispatch"), "now: pi anthropic/claude-sonnet-5-5")
	wantContains(t, h.must("spawn", "a", "--auto"), "skipped claude sonnet: claude is limited", "would use: pi anthropic/claude-sonnet-5-5")
	h.must("new", "b", "--kind", "ship", "--harness", "pi", "--model", "anthropic/claude-sonnet-5-5", "--effort", "high")

	// Out of Anthropic credits as well: the weaker model is only offered, never taken.
	h.must("health", "set", "pi-anthropic", "limited")
	code, out := h.run("spawn", "a", "--auto")
	if code != 1 {
		t.Fatalf("parked spawn should exit 1: %d", code)
	}
	wantContains(t, out, "skipped pi anthropic/claude-sonnet-5-5: pi:anthropic is limited", "degraded option: pi flash")
	if code, out = h.run("new", "c", "--kind", "ship", "--harness", "pi", "--model", "anthropic/claude-sonnet-5-5", "--effort", "high"); code != 1 || !strings.Contains(out, "pi:anthropic is limited") {
		t.Fatalf("anthropic lane used while limited: %d %s", code, out)
	}
	// OpenRouter being down does not block direct Anthropic; the pi parent does.
	h.must("health", "set", "pi-anthropic", "ok")
	h.must("health", "set", "pi:openrouter", "limited")
	h.must("new", "d", "--kind", "ship", "--harness", "pi", "--model", "anthropic/claude-sonnet-5-5", "--effort", "high")
	h.must("health", "set", "pi", "limited")
	if code, out := h.run("new", "e", "--kind", "ship", "--harness", "pi", "--model", "anthropic/claude-sonnet-5-5", "--effort", "high"); code != 1 || !strings.Contains(out, "pi is limited") {
		t.Fatalf("pi parent was ignored: %d %s", code, out)
	}
}

func TestAdversaryScopeNamesOnlyRealSpecFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "see a.go:3 and a.go:1, missing.go:9, https://example.com/x.go:2"
	files := specFiles(repo, src)
	if !slices.Equal(files, []string{"a.go"}) {
		t.Fatalf("specFiles = %v, want [a.go]", files)
	}
	if got := adversaryScope(files); !strings.Contains(got, "a.go") || !strings.Contains(got, "empty findings list") {
		t.Fatalf("scope clause = %q", got)
	}
	if adversaryScope(nil) != "" {
		t.Fatal("no files should add no scope clause")
	}
}

func TestAcceptanceCommandSkipsProse(t *testing.T) {
	for span, want := range map[string]string{
		"scripts/agent/check test ./internal/x/...": "scripts/agent/check",
		"go test ./...":            "go",
		"./run.sh":                 "./run.sh",
		"models.go":                "",
		"crm:invoices.refund":      "",
		"Retry-After":              "",
		"SANDBOX_STRIPE_*":         "",
		"safehttp":                 "",
		"ON CONFLICT DO NOTHING":   "",
		"models.go owns the table": "",
	} {
		if got, ok := acceptanceCommand(span); got != want || ok != (want != "") {
			t.Errorf("acceptanceCommand(%q) = %q, %v; want %q", span, got, ok, want)
		}
	}
}

// A file renamed into the owned paths must still report the path it left, or
// a move from outside owns would pass the scope check.
func TestVerifyFlagsRenameFromOutsideOwns(t *testing.T) {
	h := setup(t)
	os.MkdirAll(filepath.Join(h.repo, "outside"), 0o755)
	os.WriteFile(filepath.Join(h.repo, "outside", "f.go"), []byte("package x\n\nfunc F() {}\n"), 0o644)
	h.git("add", "-A")
	h.git("commit", "-q", "-m", "add outside/f.go")
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte("check = \"true\"\n"), 0o644)

	h.task("t1")
	d := h.dir("t1")
	path := filepath.Join(d, "task.md")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, bytes.Replace(b, []byte("\ndepends_on:"), []byte("\nowns: owned/\ndepends_on:"), 1), 0o644)
	wt := filepath.Join(filepath.Dir(d), "worktrees", "t1")
	h.gitIn(h.repo, "worktree", "add", "-q", "-b", "orch/t1", wt, "main")
	os.MkdirAll(filepath.Join(wt, "owned"), 0o755)
	h.gitIn(wt, "mv", "outside/f.go", "owned/f.go")

	files, err := changedFiles(wt, "main")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if strings.Join(files, ",") != "outside/f.go,owned/f.go" {
		t.Fatalf("changedFiles = %v; a rename must list both paths", files)
	}
	if n := changedLines(wt, "main"); n != 6 {
		t.Fatalf("changedLines = %d; a rename counts as delete plus add (3 + 3)", n)
	}
	if code, out := h.run("verify", "t1"); code != 1 || !strings.Contains(out, "outside_owns[1]:") || !strings.Contains(out, "outside/f.go") {
		t.Fatalf("rename from outside owns passed: %d %s", code, out)
	}
}

// orch ask lists only the questions of leads this orchestrator watches.
func TestAskListsOnlyMyLeads(t *testing.T) {
	h := setup(t)
	h.task("a1")
	h.must("spawn", "a1")
	h.must("report", "--dir", h.dir("a1"), "--key", "mine", "needs-decision", "Question from mine?")
	t.Setenv("HERDR_PANE_ID", "w2:p0")
	h.task("b1")
	h.must("spawn", "b1")
	h.must("report", "--dir", h.dir("b1"), "--key", "theirs", "needs-decision", "Question from theirs?")

	h.must("ask")
	page := h.askPage()
	if !strings.Contains(page, "Question from theirs?") || strings.Contains(page, "Question from mine?") {
		t.Fatalf("w2:p0 page should hold only its own lead's question:\n%s", page)
	}
	t.Setenv("HERDR_PANE_ID", "w1:p0")
	h.must("ask")
	page = h.askPage()
	if !strings.Contains(page, "Question from mine?") || strings.Contains(page, "Question from theirs?") {
		t.Fatalf("w1:p0 page should hold only its own lead's question:\n%s", page)
	}
}

func TestBriefTellsLeadToReadInboxBeforeDone(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("t1"), "task.md"))), "inbox --dir "+h.dir("t1")+"`. Run it once more before you report `done`")
}

func TestLeadStopMentionsUnreadMailOnce(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	send := func(text string) {
		t.Setenv("ORCH_ROLE", "")
		h.must("send", "t1", text)
		t.Setenv("ORCH_ROLE", "lead")
	}
	t.Setenv("ORCH_ROLE", "lead")
	t.Setenv("ORCH_TASK_DIR", d)
	stop := func(active bool) (int, string) {
		return h.hook("stop", map[string]any{"session_id": "x", "stop_hook_active": active})
	}
	h.must("report", "working", "step one")
	if code, out := stop(false); code != 0 {
		t.Fatalf("no mail, report written = %d %q", code, out)
	}
	send("first")
	h.must("report", "working", "step two")
	code, out := stop(false)
	if code != 2 || !strings.Contains(out, "you have 1 unread message(s): run `/skill/scripts/orch inbox --dir "+d+"`") {
		t.Fatalf("unread mail after a report = %d %q", code, out)
	}
	h.must("report", "working", "step three")
	if code, out := stop(false); code != 0 {
		t.Fatalf("same unread mail mentioned twice: %d %q", code, out)
	}
	if code, out := stop(true); code != 0 {
		t.Fatalf("stop_hook_active blocked: %d %q", code, out)
	}
	send("second")
	h.must("report", "working", "step four")
	if code, out := stop(false); code != 2 || !strings.Contains(out, "2 unread message(s)") {
		t.Fatalf("new mail not mentioned: %d %q", code, out)
	}
}

func TestWatchRingsDoorbellOnceForStaleUnreadMail(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	h.must("send", "t1", "please look")
	msg := filepath.Join(d, "inbox", "001.msg")
	age := func(m time.Duration) {
		old := h.now.Add(-m)
		if err := os.Chtimes(msg, old, old); err != nil {
			t.Fatal(err)
		}
	}
	bells := func() int { return len(callsOf(h.fakeState(), "agent", "prompt", "wt1:p1")) }
	base := bells()
	setStatus := func(s string) { h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Status = s }) }

	for _, s := range []string{"working", "blocked"} {
		setStatus(s)
		age(time.Hour)
		h.must("watch", "--once")
		if bells() != base {
			t.Fatalf("rang a %s lead", s)
		}
	}
	setStatus("idle")
	age(9 * time.Minute)
	if out := h.must("watch", "--once"); strings.Contains(out, "reminder") || bells() != base {
		t.Fatalf("rang before unread_minutes: %q", out)
	}
	age(10 * time.Minute)
	out := h.must("watch", "--once")
	wantContains(t, out, "orch: t1 reminder: 1 unread message(s) for 10m; rang the doorbell again")
	if bells() != base+1 {
		t.Fatalf("bells = %d, want %d", bells(), base+1)
	}
	if out := h.must("watch", "--once"); strings.Contains(out, "reminder") || bells() != base+1 {
		t.Fatalf("rang the same batch twice: %q", out)
	}
	h.must("inbox", "--dir", d)
	h.must("watch", "--once")
	h.must("send", "t1", "again")
	msg = filepath.Join(d, "inbox", "002.msg")
	age(time.Hour)
	setStatus("idle") // the fake marks a prompted agent working
	h.must("watch", "--once")
	if bells() != base+3 { // the send's own bell plus one re-ring for the new batch
		t.Fatalf("new batch not rung: bells = %d", bells())
	}
}

func TestWatchUnreadMinutesZeroDisablesReminder(t *testing.T) {
	h := setup(t)
	if err := os.WriteFile(filepath.Join(h.tmp, "config.toml"), []byte("unread_minutes = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.task("t1")
	h.must("spawn", "t1")
	h.must("send", "t1", "please look")
	old := h.now.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(h.dir("t1"), "inbox", "001.msg"), old, old); err != nil {
		t.Fatal(err)
	}
	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Status = "idle" })
	before := len(callsOf(h.fakeState(), "agent", "prompt", "wt1:p1"))
	if out := h.must("watch", "--once"); strings.Contains(out, "reminder") || len(callsOf(h.fakeState(), "agent", "prompt", "wt1:p1")) != before {
		t.Fatalf("reminder with unread_minutes = 0: %q", out)
	}
}

func TestUnreadMinutesIsAUserKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orchestrate.toml")
	if err := os.WriteFile(path, []byte("unread_minutes = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := readRepoConfig(path, &repoConfig{})
	if err == nil || !strings.Contains(err.Error(), "is not allowed here") {
		t.Fatalf("repo file set a user key: %v", err)
	}
}

func TestReportRefusesReplacedSession(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	epoch := h.lead("t1").Epoch
	if epoch == "" {
		t.Fatal("spawn recorded no epoch")
	}
	log := filepath.Join(d, "status.log")
	before := string(mustRead(t, log))

	t.Setenv("ORCH_EPOCH", "stale")
	if code, out := h.run("report", "--dir", d, "working", "ghost"); code != 1 || !strings.Contains(out, "replaced by a respawn") {
		t.Fatalf("stale epoch report = %d %q", code, out)
	}
	if string(mustRead(t, log)) != before {
		t.Fatal("refused report wrote to status.log")
	}
	t.Setenv("ORCH_EPOCH", epoch)
	h.must("report", "--dir", d, "working", "current")
	t.Setenv("ORCH_EPOCH", "")
	h.must("report", "--dir", d, "working", "manual")

	h.must("respawn", "t1", "--harness", "pi", "--model", "flash")
	if h.lead("t1").Epoch == epoch {
		t.Fatal("respawn kept the old epoch")
	}
	t.Setenv("ORCH_EPOCH", epoch)
	if code, _ := h.run("report", "--dir", d, "working", "old session"); code != 1 {
		t.Fatalf("replaced session could report: %d", code)
	}
}
