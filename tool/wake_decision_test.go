package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func wakeAnswers() map[string]json.RawMessage {
	return answers("recovers", `{"type":"noul","noul":0.8}`, "needs_orchestrator", `{"type":"noul","noul":0.1}`)
}

func (h *harness) wakeRoot() string { return filepath.Dir(h.dir("t1")) }

// wakeClockAt makes the outcome resolver read the harness clock.
func (h *harness) wakeClockFollows() {
	old := wakeClock
	wakeClock = func() time.Time { return h.now }
	h.t.Cleanup(func() { wakeClock = old })
}

func (h *harness) wakeRecords() []decisionRecord {
	h.t.Helper()
	recs, err := readJSONLines[decisionRecord](decisionFile(h.wakeRoot(), wakeUse))
	if err != nil {
		h.t.Fatal(err)
	}
	return recs
}

func (h *harness) wakeOutcomes() []decisionOutcome {
	h.t.Helper()
	outs, err := readJSONLines[decisionOutcome](decisionOutcomeFile(h.wakeRoot(), wakeUse))
	if err != nil {
		h.t.Fatal(err)
	}
	return outs
}

// stallT1 spawns t1 and advances until the watcher raises its stalled alert.
func (h *harness) stallT1() (window int) {
	h.t.Helper()
	h.task("t1")
	h.must("spawn", "t1")
	h.must("watch", "--once")
	s, err := h.a.site()
	if err != nil {
		h.t.Fatal(err)
	}
	h.now = h.now.Add(time.Duration(s.cfg.StallMinutes+1) * time.Minute)
	wantContains(h.t, h.must("watch", "--once"), "orch: t1 stalled")
	return s.cfg.StallMinutes
}

func TestWakeStalledAlertRecordsMetaAndIdleTime(t *testing.T) {
	h := setup(t)
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return wakeAnswers(), shadowUsage{}, nil
	})
	window := h.stallT1()
	recs := h.wakeRecords()
	if len(recs) != 1 {
		t.Fatalf("wake records = %d, want 1", len(recs))
	}
	m := recs[0].Meta
	if recs[0].Subject != "t1" || recs[0].Action != "shadow" || recs[0].Error != "" {
		t.Fatalf("record = %+v", recs[0])
	}
	if int(m["window_minutes"].(float64)) != window || m["kind"] != "stalled" || m["fingerprint"] == "" {
		t.Fatalf("meta = %v", m)
	}
	if v, present := m["idle_seconds"]; !present || v != nil {
		t.Fatalf("idle_seconds before any flush = %v (present %v), want null", v, present)
	}
	// The stalled alert was flushed at h.now; the next alert comes 16 minutes later.
	h.must("report", "--dir", h.dir("t1"), "working", "back on it")
	h.must("watch", "--once")
	h.now = h.now.Add(16 * time.Minute)
	wantContains(t, h.must("watch", "--once"), "orch: t1 stalled")
	recs = h.wakeRecords()
	if len(recs) != 2 || recs[1].Meta["idle_seconds"].(float64) != 16*60 {
		t.Fatalf("second alert meta = %v", recs[len(recs)-1].Meta)
	}
}

func TestWakeQuietAlertUsesQuietMinutes(t *testing.T) {
	h := setup(t)
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return wakeAnswers(), shadowUsage{}, nil
	})
	h.task("t1")
	h.must("spawn", "t1")
	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Status = "idle" })
	h.must("watch", "--once")
	s, _ := h.a.site()
	h.now = h.now.Add(time.Duration(s.cfg.QuietMinutes+1) * time.Minute)
	wantContains(t, h.must("watch", "--once"), "orch: t1 quiet")
	recs := h.wakeRecords()
	if len(recs) != 1 || recs[0].Meta["kind"] != "quiet" || int(recs[0].Meta["window_minutes"].(float64)) != s.cfg.QuietMinutes {
		t.Fatalf("records = %+v", recs)
	}
	if s.cfg.QuietMinutes == s.cfg.StallMinutes {
		t.Fatal("quiet_minutes equals stall_minutes: the test cannot tell which window was used")
	}
}

func TestWakeHangingDeciderNeverDelaysTheAlert(t *testing.T) {
	h := setup(t)
	old := wakeDeadline
	wakeDeadline = 300 * time.Millisecond
	t.Cleanup(func() { wakeDeadline = old })
	fakeDecider(t, func(ctx context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		<-ctx.Done()
		return nil, shadowUsage{}, ctx.Err()
	})
	h.task("t1")
	h.must("spawn", "t1")
	s, _ := h.a.site()
	sink := newWakeSink(s, filepath.Join(s.root, "watch.json"))
	l, _ := loadLead(h.dir("t1"))
	start := time.Now()
	sink.alert(h.a, openLead{dir: h.dir("t1"), lead: l}, herdrAgent{}, "stalled", 16, s.cfg.StallMinutes, "fp")
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("alert call blocked for %v on a hanging decider", d)
	}
	waitWake()
	recs := h.wakeRecords()
	if len(recs) != 1 || recs[0].Reason != "timeout" {
		t.Fatalf("records = %+v, want one timeout record written before exit", recs)
	}
}

func TestWakeOutcomeOrchestratorActedWhenSendInsideWindow(t *testing.T) {
	h := setup(t)
	h.wakeClockFollows()
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return wakeAnswers(), shadowUsage{}, nil
	})
	window := h.stallT1()
	s, _ := h.a.site()
	resolveOutcomes(s)
	if n := len(h.wakeOutcomes()); n != 0 {
		t.Fatalf("outcome written before the window ended: %d", n)
	}
	h.must("send", "t1", "are you stuck?")
	h.now = h.now.Add(time.Duration(window+1) * time.Minute)
	h.must("decisions", "report", "--use", "wake")
	h.must("decisions", "report", "--use", "wake")
	outs := h.wakeOutcomes()
	if len(outs) != 1 || outs[0].Outcome["orchestrator_acted"] != true || outs[0].Outcome["progressed_alone"] != false {
		t.Fatalf("outcomes = %+v", outs)
	}
}

func TestWakeOutcomeProgressedAloneOnWorktreeChange(t *testing.T) {
	h := setup(t)
	h.wakeClockFollows()
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return wakeAnswers(), shadowUsage{}, nil
	})
	window := h.stallT1()
	wt := filepath.Join(filepath.Dir(h.dir("t1")), "worktrees", "t1")
	os.WriteFile(filepath.Join(wt, "x.go"), []byte("package x\n"), 0o644)
	h.now = h.now.Add(time.Duration(window+1) * time.Minute)
	s, _ := h.a.site()
	resolveOutcomes(s)
	outs := h.wakeOutcomes()
	if len(outs) != 1 || outs[0].Outcome["progressed_alone"] != true || outs[0].Outcome["orchestrator_acted"] != false {
		t.Fatalf("outcomes = %+v", outs)
	}
}

func TestWakeOutcomeStillNotProgressedWhenNothingHappens(t *testing.T) {
	h := setup(t)
	h.wakeClockFollows()
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return wakeAnswers(), shadowUsage{}, nil
	})
	window := h.stallT1()
	h.now = h.now.Add(time.Duration(window+1) * time.Minute)
	s, _ := h.a.site()
	resolveOutcomes(s)
	outs := h.wakeOutcomes()
	if len(outs) != 1 || outs[0].Outcome["progressed_alone"] != false || outs[0].Outcome["orchestrator_acted"] != false {
		t.Fatalf("outcomes = %+v (the watcher's own stalled note must not count as an action or as lead progress)", outs)
	}
}

func TestWakeClosedLeadStillGetsOutcomeFromReport(t *testing.T) {
	h := setup(t)
	h.wakeClockFollows()
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return wakeAnswers(), shadowUsage{}, nil
	})
	window := h.stallT1()
	h.must("report", "--dir", h.dir("t1"), "done", "finished")
	h.must("close", "t1")
	h.now = h.now.Add(time.Duration(window+1) * time.Minute)
	h.must("decisions", "report", "--use", "wake")
	outs := h.wakeOutcomes()
	if len(outs) != 1 || outs[0].Outcome["orchestrator_acted"] != true {
		t.Fatalf("outcomes = %+v", outs)
	}
}

func TestWakeDisabledRoleRecordsNothing(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[roles.wake]\nenabled = false\n"), 0o644)
	fakeDecider(t, func(context.Context) (map[string]json.RawMessage, shadowUsage, error) {
		return wakeAnswers(), shadowUsage{}, nil
	})
	h.stallT1() // still prints the alert
	waitWake()
	if _, err := os.Stat(decisionFile(h.wakeRoot(), wakeUse)); !os.IsNotExist(err) {
		t.Fatalf("a disabled wake role wrote a record file (stat err %v)", err)
	}
}

func TestCloseWritesTriageOutcome(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	h.must("report", "--dir", h.dir("t1"), "done", "finished")
	h.must("close", "t1")
	outs, err := readJSONLines[decisionOutcome](decisionOutcomeFile(h.wakeRoot(), "triage"))
	if err != nil || len(outs) != 1 {
		t.Fatalf("triage outcomes after close = %v (err %v), want 1", outs, err)
	}
}

func TestSendCompactFocusLeaveActionNotes(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	h.must("send", "t1", "hello")
	h.must("focus", "t1")
	notes := ""
	for _, e := range readStatus(h.dir("t1")) {
		if e.By == "orch" {
			notes += e.Text + "\n"
		}
	}
	wantContains(t, notes, "sent message 001", "focused ")
}
