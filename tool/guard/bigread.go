package guard

import (
	"fmt"
	"io"
	"math"
	"os"
	"os/user"
	"strings"
)

// bigReadBytes: a whole-file Read of a regular file over this many bytes is
// blocked; a range (offset or limit) always passes.
const bigReadBytes = 30720

// Files Claude Code's Read tool opens specially, so a size limit does not apply.
var readExemptSuffixes = set(".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf", ".ipynb")

func bigReadReason(path string, size int64) string {
	kilobytes := math.Max(1, math.RoundToEven(float64(size)/1024))
	return fmt.Sprintf("%s (deny-big-read): %s is %d KB; reading it whole floods the context.\n", Marker, path, int(kilobytes)) +
		"Use Read with offset and limit and take only the section you need.\n" +
		`Get the line numbers first: rg -n PATTERN file (a doc: rg -n "^#{1,3} " file).`
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
	info, err := os.Stat(expandUser(path))
	if err != nil || !info.Mode().IsRegular() || info.Size() <= bigReadBytes {
		return 0
	}
	io.WriteString(stderr, bigReadReason(path, info.Size())+"\n")
	return 2
}
