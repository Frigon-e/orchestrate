package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// toolRel is where the orch Go package lives inside a checkout.
const toolRel = "tool"

// wideSelection is the test count above which test-changed warns.
const wideSelection = 150

var (
	testFuncRe = regexp.MustCompile(`(?m)^func (Test\w*)\(`)
	declRe     = regexp.MustCompile(`^[+-](?:func (?:\([^)]*\) )?|type )(\w+)`)
)

// cmdTestChanged runs only the tests that belong to what the branch changed
// compared to a base branch.
func cmdTestChanged(a *app, args []string) error {
	fs := a.flags("test-changed")
	base := fs.String("base", "main", "branch to compare against")
	printOnly := fs.Bool("print", false, "print the -run pattern and the test count, run nothing")
	extra := fs.String("run-extra", "", "extra regexp of test names to add to the selection")
	all := fs.Bool("all", false, "run the whole package (explicit opt-in)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return &exitError{code: 2, msg: "usage: orch test-changed [--base main] [--print] [--run-extra <regex>] [--all]"}
	}
	dir, err := toolDir(a.cwd)
	if err != nil {
		return err
	}
	tests, err := selectChangedTests(dir, *base)
	if err != nil {
		return err
	}
	if *extra != "" {
		re, err := regexp.Compile(*extra)
		if err != nil {
			return fail("--run-extra: %v", err)
		}
		names, err := allTestNames(dir)
		if err != nil {
			return err
		}
		for _, n := range names {
			if re.MatchString(n) {
				tests = append(tests, n)
			}
		}
		tests = dedupeSorted(tests)
	}
	pattern := runPattern(tests)
	if *printOnly {
		fmt.Fprintf(a.out, "-run '%s'\nselected: %d tests\n", pattern, len(tests))
		return nil
	}
	if len(tests) > wideSelection {
		fmt.Fprintf(a.out, "wide selection: %d tests\n", len(tests))
	}
	start := time.Now()
	for _, step := range [][]string{{"build", "./..."}, {"vet", "./..."}} {
		if out, err := runGo(dir, step...); err != nil {
			fmt.Fprint(a.errw, out)
			return fail("go %s failed", step[0])
		}
	}
	testArgs := []string{"test", "-v", "-count=1", "-p", "2", "-timeout", "30m"}
	switch {
	case *all:
		fmt.Fprintln(a.out, "running the whole package (--all)")
	case len(tests) == 0:
		fmt.Fprintln(a.out, "no tests selected for this diff; build and vet passed")
		return nil
	default:
		testArgs = append(testArgs, "-run", pattern)
	}
	out, testErr := runGo(dir, append(testArgs, ".")...)
	passed, failed, skipped := countResults(out)
	if testErr != nil {
		fmt.Fprint(a.errw, out)
	}
	fmt.Fprintf(a.out, "selected %d, passed %d, failed %d, skipped %d, %.1fs\n",
		len(tests), passed, failed, skipped, time.Since(start).Seconds())
	if testErr != nil {
		return &exitError{code: 1}
	}
	return nil
}

func runGo(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// countResults counts top-level "--- PASS|FAIL|SKIP" lines of go test -v.
func countResults(out string) (passed, failed, skipped int) {
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "--- PASS"):
			passed++
		case strings.HasPrefix(l, "--- FAIL"):
			failed++
		case strings.HasPrefix(l, "--- SKIP"):
			skipped++
		}
	}
	return
}

// toolDir returns the orch package directory of the checkout containing cwd.
func toolDir(cwd string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", fail("not in a git checkout: %v", err)
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), filepath.FromSlash(toolRel))
	if _, err := os.Stat(dir); err != nil {
		return "", fail("no %s in this checkout", toolRel)
	}
	return dir, nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fail("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// selectChangedTests returns the sorted test names that belong to the changes
// in dir between the merge base of HEAD and base and the working tree.
func selectChangedTests(dir, base string) ([]string, error) {
	mb, err := gitOut(dir, "merge-base", "HEAD", base)
	if err != nil {
		return nil, err
	}
	mb = strings.TrimSpace(mb)
	names, err := gitOut(dir, "diff", "--name-only", "--relative", mb, "--", ".")
	if err != nil {
		return nil, err
	}
	untracked, err := gitOut(dir, "ls-files", "--others", "--exclude-standard", "--", ".")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(names+untracked, "\n") {
		if f = strings.TrimSpace(f); strings.HasSuffix(f, ".go") && !strings.Contains(f, "/") {
			files = append(files, f)
		}
	}
	diff, err := gitOut(dir, "diff", "-U0", "--relative", mb, "--", ".")
	if err != nil {
		return nil, err
	}
	// Untracked files are all additions; show them to the declaration scan as such.
	for _, f := range strings.Split(untracked, "\n") {
		if strings.HasSuffix(f, ".go") && !strings.Contains(f, "/") {
			if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
				diff += "\n+" + strings.ReplaceAll(string(b), "\n", "\n+")
			}
		}
	}
	return pickTests(dir, files, changedIdents(diff))
}

// changedIdents lists the declared names on the +/- lines of a -U0 diff.
func changedIdents(diff string) []string {
	var ids []string
	for _, l := range strings.Split(diff, "\n") {
		if m := declRe.FindStringSubmatch(l); m != nil {
			ids = append(ids, m[1])
		}
	}
	return dedupeSorted(ids)
}

// pickTests applies the selection rules to changed files and identifiers.
func pickTests(dir string, files, idents []string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var testFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
			testFiles = append(testFiles, e.Name())
		}
	}
	chosen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			chosen[f] = true
			continue
		}
		stem := strings.TrimSuffix(f, ".go")
		for _, t := range testFiles {
			if t == stem+"_test.go" || (strings.HasPrefix(t, stem+"_") && strings.HasSuffix(t, "_test.go")) {
				chosen[t] = true
			}
		}
	}
	var refs *regexp.Regexp
	if len(idents) > 0 {
		quoted := make([]string, len(idents))
		for i, id := range idents {
			quoted[i] = regexp.QuoteMeta(id)
		}
		refs = regexp.MustCompile(`\b(?:` + strings.Join(quoted, "|") + `)\b`)
	}
	var tests []string
	for _, t := range testFiles {
		b, err := os.ReadFile(filepath.Join(dir, t))
		if err != nil {
			if chosen[t] {
				return nil, err
			}
			continue
		}
		if !chosen[t] && (refs == nil || !refs.Match(b)) {
			continue
		}
		for _, m := range testFuncRe.FindAllStringSubmatch(string(b), -1) {
			tests = append(tests, m[1])
		}
	}
	return dedupeSorted(tests), nil
}

// allTestNames lists every Test function in the package, for --run-extra.
func allTestNames(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range testFuncRe.FindAllStringSubmatch(string(b), -1) {
			names = append(names, m[1])
		}
	}
	return dedupeSorted(names), nil
}

func runPattern(tests []string) string {
	return "^(" + strings.Join(tests, "|") + ")$"
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	var out []string
	for _, s := range in {
		if len(out) == 0 || s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
