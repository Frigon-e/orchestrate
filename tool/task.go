package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// briefCharLimit is the size of task.md past which orch check warns: a lead
// re-reads the whole brief on every turn and again after each compaction.
const briefCharLimit = 15000

var (
	taskIDRe         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,26}$`)
	headingRe        = regexp.MustCompile(`^# (\S.*?)\s*$`)
	requiredSections = []string{"Intent", "Scope", "Pointers", "Spec", "Acceptance", "Authority", "Report-back"}
	frontmatterKeys  = []string{"id", "kind", "repo", "base", "harness", "model", "effort", "depends_on"}
)

func taskDir(root, id string) (string, error) {
	if !taskIDRe.MatchString(id) {
		return "", fail("task id %q must match %s", id, taskIDRe)
	}
	return filepath.Join(root, id), nil
}

func leadName(id string) string { return "lead-" + id }

// readTask splits task.md into its frontmatter keys and its body.
func readTask(path string) (map[string]string, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	text, fm := string(b), map[string]string{}
	if !strings.HasPrefix(text, "---\n") {
		return fm, text, nil
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return fm, text, nil
	}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return fm, text[4+end+5:], nil
}

// sections maps each top-level `# Heading` outside code fences to its text.
func sections(body string) map[string]string {
	lines := map[string][]string{}
	name, fenced := "", false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if !fenced {
			if m := headingRe.FindStringSubmatch(line); m != nil {
				name = m[1]
				lines[name] = nil
				continue
			}
		}
		if name != "" {
			lines[name] = append(lines[name], line)
		}
	}
	out := make(map[string]string, len(lines))
	for k, v := range lines {
		out[k] = strings.TrimSpace(strings.Join(v, "\n"))
	}
	return out
}

// labelled returns the value of a `Label: value` line.
func labelled(text, label string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(label) + `:[ \t]*(.*)$`)
	if m := re.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func depends(fm map[string]string) []string {
	var out []string
	for _, d := range strings.Split(strings.Trim(fm["depends_on"], "[] "), ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// checkTask refuses a brief with gaps: problems block spawning, warnings don't.
func checkTask(root, id string) (problems, warnings []string, err error) {
	dir, err := taskDir(root, id)
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, "task.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("%s does not exist (run: orch new %s ...)", path, id)}, nil, nil
	}
	fm, body, _ := readTask(path)
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	for _, key := range frontmatterKeys {
		if _, ok := fm[key]; !ok {
			add("frontmatter: missing `%s`", key)
		}
	}
	if fm["id"] != "" && fm["id"] != id {
		add("frontmatter: id %q does not match directory %q", fm["id"], id)
	}
	if k := fm["kind"]; k != "ship" && k != "scout" {
		add("frontmatter: kind must be ship or scout")
	}
	if h := fm["harness"]; h == "auto" {
		add("frontmatter: harness is auto; run `orch spawn %s --auto` to choose a profile", id)
	} else if h != "claude" && h != "pi" {
		add("frontmatter: harness must be claude or pi")
	}
	for _, key := range []string{"model", "effort", "base"} {
		if v, ok := fm[key]; ok && v == "" {
			add("frontmatter: `%s` is empty (use `default` to inherit)", key)
		}
	}
	if repo := fm["repo"]; repo != "" {
		if st, err := os.Stat(repo); err != nil || !st.IsDir() {
			add("frontmatter: repo %s is not a directory", repo)
		}
	}
	if strings.Contains(string(raw), "{{") {
		add("unfilled {{placeholder}} left in task.md")
	}
	secs := sections(body)
	for _, name := range requiredSections {
		text, ok := secs[name]
		switch {
		case !ok:
			add("section `# %s` is missing", name)
		case text == "":
			add("section `# %s` is empty", name)
		}
	}
	for _, label := range []string{"In", "Out"} {
		if labelled(secs["Scope"], label) == "" {
			add("Scope needs a non-empty `%s:` line", label)
		}
	}
	for _, label := range []string{"May decide", "Must escalate", "Never"} {
		if labelled(secs["Authority"], label) == "" {
			add("Authority needs a non-empty `%s:` line", label)
		}
	}
	for _, dep := range depends(fm) {
		depDir, err := taskDir(root, dep)
		if err != nil {
			add("depends_on: %v", err)
			continue
		}
		if st, err := os.Stat(depDir); err != nil || !st.IsDir() {
			add("depends_on: no task %q", dep)
		} else if lastState(leadEntries(readStatus(depDir))) != "done" {
			add("depends_on: %q is not done yet", dep)
		}
	}
	if n := utf8.RuneCount(raw); n > briefCharLimit {
		warnings = append(warnings, fmt.Sprintf("task.md is %d characters; the lead re-reads it on every turn, so keep it under %d. Largest sections: %s", n, briefCharLimit, largestSections(secs, 3)))
	}
	return problems, warnings, nil
}

// largestSections names the n biggest sections with their sizes in characters.
func largestSections(secs map[string]string, n int) string {
	names := make([]string, 0, len(secs))
	for k := range secs {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if a, b := len(secs[names[i]]), len(secs[names[j]]); a != b {
			return a > b
		}
		return names[i] < names[j]
	})
	if len(names) > n {
		names = names[:n]
	}
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = fmt.Sprintf("%s (%d)", k, utf8.RuneCountInString(secs[k]))
	}
	return strings.Join(parts, ", ")
}

// setFrontMatter sets key in task.md's front matter, adding it before the
// closing line when it is absent.
func setFrontMatter(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(b)
	end := strings.Index(text, "\n---\n")
	if !strings.HasPrefix(text, "---\n") || end < 0 {
		return fail("%s has no front matter", path)
	}
	lines := strings.Split(text[4:end], "\n")
	set := false
	for i, l := range lines {
		if k, _, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == key {
			lines[i], set = key+": "+value, true
		}
	}
	if !set {
		lines = append(lines, key+": "+value)
	}
	return os.WriteFile(path, []byte("---\n"+strings.Join(lines, "\n")+text[end:]), 0o644)
}
