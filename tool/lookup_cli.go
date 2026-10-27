package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// LookupCLI is the CLI access added to a read-only agent invocation.
type LookupCLI struct {
	Env    []string
	Tools  []string
	Notice string
	// Extension is a pi extension that limits the shell to the lookup commands; a pass
	// loads it only when the restricted shell is asked for.
	Extension string
	// Guard is the tool guard extension (pi_ctx_reminder.ts); an open-shell pi
	// pass loads it with -e, a restricted-shell pass does not (the shell is restricted).
	Guard string
	Ready string // per-invocation acknowledgement from the loaded pi guard
	// Hook is the shell command of the Claude PreToolUse hook on Bash that refuses
	// anything but one plain lookup call (orch lookup-only-check); empty for pi.
	Hook string
}

const lookupNotice = `READ ONLY. Nothing in the repository may be edited, created, deleted, moved or formatted.
Do not run commands that write. No network.

HOW TO LOOK AT CODE. You have no project instructions, so this is the whole guide.
Your shell runs from the repository root, so paths are repo-relative: never cd.
Look things up in this order, and send independent lookups in one message (several tool calls at once):
  jbcontext search "<one behaviour in plain language>"   when you do not know where the code is. One concept per query, several queries in one message. The results are candidates, not proof of absence.
  orch lsp def|refs|callers|impls <Name>                  for a symbol you can name (Name, pkg.Name or Type.Method). orch lsp symbols <file> outlines a file; orch lsp hover <path:line:col> gives a type.
  rg -n -w '<text>'                                       for exact text
  fd <name>                                               for file names
A lookup that cannot answer says so; then use rg or the read tool.

HOW TO READ CODE. Use your native read tool with offset and limit. When you need several files or ranges, issue all the reads in one turn. Never use sed, cat, head or tail, and never read a whole large file to find one function: orch lsp def or rg -n gives its line, then read that range.
A violation fails the review.`

// restrictedShellNotice is added to the notice when the shell is limited to the lookup commands.
const restrictedShellNotice = `YOUR SHELL RUNS ONLY jbcontext search, orch lsp, rg, fd, ls and wc. One command per call, no pipes, no redirects, no unquoted globs, no cd, and no grep, head, tail, sed, cat or find: the shell blocks them. Put independent lookups in separate calls in one message.`

// LookupCLIFor pins the repository root and puts a shim for this binary first on PATH.
func LookupCLIFor(harness, code string) (LookupCLI, error) {
	exe, err := os.Executable()
	if err != nil {
		return LookupCLI{}, err
	}
	return lookupCLIForExecutable(harness, code, exe)
}

func lookupCLIForExecutable(harness, code, exe string) (LookupCLI, error) {
	var tools []string
	switch harness {
	case "pi":
		tools = []string{"bash"}
	case "claude":
		tools = []string{"Bash(jbcontext search:*)", "Bash(orch lsp:*)", "Bash(rg:*)", "Bash(fd:*)"}
	default:
		return LookupCLI{}, fmt.Errorf("unsupported ctx harness %q", harness)
	}
	root, err := filepath.Abs(code)
	if err != nil {
		return LookupCLI{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return LookupCLI{}, err
	}
	top, err := git(root, "rev-parse", "--show-toplevel")
	if err != nil || evalOr(top) != root {
		return LookupCLI{}, fmt.Errorf("ctx CLI needs a git repository root")
	}
	// Keep the target path, not its inode. Replacing the executable on rebuild
	// leaves existing shims pointing at the newly built binary.
	sum := sha256.Sum256([]byte(exe))
	dir := filepath.Join(ctxCacheDir(root), fmt.Sprintf("cli-%x", sum[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return LookupCLI{}, err
	}
	shim := filepath.Join(dir, "orch")
	if err := os.Symlink(exe, shim); err != nil {
		if target, readErr := os.Readlink(shim); readErr != nil || target != exe {
			return LookupCLI{}, fmt.Errorf("create ctx CLI shim: %w", err)
		}
	}
	cli := LookupCLI{
		Env:    []string{"ORCH_CTX_ROOT=" + root, "ORCH_CTX_READONLY=1", "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")},
		Tools:  tools,
		Notice: lookupNotice,
	}
	if harness == "claude" {
		cli.Hook = shellQuote(shim) + " lookup-only-check"
	}
	if harness == "pi" {
		// The extension runs the orch-guard binary; leads and gate panes inherit this.
		if bin := guardBinary(); bin != "" {
			cli.Env = append(cli.Env, "ORCH_GUARD_BIN="+bin)
		}
		if cli.Extension, err = writeLookupExtension(dir); err != nil {
			return LookupCLI{}, err
		}
		if cli.Guard, err = writeCtxReminderExtension(dir); err != nil {
			return LookupCLI{}, err
		}
	}
	return cli, nil
}

// guardBinary is the orch-guard binary the pi guard extension runs: ORCH_GUARD_BIN
// when set, else hooks/bin/orch-guard of the skill. Empty when neither is known.
func guardBinary() string {
	if bin := os.Getenv("ORCH_GUARD_BIN"); bin != "" {
		return bin
	}
	if dir, err := skillDir(); err == nil {
		return filepath.Join(dir, "hooks", "bin", "orch-guard")
	}
	return ""
}
