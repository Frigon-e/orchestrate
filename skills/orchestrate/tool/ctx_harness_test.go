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

type ctxShellResult struct {
	advertised bool
	received   bool
	failed     bool
	text       string
	tools      []string
}

// Installed harness smoke tests use a loopback mock, isolated homes and dummy
// credentials. The mock forces each command rather than trusting the notice.
func TestCtxInstalledHarnesses(t *testing.T) {
	if os.Getenv("ORCH_TEST_CTX_HARNESSES") != "1" {
		t.Skip("set ORCH_TEST_CTX_HARNESSES=1 for local mock harness verification")
	}
	c := newCtxHarness(t)
	bin := filepath.Join(t.TempDir(), "orch")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	outsideFile := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside repo sentinel\nsecond line not requested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"pi", "claude"} {
		t.Run(h, func(t *testing.T) {
			tests := []struct {
				command string
				allow   bool
				want    string
			}{{"orch ctx find x", true, "path: pay/pay.go"}}
			if h == "claude" {
				tests = append(tests, []struct {
					command string
					allow   bool
					want    string
				}{
					{"orch ctx find x; touch f", false, ""},
					{"orch ctx find x && rm f", false, ""},
					{"orch ctx find x > out", false, ""},
					{"cat file", true, "outside allowlist sentinel"},
					{fmt.Sprintf("cat %q", outsideFile), false, ""},
					{"touch f", false, ""},
					{"orch ctx find $(touch f)", false, ""},
					{"$(touch f) orch ctx find x", false, ""},
					{"orch ctx find `touch f`", false, ""},
				}...)
			}
			for _, tc := range tests {
				t.Run(tc.command, func(t *testing.T) {
					// rm has a real target. Other write probes must not create f or out.
					f := filepath.Join(c.repo, "f")
					if strings.Contains(tc.command, "rm f") {
						if err := os.WriteFile(f, []byte("keep me"), 0o644); err != nil {
							t.Fatal(err)
						}
					} else {
						_ = os.Remove(f)
					}
					if err := os.WriteFile(filepath.Join(c.repo, "file"), []byte("outside allowlist sentinel"), 0o644); err != nil {
						t.Fatal(err)
					}
					res := ctxRunShellHarness(t, h, bin, c.repo, tc.command)
					t.Logf("command=%q advertised=%v received=%v is_error=%v result=%s", tc.command, res.advertised, res.received, res.failed, res.text)
					if !res.advertised || !res.received {
						t.Fatal("shell tool round trip missing")
					}
					if tc.allow {
						if res.failed || !strings.Contains(res.text, tc.want) {
							t.Fatal("expected read/search command did not run")
						}
					} else {
						if !res.failed {
							t.Error("command was not refused")
						}
						if strings.Contains(tc.command, "rm f") {
							raw, err := os.ReadFile(f)
							if err != nil || string(raw) != "keep me" {
								t.Error("rm probe changed f")
							}
						} else if _, err := os.Stat(f); !os.IsNotExist(err) {
							t.Error("write probe created f")
						}
						if _, err := os.Stat(filepath.Join(c.repo, "out")); !os.IsNotExist(err) {
							t.Error("redirect probe created out")
						}
					}
				})
			}
			if h == "claude" {
				t.Run("native Read outside repo", func(t *testing.T) {
					res := ctxRunHarnessTool(t, h, bin, c.repo, "Read", map[string]any{"file_path": outsideFile, "offset": 1, "limit": 1})
					t.Logf("native Read file=%q offset=1 limit=1 advertised=%v received=%v is_error=%v result=%s", outsideFile, res.advertised, res.received, res.failed, res.text)
					if !res.advertised || !res.received || res.failed || !strings.Contains(res.text, "outside repo sentinel") || strings.Contains(res.text, "second line not requested") {
						t.Fatal("native Read outside-repo partial read did not match the observed permission policy")
					}
				})
			}
		})
	}
}

func ctxRunShellHarness(t *testing.T, harness, bin, repo, command string) ctxShellResult {
	t.Helper()
	tool := "Bash"
	if harness == "pi" {
		tool = "bash"
	}
	return ctxRunHarnessTool(t, harness, bin, repo, tool, map[string]any{"command": command})
}

func ctxRunHarnessTool(t *testing.T, harness, bin, repo, tool string, input map[string]any) ctxShellResult {
	t.Helper()
	path, err := exec.LookPath(harness)
	if err != nil {
		t.Skip(err)
	}
	var mu sync.Mutex
	var result ctxShellResult
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(r.URL.Path, "count_tokens") {
			fmt.Fprint(w, `{"input_tokens":1}`)
			return
		}
		advertised, received := false, false
		for _, v := range req.Tools {
			advertised = advertised || v.Name == tool
		}
		mu.Lock()
		result.advertised = result.advertised || advertised
		if advertised {
			result.tools = nil
			for _, v := range req.Tools {
				result.tools = append(result.tools, v.Name)
			}
		}
		for _, msg := range req.Messages {
			var blocks []struct {
				Type    string          `json:"type"`
				ID      string          `json:"tool_use_id"`
				Content json.RawMessage `json:"content"`
				IsError bool            `json:"is_error"`
			}
			if json.Unmarshal(msg.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type == "tool_result" && b.ID == "toolu_ctx_cli" {
					received, result.received, result.failed = true, true, b.IsError
					var text string
					if json.Unmarshal(b.Content, &text) == nil {
						result.text = text
					} else {
						result.text = string(b.Content)
					}
				}
			}
		}
		mu.Unlock()
		var block any = map[string]any{"type": "text", "text": "ctx mock complete"}
		reason := "end_turn"
		if advertised && !received {
			reason = "tool_use"
			block = map[string]any{"type": "tool_use", "id": "toolu_ctx_cli", "name": tool, "input": input}
		}
		ctxMockReply(w, req.Stream, block, reason)
	}))
	defer srv.Close()
	cfg, err := ctxCLIForExecutable(harness, repo, bin)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	agentDir := filepath.Join(home, "pi-agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	models, _ := json.Marshal(map[string]any{"providers": map[string]any{"ctx-mock": map[string]any{"baseUrl": srv.URL, "api": "anthropic-messages", "apiKey": "mock", "models": []any{map[string]any{"id": "mock", "contextWindow": 200000, "maxTokens": 4096}}}}})
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), models, 0o600); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(key, "ORCH_") || strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_") || strings.HasPrefix(key, "PI_") || key == "HOME" || key == "PATH" {
			continue
		}
		env = append(env, v)
	}
	env = append(env, "HOME="+home, "PI_CODING_AGENT_DIR="+agentDir, "ANTHROPIC_BASE_URL="+srv.URL, "ANTHROPIC_API_KEY=mock", "ANTHROPIC_AUTH_TOKEN=mock", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "ORCH_CONFIG="+filepath.Join(home, "config.toml"), "ORCH_CACHE_DIR="+filepath.Join(home, "cache"), "ORCH_JBCONTEXT="+os.Getenv("ORCH_JBCONTEXT"), "ORCH_GOPLS="+os.Getenv("ORCH_GOPLS"), "ORCH_RG="+os.Getenv("ORCH_RG"))
	env = append(env, cfg.Env...)
	allowed := append([]string{"Read", "Grep", "Glob"}, cfg.Tools...)
	args := []string{"-p", "--model", "haiku", "--tools", "Read,Grep,Glob,Bash", "--allowedTools", strings.Join(allowed, ","), "--permission-mode", "dontAsk", "--permission-prompts", "none", "--max-turns", "3", "--no-session-persistence", "--setting-sources", "", "--disable-slash-commands"}
	if harness == "pi" {
		args = []string{"--print", "--no-session", "--no-extensions", "--no-context-files", "--no-skills", "--no-prompt-templates", "--offline", "--provider", "ctx-mock", "--model", "mock", "--tools", strings.Join(append([]string{"read", "grep", "find", "ls"}, cfg.Tools...), ",")}
	}
	args = append(args, cfg.Notice+"\nRun the requested lookup, then stop.")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir, cmd.Env = repo, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", harness, err, out)
	}
	t.Logf("%s stdout: %s", harness, strings.TrimSpace(string(out)))
	mu.Lock()
	defer mu.Unlock()
	native := []string{"Read", "Grep", "Glob", "Bash"}
	if harness == "pi" {
		native = []string{"read", "grep", "find", "ls", "bash"}
	}
	t.Logf("%s advertised tools: %s", harness, strings.Join(result.tools, ","))
	for _, name := range native {
		found := false
		for _, have := range result.tools {
			found = found || have == name
		}
		if !found {
			t.Errorf("native pass tool missing: %s", name)
		}
	}
	return result
}

func ctxMockReply(w http.ResponseWriter, stream bool, block any, reason string) {
	if !stream {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_ctx", "type": "message", "role": "assistant", "model": "mock", "content": []any{block}, "stop_reason": reason, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_ctx", "type": "message", "role": "assistant", "model": "mock", "content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}})
	if b := block.(map[string]any); reason == "tool_use" {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": b["id"], "name": b["name"], "input": map[string]any{}}})
		input, _ := json.Marshal(b["input"])
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": string(input)}})
	} else {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "ctx mock complete"}})
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
	send("message_stop", map[string]string{"type": "message_stop"})
}
