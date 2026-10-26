package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type portabilityFixture struct {
	name, language, path, other, test, ext string
	line                                   int
	old, current                           string
}

var portabilityFixtures = []portabilityFixture{
	{"go", "go", "parser/parser.go", "other/parser.go", "parser/parser_test.go", ".go", 6, "return strings.TrimSpace(value)", "return strings.ToLower(strings.TrimSpace(value))"},
	{"py", "python", "parser/reader.py", "other/reader.py", "tests/test_reader.py", ".py", 1, "return value.strip()", "return value.strip().lower()"},
	{"ts", "typescript", "parser/reader.ts", "other/reader.ts", "parser/reader.test.ts", ".ts", 2, "return value.trim();", "return value.trim().toLowerCase();"},
}

// Every fixture starts from a committed copy. No installed language server,
// model, semantic index or ripgrep is used by these tests.
func fixtureRepo(t *testing.T, name string) *harness {
	t.Helper()
	h := setup(t)
	t.Setenv("ORCH_PYLS", filepath.Join(h.tmp, "missing-pyls"))
	root := filepath.Join("testdata", "fixtures", name)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Editors/analyzers may leave ignored caches beside fixture inputs.
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(h.repo, rel)), 0755); err != nil {
			return err
		}
		writeFiles(t, h.repo, map[string]string{rel: string(data)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h.git("add", ".")
	h.git("commit", "-qm", "fixture source")
	fixtureRG(t, h, "")
	return h
}

// Fixed transcripts exercise the real streaming decoder and CLI without any
// dependency on a machine's search tools. The caller chooses the answer key.
func fixtureRG(t *testing.T, h *harness, transcript string) {
	t.Helper()
	bin := filepath.Join(h.tmp, "fixture-rg")
	writeFiles(t, h.tmp, map[string]string{"rg-response": transcript, "fixture-rg": "#!/bin/sh\nif [ ! -s '" + filepath.Join(h.tmp, "rg-response") + "' ]; then exit 1; fi\n/bin/cat '" + filepath.Join(h.tmp, "rg-response") + "'\n"})
	if err := os.Chmod(bin, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_RG", bin)
}
func fixtureMatch(path string, line int, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "match", "data": map[string]any{"path": map[string]string{"text": path}, "lines": map[string]string{"text": text + "\n"}, "line_number": line, "submatches": []any{}}})
	return string(b) + "\n"
}

const fixtureSummary = "{\"type\":\"summary\",\"data\":{}}\n"

func fixtureDeclaration(t *testing.T, h *harness, f portabilityFixture) string {
	t.Helper()
	var out strings.Builder
	for _, path := range []string{f.path, f.other} {
		lines := strings.Split(string(mustRead(t, filepath.Join(h.repo, path))), "\n")
		for n, line := range lines {
			if strings.Contains(line, "Normalize(value") && !strings.Contains(line, "return Normalize") {
				out.WriteString(fixtureMatch(path, n+1, line))
				break
			}
		}
	}
	if out.Len() == 0 {
		t.Fatalf("missing declarations for %s", f.name)
	}
	return out.String() + fixtureSummary
}
