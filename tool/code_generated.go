package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Generated files. One function decides, for find, text, batch, sym callers,
// look-alikes and reuse candidates: a path pattern (the language registry and
// the universal list), a header marker in the first lines, or a
// `linguist-generated` attribute in the repo's .gitattributes. They are hidden
// by default and counted like tests (`--generated include|only`). A symbol's
// definition may live in a generated file (sqlc store methods, say): it is the
// real definition, shown labelled `generated`; generated files are still never
// callers, look-alikes or reuse candidates.

var genHeaderRe = regexp.MustCompile(`(?i)(code generated .*do not edit|auto-?generated|@generated|this file (?:was|is) (?:auto-?)?generated|do not edit)`)

const genHeaderLines = 5

// passGen applies the generated mode (none, include or only) to a path.
func (e *codeEnv) passGen(mode, path string) bool {
	switch mode {
	case "include":
		return true
	case "only":
		return e.isGenerated(path)
	}
	return !e.isGenerated(path)
}

// isGenerated is the single generated-file test.
func (e *codeEnv) isGenerated(path string) bool {
	if isGeneratedPath(path) {
		return true
	}
	e.genMu.Lock()
	v, ok := e.genCache[path]
	e.genMu.Unlock()
	if ok {
		return v
	}
	v = e.genHeader(path) || e.gitattrGenerated(path)
	e.genMu.Lock()
	if e.genCache == nil {
		e.genCache = map[string]bool{}
	}
	e.genCache[path] = v
	e.genMu.Unlock()
	return v
}

// genHeader reads only the first lines of a file.
func (e *codeEnv) genHeader(path string) bool {
	f, err := os.Open(filepath.Join(e.Code, filepath.FromSlash(path)))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 4096)
	for i := 0; i < genHeaderLines && sc.Scan(); i++ {
		if genHeaderRe.MatchString(sc.Text()) {
			return true
		}
	}
	_ = sc.Err() // a read error or an over-long line just means no header was found
	return false
}

type attrRule struct {
	re  *regexp.Regexp
	set bool
}

// attrGlob turns a .gitattributes pattern into a regular expression over a
// repo-relative slash path.
func attrGlob(p string) *regexp.Regexp {
	anchored := strings.HasPrefix(p, "/") || strings.Contains(strings.TrimSuffix(p, "/"), "/")
	p = strings.TrimPrefix(p, "/")
	dir := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			i++
			if i+1 < len(p) && p[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	pre := "^"
	if !anchored {
		pre = "^(?:.*/)?"
	}
	post := "$"
	if dir {
		post = "/.*$"
	}
	re, err := regexp.Compile(pre + b.String() + post)
	if err != nil {
		return nil
	}
	return re
}

func (e *codeEnv) gitattrGenerated(path string) bool {
	e.attrOnce.Do(func() {
		b, err := os.ReadFile(filepath.Join(e.Code, ".gitattributes"))
		if err != nil {
			return
		}
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 || strings.HasPrefix(f[0], "#") {
				continue
			}
			for _, a := range f[1:] {
				switch a {
				case "linguist-generated", "linguist-generated=true":
					if re := attrGlob(f[0]); re != nil {
						e.attrRules = append(e.attrRules, attrRule{re, true})
					}
				case "-linguist-generated", "linguist-generated=false":
					if re := attrGlob(f[0]); re != nil {
						e.attrRules = append(e.attrRules, attrRule{re, false})
					}
				}
			}
		}
	})
	gen := false
	for _, r := range e.attrRules { // the last matching line wins
		if r.re.MatchString(path) {
			gen = r.set
		}
	}
	return gen
}

// dropIgnored removes the paths git ignores. rg and the file lists already
// leave ignored files out; this guards what jbcontext returns, whose own
// handling of ignored files is not documented.
func (e *codeEnv) ignoredPaths(paths []string) map[string]bool {
	out := map[string]bool{}
	if len(paths) == 0 {
		return out
	}
	cmd := exec.Command("git", "check-ignore", "--stdin")
	cmd.Dir = e.Code
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	b, _ := cmd.Output() // exit 1 means nothing is ignored
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[l] = true
		}
	}
	return out
}
