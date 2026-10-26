package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/toon-format/toon-go"
)

func decodedOutput(t *testing.T, b []byte, audience outputAudience) any {
	t.Helper()
	var v any
	var err error
	if audience == outputAgent {
		v, err = toon.Decode(b)
	} else {
		err = json.Unmarshal(b, &v)
	}
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	return v
}

// agentTrimmed is the full v2 selection as the agent and human views print it:
// no excerpt counters, no empty omissions or notes, no binary path in sources,
// and no match flag when every line is a match. --json keeps all of them.
func agentTrimmed(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	maps.Copy(out, obj)
	delete(out, "excerpt_budget_bytes")
	delete(out, "excerpt_bytes")
	if o, ok := out["omissions"].([]any); ok && len(o) == 0 {
		delete(out, "omissions")
	}
	if n, ok := out["notes"]; ok && (n == nil || len(n.([]any)) == 0) {
		delete(out, "notes")
	}
	if src, ok := out["sources"].(string); ok {
		var parts []string
		for _, p := range strings.Split(src, "; ") {
			tool, detail, _ := strings.Cut(p, " ")
			if strings.HasPrefix(detail, "/") || strings.HasPrefix(detail, "~/") {
				p = tool
			}
			parts = append(parts, p)
		}
		out["sources"] = strings.Join(parts, "; ")
	}
	// A batch prints the scope and exclusions its lookups share once.
	if rows, ok := out["result"].([]any); ok {
		for _, key := range []string{"searched_scope", "excluded"} {
			var first any
			count, same := 0, true
			for _, r := range rows {
				if v, ok := r.(map[string]any)[key]; ok && !reflect.DeepEqual(v, []any{}) && v != "" {
					if count > 0 && !reflect.DeepEqual(v, first) {
						same = false
					}
					first = v
					count++
				}
			}
			if same && count >= 2 {
				out[key] = first
				for _, r := range rows {
					delete(r.(map[string]any), key)
				}
			}
		}
	}
	var lines []map[string]any
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			if _, ok := x["n"]; ok {
				lines = append(lines, x)
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(out["result"])
	for _, l := range lines {
		if l["match"] != true {
			return out
		}
	}
	for _, l := range lines {
		delete(l, "match")
	}
	return out
}

// ctxTestObject/assertions inspect selected data, not incidental TOON layout.
func ctxTestObject(t *testing.T, out string) map[string]any {
	t.Helper()
	return decodedOutput(t, []byte(out), outputAgent).(map[string]any)
}

func ctxTestStrings(t *testing.T, out string) string {
	t.Helper()
	var values []string
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			values = append(values, v)
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(v[k])
			}
		}
	}
	walk(ctxTestObject(t, out))
	return strings.Join(values, "\n")
}

func ctxTestSourceLines(t *testing.T, out, path string) map[int]map[string]any {
	t.Helper()
	lines := map[int]map[string]any{}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			if v["path"] == path {
				rows, _ := v["lines"].([]any)
				for _, value := range rows {
					row := value.(map[string]any)
					n := int(row["n"].(float64))
					if _, ok := lines[n]; !ok {
						lines[n] = row
					}
				}
			}
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(ctxTestObject(t, out))
	return lines
}

func ctxTestHits(t *testing.T, out string, total, shown int) {
	t.Helper()
	r := ctxTestObject(t, out)["result"].(map[string]any)
	if r["total"] != float64(total) || r["shown"] != float64(shown) {
		t.Fatalf("hit counts want %d/%d: %#v", shown, total, r)
	}
}

func assertOutputEqual(t *testing.T, v any) {
	t.Helper()
	s, err := selectOutput(v)
	if err != nil {
		t.Fatal(err)
	}
	var want any
	for _, audience := range []outputAudience{outputHuman, outputAgent} {
		b, err := s.render(audience)
		if err != nil {
			t.Fatal(err)
		}
		got := decodedOutput(t, b, audience)
		if audience == outputHuman {
			want = got
		} else if !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip mismatch\ngot %#v\nwant %#v\n%s", got, want, b)
		}
	}
}

func TestAgentOutputNormalizedRoundTrip(t *testing.T) {
	python := "def greet(name):\n\tif name == \"世界\":\n\t\treturn 'hello, [friend]: λ'\n\n"
	ts := "export function greet(name: string) {\n\tconst label = `hi ${name}`;\n  return {label, ok: false};\n}\n"
	v := map[string]any{
		"code": []any{python, ts}, "null": nil, "false": false,
		"objects": map[string]any{}, "array": []any{},
		"ids":     []string{"42", "42", "false", "null", "05", "-1", "#hash", "[]"},
		"quoted":  "a,b|c:d\t\"quote\"\\path\r\n\x00世界👋",
		"numbers": []any{json.Number("9007199254740991"), json.Number("9007199254740992"), json.Number("9007199254740993"), json.Number("-9007199254740993"), json.Number("9223372036854775807"), json.Number("18446744073709551615"), json.Number("9.007199254740993e15"), json.Number("9007199254740993.0"), json.Number("1.00000000000000001"), json.Number("0.1"), json.Number("1e-7"), json.Number("-0")},
	}
	assertOutputEqual(t, v)
	s, _ := selectOutput(v)
	b, _ := s.render(outputAgent)
	got := decodedOutput(t, b, outputAgent).(map[string]any)
	code := got["code"].([]any)
	if code[0] != python || code[1] != ts {
		t.Fatal("source bytes/indentation changed")
	}
	for _, id := range got["ids"].([]any) {
		if _, ok := id.(string); !ok {
			t.Fatalf("ID changed type: %#v", id)
		}
	}
	ns := got["numbers"].([]any)
	if ns[2] != "9007199254740993" || ns[6] != "9007199254740993" || ns[7] != "9007199254740993" {
		t.Fatalf("integer rounded: %#v", ns)
	}
	// An independent normalized expectation pins every selected field, not
	// just agreement between two serializers using the same adapter.
	v["numbers"] = []any{float64(9007199254740991), "9007199254740992", "9007199254740993", "-9007199254740993", "9223372036854775807", "18446744073709551615", "9007199254740993", "9007199254740993", "1.00000000000000001", 0.1, 1e-7, 0}
	wantBytes, _ := json.Marshal(v)
	want := decodedOutput(t, wantBytes, outputJSON)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized JSON differs\ngot %#v\nwant %#v", got, want)
	}
}

func TestAgentOutputHugeExponentIsLosslessAndBounded(t *testing.T) {
	for _, value := range []string{"1e99999999", "-1e99999999", "1e-99999999", "1e99999999999999999999999999"} {
		s, err := selectOutput(json.Number(value))
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.render(outputAgent)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodedOutput(t, b, outputAgent); got != value {
			t.Fatalf("out-of-domain number changed: %#v", got)
		}
	}
}

type outputEmbedded struct {
	ID string `json:"id"`
}
type outputCustom struct{}

func (outputCustom) MarshalJSON() ([]byte, error) { return []byte(`{"custom":false}`), nil }

func TestAgentOutputUsesJSONContract(t *testing.T) {
	v := struct {
		outputEmbedded
		Raw    json.RawMessage `json:"raw"`
		Custom outputCustom    `json:"hook"`
		Absent string          `json:"absent,omitempty"`
	}{outputEmbedded: outputEmbedded{"42"}, Raw: json.RawMessage(`{"value":null}`)}
	assertOutputEqual(t, v)
	s, _ := selectOutput(v)
	got, _ := s.render(outputJSON)
	want, _ := json.MarshalIndent(v, "", "  ")
	if !bytes.Equal(got, want) {
		t.Fatalf("machine bytes changed: %s != %s", got, want)
	}
	if !strings.Contains(string(got), `"id": "42"`) || strings.Contains(string(got), "outputEmbedded") {
		t.Fatalf("embedded field lost: %s", got)
	}
}

func TestAgentOutputSelectionAndErrors(t *testing.T) {
	selected := &ctxView{Status: "partial", Kind: "text", Query: "x", Total: 10, Shown: 1, Cut: "9 matches omitted; --page 2", Parts: []*ctxPart{{Files: []ctxFileHits{{Path: "a.ts", Lines: []ctxLine{{N: 3, Text: "\treturn false", Match: true}}}}}}}
	assertOutputEqual(t, map[string]any{"result": selected, "sources": "rg", "notes": []string{}})
	assertOutputEqual(t, map[string]any{"status": "failed", "error": "bad \"[value]:\"\nnext line", "help": []string{"orch ctx text x --fixed"}})
	for _, scalar := range []string{"42", "false", "null", "[]", "#x", "-x", "a\tb", ""} {
		v, err := toon.DecodeString(agentScalar(scalar))
		if err != nil || v != scalar {
			t.Fatalf("scalar %q -> %#v (%v)", scalar, v, err)
		}
	}
}

// These are unmodified upstream v4.4.0 encoding fixtures. All files in the
// vendored encode category run, with no skipped cases or regenerated goldens.
func TestToonSpecFixtures(t *testing.T) {
	paths, err := filepath.Glob("testdata/toon-spec/encode/*.json")
	if err != nil || len(paths) != 9 {
		t.Fatalf("missing reference fixtures: %v %v", paths, err)
	}
	for _, path := range paths {
		var fixture struct {
			Tests []struct {
				Name     string          `json:"name"`
				Input    json.RawMessage `json:"input"`
				Expected string          `json:"expected"`
				Options  struct {
					IndentSize int    `json:"indentSize"`
					Delimiter  string `json:"delimiter"`
				} `json:"options"`
				ShouldError bool `json:"shouldError"`
			} `json:"tests"`
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &fixture); err != nil {
			t.Fatal(err)
		}
		for _, tc := range fixture.Tests {
			t.Run(filepath.Base(path)+"/"+tc.Name, func(t *testing.T) {
				dec := json.NewDecoder(bytes.NewReader(tc.Input))
				dec.UseNumber()
				// Reference fixtures exercise the pinned encoder's numeric domain;
				// application stringification is tested separately above.
				v, err := orderedOutputJSON(dec, false)
				if err != nil {
					t.Fatal(err)
				}
				opts := []toon.EncoderOption{}
				if tc.Options.IndentSize > 0 {
					opts = append(opts, toon.WithIndent(tc.Options.IndentSize))
				}
				if tc.Options.Delimiter != "" {
					opts = append(opts, toon.WithDelimiter(toon.Delimiter([]rune(tc.Options.Delimiter)[0])))
				}
				got, err := toon.MarshalString(v, opts...)
				if tc.ShouldError {
					if err == nil {
						t.Fatal("expected encoding error")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got != tc.Expected {
					t.Fatalf("fixture mismatch\ngot:\n%s\nwant:\n%s", got, tc.Expected)
				}
			})
		}
	}
}

func TestAgentOutputHumanQuestion(t *testing.T) {
	c := &card{question: "Replace the file?", context: "Current code overwrites it. The recovery behavior is unknown.", recommend: "refuse", next: "The owner must choose refuse or replace."}
	c.options = append(c.options, struct{ id, label, body string }{"refuse", "Refuse", "Keep the file."}, struct{ id, label, body string }{"replace", "Replace", "Lose the existing file."})
	c.extra = append(c.extra, struct{ title, body string }{"Uncertainty", "No recovery test exists."})
	v := askOutputView("/tmp/ask/index.html", []askQuestion{{Label: "export.overwrite", Question: c.question, source: c}}, nil)
	assertOutputEqual(t, v)
	s, _ := selectOutput(v)
	b, _ := s.render(outputHuman)
	got := decodedOutput(t, b, outputHuman).(map[string]any)
	q := got["decisions"].([]any)[0].(map[string]any)
	if len(q["options"].([]any)) != 2 || q["answer_needed_from"] != "owner" || q["id"] != "export.overwrite" {
		t.Fatalf("lost decision: %s", b)
	}
	wantContains(t, string(b), `{"id": "refuse", "choice": "Refuse", "effect": "Keep the file."}`, "recovery behavior is unknown", "No recovery test exists.")
	if strings.Index(string(b), `"question"`) > strings.Index(string(b), `"context"`) {
		t.Fatal("deep context before question")
	}
}

func TestAgentOutputLegacyJSONBytes(t *testing.T) {
	// This literal is the pre-B3 ctxEmit MarshalIndent output for the fixture.
	// In particular: no schema_version/IDs, null notes and trimmed source.
	v := &ctxView{Kind: "text", Query: "x", Unit: "matches", Total: 1, Shown: 1, Parts: []*ctxPart{{Files: []ctxFileHits{{Path: "a.py", Lines: []ctxLine{{N: 2, Text: "return False", Match: true}}}}}}}
	e := &ctxEnv{}
	var b bytes.Buffer
	a := &app{out: &b}
	if err := a.ctxEmit(e, true, v); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"notes\": null,\n  \"result\": {\n    \"kind\": \"text\",\n    \"query\": \"x\",\n    \"unit\": \"matches\",\n    \"total\": 1,\n    \"shown\": 1,\n    \"parts\": [\n      {\n        \"files\": [\n          {\n            \"path\": \"a.py\",\n            \"lines\": [\n              {\n                \"n\": 2,\n                \"text\": \"return False\",\n                \"match\": true\n              }\n            ]\n          }\n        ]\n      }\n    ]\n  },\n  \"sources\": \"none\"\n}\n"
	if b.String() != want {
		t.Fatalf("legacy bytes changed\ngot %s\nwant %s", b.String(), want)
	}
}

func TestAgentOutputV2BatchConceptTipOnceWithoutMutatingViews(t *testing.T) {
	views := []*ctxView{
		{Kind: "text", Query: "StartCheckout", Total: 1, Shown: 1},
		{Kind: "find", Query: "checkout gateway", Total: 1, Shown: 1, Tip: conceptTipText},
		{Kind: "find", Query: "payment gateway", Total: 1, Shown: 1, Tip: conceptTipText},
	}
	legacy := func() string {
		var b bytes.Buffer
		a := &app{out: &b}
		if err := a.ctxEmit(&ctxEnv{}, true, views); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	before := legacy()
	var selected any
	for _, audience := range []outputAudience{outputAgent, outputHuman, outputJSON} {
		var b bytes.Buffer
		a := &app{out: ctxV2Output{&b, audience}}
		if err := a.ctxEmitV2(&ctxEnv{}, views, ctxFmt{SchemaVersion: 2}, 1000); err != nil {
			t.Fatal(err)
		}
		decoded := decodedOutput(t, b.Bytes(), audience)
		if audience == outputJSON {
			decoded = agentTrimmed(decoded)
		}
		results := decoded.(map[string]any)["result"].([]any)
		tips := 0
		for i, result := range results {
			if tip, ok := result.(map[string]any)["tip"]; ok {
				tips++
				if i != 1 || tip != conceptTipText {
					t.Fatalf("tip changed or moved from first concept lookup: %#v", results)
				}
			}
		}
		if tips != 1 {
			t.Errorf("audience %v emitted %d tips, want exactly one", audience, tips)
		}
		if selected != nil && !reflect.DeepEqual(selected, decoded) {
			t.Errorf("audiences selected different batch facts: %#v != %#v", selected, decoded)
		}
		selected = decoded
		if views[1].Tip != conceptTipText || views[2].Tip != conceptTipText || legacy() != before {
			t.Fatal("v2 selection mutated cached views or legacy JSON bytes")
		}
	}
}

func TestAgentOutputV2BudgetAndSource(t *testing.T) {
	c := newCtxHarness(t)
	python := "def greet(name):\n\tif name == \"世界\":\n\t\treturn 'a,b: [x]'  \n\n"
	ts := "export const greet = () => {\n\treturn {message: \"世界\", ok: false};\n};\n"
	os.WriteFile(filepath.Join(c.repo, "sample.py"), []byte(python), 0o644)
	os.WriteFile(filepath.Join(c.repo, "sample.ts"), []byte(ts), 0o644)
	e := newCtxEnv(c.repo, ctxOpts{Cfg: config{}})
	f := ctxFmt{SchemaVersion: 2}
	view := e.textView(rgQuery{Pattern: "greet"}, "include", rgResult{Hits: []rgHit{{Path: "sample.py", Line: 2, Text: "trimmed"}, {Path: "sample.ts", Line: 2, Text: "trimmed"}}}, 10, 1, 2, f)
	var want any
	for _, audience := range []outputAudience{outputHuman, outputAgent, outputJSON} {
		var b bytes.Buffer
		a := &app{out: ctxV2Output{&b, audience}}
		if err := a.ctxEmitV2(e, view, f, 10000); err != nil {
			t.Fatal(err)
		}
		got := decodedOutput(t, b.Bytes(), audience)
		if audience == outputJSON {
			got = agentTrimmed(got)
		}
		if audience == outputHuman {
			want = got
		} else if !reflect.DeepEqual(got, want) {
			t.Fatalf("different selection: %s", b.String())
		}
		result := got.(map[string]any)["result"].(map[string]any)
		files := result["parts"].([]any)[0].(map[string]any)["files"].([]any)
		for _, file := range files {
			obj := file.(map[string]any)
			path := obj["path"].(string)
			native := strings.Split(map[string]string{"sample.py": python, "sample.ts": ts}[path], "\n")
			if obj["id"] == "" {
				t.Fatal("missing evidence ID")
			}
			for _, line := range obj["lines"].([]any) {
				row := line.(map[string]any)
				if row["text"] != native[int(row["n"].(float64))-1] {
					t.Fatalf("source bytes lost: %#v", row)
				}
			}
		}
	}
	for _, audience := range []outputAudience{outputHuman, outputAgent, outputJSON} {
		var b bytes.Buffer
		a := &app{out: ctxV2Output{&b, audience}}
		if err := a.ctxEmitV2(e, view, f, 5); err != nil {
			t.Fatal(err)
		}
		got := decodedOutput(t, b.Bytes(), audience)
		if audience == outputJSON {
			if got.(map[string]any)["excerpt_bytes"].(float64) > 5 {
				t.Fatalf("budget not enforced %s", b.String())
			}
			got = agentTrimmed(got)
		}
		if audience == outputHuman {
			want = got
		} else if !reflect.DeepEqual(got, want) {
			t.Fatal("budget differs by serializer")
		}
		obj := got.(map[string]any)
		if obj["status"] != "partial" {
			t.Fatalf("budget not enforced %s", b.String())
		}
		if _, ok := obj["excerpt_bytes"]; ok {
			t.Fatalf("excerpt counter printed for a reader: %s", b.String())
		}
		omissions := obj["omissions"].([]any)
		if len(omissions) == 0 {
			t.Fatal("silent truncation")
		}
		for _, omission := range omissions {
			o := omission.(map[string]any)
			if o["id"] == "" || !strings.Contains(o["expand"].(string), "native read") {
				t.Fatalf("no expansion: %#v", o)
			}
		}
	}
	// Selection must not mutate cached data or the legacy fixture.
	if view.Parts[0].Files[0].ID != "" || len(view.Parts[0].Files[0].Lines) == 0 {
		t.Fatal("selection mutated observations")
	}
}

func TestAgentOutputReferenceRelationships(t *testing.T) {
	first := &ctxView{Kind: "text", Query: "context", Unit: "matches", Total: 1, Shown: 1, Parts: []*ctxPart{{Files: []ctxFileHits{{Path: "a.py", Lines: []ctxLine{{N: 2, Text: "\treturn False"}}}}}}}
	second := &ctxView{Kind: "text", Query: "False", Unit: "matches", Total: 1, Shown: 1, Parts: []*ctxPart{{Files: []ctxFileHits{{Path: "a.py", Lines: []ctxLine{{N: 2, Text: "\treturn False", Match: true, In: "def check"}}}}}}}
	for _, audience := range []outputAudience{outputAgent, outputHuman, outputJSON} {
		var b bytes.Buffer
		a := &app{out: ctxV2Output{&b, audience}}
		if err := a.ctxEmitV2(&ctxEnv{ReadOnly: true}, []*ctxView{first, second}, ctxFmt{SchemaVersion: 2}, 100); err != nil {
			t.Fatal(err)
		}
		rows := decodedOutput(t, b.Bytes(), audience).(map[string]any)["result"].([]any)
		line := func(i int) map[string]any {
			return rows[i].(map[string]any)["parts"].([]any)[0].(map[string]any)["files"].([]any)[0].(map[string]any)["lines"].([]any)[0].(map[string]any)
		}
		one, two := line(0), line(1)
		if one["text"] != "\treturn False" || two["reference"] != one["id"] || two["match"] != true || two["in"] != "def check" || two["n"] != float64(2) {
			t.Fatalf("lost reference relationship: %#v %#v", one, two)
		}
		if _, ok := two["text"]; ok {
			t.Fatal("repeated source body")
		}
	}
}

func TestAgentOutputAudienceOverrides(t *testing.T) {
	c := newCtxHarness(t)
	legacy := c.must("ctx", "text", "StartCheckout", "--json")
	if got := c.must("ctx", "text", "StartCheckout", "--json", "--schema-version", "2", "--schema-version", "1"); got != legacy {
		t.Fatal("last schema flag did not preserve legacy bytes")
	}
	human := c.must("ctx", "text", "StartCheckout", "--json", "--json=false", "--human")
	if got := decodedOutput(t, []byte(human), outputHuman).(map[string]any)["schema_version"]; got != float64(2) {
		t.Fatalf("human schema=%#v", got)
	}
	for _, args := range [][]string{{"--human", "--human=false"}, {"--glob", "--json"}} {
		out := c.must(append([]string{"ctx", "text", "StartCheckout"}, args...)...)
		if got := ctxTestObject(t, out)["schema_version"]; got != float64(2) {
			t.Fatalf("agent schema=%#v", got)
		}
	}
}

func TestAgentOutputCtxCommandEquality(t *testing.T) {
	c := newCtxHarness(t)
	for _, args := range [][]string{{"ctx", "text", "StartCheckout", "--limit", "1"}, {"ctx", "text", "zzzNone"}, {"ctx", "status"}, {"ctx", "batch", "-q", "text:StartCheckout limit=1", "-q", "text:zzzNone"}} {
		agent := c.must(args...)
		machine := c.must(append(append([]string{}, args...), "--json", "--schema-version", "2")...)
		human := c.must(append(append([]string{}, args...), "--human")...)
		want := agentTrimmed(decodedOutput(t, []byte(machine), outputJSON))
		if got := decodedOutput(t, []byte(agent), outputAgent); !reflect.DeepEqual(got, want) {
			t.Fatalf("ctx audiences differ\nagent %s\njson %s", agent, machine)
		}
		if got := decodedOutput(t, []byte(human), outputHuman); !reflect.DeepEqual(got, want) {
			t.Fatalf("ctx human differs: %s", human)
		}
	}
	for _, flag := range []string{"", "--json", "--human"} {
		args := []string{"ctx", "text", "x", "--bad=[quote]"}
		audience := outputAgent
		if flag != "" {
			args = append(args, flag)
			if flag == "--json" {
				audience = outputJSON
			} else {
				audience = outputHuman
			}
		}
		code, out := c.run(args...)
		if code != 2 {
			t.Fatalf("exit %d: %s", code, out)
		}
		got := decodedOutput(t, []byte(out), audience).(map[string]any)
		if got["status"] != "failed" || got["error"] == nil {
			t.Fatalf("unstructured error %s", out)
		}
	}
}

func TestAgentOutputTrimsWhatAReaderCannotUse(t *testing.T) {
	emit := func(audience outputAudience, lines ...ctxLine) string {
		e := newCtxEnv(t.TempDir(), ctxOpts{Cfg: config{}})
		e.use("rg", "/opt/homebrew/bin/rg")
		e.use("jbcontext", "index abc1234 (HEAD abc1234, stale)")
		view := &ctxView{Kind: "text", Query: "x", Unit: "matches", Total: 1, Shown: 1, Parts: []*ctxPart{{Files: []ctxFileHits{{Path: "a.py", Lines: lines}}}}}
		var b bytes.Buffer
		a := &app{out: ctxV2Output{&b, audience}}
		if err := a.ctxEmitV2(e, view, ctxFmt{SchemaVersion: 2}, 1000); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	hit := ctxLine{N: 2, Text: "return False", Match: true, In: "def check"}
	agent := emit(outputAgent, hit, ctxLine{N: 3, Text: "x = 1", Match: true})
	for _, gone := range []string{"match:", "omissions", "notes:", "excerpt_budget_bytes", "excerpt_bytes", "/opt/homebrew"} {
		if strings.Contains(agent, gone) {
			t.Errorf("%q printed for a reader:\n%s", gone, agent)
		}
	}
	if !strings.Contains(agent, `sources: "jbcontext index abc1234 (HEAD abc1234, stale); rg"`) {
		t.Errorf("tool names and the staleness note must stay:\n%s", agent)
	}
	withContext := emit(outputAgent, ctxLine{N: 1, Text: "def check():"}, hit)
	if !strings.Contains(withContext, "match: true") {
		t.Errorf("match must stay when a row is a context line:\n%s", withContext)
	}
	full := ctxTestObjectJSON(t, emit(outputJSON, hit))
	for _, kept := range []string{"excerpt_budget_bytes", "excerpt_bytes", "omissions", "notes", "sources"} {
		if _, ok := full[kept]; !ok {
			t.Errorf("--json lost %q: %#v", kept, full)
		}
	}
	if !strings.Contains(full["sources"].(string), "/opt/homebrew/bin/rg") {
		t.Errorf("--json sources lost the path: %#v", full["sources"])
	}
	line := full["result"].(map[string]any)["parts"].([]any)[0].(map[string]any)["files"].([]any)[0].(map[string]any)["lines"].([]any)[0].(map[string]any)
	if line["match"] != true {
		t.Errorf("--json lost match: %#v", line)
	}
}

func ctxTestObjectJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	return decodedOutput(t, []byte(out), outputJSON).(map[string]any)
}

// A plain session cites no evidence ids, so rows carry none: the file keeps its
// id and a repeated row names the place it was shown, as path:line.
func TestAgentOutputPlainSessionDropsLineIDs(t *testing.T) {
	first := &ctxView{Kind: "text", Query: "context", Unit: "matches", Total: 1, Shown: 1, Parts: []*ctxPart{{Files: []ctxFileHits{{Path: "a.py", Lines: []ctxLine{{N: 2, Text: "\treturn False"}}}}}}}
	second := &ctxView{Kind: "text", Query: "False", Unit: "matches", Total: 1, Shown: 1, Parts: []*ctxPart{{Files: []ctxFileHits{{Path: "a.py", Lines: []ctxLine{{N: 2, Text: "\treturn False", Match: true, In: "def check"}}}}}}}
	for _, audience := range []outputAudience{outputAgent, outputHuman, outputJSON} {
		var b bytes.Buffer
		a := &app{out: ctxV2Output{&b, audience}}
		if err := a.ctxEmitV2(&ctxEnv{}, []*ctxView{first, second}, ctxFmt{SchemaVersion: 2}, 100); err != nil {
			t.Fatal(err)
		}
		rows := decodedOutput(t, b.Bytes(), audience).(map[string]any)["result"].([]any)
		file := func(i int) map[string]any {
			return rows[i].(map[string]any)["parts"].([]any)[0].(map[string]any)["files"].([]any)[0].(map[string]any)
		}
		one, two := file(0)["lines"].([]any)[0].(map[string]any), file(1)["lines"].([]any)[0].(map[string]any)
		if _, ok := one["id"]; ok || one["text"] != "\treturn False" {
			t.Fatalf("plain row carries an id or lost its text: %#v", one)
		}
		if id, _ := file(0)["id"].(string); !strings.HasPrefix(id, "ctx-") || len(id) != len("ctx-")+8 {
			t.Fatalf("file id=%q, want ctx- and 8 hex", id)
		}
		if two["reference"] != "a.py:2" || two["in"] != "def check" || two["match"] != true {
			t.Fatalf("repeated row: %#v", two)
		}
		if _, ok := file(1)["references"]; ok {
			t.Fatalf("plain file lists line ids: %#v", file(1))
		}
	}
}

func TestAgentOutputBatchPrintsSharedScopeOnce(t *testing.T) {
	excl := []string{"gitignored and hidden files", "tests=none"}
	mk := func(q, scope string, excluded []string) *ctxView {
		return &ctxView{Kind: "text", Query: q, Unit: "matches", Total: 0, SearchedScope: scope, Excluded: excluded}
	}
	views := []*ctxView{mk("a", "checkout root /r", excl), mk("b", "checkout root /r", excl), mk("c", "checkout root /r; path pay", excl)}
	var agent, full bytes.Buffer
	if err := (&app{out: ctxV2Output{&agent, outputAgent}}).ctxEmitV2(&ctxEnv{}, views, ctxFmt{SchemaVersion: 2}, 100); err != nil {
		t.Fatal(err)
	}
	if err := (&app{out: ctxV2Output{&full, outputJSON}}).ctxEmitV2(&ctxEnv{}, views, ctxFmt{SchemaVersion: 2}, 100); err != nil {
		t.Fatal(err)
	}
	got := decodedOutput(t, agent.Bytes(), outputAgent).(map[string]any)
	if _, ok := got["excluded"]; !ok || strings.Count(agent.String(), "tests=none") != 1 {
		t.Errorf("shared exclusions not printed once:\n%s", agent.String())
	}
	if _, ok := got["searched_scope"]; ok {
		t.Errorf("differing scopes must stay per lookup:\n%s", agent.String())
	}
	if strings.Count(agent.String(), "checkout root /r") != 3 {
		t.Errorf("per-lookup scopes lost:\n%s", agent.String())
	}
	if strings.Count(full.String(), "tests=none") != 3 {
		t.Errorf("--json must keep exclusions on every lookup:\n%s", full.String())
	}
	if want := agentTrimmed(decodedOutput(t, full.Bytes(), outputJSON)); !reflect.DeepEqual(got, want) {
		t.Errorf("agent view differs from the trimmed JSON:\n%#v\n%#v", got, want)
	}
}
