package guard

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The patterns are the ones the first version of the rule blocked in a day of
// real sessions; most were exact-text searches the model then simply retried.
func TestSearchFirstRoutesRealPatterns(t *testing.T) {
	shortWait(t)
	lsp := map[string]string{ // pattern -> the orch lsp def names
		`ctxEnvFor|ctxReadiness|func `: "ctxEnvFor ctxReadiness",
		`\bctxEnv\b|ctxOpts|ctxWithinRoot|ctxReadiness|newCtxEnv|ctxEnvFor`: "ctxEnv ctxOpts ctxWithinRoot ctxReadiness newCtxEnv ctxEnvFor",
		`paneOrchOnPath`: "paneOrchOnPath",
		`cardDoc`:        "cardDoc",
		`claimLease|ClaimLease`:                                "claimLease ClaimLease",
		`StatusBadge|statusVariant|statusTone|StatusDot`:       "StatusBadge statusVariant statusTone StatusDot",
		`StateCard`:                                            "StateCard",
		`createFileRoute`:                                      "createFileRoute",
		`toMatchScreenshot`:                                    "toMatchScreenshot",
		`dbtest\.New|NewWorld\(|crmtest\.New`:                  "dbtest.New NewWorld crmtest.New",
		`func TestCtxServerOverridesPATHAndSymlinks`:           "TestCtxServerOverridesPATHAndSymlinks",
		`stackTitle`:                                           "stackTitle",
		`PencilIcon`:                                           "PencilIcon",
		`type Gateway\b`:                                       "Gateway",
		`MaxAttempts|MAX_ATTEMPTS`:                             "MaxAttempts MAX_ATTEMPTS",
		`func.*applyRefund`:                                    "applyRefund",
	}
	jb := []string{
		`exempt|allow|skip|ignore|only instruction|must change instruction`,
		`refund_before_payment|unknown_payment|out_of_order|known gap|refund-before|RefundBefore|refund before`,
		`refund retry`,
	}
	pass := []string{
		`dialog-bg`, `theme`, `rivertest`, `^package`, `env|Env`, `fake.?wings|fakewings`,
		`CtxTool|CtxShell|gateReviewPolicy|gateReviewPolicyVersion|CtxProfile|ctxTool|ctxOnly|\.cli\b|cli\.`,
		`ReceivedAt|received_at`, `no space|WALL|Efficacy|Killed|Lived`,
		`ctxOnlyNotice|ctxCLINotice|ctxBudgetHint|CtxCLI|ctxOnlyReady|ORCH_CTX_ONLY|ctx-only|ctx_only`,
		`staff:invite|SetupLink|setup`, `toolbar=|<DataTable|onSearchChange`, `Not run`, `^done`, `^func Test`,
		`@xl|@max-xl|@3xl|@5xl|@md|container|stack|whitespace|priority|fold`, `DEVSTACK_ADMIN_URL`,
		`^export|^function|^const [A-Z_]+ =`, `toMatchScreenshot|@vitest/browser|vitest/browser|browser:`, `error`,
		`Notes|Survival|2FA|servers owned|columnheader|toHaveClass`, `SHOTS_DIR`, `DEVSTACK_ADMIN_URL|ADMIN_URL`,
		`Driver struct`, `RateLimit|ratelimit`, `admin|seed|password`, `RefundRetryIn|RefundWindow|refund_unmatched|dispute`,
		`gateway_events|staff_alerts`, `rate|429|throttl|RATE`, `RATE_LIMIT|Too Many Attempts|TooManyAttempts`,
		`PaymentsEventFailed|PaymentsEventProcessed|RefundUnmatchedKey|attempts`, `func Benchmark|func Test`,
		`query-boundary|QueryBoundary`, `wrap|stackPart|label`, `dev:seed|DevSeed`, `customers\.New\(|customers\.Deps\{`,
		`make run-crm|devstack|storefront:dev-key`, `icon-xs|size-6\b|\b24 ?px|row-actions|RowAction`, `flush`,
		`<SectionCard|</SectionCard|<DataTable|<Dialog |<Sheet |<Tabs|<TabsContent|<form|ListPage`,
		`tax_rates_no_overlap|EXCLUDE`, `theme.json|theme.css|503|ServiceUnavailable`, `ssh|rsync|HOST|known_hosts`,
		`node`, `MAX_RETRIES`, `FAIL`, `RESULT|FAIL|SUMMARY`, `wave 2b|wave 3`, `Command-line tools`,
	}
	run := func(p string) (int, string) {
		repo := sfEnv(t, true)
		return sfPre(t, repo, "rg -n '"+strings.ReplaceAll(p, "'", `'\''`)+"' src")
	}
	for p, names := range lsp {
		code, msg := run(p)
		if code != 2 || !strings.Contains(msg, "Run: orch lsp def "+names+" (") || strings.Contains(msg, "jbcontext search") {
			t.Errorf("%s: want orch lsp def %s, got %d %q", p, names, code, msg)
		}
	}
	for _, p := range jb {
		if code, msg := run(p); code != 2 || !strings.Contains(msg, "jbcontext search") {
			t.Errorf("%s: want jbcontext, got %d %q", p, code, msg)
		}
	}
	for _, p := range pass {
		if code, msg := run(p); code != 0 {
			t.Errorf("%s: want pass, got %q", p, msg)
		}
	}
}

// sfEnv: a git repo, a state dir and stubs for rg, orch and jbcontext alone on PATH.
func sfEnv(t *testing.T, indexed bool) (repo string) {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"rg", "orch", "jbcontext"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repo, _ = filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("ORCH_TOOL_GUARD_STATE_DIR", t.TempDir())
	for _, k := range []string{"ORCH_TOOL_GUARD", "ORCH_TOOL_GUARD_SKIP", "ORCH_LOOKUP_COACH", "ORCH_LOOKUP_LOG"} {
		t.Setenv(k, "")
	}
	old := jbIndexedProbe
	t.Cleanup(func() { jbIndexedProbe = old })
	jbIndexedProbe = func(string) bool { return indexed }
	return repo
}

func sfPre(t *testing.T, repo, command string) (int, string) {
	t.Helper()
	ev := piEvent(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "session_id": "s1", "cwd": repo,
		"tool_input": map[string]any{"command": command}})
	var stderr bytes.Buffer
	return Pre(ev, &stderr), stderr.String()
}

func sfEvent(t *testing.T, repo, name string) *Event {
	return piEvent(t, map[string]any{"hook_event_name": name, "session_id": "s1", "cwd": repo, "prompt": "go"})
}

func sfLog(t *testing.T) string {
	data, _ := os.ReadFile(LogPath())
	return string(data)
}

func TestSearchFirstBlocksOncePerPromptAndRearms(t *testing.T) {
	repo := sfEnv(t, true)
	Coach(sfEvent(t, repo, "UserPromptSubmit"), &bytes.Buffer{})
	code, msg := sfPre(t, repo, "rg -n 'refund retry' src")
	if code != 2 || !strings.Contains(msg, "deny-search-first") || !strings.Contains(msg, "jbcontext search") {
		t.Fatalf("first concept rg: %d %q", code, msg)
	}
	if strings.Contains(msg, "—") {
		t.Error("em dash in reason")
	}
	log := sfLog(t)
	if !strings.Contains(log, `"tool":"blocked:rg"`) || !strings.Contains(log, `"reminder":"search-first"`) || !strings.Contains(log, `"query":"refund retry"`) {
		t.Fatalf("log row: %s", log)
	}
	if code, _ := sfPre(t, repo, "rg -n 'refund retry' src"); code != 0 {
		t.Fatal("the retry was blocked")
	}
	if code, _ := sfPre(t, repo, "rg -n other src"); code != 0 {
		t.Fatal("a later rg in the same window was blocked")
	}
	Coach(sfEvent(t, repo, "UserPromptSubmit"), &bytes.Buffer{})
	if code, _ := sfPre(t, repo, "rg -n 'refund retry' src"); code != 2 {
		t.Fatal("a new prompt did not re-arm the rule")
	}
}

func TestSearchFirstFollowThroughIsLogged(t *testing.T) {
	repo := sfEnv(t, true)
	if code, _ := sfPre(t, repo, "rg -n 'refund retry'"); code != 2 {
		t.Fatal("not blocked")
	}
	post := piEvent(t, map[string]any{"hook_event_name": "PostToolUse", "session_id": "s1", "cwd": repo, "tool_name": "Bash",
		"tool_input":    map[string]any{"command": `jbcontext search "toast"`},
		"tool_response": map[string]any{"stdout": "ok", "stderr": ""}})
	Coach(post, &bytes.Buffer{})
	if log := sfLog(t); !strings.Contains(log, `"tool":"jbcontext"`) || !strings.Contains(log, `"followed":true`) {
		t.Fatalf("log: %s", log)
	}
}

func TestSearchFirstSymbolNamesTheSymbol(t *testing.T) {
	repo := sfEnv(t, false)
	code, msg := sfPre(t, repo, `rg -n "FooBar" src`)
	if code != 2 || !strings.Contains(msg, "orch lsp def FooBar") || !strings.Contains(msg, "orch lsp refs FooBar") {
		t.Fatalf("symbol rg: %d %q", code, msg)
	}
}

func TestSearchFirstStaysOutOfTheWay(t *testing.T) {
	repo := sfEnv(t, true)
	outside := t.TempDir()
	cases := map[string]string{
		"fixed string":  "rg -F toast src",
		"files":         "rg --files src",
		"file target":   "rg toast " + filepath.Join(repo, "main.go"),
		"outside repo":  "rg toast " + outside,
		"exact pattern": "rg -n '^## ' src",
	}
	for name, line := range cases {
		if code, msg := sfPre(t, repo, line); code != 0 {
			t.Errorf("%s: blocked %q", name, msg)
		}
	}
	if code, _ := sfPre(t, repo, `jbcontext search "x"`); code != 0 {
		t.Fatal("jbcontext blocked")
	}
	if code, _ := sfPre(t, repo, "rg -n 'refund retry' src"); code != 0 {
		t.Fatal("blocked after a jbcontext call in the same window")
	}
}

func TestSearchFirstNeedsAnIndexedRepoForConcepts(t *testing.T) {
	repo := sfEnv(t, false)
	if code, _ := sfPre(t, repo, "rg -n 'refund retry' src"); code != 0 {
		t.Fatal("blocked in a repo jbcontext has not indexed")
	}
}

func TestSearchFirstSkipAndGuardOff(t *testing.T) {
	for _, env := range []map[string]string{{"ORCH_TOOL_GUARD_SKIP": "search-first"}, {"ORCH_TOOL_GUARD_SKIP": "deny-search-first"}, {"ORCH_TOOL_GUARD": "off"}} {
		repo := sfEnv(t, true)
		for k, v := range env {
			t.Setenv(k, v)
		}
		if code, _ := sfPre(t, repo, "rg -n 'refund retry' src"); code != 0 {
			t.Errorf("%v: blocked", env)
		}
	}
}

func TestSearchFirstProbeRunsOncePerRepo(t *testing.T) {
	repo := sfEnv(t, false)
	n := 0
	jbIndexedProbe = func(string) bool { n++; return false }
	sfPre(t, repo, "rg -n 'refund retry' src")
	sfPre(t, repo, "rg -n 'edge case' src")
	if n != 1 {
		t.Fatalf("probe ran %d times", n)
	}
}

// A prompt in either harness refreshes its checkout's jbcontext index in the
// background, at most once per jbRefreshEvery for each checkout.
func TestPromptRefreshesTheIndexOncePerCheckout(t *testing.T) {
	repo := sfEnv(t, true)
	t.Setenv("ORCH_JB_REFRESH", "")
	other, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(other, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var started []string
	old := jbRefreshStart
	t.Cleanup(func() { jbRefreshStart = old })
	jbRefreshStart = func(root string) { started = append(started, root) }
	claude := func(cwd string) { Coach(sfEvent(t, cwd, "UserPromptSubmit"), &bytes.Buffer{}) }
	pi := func(cwd string) {
		PiCoach(piEvent(t, map[string]any{"event": "before_agent_start", "session": "p1", "cwd": cwd}))
	}
	claude(filepath.Join(repo, "."))
	pi(repo)
	pi(other) // a worktree is its own checkout
	if want := []string{repo, other}; !reflect.DeepEqual(started, want) {
		t.Fatalf("started %v, want %v", started, want)
	}
	// Once the wait is over, the next prompt refreshes again.
	for _, stamp := range must(filepath.Glob(filepath.Join(StateDir(), "jbrefresh", "*"))) {
		old := time.Now().Add(-jbRefreshEvery - time.Minute)
		if err := os.Chtimes(stamp, old, old); err != nil {
			t.Fatal(err)
		}
	}
	claude(repo)
	t.Setenv("ORCH_JB_REFRESH", "off")
	pi(other)
	if len(started) != 3 || started[2] != repo {
		t.Fatalf("after the wait and with ORCH_JB_REFRESH=off: %v", started)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// The probe takes seconds, so its answer holds across sessions until it is stale.
func TestSearchFirstProbeAnswerIsSharedAcrossSessions(t *testing.T) {
	repo := sfEnv(t, false)
	n := 0
	jbIndexedProbe = func(string) bool { n++; return true }
	for _, session := range []string{"a", "b"} {
		ev := piEvent(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "session_id": session, "cwd": repo,
			"tool_input": map[string]any{"command": "rg -n 'refund retry' src"}})
		if code := Pre(ev, &bytes.Buffer{}); code != 2 {
			t.Fatalf("session %s: exit %d", session, code)
		}
	}
	if n != 1 {
		t.Fatalf("probe ran %d times for two sessions", n)
	}
	path := filepath.Join(StateDir(), "jbindex.json")
	stale := []byte(`{"` + repo + `":{"indexed":true,"at":"2000-01-01T00:00:00Z"}}`)
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	ev := piEvent(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "session_id": "c", "cwd": repo,
		"tool_input": map[string]any{"command": "rg -n 'refund retry' src"}})
	Pre(ev, &bytes.Buffer{})
	if n != 2 {
		t.Fatalf("a stale answer was not probed again: %d probes", n)
	}
}

func TestPiSearchFirstBlocksOncePerPrompt(t *testing.T) {
	repo := sfEnv(t, true)
	run := func(command string) PiResponse {
		return PiPre(piEvent(t, map[string]any{"event": "tool_call", "session": "p1", "cwd": repo, "tool": "bash",
			"input": map[string]any{"command": command}, "resultHook": true}))
	}
	PiCoach(piEvent(t, map[string]any{"event": "before_agent_start", "session": "p1", "cwd": repo}))
	if res := run("rg -n 'refund retry' src"); !res.Block || !strings.Contains(res.Reason, "deny-search-first") {
		t.Fatalf("first: %+v", res)
	}
	if !strings.Contains(sfLog(t), `"harness":"pi"`) {
		t.Fatalf("no pi row: %s", sfLog(t))
	}
	if res := run("rg -n 'refund retry' src"); res.Block {
		t.Fatal("retry blocked")
	}
	PiCoach(piEvent(t, map[string]any{"event": "before_agent_start", "session": "p1", "cwd": repo}))
	if res := run("rg -n 'refund retry' src"); !res.Block {
		t.Fatal("not re-armed")
	}
	PiCoach(piEvent(t, map[string]any{"event": "before_agent_start", "session": "p1", "cwd": repo}))
	if res := run(`jbcontext search "x"`); res.Block {
		t.Fatal("jbcontext blocked")
	}
	if res := run("rg -n 'refund retry' src"); res.Block {
		t.Fatal("blocked after jbcontext in the window")
	}
}

func shortWait(t *testing.T) {
	old := searchFirstWait
	searchFirstWait = 150 * time.Millisecond
	t.Cleanup(func() { searchFirstWait = old })
}

func TestSearchFirstWaitsForAConcurrentSiblingLookup(t *testing.T) {
	shortWait(t)
	repo := sfEnv(t, true)
	done := make(chan int, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		code, _ := sfPre(t, repo, `jbcontext search "toast"`)
		done <- code
	}()
	if code, msg := sfPre(t, repo, "rg -n 'refund retry' src"); code != 0 {
		t.Errorf("rg with a concurrent jbcontext sibling was blocked: %q", msg)
	}
	if <-done != 0 {
		t.Error("jbcontext blocked")
	}

	repo = sfEnv(t, true)
	sfPre(t, repo, `jbcontext search "toast"`)
	if code, _ := sfPre(t, repo, "rg -n 'refund retry' src"); code != 0 {
		t.Error("rg after an earlier jbcontext was blocked")
	}

	repo = sfEnv(t, true)
	start := time.Now()
	if code, _ := sfPre(t, repo, "rg -n 'refund retry' src"); code != 2 {
		t.Error("control: nothing concurrent must block")
	}
	if time.Since(start) < searchFirstWait {
		t.Error("the block did not wait for a sibling")
	}
	start = time.Now()
	sfPre(t, repo, "rg -n 'refund retry' src")
	if time.Since(start) >= searchFirstWait {
		t.Error("the retry waited again")
	}
}

func TestPiSearchFirstWaitsForAConcurrentSiblingLookup(t *testing.T) {
	shortWait(t)
	repo := sfEnv(t, true)
	run := func(command string) PiResponse {
		return PiPre(piEvent(t, map[string]any{"event": "tool_call", "session": "p1", "cwd": repo, "tool": "bash",
			"input": map[string]any{"command": command}, "resultHook": true}))
	}
	done := make(chan PiResponse, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		done <- run(`jbcontext search "toast"`)
	}()
	if res := run("rg -n 'refund retry' src"); res.Block {
		t.Errorf("rg with a concurrent jbcontext sibling was blocked: %q", res.Reason)
	}
	<-done
}

func TestSearchFirstAllCapsWordsAreNotSymbols(t *testing.T) {
	shortWait(t)
	repo := sfEnv(t, true)
	for _, p := range []string{"FAIL", "RESULT|FAIL|SUMMARY"} {
		if code, msg := sfPre(t, repo, "rg -n '"+p+"' src"); code != 0 {
			t.Errorf("%s blocked: %q", p, msg)
		}
	}
	// An ALL_CAPS name is as often an env var, which orch lsp cannot find.
	if code, msg := sfPre(t, repo, "rg -n MAX_RETRIES src"); code != 0 {
		t.Errorf("MAX_RETRIES blocked: %q", msg)
	}
}

func TestSearchFirstJudgesEveryRgSegmentAndNamesAllIdentifiers(t *testing.T) {
	shortWait(t)
	type want struct {
		block bool
		has   []string
		lacks []string
	}
	cases := []struct {
		name, line string
		want       want
	}{
		{"mixed first segment", `cd REPO && rg -n "func.*applyRefund|refund_unmatched|refund.unmatched" internal/crm docs/crm | head -30; rg -n "MaxAttempts|NextRetry|RetryPolicy" internal/crm internal/platform --glob '!*_test.go' | head -30`,
			want{true, []string{"orch lsp def MaxAttempts NextRetry RetryPolicy"}, []string{"jbcontext search"}}},
		{"several names", `rg -n "MaxAttempts|NextRetry|RetryPolicy" internal`,
			want{true, []string{"orch lsp def MaxAttempts NextRetry RetryPolicy", "orch lsp refs MaxAttempts"}, []string{"jbcontext search"}}},
		{"func prefix", `rg 'func.*applyRefund'`, want{true, []string{"orch lsp def applyRefund"}, nil}},
		{"type prefix", `rg 'type Gateway\b'`, want{true, []string{"orch lsp def Gateway"}, nil}},
		{"qualified names", `rg 'pay\.Gateway|Stripe.StartCheckout' src`,
			want{true, []string{"orch lsp def pay.Gateway Stripe.StartCheckout"}, []string{"jbcontext search"}}},
		{"piped stdin", `git log | rg foo`, want{false, nil, nil}},
		{"bare caps", `rg -n FAIL | rg PASS`, want{false, nil, nil}},
		{"doc only", `rg -n 'ErrNotFound|ErrConflict' -g '*.md'`, want{false, nil, nil}},
		{"second segment", `ls; rg -n FooBar src`, want{true, []string{"orch lsp def FooBar"}, nil}},
		{"first not candidate, second is", `rg -F toast src; rg -n 'RetryPolicy' src`, want{true, []string{"orch lsp def RetryPolicy"}, nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := sfEnv(t, true)
			code, msg := sfPre(t, repo, strings.ReplaceAll(c.line, "REPO", repo))
			if (code == 2) != c.want.block {
				t.Fatalf("exit %d, want block=%v: %q", code, c.want.block, msg)
			}
			for _, h := range c.want.has {
				if !strings.Contains(msg, h) {
					t.Errorf("reason lacks %q: %q", h, msg)
				}
			}
			for _, l := range c.want.lacks {
				if strings.Contains(msg, l) {
					t.Errorf("reason has %q: %q", l, msg)
				}
			}
		})
	}
}

func TestPiLadderNoteSkipsRgTheRuleCoversButKeepsOtherSearches(t *testing.T) {
	repo := sfEnv(t, true)
	hasLadder := func(command string) bool {
		res := PiCoach(piEvent(t, map[string]any{"event": "tool_result", "session": "p1", "cwd": repo, "tool": "bash", "turn": 1,
			"toolCallId": "c1", "input": map[string]any{"command": command}, "content": []string{"x"}}))
		return strings.Contains(strings.Join(res.AppendText, "\n"), "Lookup order:")
	}
	if hasLadder("rg -n 'refund retry' src") {
		t.Error("ladder note on an rg the rule judged")
	}
	if !hasLadder("fd toast") {
		t.Error("no ladder note on fd")
	}
	if !hasLadder("rg -F toast src") {
		t.Error("no ladder note on an rg the rule did not judge")
	}
}
