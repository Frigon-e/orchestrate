package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// The Claude counterpart of pi_lookup_only.ts: a PreToolUse hook on Bash that a
// gate pass is launched with runs `orch lookup-only-check`, which refuses any Bash
// command that is not exactly one plain lookup call. Prefix allow rules such as
// Bash(rg:*) cannot do this: a flag that runs another program (rg --pre, fd -x)
// may sit anywhere in the command. restrictedRefusal mirrors `refusal` in
// pi_lookup_only.ts; both are tested against testdata/lookup_only_cases.json.

var restrictedAllowed = regexp.MustCompile(`^(jbcontext search|orch lsp|rg|fd|ls|wc)(\s|$)`)

var (
	rgExecFlag = regexp.MustCompile(`^--(pre|pre-glob|hostname-bin)(=|$)`)
	fdExecFlag = regexp.MustCompile(`^--(exec|exec-batch)(=|$)`)
	fdExecCl   = regexp.MustCompile(`^-[A-Za-z]*[xX]`)
)

// restrictedWords splits a command line that passed the quote checks, quotes removed.
func restrictedWords(cmd string) []string {
	var out []string
	var cur strings.Builder
	has := false
	var quote rune
	for _, ch := range cmd {
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else {
				cur.WriteRune(ch)
			}
		case ch == '\'' || ch == '"':
			quote = ch
			has = true
		case unicode.IsSpace(ch):
			if has {
				out = append(out, cur.String())
			}
			cur.Reset()
			has = false
		default:
			cur.WriteRune(ch)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	return out
}

// restrictedExecFlag says why the flags of an rg or fd call are refused: they run another program.
// Every word is scanned, even after "--": a flag that takes a value (rg -g --)
// can swallow the "--", so it does not reliably end the flags. A pattern that
// looks like an exec flag is refused too.
func restrictedExecFlag(cmd string) string {
	w := restrictedWords(cmd)
	if len(w) == 0 {
		return ""
	}
	flags := w[1:]
	name := func(f string) string { return strings.SplitN(f, "=", 2)[0] }
	switch w[0] {
	case "rg":
		for _, f := range flags {
			if rgExecFlag.MatchString(f) {
				return "rg " + name(f) + " runs another program"
			}
		}
	case "fd":
		for _, f := range flags {
			if fdExecFlag.MatchString(f) || fdExecCl.MatchString(f) {
				return "fd " + name(f) + " runs another program"
			}
		}
	}
	return ""
}

// restrictedCharWhy says what a character that is refused outside quotes does.
func restrictedCharWhy(ch rune) string {
	switch ch {
	case '|':
		return "a pipe sends output to another command"
	case '<', '>':
		return "a redirect reads or writes a file"
	case '*', '?', '[', ']':
		return "an unquoted glob expands to file names; quote the pattern"
	case '$', '`':
		return "a substitution runs another command or expands a variable; quote it with single quotes"
	case '(', ')', '{', '}':
		return "a subshell or group"
	case '~':
		return "home-directory expansion"
	case '!':
		return "history expansion"
	case '#':
		return "a comment"
	case '\\':
		return "an escape; quote instead"
	case '\r':
		return "a carriage return"
	}
	return ""
}

// restrictedSplit cuts a command line at the unquoted ;, && and newline that join
// lookups, and at the unquoted || and lone & that do not. bad lists the refused
// connectors found. A lone | stays in its part, where the character check refuses it.
func restrictedSplit(cmd string) (parts []string, bad []string) {
	r := []rune(cmd)
	var cur strings.Builder
	var quote rune
	for i := 0; i < len(r); i++ {
		ch := r[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			cur.WriteRune(ch)
			continue
		}
		next := rune(0)
		if i+1 < len(r) {
			next = r[i+1]
		}
		switch {
		case ch == '\'' || ch == '"':
			quote = ch
			cur.WriteRune(ch)
			continue
		case ch == ';' || ch == '\n':
		case ch == '&' && next == '&':
			i++
		case ch == '&':
			bad = append(bad, "&")
		case ch == '|' && next == '|':
			i++
			bad = append(bad, "||")
		default:
			cur.WriteRune(ch)
			continue
		}
		parts = append(parts, strings.TrimSpace(cur.String()))
		cur.Reset()
	}
	parts = append(parts, strings.TrimSpace(cur.String()))
	return parts, bad
}

// restrictedPartRefusal says why one part of a command line is not a plain lookup call, or "" when it is.
func restrictedPartRefusal(cmd string) string {
	if !restrictedAllowed.MatchString(cmd) {
		return "only jbcontext search, orch lsp, rg, fd, ls and wc run in this shell"
	}
	var quote rune
	for _, ch := range cmd {
		if quote == '\'' {
			if ch == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if ch == '"' {
				quote = 0
			} else if ch == '$' || ch == '`' || ch == '\\' {
				return "no $, backtick or backslash inside double quotes (use single quotes)"
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
		} else if why := restrictedCharWhy(ch); why != "" {
			return fmt.Sprintf("unquoted %q: %s", string(ch), why)
		}
	}
	if quote != 0 {
		return "unterminated quote"
	}
	return restrictedExecFlag(cmd)
}

// restrictedRefusal says why a command line is not plain lookup calls joined by ; or &&, or "" when it is.
func restrictedRefusal(command string) string {
	parts, bad := restrictedSplit(strings.TrimSpace(command))
	for i, p := range parts {
		if p == "" && len(parts) > 1 {
			return "empty command between separators"
		}
		why := restrictedPartRefusal(p)
		if why == "" {
			continue
		}
		if len(parts) > 1 {
			text := []rune(p)
			if len(text) > 60 {
				text = text[:60]
			}
			return fmt.Sprintf("part %d of %d (%s): %s", i+1, len(parts), string(text), why)
		}
		return why
	}
	if len(bad) > 0 {
		why := `unquoted "||": || chains commands; only ; and && can join lookups`
		if bad[0] == "&" {
			why = `unquoted "&": & runs a command in the background`
		}
		return why + "\nSend these as separate calls in one message:\n- " + strings.Join(parts, "\n- ")
	}
	return ""
}

const restrictedDenyTail = "This shell runs one lookup per call, or several joined with ; or &&: jbcontext search, orch lsp, rg, fd, ls or wc, with no pipes, redirects or unquoted globs. " +
	"Independent lookups are better as separate calls in one message: they run in parallel and their output stays separate. " +
	"Read files with the read tool (offset and limit); there is no cd, grep, head, tail, sed, cat or find."

// cmdLookupOnlyCheck is the hidden `orch lookup-only-check`: a Claude PreToolUse hook
// that reads the hook JSON on stdin and exits 2 with a reason for a Bash command
// that is not one allowed lookup. A payload it cannot read is refused too.
func cmdLookupOnlyCheck(a *app, args []string) error {
	raw, err := io.ReadAll(a.stdin)
	if err != nil {
		return &exitError{code: 2, msg: "restricted-shell check could not read the hook input"}
	}
	var in struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return &exitError{code: 2, msg: "restricted-shell check could not parse the hook input"}
	}
	if in.ToolName != "Bash" {
		return nil
	}
	if why := restrictedRefusal(in.ToolInput.Command); why != "" {
		return &exitError{code: 2, msg: "Blocked: " + why + "\n" + restrictedDenyTail}
	}
	return nil
}
