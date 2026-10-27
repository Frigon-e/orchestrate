package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var snakeRe = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)

var bulletRe = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(.*\S)`)

// briefConceptHits is the number of chunks a gate enrichment concept query keeps.
const briefConceptHits = 4

// inOwns reports whether path is one of the owns entries or below one.
func inOwns(path string, owns []string) bool {
	for _, o := range owns {
		o = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(o), "./"), "/")
		if o != "" && (path == o || strings.HasPrefix(path, o+"/")) {
			return true
		}
	}
	return false
}

func addUniq(list []string, s string, max int) []string {
	if slices.Contains(list, s) {
		return list
	}
	if len(list) >= max {
		return list
	}
	return append(list, s)
}

// repoPath cleans a path and refuses one outside the repo.
func repoPath(p string) (string, bool) {
	p = filepath.ToSlash(filepath.Clean(strings.TrimPrefix(strings.TrimSpace(p), "./")))
	if p == "." || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

// passTests applies the tests mode (none, include or only) to a path.
func passTests(mode, path string) bool {
	switch mode {
	case "include":
		return true
	case "only":
		return isTestPath(path)
	}
	return !isTestPath(path)
}

// lineAt is line n of ls without its indentation, "" past the ends.
func lineAt(ls []string, n int, version ...int) string {
	if n < 1 || n > len(ls) {
		return ""
	}
	if len(version) > 0 && version[0] == 2 {
		return ls[n-1]
	}
	return strings.TrimSpace(ls[n-1])
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func homePath(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h+"/") {
		return "~" + p[len(h):]
	}
	return p
}

// docLine is the first line of the comment block right above line ln (1-based).
func docLine(lines []string, ln int) string {
	top := -1
	for i := ln - 2; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*") {
			top = i
			continue
		}
		break
	}
	if top < 0 {
		return ""
	}
	for i := top; i < ln-1; i++ {
		t := strings.TrimSpace(lines[i])
		t = strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(t, "/**"), "/*"), "/*"))
		t = strings.TrimSpace(strings.TrimSuffix(t, "*/"))
		if t != "" && t != "*" {
			return short(t, 140)
		}
	}
	return ""
}

func splitWhere(where string) (string, int) {
	i := strings.LastIndex(where, ":")
	if i < 0 {
		return where, 0
	}
	var n int
	fmt.Sscanf(where[i+1:], "%d", &n)
	return where[:i], n
}

func quoteList(ss []string) string {
	var out []string
	for _, s := range ss {
		out = append(out, "`"+s+"`")
	}
	return strings.Join(out, ", ")
}

// agentScalar is the shared boundary for legacy prose renderers and embedded
// metadata cells. It uses the same conforming encoder, not hand-written quoting.
func agentScalar(v string) string {
	s, _ := selectOutput(v) // A Go string is always JSON serializable.
	b, err := s.render(outputAgent)
	if err != nil { // encoding/json already guarantees valid UTF-8.
		panic(err)
	}
	return string(b)
}
