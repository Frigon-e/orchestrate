package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const staleAnswer = `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"old"}]}}` + "\n"

// fakeAgentAfter points ORCH_HERDR at a script whose `agent get` reports
// agent_not_found for the first misses calls (never, when misses < 0) and a working agent after.
func fakeAgentAfter(t *testing.T, misses int) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "herdr")
	body := "#!/bin/sh\nn=$(cat '" + dir + "/n' 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > '" + dir + "/n'\n" +
		"if [ " + strconv.Itoa(misses) + " -lt 0 ] || [ $n -le " + strconv.Itoa(misses) + " ]; then echo '{\"error\":{\"code\":\"agent_not_found\"}}' >&2; exit 1; fi\n" +
		"echo '{\"result\":{\"agent\":{\"agent_status\":\"working\"}}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_HERDR", script)
	t.Setenv("HERDR_ENV", "1") // the backend is selected from the env, not assumed
}

func staleSession(t *testing.T) string {
	t.Helper()
	session := t.TempDir()
	if err := os.WriteFile(filepath.Join(session, "old.jsonl"), []byte(staleAnswer), 0o644); err != nil {
		t.Fatal(err)
	}
	return session
}

func TestWaitPaneTurnIgnoresSessionFilesFromAnEarlierRun(t *testing.T) {
	oldPoll := gateLeavePoll
	gateLeavePoll = time.Millisecond
	t.Cleanup(func() { gateLeavePoll = oldPoll })
	session := staleSession(t)
	fakeAgentAfter(t, 3)
	// The agent only appears on the 4th `agent get`: returning nil earlier
	// means the stale answer was taken for this run's.
	if err := waitPaneTurn(context.Background(), "p1", session, "", sessionFiles(session)); err != nil {
		t.Fatal(err)
	}
	n, _ := os.ReadFile(filepath.Join(filepath.Dir(os.Getenv("ORCH_HERDR")), "n"))
	if strings.TrimSpace(string(n)) != "4" {
		t.Fatalf("agent get calls = %s, want 4", n)
	}
}

func TestWaitPaneTurnWithStaleSessionStillFailsWhenNoAgentAppears(t *testing.T) {
	oldPoll, oldWait := gateLeavePoll, gateStartWait
	gateLeavePoll, gateStartWait = time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { gateLeavePoll, gateStartWait = oldPoll, oldWait })
	session := staleSession(t)
	fakeAgentAfter(t, -1)
	err := waitPaneTurn(context.Background(), "p1", session, "", sessionFiles(session))
	if err == nil || !strings.Contains(err.Error(), "saw no agent") {
		t.Fatalf("err = %v, want saw no agent", err)
	}
}

func TestPaneAnsweredCountsOnlyFilesNotInTheSnapshot(t *testing.T) {
	session := staleSession(t)
	old := sessionFiles(session)
	if paneAnswered(session, "", old) {
		t.Fatal("stale file counted as an answer")
	}
	if !paneAnswered(session, "", nil) {
		t.Fatal("without a snapshot the file is an answer")
	}
	if err := os.WriteFile(filepath.Join(session, "new.jsonl"), []byte(staleAnswer), 0o644); err != nil {
		t.Fatal(err)
	}
	if !paneAnswered(session, "", old) {
		t.Fatal("a new file with an assistant message must count")
	}
}

func TestPassSessionDirClearsStaleFilesOnlyOnTheFirstInvocation(t *testing.T) {
	gdir := t.TempDir()
	dir := filepath.Join(gdir, "sessions", "group-api")
	stale := filepath.Join(dir, "old.jsonl")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(stale, []byte(staleAnswer), 0o644)
	if got := passSessionDir(gdir, "group:api", 1); got != dir || exists(stale) {
		t.Fatalf("first invocation = %s, stale exists %v", got, exists(stale))
	}
	os.MkdirAll(dir, 0o755)
	os.WriteFile(stale, []byte(staleAnswer), 0o644)
	if got := passSessionDir(gdir, "group:api", 2); got != filepath.Join(dir, "attempt-2") || !exists(stale) {
		t.Fatalf("second invocation = %s, stale exists %v", got, exists(stale))
	}
}
