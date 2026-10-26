package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFixtureRetrievalTruth(t *testing.T) {
	for _, f := range portabilityFixtures {
		t.Run(f.name, func(t *testing.T) {
			h := fixtureRepo(t, f.name)
			t.Run("excluded-before-production", func(t *testing.T) {
				var raw strings.Builder
				// More excluded matches than the retention cap, then a production match.
				for range 2105 {
					raw.WriteString(fixtureMatch(f.test, 1, "Normalize test"))
					raw.WriteString(fixtureMatch("generated/sample"+f.ext, 1, "Normalize generated"))
					raw.WriteString(fixtureMatch("vendor/sample"+f.ext, 1, "Normalize vendor"))
				}
				raw.WriteString(fixtureMatch(f.path, f.line, "Normalize production"))
				raw.WriteString(fixtureSummary)
				fixtureRG(t, h, raw.String())
				code, out := h.run("ctx", "text", "Normalize", "--json")
				v, _ := truthResult(t, out)
				if code != 0 || v.Total != 1 || v.Hidden != 2105 || v.HiddenG != 4210 || v.Zero != "" || !strings.Contains(out, f.path) {
					t.Fatalf("excluded hits hid production: %d %s", code, out)
				}
			})
			t.Run("receiver-and-unavailable-server", func(t *testing.T) {
				fixtureRG(t, h, fixtureDeclaration(t, h, f))
				e := newCtxEnv(h.repo, ctxOpts{})
				fact := e.Sym("Wrong.Normalize")
				if len(fact.Defs) < 2 {
					t.Fatalf("same-name modules lost: %+v", fact)
				}
				for _, d := range fact.Defs {
					if d.Match == "exact" {
						t.Fatalf("wrong receiver became exact: %+v", d)
					}
				}
				// A name-only scan is not a semantic reference graph.
				out := h.must("ctx", "sym", "Normalize", "--json")
				v, _ := truthResult(t, out)
				if v.Status != "partial" || v.CountKind == "exact" || len(v.Errors) == 0 || strings.Contains(out, "used by 0") {
					t.Fatalf("missing server became zero references: %s", out)
				}
			})
			t.Run("body-change-renders-checkout", func(t *testing.T) {
				before := string(mustRead(t, filepath.Join(h.repo, f.path)))
				current := strings.Replace(before, f.old, f.current, 1)
				if before == current {
					t.Fatal("body mutation did not apply")
				}
				writeFiles(t, h.repo, map[string]string{f.path: current})
				b, _ := json.Marshal(map[string]any{"type": "search_result", "results": []any{map[string]any{"result": map[string]any{"sourcePosition": map[string]string{"relativePath": f.path}}, "content": before, "contentStartLine": 1}}})
				bin := filepath.Join(h.tmp, "fixture-jb")
				writeFiles(t, h.tmp, map[string]string{"jb-response": string(b), "fixture-jb": "#!/bin/sh\ncase \"$1\" in\nstatus) echo '{\"type\":\"status_result\",\"indices\":[]}';;\nsearch) /bin/cat '" + filepath.Join(h.tmp, "jb-response") + "';;\nesac\n"})
				if err := os.Chmod(bin, 0755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("ORCH_JBCONTEXT", bin)
				out := h.must("ctx", "find", "Code that normalizes a file name", "--json")
				if !strings.Contains(out, f.current) {
					t.Fatalf("did not render current body: %s", out)
				}
				// Compare selected data from the actual CLI, not independently built objects.
				toonOut := h.must("ctx", "find", "Code that normalizes a file name")
				out = h.must("ctx", "find", "Code that normalizes a file name", "--json", "--schema-version", "2")
				var jsonValue any
				if err := json.Unmarshal([]byte(out), &jsonValue); err != nil {
					t.Fatal(err)
				}
				toonValue := decodedOutput(t, []byte(toonOut), outputAgent)
				// The agent view drops what --json keeps (counters, empty fields, paths).
				jsonValue = agentTrimmed(jsonValue)
				if !reflect.DeepEqual(jsonValue, toonValue) {
					t.Fatalf("selection differs: JSON %#v TOON %#v", jsonValue, toonValue)
				}
			})
		})
	}
}

func TestFixtureMixedRoots(t *testing.T) {
	h := fixtureRepo(t, "mixed")
	for _, tc := range []struct{ path, language, root string }{{"processor/reader.py", "python", "processor/"}, {"cli/reader.ts", "typescript", "cli/"}} {
		t.Run(tc.language, func(t *testing.T) {
			r, ok := rootFor(h.repo, tc.path, tc.language)
			if !ok || r.Dir != tc.root {
				t.Fatalf("wrong project root: %+v %v", r, ok)
			}
		})
	}
	statuses := newCtxEnv(h.repo, ctxOpts{}).langStatuses()
	seen := map[string]bool{}
	for _, status := range statuses {
		if status.ID != "python" && status.ID != "typescript" {
			continue
		}
		seen[status.ID] = true
		if status.Server != "" || status.Hint == "" {
			t.Fatalf("unavailable %s tool not disclosed: %+v", status.ID, status)
		}
	}
	if !seen["python"] || !seen["typescript"] {
		t.Fatalf("missing root languages: %+v", statuses)
	}
	t.Run("language-specific-tool-routing", func(t *testing.T) {
		py, ts := filepath.Join(h.tmp, "fixture-pyls"), filepath.Join(h.tmp, "fixture-tsls")
		writeFiles(t, h.tmp, map[string]string{"fixture-pyls": "#!/bin/sh\nexit 0\n", "fixture-tsls": "#!/bin/sh\nexit 0\n"})
		for _, bin := range []string{py, ts} {
			if err := os.Chmod(bin, 0755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("ORCH_PYLS", py)
		t.Setenv("ORCH_TSLS", ts)
		bin, _, ok, why := langServerReason(filepath.Join(h.repo, "processor"), langByID("python"))
		if !ok || bin != py {
			t.Fatalf("Python root chose wrong tool: %s %s", bin, why)
		}
		bin, why = tsServerChoice(filepath.Join(h.repo, "cli"))
		if bin != ts {
			t.Fatalf("TypeScript root chose wrong tool: %s %s", bin, why)
		}
	})
	fixtureRG(t, h, fixtureMatch("processor/reader.py", 1, "def Normalize(value):")+fixtureMatch("cli/reader.ts", 1, "export function Normalize(value: string): string {")+fixtureSummary)
	out := h.must("ctx", "sym", "Normalize", "--json")
	v, _ := truthResult(t, out)
	if v.Status != "partial" || v.CountKind == "exact" || len(v.Errors) == 0 || !strings.Contains(out, "processor/reader.py") || !strings.Contains(out, "cli/reader.ts") {
		t.Fatalf("mixed-root degradation hidden: %s", out)
	}
}
