package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// asLead makes later orch calls run as the shipped lead of id.
func asLead(t *testing.T, h *harness, id string) {
	t.Helper()
	l := h.lead(id)
	t.Setenv("ORCH_ROLE", "lead")
	t.Setenv("ORCH_TASK_DIR", h.dir(id))
	t.Setenv("ORCH_EPOCH", l.Epoch)
}

func asOrchestrator(t *testing.T) {
	t.Helper()
	t.Setenv("ORCH_ROLE", "")
	t.Setenv("ORCH_TASK_DIR", "")
	t.Setenv("ORCH_EPOCH", "")
}

// ownedLead spawns t1 owning two paths and runs later calls as that lead.
func ownedLead(t *testing.T, h *harness) {
	t.Helper()
	h.task("t1")
	p := filepath.Join(h.dir("t1"), "task.md")
	b := mustRead(t, p)
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "id: t1\n", "id: t1\nowns: pkg/a.go, docs/b.md\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	h.must("spawn", "t1")
	asLead(t, h, "t1")
}

func subStart(h *harness, name string) (int, string) {
	return h.run("sub", "start", name, "--harness", "claude", "--model", "sonnet", "look at the parser")
}

func TestSubStartRefusals(t *testing.T) {
	h := setup(t)
	ownedLead(t, h)

	if code, out := h.run("sub", "start", "Bad_Name", "--harness", "claude", "--model", "sonnet", "x"); code != 1 || !strings.Contains(out, "must match") {
		t.Fatalf("bad name = %d %q", code, out)
	}
	if code, out := h.run("sub", "start", "a-name-too-long-x", "--harness", "claude", "--model", "sonnet", "x"); code != 1 || !strings.Contains(out, "must match") {
		t.Fatalf("long name = %d %q", code, out)
	}
	if code, out := h.run("sub", "start", "w", "--harness", "claude", "--model", "sonnet", "x"); code != 0 {
		t.Fatalf("a valid name was refused: %q", out)
	}
	if code, out := subStart(h, "w"); code != 1 || !strings.Contains(out, "already open") {
		t.Fatalf("duplicate = %d %q", code, out)
	}
	for _, n := range []string{"b", "c"} {
		if code, out := subStart(h, n); code != 0 {
			t.Fatalf("sub %s: %q", n, out)
		}
	}
	if n := len(openSubs(h.lead("t1"))); n != 3 {
		t.Fatalf("open subs = %d", n)
	}

	t.Setenv("ORCH_EPOCH", "stale")
	if code, out := subStart(h, "e"); code != 1 || !strings.Contains(out, "replaced by a respawn") {
		t.Fatalf("stale epoch = %d %q", code, out)
	}
	asOrchestrator(t)
	if code, out := subStart(h, "e"); code != 1 || !strings.Contains(out, "inside a lead's pane") {
		t.Fatalf("outside a lead = %d %q", code, out)
	}
}

func TestSubStartRefusesScoutAndUnusableModel(t *testing.T) {
	h := setup(t)
	h.task("s1", "--kind", "scout")
	h.must("spawn", "s1")
	asLead(t, h, "s1")
	if code, out := subStart(h, "w"); code != 1 || !strings.Contains(out, "managed ship worktree") {
		t.Fatalf("scout = %d %q", code, out)
	}

	h2 := setup(t)
	ownedLead(t, h2)
	h2.must("health", "set", "claude", "limited", "--for", "1h")
	if code, out := subStart(h2, "w"); code != 1 || len(h2.herdrCalls("pane", "split")) != 1 {
		t.Fatalf("limited harness = %d %q (splits %d)", code, out, len(h2.herdrCalls("pane", "split")))
	}
}

func TestSubStartPlacesHelperInTheLeadsCheckout(t *testing.T) {
	h := setup(t)
	ownedLead(t, h)
	leadPane := h.lead("t1").PaneID
	out := h.must("sub", "start", "first", "--harness", "claude", "--model", "sonnet", "look at the parser")
	if strings.TrimSpace(out) != "sub-t1-first" {
		t.Fatalf("printed %q", out)
	}
	splits := h.herdrCalls("pane", "split")
	sp := splits[len(splits)-1]
	if sp[2] != leadPane || flagValue(sp, "--direction") != "right" || flagValue(sp, "--cwd") != h.wt("t1") {
		t.Fatalf("first split = %v", sp)
	}
	if !slices.Contains(sp, "ORCH_ROLE=sub") || !slices.Contains(sp, "ORCH_PARENT=t1") {
		t.Fatalf("split env = %v", sp)
	}
	for _, a := range sp {
		if strings.HasPrefix(a, "ORCH_TASK_DIR") || strings.HasPrefix(a, "ORCH_EPOCH") {
			t.Fatalf("helper got %s", a)
		}
	}
	subs := h.lead("t1").Subs
	if len(subs) != 1 || subs[0].Name != "first" || subs[0].Harness != "claude" || subs[0].PaneID == "" || subs[0].StartedAt == 0 {
		t.Fatalf("subs = %+v", subs)
	}
	if len(h.herdrCalls("agent", "start", "sub-t1-first", "--kind", "claude", "--pane", subs[0].PaneID)) != 1 {
		t.Fatalf("agent not started: %v", h.fakeState().Calls)
	}
	var prompt string
	for _, c := range h.herdrCalls("agent", "prompt", subs[0].PaneID) {
		prompt = c[3]
	}
	wantContains(t, prompt, "helper first for lead t1", h.wt("t1"), "pkg/a.go, docs/b.md", "look at the parser", "Do not commit")

	h.must("sub", "start", "second", "--harness", "claude", "--model", "sonnet", "x")
	splits = h.herdrCalls("pane", "split")
	sp = splits[len(splits)-1]
	if sp[2] != subs[0].PaneID || flagValue(sp, "--direction") != "down" {
		t.Fatalf("second split = %v", sp)
	}
}

func TestSubListReadAndClose(t *testing.T) {
	h := setup(t)
	ownedLead(t, h)
	h.must("sub", "start", "one", "--harness", "claude", "--model", "sonnet", "x")
	h.must("sub", "start", "two", "--harness", "claude", "--model", "sonnet", "x")
	pane := h.lead("t1").Subs[0].PaneID

	out := h.must("sub", "list")
	wantContains(t, out, "one  claude/sonnet  working  "+pane, "two  claude/sonnet")
	if got := h.must("sub", "read", "one", "--lines", "3"); got != "line 248\nline 249\nline 250\n" {
		t.Fatalf("read = %q", got)
	}
	if reads := h.herdrCalls("agent", "read", "sub-t1-one"); len(reads) != 1 {
		t.Fatalf("read calls = %v", reads)
	}
	if code, _ := h.run("sub", "read", "nope"); code != 1 {
		t.Fatal("read of an unknown helper succeeded")
	}

	h.must("sub", "close", "one")
	if _, ok := h.fakeState().Panes[pane]; ok {
		t.Fatal("helper pane still open")
	}
	if s := h.lead("t1").Subs[0]; s.ClosedAt == 0 {
		t.Fatalf("not marked closed: %+v", s)
	}
	out = h.must("sub", "list")
	if strings.Contains(out, "one ") || !strings.Contains(out, "two ") {
		t.Fatalf("list = %q", out)
	}
	wantContains(t, h.must("sub", "list", "--all"), "one  claude/sonnet  closed")
	if code, _ := h.run("sub", "close", "one"); code != 1 {
		t.Fatal("closing a closed helper succeeded")
	}
}

func TestStatusShowsHelperCount(t *testing.T) {
	h := setup(t)
	ownedLead(t, h)
	h.must("sub", "start", "one", "--harness", "claude", "--model", "sonnet", "x")
	h.must("sub", "start", "two", "--harness", "claude", "--model", "sonnet", "x")
	asOrchestrator(t)
	wantContains(t, h.must("status"), "claude/sonnet+2")
	wantContains(t, h.must("status", "--json"), `"subs": 2`, `"name": "one"`)
}

func TestCloseAndRespawnCloseHelpersFirst(t *testing.T) {
	for _, via := range []string{"close", "respawn"} {
		t.Run(via, func(t *testing.T) {
			h := setup(t)
			ownedLead(t, h)
			h.must("sub", "start", "one", "--harness", "claude", "--model", "sonnet", "x")
			h.must("sub", "start", "two", "--harness", "claude", "--model", "sonnet", "x")
			old := h.lead("t1")
			p1, p2 := old.Subs[0].PaneID, old.Subs[1].PaneID
			// helper two is already gone: it is only marked closed
			h.editFake(func(st *fakeState) { delete(st.Panes, p2); delete(st.Agents, p2) })
			asOrchestrator(t)
			if via == "close" {
				h.must("report", "--dir", h.dir("t1"), "done", "branch orch/t1")
				h.must("close", "t1")
				if _, err := os.Stat(h.wt("t1")); err == nil {
					t.Fatal("clean merged worktree was not removed")
				}
			} else {
				os.WriteFile(filepath.Join(h.dir("t1"), "handoff.md"), []byte("continue\n"), 0o644)
				h.must("respawn", "t1")
			}
			closes := h.herdrCalls("pane", "close")
			var order []string
			for _, c := range closes {
				if !strings.HasSuffix(c[2], ":p0") { // the workspace root pane spawn closes
					order = append(order, c[2])
				}
			}
			if len(order) < 2 || order[0] != p1 || order[1] != old.PaneID || slices.Contains(order, p2) {
				t.Fatalf("pane close order = %v, want %s then %s", order, p1, old.PaneID)
			}
			log := string(mustRead(t, filepath.Join(h.dir("t1"), "status.log")))
			wantContains(t, log, "closed 2 helper panes")
			if via == "close" {
				for _, s := range h.lead("t1").Subs {
					if s.ClosedAt == 0 {
						t.Fatalf("helper %s left open", s.Name)
					}
				}
			} else if n := len(h.lead("t1").Subs); n != 0 {
				t.Fatalf("respawned lead inherited %d helpers", n)
			}
		})
	}
}

func TestSubOverflowTabsAndSlotReuse(t *testing.T) {
	h := setup(t)
	ownedLead(t, h)
	start := func(n string) subAgent {
		h.must("sub", "start", n, "--harness", "claude", "--model", "sonnet", "x")
		subs := h.lead("t1").Subs
		return subs[len(subs)-1]
	}
	lastSplit := func() []string {
		s := h.herdrCalls("pane", "split")
		return s[len(s)-1]
	}
	leadTab := h.lead("t1").TabID
	s1, s2, s3 := start("a"), start("b"), start("c")
	for _, s := range []subAgent{s1, s2, s3} {
		if s.TabID != leadTab {
			t.Fatalf("helper %s in tab %q, want the lead's %q", s.Name, s.TabID, leadTab)
		}
	}
	if n := len(h.herdrCalls("tab", "create")); n != 0 {
		t.Fatalf("tab create before the 4th helper: %d", n)
	}

	s4 := start("d")
	creates := h.herdrCalls("tab", "create")
	if len(creates) != 1 {
		t.Fatalf("tab create calls = %v", creates)
	}
	c := creates[0]
	if flagValue(c, "--workspace") != "wt1" || flagValue(c, "--label") != "t1-helpers" || flagValue(c, "--cwd") != h.wt("t1") ||
		!slices.Contains(c, "ORCH_ROLE=sub") || !slices.Contains(c, "ORCH_PARENT=t1") {
		t.Fatalf("tab create = %v", c)
	}
	for _, a := range c {
		if strings.HasPrefix(a, "ORCH_TASK_DIR") || strings.HasPrefix(a, "ORCH_EPOCH") {
			t.Fatalf("overflow tab got %s", a)
		}
	}
	if s4.TabID == "" || s4.TabID == leadTab {
		t.Fatalf("4th helper tab = %q", s4.TabID)
	}
	if len(h.herdrCalls("agent", "start", "sub-t1-d", "--kind", "claude", "--pane", s4.PaneID)) != 1 {
		t.Fatal("4th helper not started in the new tab's root pane")
	}

	s5 := start("e")
	if len(h.herdrCalls("tab", "create")) != 1 {
		t.Fatal("5th helper opened another tab")
	}
	if sp := lastSplit(); sp[2] != s4.PaneID || flagValue(sp, "--direction") != "right" || s5.TabID != s4.TabID {
		t.Fatalf("5th split = %v (tab %s)", sp, s5.TabID)
	}

	// a closed helper frees its slot in the lead's tab
	h.must("sub", "close", "b")
	s6 := start("f")
	if sp := lastSplit(); sp[2] != s3.PaneID || flagValue(sp, "--direction") != "down" || s6.TabID != leadTab {
		t.Fatalf("reused slot split = %v (tab %s)", sp, s6.TabID)
	}
	if len(h.herdrCalls("tab", "create")) != 1 {
		t.Fatal("a free slot still opened a tab")
	}
}

func TestSubFallsBackWhenSplitTargetVanished(t *testing.T) {
	h := setup(t)
	ownedLead(t, h)
	h.must("sub", "start", "a", "--harness", "claude", "--model", "sonnet", "x")
	gone := h.lead("t1").Subs[0].PaneID
	h.editFake(func(st *fakeState) { delete(st.Panes, gone); delete(st.Agents, gone) })
	h.must("sub", "start", "b", "--harness", "claude", "--model", "sonnet", "x")
	splits := h.herdrCalls("pane", "split")
	sp := splits[len(splits)-1]
	if sp[2] != h.lead("t1").PaneID || flagValue(sp, "--direction") != "right" {
		t.Fatalf("fallback split = %v", sp)
	}
	if len(h.lead("t1").Subs) != 2 {
		t.Fatalf("subs = %+v", h.lead("t1").Subs)
	}
}
