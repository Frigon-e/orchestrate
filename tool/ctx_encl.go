package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Reading code around a hit without opening the file: the declaration that
// encloses a line, the doc comment and body of a definition, and how big the
// package around a file is. All of it is heuristic text reading (column-0
// declarations), never a parse, and is labelled that way where it matters.

func indentOf(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// enclosing names the declaration that holds line ln (1-based): "func
// StartCheckout", "method Stripe.Pay", "class Cart". It is the nearest
// declaration above with less indentation (in a language with brackets, a
// column-0 line belongs to the top-level declaration above it). It is empty when the line is itself a top-level
// declaration, when none is found within 800 lines above, or for a language it
// does not read.
func enclosing(ls []string, ln int, path string) string {
	_, name := enclosingAt(ls, ln, path)
	return name
}

// enclosingAt is enclosing with the 1-based line the enclosing declaration
// starts on (0 when there is none).
func enclosingAt(ls []string, ln int, path string) (int, string) {
	l := langOf(path)
	if l == nil || ln < 1 || ln > len(ls) {
		return 0, ""
	}
	hitLine := ls[ln-1]
	hit := indentOf(hitLine)
	if k, _, _ := l.declOfLine(hitLine); k != "" && hit == 0 {
		return 0, ""
	}
	if hit == 0 && l.BraceEnd && isCommentLine(l, strings.TrimSpace(hitLine)) {
		// A comment at column 0 under a closed declaration belongs to no function.
		for i := ln - 2; i >= 0; i-- {
			t := strings.TrimSpace(ls[i])
			if t == "" || isCommentLine(l, t) {
				continue
			}
			if indentOf(ls[i]) == 0 && (t[0] == '}' || t[0] == ')') {
				return 0, ""
			}
			break
		}
	}
	for i := ln - 2; i >= 0 && ln-i <= 800; i-- {
		if strings.TrimSpace(ls[i]) == "" {
			continue
		}
		k, n, top := l.declOfLine(ls[i])
		if k == "" {
			continue
		}
		c := indentOf(ls[i])
		if top && c > 0 {
			continue
		}
		if c < hit || (c == 0 && l.BraceEnd) {
			return i + 1, k + " " + n
		}
	}
	return 0, ""
}

// keepIndent keeps a line's shape but spends one character per leading tab.
func keepIndent(s string) string {
	s = strings.TrimRight(s, " \t\r")
	t := strings.TrimLeft(s, "\t")
	return strings.Repeat(" ", len(s)-len(t)) + t
}

// docBlock is the comment block right above line ln, markers removed.
func docBlock(ls []string, ln int, path string) []string {
	l := langOf(path)
	top := -1
	for i := ln - 2; i >= 0 && i < len(ls); i-- {
		if isCommentLine(l, strings.TrimSpace(ls[i])) {
			top = i
			continue
		}
		break
	}
	if top < 0 {
		return nil
	}
	var out []string
	for i := top; i < ln-1; i++ {
		t := strings.TrimSpace(ls[i])
		t = strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(t, "/**"), "/*"), "/*#!"))
		t = strings.TrimSpace(strings.TrimSuffix(t, "*/"))
		if t = strings.TrimSpace(strings.TrimPrefix(t, "* ")); t != "" && t != "*" {
			out = append(out, t)
		}
	}
	return out
}

// bodyPreview is the definition starting at line ln: up to max lines, and how
// many more lines the definition has. A definition ends where its brackets
// close; a single statement ends on its line.
func bodyPreview(ls []string, ln, max int) (body []string, more int) {
	bal, opened, end := 0, false, ln-1
	for i := ln - 1; i < len(ls) && i < ln+399; i++ {
		l := strings.TrimSpace(ls[i])
		if !strings.HasPrefix(l, "//") {
			for _, c := range l {
				switch c {
				case '{', '(', '[':
					bal++
					opened = true
				case '}', ')', ']':
					bal--
				}
			}
		}
		end = i
		chained := i+1 < len(ls) && strings.HasPrefix(strings.TrimSpace(ls[i+1]), ".")
		if !chained && ((opened && bal <= 0) || (!opened && !strings.HasSuffix(l, "=") && !strings.HasSuffix(l, ",") && !strings.HasSuffix(l, "=>"))) {
			break
		}
	}
	for i := ln - 1; i <= end && i < len(ls); i++ {
		if len(body) >= max {
			more = end - i + 1
			break
		}
		body = append(body, ls[i])
	}
	return body, more
}

// declNames lists the declarations that start in lines, as "func A, type B".
func declNames(ls []string, path string) []string {
	l := langOf(path)
	if l == nil {
		return nil
	}
	var out []string
	for _, line := range ls {
		if k, n, _ := l.declOfLine(line); k != "" && n != "" && indentOf(line) == 0 {
			out = append(out, k+" "+n)
		}
	}
	return out
}

// pkgExports sizes the package around a file: how many exported symbols its
// directory declares, and in how many files of the same language.
func (e *ctxEnv) pkgExports(path string) string {
	l := langOf(path)
	if l == nil {
		return ""
	}
	dir, n, nf := filepath.ToSlash(filepath.Dir(path)), 0, 0
	for _, f := range e.files() {
		if filepath.ToSlash(filepath.Dir(f)) != dir || langOf(f) != l || isTestPath(f) || nf >= 80 {
			continue
		}
		nf++
		for _, line := range e.Lines(f) {
			if k, name, _ := l.declOfLine(line); k != "" && indentOf(line) == 0 && l.isExported(line, name) {
				n++
			}
		}
	}
	if nf == 0 {
		return ""
	}
	return fmt.Sprintf("package %s declares %d exported symbols in %d file%s", dir, n, nf, plural(nf))
}

// The lines a definition shows: a type whole up to defTypeLines, a function's
// signature and first defFuncLines lines, a const or var constWindowLines
// lines (see constWindow), never more than constMaxLines.
const (
	defTypeLines     = 120
	defFuncLines     = 40
	constWindowLines = 30
	constMaxLines    = 60
)

// constWindow picks the lines to show for the const or var defined at line ln
// (1-based, inclusive bounds). Inside a function whose start is within
// constWindowLines above it, the window starts at the function and runs
// constWindowLines lines, so the value reads in its function. Anywhere else it
// is constWindowLines lines around the definition. Either way the whole
// declaration is inside, up to constMaxLines.
func constWindow(ls []string, ln int, path string) (start, end int) {
	if ln < 1 || ln > len(ls) {
		return ln, ln - 1 // the file does not hold the line: an empty window
	}
	decl, _ := bodyPreview(ls, ln, constMaxLines)
	declEnd := ln + len(decl) - 1
	if fn, _ := enclosingAt(ls, ln, path); fn > 0 && ln-fn <= constWindowLines {
		start = fn
	} else {
		start = max(ln-constWindowLines/2, 1)
	}
	end = max(start+constWindowLines-1, declEnd)
	end = min(end, len(ls), start+constMaxLines-1)
	return start, end
}

// cleanDocLine is a comment line without its marker.
func cleanDocLine(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(t, "/**"), "/*"), "/*#!"))
	t = strings.TrimSpace(strings.TrimSuffix(t, "*/"))
	return strings.TrimSpace(strings.TrimPrefix(t, "* "))
}

// defBody is the definition whose position is line ln, as its doc and its code.
// The doc is what the definition says about itself and is not counted against
// max: comment lines at ln (when the position points at the comment above the
// code) and a Python docstring. The code starts at the signature and runs up
// to max lines, or to the end of the definition, whichever comes first: where
// brackets close, or, in a language without them, where the indentation falls
// back. start is the line the code starts on, more the lines past max.
func defBody(ls []string, ln int, path string, max int) (doc, body []string, start, more int) {
	l := langOf(path)
	start = ln
	if ln < 1 || ln > len(ls) {
		return nil, nil, ln, 0 // the file does not hold the line
	}
	for start <= len(ls) && isCommentLine(l, strings.TrimSpace(ls[start-1])) {
		if t := cleanDocLine(ls[start-1]); t != "" && t != "*" {
			doc = append(doc, t)
		}
		start++
	}
	if start > len(ls) {
		return doc, nil, ln, 0
	}
	if l == nil || (l.ID != "python" && l.ID != "ruby") {
		all, rest := bodyPreview(ls, start, 400)
		return doc, capLines(all, max, rest, &more), start, more
	}
	// Indentation decides: the definition holds the lines indented deeper than
	// its signature, and a closing "end" at the signature's indent.
	sig := indentOf(ls[start-1])
	end := start
	for i := start; i < len(ls) && i < start+400; i++ {
		t := strings.TrimSpace(ls[i])
		if t == "" {
			continue
		}
		if indentOf(ls[i]) > sig {
			end = i + 1
			continue
		}
		if l.ID == "ruby" && t == "end" {
			end = i + 1
		}
		break
	}
	lines := append([]string(nil), ls[start-1:end]...)
	if l.ID == "python" {
		// A docstring right after the signature is doc, not code.
		hdr := 0
		for hdr < len(lines) && !strings.HasSuffix(strings.TrimSpace(lines[hdr]), ":") {
			hdr++
		}
		if hdr+1 < len(lines) {
			first := strings.TrimSpace(lines[hdr+1])
			if q := docstringQuote(first); q != "" {
				last := hdr + 1
				for last < len(lines) {
					t := strings.TrimSpace(lines[last])
					if last > hdr+1 && strings.Contains(t, q) || last == hdr+1 && strings.Count(t, q) >= 2 {
						break
					}
					last++
				}
				last = min(last, len(lines)-1)
				for _, d := range lines[hdr+1 : last+1] {
					if t := strings.Trim(strings.TrimSpace(d), `"'rRbBuU`); t != "" {
						doc = append(doc, t)
					}
				}
				lines = append(lines[:hdr+1], lines[last+1:]...)
			}
		}
	}
	return doc, capLines(lines, max, 0, &more), start, more
}

// docstringQuote is the triple quote a Python line opens a docstring with.
func docstringQuote(t string) string {
	t = strings.TrimLeft(t, "rRbBuU")
	for _, q := range []string{`"""`, `'''`} {
		if strings.HasPrefix(t, q) {
			return q
		}
	}
	return ""
}

// capLines keeps the first max lines and counts the lines past them, and rest
// more that bodyPreview already cut.
func capLines(lines []string, max, rest int, more *int) []string {
	if len(lines) > max {
		*more = len(lines) - max + rest
		return lines[:max]
	}
	*more = rest
	return lines
}
