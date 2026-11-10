package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoverClaudeSessionTakesTheNewestTranscriptOfTheWorktree(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	wt := "/Users/x/orca/workspaces/r/t1"
	folder := filepath.Join(cfg, "projects", "-Users-x-orca-workspaces-r-t1")
	os.MkdirAll(folder, 0o755)
	other := filepath.Join(cfg, "projects", "-Users-x-orca-workspaces-r-t2")
	os.MkdirAll(other, 0o755)
	old, newer := filepath.Join(folder, "old-id.jsonl"), filepath.Join(folder, "new-id.jsonl")
	os.WriteFile(old, []byte("{}"), 0o644)
	os.WriteFile(newer, []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(other, "foreign.jsonl"), []byte("{}"), 0o644)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	if got := discoverClaudeSession(wt); got != "new-id" {
		t.Fatalf("session = %q, want new-id", got)
	}
	if got := discoverClaudeSession("/Users/x/orca/workspaces/r/none"); got != "" {
		t.Fatalf("session for a worktree with no transcript = %q", got)
	}
	l := &lead{Harness: "claude", WorktreePath: wt}
	if got := sessionOf(AgentInfo{}, l); got != "new-id" {
		t.Fatalf("sessionOf = %q, want the discovered session", got)
	}
	if got := sessionOf(AgentInfo{}, &lead{Harness: "claude", Session: "chosen", WorktreePath: wt}); got != "chosen" {
		t.Fatalf("sessionOf = %q, want the session orch chose", got)
	}
}
