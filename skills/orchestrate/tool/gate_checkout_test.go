package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckoutFingerprint(t *testing.T) {
	for _, kind := range []string{"tracked", "untracked", "deleted", "dirty-before", "commit"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			file := filepath.Join(h.repo, "tracked.txt")
			os.WriteFile(file, []byte("before"), 0o644)
			h.git("add", "tracked.txt")
			h.git("commit", "-qm", "fixture")
			if kind == "dirty-before" {
				os.WriteFile(file, []byte("already dirty"), 0o644)
			}
			before, err := checkoutFingerprint(h.repo)
			if err != nil {
				t.Fatal(err)
			}
			same, err := checkoutFingerprint(h.repo)
			if err != nil || len(checkoutChanges(h.repo, before, same)) != 0 {
				t.Fatalf("unchanged checkout: %v", err)
			}
			path := "tracked.txt"
			switch kind {
			case "deleted":
				os.Remove(file)
			case "untracked":
				path = "new file.txt"
				os.WriteFile(filepath.Join(h.repo, path), []byte("new"), 0o644)
			default:
				os.WriteFile(file, []byte("after"), 0o644)
			}
			if kind == "commit" {
				h.git("add", "tracked.txt")
				h.git("commit", "-qm", "agent commit")
			}
			after, err := checkoutFingerprint(h.repo)
			if err != nil {
				t.Fatal(err)
			}
			if got := checkoutChanges(h.repo, before, after); len(got) != 1 || got[0] != path {
				t.Fatalf("changes: %v", got)
			}
		})
	}
}

func TestCheckoutUntrackedContentAndIgnoredLimit(t *testing.T) {
	h := setup(t)
	os.WriteFile(filepath.Join(h.repo, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	os.WriteFile(filepath.Join(h.repo, "new.txt"), []byte("one"), 0o644)
	before, err := checkoutFingerprint(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(h.repo, "new.txt"), []byte("two"), 0o644)
	after, err := checkoutFingerprint(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkoutChanges(h.repo, before, after); len(got) != 1 || got[0] != "new.txt" {
		t.Fatal(got)
	}
	os.WriteFile(filepath.Join(h.repo, "ignored.txt"), []byte("not covered"), 0o644)
	ignored, err := checkoutFingerprint(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkoutChanges(h.repo, after, ignored); len(got) != 0 {
		t.Fatalf("ignored file unexpectedly covered: %v", got)
	}
}

func TestGateCheckoutMutationDiscardsAndStops(t *testing.T) {
	// Representative cases only. Between them every harness, every change kind
	// and both ctx settings still appear at least once.
	for _, c := range []struct {
		harness string
		kind    string
		ctxOn   bool
	}{
		{"pi", "tracked", false},
		{"pi", "deleted", true},
		{"claude", "untracked", false},
		{"claude", "none", true},
	} {
		harness, kind, ctxOn := c.harness, c.kind, c.ctxOn
		t.Run(fmt.Sprintf("%s/%s/ctx-%v", harness, kind, ctxOn), func(t *testing.T) {
			h := setup(t)
			tracked := filepath.Join(h.repo, "tracked.txt")
			os.WriteFile(tracked, []byte("before"), 0o644)
			h.git("add", "tracked.txt")
			h.git("commit", "-qm", "fixture")
			mutation := ""
			switch kind {
			case "tracked":
				mutation = "printf changed > tracked.txt\n"
			case "untracked":
				mutation = "printf new > new.txt\n"
			case "deleted":
				mutation = "rm tracked.txt\n"
			}
			reply := "printf '{}'"
			if harness == "claude" {
				reply = `printf '%s\n' '{"type":"result","result":"{}"}'`
			}
			t.Setenv("ORCH_"+strings.ToUpper(harness), fakeRunner(t, harness, mutation+reply))
			rec := &gateRecord{PassErrors: map[string]string{}, PassSeconds: map[string]int{}, PassKept: map[string]string{}, PassTokens: map[string]passTokens{}}
			var applied atomic.Int32
			var passes []gatePass
			for i := range 12 {
				passes = append(passes, gatePass{name: fmt.Sprintf("p%d", i), class: "package", prompt: "review", apply: func(_ string, r *gateRecord, _ string, _ *spec) error {
					applied.Add(1)
					r.Findings = append(r.Findings, gateFinding{Claim: "must be discarded"})
					return nil
				}})
			}
			runner, _ := roleSite(profile{Harness: harness, Model: "test"}).newRoleRun("gate", modelOptions{})
			runGatePasses(filepath.Join(h.tmp, "gate"), h.repo, "test", passes, rec, nil, gateOpts{runner: runner, headless: true, ctxTool: ctxOn})
			if kind == "none" {
				if len(rec.TreeChanged) > 0 || applied.Load() != 12 || len(rec.PassErrors) > 0 {
					t.Fatalf("unchanged: %+v, applied %d", rec, applied.Load())
				}
				return
			}
			want := "tracked.txt"
			if kind == "untracked" {
				want = "new.txt"
			}
			if len(rec.TreeChanged) != 1 || rec.TreeChanged[0] != want || len(rec.TreeChangedPasses) == 0 || len(rec.Findings) != 0 || rec.State != "" || rec.PassErrors["all"] == "" {
				t.Fatalf("mutation not rejected: %+v", rec)
			}
			if len(rec.PassSeconds) >= 12 {
				t.Fatal("remaining passes were not canceled")
			}
			if kind == "deleted" {
				if exists(tracked) {
					t.Fatal("deletion was reverted")
				}
			} else if !exists(filepath.Join(h.repo, want)) {
				t.Fatal("write was reverted")
			}
		})
	}
}

func TestGatePaneCheckoutMutation(t *testing.T) {
	// Representative cases only. Both harnesses and both ctx settings stay
	// covered; the untracked kind is covered by the passes matrix above and by
	// TestCheckoutFingerprint.
	for _, c := range []struct {
		harness string
		kind    string
		ctxOn   bool
	}{
		{"pi", "deleted", false},
		{"claude", "tracked", true},
	} {
		harness, kind, ctxOn := c.harness, c.kind, c.ctxOn
		t.Run(fmt.Sprintf("%s/%s/ctx-%v", harness, kind, ctxOn), func(t *testing.T) {
			h := setup(t)
			gateWarmDelay = time.Minute // leave a third pass queued until the check cancels it
			gdir := h.gateRepo(1)
			t.Setenv("ORCH_FAKE_CLAUDE_REPO", h.repo)
			dir := h.fakePi(gatePasses(nil))
			path := filepath.Join(h.repo, "internal/store.go")
			if kind == "untracked" {
				path = filepath.Join(h.repo, "new.txt")
			}
			mutation := fmt.Sprintf("printf changed > %q\n", path)
			if kind == "deleted" {
				mutation = fmt.Sprintf("rm %q\n", path)
			}
			bin := filepath.Join(dir, "pi")
			original := mustRead(t, bin)
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+mutation+string(original)), 0o755); err != nil {
				t.Fatal(err)
			}
			code, out := h.run("gate", "spec.md", "--runner", harness+":test", "--ctx-tool", onOff(ctxOn))
			if code != 1 {
				t.Fatalf("%d %s", code, out)
			}
			view := wantGateState(t, out, "stale")
			rec := readRecord(t, gdir)
			if view.Warning != treeWarning(&rec) || !strings.HasPrefix(view.Warning, "UNVERIFIED: checkout changed:") {
				t.Fatalf("checkout warning missing or differs from recorded facts: %+v", view)
			}
			if rec.State != "stale" || len(rec.TreeChanged) != 1 || len(rec.Findings) > 0 || len(rec.TreeChangedPasses) == 0 {
				t.Fatalf("%+v", rec)
			}
			if len(h.gateCalls("agent", "start")) >= 3 {
				t.Fatal("queued pane pass was not canceled")
			}
			if kind == "deleted" && exists(path) || kind != "deleted" && !exists(path) {
				t.Fatal("mutation was reverted")
			}
		})
	}
}

func TestCheckoutConcurrentPassesCancel(t *testing.T) {
	h := setup(t)
	g, err := newCheckoutGuard(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer g.cancel()
	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.run("waiting", func() (string, string, error) { close(ready); <-g.ctx.Done(); return "", "", g.ctx.Err() })
	}()
	<-ready
	_, _, err = g.run("writer", func() (string, string, error) {
		os.WriteFile(filepath.Join(h.repo, "new.txt"), []byte("new"), 0o644)
		return "{}", "", nil
	})
	if err == nil {
		t.Fatal("writer accepted")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("in-flight pass not canceled")
	}
	rec := &gateRecord{PassErrors: map[string]string{}, Findings: []gateFinding{{Claim: "earlier result"}}}
	g.finish(rec)
	if strings.Join(rec.TreeChangedPasses, ",") != "waiting,writer" || len(rec.Findings) != 1 || len(rec.TreeChanged) != 1 || rec.TreeChanged[0] != "new.txt" || !strings.Contains(rec.PassErrors["all"], "checkout changed") {
		t.Fatalf("mutation did not withhold approval: %+v", rec)
	}
}

func TestCheckoutGuardDiscardsWhenVerificationFails(t *testing.T) {
	h := setup(t)
	g, err := newCheckoutGuard(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer g.cancel()
	out, _, err := g.run("uncheckable", func() (string, string, error) {
		if err := os.Rename(filepath.Join(h.repo, ".git"), filepath.Join(h.repo, "git-backup")); err != nil {
			t.Fatal(err)
		}
		return "must be discarded", "", nil
	})
	if err == nil || out != "" || g.ctx.Err() == nil {
		t.Fatalf("uncheckable result accepted: output %q, err %v", out, err)
	}
	_, _, err = g.run("queued", func() (string, string, error) {
		t.Fatal("a queued pass ran after checkout verification failed")
		return "", "", nil
	})
	if err == nil {
		t.Fatal("queued pass accepted")
	}
	rec := &gateRecord{PassErrors: map[string]string{}, Findings: []gateFinding{{Claim: "earlier result"}}}
	g.finish(rec)
	if rec.TreeCheckError == "" || len(rec.Findings) != 1 || len(rec.TreeChanged) != 0 || !strings.Contains(rec.PassErrors["all"], "could not be checked") {
		t.Fatalf("verification failure not recorded: %+v", rec)
	}
	if !strings.HasPrefix(treeWarning(rec), "UNVERIFIED: checkout could not be checked:") {
		t.Fatal(treeWarning(rec))
	}
}

func TestGateCheckoutWarningInV2AndFirstInReport(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	t.Setenv("HERDR_ENV", "")
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", `printf changed > agent.txt
printf '{}'`))
	code, out := h.run("gate", "spec.md", "--runner", "pi:test", "--ctx-tool", "off")
	if code != 1 {
		t.Fatalf("%d %s", code, out)
	}
	view := wantGateState(t, out, "stale")
	rec := readRecord(t, gdir)
	if view.Warning != treeWarning(&rec) || !strings.HasPrefix(view.Warning, "UNVERIFIED: checkout changed:") {
		t.Fatalf("checkout warning missing or differs from recorded facts: %+v", view)
	}
	if rec.State != "stale" || rec.CtxTool != "off" || len(rec.Findings) != 0 {
		t.Fatalf("%+v", rec)
	}
	report := string(mustRead(t, filepath.Join(gdir, "report.md")))
	if !strings.HasPrefix(report, "UNVERIFIED: checkout changed:") {
		t.Fatal(report)
	}
	var raw map[string]any
	json.Unmarshal(mustRead(t, filepath.Join(gdir, "gate.json")), &raw)
	if raw["tree_changed"] == nil || raw["tree_changed_passes"] == nil {
		t.Fatal(raw)
	}
}

func TestCheckoutUntrackedSymlinkAndDirectory(t *testing.T) {
	h := setup(t)
	os.WriteFile(filepath.Join(h.repo, "tracked.txt"), []byte("x"), 0o644)
	h.git("add", "tracked.txt")
	h.git("commit", "-qm", "fixture")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(h.repo, "node_modules")); err != nil {
		t.Fatal(err)
	}
	os.Symlink(filepath.Join(outside, "missing"), filepath.Join(h.repo, "dangling"))
	before, err := checkoutFingerprint(h.repo)
	if err != nil {
		t.Fatalf("a symlink in the checkout must not fail the check: %v", err)
	}
	same, err := checkoutFingerprint(h.repo)
	if err != nil || len(checkoutChanges(h.repo, before, same)) != 0 {
		t.Fatalf("unchanged checkout with links: %v", err)
	}
	os.Remove(filepath.Join(h.repo, "node_modules"))
	os.Symlink(t.TempDir(), filepath.Join(h.repo, "node_modules"))
	after, err := checkoutFingerprint(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if ch := checkoutChanges(h.repo, before, after); len(ch) != 1 || ch[0] != "node_modules" {
		t.Fatalf("a retargeted link must show as a change, got %v", ch)
	}
}
