package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const quickRequest = "make slugify in slug.py return an empty string when no letter or digit remains"

func quickRepo(t *testing.T, h *harness, check string) {
	t.Helper()
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(autoConfig), 0o644)
	os.MkdirAll(filepath.Join(h.repo, ".agents"), 0o755)
	body := ""
	if check != "" {
		body = "check = \"" + check + "\"\n"
	}
	os.WriteFile(filepath.Join(h.repo, ".agents", "orchestrate.toml"), []byte(body), 0o644)
}

func TestQuickStructuralLimits(t *testing.T) {
	h := setup(t)
	quickRepo(t, h, "")
	refuse := func(want string, args ...string) {
		t.Helper()
		code, out := h.run(append([]string{"quick", "q1"}, args...)...)
		if code != 1 || !strings.Contains(out, want) {
			t.Fatalf("quick %v = %d %s; want refusal with %q", args, code, out, want)
		}
		if _, err := os.Stat(h.dir("q1")); err == nil {
			t.Fatalf("a refused quick job left %s behind", h.dir("q1"))
		}
	}
	refuse("needs a `check` command", "--owns", "slug.py", quickRequest)

	quickRepo(t, h, "true")
	refuse("1 to 3 files", quickRequest)
	refuse("1 to 3 files", "--owns", "a.py,b.py,c.py,d.py", quickRequest)
	refuse("plain relative file path", "--owns", "src/", quickRequest)
	os.MkdirAll(filepath.Join(h.repo, "src"), 0o755)
	os.WriteFile(filepath.Join(h.repo, "src", "slug.py"), []byte("x = 1\n"), 0o644)
	h.git("add", "-A")
	h.git("commit", "-q", "-m", "add src")
	refuse("is a directory", "--owns", "src", quickRequest)
	refuse("is a directory", "--owns", "src/slug.py,src", quickRequest)
	refuse("plain relative file path", "--owns", "src/*.py", quickRequest)
	refuse("plain relative file path", "--owns", "../x.py", quickRequest)
	refuse("under 20 characters", "--owns", "slug.py", "fix it")
	refuse("which is a spec", "--owns", "slug.py", strings.Repeat("word ", 200))

	h.must("hold", "on", "testing")
	refuse("spawning is on hold", "--owns", "slug.py", quickRequest)
	h.must("hold", "off")
	h.must("health", "set", "pi", "limited")
	h.must("health", "set", "claude", "limited")
	refuse("no usable profile", "--owns", "slug.py", quickRequest)
}

func TestQuickShadowRunsAndRecordsTheVerdict(t *testing.T) {
	h := setup(t)
	quickRepo(t, h, "true")

	// Low confidence would move a brief up a tier; the structural limits
	// already bound a quick job, so a clear and easy request still runs.
	low := settledAnswers
	low.conf = 0.5
	h.dec.set(200, decReply(low))
	out := h.must("quick", "q1", "--owns", "slug.py", quickRequest)
	wantContains(t, out, "quick: q1 on pi flash (effort low), owns slug.py", "spawned lead-q1 (pi flash)")
	fm, body, _ := readTask(filepath.Join(h.dir("q1"), "task.md"))
	if fm["quick"] != "true" || fm["owns"] != "slug.py" || fm["tier"] != "quick" || fm["harness"] != "pi" {
		t.Fatalf("task front matter = %v", fm)
	}
	secs := sections(body)
	wantContains(t, secs["Spec"]+secs["Acceptance"]+secs["Authority"], quickRequest, "`true` passes", "one-attempt job", "Never: push, merge, edit outside the owned files")
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("q1"), "status.log"))), "quick: make slugify")
	if n := h.dec.count(); n != 1 {
		t.Fatalf("triage asked %d times; spawn must reuse the quick verdict", n)
	}
	recs := readTriage(filepath.Dir(h.dir("q1")))
	if got := recs[len(recs)-1]; got.Tier != "quick" || got.Actual != "pi flash" {
		t.Fatalf("last triage record = %+v", got)
	}

	// A hard request in shadow mode warns and still runs.
	h.dec.set(200, decReply(planningAnswers))
	out = h.must("quick", "q2", "--owns", "slug.py", quickRequest+" and rework the whole parser")
	wantContains(t, out, "warning: triage says this is not a quick job", "quick_mode is shadow, so it runs anyway", "spawned lead-q2")
	if recs := readTriage(filepath.Dir(h.dir("q2"))); recs[len(recs)-1].Tier != "not-quick" {
		t.Fatalf("hard request logged as %q", recs[len(recs)-1].Tier)
	}

	// An unreachable decision model never blocks in shadow mode.
	h.dec.set(500, `{"error":"down"}`)
	wantContains(t, h.must("quick", "q3", "--owns", "slug.py", quickRequest), "warning: triage unavailable", "spawned lead-q3")

	// The report separates the quick verdicts, and counts a quick job that
	// went wrong as the costly error.
	h.must("report", "--dir", h.dir("q1"), "done", "branch orch/q1")
	h.must("report", "--dir", h.dir("q2"), "failed", "could not do it")
	rep := h.must("triage", "report")
	wantContains(t, rep, "quick,1,0,1,0%", "not-quick,1,1,0,100%")
}

func TestQuickEnforceRefusesAndCleansUp(t *testing.T) {
	h := setup(t)
	quickRepo(t, h, "true")
	cfg := string(mustRead(t, os.Getenv("ORCH_CONFIG")))
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("quick_mode = \"enforce\"\n"+cfg), 0o644)

	h.dec.set(200, decReply(planningAnswers))
	code, out := h.run("quick", "q1", "--owns", "slug.py", quickRequest)
	if code != 1 || !strings.Contains(out, "not a quick job") {
		t.Fatalf("enforce let a hard job through: %d %s", code, out)
	}
	if _, err := os.Stat(h.dir("q1")); err == nil {
		t.Fatal("a refused quick job left its task behind")
	}
	if len(callsOf(h.fakeState(), "agent", "start")) != 0 {
		t.Fatal("a pane was started for a refused quick job")
	}
	h.dec.set(500, `{"error":"down"}`)
	if code, out := h.run("quick", "q2", "--owns", "slug.py", quickRequest); code != 1 || !strings.Contains(out, "triage is off") || !strings.Contains(out, "pi:openrouter") {
		t.Fatalf("enforce guessed without a verdict: %d %s", code, out)
	}
	// A reworded request is a new text, so it is rated again.
	h.dec.set(200, decReply(settledAnswers))
	h.must("quick", "q1", "--owns", "slug.py", quickRequest+", changing only the function body")

	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("quick_mode = \"off\"\n"+cfg), 0o644)
	if code, out := h.run("quick", "q9", "--owns", "slug.py", quickRequest); code != 1 || !strings.Contains(out, "quick is off") {
		t.Fatalf("quick ran while off: %d %s", code, out)
	}
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("quick_mode = \"loud\"\n"), 0o644)
	if code, out := h.run("status"); code != 1 || !strings.Contains(out, "quick_mode must be") {
		t.Fatalf("bad quick_mode accepted: %d %s", code, out)
	}
}

func TestQuickJobGetsOneAttempt(t *testing.T) {
	h := setup(t)
	quickRepo(t, h, "test -f slug.py")
	h.must("quick", "q1", "--owns", "slug.py", quickRequest)
	h.task("full", "--harness", "pi", "--model", "flash", "--effort", "low")
	wt := filepath.Join(filepath.Dir(h.dir("q1")), "worktrees", "q1") // made by spawn inside Herdr

	// The lead reports done; the watcher verifies it without being asked.
	h.must("report", "--dir", h.dir("q1"), "done", "branch orch/q1")
	wantContains(t, h.must("watch", "--once"), "q1 done (quick): verify: fail", "frozen: quick job failed verification")
	if r := frozenReason(readStatus(h.dir("q1"))); !strings.Contains(r, "one attempt") {
		t.Fatalf("quick job not frozen after a failed verify: %q", r)
	}

	// The same failure on a full task is not a one-attempt job.
	_ = os.WriteFile(filepath.Join(h.dir("full"), "status.log"), nil, 0o644)
	if err := (&app{now: h.a.now}).appendStatus(h.dir("full"), "note", "verify fail: check exit 1", "", "orch"); err != nil {
		t.Fatal(err)
	}
	if r := frozenReason(readStatus(h.dir("full"))); r != "" {
		t.Fatalf("a full task froze after one failed verify: %q", r)
	}

	// A passing verify on a quick job is only reported.
	os.WriteFile(filepath.Join(wt, "slug.py"), []byte("x = 1\n"), 0o644)
	h.must("unfreeze", "q1")
	h.must("report", "--dir", h.dir("q1"), "note", "fixed by the orchestrator")
	h.must("report", "--dir", h.dir("q1"), "done", "branch orch/q1 again")
	out := h.must("watch", "--once")
	wantContains(t, out, "q1 done (quick): verify: pass")
	if strings.Contains(out, "frozen") {
		t.Fatalf("a passing quick job froze: %s", out)
	}
}

// A quick job carries its own verdict. The spawn-time triage gate, which only
// knows briefs, must not turn a quick verdict into "not settled".
func TestQuickIsNotJudgedTwice(t *testing.T) {
	h := setup(t)
	quickRepo(t, h, "true")
	cfg := string(mustRead(t, os.Getenv("ORCH_CONFIG")))
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("triage_mode = \"enforce\"\nweak_models = [\"flash\"]\n"+cfg), 0o644)
	h.dec.set(200, decReply(settledAnswers))
	wantContains(t, h.must("quick", "q1", "--owns", "slug.py", quickRequest), "spawned lead-q1 (pi flash)")
	if n := h.dec.count(); n != 1 {
		t.Fatalf("quick with enforced spawn triage asked %d times, want once", n)
	}
}

// Files the repo's .gitignore covers are build output, not the lead's work:
// verify, the stall detector and doctor must not look at them.
func TestIgnoredFilesAreNeverChecked(t *testing.T) {
	h := setup(t)
	quickRepo(t, h, "true")
	os.WriteFile(filepath.Join(h.repo, ".gitignore"), []byte("__pycache__/\n*.log\n"), 0o644)
	h.gitIn(h.repo, "add", ".gitignore")
	h.gitIn(h.repo, "commit", "-q", "-m", "ignore")
	h.dec.set(200, decReply(settledAnswers))
	h.must("quick", "q1", "--owns", "slug.py", quickRequest)
	wt := filepath.Join(filepath.Dir(h.dir("q1")), "worktrees", "q1") // made by spawn inside Herdr
	os.WriteFile(filepath.Join(wt, "slug.py"), []byte("x = 1\n"), 0o644)
	os.MkdirAll(filepath.Join(wt, "__pycache__"), 0o755)
	os.WriteFile(filepath.Join(wt, "__pycache__", "slug.pyc"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(wt, "debug.log"), []byte("x"), 0o644)

	out := h.must("verify", "q1")
	wantContains(t, out, "verify: pass", "1 changed")
	before := worktreeFingerprint(wt)
	os.WriteFile(filepath.Join(wt, "debug.log"), []byte("more output"), 0o644)
	os.WriteFile(filepath.Join(wt, "__pycache__", "other.pyc"), []byte("y"), 0o644)
	if worktreeFingerprint(wt) != before {
		t.Fatal("ignored files counted as progress for the stall detector")
	}
	os.WriteFile(filepath.Join(wt, "slug.py"), []byte("x = 2\n"), 0o644)
	if worktreeFingerprint(wt) == before {
		t.Fatal("changing an owned file did not count as progress")
	}
	h.gitIn(wt, "add", "slug.py")
	h.gitIn(wt, "commit", "-q", "-m", "work")
	h.must("report", "--dir", h.dir("q1"), "done", "branch orch/q1")
	if _, out := h.run("doctor"); strings.Contains(out, "uncommitted") {
		t.Fatalf("doctor flagged ignored files: %s", out)
	}
}

// The audit shows, in time order, when each system went right or wrong.
func TestAuditTimeline(t *testing.T) {
	h := setup(t)
	quickRepo(t, h, "test -f slug.py")
	h.dec.set(200, decReply(settledAnswers))
	h.must("health", "set", "pi", "limited", "--for", "1h", "--reason", "out of credit")
	h.now = h.now.Add(time.Minute)
	h.must("health", "set", "pi", "ok")
	h.now = h.now.Add(time.Minute)
	h.must("quick", "q1", "--owns", "slug.py", quickRequest)
	h.now = h.now.Add(time.Minute)
	h.must("report", "--dir", h.dir("q1"), "done", "branch orch/q1")
	h.must("watch", "--once") // verify fails (no slug.py) and the quick job freezes
	h.now = h.now.Add(time.Minute)
	h.must("hold", "on", "looking")
	h.must("hold", "off")

	out := h.must("audit")
	order := []string{"health   pi", "problem pi limited", "health   pi", "ok      pi marked ok", "quick    q1", "triage   q1", "task     q1", "verify   q1", "problem verify fail", "freeze   q1", "hold"}
	at := 0
	for _, want := range order {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("audit lacks %q in order:\n%s", want, out)
		}
		at += i
	}
	wantContains(t, out, "summary[", "verify,0,1,0", "freeze,0,1,0", "health,1,1,0")

	bad := h.must("audit", "--problems")
	if strings.Contains(bad, "marked ok") || strings.Contains(bad, "quick    q1") || !strings.Contains(bad, "verify fail") {
		t.Fatalf("--problems shows the wrong events:\n%s", bad)
	}
	only := h.must("audit", "q1", "--system", "verify")
	if strings.Contains(only, "health") || !strings.Contains(only, "verify   q1") {
		t.Fatalf("filters ignored:\n%s", only)
	}
	h.now = h.now.Add(48 * time.Hour)
	if out := h.must("audit", "--since", "6h"); !strings.Contains(out, "no events") {
		t.Fatalf("--since kept old events:\n%s", out)
	}
	if code, out := h.run("audit", "--since", "soon"); code != 1 || !strings.Contains(out, "not a duration") {
		t.Fatalf("bad --since accepted: %d %s", code, out)
	}
}

// Private text stays off third-party models: triage, spawn and --auto all
// refuse to send it, and verify never hands a lead-written base to git.
func TestPrivateAndTamperedTasks(t *testing.T) {
	h := setup(t)
	quickRepo(t, h, "true")
	h.task("p", "--harness", "pi", "--model", "flash", "--effort", "low")
	h.task("p2", "--harness", "claude", "--model", "sonnet", "--effort", "high")
	setFrontMatter(filepath.Join(h.dir("p2"), "task.md"), "private", "true")
	hits := h.dec.count()
	if code, out := h.run("triage", "p2"); code != 1 || !strings.Contains(out, "p2 is private") {
		t.Fatalf("triage rated a private task: %d %s", code, out)
	}
	if out := h.must("spawn", "p2"); strings.Contains(out, "triage") {
		t.Fatalf("spawning a private task mentioned triage: %s", out)
	}
	if h.dec.count() != hits {
		t.Fatal("a private task's text reached the triage model at spawn")
	}

	wt := filepath.Join(filepath.Dir(h.dir("p")), "worktrees", "p")
	h.gitIn(h.repo, "worktree", "add", "-q", "-b", "orch/p", wt, "main")
	for _, bad := range []string{"main@--output=/tmp/x", "main@HEAD", "main@zzzzzzz"} {
		setFrontMatter(filepath.Join(h.dir("p"), "task.md"), "base", bad)
		if code, out := h.run("verify", "p"); code != 1 || !strings.Contains(out, "is not a commit sha") {
			t.Fatalf("verify accepted base %q: %d %s", bad, code, out)
		}
	}
}
