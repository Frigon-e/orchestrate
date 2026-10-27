package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCtxCLIShimNoticeAndTools(t *testing.T) {
	c := newCtxHarness(t)
	exe := filepath.Join(t.TempDir(), "orch-current")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf first"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"pi", "claude"} {
		cfg, err := ctxCLIForExecutable(h, c.repo, exe)
		if err != nil {
			t.Fatal(err)
		}
		tools := []string{"bash"}
		if h == "claude" {
			tools = []string{"Bash(jbcontext search:*)", "Bash(orch lsp:*)", "Bash(rg:*)", "Bash(fd:*)"}
		}
		if !reflect.DeepEqual(cfg.Tools, tools) {
			t.Fatalf("tools=%v", cfg.Tools)
		}
		if len(cfg.Env) != 3 || cfg.Env[0] != "ORCH_CTX_ROOT="+evalOr(c.repo) || cfg.Env[1] != "ORCH_CTX_READONLY=1" {
			t.Fatalf("env=%v", cfg.Env)
		}
		shimDir, _, _ := strings.Cut(strings.TrimPrefix(cfg.Env[2], "PATH="), string(os.PathListSeparator))
		shim := filepath.Join(shimDir, "orch")
		if !strings.HasPrefix(shimDir, ctxCacheDir(evalOr(c.repo))+string(filepath.Separator)) {
			t.Fatal(shimDir)
		}
		if target, err := os.Readlink(shim); err != nil || target != exe {
			t.Fatalf("target=%q err=%v", target, err)
		}
		again, err := ctxCLIForExecutable(h, c.repo, exe)
		if err != nil || !reflect.DeepEqual(again, cfg) {
			t.Fatalf("not idempotent: %+v %v", again, err)
		}
		lines := strings.Split(cfg.Notice, "\n")
		if len(lines) < 10 || len(lines) > 30 || !strings.HasPrefix(cfg.Notice, "READ ONLY.") {
			t.Fatal(cfg.Notice)
		}
		for _, part := range []string{"edited, created, deleted, moved or formatted", "No network", "jbcontext search", "One concept per query", "not proof of absence", "orch lsp def|refs|callers|impls", "rg -n -w", "fd <name>", "independent lookups in one message", "never cd", "native read tool with offset and limit", "issue all the reads in one turn", "Never use sed, cat, head or tail", "A violation fails the review"} {
			if !strings.Contains(cfg.Notice, part) {
				t.Fatal(part)
			}
		}
		if strings.Contains(cfg.Notice, "orch ctx") || strings.Contains(cfg.Notice, "read:") {
			t.Fatal("notice recommends a script-only read command")
		}
		cmd := exec.Command("/bin/sh", "-c", "orch")
		cmd.Env = append(os.Environ(), cfg.Env...)
		if out, err := cmd.Output(); err != nil || string(out) != "first" {
			t.Fatalf("shim execution=%q %v", out, err)
		}
		// Atomically replace the binary path, like go build does. The old shim
		// must follow the path to the replacement rather than retain an inode.
		next := exe + ".new"
		if err := os.WriteFile(next, []byte("#!/bin/sh\nprintf rebuilt"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, exe); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(shim).Output(); err != nil || string(out) != "rebuilt" {
			t.Fatalf("rebuilt shim=%q %v", out, err)
		}
		if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf first"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CtxCLIFor("unknown", c.repo); err == nil {
		t.Fatal("unknown harness accepted")
	}
	if _, err := CtxCLIFor("pi", t.TempDir()); err == nil {
		t.Fatal("non-repo accepted")
	}
	if cfg, err := CtxCLIFor("pi", c.repo); err != nil || len(cfg.Tools) != 1 {
		t.Fatalf("exported function: %+v %v", cfg, err)
	}
}

func TestCtxCLICommandsOnly(t *testing.T) {
	for _, name := range ctxSubcommands {
		if name == "mcp" {
			t.Fatal("removed command still advertised")
		}
	}
	c := newCtxHarness(t)
	if err := cmdCtx(c.a, []string{"mcp"}); err == nil {
		t.Fatal("removed command accepted")
	}
	pinCtx(t, c)
	for _, sub := range []string{"refresh", "status", "watch", "brief", "mcp"} {
		if err := cmdCtx(c.a, []string{sub}); err == nil {
			t.Fatalf("read-only command allowed: %s", sub)
		}
	}
}
