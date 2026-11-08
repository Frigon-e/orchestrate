package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// shipBranch is the branch of a ship lead's worktree. The repo config has no
// key for branch naming, so it is always orch/<id>.
func shipBranch(id string) string { return "orch/" + id }

// worktreeBranch is the branch checked out in the worktree at path, or "" when
// path is not a worktree of repo (or is on a detached head).
func worktreeBranch(repo, path string) string {
	out, err := git(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	want := resolvedPath(path)
	var cur string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			cur = resolvedPath(strings.TrimPrefix(line, "worktree "))
		case cur == want && strings.HasPrefix(line, "branch refs/heads/"):
			return strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	return ""
}

func resolvedPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// placeShip puts a ship lead in its own backend workspace on a worktree: made
// fresh, or for a respawn reopened as it was left. The agent's pane is split
// off the workspace's root pane so it gets the lead env, and the root pane is
// closed once nothing else lives in it.
func (a *app) placeShip(s site, dir, id, base string, prev *lead, env map[string]string) (pane, tab, workspace string, err error) {
	b, err := useBackend()
	if err != nil {
		return "", "", "", err
	}
	mgr, _, ok := capabilityAs[WorktreeManager](b, CapWorktreeManage)
	if !ok || !b.Has(CapWorktreeManage) {
		return "", "", "", fail("backend %s does not manage worktrees", b.Name())
	}
	lay, ok := b.Backend.(Layout)
	if !ok || !b.Has(CapLayout) {
		return "", "", "", fail("backend %s cannot place a pane in a worktree workspace", b.Name())
	}
	ctx := context.Background()
	wt, branch := worktreePath(dir, id), shipBranch(id)
	fresh := prev == nil || worktreeBranch(s.repo, wt) == ""
	spec := WorktreeSpec{Name: id, Branch: branch, Base: base, Repo: s.repo, Path: wt}
	var made Worktree
	if fresh {
		if _, err := os.Lstat(wt); err == nil {
			return "", "", "", fail("worktree path %s already exists; remove or move it before spawning %s", wt, id)
		}
		if _, err := git(s.repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			return "", "", "", fail("branch %s already exists in %s; delete it (git branch -d %s) or pick another task id", branch, s.repo, branch)
		}
		made, err = mgr.CreateWorktree(ctx, spec)
	} else {
		made, err = mgr.OpenWorktree(ctx, spec)
	}
	if err != nil {
		return "", "", "", err
	}
	undo := func() {
		if fresh {
			_ = mgr.RemoveWorktree(ctx, spec)
			_, _ = git(s.repo, "branch", "-d", branch)
		}
	}
	if fresh {
		a.linkEnv(s.repo, wt, dir)
	}
	split, err := lay.Split(ctx, made.Root, Spec{Direction: "right", Cwd: wt, Env: env})
	if err != nil {
		undo()
		return "", "", "", err
	}
	if made.Panes <= 1 { // a reopened workspace may hold the user's own panes
		_ = b.Close(ctx, made.Root)
	}
	return string(split.Handle), string(made.Tab), string(made.Workspace), nil
}

// linkEnv symlinks the checkout's .env into a new worktree, so the lead runs
// with the same local config. Only .env, never over a file, never when the
// link could end up committed or when the checkout is on its main branch.
func (a *app) linkEnv(repo, wt, dir string) {
	src, dst := filepath.Join(repo, ".env"), filepath.Join(wt, ".env")
	why := ""
	if b, _ := git(repo, "rev-parse", "--abbrev-ref", "HEAD"); b == "main" || b == "master" {
		why = "the checkout is on " + b
	} else if _, err := os.Lstat(src); err != nil {
		why = "the checkout has no .env"
	} else if _, err := os.Lstat(dst); err == nil {
		why = "the worktree already has a .env"
	} else if _, err := git(wt, "check-ignore", "-q", ".env"); err != nil {
		why = ".env is not gitignored"
	} else if err := os.Symlink(src, dst); err != nil {
		why = err.Error()
	}
	if why == "" {
		fmt.Fprintf(a.out, ".env linked into %s\n", wt)
		return
	}
	_ = a.appendStatus(dir, "note", ".env not linked: "+why, "", "orch")
	fmt.Fprintf(a.out, ".env not linked: %s\n", why)
}

// removeShipWorktree is the last step of `orch close`: it removes the lead's
// worktree and branch only when nothing would be lost, and says why it kept
// them otherwise. It never forces.
func (a *app) removeShipWorktree(s site, dir string, l *lead, fm map[string]string) error {
	wt := worktreePath(dir, l.ID)
	branch := worktreeBranch(s.repo, wt)
	if branch == "" {
		fmt.Fprintf(a.out, "worktree %s is already gone or on no branch; nothing removed\n", wt)
		return nil
	}
	base := s.cfg.BaseBranch
	if base == "" {
		base, _, _ = strings.Cut(fm["base"], "@")
	}
	keep := func(why, fix string) error {
		line := fmt.Sprintf("kept worktree %s (branch %s): %s. %s", wt, branch, why, fix)
		fmt.Fprintln(a.out, line)
		return a.appendStatus(dir, "note", line, "", "orch")
	}
	manual := fmt.Sprintf("git -C %s worktree remove %s && git -C %s branch -d %s", s.repo, wt, s.repo, branch)
	if st, err := git(wt, "status", "--porcelain"); err != nil || st != "" {
		return keep("it has uncommitted changes", "Commit or discard them, then remove it with: "+manual)
	}
	if !isAncestor(s.repo, branch, base) {
		return keep("its branch is not merged into "+base, "Merge it first (orch verify "+l.ID+"), then remove it with: "+manual+". To drop the work instead, use git worktree remove --force and git branch -D, after the user agrees")
	}
	b, err := useBackend()
	if err != nil {
		return err
	}
	mgr, _, ok := capabilityAs[WorktreeManager](b, CapWorktreeManage)
	if !ok || !b.Has(CapWorktreeManage) {
		return fail("backend %s does not manage worktrees", b.Name())
	}
	if err := mgr.RemoveWorktree(context.Background(), WorktreeSpec{Repo: s.repo, Path: wt}); err != nil {
		if errors.Is(err, ErrWorktreeDirty) {
			return keep(backendLabel()+" reports it dirty", "Remove it yourself with: "+manual)
		}
		return err
	}
	line := fmt.Sprintf("removed worktree %s", wt)
	if _, err := git(s.repo, "branch", "-d", branch); err != nil {
		line += fmt.Sprintf("; kept branch %s (git branch -d refused it; check it is merged)", branch)
	} else {
		line += " and branch " + branch
	}
	fmt.Fprintln(a.out, line)
	return nil
}
