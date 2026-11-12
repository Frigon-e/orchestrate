package guard

import (
	"fmt"
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
	sedPrintRange = regexp.MustCompile(`^([0-9]+)(?:,(\+?[0-9]+))?p(?:;([0-9]+)q)?$`)
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

// countOption reads the line count of head or tail: -n N, -nN, -N, --lines=N
// or --lines N. The value keeps its sign so tail's +N shows.
func countOption(args []string) (value string, files []string, ok bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return value, append(files, args[i+1:]...), true
		case arg == "-n" || arg == "--lines":
			if i+1 >= len(args) {
				return "", nil, false
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
			return "", nil, false
		default:
			files = append(files, arg)
		}
	}
	return value, files, true
}

// headLines is the first N lines head prints.
func headLines(args []string) (n int, files []string, ok bool) {
	value, files, ok := countOption(args)
	if !ok {
		return 0, nil, false
	}
	if value == "" {
		return 10, files, true
	}
	if !digits.MatchString(value) || atoi(value) < 1 {
		return 0, nil, false
	}
	return atoi(value), files, true
}

// tailLines is tail's count: plus for -n +N (from line N), else the last N lines.
func tailLines(args []string) (plus bool, n int, files []string, ok bool) {
	value, files, ok := countOption(args)
	if !ok {
		return false, 0, nil, false
	}
	if value == "" {
		return false, 10, files, true
	}
	number := strings.TrimPrefix(value, "+")
	if !digits.MatchString(number) || atoi(number) < 1 {
		return false, 0, nil, false
	}
	return number != value, atoi(number), files, true
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
	switch name {
	case "sed", "gsed":
		a := parseSed(args)
		offset, limit, ok = sedSelection(a)
		return offset, limit, a.files, ok
	case "awk", "gawk", "mawk":
		a := parseAwk(args)
		offset, limit, ok = awkSelection(a)
		return offset, limit, a.files, ok
	case "head":
		n, files, ok := headLines(args)
		return 1, n, files, ok
	case "tail":
		plus, n, files, ok := tailLines(args)
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
		n, files, ok := headLines(aa)
		plus, k, more, okTail := tailLines(ba)
		if ok && okTail && !plus && len(files) == 1 && plainFile(files[0]) && len(more) == 0 && k <= n {
			return readRange{files[0], n - k + 1, k}, true
		}
	case an == "tail" && bn == "head":
		plus, from, files, ok := tailLines(aa)
		n, more, okHead := headLines(ba)
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

func readCall(h harness, file string, offset, limit int) string {
	call := fmt.Sprintf("Read file_path=%s offset=%d", file, offset)
	if h == forPi {
		call = fmt.Sprintf("read path=%s offset=%d", file, offset)
	}
	if limit > 0 {
		call += fmt.Sprintf(" limit=%d", limit)
	}
	return call
}

func readRangeReason(line string, h harness) string {
	head := Marker + " (deny-read-range): sed, awk, head and tail output is not a Read, so a later Edit of the file fails.\n"
	r, ok := readRangeHit(line)
	if !ok {
		return head + "Send " + piOr(h, "read", "Read") + " with offset and limit instead, as a parallel call in the same message as your other commands."
	}
	text := "Send " + readCall(h, r.file, r.offset, r.limit) + " instead, as a parallel call in the same message as your other commands."
	if h == forClaude && !strings.HasPrefix(r.file, "/") {
		text += " Read needs an absolute path."
	}
	return head + text
}
