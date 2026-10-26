package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectorLanesAndParentLimits(t *testing.T) {
	h := setup(t)
	cases := map[string]string{"openrouter/~deepseek/flash": "pi:openrouter", "openrouter/anthropic/claude": "pi:openrouter:anthropic", "openai/gpt-x": "pi:openai", "anthropic/claude": "pi:anthropic", "other/model": "pi:other", "flash": "pi"}
	for model, want := range cases {
		if got := lane("pi", model); got != want {
			t.Fatalf("%s: %s != %s", model, got, want)
		}
	}
	h.must("health", "set", "pi:openrouter", "limited")
	cur := loadHealth(h.now)
	for _, name := range []string{"pi:openrouter", "pi:openrouter:anthropic"} {
		if parent, _, ok := limitedBy(cur, name); !ok || parent != "pi:openrouter" {
			t.Fatalf("parent %s: %s %v", name, parent, ok)
		}
	}
	if _, _, ok := limitedBy(cur, "pi:openai"); ok {
		t.Fatal("OpenRouter affected OpenAI")
	}
	h.must("health", "set", "pi-anthropic", "limited")
	cur = loadHealth(h.now)
	if _, ok := cur["pi:anthropic"]; !ok {
		t.Fatal("legacy alias not canonical")
	}
	wantContains(t, h.must("health"), "pi:openrouter:", "pi:openrouter:anthropic:", "pi:anthropic:")
	h.must("health", "set", "pi", "limited")
	for _, name := range []string{"pi:openai", "pi:other"} {
		if _, _, ok := limitedBy(loadHealth(h.now), name); !ok {
			t.Fatalf("pi parent missed %s", name)
		}
	}
}

func TestConnectorConditionalAllowLane(t *testing.T) {
	h := setup(t)
	cfg := config{userConfig: userConfig{AllowedModels: map[string][]string{"claude": {"sonnet"}}, AllowedWhenLimited: []limitedAllow{{Harness: "pi", Lane: "pi:openrouter", Allow: map[string][]string{"claude": {"haiku"}}}}}}
	if err := cfg.modelAllowed("claude", "haiku", h.now); err == nil {
		t.Fatal("emergency model allowed without limit")
	}
	h.must("health", "set", "pi:openai", "limited")
	if err := cfg.modelAllowed("claude", "haiku", h.now); err == nil {
		t.Fatal("wrong connector enabled model")
	}
	h.must("health", "set", "pi:openrouter", "limited")
	if err := cfg.modelAllowed("claude", "haiku", h.now); err != nil {
		t.Fatal(err)
	}
}

func TestAPIDeciderDoesNotNeedPiOrPiAllowlist(t *testing.T) {
	h := setup(t)
	h.task("a")
	t.Setenv("ORCH_PI", filepath.Join(h.tmp, "missing-pi"))
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(`[allowed_models]
claude = ["sonnet"]
[roles.decider]
use = [{harness="api",provider="openrouter",model="perplexity/pplx-decider-v1.2-20261016"}]
`), 0o644)
	wantContains(t, h.must("triage", "a"), "a: settled")
	if h.dec.count() != 1 {
		t.Fatalf("API calls: %d", h.dec.count())
	}
	h.must("health", "set", "pi:openai", "limited")
	h.must("triage", "a", "--force")
	h.must("health", "set", "pi:openrouter", "limited")
	out := h.must("triage", "a", "--force")
	wantContains(t, out, "skipped: decider off", "limited (pi:openrouter)")
	if h.dec.count() != 2 {
		t.Fatal("limited API was called")
	}
	recs := readTriage(filepath.Dir(h.dir("a")))
	if recs[len(recs)-1].Skipped != "decider off" || recs[len(recs)-1].Tier != "" {
		t.Fatalf("skip invented a tier: %+v", recs[len(recs)-1])
	}
}

func TestOpenRouterUnknownScopeAndKnownRestriction(t *testing.T) {
	h := setup(t)
	for _, tc := range []struct {
		name, body, scope string
		unknown           bool
	}{
		{"unknown", `{"data":{"limit_remaining":0,"budgets":[],"effective_budget":null}}`, "pi:openrouter", true},
		{"restricted", `{"data":{"limit_remaining":0,"allowed_models":["anthropic/claude"]}}`, "pi:openrouter:anthropic", false},
		{"mixed", `{"data":{"limit_remaining":0,"allowed_models":["anthropic/claude","deepseek/flash"]}}`, "pi:openrouter", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			t.Setenv("ORCH_OPENROUTER_KEY_URL", srv.URL)
			key := openRouterReadiness()
			if key.state != "key empty" || key.lane != tc.scope || key.unknownScope != tc.unknown {
				t.Fatalf("%+v", key)
			}
			r, _ := roleSite(profile{Harness: "pi", Model: gateModelDef}, profile{Harness: "pi", Model: "anthropic/claude"}).newRoleRun("gate", modelOptions{})
			choices, _ := r.available()
			want := 1
			if !tc.unknown {
				want = 2
			}
			if len(choices) != want {
				t.Fatalf("choices %v", choices)
			}
			var out bytes.Buffer
			printRoleReadiness(&out, site{})
			if tc.unknown {
				wantContains(t, out.String(), "unknown scope", "orch health set pi:openrouter limited --for 3h")
			}
		})
	}
	_ = h
}

func TestAutoOffAndExplicitTierSkipTriage(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(autoConfig+"\n[roles.decider]\nenabled = false\n"), 0o644)
	h.task("a", "--harness", "auto", "--model", "auto", "--effort", "auto")
	out := h.must("spawn", "a", "--auto")
	wantContains(t, out, "triage is off", "choose a profile by hand (orch spawn a --harness H --model M)", "pass --tier settled|standard|planning")
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("off message is not one line: %s", out)
	}
	wantContains(t, h.must("spawn", "a", "--auto", "--tier", "standard"), "a is standard work (user-selected tier)", "would use: claude sonnet")
	h.must("spawn", "a", "--auto", "--tier", "standard", "--apply")
	if h.dec.count() != 0 {
		t.Fatal("--tier called Decider")
	}
	fm, _, _ := readTask(filepath.Join(h.dir("a"), "task.md"))
	if fm["tier"] != "standard" {
		t.Fatal(fm)
	}
	h.task("b", "--harness", "auto", "--model", "auto", "--effort", "auto")
	h.must("spawn", "b", "--harness", "claude", "--model", "sonnet")
	if h.dec.count() != 0 {
		t.Fatal("manual profile called Decider")
	}
	if code, _ := h.run("spawn", "b", "--auto", "--tier", "easy"); code == 0 {
		t.Fatal("invalid tier accepted")
	}
}

func TestAutoTierSkipsOnlyLimitedConnector(t *testing.T) {
	h := setup(t)
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(`[[dispatch]]
when = "settled"
tier = "settled"
use = [{harness="pi",model="openrouter/deepseek/flash"},{harness="pi",model="openai/gpt-x"},{harness="claude",model="haiku"}]
`), 0o644)
	h.task("a", "--harness", "auto", "--model", "auto", "--effort", "auto")
	h.must("health", "set", "pi:openrouter", "limited")
	wantContains(t, h.must("spawn", "a", "--auto", "--tier", "settled"), "pi:openrouter is limited", "would use: pi openai/gpt-x")
	if h.dec.count() != 0 {
		t.Fatal("manual tier called API")
	}
}

func TestQuickOffShadowAndEnforce(t *testing.T) {
	h := setup(t)
	quickRepo(t, h, "test -f slug.py")
	cfg := "quick_mode = \"shadow\"\n" + autoConfig + "\n[roles.decider]\nenabled = false\n"
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(cfg), 0o644)
	h.must("quick", "q1", "--owns", "slug.py", "make slugify in slug.py return an empty string when no letter or digit remains")
	wantContains(t, string(mustRead(t, filepath.Join(h.dir("q1"), "status.log"))), "skipped: decider off")
	recs := readTriage(filepath.Dir(h.dir("q1")))
	if len(recs) != 1 || recs[0].Skipped != "decider off" || recs[0].Tier != "" {
		t.Fatalf("records: %+v", recs)
	}
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(strings.Replace(cfg, "quick_mode = \"shadow\"", "quick_mode = \"enforce\"", 1)), 0o644)
	code, out := h.run("quick", "q2", "--owns", "slug.py", "make slugify in slug.py return an empty string when no letter or digit remains")
	if code != 1 {
		t.Fatalf("enforce passed: %d %s", code, out)
	}
	wantContains(t, out, "triage is off", "choose a profile by hand")
	if exists(h.dir("q2")) {
		t.Fatal("enforce left a task")
	}
	if h.dec.count() != 0 {
		t.Fatal("disabled API called")
	}
}

func TestProviderCredentialsUseReadOnlyAuthCheck(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("ORCH_PI", fakeRunner(t, "pi", fmt.Sprintf("printf '%%s\\n' \"$*\" >> %q\nprintf '{\"status\":\"ready\"}'", log)))
	for _, provider := range []string{"openai", "anthropic"} {
		if !providerCredentialReady(provider) {
			t.Fatal(provider)
		}
	}
	calls := string(mustRead(t, log))
	if strings.Count(calls, "--no-refresh") != 2 || strings.Contains(calls, "--credentials") {
		t.Fatal(calls)
	}
}

func TestAPIProviderAndLegacyModelAreConfigurable(t *testing.T) {
	h := setup(t)
	h.task("a")
	t.Setenv("OTHER_API_KEY", "other-key")
	t.Setenv("ORCH_OTHER_DECISIONS_URL", os.Getenv("ORCH_TRIAGE_URL"))
	os.WriteFile(os.Getenv("ORCH_CONFIG"), []byte(`[roles.decider]
use = [{harness="api",provider="other",model="other/model"}]
`), 0o644)
	h.must("triage", "a")
	if h.dec.auth != "Bearer other-key" {
		t.Fatal("wrong connector credential")
	}
	cfg, err := loadConfig(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TriageModel = "legacy/model"
	r, _ := (site{cfg: cfg}).newRoleRun("decider", modelOptions{})
	if r.choices[0].Provider != "other" || r.choices[0].Model != "legacy/model" {
		t.Fatal(r.choices)
	}
}
