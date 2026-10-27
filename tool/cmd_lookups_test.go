package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var lookupsNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// lookupsFixture writes a log with two Claude sessions in two repos, one old
// Claude line and one pi session, and points the report at it.
func lookupsFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ORCH_TOOL_GUARD_STATE_DIR", dir)
	recent := lookupsNow.Add(-time.Hour).Format("2006-01-02T15:04:05.000000Z")
	old := lookupsNow.Add(-10 * 24 * time.Hour).Format("2006-01-02T15:04:05.000000Z")
	type m = map[string]any
	line := func(ts, harness, session, repo string, turn any, tool, query string, extra m) m {
		out := m{"ts": ts, "harness": harness, "session": session, "repo": repo, "turn": turn, "tool": tool, "query": query,
			"ok": true, "empty": false, "bytes": 10, "duration_ms": nil, "reminder": nil, "followed": nil}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	lines := []m{
		line(recent, "claude", "s1", "/r/a", "t1", "jbcontext", "a", nil),
		line(recent, "claude", "s1", "/r/a", "t1", "rg", "FooBar", nil),
		line(recent, "claude", "s1", "/r/a", "t2", "rg", "zzz", m{"empty": true}),
		line(recent, "claude", "s1", "/r/a", "t2", "read", "/f.go:1-10", nil),
		line(recent, "claude", "s1", "/r/a", "t3", "read", "/f.go:1-10", nil),
		line(recent, "claude", "s2", "/r/b", "t9", "rg", "gateway", m{"reminder": "concept-first"}),
		line(recent, "claude", "s2", "/r/b", "t10", "jbcontext", "q", m{"ok": false, "followed": true}),
		line(recent, "claude", "s2", "/r/b", "t10", "rg", "parse_token", m{"reminder": "symbol-via-lsp"}),
		line(recent, "claude", "s2", "/r/b", "t11", "rg", "x", m{"followed": false}),
		line(old, "claude", "s3", "/r/b", "t1", "rg", "old", nil),
		line(recent, "pi", "p1", "/r/a", 0, "read", "a.go", nil),
		line(recent, "pi", "p1", "/r/a", 1, "lsp:refs", "Gateway", nil),
	}
	var buf bytes.Buffer
	for _, l := range lines {
		raw, _ := json.Marshal(l)
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	buf.WriteString("not json\n")
	if err := os.WriteFile(filepath.Join(dir, "lookups.jsonl"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runLookups(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errw bytes.Buffer
	a := &app{out: &out, errw: &errw, now: func() time.Time { return lookupsNow }}
	code := a.run(append([]string{"lookups", "report"}, args...))
	return out.String() + errw.String(), code
}

func lookupsJSON(t *testing.T, args ...string) lookupReport {
	t.Helper()
	text, code := runLookups(t, append(args, "--json")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, text)
	}
	var report lookupReport
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	return report
}

func harnessOf(t *testing.T, report lookupReport, name string) lookupHarnessReport {
	t.Helper()
	for _, h := range report.Harnesses {
		if h.Harness == name {
			return h
		}
	}
	t.Fatalf("no %s section in %+v", name, report)
	return lookupHarnessReport{}
}

func TestLookupsReportMeasuresTurnsToolsAndReminders(t *testing.T) {
	lookupsFixture(t)
	report := lookupsJSON(t, "--since", "7d")
	if len(report.Harnesses) != 2 || report.Harnesses[0].Harness != "claude" || report.Harnesses[1].Harness != "pi" {
		t.Fatalf("harness sections: %+v", report.Harnesses)
	}
	h := harnessOf(t, report, "claude")
	if h.Sessions != 2 || h.Lookups != 9 || h.Turns != 6 {
		t.Errorf("sessions/lookups/turns = %d/%d/%d, want 2/9/6 (the 10 day old line is outside 7d)", h.Sessions, h.Lookups, h.Turns)
	}
	if h.LookupsPerTurn != 1.5 || h.TurnsWithTwoPlus != 0.5 {
		t.Errorf("per turn %v, share with 2+ %v, want 1.5 and 0.5", h.LookupsPerTurn, h.TurnsWithTwoPlus)
	}
	if h.SessionsWithFirst != 2 || h.SessionsFirstGood != 0.5 {
		t.Errorf("first search good %v of %d, want 0.5 of 2 (s1 jbcontext, s2 rg)", h.SessionsFirstGood, h.SessionsWithFirst)
	}
	if h.SymbolRg != 2 {
		t.Errorf("symbol rg = %d, want 2 (FooBar, parse_token)", h.SymbolRg)
	}
	if got := h.Tools["rg"]; got.Lookups != 5 || got.Empty != 1 || got.EmptyPc != 0.2 {
		t.Errorf("rg stats %+v", got)
	}
	if got := h.Tools["jbcontext"]; got.Lookups != 2 || got.Errors != 1 || got.ErrorPc != 0.5 {
		t.Errorf("jbcontext stats %+v", got)
	}
	if got := h.Reminders["concept-first"]; got.Fired != 1 || got.Followed != 1 || got.FollowRate != 1 {
		t.Errorf("concept-first %+v", got)
	}
	if got := h.Reminders["symbol-via-lsp"]; got.Fired != 1 || got.Ignored != 1 || got.FollowRate != 0 {
		t.Errorf("symbol-via-lsp %+v", got)
	}
	if len(h.RepeatedReads) != 1 || h.RepeatedReads[0].Query != "/f.go:1-10" || h.RepeatedReads[0].Count != 2 || h.RepeatedReads[0].Session != "s1" {
		t.Errorf("repeated reads %+v", h.RepeatedReads)
	}
	p := harnessOf(t, report, "pi")
	if p.Lookups != 2 || p.SessionsWithFirst != 1 || p.SessionsFirstGood != 1 {
		t.Errorf("pi %+v", p)
	}
}

func TestLookupsReportWithoutSinceKeepsOldLines(t *testing.T) {
	lookupsFixture(t)
	h := harnessOf(t, lookupsJSON(t), "claude")
	if h.Lookups != 10 || h.Sessions != 3 {
		t.Errorf("lookups/sessions = %d/%d, want 10/3", h.Lookups, h.Sessions)
	}
}

func TestLookupsReportFiltersByRepo(t *testing.T) {
	lookupsFixture(t)
	report := lookupsJSON(t, "--repo", "/r/a")
	if h := harnessOf(t, report, "claude"); h.Lookups != 5 || h.Sessions != 1 {
		t.Errorf("claude in /r/a: %d lookups, %d sessions, want 5 and 1", h.Lookups, h.Sessions)
	}
	if report.Lookups != 7 {
		t.Errorf("total %d, want 7 (5 claude + 2 pi)", report.Lookups)
	}
	if got := lookupsJSON(t, "--repo", "/r"); got.Lookups != 12 {
		t.Errorf("a repo path covers the directories below it: %d lookups, want 12", got.Lookups)
	}
	if got := lookupsJSON(t, "--repo", "/r/"); got.Lookups != 12 {
		t.Errorf("a trailing slash changes the filter: %d lookups, want 12", got.Lookups)
	}
}

func TestLookupsReportPlainTable(t *testing.T) {
	lookupsFixture(t)
	text, code := runLookups(t, "--since", "7d")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, text)
	}
	for _, want := range []string{"== claude ==", "== pi ==", "1.50 mean, 50% of turns with 2+", "50% of 2 sessions",
		"concept-first", "100% (1 of 1)", "symbol-via-lsp", "0% (0 of 1)", "2x /f.go:1-10"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
}

func TestLookupsReportEmptyAndBadInput(t *testing.T) {
	t.Setenv("ORCH_TOOL_GUARD_STATE_DIR", t.TempDir())
	text, code := runLookups(t)
	if code != 0 || !strings.Contains(text, "no lookups logged") {
		t.Errorf("missing log: exit %d %q", code, text)
	}
	if _, code := runLookups(t, "--since", "soon"); code != 1 {
		t.Errorf("a bad --since exits %d, want 1", code)
	}
	var out, errw bytes.Buffer
	a := &app{out: &out, errw: &errw, now: time.Now}
	if code := a.run([]string{"lookups"}); code != 2 {
		t.Errorf("bare `orch lookups` exits %d, want 2", code)
	}
}

func TestLookupsSinceUnits(t *testing.T) {
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "36h": 36 * time.Hour, "7d": 7 * 24 * time.Hour, "0.5d": 12 * time.Hour} {
		if got, err := parseSince(in); err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "-1d", "0h", "x"} {
		if _, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q) succeeded", in)
		}
	}
}
