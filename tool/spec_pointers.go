package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// checkPointer uses the same declaration registry as ctx. Unknown languages
// have only word evidence, which callers must label as a fallback.
func checkPointer(repo, ptr string) (string, error) {
	m := pointerRe.FindStringSubmatch(ptr)
	path := ptr
	if m != nil {
		path = m[1]
	}
	full := filepath.Join(repo, filepath.FromSlash(path))
	rel, err := filepath.Rel(repo, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("pointer %s is outside the repo", ptr)
	}
	if info, err := os.Stat(full); err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("pointer %s is not a regular file", ptr)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		if m != nil && m[4] == "" {
			return "", fmt.Errorf("pointer %s does not resolve to a line in the repo (file %s could not be read: %v)", ptr, path, err)
		}
		return "", fmt.Errorf("pointer %s: file %s could not be read: %v", ptr, path, err)
	}
	if m == nil {
		return "", nil
	}
	if m[4] == "" {
		start, _ := strconv.Atoi(m[2])
		end := start
		if m[3] != "" {
			end, _ = strconv.Atoi(m[3])
		}
		lines := bytes.Count(b, []byte("\n")) + 1
		if start < 1 || end < start || end > lines {
			return "", fmt.Errorf("pointer %s does not resolve to a line in the repo (file has %d lines)", ptr, lines)
		}
		return "", nil
	}
	name := m[4]
	base := name[strings.LastIndex(name, ".")+1:]
	ls := strings.Split(string(b), "\n")
	l := langOf(path)
	if l != nil && len(l.Decl) > 0 {
		re := regexp.MustCompile(declPattern(l, base))
		var found []string
		for i, line := range ls {
			if !re.MatchString(line) {
				continue
			}
			_, declared, _ := l.declOfLine(line)
			if !strings.Contains(name, ".") || declared == name {
				return "", nil
			}
			owner := strings.TrimSuffix(name, "."+base)
			scope := enclosing(ls, i+1, path)
			if strings.HasSuffix(scope, " "+owner) {
				return "", nil
			}
			found = append(found, fmt.Sprintf("%s:%d (%s)", path, i+1, declared))
		}
		return "", fmt.Errorf("pointer %s: no declaration of %s in %s; found %s", ptr, name, path, orDefault(strings.Join(found, ", "), "none"))
	}
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(base) + `\b`)
	if word.Match(b) {
		return fmt.Sprintf("pointer %s resolved by word match in %s (no language declaration tier)", ptr, path), nil
	}
	return "", fmt.Errorf("pointer %s: no language declaration tier; word %s not found in %s", ptr, base, path)
}
