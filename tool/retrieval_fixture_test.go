package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureRetrievalTruth(t *testing.T) {
	for _, f := range portabilityFixtures {
		t.Run(f.name, func(t *testing.T) {
			h := fixtureRepo(t, f.name)
			t.Run("receiver-and-unavailable-server", func(t *testing.T) {
				fixtureRG(t, h, fixtureDeclaration(t, h, f))
				e := newCodeEnv(h.repo, codeOpts{})
				fact := e.Sym("Wrong.Normalize")
				if len(fact.Defs) < 2 {
					t.Fatalf("same-name modules lost: %+v", fact)
				}
				for _, d := range fact.Defs {
					if d.Match == "exact" {
						t.Fatalf("wrong receiver became exact: %+v", d)
					}
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
	statuses := newCodeEnv(h.repo, codeOpts{}).langStatuses()
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
}
