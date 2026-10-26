package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const shadowTimeout = 90 * time.Second

type shadowUsage struct {
	Cost   float64 `json:"cost"`
	Tokens int     `json:"total_tokens"`
}

func postDecisionsWith(p profile, state, questions map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
	return postDecisionsWithContext(context.Background(), p, state, questions)
}

// Adjudication experiments use the same typed decisions boundary with a shared
// run deadline. Existing shadow and triage callers retain their timeout/routing.
func postDecisionsWithContext(ctx context.Context, p profile, state, questions map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
	var u shadowUsage
	key, err := providerKey(p.Provider)
	if err != nil {
		return nil, "", u, err
	}
	body := decisionBody(p.Model, state, questions)
	endpoint := envOr("ORCH_TRIAGE_URL", triageURLDefault)
	if p.Provider != "openrouter" {
		endpoint = os.Getenv("ORCH_" + strings.ToUpper(strings.ReplaceAll(p.Provider, "-", "_")) + "_DECISIONS_URL")
		if endpoint == "" {
			return nil, "", u, fmt.Errorf("%s decisions connector has no endpoint", p.Provider)
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", u, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: shadowTimeout}).Do(req)
	if err != nil {
		return nil, "", u, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, "", u, fmt.Errorf("decisions API returned %d: %s", resp.StatusCode, short(strings.TrimSpace(string(raw)), 200))
	}
	var r struct {
		Model   string               `json:"model"`
		Answers map[string]decAnswer `json:"answers"`
		Usage   shadowUsage          `json:"usage"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, "", u, fmt.Errorf("decisions API reply is not JSON: %v", err)
	}
	return r.Answers, r.Model, r.Usage, nil
}

func decisionBody(model string, state, questions map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"model": model, "state": state, "questions": questions})
	return b
}
