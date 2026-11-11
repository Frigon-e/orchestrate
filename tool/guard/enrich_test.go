package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The language server is a fake `orch` script that answers `lsp <verb> <name>
// --json` from fixture files, in the shapes the real `orch lsp ... --json` prints
// (copied from runs in a real repository).

const fakeOrch = `#!/bin/sh
[ "$1" = lsp ] || exit 2
verb=$2
name=$3
if [ "$verb" = warm ]; then echo "$name" > "$FAKE_DIR/warm.marker"; exit 0; fi
if [ -n "$FAKE_SLOW" ]; then exec sleep 5; fi
f="$FAKE_DIR/$verb.$name.json"
if [ -f "$f" ]; then cat "$f"; exit 0; fi
echo "{\"results\":[{\"name\":\"$name\",\"status\":\"not_found\",\"items\":[]}],\"verb\":\"$verb\"}"
exit 1
`

const (
	capDef     = `{"results":[{"name":"capacityHealth","status":"ok","items":[{"path":"internal/panel/app/ext_nodes.go","line":161,"kind":"Function","text":"func capacityHealth(h nodefeed.Health, now time.Time) ext.NodeHealth {","doc":"capacityHealth is a node's health for the capacity export: online only when"}]}],"verb":"def"}`
	capCallers = `{"results":[{"name":"capacityHealth","status":"ok","total":3,"items":[` +
		`{"path":"internal/panel/app/ext_nodes.go","line":152,"in":"Capacity"},` +
		`{"path":"internal/panel/app/ext_nodes_staleness_test.go","line":30,"in":"TestCapacityHealthStaleness"},` +
		`{"path":"internal/panel/modules/virtnodes/capacity.go","line":44,"in":"summarise"}]}],"verb":"callers"}`
	capSig = "func capacityHealth(h nodefeed.Health, now time.Time) ext.NodeHealth"
	capDoc = "capacityHealth is a node's health for the capacity export: online only when"

	nodeHealthAmbiguous = `{"results":[{"name":"NodeHealth","status":"ambiguous","message":"NodeHealth is ambiguous (3 candidates)","items":[` +
		`{"path":"internal/panel/ext/nodes.go","line":125,"kind":"Struct"},` +
		`{"path":"internal/virt/panelapi/ext.go","line":77,"kind":"Struct"},` +
		`{"path":"internal/panel/store/models.go","line":456,"kind":"Struct"}]}],"verb":"def"}`
	extNodeHealthDef  = `{"results":[{"name":"ext.NodeHealth","status":"ok","items":[{"path":"internal/panel/ext/nodes.go","line":125,"kind":"Struct","text":"type NodeHealth struct {","doc":"NodeHealth is the panel's view of a node's Wings: its own 60-second check"}]}],"verb":"def"}`
	extNodeHealthRefs = `{"results":[{"name":"ext.NodeHealth","status":"ok","total":3,"items":[` +
		`{"path":"internal/panel/app/ext_nodes.go","line":161,"text":"func capacityHealth"},` +
		`{"path":"internal/panel/app/ext_nodes.go","line":163,"text":"return ext.NodeHealth{"},` +
		`{"path":"internal/panel/ext/nodes.go","line":111,"text":"Health NodeHealth"}]}],"verb":"refs"}`
)

type enrichBox struct {
	t    *testing.T
	repo string
	fake string
}

func newEnrichBox(t *testing.T) *enrichBox {
	t.Helper()
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := t.TempDir()
	bin := filepath.Join(fake, "orch")
	if err := os.WriteFile(bin, []byte(fakeOrch), 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"ORCH_BIN": bin, "FAKE_DIR": fake, "FAKE_SLOW": "", "ORCH_TOOL_GUARD_STATE_DIR": t.TempDir(), "ORCH_CACHE_DIR": t.TempDir(),
		"ORCH_LSP_ENRICH": "", "ORCH_LSP_ENRICH_BUDGET_MS": "5000", "ORCH_TOOL_GUARD": "", "ORCH_LOOKUP_COACH": "", "ORCH_LOOKUP_LOG": "off",
	} {
		t.Setenv(k, v)
	}
	old := dirtyFingerprint
	t.Cleanup(func() { dirtyFingerprint = old })
	dirtyFingerprint = func(string) string { return "fp1" }
	b := &enrichBox{t, repo, fake}
	b.fx("def", "capacityHealth", capDef)
	b.fx("callers", "capacityHealth", capCallers)
	return b
}

func (b *enrichBox) fx(verb, name, body string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.fake, verb+"."+name+".json"), []byte(body), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

func (b *enrichBox) event(session, tool string, input map[string]any, response any) *Event {
	b.t.Helper()
	ev, ok := ParseEvent(jsonBytes(b.t, map[string]any{
		"hook_event_name": "PostToolUse", "session_id": session, "cwd": b.repo, "tool_name": tool,
		"tool_input": input, "tool_response": response,
	}))
	if !ok {
		b.t.Fatal("bad event")
	}
	return ev
}

func (b *enrichBox) bash(session, cmd, out string) *Event {
	return b.event(session, "Bash", map[string]any{"command": cmd}, map[string]any{"stdout": out, "stderr": ""})
}

// run is the enrichment of a Bash search in a fresh session.
func (b *enrichBox) run(cmd, out string) string {
	b.t.Helper()
	return Enrich(b.bash("s-"+cmd, cmd, out))
}

func eq(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func parsed(t *testing.T, command string) *Event {
	ev, ok := ParseEvent(jsonBytes(t, bashEvent(command)))
	if !ok {
		t.Fatal("bad event")
	}
	return ev
}

func TestEnrichNamesAreTheSymbolsOfTheSearchPattern(t *testing.T) {
	for command, want := range map[string]string{
		`rg -n "NodeHealth|LastContactAt" src`:         "NodeHealth,LastContactAt",
		`rg -e capacityHealth -e summarise`:            "capacityHealth,summarise",
		`grep -rn "capacityHealth\|summarise" src`:     "capacityHealth,summarise",
		`rg '\bcapacityHealth\b' src`:                  "capacityHealth",
		`rg "func capacityHealth\(" src`:               "capacityHealth",
		`rg 'func \(s \*Server\) HandleThing\(' src`:   "HandleThing",
		`git grep -n "^capacityHealth$"`:               "capacityHealth",
		`rg ext.NodeHealth src`:                        "ext.NodeHealth",
		`rg "failed to load" src`:                      "",
		`rg foo src`:                                   "",
		`rg a.b.c src`:                                 "",
		`rg -F "capacity health" src`:                  "",
		`rg -i capacityHealth src`:                     "",
		`rg -g '*.md' capacityHealth`:                  "",
		`rg capacityHealth README.md`:                  "",
		`go test ./... | grep capacityHealth`:          "",
		`cat out.txt | rg capacityHealth`:              "",
		`grep capacityHealth notes.txt`:                "",
		`grep -n "^capacityHealth" ../go.mod`:          "",
		`rg capacityHealth go.sum schema.sql`:          "",
		`rg capacityHealth app.go schema.sql`:          "capacityHealth",
		`cd internal && rg -n capacityHealth . | head`: "capacityHealth",
	} {
		var names []string
		for _, c := range searchCallsOf(parsed(t, command), "/repo") {
			if codeSearch(c) {
				names = append(names, patternNames(c)...)
			}
		}
		if got := strings.Join(names, ","); got != want {
			t.Errorf("%s: names %q, want %q", command, got, want)
		}
	}
}

func TestEnrichIsSilentWhenTheSearchIsNotForASymbol(t *testing.T) {
	b := newEnrichBox(t)
	for _, cmd := range []string{
		`rg -n -i capacityHealth internal`,
		`rg -n -g '*.md' capacityHealth`,
		`rg -n "failed to load" internal`,
		`rg -n -F "capacity health" internal`,
		`rg -n notASymbolAnywhere internal`, // the language server has no such symbol
	} {
		if out := b.run(cmd, ""); out != "" {
			t.Errorf("%s: %q", cmd, out)
		}
	}
	// The control: the same facts do come out for a plain symbol search.
	if out := b.run(`rg -n capacityHealth internal`, ""); !strings.HasPrefix(out, "lsp capacityHealth (func): ") {
		t.Errorf("control search was silent: %q", out)
	}
}

func TestEnrichDropsTheShownDefinitionAndListsTheHiddenCaller(t *testing.T) {
	b := newEnrichBox(t)
	shown := "internal/panel/app/ext_nodes.go:161:func capacityHealth(h nodefeed.Health, now time.Time) ext.NodeHealth {\n"
	eq(t, b.run("rg -n capacityHealth internal/panel/app", shown),
		"lsp capacityHealth (func): "+capSig+"\n"+
			"  "+capDoc+"\n"+
			"  callers in scope, not shown above: internal/panel/app/ext_nodes.go:152 in Capacity\n"+
			"  tests: 1 caller\n"+
			"  +1 caller outside internal/panel/app: internal/panel/modules/virtnodes 1")
}

func TestEnrichShowsAnUnseenDefinitionEvenOutsideTheScope(t *testing.T) {
	b := newEnrichBox(t)
	eq(t, b.run("rg -n capacityHealth internal/panel/modules", ""),
		"lsp capacityHealth (func): "+capSig+"\n"+
			"  defined: internal/panel/app/ext_nodes.go:161\n"+
			"  "+capDoc+"\n"+
			"  callers in scope, not shown above: internal/panel/modules/virtnodes/capacity.go:44 in summarise\n"+
			"  +2 callers outside internal/panel/modules: internal/panel/app 2")
}

func TestEnrichAppliesGlobExcludesAndCdToTheScope(t *testing.T) {
	b := newEnrichBox(t)
	def := "internal/panel/app/ext_nodes.go:161:func capacityHealth\n"
	eq(t, b.run("rg -n capacityHealth -g '!*_test.go' internal/panel/app", def),
		"lsp capacityHealth (func): "+capSig+"\n"+
			"  "+capDoc+"\n"+
			"  callers in scope, not shown above: internal/panel/app/ext_nodes.go:152 in Capacity\n"+
			"  +2 callers outside internal/panel/app -g !*_test.go: internal/panel/app 1, internal/panel/modules/virtnodes 1")
	// After a cd the paths of the output and of the scope are relative to it.
	eq(t, b.run("cd internal/panel/app && rg -n capacityHealth .", "ext_nodes.go:161:func capacityHealth\n"),
		"lsp capacityHealth (func): "+capSig+"\n"+
			"  "+capDoc+"\n"+
			"  callers in scope, not shown above: internal/panel/app/ext_nodes.go:152 in Capacity\n"+
			"  tests: 1 caller\n"+
			"  +1 caller outside .: internal/panel/modules/virtnodes 1")
}

func TestEnrichAddsTheRefsAHeadCutHid(t *testing.T) {
	b := newEnrichBox(t)
	def := "internal/panel/app/ext_nodes.go:161:func capacityHealth\n"
	cut := b.run("rg -n capacityHealth . | head -5", def)
	if !strings.Contains(cut, "callers in scope, not shown above: internal/panel/app/ext_nodes.go:152 in Capacity; internal/panel/modules/virtnodes/capacity.go:44 in summarise") {
		t.Errorf("the callers the head cut off are missing: %q", cut)
	}
	all := def + "internal/panel/app/ext_nodes.go:152:cn.Health = capacityHealth(h, now)\n" +
		"internal/panel/app/ext_nodes_staleness_test.go:30:got := capacityHealth(tc.in, now)\n" +
		"internal/panel/modules/virtnodes/capacity.go:44:capacityHealth(x)\n"
	if out := b.run("rg -n capacityHealth .", all); out != "" {
		t.Errorf("every line was shown, yet: %q", out)
	}
}

func TestEnrichGrepToolFilesWithMatches(t *testing.T) {
	b := newEnrichBox(t)
	ev := b.event("g1", "Grep", map[string]any{"pattern": "capacityHealth", "path": "internal/panel/app"},
		map[string]any{"mode": "files_with_matches", "filenames": []string{"internal/panel/app/ext_nodes.go"}, "numFiles": 1})
	eq(t, Enrich(ev),
		"lsp capacityHealth (func): "+capSig+"\n"+
			"  "+capDoc+"\n"+
			"  tests: 1 caller\n"+
			"  +1 caller outside internal/panel/app: internal/panel/modules/virtnodes 1")
}

func TestEnrichNarrowsAnAmbiguousNameByTheScope(t *testing.T) {
	b := newEnrichBox(t)
	b.fx("def", "NodeHealth", nodeHealthAmbiguous)
	b.fx("def", "ext.NodeHealth", extNodeHealthDef)
	b.fx("refs", "ext.NodeHealth", extNodeHealthRefs)
	eq(t, b.run("rg -n NodeHealth internal/panel/ext", "internal/panel/ext/nodes.go:125:type NodeHealth struct {\n"),
		"lsp NodeHealth (struct): type NodeHealth struct\n"+
			"  NodeHealth is the panel's view of a node's Wings: its own 60-second check\n"+
			"  refs in scope, not shown above, by package: internal/panel/ext 1\n"+
			"  +2 refs outside internal/panel/ext: internal/panel/app 2\n"+
			"  same name: panelapi.NodeHealth (struct) internal/virt/panelapi/ext.go:77\n"+
			"  same name: store.NodeHealth (struct) internal/panel/store/models.go:456")
	// Nothing narrows it: only the same-name symbols are listed.
	eq(t, b.run("rg -n NodeHealth", ""),
		"lsp NodeHealth (ambiguous): 3 symbols named NodeHealth\n"+
			"  same name: ext.NodeHealth (struct) internal/panel/ext/nodes.go:125\n"+
			"  same name: panelapi.NodeHealth (struct) internal/virt/panelapi/ext.go:77\n"+
			"  same name: store.NodeHealth (struct) internal/panel/store/models.go:456")
}

func TestEnrichDropsANameWithMoreThanTenDefinitions(t *testing.T) {
	b := newEnrichBox(t)
	var items []string
	for i := 0; i < 11; i++ {
		items = append(items, fmt.Sprintf(`{"path":"p%d/a.go","line":1,"kind":"Field"}`, i))
	}
	b.fx("def", "LastContactAt", `{"results":[{"name":"LastContactAt","status":"ambiguous","items":[`+strings.Join(items, ",")+`]}],"verb":"def"}`)
	if out := b.run("rg -n LastContactAt", ""); out != "" {
		t.Errorf("a generic name was enriched: %q", out)
	}
}

// fixtureFuncs makes n functions, each with 10 callers that no search output shows.
func (b *enrichBox) fixtureFuncs(names ...string) {
	for _, name := range names {
		b.fx("def", name, fmt.Sprintf(`{"results":[{"name":%q,"status":"ok","items":[{"path":"internal/x/%s.go","line":3,"kind":"Function","text":"func %s()"}]}],"verb":"def"}`, name, name, name))
		var items []string
		for i := 0; i < 10; i++ {
			items = append(items, fmt.Sprintf(`{"path":"internal/y/f%d.go","line":%d,"in":"C%d"}`, i, i+1, i))
		}
		b.fx("callers", name, fmt.Sprintf(`{"results":[{"name":%q,"status":"ok","total":10,"items":[%s]}],"verb":"callers"}`, name, strings.Join(items, ",")))
	}
}

func TestEnrichBudgetShrinksWithTheNumberOfNames(t *testing.T) {
	b := newEnrichBox(t)
	b.fixtureFuncs("nameA", "nameB", "nameC", "nameD", "nameE", "nameF")
	callerEntries := func(out string) (counts []int) {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "  callers in scope") {
				counts = append(counts, strings.Count(l, " in C"))
			}
		}
		return counts
	}
	one := b.run("rg -n nameA .", "")
	if got := callerEntries(one); len(got) != 1 || got[0] != 8 || !strings.HasSuffix(one, "\nmore: orch lsp callers nameA") {
		t.Errorf("one name: %v\n%s", got, one)
	}
	two := b.run("rg -n 'nameA|nameB' .", "")
	if got := callerEntries(two); len(got) != 2 || got[0] != 3 || got[1] != 3 || !strings.HasSuffix(two, "\nmore: orch lsp callers nameA; orch lsp callers nameB") {
		t.Errorf("two names: %v\n%s", got, two)
	}
	three := b.run("rg -n 'nameA|nameB|nameC' .", "")
	if n := len(strings.Split(three, "\n")); n != 6 || strings.Contains(three, "  callers in scope") {
		t.Errorf("three names want header and counts line each (6 lines), got %d:\n%s", n, three)
	}
	six := b.run("rg -n 'nameA|nameB|nameC|nameD|nameE|nameF' .", "")
	if !strings.HasSuffix(six, "\n+2 names: orch lsp def nameE nameF") || strings.Contains(six, "lsp nameE (") {
		t.Errorf("six names:\n%s", six)
	}
}

func TestEnrichSaysEachNameOncePerSessionUntilTheDirtyFilesChange(t *testing.T) {
	b := newEnrichBox(t)
	cmd := "rg -n capacityHealth internal/panel/app"
	ask := func(session string) string { return Enrich(b.bash(session, cmd, "")) }
	if ask("d1") == "" {
		t.Fatal("first search was silent")
	}
	if out := ask("d1"); out != "" {
		t.Errorf("the same name in the same state was said twice: %q", out)
	}
	if ask("d2") == "" {
		t.Error("another session got nothing")
	}
	dirtyFingerprint = func(string) string { return "fp2" }
	if ask("d1") == "" {
		t.Error("an edited file did not re-enable the name")
	}
}

func coachOut(ev *Event) string {
	var out bytes.Buffer
	Coach(ev, &out)
	return out.String()
}

func TestEnrichDoesNotDependOnTheGuardAndCoachSwitches(t *testing.T) {
	b := newEnrichBox(t)
	for name, env := range map[string]map[string]string{
		"guard off": {"ORCH_TOOL_GUARD": "off"}, "coach off": {"ORCH_LOOKUP_COACH": "off"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		out := coachOut(b.bash("sw-"+name, "rg -n capacityHealth internal/panel/app", ""))
		if !strings.Contains(out, `"hookEventName": "PostToolUse"`) || !strings.Contains(out, "lsp capacityHealth (func)") {
			t.Errorf("%s: %q", name, out)
		}
		for k := range env {
			t.Setenv(k, "")
		}
	}
	t.Setenv("ORCH_LOOKUP_COACH", "off")
	t.Setenv("ORCH_LSP_ENRICH", "off")
	if out := coachOut(b.bash("sw-enrich-off", "rg -n capacityHealth internal/panel/app", "")); out != "" {
		t.Errorf("ORCH_LSP_ENRICH=off still enriched: %q", out)
	}
	// A Grep tool call is no coach lookup, and is enriched all the same.
	t.Setenv("ORCH_LSP_ENRICH", "")
	grep := b.event("sw-grep", "Grep", map[string]any{"pattern": "capacityHealth", "path": "internal/panel/modules"}, map[string]any{"filenames": []string{}})
	if out := coachOut(grep); !strings.Contains(out, "lsp capacityHealth (func)") {
		t.Errorf("Grep tool: %q", out)
	}
}

func TestEnrichReplacesTheSymbolViaLspReminder(t *testing.T) {
	b := newEnrichBox(t)
	// The first rg of a window gets concept-first; the second symbol rg would get symbol-via-lsp.
	second := func(session, enrich string) string {
		t.Setenv("ORCH_LSP_ENRICH", "off")
		coachOut(b.bash(session, "rg -n capacityHealth internal/panel/app", ""))
		t.Setenv("ORCH_LSP_ENRICH", enrich)
		return coachOut(b.bash(session, "rg -n capacityHealth internal/panel/app", ""))
	}
	if out := second("r-off", "off"); !strings.Contains(out, "orch lsp def|refs|callers") {
		t.Fatalf("the reminder did not fire without enrichment: %q", out)
	}
	out := second("r-on", "")
	if !strings.Contains(out, "lsp capacityHealth (func)") || strings.Contains(out, "orch lookup coach") {
		t.Errorf("enrichment and the reminder both came out: %q", out)
	}
}

func TestEnrichBeyondTheBudgetIsSilentAndStartsWarm(t *testing.T) {
	b := newEnrichBox(t)
	t.Setenv("FAKE_SLOW", "1")
	t.Setenv("ORCH_LSP_ENRICH_BUDGET_MS", "200")
	if out := b.run("rg -n capacityHealth internal/panel/app", ""); out != "" {
		t.Errorf("a slow language server still answered: %q", out)
	}
	marker := filepath.Join(b.fake, "warm.marker")
	waitFor(t, "orch lsp warm", func() bool { _, err := os.Stat(marker); return err == nil })
	if data, _ := os.ReadFile(marker); strings.TrimSpace(string(data)) != b.repo {
		t.Errorf("warm ran for %q, want %q", data, b.repo)
	}
}

func TestEnrichLogsEveryDecision(t *testing.T) {
	b := newEnrichBox(t)
	b.run(`rg -n "failed to load" internal`, "")
	b.run("rg -n capacityHealth internal/panel/app", "")
	data, err := os.ReadFile(filepath.Join(os.Getenv("ORCH_CACHE_DIR"), "lsp-enrich.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var decisions []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row struct {
			Decision string
			Names    []string
			Lines    int
			Session  string
			Cwd      string
		}
		if json.Unmarshal([]byte(l), &row) != nil || row.Session == "" || row.Cwd != b.repo {
			t.Fatalf("bad row %q", l)
		}
		decisions = append(decisions, fmt.Sprintf("%s:%v:%d", row.Decision, row.Names, row.Lines))
	}
	if want := "no-names:[]:0 fired:[capacityHealth]:6"; strings.Join(decisions, " ") != want {
		t.Errorf("decisions %v, want %q", decisions, want)
	}
}
