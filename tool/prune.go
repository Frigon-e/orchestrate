package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// pruneItem is one linked worktree of the repo and what `orch prune` makes of it.
type pruneItem struct {
	path, branch string
	keep         string // why it stays; empty when it is removable
	stale        bool   // removable only as a worktree: the branch is unmerged and stays
	commits      int    // unmerged commits, for a stale item
	ageDays      int    // days since the branch tip, for a stale item
}

type gitWorktree struct {
	path, branch                     string
	detached, bare, locked, prunable bool
	prunableWhy                      string
}

func listGitWorktrees(repo string) ([]gitWorktree, error) {
	out, err := git(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var all []gitWorktree
	for _, line := range strings.Split(out, "\n") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			all = append(all, gitWorktree{path: val})
		case "":
		default:
			if len(all) == 0 {
				continue
			}
			w := &all[len(all)-1]
			switch key {
			case "branch":
				w.branch = strings.TrimPrefix(val, "refs/heads/")
			case "detached":
				w.detached = true
			case "bare":
				w.bare = true
			case "locked":
				w.locked = true
			case "prunable":
				w.prunable = true
			}
		}
	}
	return all, nil
}

// pruneRoot is the directory that holds the worktrees orch makes for the repo.
func pruneRoot(s site) string {
	return resolvedPath(filepath.Dir(worktreePath(filepath.Join(s.root, "x"), "x")))
}

// herdrCheckouts is the checkout path of every open Herdr workspace, by path.
func herdrCheckouts() (map[string]string, error) {
	if !herdrManaged() {
		return nil, fail("HERDR_ENV is not 1")
	}
	var res struct {
		Result struct {
			Workspaces []struct {
				ID       string `json:"workspace_id"`
				Worktree struct {
					Checkout string `json:"checkout_path"`
				} `json:"worktree"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := herdrJSON(&res, herdrTimeout, "workspace", "list"); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, w := range res.Result.Workspaces {
		if w.Worktree.Checkout != "" {
			m[resolvedPath(w.Worktree.Checkout)] = w.ID
		}
	}
	return m, nil
}

// staleRule lets an unmerged worktree go when its branch sat idle for days
// while the base branch kept moving. Only the worktree goes; the branch stays.
type staleRule struct {
	on   bool
	days int
	now  time.Time
}

func commitTime(repo, rev string) (time.Time, bool) {
	out, err := git(repo, "log", "-1", "--format=%ct", rev)
	n, perr := strconv.ParseInt(out, 10, 64)
	if err != nil || perr != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// check says whether the unmerged branch is stale, and records why on it.
func (r staleRule) check(repo, branch, base string, it *pruneItem) bool {
	tip, ok1 := commitTime(repo, branch)
	baseTip, ok2 := commitTime(repo, base)
	if !ok1 || !ok2 {
		return false
	}
	limit := time.Duration(r.days) * 24 * time.Hour
	if r.now.Sub(tip) <= limit || !baseTip.After(tip) || r.now.Sub(baseTip) > limit {
		return false
	}
	out, err := git(repo, "rev-list", "--count", base+".."+branch)
	n, perr := strconv.Atoi(out)
	if err != nil || perr != nil {
		return false
	}
	if c, err := git(repo, "rev-list", "--count", branch+".."+base); err != nil || c == "0" {
		return false
	}
	it.stale, it.commits, it.ageDays = true, n, int(r.now.Sub(tip).Hours()/24)
	return true
}

// staleFlag is --stale[=DAYS]: bare it means 30 days.
type staleFlag struct{ days int }

func (f *staleFlag) IsBoolFlag() bool { return true }
func (f *staleFlag) String() string {
	if f == nil || f.days == 0 {
		return ""
	}
	return strconv.Itoa(f.days)
}
func (f *staleFlag) Set(v string) error {
	if v == "true" {
		f.days = 30
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return fmt.Errorf("--stale takes a number of days of at least 1")
	}
	f.days = n
	return nil
}

// joinStale turns "--stale 45" into "--stale=45", since a bool-style flag
// cannot take its value as the next argument.
func joinStale(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--stale" && i+1 < len(args) {
			if _, err := strconv.Atoi(args[i+1]); err == nil {
				out = append(out, "--stale="+args[i+1])
				i++
				continue
			}
		}
		out = append(out, args[i])
	}
	return out
}

// pruneScan judges every linked worktree of the repo. It changes nothing.
func pruneScan(s site, stale staleRule) ([]pruneItem, error) {
	base := s.cfg.BaseBranch
	if base == "" || base == "HEAD" {
		return nil, fail("cannot tell the base branch (set base_branch in the repo config or check out a branch); nothing removed")
	}
	if _, err := git(s.repo, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return nil, fail("base branch %s does not exist; nothing removed", base)
	}
	all, err := listGitWorktrees(s.repo)
	if err != nil {
		return nil, fail("git worktree list failed: %v", err)
	}
	root, main := pruneRoot(s), resolvedPath(s.repo)
	owned := map[string]bool{}
	for _, ol := range openLeads(s.root) {
		owned[resolvedPath(worktreePath(ol.dir, filepath.Base(ol.dir)))] = true
	}
	var checkouts map[string]string
	var checkoutsErr error
	checkoutsRead := false
	var items []pruneItem
	for i, w := range all {
		p := resolvedPath(w.path)
		if i == 0 || p == main {
			continue
		}
		it := pruneItem{path: w.path, branch: w.branch, ageDays: -1}
		if w.branch != "" && !stale.now.IsZero() {
			if tip, ok := commitTime(s.repo, w.branch); ok {
				it.ageDays = int(stale.now.Sub(tip).Hours() / 24)
			}
		}
		switch {
		case !strings.HasPrefix(p, root+string(filepath.Separator)):
			it.keep = "outside the orch worktree root"
		case w.locked:
			it.keep = "locked"
		case w.prunable:
			it.keep = "prunable (directory missing): run git worktree prune"
		case w.bare || w.detached || w.branch == "":
			it.keep = "detached"
		case !strings.HasPrefix(w.branch, "orch/"):
			it.keep = "not an orch branch"
		case strings.HasPrefix(w.branch, "orch/int-"):
			it.keep = "integration branch"
		case owned[p]:
			it.keep = "open lead"
		default:
			if st, err := git(w.path, "status", "--porcelain"); err != nil || st != "" {
				it.keep = "uncommitted changes"
			} else if merged := isAncestor(s.repo, w.branch, base); !merged && (!stale.on || !stale.check(s.repo, w.branch, base, &it)) {
				it.keep = "branch not merged into " + base
			} else {
				if !checkoutsRead {
					checkouts, checkoutsErr = herdrCheckouts()
					checkoutsRead = true
				}
				if checkoutsErr != nil {
					it.keep = "Herdr state unknown"
				} else if id, ok := checkouts[p]; ok {
					it.keep = "open in Herdr workspace " + id
				}
			}
		}
		items = append(items, it)
	}
	return items, nil
}

// gitCombined runs git and returns its combined output, for refusal messages.
func gitCombined(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func lastGitLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func cmdPrune(a *app, args []string) error {
	fs := a.flags("prune")
	apply := fs.Bool("apply", false, "remove the worktrees")
	var sf staleFlag
	fs.Var(&sf, "stale", "also remove the worktree of an idle unmerged branch (keeps the branch); DAYS defaults to 30")
	pos, err := parseArgs(fs, joinStale(args))
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 0, "prune [--apply] [--stale[=DAYS]]"); err != nil {
		return err
	}
	s, err := a.site()
	if err != nil {
		return err
	}
	items, err := pruneScan(s, staleRule{on: sf.days > 0, days: sf.days, now: a.now()})
	if err != nil {
		return err
	}
	var removed, kept []string
	removable := 0
	for _, it := range items {
		age := ""
		if it.ageDays >= 0 {
			age = fmt.Sprintf(", last commit %d days ago", it.ageDays)
		}
		line := fmt.Sprintf("%s (branch %s%s)", it.path, it.branch, age)
		if it.stale {
			line = fmt.Sprintf("%s (stale, branch kept: %s, %d unmerged commits%s)", it.path, it.branch, it.commits, age)
		}
		if it.keep != "" {
			kept = append(kept, fmt.Sprintf("kept %s: %s", line, it.keep))
			continue
		}
		removable++
		if !*apply {
			removed = append(removed, "would remove "+line)
			continue
		}
		if st, err := git(it.path, "status", "--porcelain"); err != nil || st != "" {
			kept = append(kept, fmt.Sprintf("kept %s: uncommitted changes", line))
			continue
		}
		if out, err := gitCombined(s.repo, "worktree", "remove", it.path); err != nil {
			kept = append(kept, fmt.Sprintf("kept %s: git refused: %s", line, lastGitLine(out)))
			continue
		}
		if it.stale {
			removed = append(removed, "removed "+line)
			continue
		}
		if out, err := gitCombined(s.repo, "branch", "-d", it.branch); err != nil {
			removed = append(removed, fmt.Sprintf("removed %s; kept branch %s: git refused: %s", it.path, it.branch, lastGitLine(out)))
			continue
		}
		removed = append(removed, "removed "+line)
	}
	for _, l := range append(removed, kept...) {
		fmt.Fprintln(a.out, l)
	}
	if *apply {
		fmt.Fprintf(a.out, "prune: %d removed, %d kept\n", len(removed), len(kept))
		return nil
	}
	fmt.Fprintf(a.out, "prune: %d would be removed, %d kept\n", removable, len(kept))
	if removable > 0 {
		fmt.Fprintln(a.out, "run orch prune --apply to remove these")
	}
	return nil
}
