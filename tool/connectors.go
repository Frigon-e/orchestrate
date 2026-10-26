package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

func (p profile) lane() string {
	if p.Harness == "api" {
		return "pi:" + p.Provider
	}
	return lane(p.Harness, p.Model)
}

func providerKey(provider string) (string, error) {
	vars := []string{strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_API_KEY"}
	if provider == "openrouter" {
		vars = append([]string{"ORCH_TRIAGE_KEY"}, vars...)
	}
	for _, name := range vars {
		if key := os.Getenv(name); key != "" {
			return key, nil
		}
	}
	if !installed("pi") {
		return "", fmt.Errorf("%s key empty", provider)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, runnerBinary("pi"), "auth", "print-bearer-token", "--provider", provider).Output()
	if key := strings.TrimSpace(string(out)); err == nil && key != "" {
		return key, nil
	}
	return "", fmt.Errorf("%s key empty", provider)
}

func providerCredentialReady(provider string) bool {
	name := strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_API_KEY"
	if os.Getenv(name) != "" || (provider == "openrouter" && os.Getenv("ORCH_TRIAGE_KEY") != "") {
		return true
	}
	if !installed("pi") {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, runnerBinary("pi"), "auth", "check", "--provider", provider, "--json", "--no-refresh").Output()
	var status struct {
		Status string `json:"status"`
	}
	return err == nil && json.Unmarshal(out, &status) == nil && status.Status == "ready"
}

type keyReadiness struct {
	state        string
	lane         string
	unknownScope bool
	err          error
}

func openRouterReadiness() keyReadiness {
	result := keyReadiness{state: "ok", lane: "pi:openrouter", unknownScope: true}
	key, err := providerKey("openrouter")
	if err != nil {
		result.state = "key empty"
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", envOr("ORCH_OPENROUTER_KEY_URL", "https://openrouter.ai/api/v1/key"), nil)
	if err != nil {
		result.err = err
		return result
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		result.err = err
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		result.state = "key empty"
		return result
	}
	if resp.StatusCode != 200 {
		result.err = fmt.Errorf("key check returned %d", resp.StatusCode)
		return result
	}
	var body struct {
		Data struct {
			Remaining        *float64 `json:"limit_remaining"`
			AllowedModels    []string `json:"allowed_models"`
			AllowedProviders []string `json:"allowed_providers"`
		} `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		result.err = err
		return result
	}
	// A label or account name is not evidence of model restrictions. The live
	// endpoint may omit scope entirely; never infer it from the configured model.
	restricted := len(body.Data.AllowedModels) > 0
	for _, model := range body.Data.AllowedModels {
		restricted = restricted && strings.Contains(model, "anthropic/")
	}
	if len(body.Data.AllowedProviders) == 1 && body.Data.AllowedProviders[0] == "anthropic" {
		restricted = true
	}
	if restricted {
		result.lane = "pi:openrouter:anthropic"
		result.unknownScope = false
	}
	if body.Data.Remaining != nil && *body.Data.Remaining <= 0 {
		result.state = "key empty"
	}
	return result
}

func (r *roleRun) providerReadiness(provider string) keyReadiness {
	if r.keys == nil {
		r.keys = map[string]keyReadiness{}
	}
	if key, ok := r.keys[provider]; ok {
		return key
	}
	result := keyReadiness{state: "ok", lane: "pi:" + provider}
	if provider == "openrouter" {
		result = openRouterReadiness()
	} else if !providerCredentialReady(provider) {
		result.state = "key empty"
	}
	r.keys[provider] = result
	return result
}

// adjudicationTransport is deliberately separate from triage routing. The
// decisions endpoint has native object state and finite typed choices, not chat
// or arbitrary explanation fields. Explanations must be selected from complete
// recorded proposals; adapters must not fabricate missing support.
func adjudicationTransport(p profile) (string, error) {
	switch p.Harness {
	case "pi", "claude":
		return "text", nil
	case "api":
		if p.Provider == "openrouter" || os.Getenv("ORCH_"+strings.ToUpper(strings.ReplaceAll(p.Provider, "-", "_"))+"_DECISIONS_URL") != "" {
			return "decisions", nil
		}
		return "", fmt.Errorf("%s decisions connector has no endpoint", p.Provider)
	}
	return "", fmt.Errorf("unsupported adjudication connector %q", p.Harness)
}

func laneWithin(name, parent string) bool {
	return name == parent || strings.HasPrefix(name, parent+":")
}
