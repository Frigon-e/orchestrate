// Package guard holds the Claude Code tool guards and the lookup coach: the
// shell parsing, the deny rules, the big-read rule, the coach state and the
// lookup log record. cmd/orch-guard is its command line.
package guard

import (
	"regexp"
	"strings"
)

// A word records whether it came from a quote, so a quoted word is never a command.
type word struct {
	text   string
	quoted bool
}

// A segment records a pipe on either side and a redirect, because a pipeline
// formats or forwards input and a redirect writes a file.
type segment struct {
	words    []word
	stdin    bool
	stdout   bool
	redirect bool
}

var (
	sudoValue = regexp.MustCompile(`^-[ugpCDhTr]$`)
	varPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	sudoFlag  = regexp.MustCompile(`^-[A-Za-z]`)
	inPlace   = regexp.MustCompile(`^-[A-Za-z]*i`)
	recursive = regexp.MustCompile(`^-[A-Za-z]*[rR]`)
	sedPrint  = regexp.MustCompile(`^[0-9$,; ]*p$`)
	sedFlagN  = regexp.MustCompile(`^-[A-Za-z]*n$`)
)

// segments splits a command line on unquoted separators, keeping quote
// awareness. With dqRaw a backslash inside double quotes stays unless it
// escapes a quote, a backslash, a dollar or a backtick, as bash does.
func segments(command string, dqRaw bool) []segment {
	var out []segment
	var words []word
	var text strings.Builder
	started, quoted := false, false
	var quote rune
	escaped := false
	stdinPipe, redirect := false, false

	endWord := func() {
		if !started {
			return
		}
		words = append(words, word{text.String(), quoted})
		text.Reset()
		started, quoted = false, false
	}
	endSegment := func(pipeOut bool) {
		endWord()
		if len(words) > 0 {
			out = append(out, segment{words, stdinPipe, pipeOut, redirect})
		}
		words = nil
		stdinPipe = pipeOut
		redirect = false
	}

	for _, ch := range command {
		if quote != 0 {
			started, quoted = true, true
			switch {
			case escaped:
				escaped = false
				if dqRaw && !strings.ContainsRune("\"\\$`", ch) {
					text.WriteByte('\\')
				}
				text.WriteRune(ch)
			case quote == '"' && ch == '\\':
				escaped = true
			case ch == quote:
				quote = 0
			default:
				text.WriteRune(ch)
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"':
			quote = ch
			started, quoted = true, true
		case ch == '>' || ch == '<':
			endWord()
			redirect = true
		case ch == '|':
			endSegment(true)
		case strings.ContainsRune(";&\n\r()", ch):
			endSegment(false)
		case ch == ' ' || ch == '\t':
			endWord()
		default:
			text.WriteRune(ch)
			started = true
		}
	}
	endSegment(false)
	return out
}

func base(w string) string {
	parts := strings.Split(strings.ReplaceAll(w, "\\", ""), "/")
	return parts[len(parts)-1]
}

// command is the command a segment runs, with its arguments. Leading VAR=value
// prefixes, sudo and a path to the tool do not hide the command from a rule.
func command(s segment) (string, []string) {
	words := s.words
	at, total := 0, len(words)
	for at < total && !words[at].quoted && varPrefix.MatchString(words[at].text) {
		at++
	}
	for at < total && !words[at].quoted && base(words[at].text) == "sudo" {
		at++
		for at < total && !words[at].quoted && sudoFlag.MatchString(words[at].text) {
			flag := words[at].text
			at++
			if sudoValue.MatchString(flag) {
				at++
			}
		}
		for at < total && !words[at].quoted && varPrefix.MatchString(words[at].text) {
			at++
		}
	}
	if at >= total || words[at].quoted {
		return "", nil
	}
	args := make([]string, 0, total-at-1)
	for _, w := range words[at+1:] {
		args = append(args, w.text)
	}
	return base(words[at].text), args
}

func set(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

func anyOf(args []string, f func(string) bool) bool {
	for _, a := range args {
		if f(a) {
			return true
		}
	}
	return false
}
