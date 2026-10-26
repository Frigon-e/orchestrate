package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCtxStatusMissingConfiguredServers(t *testing.T) {
	h := setup(t)
	writeFiles(t, h.repo, map[string]string{"app.ts": "export const value = 1;\n", "app.py": "value = 1\n", "main.go": "package main\n"})
	h.git("add", ".")
	h.git("commit", "-qm", "language status")
	missing := filepath.Join(h.tmp, "missing-server")
	t.Setenv("ORCH_PYLS", missing)
	t.Setenv("ORCH_TSLS", missing)
	t.Setenv("ORCH_GOPLS", missing)
	statuses := newCtxEnv(h.repo, ctxOpts{}).langStatuses()
	if len(statuses) != 3 {
		t.Fatalf("language statuses: %+v", statuses)
	}
	for _, status := range statuses {
		if status.Server != "" || !strings.Contains(status.Hint, langByID(status.ID).Hint) {
			t.Fatalf("missing server advertised as available or without install hint: %+v", status)
		}
	}
	// Lookup selection still carries the missing TypeScript override so actual
	// lookups retain the established failed-execution diagnostics.
	if bin, _ := tsServerChoice(h.repo); bin != missing {
		t.Fatalf("lookup semantics changed: %q", bin)
	}
}
