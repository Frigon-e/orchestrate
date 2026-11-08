package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// The hook contract: the repo owns one setup script and one archive script.
// A backend that declares worktree.setup-hook or worktree.archive-hook runs them
// itself and orch reads only the result. Every other backend (and a native run
// that is refused before any script ran) gets orch's own runner below, which is
// the only code that reads the script text.

// hookTimeout bounds one script run by orch's runner.
var hookTimeout = 120 * time.Second

func init() {
	// The registry's fallbacks: the runner reads the script text from the repo
	// named by HookEnv.RootPath.
	_ = setCapabilityFallback(CapSetupHook, &scriptHooks{})
	_ = setCapabilityFallback(CapArchiveHook, &scriptHooks{})
}

// scriptHooks is orch's own runner for the setup and archive scripts. Branch and
// TaskID, when set, are given to the script as ORCH_BRANCH and ORCH_TASK_ID,
// which only this runner can provide (a native run has no such variables).
type scriptHooks struct {
	Branch string
	TaskID string
	// Output, when set, receives the script's combined output.
	Output io.Writer
}

var (
	_ SetupHook   = (*scriptHooks)(nil)
	_ ArchiveHook = (*scriptHooks)(nil)
)

func (s *scriptHooks) RunSetup(ctx context.Context, env HookEnv) (HookResult, error) {
	return s.run(ctx, "setup", env)
}

func (s *scriptHooks) RunArchive(ctx context.Context, env HookEnv) (HookResult, error) {
	return s.run(ctx, "archive", env)
}

// run reads the script for kind from the repo and runs it with the worktree as
// working directory. No script is a successful run. A script that exits non-zero
// is a HookDone result with its exit code, not an error.
func (s *scriptHooks) run(ctx context.Context, kind string, env HookEnv) (HookResult, error) {
	script, err := repoHookScript(env.RootPath, kind)
	if err != nil {
		return HookResult{}, err
	}
	if strings.TrimSpace(script) == "" {
		return HookResult{Status: HookDone}, nil
	}
	extra := map[string]string{
		"ORCH_ROOT_PATH":      env.RootPath,
		"ORCH_WORKTREE_PATH":  env.WorktreePath,
		"ORCH_WORKSPACE_NAME": env.WorkspaceName,
	}
	if s.Branch != "" {
		extra["ORCH_BRANCH"] = s.Branch
	}
	if s.TaskID != "" {
		extra["ORCH_TASK_ID"] = s.TaskID
	}
	cctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", script)
	cmd.Dir = env.WorktreePath
	// childEnv("") drops every registered backend's variables, so a script sees
	// the same set whichever multiplexer orch runs in.
	cmd.Env = childEnv("", os.Environ(), extra)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	if s.Output != nil {
		_, _ = s.Output.Write(out.Bytes())
	}
	if ctx.Err() != nil {
		return HookResult{}, ctx.Err()
	}
	if cctx.Err() != nil {
		return HookResult{}, fail("%s hook timed out after %s", kind, hookTimeout)
	}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
		return HookResult{Status: HookDone}, nil
	case errors.As(runErr, &ee) && ee.ExitCode() >= 0:
		return HookResult{Status: HookDone, ExitCode: ee.ExitCode()}, nil
	}
	return HookResult{}, fail("%s hook could not run: %v", kind, runErr)
}

// repoHookScript is the script text for kind ("setup" or "archive"): the orch
// config keys hooks.<kind> in <repo>/.agents/orchestrate.toml, then
// scripts.<kind> in <repo>/orca.yaml when the key is absent. Only the runner
// above calls it; a native run never does.
func repoHookScript(repo, kind string) (string, error) {
	var cfg struct {
		Hooks map[string]string `toml:"hooks"`
	}
	path := filepath.Join(repo, ".agents", "orchestrate.toml")
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return "", fail("%s: %v", path, err)
		}
		if s, ok := cfg.Hooks[kind]; ok {
			return s, nil
		}
	}
	b, err := os.ReadFile(filepath.Join(repo, "orca.yaml"))
	if err != nil {
		return "", nil
	}
	return orcaYAMLScript(string(b), kind), nil
}

// orcaYAMLScript reads scripts.<kind> from orca.yaml text: a plain or quoted
// scalar, or a block scalar (| or >). It handles that one shape and nothing
// else, so no YAML dependency is needed.
func orcaYAMLScript(text, kind string) string {
	lines := strings.Split(text, "\n")
	indentOf := func(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }
	blank := func(l string) bool { return strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") }
	for i, l := range lines {
		if strings.TrimRight(l, " \t") != "scripts:" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if blank(lines[j]) {
				continue
			}
			if indentOf(lines[j]) == 0 {
				return ""
			}
			key, val, ok := strings.Cut(strings.TrimSpace(lines[j]), ":")
			if !ok || key != kind {
				continue
			}
			val = strings.TrimSpace(val)
			if !strings.HasPrefix(val, "|") && !strings.HasPrefix(val, ">") {
				return strings.Trim(val, `"'`)
			}
			keyIndent := indentOf(lines[j])
			var body []string
			block := -1
			for _, bl := range lines[j+1:] {
				if strings.TrimSpace(bl) != "" {
					if indentOf(bl) <= keyIndent {
						break
					}
					if block < 0 {
						block = indentOf(bl)
					}
				}
				if block >= 0 && len(bl) >= block {
					bl = bl[block:]
				} else if strings.TrimSpace(bl) == "" {
					bl = ""
				}
				body = append(body, bl)
			}
			return strings.TrimRight(strings.Join(body, "\n"), "\n") + "\n"
		}
	}
	return ""
}

// runWorktreeHook runs one hook ("setup" or "archive") for a worktree: through
// the backend when it declares the capability and config has it native, through
// the runner otherwise. A refusal before any script ran falls back to the
// runner; a hook that ran and failed is returned as it is and never rerun.
// native says which side produced the result.
func runWorktreeHook(ctx context.Context, a *Active, kind string, env HookEnv, fb *scriptHooks) (res HookResult, native bool, err error) {
	capName := map[string]string{"setup": CapSetupHook, "archive": CapArchiveHook}[kind]
	if capName == "" {
		return HookResult{}, false, fail("unknown hook %q", kind)
	}
	if a.Has(capName) {
		if kind == "setup" {
			res, err = a.Backend.(SetupHook).RunSetup(ctx, env)
		} else {
			res, err = a.Backend.(ArchiveHook).RunArchive(ctx, env)
		}
		if !errors.Is(err, ErrHookRefused) {
			return res, true, err
		}
	}
	res, err = fb.run(ctx, kind, env)
	return res, false, err
}

// createWorktreeWithSetup creates a worktree and runs the repo's setup script
// for it. A native setup is requested at creation and comes back Started; the
// runner's comes back Done with the script's exit code. A native setup refused
// before running (the worktree was made without it) runs through the runner.
func createWorktreeWithSetup(ctx context.Context, a *Active, w WorktreeSpec, env HookEnv, fb *scriptHooks) (Worktree, HookResult, error) {
	mgr, _, ok := capabilityAs[WorktreeManager](a, CapWorktreeManage)
	if !ok || !a.Has(CapWorktreeManage) {
		return Worktree{}, HookResult{}, fail("backend %s does not manage worktrees", a.Name())
	}
	w.NativeSetup = a.Has(CapSetupHook)
	made, err := mgr.CreateWorktree(ctx, w)
	if err != nil {
		return Worktree{}, HookResult{}, err
	}
	env.WorktreePath = made.Path
	res, _, err := runWorktreeHook(ctx, a, "setup", env, fb)
	return made, res, err
}

// removeWorktreeWithArchive removes a worktree the way every backend must: orch's
// own checks first (merged, clean; the caller passes them as checks), then the
// archive hook, then the removal. Nothing runs when a check fails. A failing
// archive hook blocks the removal, native (it comes back from RemoveWorktree)
// or through the runner, and is never rerun or forced past.
func removeWorktreeWithArchive(ctx context.Context, a *Active, w WorktreeSpec, env HookEnv, checks func() error, fb *scriptHooks) error {
	if checks != nil {
		if err := checks(); err != nil {
			return err
		}
	}
	mgr, _, ok := capabilityAs[WorktreeManager](a, CapWorktreeManage)
	if !ok || !a.Has(CapWorktreeManage) {
		return fail("backend %s does not manage worktrees", a.Name())
	}
	if a.Has(CapArchiveHook) {
		// RunArchive arms the removal; the hook runs inside it.
		native := env
		native.WorktreePath = w.Path // the key RemoveWorktree looks the arming up by
		_, err := a.Backend.(ArchiveHook).RunArchive(ctx, native)
		if err == nil {
			err = mgr.RemoveWorktree(ctx, w)
			if err == nil || !errors.Is(err, ErrHookRefused) {
				return err
			}
		} else if !errors.Is(err, ErrHookRefused) {
			return err
		}
	}
	// Runner path: also a native refusal, which ran nothing.
	env.WorktreePath = cmpPath(env.WorktreePath, w.Path)
	res, err := fb.run(ctx, "archive", env)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return &HookFailedError{Hook: "archive", ExitCode: res.ExitCode, Err: fmt.Errorf("archive script exited %d", res.ExitCode)}
	}
	return mgr.RemoveWorktree(ctx, w)
}

func cmpPath(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
