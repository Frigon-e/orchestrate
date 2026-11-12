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
	"sync"
)

// bigReadBytes: a whole-file Read of a regular file over this many bytes is
// blocked; a range (offset or limit) always passes.
const bigReadBytes = 30720

// Files Claude Code's Read tool opens specially, so a size limit does not apply.
var readExemptSuffixes = set(".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf", ".ipynb")

// A code file passes up to codeReadLines lines and codeReadBytes bytes; the byte
// cap catches generated and minified files. A session keeps the newest
// bigReadsKept warned paths, and only the first inspectMax bytes of a file are
// counted and outlined (a bigger file blocks either way).
const (
	codeReadLines = 2000
	codeReadBytes = 102400
	outlineMax    = 50
	inspectMax    = 1 << 20
	bigReadsKept  = 50
)

// An outliner lists the lines that start a section of one kind of file. Its
// regexp compiles on first use, so the hook pays for it only on a block.
type outliner struct {
	what, pattern string
	fenced        bool // skip lines inside ``` blocks
	re            func() *regexp.Regexp
}

func newOutliner(what, pattern string, fenced bool) *outliner {
	return &outliner{what, pattern, fenced, sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(pattern) })}
}

var (
	codeOutline = newOutliner("outline", `^(export\s+)?(default\s+)?(pub(\([a-z]+\))?\s+)?(async\s+)?(func|type|class|def|fn|impl|struct|interface|enum|trait|function|const|var|let|mod|module|object)\b`, false)
	docOutline  = newOutliner("headings", `^#{1,3} `, true)
	// outlines maps an extension to its outliner; a code file also gets the code limit.
	outlines = map[string]*outliner{}
)

func init() {
	for _, ext := range []string{".go", ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rs", ".java", ".kt", ".kts",
		".swift", ".rb", ".php", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".scala", ".sh", ".bash", ".zsh", ".lua", ".vue",
		".svelte", ".sql", ".proto", ".ex", ".exs", ".dart", ".zig"} {
		outlines[ext] = codeOutline
	}
	for _, ext := range []string{".md", ".mdx", ".markdown"} {
		outlines[ext] = docOutline
	}
}

// bigFile is a file over its limit: its size, and its line count and outline
// when it has an outliner.
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
	o := outlines[strings.ToLower(splitExt(target))]
	if o == nil {
		return f, true
	}
	file, err := os.Open(target)
	if err != nil {
		return bigFile{}, false
	}
	data, err := io.ReadAll(io.LimitReader(file, inspectMax))
	file.Close()
	if err != nil {
		return bigFile{}, false
	}
	f.lines = bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		f.lines++
	}
	if o == codeOutline && f.size <= codeReadBytes && f.lines <= codeReadLines {
		return bigFile{}, false
	}
	f.what = o.what
	re := o.re()
	var entries []string
	more, inFence, n := 0, false, 0
	for line := range bytes.Lines(data) {
		n++
		if o.fenced && bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte("```")) {
			inFence = !inFence
			continue
		}
		if inFence || !re.Match(line) {
			continue
		}
		if len(entries) == outlineMax {
			more++
			continue
		}
		entries = append(entries, fmt.Sprintf("  %d: %s", n, cutRunes(string(bytes.TrimRight(line, " \t\r\n")), 100)))
	}
	if more > 0 {
		entries = append(entries, fmt.Sprintf("  ... %d more; rg -n %q file for the rest", more, o.pattern))
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
		unit := "lines"
		if f.lines == 1 {
			unit = "line"
		}
		head += fmt.Sprintf("%d %s (%d KB)", f.lines, unit, kilobytes)
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
	state := lockedState{State: &State{}}
	if v, _ := ev.Value("session_id"); truthy(v) {
		state = openState(pyStr(v))
		defer state.close()
	}
	reason := bigReadVerdict(state, path, target, f, forClaude)
	if reason == "" {
		return 0
	}
	io.WriteString(stderr, reason+"\n")
	return 2
}

// bigReadVerdict is the block text for a whole read of a file over its limit,
// or "" when this session was already warned about the file: a repeat passes,
// and keeps passing. Only a saved state can let a repeat through, so only then
// does the text offer it.
func bigReadVerdict(state lockedState, path, target string, f bigFile, h harness) string {
	abs, err := filepath.Abs(target)
	if err != nil || state.file == nil {
		return bigReadReason(path, f, false, h)
	}
	if slices.Contains(state.BigReadsWarned, abs) {
		return ""
	}
	state.BigReadsWarned = append(state.BigReadsWarned, abs)
	if len(state.BigReadsWarned) > bigReadsKept {
		state.BigReadsWarned = state.BigReadsWarned[len(state.BigReadsWarned)-bigReadsKept:]
	}
	return bigReadReason(path, f, true, h)
}
