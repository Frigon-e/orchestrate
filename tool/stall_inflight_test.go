package main

import (
	"fmt"
	"testing"
	"time"
)

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func claudeUse(at time.Time, id string) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"x"}}]}}`, ts(at), id)
}

func claudeResult(at time.Time, id string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, ts(at), id)
}

func inflightPiCalls(at time.Time, ids ...string) string {
	blocks := ""
	for i, id := range ids {
		if i > 0 {
			blocks += ","
		}
		blocks += fmt.Sprintf(`{"type":"toolCall","id":%q,"name":"bash","arguments":{"command":"x%d"}}`, id, i)
	}
	return fmt.Sprintf(`{"type":"message","id":"a1","timestamp":%q,"message":{"role":"assistant","content":[%s]}}`, ts(at), blocks)
}

func piResult(at time.Time, id string) string {
	return fmt.Sprintf(`{"type":"message","id":"a2","timestamp":%q,"message":{"role":"toolResult","toolCallId":%q,"toolName":"bash","content":[{"type":"text","text":"ok"}],"isError":false}}`, ts(at), id)
}

// spawnAndWait spawns t1 and lets the watcher take its first fingerprint, then
// advances the fake clock by wait and writes the transcript lines built from
// the spawn time.
func inflightLead(h *harness, harnessName string, wait time.Duration, lines func(spawn time.Time) []string) {
	h.t.Helper()
	if harnessName == "pi" {
		h.task("t1", "--harness", "pi", "--model", "m", "--effort", "low")
	} else {
		h.task("t1")
	}
	h.must("spawn", "t1")
	l, _ := loadLead(h.dir("t1"))
	session := l.Session // pi gets its id from orch; Claude's comes from Herdr
	if harnessName == "claude" {
		session = "s1"
		h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Session = session })
	}
	spawn := h.now
	h.must("watch", "--once")
	h.now = h.now.Add(wait)
	h.transcript(harnessName, session, lines(spawn)...)
}

func TestWatchStalledToolInFlight(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		wait    time.Duration
		lines   func(spawn time.Time) []string
		stalled bool
	}{
		{"claude call running 16m suppresses", "claude", 16 * time.Minute,
			func(s time.Time) []string { return []string{claudeUse(s, "toolu_A")} }, false},
		{"claude call running over 3x stall_minutes alerts", "claude", 46 * time.Minute,
			func(s time.Time) []string { return []string{claudeUse(s, "toolu_A")} }, true},
		{"claude call with its result alerts", "claude", 16 * time.Minute,
			func(s time.Time) []string {
				return []string{claudeUse(s, "toolu_A"), claudeResult(s.Add(time.Minute), "toolu_A")}
			}, true},
		{"claude api-request-blob copy is ignored", "claude", 16 * time.Minute,
			func(s time.Time) []string {
				return []string{`{"type":"api-request-blob","timestamp":"` + ts(s) + `","message":{"content":[{"type":"tool_use","id":"toolu_A","name":"Bash","input":{}}]}}`}
			}, true},
		{"claude sidechain call is ignored", "claude", 16 * time.Minute,
			func(s time.Time) []string {
				return []string{`{"type":"assistant","isSidechain":true,"timestamp":"` + ts(s) + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_A","name":"Bash","input":{}}]}}`}
			}, true},
		{"pi call without result suppresses", "pi", 16 * time.Minute,
			func(s time.Time) []string { return []string{inflightPiCalls(s, "c1")} }, false},
		{"pi two calls, one answered, still suppresses", "pi", 16 * time.Minute,
			func(s time.Time) []string {
				return []string{inflightPiCalls(s, "c1", "c2"), piResult(s.Add(time.Minute), "c1")}
			}, false},
		{"pi call with its result alerts", "pi", 16 * time.Minute,
			func(s time.Time) []string {
				return []string{inflightPiCalls(s, "c1"), piResult(s.Add(time.Minute), "c1")}
			}, true},
		{"pi call running over 3x stall_minutes alerts", "pi", 46 * time.Minute,
			func(s time.Time) []string { return []string{inflightPiCalls(s, "c1")} }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := setup(t)
			inflightLead(h, c.harness, c.wait, c.lines)
			out := h.must("watch", "--once")
			if c.stalled {
				wantContains(t, out, "orch: t1 stalled")
			} else if out != "" {
				t.Fatalf("alerted while a tool call was running: %q", out)
			}
		})
	}
}

func TestInFlightSinceNewestUnmatched(t *testing.T) {
	h := setup(t)
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	h.transcript("claude", "s1",
		claudeUse(base, "toolu_A"),
		claudeUse(base.Add(5*time.Minute), "toolu_B"),
		claudeUse(base.Add(9*time.Minute), "toolu_C"),
		claudeResult(base.Add(10*time.Minute), "toolu_C"),
		"not json toolu_D tool_use")
	start, ok := inFlightSince("claude", "s1")
	if !ok || start != base.Add(5*time.Minute).Unix() {
		t.Fatalf("inFlightSince = %d, %v; want %d, true", start, ok, base.Add(5*time.Minute).Unix())
	}
}

func TestInFlightSinceNone(t *testing.T) {
	h := setup(t)
	if _, ok := inFlightSince("claude", "missing"); ok {
		t.Fatal("a missing transcript reported a call in flight")
	}
	h.transcript("claude", "empty", "")
	if _, ok := inFlightSince("claude", "empty"); ok {
		t.Fatal("an empty transcript reported a call in flight")
	}
	if _, ok := inFlightSince("codex", "empty"); ok {
		t.Fatal("an unknown harness reported a call in flight")
	}
}
