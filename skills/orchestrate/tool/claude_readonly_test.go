package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Installed-harness verification is opt-in. All model traffic uses a loopback
// mock, isolated settings and dummy credentials; no paid model is contacted.
func TestInstalledClaudeRefusesMutatingTools(t *testing.T) {
	if os.Getenv("ORCH_TEST_RUNNER_CLAUDE") != "1" {
		t.Skip("set ORCH_TEST_RUNNER_CLAUDE=1 for local mock verification")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip(err)
	}
	repo := t.TempDir()
	home := t.TempDir()
	original := filepath.Join(repo, "existing.txt")
	os.WriteFile(original, []byte("unchanged\n"), 0o600)
	names := []string{"Bash", "Edit", "Write"}
	inputs := []map[string]any{
		{"command": "printf forbidden > " + filepath.Join(repo, "bash-created.txt")},
		{"file_path": original, "old_string": "unchanged", "new_string": "changed"},
		{"file_path": filepath.Join(repo, "write-created.txt"), "content": "forbidden"},
	}
	var mu sync.Mutex
	denied := map[string]string{}
	advertised := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "count_tokens") {
			fmt.Fprint(w, `{"input_tokens":1}`)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		mu.Lock()
		for _, tool := range req.Tools {
			advertised[tool.Name] = true
		}
		for _, message := range req.Messages {
			var parts []struct {
				Type    string          `json:"type"`
				ID      string          `json:"tool_use_id"`
				IsError bool            `json:"is_error"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(message.Content, &parts) != nil {
				continue
			}
			for _, part := range parts {
				if part.Type == "tool_result" && strings.HasPrefix(part.ID, "denied_") {
					if part.IsError {
						denied[strings.TrimPrefix(part.ID, "denied_")] = string(part.Content)
					} else {
						denied[strings.TrimPrefix(part.ID, "denied_")] = "NOT AN ERROR"
					}
				}
			}
		}
		step := 0
		for step < len(names) && denied[names[step]] != "" {
			step++
		}
		mu.Unlock()
		reason := "end_turn"
		block := map[string]any{"type": "text", "text": "readonly mock complete"}
		if step < len(names) {
			reason = "tool_use"
			block = map[string]any{"type": "tool_use", "id": "denied_" + names[step], "name": names[step], "input": inputs[step]}
		}
		if !req.Stream {
			json.NewEncoder(w).Encode(map[string]any{"id": "msg_denial", "type": "message", "role": "assistant", "model": "mock", "content": []any{block}, "stop_reason": reason, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event string, value any) {
			b, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		}
		send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_denial", "type": "message", "role": "assistant", "model": "mock", "content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}})
		if step < len(names) {
			send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "denied_" + names[step], "name": names[step], "input": map[string]any{}}})
			raw, _ := json.Marshal(inputs[step])
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": string(raw)}})
		} else {
			send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "readonly mock complete"}})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
		send("message_stop", map[string]string{"type": "message_stop"})
	}))
	defer srv.Close()
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_") || strings.HasPrefix(key, "ORCH_") || key == "HOME" || key == "CLAUDECODE" {
			continue
		}
		env = append(env, value)
	}
	env = append(env, "HOME="+home, "CLAUDE_CONFIG_DIR="+filepath.Join(home, "claude"), "ANTHROPIC_BASE_URL="+srv.URL, "ANTHROPIC_API_KEY=mock", "ANTHROPIC_AUTH_TOKEN=mock", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1")
	args := append([]string{"-p", "--output-format", "stream-json", "--verbose"}, claudeArgs(profile{Harness: "claude", Model: "haiku", Effort: "low"}, false)...)
	args = append(args, "--setting-sources", "", "--no-session-persistence", "--max-turns", "6", "Verify the available tools, then stop.")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = repo
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installed Claude: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range names {
		t.Logf("%s advertised=%v result=%s", name, advertised[name], denied[name])
		if advertised[name] || denied[name] == "" || denied[name] == "NOT AN ERROR" {
			t.Errorf("%s was not refused", name)
		}
	}
	if got := string(mustRead(t, original)); got != "unchanged\n" {
		t.Fatal("Edit changed the file")
	}
	for _, name := range []string{"bash-created.txt", "write-created.txt"} {
		if exists(filepath.Join(repo, name)) {
			t.Fatalf("mutating tool created %s", name)
		}
	}
}
