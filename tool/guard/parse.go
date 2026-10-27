// Package guard holds the Claude Code tool guards and the lookup coach: the
// shell parsing, the deny rules, the big-read rule, the coach state and the
// lookup log record. cmd/orch-guard is its command line.
package guard

import (
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
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

// shellDepth caps how many bash -c scripts nest inside each other.
const shellDepth = 3

var shellFlagC = regexp.MustCompile(`^-[A-Za-z]*c[A-Za-z]*$`)

// segments parses a command line as bash and returns every call in source
// order: top level, both sides of && || ; and pipes, subshells, blocks, loops,
// branches, function bodies, command and process substitutions, and the script
// of bash -c. Heredoc bodies are data. With dqRaw a backslash inside double
// quotes stays unless it escapes a quote, a backslash, a dollar or a backtick,
// as bash does. A line the parser rejects falls back to tokenSegments, so the
// guard never fails closed.
func segments(command string, dqRaw bool) []segment {
	return parseSegments(command, dqRaw, 0)
}

type pipeFlags struct{ stdin, stdout, redirect bool }

func parseSegments(command string, dqRaw bool, depth int) (out []segment) {
	defer func() {
		if recover() != nil {
			out = tokenSegments(command, dqRaw)
		}
	}()
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return tokenSegments(command, dqRaw)
	}
	flags := map[syntax.Node]pipeFlags{}
	syntax.Walk(file, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Stmt:
			f := flags[n]
			for _, r := range n.Redirs {
				if r.Op != syntax.DplOut && r.Op != syntax.DplIn {
					f.redirect = true
				}
			}
			if b, ok := n.Cmd.(*syntax.BinaryCmd); ok {
				if b.Op == syntax.Pipe || b.Op == syntax.PipeAll {
					flags[b.X] = pipeFlags{stdin: f.stdin, stdout: true}
					flags[b.Y] = pipeFlags{stdin: true, stdout: f.stdout}
				} else {
					flags[b.X] = pipeFlags{stdin: f.stdin}
					flags[b.Y] = pipeFlags{stdout: f.stdout}
				}
			} else if n.Cmd != nil {
				flags[n.Cmd] = f
			}
		case *syntax.CallExpr:
			var words []word
			for _, a := range n.Assigns {
				words = append(words, word{command[a.Pos().Offset():a.End().Offset()], false})
			}
			for _, a := range n.Args {
				words = append(words, wordOf(a, command, dqRaw))
			}
			if len(words) == 0 {
				return true
			}
			f := flags[n]
			seg := segment{words, f.stdin, f.stdout, f.redirect}
			out = append(out, seg)
			if depth < shellDepth {
				if script, ok := shellScript(seg); ok {
					out = append(out, parseSegments(script, dqRaw, depth+1)...)
				}
			}
		}
		return true
	})
	return out
}

// shellScript is the script of a bash|sh|zsh|dash -c call, when it is literal.
func shellScript(s segment) (string, bool) {
	name, args := command(s)
	switch name {
	case "bash", "sh", "zsh", "dash":
	default:
		return "", false
	}
	for i, a := range args {
		if shellFlagC.MatchString(a) && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// wordOf is the text of one shell word: the value of literal and quoted parts,
// the source text of expansions. It is quoted when every part is quoted.
func wordOf(w *syntax.Word, src string, dqRaw bool) word {
	var text strings.Builder
	quoted := len(w.Parts) > 0
	for _, p := range w.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			text.WriteString(p.Value)
			quoted = false
		case *syntax.SglQuoted:
			text.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				if l, ok := q.(*syntax.Lit); ok {
					text.WriteString(dqUnescape(l.Value, dqRaw))
				} else {
					text.WriteString(src[q.Pos().Offset():q.End().Offset()])
				}
			}
		default:
			text.WriteString(src[p.Pos().Offset():p.End().Offset()])
			quoted = false
		}
	}
	return word{text.String(), quoted}
}

// dqUnescape resolves backslashes in the literal text of a double-quoted part.
func dqUnescape(v string, dqRaw bool) string {
	if !strings.Contains(v, "\\") {
		return v
	}
	var b strings.Builder
	escaped := false
	for _, ch := range v {
		if escaped {
			escaped = false
			if dqRaw && !strings.ContainsRune("\"\\$`", ch) {
				b.WriteByte('\\')
			}
			b.WriteRune(ch)
		} else if ch == '\\' {
			escaped = true
		} else {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// tokenSegments is the fallback tokenizer for lines the bash parser rejects. It
// splits a command line on unquoted separators, keeping quote awareness.
func tokenSegments(command string, dqRaw bool) []segment {
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
