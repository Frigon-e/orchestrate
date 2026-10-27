package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeSource is a source tree with a build script that records each run.
func fakeSource(t *testing.T) (root, marks string) {
	root = t.TempDir()
	marks = filepath.Join(root, "marks")
	os.MkdirAll(marks, 0o755)
	for name, body := range map[string]string{
		"tool/go.mod":              "module x\n",
		"tool/guard/a.go":          "package guard\n",
		"tool/guard/a_test.go":     "package guard\n",
		"tool/cmd/orch-guard/m.go": "package main\n",
		"scripts/build-guard":      "#!/bin/sh\ntouch \"" + marks + "/ran.$$\"\n",
	} {
		path := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ORCH_TOOL_GUARD_STATE_DIR", filepath.Join(root, "state"))
	return root, marks
}

func runs(marks string) int {
	entries, _ := os.ReadDir(marks)
	return len(entries)
}

// waitFor polls a condition for up to ten seconds.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStaleBinaryStartsOneBackgroundRebuild(t *testing.T) {
	root, marks := fakeSource(t)
	CheckStale("an older fingerprint", root)
	waitFor(t, "the rebuild to run", func() bool { return runs(marks) == 1 })
}

func TestFreshBinaryStartsNoRebuild(t *testing.T) {
	root, marks := fakeSource(t)
	fp, err := Fingerprint(filepath.Join(root, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	CheckStale(fp, root)
	// A stale check after it proves the first did not queue a run: one run in total.
	CheckStale("older", root)
	waitFor(t, "the stale rebuild to run", func() bool { return runs(marks) >= 1 })
	if n := runs(marks); n != 1 {
		t.Errorf("%d rebuilds, want 1 (the stale check only)", n)
	}
}

func TestFingerprintIgnoresTestFilesAndChangesWithSource(t *testing.T) {
	root, _ := fakeSource(t)
	tool := filepath.Join(root, "tool")
	before, _ := Fingerprint(tool)
	os.WriteFile(filepath.Join(tool, "guard", "a_test.go"), []byte("package guard // changed\n"), 0o644)
	if after, _ := Fingerprint(tool); after != before {
		t.Error("a test file changed the fingerprint")
	}
	os.WriteFile(filepath.Join(tool, "guard", "a.go"), []byte("package guard // changed\n"), 0o644)
	if after, _ := Fingerprint(tool); after == before {
		t.Error("a source change did not change the fingerprint")
	}
}

func TestRebuildHeldByALockIsNotStartedTwice(t *testing.T) {
	root, marks := fakeSource(t)
	state := StateDir()
	os.MkdirAll(state, 0o755)
	lock, err := os.OpenFile(filepath.Join(state, "guard-build.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
	CheckStale("older", root)
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	CheckStale("older", root)
	waitFor(t, "the rebuild to run", func() bool { return runs(marks) >= 1 })
	if n := runs(marks); n != 1 {
		t.Errorf("%d rebuilds, want 1: the call under the lock must not have started one", n)
	}
}

func TestMissingSourceDirSkipsTheCheck(t *testing.T) {
	t.Setenv("ORCH_TOOL_GUARD_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	CheckStale("older", "")
	CheckStale("older", filepath.Join(t.TempDir(), "gone"))
	if _, err := os.Stat(StateDir()); err == nil {
		t.Error("a skipped check created the state dir")
	}
}

// build-guard (bash) and Fingerprint (Go) must agree: the script stamps the
// binary and the binary re-hashes its sources with the other.
func TestBuildGuardFingerprintMatchesTheLibrary(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command(filepath.Join(root, "scripts", "build-guard"), "--print-fingerprint").Output()
	if err != nil {
		t.Skipf("build-guard cannot run here: %v", err)
	}
	want, err := Fingerprint(filepath.Join(root, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("build-guard says %s, Fingerprint says %s", got, want)
	}
}

// failingBuild makes the fake build script record its run and fail.
func failingBuild(t *testing.T, root, marks string) {
	t.Helper()
	body := "#!/bin/sh\ntouch \"" + marks + "/ran.$$\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(root, "scripts", "build-guard"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// settled waits for any rebuild that is running: it holds the lock until it ends.
func settled(t *testing.T) {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(StateDir(), "guard-build.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
}

func TestFailedBuildIsNotRetriedForTheSameSourceFor10Minutes(t *testing.T) {
	root, marks := fakeSource(t)
	failingBuild(t, root, marks)
	failed := filepath.Join(StateDir(), "guard-build-failed")
	CheckStale("older", root)
	waitFor(t, "the failure to be recorded", func() bool { _, err := os.Stat(failed); return err == nil })
	fp, _ := Fingerprint(filepath.Join(root, "tool"))
	if data, _ := os.ReadFile(failed); !strings.HasPrefix(string(data), fp+" ") {
		t.Fatalf("failure record %q does not name the fingerprint %s", data, fp)
	}
	CheckStale("older", root)
	CheckStale("a third fingerprint of the same sources", root)
	settled(t)
	if n := runs(marks); n != 1 {
		t.Fatalf("%d builds, want 1: a failed build must not restart for the same sources", n)
	}

	// Changed sources are a new build, whatever failed before.
	os.WriteFile(filepath.Join(root, "tool", "guard", "a.go"), []byte("package guard // fixed\n"), 0o644)
	CheckStale("older", root)
	waitFor(t, "a build of the changed sources", func() bool { return runs(marks) == 2 })
	settled(t)

	// After ten minutes the same sources are tried again.
	fp, _ = Fingerprint(filepath.Join(root, "tool"))
	old := time.Now().Add(-11 * time.Minute).Unix()
	os.WriteFile(failed, []byte(fp+" "+itoa(int(old))+"\n"), 0o644)
	CheckStale("older", root)
	waitFor(t, "the retry after the backoff", func() bool { return runs(marks) == 3 })
	settled(t)
	recent := time.Now().Add(-9 * time.Minute).Unix()
	os.WriteFile(failed, []byte(fp+" "+itoa(int(recent))+"\n"), 0o644)
	CheckStale("older", root)
	settled(t)
	if n := runs(marks); n != 3 {
		t.Fatalf("%d builds, want 3: nine minutes is inside the backoff", n)
	}
}
