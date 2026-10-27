package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// The Claude counterpart of pi_ctx_only.ts: a PreToolUse hook on Bash that a
// gate pass is launched with runs `orch ctx-only-check`, which refuses any Bash
// command that is not exactly one plain lookup call. Prefix allow rules such as
// Bash(rg:*) cannot do this: a flag that runs another program (rg --pre, fd -x)
// may sit anywhere in the command. ctxOnlyRefusal mirrors `refusal` in
// pi_ctx_only.ts; both are tested against testdata/ctx_only_cases.json.

var ctxOnlyAllowed = regexp.MustCompile(`^(jbcontext search|orch lsp|rg|fd|ls|wc)(\s|$)`)

var (
	rgExecFlag = regexp.MustCompile(`^--(pre|pre-glob|hostname-bin)(=|$)`)
	fdExecFlag = regexp.MustCompile(`^--(exec|exec-batch)(=|$)`)
	fdExecCl   = regexp.MustCompile(`^-[A-Za-z]*[xX]`)
)

// ctxOnlyWords splits a command line that passed the quote checks, quotes removed.
func ctxOnlyWords(cmd string) []string {
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

// ctxOnlyExecFlag says why the flags of an rg or fd call are refused: they run another program.
func ctxOnlyExecFlag(cmd string) string {
	w := ctxOnlyWords(cmd)
	if len(w) == 0 {
		return ""
	}
	flags := w[1:]
	for i, f := range w {
		if i > 0 && f == "--" {
			flags = w[1:i]
			break
		}
	}
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

// ctxOnlyRefusal says why a command line is not one plain lookup call, or "" when it is.
func ctxOnlyRefusal(command string) string {
	cmd := strings.TrimSpace(command)
	if !ctxOnlyAllowed.MatchString(cmd) {
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
		} else if strings.ContainsRune(";&|<>(){}`$\\\n\r*?[]~!#", ch) {
			return fmt.Sprintf("no unquoted %q: no pipes, redirects, globs or substitutions", string(ch))
		}
	}
	if quote != 0 {
		return "unterminated quote"
	}
	return ctxOnlyExecFlag(cmd)
}

const ctxOnlyDenyTail = "This shell runs one command per call: jbcontext search, orch lsp, rg, fd, ls or wc, with no pipes, redirects or unquoted globs. " +
	"Put independent lookups in separate calls in one message. " +
	"Read files with the read tool (offset and limit); there is no cd, grep, head, tail, sed or cat."

// cmdCtxOnlyCheck is the hidden `orch ctx-only-check`: a Claude PreToolUse hook
// that reads the hook JSON on stdin and exits 2 with a reason for a Bash command
// that is not one allowed lookup. A payload it cannot read is refused too.
func cmdCtxOnlyCheck(a *app, args []string) error {
	raw, err := io.ReadAll(a.stdin)
	if err != nil {
		return &exitError{code: 2, msg: "ctx-only check could not read the hook input"}
	}
	var in struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return &exitError{code: 2, msg: "ctx-only check could not parse the hook input"}
	}
	if in.ToolName != "Bash" {
		return nil
	}
	if why := ctxOnlyRefusal(in.ToolInput.Command); why != "" {
		return &exitError{code: 2, msg: "Blocked: " + why + ". " + ctxOnlyDenyTail}
	}
	return nil
}
