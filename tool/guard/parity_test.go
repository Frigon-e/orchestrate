package guard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Parity: every case of the shared tables, and more, runs through both
// hooks/claude/toolguard.py and orch-guard, and the two must agree on the exit
// code, stderr, stdout, the log rows (but for the timestamp) and the state file.
// Phase 2 deletes toolguard.py; this is what shows the port loses nothing.

// pythonRunner skips under -short: the comparison spawns Python a few thousand times (about a minute).
func pythonRunner(t *testing.T) python {
	if testing.Short() {
		t.Skip("parity with toolguard.py is skipped under -short")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	return python{filepath.Join(repoRoot(t), "hooks", "claude", "toolguard.py")}
}

// extraCommands are commands the case table does not hold, to widen the comparison.
var extraCommands = []string{
	"find . -name x", "FOO=1 find . -delete", "sudo -u root find / -exec ls {} +", "echo find . -exec x",
	"'find' . -delete", "fd -e go | xargs grep foo", "rg foo | grep bar", "git grep foo", "git grep foo | head",
	"grep -r foo", "grep -r foo | head", "grep -e foo -f pat.txt", "egrep -rn x .", "ag foo src", "grep -A 3 foo file",
	"echo x | grep foo file", "sed -n '5,10p' file.go", "sed -n 5p /abs/file", "sed -n -e '2,3p' a.txt",
	"sed -e 's/a/b/' -n file", "sed -n '10,5p' x", "sed -n '0,0p' x", "sed -ne 3p f", "sed p f", "cat -n file",
	"cat - file", "cat a b", "cat", "cat ~/x", "sudo -E cat /etc/hosts", "perl -pi -e 's/a/b/' f", "perl -e 1",
	"sed -i.bak s/a/b/ f", "gsed --in-place x f", "cd src && cat foo.go", "ls; cat a.txt; rg x", "cat a.txt &",
	`echo "find . -exec"`, `x="$(find . -delete)"`, "(find . -delete)", "find . | grep x", "find . | xargs rg y",
	"find . -type f | sort | grep x", "cat a >b", "tee <a", "env find . -exec x", "xargs find", `\find . -exec x ;`,
	"grep foo a.txt b.txt && cat c.txt", "cat a.txt; grep -rn x .", "sed -n 3p a.txt | cat", "sudo -u root -E grep -r x .",
	"find . -newer x -exec cat {} +", "rg -n foo | head", "FOO=bar cat notes.md", "  cat notes.md  ", "\tcat notes.md",
	"cat 'a b.txt'", `cat "a b.txt"`, "sed -n '1,20p' \"my file.go\"", "sed -n 1,3p rel/path.go", "sed --expression='2p' -n f",
	"cat é.txt", "grep -rn é .", "git -C x grep foo", "git log | grep fix",
}

func TestParityPreOnEveryCaseAndMoreCommands(t *testing.T) {
	py, goBin := pythonRunner(t), goBinary{}
	f := loadToolguard(t)
	paths := situationPaths(t, f)
	type probe struct {
		name, situation string
		payload         []byte
		env             map[string]string
	}
	var probes []probe
	for _, c := range f.Cases {
		probes = append(probes, probe{c.Name, c.Situation, jsonBytes(t, bashEvent(c.Command)), nil})
	}
	for _, c := range extraCommands {
		for _, sit := range []string{"fd-rg", "none"} {
			probes = append(probes, probe{c + " [" + sit + "]", sit, jsonBytes(t, bashEvent(c)), nil})
		}
	}
	for _, env := range []map[string]string{
		{"ORCH_TOOL_GUARD": "off"}, {"ORCH_TOOL_GUARD_SKIP": "find-chains,grep-rg"}, {"ORCH_TOOL_GUARD_SKIP": " deny-find , cat-head-tail"},
	} {
		for _, c := range []string{"find . -name x -exec ls {} ;", "find . -name x", "grep -rn x src", "cat a.txt", "sed -i s/a/b/ f"} {
			probes = append(probes, probe{c + " " + strings.Join(mapValues(env), ","), "fd-rg", jsonBytes(t, bashEvent(c)), env})
		}
	}
	for _, raw := range []string{"not json", "[]", "null", `{"tool_name":"Bash"}`, `{"tool_name":"Bash","tool_input":{"command":7}}`,
		`{"tool_input":{"command":"grep -rn x ."}}`, `{"tool_name":5,"tool_input":{"command":"grep -rn x ."}}`,
		`{"tool_name":"Read","tool_input":{"command":"grep -rn x ."}}`, "{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"cat \xff\"}}"} {
		probes = append(probes, probe{"raw " + raw, "fd-rg", []byte(raw), nil})
	}
	files := t.TempDir()
	for name, size := range map[string]int{"big.txt": 31744, "edge.md": 30721, "exact.md": 30720, "small.go": 10, "big.PNG": 99999, ".png": 99999, "x.tar.gz": 99999, "noext": 99999, "weird.": 99999} {
		os.WriteFile(filepath.Join(files, name), []byte(strings.Repeat("x", size)), 0o644)
	}
	os.Mkdir(filepath.Join(files, "dir"), 0o755)
	for _, name := range []string{"big.txt", "edge.md", "exact.md", "small.go", "big.PNG", ".png", "x.tar.gz", "noext", "weird.", "dir", "missing", " "} {
		path := filepath.Join(files, name)
		for _, extra := range []map[string]any{nil, {"limit": 5}, {"offset": 0}, {"offset": ""}, {"pages": "1"}, {"offset": nil, "limit": nil}} {
			probes = append(probes, probe{"read " + name + " " + string(jsonBytes(t, extra)), "fd-rg", jsonBytes(t, readEvent(path, extra)), nil})
		}
	}
	probes = append(probes, probe{"read ~", "fd-rg", jsonBytes(t, readEvent("~/nope-"+filepath.Base(files), nil)), nil},
		probe{"read skip", "fd-rg", jsonBytes(t, readEvent(filepath.Join(files, "big.txt"), nil)), map[string]string{"ORCH_TOOL_GUARD_SKIP": "deny-big-read"}})
	for _, p := range probes {
		pc, pe := py.pre(t, p.payload, paths[p.situation], p.env)
		gc, ge := goBin.pre(t, p.payload, paths[p.situation], p.env)
		if pc != gc || pe != ge {
			t.Errorf("%s:\n python: exit %d %q\n go:     exit %d %q", p.name, pc, pe, gc, ge)
		}
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// alternating sends each coach call to the next runner in turn, so a session
// switches between the two implementations on every call and keeps its state.
type alternating struct {
	runners []runner
	n       int
}

func (a *alternating) name() string { return "alternating" }
func (a *alternating) pre(t *testing.T, payload []byte, pathDir string, env map[string]string) (int, string) {
	return a.runners[0].pre(t, payload, pathDir, env)
}
func (a *alternating) coach(t *testing.T, payload []byte, stateDir string, env map[string]string) (int, string) {
	a.n++
	return a.runners[a.n%len(a.runners)].coach(t, payload, stateDir, env)
}

// A scenario drives one sandbox and returns what each call printed.
type scenario struct {
	name string
	run  func(r runner, box *sandbox) []any
}

func single(s spec, o runOpts) func(r runner, box *sandbox) []any {
	return func(r runner, box *sandbox) []any {
		box.writeTranscript([][]spec{{s}})
		code, stdout := box.run(r, s, "tu_0_0", o)
		return []any{code, stdout}
	}
}

func coachScenarios(t *testing.T) []scenario {
	cases := loadCoach(t)
	var out []scenario
	for _, c := range cases.ClassifyRaw {
		if !isClaudeCase(c) {
			continue
		}
		name := c["name"].(string)
		if strings.HasPrefix(name, "symbol ") {
			prime := spec{"bash": `jbcontext search "x"`}
			out = append(out, scenario{"classify " + name, func(r runner, box *sandbox) []any {
				box.writeTranscript([][]spec{{prime}, {c}})
				var trace []any
				for i, s := range []spec{prime, c} {
					code, stdout := box.run(r, s, "tu_"+itoa(i)+"_0", runOpts{})
					trace = append(trace, code, stdout)
				}
				return trace
			}})
			continue
		}
		out = append(out, scenario{"classify " + name, single(c, runOpts{})})
	}
	for _, seq := range cases.Sequences {
		out = append(out, scenario{"sequence " + seq.Name, func(r runner, box *sandbox) []any {
			box.writeTranscript(seq.Turns)
			var trace []any
			for ti, calls := range seq.Turns {
				for ci, s := range calls {
					code, stdout := box.run(r, s, "tu_"+itoa(ti)+"_"+itoa(ci), runOpts{})
					trace = append(trace, code, stdout)
				}
			}
			return trace
		}})
	}
	for _, seq := range cases.Prompts {
		out = append(out, scenario{"prompts " + seq.Name, func(r runner, box *sandbox) []any {
			var turns [][]spec
			for _, raw := range seq.Steps {
				var calls []spec
				if json.Unmarshal(raw, &calls) == nil {
					turns = append(turns, calls)
				}
			}
			box.writeTranscript(turns)
			var trace []any
			turn := 0
			for _, raw := range seq.Steps {
				var calls []spec
				if json.Unmarshal(raw, &calls) != nil {
					code, stdout := box.prompt(r, nil)
					trace = append(trace, code, stdout)
					continue
				}
				for ci, s := range calls {
					code, stdout := box.run(r, s, "tu_"+itoa(turn)+"_"+itoa(ci), runOpts{})
					trace = append(trace, code, stdout)
				}
				turn++
			}
			return trace
		}})
	}
	exit := func(s string) *string { return &s }
	long := strings.Repeat("é", 150) + strings.Repeat("x", 100)
	for name, o := range map[string]runOpts{
		"failure exit 1":              {failure: exit("Exit code 1")},
		"failure exit 1 newline":      {failure: exit("Exit code 1\n")},
		"failure exit 1 with text":    {failure: exit("Exit code 1\nrg: bad regex")},
		"failure exit 2":              {failure: exit("Exit code 2\n")},
		"failure boom":                {failure: exit("boom")},
		"failure empty":               {failure: exit("")},
		"response string":             {response: "plain text é"},
		"response stdout only":        {response: map[string]any{"stdout": "abc"}},
		"response stderr only":        {response: map[string]any{"stderr": "ééé"}},
		"response null stdout":        {response: map[string]any{"stdout": nil, "stderr": "x"}},
		"response read shape":         {response: map[string]any{"type": "text", "file": map[string]any{"filePath": "/a/é.go", "numLines": 3, "ratio": 1.5, "big": 1e20, "tiny": 0.00001, "list": []any{1, 2.0, "x", nil, true, map[string]any{"k": "\U0001F600"}}}}},
		"response list":               {response: []any{"a", 1, 2.25}},
		"response number":             {response: 42},
		"response empty object":       {response: map[string]any{}},
		"duration int":                {extra: map[string]any{"duration_ms": 12}},
		"duration float":              {extra: map[string]any{"duration_ms": 12.5}},
		"duration string":             {extra: map[string]any{"duration_ms": "12"}},
		"duration null":               {extra: map[string]any{"duration_ms": nil}},
		"coach off":                   {env: map[string]string{"ORCH_LOOKUP_COACH": "off"}},
		"log off":                     {env: map[string]string{"ORCH_LOOKUP_LOG": "off"}},
		"guard off":                   {env: map[string]string{"ORCH_TOOL_GUARD": "off"}},
		"session with slash":          {session: "a/b c"},
		"session unicode":             {session: "é-1"},
		"session number":              {extra: map[string]any{"session_id": 5}},
		"session null":                {extra: map[string]any{"session_id": nil}},
		"unknown tool use id":         {extra: map[string]any{"tool_use_id": "tu_nope"}},
		"tool use id number":          {extra: map[string]any{"tool_use_id": 5}},
		"no transcript":               {extra: map[string]any{"transcript_path": "/nonexistent/t.jsonl"}},
		"transcript path number":      {extra: map[string]any{"transcript_path": 5}},
		"cwd missing":                 {extra: map[string]any{"cwd": nil}},
		"cwd relative":                {extra: map[string]any{"cwd": "."}},
		"no hook event name":          {extra: map[string]any{"hook_event_name": nil}},
		"error and response together": {extra: map[string]any{"error": "x"}},
	} {
		out = append(out, scenario{"behavior " + name, single(spec{"bash": "rg -n gateway src"}, o)})
	}
	for name, s := range map[string]spec{
		"long unicode query":        {"bash": "jbcontext search \"" + long + "\""},
		"rg double quote escape":    {"bash": `rg -n "a\"b\\c\d" src`},
		"read offset limit":         {"read": map[string]any{"path": "/a/b.go", "offset": 10, "limit": 20}},
		"read offset only":          {"read": map[string]any{"path": "/a/b.go", "offset": 10}},
		"read zero offset":          {"read": map[string]any{"path": "/a/b.go", "offset": 0}},
		"read float offset":         {"read": map[string]any{"path": "/a/b.go", "offset": 1.5, "limit": "7"}},
		"read bool limit":           {"read": map[string]any{"path": "/a/b.go", "limit": true}},
		"read empty path":           {"read": map[string]any{"path": ""}},
		"lsp full":                  {"lsp": map[string]any{"operation": "goToDefinition", "filePath": "/a.go", "line": 3}},
		"lsp no path":               {"lsp": map[string]any{"operation": "hover"}},
		"lsp null line":             {"lsp": map[string]any{"operation": "hover", "filePath": "/a.go", "line": nil}},
		"lsp no operation":          {"lsp": map[string]any{"filePath": "/a.go"}},
		"mcp query":                 {"mcp": map[string]any{"query": "find the gateway"}},
		"mcp q":                     {"mcp": map[string]any{"q": "other"}},
		"mcp neither":               {"mcp": map[string]any{}},
		"mcp zero query then q":     {"mcp": map[string]any{"query": 0, "q": "second"}},
		"mcp number query":          {"mcp": map[string]any{"query": 12}},
		"rg symbol word flag":       {"bash": "rg -w gateway src"},
		"rg symbol glob doc":        {"bash": "rg FooBar -g '*.md'"},
		"rg symbol type doc":        {"bash": "rg FooBar -t yaml"},
		"rg symbol path doc":        {"bash": "rg FooBar docs/readme.md"},
		"rg symbol func prefix":     {"bash": "rg 'func FooBar('"},
		"rg symbol dotted":          {"bash": "rg Foo.Bar"},
		"rg symbol underscore":      {"bash": "rg foo_bar src"},
		"rg symbol regexp flag":     {"bash": "rg --regexp=Foo src -e GoodName"},
		"rg symbol after dashdash":  {"bash": "rg -- FooBar"},
		"rg symbol glob equals":     {"bash": "rg FooBar --glob=*.{md,json}"},
		"fd flags":                  {"bash": "fd -e go -t f gateway src"},
		"orch ctx flags":            {"bash": "orch ctx text -v Foo bar"},
		"orch lsp flags first":      {"bash": "orch lsp --json def X"},
		"jbcontext no query":        {"bash": "jbcontext search"},
		"cd then rg":                {"bash": "cd x; rg Foo"},
		"cd alone":                  {"bash": "cd x"},
		"sudo rg":                   {"bash": "sudo rg Foo"},
		"quoted tool name":          {"bash": `"rg" Foo`},
		"bash command not a string": {"bash": 5},
	} {
		out = append(out, scenario{"classify " + name, single(s, runOpts{})})
	}
	return out
}

// finish adds what the run left on disk: the log rows without their timestamps
// and each session's state with the defaults filled in, the sandbox path blanked.
func finish(box *sandbox, trace []any) []any {
	rows := box.log()
	for _, row := range rows {
		delete(row, "ts")
	}
	states := map[string]any{}
	files, _ := filepath.Glob(filepath.Join(box.state, "coach", "*.json"))
	for _, path := range files {
		data, _ := os.ReadFile(path)
		state := map[string]any{"pending": nil, "window_first": nil, "turns": []any{}, "first_search_done": false,
			"concept_seen": false, "fired": []any{}, "prompt": 0.0}
		var got map[string]any
		json.Unmarshal(data, &got)
		for k, v := range got {
			state[k] = v
		}
		states[filepath.Base(path)] = state
	}
	var norm any
	raw := strings.ReplaceAll(string(jsonBytes(box.t, []any{trace, rows, states})), box.dir, "<dir>")
	json.Unmarshal([]byte(raw), &norm)
	return []any{norm}
}

func TestParityCoachOnEveryScenario(t *testing.T) {
	py := pythonRunner(t)
	for _, sc := range coachScenarios(t) {
		t.Run(sc.name, func(t *testing.T) {
			var traces [][]any
			var names []string
			for _, r := range []runner{py, goBinary{}, &alternating{runners: []runner{py, goBinary{}}}} {
				box := newSandbox(t)
				traces = append(traces, finish(box, sc.run(r, box)))
				names = append(names, r.name())
			}
			for i := 1; i < len(traces); i++ {
				if !reflect.DeepEqual(traces[0], traces[i]) {
					t.Errorf("%s and %s differ:\n %s: %s\n %s: %s", names[0], names[i], names[0], jsonBytes(t, traces[0]), names[i], jsonBytes(t, traces[i]))
				}
			}
		})
	}
}
