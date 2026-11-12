package guard

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// bigReadBytes: a whole-file Read of a regular file over this many bytes is
// blocked; a range (offset or limit) always passes.
const bigReadBytes = 30720

// Files Claude Code's Read tool opens specially, so a size limit does not apply.
var readExemptSuffixes = set(".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf", ".ipynb")

// A code file passes up to codeReadLines lines and codeReadBytes bytes; the byte
// cap catches generated and minified files.
const (
	codeReadLines = 2000
	codeReadBytes = 102400
	outlineMax    = 50
)

var codeExts = set(".go", ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rs", ".java", ".kt", ".kts",
	".swift", ".rb", ".php", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".scala", ".sh", ".bash", ".zsh", ".lua", ".vue",
	".svelte", ".sql", ".proto", ".ex", ".exs", ".dart", ".zig")

var docExts = set(".md", ".mdx", ".markdown")

const (
	codeOutlinePattern = `^(export\s+)?(default\s+)?(pub(\([a-z]+\))?\s+)?(async\s+)?(func|type|class|def|fn|impl|struct|interface|enum|trait|function|const|var|let|mod|module|object)\b`
	docOutlinePattern  = `^#{1,3} `
)

var (
	codeOutlineRe = regexp.MustCompile(codeOutlinePattern)
	docOutlineRe  = regexp.MustCompile(docOutlinePattern)
)

// bigFile is a file over its limit: its size, its line count and outline when
// they were computed (code and docs only), and what the outline lists.
type bigFile struct {
	size    int64
	lines   int
	outline string
	what    string
}

// inspectBigRead reports whether target is a regular file over its limit. A
// file it cannot read passes.
func inspectBigRead(target string) (bigFile, bool) {
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= bigReadBytes {
		return bigFile{}, false
	}
	f := bigFile{size: info.Size()}
	ext := strings.ToLower(splitExt(target))
	code, doc := codeExts[ext], docExts[ext]
	if !code && !doc {
		return f, true
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return bigFile{}, false
	}
	f.lines = bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		f.lines++
	}
	if code && f.size <= codeReadBytes && f.lines <= codeReadLines {
		return bigFile{}, false
	}
	re, pattern, fenced := codeOutlineRe, codeOutlinePattern, false
	f.what = "outline"
	if doc {
		re, pattern, fenced = docOutlineRe, docOutlinePattern, true
		f.what = "headings"
	}
	var entries []string
	inFence := false
	for i, line := range strings.Split(string(data), "\n") {
		if fenced && strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			continue
		}
		if inFence || !re.MatchString(line) {
			continue
		}
		entries = append(entries, fmt.Sprintf("  %d: %s", i+1, cutRunes(strings.TrimRight(line, " \t\r"), 100)))
	}
	if len(entries) > outlineMax {
		more := len(entries) - outlineMax
		entries = append(entries[:outlineMax], fmt.Sprintf("  ... %d more; rg -n %q file for the rest", more, pattern))
	}
	f.outline = strings.Join(entries, "\n")
	return f, true
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// bigReadReason is the block text; repeat adds the line saying a second Read passes.
func bigReadReason(path string, f bigFile, repeat bool, h harness) string {
	kilobytes := int(math.Max(1, math.RoundToEven(float64(f.size)/1024)))
	head := fmt.Sprintf("%s (deny-big-read): %s is ", Marker, path)
	if f.lines > 0 {
		head += fmt.Sprintf("%d lines (%d KB)", f.lines, kilobytes)
	} else {
		head += fmt.Sprintf("%d KB", kilobytes)
	}
	out := head + "; reading it whole floods the context.\n"
	if f.outline != "" {
		out += "Read only the section you need with " + h.read + " offset and limit. Its " + f.what + " (line: text):\n" + f.outline
	} else {
		out += "Use " + h.read + " with offset and limit and take only the section you need.\n" +
			`Get the line numbers first: rg -n PATTERN file (a doc: rg -n "^#{1,3} " file).`
	}
	if repeat {
		out += "\nIf you need the whole file, send this same " + h.read + " again and it passes."
	}
	return out
}

// bigReadSkipped: ORCH_TOOL_GUARD=off disables the rule; ORCH_TOOL_GUARD_SKIP
// takes its short or full name.
func bigReadSkipped() bool {
	if os.Getenv("ORCH_TOOL_GUARD") == "off" {
		return true
	}
	skip := skipped()
	return skip["big-read"] || skip["deny-big-read"]
}

// splitExt is Python's os.path.splitext suffix: a leading dot of the file name is not an extension.
func splitExt(path string) string {
	sep := strings.LastIndex(path, "/")
	dot := strings.LastIndex(path, ".")
	if dot > sep {
		for i := sep + 1; i < dot; i++ {
			if path[i] != '.' {
				return path[dot:]
			}
		}
	}
	return ""
}

// expandUser is Python's os.path.expanduser: ~ and ~/x use HOME, ~name that user's home.
func expandUser(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	i := strings.Index(path, "/")
	if i < 0 {
		i = len(path)
	}
	home := ""
	if i == 1 {
		if home = os.Getenv("HOME"); home == "" {
			u, err := user.Current()
			if err != nil {
				return path
			}
			home = u.HomeDir
		}
	} else {
		u, err := user.Lookup(path[1:i])
		if err != nil {
			return path
		}
		home = u.HomeDir
	}
	if out := strings.TrimRight(home, "/") + path[i:]; out != "" {
		return out
	}
	return "/"
}

// bigRead blocks a Read of a big file with no range, so the whole file lands in
// context. It fails open: a payload that is not a Read of a path, a range of any
// kind, a PDF page selection, a file Read opens specially, and a path that is
// not a regular file over the limit (missing, a directory, unreadable) all pass.
func bigRead(ev *Event, stderr io.Writer) int {
	if bigReadSkipped() {
		return 0
	}
	input, ok := ev.Object("tool_input")
	if !ok {
		return 0
	}
	path, ok := input["file_path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return 0
	}
	for _, key := range []string{"offset", "limit", "pages"} {
		if v, present := input[key]; present && v != nil && v != "" {
			return 0
		}
	}
	if readExemptSuffixes[strings.ToLower(splitExt(path))] {
		return 0
	}
	target := expandUser(path)
	f, over := inspectBigRead(target)
	if !over {
		return 0
	}
	v, _ := ev.Value("session_id")
	session := ""
	if truthy(v) {
		session = pyStr(v)
	}
	if session != "" {
		if abs, err := filepath.Abs(target); err == nil {
			state := openState(session)
			defer state.close()
			if state.bigReadWarned(abs) {
				return 0
			}
		} else {
			session = ""
		}
	}
	io.WriteString(stderr, bigReadReason(path, f, session != "", forClaude)+"\n")
	return 2
}

// bigReadWarned reports whether path was already blocked once this session
// (so a repeat passes, and keeps passing); otherwise it records the warning.
// The list keeps the newest bigReadsKept paths.
func (s *State) bigReadWarned(path string) bool {
	if slices.Contains(s.BigReadsAllowed, path) {
		return true
	}
	s.BigReadsAllowed = append(s.BigReadsAllowed, path)
	if len(s.BigReadsAllowed) > bigReadsKept {
		s.BigReadsAllowed = s.BigReadsAllowed[len(s.BigReadsAllowed)-bigReadsKept:]
	}
	return false
}

const bigReadsKept = 50
