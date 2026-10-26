package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const verifyTimeout = 15 * time.Minute

// baseSHARe is the only form of base that reaches git: a hex commit id, so a
// value in a lead-writable task.md cannot become a git option.
var baseSHARe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// cmdVerify checks a ship lead's work without trusting its `done` line: it
// runs the repo check command in the lead's worktree and lists the files the
// lead changed outside the paths the task owns. The check command comes from
// the main checkout's repo config, so a lead cannot change it from its branch.
func cmdVerify(a *app, args []string) error {
	fs := a.flags("verify")
	noMerge := fs.Bool("no-merge", false, "do not merge a passing package into its integration branch")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 1, "verify <id> [--no-merge]"); err != nil {
		return err
	}
	id := pos[0]
	s, err := a.site()
	if err != nil {
		return err
	}
	dir, err := taskDir(s.root, id)
	if err != nil {
		return err
	}
	fm, _, err := readTask(filepath.Join(dir, "task.md"))
	if err != nil {
		return fail("no task %s", id)
	}
	if fm["kind"] != "ship" {
		fmt.Fprintf(a.out, "verify: skipped\nreason: %s is a %s task; there is no code to verify\n", id, fm["kind"])
		return nil
	}
	wt := worktreePath(dir, id)
	if st, err := os.Stat(wt); err != nil || !st.IsDir() {
		return fail("no worktree at %s: the lead has not created it yet", wt)
	}
	base := fm["base"]
	if _, sha, ok := strings.Cut(base, "@"); ok {
		base = sha
	}
	if !baseSHARe.MatchString(base) {
		return fail("task.md base %q is not a commit sha; verify will not pass it to git", fm["base"])
	}

	integrating := fm["integration"] != "" && !*noMerge
	candidate := ""
	if integrating {
		candidate, err = git(wt, "rev-parse", "HEAD")
		if err != nil || !baseSHARe.MatchString(candidate) {
			return fail("cannot read candidate HEAD of %s", id)
		}
		if err := unchangedCandidate(wt, candidate); err != nil {
			return fail("candidate is not clean: %v; commit the work first, or use orch verify %s --no-merge for preliminary working-tree validation", err, id)
		}
	}

	files, err := changedFiles(wt, base)
	if err != nil {
		return err
	}
	owns := splitList(fm["owns"])
	var outside []string
	for _, f := range files {
		if len(owns) > 0 && !ownedPath(owns, f) {
			outside = append(outside, f)
		}
	}
	lines := changedLines(wt, base)

	result, checkLine, tail := "pass", "", ""
	if s.cfg.Check == "" {
		result, checkLine = "unverified", "none configured: set `check` in the repo config"
	} else {
		code, out := runCheck(wt, s.cfg.Check)
		checkLine = fmt.Sprintf("exit %d (%s)", code, s.cfg.Check)
		if code != 0 {
			result, tail = "fail", out
		}
	}
	if len(outside) > 0 {
		result = "fail"
	}
	var candidateErr error
	if integrating {
		candidateErr = unchangedCandidate(wt, candidate)
		if candidateErr != nil {
			result = "fail"
		}
	}

	wording := result
	if result == "pass" && !integrating {
		wording += " (not integrated)"
	}
	fmt.Fprintf(a.out, "verify: %s\ncheck: %s\nfiles: %d changed, %d lines\n", wording, agentScalar(checkLine), len(files), lines)
	if candidateErr != nil {
		fmt.Fprintf(a.out, "candidate: changed after checks: %v; integration refused\n", candidateErr)
	}
	switch {
	case len(owns) == 0:
		fmt.Fprintln(a.out, "scope: not checked (the task has no `owns:` front matter)")
	case len(outside) == 0:
		fmt.Fprintf(a.out, "scope: all files inside %s\n", agentScalar(strings.Join(owns, ", ")))
	default:
		fmt.Fprintf(a.out, "outside_owns[%d]:\n", len(outside))
		for _, f := range outside {
			fmt.Fprintf(a.out, "  %s\n", agentScalar(f))
		}
	}
	if tail != "" {
		fmt.Fprintf(a.out, "output (last lines):\n%s\n", tail)
	}
	note := fmt.Sprintf("verify %s: check %s; %d files, %d outside owns", wording, checkLine, len(files), len(outside))
	_ = a.appendStatus(dir, "note", note, "", "orch")
	if result != "pass" {
		return &exitError{code: 1, msg: ""}
	}
	if integrating {
		return a.integrate(s, dir, id, fm, candidate)
	}
	return nil
}

// unchangedCandidate excludes ignored build outputs, but never tracked changes or
// untracked implementation files. HEAD is compared with the commit pinned before
// scope and acceptance checks, not read afresh as the commit to integrate.
func unchangedCandidate(wt, candidate string) error {
	head, err := git(wt, "rev-parse", "HEAD")
	if err != nil {
		return fail("cannot read HEAD in %s", wt)
	}
	if head != candidate {
		return fail("HEAD moved from %s to %s", candidate, head)
	}
	dirty, err := git(wt, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return fail("cannot read working-tree state in %s", wt)
	}
	if dirty != "" {
		return fail("non-ignored working-tree changes in %s:\n%s", wt, dirty)
	}
	return nil
}

// changedFiles lists every path that differs from base in the worktree:
// committed, staged, modified and untracked. Renames are not detected, so a
// file moved into the owned paths still lists the source it left.
func changedFiles(wt, base string) ([]string, error) {
	diff, err := git(wt, "diff", "--name-only", "--no-renames", base, "--")
	if err != nil {
		return nil, fail("git diff in %s failed: is %s a commit in that worktree?", wt, base)
	}
	untracked, _ := git(wt, "ls-files", "--others", "--exclude-standard")
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.Split(diff+"\n"+untracked, "\n") {
		if f = strings.TrimSpace(f); f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// changedLines counts added plus removed lines against base, tracked files only.
func changedLines(wt, base string) int {
	out, err := git(wt, "diff", "--numstat", "--no-renames", base, "--")
	if err != nil {
		return 0
	}
	total := 0
	for _, l := range strings.Split(out, "\n") {
		var add, del int
		if n, _ := fmt.Sscanf(l, "%d\t%d\t", &add, &del); n == 2 {
			total += add + del
		}
	}
	return total
}

// ownedPath reports whether file is one of owns or sits under an owned directory.
func ownedPath(owns []string, file string) bool {
	for _, o := range owns {
		o = strings.TrimSuffix(o, "/")
		if file == o || strings.HasPrefix(file, o+"/") {
			return true
		}
	}
	return false
}

// runCheck runs the repo check command in dir and returns its exit code and
// the last lines of its output.
func runCheck(dir, command string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()
	cmd := processCommandContext(ctx, "sh", "-c", command)
	defer cmd.Cancel()
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if ctx.Err() != nil {
			fmt.Fprintf(&buf, "\ncheck timed out after %s\n", verifyTimeout)
		}
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return code, strings.Join(lines, "\n")
}
