package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func limitLine(at time.Time, resets int64, status string) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"timestamp":%q,"isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,"message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"limit"}]},"quotaLimits":{"status":%q,"resetsAt":%d,"rateLimitType":"five_hour"}}`, ts(at), status, resets)
}

func apiErrLine(at time.Time, kind string, status int) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"timestamp":%q,"isApiErrorMessage":true,"error":%q,"apiErrorStatus":%d,"message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}`, ts(at), kind, status)
}

func TestClaudeLimitSince(t *testing.T) {
	h := setup(t)
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	h.transcript("claude", "s1",
		limitLine(base, 1000, "rejected"),
		limitLine(base.Add(2*time.Minute), 2000, "rejected"),
		limitLine(base.Add(time.Minute), 1500, "rejected"),
		apiErrLine(base.Add(5*time.Minute), "server_error", 529),
		apiErrLine(base.Add(6*time.Minute), "authentication_failed", 401),
		limitLine(base.Add(7*time.Minute), 3000, "allowed_warning"),
		"not json isApiErrorMessage quotaLimits")
	got, ok := claudeLimitSince("s1", "")
	if !ok || got.ResetsAt != 2000 || got.Type != "five_hour" || got.Stamp != ts(base.Add(2*time.Minute)) {
		t.Fatalf("got %+v, %v; want the newest rejection (resetsAt 2000)", got, ok)
	}
	if _, ok := claudeLimitSince("s1", ts(base.Add(2*time.Minute))); ok {
		t.Fatal("a line equal to after counted; only strictly newer lines may")
	}
	got, ok = claudeLimitSince("s1", ts(base.Add(90*time.Second)))
	if !ok || got.ResetsAt != 2000 {
		t.Fatalf("after bound: got %+v, %v", got, ok)
	}
	if _, ok := claudeLimitSince("missing", ""); ok {
		t.Fatal("a missing transcript reported a limit")
	}
}

func TestAutoLimit(t *testing.T) {
	h := setup(t)
	now := h.now.Unix()
	events := func() int {
		b, _ := os.ReadFile(eventLogPath(filepath.Dir(healthPath())))
		return strings.Count(string(b), `"system":"health"`)
	}
	if wrote, err := autoLimit("claude", now+1000, "r", now); err != nil || !wrote {
		t.Fatalf("first write = %v, %v", wrote, err)
	}
	e := loadHealth(h.now)["claude"]
	if e.Source != "auto" || e.Until != now+1000 || e.State != "limited" {
		t.Fatalf("entry = %+v", e)
	}
	if wrote, _ := autoLimit("claude", now+1000, "r", now+10); wrote || events() != 1 {
		t.Fatalf("same Until wrote again (events %d)", events())
	}
	if wrote, _ := autoLimit("claude", now+2000, "r", now+20); !wrote {
		t.Fatal("later Until did not replace the auto entry")
	}
	e = loadHealth(h.now)["claude"]
	if e.Until != now+2000 || e.Since != now {
		t.Fatalf("replaced entry = %+v; want Until %d and the old Since %d", e, now+2000, now)
	}
	h.must("health", "set", "claude", "limited", "--for", "1h")
	if wrote, _ := autoLimit("claude", now+9000, "r", now+30); wrote {
		t.Fatal("auto overwrote an active manual entry")
	}
	if e := loadHealth(h.now)["claude"]; e.Source != "" || e.Until != now+3600 {
		t.Fatalf("manual entry changed: %+v", e)
	}
	h.now = h.now.Add(2 * time.Hour)
	if wrote, _ := autoLimit("claude", h.now.Unix()+500, "r", h.now.Unix()); !wrote {
		t.Fatal("an expired manual entry blocked auto")
	}
}

func TestHealthListShowsAuto(t *testing.T) {
	h := setup(t)
	if _, err := autoLimit("claude", h.now.Unix()+3600, "r", h.now.Unix()); err != nil {
		t.Fatal(err)
	}
	wantContains(t, h.must("health"), "(auto)")
	h.must("health", "set", "claude", "ok")
	if _, ok := loadHealth(h.now)["claude"]; ok {
		t.Fatal("set ok did not remove an auto entry")
	}
}

func TestWatchRecordsClaudeLimit(t *testing.T) {
	h := setup(t)
	h.task("t1")
	h.must("spawn", "t1")
	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Session = "s1" })
	spawn := h.now
	resets := spawn.Add(3 * time.Hour).Unix()
	h.transcript("claude", "s1", apiErrLine(spawn.Add(time.Minute), "server_error", 529))
	tick := func() string {
		h.now = h.now.Add(61 * time.Second)
		return h.must("watch", "--once")
	}
	if out := tick(); strings.Contains(out, "limit:") {
		t.Fatalf("529 line produced %q", out)
	}
	if _, ok := loadHealth(h.now)["claude"]; ok {
		t.Fatal("529 wrote health")
	}
	h.transcript("claude", "s1", apiErrLine(spawn.Add(time.Minute), "server_error", 529), limitLine(h.now.Add(time.Second), resets, "rejected"))
	out := tick()
	wantContains(t, out, "limit:")
	if e := loadHealth(h.now)["claude"]; e.Source != "auto" || e.Until != resets {
		t.Fatalf("entry = %+v", e)
	}
	if out := tick(); strings.Contains(out, "limit:") {
		t.Fatalf("second tick repeated: %q", out)
	}
	h.must("health", "set", "claude", "ok")
	tick()
	if _, ok := loadHealth(h.now)["claude"]; ok {
		t.Fatal("old line undid manual ok")
	}
	h.transcript("claude", "s1", limitLine(h.now.Add(time.Second), resets, "rejected"), limitLine(h.now.Add(2*time.Second), resets+60, "rejected"))
	wantContains(t, tick(), "limit:")
	if e := loadHealth(h.now)["claude"]; e.Until != resets+60 {
		t.Fatalf("new rejection entry = %+v", e)
	}
}

func TestWatchIgnoresPiTranscriptForLimits(t *testing.T) {
	h := setup(t)
	h.task("t1", "--harness", "pi", "--model", "m", "--effort", "low")
	h.must("spawn", "t1")
	h.editFake(func(st *fakeState) { st.Agents["wt1:p1"].Session = "s1" })
	h.transcript("claude", "s1", limitLine(h.now.Add(time.Second), h.now.Unix()+9000, "rejected"))
	h.now = h.now.Add(61 * time.Second)
	if out := h.must("watch", "--once"); strings.Contains(out, "limit:") {
		t.Fatalf("pi lead scanned: %q", out)
	}
	if _, ok := loadHealth(h.now)["claude"]; ok {
		t.Fatal("pi lead wrote claude health")
	}
}
