package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// CtxCLI is the CLI access added to a read-only agent invocation.
type CtxCLI struct {
	Env    []string
	Tools  []string
	Notice string
	// Extension is a pi extension that limits the shell to orch ctx; a pass
	// loads it only when the ctx-only shell is asked for.
	Extension string
	// Guard is the tool guard extension (pi_ctx_reminder.ts); an open-shell pi
	// pass loads it with -e, a ctx-only pass does not (the shell is restricted).
	Guard string
	Ready string // per-invocation acknowledgement from the loaded pi guard
}

const ctxCLINotice = `READ ONLY. Nothing in the repository may be edited, created, deleted, moved or formatted.
Do not run commands that write. No network.

HOW TO LOOK AT CODE. You have no project instructions, so this is the whole guide.
Your shell runs from the repository root, so paths are repo-relative: never cd.
Use "orch ctx" first for search: hits are grouped by file, tests and generated files are hidden by default, and independent lookups can share a call. If a backend cannot answer, use scoped native search or inspect source; in a ctx-only shell, shell fallbacks are blocked.
` + ctxResultGuide + `
  orch ctx batch -q "find:Code that ..." -q "symbol:Name refs=3" -q "text:pattern glob=dir/**"   up to 50 lookups; answers return in order, a repeated place prints once. Normal output budgets are about 70000 characters/batch and 9000/lookup; narrow broad lookups or run a cut lookup alone.
The lookups (find, sym and text are the batch kinds find, symbol and text):
  orch ctx find "<one behaviour in plain words>"              ranked candidates when you do not know a name. Write "Code that lists payment gateways" or "Code that verifies a webhook signature, producing a stored receipt" (one behaviour per query, not a list of nouns)
  orch ctx sym <Name|Type.Method> [--refs N]                  exact/candidate definitions, doc, body preview and available references/implementers
  orch ctx text '<regex>' [--glob 'dir/**'] [--word] [-F]      pattern matches in the stated scope (--files-only lists files)
An answer ends with a sources: line and notes (a stale index, files edited in this worktree): read the notes.

HOW TO READ CODE. Use your native read tool with offset and limit. When you need several files or ranges, issue all the reads in one turn. Never use sed, cat, head or tail, and never read a whole large file to find one function: orch ctx sym or text gives its line, then read that range.
A violation fails the review.`

// ctxOnlyNotice is added to the notice when the shell is limited to orch ctx.
const ctxOnlyNotice = `YOUR SHELL RUNS ONLY orch ctx. One call per command, no pipes, no redirects, no globs, no cd, and no grep, head, tail, sed, cat, ls or find: the shell blocks them. Shorten output with --compact, or limit=N on a lookup in a batch (--limit N on a single call).`

// CtxCLIFor pins orch ctx and puts a shim for this binary first on PATH.
func CtxCLIFor(harness, code string) (CtxCLI, error) {
	exe, err := os.Executable()
	if err != nil {
		return CtxCLI{}, err
	}
	return ctxCLIForExecutable(harness, code, exe)
}

func ctxCLIForExecutable(harness, code, exe string) (CtxCLI, error) {
	var tools []string
	switch harness {
	case "pi":
		tools = []string{"bash"}
	case "claude":
		tools = []string{"Bash(orch ctx:*)"}
	default:
		return CtxCLI{}, fmt.Errorf("unsupported ctx harness %q", harness)
	}
	root, err := filepath.Abs(code)
	if err != nil {
		return CtxCLI{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return CtxCLI{}, err
	}
	top, err := git(root, "rev-parse", "--show-toplevel")
	if err != nil || evalOr(top) != root {
		return CtxCLI{}, fmt.Errorf("ctx CLI needs a git repository root")
	}
	// Keep the target path, not its inode. Replacing the executable on rebuild
	// leaves existing shims pointing at the newly built binary.
	sum := sha256.Sum256([]byte(exe))
	dir := filepath.Join(ctxCacheDir(root), fmt.Sprintf("cli-%x", sum[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return CtxCLI{}, err
	}
	shim := filepath.Join(dir, "orch")
	if err := os.Symlink(exe, shim); err != nil {
		if target, readErr := os.Readlink(shim); readErr != nil || target != exe {
			return CtxCLI{}, fmt.Errorf("create ctx CLI shim: %w", err)
		}
	}
	cli := CtxCLI{
		Env:    []string{"ORCH_CTX_ROOT=" + root, "ORCH_CTX_READONLY=1", "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")},
		Tools:  tools,
		Notice: ctxCLINotice,
	}
	if harness == "pi" {
		if cli.Extension, err = writeCtxOnlyExtension(dir); err != nil {
			return CtxCLI{}, err
		}
		if cli.Guard, err = writeCtxReminderExtension(dir); err != nil {
			return CtxCLI{}, err
		}
	}
	return cli, nil
}
