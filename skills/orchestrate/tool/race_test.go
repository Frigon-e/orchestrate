package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// failHerdr puts a wrapper in front of the fake herdr that fails every call
// whose first two words are in cmds, once the returned flag file exists.
func failHerdr(t *testing.T, h *harness, cmds ...string) (flag string) {
	t.Helper()
	flag = filepath.Join(h.tmp, "herdr-fail.flag")
	var cases []string
	for _, c := range cmds {
		cases = append(cases, `"`+c+`"`)
	}
	script := filepath.Join(h.tmp, "herdr-fail.sh")
	body := "#!/bin/sh\nif [ -e " + flag + " ]; then\n  case \"$1 $2\" in\n    " + strings.Join(cases, "|") +
		")\n      echo '{\"error\":{\"code\":\"boom\",\"message\":\"injected failure\"}}' >&2\n      exit 1;;\n  esac\nfi\nexec " + os.Args[0] + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_HERDR", script)
	return flag
}

// runConcurrently runs each command line on its own app, all released at once,
// and returns exit codes and output in order.
func (h *harness) runConcurrently(cmds ...[]string) ([]int, []string) {
	codes, outs := make([]int, len(cmds)), make([]string, len(cmds))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, args := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			a := &app{cwd: h.repo, stdin: &bytes.Buffer{}, out: &out, errw: &out, now: time.Now}
			<-start
			codes[i] = a.run(args)
			outs[i] = out.String()
		}()
	}
	close(start)
	wg.Wait()
	return codes, outs
}

func TestConcurrentSendsKeepEveryMessage(t *testing.T) {
	h := setup(t)
	h.task("t1")
	if err := saveLead(h.dir("t1"), &lead{ID: "t1", ClosedAt: 1}); err != nil {
		t.Fatal(err)
	}
	const count = 32
	cmds := make([][]string, count)
	for i := range cmds {
		cmds[i] = []string{"send", "t1", fmt.Sprintf("unique-message-%02d", i)}
	}
	codes, outs := h.runConcurrently(cmds...)
	for i, code := range codes {
		if code != 0 {
			t.Fatalf("send %d failed: %s", i, outs[i])
		}
	}
	msgs := pendingMessages(h.dir("t1"))
	if len(msgs) != count {
		t.Fatalf("retained %d of %d messages", len(msgs), count)
	}
	seen := map[string]int{}
	for _, path := range msgs {
		body := string(mustRead(t, path))
		_, payload, ok := strings.Cut(body, "\n\n")
		if !ok {
			t.Fatalf("message has no body: %q", body)
		}
		seen[strings.TrimSpace(payload)]++
	}
	for _, cmd := range cmds {
		if n := seen[cmd[2]]; n != 1 {
			t.Errorf("payload %q appeared %d times", cmd[2], n)
		}
	}
}

func TestConcurrentSpawnsRespectMaxLeads(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("max_leads = 1\n"), 0o644)
	h.task("a")
	h.task("b")
	codes, outs := h.runConcurrently([]string{"spawn", "a"}, []string{"spawn", "b"})
	if !(codes[0] == 0 && codes[1] == 75) && !(codes[0] == 75 && codes[1] == 0) {
		t.Fatalf("exit codes = %v, want one 0 and one 75:\n%s\n%s", codes, outs[0], outs[1])
	}
	for _, o := range outs {
		if o != "" && !strings.Contains(o, "spawned") && !strings.Contains(o, "deferred: 1/1 leads open") {
			t.Fatalf("unexpected output:\n%s", o)
		}
	}
	if n := len(openLeads(filepath.Dir(h.dir("a")))); n != 1 {
		t.Fatalf("%d leads open with max_leads = 1", n)
	}
	if n := len(h.fakeState().Panes); n != 1 {
		t.Fatalf("%d panes open with max_leads = 1", n)
	}
}

func TestConcurrentSpawnsOfOneTaskOpenOneLead(t *testing.T) {
	h := setup(t)
	h.task("t1")
	codes, outs := h.runConcurrently([]string{"spawn", "t1"}, []string{"spawn", "t1"})
	ok, lost := 0, 0
	for i, c := range codes {
		switch {
		case c == 0:
			ok++
		case c == 1 && strings.Contains(outs[i], "is already open in pane"):
			lost++
		}
	}
	if ok != 1 || lost != 1 {
		t.Fatalf("exit codes = %v:\n%s\n%s", codes, outs[0], outs[1])
	}
	if n := len(h.fakeState().Panes); n != 1 {
		t.Fatalf("%d panes for one task, the second spawn left an untracked pane", n)
	}
	if _, ok := h.fakeState().Panes[h.lead("t1").PaneID]; !ok {
		t.Fatal("the lead record points at a pane that is not open")
	}
}

func TestSpawnPromptFailureLeavesRecoverableLead(t *testing.T) {
	h := setup(t)
	h.task("t1")
	flag := failHerdr(t, h, "agent prompt")
	os.WriteFile(flag, nil, 0o644)
	code, out := h.run("spawn", "t1")
	if code != 1 {
		t.Fatalf("spawn with a failing prompt = %d %s", code, out)
	}
	wantContains(t, out, "launch prompt failed", "orch peek t1", "orch close t1 --force")
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("t1"), "status.log"))), "agent prompt failed in pane wt1:p1")
	l := h.lead("t1")
	if _, ok := h.fakeState().Panes[l.PaneID]; !ok || l.ClosedAt != 0 {
		t.Fatalf("failed prompt left no recoverable open lead: %+v", l)
	}
}

func TestRespawnRefusedLeavesLeadAndFilesAlone(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	d := h.dir("t1")
	handoff := filepath.Join(d, "handoff.md")
	os.WriteFile(handoff, []byte("next: the settings page\n"), 0o644)
	before := mustRead(t, filepath.Join(d, "task.md"))
	h.must("hold", "on", "maintenance")

	for _, args := range [][]string{{"respawn", "t1"}, {"respawn", "t1", "--harness", "pi", "--model", "flash"}} {
		code, out := h.run(args...)
		if code != 1 || !strings.Contains(out, "spawning is on hold: maintenance") {
			t.Fatalf("%v under a hold = %d %s", args, code, out)
		}
		if l := h.lead("t1"); l.ClosedAt != 0 {
			t.Fatalf("%v closed the lead before the replacement was checked", args)
		}
		if _, ok := h.fakeState().Panes["wt1:p1"]; !ok {
			t.Fatalf("%v closed the old pane", args)
		}
		if _, err := os.Stat(handoff); err != nil {
			t.Fatalf("%v moved handoff.md: %v", args, err)
		}
		if after := mustRead(t, filepath.Join(d, "task.md")); !bytes.Equal(before, after) {
			t.Fatalf("%v rewrote task.md:\n%s", args, after)
		}
	}

	// A limited target harness is refused the same way, before anything moves.
	h.must("hold", "off")
	h.must("health", "set", "pi", "limited")
	if code, out := h.run("respawn", "t1", "--harness", "pi", "--model", "flash"); code != 1 || !strings.Contains(out, "pi is limited") {
		t.Fatalf("respawn onto a limited harness = %d %s", code, out)
	}
	if _, err := os.Stat(handoff); err != nil || h.lead("t1").ClosedAt != 0 {
		t.Fatal("a refused respawn changed the lead or its handoff")
	}
}

func TestRespawnRestoresHandoffAndProfileWhenNoPaneOpens(t *testing.T) {
	h := setup(t)
	h.task("t1")
	flag := failHerdr(t, h, "pane split", "tab create")
	h.must("spawn", "t1")
	d := h.dir("t1")
	handoff := filepath.Join(d, "handoff.md")
	os.WriteFile(handoff, []byte("next: the settings page\n"), 0o644)
	before := mustRead(t, filepath.Join(d, "task.md"))

	os.WriteFile(flag, nil, 0o644)
	code, out := h.run("respawn", "t1", "--harness", "pi", "--model", "flash")
	if code != 1 || !strings.Contains(out, "the new lead did not start") {
		t.Fatalf("respawn with no pane = %d %s", code, out)
	}
	if _, err := os.Stat(handoff); err != nil {
		t.Fatalf("handoff.md not restored: %v", err)
	}
	if kept, _ := filepath.Glob(filepath.Join(d, "handoff-*.md")); len(kept) != 0 {
		t.Fatalf("renamed handoff left behind: %v", kept)
	}
	if after := mustRead(t, filepath.Join(d, "task.md")); !bytes.Equal(before, after) {
		t.Fatalf("task.md profile not restored:\n%s", after)
	}
}

func TestSecondWatcherCannotRun(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	h.must("report", "--dir", h.dir("t1"), "failed", "tests do not build")
	root := filepath.Dir(h.dir("t1"))

	// Another watcher holds the lock: this one must not run or move the cursor.
	f, err := os.OpenFile(filepath.Join(root, "watch-w1-p0.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	code, out := h.run("watch", "--once")
	if code != 1 || !strings.Contains(out, "a watcher is already running") || strings.Contains(out, "tests do not build") {
		t.Fatalf("watch while the lock is held = %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "watch-w1-p0.json")); !os.IsNotExist(err) {
		t.Fatal("a watcher that lost the lock wrote the cursor")
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	// With the lock free, two watchers started together deliver the event once.
	_, outs := h.runConcurrently(
		[]string{"watch", "--once"},
		[]string{"watch", "--once"})
	if n := strings.Count(outs[0]+outs[1], "orch: t1 failed: tests do not build"); n != 1 {
		t.Fatalf("event delivered %d times:\n%s\n%s", n, outs[0], outs[1])
	}
}

func TestAutoDryRunWritesNothing(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(autoConfig), 0o644)
	h.task("a", "--harness", "auto", "--model", "auto", "--effort", "auto")
	before := mustRead(t, filepath.Join(h.dir("a"), "task.md"))
	h.dec.set(200, decReply(settledAnswers))
	wantContains(t, h.must("spawn", "a", "--auto"), "would use: pi flash", "dry run: nothing changed")
	if after := mustRead(t, filepath.Join(h.dir("a"), "task.md")); !bytes.Equal(before, after) {
		t.Fatalf("the dry run rewrote task.md:\n%s", after)
	}
	if recs := readTriage(filepath.Dir(h.dir("a"))); len(recs) != 0 {
		t.Fatalf("the dry run wrote triage records: %v", recs)
	}
}

func TestAutoApplyRefusedStaysRetryable(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(strings.Replace(autoConfig, "max_leads = 8", "max_leads = 1", 1)), 0o644)
	h.task("full")
	h.must("spawn", "full")
	h.task("a", "--harness", "auto", "--model", "auto", "--effort", "auto")
	h.dec.set(200, decReply(settledAnswers))

	// The cap is only known at spawn time, after the profile was written.
	code, out := h.run("spawn", "a", "--auto", "--apply")
	if code != 75 || !strings.Contains(out, "deferred: 1/1 leads open") {
		t.Fatalf("apply past the cap = %d %s", code, out)
	}
	fm, _, _ := readTask(filepath.Join(h.dir("a"), "task.md"))
	if fm["harness"] != "auto" || fm["model"] != "auto" || fm["effort"] != "auto" {
		t.Fatalf("a refused apply left the profile %v", fm)
	}
	h.must("close", "full", "--force")
	wantContains(t, h.must("spawn", "a", "--auto", "--apply"), "spawned lead-a (pi flash)")

	// A hold is refused before the profile is written at all.
	h.task("b", "--harness", "auto", "--model", "auto", "--effort", "auto")
	h.must("hold", "on", "maintenance")
	if code, out := h.run("spawn", "b", "--auto", "--apply"); code != 1 || !strings.Contains(out, "spawning is on hold") {
		t.Fatalf("apply under a hold = %d %s", code, out)
	}
	if fm, _, _ := readTask(filepath.Join(h.dir("b"), "task.md")); fm["harness"] != "auto" {
		t.Fatalf("apply under a hold left the profile %v", fm)
	}
}

func TestAutoApplyKeepsTheProfileOfAnySurvivingLead(t *testing.T) {
	for _, promptFails := range []bool{false, true} {
		name := "pane creation fails"
		cmds := []string{"pane split", "tab create"}
		if promptFails {
			name, cmds = "prompt fails after the lead opens", []string{"agent prompt"}
		}
		t.Run(name, func(t *testing.T) {
			h := setup(t)
			writeFiles(t, filepath.Dir(os.Getenv("ORCH_CONFIG")), map[string]string{filepath.Base(os.Getenv("ORCH_CONFIG")): autoConfig})
			h.task("a", "--harness", "auto", "--model", "auto", "--effort", "auto")
			flag := failHerdr(t, h, cmds...)
			writeFiles(t, filepath.Dir(flag), map[string]string{filepath.Base(flag): ""})
			code, out := h.run("spawn", "a", "--auto", "--apply")
			if code != 1 || !strings.Contains(out, "using: pi flash") || !strings.Contains(out, "injected failure") {
				t.Fatalf("auto launch failure = %d %s", code, out)
			}
			fm, _, err := readTask(filepath.Join(h.dir("a"), "task.md"))
			if err != nil {
				t.Fatal(err)
			}
			l, err := loadLead(h.dir("a"))
			if err != nil {
				t.Fatal(err)
			}
			if promptFails {
				if l == nil || l.ClosedAt != 0 || fm["harness"] != "pi" || fm["model"] != "flash" || fm["effort"] != "low" {
					t.Fatalf("surviving lead lost its selected profile: lead %+v, task %v", l, fm)
				}
				if _, ok := h.fakeState().Panes[l.PaneID]; !ok {
					t.Fatal("surviving lead points at no open pane")
				}
				return
			}
			if l != nil && l.ClosedAt == 0 {
				t.Fatalf("pane creation failure left an open lead: %+v", l)
			}
			if fm["harness"] != "auto" || fm["model"] != "auto" || fm["effort"] != "auto" {
				t.Fatalf("launch failure did not restore auto profile: %v", fm)
			}
			if err := os.Remove(flag); err != nil {
				t.Fatal(err)
			}
			wantContains(t, h.must("spawn", "a", "--auto", "--apply"), "spawned lead-a (pi flash)")
		})
	}
}

func TestRespawnCanBeRetriedAfterTheReplacementFailedToStart(t *testing.T) {
	h := setup(t)
	h.task("t1")
	flag := failHerdr(t, h, "pane split", "tab create")
	h.must("spawn", "t1")
	d := h.dir("t1")
	os.WriteFile(filepath.Join(d, "handoff.md"), []byte("next: the settings page\n"), 0o644)

	os.WriteFile(flag, nil, 0o644)
	code, out := h.run("respawn", "t1")
	if code != 1 || !strings.Contains(out, "run `orch respawn t1` again") {
		t.Fatalf("respawn with no pane = %d %s", code, out)
	}

	os.Remove(flag)
	wantContains(t, h.must("respawn", "t1"), "spawned lead-t1")
	l, err := loadLead(d)
	if err != nil || l == nil || l.ClosedAt != 0 {
		t.Fatalf("no open lead after the retry: %+v %v", l, err)
	}
	if kept, _ := filepath.Glob(filepath.Join(d, "handoff-*.md")); len(kept) != 1 || exists(filepath.Join(d, "handoff.md")) {
		t.Fatalf("the retry did not carry the handoff: %v", kept)
	}

	// A closed lead with no handoff is still not respawnable.
	h.must("close", "t1", "--force")
	if code, out := h.run("respawn", "t1"); code != 1 || !strings.Contains(out, "no open lead for t1") {
		t.Fatalf("respawn of a closed lead without a handoff = %d %s", code, out)
	}
}
