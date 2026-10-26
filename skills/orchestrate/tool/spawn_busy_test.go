package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// busyHerdr puts a wrapper in front of the fake herdr that answers the first
// n `agent start` calls with agent_pane_busy, as Herdr does while the new
// pane's shell is still starting.
func busyHerdr(t *testing.T, h *harness, n int) {
	t.Helper()
	counter := filepath.Join(h.tmp, "busy-count")
	script := filepath.Join(h.tmp, "herdr-busy.sh")
	body := fmt.Sprintf(`#!/bin/sh
if [ "$1 $2" = "agent start" ]; then
  c=$(cat %[1]q 2>/dev/null || echo 0)
  echo $((c + 1)) > %[1]q
  if [ "$c" -lt %[2]d ]; then
    echo '{"error":{"code":"agent_pane_busy","message":"agent target pane '"$5"' is not an available shell"}}' >&2
    exit 1
  fi
fi
exec %[3]q "$@"
`, counter, n, os.Args[0])
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_HERDR", script)
	oldFor, oldBackoff := agentStartBusyFor, agentStartBackoff
	agentStartBusyFor, agentStartBackoff = 2*time.Second, 5*time.Millisecond
	t.Cleanup(func() { agentStartBusyFor, agentStartBackoff = oldFor, oldBackoff })
}

func TestSpawnRetriesWhilePaneBusy(t *testing.T) {
	h := setup(t)
	busyHerdr(t, h, 3)
	h.task("t1")
	out := h.must("spawn", "t1")
	wantContains(t, out, "spawned lead-t1")
	if strings.Contains(out, "not ready") {
		t.Fatalf("spawn reported not ready:\n%s", out)
	}
	if ag := h.fakeState().Agents[h.lead("t1").PaneID]; ag == nil || ag.Status != "working" {
		t.Fatalf("agent not launched and prompted: %+v", ag)
	}
}

func TestSpawnGivesUpWhenPaneStaysBusy(t *testing.T) {
	h := setup(t)
	busyHerdr(t, h, 1<<30)
	agentStartBusyFor = 100 * time.Millisecond
	h.task("t1")
	out := h.must("spawn", "t1")
	wantContains(t, out, "opened pane wt1:p1 for lead-t1, but the agent is not ready",
		"agent_pane_busy", "orch close t1 --force")
	if len(h.fakeState().Agents) != 0 {
		t.Fatal("agent must not be started")
	}
}
