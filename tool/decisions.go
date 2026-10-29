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

// decisionError tags a failure with the reason the decision record keeps.
type decisionError struct {
	Reason string
	Err    error
}

func (e *decisionError) Error() string { return e.Err.Error() }
func (e *decisionError) Unwrap() error { return e.Err }

// Adjudication experiments use the same typed decisions boundary with a shared
// run deadline. Existing shadow and triage callers retain their timeout/routing.
func postDecisionsWithContext(ctx context.Context, p profile, state, questions map[string]any) (map[string]decAnswer, string, shadowUsage, error) {
	raw, model, u, err := postDecisionsRawWithContext(ctx, p, state, questions)
	if err != nil {
		return nil, "", u, err
	}
	answers := make(map[string]decAnswer, len(raw))
	for k, r := range raw {
		var a decAnswer
		if err := json.Unmarshal(r, &a); err != nil {
			return nil, "", u, fmt.Errorf("decisions API reply is not JSON: %v", err)
		}
		answers[k] = a
	}
	return answers, model, u, nil
}

// postDecisionsRawWithContext returns each answer as the JSON the provider sent,
// so a caller can tell a missing key from a zero value.
func postDecisionsRawWithContext(ctx context.Context, p profile, state, questions map[string]any) (map[string]json.RawMessage, string, shadowUsage, error) {
	var u shadowUsage
	key, err := providerKey(p.Provider)
	if err != nil {
		return nil, "", u, &decisionError{"no_key", err}
	}
	body := decisionBody(p.Model, state, questions)
	endpoint := envOr("ORCH_TRIAGE_URL", triageURLDefault)
	if p.Provider != "openrouter" {
		endpoint = os.Getenv("ORCH_" + strings.ToUpper(strings.ReplaceAll(p.Provider, "-", "_")) + "_DECISIONS_URL")
		if endpoint == "" {
			return nil, "", u, &decisionError{"no_endpoint", fmt.Errorf("%s decisions connector has no endpoint", p.Provider)}
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
		return nil, "", u, &decisionError{fmt.Sprintf("http_%d", resp.StatusCode), fmt.Errorf("decisions API returned %d: %s", resp.StatusCode, short(strings.TrimSpace(string(raw)), 200))}
	}
	var r struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   shadowUsage                `json:"usage"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, "", u, &decisionError{"bad_reply", fmt.Errorf("decisions API reply is not JSON: %v", err)}
	}
	return r.Answers, r.Model, r.Usage, nil
}

func decisionBody(model string, state, questions map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"model": model, "state": state, "questions": questions})
	return b
}
