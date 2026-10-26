package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

const herdrTimeout = 60 * time.Second

type herdrAgent struct {
	PaneID  string `json:"pane_id"`
	TabID   string `json:"tab_id"`
	Agent   string `json:"agent"`
	Status  string `json:"agent_status"`
	Session struct {
		Value string `json:"value"`
	} `json:"agent_session"`
}

func herdrCmd(timeout time.Duration, args ...string) ([]byte, error) {
	return herdrCmdContext(context.Background(), timeout, args...)
}

func herdrCmdContext(parent context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, envOr("ORCH_HERDR", "herdr"), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	what := strings.Join(args[:min(2, len(args))], " ")
	if parent.Err() != nil {
		return out, parent.Err()
	}
	if ctx.Err() != nil {
		return out, fail("herdr %s timed out after %s", what, timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return out, fail("herdr %s failed: %s", what, msg)
	}
	return out, nil
}

func herdrJSON(v any, timeout time.Duration, args ...string) error {
	out, err := herdrCmd(timeout, args...)
	if err != nil || v == nil || len(bytes.TrimSpace(out)) == 0 {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fail("herdr %s: unexpected output: %v", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return nil
}

// agentMap is every agent Herdr knows, by pane id.
func agentMap() (map[string]herdrAgent, error) {
	var res struct {
		Result struct {
			Agents []herdrAgent `json:"agents"`
		} `json:"result"`
	}
	if err := herdrJSON(&res, herdrTimeout, "agent", "list"); err != nil {
		return nil, err
	}
	m := make(map[string]herdrAgent, len(res.Result.Agents))
	for _, ag := range res.Result.Agents {
		m[ag.PaneID] = ag
	}
	return m, nil
}

// paneExists tells a closed pane apart from Herdr being unreachable.
func paneExists(id string) (bool, error) {
	_, err := herdrCmd(herdrTimeout, "pane", "get", id)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "pane_not_found") {
		return false, nil
	}
	return false, err
}
