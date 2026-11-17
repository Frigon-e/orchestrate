package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The decision tables are hooks/claude/toolguard_cases.json, shared with the pi
// guard; these tests assert the Claude side of them, as toolguard_test.py did.

// replacement is what a block must name, by rule: sed-inplace and read-range are left.
var replacement = map[string]map[string]string{
	"fd-rg": {"sed-inplace": "built-in Edit", "read-range": "Read file_path="},
	"rg":    {"sed-inplace": "built-in Edit", "read-range": "Read file_path="},
	"fd":    {"sed-inplace": "built-in Edit", "read-range": "Read file_path="},
	"none":  {"sed-inplace": "built-in Edit", "read-range": "Read file_path="},
}

func nonBlank(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

func TestBashCasesBlockAndAllow(t *testing.T) {
	f := loadToolguard(t)
	paths := situationPaths(t, f)
	r := inProc{}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			payload := jsonBytes(t, fromOrigin(bashEvent(c.Command), c.Origin))
			if c.Rule == "" {
				if code, stderr := r.pre(t, payload, paths[c.Situation], nil); code != 0 || strings.TrimSpace(stderr) != "" {
					t.Fatalf("exit %d, want 0; stderr %q", code, stderr)
				}
				return
			}
			// No retry escape: the same command is blocked every time.
			for _, attempt := range []string{"first", "retry"} {
				code, stderr := r.pre(t, payload, paths[c.Situation], nil)
				reason := strings.TrimSpace(stderr)
				if code != 2 {
					t.Fatalf("%s: exit %d, want 2: %s", attempt, code, reason)
				}
				if !strings.Contains(reason, "(deny-"+c.Rule+")") {
					t.Errorf("%s: reason does not name deny-%s: %s", attempt, c.Rule, reason)
				}
				if !strings.Contains(reason, Marker) {
					t.Errorf("%s: reason does not name the guard: %s", attempt, reason)
				}
				if n := nonBlank(reason); n < 1 || n > 3 {
					t.Errorf("%s: reason is %d lines, want 1 to 3: %s", attempt, n, reason)
				}
				if strings.Contains(reason, "orch ctx") {
					t.Errorf("%s: reason names orch ctx: %s", attempt, reason)
				}
				if want := replacement[c.Situation][c.Rule]; !strings.Contains(reason, want) {
					t.Errorf("%s: reason must name %q: %s", attempt, want, reason)
				}
				for _, need := range c.ReasonHas["claude"] {
					if !strings.Contains(reason, need) {
						t.Errorf("%s: reason must carry %q: %s", attempt, need, reason)
					}
				}
				for _, bad := range c.ReasonLacks["claude"] {
					if strings.Contains(reason, bad) {
						t.Errorf("%s: reason must not carry %q: %s", attempt, bad, reason)
					}
				}
			}
		})
	}
}

// Every example the task requires, with the decision it must get. Presence is
// checked here; the case file is what the tests above execute.
func TestRequiredCasesArePresent(t *testing.T) {
	required := []struct{ command, situation, rule string }{
		{`find . -name '*.log' -exec rm {} \;`, "fd-rg", ""},
		{"find . -name '*.ts' | grep -v node_modules", "fd-rg", ""},
		{"find . -type f | xargs grep -l TODO", "fd-rg", ""},
		{"sed -i 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"},
		{"sed -i.bak 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"},
		{"sed -i '' 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"},
		{"gsed -i 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"},
		{"sed --in-place 's/foo/bar/' file.txt", "fd-rg", "sed-inplace"},
		{"cat notes.txt", "fd-rg", ""},
		{"head -n 20 notes.txt", "fd-rg", "read-range"},
		{"tail -c 100 notes.txt", "fd-rg", ""},
		{"sudo cat /etc/hosts", "fd-rg", ""},
		{"sed 's/foo/bar/' file.txt", "fd-rg", ""},
		{"git diff | sed 's/^/  /'", "fd-rg", ""},
		{"tail -f /var/log/app.log", "fd-rg", ""},
		{"cat file1 file2", "fd-rg", ""},
		{"cat notes.txt | grep TODO", "fd-rg", ""},
		{"head -n 5 file | wc -l", "fd-rg", ""},
		{"cat notes.txt > copy.txt", "fd-rg", ""},
		{`grep -rn "findUser" src`, "fd-rg", ""},
		{"ls my-find-exec-project/", "fd-rg", ""},
		{"grep -rn x src", "fd-rg", ""},
		{"rg Foo internal/", "fd-rg", ""},
		{"go test ./... | grep FAIL", "fd-rg", ""},
		{"rg -n foo tool", "fd-rg", ""},
		{"grep -rn foo .", "fd-rg", ""},
		{"grep -rn foo .", "none", ""},
	}
	have := map[[2]string]string{}
	for _, c := range loadToolguard(t).Cases {
		have[[2]string{c.Command, c.Situation}] = c.Rule
	}
	for _, r := range required {
		rule, ok := have[[2]string{r.command, r.situation}]
		if !ok {
			t.Errorf("required case %q (%s) is missing", r.command, r.situation)
		} else if rule != r.rule {
			t.Errorf("required case %q (%s) has rule %q, want %q", r.command, r.situation, rule, r.rule)
		}
	}
}

// ORCH_TOOL_GUARD=off and ORCH_TOOL_GUARD_SKIP are the only escapes.
func TestSwitchesDisableRules(t *testing.T) {
	paths := situationPaths(t, loadToolguard(t))
	payload := jsonBytes(t, bashEvent("sed -i 's/a/b/' file.txt"))
	if code, _ := (inProc{}).pre(t, payload, paths["fd-rg"], nil); code != 2 {
		t.Fatal("sed -i is not blocked without a switch")
	}
	for name, env := range map[string]map[string]string{
		"ORCH_TOOL_GUARD=off": {"ORCH_TOOL_GUARD": "off"},
		"skip by short name":  {"ORCH_TOOL_GUARD_SKIP": "sed-inplace"},
		"skip by full name":   {"ORCH_TOOL_GUARD_SKIP": "deny-sed-inplace"},
	} {
		if code, stderr := (inProc{}).pre(t, payload, paths["fd-rg"], env); code != 0 {
			t.Errorf("%s did not disable deny-sed-inplace: %s", name, stderr)
		}
	}
	// Each rule has its own switch: skipping one leaves the other blocking.
	rangeRead := jsonBytes(t, bashEvent("sed -n '10,20p' notes.txt"))
	if code, _ := (inProc{}).pre(t, rangeRead, paths["fd-rg"], nil); code != 2 {
		t.Fatal("sed -n '10,20p' is not blocked without a switch")
	}
	for _, skip := range []string{"read-range", "deny-read-range"} {
		if code, stderr := (inProc{}).pre(t, rangeRead, paths["fd-rg"], map[string]string{"ORCH_TOOL_GUARD_SKIP": skip}); code != 0 {
			t.Errorf("skip %s did not disable deny-read-range: %s", skip, stderr)
		}
	}
	if code, _ := (inProc{}).pre(t, rangeRead, paths["fd-rg"], map[string]string{"ORCH_TOOL_GUARD_SKIP": "sed-inplace"}); code != 2 {
		t.Error("skipping sed-inplace let a read-range through")
	}
}

// A non-Bash event, a missing command and broken JSON must all allow.
func TestPreAllowsEventsItCannotRead(t *testing.T) {
	paths := situationPaths(t, loadToolguard(t))
	payloads := map[string]string{
		"non-Bash tool":   `{"tool_name":"Grep","tool_input":{"command":"grep -rn x src"}}`,
		"missing command": `{"tool_name":"Bash","tool_input":{}}`,
		"empty command":   `{"tool_name":"Bash","tool_input":{"command":"  "}}`,
		"broken JSON":     `not json`,
		"a JSON list":     `[]`,
		"a JSON null":     `null`,
		"bad UTF-8":       "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"grep -rn x src \xff\"}}",
		"command number":  `{"tool_name":"Bash","tool_input":{"command":5}}`,
		"tool_input text": `{"tool_name":"Bash","tool_input":"grep -rn x src"}`,
	}
	for name, p := range payloads {
		if code, stderr := (inProc{}).pre(t, []byte(p), paths["fd-rg"], nil); code != 0 || stderr != "" {
			t.Errorf("%s: did not allow cleanly: %d %q", name, code, stderr)
		}
	}
	// A Read event carrying a command is a Read, not a Bash call.
	if code, _ := (inProc{}).pre(t, []byte(`{"tool_name":"Read","tool_input":{"command":"grep -rn x src"}}`), paths["fd-rg"], nil); code != 0 {
		t.Error("a Read event ran the Bash rules")
	}
	// No tool name at all is still checked as Bash, as the Bash hooks always did.
	if code, _ := (inProc{}).pre(t, []byte(`{"tool_input":{"command":"sed -i s/a/b/ x.go"}}`), paths["fd-rg"], nil); code != 2 {
		t.Error("an event with no tool name was not checked as Bash")
	}
}

// The lookup blocks are gone: find, grep and a whole-file cat all pass, in every PATH situation.
func TestRemovedLookupBlocksAllow(t *testing.T) {
	paths := situationPaths(t, loadToolguard(t))
	for _, command := range []string{
		"grep -rn Normalize src", "git grep -n Normalize", "egrep -n Gateway internal", "ag Gateway internal",
		"find . -name '*.ts'", `find . -name x -exec ls {} \;`, "find . -type f | xargs grep -l TODO", "fd -e go | grep Gateway",
		"cat notes.txt",
	} {
		for situation, dir := range paths {
			if code, stderr := (inProc{}).pre(t, jsonBytes(t, bashEvent(command)), dir, nil); code != 0 || stderr != "" {
				t.Errorf("%s (%s) was blocked: %q", command, situation, stderr)
			}
		}
	}
}

const bigReadLookup = `Get the line numbers first: rg -n PATTERN file (a doc: rg -n "^#{1,3} " file).`

func TestBigReadCases(t *testing.T) {
	f := loadToolguard(t)
	paths := situationPaths(t, f)
	files := filepath.Join(t.TempDir(), "big-read-files")
	if err := os.MkdirAll(files, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.ReadCases {
		t.Run(c.Name, func(t *testing.T) {
			path := filepath.Join(files, c.File)
			switch kind := c.Kind; {
			case kind == "dir":
				os.MkdirAll(path, 0o755)
			case kind == "" || kind == "file":
				if _, err := os.Stat(path); err != nil {
					if err := os.WriteFile(path, fileContent(c.Bytes, c.Lines), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			payload := jsonBytes(t, fromOrigin(readEvent(path, c.Input), c.Origin))
			if !c.Block {
				if code, stderr := (inProc{}).pre(t, payload, paths[c.Situation], nil); code != 0 || strings.TrimSpace(stderr) != "" {
					t.Fatalf("exit %d, want 0: %s", code, stderr)
				}
				return
			}
			for _, attempt := range []string{"first", "retry"} {
				code, stderr := inProc{}.pre(t, payload, paths[c.Situation], nil)
				reason := strings.TrimSpace(stderr)
				if code != 2 {
					t.Fatalf("%s: exit %d, want 2: %s", attempt, code, reason)
				}
				info, _ := os.Stat(path)
				if !strings.Contains(reason, "(deny-big-read)") || !strings.Contains(reason, Marker) {
					t.Errorf("%s: reason does not name the rule and the guard: %s", attempt, reason)
				}
				if n := nonBlank(reason); n < 2 || n > 5 {
					t.Errorf("%s: reason is %d lines, want 2 to 5: %s", attempt, n, reason)
				}
				if want := fmt.Sprintf("%.0f KB", float64(info.Size())/1024); !strings.Contains(reason, want) {
					t.Errorf("%s: reason does not name the size %s: %s", attempt, want, reason)
				}
				if !strings.Contains(reason, "offset and limit") {
					t.Errorf("%s: reason does not name offset and limit: %s", attempt, reason)
				}
				if !strings.Contains(reason, bigReadLookup) {
					t.Errorf("%s: reason must carry the rg -n lookup: %s", attempt, reason)
				}
				if strings.Contains(reason, "orch ctx") {
					t.Errorf("%s: reason names orch ctx: %s", attempt, reason)
				}
			}
		})
	}
}

func TestBigReadSwitchesAndRetry(t *testing.T) {
	paths := situationPaths(t, loadToolguard(t))
	big := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 31744)), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(extra map[string]any, env map[string]string) (int, string) {
		return inProc{}.pre(t, jsonBytes(t, readEvent(big, extra)), paths["fd-rg"], env)
	}
	// A retry with a limit passes where the bare read was blocked.
	if code, _ := run(nil, nil); code != 2 {
		t.Error("the bare read was not blocked")
	}
	// With no session id there is nowhere to remember the warning: every bare read blocks, with no "send again" line.
	if code, stderr := run(nil, nil); code != 2 || strings.Contains(stderr, "again") {
		t.Errorf("a repeat with no session id: exit %d, %s", code, stderr)
	}
	if code, _ := run(map[string]any{"limit": 100}, nil); code != 0 {
		t.Error("the retry with limit was not allowed")
	}
	for label, env := range map[string]map[string]string{
		"ORCH_TOOL_GUARD=off":     {"ORCH_TOOL_GUARD": "off"},
		"the big-read skip":       {"ORCH_TOOL_GUARD_SKIP": "big-read"},
		"the deny-big-read skip":  {"ORCH_TOOL_GUARD_SKIP": "deny-big-read"},
		"a skip list with spaces": {"ORCH_TOOL_GUARD_SKIP": "grep-rg, big-read"},
	} {
		if code, stderr := run(nil, env); code != 0 || stderr != "" {
			t.Errorf("%s did not disable the rule: %d %s", label, code, stderr)
		}
	}
	if code, _ := run(nil, map[string]string{"ORCH_TOOL_GUARD_SKIP": "grep-rg,find"}); code != 2 {
		t.Error("another rule's skip also disabled big-read")
	}
}

// The second whole Read of a big file in a session passes and keeps passing; another
// session or another path still blocks.
func TestBigReadRepeatPasses(t *testing.T) {
	paths := situationPaths(t, loadToolguard(t))
	dir := t.TempDir()
	env := map[string]string{"ORCH_TOOL_GUARD_STATE_DIR": filepath.Join(dir, "state")}
	var files [2]string
	for i := range files {
		files[i] = filepath.Join(dir, fmt.Sprintf("big%d.txt", i))
		if err := os.WriteFile(files[i], []byte(strings.Repeat("x", 31744)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path, session string) (int, string) {
		ev := readEvent(path, nil)
		ev["session_id"] = session
		return inProc{}.pre(t, jsonBytes(t, ev), paths["fd-rg"], env)
	}
	code, stderr := read(files[0], "s1")
	if code != 2 || !strings.Contains(stderr, "send this same Read again and it passes") {
		t.Fatalf("first read: exit %d, %s", code, stderr)
	}
	for _, attempt := range []string{"second", "third"} {
		if code, stderr := read(files[0], "s1"); code != 0 || stderr != "" {
			t.Errorf("%s read of the same path and session: exit %d, %s", attempt, code, stderr)
		}
	}
	if code, _ := read(files[0], "s2"); code != 2 {
		t.Error("another session was let through")
	}
	if code, _ := read(files[1], "s1"); code != 2 {
		t.Error("another path was let through")
	}
	if code, _ := read(files[1], "s1"); code != 0 {
		t.Error("the other path did not pass on its own repeat")
	}
}

// The block lists top-level declarations of code and headings of docs, with their line numbers.
func TestBigReadOutline(t *testing.T) {
	paths := situationPaths(t, loadToolguard(t))
	dir := t.TempDir()
	block := func(name string, lines []string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, stderr := inProc{}.pre(t, jsonBytes(t, readEvent(path, nil)), paths["fd-rg"], nil)
		if code != 2 {
			t.Fatalf("%s: exit %d, want 2", name, code)
		}
		return stderr
	}
	filler := func(n int, first ...string) []string {
		lines := append([]string{}, first...)
		for len(lines) < n {
			lines = append(lines, "// filler line to make the file long enough")
		}
		return lines
	}
	goLines := filler(2100, "package x", "", "import \"os\"", "", "type Cell struct {", "\tfunc inner() {}", "}", "func (c Cell) Run() {}   ", "var long = \""+strings.Repeat("y", 150)+"\"")
	got := block("a.go", goLines)
	for _, want := range []string{"is 2100 lines (", "(line: text):", "\n  5: type Cell struct {\n", "\n  8: func (c Cell) Run() {}\n", "  9: var long = \"" + strings.Repeat("y", 100-len("var long = \"")) + "\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("go outline lacks %q: %s", want, got)
		}
	}
	for _, bad := range []string{"package", "import", "inner", "more;"} {
		if strings.Contains(got, bad) {
			t.Errorf("go outline has %q: %s", bad, got)
		}
	}
	many := []string{}
	for i := 0; i < 60; i++ {
		many = append(many, fmt.Sprintf("func f%d() {}", i))
	}
	got = block("many.go", filler(2100, many...))
	if !strings.Contains(got, "  50: func f49() {}\n") || strings.Contains(got, "f50()") || !strings.Contains(got, "  ... 10 more; rg -n ") {
		t.Errorf("a 60-declaration outline is not cut at 50 with a rest line: %s", got)
	}
	md := filler(1100, "# Title", "text", "```sh", "# not a heading", "```", "## Part", "#### too deep")
	got = block("a.md", md)
	if !strings.Contains(got, "headings (line: text):\n  1: # Title\n  6: ## Part\n") || strings.Contains(got, "not a heading") || strings.Contains(got, "too deep") {
		t.Errorf("markdown outline: %s", got)
	}
	got = block("app.log", filler(1100))
	if strings.Contains(got, "(line: text)") || !strings.Contains(got, bigReadLookup) {
		t.Errorf("a log must carry the rg hint and no outline: %s", got)
	}
}

// Fail open on anything but a Read of a path.
func TestBigReadFailsOpen(t *testing.T) {
	paths := situationPaths(t, loadToolguard(t))
	big := filepath.Join(t.TempDir(), "big.txt")
	os.WriteFile(big, []byte(strings.Repeat("x", 31744)), 0o644)
	payloads := map[string]any{
		"broken JSON":           "not json",
		"a JSON list":           "[]",
		"another tool":          map[string]any{"tool_name": "Grep", "tool_input": map[string]any{"file_path": big}},
		"no tool name":          map[string]any{"tool_input": map[string]any{"file_path": big}},
		"no tool input":         map[string]any{"tool_name": "Read"},
		"a broken tool_input":   map[string]any{"tool_name": "Read", "tool_input": big},
		"no file_path":          map[string]any{"tool_name": "Read", "tool_input": map[string]any{}},
		"an empty file_path":    map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": " "}},
		"a number as file_path": map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": 5}},
		"a NUL in file_path":    map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": big + "\x00"}},
		"a zero offset":         map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": big, "offset": 0}},
		"a pages selection":     map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": big, "pages": "1-3"}},
	}
	for name, p := range payloads {
		raw, ok := p.(string)
		if !ok {
			raw = string(jsonBytes(t, p))
		}
		if code, stderr := (inProc{}).pre(t, []byte(raw), paths["fd-rg"], nil); code != 0 || stderr != "" {
			t.Errorf("%s: did not allow cleanly: %d %q", name, code, stderr)
		}
	}
	// A ~ path is expanded before the size check.
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "big.txt"), []byte(strings.Repeat("x", 31744)), 0o644)
	t.Setenv("HOME", home)
	if code, _ := (inProc{}).pre(t, jsonBytes(t, readEvent("~/big.txt", nil)), paths["fd-rg"], nil); code != 2 {
		t.Error("a ~ path to a big file was not blocked")
	}
}
