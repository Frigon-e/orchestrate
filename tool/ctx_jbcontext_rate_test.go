package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// jbRateFake is a jbcontext stand-in for `search`: it logs each call to
// <dir>/calls.log, records the most processes running at once in <dir>/max,
// sleeps for <dir>/sleep seconds, and answers 429 for as many calls as
// <dir>/fail429 holds (a number; "all" means every call).
func jbRateFake(t *testing.T) (bin, dir string) {
	t.Helper()
	t.Setenv("ORCH_CACHE_DIR", t.TempDir())
	t.Setenv("ORCH_JB_BACKOFF_MS", "10")
	dir = t.TempDir()
	bin = filepath.Join(dir, "jbcontext")
	script := `#!/bin/sh
D="` + dir + `"
[ "$1" = search ] || exit 0
echo "$$ $*" >> "$D/calls.log"
mkdir "$D/run.$$"
n=$(ls "$D" | grep -c '^run\.')
echo "$n" >> "$D/conc.log"
[ -f "$D/sleep" ] && sleep "$(cat "$D/sleep")"
rmdir "$D/run.$$"
if [ -f "$D/fail429" ]; then
  want=$(cat "$D/fail429"); seen=$(wc -l < "$D/calls.log" | tr -d ' ')
  if [ "$want" = all ] || [ "$seen" -le "$want" ]; then
    echo "Limit reached. Please try again later (HTTP 429)" >&2
    exit 1
  fi
fi
echo '{"type":"search_result","results":[]}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func jbRateLines(dir, name string) []string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return strings.Fields(string(b))
}

func jbRateMax(dir string) int {
	m := 0
	for _, s := range jbRateLines(dir, "conc.log") {
		if n, _ := strconv.Atoi(s); n > m {
			m = n
		}
	}
	return m
}

func jbRateGit(t *testing.T) string {
	t.Helper()
	code := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = code
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return code
}

func TestJbcontextIdenticalQueriesRunOneProcess(t *testing.T) {
	bin, dir := jbRateFake(t)
	os.WriteFile(filepath.Join(dir, "sleep"), []byte("0.3"), 0o644)
	code, cache := t.TempDir(), t.TempDir()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := searchCode(bin, code, cache, "", "", 3, "same question"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(jbRateLines(dir, "conc.log")); n != 1 {
		t.Errorf("jbcontext ran %d times for 8 identical concurrent queries, want 1", n)
	}
}

// jbRateBurst runs n different searches at once and returns the fake's dir.
func jbRateBurst(t *testing.T, n int, sleep string) string {
	t.Helper()
	bin, dir := jbRateFake(t)
	os.WriteFile(filepath.Join(dir, "sleep"), []byte(sleep), 0o644)
	jbMarkWarm()
	code, cache := t.TempDir(), t.TempDir()
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := searchCode(bin, code, cache, "", "", 3, fmt.Sprintf("question %d", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := len(jbRateLines(dir, "conc.log")); got != n {
		t.Errorf("jbcontext ran %d times, want %d", got, n)
	}
	return dir
}

func TestJbcontextRunsAtMostFiveProcessesAtOnce(t *testing.T) {
	dir := jbRateBurst(t, 20, "0.4")
	if m := jbRateMax(dir); m > 5 || m < 4 {
		t.Errorf("%d jbcontext processes ran at once, want 4 to 5", m)
	}
}

func TestJbcontextSlotsEnvOneSerializes(t *testing.T) {
	t.Setenv("ORCH_JB_SLOTS", "1")
	dir := jbRateBurst(t, 4, "0.1")
	if m := jbRateMax(dir); m != 1 {
		t.Errorf("%d jbcontext processes ran at once with ORCH_JB_SLOTS=1, want 1", m)
	}
}

func TestJbcontextSlotsEnvInvalidFallsBackToFive(t *testing.T) {
	for _, v := range []string{"", "0", "33", "-1", "many"} {
		t.Setenv("ORCH_JB_SLOTS", v)
		if n := jbSlotCount(); n != 5 {
			t.Errorf("ORCH_JB_SLOTS=%q gives %d slots, want 5", v, n)
		}
	}
	t.Setenv("ORCH_JB_SLOTS", "32")
	if n := jbSlotCount(); n != 32 {
		t.Errorf("ORCH_JB_SLOTS=32 gives %d slots", n)
	}
}

// A cold stamp lets one call run alone; the rest start only after it ends.
func TestJbcontextColdStampRunsOneCallFirst(t *testing.T) {
	bin, dir := jbRateFake(t)
	os.WriteFile(filepath.Join(dir, "sleep"), []byte("0.4"), 0o644)
	code, cache := t.TempDir(), t.TempDir()
	var wg sync.WaitGroup
	start := time.Now()
	for i := range 5 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := searchCode(bin, code, cache, "", "", 3, fmt.Sprintf("question %d", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	conc := jbRateLines(dir, "conc.log")
	if len(conc) != 5 || conc[0] != "1" {
		t.Errorf("conc.log = %v, want 5 calls and the first one alone", conc)
	}
	// One call of 0.4 s, then the other four together: two rounds, not one.
	if el := time.Since(start); el < 750*time.Millisecond {
		t.Errorf("5 cold calls took %s, want at least two rounds of 0.4 s", el)
	}
	if jbCold() {
		t.Error("stamp is still cold after successful calls")
	}
	if m := jbRateMax(dir); m < 2 {
		t.Errorf("max %d: slots did not open after the first call", m)
	}
}

// TestJbcontextCapHoldsAcrossProcesses starts other orch-like processes (this
// test binary) that each search different queries: only the machine-wide cap
// can hold them to two jbcontext processes at once.
func TestJbcontextCapHoldsAcrossProcesses(t *testing.T) {
	if os.Getenv("ORCH_JB_HELPER_QUERY") != "" {
		if _, err := searchCode(os.Getenv("ORCH_JBCONTEXT"), os.Getenv("ORCH_JB_HELPER_CODE"), os.Getenv("ORCH_JB_HELPER_CACHE"), "", "", 3, os.Getenv("ORCH_JB_HELPER_QUERY")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	t.Setenv("ORCH_JB_SLOTS", "2")
	bin, dir := jbRateFake(t)
	os.WriteFile(filepath.Join(dir, "sleep"), []byte("0.4"), 0o644)
	code := t.TempDir()
	var procs []*exec.Cmd
	for i := range 5 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestJbcontextCapHoldsAcrossProcesses$")
		cmd.Env = append(os.Environ(), "ORCH_JBCONTEXT="+bin, "ORCH_JB_HELPER_CODE="+code, "ORCH_JB_HELPER_CACHE="+t.TempDir(), fmt.Sprintf("ORCH_JB_HELPER_QUERY=question %d", i))
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, cmd)
	}
	for _, p := range procs {
		if err := p.Wait(); err != nil {
			t.Errorf("helper: %v", err)
		}
	}
	if n := len(jbRateLines(dir, "conc.log")); n != 5 {
		t.Errorf("jbcontext ran %d times, want 5", n)
	}
	if m := jbRateMax(dir); m > 2 {
		t.Errorf("%d jbcontext processes ran at once across processes, want at most 2", m)
	}
}

func TestJbcontextRateLimitIsRetried(t *testing.T) {
	bin, dir := jbRateFake(t)
	os.WriteFile(filepath.Join(dir, "fail429"), []byte("2"), 0o644)
	hits, err := searchCode(bin, t.TempDir(), t.TempDir(), "", "", 3, "retry me")
	if err != nil {
		t.Fatalf("429 twice then success should succeed, got %v", err)
	}
	if hits == nil || len(jbRateLines(dir, "conc.log")) != 3 {
		t.Errorf("hits=%v calls=%d, want 3 calls", hits, len(jbRateLines(dir, "conc.log")))
	}
}

func TestJbcontextAlwaysRateLimitedGivesClearErrorAndCooldown(t *testing.T) {
	bin, dir := jbRateFake(t)
	os.WriteFile(filepath.Join(dir, "fail429"), []byte("all"), 0o644)
	code, cache := t.TempDir(), t.TempDir()
	_, err := searchCode(bin, code, cache, "", "", 3, "first question")
	if err == nil || !strings.Contains(err.Error(), "jbcontext rate limited (429)") || !strings.Contains(err.Error(), "first question") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "license") || strings.Contains(err.Error(), "\n") {
		t.Errorf("error must be one line without the license text: %q", err)
	}
	if n := len(jbRateLines(dir, "conc.log")); n != 4 {
		t.Errorf("calls = %d, want 1 + 3 retries", n)
	}
	// The cooldown skips jbcontext for the next query.
	_, err = searchCode(bin, code, cache, "", "", 3, "second question")
	if err == nil || !strings.Contains(err.Error(), "jbcontext rate limited (429)") || !strings.Contains(err.Error(), "second question") {
		t.Errorf("second error = %v", err)
	}
	if n := len(jbRateLines(dir, "conc.log")); n != 4 {
		t.Errorf("calls during the cooldown = %d, want still 4", n)
	}
}

func TestJbcontextRateLimitedConceptAnswersFromRg(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	bin, dir := jbRateFake(t)
	os.WriteFile(filepath.Join(dir, "fail429"), []byte("all"), 0o644)
	code := jbRateGit(t)
	os.WriteFile(filepath.Join(code, "refund.go"), []byte("package a\n\nfunc IssueRefund() {}\n"), 0o644)
	e := jbEnv(bin, code, config{})
	e.Policy = "off" // no index calls: only the search runs
	e.NoJB = false
	out := e.lookupConcept("where is the refund issued", "")
	wantContains(t, out, "jbcontext skipped: rate limited (429)", "refund.go:3")
	if strings.Contains(out, "license") {
		t.Errorf("output mentions the license: %s", out)
	}
}

func TestJbcontextCachedResultNeedsNoProcess(t *testing.T) {
	bin, _ := jbRateFake(t)
	code, cache := t.TempDir(), t.TempDir()
	if _, err := searchCode(bin, code, cache, "", "", 3, "cache me"); err != nil {
		t.Fatal(err)
	}
	os.Remove(bin)
	if _, err := searchCode(bin, code, cache, "", "", 3, "cache me"); err != nil {
		t.Errorf("cached search needed the process: %v", err)
	}
}
