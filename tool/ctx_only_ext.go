package main

import (
	_ "embed"
	"os"
	"path/filepath"
)

// ctxOnlyExtension is the pi extension that limits a pass's shell to orch ctx.
//
//go:embed pi_ctx_only.ts
var ctxOnlyExtension []byte

// writeCtxOnlyExtension puts the extension next to the PATH shim, so the pass
// loads it with -e from a path that does not depend on where the skill lives.
func writeCtxOnlyExtension(dir string) (string, error) {
	return writeExtension(dir, "orch-ctx-only.ts", ctxOnlyExtension)
}

// ctxReminderExtension is the tool guard: it blocks grep, find, sed and cat and
// points the model to orch ctx. An open-shell gate pass loads this embedded
// copy, the same code as the checkout, never the one installed under ~/.pi.
//
//go:embed pi_ctx_reminder.ts
var ctxReminderExtension []byte

func writeCtxReminderExtension(dir string) (string, error) {
	return writeExtension(dir, "orch-ctx-reminder.ts", ctxReminderExtension)
}

func writeExtension(dir, name string, body []byte) (string, error) {
	path := filepath.Join(dir, name)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(body) {
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, "ext-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}
