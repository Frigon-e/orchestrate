package guard

import (
	"regexp"
	"strconv"
	"strings"
)

// The block texts of deny-read-range and deny-sed-inplace name the exact tool
// call to send instead. This file reads the sed, awk, head, tail, cat and perl
// command lines well enough to do that; a command it cannot read cleanly gets
// no translation, never a guessed one.

// plainFile is a file argument that names one file: no glob, expansion,
// quoting or blank, so a Read or Edit of it means what the shell command did.
func plainFile(f string) bool {
	return f != "" && f != "-" && !strings.HasPrefix(f, "-") && !strings.ContainsAny(f, "*?[]{}$`()<>|&;!\\ \t\n'\"")
}

func plainFiles(files []string) bool {
	for _, f := range files {
		if !plainFile(f) {
			return false
		}
	}
	return true
}

var digits = regexp.MustCompile(`^[0-9]+$`)

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ---- sed ----

type sedArgs struct {
	scripts []string
	files   []string
	quiet   bool
	ere     bool
	inplace bool
	ok      bool // every argument was understood
}

func parseSed(args []string) sedArgs {
	a := sedArgs{ok: true}
	var rest []string
	noMore := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if noMore || arg == "-" || !strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			continue
		}
		if arg == "--" {
			noMore = true
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name, val, hasVal := strings.Cut(arg, "=")
			switch name {
			case "--quiet", "--silent":
				a.quiet = true
			case "--regexp-extended":
				a.ere = true
			case "--in-place":
				a.inplace = true
			case "--posix", "--separate", "--null-data", "--unbuffered", "--sandbox", "--debug", "--follow-symlinks":
			case "--expression":
				switch {
				case hasVal:
					a.scripts = append(a.scripts, val)
				case i+1 < len(args):
					i++
					a.scripts = append(a.scripts, args[i])
				default:
					a.ok = false
				}
			default:
				a.ok = false
			}
			continue
		}
	cluster:
		for j := 1; j < len(arg); j++ {
			switch arg[j] {
			case 'n':
				a.quiet = true
			case 'E', 'r':
				a.ere = true
			case 's', 'u', 'z':
			case 'i': // the rest of the word is the backup suffix; BSD sed takes it as the next word
				a.inplace = true
				if arg == "-i" && i+1 < len(args) && args[i+1] == "" {
					i++
				}
				break cluster
			case 'e':
				switch {
				case j+1 < len(arg):
					a.scripts = append(a.scripts, arg[j+1:])
				case i+1 < len(args):
					i++
					a.scripts = append(a.scripts, args[i])
				default:
					a.ok = false
				}
				break cluster
			default:
				a.ok = false
				break cluster
			}
		}
	}
	if len(a.scripts) == 0 {
		if len(rest) == 0 {
			a.ok = false
		} else {
			a.scripts, rest = rest[:1], rest[1:]
		}
	}
	a.files = rest
	return a
}

var (
	sedPrintRange = regexp.MustCompile(`^([0-9]+)(?:,(\+?[0-9]+|\$))?p(?:;([0-9]+)q)?$`)
	sedKeepRange  = regexp.MustCompile(`^([0-9]+),([0-9]+)!d$`)
	sedQuitAt     = regexp.MustCompile(`^([0-9]+)q$`)
)

// sedSelection is the line range a print-only sed shows.
func sedSelection(a sedArgs) (offset, limit int, ok bool) {
	if !a.ok || a.inplace || len(a.scripts) != 1 {
		return
	}
	script := strings.TrimSpace(a.scripts[0])
	if a.quiet {
		m := sedPrintRange.FindStringSubmatch(script)
		if m == nil {
			return
		}
		start := atoi(m[1])
		end := start
		if m[2] == "$" { // a,$ runs to the end of the file: no limit
			if start < 1 || m[3] != "" {
				return
			}
			return start, 0, true
		}
		switch {
		case strings.HasPrefix(m[2], "+"):
			end = start + atoi(m[2][1:])
		case m[2] != "":
			end = atoi(m[2])
		}
		if start < 1 || end < start || (m[3] != "" && atoi(m[3]) < end) {
			return
		}
		return start, end - start + 1, true
	}
	if m := sedKeepRange.FindStringSubmatch(script); m != nil {
		start, end := atoi(m[1]), atoi(m[2])
		if start >= 1 && end >= start {
			return start, end - start + 1, true
		}
		return
	}
	if m := sedQuitAt.FindStringSubmatch(script); m != nil && atoi(m[1]) >= 1 {
		return 1, atoi(m[1]), true
	}
	return
}

// ---- head and tail ----

// countArg reads the line count of head or tail: -n N, -nN, -N, --lines=N or
// --lines N, default 10. Only tail takes +N (from line N): plus reports it.
func countArg(args []string, allowPlus bool) (n int, plus bool, files []string, ok bool) {
	value := ""
parse:
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			files = append(files, args[i+1:]...)
			break parse
		case arg == "-n" || arg == "--lines":
			if i+1 >= len(args) {
				return 0, false, nil, false
			}
			i++
			value = args[i]
		case strings.HasPrefix(arg, "--lines="):
			value = arg[len("--lines="):]
		case strings.HasPrefix(arg, "-n"):
			value = arg[2:]
		case len(arg) > 1 && arg[0] == '-' && digits.MatchString(arg[1:]):
			value = arg[1:]
		case arg != "-" && strings.HasPrefix(arg, "-"):
			return 0, false, nil, false
		default:
			files = append(files, arg)
		}
	}
	if value == "" {
		return 10, false, files, true
	}
	number := value
	if allowPlus {
		number = strings.TrimPrefix(value, "+")
	}
	if !digits.MatchString(number) || atoi(number) < 1 {
		return 0, false, nil, false
	}
	return atoi(number), number != value, files, true
}

// ---- awk ----

type awkArgs struct {
	program string
	hasProg bool
	files   []string
	simple  bool // no option but -i inplace
	inplace bool
}

func parseAwk(args []string) awkArgs {
	a := awkArgs{simple: true}
	fromFile := false
	i := 0
loop:
	for ; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			i++
			break loop
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			break loop
		case arg == "-i" && i+1 < len(args) && args[i+1] == "inplace":
			a.inplace = true
			i++
		case arg == "-iinplace":
			a.inplace = true
		case arg == "-v" || arg == "-F" || arg == "-f" || arg == "-i" || arg == "-e" || arg == "--source" ||
			arg == "--include" || arg == "--assign" || arg == "--field-separator" || arg == "--file":
			a.simple = false
			fromFile = fromFile || arg == "-f" || arg == "--file"
			i++
		default:
			a.simple = false
		}
	}
	if i > len(args) {
		i = len(args)
	}
	switch {
	case fromFile:
		a.files = args[i:]
	case i < len(args):
		a.program, a.hasProg, a.files = args[i], true, args[i+1:]
	}
	return a
}

var (
	awkTerm   = regexp.MustCompile(`^F?NR\s*(==|>=|<=|>|<)\s*([0-9]+)$`)
	awkSpan   = regexp.MustCompile(`^F?NR\s*==\s*([0-9]+)\s*,\s*F?NR\s*==\s*([0-9]+)$`)
	awkAction = regexp.MustCompile(`^(.*?)\s*\{\s*print(?:\s+\$0)?\s*(;\s*exit)?\s*;?\s*\}$`)
)

// awkLines reads a condition on NR (terms joined by &&, or the span NR==A,NR==B)
// as the lines it selects: from lo to hi, hi 0 meaning to the end.
func awkLines(cond string) (lo, hi int, ok bool) {
	cond = strings.TrimSpace(cond)
	if m := awkSpan.FindStringSubmatch(cond); m != nil {
		lo, hi = atoi(m[1]), atoi(m[2])
		return lo, hi, lo >= 1 && hi >= lo
	}
	lo = 1
	hasHi := false
	upper := func(n int) {
		if !hasHi || n < hi {
			hi, hasHi = n, true
		}
	}
	for _, term := range strings.Split(cond, "&&") {
		m := awkTerm.FindStringSubmatch(strings.TrimSpace(term))
		if m == nil || atoi(m[2]) < 1 {
			return 0, 0, false
		}
		n := atoi(m[2])
		switch m[1] {
		case "==":
			lo = max(lo, n)
			upper(n)
		case ">=":
			lo = max(lo, n)
		case ">":
			lo = max(lo, n+1)
		case "<=":
			upper(n)
		case "<":
			upper(n - 1)
		}
	}
	if hasHi && hi < lo {
		return 0, 0, false
	}
	if !hasHi {
		hi = 0
	}
	return lo, hi, true
}

// awkSelection is the line range a print-only awk program shows.
func awkSelection(a awkArgs) (offset, limit int, ok bool) {
	if !a.simple || a.inplace || !a.hasProg {
		return
	}
	cond, exit := strings.TrimSpace(a.program), false
	if m := awkAction.FindStringSubmatch(cond); m != nil {
		cond, exit = m[1], m[2] != ""
	}
	lo, hi, ok := awkLines(cond)
	if !ok || (exit && (hi == 0 || hi != lo || awkSpan.MatchString(strings.TrimSpace(cond)))) {
		return 0, 0, false
	}
	if hi == 0 {
		return lo, 0, true
	}
	return lo, hi - lo + 1, true
}

// ---- read ranges ----

type readRange struct {
	file          string
	offset, limit int // limit 0: to the end of the file
}

// selection is the line range one print-only call shows, and the files it reads.
func selection(name string, args []string) (offset, limit int, files []string, ok bool) {
	switch {
	case sedFamily(name):
		a := parseSed(args)
		offset, limit, ok = sedSelection(a)
		return offset, limit, a.files, ok
	case awkFamily(name):
		a := parseAwk(args)
		offset, limit, ok = awkSelection(a)
		return offset, limit, a.files, ok
	case name == "head":
		n, _, files, ok := countArg(args, false)
		return 1, n, files, ok
	case name == "tail":
		n, plus, files, ok := countArg(args, true)
		if !ok || !plus {
			return 0, 0, nil, false
		}
		return n, 0, files, true
	}
	return
}

// catFile is the one file of a plain cat [-n] FILE.
func catFile(args []string) (string, bool) {
	var files []string
	for _, a := range args {
		if a == "-n" {
			continue
		}
		files = append(files, a)
	}
	if len(files) == 1 && plainFile(files[0]) {
		return files[0], true
	}
	return "", false
}

func clean(s segment) bool { return !s.stdin && !s.stdout && !s.redirect }

// pipelineRange reads the two-segment pipelines that are still one range read.
func pipelineRange(a, b segment) (readRange, bool) {
	if a.stdin || a.redirect || !a.stdout || !b.stdin || b.stdout || b.redirect {
		return readRange{}, false
	}
	an, aa := command(a)
	bn, ba := command(b)
	switch {
	case an == "cat":
		file, ok := catFile(aa)
		if !ok {
			return readRange{}, false
		}
		offset, limit, files, ok := selection(bn, ba)
		if ok && len(files) == 0 {
			return readRange{file, offset, limit}, true
		}
	case an == "head" && bn == "tail":
		n, _, files, ok := countArg(aa, false)
		k, plus, more, okTail := countArg(ba, true)
		if ok && okTail && !plus && len(files) == 1 && plainFile(files[0]) && len(more) == 0 && k <= n {
			return readRange{files[0], n - k + 1, k}, true
		}
	case an == "tail" && bn == "head":
		from, plus, files, ok := countArg(aa, true)
		n, _, more, okHead := countArg(ba, false)
		if ok && okHead && plus && len(files) == 1 && plainFile(files[0]) && len(more) == 0 {
			return readRange{files[0], from, n}, true
		}
	}
	return readRange{}, false
}

// readRangeHit is the first clear line-range read of one file whose output goes
// nowhere else.
func readRangeHit(line string) (readRange, bool) {
	segs := segments(line, false)
	if len(segs) == 2 && segs[0].stdout {
		return pipelineRange(segs[0], segs[1])
	}
	for _, s := range segs {
		if !clean(s) {
			continue
		}
		name, args := command(s)
		if offset, limit, files, ok := selection(name, args); ok && len(files) == 1 && plainFile(files[0]) {
			return readRange{files[0], offset, limit}, true
		}
	}
	return readRange{}, false
}

func readRangeReason(r readRange, h harness) string {
	head := Marker + " (deny-read-range): sed, awk, head and tail output is not a Read, so a later Edit of the file fails.\n"
	text := "Send " + h.readCall(r.file, r.offset, r.limit) + " instead, as a parallel call in the same message as your other commands."
	if h.absPath && !strings.HasPrefix(r.file, "/") {
		text += " " + h.read + " needs an absolute path."
	}
	return head + text
}

// ---- rewrites ----

// An edit is what a rewriting command does, in the terms of the Edit tool.
type editKind int

const (
	kindNone editKind = iota // no clear translation
	kindSubst
	kindDelete
	kindInsert
	kindGrepDel
)

type edit struct {
	kind     editKind
	old, new string
	all      bool
	lo, hi   int // the lines it touches, 0 for the whole file
	text     string
	after    bool
}

// literalPattern resolves a sed, awk or perl pattern to the plain text it
// matches, or reports that it is a real regular expression.
func literalPattern(p string, delim byte, ere, perl bool) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\':
			if i+1 >= len(p) {
				return "", false
			}
			i++
			if n := p[i]; n == delim || strings.IndexByte(".*[]^$\\/", n) >= 0 {
				b.WriteByte(n)
				continue
			}
			return "", false
		case strings.IndexByte(".*[]^$", c) >= 0:
			return "", false
		case (ere || perl) && strings.IndexByte("+?(){}|", c) >= 0:
			return "", false
		case perl && c == '@':
			return "", false
		}
		b.WriteByte(c)
	}
	return b.String(), b.Len() > 0
}

// literalReplacement resolves a replacement to plain text, or reports that it
// uses a group, & or an escape.
func literalReplacement(r string, delim byte, perl bool) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '\\':
			if i+1 >= len(r) {
				return "", false
			}
			i++
			if n := r[i]; n == delim || n == '&' || n == '\\' || n == '/' {
				b.WriteByte(n)
				continue
			}
			return "", false
		case c == '&', perl && (c == '$' || c == '@'):
			return "", false
		}
		b.WriteByte(c)
	}
	return b.String(), true
}

// splitSubst splits the text after s<delim> into pattern, replacement and flags.
func splitSubst(body string, delim byte) (pat, rep, flags string, ok bool) {
	var cur strings.Builder
	parts := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '\\' && i+1 < len(body):
			cur.WriteByte(c)
			cur.WriteByte(body[i+1])
			i++
		case c == delim:
			if parts == 0 {
				pat = cur.String()
				cur.Reset()
				parts = 1
				continue
			}
			return pat, cur.String(), body[i+1:], true
		default:
			cur.WriteByte(c)
		}
	}
	return "", "", "", false
}

func substEdit(delim byte, body string, ere, perl bool) (edit, bool) {
	pat, rep, flags, ok := splitSubst(body, delim)
	if !ok || (flags != "" && flags != "g") {
		return edit{}, false
	}
	old, okPat := literalPattern(pat, delim, ere, perl)
	repl, okRep := literalReplacement(rep, delim, perl)
	if !okPat || !okRep {
		return edit{}, false
	}
	return edit{kind: kindSubst, old: old, new: repl, all: flags == "g"}, true
}

var (
	sedDelete  = regexp.MustCompile(`^([0-9]+)(?:,([0-9]+))?d$`)
	sedSubst   = regexp.MustCompile(`(?s)^(?:([0-9]+)(?:,([0-9]+))?)?s([^\w\s\\])(.*)$`)
	sedInsert  = regexp.MustCompile(`(?s)^([0-9]+)([ia])(.*)$`)
	sedGrepDel = regexp.MustCompile(`^/([^/\\]+)/d$`)
)

// lineSpan reads an optional address pair; hi defaults to lo.
func lineSpan(first, last string) (lo, hi int, ok bool) {
	lo = atoi(first)
	hi = lo
	if last != "" {
		hi = atoi(last)
	}
	return lo, hi, lo >= 1 && hi >= lo
}

func sedEdit(script string, ere bool) (edit, bool) {
	script = strings.TrimSpace(script)
	if m := sedDelete.FindStringSubmatch(script); m != nil {
		if lo, hi, ok := lineSpan(m[1], m[2]); ok {
			return edit{kind: kindDelete, lo: lo, hi: hi}, true
		}
		return edit{}, false
	}
	if m := sedSubst.FindStringSubmatch(script); m != nil {
		e, ok := substEdit(m[3][0], m[4], ere, false)
		if m[1] != "" {
			lo, hi, okSpan := lineSpan(m[1], m[2])
			e.lo, e.hi = lo, hi
			ok = ok && okSpan
		}
		return e, ok
	}
	if m := sedInsert.FindStringSubmatch(script); m != nil {
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(m[3], " \t"), "\\"))
		lo := atoi(m[1])
		if lo < 1 || text == "" || strings.ContainsAny(text, "\\\n") {
			return edit{}, false
		}
		return edit{kind: kindInsert, lo: lo, hi: lo, text: text, after: m[2] == "a"}, true
	}
	if m := sedGrepDel.FindStringSubmatch(script); m != nil {
		old, ok := literalPattern(m[1], '/', ere, false)
		if !ok || strings.Contains(old, "'") {
			return edit{}, false
		}
		return edit{kind: kindGrepDel, old: old}, true
	}
	return edit{}, false
}

var (
	perlSubst   = regexp.MustCompile(`(?s)^s([^\w\s\\{(\[<])(.*)$`)
	awkSubstPat = regexp.MustCompile(`^\{\s*(g?sub)\(/(.*?)/\s*,\s*"([^"\\&]*)"\s*\)\s*;?\s*\}\s*1$`)
	awkSkipLine = regexp.MustCompile(`^NR\s*!=\s*([0-9]+)$`)
	awkSkipSpan = regexp.MustCompile(`^!\s*\((.*)\)$`)
)

func perlEdit(script string) (edit, bool) {
	m := perlSubst.FindStringSubmatch(strings.TrimSpace(script))
	if m == nil {
		return edit{}, false
	}
	return substEdit(m[1][0], m[2], true, true)
}

func awkEdit(program string) (edit, bool) {
	program = strings.TrimSpace(program)
	if m := awkSubstPat.FindStringSubmatch(program); m != nil {
		old, ok := literalPattern(m[2], '/', true, false)
		if !ok {
			return edit{}, false
		}
		return edit{kind: kindSubst, old: old, new: m[3], all: m[1] == "gsub"}, true
	}
	if m := awkSkipLine.FindStringSubmatch(program); m != nil && atoi(m[1]) >= 1 {
		return edit{kind: kindDelete, lo: atoi(m[1]), hi: atoi(m[1])}, true
	}
	if m := awkSkipSpan.FindStringSubmatch(program); m != nil {
		if lo, hi, ok := awkLines(m[1]); ok && hi != 0 {
			return edit{kind: kindDelete, lo: lo, hi: hi}, true
		}
	}
	return edit{}, false
}

// perlOpts is a perl one-liner's switches. A switch cluster is read letter by
// letter: a letter that takes a value (-M, -I, -e ...) ends it, so -MTime::HiRes
// or -Ilib/util is not -i. Switches end at -- or the script name.
type perlOpts struct {
	script   string   // the -e code, "" when there is none
	files    []string // the arguments after the switches, only when there is a script
	printing bool     // -p
	inplace  bool     // -i
	multi    bool     // more than one -e: no single edit
}

func perlParts(args []string) perlOpts {
	var o perlOpts
	k := 0
	for ; k < len(args); k++ {
		a := args[k]
		if a == "--" {
			k++
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
	cluster:
		for j := 1; j < len(a); j++ {
			c := a[j]
			switch {
			case c == 'p':
				o.printing = true
			case c == 'i':
				o.inplace = true
				break cluster
			case c == 'e' || c == 'E':
				if j == len(a)-1 && k+1 < len(args) {
					k++
					o.multi = o.multi || o.script != ""
					o.script = args[k]
				}
				break cluster
			case c == 'l' || c == '0': // an optional number follows, then more switches
				octal := "01234567"
				if c == '0' && j+1 < len(a) && a[j+1] == 'x' {
					octal = "0123456789abcdefABCDEF"
					j++
				}
				for j+1 < len(a) && strings.IndexByte(octal, a[j+1]) >= 0 {
					j++
				}
			case strings.IndexByte("MmIxCdDVF", c) >= 0:
				break cluster
			}
		}
	}
	if o.script == "" || k > len(args) {
		o.script, o.printing = "", false
		return o
	}
	o.files = args[k:]
	return o
}

// A rewrite is a command that rewrites a file: in place, or into a temp file
// that is moved back.
type rewrite struct {
	files []string
	edit  edit
}

func sedFamily(name string) bool { return name == "sed" || name == "gsed" }

func awkFamily(name string) bool { return name == "awk" || name == "gawk" || name == "mawk" }

// movedBack is true when a later mv puts target over one of the files.
func movedBack(later []segment, target string, files []string) (string, bool) {
	for _, s := range later {
		if name, args := command(s); name == "mv" {
			var ops []string
			for _, a := range args {
				if a != "-f" {
					ops = append(ops, a)
				}
			}
			if len(ops) == 2 && ops[0] == target {
				for _, f := range files {
					if f == ops[1] {
						return f, true
					}
				}
			}
		}
	}
	return "", false
}

func rewriteHit(line string) (rewrite, bool) {
	segs := segments(line, false)
	var first rewrite
	found := false
	for i, s := range segs {
		name, args := command(s)
		var files []string
		var e edit
		inplace := false
		switch {
		case sedFamily(name):
			a := parseSed(args)
			files, inplace = a.files, inPlaceEdit(args)
			if a.ok && len(a.scripts) == 1 {
				if ed, ok := sedEdit(a.scripts[0], a.ere); ok {
					e = ed
				}
			}
		case name == "perl":
			p := perlParts(args)
			files, inplace = p.files, p.inplace
			if p.printing && !p.multi {
				if ed, ok := perlEdit(p.script); ok {
					e = ed
				}
			}
		case awkFamily(name):
			a := parseAwk(args)
			files, inplace = a.files, a.inplace && len(a.files) > 0
			if a.hasProg && a.simple {
				if ed, ok := awkEdit(a.program); ok {
					e = ed
				}
			}
		default:
			continue
		}
		// A command whose only output is a > redirect, moved back over its source later.
		if !inplace && s.target != "" && !s.stdout {
			if f, ok := movedBack(segs[i+1:], s.target, files); ok {
				files, inplace = []string{f}, true
			}
		}
		if !inplace {
			continue
		}
		if len(files) == 0 {
			e = edit{}
		}
		if found { // several rewrites: no single translation
			first.edit = edit{}
			continue
		}
		first, found = rewrite{files: files, edit: e}, true
	}
	return first, found
}

func rgFlag(base, text string) string {
	if regexp.QuoteMeta(text) != text {
		return base + "F"
	}
	return base
}

// hereLine is the exact call that does what the rewrite does, "" when there is
// no clear one.
func hereLine(rw rewrite, h harness) string {
	here := hereBody(rw, h)
	if here == "" {
		return ""
	}
	here += "."
	if plainFiles(rw.files) {
		here += h.absNote(rw.files...)
	}
	return here
}

func hereBody(rw rewrite, h harness) string {
	e := rw.edit
	if e.kind == kindNone {
		return ""
	}
	plain := plainFiles(rw.files)
	files := rw.files
	read := func() string { return h.readCall(files[0], e.lo, e.hi-e.lo+1) }
	lines := "that line"
	if e.hi > e.lo {
		lines = "those lines"
	}
	switch e.kind {
	case kindSubst:
		if e.lo > 0 {
			if len(files) != 1 || !plain {
				return ""
			}
			return read() + ", then " + h.edit + " " + h.replacement(e.old, e.new, false) + " in " + lines
		}
		args := h.replacement(e.old, e.new, e.all)
		if !plain {
			if len(files) == 0 || strings.Contains(e.old, "'") {
				return ""
			}
			return "find the files with rg " + rgFlag("-l", e.old) + " '" + e.old + "', then " + h.edit + " each with " + args
		}
		switch {
		case len(files) == 1:
			return h.edit + " " + h.path + "=" + files[0] + " " + args
		case len(files) <= 3:
			return h.edit + " each of " + strings.Join(files, ", ") + " with " + args
		}
	case kindDelete:
		if len(files) == 1 && plain {
			return read() + ", then " + h.edit + " " + lines + " out (" + h.newText + ` "")`
		}
	case kindInsert:
		where := "before"
		if e.after {
			where = "after"
		}
		if len(files) == 1 && plain {
			return read() + ", then " + h.edit + " " + lines + " to add " + strconv.Quote(e.text) + " " + where + " it"
		}
	case kindGrepDel:
		if len(files) >= 1 && len(files) <= 3 && plain {
			return "find the lines with rg " + rgFlag("-n", e.old) + " '" + e.old + "' " + strings.Join(files, " ") + ", then " + h.edit + " them out"
		}
	}
	return ""
}
