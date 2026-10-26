package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCtxOnlyActivation models a successfully loaded guard, not model output.
// Tests of extension-load failure deliberately omit this acknowledgement.
const fakeCtxOnlyActivation = `[ -n "$ORCH_CTX_ONLY_READY" ] && printf 'ctx-only\n' > "$ORCH_CTX_ONLY_READY"
`

func fakeRunner(t *testing.T, name, script string) string {
	t.Helper()
	if name == "pi" {
		script = fakeCtxOnlyActivation + script
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func roleSite(choices ...profile) site {
	return site{cfg: config{userConfig: userConfig{Roles: map[string]modelRole{"gate": {Use: choices}}}}}
}

func TestRoleConfigAndClaudeOnlyDefaults(t *testing.T) {
	h := setup(t)
	t.Setenv("ORCH_PI", filepath.Join(h.tmp, "missing-pi"))
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "exit 0"))
	t.Setenv("ORCH_TRIAGE_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	s := site{}
	for _, name := range []string{"gate", "spec_queries"} {
		r, _ := s.newRoleRun(name, modelOptions{})
		ps, skips := r.available()
		if len(ps) != 1 || ps[0].Harness != "claude" {
			t.Fatalf("%s: %v %v", name, ps, skips)
		}
	}
	for _, name := range []string{"decider"} {
		r, _ := s.newRoleRun(name, modelOptions{})
		if ps, _ := r.available(); len(ps) != 0 {
			t.Fatalf("%s unexpectedly available: %v", name, ps)
		}
	}
	path := filepath.Join(h.tmp, "roles.toml")
	t.Setenv("ORCH_CONFIG", path)
	os.WriteFile(path, []byte(`[roles.gate]
use = [{harness="claude", model="haiku", effort="low"}]
[roles.decider]
enabled = false
`), 0o644)
	cfg, err := loadConfig(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles["decider"].Enabled == nil || *cfg.Roles["decider"].Enabled {
		t.Fatal("disabled lost in TOML")
	}
	r, _ := (site{cfg: cfg}).newRoleRun("decider", modelOptions{})
	if !strings.Contains(r.unavailable().Error(), "disabled") {
		t.Fatal(r.unavailable())
	}
}

func TestRemovedShadowRoleIsIgnoredOnLoad(t *testing.T) {
	h := setup(t)
	path := filepath.Join(h.tmp, "roles.toml")
	t.Setenv("ORCH_CONFIG", path)
	raw := "reuse_shadow = \"all\"\n[roles.shadow_flash]\nuse = [{harness=\"api\", provider=\"openrouter\", model=\"test-model\"}]\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(h.repo)
	if err != nil {
		t.Fatalf("removed keys must load without error: %v", err)
	}
	if _, ok := cfg.Roles["shadow_flash"]; ok {
		t.Fatal("removed role kept after load")
	}
}

func TestRoleAdjudicatorAPIConfigIsScoped(t *testing.T) {
	h := setup(t)
	path := filepath.Join(h.tmp, "adjudicator-roles.toml")
	t.Setenv("ORCH_CONFIG", path)
	for _, tc := range []struct {
		name, harness, provider string
		want                    bool
	}{
		{"decider", "api", "openrouter", true},
		{"gate_adjudicator", "api", "openrouter", true},
		{"gate_adjudicator", "claude", "", true},
		{"gate", "api", "openrouter", false},
		{"spec_queries", "api", "openrouter", false},
		{"decider", "claude", "", false},
		{"gate_adjudicator", "api", "", false},
		{"gate_adjudicator", "api", "bad/provider", false},
	} {
		t.Run(tc.name+"-"+tc.harness+"-"+strings.ReplaceAll(tc.provider, "/", "-"), func(t *testing.T) {
			raw := fmt.Sprintf("[roles.%s]\nenabled = false\nuse = [{harness=%q, provider=%q, model=\"test-model\"}]\n", tc.name, tc.harness, tc.provider)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(h.repo)
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want %v: %v", err == nil, tc.want, err)
			}
			if err == nil {
				r, _ := (site{cfg: cfg}).newRoleRun(tc.name, modelOptions{Runner: "claude:override"})
				if r.enabled {
					t.Fatal("parsed disabled role enabled by override")
				}
			}
		})
	}
	raw := `[roles.gate_adjudicator]
enabled = false
use = [{harness="api", provider="openrouter", model="test-model"}]
[roles.gate_adjudicator.budget]
requests = 7
input_tokens = 40000
output_tokens = 12000
seconds = 300
repairs = 1
fallbacks = 1
output_reserve = 4000
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	budget := cfg.Roles[adjudicatorRole].Budget
	if budget == nil || budget.Requests != 7 || budget.InputTokens != 40000 || budget.OutputReserve != 4000 {
		t.Fatal("role budget lost in machine config", budget)
	}
}

func TestRoleOrderedFilteringOverrideAndFallback(t *testing.T) {
	t.Setenv("ORCH_HEALTH", filepath.Join(t.TempDir(), "health.json"))
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", "exit 0"))
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "exit 0"))
	s := roleSite(profile{Harness: "pi", Model: "first"}, profile{Harness: "claude", Model: "haiku"}, profile{Harness: "pi", Model: "third"})
	r, _ := s.newRoleRun("gate", modelOptions{})
	calls := []string{}
	fn := func(p profile) (string, string, error) {
		calls = append(calls, p.Model)
		if p.Model == "first" {
			return "", "", errors.New("HTTP 429 rate limit")
		}
		return "answer", "", nil
	}
	out, _, used, err := r.run(fn)
	if err != nil || out != "answer" || used.Model != "haiku" {
		t.Fatalf("%q %v %v", out, used, err)
	}
	r.run(fn)
	if strings.Join(calls, ",") != "first,haiku,haiku" {
		t.Fatal(calls)
	}
	if _, err := os.Stat(healthPath()); !os.IsNotExist(err) {
		t.Fatal("runner wrote health")
	}
	_, skips := r.available()
	if !strings.Contains(strings.Join(skips, " "), "orch health set pi limited --for 3h") {
		t.Fatal(skips)
	}
	r, _ = s.newRoleRun("gate", modelOptions{Runner: "claude:sonnet:low,pi:other"})
	if r.choices[0].Model != "sonnet" || r.choices[0].Effort != "low" {
		t.Fatal(r.choices)
	}
	for _, text := range []string{"wat:x", "pi", "claude:", "pi:x:low:bad"} {
		if _, err := parseRunners(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestRoleHealthAllowedAndDegraded(t *testing.T) {
	t.Setenv("ORCH_HEALTH", filepath.Join(t.TempDir(), "health.json"))
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", "exit 0"))
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "exit 0"))
	s := roleSite(profile{Harness: "pi", Model: "main"}, profile{Harness: "claude", Model: "haiku", Degraded: true}, profile{Harness: "claude", Model: "sonnet"})
	s.cfg.AllowedModels = map[string][]string{"pi": {"main"}, "claude": {"haiku"}}
	saveHealth(map[string]healthEntry{"pi": {State: "limited", Since: time.Now().Unix()}})
	r, _ := s.newRoleRun("gate", modelOptions{})
	if ps, _ := r.available(); len(ps) != 0 {
		t.Fatal(ps)
	}
	_, skips := r.available()
	for _, want := range []string{"limited (pi)", "degraded", "not allowed"} {
		if !strings.Contains(strings.Join(skips, " "), want) {
			t.Fatal(skips)
		}
	}
	r, _ = s.newRoleRun("gate", modelOptions{AllowDegraded: true})
	if ps, _ := r.available(); len(ps) != 1 || ps[0].Model != "haiku" {
		t.Fatal(ps)
	}
}

func TestProviderDownDoesNotRetryPassLimits(t *testing.T) {
	for _, err := range []error{&passLimit{"429 timed out"}, errors.New("bad JSON"), errors.New("pass stalled")} {
		if providerDown(err) {
			t.Errorf("provider-down: %v", err)
		}
	}
	for _, text := range []string{"402 credit depleted", "429 rate limit", "authentication failed", "usage limit exceeded", "HTTP 503", "executable file not found in PATH"} {
		if !providerDown(errors.New(text)) {
			t.Errorf("not provider-down: %s", text)
		}
	}
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", `printf '{"status":"ready"}'`))
	r, _ := roleSite(profile{Harness: "pi", Model: "one/a"}, profile{Harness: "pi", Model: "two/b"}, profile{Harness: "pi", Model: "three/c"}).newRoleRun("gate", modelOptions{})
	n := 0
	_, _, _, err := r.run(func(profile) (string, string, error) { n++; return "", "", errors.New("HTTP 502") })
	if n != 2 || err == nil {
		t.Fatalf("calls %d: %v", n, err)
	}
}

func TestAnthropicLaneAndCreditDoesNotDisableDeepSeek(t *testing.T) {
	t.Setenv("ORCH_HEALTH", filepath.Join(t.TempDir(), "health.json"))
	for model, want := range map[string]string{"anthropic/claude-x": "pi:anthropic", "openrouter/anthropic/claude-x": "pi:openrouter:anthropic", "openrouter/~anthropic/claude-x": "pi:openrouter:anthropic"} {
		if got := lane("pi", model); got != want {
			t.Fatalf("%s: %s", model, got)
		}
	}
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", "printf token"))
	t.Setenv("ORCH_TRIAGE_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("bad key request: %v", r)
		}
		fmt.Fprint(w, `{"data":{"limit_remaining":0,"allowed_models":["anthropic/claude-x"]}}`)
	}))
	defer srv.Close()
	t.Setenv("ORCH_OPENROUTER_KEY_URL", srv.URL)
	s := roleSite(profile{Harness: "pi", Model: "openrouter/anthropic/claude-x"}, profile{Harness: "pi", Model: gateModelDef})
	r, _ := s.newRoleRun("gate", modelOptions{})
	ps, skips := r.available()
	if len(ps) != 1 || ps[0].Model != gateModelDef || !strings.Contains(strings.Join(skips, " "), "key empty") {
		t.Fatalf("%v %v", ps, skips)
	}
	var b bytes.Buffer
	printRoleReadiness(&b, s)
	if !strings.Contains(b.String(), "key empty") || !strings.Contains(b.String(), gateModelDef+": ok") {
		t.Fatal(b.String())
	}
}

func TestClaudeOnlyGateRecordsRunnerAndReadOnlyTools(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	log := filepath.Join(h.tmp, "claude-args")
	reply, _ := json.Marshal(map[string]any{"type": "result", "result": `{"packages":[{"id":"api","claims":[],"questions":[],"will_change":[],"reuse":[]}],"findings":[]}`})
	bin := fakeRunner(t, "claude", fmt.Sprintf("printf '%%s\\n' \"$@\" > %q.$$.args\nprintf '%%s\\n' '%s'", log, reply))
	readArgs := func() string {
		files, err := filepath.Glob(log + ".*.args")
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, file := range files {
			b.Write(mustRead(t, file))
		}
		return b.String()
	}
	t.Setenv("ORCH_CLAUDE", bin)
	t.Setenv("ORCH_PI", filepath.Join(h.tmp, "missing-pi"))
	t.Setenv("HERDR_ENV", "")
	out := h.must("gate", "spec.md", "--ctx-tool", "off")
	rec := readRecord(t, gdir)
	if rec.State != "pass" || len(rec.PassRunners) != 3 {
		t.Fatalf("%s: %s %v", out, rec.State, rec.PassRunners)
	}
	for _, runner := range rec.PassRunners {
		if runner != "claude haiku low" {
			t.Fatal(runner)
		}
	}
	args := readArgs()
	for _, want := range []string{"Read,Grep,Glob", "dontAsk", "--strict-mcp-config", "--effort\nlow"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q: %s", want, args)
		}
	}
	for _, bad := range []string{"Bash", "Edit", "Write", "--dangerously-skip-permissions"} {
		if strings.Contains(args, bad) {
			t.Errorf("unsafe args %q", bad)
		}
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(gdir, "report.md"))), "claude haiku low") {
		t.Fatal("runner missing from report")
	}
	s := site{repo: h.repo, cfg: config{userConfig: userConfig{Roles: map[string]modelRole{"spec_queries": {Use: []profile{{Harness: "claude", Model: "haiku"}}}}}}}
	if _, err := s.runModel("spec_queries", "queries"); err != nil {
		t.Fatal(err)
	}
	args = readArgs()
	if !strings.Contains(args, "--tools\n\n") {
		t.Fatal("runModel has tools")
	}
}

func TestClaudePaneGateUsesClaudeKindAndTranscript(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	h.sessionPi(gatePasses(nil))
	t.Setenv("ORCH_FAKE_CLAUDE_REPO", h.repo)
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", "exit 0"))
	out := h.must("gate", "spec.md", "--runner", "claude:haiku:low")
	rec := readRecord(t, gdir)
	if rec.State != "pass" {
		t.Fatalf("%s: %v", out, rec.PassErrors)
	}
	calls := h.gateCalls("agent", "start")
	if len(calls) != 3 {
		t.Fatalf("starts: %d", len(calls))
	}
	for _, call := range calls {
		args := strings.Join(call.Args, " ")
		for _, want := range []string{"--kind claude", "--tools Read,Grep,Glob", "--permission-mode dontAsk", "--session-id"} {
			if !strings.Contains(args, want) {
				t.Fatalf("missing %s: %s", want, args)
			}
		}
	}
	if len(rec.PassRunners) != 3 {
		t.Fatalf("pass runners: %v", rec.PassRunners)
	}
	for _, runner := range rec.PassRunners {
		if runner != "claude haiku low" {
			t.Fatal(runner)
		}
	}
}

func TestGateProviderFailureFallsBackToFakeClaude(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	t.Setenv("HERDR_ENV", "")
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", "echo 'HTTP 429 rate limit' >&2; exit 1"))
	reply, _ := json.Marshal(map[string]any{"type": "result", "result": `{"packages":[{"id":"api","claims":[],"questions":[],"will_change":[],"reuse":[]}],"findings":[]}`})
	t.Setenv("ORCH_CLAUDE", fakeRunner(t, "claude", fmt.Sprintf("printf '%%s\\n' '%s'", reply)))
	h.must("gate", "spec.md")
	rec := readRecord(t, gdir)
	if rec.State != "pass" || len(rec.PassRunners) != 3 {
		t.Fatalf("fallback gate: state %s, runners %v, errors %v", rec.State, rec.PassRunners, rec.PassErrors)
	}
	for _, runner := range rec.PassRunners {
		if runner != "claude haiku low" {
			t.Fatal(runner)
		}
	}
	if !strings.Contains(strings.Join(rec.Notes, " "), "orch health set pi:openrouter limited --for 3h") {
		t.Fatal(rec.Notes)
	}
	if _, err := os.Stat(healthPath()); !os.IsNotExist(err) {
		t.Fatal("health was written")
	}
}

func TestDisabledDeciderSkipsTriage(t *testing.T) {
	h := setup(t)
	h.task("t1")
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte("[roles.decider]\nenabled = false\n"), 0o644)
	wantContains(t, h.must("triage", "t1"), "decider off:", "disabled")
	if h.dec.count() != 0 {
		t.Fatal("disabled role made an API request")
	}
}

func TestSessionProviderErrorAndRecoveredError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "session.jsonl")
	failure := []byte(`{"type":"message","message":{"role":"assistant","stopReason":"error","errorMessage":"HTTP 402 credits exhausted","content":[]}}` + "\n")
	os.WriteFile(file, failure, 0o644)
	if err := sessionFailure(dir); !providerDown(err) {
		t.Fatalf("session provider failure: %v", err)
	}
	os.WriteFile(file, append(failure, []byte(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`+"\n")...), 0o644)
	if err := sessionFailure(dir); err != nil {
		t.Fatal(err)
	}
}

func TestGateWithoutRunnersStillWritesChecklist(t *testing.T) {
	h := setup(t)
	gdir := h.gateRepo(1)
	t.Setenv("ORCH_PI", filepath.Join(h.tmp, "missing-pi"))
	t.Setenv("ORCH_CLAUDE", filepath.Join(h.tmp, "missing-claude"))
	t.Setenv("HERDR_ENV", "")
	code, out := h.run("gate", "spec.md")
	rec := readRecord(t, gdir)
	if code != 1 || rec.State != "incomplete" || !strings.Contains(rec.PassErrors["all"], "not available") {
		t.Fatalf("%s: %#v", out, rec)
	}
	if _, err := os.Stat(filepath.Join(gdir, "items.json")); err != nil {
		t.Fatal(err)
	}
}
