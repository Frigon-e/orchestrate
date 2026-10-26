package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureReferenceBackendFailure(t *testing.T) {
	for _, f := range portabilityFixtures {
		t.Run(f.name, func(t *testing.T) {
			h := fixtureRepo(t, f.name)
			fixtureRG(t, h, fixtureDeclaration(t, h, f))
			e := newCtxEnv(h.repo, ctxOpts{})
			var fact symFact
			switch f.language {
			case "go":
				bin := filepath.Join(h.tmp, "fixture-gopls")
				script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\nworkspace_symbol) echo '%s:%d:6 Normalize Function';;\n*) echo 'fixture reference backend failed' >&2; exit 1;;\nesac\n", filepath.Join(h.repo, f.path), f.line)
				writeFiles(t, h.tmp, map[string]string{"fixture-gopls": script})
				if err := os.Chmod(bin, 0755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("ORCH_GOPLS", bin)
				fact = newCtxEnv(h.repo, ctxOpts{}).Sym("Normalize")
			case "typescript":
				c := truthClient(t, func(method string) any {
					if method == "workspace/symbol" {
						return []any{map[string]any{"name": "Normalize", "kind": 12, "location": map[string]any{"uri": fileURI(filepath.Join(h.repo, f.path)), "range": map[string]any{"start": map[string]int{"line": f.line - 1, "character": 16}, "end": map[string]int{"line": f.line - 1, "character": 25}}}}}
					}
					return fmt.Errorf("fixture reference backend failed")
				})
				var err error
				fact, err = tsFacts(c, h.repo, "Normalize")
				if err != nil {
					t.Fatal(err)
				}
			case "python":
				base := e.declFacts("Normalize")
				if base == nil {
					t.Fatal("missing Python declarations")
				}
				c := truthClient(t, func(string) any { return fmt.Errorf("fixture reference backend failed") })
				fact = *(&lspSession{c: c, caps: lspCaps{Refs: true}, bin: "fixture"}).refine(e, langByID("python"), base, "Normalize")
			}
			v := e.symView(fact, symOpts{})
			if len(fact.Defs) == 0 || v.Status != "partial" || v.CountKind != "unknown" || len(v.Errors) == 0 || v.Zero != "" || !strings.Contains(strings.Join(v.Errors, ";"), "references") {
				t.Fatalf("failed references treated as known zero: fact=%+v view=%+v", fact, v)
			}
		})
	}
}
