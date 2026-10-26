package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A pinned root always implies read-only, even without the explicit flag.
func ctxReadOnly() bool {
	return os.Getenv("ORCH_CTX_READONLY") == "1" || os.Getenv("ORCH_CTX_ROOT") != ""
}

func ctxPinnedRoot() (string, error) {
	root := os.Getenv("ORCH_CTX_ROOT")
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("read-only ctx requires an absolute ORCH_CTX_ROOT")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	top, err := git(root, "rev-parse", "--show-toplevel")
	if err != nil || evalOr(top) != root {
		return "", fmt.Errorf("ORCH_CTX_ROOT must be a git repository root")
	}
	return root, nil
}

func ctxReadCommand(sub string) bool {
	switch sub {
	case "find", "sym", "text", "read", "refs", "batch":
		return true
	}
	return false
}

func ctxWithinRoot(root, path string) bool {
	if filepath.IsAbs(path) {
		return false
	}
	p := filepath.Clean(path)
	if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
		return false
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root, p))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(evalOr(root), real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func ctxReadCmd(a *app, args []string) error {
	fs := a.flags("ctx read")
	from := fs.Int("from", 1, "first line (1-based)")
	to := fs.Int("to", 0, "last line (bounded by the read cap)")
	common := addCommon(fs)
	pos, done, err := ctxParse(a, "read", fs, args)
	if done {
		return err
	}
	if len(pos) != 1 {
		return ctxErr(a, "ctx read needs one repo-relative file")
	}
	f, err := common.fmt(a)
	if err != nil {
		return err
	}
	e, err := ctxEnvFor(a)
	if err != nil {
		return err
	}
	return a.ctxView(e, *common.json, f, e.readLookup(ctxLookup{Kind: "read", Path: pos[0], From: *from, To: *to}, f))
}
