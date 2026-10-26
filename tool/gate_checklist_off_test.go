package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const checklistSpecPkg = "### api: API\nkind: ship\nowns: internal/api.go\n\nAdds a helper.\n#### Builds\n- Builds the API.\n- Adds the PutRecord helper.\n  search: Code that stores records with `PutRecord`\n  reuse: internal/store.go#PutRecord\n#### Acceptance\nTests pass.\n"

func checklistOffRepo(t *testing.T, h *harness) string {
	t.Helper()
	os.MkdirAll(filepath.Join(h.repo, "internal"), 0o755)
	writeFiles(t, h.repo, map[string]string{"internal/store.go": "package internal\n\n// PutRecord stores a record.\nfunc PutRecord() {}\n"})
	h.commitSpec(planHeader + checklistSpecPkg + planTail)
	return filepath.Join(filepath.Dir(h.dir("x")), "gate", specHash(mustRead(t, filepath.Join(h.repo, "spec.md"))))
}

func TestGateCarriesAPlainReuseChecklistAndRunsNoLookup(t *testing.T) {
	h := setup(t)
	jb, jbCalls, _ := fakeJbcontext(t)
	t.Setenv("ORCH_JBCONTEXT", jb)
	t.Setenv("ORCH_GOPLS", filepath.Join(h.tmp, "no-gopls"))
	gdir := checklistOffRepo(t, h)
	h.sessionPi(gatePasses(nil))
	t.Setenv("HERDR_ENV", "")
	h.must("gate", "spec.md")

	calls := piCalls(t, h)
	wantContains(t, calls,
		"### 3a. Reuse checklist (each requirement remains a separate decision)",
		"- `api#1` Builds the API.",
		"- `api#2` Adds the PutRecord helper.",
		"  author's pointer: `internal/store.go#PutRecord`",
		"  search: `Code that stores records with `PutRecord`` (not run)")
	if strings.Contains(calls, "Use the evidence dossier") || exists(filepath.Join(gdir, "dossier")) {
		t.Errorf("the gate produced a dossier:\n%s", calls)
	}
	if exists(jbCalls) {
		t.Errorf("jbcontext ran:\n%s", mustRead(t, jbCalls))
	}
	items := string(mustRead(t, filepath.Join(gdir, "items.json")))
	wantContains(t, items, `"id": "api#1"`, `"id": "api#2"`, `"internal/store.go#PutRecord"`)
	if strings.Contains(items, "Cands") || strings.Contains(items, "candidates") {
		t.Errorf("items.json holds candidates:\n%s", items)
	}
	if rec := readRecord(t, gdir); rec.Checklist != "plain" {
		t.Errorf("record: checklist %q", rec.Checklist)
	}
	wantContains(t, string(mustRead(t, filepath.Join(gdir, "report.md"))), "Reuse checklist: plain")

	for _, mode := range []string{"off", "code"} {
		code, out := h.run("gate", "spec.md", "--dossier", mode)
		if code == 0 || !strings.Contains(out, "--dossier was removed") {
			t.Errorf("--dossier %s: exit %d, output:\n%s", mode, code, out)
		}
	}
}

func TestPlainChecklistTwoItems(t *testing.T) {
	got := plainChecklist([]gateItem{
		{ID: "api#1", Text: "Builds the API."},
		{ID: "api#2", Text: "Adds the PutRecord helper.", Queries: []string{"Code that stores records"}, Reuse: []string{"internal/store.go#PutRecord"}},
	})
	want := "\n\n### 3a. Reuse checklist (each requirement remains a separate decision)\n" +
		"No code lookups were run for this list: decide reuse or new for every item, one entry each, and search the repo yourself before you claim an existing match.\n" +
		"- `api#1` Builds the API.\n" +
		"- `api#2` Adds the PutRecord helper.\n" +
		"  author's pointer: `internal/store.go#PutRecord`\n" +
		"  search: `Code that stores records` (not run)\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
	if plainChecklist(nil) != "" {
		t.Error("an empty item list must give no section")
	}
}
