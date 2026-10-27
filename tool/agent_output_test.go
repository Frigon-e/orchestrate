package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
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
	assertOutputEqual(t, map[string]any{"status": "failed", "error": "bad \"[value]:\"\nnext line", "help": []string{"orch lsp def x"}})
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
